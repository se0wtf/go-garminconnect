package garmin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	iosClientID  = "GCM_IOS_DARK"
	iosUserAgent = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148"
	diGrantType  = "https://connectapi.garmin.com/di-oauth2-service/oauth/grant/service_ticket"
	diClientID   = "GARMIN_CONNECT_MOBILE_ANDROID_DI_2025Q2"
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
	c := &Client{
		baseURL:    baseURL,
		webURL:     mustParseURL("https://connect.garmin.com"),
		httpClient: newDefaultHTTPClient(),
		ssoURL:     mustParseURL("https://sso.garmin.com"),
		tokenURL:   mustParseURL("https://diauth.garmin.com/di-oauth2-service/oauth/token"),
		serviceURL: "https://mobile.integration.garmin.com/gcm/ios",
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

func (c *Client) mobileLogin(ctx context.Context, email, password string) (string, error) {
	response := struct {
		ResponseStatus struct {
			Type string `json:"type"`
		} `json:"responseStatus"`
		ServiceTicketID string `json:"serviceTicketId"`
	}{}
	query := url.Values{"clientId": {iosClientID}, "locale": {"en-US"}, "service": {c.serviceURL}}
	if err := c.postJSON(ctx, c.ssoURL.JoinPath("mobile/api/login"), query, map[string]any{
		"username": email, "password": password, "rememberMe": true, "captchaToken": "",
	}, map[string]string{"Origin": c.ssoURL.String(), "User-Agent": iosUserAgent}, &response); err != nil {
		return "", err
	}
	switch response.ResponseStatus.Type {
	case "SUCCESSFUL":
		if response.ServiceTicketID == "" {
			return "", errors.New("Garmin login response has no service ticket")
		}
		return response.ServiceTicketID, nil
	case "INVALID_USERNAME_PASSWORD":
		return "", ErrInvalidCredentials
	case "MFA_REQUIRED":
		return "", ErrMFARequired
	default:
		return "", fmt.Errorf("Garmin login failed: %s", response.ResponseStatus.Type)
	}
}

func (c *Client) verifyMFA(ctx context.Context, code string) (string, error) {
	response := struct {
		ResponseStatus struct {
			Type string `json:"type"`
		} `json:"responseStatus"`
		ServiceTicketID string `json:"serviceTicketId"`
	}{}
	query := url.Values{"clientId": {iosClientID}, "locale": {"en-US"}, "service": {c.serviceURL}}
	if err := c.postJSON(ctx, c.ssoURL.JoinPath("mobile/api/mfa/verifyCode"), query, map[string]string{"mfaCode": code}, nil, &response); err != nil {
		return "", err
	}
	if response.ResponseStatus.Type != "SUCCESSFUL" || response.ServiceTicketID == "" {
		return "", ErrInvalidCredentials
	}
	return response.ServiceTicketID, nil
}

func (c *Client) exchangeTicket(ctx context.Context, ticket string) error {
	result, err := c.requestToken(ctx, diClientID, map[string]string{
		"client_id":      diClientID,
		"service_ticket": ticket,
		"grant_type":     diGrantType,
		"service_url":    c.serviceURL,
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
