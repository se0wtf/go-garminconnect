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

// Login authenticates with Garmin's mobile SSO flow and returns an activity
// client. Garmin does not document this API, so it may stop working without
// notice. When MFA is enabled, provider is called after Garmin requests a code.
func Login(ctx context.Context, email, password string, provider MFAProvider, options ...Option) (*Client, error) {
	if strings.TrimSpace(email) == "" || password == "" {
		return nil, errors.New("email and password must not be empty")
	}
	c, err := newUnauthenticatedClient(options...)
	if err != nil {
		return nil, err
	}
	ticket, err := c.mobileLogin(ctx, email, password)
	if isBrowserFallbackError(err) {
		ticket, err = c.portalLogin(ctx, email, password)
	}
	if errors.Is(err, ErrMFARequired) {
		if provider == nil {
			return nil, ErrMFARequired
		}
		code, providerErr := provider(ctx)
		if providerErr != nil {
			return nil, fmt.Errorf("get MFA code: %w", providerErr)
		}
		if strings.TrimSpace(code) == "" {
			return nil, errors.New("MFA code must not be empty")
		}
		ticket, err = c.verifyMFA(ctx, code)
	}
	if err != nil {
		return nil, err
	}
	if err := c.exchangeTicket(ctx, ticket); err != nil {
		return nil, err
	}
	return c, nil
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
		portalURL:  "https://connect.garmin.com/app",
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

func isBrowserFallbackError(err error) bool {
	var httpError *HTTPError
	return errors.As(err, &httpError) && (httpError.StatusCode == http.StatusTooManyRequests || httpError.StatusCode == http.StatusForbidden)
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
