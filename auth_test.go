package garmin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testLoginOptions(server *httptest.Server) []Option {
	httpClient := server.Client()
	httpClient.Jar, _ = cookiejar.New(nil)
	return []Option{
		WithBaseURL(server.URL),
		WithHTTPClient(httpClient),
		func(c *Client) error {
			base, _ := url.Parse(server.URL)
			c.ssoURL = base
			c.tokenURL = base.JoinPath("di-oauth2-service/oauth/token")
			c.serviceURL = server.URL + "/gcm/ios"
			c.portalURL = server.URL + "/app"
			c.loginDelay = func(context.Context) error { return nil }
			return nil
		},
	}
}

func TestLoginFallsBackToBrowserFlow(t *testing.T) {
	var portalGets, portalLogins, portalVerifications int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mobile/api/login":
			http.Error(w, `{"error":{"status-code":"429"}}`, http.StatusTooManyRequests)
		case "/portal/sso/en-US/sign-in":
			portalGets++
			if r.Method != http.MethodGet || r.URL.Query().Get("clientId") != portalClientID || r.Header.Get("User-Agent") != portalUserAgent {
				t.Fatal("bad browser sign-in request")
			}
			http.SetCookie(w, &http.Cookie{Name: "GARMIN-SSO", Value: "session", Path: "/"})
			_, _ = w.Write([]byte("sign in"))
		case "/portal/api/login":
			portalLogins++
			if cookie, err := r.Cookie("GARMIN-SSO"); err != nil || cookie.Value != "session" {
				t.Fatal("browser SSO cookie was not retained")
			}
			_, _ = w.Write([]byte(`{"responseStatus":{"type":"MFA_REQUIRED"},"customerMfaInfo":{"mfaLastMethodUsed":"email"}}`))
		case "/portal/api/mfa/verifyCode":
			portalVerifications++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["mfaVerificationCode"] != "654321" || body["mfaMethod"] != "email" {
				t.Fatal("bad browser MFA payload")
			}
			_, _ = w.Write([]byte(`{"responseStatus":{"type":"SUCCESSFUL"},"serviceTicketId":"portal-ticket"}`))
		case "/di-oauth2-service/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("service_ticket") != "portal-ticket" || r.Form.Get("service_url") != "http://"+r.Host+"/app" {
				t.Fatalf("bad portal token exchange: %#v", r.Form)
			}
			_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"refresh"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := Login(context.Background(), "runner@example.com", "correct", func(context.Context) (string, error) {
		return "654321", nil
	}, testLoginOptions(server)...)
	if err != nil || client.accessToken != "access" {
		t.Fatalf("Login() = %#v, %v", client, err)
	}
	if portalGets != 1 || portalLogins != 1 || portalVerifications != 1 {
		t.Fatalf("portal calls = GET %d login %d MFA %d", portalGets, portalLogins, portalVerifications)
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
			_, _ = w.Write([]byte(`{"responseStatus":{"type":"MFA_REQUIRED"},"customerMfaInfo":{"mfaLastMethodUsed":"email"}}`))
		case "/mobile/api/mfa/verifyCode":
			var body struct {
				Method            string   `json:"mfaMethod"`
				VerificationCode  string   `json:"mfaVerificationCode"`
				RememberMyBrowser bool     `json:"rememberMyBrowser"`
				ReconsentList     []string `json:"reconsentList"`
				Setup             bool     `json:"mfaSetup"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Method != "email" || body.VerificationCode != "123456" || !body.RememberMyBrowser || body.ReconsentList == nil || body.Setup {
				t.Fatalf("bad MFA payload: %#v", body)
			}
			testServerURL := "http://" + r.Host
			if r.Header.Get("Origin") != testServerURL || r.Header.Get("User-Agent") != iosUserAgent {
				t.Fatal("missing mobile SSO headers")
			}
			if r.URL.Query().Get("clientId") != iosClientID || r.URL.Query().Get("service") != testServerURL+"/gcm/ios" {
				t.Fatal("bad MFA query")
			}
			_, _ = w.Write([]byte(`{"responseStatus":{"type":"SUCCESSFUL"},"serviceTicketId":"ticket"}`))
		case "/di-oauth2-service/oauth/token":
			_, _ = w.Write([]byte(`{"access_token":"access"}`))
		}
	}))
	defer server.Close()
	client, err := Login(context.Background(), "runner@example.com", "correct", func(context.Context) (string, error) { return " 123456\n", nil }, testLoginOptions(server)...)
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
