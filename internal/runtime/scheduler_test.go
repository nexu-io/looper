package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator"
	"github.com/nexu-io/looper/internal/fixer"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/sweeper"
	"github.com/nexu-io/looper/internal/worker"
)

func TestWorkerAgentExecutionAdapterPropagatesParseStatus(t *testing.T) {
	t.Parallel()

	adapter := workerAgentExecutionAdapter{execution: stubAgentExecution{result: agent.Result{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed", ChangedFiles: []string{"worker.go"}, Commits: []string{"abc123"}}}}

	result, err := adapter.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.ParseStatus != "parsed" {
		t.Fatalf("ParseStatus = %q, want parsed", result.ParseStatus)
	}
	if result.Status != "completed" || result.Summary != "done" || result.Stdout != "ok" || len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "worker.go" || len(result.Commits) != 1 || result.Commits[0] != "abc123" {
		t.Fatalf("result = %#v, want propagated agent result fields", result)
	}
}

func TestCommentInfosToObjectsPreservesNestedAuthorLogin(t *testing.T) {
	t.Parallel()

	comments := commentInfosToObjects([]githubinfra.CommentInfo{{ID: 1, Author: "looper", AuthorAssociation: "MEMBER", Body: "summary", URL: "https://example.test/comment/1"}})
	if len(comments) != 1 {
		t.Fatalf("len(comments) = %d, want 1", len(comments))
	}
	author, ok := comments[0]["author"].(map[string]any)
	if !ok {
		t.Fatalf("author = %#v, want nested map", comments[0]["author"])
	}
	if author["login"] != "looper" {
		t.Fatalf("author login = %#v, want looper", author["login"])
	}
}

func TestRunDefaultSchedulerTickDiscoversStoredProjectsAndProcessesQueue(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	projectTarget := "project:looper"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_1", Seq: 1, ProjectID: "looper", Type: "worker", TargetType: "project", TargetID: &projectTarget, Repo: stringPtr("nexu-io/looper"), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "looper"
	loopID := "loop_worker_1"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_1", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectTarget, Repo: stringPtr("nexu-io/looper"), DedupeKey: "worker:loop_worker_1", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	plannerRunner := &stubPlannerScheduler{}
	coordinatorRunner := &stubCoordinatorScheduler{}
	reviewerRunner := &stubReviewerScheduler{}
	fixerRunner := &stubFixerScheduler{}
	workerRunner := &stubWorkerScheduler{}
	sweeperRunner := &stubSweeperScheduler{}

	err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:              repos,
		Now:                func() time.Time { return now },
		MaxConcurrentRuns:  1,
		Planner:            plannerRunner,
		Coordinator:        coordinatorRunner,
		CoordinatorEnabled: func(string) bool { return true },
		Reviewer:           reviewerRunner,
		Fixer:              fixerRunner,
		Worker:             workerRunner,
		Sweeper:            sweeperRunner,
	})
	if err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if len(plannerRunner.discoverCalls) != 1 || plannerRunner.discoverCalls[0].ProjectID != "looper" || plannerRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("planner discover calls = %#v, want stored project discovery", plannerRunner.discoverCalls)
	}
	if len(coordinatorRunner.discoverCalls) != 1 || coordinatorRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("coordinator discover calls = %#v, want stored project repo", coordinatorRunner.discoverCalls)
	}
	if len(reviewerRunner.discoverCalls) != 1 || reviewerRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("reviewer discover calls = %#v, want stored project repo", reviewerRunner.discoverCalls)
	}
	if len(fixerRunner.discoverCalls) != 1 || fixerRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("fixer discover calls = %#v, want stored project repo", fixerRunner.discoverCalls)
	}
	if len(workerRunner.discoverCalls) != 1 || workerRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("worker discover calls = %#v, want stored project repo", workerRunner.discoverCalls)
	}
	if len(sweeperRunner.issueDiscoverCalls) != 1 || sweeperRunner.issueDiscoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("sweeper issue discovery calls = %#v, want stored project repo", sweeperRunner.issueDiscoverCalls)
	}
	if len(sweeperRunner.pullRequestDiscoverCalls) != 1 || sweeperRunner.pullRequestDiscoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("sweeper pull request discovery calls = %#v, want stored project repo", sweeperRunner.pullRequestDiscoverCalls)
	}
	if len(sweeperRunner.reconcileDiscoverCalls) != 1 || sweeperRunner.reconcileDiscoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("sweeper reconcile discovery calls = %#v, want stored project repo", sweeperRunner.reconcileDiscoverCalls)
	}
	waitForSchedulerCondition(t, func() bool {
		return workerRunner.processItemCount() == 1
	})
	if workerRunner.processItemCount() != 1 {
		t.Fatalf("worker processed items = %#v, want one queued worker run", workerRunner.processedItems)
	}
}

func TestRunDefaultSchedulerTickClaimsQueuedWorkBeforeDiscovery(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-claim-first.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	projectID := "looper"
	loopTarget := "project:looper"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_claim_first", Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &loopTarget, Repo: stringPtr("nexu-io/looper"), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	loopID := "loop_worker_claim_first"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_claim_first", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: loopTarget, Repo: stringPtr("nexu-io/looper"), DedupeKey: "worker:loop_worker_claim_first", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	plannerRunner := &queueStatusCheckingPlannerScheduler{t: t, repos: repos, queueItemID: "queue_worker_claim_first"}

	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		Planner:           plannerRunner,
		Worker:            &stubWorkerScheduler{},
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if !plannerRunner.checkedClaimedStatus {
		t.Fatal("planner discovery did not verify claimed queue item status")
	}
}

func TestRunDefaultSchedulerTickSkipsCoordinatorWhenDisabled(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinatorDB := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-coordinator-disabled.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinatorDB.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)

	runner := &stubCoordinatorScheduler{}
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:              repos,
		Now:                func() time.Time { return now },
		Coordinator:        runner,
		CoordinatorEnabled: func(string) bool { return false },
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if len(runner.discoverCalls) != 0 {
		t.Fatalf("coordinator discover calls = %#v, want none when disabled", runner.discoverCalls)
	}
}

func TestRunDefaultSchedulerTickClaimsWorkerDiscoveredDuringTickBeforeSweeper(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-discovered-worker.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)

	workerRunner := &discoveringWorkerScheduler{repos: repos, nowISO: nowISO, item: schedulerTestQueueItem("queue_worker_discovered", "worker", nowISO)}
	sweeperRunner := &assertingSweeperScheduler{t: t, beforeDiscover: func() {
		if workerRunner.processItemCount() != 1 {
			t.Fatalf("sweeper discovery started before discovered worker was claimed; processed workers = %d, want 1", workerRunner.processItemCount())
		}
	}}

	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		AsyncRunner:       immediateSchedulerRunner{},
		Worker:            workerRunner,
		Sweeper:           sweeperRunner,
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if workerRunner.processItemCount() != 1 {
		t.Fatalf("worker processed items = %#v, want discovered worker claimed before tick returned", workerRunner.processedItems)
	}
	if sweeperRunner.issueCalls != 1 {
		t.Fatalf("sweeper issue discovery calls = %d, want 1", sweeperRunner.issueCalls)
	}
}

