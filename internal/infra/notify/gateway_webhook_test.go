package notify

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

type capturedWebhookPost struct {
	url  string
	body []byte
}

func newWebhookGateway(t *testing.T, cfg config.WebhookNotificationConfig, posts *[]capturedWebhookPost) *Gateway {
	t.Helper()

	rootDir := t.TempDir()
	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)

	return NewGateway(Options{
		Config: config.NotificationConfig{
			InApp:     false,
			Osascript: config.OsascriptNotificationConfig{Enabled: false, ThrottleWindowSeconds: 60},
			Webhook:   cfg,
		},
		Repositories: repos,
		Now:          func() time.Time { return now },
		HTTPPost: func(url string, body []byte) (int, error) {
			*posts = append(*posts, capturedWebhookPost{url: url, body: append([]byte(nil), body...)})
			return 200, nil
		},
	})
}

func TestGatewayWebhookChannel(t *testing.T) {
	ctx := context.Background()

	actionRequired := SystemNotificationPayload{
		Level:      "action_required",
		Title:      "Looper Worker Needs Attention",
		Subtitle:   "task_1",
		Body:       "A worker paused for human input",
		EntityType: "task",
		EntityID:   "task_1",
		DedupeKey:  "worker.attention:task:task_1",
	}

	t.Run("disabled does not post and records skipped", func(t *testing.T) {
		var posts []capturedWebhookPost
		gateway := newWebhookGateway(t, config.WebhookNotificationConfig{
			Enabled:               false,
			Format:                "feishu",
			ThrottleWindowSeconds: 60,
		}, &posts)

		records := gateway.Notify(ctx, actionRequired)

		if len(posts) != 0 {
			t.Fatalf("posts = %d, want 0", len(posts))
		}
		if got := notificationStatus(records, "webhook"); got != "skipped" {
			t.Fatalf("webhook status = %q, want skipped", got)
		}
		if got := notificationError(records, "webhook"); got != "disabled" {
			t.Fatalf("webhook error = %q, want disabled", got)
		}
	})

	t.Run("enabled feishu action_required posts text", func(t *testing.T) {
		t.Setenv("LOOPER_TEST_WEBHOOK_URL", "https://example.test/hook")

		var posts []capturedWebhookPost
		gateway := newWebhookGateway(t, config.WebhookNotificationConfig{
			Enabled:               true,
			URLEnv:                "LOOPER_TEST_WEBHOOK_URL",
			Format:                "feishu",
			ThrottleWindowSeconds: 60,
		}, &posts)

		records := gateway.Notify(ctx, actionRequired)

		if len(posts) != 1 {
			t.Fatalf("posts = %d, want 1", len(posts))
		}
		if posts[0].url != "https://example.test/hook" {
			t.Fatalf("post url = %q, want https://example.test/hook", posts[0].url)
		}

		body := string(posts[0].body)
		assertContains(t, body, `"msg_type":"text"`)
		assertContains(t, body, "Looper Worker Needs Attention")
		assertContains(t, body, "A worker paused for human input")

		if got := notificationStatus(records, "webhook"); got != "success" {
			t.Fatalf("webhook status = %q, want success", got)
		}
	})

	t.Run("info level is filtered out", func(t *testing.T) {
		t.Setenv("LOOPER_TEST_WEBHOOK_URL", "https://example.test/hook")

		var posts []capturedWebhookPost
		gateway := newWebhookGateway(t, config.WebhookNotificationConfig{
			Enabled:               true,
			URLEnv:                "LOOPER_TEST_WEBHOOK_URL",
			Format:                "feishu",
			ThrottleWindowSeconds: 60,
		}, &posts)

		records := gateway.Notify(ctx, SystemNotificationPayload{
			Level: "info",
			Title: "Loop progressing",
			Body:  "Nothing to do",
		})

		if len(posts) != 0 {
			t.Fatalf("posts = %d, want 0", len(posts))
		}
		if got := notificationStatus(records, "webhook"); got != "skipped" {
			t.Fatalf("webhook status = %q, want skipped", got)
		}
		if got := notificationError(records, "webhook"); got != "level filtered" {
			t.Fatalf("webhook error = %q, want level filtered", got)
		}
	})

	t.Run("unset url env skips with no url", func(t *testing.T) {
		var posts []capturedWebhookPost
		gateway := newWebhookGateway(t, config.WebhookNotificationConfig{
			Enabled:               true,
			URLEnv:                "LOOPER_TEST_WEBHOOK_URL_UNSET",
			Format:                "feishu",
			ThrottleWindowSeconds: 60,
		}, &posts)

		records := gateway.Notify(ctx, actionRequired)

		if len(posts) != 0 {
			t.Fatalf("posts = %d, want 0", len(posts))
		}
		if got := notificationStatus(records, "webhook"); got != "skipped" {
			t.Fatalf("webhook status = %q, want skipped", got)
		}
		if got := notificationError(records, "webhook"); got != "no url" {
			t.Fatalf("webhook error = %q, want no url", got)
		}
	})
}

