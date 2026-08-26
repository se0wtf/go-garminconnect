package garmin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient("secret", WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestNewClientValidation(t *testing.T) {
	for _, test := range []struct {
		name, token string
		option      Option
	}{
		{"empty token", "", nil}, {"nil HTTP client", "token", WithHTTPClient(nil)},
		{"bad base URL", "token", WithBaseURL("://")}, {"nil option", "token", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var options []Option
			if test.name == "nil option" {
				options = []Option{nil}
			} else if test.option != nil {
				options = []Option{test.option}
			}
			if _, err := NewClient(test.token, options...); err == nil {
				t.Fatal("NewClient() error = nil")
			}
		})
	}
}

func TestClientHTTPError(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusBadRequest) })
	_, err := client.ActivityCount(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest || httpErr.Body != "nope" {
		t.Fatalf("unexpected error: %#v", err)
	}
}

func TestDownloadActivity(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if got, want := r.URL.Path, "/download-service/export/gpx/activity/42"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		_, _ = io.WriteString(w, "track")
	})
	body, err := client.DownloadActivity(context.Background(), 42, GPX)
	if err != nil || string(body) != "track" {
		t.Fatalf("DownloadActivity() = %q, %v", body, err)
	}
}
