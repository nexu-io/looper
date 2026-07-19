package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestAmbiguousClaimRecoveryContextIsBounded(t *testing.T) {
	t.Parallel()

	// Expired parent deadline must not become unbounded recovery, and must not
	// leave recovery already cancelled.
	parent, parentCancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer parentCancel()
	time.Sleep(2 * time.Millisecond)
	if parent.Err() == nil {
		t.Fatal("parent context should already be expired")
	}

	recoverCtx, cancel := newAmbiguousClaimRecoveryContext(parent)
	defer cancel()

	deadline, ok := recoverCtx.Deadline()
	if !ok {
		t.Fatal("recovery context missing deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > ambiguousClaimRecoveryTimeout {
		t.Fatalf("remaining = %v, want (0, %v]", remaining, ambiguousClaimRecoveryTimeout)
	}
	if err := recoverCtx.Err(); err != nil {
		t.Fatalf("recovery ctx.Err() = %v, want nil despite expired parent", err)
	}

	nilParent, nilCancel := newAmbiguousClaimRecoveryContext(nil)
	defer nilCancel()
	if _, ok := nilParent.Deadline(); !ok {
		t.Fatal("nil-parent recovery context missing deadline")
	}
}

func TestRecoverAmbiguousCancelledClaimFindsUnownedRunning(t *testing.T) {
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
	projectID := "project_ambig_recover"
	loopID := "loop_ambig_recover"
	queueID := "queue_ambig_recover"
	claimedBy := "scheduler"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Ambig", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:ambig_recover", Priority: storage.QueuePriorityWorker, Status: "running",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, ClaimedBy: &claimedBy, ClaimedAt: &nowISO,
		StartedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	input := defaultSchedulerTickInput{Repos: repos, Now: func() time.Time { return now }}
	recovered, err := recoverAmbiguousCancelledClaim(context.Background(), input, nowISO, nil)
	if err != nil {
		t.Fatalf("recoverAmbiguousCancelledClaim: %v", err)
	}
	if recovered == nil || recovered.ID != queueID {
		t.Fatalf("recovered = %#v, want %s", recovered, queueID)
	}

	// Already-owned claim must be skipped (batch sibling).
	recovered, err = recoverAmbiguousCancelledClaim(context.Background(), input, nowISO, []ownedQueueClaim{
		{item: storage.QueueItemRecord{ID: queueID}},
	})
	if err != nil {
		t.Fatalf("recover with owned: %v", err)
	}
	if recovered != nil {
		t.Fatalf("recovered = %#v, want nil when already owned", recovered)
	}
}

// Contract: ListRunningClaimedBy(claimed_by=scheduler, claimed_at=nowISO) can
// return still-running claims from an earlier batch that shared the same
// millisecond timestamp. Recovery must skip registry-owned claims so it never
// adopts a live-owned item or multi-match degrades while the new claim stays unbound.
func TestRecoverAmbiguousCancelledClaimSkipsRegistryOwned(t *testing.T) {
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
	projectID := "project_ambig_reg"
	claimedBy := "scheduler"
	// Earlier batch claim still running under a live operation lease.
	priorLoop := "loop_ambig_prior"
	priorID := "queue_ambig_prior"
	// Current ambiguous claim: durable running, not yet bound.
	currentLoop := "loop_ambig_current"
	currentID := "queue_ambig_current"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "AmbigReg", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	for i, loopID := range []string{priorLoop, currentLoop} {
		if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: int64(i + 1), ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
			t.Fatalf("Loops.Upsert %s: %v", loopID, err)
		}
	}
	for _, q := range []struct {
		id, loop, dedupe string
	}{
		{priorID, priorLoop, "worker:ambig_prior"},
		{currentID, currentLoop, "worker:ambig_current"},
	} {
		loopID := q.loop
		if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
			ID: q.id, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
			DedupeKey: q.dedupe, Priority: storage.QueuePriorityWorker, Status: "running",
			AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, ClaimedBy: &claimedBy, ClaimedAt: &nowISO,
			StartedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Fatalf("Queue.Upsert %s: %v", q.id, err)
		}
	}

	reg := NewActiveExecutionRegistry()
	priorLease, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation prior: %v", err)
	}
	priorItem := storage.QueueItemRecord{ID: priorID, LoopID: &priorLoop, Type: "worker", Status: "running"}
	if _, err := priorLease.BindClaim(priorItem); err != nil {
		t.Fatalf("BindClaim prior: %v", err)
	}
	if !reg.OwnsQueueClaim(priorID) {
		t.Fatal("prior claim must be registry-owned")
	}

	input := defaultSchedulerTickInput{
		Repos:          repos,
		Now:            func() time.Time { return now },
		OperationOwner: reg,
	}
	// owned is empty for the later batch; prior is only visible via registry.
	recovered, err := recoverAmbiguousCancelledClaim(context.Background(), input, nowISO, nil)
	if err != nil {
		t.Fatalf("recoverAmbiguousCancelledClaim: %v", err)
	}
	if recovered == nil || recovered.ID != currentID {
		t.Fatalf("recovered = %#v, want unowned %s (not registry-owned %s)", recovered, currentID, priorID)
	}

	// Only the prior registry-owned row remains: recovery must return nil, not
	// adopt the live-owned claim.
	if err := repos.Queue.Complete(context.Background(), currentID, nowISO); err != nil {
		t.Fatalf("Complete current: %v", err)
	}
	recovered, err = recoverAmbiguousCancelledClaim(context.Background(), input, nowISO, nil)
	if err != nil {
		t.Fatalf("recover after only registry-owned remains: %v", err)
	}
	if recovered != nil {
		t.Fatalf("recovered = %#v, want nil when only registry-owned claims match", recovered)
	}
}
