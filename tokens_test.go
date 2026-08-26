package garmin

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testJWT(expires time.Time, clientID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"client_id":%q}`, expires.Unix(), clientID)))
	return header + "." + payload + ".signature"
}

func TestSaveAndLoadTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "tokens.json")
	want := Tokens{AccessToken: "access", RefreshToken: "refresh", ClientID: "client"}
	if err := SaveTokens(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("LoadTokens() = %#v, want %#v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Fatalf("token mode = %o, want 600", gotMode)
	}
}

func TestLoadTokensErrors(t *testing.T) {
	if _, err := LoadTokens(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrNoTokenFile) {
		t.Fatalf("missing error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, []byte(`{"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTokens(path); err == nil {
		t.Fatal("expected invalid token file error")
	}
}

func TestAutomaticTokenRefreshAndPersistence(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "tokens.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("refresh_token") != "old-refresh" {
				t.Fatalf("refresh token = %q", r.Form.Get("refresh_token"))
			}
			_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh"}`))
		case "/activitylist-service/activities/count":
			if got := r.Header.Get("Authorization"); got != "Bearer new-access" {
				t.Fatalf("authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{"totalCount":2}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	tokenURL, _ := url.Parse(server.URL + "/token")
	client, err := NewClientWithTokens(Tokens{
		AccessToken:  testJWT(time.Now().Add(-time.Minute), "client"),
		RefreshToken: "old-refresh",
		ClientID:     "client",
	}, WithBaseURL(server.URL), WithHTTPClient(server.Client()), WithTokenFile(tokenPath), func(c *Client) error {
		c.tokenURL = tokenURL
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	count, err := client.ActivityCount(context.Background())
	if err != nil || count != 2 {
		t.Fatalf("ActivityCount() = %d, %v", count, err)
	}
	stored, err := LoadTokens(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "new-access" || stored.RefreshToken != "new-refresh" {
		t.Fatalf("stored tokens = %#v", stored)
	}
}

func TestUnauthorizedRefreshRetry(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"new-access"}`))
		case "/activitylist-service/activities/count":
			requests++
			if r.Header.Get("Authorization") == "Bearer old-access" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"totalCount":1}`))
		}
	}))
	defer server.Close()
	tokenURL, _ := url.Parse(server.URL + "/token")
	client, err := NewClientWithTokens(Tokens{AccessToken: "old-access", RefreshToken: "refresh", ClientID: "client"},
		WithBaseURL(server.URL), WithHTTPClient(server.Client()), func(c *Client) error { c.tokenURL = tokenURL; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ActivityCount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("API requests = %d, want 2", requests)
	}
}

func TestExpiredSession(t *testing.T) {
	client, err := NewClientWithTokens(Tokens{AccessToken: testJWT(time.Now().Add(-time.Hour), "client")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ActivityCount(context.Background()); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("error = %v, want ErrSessionExpired", err)
	}
}

func TestRefreshRateLimitIsNotExpiredSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()
	tokenURL, _ := url.Parse(server.URL)
	client, err := NewClientWithTokens(Tokens{
		AccessToken:  testJWT(time.Now().Add(-time.Hour), "client"),
		RefreshToken: "refresh",
		ClientID:     "client",
	}, WithHTTPClient(server.Client()), func(c *Client) error { c.tokenURL = tokenURL; return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ActivityCount(context.Background())
	if err == nil || errors.Is(err, ErrSessionExpired) {
		t.Fatalf("error = %v, want transient refresh error", err)
	}
}
