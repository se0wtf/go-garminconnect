package garmin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

const (
	iosClientID     = "GCM_IOS_DARK"
	iosUserAgent    = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148"
	portalClientID  = "GarminConnect"
	portalUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	diGrantType     = "https://connectapi.garmin.com/di-oauth2-service/oauth/grant/service_ticket"
	diClientID      = "GARMIN_CONNECT_MOBILE_ANDROID_DI_2025Q2"
)

var (
	// ErrInvalidCredentials indicates that Garmin rejected the email or password.
	ErrInvalidCredentials = errors.New("Garmin rejected the credentials")
	// ErrMFARequired indicates that Garmin requires a one-time MFA code.
	ErrMFARequired = errors.New("Garmin requires MFA")
)

// MFAProvider returns an MFA code after Garmin requests it. Keep the code out
// of logs and source control.
type MFAProvider func(context.Context) (string, error)

// Login authenticates with Garmin's browser SSO flow and returns an activity
// client. It falls back to Garmin's mobile SSO flow when the browser endpoint
// is unavailable. Garmin does not document these APIs, so they may stop working
// without notice. When MFA is enabled, provider is called after Garmin requests
// a code.
func Login(ctx context.Context, email, password string, provider MFAProvider, options ...Option) (*Client, error) {
	if strings.TrimSpace(email) == "" || password == "" {
		return nil, errors.New("email and password must not be empty")
	}
	c, err := newUnauthenticatedClient(options...)
	if err != nil {
		return nil, err
	}
	ticket, err := c.portalLogin(ctx, email, password)
	if shouldFallBackFromPortal(ctx, err) {
		ticket, err = c.mobileLogin(ctx, email, password)
	}
	ticket, err = c.resolveMFA(ctx, ticket, err, provider)
	if err != nil {
		return nil, err
	}
	if c.authFlow == "portal" {
		if err := c.exchangeTicket(ctx, ticket); err != nil {
			return nil, err
		}
		webTicket, webErr := c.portalLogin(ctx, email, password)
		webTicket, webErr = c.resolveMFA(ctx, webTicket, webErr, provider)
		if webErr != nil {
			return nil, fmt.Errorf("establish Garmin Dive session: %w", webErr)
		}
		if err := c.establishPortalSession(ctx, webTicket); err != nil {
			return nil, err
		}
	} else if err := c.exchangeTicket(ctx, ticket); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) resolveMFA(ctx context.Context, ticket string, err error, provider MFAProvider) (string, error) {
	if !errors.Is(err, ErrMFARequired) {
		return ticket, err
	}
	if provider == nil {
		return "", ErrMFARequired
	}
	code, providerErr := provider(ctx)
	if providerErr != nil {
		return "", fmt.Errorf("get MFA code: %w", providerErr)
	}
	if strings.TrimSpace(code) == "" {
		return "", errors.New("MFA code must not be empty")
	}
	return c.verifyMFA(ctx, code)
}

