package planestrict

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
)

type TaskLink struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Type  string `json:"type,omitempty"`
}

type TaskEvent struct {
	ID          string     `json:"id"`
	Type        string     `json:"type,omitempty"`
	Phase       string     `json:"phase,omitempty"`
	SourceID    string     `json:"source_id,omitempty"`
	Kind        string     `json:"kind,omitempty"`
	Title       string     `json:"title"`
	Body        string     `json:"body,omitempty"`
	ActorName   string     `json:"actor_name,omitempty"`
	ActorOpenID string     `json:"actor_open_id,omitempty"`
	SourceURL   string     `json:"source_url,omitempty"`
	OccurredAt  string     `json:"occurred_at,omitempty"`
	Links       []TaskLink `json:"links,omitempty"`
	Replaces    string     `json:"replaces,omitempty"`
}

type TaskContext struct {
	Summary      string      `json:"summary"`
	NextStep     string      `json:"next_step"`
	Context      string      `json:"context"`
	ThreadURL    string      `json:"thread_url,omitempty"`
	ChatURL      string      `json:"chat_url,omitempty"`
	ChatName     string      `json:"chat_name,omitempty"`
	ChatScopeKey string      `json:"chat_scope_key,omitempty"`
	Links        []TaskLink  `json:"links,omitempty"`
	Events       []TaskEvent `json:"events,omitempty"`
}

func (c *Client) PublishTaskContext(ctx context.Context, dispatch Dispatch, input TaskContext) (TaskContext, error) {
	if dispatch.ExecutionAttemptID == nil {
		return TaskContext{}, errors.New("Plane task context: execution attempt is missing")
	}
	stateVersion, fencingToken := dispatch.StateVersion, dispatch.FencingToken
	body := map[string]any{
		"expected_state_version": dispatch.StateVersion,
		"execution_attempt_id":   *dispatch.ExecutionAttemptID,
		"fencing_token":          dispatch.FencingToken,
		"session_id":             UUIDString(c.credentials.SessionID),
		"instance_nonce":         base64.RawURLEncoding.EncodeToString(c.credentials.InstanceNonce[:]),
		"context":                input,
	}
	var response struct {
		Context TaskContext `json:"context"`
	}
	err := c.do(ctx, http.MethodPost, c.projectPath("looper", "dispatch", dispatch.ID, "context"), nil, body, &signingContext{
		dispatchID: dispatch.ID, dispatchRevision: dispatch.Revision, stateVersion: &stateVersion,
		attemptID: dispatch.ExecutionAttemptID, fencingToken: &fencingToken,
	}, &response)
	return response.Context, err
}
