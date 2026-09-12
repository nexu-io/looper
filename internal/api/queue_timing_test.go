package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

func TestHandlerActiveRunsQueueTimingSurvivesDebounce(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	repos := rt.Services().Repositories
	ctx := context.Background()
	now := time.Now().UTC()
	created := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	enqueued := now.Add(-8 * time.Minute).Format(time.RFC3339Nano)
	updated := now.Format(time.RFC3339Nano)
	projectID, loopID := "queue_timing", "loop_queue_timing"
	future := now.Add(2 * time.Minute).Format(time.RFC3339Nano)
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Timing", RepoPath: "/tmp/repos/timing", CreatedAt: created, UpdatedAt: created}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 5015, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &projectID, Status: "queued", NextRunAt: &future, CreatedAt: created, UpdatedAt: updated}); err != nil {
		t.Fatal(err)
	}
	readItem := func() map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/active", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
		}
		items := parseJSONMap(t, rec.Body.Bytes())["data"].(map[string]any)["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("want one queued loop, got %v", items)
		}
		return items[0].(map[string]any)
	}
	queue := storage.QueueItemRecord{
		ID: "queue_timing", ProjectID: &projectID, LoopID: &loopID,
		Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:queue_timing", Priority: storage.QueuePriorityWorker,
		Status: "queued", AvailableAt: future, MaxAttempts: -1,
		CreatedAt: enqueued, UpdatedAt: updated,
	}
	for _, delay := range []time.Duration{2 * time.Minute, 5 * time.Minute} {
		queue.AvailableAt = now.Add(delay).Format(time.RFC3339Nano)
		if err := repos.Queue.Upsert(ctx, queue); err != nil {
			t.Fatal(err)
		}
		item := readItem()
		assertEqual(t, item["startedAt"], enqueued)
		assertEqual(t, item["availableAt"], queue.AvailableAt)
	}
}
