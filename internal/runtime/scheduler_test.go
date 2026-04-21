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
	if len(workerRunner.processClaims) != 1 {
		t.Fatalf("worker process claims = %#v, want one queued worker run", workerRunner.processClaims)
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
	if len(plannerRunner.processClaims) != 1 || len(reviewerRunner.processClaims) != 1 || len(fixerRunner.processClaims) != 1 || len(workerRunner.processClaims) != 1 {
		t.Fatalf("process claims = planner:%#v reviewer:%#v fixer:%#v worker:%#v, want one each", plannerRunner.processClaims, reviewerRunner.processClaims, fixerRunner.processClaims, workerRunner.processClaims)
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
	if got := atomic.LoadInt32(&runner.calls); got != 2 {
		t.Fatalf("worker ProcessNext calls = %d, want 2", got)
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
	if len(workerRunner.processClaims) != 1 {
		t.Fatalf("worker process claims = %#v, want queue processing to continue", workerRunner.processClaims)
	}
}

type stubPlannerScheduler struct {
	mu            sync.Mutex
	discoverCalls []planner.DiscoveryInput
	processClaims []string
	discoverErr   error
	processErr    error
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

type stubReviewerScheduler struct {
	mu            sync.Mutex
	discoverCalls []reviewer.DiscoveryInput
	processClaims []string
	discoverErr   error
	processErr    error
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

type stubFixerScheduler struct {
	mu            sync.Mutex
	discoverCalls []fixer.DiscoveryInput
	processClaims []string
	discoverErr   error
	processErr    error
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

type stubWorkerScheduler struct {
	mu            sync.Mutex
	processClaims []string
	processErr    error
}

func (s *stubWorkerScheduler) ProcessNext(_ context.Context, claimedBy string) (*worker.ProcessResult, error) {
	s.mu.Lock()
	s.processClaims = append(s.processClaims, claimedBy)
	s.mu.Unlock()
	return nil, s.processErr
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
