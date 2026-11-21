package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/projects"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

func TestCatalogSchedulerPreservesNotificationTransportAcrossConfigSnapshots(t *testing.T) {
	for _, transport := range []string{"github", "feishu"} {
		t.Run(transport, func(t *testing.T) {
			testCatalogSchedulerPreservesNotificationTransport(t, transport)
		})
	}
}

func testCatalogSchedulerPreservesNotificationTransport(t *testing.T, transport string) {
	t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
	t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

	root := t.TempDir()
	daemonConfig, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	vendor := config.AgentVendorCodex
	daemonConfig.Agent.Vendor = &vendor
	daemonConfig.HITL.AnswerTransport = transport
	if transport == "feishu" {
		daemonConfig.HITL.Feishu = &config.HITLFeishuConfig{
			Inbound: "cf-inbox", EventInboxURLEnv: "LEGACY_INBOX_URL", EventInboxTokenEnv: "LEGACY_INBOX_TOKEN",
		}
		if err := config.Validate(daemonConfig); err != nil {
			t.Fatal(err)
		}
	}
	daemonConfig.Notifications.Webhook.Enabled = true
	daemonConfig.Notifications.Webhook.Format = "feishu"
	daemonConfig.Notifications.Webhook.Mode = "app"
	daemonConfig.Notifications.Webhook.AppIDEnv = "LOOPER_TEST_FEISHU_APP_ID"
	daemonConfig.Notifications.Webhook.AppSecretEnv = "LOOPER_TEST_FEISHU_APP_SECRET"
	daemonConfig.Notifications.Webhook.ChatID = "oc_group_chat_123"
	catalog := projects.NewCatalog(daemonConfig)
	coordinator := openMigratedCoordinator(t, filepath.Join(root, "notification-snapshots.sqlite"), t.TempDir())
	repositories := storage.NewRepositories(coordinator.DB())
	handlers := buildCatalogSchedulerHandlers(
		catalog,
		nil,
		"",
		&capturingSchedulerLogger{},
		coordinator,
		repositories,
		nil,
		nil,
		NewActiveExecutionRegistry(),
		nil,
		nil,
		time.Now,
		nil,
		nil,
		nil,
		nil,
	)
	if handlers.webhook != nil {
		t.Cleanup(handlers.webhook.Close)
	}
	if handlers.snapshot == nil {
		t.Fatal("catalog scheduler did not retain its snapshot builder")
	}
	if handlers.notificationGateways == nil {
		t.Fatal("catalog scheduler did not retain its notification gateway factory")
	}
	var cards []string
	requester := func(calls *[]string) notify.FeishuAppHTTPFunc {
		return func(_ context.Context, method, url string, _ map[string]string, body []byte) (int, []byte, error) {
			*calls = append(*calls, method+" "+url)
			cards = append(cards, string(body))
			if strings.Contains(url, "/auth/v3/tenant_access_token/internal") {
				return 200, []byte(`{"code":0,"msg":"ok","tenant_access_token":"t-abc123","expire":7200}`), nil
			}
			return 200, []byte(`{"code":0,"msg":"success","data":{"message_id":"om_ask"}}`), nil
		}
	}

	var firstCalls, secondCalls []string
	handlers.notificationGateways.feishuAppHTTP = requester(&firstCalls)
	firstSnapshot := handlers.snapshot()
	if firstSnapshot.notificationGateways != handlers.notificationGateways || firstSnapshot.input == nil {
		t.Fatal("first catalog snapshot did not use the catalog notification factory")
	}
	firstInput := firstSnapshot.input(Services{Repositories: repositories})
	if firstInput.OnHITLAsk == nil {
		t.Fatal("first catalog snapshot did not expose its worker HITL notification path")
	}
	sourceURL := "https://plane.example/issues/42"
	if transport == "feishu" {
		sourceURL = ""
	}
	if err := firstInput.OnHITLAsk(context.Background(), worker.HITLAskNotification{
		LoopID: "loop_shared", LoopSeq: 42, Question: "Redis or Postgres?", Options: []string{"redis", "postgres"}, SourceURL: sourceURL,
	}); err != nil {
		t.Fatalf("first snapshot worker HITL notification error = %v", err)
	}

	if transport == "feishu" {
		joined := strings.Join(cards, "\n")
		// The legacy callback still has actionable options, without requiring a
		// source-system URL that old daemon tasks never recorded.
		if !strings.Contains(joined, `\"answer\":\"redis\"`) || strings.Contains(joined, "飞书回复不会被读取") {
			t.Fatalf("legacy callback card lost its reply contract: %s", joined)
		}
	}

	next := config.CloneConfig(daemonConfig)
	model := "gpt-5.1"
	next.Agent.Model = &model
	catalog.PublishGlobals(next)
	handlers.notificationGateways.feishuAppHTTP = requester(&secondCalls)
	secondSnapshot := handlers.snapshot()
	if secondSnapshot.notificationGateways != handlers.notificationGateways || secondSnapshot.input == nil {
		t.Fatal("second catalog snapshot did not reuse the catalog notification factory")
	}
	secondInput := secondSnapshot.input(Services{Repositories: repositories})
	if secondInput.OnHITLAnswerDelivered == nil {
		t.Fatal("second catalog snapshot did not expose its HITL answer notification path")
	}
	secondInput.OnHITLAnswerDelivered(context.Background(), "loop_shared", "postgres")
	if len(secondCalls) != 1 || !strings.HasPrefix(secondCalls[0], "PATCH ") || !strings.Contains(secondCalls[0], "/messages/om_ask") {
		t.Fatalf("second snapshot calls = %#v, want one answer-card PATCH through its actual scheduler callback", secondCalls)
	}
}
