package worker

import (
	"context"
	"time"

	"github.com/nexu-io/looper/internal/eventlog"
)

func (r *Runner) track(ctx context.Context, kind, key, projectID, loopID, runID string, payload map[string]any) {
	err := eventlog.Track(ctx, r.repos, eventlog.AppendInput{ID: kind + ":" + key, EventType: kind,
		ProjectID: optionalString(projectID), LoopID: optionalString(loopID), RunID: optionalString(runID),
		Payload: payload, CreatedAt: r.now()})
	if err != nil && r.logger != nil {
		r.logger.Warn("task tracking write failed", map[string]any{"eventType": kind, "loopId": loopID, "runId": runID})
	}
}

func elapsedMilliseconds(start, end string) *int64 {
	a, err := time.Parse(time.RFC3339Nano, start)
	if err != nil {
		return nil
	}
	b, err := time.Parse(time.RFC3339Nano, end)
	if err != nil || b.Before(a) {
		return nil
	}
	ms := b.Sub(a).Milliseconds()
	return &ms
}
