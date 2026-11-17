package reviewer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func TestCreateRunContextConcurrentDoubleCreateOneRunning(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	nowISO := fixture.nowISO()
	loop := storage.LoopRecord{
		ID: "loop_reviewer_create_race", Seq: 1, ProjectID: "project_1", Type: "reviewer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := runner.createRunContext(context.Background(), loop)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var successes int
	var busy int
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		var loopErr *loopError
		if !errors.As(err, &loopErr) || loopErr.kind != FailureRetryableTransient {
			t.Fatalf("createRunContext() unexpected error = %v", err)
		}
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
			t.Fatalf("createRunContext leaked UNIQUE constraint error: %v", err)
		}
		busy++
	}
	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1", successes)
	}
	if busy != workers-1 {
		t.Fatalf("busy losers = %d, want %d", busy, workers-1)
	}
	hasRunning, err := fixture.repos.Runs.HasRunningByLoopID(context.Background(), loop.ID)
	if err != nil || !hasRunning {
		t.Fatalf("HasRunningByLoopID() = %v, %v; want true, nil", hasRunning, err)
	}
}

func TestCreateRunContextTreatsActiveRunningRunAsRetryable(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	nowISO := fixture.nowISO()
	loop := storage.LoopRecord{
		ID: "loop_reviewer_active_run", Seq: 1, ProjectID: "project_1", Type: "reviewer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	active := storage.RunRecord{
		ID: "run_reviewer_active", LoopID: loop.ID, Status: "running",
		CurrentStep: stringPtr(string(stepDiscover)), StartedAt: nowISO, LastHeartbeatAt: &nowISO,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Runs.Upsert(context.Background(), active); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	_, err := runner.createRunContext(context.Background(), loop)
	if err == nil {
		t.Fatal("createRunContext() error = nil, want retryable active-run error")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureRetryableTransient {
		t.Fatalf("createRunContext() error = %#v, want retryable_transient loopError", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
		t.Fatalf("createRunContext leaked UNIQUE constraint error: %v", err)
	}
	preserved, err := fixture.repos.Runs.GetByID(context.Background(), active.ID)
	if err != nil || preserved == nil || preserved.Status != "running" {
		t.Fatalf("active run = (%#v, %v), want still running", preserved, err)
	}
}

func TestProcessClaimedQueueItemMapsActiveRunConflictToRetryable(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(42)
	nowISO := fixture.nowISO()
	loopID := "loop_reviewer_active_claim"
	queueID := "queue_reviewer_active_claim"
	targetID := "pr:acme/looper:42"
	loop := storage.LoopRecord{
		ID: loopID, Seq: 2, ProjectID: "project_1", Type: "reviewer",
		TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	active := storage.RunRecord{
		ID: "run_reviewer_claim_active", LoopID: loopID, Status: "running",
		CurrentStep: stringPtr(string(stepReview)), StartedAt: nowISO, LastHeartbeatAt: &nowISO,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Runs.Upsert(context.Background(), active); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{
		ID: queueID, ProjectID: stringPtr("project_1"), LoopID: &loopID, Type: "reviewer",
		TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "reviewer:" + loopID, Priority: storage.QueuePriorityReviewer,
		Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 5,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	_, err := runner.ProcessClaimedQueueItem(context.Background(), queueItem)
	if err == nil {
		t.Fatal("ProcessClaimedQueueItem() error = nil, want active-run setup failure")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureRetryableTransient {
		t.Fatalf("ProcessClaimedQueueItem() error = %#v, want retryable_transient", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
		t.Fatalf("ProcessClaimedQueueItem leaked UNIQUE constraint: %v", err)
	}

	updatedQueue, err := fixture.repos.Queue.GetByID(context.Background(), queueID)
	if err != nil || updatedQueue == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v)", updatedQueue, err)
	}
	// Normal setup-failure path requeues retryable_transient (may burn one attempt).
	// Must not land in manual_intervention on the UNIQUE race fingerprint.
	if updatedQueue.Status == "manual_intervention" || updatedQueue.Status == "failed" {
		t.Fatalf("queue status = %s kind=%v, want requeued not terminal MI/failed", updatedQueue.Status, updatedQueue.LastErrorKind)
	}
	if updatedQueue.LastErrorKind != nil && *updatedQueue.LastErrorKind == string(FailureNonRetryable) {
		t.Fatalf("queue last_error_kind = non_retryable, want retryable_transient")
	}
	if updatedQueue.LastErrorKind != nil && *updatedQueue.LastErrorKind != string(FailureRetryableTransient) && updatedQueue.Status == "queued" {
		t.Fatalf("queue last_error_kind = %v, want retryable_transient", *updatedQueue.LastErrorKind)
	}
}
