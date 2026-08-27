// Package garmin provides a small client for the undocumented Garmin Connect
// activity API.
//
// The API is not affiliated with Garmin and may change without notice.
package garmin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://connectapi.garmin.com"
	maxListLimit   = 1000
	defaultTimeout = 30 * time.Second
)

// Client is a client for Garmin Connect activity endpoints. It is safe for
// concurrent use provided its Options are not mutated after construction.
type Client struct {
	baseURL        *url.URL
	webURL         *url.URL
	httpClient     *http.Client
	tokenMu        sync.Mutex
	accessToken    string
	refreshToken   string
	clientID       string
	csrfToken      string
	browserCookies []BrowserCookie
	tokenFile      string
	ssoURL         *url.URL
	tokenURL       *url.URL
	serviceURL     string
	portalURL      string
	authService    string
	authFlow       string
	mfaMethod      string
	loginDelay     func(context.Context) error
}

// WithWebBaseURL overrides the connect.garmin.com base URL used by web-only
// APIs such as Garmin Dive. It is primarily useful for tests.
func WithWebBaseURL(rawURL string) Option {
	return func(c *Client) error {
		u, err := parseBaseURL(rawURL)
		if err != nil {
			return err
		}
		c.webURL = u
		return nil
	}
}

// Option configures a Client.
type Option func(*Client) error

// WithBaseURL overrides the API base URL. It is useful for tests and for
// Garmin regional endpoints.
func WithBaseURL(rawURL string) Option {
	return func(c *Client) error {
		u, err := parseBaseURL(rawURL)
		if err != nil {
			return err
		}
		c.baseURL = u
		return nil
	}
}

func parseBaseURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid base URL %q", rawURL)
	}
	return u, nil
}

// WithHTTPClient sets the HTTP client used for requests.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) error {
		if httpClient == nil {
			return errors.New("HTTP client must not be nil")
		}
		c.httpClient = httpClient
		return nil
	}
}

// NewClient creates an authenticated Garmin Connect client. The access token
// is sent only in the Authorization header and is never logged by this package.
func NewClient(accessToken string, options ...Option) (*Client, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, errors.New("access token must not be empty")
	}
	baseURL, _ := url.Parse(defaultBaseURL)
	c := &Client{
		baseURL:     baseURL,
		webURL:      mustParseURL("https://connect.garmin.com"),
		httpClient:  newDefaultHTTPClient(),
		accessToken: accessToken,
		ssoURL:      mustParseURL("https://sso.garmin.com"),
		tokenURL:    mustParseURL("https://diauth.garmin.com/di-oauth2-service/oauth/token"),
		serviceURL:  "https://mobile.integration.garmin.com/gcm/ios",
		portalURL:   "https://connect.garmin.com/app/",
		loginDelay:  browserLoginDelay,
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("client option must not be nil")
		}
		if err := option(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func newDefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: defaultTimeout}
}

func mustParseURL(rawURL string) *url.URL {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return u
}

func (c *Client) get(ctx context.Context, path string, query url.Values, dst any) error {
	return c.getFrom(ctx, c.baseURL, path, query, dst)
}

func (c *Client) getFrom(ctx context.Context, baseURL *url.URL, path string, query url.Values, dst any) error {
	u := baseURL.JoinPath(path)
	u.RawQuery = query.Encode()
	response, err := c.doAuthenticatedGET(ctx, u, "application/json")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return newHTTPError(response)
	}
	if dst == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode Garmin response: %w", err)
	}
	return nil
}

func (c *Client) download(ctx context.Context, path string) ([]byte, error) {
	u := c.baseURL.JoinPath(path)
	response, err := c.doAuthenticatedGET(ctx, u, "*/*")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, newHTTPError(response)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read Garmin response: %w", err)
	}
	return body, nil
}

func (c *Client) doAuthenticatedGET(ctx context.Context, endpoint *url.URL, accept string) (*http.Response, error) {
	token, err := c.accessTokenForRequest(ctx)
	if err != nil {
		return nil, err
	}
	response, err := c.sendGET(ctx, endpoint, accept, token)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusUnauthorized {
		return response, nil
	}
	response.Body.Close()
	if !c.hasRefreshToken() {
		return nil, ErrSessionExpired
	}
	if err := c.refreshAfterUnauthorized(ctx, token); err != nil {
		return nil, err
	}
	token, err = c.accessTokenForRequest(ctx)
	if err != nil {
		return nil, err
	}
	response, err = c.sendGET(ctx, endpoint, accept, token)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		response.Body.Close()
		return nil, ErrSessionExpired
	}
	return response, nil
}

func (c *Client) sendGET(ctx context.Context, endpoint *url.URL, accept, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", accept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("User-Agent", "GCM-Android-5.23")
	req.Header.Set("X-App-Ver", "10861")
	req.Header.Set("X-Garmin-Client-Platform", "Android")
	req.Header.Set("X-Garmin-Paired-App-Version", "10861")
	req.Header.Set("X-Garmin-User-Agent", "com.garmin.android.apps.connectmobile/5.23; ; Google/sdk_gphone64_arm64/google; Android/33; Dalvik/2.1.0")
	req.Header.Set("X-GCExperience", "GC5")
	req.Header.Set("X-Lang", "en")
	if endpoint.Host == c.webURL.Host {
		c.tokenMu.Lock()
		csrfToken := c.csrfToken
		browserCookies := cloneBrowserCookies(c.browserCookies)
		c.tokenMu.Unlock()
		req.Header.Del("Authorization")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Connect-Csrf-Token", csrfToken)
		referer := c.webURL.JoinPath("app/")
		if marker := strings.LastIndex(endpoint.Path, "/by/connectid/"); marker >= 0 {
			activityID := strings.TrimPrefix(endpoint.Path[marker:], "/by/connectid/")
			referer = c.webURL.JoinPath("app/activity", activityID)
		}
		req.Header.Set("Referer", referer.String())
		req.Header.Set("DNT", "1")
		req.Header.Set("Priority", "u=4")
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Sec-GPC", "1")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:154.0) Gecko/20100101 Firefox/154.0")
		if !hasBrowserCookie(browserCookies, "SESSIONID") {
			sessionID, err := newGarminSessionID()
			if err != nil {
				return nil, err
			}
			req.AddCookie(&http.Cookie{Name: "SESSIONID", Value: sessionID})
		}
	}
	response, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Garmin request: %w", err)
	}
	return response, nil
}

func newGarminSessionID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate Garmin session ID: %w", err)
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	uuid := fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
	return base64.RawStdEncoding.EncodeToString([]byte(uuid)), nil
}

// HTTPError describes an unsuccessful response from Garmin Connect.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("Garmin API returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("Garmin API returned HTTP %d: %s", e.StatusCode, e.Body)
}

func newHTTPError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	return &HTTPError{StatusCode: response.StatusCode, Body: string(bytes.TrimSpace(body))}
}
