package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
)

// Cross-component: after durable parks exist without a live finalize callback
// (crash-before-notify window), recovery rescan emits once; re-rescan dedupes.
func TestHumanAttentionContract_RecoveryRescanAwaitingAndManual(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	capturePath := filepath.Join(root, "osascript.log")
	scriptPath := filepath.Join(root, "osascript")
	writeHumanAttentionOsascript(t, scriptPath, capturePath, false)

	coordinator := openMigratedCoordinator(t, filepath.Join(root, "recovery-rescan.sqlite"), filepath.Join(root, "backups"))
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 29, 15, 0, 0, 0, time.UTC)
	nowISO := eventlog.FormatJavaScriptISOString(now)

	projectID := "project_recovery_rescan"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Recovery Rescan", RepoPath: filepath.Join(root, "repo"),
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	// Durable awaiting_human park with no prior notification (missed finalize callback).
	awaitLoopID := "loop_await_missed_notify"
	awaitTarget := projectID
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: awaitLoopID, Seq: 701, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &awaitTarget, Repo: stringPtr("acme/looper"),
		Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(awaiting_human) error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_await_missed", LoopID: awaitLoopID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(await) error = %v", err)
	}

	// Hard manual_intervention park (latest queue row).
	manualLoopID := "loop_manual_missed_notify"
	manualTarget := "pr:acme/looper:99"
	prNumber := int64(99)
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: manualLoopID, Seq: 702, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &manualTarget, Repo: stringPtr("acme/looper"),
		PRNumber: &prNumber, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(manual) error = %v", err)
	}
	checkpoint := `{"resumePolicy":"manual_intervention"}`
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_manual_missed", LoopID: manualLoopID, Status: "failed",
		CheckpointJSON: &checkpoint, Summary: stringPtr("orphan quarantine"),
		StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(manual) error = %v", err)
	}
	manualKind := "manual_intervention"
	finished := nowISO
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_manual_missed", ProjectID: &projectID, LoopID: &manualLoopID,
		Type: "fixer", TargetType: "pull_request", TargetID: manualTarget,
		Repo: stringPtr("acme/looper"), PRNumber: &prNumber,
		DedupeKey: "fixer:manual_missed", Priority: storage.QueuePriorityFixer,
		Status: "manual_intervention", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3,
		LastError: stringPtr("orphan quarantine"), LastErrorKind: &manualKind,
		FinishedAt: &finished, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(manual) error = %v", err)
	}

	// Ordinary exhausted retry under manual_intervention status must stay silent.
	ordinaryLoopID := "loop_ordinary_missed"
	ordinaryTarget := projectID
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: ordinaryLoopID, Seq: 703, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &ordinaryTarget, Status: "paused",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(ordinary) error = %v", err)
	}
	ordinaryKind := "retryable_transient"
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_ordinary_missed", ProjectID: &projectID, LoopID: &ordinaryLoopID,
		Type: "worker", TargetType: "project", TargetID: ordinaryTarget,
		DedupeKey: "worker:ordinary_missed", Priority: storage.QueuePriorityWorker,
		Status: "manual_intervention", AvailableAt: nowISO, Attempts: 3, MaxAttempts: 3,
		LastError: stringPtr("network blip"), LastErrorKind: &ordinaryKind,
		FinishedAt: &finished, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(ordinary) error = %v", err)
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
	osascriptPath := scriptPath
	cfg.Tools.OsascriptPath = &osascriptPath
	cfg.Daemon.LogDir = filepath.Join(root, "logs")
	cfg.Server.AuthMode = config.AuthModeNone

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})

	ids := collectHumanAttentionLoopIDs(ctx, repos)
	if len(ids) < 2 {
		t.Fatalf("collectHumanAttentionLoopIDs() = %v, want at least awaiting + manual", ids)
	}

	// Synchronous rescan path (same body as the async recovery schedule).
	rt.notifyDurableHumanAttentionParksBestEffort(ctx, repos)
	assertHumanAttentionInAppCount(t, repos, awaitLoopID, 1)
	assertHumanAttentionInAppCount(t, repos, manualLoopID, 1)
	assertHumanAttentionInAppCount(t, repos, ordinaryLoopID, 0)

	// Permanent entry dedupe: second rescan does not resend.
	rt.notifyDurableHumanAttentionParksBestEffort(ctx, repos)
	assertHumanAttentionInAppCount(t, repos, awaitLoopID, 1)
	assertHumanAttentionInAppCount(t, repos, manualLoopID, 1)
}
