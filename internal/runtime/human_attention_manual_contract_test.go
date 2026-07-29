package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/storage"
)

// Cross-component: hard manual_intervention notifies once; ordinary error kinds
// parked under the same queue status do not.
func TestHumanAttentionContract_ManualInterventionFilter(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	capturePath := filepath.Join(root, "osascript.log")
	scriptPath := filepath.Join(root, "osascript")
	writeHumanAttentionOsascript(t, scriptPath, capturePath, false)

	coordinator := openMigratedCoordinator(t, filepath.Join(root, "manual.sqlite"), filepath.Join(root, "backups"))
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 29, 12, 0, 0, 0, time.UTC)
	nowISO := eventlog.FormatJavaScriptISOString(now)

	projectID := "project_manual_attention"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Manual Attention", RepoPath: filepath.Join(root, "repo"),
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	gateway := notify.NewGateway(notify.Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				SoundForLevels:        []config.NotificationSoundLevel{config.NotificationSoundLevelActionRequired},
				ThrottleWindowSeconds: 60,
			},
		},
		OsascriptPath:     scriptPath,
		LogFilePath:       filepath.Join(root, "logs", "looperd.log"),
		DashboardBaseURL:  "http://127.0.0.1:17310",
		DashboardAuthMode: config.AuthModeNone,
		Repositories:      repos,
		Now:               func() time.Time { return now },
	})

	manualLoopID := "loop_manual_hold"
	manualTarget := "pr:acme/looper:42"
	prNumber := int64(42)
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: manualLoopID, Seq: 617, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &manualTarget, Repo: stringPtr("acme/looper"),
		PRNumber: &prNumber, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(manual loop) error = %v", err)
	}
	checkpoint := `{"resumePolicy":"manual_intervention"}`
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_manual_hold", LoopID: manualLoopID, Status: "failed",
		CheckpointJSON: &checkpoint, Summary: stringPtr("dirty worktree"),
		StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(manual run) error = %v", err)
	}
	manualKind := "manual_intervention"
	finished := nowISO
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_manual_hold", ProjectID: &projectID, LoopID: &manualLoopID,
		Type: "fixer", TargetType: "pull_request", TargetID: manualTarget,
		Repo: stringPtr("acme/looper"), PRNumber: &prNumber,
		DedupeKey: "fixer:manual_hold", Priority: storage.QueuePriorityFixer,
		Status: "manual_intervention", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3,
		LastError: stringPtr("dirty worktree"), LastErrorKind: &manualKind,
		FinishedAt: &finished, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(manual) error = %v", err)
	}
	notifyDurableHumanAttention(ctx, gateway, repos, manualLoopID)
	assertHumanAttentionInAppCount(t, repos, manualLoopID, 1)
	// Dedupe: unchanged park does not resend.
	notifyDurableHumanAttention(ctx, gateway, repos, manualLoopID)
	assertHumanAttentionInAppCount(t, repos, manualLoopID, 1)

	// Ordinary failure kind parked as queue status manual_intervention must NOT notify.
	ordinaryLoopID := "loop_ordinary_fail"
	ordinaryTarget := projectID
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: ordinaryLoopID, Seq: 618, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &ordinaryTarget, Status: "paused",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(ordinary) error = %v", err)
	}
	ordinaryKind := "retryable_transient"
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_ordinary_fail", ProjectID: &projectID, LoopID: &ordinaryLoopID,
		Type: "worker", TargetType: "project", TargetID: ordinaryTarget,
		DedupeKey: "worker:ordinary", Priority: storage.QueuePriorityWorker,
		Status: "manual_intervention", AvailableAt: nowISO, Attempts: 3, MaxAttempts: 3,
		LastError: stringPtr("network blip"), LastErrorKind: &ordinaryKind,
		FinishedAt: &finished, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(ordinary) error = %v", err)
	}
	notifyDurableHumanAttention(ctx, gateway, repos, ordinaryLoopID)
	assertHumanAttentionInAppCount(t, repos, ordinaryLoopID, 0)
}