func newUnauthenticatedClient(options ...Option) (*Client, error) {
	baseURL, _ := url.Parse(defaultBaseURL)
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create Garmin login cookie jar: %w", err)
	}
	c := &Client{
		baseURL:    baseURL,
		webURL:     mustParseURL("https://connect.garmin.com"),
		httpClient: &http.Client{Timeout: defaultTimeout, Jar: jar},
		ssoURL:     mustParseURL("https://sso.garmin.com"),
		tokenURL:   mustParseURL("https://diauth.garmin.com/di-oauth2-service/oauth/token"),
		serviceURL: "https://mobile.integration.garmin.com/gcm/ios",
		portalURL:  "https://connect.garmin.com/app/",
		loginDelay: browserLoginDelay,
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

type loginResponse struct {
	ResponseStatus struct {
		Type string `json:"type"`
	} `json:"responseStatus"`
	CustomerMFAInfo struct {
		LastMethodUsed string `json:"mfaLastMethodUsed"`
	} `json:"customerMfaInfo"`
	ServiceTicketID string `json:"serviceTicketId"`
	Error           struct {
		StatusCode string `json:"status-code"`
	} `json:"error"`
}

func (c *Client) mobileLogin(ctx context.Context, email, password string) (string, error) {
	var response loginResponse
	query := url.Values{"clientId": {iosClientID}, "locale": {"en-US"}, "service": {c.serviceURL}}
	if err := c.postJSON(ctx, c.ssoURL.JoinPath("mobile/api/login"), query, credentialPayload(email, password), c.mobileSSOHeaders(), &response); err != nil {
		return "", err
	}
	return c.completeLogin(response, "mobile", c.serviceURL)
}

func (c *Client) portalLogin(ctx context.Context, email, password string) (string, error) {
	query := url.Values{"clientId": {portalClientID}, "locale": {"en-US"}, "service": {c.portalURL}}
	signIn := c.ssoURL.JoinPath("portal/sso/en-US/sign-in")
	signIn.RawQuery = url.Values{"clientId": {portalClientID}, "service": {c.portalURL}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signIn.String(), nil)
	if err != nil {
		return "", fmt.Errorf("create Garmin browser login request: %w", err)
	}
	for key, value := range c.portalSSOHeaders(signIn.String()) {
		req.Header.Set(key, value)
	}
	response, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("start Garmin browser login: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		responseErr := newHTTPError(response)
		response.Body.Close()
		return "", responseErr
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if err := c.loginDelay(ctx); err != nil {
		return "", err
	}
	var result loginResponse
	if err := c.postJSON(ctx, c.ssoURL.JoinPath("portal/api/login"), query, credentialPayload(email, password), c.portalSSOHeaders(signIn.String()), &result); err != nil {
		return "", err
	}
	return c.completeLogin(result, "portal", c.portalURL)
}

func credentialPayload(email, password string) map[string]any {
	return map[string]any{"username": email, "password": password, "rememberMe": true, "captchaToken": ""}
}

func (c *Client) completeLogin(response loginResponse, flow, service string) (string, error) {
	if response.Error.StatusCode == "429" {
		return "", &HTTPError{StatusCode: http.StatusTooManyRequests}
	}
	switch response.ResponseStatus.Type {
	case "SUCCESSFUL":
		if response.ServiceTicketID == "" {
			return "", errors.New("Garmin login response has no service ticket")
		}
		c.authFlow, c.authService = flow, service
		return response.ServiceTicketID, nil
	case "INVALID_USERNAME_PASSWORD":
		return "", ErrInvalidCredentials
	case "MFA_REQUIRED":
		c.authFlow, c.authService = flow, service
		c.mfaMethod = response.CustomerMFAInfo.LastMethodUsed
		if c.mfaMethod == "" {
			c.mfaMethod = "email"
		}
		return "", ErrMFARequired
	default:
		return "", fmt.Errorf("Garmin login failed: %s", response.ResponseStatus.Type)
	}
}

func shouldFallBackFromPortal(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == nil && !errors.Is(err, ErrInvalidCredentials) && !errors.Is(err, ErrMFARequired)
}

func browserLoginDelay(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) verifyMFA(ctx context.Context, code string) (string, error) {
	response := struct {
		ResponseStatus struct {
			Type string `json:"type"`
		} `json:"responseStatus"`
		ServiceTicketID string `json:"serviceTicketId"`
	}{}
	flow, clientID, service := c.authFlow, iosClientID, c.serviceURL
	if flow == "portal" {
		clientID, service = portalClientID, c.portalURL
	}
	query := url.Values{"clientId": {clientID}, "locale": {"en-US"}, "service": {service}}
	headers := c.mobileSSOHeaders()
	if flow == "portal" {
		headers = c.portalSSOHeaders(c.ssoURL.JoinPath("portal/sso/en-US/sign-in").String())
	}
	if err := c.postJSON(ctx, c.ssoURL.JoinPath(flow+"/api/mfa/verifyCode"), query, map[string]any{
		"mfaMethod":           c.mfaMethod,
		"mfaVerificationCode": strings.TrimSpace(code),
		"rememberMyBrowser":   true,
		"reconsentList":       []string{},
		"mfaSetup":            false,
	}, headers, &response); err != nil {
		return "", err
	}
	if response.ResponseStatus.Type != "SUCCESSFUL" || response.ServiceTicketID == "" {
		return "", ErrInvalidCredentials
	}
	return response.ServiceTicketID, nil
}

func (c *Client) mobileSSOHeaders() map[string]string {
	return map[string]string{
		"Accept":     "application/json, text/plain, */*",
		"Origin":     c.ssoURL.String(),
		"User-Agent": iosUserAgent,
	}
}

func (c *Client) portalSSOHeaders(referer string) map[string]string {
	return map[string]string{
		"Accept":          "application/json, text/plain, */*",
		"Accept-Language": "en-US,en;q=0.9",
		"Origin":          c.ssoURL.String(),
		"Referer":         referer,
		"User-Agent":      portalUserAgent,
	}
}

func (c *Client) exchangeTicket(ctx context.Context, ticket string) error {
	serviceURL := c.authService
	if serviceURL == "" {
		serviceURL = c.serviceURL
	}
	result, err := c.requestToken(ctx, diClientID, map[string]string{
		"client_id":      diClientID,
		"service_ticket": ticket,
		"grant_type":     diGrantType,
		"service_url":    serviceURL,
	})
	if err != nil {
		return err
	}
	clientID := clientIDFromToken(result.AccessToken)
	if clientID == "" {
		clientID = diClientID
	}
	return c.setTokens(Tokens{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ClientID:     clientID,
	})
}

func (c *Client) establishPortalSession(ctx context.Context, ticket string) error {
	service, err := url.Parse(c.authService)
	if err != nil || service.Scheme == "" || service.Host == "" {
		return errors.New("Garmin portal authentication service is invalid")
	}
	service.RawQuery = url.Values{"ticket": {ticket}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, service.String(), nil)
	if err != nil {
		return fmt.Errorf("create Garmin web session request: %w", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("User-Agent", portalUserAgent)
	response, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("establish Garmin web session: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		responseErr := newHTTPError(response)
		response.Body.Close()
		return fmt.Errorf("establish Garmin web session: %w", responseErr)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	response.Body.Close()
	if err != nil {
		return fmt.Errorf("read Garmin web session: %w", err)
	}
	csrfToken := extractCSRFToken(string(body))
	if csrfToken == "" {
		return errors.New("Garmin web session has no CSRF token")
	}
	c.tokenMu.Lock()
	c.csrfToken = csrfToken
	c.tokenMu.Unlock()
	if !hasBrowserCookie(c.browserAuthenticationCookies(), "JWT_WEB") {
		return errors.New("Garmin web session has no JWT_WEB cookie")
	}
	refresh := c.webURL.JoinPath("services/auth/token/di-oauth/refresh")
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, refresh.String(), strings.NewReader(""))
	if err != nil {
		return fmt.Errorf("create Garmin web session refresh request: %w", err)
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Csrf-Token", csrfToken)
	req.Header.Set("Origin", c.webURL.String())
	req.Header.Set("Referer", c.webURL.JoinPath("app/").String())
	req.Header.Set("User-Agent", portalUserAgent)
	response, err = c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("refresh Garmin web session: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		responseErr := newHTTPError(response)
		response.Body.Close()
		return fmt.Errorf("refresh Garmin web session: %w", responseErr)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if err := c.reloadPortalSession(ctx); err != nil {
		return err
	}
	browserCookies := c.browserAuthenticationCookies()
	if len(browserCookies) == 0 {
		return errors.New("Garmin web session refresh returned no authentication cookies")
	}
	tokens := c.Tokens()
	tokens.BrowserCookies = browserCookies
	return c.setTokens(tokens)
}

func (c *Client) reloadPortalSession(ctx context.Context) error {
	page := c.webURL.JoinPath("app/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page.String(), nil)
	if err != nil {
		return fmt.Errorf("create Garmin web app request: %w", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("User-Agent", portalUserAgent)
	response, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("reload Garmin web session: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("reload Garmin web session: %w", newHTTPError(response))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("read Garmin web app: %w", err)
	}
	if csrfToken := extractCSRFToken(string(body)); csrfToken != "" {
		c.tokenMu.Lock()
		c.csrfToken = csrfToken
		c.tokenMu.Unlock()
	}
	return nil
}

func extractCSRFToken(page string) string {
	const prefix = `meta name="csrf-token" content="`
	start := strings.Index(page, prefix)
	if start < 0 {
		return ""
	}
	value := page[start+len(prefix):]
	end := strings.IndexByte(value, '"')
	if end < 0 {
		return ""
	}
	return value[:end]
}

func (c *Client) browserAuthenticationCookies() []BrowserCookie {
	if c.httpClient.Jar == nil {
		return nil
	}
	allowed := map[string]bool{"JWT_WEB": true, "SESSIONID": true, "session": true}
	var result []BrowserCookie
	for _, cookie := range c.httpClient.Jar.Cookies(c.webURL) {
		if allowed[cookie.Name] && cookie.Value != "" {
			result = append(result, BrowserCookie{Name: cookie.Name, Value: cookie.Value})
		}
	}
	return result
}

func hasBrowserCookie(cookies []BrowserCookie, name string) bool {
	for _, cookie := range cookies {
		if cookie.Name == name && cookie.Value != "" {
			return true
		}
	}
	return false
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func (c *Client) requestToken(ctx context.Context, clientID string, values map[string]string) (tokenResponse, error) {
	form := url.Values{}
	for key, value := range values {
		form.Set(key, value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(clientID+":")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("request Garmin token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return tokenResponse{}, newHTTPError(response)
	}
	var result tokenResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return tokenResponse{}, fmt.Errorf("decode Garmin token response: %w", err)
	}
	if result.AccessToken == "" {
		return tokenResponse{}, errors.New("Garmin token response has no access token")
	}
	return result, nil
}

func (c *Client) postJSON(ctx context.Context, endpoint *url.URL, query url.Values, input any, headers map[string]string, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode Garmin request: %w", err)
	}
	u := *endpoint
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Garmin request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return newHTTPError(response)
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return fmt.Errorf("decode Garmin response: %w", err)
	}
	return nil
}
