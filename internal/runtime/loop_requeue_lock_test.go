package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
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

// TestEnqueueHumanMessageToLoopSharesTargetLock ensures free-text requeue of a
// different loop on the same PR target waits on the target mutex held by
// discard+retry — per-loop exclusion alone cannot prevent that race.
func TestEnqueueHumanMessageToLoopSharesTargetLock(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.July, 14, 17, 0, 0, 0, time.UTC)
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

	projectID := "project_requeue_target_lock"
	repo := "acme/looper"
	prNumber := int64(42)
	prTarget := "pr:acme/looper:42"
	const waitingLoopID = "loop_requeue_target_waiting"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Target lock", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: waitingLoopID, Seq: 3, ProjectID: projectID, Type: "worker",
		TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber,
		Status: "waiting", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	loopIDPtr, projectIDPtr := waitingLoopID, projectID
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_requeue_target_waiting", LoopID: &loopIDPtr, ProjectID: &projectIDPtr,
		Type: "worker", TargetType: "pull_request", TargetID: prTarget, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "worker:" + projectID + ":" + repo + ":42", Priority: storage.QueuePriorityWorker,
		Status: "cancelled", AvailableAt: nowISO, MaxAttempts: 3,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	// Hold the shared PR target lock as discard+retry of a *different* fixer would.
	key := LoopTargetGuardKey(projectID, string(domain.LoopTypeFixer), string(domain.LoopTargetTypePullRequest), "pull_request:acme/looper:42")
	unlockTarget := LockLoopTarget(key)

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- enqueueHumanMessageToLoop(context.Background(), repos, nowISO, waitingLoopID, "please continue")
	}()

	<-started
	select {
	case err := <-done:
		unlockTarget()
		t.Fatalf("enqueueHumanMessageToLoop completed while PR target lock held: err=%v", err)
	case <-time.After(150 * time.Millisecond):
		// Still blocked on shared PR worktree key — expected.
	}

	// While blocked, the waiting loop must not have been requeued yet.
	loop, err := repos.Loops.GetByID(context.Background(), waitingLoopID)
	if err != nil || loop == nil || loop.Status != "waiting" {
		unlockTarget()
		t.Fatalf("loop while target lock held = %#v, %v, want waiting", loop, err)
	}
	active, err := repos.Queue.FindActiveByLoopID(context.Background(), waitingLoopID)
	if err != nil {
		unlockTarget()
		t.Fatalf("FindActiveByLoopID() error = %v", err)
	}
	if active != nil {
		unlockTarget()
		t.Fatalf("active queue while target lock held = %#v, want nil", active)
	}

	unlockTarget()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("enqueueHumanMessageToLoop after target unlock error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("enqueueHumanMessageToLoop did not complete after PR target lock release")
	}

	loop, err = repos.Loops.GetByID(context.Background(), waitingLoopID)
	if err != nil || loop == nil || loop.Status != "queued" {
		t.Fatalf("loop after enqueue = %#v, %v, want queued", loop, err)
	}
}

// TestLoopTargetGuardKeyOmitsTypeForPullRequest documents that PR worktree
// keys are shared across loop types while non-PR keys remain type-scoped.
func TestLoopTargetGuardKeyOmitsTypeForPullRequest(t *testing.T) {
	fixer := LoopTargetGuardKey("proj", "fixer", "pull_request", "pull_request:acme/looper:42")
	worker := LoopTargetGuardKey("proj", "worker", "pull_request", "pull_request:acme/looper:42")
	if fixer == "" || fixer != worker {
		t.Fatalf("PR keys = %q / %q, want equal non-empty shared key", fixer, worker)
	}
	issueFixer := LoopTargetGuardKey("proj", "fixer", "issue", "issue:acme/looper:7")
	issueWorker := LoopTargetGuardKey("proj", "worker", "issue", "issue:acme/looper:7")
	if issueFixer == "" || issueFixer == issueWorker {
		t.Fatalf("issue keys = %q / %q, want distinct type-scoped keys", issueFixer, issueWorker)
	}
	if got := LoopTargetGuardKey("proj", "worker", "project", "project:proj"); got != "" {
		t.Fatalf("project worker key = %q, want empty (concurrent workers)", got)
	}
}

// TestDeferredReviewerRecoverySharesPRTargetLock ensures deferred recovery
// requeue waits on the same PR target mutex discard+retry holds, so it cannot
// activate a reviewer sibling between discard preflight and git reset.
func TestDeferredReviewerRecoverySharesPRTargetLock(t *testing.T) {
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.Behavior.Loop.StopOnApproved = true
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, filepath.Join(workingDir, "backups"))
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 14, 18, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	projectRepoPath := filepath.Join(workingDir, "repo")
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "project_deferred_target_lock", Name: "Deferred target lock", RepoPath: projectRepoPath, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	const loopID = "loop_deferred_target_lock"
	seedFailedReviewerRecoveryLoop(t, repositories, "project_deferred_target_lock", loopID, 1, nowISO)

	// seedFailedReviewerRecoveryLoop uses prNumber = 42+seq → 43.
	key := LoopTargetGuardKey("project_deferred_target_lock", string(domain.LoopTypeFixer), string(domain.LoopTargetTypePullRequest), "pull_request:acme/looper:43")
	unlockTarget := LockLoopTarget(key)

	githubGateway := githubinfra.New(githubinfra.Options{GHRun: func(ctx context.Context, options shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: "other\n"}, nil
	}})
	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})

	started := make(chan struct{})
	done := make(chan struct {
		n   int64
		err error
	}, 1)
	go func() {
		close(started)
		n, err := rt.runDeferredReviewerRecovery(context.Background(), repositories, githubGateway, now)
		done <- struct {
			n   int64
			err error
		}{n, err}
	}()
	<-started
	select {
	case result := <-done:
		unlockTarget()
		t.Fatalf("runDeferredReviewerRecovery completed while PR target lock held: n=%d err=%v", result.n, result.err)
	case <-time.After(150 * time.Millisecond):
		// Still blocked — expected.
	}

	loop, err := repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil || loop.Status != "failed" {
		unlockTarget()
		t.Fatalf("loop while target lock held = %#v, %v, want failed", loop, err)
	}

	unlockTarget()

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("runDeferredReviewerRecovery after unlock error = %v", result.err)
		}
		if result.n != 1 {
			t.Fatalf("runDeferredReviewerRecovery after unlock = %d, want 1", result.n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDeferredReviewerRecovery did not complete after PR target lock release")
	}
	assertLoopStatus(t, repositories, loopID, "queued")
}
