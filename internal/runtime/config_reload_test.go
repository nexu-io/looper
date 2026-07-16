package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	networkclient "github.com/nexu-io/looper/internal/network/client"
)

type recordingNetworkManager struct {
	configs []config.Config
}

func (m *recordingNetworkManager) Start(context.Context) error { return nil }
func (m *recordingNetworkManager) Stop()                       {}
func (m *recordingNetworkManager) Status() networkclient.Status {
	return networkclient.Status{}
}
func (m *recordingNetworkManager) UpdateConfig(cfg config.Config) {
	m.configs = append(m.configs, config.CloneConfig(cfg))
}

func TestReloadConfigPublishesHotSnapshotAndKeepsMaterializedProjects(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "import-input"}}
	oldVendor := config.AgentVendorCodex
	cfg.Agent.Vendor = &oldVendor
	loaded := config.LoadedFileConfig{
		Config: cfg,
		Metadata: config.LoadFileMetadata{
			ConfigPath:        "/tmp/config.toml",
			ConfigFilePresent: true,
		},
	}
	next := loaded
	next.Config = config.CloneConfig(cfg)
	next.Config.Scheduler.MaxConcurrentRuns++
	next.Config.Roles.Worker.AutoDiscovery = !cfg.Roles.Worker.AutoDiscovery
	newVendor := config.AgentVendorOpenCode
	next.Config.Agent.Vendor = &newVendor
	next.Config.Projects = []config.ProjectRefConfig{{ID: "import-input"}}

	rt := New(Options{
		Config:        cfg,
		InitialConfig: loaded,
		ReloadConfig: func() (config.LoadedFileConfig, error) {
			return next, nil
		},
	})
	networkManager := &recordingNetworkManager{}
	rt.networkManager = networkManager
	rt.projectCatalog.Publish([]config.ProjectRefConfig{{ID: "database-project", Name: "Database project", RepoPath: t.TempDir()}})
	operationSnapshot := rt.Config()

	if err := rt.ReloadConfig(context.Background()); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	got := rt.Config()
	if got.Scheduler.MaxConcurrentRuns != next.Config.Scheduler.MaxConcurrentRuns {
		t.Fatalf("Config().Scheduler.MaxConcurrentRuns = %d, want %d", got.Scheduler.MaxConcurrentRuns, next.Config.Scheduler.MaxConcurrentRuns)
	}
	if got.Agent.Vendor == nil || *got.Agent.Vendor != newVendor {
		t.Fatalf("Config().Agent.Vendor = %#v, want %q", got.Agent.Vendor, newVendor)
	}
	if len(got.Projects) != 1 || got.Projects[0].ID != "database-project" {
		t.Fatalf("Config().Projects = %#v, want materialized database project", got.Projects)
	}
	if len(networkManager.configs) != 1 {
		t.Fatalf("network UpdateConfig calls = %d, want 1", len(networkManager.configs))
	}
	networkSnapshot := networkManager.configs[0]
	if networkSnapshot.Roles.Worker.AutoDiscovery != next.Config.Roles.Worker.AutoDiscovery || len(networkSnapshot.Projects) != 1 || networkSnapshot.Projects[0].ID != "database-project" {
		t.Fatalf("network config snapshot = %#v, want hot globals plus materialized project", networkSnapshot)
	}
	if operationSnapshot.Scheduler.MaxConcurrentRuns != cfg.Scheduler.MaxConcurrentRuns {
		t.Fatalf("captured operation snapshot changed = %d, want %d", operationSnapshot.Scheduler.MaxConcurrentRuns, cfg.Scheduler.MaxConcurrentRuns)
	}
	if operationSnapshot.Agent.Vendor == nil || *operationSnapshot.Agent.Vendor != oldVendor {
		t.Fatalf("captured operation vendor = %#v, want retained %q", operationSnapshot.Agent.Vendor, oldVendor)
	}
}

