package garmin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestGetDiveDetails(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/gcsalt-api/diving/v1/dive/detail/by/connectid/42"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("authorization = %q, want empty for JWT_WEB", got)
		}
		if r.Header.Get("Connect-Csrf-Token") != "csrf-value" || r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			t.Fatal("missing Garmin API headers")
		}
		if got, want := r.Header.Get("Referer"), clientURL(r)+"/app/activity/42"; got != want {
			t.Fatalf("referer = %q, want %q", got, want)
		}
		for name, value := range map[string]string{"JWT_WEB": "web-token", "session": "browser-session"} {
			if cookie, err := r.Cookie(name); err != nil || cookie.Value != value {
				t.Fatalf("missing Garmin browser cookie %s", name)
			}
		}
		sessionID, err := r.Cookie("SESSIONID")
		if err != nil {
			t.Fatal("missing generated SESSIONID")
		}
		decoded, err := base64.RawStdEncoding.DecodeString(sessionID.Value)
		if err != nil || len(decoded) != 36 {
			t.Fatalf("invalid generated SESSIONID %q", sessionID.Value)
		}
		_, _ = w.Write([]byte(`{"diveId":42,"maxDepth":25.4}`))
	})
	client.webURL = client.baseURL
	client.csrfToken = "csrf-value"
	client.browserCookies = []BrowserCookie{{Name: "JWT_WEB", Value: "web-token"}, {Name: "session", Value: "browser-session"}}
	if err := client.restoreBrowserCookies(); err != nil {
		t.Fatal(err)
	}
	details, err := client.GetDiveDetails(context.Background(), 42)
	if err != nil || !json.Valid(details) {
		t.Fatalf("GetDiveDetails() = %s, %v", details, err)
	}
}

func clientURL(r *http.Request) string {
	return "http://" + r.Host
}

func TestGetDiveDetailsValidation(t *testing.T) {
	client, _ := NewClient("token")
	if _, err := client.GetDiveDetails(context.Background(), 0); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestGetDiveDetailsRequiresBrowserSession(t *testing.T) {
	client, _ := NewClient("token")
	if _, err := client.GetDiveDetails(context.Background(), 42); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("error = %v, want ErrSessionExpired", err)
	}
}

func TestGetDiveDetailsExpiredBrowserSession(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "expired", http.StatusForbidden)
	})
	client.webURL = client.baseURL
	client.csrfToken = "csrf-value"
	client.browserCookies = []BrowserCookie{{Name: "session", Value: "expired"}}
	if err := client.restoreBrowserCookies(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetDiveDetails(context.Background(), 42); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("error = %v, want ErrSessionExpired", err)
	}
}
