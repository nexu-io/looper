package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestCancelledClaimFinalizeContextIsBounded(t *testing.T) {
	t.Parallel()

	// Expired parent deadline must not become unbounded finalize, and must not
	// leave finalize already cancelled.
	parent, parentCancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer parentCancel()
	time.Sleep(2 * time.Millisecond)
	if parent.Err() == nil {
		t.Fatal("parent context should already be expired")
	}

	finalizeCtx, cancel := newCancelledClaimFinalizeContext(parent)
	defer cancel()

	deadline, ok := finalizeCtx.Deadline()
	if !ok {
		t.Fatal("finalize context missing deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > cancelledClaimFinalizeTimeout {
		t.Fatalf("remaining = %v, want (0, %v]", remaining, cancelledClaimFinalizeTimeout)
	}
	if err := finalizeCtx.Err(); err != nil {
		t.Fatalf("finalize ctx.Err() = %v, want nil despite expired parent", err)
	}

	nilParent, nilCancel := newCancelledClaimFinalizeContext(nil)
	defer nilCancel()
	if _, ok := nilParent.Deadline(); !ok {
		t.Fatal("nil-parent finalize context missing deadline")
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
// BindClaim refuse handling, finalizeCancelledClaim's MarkRetryIfRunning must
// no-op (zero rows) and leave the row cancelled — stop must not resurrect
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
