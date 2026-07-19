package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

func TestAdmitOperationRefusesWhenAdmissionClosed(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	_ = reg.BeginShutdown("test stop")
	_, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if !errors.Is(err, ErrOperationAdmissionClosed) {
		t.Fatalf("AdmitOperation error = %v, want ErrOperationAdmissionClosed", err)
	}
}

func TestBindClaimRefusesAfterShutdownWithoutStartingPermit(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}

	// Mark admission closed without BeginShutdown wait (bound finalizer drain).
	// BindClaim must return an explicit refuse and retain ownership until Release.
	reg.mu.Lock()
	reg.admissionClosed = true
	reg.shutdownReason = "drain"
	reg.mu.Unlock()

	loopID := "loop-bind-refuse"
	item := storage.QueueItemRecord{ID: "qi-bind-refuse", Type: "worker", LoopID: &loopID, Status: "running"}
	permit, err := lease.BindClaim(item)
	if !errors.Is(err, ErrOperationLeaseCancelled) {
		t.Fatalf("BindClaim error = %v, want ErrOperationLeaseCancelled", err)
	}
	if permit.Valid() {
		t.Fatal("BindClaim must not return a valid permit after cancel")
	}
	if !reg.OwnsQueueClaim(item.ID) {
		t.Fatal("cancelled bind must retain ownership until durable finalize + Release")
	}
	lease.Release()
	if reg.OwnsQueueClaim(item.ID) {
		t.Fatal("Release must drop ownership")
	}
}

func TestBindClaimRefusesWhenLoopStopping(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}
	release, err := reg.BeginLoopStop("loop-stop", "halt")
	if err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	defer release()

	loopID := "loop-stop"
	item := storage.QueueItemRecord{ID: "qi-loop-stop", Type: "worker", LoopID: &loopID, Status: "running"}
	permit, err := lease.BindClaim(item)
	if !errors.Is(err, ErrOperationLeaseCancelled) {
		t.Fatalf("BindClaim error = %v, want ErrOperationLeaseCancelled", err)
	}
	if permit.Valid() {
		t.Fatal("permit must be invalid when loop is stopping")
	}
	if !lease.Owns(item.ID) {
		t.Fatal("lease must own claim until Release after cancel-bind")
	}
	lease.Release()
}

func TestClaimMissReleasesOperationLeaseImmediately(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	reg := NewActiveExecutionRegistry()

	claimed, err := claimAndRunScheduledQueueItems(context.Background(), 1, defaultSchedulerTickInput{
		Repos:          repos,
		Now:            func() time.Time { return now },
		OperationOwner: reg,
		AsyncRunner:    immediateSchedulerRunner{},
		Worker:         &stubWorkerScheduler{},
	})
	if err != nil {
		t.Fatalf("claimAndRunScheduledQueueItems: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed = %#v, want empty on miss", claimed)
	}
	if reg.PendingOperationCount() != 0 || reg.BoundOperationCount() != 0 {
		t.Fatalf("pending=%d bound=%d, want both 0 after claim miss release", reg.PendingOperationCount(), reg.BoundOperationCount())
	}
}

