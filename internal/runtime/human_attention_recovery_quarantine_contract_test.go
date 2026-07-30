package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
)

// Recovery quarantine must not deliver human-attention notifications on the
// critical path (notification transports can be slow or unavailable).
func TestHumanAttentionContract_QuarantineDoesNotNotifySynchronously(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	// Slow osascript would hang startup if quarantine still notified inline.
	slowScript := filepath.Join(root, "osascript")
	if err := os.WriteFile(slowScript, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(slow osascript) error = %v", err)
	}

	coordinator := openMigratedCoordinator(t, filepath.Join(root, "quarantine-sync.sqlite"), filepath.Join(root, "backups"))
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 29, 15, 30, 0, 0, time.UTC)
	nowISO := eventlog.FormatJavaScriptISOString(now)

	projectID := "project_quarantine_sync"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Quarantine Sync", RepoPath: filepath.Join(root, "repo"),
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loopID := "loop_quarantine_sync"
	target := projectID
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 710, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &target, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	runID := "run_quarantine_sync"
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: runID, LoopID: loopID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_quarantine_sync", ProjectID: &projectID, LoopID: &loopID,
		Type: "worker", TargetType: "project", TargetID: target,
		DedupeKey: "worker:quarantine_sync", Priority: storage.QueuePriorityWorker,
		Status: "running", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	execID := "exec_quarantine_sync"
	if err := repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{
		ID: execID, ProjectID: &projectID, LoopID: &loopID, RunID: &runID,
		Vendor: "codex", Status: "running", StartedAt: nowISO,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Notifications.InApp = true
	cfg.Notifications.Osascript = config.OsascriptNotificationConfig{
		Enabled:               true,
		SoundForLevels:        []config.NotificationSoundLevel{config.NotificationSoundLevelActionRequired},
		ThrottleWindowSeconds: 60,
	}
	osascriptPath := slowScript
	cfg.Tools.OsascriptPath = &osascriptPath
	cfg.Daemon.LogDir = filepath.Join(root, "logs")

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	execution, err := repos.AgentExecutions.GetByID(ctx, execID)
	if err != nil || execution == nil {
		t.Fatalf("AgentExecutions.GetByID() = %#v err=%v", execution, err)
	}

	start := time.Now()
	quarantined, wrote, err := rt.quarantineRecoveryEvidence(ctx, repos, *execution, nowISO, "startup recovery: uncertain (test)")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("quarantineRecoveryEvidence() error = %v", err)
	}
	if !quarantined || !wrote {
		t.Fatalf("quarantineRecoveryEvidence() quarantined=%v wrote=%v, want true/true", quarantined, wrote)
	}
	// Budget well under the local notification command timeout.
	if elapsed > 2*time.Second {
		t.Fatalf("quarantineRecoveryEvidence took %v; must not run interactive notify on the critical path", elapsed)
	}
	// No notification rows yet — delivery is deferred to async recovery rescan.
	assertHumanAttentionInAppCount(t, repos, loopID, 0)
}
