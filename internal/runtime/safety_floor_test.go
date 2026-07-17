package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// Contract: mutations and claims are gated until admission is ready; there is
// no dual ready-flag Authority that can disagree with admission (#575).
func TestSafetyFloorMutationsAndClaimsGatedUntilReady(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir

	rt := New(Options{
		Config:        cfg,
		Logger:        &testLogger{},
		DeferRecovery: true,
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	if got := rt.AdmissionState(); got != AdmissionStarting {
		t.Fatalf("AdmissionState() after Start+DeferRecovery = %q, want starting", got)
	}
	if err := rt.AllowMutations(); !errors.Is(err, ErrAdmissionNotReady) {
		t.Fatalf("AllowMutations() while starting = %v, want ErrAdmissionNotReady", err)
	}
	if err := rt.AllowClaim(); !errors.Is(err, ErrAdmissionNotReady) {
		t.Fatalf("AllowClaim() while starting = %v, want ErrAdmissionNotReady", err)
	}
	// ownershipAcquired must not open mutations — only admission does.
	if rt.ownershipAcquired {
		t.Fatal("ownershipAcquired = true before CompleteStartup")
	}

	var claimCalls atomic.Int64
	rt.mu.Lock()
	rt.defaultSchedulerClaim = func(context.Context, Services) error {
		claimCalls.Add(1)
		return nil
	}
	rt.mu.Unlock()
	rt.executeSchedulerClaimPass(context.Background())
	if claimCalls.Load() != 0 {
		t.Fatalf("claim pump ran while starting, calls=%d", claimCalls.Load())
	}

	if err := rt.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("CompleteStartup() error = %v", err)
	}
	if got := rt.AdmissionState(); got != AdmissionReady {
		t.Fatalf("AdmissionState() after CompleteStartup = %q, want ready", got)
	}
	if !rt.ownershipAcquired {
		t.Fatal("ownershipAcquired = false after CompleteStartup")
	}
	if err := rt.AllowMutations(); err != nil {
		t.Fatalf("AllowMutations() after ready = %v", err)
	}
	if err := rt.AllowClaim(); err != nil {
		t.Fatalf("AllowClaim() after ready = %v", err)
	}

	beforeReadyClaims := claimCalls.Load()
	rt.executeSchedulerClaimPass(context.Background())
	if claimCalls.Load() <= beforeReadyClaims {
		t.Fatalf("claim pump calls after ready = %d, want > %d", claimCalls.Load(), beforeReadyClaims)
	}
	claimsWhenReady := claimCalls.Load()

	// Dual-flag invariant: forcing ownershipAcquired alone must not admit work
	// when admission is degraded.
	if err := rt.MarkDegraded("test degrade"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}
	rt.ownershipAcquired = true
	if err := rt.AllowMutations(); !errors.Is(err, ErrAdmissionDegraded) {
		t.Fatalf("AllowMutations() while degraded with ownershipAcquired = %v, want degraded", err)
	}
	if err := rt.AllowClaim(); !errors.Is(err, ErrAdmissionDegraded) {
		t.Fatalf("AllowClaim() while degraded with ownershipAcquired = %v, want degraded", err)
	}
	rt.executeSchedulerClaimPass(context.Background())
	if claimCalls.Load() != claimsWhenReady {
		t.Fatalf("claim pump advanced while degraded, calls=%d want %d", claimCalls.Load(), claimsWhenReady)
	}
}

func TestSafetyFloorRecoveryNoActAndQuarantineBlocksOverlap(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)
	oldISO := formatJavaScriptISOString(startedAt.Add(-2 * time.Hour))

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	projectID := "project_1"
	loopID := "loop_live_orphan"
	runID := "run_live_orphan"
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := seedRepos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 42, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := seedRepos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("execute"), StartedAt: oldISO, LastHeartbeatAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := seedRepos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_live_orphan", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:project_1:loop_live_orphan", Priority: storage.QueuePriorityWorker, Status: "running",
		AvailableAt: oldISO, Attempts: 1, MaxAttempts: 3, ClaimedBy: stringPtr("scheduler"), ClaimedAt: stringPtr(oldISO),
		StartedAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	pid := int64(9999)
	if err := seedRepos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "agent_live_orphan", ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		PID: &pid, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`), CWD: stringPtr(workingDir),
		HeartbeatCount: 0, StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed close error = %v", err)
	}

	signaled := false
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now:    func() time.Time { return startedAt },
		ReadProcessCommand: func(context.Context, int) (string, error) {
			return "codex exec", nil
		},
		SignalProcess: func(int, syscall.Signal) error {
			signaled = true
			return nil
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	if signaled {
		t.Fatal("recovery signaled raw process group")
	}
	services := rt.Services()
	execution, err := services.Repositories.AgentExecutions.GetByID(context.Background(), "agent_live_orphan")
	if err != nil {
		t.Fatalf("GetByID error = %v", err)
	}
	if execution == nil || execution.Status != "running" || execution.EndedAt != nil {
		t.Fatalf("execution = %#v, want running evidence (not terminalized)", execution)
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	if loop == nil || loop.Status != "paused" {
		t.Fatalf("loop = %#v, want paused", loop)
	}
	queue, err := services.Repositories.Queue.GetByID(context.Background(), "queue_live_orphan")
	if err != nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	if queue == nil || queue.Status != "manual_intervention" {
		t.Fatalf("queue = %#v, want manual_intervention", queue)
	}
	run, err := services.Repositories.Runs.GetByID(context.Background(), runID)
	if err != nil {
		t.Fatalf("Runs.GetByID error = %v", err)
	}
	if run == nil || run.Status != "running" {
		t.Fatalf("run = %#v, want still running evidence (no false interrupt cleanliness)", run)
	}

	// Claim must not pick up quarantined work after admission is ready.
	if err := rt.AllowClaim(); err != nil {
		t.Fatalf("AllowClaim() = %v", err)
	}
	claimed, err := services.Repositories.Queue.ClaimNext(context.Background(), nowISO, "scheduler")
	if err != nil {
		t.Fatalf("ClaimNext error = %v", err)
	}
	if claimed != nil {
		t.Fatalf("ClaimNext = %#v, want nil for quarantined work", claimed)
	}
}

func TestSafetyFloorBeginShutdownClosesAdmissionBeforeStorage(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir

	rt := New(Options{Config: cfg, Logger: &testLogger{}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if rt.AdmissionState() != AdmissionReady {
		t.Fatalf("AdmissionState() = %q, want ready", rt.AdmissionState())
	}

	rt.BeginShutdown("test stop")
	if rt.AdmissionState() != AdmissionStopping {
		t.Fatalf("AdmissionState() after BeginShutdown = %q, want stopping", rt.AdmissionState())
	}
	if err := rt.AllowMutations(); !errors.Is(err, ErrAdmissionStopping) {
		t.Fatalf("AllowMutations after BeginShutdown = %v", err)
	}
	// Storage still available until Stop closes it.
	if services := rt.Services(); services.Coordinator == nil {
		t.Fatal("Services().Coordinator = nil after BeginShutdown, want storage retained until Stop")
	}
	rt.Stop("test stop")
}
