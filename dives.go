package garmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// GetDiveDetails returns Garmin Dive's full, undocumented detail document for
// an activity. The raw JSON preserves fields Garmin may add without notice.
func (c *Client) GetDiveDetails(ctx context.Context, activityID int64) (json.RawMessage, error) {
	if activityID <= 0 {
		return nil, errors.New("activity ID must be positive")
	}
	if err := c.ensureBrowserSession(); err != nil {
		return nil, err
	}
	var result json.RawMessage
	path := fmt.Sprintf("gcsalt-api/diving/v1/dive/detail/by/connectid/%d", activityID)
	if err := c.getFrom(ctx, c.webURL, path, nil, &result); err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden) {
			return nil, fmt.Errorf("%w: %v", ErrSessionExpired, err)
		}
		return nil, err
	}
	return result, nil
}

func (c *Client) ensureBrowserSession() error {
	c.tokenMu.Lock()
	ready := c.csrfToken != "" && len(c.browserCookies) != 0
	c.tokenMu.Unlock()
	if !ready {
		return ErrSessionExpired
	}
	return nil
}
