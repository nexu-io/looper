package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/e2e/harness"
)

func TestFeishuIsNotificationOnly(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
	cfg := configWithFakeTools(t, bins, home, repo, fakeGH, fakeAgent, port)
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)

	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	defer proc.Stop(context.Background())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proc.BaseURL()+"/api/v1/hitl/feishu", strings.NewReader(`{"type":"url_verification","challenge":"must-not-echo"}`))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST removed Feishu inbound route: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; Feishu must not accept callbacks", response.StatusCode)
	}
}