func TestRunDefaultSchedulerTickSecondClaimPassDoesNotExceedAvailableSlots(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-second-pass-slots.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)
	queued := schedulerTestQueueItem("queue_worker_existing", "worker", nowISO)
	if err := repos.Queue.Upsert(context.Background(), queued); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	discovered := schedulerTestQueueItem("queue_worker_discovered", "worker", nowISO)
	workerRunner := &discoveringWorkerScheduler{repos: repos, nowISO: nowISO, item: discovered}
	var wakes int32
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:                repos,
		Now:                  func() time.Time { return now },
		MaxConcurrentRuns:    1,
		AsyncRunner:          immediateSchedulerRunner{},
		RequestSchedulerWake: func() { atomic.AddInt32(&wakes, 1) },
		Worker:               workerRunner,
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if workerRunner.processItemCount() != 1 || workerRunner.processedItems[0] != queued.ID {
		t.Fatalf("worker processed items = %#v, want only first-pass item", workerRunner.processedItems)
	}
	item, err := repos.Queue.GetByID(context.Background(), discovered.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if item == nil || item.Status != "queued" {
		t.Fatalf("discovered item = %#v, want still queued because first-pass run used the only slot", item)
	}
	if got := atomic.LoadInt32(&wakes); got != 0 {
		t.Fatalf("scheduler wake requests = %d, want none when discovered work could not be claimed", got)
	}
}

func TestRunDefaultSchedulerTickSecondClaimPassDoesNotDoubleClaim(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-no-double-claim.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)
	queued := schedulerTestQueueItem("queue_worker_existing", "worker", nowISO)
	if err := repos.Queue.Upsert(context.Background(), queued); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	workerRunner := &stubWorkerScheduler{}
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 2,
		AsyncRunner:       immediateSchedulerRunner{},
		Worker:            workerRunner,
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if workerRunner.processItemCount() != 1 || workerRunner.processedItems[0] != queued.ID {
		t.Fatalf("worker processed items = %#v, want existing item exactly once", workerRunner.processedItems)
	}
}

func TestRunDefaultSchedulerTickSecondClaimPassPreservesQueueEligibilityRules(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-second-pass-eligibility.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)

	lockKey := "repo:nexu-io/looper"
	lockedA := schedulerTestQueueItem("queue_worker_lock_a", "worker", nowISO)
	lockedA.LockKey = &lockKey
	lockedB := schedulerTestQueueItem("queue_worker_lock_b", "worker", nowISO)
	lockedB.LockKey = &lockKey
	repo := "nexu-io/looper"
	pr := int64(281)
	reviewerItem := schedulerTestQueueItem("queue_reviewer_pr", "reviewer", nowISO)
	reviewerItem.Repo = &repo
	reviewerItem.PRNumber = &pr
	fixerItem := schedulerTestQueueItem("queue_fixer_pr", "fixer", nowISO)
	fixerItem.Repo = &repo
	fixerItem.PRNumber = &pr

	plannerRunner := &enqueueingPlannerScheduler{repos: repos, items: []storage.QueueItemRecord{lockedA, lockedB}}
	reviewerRunner := &enqueueingReviewerScheduler{repos: repos, item: reviewerItem}
	fixerRunner := &enqueueingFixerScheduler{repos: repos, item: fixerItem}
	workerRunner := &stubWorkerScheduler{}
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 4,
		AsyncRunner:       immediateSchedulerRunner{},
		Planner:           plannerRunner,
		Reviewer:          reviewerRunner,
		Fixer:             fixerRunner,
		Worker:            workerRunner,
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if workerRunner.processItemCount() != 1 {
		t.Fatalf("worker processed items = %#v, want one of two same-lock discovered workers", workerRunner.processedItems)
	}
	if reviewerRunner.processItemCount() != 1 {
		t.Fatalf("reviewer processed items = %#v, want reviewer claimed", reviewerRunner.processedItems)
	}
	if fixerRunner.processItemCount() != 0 {
		t.Fatalf("fixer processed items = %#v, want fixer gated behind reviewer", fixerRunner.processedItems)
	}
}

func TestRunDefaultSchedulerTickDiscoveryWakeIsBounded(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-wake-bounded.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)

	var wakes int32
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:                   repos,
		Now:                     func() time.Time { return now },
		MaxConcurrentRuns:       4,
		RequestSchedulerWake:    func() { atomic.AddInt32(&wakes, 1) },
		Planner:                 &enqueueingPlannerScheduler{repos: repos, items: []storage.QueueItemRecord{schedulerTestQueueItem("queue_worker_from_planner_a", "worker", nowISO), schedulerTestQueueItem("queue_worker_from_planner_b", "worker", nowISO)}},
		Reviewer:                &enqueueingReviewerScheduler{repos: repos, item: schedulerTestQueueItem("queue_reviewer_wake", "reviewer", nowISO)},
		Fixer:                   &enqueueingFixerScheduler{repos: repos, item: schedulerTestQueueItem("queue_fixer_wake", "fixer", nowISO)},
		Worker:                  &discoveringWorkerScheduler{repos: repos, nowISO: nowISO, item: schedulerTestQueueItem("queue_worker_wake", "worker", nowISO)},
		SweeperDiscoveryEnabled: boolPtr(false),
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if got := atomic.LoadInt32(&wakes); got != 3 {
		t.Fatalf("scheduler wake requests = %d, want one wake per claim phase that picked up discovered work", got)
	}
}