func TestClaimOwnershipSpanAndFinalizeBeforeRelease(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	projectID := "project_own_span"
	loopID := "loop_own_span"
	queueID := "queue_own_span"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Own", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:own_span", Priority: storage.QueuePriorityWorker, Status: "queued",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	reg := NewActiveExecutionRegistry()
	var sawOwnedDuringProcess atomic.Bool
	worker := &ownershipProbeWorker{
		onProcess: func(item storage.QueueItemRecord) {
			if reg.OwnsQueueClaim(item.ID) {
				sawOwnedDuringProcess.Store(true)
			}
			if err := repos.Queue.Complete(context.Background(), item.ID, nowISO); err != nil {
				t.Errorf("Complete during process: %v", err)
			}
		},
	}

	claimed, err := claimAndRunScheduledQueueItems(context.Background(), 1, defaultSchedulerTickInput{
		Repos:          repos,
		Now:            func() time.Time { return now },
		OperationOwner: reg,
		AsyncRunner:    immediateSchedulerRunner{},
		Worker:         worker,
	})
	if err != nil {
		t.Fatalf("claimAndRunScheduledQueueItems: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != queueID {
		t.Fatalf("claimed = %#v, want %s", claimed, queueID)
	}
	if !sawOwnedDuringProcess.Load() {
		t.Fatal("while daemon-live running claim must be owned by operation lease during processor")
	}
	if reg.BoundOperationCount() != 0 {
		t.Fatalf("bound ops = %d, want 0 after durable finalize + Release", reg.BoundOperationCount())
	}
	got, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status != "completed" {
		t.Fatalf("queue status = %#v, want completed", got)
	}
}

func TestStopBindRaceNeverStartsProcessor(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	projectID := "project_stop_bind"
	loopID := "loop_stop_bind"
	queueID := "queue_stop_bind"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "StopBind", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 2, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:stop_bind", Priority: storage.QueuePriorityWorker, Status: "queued",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	reg := NewActiveExecutionRegistry()
	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}
	item, err := repos.Queue.ClaimNextOfType(context.Background(), nowISO, "scheduler", "worker")
	if err != nil || item == nil {
		t.Fatalf("ClaimNextOfType = (%#v, %v)", item, err)
	}
	release, err := reg.BeginLoopStop(loopID, "stop before bind")
	if err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	defer release()

	permit, bindErr := lease.BindClaim(*item)
	if !errors.Is(bindErr, ErrOperationLeaseCancelled) {
		t.Fatalf("BindClaim = %v, want ErrOperationLeaseCancelled", bindErr)
	}
	if permit.Valid() {
		t.Fatal("processor must not receive a valid permit after cancelled lease")
	}

	if err := finalizeCancelledClaim(context.Background(), *item, defaultSchedulerTickInput{
		Repos: repos,
		Now:   func() time.Time { return now },
	}, func() time.Time { return now }); err != nil {
		t.Fatalf("finalizeCancelledClaim: %v", err)
	}
	lease.Release()

	got, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status != "queued" {
		t.Fatalf("after cancelled bind = %#v, want requeued", got)
	}
	if reg.OwnsQueueClaim(queueID) {
		t.Fatal("ownership must drop after durable requeue + Release")
	}
}

// Contract: after BindClaim returns a valid permit, BeginLoopStop may cancel
// the lease before the processor goroutine starts. The permit remains Valid(),
// but runOwnedQueueClaims must requeue under retained ownership and must not
// invoke the processor with the detached dispatch context.
func TestPostBindStopNeverStartsProcessor(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	projectID := "project_post_bind_stop"
	loopID := "loop_post_bind_stop"
	queueID := "queue_post_bind_stop"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "PostBindStop", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 3, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:post_bind_stop", Priority: storage.QueuePriorityWorker, Status: "queued",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	reg := NewActiveExecutionRegistry()
	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}
	item, err := repos.Queue.ClaimNextOfType(context.Background(), nowISO, "scheduler", "worker")
	if err != nil || item == nil {
		t.Fatalf("ClaimNextOfType = (%#v, %v)", item, err)
	}
	permit, bindErr := lease.BindClaim(*item)
	if bindErr != nil || !permit.Valid() {
		t.Fatalf("BindClaim = (%#v, %v), want valid permit", permit, bindErr)
	}

	// Post-bind stop: lease is cancelled while permit remains Valid().
	release, err := reg.BeginLoopStop(loopID, "stop after bind before processor")
	if err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	defer release()
	if lease.Context().Err() == nil {
		t.Fatal("lease context must be cancelled after BeginLoopStop")
	}
	if !permit.Valid() {
		t.Fatal("permit remains Valid() after post-bind stop; that is the race under test")
	}

	var started atomic.Bool
	worker := &ownershipProbeWorker{
		onProcess: func(storage.QueueItemRecord) {
			started.Store(true)
		},
	}
	// Detached dispatch ctx matches production: scheduler cancel is stripped so
	// finalize can still write; the gate must be the operation lease instead.
	dispatchCtx := context.WithoutCancel(context.Background())
	if err := runOwnedQueueClaims(dispatchCtx, []ownedQueueClaim{{
		item:   *item,
		lease:  lease,
		permit: permit,
	}}, defaultSchedulerTickInput{
		Repos:          repos,
		Now:            func() time.Time { return now },
		OperationOwner: reg,
		AsyncRunner:    immediateSchedulerRunner{},
		Worker:         worker,
	}); err != nil {
		t.Fatalf("runOwnedQueueClaims: %v", err)
	}
	if started.Load() {
		t.Fatal("processor must not start after post-bind lease cancel")
	}
	got, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status != "queued" {
		t.Fatalf("after post-bind stop = %#v, want requeued", got)
	}
	if reg.OwnsQueueClaim(queueID) {
		t.Fatal("ownership must drop after durable requeue + Release")
	}
}

