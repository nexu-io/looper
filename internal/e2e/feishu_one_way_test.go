package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/e2e/harness"
)

func TestLegacyDaemonFeishuCallbackRemainsAvailable(t *testing.T) {
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proc.BaseURL()+"/api/v1/hitl/feishu", strings.NewReader(`{"type":"url_verification","challenge":"legacy-challenge"}`))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST legacy Feishu inbound route: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("handshake status = %d, want 200", response.StatusCode)
	}
	var body struct {
		Challenge string `json:"challenge"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || body.Challenge != "legacy-challenge" {
		t.Fatalf("handshake body = %#v, %v", body, err)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, proc.BaseURL()+"/api/v1/hitl/feishu", strings.NewReader(`{"action":{"tag":"button","value":{"loopSeq":"1","answer":"yes"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unconfigured card action status = %d, want 403", response.StatusCode)
	}
}
