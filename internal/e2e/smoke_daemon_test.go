package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/e2e/harness"
)

func TestSmokeLooperdBootsWithDefaultConfig(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	port := harness.MustFreePort(t)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{Port: port})
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, nil, cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, err := proc.WaitForReady(ctx)
	if err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	service, _ := status["service"].(map[string]any)
	if service == nil {
		t.Fatalf("status.service missing: %#v", status)
	}
	if strings.TrimSpace(anyString(service["version"])) == "" {
		t.Fatalf("status.service.version missing: %#v", service)
	}
	if _, ok := service["healthy"]; !ok {
		t.Fatalf("status.service.healthy missing: %#v", service)
	}
	requirePathExists(t, home.DBPath)
	requirePathExists(t, home.LogDir)
	requirePathExists(t, home.BackupDir)
	requirePathExists(t, home.WorktreeRoot)
	requirePathExists(t, loopLogsPath(home))
	proc.Stop(context.Background())
}

func TestSmokeLooperdBootsWithRolesConfig(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	port := harness.MustFreePort(t)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{Port: port})
	cfg.Roles.Worker.AutoDiscovery = false
	cfg.Roles.Fixer.AutoDiscovery = false
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, nil, cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	proc.Stop(context.Background())
}

func TestSmokeLooperdBootsWithUnknownConfigFields(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	port := harness.MustFreePort(t)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{Port: port})
	harness.WriteConfig(t, home.ConfigPath, cfg, map[string]any{
		"legacyTopLevel": map[string]any{"enabled": true},
	})
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, nil, cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	proc.Stop(context.Background())
}

func TestSmokeLooperdBootsWithExplicitToolPaths(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	port := harness.MustFreePort(t)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{}})
	fakeAgent := harness.NewFakeAgent(t, bins)
	cfg := configWithFakeTools(t, bins, home, repo, fakeGH, fakeAgent, port)
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, err := proc.WaitForReady(ctx)
	if err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	tools, _ := status["tools"].(map[string]any)
	if tools == nil || tools["gh"] != true || tools["git"] != true || tools["osascript"] != true {
		t.Fatalf("status.tools = %#v, want all explicit tools present", tools)
	}
	proc.Stop(context.Background())
}

func TestSmokeLooperdBootsWithoutOptionalConfigSections(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	port := harness.MustFreePort(t)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{Port: port})
	payload, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	for _, key := range []string{"notifications", "disclosure", "tools", "reviewer", "instructions", "projects", "roles"} {
		delete(doc, key)
	}
	overrides, ok := doc["daemon"].(map[string]any)
	if !ok {
		t.Fatalf("daemon section missing from config doc: %#v", doc)
	}
	overrides["workingDirectory"] = home.WorkingDir
	formatted, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal optional-sections config: %v", err)
	}
	if err := os.WriteFile(home.ConfigPath, formatted, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, nil, cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	proc.Stop(context.Background())
}

func TestSmokeLooperdFailsFastWithInvalidOsascriptPathWhenEnabled(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	port := harness.MustFreePort(t)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{Port: port, EnableOsascript: true, ToolPaths: harness.TestToolPaths{Osascript: filepath.Join(home.Root, "missing-osascript")}})
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, nil, cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := proc.WaitForReady(ctx)
	if err == nil {
		t.Fatal("expected startup failure for invalid osascript path")
	}
	stderr, readErr := os.ReadFile(filepath.Join(home.ArtifactsDir, "looperd.stderr.log"))
	if readErr != nil {
		t.Fatalf("read stderr: %v", readErr)
	}
	if !strings.Contains(string(stderr), "tools.osascriptPath") {
		t.Fatalf("stderr = %s, want tools.osascriptPath validation", string(stderr))
	}
}

func anyString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