// Contract: AsyncRunner may delay start until after BeginLoopStop cancels the
// bound lease. The launch-time permit check saw a live lease; the deferred run
// must revalidate lease.Context() and requeue without invoking the processor.
func TestPostBindStopDuringAsyncLaunchNeverStartsProcessor(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	projectID := "project_async_post_bind"
	loopID := "loop_async_post_bind"
	queueID := "queue_async_post_bind"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "AsyncPostBind", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 4, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:async_post_bind", Priority: storage.QueuePriorityWorker, Status: "queued",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	reg := NewActiveExecutionRegistry()
	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}
	item, err := repos.Queue.ClaimNextOfType(context.Background(), nowISO, "scheduler", "worker")
	if err != nil || item == nil {
		t.Fatalf("ClaimNextOfType = (%#v, %v)", item, err)
	}
	permit, bindErr := lease.BindClaim(*item)
	if bindErr != nil || !permit.Valid() {
		t.Fatalf("BindClaim = (%#v, %v), want valid permit", permit, bindErr)
	}

	var stopRelease func()
	t.Cleanup(func() {
		if stopRelease != nil {
			stopRelease()
		}
	})
	async := callbackSchedulerRunner{before: func() {
		release, stopErr := reg.BeginLoopStop(loopID, "stop during async launch")
		if stopErr != nil {
			t.Errorf("BeginLoopStop during async: %v", stopErr)
			return
		}
		stopRelease = release
	}}

	var started atomic.Bool
	worker := &ownershipProbeWorker{
		onProcess: func(storage.QueueItemRecord) {
			started.Store(true)
		},
	}
	if err := runOwnedQueueClaims(context.Background(), []ownedQueueClaim{{
		item:   *item,
		lease:  lease,
		permit: permit,
	}}, defaultSchedulerTickInput{
		Repos:          repos,
		Now:            func() time.Time { return now },
		OperationOwner: reg,
		AsyncRunner:    async,
		Worker:         worker,
	}); err != nil {
		t.Fatalf("runOwnedQueueClaims: %v", err)
	}
	if started.Load() {
		t.Fatal("processor must not start when lease is cancelled between schedule and run")
	}
	got, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status != "queued" {
		t.Fatalf("after async post-bind stop = %#v, want requeued", got)
	}
	if reg.OwnsQueueClaim(queueID) {
		t.Fatal("ownership must drop after durable requeue + Release")
	}
}

func TestFinalizePersistFailureRetainsOwnershipAndDegrades(t *testing.T) {
	t.Parallel()

	reg := NewActiveExecutionRegistry()
	var degraded atomic.Bool
	reg.SetOnHardPersistFailure(func(error) { degraded.Store(true) })

	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}
	loopID := "loop-fin-fail"
	item := storage.QueueItemRecord{ID: "qi-fin-fail", Type: "worker", LoopID: &loopID, Status: "running"}
	permit, err := lease.BindClaim(item)
	if err != nil || !permit.Valid() {
		t.Fatalf("BindClaim = (%#v, %v)", permit, err)
	}
	if !reg.OwnsQueueClaim(item.ID) {
		t.Fatal("expected ownership after bind")
	}

	reg.ReportHardPersistFailure(errors.New("sqlite disk full"))
	if !degraded.Load() {
		t.Fatal("hard finalize failure must invoke degrade hook")
	}
	if !reg.OwnsQueueClaim(item.ID) {
		t.Fatal("ownership must be retained after finalize failure")
	}
	if reg.BoundOperationCount() != 1 {
		t.Fatalf("bound ops = %d, want 1 retained", reg.BoundOperationCount())
	}
}

