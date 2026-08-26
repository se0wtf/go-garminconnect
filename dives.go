package garmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// GetDiveDetails returns Garmin Dive's full, undocumented detail document for
// an activity. The raw JSON preserves fields Garmin may add without notice.
func (c *Client) GetDiveDetails(ctx context.Context, activityID int64) (json.RawMessage, error) {
	if activityID <= 0 {
		return nil, errors.New("activity ID must be positive")
	}
	var result json.RawMessage
	path := fmt.Sprintf("gcsalt-api/diving/v1/dive/detail/by/connectid/%d", activityID)
	if err := c.getFrom(ctx, c.webURL, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}
