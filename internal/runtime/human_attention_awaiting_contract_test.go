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

// Cross-component: durable awaiting_human entry → one notification, permanent
// entry dedupe on re-observe, leave+re-enter with a new run re-notifies.
func TestHumanAttentionContract_AwaitingHumanTransitionAndDedupe(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	capturePath := filepath.Join(root, "osascript.log")
	scriptPath := filepath.Join(root, "osascript")
	writeHumanAttentionOsascript(t, scriptPath, capturePath, false)

	coordinator := openMigratedCoordinator(t, filepath.Join(root, "human-attention.sqlite"), filepath.Join(root, "backups"))
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 29, 12, 0, 0, 0, time.UTC)
	nowISO := eventlog.FormatJavaScriptISOString(now)

	projectID := "project_human_attention"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Human Attention", RepoPath: filepath.Join(root, "repo"),
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	loopID := "loop_human_attention"
	targetID := projectID
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 616, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Repo: stringPtr("acme/looper"),
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(running) error = %v", err)
	}
	runID := "run_human_attention_1"
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: runID, LoopID: loopID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	gateway := notify.NewGateway(notify.Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				SoundForLevels:        []config.NotificationSoundLevel{config.NotificationSoundLevelActionRequired},
				ThrottleWindowSeconds: 1,
			},
		},
		OsascriptPath: scriptPath,
		Repositories:  repos,
		Now:           func() time.Time { return now },
	})

	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 616, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Repo: stringPtr("acme/looper"),
		Status: "awaiting_human", LastRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(awaiting_human) error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: runID, LoopID: loopID, Status: "interrupted", Summary: stringPtr("Awaiting human decision"),
		StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(interrupted) error = %v", err)
	}

	notifyDurableHumanAttention(ctx, gateway, repos, loopID)
	assertHumanAttentionInAppCount(t, repos, loopID, 1)
	assertOsascriptContains(t, capturePath, "display notification")
	assertOsascriptNotContains(t, capturePath, "display dialog")
	assertOsascriptLacksSensitive(t, capturePath)

	// Unchanged parked state / re-observe (daemon restart simulation) must not resend.
	notifyDurableHumanAttention(ctx, gateway, repos, loopID)
	assertHumanAttentionInAppCount(t, repos, loopID, 1)

	// Leave human-attention, then re-enter with a new run → new notification.
	leaveAt := eventlog.FormatJavaScriptISOString(now.Add(time.Minute))
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 616, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Repo: stringPtr("acme/looper"),
		Status: "running", CreatedAt: nowISO, UpdatedAt: leaveAt,
	}); err != nil {
		t.Fatalf("Loops.Upsert(running re-enter) error = %v", err)
	}
	runID2 := "run_human_attention_2"
	reenterAt := eventlog.FormatJavaScriptISOString(now.Add(2 * time.Minute))
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: runID2, LoopID: loopID, Status: "interrupted",
		StartedAt: reenterAt, EndedAt: &reenterAt, CreatedAt: reenterAt, UpdatedAt: reenterAt,
	}); err != nil {
		t.Fatalf("Runs.Upsert(run2) error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 616, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Repo: stringPtr("acme/looper"),
		Status: "awaiting_human", LastRunAt: &reenterAt, CreatedAt: nowISO, UpdatedAt: reenterAt,
	}); err != nil {
		t.Fatalf("Loops.Upsert(awaiting_human re-enter) error = %v", err)
	}
	// Advance clock past osascript throttle so a genuine new entry is not throttle-skipped.
	gateway = notify.NewGateway(notify.Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				SoundForLevels:        []config.NotificationSoundLevel{config.NotificationSoundLevelActionRequired},
				ThrottleWindowSeconds: 1,
			},
		},
		OsascriptPath: scriptPath,
		Repositories:  repos,
		Now:           func() time.Time { return now.Add(3 * time.Minute) },
	})
	notifyDurableHumanAttention(ctx, gateway, repos, loopID)
	assertHumanAttentionInAppCount(t, repos, loopID, 2)
}