func TestRunnerErrorStillFinalizesThenReleases(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	projectID := "project_runner_err"
	loopID := "loop_runner_err"
	queueID := "queue_runner_err"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "RunnerErr", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 3, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:runner_err", Priority: storage.QueuePriorityWorker, Status: "queued",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	reg := NewActiveExecutionRegistry()
	workerStub := &stubWorkerScheduler{processErr: errors.New("runner boom")}
	_, err = claimAndRunScheduledQueueItems(context.Background(), 1, defaultSchedulerTickInput{
		Repos:          repos,
		Now:            func() time.Time { return now },
		OperationOwner: reg,
		AsyncRunner:    immediateSchedulerRunner{},
		Worker:         workerStub,
	})
	if err != nil {
		t.Fatalf("claimAndRunScheduledQueueItems: %v", err)
	}
	got, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status == "running" {
		t.Fatalf("after runner error = %#v, want durable finalize (not running)", got)
	}
	if reg.BoundOperationCount() != 0 {
		t.Fatalf("bound ops = %d, want 0 after typed finalize + Release", reg.BoundOperationCount())
	}
}

func TestFinalizeCancelledClaimUsesDetachedContext(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	projectID := "project_fin_cancel_ctx"
	loopID := "loop_fin_cancel_ctx"
	queueID := "queue_fin_cancel_ctx"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "FinCancel", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 4, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:fin_cancel_ctx", Priority: storage.QueuePriorityWorker, Status: "running",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	// Simulate BeginShutdown cancelling the scheduler context before requeue.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	item := storage.QueueItemRecord{ID: queueID, Attempts: 0, Status: "running"}
	if err := finalizeCancelledClaim(ctx, item, defaultSchedulerTickInput{
		Repos: repos,
		Now:   func() time.Time { return now },
	}, func() time.Time { return now }); err != nil {
		t.Fatalf("finalizeCancelledClaim with cancelled ctx: %v", err)
	}
	got, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status != "queued" {
		t.Fatalf("after finalize with cancelled ctx = %#v, want requeued", got)
	}
}

// Contract: when CancelByLoop terminalizes a claim after ClaimNext* and before
// BindClaim refuse handling, finalizeCancelledClaim must not MarkRetry the
// cancelled row back to queued (stop must not resurrect cancelled work).
func TestFinalizeCancelledClaimPreservesExternalCancellation(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	projectID := "project_fin_cancel_term"
	loopID := "loop_fin_cancel_term"
	queueID := "queue_fin_cancel_term"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "FinCancelTerm", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 6, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "stopping", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:fin_cancel_term", Priority: storage.QueuePriorityWorker, Status: "running",
		AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	reg := NewActiveExecutionRegistry()
	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}
	item := storage.QueueItemRecord{ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", Status: "running", Attempts: 1}
	// Claim is bound ownership-wise only after BindClaim; here CancelByLoop races
	// before bind, then BindClaim refuses — the refuse path calls finalizeCancelledClaim.
	reason := "loop terminated"
	if _, err := repos.Queue.CancelByLoop(context.Background(), loopID, nowISO, &reason); err != nil {
		t.Fatalf("CancelByLoop: %v", err)
	}
	release, err := reg.BeginLoopStop(loopID, "terminal stop")
	if err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	defer release()
	permit, bindErr := lease.BindClaim(item)
	if !errors.Is(bindErr, ErrOperationLeaseCancelled) {
		t.Fatalf("BindClaim = %v, want ErrOperationLeaseCancelled", bindErr)
	}
	if permit.Valid() {
		t.Fatal("processor must not receive a valid permit after cancelled lease")
	}

	if err := finalizeCancelledClaim(context.Background(), item, defaultSchedulerTickInput{
		Repos: repos,
		Now:   func() time.Time { return now },
	}, func() time.Time { return now }); err != nil {
		t.Fatalf("finalizeCancelledClaim after external cancel: %v", err)
	}
	lease.Release()

	got, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status != "cancelled" {
		t.Fatalf("after refused bind finalize = %#v, want cancelled (not resurrected to queued)", got)
	}
	if reg.OwnsQueueClaim(queueID) {
		t.Fatal("ownership must drop after terminal cancel observed + Release")
	}
}

