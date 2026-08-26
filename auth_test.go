package garmin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testLoginOptions(server *httptest.Server) []Option {
	return []Option{
		WithBaseURL(server.URL),
		WithHTTPClient(server.Client()),
		func(c *Client) error {
			base, _ := url.Parse(server.URL)
			c.ssoURL = base
			c.tokenURL = base.JoinPath("di-oauth2-service/oauth/token")
			c.serviceURL = server.URL + "/gcm/ios"
			return nil
		},
	}
}

func TestLogin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mobile/api/login":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["username"] != "runner@example.com" || body["password"] != "correct" {
				t.Fatal("incorrect credentials payload")
			}
			_, _ = w.Write([]byte(`{"responseStatus":{"type":"SUCCESSFUL"},"serviceTicketId":"ticket"}`))
		case "/di-oauth2-service/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("service_ticket") != "ticket" {
				t.Fatal("missing ticket")
			}
			_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"refresh"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := Login(context.Background(), "runner@example.com", "correct", nil, testLoginOptions(server)...)
	if err != nil || client.accessToken != "access" || client.refreshToken != "refresh" {
		t.Fatalf("Login() = %#v, %v", client, err)
	}
}

func TestLoginMFA(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mobile/api/login":
			_, _ = w.Write([]byte(`{"responseStatus":{"type":"MFA_REQUIRED"}}`))
		case "/mobile/api/mfa/verifyCode":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["mfaCode"] != "123456" {
				t.Fatal("bad MFA code")
			}
			_, _ = w.Write([]byte(`{"responseStatus":{"type":"SUCCESSFUL"},"serviceTicketId":"ticket"}`))
		case "/di-oauth2-service/oauth/token":
			_, _ = w.Write([]byte(`{"access_token":"access"}`))
		}
	}))
	defer server.Close()
	client, err := Login(context.Background(), "runner@example.com", "correct", func(context.Context) (string, error) { return "123456", nil }, testLoginOptions(server)...)
	if err != nil || client.accessToken != "access" {
		t.Fatalf("Login() = %#v, %v", client, err)
	}
}

func TestLoginErrors(t *testing.T) {
	if _, err := Login(context.Background(), "", "password", nil); err == nil {
		t.Fatal("expected validation error")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"responseStatus":{"type":"MFA_REQUIRED"}}`))
	}))
	defer server.Close()
	_, err := Login(context.Background(), "runner@example.com", "password", nil, testLoginOptions(server)...)
	if !errors.Is(err, ErrMFARequired) {
		t.Fatalf("error = %v", err)
	}
	_, err = Login(context.Background(), "runner@example.com", "password", func(context.Context) (string, error) { return "", nil }, testLoginOptions(server)...)
	if err == nil || !strings.Contains(err.Error(), "MFA code") {
		t.Fatalf("error = %v", err)
	}
}