func TestIndependentClaimPassClaimsQueuedWorkWhileDiscoveryIsBlocked(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-claim-pump.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)

	plannerRunner := &blockingPlannerDiscoveryScheduler{started: make(chan struct{}), release: make(chan struct{})}
	workerRunner := &stubWorkerScheduler{}
	input := defaultSchedulerTickInput{
		Repos:                   repos,
		Now:                     func() time.Time { return now },
		MaxConcurrentRuns:       5,
		ClaimMu:                 &sync.Mutex{},
		AsyncRunner:             immediateSchedulerRunner{},
		Planner:                 plannerRunner,
		Worker:                  workerRunner,
		SweeperDiscoveryEnabled: boolPtr(false),
	}

	tickDone := make(chan error, 1)
	go func() {
		tickDone <- runDefaultSchedulerTick(context.Background(), input)
	}()

	select {
	case <-plannerRunner.started:
	case <-time.After(time.Second):
		t.Fatal("planner discovery did not block")
	}

	for i := 0; i < 5; i++ {
		item := schedulerTestQueueItem(fmt.Sprintf("queue_worker_claim_pump_%d", i), "worker", nowISO)
		if err := repos.Queue.Upsert(context.Background(), item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	claimDone := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if err := runIndependentClaimPass(context.Background(), input); err != nil {
				claimDone <- err
				return
			}
			if workerRunner.processItemCount() == 5 {
				claimDone <- nil
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		claimDone <- fmt.Errorf("timed out waiting for independent claim pass to process all items")
	}()

	select {
	case err := <-claimDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("independent claim pass did not finish before timeout")
	}

	close(plannerRunner.release)
	if err := <-tickDone; err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
}

func TestRunDefaultSchedulerTickReconcilesLiveStaleRunsWhenCapacityIsFull(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-live-reconcile.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
	insertSchedulerProject(t, repos, workingDir, nowISO)
	projectID := "looper"
	staleLoopID := "loop_reconcile_stale"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: staleLoopID, Seq: 2, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", TargetID: stringPtr("pr:nexu-io/looper:42"), Repo: stringPtr("nexu-io/looper"), PRNumber: int64Ptr(42), Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert(stale) error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_reconcile_stale", LoopID: staleLoopID, Status: "running", CurrentStep: stringPtr("review"), StartedAt: oldISO, LastHeartbeatAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert(stale) error = %v", err)
	}
	queuedLoopID := "loop_worker_queued"
	loopTarget := "project:project_1"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: queuedLoopID, Seq: 3, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &loopTarget, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert(queued) error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_after_reconcile", ProjectID: &projectID, LoopID: &queuedLoopID, Type: "worker", TargetType: "project", TargetID: loopTarget, DedupeKey: "worker:loop_worker_queued", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	workerRunner := &stubWorkerScheduler{}
	reconcileCalls := 0
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		AsyncRunner:       immediateSchedulerRunner{},
		ReconcileStaleRuns: func(context.Context) (StaleRunReconcileSummary, error) {
			reconcileCalls++
			if err := interruptRecoveryRun(context.Background(), repos, storage.RunRecord{ID: "run_reconcile_stale", LoopID: staleLoopID}, storage.LoopRecord{ID: staleLoopID, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", Status: "running"}, nowISO, "Interrupted stale running run during stale-run reconciliation"); err != nil {
				return StaleRunReconcileSummary{}, err
			}
			return StaleRunReconcileSummary{Mode: "live", CandidateRuns: 1, InterruptedRuns: 1, LoopsRequeued: 1, RunIDs: []string{"run_reconcile_stale"}, LoopIDs: []string{staleLoopID}}, nil
		},
		Worker: workerRunner,
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if reconcileCalls == 0 {
		t.Fatal("reconcile calls = 0, want at least one live reconcile attempt")
	}
	waitForSchedulerCondition(t, func() bool { return workerRunner.processItemCount() == 1 })
	if workerRunner.processItemCount() != 1 || workerRunner.processedItems[0] != "queue_worker_after_reconcile" {
		t.Fatalf("worker processed items = %#v, want queued item claimed after live reconcile", workerRunner.processedItems)
	}
}

func TestRunDefaultSchedulerTickLogsClaimPhasesAndSlowLanes(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-logs.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)
	logger := &capturingSchedulerLogger{}
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Scheduler.SlowLaneWarnThresholdMS = 1

	plannerRunner := &sleepingPlannerScheduler{delay: 5 * time.Millisecond}
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:                    repos,
		Logger:                   logger,
		Now:                      func() time.Time { return now },
		MaxConcurrentRuns:        1,
		Config:                   &cfg,
		Planner:                  plannerRunner,
		ReviewerDiscoveryEnabled: boolPtr(false),
		FixerDiscoveryEnabled:    boolPtr(false),
		WorkerDiscoveryEnabled:   boolPtr(false),
		SweeperDiscoveryEnabled:  boolPtr(false),
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}

	logger.requireMessage(t, "scheduler tick start")
	logger.requireMessage(t, "scheduler tick end")
	logger.requireMessage(t, "scheduler tick summary")
	logger.requireContextValue(t, "scheduler claim phase", "phase", "pre_discovery")
	logger.requireContextValue(t, "scheduler claim phase", "phase", "post_planner_discovery")
	logger.requireContextValue(t, "scheduler lane start", "lane", "planner discovery")
	logger.requireContextValue(t, "scheduler lane end", "lane", "planner discovery")
	logger.requireContextValue(t, "scheduler lane slow", "lane", "planner discovery")
}

func TestRunScheduledQueueItemsDispatchesEachSupportedType(t *testing.T) {
	t.Parallel()

	queueItems := []storage.QueueItemRecord{{ID: "planner-item", Type: "planner"}, {ID: "reviewer-item", Type: "reviewer"}, {ID: "fixer-item", Type: "fixer"}, {ID: "worker-item", Type: "worker"}, {ID: "sweeper-warn-item", Type: "sweeper:warn"}, {ID: "sweeper-close-item", Type: "sweeper:close"}, {ID: "sweeper-reconcile-item", Type: "sweeper:reconcile"}}
	plannerRunner := &stubPlannerScheduler{}
	reviewerRunner := &stubReviewerScheduler{}
	fixerRunner := &stubFixerScheduler{}
	workerRunner := &stubWorkerScheduler{}
	sweeperRunner := &stubSweeperScheduler{}

	err := runScheduledQueueItems(context.Background(), queueItems, defaultSchedulerTickInput{
		Planner:  plannerRunner,
		Reviewer: reviewerRunner,
		Fixer:    fixerRunner,
		Worker:   workerRunner,
		Sweeper:  sweeperRunner,
	})
	if err != nil {
		t.Fatalf("runScheduledQueueItems() error = %v", err)
	}
	waitForSchedulerCondition(t, func() bool {
		return plannerRunner.processItemCount() == 1 && reviewerRunner.processItemCount() == 1 && fixerRunner.processItemCount() == 1 && workerRunner.processItemCount() == 1 && sweeperRunner.processItemCount() == 3
	})
	if plannerRunner.processItemCount() != 1 || reviewerRunner.processItemCount() != 1 || fixerRunner.processItemCount() != 1 || workerRunner.processItemCount() != 1 || sweeperRunner.processItemCount() != 3 {
		t.Fatalf("processed items = planner:%#v reviewer:%#v fixer:%#v worker:%#v sweeper:%#v, want planner/reviewer/fixer/worker once and sweeper three times", plannerRunner.processedItems, reviewerRunner.processedItems, fixerRunner.processedItems, workerRunner.processedItems, sweeperRunner.processedItems)
	}
}

func TestRunScheduledQueueItemsProcessesItemsConcurrently(t *testing.T) {
	t.Parallel()

	runner := &parallelWorkerScheduler{
		secondStarted: make(chan struct{}),
	}
	err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "worker"}, {Type: "worker"}}, defaultSchedulerTickInput{
		Worker: runner,
	})
	if err != nil {
		t.Fatalf("runScheduledQueueItems() error = %v", err)
	}
	waitForSchedulerCondition(t, func() bool {
		return atomic.LoadInt32(&runner.calls) == 2
	})
	if got := atomic.LoadInt32(&runner.calls); got != 2 {
		t.Fatalf("worker ProcessNext calls = %d, want 2", got)
	}
}

func TestRunScheduledQueueItemsReturnsBeforeClaimedRunsFinish(t *testing.T) {
	t.Parallel()

	runner := &blockingWorkerScheduler{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	returned := make(chan error, 1)
	go func() {
		returned <- runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "worker"}}, defaultSchedulerTickInput{Worker: runner})
	}()

	select {
	case <-runner.started:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("worker ProcessNext did not start")
	}

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("runScheduledQueueItems() error = %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("runScheduledQueueItems() did not return before claimed run finished")
	}

	close(runner.release)
}

