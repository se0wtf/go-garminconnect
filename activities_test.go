package garmin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

func TestListActivities(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/activitylist-service/activities/search/activities"; got != want {
			t.Fatalf("path = %q", got)
		}
		if got, want := r.URL.Query().Get("start"), "4"; got != want {
			t.Errorf("start = %q", got)
		}
		if got, want := r.URL.Query().Get("limit"), "2"; got != want {
			t.Errorf("limit = %q", got)
		}
		if got, want := r.URL.Query().Get("activityType"), "running"; got != want {
			t.Errorf("type = %q", got)
		}
		_, _ = w.Write([]byte(`[{"activityId":7,"activityName":"Morning run","distance":1000,"activityTypeDTO":{"typeId":1,"typeKey":"running"}}]`))
	})
	activities, err := client.ListActivities(context.Background(), ListOptions{Start: 4, Limit: 2, ActivityType: " running "})
	if err != nil || len(activities) != 1 || activities[0].ID != 7 || activities[0].ActivityType.Key != "running" {
		t.Fatalf("ListActivities() = %#v, %v", activities, err)
	}
}

func TestListActivitiesDefaultAndValidation(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != "20" {
			t.Errorf("limit = %q", got)
		}
		_, _ = w.Write([]byte(`[]`))
	})
	if _, err := client.ListActivities(context.Background(), ListOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, options := range []ListOptions{{Start: -1}, {Limit: -1}, {Limit: 1001}} {
		if _, err := client.ListActivities(context.Background(), options); err == nil {
			t.Errorf("ListActivities(%+v) error = nil", options)
		}
	}
}

func TestActivitiesPagination(t *testing.T) {
	calls := 0
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		if start == 0 {
			_, _ = w.Write([]byte(`[{"activityId":1},{"activityId":2}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"activityId":3}]`))
	})
	activities, err := client.Activities(context.Background(), ListOptions{Limit: 2})
	if err != nil || len(activities) != 3 || calls != 2 {
		t.Fatalf("Activities() = %#v, calls=%d, err=%v", activities, calls, err)
	}
}

func TestActivityMethods(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activitylist-service/activities/count":
			_, _ = w.Write([]byte(`{"totalCount":12}`))
		case "/activity-service/activity/4":
			_, _ = w.Write([]byte(`{"activityId":4}`))
		case "/activity-service/activity/4/details":
			if r.URL.Query().Get("maxChartSize") != "12" || r.URL.Query().Get("maxPolylineSize") != "13" {
				t.Error("missing detail options")
			}
			_, _ = w.Write([]byte(`{"activityId":4,"activityDetailMetrics":[]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})
	count, err := client.ActivityCount(context.Background())
	if err != nil || count != 12 {
		t.Fatalf("ActivityCount() = %d, %v", count, err)
	}
	activity, err := client.GetActivity(context.Background(), 4)
	if err != nil || !json.Valid(activity) {
		t.Fatalf("GetActivity() = %s, %v", activity, err)
	}
	details, err := client.GetActivityDetails(context.Background(), 4, DetailOptions{MaxChartSize: 12, MaxPolylineSize: 13})
	if err != nil || !json.Valid(details) {
		t.Fatalf("GetActivityDetails() = %s, %v", details, err)
	}
}

func TestActivityMethodsValidation(t *testing.T) {
	client, _ := NewClient("token")
	for _, call := range []func() error{
		func() error { _, err := client.GetActivity(context.Background(), 0); return err },
		func() error {
			_, err := client.GetActivityDetails(context.Background(), 1, DetailOptions{MaxChartSize: -1})
			return err
		},
		func() error { _, err := client.DownloadActivity(context.Background(), 0, GPX); return err },
		func() error { _, err := client.DownloadActivity(context.Background(), 1, "pdf"); return err },
	} {
		if err := call(); err == nil {
			t.Error("expected validation error")
		}
	}
}
