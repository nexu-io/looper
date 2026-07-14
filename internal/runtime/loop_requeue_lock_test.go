package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

// TestEnqueueHumanMessageToLoopSharesRequeueLock ensures free-text requeue waits
// on LockLoopRequeue — the same exclusion API discard+retry holds — so a
// concurrent discard cannot wipe the worktree for a message-driven continuation.
func TestEnqueueHumanMessageToLoopSharesRequeueLock(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.July, 14, 16, 0, 0, 0, time.UTC)
	nowISO := now.Format("2006-01-02T15:04:05.000Z")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())

	const loopID = "loop_requeue_lock_message"
	projectID := "project_requeue_lock_message"
	targetID := projectID
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Requeue lock", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 1, ProjectID: projectID, Type: "fixer",
		TargetType: "project", TargetID: &targetID, Status: "paused",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	loopIDPtr, projectIDPtr := loopID, projectID
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_requeue_lock_message", LoopID: &loopIDPtr, ProjectID: &projectIDPtr,
		Type: "fixer", TargetType: "project", TargetID: targetID, DedupeKey: "fixer:requeue_lock_message",
		Priority: storage.QueuePriorityFixer, Status: "cancelled", AvailableAt: nowISO, MaxAttempts: 3,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	// Simulate discard+retry holding the shared exclusion across preflight→git.
	unlock := LockLoopRequeue(loopID)

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- enqueueHumanMessageToLoop(context.Background(), repos, nowISO, loopID, "please continue")
	}()

	<-started
	select {
	case err := <-done:
		unlock()
		t.Fatalf("enqueueHumanMessageToLoop completed while LockLoopRequeue held: err=%v", err)
	case <-time.After(150 * time.Millisecond):
		// Still blocked — expected.
	}

	// While blocked, the loop must still be paused with no active queue.
	loop, err := repos.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil || loop.Status != "paused" {
		unlock()
		t.Fatalf("loop while lock held = %#v, %v, want paused", loop, err)
	}
	active, err := repos.Queue.FindActiveByLoopID(context.Background(), loopID)
	if err != nil {
		unlock()
		t.Fatalf("FindActiveByLoopID() error = %v", err)
	}
	if active != nil {
		unlock()
		t.Fatalf("active queue while lock held = %#v, want nil", active)
	}

	unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("enqueueHumanMessageToLoop after unlock error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("enqueueHumanMessageToLoop did not complete after LockLoopRequeue release")
	}

	loop, err = repos.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil || loop.Status != "queued" {
		t.Fatalf("loop after enqueue = %#v, %v, want queued", loop, err)
	}
	active, err = repos.Queue.FindActiveByLoopID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("FindActiveByLoopID() after enqueue error = %v", err)
	}
	if active == nil || active.Status != "queued" {
		t.Fatalf("active queue after enqueue = %#v, want queued", active)
	}
}

// TestDeliverHITLAnswerToLoopSharesRequeueLock mirrors free-text exclusion for
// poll-delivered HITL answers that requeue without the API Handler locks.
func TestDeliverHITLAnswerToLoopSharesRequeueLock(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.July, 14, 16, 0, 0, 0, time.UTC)
	nowISO := now.Format("2006-01-02T15:04:05.000Z")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())

	const loopID = "loop_requeue_lock_answer"
	projectID := "project_requeue_lock_answer"
	targetID := projectID
	meta := `{"hitl":{"question":"ok?","sessionId":"s1","status":"awaiting","askedAt":"2026-07-14T15:00:00.000Z"}}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Requeue answer lock", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 2, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Status: "awaiting_human",
		MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	loopIDPtr, projectIDPtr := loopID, projectID
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_requeue_lock_answer", LoopID: &loopIDPtr, ProjectID: &projectIDPtr,
		Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:requeue_lock_answer",
		Priority: storage.QueuePriorityWorker, Status: "cancelled", AvailableAt: nowISO, MaxAttempts: 3,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	unlock := LockLoopRequeue(loopID)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- deliverHITLAnswerToLoop(context.Background(), repos, nowISO, loopID, "yes")
	}()
	<-started
	select {
	case err := <-done:
		unlock()
		t.Fatalf("deliverHITLAnswerToLoop completed while LockLoopRequeue held: err=%v", err)
	case <-time.After(150 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("deliverHITLAnswerToLoop after unlock error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deliverHITLAnswerToLoop did not complete after LockLoopRequeue release")
	}
	loop, err := repos.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil || loop.Status != "running" {
		t.Fatalf("loop after answer = %#v, %v, want running", loop, err)
	}
}
