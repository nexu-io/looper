package e2e

import (
	"context"
	"os"
	"os/exec"
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

func TestSmokeLooperdFailsFastWithBotIdentityWithoutHostingCLI(t *testing.T) {
	bins := harness.MustBinaries(t)
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"bot_missing_cli", "bot_configured_cli", "legacy_missing_cli"} {
		t.Run(scenario, func(t *testing.T) {
			home := harness.NewTempHome(t)
			repo := harness.CreateSeededRepo(t, "git")
			fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
			fakeAgent := harness.NewFakeAgent(t, bins)
			cfg := configWithFakeTools(t, bins, home, repo, fakeGH, fakeAgent, harness.MustFreePort(t))
			cfg.Tools.GitPath = &gitPath
			cfg.Projects[0].Repo = "acme/repo"
			if scenario != "bot_configured_cli" {
				cfg.Tools.LooperPath = nil
			}
			if scenario != "legacy_missing_cli" {
				cfg.Identities = map[string]config.HostingIdentityConfig{"bot": {Kind: config.HostingIdentityGitHubApp, AppID: 1, InstallationID: 2, PrivateKeyFile: filepath.Join(home.Root, "missing.pem")}}
				cfg.Projects[0].Identity = "bot"
			}
			cfg.Roles.Planner.AutoDiscovery = false
			cfg.Roles.Worker.AutoDiscovery = false
			cfg.Roles.Reviewer.Discovery.AutoDiscovery = false
			cfg.Roles.Fixer.AutoDiscovery = false
			harness.WriteConfig(t, home.ConfigPath, cfg, nil)
			env := fakeGH.EnvMap()
			// Match go run ./cmd/looperd with no separately installed looper on PATH.
			env["PATH"] = t.TempDir()
			proc := harness.StartLooperd(t, bins, home, home.ConfigPath, env, cfg.Server.Host, cfg.Server.Port)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := proc.WaitForReady(ctx)
			if scenario != "bot_missing_cli" {
				if err != nil {
					t.Fatalf("valid tool configuration failed startup: %v", err)
				}
				proc.Stop(context.Background())
				return
			}
			if err == nil {
				t.Fatal("bot daemon became ready without its hosting CLI")
			}
			stderr, readErr := os.ReadFile(filepath.Join(home.ArtifactsDir, "looperd.stderr.log"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, hint := range []string{"tools.looperPath", "go build -o dist/looper ./cmd/looper"} {
				if !strings.Contains(string(stderr), hint) {
					t.Fatalf("startup error lacks %q: %s", hint, stderr)
				}
			}
			if _, err := os.Stat(home.DBPath); !os.IsNotExist(err) {
				t.Fatalf("invalid tools reached database startup: %v", err)
			}
		})
	}
}
