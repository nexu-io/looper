package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestConsumeAskSentinelReadsAndRemoves(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".looper"), 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	path := filepath.Join(dir, ".looper", "ask.json")
	if err := os.WriteFile(path, []byte(`{"question":"Redis or Postgres?","options":["redis","postgres"]}`), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	ask, err := consumeAskSentinel(dir)
	if err != nil {
		t.Fatalf("consumeAskSentinel error = %v", err)
	}
	if ask == nil || ask.Question != "Redis or Postgres?" || len(ask.Options) != 2 {
		t.Fatalf("ask = %#v, want the parsed question + 2 options", ask)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("sentinel file must be removed after consumption")
	}

	// No sentinel -> nil, nil (both a missing file and an absent .looper dir).
	if again, err := consumeAskSentinel(dir); err != nil || again != nil {
		t.Fatalf("second consumeAskSentinel = (%#v, %v), want (nil, nil)", again, err)
	}
	if missing, err := consumeAskSentinel(t.TempDir()); err != nil || missing != nil {
		t.Fatalf("consumeAskSentinel(empty dir) = (%#v, %v), want (nil, nil)", missing, err)
	}
}

func TestSuspendForHumanTransitionsAndNotifies(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	// Move the seeded loop + queue item into an in-flight state.
	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	loop.Status = "running"
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	queueItem, err := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
	if err != nil || queueItem == nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	queueItem.Status = "running"
	if err := fixture.repos.Queue.Upsert(ctx, *queueItem); err != nil {
		t.Fatalf("Queue.Upsert error = %v", err)
	}
	run := storage.RunRecord{ID: "run_worker_1", LoopID: "loop_worker_1", Status: "running", CurrentStep: stringPtr("execute"), LastCompletedStep: stringPtr("plan"), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert error = %v", err)
	}
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID error = %v", err)
	}

	var sent []HITLAskNotification
	runner := New(Options{
		DB:          fixture.coordinator.DB(),
		Repos:       fixture.repos,
		Logger:      fixture.logger,
		Now:         fixture.now,
		HITLEnabled: true,
		HITLNotify: func(_ context.Context, n HITLAskNotification) error {
			sent = append(sent, n)
			return nil
		},
	})

	awaiting := &awaitingHumanError{question: "Which datastore?", options: []string{"redis", "postgres"}, sessionID: "sess-xyz", executionID: "agent-1", vendor: "codex"}
	result, err := runner.suspendForHuman(ctx, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: *queueItem}, run, workerCheckpoint{}, awaiting)
	if err != nil {
		t.Fatalf("suspendForHuman error = %v", err)
	}
	if result.Status != "awaiting_human" {
		t.Fatalf("result.Status = %q, want awaiting_human", result.Status)
	}

	// Loop suspended + ask persisted.
	got, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	if got.Status != "awaiting_human" {
		t.Fatalf("loop status = %q, want awaiting_human", got.Status)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok || ask.Question != "Which datastore?" || ask.SessionID != "sess-xyz" || ask.Status != "awaiting" {
		t.Fatalf("persisted ask = %#v (ok=%v), want the question + session + awaiting", ask, ok)
	}

	// Queue item cancelled so the scheduler stops retrying (resume requeues it).
	q, err := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
	if err != nil || q == nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	if q.Status != "cancelled" {
		t.Fatalf("queue status = %q, want cancelled", q.Status)
	}

	// Run ended as interrupted (resumable from checkpoint).
	finishedRun, err := fixture.repos.Runs.GetByID(ctx, "run_worker_1")
	if err != nil || finishedRun == nil {
		t.Fatalf("Runs.GetByID error = %v", err)
	}
	if finishedRun.Status != "interrupted" {
		t.Fatalf("run status = %q, want interrupted", finishedRun.Status)
	}

	// Ask-card sent.
	if len(sent) != 1 {
		t.Fatalf("HITLNotify calls = %d, want 1", len(sent))
	}
	if sent[0].LoopSeq != 1 || sent[0].Question != "Which datastore?" || len(sent[0].Options) != 2 {
		t.Fatalf("notification = %#v, want loop seq 1 + question + 2 options", sent[0])
	}
}