func TestRunScheduledQueueItemsRequeuesRetryableSweeperFailure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-sweeper-retry.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_sweeper_warn_retry", ProjectID: &projectID, Type: "sweeper:warn", TargetType: "issue", TargetID: "nexu-io/looper#41", Repo: &repo, DedupeKey: "sweeper:warn:nexu-io/looper#41", Priority: 1, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	runner := sweeper.New(sweeper.Options{Repos: repos, GitHub: schedulerTestSweeperGitHub{viewIssueErr: errors.New("temporary github failure")}, Now: func() time.Time { return now }, Config: &cfg})
	tracker := &schedulerTaskTracker{}

	if err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{queueItem}, defaultSchedulerTickInput{Sweeper: runner, AsyncRunner: tracker}); err != nil {
		t.Fatalf("runScheduledQueueItems() error = %v", err)
	}
	tracker.Wait()

	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want retried queue item")
	}
	if updated.Status != "queued" || updated.Attempts != 1 || updated.FinishedAt != nil {
		t.Fatalf("queue item = %#v, want queued retry with incremented attempts", updated)
	}
	if updated.AvailableAt == nowISO {
		t.Fatalf("queue item available_at = %q, want backoff delay after retry", updated.AvailableAt)
	}
	assertQueueRetryError(t, updated, "temporary github failure")
}

func TestClaimAndRunScheduledQueueItemsBackfillsAvailableSlots(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "backfill.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_sched", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_active", Seq: 1, ProjectID: "project_sched", Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	loopID := "loop_active"
	for _, item := range []storage.QueueItemRecord{
		{ID: "worker_locked_1", LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project_sched", DedupeKey: "d_worker_locked_1", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, LockKey: stringPtr("repo:acme/looper"), CreatedAt: "2026-04-21T07:40:00.000Z", UpdatedAt: nowISO},
		{ID: "worker_locked_2", LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project_sched", DedupeKey: "d_worker_locked_2", Priority: 2, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, LockKey: stringPtr("repo:acme/looper"), CreatedAt: "2026-04-21T07:41:00.000Z", UpdatedAt: nowISO},
		{ID: "worker_fallback", LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project_sched", DedupeKey: "d_worker_fallback", Priority: 3, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: "2026-04-21T07:42:00.000Z", UpdatedAt: nowISO},
	} {
		if err := repos.Queue.Upsert(context.Background(), item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	workerRunner := &stubWorkerScheduler{}
	if _, err := claimAndRunScheduledQueueItems(context.Background(), 2, defaultSchedulerTickInput{
		Repos:  repos,
		Now:    func() time.Time { return now },
		Worker: workerRunner,
	}); err != nil {
		t.Fatalf("claimAndRunScheduledQueueItems() error = %v", err)
	}
	waitForSchedulerCondition(t, func() bool {
		return workerRunner.processItemCount() == 2
	})
	if workerRunner.processItemCount() != 2 {
		t.Fatalf("worker processed items = %#v, want worker_locked_1 and worker_fallback", workerRunner.processedItems)
	}
}

func TestRunScheduledQueueItemsRejectsUnsupportedType(t *testing.T) {
	t.Parallel()

	err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "mystery"}}, defaultSchedulerTickInput{})
	if err == nil || !strings.Contains(err.Error(), "unsupported queue item type") {
		t.Fatalf("runScheduledQueueItems() error = %v, want unsupported queue item type", err)
	}
}

func TestRunScheduledQueueItemsErrorsWhenRunnerMissing(t *testing.T) {
	t.Parallel()

	err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "worker"}}, defaultSchedulerTickInput{})
	if err == nil || !strings.Contains(err.Error(), "worker runner is not configured") {
		t.Fatalf("runScheduledQueueItems() error = %v, want missing worker runner error", err)
	}
}

func TestRunScheduledQueueItemsErrorsWhenSweeperRunnerMissing(t *testing.T) {
	t.Parallel()

	err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "sweeper:warn"}}, defaultSchedulerTickInput{})
	if err == nil || !strings.Contains(err.Error(), "sweeper runner is not configured") {
		t.Fatalf("runScheduledQueueItems() error = %v, want missing sweeper runner error", err)
	}
}

func TestProcessSnapshotQueueItemRetriesTransientCaptureFailure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-retry.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	prNumber := int64(109)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_1", ProjectID: &projectID, Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Snapshotter: stubSnapshotScheduler{err: errors.New("gh timeout")},
	})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want retried queue item")
	}
	if updated.Status != "queued" || updated.Attempts != 1 || updated.FinishedAt != nil {
		t.Fatalf("queue item = %#v, want queued retry with one attempt and no finished_at", updated)
	}
	if updated.LastError == nil || *updated.LastError != "gh timeout" || updated.LastErrorKind == nil || *updated.LastErrorKind != "retryable_transient" {
		t.Fatalf("queue item error = (%v, %v), want retryable gh timeout", updated.LastError, updated.LastErrorKind)
	}
}

func TestProcessSnapshotQueueItemRetriesTransientPersistenceFailure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-upsert-retry.sqlite"), backupDir)
	repos := storage.NewRepositories(&failingSnapshotUpsertQuerier{db: coordinator.DB(), err: errors.New("database is locked")})
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	prNumber := int64(109)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_upsert_1", ProjectID: &projectID, Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Snapshotter: stubSnapshotScheduler{},
	})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want retried queue item")
	}
	if updated.Status != "queued" || updated.Attempts != 1 || updated.FinishedAt != nil {
		t.Fatalf("queue item = %#v, want queued retry with one attempt and no finished_at", updated)
	}
	if updated.LastError == nil || !strings.Contains(*updated.LastError, "database is locked") || updated.LastErrorKind == nil || *updated.LastErrorKind != "retryable_transient" {
		t.Fatalf("queue item error = (%v, %v), want retryable database is locked", updated.LastError, updated.LastErrorKind)
	}
}

func TestProcessSnapshotQueueItemRetriesTransientProjectLookupFailure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-project-lookup-retry.sqlite"), backupDir)
	setupRepos := storage.NewRepositories(coordinator.DB())
	repos := storage.NewRepositories(&failingProjectLookupQuerier{db: coordinator.DB()})
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	prNumber := int64(109)
	if err := setupRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_project_lookup_1", ProjectID: &projectID, Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := setupRepos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Snapshotter: stubSnapshotScheduler{},
	})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want retried queue item")
	}
	if updated.Status != "queued" || updated.Attempts != 1 || updated.FinishedAt != nil {
		t.Fatalf("queue item = %#v, want queued retry with one attempt and no finished_at", updated)
	}
	if updated.LastError == nil || !strings.Contains(*updated.LastError, "get project by id") || updated.LastErrorKind == nil || *updated.LastErrorKind != "retryable_transient" {
		t.Fatalf("queue item error = (%v, %v), want retryable project lookup failure", updated.LastError, updated.LastErrorKind)
	}
}

func TestProcessSnapshotQueueItemRetriesTransientCompletionFailure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-complete-retry.sqlite"), backupDir)
	setupRepos := storage.NewRepositories(coordinator.DB())
	repos := storage.NewRepositories(&failingQueueCompleteQuerier{db: coordinator.DB(), err: errors.New("database is locked")})
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	prNumber := int64(109)
	if err := setupRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_complete_1", ProjectID: &projectID, Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := setupRepos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Snapshotter: stubSnapshotScheduler{},
	})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want retried queue item")
	}
	if updated.Status != "queued" || updated.Attempts != 1 || updated.FinishedAt != nil {
		t.Fatalf("queue item = %#v, want queued retry with one attempt and no finished_at", updated)
	}
	assertQueueRetryError(t, updated, "database is locked")
}

