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

// Cross-component lifecycle: when an operator terminates (or completes) a paused
// manual-intervention loop before the async observer / recovery rescan runs, the
// queue row can remain manual_intervention because CancelByLoop only cancels
// queued/running. Observation must not emit a human-attention alert for that
// stale park.
func TestHumanAttentionContract_SkipManualAlertAfterTerminalLoop(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	capturePath := filepath.Join(root, "osascript.log")
	scriptPath := filepath.Join(root, "osascript")
	writeHumanAttentionOsascript(t, scriptPath, capturePath, false)

	coordinator := openMigratedCoordinator(t, filepath.Join(root, "terminal-manual.sqlite"), filepath.Join(root, "backups"))
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 29, 16, 30, 0, 0, time.UTC)
	nowISO := eventlog.FormatJavaScriptISOString(now)

	projectID := "project_terminal_manual"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Terminal Manual", RepoPath: filepath.Join(root, "repo"),
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

	seedManualPark := func(t *testing.T, loopID string, seq int64, loopStatus string) {
		t.Helper()
		target := "pr:acme/looper:" + loopID
		prNumber := seq
		if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
			ID: loopID, Seq: seq, ProjectID: projectID, Type: "fixer",
			TargetType: "pull_request", TargetID: &target, Repo: stringPtr("acme/looper"),
			PRNumber: &prNumber, Status: loopStatus, CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loopID, err)
		}
		checkpoint := `{"resumePolicy":"manual_intervention"}`
		if err := repos.Runs.Upsert(ctx, storage.RunRecord{
			ID: "run_" + loopID, LoopID: loopID, Status: "failed",
			CheckpointJSON: &checkpoint, Summary: stringPtr("dirty worktree"),
			StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", loopID, err)
		}
		manualKind := "manual_intervention"
		finished := nowISO
		if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
			ID: "queue_" + loopID, ProjectID: &projectID, LoopID: &loopID,
			Type: "fixer", TargetType: "pull_request", TargetID: target,
			Repo: stringPtr("acme/looper"), PRNumber: &prNumber,
			DedupeKey: "fixer:" + loopID, Priority: storage.QueuePriorityFixer,
			// Stale hold: loop is terminal, but CancelByLoop never clears this status.
			Status: "manual_intervention", AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3,
			LastError: stringPtr("dirty worktree"), LastErrorKind: &manualKind,
			FinishedAt: &finished, CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", loopID, err)
		}
	}

	// Terminate-before-notify (async observer race).
	terminatedID := "loop_terminated_before_notify"
	seedManualPark(t, terminatedID, 801, "terminated")
	notifyDurableHumanAttention(ctx, gateway, repos, terminatedID)
	assertHumanAttentionInAppCount(t, repos, terminatedID, 0)

	// Completed can be reached from paused without clearing the queue hold.
	completedID := "loop_completed_before_notify"
	seedManualPark(t, completedID, 802, "completed")
	notifyDurableHumanAttention(ctx, gateway, repos, completedID)
	assertHumanAttentionInAppCount(t, repos, completedID, 0)

	// Recovery rescan must also skip terminal loops with stale manual_intervention rows.
	rt := &Runtime{}
	rt.notifyDurableHumanAttentionParksBestEffort(ctx, repos)
	assertHumanAttentionInAppCount(t, repos, terminatedID, 0)
	assertHumanAttentionInAppCount(t, repos, completedID, 0)

	// Control: still-active paused park continues to notify.
	activeID := "loop_active_manual_park"
	seedManualPark(t, activeID, 803, "paused")
	notifyDurableHumanAttention(ctx, gateway, repos, activeID)
	assertHumanAttentionInAppCount(t, repos, activeID, 1)
}
