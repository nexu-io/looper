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
	t.Setenv("LOOPER_TEST_FEISHU_APP_ID", "cli_app_id")
	t.Setenv("LOOPER_TEST_FEISHU_APP_SECRET", "app_secret_value")

	root := t.TempDir()
	daemonConfig, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	vendor := config.AgentVendorCodex
	daemonConfig.Agent.Vendor = &vendor
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
	requester := func(calls *[]string) notify.FeishuAppHTTPFunc {
		return func(_ context.Context, method, url string, _ map[string]string, _ []byte) (int, []byte, error) {
			*calls = append(*calls, method+" "+url)
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
	if err := firstInput.OnHITLAsk(context.Background(), worker.HITLAskNotification{
		LoopID: "loop_shared", LoopSeq: 42, Question: "Redis or Postgres?", Options: []string{"redis", "postgres"},
	}); err != nil {
		t.Fatalf("first snapshot worker HITL notification error = %v", err)
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
