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

// Contract: exited leader PID alone is uncertain — descendants may remain.
// Recovery must not signal, terminalize agent_executions, requeue, or overlap.
func TestStartupRecoveryExitedLeaderDoesNotConfirmDeadOrAct(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.July, 19, 10, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)
	oldISO := formatJavaScriptISOString(startedAt.Add(-time.Hour))

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	projectID := "project_leader_exit"
	loopID := "loop_leader_exit"
	runID := "run_leader_exit"
	queueID := "queue_leader_exit"
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Leader", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := seedRepos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := seedRepos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("execute"), StartedAt: oldISO, LastHeartbeatAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := seedRepos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:project_leader_exit:loop_leader_exit", Priority: storage.QueuePriorityWorker, Status: "running",
		AvailableAt: oldISO, Attempts: 1, MaxAttempts: 3, ClaimedBy: stringPtr("scheduler"), ClaimedAt: stringPtr(oldISO),
		StartedAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	// Leader PID gone (empty command = not running). Descendants may still be
	// live under a reused or unrecorded PGID — classification must be uncertain.
	deadLeaderPID := int64(7777)
	if err := seedRepos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "agent_leader_exit", ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		PID: &deadLeaderPID, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`), CWD: stringPtr(workingDir),
		HeartbeatCount: 0, StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed close error = %v", err)
	}

	var signals []syscall.Signal
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now:    func() time.Time { return startedAt },
		ReadProcessCommand: func(context.Context, int) (string, error) {
			return "", nil // leader not running
		},
		SignalProcess: func(_ int, sig syscall.Signal) error {
			signals = append(signals, sig)
			return nil
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	if len(signals) != 0 {
		t.Fatalf("SignalProcess called with %v; want no raw PID/PGID action", signals)
	}
	services := rt.Services()
	execution, err := services.Repositories.AgentExecutions.GetByID(context.Background(), "agent_leader_exit")
	if err != nil {
		t.Fatalf("GetByID error = %v", err)
	}
	if execution == nil || execution.Status != "running" || execution.EndedAt != nil {
		t.Fatalf("execution = %#v, want still-running evidence (not terminal from leader exit)", execution)
	}
	queue, err := services.Repositories.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	if queue == nil || queue.Status != "manual_intervention" {
		t.Fatalf("queue = %#v, want manual_intervention quarantine", queue)
	}
	// Running claim must not be claimable after quarantine (no overlap).
	if err := rt.AllowClaim(); err != nil {
		t.Fatalf("AllowClaim() = %v", err)
	}
	claimed, err := services.Repositories.Queue.ClaimNext(context.Background(), nowISO, "scheduler")
	if err != nil {
		t.Fatalf("ClaimNext error = %v", err)
	}
	if claimed != nil {
		t.Fatalf("ClaimNext = %#v, want nil (no overlapping work)", claimed)
	}

	recovery := rt.RecoverySummary()
	if recovery.OrphanAgentCleanup.ConfirmedDeadCount != 0 {
		t.Fatalf("ConfirmedDeadCount = %d, want 0 (leader exit is not confirmed-dead)", recovery.OrphanAgentCleanup.ConfirmedDeadCount)
	}
	if recovery.OrphanAgentCleanup.UncertainCount != 1 {
		t.Fatalf("UncertainCount = %d, want 1", recovery.OrphanAgentCleanup.UncertainCount)
	}
	if recovery.OrphanAgentCleanup.CleanedCount != 0 || recovery.OrphanAgentCleanup.QuarantinedCount != 1 {
		t.Fatalf("OrphanAgentCleanup = %#v, want quarantined without cleaned", recovery.OrphanAgentCleanup)
	}
	if recovery.LoopsRequeued != 0 {
		t.Fatalf("LoopsRequeued = %d, want 0", recovery.LoopsRequeued)
	}

	events, err := services.Repositories.Events.ListByEntity(context.Background(), "agent_execution", "agent_leader_exit")
	if err != nil {
		t.Fatalf("ListByEntity error = %v", err)
	}
	if containsEventType(events, "agent.killed") {
		t.Fatalf("events = %#v, want no agent.killed", events)
	}
	if !containsEventType(events, "looperd.recovery.containment_classified") {
		t.Fatalf("events = %#v, want containment_classified", events)
	}
	if !containsEventType(events, "looperd.recovery.execution_quarantined") {
		t.Fatalf("events = %#v, want execution_quarantined", events)
	}
}

// Contract: observed-live (including TERM-resistant) evidence is never adopted
// as live ownership and never receives fire-and-forget SIGTERM/SIGKILL on raw PGID.
func TestStartupRecoveryObservedLiveNoSignalNoRequeue(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.July, 19, 11, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)
	oldISO := formatJavaScriptISOString(startedAt.Add(-time.Hour))

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	projectID := "project_observed_live"
	loopID := "loop_observed_live"
	runID := "run_observed_live"
	queueID := "queue_observed_live"
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Live", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := seedRepos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 2, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := seedRepos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("execute"), StartedAt: oldISO, LastHeartbeatAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := seedRepos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:project_observed_live:loop_observed_live", Priority: storage.QueuePriorityWorker, Status: "running",
		AvailableAt: oldISO, Attempts: 1, MaxAttempts: 3, ClaimedBy: stringPtr("scheduler"), ClaimedAt: stringPtr(oldISO),
		StartedAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	livePID := int64(8888)
	if err := seedRepos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "agent_observed_live", ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		PID: &livePID, CommandJSON: stringPtr(`{"command":"codex","args":["exec","--term-resistant"]}`), CWD: stringPtr(workingDir),
		HeartbeatCount: 1, StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed close error = %v", err)
	}

	var signaled []struct {
		pid int
		sig syscall.Signal
	}
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now:    func() time.Time { return startedAt },
		ReadProcessCommand: func(_ context.Context, pid int) (string, error) {
			if pid != int(livePID) {
				return "", nil
			}
			return "codex exec --term-resistant", nil
		},
		SignalProcess: func(pid int, sig syscall.Signal) error {
			signaled = append(signaled, struct {
				pid int
				sig syscall.Signal
			}{pid, sig})
			return nil
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	if len(signaled) != 0 {
		t.Fatalf("SignalProcess called %#v; want no fire-and-forget SIGTERM/SIGKILL on raw PID/PGID", signaled)
	}

	services := rt.Services()
	execution, err := services.Repositories.AgentExecutions.GetByID(context.Background(), "agent_observed_live")
	if err != nil {
		t.Fatalf("GetByID error = %v", err)
	}
	if execution == nil || execution.Status != "running" {
		t.Fatalf("execution = %#v, want running evidence", execution)
	}
	queue, err := services.Repositories.Queue.GetByID(context.Background(), queueID)
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
		t.Fatalf("run = %#v, want still running (no false interrupt cleanliness)", run)
	}

	recovery := rt.RecoverySummary()
	if recovery.OrphanAgentCleanup.ObservedLiveCount != 1 {
		t.Fatalf("ObservedLiveCount = %d, want 1", recovery.OrphanAgentCleanup.ObservedLiveCount)
	}
	if recovery.OrphanAgentCleanup.ConfirmedDeadCount != 0 || recovery.OrphanAgentCleanup.CleanedCount != 0 {
		t.Fatalf("cleanup = %#v, want no confirmed-dead / cleaned", recovery.OrphanAgentCleanup)
	}
	if recovery.LoopsRequeued != 0 {
		t.Fatalf("LoopsRequeued = %d, want 0", recovery.LoopsRequeued)
	}

	// Lease/finalize semantics: quarantined running claim is not re-claimable.
	if err := rt.AllowClaim(); err != nil {
		t.Fatalf("AllowClaim() = %v", err)
	}
	claimed, err := services.Repositories.Queue.ClaimNext(context.Background(), nowISO, "scheduler")
	if err != nil {
		t.Fatalf("ClaimNext error = %v", err)
	}
	if claimed != nil {
		t.Fatalf("ClaimNext = %#v, want nil", claimed)
	}
}
