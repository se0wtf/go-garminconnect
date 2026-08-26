package garmin

import (
	"context"
	"os"
	"testing"
)

// TestGarminIntegration exercises the live, read-only activity API. It is
// skipped unless GARMIN_EMAIL and GARMIN_PASSWORD are deliberately supplied.
func TestGarminIntegration(t *testing.T) {
	email, password := os.Getenv("GARMIN_EMAIL"), os.Getenv("GARMIN_PASSWORD")
	if email == "" || password == "" {
		t.Skip("set GARMIN_EMAIL and GARMIN_PASSWORD to run the live integration test")
	}
	provider := func(context.Context) (string, error) { return os.Getenv("GARMIN_MFA_CODE"), nil }
	client, err := Login(context.Background(), email, password, provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ActivityCount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListActivities(context.Background(), ListOptions{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	dives, err := client.ListActivities(context.Background(), ListOptions{Limit: 1, ActivityType: "diving"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dives) == 0 {
		t.Log("account contains no dives; skipping live dive-detail request")
		return
	}
	if _, err := client.GetDiveDetails(context.Background(), dives[0].ID); err != nil {
		t.Fatal(err)
	}
}