func TestSchedulerAvailableSlotsAccountsForRunningQueueItems(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "slots.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectID := "looper"
	loopID := "loop_worker_running"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	projectTarget := "project:looper"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &projectTarget, Repo: stringPtr("nexu-io/looper"), Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_running", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project:looper", DedupeKey: "worker:running", Priority: 1, Status: "running", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert(running) error = %v", err)
	}

	available, err := schedulerAvailableSlots(context.Background(), repos, 1)
	if err != nil {
		t.Fatalf("schedulerAvailableSlots() error = %v", err)
	}
	if available != 0 {
		t.Fatalf("schedulerAvailableSlots() = %d, want 0", available)
	}
}

func TestRunDefaultSchedulerTickContinuesAfterDiscoveryError(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "errors.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	projectTarget := "project:looper"
	projectID := "looper"
	loopID := "loop_worker_1"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &projectTarget, Repo: stringPtr("nexu-io/looper"), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_1", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectTarget, Repo: stringPtr("nexu-io/looper"), DedupeKey: "worker:loop_worker_1", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	plannerRunner := &stubPlannerScheduler{discoverErr: errors.New("planner boom")}
	workerRunner := &stubWorkerScheduler{}
	err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		Planner:           plannerRunner,
		Worker:            workerRunner,
	})
	if err == nil || !strings.Contains(err.Error(), "planner discovery failed") {
		t.Fatalf("runDefaultSchedulerTick() error = %v, want joined discovery error", err)
	}
	waitForSchedulerCondition(t, func() bool {
		return workerRunner.processItemCount() == 1
	})
	if workerRunner.processItemCount() != 1 {
		t.Fatalf("worker processed items = %#v, want queue processing to continue", workerRunner.processedItems)
	}
}

func TestRunDefaultSchedulerTickAutoFlipsSweeperRepoToDryRunOnTimeoutThreshold(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "sweeper-backpressure.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Sweeper.DryRun = false
	cfg.Roles.Sweeper.Proposer.TimeoutRateDryRunThreshold = 0.5
	cfg.Roles.Sweeper.Proposer.TimeoutRateDryRunMinSamples = 3
	sweeperRunner := &stubSweeperScheduler{stats: sweeper.RepoStats{ProjectID: "looper", Repo: "nexu-io/looper", AgentProposalCount: 3, AgentTimeoutRate: 2.0 / 3.0, AgentTimeouts: 2}}

	err = runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:                    repos,
		Now:                      func() time.Time { return now },
		Sweeper:                  sweeperRunner,
		Config:                   &cfg,
		SweeperDiscoveryEnabled:  boolPtr(true),
		PlannerDiscoveryEnabled:  boolPtr(false),
		ReviewerDiscoveryEnabled: boolPtr(false),
		FixerDiscoveryEnabled:    boolPtr(false),
		WorkerDiscoveryEnabled:   boolPtr(false),
	})
	if err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	stored, err := repos.Projects.GetByID(context.Background(), "looper")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if stored == nil || stored.MetadataJSON == nil || !strings.Contains(*stored.MetadataJSON, `"autoDryRun":true`) {
		t.Fatalf("project metadata = %#v, want autoDryRun backpressure override", stored)
	}
	notification, err := repos.Notifications.GetLatestByDedupe(context.Background(), "in_app", "runtime.sweeper.auto_dry_run:looper:nexu-io/looper")
	if err != nil {
		t.Fatalf("Notifications.GetLatestByDedupe() error = %v", err)
	}
	if notification == nil || notification.Level != "warning" || !strings.Contains(notification.Body, "agent timeout rate") {
		t.Fatalf("notification = %#v, want warning backpressure alert", notification)
	}
	if len(sweeperRunner.issueDiscoverCalls) != 1 || len(sweeperRunner.pullRequestDiscoverCalls) != 1 || len(sweeperRunner.reconcileDiscoverCalls) != 1 {
		t.Fatalf("sweeper discovery calls = issues:%#v prs:%#v reconcile:%#v, want discovery to continue under dry-run", sweeperRunner.issueDiscoverCalls, sweeperRunner.pullRequestDiscoverCalls, sweeperRunner.reconcileDiscoverCalls)
	}
}

func TestGithubCLIAutoPROpeningAvailableRechecksAuthenticatedCLIWithoutConfiguredPath(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	authenticatedPath := filepath.Join(rootDir, "gh-authenticated")
	writeExecutable(t, authenticatedPath, `#!/bin/sh
case "$*" in
  "auth status --hostname github.com")
    exit 0
    ;;
  *)
    printf '{}'
    ;;
esac
`)
	unauthenticatedPath := filepath.Join(rootDir, "gh-unauthenticated")
	writeExecutable(t, unauthenticatedPath, `#!/bin/sh
case "$*" in
  "auth status --hostname github.com")
    exit 1
    ;;
  *)
    printf '{}'
    ;;
esac
`)

	logger := &testLogger{}
	authenticatedGateway := githubinfra.New(githubinfra.Options{GHPath: authenticatedPath, CWD: rootDir})
	unauthenticatedGateway := githubinfra.New(githubinfra.Options{GHPath: unauthenticatedPath, CWD: rootDir})

	if !githubCLIAutoPROpeningAvailable(context.Background(), config.Config{Tools: config.ToolPathsConfig{GHPath: &authenticatedPath}}, authenticatedGateway, logger, "nexu-io/looper", rootDir) {
		t.Fatal("githubCLIAutoPROpeningAvailable() = false, want true for authenticated gh cli")
	}
	if githubCLIAutoPROpeningAvailable(context.Background(), config.Config{Tools: config.ToolPathsConfig{GHPath: &unauthenticatedPath}}, unauthenticatedGateway, logger, "nexu-io/looper", rootDir) {
		t.Fatal("githubCLIAutoPROpeningAvailable() = true, want false for unauthenticated gh cli")
	}
	if !githubCLIAutoPROpeningAvailable(context.Background(), config.Config{}, authenticatedGateway, logger, "nexu-io/looper", rootDir) {
		t.Fatal("githubCLIAutoPROpeningAvailable() = false, want true when gateway can recheck authenticated gh cli without configured path")
	}
}

func TestGithubCLIAutoPROpeningAvailableUsesRepoHostname(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	logPath := filepath.Join(rootDir, "gh.log")
	scriptPath := filepath.Join(rootDir, "gh")
	writeExecutable(t, scriptPath, fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  "auth status --hostname github.example.com")
    exit 0
    ;;
  *)
    exit 1
    ;;