func TestDurableCompleteClaimReleasesWhenExternallyCancelled(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	t.Cleanup(func() { _ = coordinator.Close() })
	repos := storage.NewRepositories(coordinator.DB())
	projectID := "project_parked_cancel"
	loopID := "loop_parked_cancel"
	queueID := "queue_parked_cancel"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "ParkedCancel", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	// Parked loop status (human_takeover) matches schedulerLoopParked observation.
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 5, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "human_takeover", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:parked_cancel", Priority: storage.QueuePriorityWorker, Status: "running",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	reg := NewActiveExecutionRegistry()
	lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}
	item := storage.QueueItemRecord{ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", Status: "running"}
	permit, err := lease.BindClaim(item)
	if err != nil || !permit.Valid() {
		t.Fatalf("BindClaim = (%#v, %v)", permit, err)
	}

	// Concurrent pause/terminate: CancelByLoop moves the claim to cancelled.
	reason := "human takeover"
	if _, err := repos.Queue.CancelByLoop(context.Background(), loopID, nowISO, &reason); err != nil {
		t.Fatalf("CancelByLoop: %v", err)
	}

	// Parked path must durable-complete (or observe already terminal) then Release.
	if err := durableCompleteClaim(context.Background(), item, defaultSchedulerTickInput{
		Repos: repos,
		Now:   func() time.Time { return now },
	}, func() time.Time { return now }); err != nil {
		t.Fatalf("durableCompleteClaim after external cancel: %v", err)
	}
	lease.Release()
	if reg.OwnsQueueClaim(queueID) {
		t.Fatal("lease must release after externally cancelled claim is observed terminal")
	}
	got, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status != "cancelled" {
		t.Fatalf("queue status = %#v, want cancelled", got)
	}
}

func TestReleaseAndBoundCancelShareLockOrder(t *testing.T) {
	t.Parallel()
	// Concurrent Release (finalize) with BeginLoopStop bound-op scan must not
	// deadlock under the registry-then-lease lock order.
	reg := NewActiveExecutionRegistry()
	const n = 32
	leases := make([]OperationLease, 0, n)
	loopID := "loop-lock-order"
	for i := 0; i < n; i++ {
		lease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
		if err != nil {
			t.Fatalf("AdmitOperation: %v", err)
		}
		item := storage.QueueItemRecord{
			ID:     "qi-lock-" + strconv.Itoa(i),
			Type:   "worker",
			LoopID: &loopID,
			Status: "running",
		}
		permit, err := lease.BindClaim(item)
		if err != nil || !permit.Valid() {
			t.Fatalf("BindClaim: %v permit=%v", err, permit.Valid())
		}
		leases = append(leases, lease)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, lease := range leases {
			lease.Release()
		}
	}()
	// Interleave stop scans that take r.mu then l.mu.
	for i := 0; i < n; i++ {
		release, err := reg.BeginLoopStop("loop-lock-order", "halt")
		if err != nil {
			t.Fatalf("BeginLoopStop: %v", err)
		}
		release()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Release goroutine deadlocked with BeginLoopStop bound-op scan")
	}
	if reg.BoundOperationCount() != 0 {
		t.Fatalf("bound ops = %d, want 0 after all Release", reg.BoundOperationCount())
	}
}

// callbackSchedulerRunner invokes before (if set) then runs fn synchronously.
// Used to inject BeginLoopStop between schedule and processor start.
type callbackSchedulerRunner struct {
	before func()
}

func (r callbackSchedulerRunner) Go(fn func()) {
	if r.before != nil {
		r.before()
	}
	fn()
}

// ownershipProbeWorker implements workerScheduler for ownership-span contract tests.
type ownershipProbeWorker struct {
	onProcess func(storage.QueueItemRecord)
}

func (s *ownershipProbeWorker) ProcessNext(context.Context, string) (*worker.ProcessResult, error) {
	return nil, nil
}

func (s *ownershipProbeWorker) ProcessClaimedQueueItem(_ context.Context, item storage.QueueItemRecord) (*worker.ProcessResult, error) {
	if s.onProcess != nil {
		s.onProcess(item)
	}
	return &worker.ProcessResult{}, nil
}
