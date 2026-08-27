package garmin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const tokenRefreshWindow = 15 * time.Minute

var (
	// ErrSessionExpired indicates that cached credentials can no longer be
	// refreshed and the caller must perform a new credential login.
	ErrSessionExpired = errors.New("Garmin session expired")
	// ErrNoTokenFile indicates that a requested token cache does not exist.
	ErrNoTokenFile = errors.New("Garmin token file does not exist")
)

// Tokens contains the credentials needed to resume Garmin API and browser
// sessions. RefreshToken, CSRFToken, and BrowserCookies grant account access
// and must be protected.
type Tokens struct {
	AccessToken    string          `json:"access_token"`
	RefreshToken   string          `json:"refresh_token,omitempty"`
	ClientID       string          `json:"client_id,omitempty"`
	CSRFToken      string          `json:"csrf_token,omitempty"`
	BrowserCookies []BrowserCookie `json:"browser_cookies,omitempty"`
}

// BrowserCookie is an authentication cookie required by Garmin's browser-only
// APIs. Values are secrets and must be protected like refresh tokens.
type BrowserCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// WithTokenFile makes the client atomically persist tokens after login and
// refresh. It does not load an existing file; use NewClientFromTokenFile for
// that purpose.
func WithTokenFile(path string) Option {
	return func(c *Client) error {
		if strings.TrimSpace(path) == "" {
			return errors.New("token file path must not be empty")
		}
		c.tokenFile = path
		return nil
	}
}

// NewClientWithTokens resumes a Garmin session from previously saved tokens.
func NewClientWithTokens(tokens Tokens, options ...Option) (*Client, error) {
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return nil, errors.New("access token must not be empty")
	}
	c, err := newUnauthenticatedClient(options...)
	if err != nil {
		return nil, err
	}
	c.accessToken = tokens.AccessToken
	c.refreshToken = tokens.RefreshToken
	c.clientID = tokens.ClientID
	c.csrfToken = tokens.CSRFToken
	c.browserCookies = cloneBrowserCookies(tokens.BrowserCookies)
	if err := c.restoreBrowserCookies(); err != nil {
		return nil, err
	}
	return c, nil
}

// NewClientFromTokenFile loads a securely cached Garmin session.
func NewClientFromTokenFile(path string, options ...Option) (*Client, error) {
	tokens, err := LoadTokens(path)
	if err != nil {
		return nil, err
	}
	options = append(options, WithTokenFile(path))
	return NewClientWithTokens(tokens, options...)
}

// Tokens returns a consistent snapshot of the client's current tokens.
func (c *Client) Tokens() Tokens {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	return c.tokensLocked()
}

func (c *Client) tokensLocked() Tokens {
	return Tokens{
		AccessToken:    c.accessToken,
		RefreshToken:   c.refreshToken,
		ClientID:       c.clientID,
		CSRFToken:      c.csrfToken,
		BrowserCookies: cloneBrowserCookies(c.browserCookies),
	}
}

func (c *Client) setTokens(tokens Tokens) error {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.accessToken = tokens.AccessToken
	c.refreshToken = tokens.RefreshToken
	c.clientID = tokens.ClientID
	c.csrfToken = tokens.CSRFToken
	c.browserCookies = cloneBrowserCookies(tokens.BrowserCookies)
	return c.persistTokensLocked()
}

func cloneBrowserCookies(cookies []BrowserCookie) []BrowserCookie {
	return append([]BrowserCookie(nil), cookies...)
}

func (c *Client) restoreBrowserCookies() error {
	if len(c.browserCookies) == 0 {
		return nil
	}
	if c.httpClient.Jar == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return fmt.Errorf("create Garmin browser cookie jar: %w", err)
		}
		c.httpClient.Jar = jar
	}
	cookies := make([]*http.Cookie, 0, len(c.browserCookies))
	for _, cookie := range c.browserCookies {
		if cookie.Name == "" || cookie.Value == "" {
			return errors.New("Garmin browser cookie must have a name and value")
		}
		cookies = append(cookies, &http.Cookie{Name: cookie.Name, Value: cookie.Value, Path: "/", Secure: c.webURL.Scheme == "https", HttpOnly: true})
	}
	c.httpClient.Jar.SetCookies(c.webURL, cookies)
	return nil
}

