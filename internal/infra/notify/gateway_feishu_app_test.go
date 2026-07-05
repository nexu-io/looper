package notify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

type capturedFeishuCall struct {
	method  string
	url     string
	headers map[string]string
	body    []byte
}

func newFeishuAppGateway(t *testing.T, cfg config.WebhookNotificationConfig, calls *[]capturedFeishuCall) *Gateway {
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
		FeishuAppHTTP: func(_ context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
			*calls = append(*calls, capturedFeishuCall{method: method, url: url, headers: headers, body: append([]byte(nil), body...)})
			if strings.Contains(url, "/auth/v3/tenant_access_token/internal") {
				return 200, []byte(`{"code":0,"msg":"ok","tenant_access_token":"t-abc123","expire":7200}`), nil
			}
			return 200, []byte(`{"code":0,"msg":"success","data":{"message_id":"om_msg"}}`), nil
		},
	})
}

func appModeConfig() config.WebhookNotificationConfig {
	return config.WebhookNotificationConfig{
		Enabled:               true,
		Format:                "feishu",
		Mode:                  "app",
		AppIDEnv:              "LOOPER_TEST_FEISHU_APP_ID",
		AppSecretEnv:          "LOOPER_TEST_FEISHU_APP_SECRET",
		ChatID:                "oc_group_chat_123",
		ThrottleWindowSeconds: 60,
	}
}

