package feishubridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// InboxEvent retains the old inbox envelope while forwarding the complete
// Feishu event to the upstream SDK (including mentions, media and message ID).
type InboxEvent struct {
	ID       int64           `json:"id"`
	Kind     string          `json:"kind"`
	ChatID   string          `json:"chatId"`
	RootID   string          `json:"rootId"`
	ThreadID string          `json:"threadId"`
	Raw      json.RawMessage `json:"raw"`
}

// PollInbox reads without consuming globally. The returned cursor advances only
// over events the local consumer accepted or explicitly ignored. A failure is
// retried on the next poll, rather than silently losing that human reply.
func PollInbox(ctx context.Context, endpoint, token string, since int64, consume func(InboxEvent) error) (int64, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return since, fmt.Errorf("invalid Feishu inbox URL")
	}
	query := u.Query()
	query.Set("since", strconv.FormatInt(since, 10))
	query.Set("limit", "200")
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return since, fmt.Errorf("invalid Feishu inbox request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return since, fmt.Errorf("Feishu inbox request failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return since, fmt.Errorf("Feishu inbox HTTP %d", res.StatusCode)
	}
	var page struct {
		OK     bool         `json:"ok"`
		Events []InboxEvent `json:"events"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&page); err != nil {
		return since, fmt.Errorf("decode Feishu inbox: %w", err)
	}
	if !page.OK {
		return since, fmt.Errorf("Feishu inbox rejected read")
	}
	for _, event := range page.Events {
		if event.ID <= since {
			continue
		}
		if err := consume(event); err != nil {
			return since, err
		}
		since = event.ID
	}
	return since, nil
}