func (c *Client) persistTokensLocked() error {
	if c.tokenFile == "" {
		return nil
	}
	if err := SaveTokens(c.tokenFile, c.tokensLocked()); err != nil {
		return fmt.Errorf("save Garmin tokens: %w", err)
	}
	return nil
}

func (c *Client) accessTokenForRequest(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.accessToken == "" {
		return "", ErrSessionExpired
	}
	expiresSoon, expiryKnown := tokenExpiresSoon(c.accessToken, time.Now())
	if expiresSoon {
		if c.refreshToken == "" {
			if expiryKnown {
				return "", ErrSessionExpired
			}
			return c.accessToken, nil
		}
		if err := c.refreshLocked(ctx); err != nil {
			return "", err
		}
	}
	return c.accessToken, nil
}

func (c *Client) hasRefreshToken() bool {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	return c.refreshToken != ""
}

func (c *Client) refreshAfterUnauthorized(ctx context.Context, rejectedToken string) error {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.accessToken != rejectedToken {
		return nil
	}
	if c.refreshToken == "" {
		return ErrSessionExpired
	}
	return c.refreshLocked(ctx)
}

func (c *Client) refreshLocked(ctx context.Context) error {
	clientID := c.clientID
	if clientID == "" {
		clientID = diClientID
	}
	result, err := c.requestToken(ctx, clientID, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     clientID,
		"refresh_token": c.refreshToken,
	})
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && (httpErr.StatusCode == 400 || httpErr.StatusCode == 401 || httpErr.StatusCode == 403) {
			return fmt.Errorf("%w: %v", ErrSessionExpired, err)
		}
		return fmt.Errorf("refresh Garmin token: %w", err)
	}
	c.accessToken = result.AccessToken
	if result.RefreshToken != "" {
		c.refreshToken = result.RefreshToken
	}
	if extracted := clientIDFromToken(result.AccessToken); extracted != "" {
		c.clientID = extracted
	} else {
		c.clientID = clientID
	}
	return c.persistTokensLocked()
}

func tokenExpiresSoon(token string, now time.Time) (bool, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return false, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false, false
	}
	var claims struct {
		Expires json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&claims); err != nil || claims.Expires == "" {
		return false, false
	}
	expires, err := strconv.ParseInt(claims.Expires.String(), 10, 64)
	if err != nil {
		return false, false
	}
	return !now.Add(tokenRefreshWindow).Before(time.Unix(expires, 0)), true
}

func clientIDFromToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		ClientID string `json:"client_id"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.ClientID
}

// LoadTokens reads a token file created by SaveTokens.
func LoadTokens(path string) (Tokens, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Tokens{}, ErrNoTokenFile
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("inspect token file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Tokens{}, errors.New("token file must not be a symbolic link")
	}
	file, err := os.Open(path)
	if err != nil {
		return Tokens{}, fmt.Errorf("open token file: %w", err)
	}
	defer file.Close()
	var tokens Tokens
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&tokens); err != nil {
		return Tokens{}, fmt.Errorf("decode token file: %w", err)
	}
	if tokens.AccessToken == "" {
		return Tokens{}, errors.New("token file has no access token")
	}
	return tokens, nil
}

// SaveTokens atomically writes tokens with owner-only permissions.
func SaveTokens(path string, tokens Tokens) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("token file path must not be empty")
	}
	if tokens.AccessToken == "" {
		return errors.New("access token must not be empty")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create token directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("secure token directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("token file must not be a symbolic link")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect token file: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".garmin-tokens-*")
	if err != nil {
		return fmt.Errorf("create temporary token file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure temporary token file: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	if err := encoder.Encode(tokens); err != nil {
		temporary.Close()
		return fmt.Errorf("encode token file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync token file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close token file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace token file: %w", err)
	}
	return os.Chmod(path, 0o600)
}
