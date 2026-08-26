package garmin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestGetDiveDetails(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/gcsalt-api/diving/v1/dive/detail/by/connectid/42"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"diveId":42,"maxDepth":25.4}`))
	})
	client.webURL = client.baseURL
	details, err := client.GetDiveDetails(context.Background(), 42)
	if err != nil || !json.Valid(details) {
		t.Fatalf("GetDiveDetails() = %s, %v", details, err)
	}
}

func TestGetDiveDetailsValidation(t *testing.T) {
	client, _ := NewClient("token")
	if _, err := client.GetDiveDetails(context.Background(), 0); err == nil {
		t.Fatal("expected validation error")
	}
}
