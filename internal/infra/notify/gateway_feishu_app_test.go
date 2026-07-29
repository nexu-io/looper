package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

type capturedFeishuCall struct {
	method  string
	url     string
	headers map[string]string
	body    []byte
}

func newFeishuAppGateway(t *testing.T, cfg config.WebhookNotificationConfig, calls *[]capturedFeishuCall, states ...*GatewayState) *Gateway {
	t.Helper()

	rootDir := t.TempDir()
	coordinator := openNotifyCoordinator(t, rootDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)
	var state *GatewayState
	if len(states) > 0 {
		state = states[0]
	}

	return NewGateway(Options{
		Config: config.NotificationConfig{
			InApp:     false,
			Osascript: config.OsascriptNotificationConfig{Enabled: false, ThrottleWindowSeconds: 60},
			Webhook:   cfg,
		},
		Repositories: repos,
		State:        state,
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

	t.Run("SendHITLAsk posts a one-way card with exact action URL", func(t *testing.T) {
		t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
		t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

		var calls []capturedFeishuCall
		gateway := newFeishuAppGateway(t, appModeConfig(), &calls)

		if err := gateway.SendHITLAsk(ctx, HITLAskCard{ProjectID: "od", LoopSeq: 71, Repo: "acme/looper", Title: "Which datastore?", Question: "Redis or Postgres for the cache?", Options: []string{"redis", "postgres"}, SourceURL: "https://github.com/acme/looper/pull/7#issuecomment-71"}); err != nil {
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
		if !strings.Contains(envelope.Content, "https://github.com/acme/looper/pull/7#issuecomment-71") || strings.Contains(envelope.Content, `"answer":"redis"`) {
			t.Fatalf("card must deep-link outward without callback values: %s", envelope.Content)
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

func TestFeishuCardOutboxRetriesAndCreatesRunlessCoordinatorAnchor(t *testing.T) {
	t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
	t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")
	ctx := context.Background()
	coordinator := openNotifyCoordinator(t, t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	projectID := "project_1"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Project", RepoPath: t.TempDir(), CreatedAt: eventISO(now), UpdatedAt: eventISO(now)}); err != nil {
		t.Fatal(err)
	}
	repo := "acme/looper"
	target := "issue:acme/looper:42"
	metadata := `{"issueNumber":42,"issueUrl":"https://plane.test/issues/42","title":"Runless intake"}`
	loop := storage.LoopRecord{ID: "loop_coordinator", Seq: 42, ProjectID: projectID, Type: "coordinator", TargetType: "issue", TargetID: &target, Repo: &repo, Status: "running", MetadataJSON: &metadata, CreatedAt: eventISO(now), UpdatedAt: eventISO(now)}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	messageAttempts := 0
	gateway := NewGateway(Options{
		Config: config.NotificationConfig{Webhook: appModeConfig()}, Repositories: repos, Now: func() time.Time { return now },
		FeishuAppHTTP: func(_ context.Context, _ string, url string, _ map[string]string, _ []byte) (int, []byte, error) {
			if strings.Contains(url, "/auth/v3/tenant_access_token/internal") {
				return 200, []byte(`{"code":0,"tenant_access_token":"token","expire":7200}`), nil
			}
			messageAttempts++
			if messageAttempts <= 2 {
				return 500, []byte(`{"code":1,"msg":"temporary"}`), nil
			}
			return 200, []byte(`{"code":0,"data":{"message_id":"om_` + strconv.Itoa(messageAttempts) + `"}}`), nil
		},
	})
	err := gateway.PostThreadDecisionCard(ctx, loop.ID, "Choose A or B", "https://plane.test/pages/p#comment-c", []string{"ou_owner", "ou_extra"})
	if err == nil {
		t.Fatal("first delivery error = nil, want transient failure")
	}
	records, _ := repos.Notifications.List(ctx, 10)
	if len(records) != 1 || records[0].Status != "pending" {
		t.Fatalf("outbox after failure = %#v", records)
	}
	now = now.Add(15 * time.Second)
	if retried, err := gateway.RetryPendingCards(ctx, 10); err != nil || retried != 1 {
		t.Fatalf("RetryPendingCards() = %d, %v", retried, err)
	}
	records, _ = repos.Notifications.List(ctx, 10)
	if records[0].Status != "delivered" || records[0].SentAt == nil {
		t.Fatalf("outbox after retry = %#v", records[0])
	}
	root, err := repos.FeishuThreads.RootByLoop(ctx, loop.ID)
	if err != nil || root == "" {
		t.Fatalf("run-less coordinator root = %q, %v", root, err)
	}
	if runs, err := repos.Runs.ListByLoop(ctx, loop.ID); err != nil || len(runs) != 0 {
		t.Fatalf("coordinator runs = %#v, %v; anchor must not require a run", runs, err)
	}
}

func eventISO(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func TestFeishuHeaderFallbackNeverLeaksLoopID(t *testing.T) {
	gateway := &Gateway{}
	text := gateway.feishuThreadHeaderText(context.Background(), "loop_secret_internal_id")
	if strings.Contains(text, "loop_secret_internal_id") {
		t.Fatalf("fallback leaked internal loop id: %q", text)
	}
}

func TestPostThreadApprovalCardUsesApprovalCopyAndSeparateMessageKey(t *testing.T) {
	t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
	t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

	var calls []capturedFeishuCall
	gateway := newFeishuAppGateway(t, appModeConfig(), &calls)
	ctx := context.Background()
	now := eventISO(time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC))
	if err := gateway.repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project_1", Name: "Project", RepoPath: t.TempDir(), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	loop := storage.LoopRecord{ID: "loop_approval", Seq: 42, ProjectID: "project_1", Type: "planner", TargetType: "issue", Status: "completed", CreatedAt: now, UpdatedAt: now}
	if err := gateway.repositories.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}

	if err := gateway.PostThreadApprovalCard(ctx, loop.ID, "方案已通过 REVIEW", "https://plane.test/pages/spec#comment-review", []string{"ou_looper_owner"}); err != nil {
		t.Fatalf("PostThreadApprovalCard() error = %v", err)
	}
	var bodies strings.Builder
	for _, call := range calls {
		bodies.Write(call.body)
	}
	posted := bodies.String()
	if !strings.Contains(posted, "技术方案已完成 GRILL + REVIEW") || !strings.Contains(posted, "前往 Plane 审核") {
		t.Fatalf("Feishu calls missing approval-specific copy: %s", posted)
	}
	if strings.Contains(posted, "这个需求有个地方需要你来拍板") {
		t.Fatalf("approval card reused product-decision copy: %s", posted)
	}
	if got := gateway.loopMetaString(ctx, loop.ID, approvalCardMsgIDKey); got == "" {
		t.Fatal("approval card message id was not persisted")
	}
	if got := gateway.loopMetaString(ctx, loop.ID, decisionCardMsgIDKey); got != "" {
		t.Fatalf("product decision message id = %q, want empty", got)
	}
}

func TestPostThreadProductSpecCardPointsToPlaneSpecWork(t *testing.T) {
	t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
	t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

	var calls []capturedFeishuCall
	gateway := newFeishuAppGateway(t, appModeConfig(), &calls)
	ctx := context.Background()
	now := eventISO(time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC))
	if err := gateway.repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project_1", Name: "Project", RepoPath: t.TempDir(), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	loop := storage.LoopRecord{ID: "loop_product_spec", Seq: 43, ProjectID: "project_1", Type: "planner", TargetType: "issue", Status: "awaiting_human", CreatedAt: now, UpdatedAt: now}
	if err := gateway.repositories.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}

	if err := gateway.PostThreadProductSpecCard(ctx, loop.ID, "请补齐首版范围", "https://plane.test/issues/582#comment-product", []string{"ou_product_owner"}); err != nil {
		t.Fatalf("PostThreadProductSpecCard() error = %v", err)
	}
	var bodies strings.Builder
	for _, call := range calls {
		bodies.Write(call.body)
	}
	posted := bodies.String()
	if !strings.Contains(posted, "请先补充产品 spec") || !strings.Contains(posted, "前往 Plane 补 spec") {
		t.Fatalf("Feishu calls missing product-spec copy: %s", posted)
	}
	if strings.Contains(posted, "技术方案已完成") || strings.Contains(posted, "这个需求有个地方需要你来拍板") {
		t.Fatalf("product-spec card reused another gate's copy: %s", posted)
	}
	if got := gateway.loopMetaString(ctx, loop.ID, decisionCardMsgIDKey); got == "" {
		t.Fatal("product-spec card message id was not persisted")
	}
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

func TestFeishuCardsAttributeTheLocalLooperOwner(t *testing.T) {
	card, err := buildFeishuAskCard(HITLAskCard{
		LoopSeq: 9, Question: "是否继续?", Options: []string{"继续"}, OwnerOpenID: "ou_local_owner",
	})
	if err != nil {
		t.Fatalf("buildFeishuAskCard() error = %v", err)
	}
	if raw := string(card); !strings.Contains(raw, "来自 ") || !strings.Contains(raw, "ou_local_owner") || !strings.Contains(raw, " 的 Looper") {
		t.Fatalf("ask card missing local Looper owner attribution: %s", raw)
	}

	// Membership is deliberately not checked: an open_id outside the destination
	// group is still emitted as an @ tag (Feishu may render it grey).
	if card, ok := feishuLiveFeedCardWithOwner([]string{"✅ git status"}, 3, "ou_not_in_chat"); !ok || !strings.Contains(card, "ou_not_in_chat") || !strings.Contains(card, "来自 ") {
		t.Fatalf("live card must preserve out-of-chat owner attribution: %q", card)
	}

	fallback, err := buildFeishuAskCard(HITLAskCard{LoopSeq: 10, Question: "A?", Options: []string{"A"}})
	if err != nil {
		t.Fatalf("buildFeishuAskCard(no owner) error = %v", err)
	}
	if !strings.Contains(string(fallback), "未配置 owner") {
		t.Fatalf("missing-owner card should diagnose attribution config: %s", fallback)
	}

	coordinator := openNotifyCoordinator(t, t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	now := "2026-07-17T12:00:00.000Z"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "open-design", Name: "Open Design", RepoPath: t.TempDir(), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	title := `{"worker":{"issueUrl":"https://plane.example/issues/582"},"issueTitle":"导出还原度"}`
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop-owner", Seq: 11, ProjectID: "open-design", Type: "planner", TargetType: "issue", Status: "running", MetadataJSON: &title, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	gateway := NewGateway(Options{Repositories: repos, ResolveOwnerOpenID: func(projectID string) string {
		if projectID != "open-design" {
			t.Fatalf("owner resolver project = %q", projectID)
		}
		return "ou_coworker_owner"
	}})
	anchor, ok := gateway.feishuThreadHeaderCard(context.Background(), "loop-owner")
	if !ok || !strings.Contains(anchor, "ou_coworker_owner") || !strings.Contains(anchor, "来自 ") {
		t.Fatalf("task anchor missing deployment owner attribution: %q", anchor)
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
		"⭐ 中文（推荐）",                                          // recommended option marked prominently
		"置信度 中",                                             // confidence
		"写\\\"Welcome",                                      // a consequence (quote json-escaped)
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("decision-brief card missing %q\ncard=%s", want, raw)
		}
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
	// An UNRECOGNISED command must never leak onto the human-scannable anchor as a
	// raw shell line (P2: e.g. `tmpdir=$(mktemp -d …`) — it becomes a generic phase.
	if got := feishuPhaseFromTail([]string{"✅ tmpdir=$(mktemp -d /private/tmp/looper.XXXX)"}); got != "正在处理…" || strings.Contains(got, "mktemp") {
		t.Fatalf("feishuPhaseFromTail(unknown) = %q; want generic phase, no raw command", got)
	}
	// feishuAnchorBrief falls back to the phase when no summary is in metadata, and
	// the live feed card carries the raw feed for the in-thread surface.
	if brief := feishuAnchorBrief(nil, []string{"✅ gh pr create --fill"}); brief != "🔧 正在开 PR" {
		t.Fatalf("feishuAnchorBrief = %q; want 🔧 正在开 PR", brief)
	}
	if _, ok := feishuLiveFeedCard(nil, 0); ok {
		t.Fatalf("feishuLiveFeedCard(empty) should not render")
	}
	// Header-less now — the body leads with 🔧 实时进度, so no separate title.
	if card, ok := feishuLiveFeedCard([]string{"✅ git push"}, 90); !ok || strings.Contains(card, "\"header\"") || !strings.Contains(card, "🔧 实时进度") {
		t.Fatalf("feishuLiveFeedCard: want header-less card with 🔧 实时进度 body, got %q", card)
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
	g := &Gateway{state: &GatewayState{
		liveTails: map[string]liveTailEntry{
			"loop_x": {lines: []string{"a", "b"}, elapsedSec: 90},
		},
	}}
	lines, el := g.liveTailFor("loop_x")
	if len(lines) != 2 || lines[0] != "a" || el != 90 {
		t.Fatalf("liveTailFor = %v, %d", lines, el)
	}
	if l, e := g.liveTailFor("unknown"); l != nil || e != 0 {
		t.Fatalf("liveTailFor(unknown) = %v, %d; want nil, 0", l, e)
	}
}

func TestFeishuLoopFlowchartStyle(t *testing.T) {
	cases := []struct {
		name         string
		loopType     string
		status       string
		hasPR        bool
		awaitingSpec bool
		nodeHPhase   string
		template     string
		contains     string
	}{
		// Running lanes — the header names the flowchart node by role.
		{"coordinator triages", "coordinator", "running", false, false, "", "blue", "分诊中"},
		{"planner writes tech spec", "planner", "running", false, false, "", "blue", "编写技术方案中"},
		{"worker implements", "worker", "running", false, false, "", "blue", "实现中"},
		{"reviewer reviews", "reviewer", "running", false, false, "", "blue", "评审中"},
		{"fixer fixes", "fixer", "running", false, false, "", "blue", "修复中"},
		{"unknown role falls back", "", "running", false, false, "", "blue", "处理中"},
		// node H sub-phases (planner spec pipeline).
		{"grilling", "planner", "running", false, false, "grilling", "blue", "方案拷问中"},
		{"reviewing", "planner", "running", false, false, "reviewing", "blue", "spec 评审中"},
		{"awaiting human review", "planner", "running", false, false, "awaiting_human_review", "orange", "需要人类审核 spec"},
		{"phase wins over generic completed", "planner", "completed", false, false, "grilling", "blue", "方案拷问中"},
		// node E hold: product-spec wait wins over a node H phase and a generic ask.
		{"planner awaits product spec", "planner", "awaiting_human", false, true, "", "orange", "等待产品方案"},
		{"generic HITL ask", "worker", "awaiting_human", false, false, "", "orange", "等你定夺"},
		{"awaiting spec while running", "planner", "running", false, true, "", "orange", "等待产品方案"},
		// node H: a completed planner (no phase marker) awaits a human's approve.
		{"planner completed → needs human review", "planner", "completed", false, false, "", "orange", "需要人类审核 spec"},
		// Worker delivery + terminals. An OPEN PR is NOT delivered — a just-opened impl
		// PR defaults to 待 review (never the old "已交付 · 待合并"); the real state is
		// layered on from the PR snapshot (§A). 已交付 is reserved for a MERGED PR.
		{"worker delivered a PR", "worker", "completed", true, false, "", "blue", "待 review"},
		{"worker completed no PR", "worker", "completed", false, false, "", "green", "已完成"},
		{"merged terminal", "worker", "merged", false, false, "", "green", "已合并"},
		{"failed terminal", "worker", "failed", false, false, "", "red", "需要处理"},
		{"abandoned terminal", "planner", "abandoned", false, false, "", "red", "需要处理"},
	}
	for _, want := range cases {
		gotT, gotL := feishuLoopFlowchartStyle(want.loopType, want.status, want.hasPR, want.awaitingSpec, want.nodeHPhase, "", 0)
		if gotT != want.template || !strings.Contains(gotL, want.contains) {
			t.Fatalf("%s: feishuLoopFlowchartStyle(%q,%q,%v,%v,%q) = (%q,%q); want template %q label~%q", want.name, want.loopType, want.status, want.hasPR, want.awaitingSpec, want.nodeHPhase, gotT, gotL, want.template, want.contains)
		}
	}
	// A just-opened impl PR must never say 已交付, and carries its PR number when known.
	if tpl, label := feishuLoopFlowchartStyle("worker", "completed", true, false, "", "", 907); tpl != "blue" || !strings.Contains(label, "待 review") || !strings.Contains(label, "PR #907") || strings.Contains(label, "已交付") {
		t.Fatalf("delivered worker PR = (%q,%q); want blue 待 review · PR #907, never 已交付", tpl, label)
	}
}

func TestIntakeOutcomeStyle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		outcome  string
		ok       bool
		template string
		contains string
	}{
		{"routed_plan", true, "blue", "写技术方案"},
		{"routed_worker", true, "blue", "待实现"},
		{"hold_product", true, "orange", "等待产品方案"},
		{"needs_human", true, "orange", "转人工"},
		{"out_of_scope", true, "grey", "超出范围"},
		{"OUT_OF_SCOPE", true, "grey", "超出范围"}, // case-insensitive
		{"unknown", false, "", ""},
		{"", false, "", ""},
	}
	for _, tc := range cases {
		got, ok := intakeOutcomeStyle(tc.outcome)
		if ok != tc.ok {
			t.Fatalf("intakeOutcomeStyle(%q) ok = %v, want %v", tc.outcome, ok, tc.ok)
		}
		if !tc.ok {
			continue
		}
		if got.template != tc.template || !strings.Contains(got.label, tc.contains) {
			t.Fatalf("intakeOutcomeStyle(%q) = (%q,%q); want template %q label~%q", tc.outcome, got.template, got.label, tc.template, tc.contains)
		}
	}
}

func TestAnchorOutcomeOverrideRoundTrip(t *testing.T) {
	t.Parallel()
	g := &Gateway{}
	if _, ok := g.anchorOutcomeOverride("loop-x"); ok {
		t.Fatal("no override should exist before FinalizeIntakeAnchor")
	}
	// FinalizeIntakeAnchor with app-bot unconfigured still records the override (the
	// RefreshThreadHeader tail no-ops), so the next render picks it up.
	g.FinalizeIntakeAnchor(context.Background(), "loop-x", "needs_human")
	style, ok := g.anchorOutcomeOverride("loop-x")
	if !ok || !strings.Contains(style.label, "转人工") {
		t.Fatalf("anchorOutcomeOverride(loop-x) = (%+v,%v); want the 转人工 override", style, ok)
	}
	// An unknown outcome records nothing (falls through to the loop's real status).
	g.FinalizeIntakeAnchor(context.Background(), "loop-y", "mystery")
	if _, ok := g.anchorOutcomeOverride("loop-y"); ok {
		t.Fatal("an unknown outcome must not record an override")
	}
}

func TestPRCardStateFromSnapshotMapsReviewCycle(t *testing.T) {
	cases := []struct {
		name       string
		review     string
		checks     string
		labels     []string
		unresolved int64
		want       prCardState
	}{
		{"failing CI wins", "APPROVED", "success, failure", nil, 0, prCardStateChecksFailed},
		{"changes requested", "CHANGES_REQUESTED", "success", nil, 0, prCardStateChangesRequested},
		{"CI still running", "REVIEW_REQUIRED", "success, pending", nil, 0, prCardStateChecksRunning},
		{"approved + green", "APPROVED", "success, success", nil, 0, prCardStateApproved},
		{"awaiting review", "REVIEW_REQUIRED", "success", nil, 2, prCardStateReviewPending},
		{"no decision yet", "", "", nil, 0, prCardStateReviewPending},
		{"in_progress running", "", "in_progress", nil, 0, prCardStateChecksRunning},
		// QA validation gate: APPROVED splits on the needs-validation / validated labels.
		{"approved + needs-validation → awaiting QA", "APPROVED", "success", []string{"needs-validation"}, 0, prCardStateAwaitingValidation},
		{"approved + validated → validated", "APPROVED", "success", []string{"validated"}, 0, prCardStateValidated},
		{"approved + validated wins over needs-validation", "APPROVED", "success", []string{"needs-validation", "validated"}, 0, prCardStateValidated},
		{"approved + no gate label → plain approved", "APPROVED", "success", []string{"enhancement"}, 0, prCardStateApproved},
		{"needs-validation label is case-insensitive", "APPROVED", "success", []string{"Needs-Validation"}, 0, prCardStateAwaitingValidation},
		// Failing CI still wins even when the QA labels are present.
		{"failing CI beats needs-validation", "APPROVED", "failure", []string{"needs-validation"}, 0, prCardStateChecksFailed},
	}
	for _, tc := range cases {
		got, ok := prCardStateFromSnapshot(tc.review, tc.checks, tc.labels, tc.unresolved)
		if !ok || got != tc.want {
			t.Fatalf("%s: prCardStateFromSnapshot(%q,%q,%v,%d) = %q,%v; want %q", tc.name, tc.review, tc.checks, tc.labels, tc.unresolved, got, ok, tc.want)
		}
	}
}

func TestPRCardStateStyleTitles(t *testing.T) {
	cases := []struct {
		state    prCardState
		template string
		contains string
	}{
		{prCardStateChecksFailed, "red", "CI 失败"},
		{prCardStateChangesRequested, "orange", "待修改"},
		{prCardStateChecksRunning, "blue", "CI 检查中"},
		{prCardStateAwaitingValidation, "orange", "待 QA 验收"},
		{prCardStateValidated, "turquoise", "验收通过"},
		{prCardStateApproved, "turquoise", "待合并"},
		{prCardStateReviewPending, "blue", "待 review"},
	}
	for _, tc := range cases {
		gotT, gotL := prCardStateStyle(tc.state, "8")
		if gotT != tc.template || !strings.Contains(gotL, tc.contains) || !strings.Contains(gotL, "PR #8") {
			t.Fatalf("prCardStateStyle(%q) = (%q,%q); want template %q label~%q with PR #8", tc.state, gotT, gotL, tc.template, tc.contains)
		}
	}
}

func TestPRNumberFromTargetOrURL(t *testing.T) {
	cases := []struct {
		target string
		prURL  string
		want   int64
	}{
		{"pr:owner/repo:8", "", 8},
		{"issue:owner/repo:3", "https://github.com/owner/repo/pull/12", 12},
		{"", "https://github.com/owner/repo/pull/45", 45},
		{"project:abc", "", 0},
	}
	for _, tc := range cases {
		if got := prNumberFromTargetOrURL(tc.target, tc.prURL); got != tc.want {
			t.Fatalf("prNumberFromTargetOrURL(%q,%q) = %d, want %d", tc.target, tc.prURL, got, tc.want)
		}
	}
}

func TestLoopIssueNumberReadsBothMetadataShapes(t *testing.T) {
	cases := []struct {
		name string
		meta string
		want int64
	}{
		{"planner top-level", `{"loopType":"planner","issueNumber":42}`, 42},
		{"worker nested", `{"worker":{"issueNumber":7,"issueUrl":"https://x/issues/7"}}`, 7},
		{"numeric string", `{"issueNumber":"13"}`, 13},
		{"none", `{"repo":"owner/repo"}`, 0},
		{"empty", ``, 0},
	}
	for _, tc := range cases {
		meta := tc.meta
		if got := loopIssueNumber(&meta); got != tc.want {
			t.Fatalf("%s: loopIssueNumber = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestLoopTaskKeyFromRecordCollapsesSiblingLoops(t *testing.T) {
	repo := "owner/repo"
	plannerMeta := `{"loopType":"planner","issueNumber":9}`
	workerMeta := `{"worker":{"issueNumber":9}}`
	planner := &storage.LoopRecord{Repo: &repo, MetadataJSON: &plannerMeta}
	worker := &storage.LoopRecord{Repo: &repo, MetadataJSON: &workerMeta}
	// Planner and worker for the same issue must derive the SAME task key → one card.
	if pk, wk := loopTaskKeyFromRecord(planner), loopTaskKeyFromRecord(worker); pk != "issue:owner/repo:9" || pk != wk {
		t.Fatalf("task keys planner=%q worker=%q; want both issue:owner/repo:9", pk, wk)
	}
	// No issue number → no task key (falls back to per-loop keying).
	noIssueMeta := `{"repo":"owner/repo"}`
	if k := loopTaskKeyFromRecord(&storage.LoopRecord{Repo: &repo, MetadataJSON: &noIssueMeta}); k != "" {
		t.Fatalf("task key for issue-less loop = %q, want empty", k)
	}
	// No repo → no task key.
	onlyIssueMeta := `{"issueNumber":5}`
	if k := loopTaskKeyFromRecord(&storage.LoopRecord{MetadataJSON: &onlyIssueMeta}); k != "" {
		t.Fatalf("task key without repo = %q, want empty", k)
	}
}

func TestFeishuThreadHeaderCardLinksCheckpointIssueURL(t *testing.T) {
	ctx := context.Background()
	coordinator := openNotifyCoordinator(t, t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	now := "2026-07-16T02:00:00.000Z"
	projectID := "project_checkpoint_link"
	loopID := "loop_checkpoint_link"
	target := "issue:nexu-io/open-design:582"
	repo := "nexu-io/open-design"
	metadata := `{"issueNumber":582,"title":"High-fidelity export"}`
	checkpoint := `{"issue":{"url":"https://plane.example/open-design/browse/OPEND-582"}}`
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Project", RepoPath: t.TempDir(), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &target, Repo: &repo, Status: "running", MetadataJSON: &metadata, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run_checkpoint_link", LoopID: loopID, Status: "running", CheckpointJSON: &checkpoint, StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	gateway := NewGateway(Options{Repositories: repos})
	card, ok := gateway.feishuThreadHeaderCard(ctx, loopID)
	if !ok {
		t.Fatal("feishuThreadHeaderCard() did not render")
	}
	want := `[Issue #582](https://plane.example/open-design/browse/OPEND-582)`
	if !strings.Contains(card, want) {
		t.Fatalf("checkpoint issue reference is not clickable; want %q in %s", want, card)
	}
}

func TestGatewayStateSurvivesConfigSpecificGatewaySnapshots(t *testing.T) {
	t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
	t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

	state := NewGatewayState()
	var firstCalls, secondCalls []capturedFeishuCall
	first := newFeishuAppGateway(t, appModeConfig(), &firstCalls, state)
	second := newFeishuAppGateway(t, appModeConfig(), &secondCalls, state)
	if err := first.SendHITLAsk(context.Background(), HITLAskCard{
		LoopID: "loop_shared", LoopSeq: 42, Question: "Redis or Postgres?", Options: []string{"redis", "postgres"}, SourceURL: "https://plane.example/issues/42",
	}); err != nil {
		t.Fatalf("first snapshot SendHITLAsk() error = %v", err)
	}

	second.MarkAskAnswered(context.Background(), "loop_shared", "postgres")
	if len(secondCalls) != 1 || secondCalls[0].method != "PATCH" || !strings.Contains(secondCalls[0].url, "/messages/om_msg") || !strings.Contains(string(secondCalls[0].body), "postgres") {
		t.Fatalf("second snapshot calls = %#v, want one answer-card PATCH", secondCalls)
	}
	state.liveMu.Lock()
	_, retained := state.askCards["loop_shared"]
	state.liveMu.Unlock()
	if retained {
		t.Fatal("answered ask card remained in shared state")
	}
}

func TestGatewayStateBoundsLoopTransportEntries(t *testing.T) {
	state := NewGatewayState()
	state.liveMu.Lock()
	state.liveTails = make(map[string]liveTailEntry)
	for index := 0; index <= gatewayStateLoopLimit; index++ {
		loopID := fmt.Sprintf("loop_%05d", index)
		state.touchLoopLocked(loopID)
		state.liveTails[loopID] = liveTailEntry{lines: []string{"active"}}
	}
	tracked := len(state.loopLastUsed)
	tails := len(state.liveTails)
	_, oldestRetained := state.liveTails["loop_00000"]
	_, newestRetained := state.liveTails[fmt.Sprintf("loop_%05d", gatewayStateLoopLimit)]
	state.liveMu.Unlock()

	if tracked != gatewayStateLoopLimit || tails != gatewayStateLoopLimit || oldestRetained || !newestRetained {
		t.Fatalf("bounded state = tracked %d tails %d oldest %v newest %v", tracked, tails, oldestRetained, newestRetained)
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

func TestPRMergeStateFromSnapshot(t *testing.T) {
	merged := `{"detail":{"State":"MERGED"},"diff":"..."}`
	if s := prMergeStateFromSnapshot(&merged); s != "MERGED" {
		t.Fatalf("merged = %q, want MERGED", s)
	}
	mergedAt := `{"detail":{"State":"OPEN","MergedAt":"2026-07-07T00:00:00Z"}}`
	if s := prMergeStateFromSnapshot(&mergedAt); s != "MERGED" {
		t.Fatalf("mergedAt = %q, want MERGED", s)
	}
	closed := `{"detail":{"State":"CLOSED"}}`
	if s := prMergeStateFromSnapshot(&closed); s != "CLOSED" {
		t.Fatalf("closed = %q, want CLOSED", s)
	}
	open := `{"detail":{"State":"OPEN"}}`
	if s := prMergeStateFromSnapshot(&open); s != "OPEN" {
		t.Fatalf("open = %q, want OPEN", s)
	}
	if s := prMergeStateFromSnapshot(nil); s != "" {
		t.Fatalf("nil = %q, want empty", s)
	}
}

// prLabelsFromSnapshot reads the PR labels the QA gate keys on out of a snapshot's
// captured detail payload — the same {"detail":{"Labels":[...]}} shape both
// github.CapturePullRequestSnapshot and github.SnapshotFromDetail marshal (the
// PullRequestDetail struct has no json tags, so the field stays capitalized).
func TestPRLabelsFromSnapshot(t *testing.T) {
	withLabels := `{"detail":{"State":"OPEN","ReviewDecision":"APPROVED","Labels":["needs-validation","enhancement"]},"diff":"..."}`
	got := prLabelsFromSnapshot(&withLabels)
	if len(got) != 2 || got[0] != "needs-validation" || got[1] != "enhancement" {
		t.Fatalf("prLabelsFromSnapshot = %v, want [needs-validation enhancement]", got)
	}
	// End-to-end through the state mapper: APPROVED + needs-validation → 🧪 待 QA 验收.
	if state, ok := prCardStateFromSnapshot("APPROVED", "success", prLabelsFromSnapshot(&withLabels), 0); !ok || state != prCardStateAwaitingValidation {
		t.Fatalf("APPROVED + needs-validation snapshot → %q,%v; want awaiting_validation", state, ok)
	}
	noLabels := `{"detail":{"State":"OPEN"}}`
	if got := prLabelsFromSnapshot(&noLabels); got != nil {
		t.Fatalf("no Labels field → %v, want nil", got)
	}
	if got := prLabelsFromSnapshot(nil); got != nil {
		t.Fatalf("nil payload → %v, want nil", got)
	}
	garbage := `not json`
	if got := prLabelsFromSnapshot(&garbage); got != nil {
		t.Fatalf("garbage payload → %v, want nil", got)
	}
}

// A worker loop shepherding its impl PR animates the card through the PR-driving
// lane; the "ready" state reads 待合并 (a human merges — the bot never does), and
// 🎉已合并 comes only from the terminal "merged" outcome, never from shepherding.
func TestFeishuLoopFlowchartStyleShepherding(t *testing.T) {
	cases := []struct {
		phase    string
		template string
		contains string
	}{
		{"reviewing", "blue", "评审中"},
		{"fixing", "blue", "修复中"},
		{"awaiting_merge", "turquoise", "待合并"},
		{"", "blue", "评审中"},
	}
	for _, c := range cases {
		gotT, gotL := feishuLoopFlowchartStyle("worker", "shepherding", true, false, "", c.phase, 0)
		if gotT != c.template || !strings.Contains(gotL, c.contains) {
			t.Fatalf("shepherd phase %q = (%q,%q); want template %q label~%q", c.phase, gotT, gotL, c.template, c.contains)
		}
		if strings.Contains(gotL, "已合并") {
			t.Fatalf("shepherding must never show 已合并 (only terminal merged does): phase %q → %q", c.phase, gotL)
		}
	}
	// merged terminal wins regardless of shepherd phase
	if _, label := feishuLoopFlowchartStyle("worker", "merged", true, false, "", "fixing", 0); !strings.Contains(label, "已合并") {
		t.Fatalf("merged terminal must show 已合并, got %q", label)
	}
	// During a shepherd FIX pass the loop status is "running", not "shepherding" — the
	// card must still show the shepherd phase, not drop back to the generic 实现中.
	if _, label := feishuLoopFlowchartStyle("worker", "running", true, false, "", "fixing", 0); !strings.Contains(label, "修复中") {
		t.Fatalf("running shepherd fix pass must show 修复中, got %q", label)
	}
	if _, label := feishuLoopFlowchartStyle("worker", "running", true, false, "", "awaiting_validation", 0); !strings.Contains(label, "待验收") {
		t.Fatalf("running shepherd (awaiting_validation) must show 待验收, got %q", label)
	}
	// A plain worker run with no shepherd marker still reads 实现中.
	if _, label := feishuLoopFlowchartStyle("worker", "running", false, false, "", "", 0); !strings.Contains(label, "实现中") {
		t.Fatalf("plain worker run must show 实现中, got %q", label)
	}
	if !feishuLoopAwaitingMerge("shepherding") {
		t.Fatal("feishuLoopAwaitingMerge(shepherding) = false, want true (card should mirror PR state)")
	}
}
