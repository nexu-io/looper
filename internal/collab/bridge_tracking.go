package collab

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/feishubridge"
	"github.com/nexu-io/looper/internal/storage"
)

func bridgeRunID(loopID, turnID string) string {
	if turnID == "" {
		return eventlog.NewEventID("run_bridge")
	}
	return fmt.Sprintf("run_bridge_%x", sha256.Sum256([]byte(loopID+":"+turnID)))
}

// Callback identity, not the task conclusion, controls replay detection.
func (h *bridgeHost) bridgeDuplicate(ctx context.Context, loop storage.LoopRecord, event feishubridge.TurnEvent) bool {
	if event.TurnID == "" {
		return false
	} // compatibility with older prebuilt bridges
	run, err := h.repos.Runs.GetByID(ctx, bridgeRunID(loop.ID, event.TurnID))
	if err != nil {
		h.warn("turn tracking read failed", err)
		return false
	}
	return run != nil && (event.Phase == "started" || run.EndedAt != nil)
}

func (h *bridgeHost) track(ctx context.Context, loop storage.LoopRecord, kind, key, runID string, payload any) {
	var run *string
	if runID != "" {
		run = &runID
	}
	if err := eventlog.Track(ctx, h.repos, eventlog.AppendInput{ID: kind + ":" + key, EventType: kind, ProjectID: &loop.ProjectID, LoopID: &loop.ID, RunID: run, Payload: payload}); err != nil {
		h.warn("task tracking write failed", err)
	}
}

func (h *bridgeHost) trackTurn(ctx context.Context, loop storage.LoopRecord, state bridgeTask, event feishubridge.TurnEvent, taskStatus string) {
	if err := h.saveTurn(ctx, loop, state, event, taskStatus); err != nil {
		h.warn("turn tracking write failed", err)
	}
}

func (h *bridgeHost) saveTurn(ctx context.Context, loop storage.LoopRecord, state bridgeTask, event feishubridge.TurnEvent, taskStatus string) error {
	id := state.TrackingRunID
	if id == "" {
		return nil
	} // legacy callback without an observed start
	now := eventlog.FormatJavaScriptISOString(time.Now())
	run, err := h.repos.Runs.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if run == nil {
		run = &storage.RunRecord{ID: id, LoopID: loop.ID, Status: "running", StartedAt: now, CreatedAt: now}
	}
	execID := "agent_" + id
	execution, err := h.repos.AgentExecutions.GetByID(ctx, execID)
	if err != nil {
		return err
	}
	if execution == nil {
		execution = &storage.AgentExecutionRecord{ID: execID, ProjectID: &loop.ProjectID, LoopID: &loop.ID, RunID: &id, Vendor: "codex", Status: "running", StartedAt: run.StartedAt, CreatedAt: run.CreatedAt}
	}
	if event.SessionID != "" {
		execution.NativeSessionID = &event.SessionID
	}
	terminal := event.Phase == "finished" || event.Phase == "failed"
	if terminal {
		run.Status, execution.Status = "failed", "failed"
		if event.Phase == "finished" && event.Terminal == "done" {
			run.Status, execution.Status = "success", "completed"
		}
		run.EndedAt, execution.EndedAt = &now, &now
	}
	run.UpdatedAt, execution.UpdatedAt = now, now
	if err := h.repos.Runs.Upsert(ctx, *run); err != nil {
		return err
	}
	if err := h.repos.AgentExecutions.Upsert(ctx, *execution); err != nil {
		return err
	}
	payload := map[string]any{"status": run.Status, "transport": "feishu_bridge", "source": "observed_callback"}
	if event.Phase == "started" {
		h.track(ctx, loop, "run.started", id, id, payload)
		h.track(ctx, loop, "agent.started", execID, id, payload)
	} else if terminal {
		h.track(ctx, loop, "agent.completed", execID, id, payload)
		h.track(ctx, loop, "run.ended", id, id, payload)
		if event.Phase == "finished" && event.Terminal == "done" && taskStatus != "failed" {
			for i, report := range state.Outcome.Repositories {
				h.track(ctx, loop, "repository.reported", fmt.Sprintf("%s:%d", id, i), id, report)
			}
			h.track(ctx, loop, "artifacts.reported", id, id, map[string]any{"count": len(state.Outcome.PullRequests), "source": "agent_report"})
		}
	}
	if loop.Status != taskStatus {
		h.track(ctx, loop, "task.state_changed", id+":"+event.Phase, id, map[string]any{"from": loop.Status, "to": taskStatus})
	}
	if taskStatus == "awaiting_human" {
		h.track(ctx, loop, "human.requested", id, id, map[string]any{"transport": "feishu_bridge"})
	}
	if event.Phase == "started" && loop.Status == "awaiting_human" {
		h.track(ctx, loop, "human.answered", id, id, map[string]any{"transport": "feishu_bridge"})
	}
	return nil
}

// Shutdown may miss a terminal callback; record uncertainty, never success.
func (h *bridgeHost) interruptTrackedTurns(ctx context.Context) error {
	now := eventlog.FormatJavaScriptISOString(time.Now())
	_, err := h.db.ExecContext(ctx, `UPDATE agent_executions SET status='failed',error_message='Host restarted without a terminal callback; outcome unknown', ended_at=?,updated_at=? WHERE status='running' AND run_id IN (SELECT r.id FROM runs r JOIN loops l ON l.id=r.loop_id WHERE r.id LIKE 'run_bridge_%' AND json_extract(l.metadata_json,'$.bridge') IS NOT NULL)`, now, now)
	if err != nil {
		return err
	}
	_, err = h.db.ExecContext(ctx, `UPDATE runs SET status='interrupted', ended_at=?,updated_at=? WHERE status='running' AND id LIKE 'run_bridge_%' AND loop_id IN (SELECT id FROM loops WHERE json_extract(metadata_json,'$.bridge') IS NOT NULL)`, now, now)
	return err
}
