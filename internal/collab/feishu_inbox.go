package collab

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/feishubridge"
)

func sharedFeishuInbox(cfg config.Config) (endpoint, token string, err error) {
	urlEnv, tokenEnv := "LOOPER_FEISHU_INBOX_URL", "LOOPER_FEISHU_INBOX_TOKEN"
	if fs := cfg.HITL.Feishu; fs != nil {
		if fs.EventInboxURLEnv != "" {
			urlEnv = fs.EventInboxURLEnv
		}
		if fs.EventInboxTokenEnv != "" {
			tokenEnv = fs.EventInboxTokenEnv
		}
	}
	endpoint, token = strings.TrimSpace(os.Getenv(urlEnv)), strings.TrimSpace(os.Getenv(tokenEnv))
	if endpoint == "" && token == "" {
		if fs := cfg.HITL.Feishu; fs != nil && (fs.EventInboxURLEnv != "" || fs.EventInboxTokenEnv != "") {
			return "", "", fmt.Errorf("shared Feishu inbox needs environment %s and %s", urlEnv, tokenEnv)
		}
		return "", "", nil // Existing single-instance WebSocket mode.
	}
	u, parseErr := url.Parse(endpoint)
	if parseErr != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))) {
		return "", "", fmt.Errorf("shared Feishu inbox requires HTTPS (HTTP is allowed only on localhost)")
	}
	if token == "" {
		return "", "", fmt.Errorf("shared Feishu inbox needs environment %s", tokenEnv)
	}
	return endpoint, token, nil
}

// Ownership comes from the task's existing chat/root/native-thread records, not
// from the sender's role or a new device-routing registry. Shared groups work
// because a reply belongs to one task topic, even when several hosts use it.
func (h *bridgeHost) ownsInboxEvent(ctx context.Context, event feishubridge.InboxEvent) (bool, error) {
	if event.ChatID == "" || (event.RootID == "" && event.ThreadID == "") {
		return false, nil
	}
	var id string
	err := h.db.QueryRowContext(ctx, `SELECT l.id FROM loops l
		JOIN feishu_threads f ON f.loop_id=l.id
		WHERE f.chat_id=? AND l.status!='terminated' AND json_extract(l.metadata_json,'$.bridge') IS NOT NULL
		AND ((?!='' AND f.root_message_id=?) OR (?!='' AND json_extract(l.metadata_json,'$.bridge.threadId')=?)) LIMIT 1`,
		event.ChatID, event.RootID, event.RootID, event.ThreadID, event.ThreadID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (h *bridgeHost) deliverInboxEvent(ctx context.Context, appID string, event feishubridge.InboxEvent) error {
	// Legacy inbox rows cannot reproduce SDK events; they remain readable by old
	// consumers but must never be fabricated as fresh messages on the new bridge.
	if len(event.Raw) == 0 || string(event.Raw) == "null" {
		return nil
	}
	var envelope struct {
		Header struct {
			AppID string `json:"app_id"`
		} `json:"header"`
	}
	if err := json.Unmarshal(event.Raw, &envelope); err != nil {
		return fmt.Errorf("invalid raw Feishu inbox event %d", event.ID)
	}
	if envelope.Header.AppID != appID {
		return nil
	}
	if event.Kind == "message" {
		owned, err := h.ownsInboxEvent(ctx, event)
		if err != nil || !owned {
			return err
		}
	} else if event.Kind != "card_action" && event.Kind != "reaction" {
		return nil
	}
	// Card/reaction events identify an individual card, not the task root.
	// Upstream resolves that card's real topic and checks its local session.
	return h.client.Request(ctx, http.MethodPost, "/inbound", event.Raw)
}

func (h *bridgeHost) maintainFeishuInbox(ctx context.Context, endpoint, token, appID string) {
	key := "feishu_inbox_cursor:" + appID + ":" + endpoint
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var lastWarning time.Time
	for ctx.Err() == nil {
		since, err := h.repos.Counters.Get(ctx, key)
		if err == nil {
			var next int64
			next, err = feishubridge.PollInbox(ctx, endpoint, token, since, func(event feishubridge.InboxEvent) error {
				return h.deliverInboxEvent(ctx, appID, event)
			})
			if next > since {
				err = errors.Join(err, h.repos.Counters.SetMax(ctx, key, next))
			}
		}
		if err != nil && ctx.Err() == nil && time.Since(lastWarning) >= 30*time.Second {
			h.warn("shared Feishu inbox poll failed; cursor retained for retry", err)
			lastWarning = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
