package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// No-HITL needs_human scope hold must produce action-required human attention.
func TestHumanAttentionContract_ReviewScopeHumanRequired(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	capturePath := filepath.Join(root, "osascript.log")
	scriptPath := filepath.Join(root, "osascript")
	writeHumanAttentionOsascript(t, scriptPath, capturePath, false)

	coordinator := openMigratedCoordinator(t, filepath.Join(root, "scope-attention.sqlite"), filepath.Join(root, "backups"))
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 29, 12, 0, 0, 0, time.UTC)
	nowISO := eventlog.FormatJavaScriptISOString(now)

	projectID := "project_scope_attention"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Scope Attention", RepoPath: filepath.Join(root, "repo"),
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewerMeta := `{"loop":{"iterationCount":1}}`
	reviewer := storage.LoopRecord{
		ID: "loop_scope_attention", Seq: 718, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &reviewerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Loops.Upsert(ctx, reviewer); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	parked, err := loops.ParkReviewScopeHuman(ctx, repos, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr,
		NowISO: nowISO, HITLEnabled: false,
		Question: "Clarify AGENTS.md rule X before unpause",
		Evidence: "PR non-goals exclude API expansion",
	})
	if err != nil {
		t.Fatalf("Park: %v", err)
	}
	if parked.Status != "paused" || !loops.IsReviewScopeHumanRequiredPause(parked.MetadataJSON) {
		t.Fatalf("parked = %#v, want no-HITL scope pause", parked)
	}

	ids := collectHumanAttentionLoopIDs(ctx, repos)
	found := false
	for _, id := range ids {
		if id == reviewer.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("collectHumanAttentionLoopIDs() = %v, want scope hold %s", ids, reviewer.ID)
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

	notifyDurableHumanAttention(ctx, gateway, repos, reviewer.ID)
	assertHumanAttentionInAppCount(t, repos, reviewer.ID, 1)
	assertHumanAttentionInAppBodyContains(t, repos, reviewer.ID, "Clarify AGENTS.md rule X before unpause")
	assertHumanAttentionInAppBodyContains(t, repos, reviewer.ID, "PR non-goals exclude API expansion")
	assertOsascriptContains(t, capturePath, "display notification")
	// Re-observe must not resend.
	notifyDurableHumanAttention(ctx, gateway, repos, reviewer.ID)
	assertHumanAttentionInAppCount(t, repos, reviewer.ID, 1)
}
