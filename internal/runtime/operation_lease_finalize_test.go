package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

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

// Contract: when CancelByLoop terminalizes a claim after ClaimNext* and before
// BindClaim refuse handling, finalizeCancelledClaim's status-guarded MarkRetry
// must no-op (zero rows) and leave the row cancelled — stop must not resurrect
// cancelled work even if a pre-read would have raced.
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
