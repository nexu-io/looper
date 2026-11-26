package collab

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/eventlog"
)

type humanObservation struct {
	ChatID        string `json:"chatId"`
	RootID        string `json:"rootId"`
	ThreadID      string `json:"threadId"`
	MessageID     string `json:"messageId"`
	ParticipantID string `json:"participantId"`
	At            string `json:"at"`
	MessageURL    string `json:"messageUrl"`
}

func (h *bridgeHost) humanEvent(w http.ResponseWriter, r *http.Request) {
	var event humanObservation
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		http.Error(w, "invalid human metadata", 400)
		return
	}
	if err := h.recordHuman(r.Context(), event); err != nil {
		http.Error(w, "human metadata could not be recorded", 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (h *bridgeHost) recordHuman(ctx context.Context, event humanObservation) error {
	at, err := time.Parse(time.RFC3339Nano, event.At)
	if err != nil || !strings.HasPrefix(event.MessageID, "om_") || !strings.HasPrefix(event.ParticipantID, "ou_") {
		return fmt.Errorf("invalid human source metadata")
	}
	if event.MessageURL != "" {
		u, err := url.Parse(event.MessageURL)
		if err != nil || u.Scheme != "https" || u.Host != "applink.feishu.cn" || u.User != nil {
			return fmt.Errorf("invalid message link")
		}
	}
	var loopID, projectID string
	err = h.db.QueryRowContext(ctx, `SELECT l.id,l.project_id FROM loops l JOIN feishu_threads f ON f.loop_id=l.id
		WHERE f.chat_id=? AND json_extract(l.metadata_json,'$.bridge') IS NOT NULL
		AND ((?!='' AND f.root_message_id=?) OR (?!='' AND json_extract(l.metadata_json,'$.bridge.threadId')=?)) LIMIT 1`,
		event.ChatID, event.RootID, event.RootID, event.ThreadID, event.ThreadID).Scan(&loopID, &projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return eventlog.Track(ctx, h.repos, eventlog.AppendInput{
		ID: "human.feishu:" + loopID + ":" + event.MessageID, EventType: "human.collaboration", LoopID: &loopID, ProjectID: &projectID, CreatedAt: at,
		Payload: map[string]any{"human": map[string]string{"source": "feishu", "participantId": event.ParticipantID, "kind": "discussion", "messageId": event.MessageID, "messageUrl": event.MessageURL}},
	})
}
