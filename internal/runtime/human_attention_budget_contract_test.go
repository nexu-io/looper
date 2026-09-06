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

// No-HITL review-fix budget exhausted hold must produce action-required human attention.
func TestHumanAttentionContract_ReviewFixBudgetExhausted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	capturePath := filepath.Join(root, "osascript.log")
	scriptPath := filepath.Join(root, "osascript")
	writeHumanAttentionOsascript(t, scriptPath, capturePath, false)

	coordinator := openMigratedCoordinator(t, filepath.Join(root, "budget-attention.sqlite"), filepath.Join(root, "backups"))
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 29, 12, 0, 0, 0, time.UTC)
	nowISO := eventlog.FormatJavaScriptISOString(now)

	projectID := "project_budget_attention"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Budget Attention", RepoPath: filepath.Join(root, "repo"),
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewerMeta := `{"loop":{"iterationCount":3}}`
	reviewer := storage.LoopRecord{
		ID: "loop_budget_attention", Seq: 717, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &reviewerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Loops.Upsert(ctx, reviewer); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	parked, err := loops.ParkReviewFixBudget(ctx, repos, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 3, Cap: 3,
		NowISO: nowISO, HITLEnabled: false, LiveCaps: loops.ReviewFixBudgetLiveCaps{ReviewerMaxPublishes: 3, FixerMaxPushes: 3},
	})
	if err != nil {
		t.Fatalf("Park: %v", err)
	}
	if parked.Status != "paused" || !loops.IsReviewFixBudgetExhaustedPause(parked.MetadataJSON) {
		t.Fatalf("parked = %#v, want no-HITL exhausted pause", parked)
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
		t.Fatalf("collectHumanAttentionLoopIDs() = %v, want exhausted hold %s", ids, reviewer.ID)
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
	assertOsascriptContains(t, capturePath, "display notification")

	// Re-observe must not resend.
	notifyDurableHumanAttention(ctx, gateway, repos, reviewer.ID)
	assertHumanAttentionInAppCount(t, repos, reviewer.ID, 1)
}
