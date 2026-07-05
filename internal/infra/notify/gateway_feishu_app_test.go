package notify

import (
	"context"
	"encoding/json"
	"github.com/nexu-io/looper/internal/loops"
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

func TestBuildFeishuAskCardRendersDecisionBrief(t *testing.T) {
	card, err := buildFeishuAskCard(HITLAskCard{
		LoopSeq:  132,
		Repo:     "nexu-io/synclo-test",
		Title:    "welcome.txt 用哪种语言?",
		Question: "welcome.txt 用哪种语言?",
		Options:  []string{"中文", "英文"},

		SourceType:   "GitHub Issue",
		SourceRef:    "#132",
		SourceURL:    "https://github.com/nexu-io/synclo-test/issues/132",
		TriggerLogin: "lefarcen",

		Recommendation:    "README 都是中文,推荐中文。",
		RecommendedOption: "中文",
		Consequences:      map[string]string{"中文": "写\"欢迎…\"", "英文": "写\"Welcome…\""},
		Confidence:        "medium",
	})
	if err != nil {
		t.Fatalf("buildFeishuAskCard() error = %v", err)
	}
	raw := string(card)
	for _, want := range []string{
		"GitHub Issue #132", // source label
		"https://github.com/nexu-io/synclo-test/issues/132", // clickable link
		"由 @lefarcen 提出",                                    // trigger attribution
		"README 都是中文",                                       // recommendation
		"⭐ 中文 · 推荐",                                         // recommended option marked prominently
		"置信度 中",                                             // confidence
		"写\\\"Welcome",                                      // a consequence (quote json-escaped)
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("decision-brief card missing %q\ncard=%s", want, raw)
		}
	}

	// Answered state: buttons gone, "✅ 已选" shown, brief still present for review.
	answered, err := buildFeishuAskCard(HITLAskCard{
		LoopSeq: 132, Title: "welcome.txt 用哪种语言?", Question: "welcome.txt 用哪种语言?",
		Options: []string{"中文", "英文"}, Recommendation: "README 都是中文,推荐中文。",
		AnsweredWith: "中文",
	})
	if err != nil {
		t.Fatalf("buildFeishuAskCard(answered) error = %v", err)
	}
	ar := string(answered)
	if !strings.Contains(ar, "已选:中文") || !strings.Contains(ar, "已定夺") || !strings.Contains(ar, "README 都是中文") {
		t.Fatalf("answered card missing selection or brief: %s", ar)
	}
	if strings.Contains(ar, `"tag":"action"`) {
		t.Fatalf("answered card should have no clickable action buttons: %s", ar)
	}

	// A bare ask (no brief) must still render — the fields are optional.
	bare, err := buildFeishuAskCard(HITLAskCard{LoopSeq: 1, Question: "A or B?", Options: []string{"A", "B"}})
	if err != nil {
		t.Fatalf("buildFeishuAskCard(bare) error = %v", err)
	}
	if strings.Contains(string(bare), "⭐") || strings.Contains(string(bare), "置信度") {
		t.Fatalf("bare ask should not render brief decorations: %s", string(bare))
	}
}

