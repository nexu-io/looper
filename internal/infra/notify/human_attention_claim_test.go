package notify

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

func TestNotifyHumanAttentionClaimIsAtomic(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rootDir := t.TempDir()
	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())

	var osascriptCalls atomic.Int64
	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			InApp: true,
			Osascript: config.OsascriptNotificationConfig{
				Enabled:               true,
				SoundForLevels:        []config.NotificationSoundLevel{config.NotificationSoundLevelActionRequired},
				ThrottleWindowSeconds: 60,
			},
		},
		OsascriptPath: "/usr/bin/osascript",
		Repositories:  repos,
		RunCommand: func(_ context.Context, _ shell.Options) (shell.Result, error) {
			osascriptCalls.Add(1)
			return shell.Result{ExitCode: 0}, nil
		},
	})

	const goroutines = 16
	var wg sync.WaitGroup
	var winners atomic.Int64
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			records := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
				LoopSeq:  9,
				Reason:   HumanAttentionManualIntervention,
				EntryKey: "queue:atomic_claim:t1",
			})
			if len(records) > 0 {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := winners.Load(); got != 1 {
		t.Fatalf("concurrent winners = %d, want exactly 1 claim", got)
	}
	if got := osascriptCalls.Load(); got != 1 {
		t.Fatalf("osascript deliveries = %d, want 1", got)
	}
	dedupeKey := HumanAttentionDedupeKey(HumanAttentionManualIntervention, "queue:atomic_claim:t1")
	latest, err := repos.Notifications.GetLatestByDedupe(ctx, "in_app", dedupeKey)
	if err != nil || latest == nil {
		t.Fatalf("GetLatestByDedupe() = (%v, %v), want reserved in_app row", latest, err)
	}
}

func TestNotifyHumanAttentionLocalOnlyUsesDurableFeishuTransport(t *testing.T) {
	t.Setenv("LOOPER_TEST_WEBHOOK_URL", "https://example.test/ha-transport")

	ctx := context.Background()
	rootDir := t.TempDir()
	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())

	// Seed a project + loop with durable hitl.transport=feishu.
	nowISO := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: "project_ha", Name: "ha", RepoPath: rootDir, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	meta := `{"hitl":{"question":"q","status":"awaiting","transport":"feishu"}}`
	targetID := "project_ha"
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: "loop_ha_feishu", Seq: 3, ProjectID: "project_ha", Type: "worker",
		TargetType: "project", TargetID: &targetID, Status: "awaiting_human",
		MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	var posts []capturedWebhookPost
	gateway := NewGateway(Options{
		Config: config.NotificationConfig{
			InApp:     true,
			Osascript: config.OsascriptNotificationConfig{Enabled: false, ThrottleWindowSeconds: 60},
			Webhook: config.WebhookNotificationConfig{
				Enabled: true, URLEnv: "LOOPER_TEST_WEBHOOK_URL", Format: "feishu", ThrottleWindowSeconds: 60,
			},
		},
		Repositories: repos,
		HTTPPost: func(url string, body []byte) (int, error) {
			posts = append(posts, capturedWebhookPost{url: url, body: append([]byte(nil), body...)})
			return 200, nil
		},
	})

	records := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
		LoopID:   "loop_ha_feishu",
		LoopSeq:  3,
		Reason:   HumanAttentionAwaitingHuman,
		EntryKey: "run:durable_feishu",
	})
	if len(posts) != 0 {
		t.Fatalf("durable feishu transport webhook posts = %d, want 0; records=%#v", len(posts), records)
	}
	if got := notificationStatus(records, "webhook"); got != "" {
		t.Fatalf("webhook status = %q, want absent when hitl.transport=feishu", got)
	}
	if got := notificationStatus(records, "in_app"); got != "success" {
		t.Fatalf("in_app status = %q, want success", got)
	}
}
