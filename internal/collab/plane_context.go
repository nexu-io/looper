package collab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/planestrict"
	"github.com/nexu-io/looper/internal/storage"
)

func (h *bridgeHost) savePlaneContext(dispatch planestrict.Dispatch) error {
	data, err := json.MarshalIndent(struct {
		Context  planestrict.TaskContext `json:"context"`
		Timeline []planestrict.TaskEvent `json:"timeline"`
	}{dispatch.TaskContext, dispatch.TaskTimeline}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(h.dir, "workspace", "tasks", "loop_plane_"+dispatch.ID+".plane.json")
	if err := os.WriteFile(path+".tmp", data, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (h *bridgeHost) publishPlaneContext(ctx context.Context, client *forge.PlaneClient, dispatch planestrict.Dispatch, loop storage.LoopRecord, state bridgeTask) error {
	var metadata struct {
		Worker struct {
			Summary string `json:"summary"`
		} `json:"worker"`
	}
	if loop.MetadataJSON != nil {
		_ = json.Unmarshal([]byte(*loop.MetadataJSON), &metadata)
	}
	summary := state.Outcome.Summary
	if loop.Status == "failed" {
		summary = metadata.Worker.Summary
	}
	chat, err := h.repos.FeishuThreads.ChatIDByLoop(ctx, loop.ID)
	if err != nil {
		return err
	}
	if savedChat, _, ok := loops.ReadFeishuChat(loop.MetadataJSON); chat == "" && ok {
		chat = savedChat
	}
	if summary == "" && chat == "" {
		return nil // An accepted queue slot has no conclusion or destination yet.
	}
	hasOutcome := summary != ""
	input := planestrict.TaskContext{Summary: summary, NextStep: state.Outcome.NextStep,
		Context: state.Outcome.Context, Links: append([]planestrict.TaskLink{}, state.Outcome.Links...),
		Events: append([]planestrict.TaskEvent{}, state.Outcome.Events...)}
	if !hasOutcome {
		input = dispatch.TaskContext
		input.Events = append([]planestrict.TaskEvent{}, state.Outcome.Events...)
	}
	if input.Context == "" {
		input.Context = dispatch.TaskContext.Context
	}
	if chat != "" && state.ThreadID != "" {
		input.ThreadURL = fmt.Sprintf("https://applink.feishu.cn/client/thread/open?open_chat_id=%s&open_thread_id=%s", chat, state.ThreadID)
	}
	if chat != "" {
		input.ChatURL = "https://applink.feishu.cn/client/chat/open?openChatId=" + chat
		_, input.ChatScopeKey, _ = loops.ReadFeishuChat(loop.MetadataJSON)
		workerInput, _, _ := readBridgeTask(loop)
		input.ChatName = taskGroupContext(workerInput, input.ChatScopeKey).Name
	}
	for _, link := range state.Outcome.PullRequests {
		found := false
		for _, existing := range input.Links {
			if existing.URL == link {
				found = true
				break
			}
		}
		if !found {
			input.Links = append(input.Links, planestrict.TaskLink{Title: "Pull Request", URL: link, Type: "pull_request"})
		}
	}
	if len(input.Events) == 0 && input.Summary != "" && input.Summary != dispatch.TaskContext.Summary {
		input.Events = append(input.Events, planestrict.TaskEvent{ID: "status:" + loop.UpdatedAt,
			Title: summary, ActorName: "Looper", Kind: "progress", OccurredAt: loop.UpdatedAt})
	}
	// The existing outcome holds unsynced facts across turns and restarts.
	// Publish bounded batches, then remove only acknowledged IDs from the latest
	// row, so a reply arriving during HTTP delivery cannot lose its new events.
	publish := func(batch planestrict.TaskContext) error {
		confirmed, err := client.PublishTaskContext(ctx, dispatch, batch)
		if err != nil {
			if len(batch.Events) > 0 {
				h.trackPlaneSyncFailure(ctx, loop, dispatch.ID, "context", loop.Status, err)
			}
			return err
		}
		// Periodic snapshots with no new timeline facts would flood the history.
		if len(batch.Events) > 0 {
			h.track(ctx, loop, "diagnostic.plane.context.synced", eventlog.NewEventID("sync"), "",
				map[string]any{"dispatchId": dispatch.ID, "count": len(batch.Events)})
		}
		// Keep the next native turn's recovery file aligned with the confirmed
		// summary, including when completion removes this task from the inbox.
		dispatch.TaskContext = confirmed
		if err := h.savePlaneContext(dispatch); err != nil {
			return err
		}
		return h.ackPlaneEvents(ctx, loop.ID, batch.Events)
	}
	for len(input.Events) > 100 {
		batch := input
		batch.Events = input.Events[:100]
		if err := publish(batch); err != nil {
			return err
		}
		input.Events = input.Events[100:]
	}
	return publish(input)
}

func (h *bridgeHost) ackPlaneEvents(ctx context.Context, id string, sent []planestrict.TaskEvent) error {
	if len(sent) == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	loop, err := h.repos.Loops.GetByID(ctx, id)
	if err != nil || loop == nil {
		return err
	}
	_, current, ok := readBridgeTask(*loop)
	if !ok || len(current.Outcome.Events) == 0 {
		return nil
	}
	ids := make(map[string]bool, len(sent))
	for _, event := range sent {
		ids[event.ID] = true
	}
	remaining := make([]planestrict.TaskEvent, 0, len(current.Outcome.Events))
	for _, event := range current.Outcome.Events {
		if !ids[event.ID] {
			remaining = append(remaining, event)
		}
	}
	data, err := json.Marshal(remaining)
	if err != nil {
		return err
	}
	_, err = h.db.ExecContext(ctx, `UPDATE loops SET metadata_json=json_set(metadata_json,'$.bridge.outcome.events',json(?)) WHERE id=? AND metadata_json=?`, string(data), id, *loop.MetadataJSON)
	return err
}