func TestLiveStatusHelpers(t *testing.T) {
	if got := humanizeElapsedSeconds(134); got != "2m14s" {
		t.Fatalf("humanizeElapsedSeconds(134) = %q; want 2m14s", got)
	}
	if got := humanizeElapsedSeconds(45); got != "45s" {
		t.Fatalf("humanizeElapsedSeconds(45) = %q; want 45s", got)
	}
	tail := feishuActivityTail([]string{"read greet.js", "npm test → 12 pass"})
	if !strings.Contains(tail, "实时进度") || !strings.Contains(tail, "· read greet.js") {
		t.Fatalf("feishuActivityTail unexpected: %q", tail)
	}
	// Anchor brief: a human phrase from the latest tool line, not the raw command.
	if got := feishuPhaseFromTail([]string{"✅ git status --short", "✅ gh pr create --fill"}); got != "正在开 PR" {
		t.Fatalf("feishuPhaseFromTail(pr create) = %q; want 正在开 PR", got)
	}
	if got := feishuPhaseFromTail([]string{"✅ git push -u origin feat/x"}); got != "正在推送分支" {
		t.Fatalf("feishuPhaseFromTail(push) = %q; want 正在推送分支", got)
	}
	// feishuAnchorBrief falls back to the phase when no summary is in metadata, and
	// the live feed card carries the raw feed for the in-thread surface.
	if brief := feishuAnchorBrief(nil, []string{"✅ gh pr create --fill"}); brief != "🔧 正在开 PR" {
		t.Fatalf("feishuAnchorBrief = %q; want 🔧 正在开 PR", brief)
	}
	if _, ok := feishuLiveFeedCard(nil, 0); ok {
		t.Fatalf("feishuLiveFeedCard(empty) should not render")
	}
	if card, ok := feishuLiveFeedCard([]string{"✅ git push"}, 90); !ok || !strings.Contains(card, "实时进度同步") {
		t.Fatalf("feishuLiveFeedCard missing header: %q", card)
	}
	// Milestone narrative renders "HH:MM · text".
	ml := feishuMilestoneList([]loops.Milestone{{At: "2026-07-05T10:30:00.000Z", Text: "已定夺:中文"}, {At: "2026-07-05T10:50:00.000Z", Text: "🔀 已开 PR #9"}})
	if !strings.Contains(ml, "进展") || !strings.Contains(ml, "已定夺:中文") || !strings.Contains(ml, "已开 PR #9") {
		t.Fatalf("feishuMilestoneList unexpected: %q", ml)
	}
	// Terminal detection gates the takeover hint (shown only while live).
	for _, s := range []string{"completed", "failed", "terminated", "stopped", "merged"} {
		if !feishuLoopStatusTerminal(s) {
			t.Fatalf("feishuLoopStatusTerminal(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"running", "queued", "awaiting_human", "human_takeover", "paused"} {
		if feishuLoopStatusTerminal(s) {
			t.Fatalf("feishuLoopStatusTerminal(%q) = true, want false", s)
		}
	}
	// PR/issue number extraction for the anchor's source + milestone lines.
	for u, want := range map[string]string{
		"https://github.com/o/r/issues/153":   "153",
		"https://github.com/o/r/pull/154":     "154",
		"https://github.com/o/r/pull/154/":    "154",
		"https://github.com/o/r/pull/154?x=1": "154",
		"https://github.com/o/r/tree/main":    "",
		"":                                    "",
	} {
		if got := urlTrailingNumber(u); got != want {
			t.Fatalf("urlTrailingNumber(%q) = %q; want %q", u, got, want)
		}
	}
	// In-memory live tail store (kept off the loop record to avoid DB races).
	g := &Gateway{liveTails: map[string]liveTailEntry{"loop_x": {lines: []string{"a", "b"}, elapsedSec: 90}}}
	lines, el := g.liveTailFor("loop_x")
	if len(lines) != 2 || lines[0] != "a" || el != 90 {
		t.Fatalf("liveTailFor = %v, %d", lines, el)
	}
	if l, e := g.liveTailFor("unknown"); l != nil || e != 0 {
		t.Fatalf("liveTailFor(unknown) = %v, %d; want nil, 0", l, e)
	}
}

func TestFeishuLoopStatusStyle(t *testing.T) {
	cases := map[string]struct{ template, contains string }{
		"awaiting_human": {"orange", "等你定夺"},
		"completed":      {"green", "已完成"},
		"failed":         {"red", "需要处理"},
		"abandoned":      {"red", "需要处理"},
		"running":        {"blue", "处理中"},
		"":               {"blue", "处理中"},
	}
	for status, want := range cases {
		gotT, gotL := feishuLoopStatusStyle(status)
		if gotT != want.template || !strings.Contains(gotL, want.contains) {
			t.Fatalf("feishuLoopStatusStyle(%q) = (%q, %q); want template %q label~%q", status, gotT, gotL, want.template, want.contains)
		}
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
