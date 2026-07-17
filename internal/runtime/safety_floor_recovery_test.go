package runtime

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

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

// Contract: recovery quarantine must not rewrite human_takeover → paused.
func TestSafetyFloorQuarantinePreservesHumanTakeover(t *testing.T) {
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
	projectID := "project_human_takeover"
	loopID := "loop_human_takeover"
	runID := "run_human_takeover"
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Takeover", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := seedRepos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 99, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "human_takeover", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := seedRepos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("execute"), StartedAt: oldISO, LastHeartbeatAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := seedRepos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_human_takeover", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:project_human_takeover:loop_human_takeover", Priority: storage.QueuePriorityWorker, Status: "running",
		AvailableAt: oldISO, Attempts: 1, MaxAttempts: 3, ClaimedBy: stringPtr("scheduler"), ClaimedAt: stringPtr(oldISO),
		StartedAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	pid := int64(4242)
	if err := seedRepos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "agent_human_takeover", ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "cancelling",
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
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	if loop == nil || loop.Status != "human_takeover" {
		t.Fatalf("loop = %#v, want human_takeover preserved", loop)
	}
	queue, err := services.Repositories.Queue.GetByID(context.Background(), "queue_human_takeover")
	if err != nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	if queue == nil || queue.Status != "manual_intervention" {
		t.Fatalf("queue = %#v, want manual_intervention", queue)
	}
}
