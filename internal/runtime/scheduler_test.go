package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/fixer"
	"github.com/powerformer/looper/internal/planner"
	"github.com/powerformer/looper/internal/reviewer"
	"github.com/powerformer/looper/internal/storage"
	"github.com/powerformer/looper/internal/worker"
)

func TestRunDefaultSchedulerTickDiscoversStoredProjectsAndProcessesQueue(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectMetadata := `{"repo":"powerformer/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	projectTarget := "project:looper"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_1", Seq: 1, ProjectID: "looper", Type: "worker", TargetType: "project", TargetID: &projectTarget, Repo: stringPtr("powerformer/looper"), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "looper"
	loopID := "loop_worker_1"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_1", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectTarget, Repo: stringPtr("powerformer/looper"), DedupeKey: "worker:loop_worker_1", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	plannerRunner := &stubPlannerScheduler{}
	reviewerRunner := &stubReviewerScheduler{}
	fixerRunner := &stubFixerScheduler{}
	workerRunner := &stubWorkerScheduler{}

	err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		Planner:           plannerRunner,
		Reviewer:          reviewerRunner,
		Fixer:             fixerRunner,
		Worker:            workerRunner,
	})
	if err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if len(plannerRunner.discoverCalls) != 1 || plannerRunner.discoverCalls[0].ProjectID != "looper" || plannerRunner.discoverCalls[0].Repo != "powerformer/looper" {
		t.Fatalf("planner discover calls = %#v, want stored project discovery", plannerRunner.discoverCalls)
	}
	if len(reviewerRunner.discoverCalls) != 1 || reviewerRunner.discoverCalls[0].Repo != "powerformer/looper" {
		t.Fatalf("reviewer discover calls = %#v, want stored project repo", reviewerRunner.discoverCalls)
	}
	if len(fixerRunner.discoverCalls) != 1 || fixerRunner.discoverCalls[0].Repo != "powerformer/looper" {
		t.Fatalf("fixer discover calls = %#v, want stored project repo", fixerRunner.discoverCalls)
	}
	waitForSchedulerCondition(t, func() bool {
		return workerRunner.processItemCount() == 1
	})
	if workerRunner.processItemCount() != 1 {
		t.Fatalf("worker processed items = %#v, want one queued worker run", workerRunner.processedItems)
	}
}

func TestRunScheduledQueueItemsDispatchesEachSupportedType(t *testing.T) {
	t.Parallel()

	queueItems := []storage.QueueItemRecord{{Type: "planner"}, {Type: "reviewer"}, {Type: "fixer"}, {Type: "worker"}}
	plannerRunner := &stubPlannerScheduler{}
	reviewerRunner := &stubReviewerScheduler{}
	fixerRunner := &stubFixerScheduler{}
	workerRunner := &stubWorkerScheduler{}

	err := runScheduledQueueItems(context.Background(), queueItems, defaultSchedulerTickInput{
		Planner:  plannerRunner,
		Reviewer: reviewerRunner,
		Fixer:    fixerRunner,
		Worker:   workerRunner,
	})
	if err != nil {
		t.Fatalf("runScheduledQueueItems() error = %v", err)
	}
	waitForSchedulerCondition(t, func() bool {
		return plannerRunner.processItemCount() == 1 && reviewerRunner.processItemCount() == 1 && fixerRunner.processItemCount() == 1 && workerRunner.processItemCount() == 1
	})
	if plannerRunner.processItemCount() != 1 || reviewerRunner.processItemCount() != 1 || fixerRunner.processItemCount() != 1 || workerRunner.processItemCount() != 1 {
		t.Fatalf("processed items = planner:%#v reviewer:%#v fixer:%#v worker:%#v, want one each", plannerRunner.processedItems, reviewerRunner.processedItems, fixerRunner.processedItems, workerRunner.processedItems)
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
	if err := claimAndRunScheduledQueueItems(context.Background(), 2, defaultSchedulerTickInput{
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
	projectMetadata := `{"repo":"powerformer/looper"}`
	projectTarget := "project:looper"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &projectTarget, Repo: stringPtr("powerformer/looper"), Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
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
	projectMetadata := `{"repo":"powerformer/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	projectTarget := "project:looper"
	projectID := "looper"
	loopID := "loop_worker_1"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &projectTarget, Repo: stringPtr("powerformer/looper"), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_1", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectTarget, Repo: stringPtr("powerformer/looper"), DedupeKey: "worker:loop_worker_1", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
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

type stubPlannerScheduler struct {
	mu             sync.Mutex
	discoverCalls  []planner.DiscoveryInput
	processClaims  []string
	processedItems []string
	discoverErr    error
	processErr     error
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
	processClaims  []string
	processedItems []string
	processErr     error
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