func TestGatewayHumanAttentionWebhookByReason(t *testing.T) {
	// awaiting_human without a Feishu ask must reach remote channels.
	// awaiting_human with a delivered Feishu ask stays local-only.
	// manual_intervention must reach an enabled generic webhook.
	t.Setenv("LOOPER_TEST_WEBHOOK_URL", "https://example.test/human-attention")

	ctx := context.Background()
	webhookCfg := config.WebhookNotificationConfig{
		Enabled:               true,
		URLEnv:                "LOOPER_TEST_WEBHOOK_URL",
		Format:                "feishu",
		ThrottleWindowSeconds: 60,
	}

	t.Run("awaiting_human without feishu ask posts webhook", func(t *testing.T) {
		var posts []capturedWebhookPost
		gateway := newWebhookGateway(t, webhookCfg, &posts)
		gateway.config.InApp = true

		records := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
			LoopSeq:  1,
			Reason:   HumanAttentionAwaitingHuman,
			EntryKey: "run:await_webhook_remote",
		})
		if len(posts) != 1 {
			t.Fatalf("awaiting_human without Feishu ask webhook posts = %d, want 1; records=%#v", len(posts), records)
		}
		if got := notificationStatus(records, "webhook"); got != "success" {
			t.Fatalf("awaiting_human without Feishu ask webhook status = %q, want success", got)
		}
	})

	t.Run("awaiting_human with feishu ask skips webhook", func(t *testing.T) {
		var posts []capturedWebhookPost
		gateway := newWebhookGateway(t, webhookCfg, &posts)
		gateway.config.InApp = true
		// Simulate a live Feishu ask card for this loop (SendHITLAsk state).
		gateway.state.askCards = map[string]askCardState{
			"loop_feishu_ask": {msgID: "om_ask_1", card: HITLAskCard{LoopID: "loop_feishu_ask"}},
		}

		records := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
			LoopID:   "loop_feishu_ask",
			LoopSeq:  1,
			Reason:   HumanAttentionAwaitingHuman,
			EntryKey: "run:await_webhook_skip",
		})
		if len(posts) != 0 {
			t.Fatalf("awaiting_human with Feishu ask webhook posts = %d, want 0", len(posts))
		}
		if got := notificationStatus(records, "webhook"); got != "" {
			t.Fatalf("awaiting_human with Feishu ask webhook status = %q, want absent (LocalOnly)", got)
		}
	})

	t.Run("manual_intervention posts webhook", func(t *testing.T) {
		var posts []capturedWebhookPost
		gateway := newWebhookGateway(t, webhookCfg, &posts)
		gateway.config.InApp = true

		records := gateway.NotifyHumanAttention(ctx, HumanAttentionInput{
			LoopSeq:  2,
			Reason:   HumanAttentionManualIntervention,
			EntryKey: "queue:q_mi:t1",
		})
		if len(posts) != 1 {
			t.Fatalf("manual_intervention webhook posts = %d, want 1; records=%#v", len(posts), records)
		}
		if got := notificationStatus(records, "webhook"); got != "success" {
			t.Fatalf("manual_intervention webhook status = %q, want success", got)
		}
		body := string(posts[0].body)
		assertContains(t, body, "Looper Needs Attention")
		assertContains(t, body, "manual intervention")
	})
}

func notificationError(records []storage.NotificationRecord, channel string) string {
	for _, record := range records {
		if record.Channel == channel {
			if record.ErrorMessage != nil {
				return *record.ErrorMessage
			}
			return ""
		}
	}

	return ""
}
