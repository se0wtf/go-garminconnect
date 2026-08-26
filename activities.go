package garmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const activitiesPath = "activitylist-service/activities/search/activities"

// Activity is the stable subset of fields returned by the activity list API.
// Garmin can add fields without notice; use GetActivityDetails for the full
// response when a field is not represented here.
type Activity struct {
	ID              int64        `json:"activityId"`
	Name            string       `json:"activityName"`
	Description     string       `json:"description"`
	StartTimeLocal  string       `json:"startTimeLocal"`
	StartTimeGMT    string       `json:"startTimeGMT"`
	ActivityType    ActivityType `json:"activityType"`
	DistanceMeters  float64      `json:"distance"`
	DurationSeconds float64      `json:"duration"`
	MovingSeconds   float64      `json:"movingDuration"`
	Calories        float64      `json:"calories"`
	AverageHR       float64      `json:"averageHR"`
	MaxHR           float64      `json:"maxHR"`
}

// ActivityType identifies the Garmin sport associated with an activity.
type ActivityType struct {
	ID        int64  `json:"typeId"`
	Key       string `json:"typeKey"`
	ParentID  int64  `json:"parentTypeId"`
	ParentKey string `json:"parentTypeKey"`
}

// ListOptions controls activity-list pagination and filtering.
type ListOptions struct {
	Start        int
	Limit        int
	ActivityType string
}

func (o ListOptions) validate() error {
	if o.Start < 0 {
		return errors.New("start must not be negative")
	}
	if o.Limit < 0 || o.Limit > maxListLimit {
		return fmt.Errorf("limit must be between 0 and %d", maxListLimit)
	}
	return nil
}

func (o ListOptions) normalized() ListOptions {
	if o.Limit == 0 {
		o.Limit = 20
	}
	return o
}

// ListActivities returns one page of activities, newest first.
func (c *Client) ListActivities(ctx context.Context, options ListOptions) ([]Activity, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}
	options = options.normalized()
	query := url.Values{
		"start": {strconv.Itoa(options.Start)},
		"limit": {strconv.Itoa(options.Limit)},
	}
	if activityType := strings.TrimSpace(options.ActivityType); activityType != "" {
		query.Set("activityType", activityType)
	}
	var activities []Activity
	if err := c.get(ctx, activitiesPath, query, &activities); err != nil {
		return nil, err
	}
	return activities, nil
}

// Activities returns all activities matching options. Limit is the page size;
// use a modest value when retrieving a large history.
func (c *Client) Activities(ctx context.Context, options ListOptions) ([]Activity, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}
	options = options.normalized()
	var all []Activity
	for {
		page, err := c.ListActivities(ctx, options)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < options.Limit {
			return all, nil
		}
		options.Start += len(page)
	}
}

// ActivityCount returns the number of activities in the account.
func (c *Client) ActivityCount(ctx context.Context) (int, error) {
	var result struct {
		TotalCount int `json:"totalCount"`
	}
	if err := c.get(ctx, "activitylist-service/activities/count", nil, &result); err != nil {
		return 0, err
	}
	return result.TotalCount, nil
}

// GetActivity returns Garmin's full summary document for one activity.
func (c *Client) GetActivity(ctx context.Context, id int64) (json.RawMessage, error) {
	return c.getActivityJSON(ctx, id, "")
}

// DetailOptions controls the size of graph and route data in an activity
// detail response. Zero values select Garmin's usual values.
type DetailOptions struct {
	MaxChartSize    int
	MaxPolylineSize int
}

// GetActivityDetails returns Garmin's full, undocumented detail document.
func (c *Client) GetActivityDetails(ctx context.Context, id int64, options DetailOptions) (json.RawMessage, error) {
	if id <= 0 {
		return nil, errors.New("activity ID must be positive")
	}
	if options.MaxChartSize < 0 || options.MaxPolylineSize < 0 {
		return nil, errors.New("detail sizes must not be negative")
	}
	query := url.Values{}
	if options.MaxChartSize != 0 {
		query.Set("maxChartSize", strconv.Itoa(options.MaxChartSize))
	}
	if options.MaxPolylineSize != 0 {
		query.Set("maxPolylineSize", strconv.Itoa(options.MaxPolylineSize))
	}
	var result json.RawMessage
	if err := c.get(ctx, fmt.Sprintf("activity-service/activity/%d/details", id), query, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) getActivityJSON(ctx context.Context, id int64, suffix string) (json.RawMessage, error) {
	if id <= 0 {
		return nil, errors.New("activity ID must be positive")
	}
	var result json.RawMessage
	if err := c.get(ctx, fmt.Sprintf("activity-service/activity/%d%s", id, suffix), nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// DownloadFormat is the exported representation of an activity.
type DownloadFormat string

const (
	Original DownloadFormat = "original"
	TCX      DownloadFormat = "tcx"
	GPX      DownloadFormat = "gpx"
	KML      DownloadFormat = "kml"
	CSV      DownloadFormat = "csv"
)

// DownloadActivity downloads an activity in format. Original is the original
// FIT archive returned by Garmin; callers are responsible for extracting it.
func (c *Client) DownloadActivity(ctx context.Context, id int64, format DownloadFormat) ([]byte, error) {
	if id <= 0 {
		return nil, errors.New("activity ID must be positive")
	}
	var path string
	switch format {
	case Original:
		path = fmt.Sprintf("download-service/files/activity/%d", id)
	case TCX, GPX, KML, CSV:
		path = fmt.Sprintf("download-service/export/%s/activity/%d", format, id)
	default:
		return nil, fmt.Errorf("unsupported download format %q", format)
	}
	return c.download(ctx, path)
}
