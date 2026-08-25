// Package garmin provides a small client for the undocumented Garmin Connect
// activity API.
//
// The API is not affiliated with Garmin and may change without notice.
package garmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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
	baseURL     *url.URL
	httpClient  *http.Client
	accessToken string
	ssoURL      *url.URL
	tokenURL    *url.URL
	serviceURL  string
}

// Option configures a Client.
type Option func(*Client) error

// WithBaseURL overrides the API base URL. It is useful for tests and for
// Garmin regional endpoints.
func WithBaseURL(rawURL string) Option {
	return func(c *Client) error {
		u, err := url.Parse(rawURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("invalid base URL %q", rawURL)
		}
		c.baseURL = u
		return nil
	}
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
		httpClient:  newDefaultHTTPClient(),
		accessToken: accessToken,
		ssoURL:      mustParseURL("https://sso.garmin.com"),
		tokenURL:    mustParseURL("https://diauth.garmin.com/di-oauth2-service/oauth/token"),
		serviceURL:  "https://mobile.integration.garmin.com/gcm/ios",
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
	u := c.baseURL.JoinPath(path)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("User-Agent", "go-garmin")

	response, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Garmin request: %w", err)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("User-Agent", "go-garmin")
	response, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Garmin request: %w", err)
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