esac
`, logPath))

	logger := &testLogger{}
	gateway := githubinfra.New(githubinfra.Options{GHPath: scriptPath, CWD: rootDir})
	if !githubCLIAutoPROpeningAvailable(context.Background(), config.Config{Tools: config.ToolPathsConfig{GHPath: &scriptPath}}, gateway, logger, "github.example.com/nexu-io/looper", rootDir) {
		t.Fatal("githubCLIAutoPROpeningAvailable() = false, want true for repo host auth")
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if log := string(logBytes); !strings.Contains(log, "auth status --hostname github.example.com") {
		t.Fatalf("gh log = %q, want repo hostname auth status", log)
	}
}

type stubPlannerScheduler struct {
	mu             sync.Mutex
	discoverCalls  []planner.DiscoveryInput
	processClaims  []string
	processedItems []string
	discoverErr    error
	processErr     error
}

type stubCoordinatorScheduler struct {
	mu            sync.Mutex
	discoverCalls []coordinator.DiscoveryInput
	discoverErr   error
}

type immediateSchedulerRunner struct{}

func (immediateSchedulerRunner) Go(fn func()) { fn() }

func insertSchedulerProject(t *testing.T, repos *storage.Repositories, workingDir, nowISO string) {
	t.Helper()
	baseBranch := "main"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
}

func schedulerTestQueueItem(id, queueType, nowISO string) storage.QueueItemRecord {
	projectID := "looper"
	return storage.QueueItemRecord{ID: id, ProjectID: &projectID, Type: queueType, TargetType: "project", TargetID: "project:looper", Repo: stringPtr("nexu-io/looper"), DedupeKey: "dedupe:" + id, Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
}

type enqueueingPlannerScheduler struct {
	stubPlannerScheduler
	repos *storage.Repositories
	items []storage.QueueItemRecord
}

func (s *enqueueingPlannerScheduler) DiscoverIssues(ctx context.Context, input planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	s.stubPlannerScheduler.DiscoverIssues(ctx, input)
	for _, item := range s.items {
		if err := s.repos.Queue.Upsert(ctx, item); err != nil {
			return planner.DiscoveryResult{}, err
		}
	}
	return planner.DiscoveryResult{QueueItems: append([]storage.QueueItemRecord(nil), s.items...)}, nil
}

type enqueueingReviewerScheduler struct {
	stubReviewerScheduler
	repos *storage.Repositories
	item  storage.QueueItemRecord
}

func (s *enqueueingReviewerScheduler) DiscoverPullRequests(ctx context.Context, input reviewer.DiscoveryInput) (reviewer.DiscoveryResult, error) {
	s.stubReviewerScheduler.DiscoverPullRequests(ctx, input)
	if err := s.repos.Queue.Upsert(ctx, s.item); err != nil {
		return reviewer.DiscoveryResult{}, err
	}
	return reviewer.DiscoveryResult{QueueItems: []storage.QueueItemRecord{s.item}}, nil
}

func (s *enqueueingReviewerScheduler) DiscoverPullRequest(ctx context.Context, input reviewer.TargetedDiscoveryInput) (reviewer.DiscoveryResult, error) {
	return reviewer.DiscoveryResult{}, nil
}

type enqueueingFixerScheduler struct {
	stubFixerScheduler
	repos *storage.Repositories
	item  storage.QueueItemRecord
}

func (s *enqueueingFixerScheduler) DiscoverPullRequests(ctx context.Context, input fixer.DiscoveryInput) (fixer.DiscoveryResult, error) {
	s.stubFixerScheduler.DiscoverPullRequests(ctx, input)
	if err := s.repos.Queue.Upsert(ctx, s.item); err != nil {
		return fixer.DiscoveryResult{}, err
	}
	return fixer.DiscoveryResult{QueueItems: []storage.QueueItemRecord{s.item}}, nil
}

func (s *enqueueingFixerScheduler) DiscoverPullRequest(ctx context.Context, input fixer.TargetedDiscoveryInput) (fixer.DiscoveryResult, error) {
	return fixer.DiscoveryResult{}, nil
}

func (s *enqueueingFixerScheduler) DiscoverPullRequestsForBaseBranchUpdate(_ context.Context, _ fixer.BaseBranchDiscoveryInput) (fixer.DiscoveryResult, error) {
	return fixer.DiscoveryResult{}, nil
}

type discoveringWorkerScheduler struct {
	stubWorkerScheduler
	repos  *storage.Repositories
	nowISO string
	item   storage.QueueItemRecord
}

func (s *discoveringWorkerScheduler) DiscoverIssues(ctx context.Context, input worker.DiscoveryInput) (worker.DiscoveryResult, error) {
	s.stubWorkerScheduler.DiscoverIssues(ctx, input)
	if err := s.repos.Queue.Upsert(ctx, s.item); err != nil {
		return worker.DiscoveryResult{}, err
	}
	return worker.DiscoveryResult{QueueItems: []storage.QueueItemRecord{s.item}}, nil
}

type assertingSweeperScheduler struct {
	stubSweeperScheduler
	t              *testing.T
	beforeDiscover func()
	issueCalls     int
}

func (s *assertingSweeperScheduler) DiscoverIssues(ctx context.Context, input sweeper.DiscoveryInput) (sweeper.DiscoveryResult, error) {
	if s.beforeDiscover != nil {
		s.beforeDiscover()
	}
	s.issueCalls++
	return s.stubSweeperScheduler.DiscoverIssues(ctx, input)
}

type queueStatusCheckingPlannerScheduler struct {
	t                    *testing.T
	repos                *storage.Repositories
	queueItemID          string
	checkedClaimedStatus bool
}

type blockingPlannerDiscoveryScheduler struct {
	stubPlannerScheduler
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingPlannerDiscoveryScheduler) DiscoverIssues(ctx context.Context, input planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	s.stubPlannerScheduler.DiscoverIssues(ctx, input)
	s.once.Do(func() { close(s.started) })
	<-s.release
	return planner.DiscoveryResult{}, nil
}

type sleepingPlannerScheduler struct {
	stubPlannerScheduler
	delay time.Duration
}

func (s *sleepingPlannerScheduler) DiscoverIssues(ctx context.Context, input planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	s.stubPlannerScheduler.DiscoverIssues(ctx, input)
	time.Sleep(s.delay)
	return planner.DiscoveryResult{}, nil
}

type capturingSchedulerLogger struct {
	mu      sync.Mutex
	entries []schedulerLogEntry
}

type schedulerLogEntry struct {
	message string
	context map[string]any
}

func (l *capturingSchedulerLogger) Debug(message string, context map[string]any) {
	l.append(message, context)
}
func (l *capturingSchedulerLogger) Info(message string, context map[string]any) {
	l.append(message, context)
}
func (l *capturingSchedulerLogger) Warn(message string, context map[string]any) {
	l.append(message, context)
}
func (l *capturingSchedulerLogger) Error(message string, context map[string]any) {
	l.append(message, context)
}

func (l *capturingSchedulerLogger) append(message string, context map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	copyContext := map[string]any{}
	for key, value := range context {
		copyContext[key] = value
	}
	l.entries = append(l.entries, schedulerLogEntry{message: message, context: copyContext})
}

func (l *capturingSchedulerLogger) requireMessage(t *testing.T, message string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.entries {
		if entry.message == message {
			return
		}
	}
	t.Fatalf("logger messages = %#v, want %q", l.entries, message)
}

func (l *capturingSchedulerLogger) requireContextValue(t *testing.T, message, key string, want any) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.entries {
		if entry.message == message && entry.context[key] == want {
			return
		}
	}
	t.Fatalf("logger entries = %#v, want %q with %s=%v", l.entries, message, key, want)
}

func (s *queueStatusCheckingPlannerScheduler) DiscoverIssues(ctx context.Context, _ planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	s.t.Helper()
	item, err := s.repos.Queue.GetByID(ctx, s.queueItemID)
	if err != nil {
		s.t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if item == nil {
		s.t.Fatal("Queue.GetByID() = nil, want claimed queue item")
	}
	if item.Status != "running" {
		s.t.Fatalf("queue item status during discovery = %q, want running", item.Status)
	}
	if item.ClaimedBy == nil || *item.ClaimedBy != "scheduler" {
		s.t.Fatalf("queue item claimed_by during discovery = %v, want scheduler", item.ClaimedBy)
	}
	s.checkedClaimedStatus = true
	return planner.DiscoveryResult{}, nil
}

func (s *queueStatusCheckingPlannerScheduler) ProcessNext(context.Context, string) (*planner.ProcessResult, error) {
	return nil, nil
}

func (s *queueStatusCheckingPlannerScheduler) ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*planner.ProcessResult, error) {
	return nil, nil

}

func (s *stubPlannerScheduler) DiscoverIssues(_ context.Context, input planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return planner.DiscoveryResult{}, s.discoverErr
}

func (s *stubPlannerScheduler) ProcessNext(_ context.Context, claimedBy string) (*planner.ProcessResult, error) {
	s.mu.Lock()
	s.processClaims = append(s.processClaims, claimedBy)
	s.mu.Unlock()
	return nil, s.processErr
}

func (s *stubPlannerScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*planner.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &planner.ProcessResult{}, s.processErr
}

func (s *stubPlannerScheduler) processClaimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processClaims)
}

func (s *stubPlannerScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

func (s *stubCoordinatorScheduler) DiscoverIssues(_ context.Context, input coordinator.DiscoveryInput) (coordinator.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return coordinator.DiscoveryResult{Ticked: true}, s.discoverErr
}

type stubReviewerScheduler struct {
	mu             sync.Mutex
	discoverCalls  []reviewer.DiscoveryInput
	processClaims  []string
	processedItems []string
	discoverErr    error
	processErr     error
}

func (s *stubReviewerScheduler) DiscoverPullRequests(_ context.Context, input reviewer.DiscoveryInput) (reviewer.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return reviewer.DiscoveryResult{}, s.discoverErr
}

func (s *stubReviewerScheduler) DiscoverPullRequest(_ context.Context, _ reviewer.TargetedDiscoveryInput) (reviewer.DiscoveryResult, error) {
	return reviewer.DiscoveryResult{}, s.discoverErr
}

func (s *stubReviewerScheduler) ProcessNext(_ context.Context, claimedBy string) (*reviewer.ProcessResult, error) {
	s.mu.Lock()
	s.processClaims = append(s.processClaims, claimedBy)
	s.mu.Unlock()
	return nil, s.processErr
}

func (s *stubReviewerScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*reviewer.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &reviewer.ProcessResult{}, s.processErr
}

func (s *stubReviewerScheduler) processClaimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processClaims)
}

func (s *stubReviewerScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

type stubFixerScheduler struct {
	mu             sync.Mutex
	discoverCalls  []fixer.DiscoveryInput
	processClaims  []string
	processedItems []string
	discoverErr    error
	processErr     error
}

func (s *stubFixerScheduler) DiscoverPullRequests(_ context.Context, input fixer.DiscoveryInput) (fixer.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return fixer.DiscoveryResult{}, s.discoverErr
}

func (s *stubFixerScheduler) DiscoverPullRequest(_ context.Context, _ fixer.TargetedDiscoveryInput) (fixer.DiscoveryResult, error) {
	return fixer.DiscoveryResult{}, s.discoverErr
}

func (s *stubFixerScheduler) DiscoverPullRequestsForBaseBranchUpdate(_ context.Context, _ fixer.BaseBranchDiscoveryInput) (fixer.DiscoveryResult, error) {
	return fixer.DiscoveryResult{}, s.discoverErr
}

func (s *stubFixerScheduler) ProcessNext(_ context.Context, claimedBy string) (*fixer.ProcessResult, error) {
	s.mu.Lock()
	s.processClaims = append(s.processClaims, claimedBy)
	s.mu.Unlock()
	return nil, s.processErr
}

func (s *stubFixerScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*fixer.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &fixer.ProcessResult{}, s.processErr
}

func (s *stubFixerScheduler) processClaimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processClaims)
}

func (s *stubFixerScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

type stubWorkerScheduler struct {
	mu             sync.Mutex
	discoverCalls  []worker.DiscoveryInput
	discoverErr    error
	processClaims  []string
	processedItems []string
	processErr     error
}

func (s *stubWorkerScheduler) DiscoverIssues(_ context.Context, input worker.DiscoveryInput) (worker.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return worker.DiscoveryResult{}, s.discoverErr
}

func (s *stubWorkerScheduler) ProcessNext(_ context.Context, claimedBy string) (*worker.ProcessResult, error) {
	s.mu.Lock()
	s.processClaims = append(s.processClaims, claimedBy)
	s.mu.Unlock()
	return nil, s.processErr
}

func (s *stubWorkerScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*worker.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &worker.ProcessResult{}, s.processErr
}

func (s *stubWorkerScheduler) processClaimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processClaims)
}

func (s *stubWorkerScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

type stubSweeperDiscoveryCall struct {
	ProjectID string
	Repo      string
}

type stubSweeperScheduler struct {
	mu                       sync.Mutex
	issueDiscoverCalls       []stubSweeperDiscoveryCall
	pullRequestDiscoverCalls []stubSweeperDiscoveryCall
	reconcileDiscoverCalls   []stubSweeperDiscoveryCall
	processedItems           []string
	discoverErr              error
	processErr               error
	stats                    sweeper.RepoStats
}

type schedulerTestSweeperGitHub struct {
	viewIssueErr error
}

func (s schedulerTestSweeperGitHub) ListOpenIssues(context.Context, githubinfra.ListOpenIssuesInput) ([]githubinfra.IssueSummary, error) {
	return nil, nil
}

func (s schedulerTestSweeperGitHub) ListOpenPullRequests(context.Context, githubinfra.ListOpenPullRequestsInput) ([]githubinfra.PullRequestSummary, error) {
	return nil, nil
}

func (s schedulerTestSweeperGitHub) ViewIssue(context.Context, githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error) {
	return githubinfra.IssueDetail{}, s.viewIssueErr
}

func (s schedulerTestSweeperGitHub) ViewPullRequest(context.Context, githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error) {
	return githubinfra.PullRequestDetail{}, nil
}

func (s schedulerTestSweeperGitHub) ListReviewThreads(context.Context, githubinfra.ListReviewThreadsInput) ([]githubinfra.ReviewThread, error) {
	return nil, nil
}

func (s schedulerTestSweeperGitHub) ListIssueComments(context.Context, githubinfra.ViewIssueInput) ([]githubinfra.CommentInfo, error) {
	return nil, nil
}

func (s schedulerTestSweeperGitHub) ListIssueTimeline(context.Context, githubinfra.IssueTimelineInput) ([]map[string]any, error) {
	return nil, nil
}

func (s schedulerTestSweeperGitHub) ListIssueReactions(context.Context, githubinfra.IssueReactionInput) ([]githubinfra.IssueReaction, error) {
	return nil, nil
}

func (s schedulerTestSweeperGitHub) ListLinkedPullRequests(context.Context, githubinfra.LinkedPullRequestsInput) ([]githubinfra.LinkedPullRequest, error) {
	return nil, nil
}

func (s schedulerTestSweeperGitHub) ListPullRequestReviewState(context.Context, githubinfra.PullRequestReviewStateInput) (githubinfra.PullRequestReviewState, error) {
	return githubinfra.PullRequestReviewState{}, nil
}

func (s schedulerTestSweeperGitHub) CreateIssueComment(context.Context, githubinfra.IssueCommentInput) (githubinfra.IssueCommentResult, error) {
	return githubinfra.IssueCommentResult{}, nil
}

func (s schedulerTestSweeperGitHub) UpdateIssueComment(context.Context, githubinfra.UpdateIssueCommentInput) error {
	return nil
}

func (s schedulerTestSweeperGitHub) CloseIssue(context.Context, githubinfra.CloseIssueInput) error {
	return nil
}

func (s schedulerTestSweeperGitHub) ClosePullRequest(context.Context, githubinfra.ClosePullRequestInput) error {
	return nil
}

func (s schedulerTestSweeperGitHub) AddIssueLabels(context.Context, githubinfra.IssueLabelsInput) error {
	return nil
}

func (s schedulerTestSweeperGitHub) RemoveIssueLabels(context.Context, githubinfra.IssueLabelsInput) error {
	return nil
}

func (s *stubSweeperScheduler) DiscoverIssues(_ context.Context, input sweeper.DiscoveryInput) (sweeper.DiscoveryResult, error) {
	s.mu.Lock()
	s.issueDiscoverCalls = append(s.issueDiscoverCalls, stubSweeperDiscoveryCall{ProjectID: input.ProjectID, Repo: input.Repo})
	s.mu.Unlock()
	return sweeper.DiscoveryResult{}, s.discoverErr
}

func (s *stubSweeperScheduler) DiscoverPullRequests(_ context.Context, input sweeper.DiscoveryInput) (sweeper.DiscoveryResult, error) {
	s.mu.Lock()
	s.pullRequestDiscoverCalls = append(s.pullRequestDiscoverCalls, stubSweeperDiscoveryCall{ProjectID: input.ProjectID, Repo: input.Repo})
	s.mu.Unlock()
	return sweeper.DiscoveryResult{}, s.discoverErr
}

func (s *stubSweeperScheduler) DiscoverReconcile(_ context.Context, input sweeper.DiscoveryInput) (sweeper.DiscoveryResult, error) {
	s.mu.Lock()
	s.reconcileDiscoverCalls = append(s.reconcileDiscoverCalls, stubSweeperDiscoveryCall{ProjectID: input.ProjectID, Repo: input.Repo})
	s.mu.Unlock()
	return sweeper.DiscoveryResult{}, s.discoverErr
}

func (s *stubSweeperScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*sweeper.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &sweeper.ProcessResult{QueueItemID: queueItem.ID, Status: "skipped"}, s.processErr
}

func (s *stubSweeperScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

func (s *stubSweeperScheduler) RepoOperatorStats(_ context.Context, _, _ string, _ int) (sweeper.RepoStats, error) {
	return s.stats, nil
}

type parallelWorkerScheduler struct {
	calls         int32
	secondStarted chan struct{}
}

func (s *parallelWorkerScheduler) ProcessNext(_ context.Context, _ string) (*worker.ProcessResult, error) {
	switch atomic.AddInt32(&s.calls, 1) {
	case 1:
		select {
		case <-s.secondStarted:
			return nil, nil
		case <-time.After(250 * time.Millisecond):
			return nil, errors.New("second worker item did not start concurrently")
		}
	case 2:
		close(s.secondStarted)
	}
	return nil, nil
}

func (s *parallelWorkerScheduler) ProcessClaimedQueueItem(ctx context.Context, _ storage.QueueItemRecord) (*worker.ProcessResult, error) {
	return s.ProcessNext(ctx, "")
}

type blockingWorkerScheduler struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingWorkerScheduler) ProcessNext(_ context.Context, _ string) (*worker.ProcessResult, error) {
	s.once.Do(func() {
		close(s.started)
	})
	<-s.release
	return nil, nil
}

func (s *blockingWorkerScheduler) ProcessClaimedQueueItem(ctx context.Context, _ storage.QueueItemRecord) (*worker.ProcessResult, error) {
	return s.ProcessNext(ctx, "")
}

type stubSnapshotScheduler struct {
	err error
}

func (s stubSnapshotScheduler) CapturePullRequestSnapshot(_ context.Context, input githubinfra.CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	return storage.PullRequestSnapshotRecord{ID: "snapshot_1", ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber, HeadSHA: "head", CapturedAt: input.CapturedAt, CreatedAt: input.CapturedAt}, s.err
}

type failingSnapshotUpsertQuerier struct {
	db  *sql.DB
	err error
}

func (q *failingSnapshotUpsertQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, "INSERT INTO pull_request_snapshots") {
		return nil, q.err
	}
	return q.db.ExecContext(ctx, query, args...)
}

func (q *failingSnapshotUpsertQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *failingSnapshotUpsertQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
}

type failingProjectLookupQuerier struct {
	db *sql.DB
}

func (q *failingProjectLookupQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(ctx, query, args...)
}

func (q *failingProjectLookupQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *failingProjectLookupQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "FROM projects WHERE id") {
		return q.db.QueryRowContext(ctx, `SELECT * FROM missing_projects_table`)
	}
	return q.db.QueryRowContext(ctx, query, args...)
}

type failingQueueCompleteQuerier struct {
	db  *sql.DB
	err error
}

func (q *failingQueueCompleteQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, "SET status = 'completed'") {
		return nil, q.err
	}
	return q.db.ExecContext(ctx, query, args...)
}

func (q *failingQueueCompleteQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *failingQueueCompleteQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
}

func assertQueueRetryError(t *testing.T, item *storage.QueueItemRecord, message string) {
	t.Helper()
	lastError := "<nil>"
	if item.LastError != nil {
		lastError = *item.LastError
	}
	lastErrorKind := "<nil>"
	if item.LastErrorKind != nil {
		lastErrorKind = *item.LastErrorKind
	}
	if item.LastError == nil || !strings.Contains(*item.LastError, message) || item.LastErrorKind == nil || *item.LastErrorKind != "retryable_transient" {
		t.Fatalf("queue item error = (%s, %s), want retryable %s", lastError, lastErrorKind, message)
	}
}

func waitForSchedulerCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition not satisfied before timeout")
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

type stubAgentExecution struct {
	result agent.Result
	err    error
}

func (s stubAgentExecution) Wait(context.Context) (agent.Result, error) {
	return s.result, s.err
}

func (s stubAgentExecution) Kill(string) error {
	return nil
}