func TestReloadConfigRejectsRestartBoundAndInvalidCandidates(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	oldVendor := config.AgentVendorCodex
	model := "gpt-5"
	cfg.Agent.Vendor = &oldVendor
	cfg.Agent.Model = &model
	cfg.Agent.Params["command"] = "/opt/codex-wrapper"
	loaded := config.LoadedFileConfig{Config: cfg, Metadata: config.LoadFileMetadata{ConfigPath: "/tmp/config.toml", ConfigFilePresent: true}}
	candidate := loaded
	candidate.Config = config.CloneConfig(cfg)
	candidate.Config.Server.Port++
	loadErr := error(nil)
	rt := New(Options{
		Config:        cfg,
		InitialConfig: loaded,
		ReloadConfig: func() (config.LoadedFileConfig, error) {
			return candidate, loadErr
		},
	})

	err = rt.ReloadConfig(context.Background())
	var reloadErr *ConfigReloadError
	if !errors.As(err, &reloadErr) || reloadErr.Kind != "restart_required" || !slices.Contains(reloadErr.Paths, "server.port") {
		t.Fatalf("ReloadConfig() error = %#v, want restart_required server.port", err)
	}
	if got := rt.Config().Server.Port; got != cfg.Server.Port {
		t.Fatalf("Config().Server.Port = %d, want last-good %d", got, cfg.Server.Port)
	}

	candidate.Config = config.CloneConfig(cfg)
	candidate.Config.Agent.Vendor = nil
	err = rt.ReloadConfig(context.Background())
	if !errors.As(err, &reloadErr) || reloadErr.Kind != "restart_required" || !slices.Equal(reloadErr.Paths, []string{"agent.model", "agent.params"}) {
		t.Fatalf("ReloadConfig() vendor-disable error = %#v, want restart_required agent.model + agent.params", err)
	}

	candidate.Config = config.CloneConfig(cfg)
	newVendor := config.AgentVendorClaudeCode
	candidate.Config.Agent.Vendor = &newVendor
	err = rt.ReloadConfig(context.Background())
	if !errors.As(err, &reloadErr) || reloadErr.Kind != "restart_required" || !slices.Equal(reloadErr.Paths, []string{"agent.model", "agent.params"}) {
		t.Fatalf("ReloadConfig() vendor-profile error = %#v, want restart_required agent.model + agent.params", err)
	}
	if got := rt.Config().Agent.Vendor; got == nil || *got != oldVendor {
		t.Fatalf("Config().Agent.Vendor = %#v, want last-good %q", got, oldVendor)
	}

	loadErr = errors.New("temporary invalid syntax")
	err = rt.ReloadConfig(context.Background())
	if !errors.As(err, &reloadErr) || reloadErr.Kind != "invalid" {
		t.Fatalf("ReloadConfig() invalid error = %#v, want invalid reload", err)
	}
	status := rt.ConfigReloadStatus()
	if status.LastError != "configuration reload rejected: config file could not be decoded or validated" {
		t.Fatalf("ConfigReloadStatus().LastError = %q", status.LastError)
	}

	loadErr = &config.ConfigValidationError{Issues: []config.ValidationIssue{
		{Path: "agent.vendor", Message: "is invalid"},
		{Path: "scheduler.maxConcurrentRuns", Message: "must be positive"},
	}}
	err = rt.ReloadConfig(context.Background())
	if !errors.As(err, &reloadErr) || !slices.Equal(reloadErr.Paths, []string{"agent.vendor", "scheduler.maxConcurrentRuns"}) {
		t.Fatalf("ReloadConfig() validation paths = %#v, want concrete sorted paths", reloadErr.Paths)
	}
	status = rt.ConfigReloadStatus()
	if !slices.Equal(status.RejectedPaths, reloadErr.Paths) || !strings.Contains(status.LastError, "agent.vendor is invalid") || !strings.Contains(status.LastError, "scheduler.maxConcurrentRuns must be positive") {
		t.Fatalf("ConfigReloadStatus() = %#v, want field-level diagnostics", status)
	}
}

func TestConfigReloadLoopRecoversFromInvalidExternalEdit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeRuntimeReloadConfig(t, path, 2)
	load := func() (config.LoadedFileConfig, error) {
		return config.LoadFile(config.LoadFileOptions{
			ConfigPathOverride: path,
			LookupEnv:          func(string) (string, bool) { return "", false },
		})
	}
	initial, err := load()
	if err != nil {
		t.Fatalf("LoadFile(initial) error = %v", err)
	}
	rt := New(Options{
		Config:               initial.Config,
		InitialConfig:        initial,
		ReloadConfig:         load,
		ConfigReloadInterval: 10 * time.Millisecond,
	})
	rt.startConfigReloadLoop()
	t.Cleanup(rt.stopConfigReloadLoop)

	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatalf("WriteFile(invalid) error = %v", err)
	}
	waitForRuntimeConfig(t, 2*time.Second, func() bool {
		return rt.ConfigReloadStatus().LastError != ""
	})
	if got := rt.Config().Scheduler.MaxConcurrentRuns; got != 2 {
		t.Fatalf("invalid edit changed MaxConcurrentRuns = %d, want 2", got)
	}

	writeRuntimeReloadConfig(t, path, 5)
	waitForRuntimeConfig(t, 2*time.Second, func() bool {
		return rt.Config().Scheduler.MaxConcurrentRuns == 5 && rt.ConfigReloadStatus().LastError == ""
	})
}

func writeRuntimeReloadConfig(t *testing.T, path string, maxConcurrent int) {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{"notifications":{"osascript":{"enabled":false}},"scheduler":{"maxConcurrentRuns":%d}}`, maxConcurrent))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
}

func waitForRuntimeConfig(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for config reload")
}
