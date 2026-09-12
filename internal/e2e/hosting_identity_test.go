package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/e2e/harness"
)

func TestSmokeLooperdBootsWithUnavailableBotAndRunsLegacyWorker(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	legacyRepo := harness.CreateSeededRepo(t, "git")
	botRepo := harness.CreateSeededRepo(t, "git")
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
	fakeAgent := harness.NewFakeAgent(t, bins)
	cfg := configWithFakeTools(t, bins, home, legacyRepo, fakeGH, fakeAgent, harness.MustFreePort(t))
	cfg.Projects[0].Repo = "acme/legacy"
	cfg.Projects = append(cfg.Projects, config.ProjectRefConfig{ID: "bot_project", Name: "Unavailable bot", RepoPath: botRepo.Path, Repo: "acme/bot", Identity: "missing-app", BaseBranch: stringPtr("main")})
	cfg.Identities = map[string]config.HostingIdentityConfig{"missing-app": {Kind: config.HostingIdentityGitHubApp, AppID: 1, InstallationID: 2, PrivateKeyFile: filepath.Join(home.Root, "missing-app-key.pem")}}
	cfg.Roles.Planner.AutoDiscovery = false
	cfg.Roles.Worker.AutoDiscovery = false
	cfg.Roles.Reviewer.Discovery.AutoDiscovery = false
	cfg.Roles.Fixer.AutoDiscovery = false
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("unavailable external bot blocked daemon readiness: %v", err)
	}
	waitForCondition(t, 5*time.Second, func() (bool, string) {
		data, _ := os.ReadFile(loopLogsPath(home))
		return strings.Contains(string(data), "hosting identity authentication failed") && strings.Contains(string(data), "missing-app"), "missing per-identity startup authentication diagnostic"
	})
	client := newAPIClient(proc.BaseURL())
	createWorker := func(project, repo string) string {
		t.Helper()
		var created struct {
			ID string `json:"id"`
		}
		client.post(t, "/api/v1/workers", map[string]any{"projectId": project, "repo": repo, "prompt": "write a local file", "baseBranch": "main"}, &created)
		return created.ID
	}
	botLoop := createWorker("bot_project", "acme/bot")
	legacyLoop := createWorker("project_1", "acme/legacy")
	botRun := waitForRunTerminal(t, client, botLoop, 30*time.Second)
	if botRun.Status != "failed" || !strings.Contains(stringValue(botRun.ErrorMessage), "missing-app") {
		t.Fatalf("bot run = %s, error=%s; want explicit selected-identity failure", botRun.Status, stringValue(botRun.ErrorMessage))
	}
	legacyRun := waitForRunTerminal(t, client, legacyLoop, 30*time.Second)
	if legacyRun.Status != "success" {
		t.Fatalf("unavailable bot blocked legacy worker: status=%s error=%s", legacyRun.Status, stringValue(legacyRun.ErrorMessage))
	}
	var status map[string]any
	client.get(t, "/api/v1/status", &status)
	service, _ := status["service"].(map[string]any)
	if service["admissionState"] != "ready" {
		t.Fatalf("external authentication failure changed daemon admission: %#v", service)
	}
	proc.Stop(context.Background())
}