func TestGatewayFeishuAppChannel(t *testing.T) {
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

	t.Run("app mode fetches token then posts text message", func(t *testing.T) {
		t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
		t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)

		records := gateway.Notify(ctx, actionRequired)

		if len(calls) != 2 {
			t.Fatalf("feishu calls = %d, want 2 (token + message)", len(calls))
		}

		// First call: tenant_access_token with app id/secret from env.
		token := calls[0]
		if !strings.Contains(token.url, "/open-apis/auth/v3/tenant_access_token/internal") {
			t.Fatalf("first call url = %q, want token endpoint", token.url)
		}
		var tokenBody map[string]string
		if err := json.Unmarshal(token.body, &tokenBody); err != nil {
			t.Fatalf("token body not JSON: %v", err)
		}
		if tokenBody["app_id"] != "cli_app_id" || tokenBody["app_secret"] != "app_secret_value" {
			t.Fatalf("token body = %#v, want app id/secret from env", tokenBody)
		}

		// Second call: interactive message to the chat, bearer token attached.
		msg := calls[1]
		if !strings.Contains(msg.url, "/open-apis/im/v1/messages?receive_id_type=chat_id") {
			t.Fatalf("second call url = %q, want im messages endpoint", msg.url)
		}
		if msg.headers["Authorization"] != "Bearer t-abc123" {
			t.Fatalf("message Authorization = %q, want Bearer t-abc123", msg.headers["Authorization"])
		}
		var envelope struct {
			ReceiveID string `json:"receive_id"`
			MsgType   string `json:"msg_type"`
			Content   string `json:"content"`
		}
		if err := json.Unmarshal(msg.body, &envelope); err != nil {
			t.Fatalf("message body not JSON: %v", err)
		}
		if envelope.ReceiveID != "oc_group_chat_123" {
			t.Fatalf("receive_id = %q, want oc_group_chat_123", envelope.ReceiveID)
		}
		// System updates are plain text now (only mid-run asks are cards).
		if envelope.MsgType != "text" {
			t.Fatalf("msg_type = %q, want text", envelope.MsgType)
		}
		// content is a JSON string {"text":"..."} with title + subtitle + body.
		if !strings.Contains(envelope.Content, "Looper Worker Needs Attention") {
			t.Fatalf("text content missing title: %s", envelope.Content)
		}
		if !strings.Contains(envelope.Content, "A worker paused for human input") {
			t.Fatalf("text content missing body: %s", envelope.Content)
		}

		if got := notificationStatus(records, "feishu_app"); got != "success" {
			t.Fatalf("feishu_app status = %q, want success", got)
		}
		if notificationStatus(records, "webhook") != "" {
			t.Fatal("webhook channel should not be recorded in app mode")
		}
	})

	t.Run("failure level posts plain text", func(t *testing.T) {
		t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
		t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)

		gateway.Notify(ctx, SystemNotificationPayload{Level: "failure", Title: "Run failed", Body: "boom"})
		if len(calls) != 2 {
			t.Fatalf("feishu calls = %d, want 2", len(calls))
		}
		var envelope struct {
			MsgType string `json:"msg_type"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(calls[1].body, &envelope); err != nil {
			t.Fatalf("message body not JSON: %v", err)
		}
		if envelope.MsgType != "text" {
			t.Fatalf("msg_type = %q, want text", envelope.MsgType)
		}
		if !strings.Contains(envelope.Content, "Run failed") || !strings.Contains(envelope.Content, "boom") {
			t.Fatalf("text content missing title/body: %s", envelope.Content)
		}
	})

	t.Run("loop notifications thread under a per-loop root", func(t *testing.T) {
		t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
		t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)

		// First notification for the loop: token + root header + the message reply.
		gateway.Notify(ctx, SystemNotificationPayload{Level: "action_required", LoopID: "loop_thread_1", Title: "PR opened", Body: "https://example/pr/1"})
		if len(calls) != 3 {
			t.Fatalf("first loop notification calls = %d, want 3 (token + root + reply)", len(calls))
		}
		// calls[1] is the root header, posted top-level as text.
		var root struct {
			MsgType string `json:"msg_type"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(calls[1].body, &root); err != nil {
			t.Fatalf("root body not JSON: %v", err)
		}
		if root.MsgType != "text" {
			t.Fatalf("root msg_type = %q, want text", root.MsgType)
		}
		if !strings.Contains(calls[1].url, "/im/v1/messages?receive_id_type=chat_id") {
			t.Fatalf("root should be posted top-level, url = %q", calls[1].url)
		}
		// calls[2] is the actual notification, threaded as a reply to the root.
		if !strings.Contains(calls[2].url, "/im/v1/messages/om_msg/reply") {
			t.Fatalf("notification should reply to root om_msg, url = %q", calls[2].url)
		}
		var reply map[string]any
		if err := json.Unmarshal(calls[2].body, &reply); err != nil {
			t.Fatalf("reply body not JSON: %v", err)
		}
		if reply["reply_in_thread"] != true {
			t.Fatalf("reply_in_thread = %v, want true", reply["reply_in_thread"])
		}

		// Second notification for the SAME loop reuses the root: just one reply call.
		before := len(calls)
		gateway.Notify(ctx, SystemNotificationPayload{Level: "action_required", LoopID: "loop_thread_1", Title: "done", Body: "merged"})
		if got := len(calls) - before; got != 1 {
			t.Fatalf("second notification calls = %d, want 1 (root reused, reply only)", got)
		}
		if !strings.Contains(calls[before].url, "/im/v1/messages/om_msg/reply") {
			t.Fatalf("second notification should reply to cached root, url = %q", calls[before].url)
		}
	})

	t.Run("disabled records skipped without any call", func(t *testing.T) {
		cfg := appModeConfig()
		cfg.Enabled = false
		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, cfg, &calls)

		records := gateway.Notify(ctx, actionRequired)
		if len(calls) != 0 {
			t.Fatalf("feishu calls = %d, want 0", len(calls))
		}
		if got := notificationStatus(records, "feishu_app"); got != "skipped" {
			t.Fatalf("feishu_app status = %q, want skipped", got)
		}
		if got := notificationError(records, "feishu_app"); got != "disabled" {
			t.Fatalf("feishu_app error = %q, want disabled", got)
		}
	})

	t.Run("missing credentials records skipped", func(t *testing.T) {
		// Env vars intentionally unset.
		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)

		records := gateway.Notify(ctx, actionRequired)
		if len(calls) != 0 {
			t.Fatalf("feishu calls = %d, want 0", len(calls))
		}
		if got := notificationStatus(records, "feishu_app"); got != "skipped" {
			t.Fatalf("feishu_app status = %q, want skipped", got)
		}
		if got := notificationError(records, "feishu_app"); got != "no app credentials" {
			t.Fatalf("feishu_app error = %q, want no app credentials", got)
		}
	})

	t.Run("SendHITLAsk posts a card with option buttons carrying loop seq + answer", func(t *testing.T) {
		t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
		t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)

		if err := gateway.SendHITLAsk(ctx, HITLAskCard{ProjectID: "od", LoopSeq: 71, Repo: "acme/looper", Title: "Which datastore?", Question: "Redis or Postgres for the cache?", Options: []string{"redis", "postgres"}}); err != nil {
			t.Fatalf("SendHITLAsk() error = %v", err)
		}
		if len(calls) != 2 {
			t.Fatalf("feishu calls = %d, want 2 (token + message)", len(calls))
		}
		var envelope struct {
			MsgType string `json:"msg_type"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(calls[1].body, &envelope); err != nil {
			t.Fatalf("message body not JSON: %v", err)
		}
		if envelope.MsgType != "interactive" {
			t.Fatalf("msg_type = %q, want interactive", envelope.MsgType)
		}
		if !strings.Contains(envelope.Content, "Redis or Postgres") {
			t.Fatalf("card missing question: %s", envelope.Content)
		}
		// Each option becomes a button whose value carries loopSeq + answer.
		if !strings.Contains(envelope.Content, `"loopSeq":"71"`) || !strings.Contains(envelope.Content, `"answer":"redis"`) || !strings.Contains(envelope.Content, `"answer":"postgres"`) {
			t.Fatalf("card missing option buttons with loopSeq/answer values: %s", envelope.Content)
		}
	})

	t.Run("SendHITLAsk errors when app not configured", func(t *testing.T) {
		cfg := appModeConfig()
		cfg.ChatID = ""
		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, cfg, &calls)
		if err := gateway.SendHITLAsk(ctx, HITLAskCard{LoopSeq: 1, Question: "q"}); err == nil {
			t.Fatal("SendHITLAsk() error = nil, want error when chatId missing")
		}
		if len(calls) != 0 {
			t.Fatalf("feishu calls = %d, want 0 when unconfigured", len(calls))
		}
	})

	t.Run("info level filtered out", func(t *testing.T) {
		t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
		t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)

		records := gateway.Notify(ctx, SystemNotificationPayload{Level: "info", Title: "progress", Body: "nothing"})
		if len(calls) != 0 {
			t.Fatalf("feishu calls = %d, want 0", len(calls))
		}
		if got := notificationStatus(records, "feishu_app"); got != "skipped" {
			t.Fatalf("feishu_app status = %q, want skipped", got)
		}
		if got := notificationError(records, "feishu_app"); got != "level filtered" {
			t.Fatalf("feishu_app error = %q, want level filtered", got)
		}
	})
}

func TestBuildFeishuAskCardRendersMention(t *testing.T) {
	card, err := buildFeishuAskCard(HITLAskCard{
		LoopSeq: 7, Repo: "acme/looper", Question: "A or B?",
		Options:        []string{"A", "B"},
		MentionOpenIds: []string{"ou_abc123", " ", "ou_def456"},
	})
	if err != nil {
		t.Fatalf("buildFeishuAskCard() error = %v", err)
	}
	// json.Marshal escapes < and > to </>; Feishu unescapes them back, so
	// decode the card and inspect the element content the way Feishu will see it.
	if !strings.Contains(cardText(t, card), "<at id=ou_abc123></at>") || !strings.Contains(cardText(t, card), "<at id=ou_def456></at>") {
		t.Fatalf("card missing @-mention tags: %s", cardText(t, card))
	}

	// No mention configured -> no <at> tag.
	plain, err := buildFeishuAskCard(HITLAskCard{LoopSeq: 7, Question: "A or B?", Options: []string{"A"}})
	if err != nil {
		t.Fatalf("buildFeishuAskCard(no mention) error = %v", err)
	}
	if strings.Contains(cardText(t, plain), "<at ") {
		t.Fatalf("unexpected @-mention when none configured: %s", cardText(t, plain))
	}
}

// cardText concatenates all lark_md element contents from a card JSON, decoded
// (so JSON < escapes appear as the < the way Feishu renders them).
func cardText(t *testing.T, card []byte) string {
	t.Helper()
	var parsed struct {
		Elements []struct {
			Text struct {
				Content string `json:"content"`
			} `json:"text"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(card, &parsed); err != nil {
		t.Fatalf("card not JSON: %v", err)
	}
	parts := make([]string, 0, len(parsed.Elements))
	for _, e := range parsed.Elements {
		parts = append(parts, e.Text.Content)
	}
	return strings.Join(parts, "\n")
}
