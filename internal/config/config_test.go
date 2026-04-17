package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigMatchesDaemonDefaults(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() error = %v", err)
	}

	config, err := DefaultConfig("/tmp/looper-cwd")
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	if config.Server.Host != "127.0.0.1" {
		t.Fatalf("DefaultConfig().Server.Host = %q, want %q", config.Server.Host, "127.0.0.1")
	}

	if config.Server.Port != 4310 {
		t.Fatalf("DefaultConfig().Server.Port = %d, want %d", config.Server.Port, 4310)
	}

	if config.Server.AuthMode != AuthModeNone {
		t.Fatalf("DefaultConfig().Server.AuthMode = %q, want %q", config.Server.AuthMode, AuthModeNone)
	}

	if config.Storage.Mode != "sqlite" {
		t.Fatalf("DefaultConfig().Storage.Mode = %q, want %q", config.Storage.Mode, "sqlite")
	}

	if config.Storage.DBPath != filepath.Join(homeDir, ".looper", "looper.sqlite") {
		t.Fatalf("DefaultConfig().Storage.DBPath = %q, want %q", config.Storage.DBPath, filepath.Join(homeDir, ".looper", "looper.sqlite"))
	}

	if config.Storage.BackupDir == nil || *config.Storage.BackupDir != filepath.Join(homeDir, ".looper", "backups") {
		t.Fatalf("DefaultConfig().Storage.BackupDir = %v, want %q", config.Storage.BackupDir, filepath.Join(homeDir, ".looper", "backups"))
	}

	if config.Scheduler.MaxConcurrentRuns != 3 {
		t.Fatalf("DefaultConfig().Scheduler.MaxConcurrentRuns = %d, want %d", config.Scheduler.MaxConcurrentRuns, 3)
	}

	if config.Logging.Level != LogLevelInfo {
		t.Fatalf("DefaultConfig().Logging.Level = %q, want %q", config.Logging.Level, LogLevelInfo)
	}

	if !config.Notifications.InApp {
		t.Fatal("DefaultConfig().Notifications.InApp = false, want true")
	}

	if !config.Notifications.Osascript.Enabled {
		t.Fatal("DefaultConfig().Notifications.Osascript.Enabled = false, want true")
	}

	if config.Daemon.Mode != DaemonModeForeground {
		t.Fatalf("DefaultConfig().Daemon.Mode = %q, want %q", config.Daemon.Mode, DaemonModeForeground)
	}

	if config.Daemon.LogDir != filepath.Join(homeDir, ".looper", "logs") {
		t.Fatalf("DefaultConfig().Daemon.LogDir = %q, want %q", config.Daemon.LogDir, filepath.Join(homeDir, ".looper", "logs"))
	}

	if config.Daemon.WorkingDirectory != "/tmp/looper-cwd" {
		t.Fatalf("DefaultConfig().Daemon.WorkingDirectory = %q, want %q", config.Daemon.WorkingDirectory, "/tmp/looper-cwd")
	}

	if config.Defaults.OpenPRStrategy != OpenPRStrategyManual {
		t.Fatalf("DefaultConfig().Defaults.OpenPRStrategy = %q, want %q", config.Defaults.OpenPRStrategy, OpenPRStrategyManual)
	}

	if len(config.Agent.Params) != 0 || len(config.Agent.Env) != 0 {
		t.Fatalf("DefaultConfig().Agent maps = %#v / %#v, want empty maps", config.Agent.Params, config.Agent.Env)
	}

	if len(config.Projects) != 0 {
		t.Fatalf("DefaultConfig().Projects len = %d, want 0", len(config.Projects))
	}
}

func TestNormalizeAppliesOverridesWithoutDroppingDefaults(t *testing.T) {
	openPRStrategy := OpenPRStrategyAllDone
	authMode := AuthModeLocalToken
	level := LogLevelDebug
	daemonMode := DaemonModeLaunchd
	trueValue := true
	falseValue := false
	port := 7000
	pollInterval := 45
	throttleWindow := 120
	maxFiles := 9
	bunPath := "/custom/bin/bun"
	localToken := "secret"
	baseURL := "http://127.0.0.1:9999"
	vendor := AgentVendorOpenCode
	model := "gpt-5.4"
	logDir := "/var/log/looper"
	baseBranch := "develop"
	repoBaseBranch := "stable"
	worktreeRoot := "/tmp/worktrees/project-a"

	projects := []ProjectRefConfig{{
		ID:           "project-a",
		Name:         "Project A",
		RepoPath:     "/repos/project-a",
		BaseBranch:   &repoBaseBranch,
		WorktreeRoot: &worktreeRoot,
	}}

	config, err := Normalize("/tmp/original-cwd", PartialConfig{
		Server: &PartialServerConfig{
			Port:       &port,
			BaseURL:    &baseURL,
			AuthMode:   &authMode,
			LocalToken: &localToken,
		},
		Scheduler: &PartialSchedulerConfig{
			PollIntervalSeconds: &pollInterval,
		},
		Agent: &PartialAgentConfig{
			Vendor: &vendor,
			Model:  &model,
			Params: map[string]any{"reasoning": "medium"},
			Env:    map[string]string{"OPENAI_API_KEY": "replace-me"},
		},
		Logging: &PartialLoggingConfig{
			Level:    &level,
			MaxFiles: &maxFiles,
		},
		Notifications: &PartialNotificationConfig{
			InApp: &falseValue,
			Osascript: &PartialOsascriptNotificationConfig{
				Enabled:               &falseValue,
				SoundForLevels:        &[]NotificationSoundLevel{NotificationSoundLevelFailure},
				ThrottleWindowSeconds: &throttleWindow,
			},
		},
		Tools: &PartialToolPathsConfig{
			BunPath: &bunPath,
		},
		Daemon: &PartialDaemonConfig{
			Mode:             &daemonMode,
			LogDir:           &logDir,
			WorkingDirectory: stringPtr("/workspace"),
			Environment:      map[string]string{"EXAMPLE_FLAG": "1"},
		},
		Defaults: &PartialDefaultsConfig{
			BaseBranch:       &baseBranch,
			AllowAutoApprove: &trueValue,
			OpenPRStrategy:   &openPRStrategy,
		},
		Projects: &projects,
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	if config.Server.Host != "127.0.0.1" {
		t.Fatalf("Normalize().Server.Host = %q, want default %q", config.Server.Host, "127.0.0.1")
	}

	if config.Server.Port != 7000 {
		t.Fatalf("Normalize().Server.Port = %d, want %d", config.Server.Port, 7000)
	}

	if config.Server.BaseURL == nil || *config.Server.BaseURL != baseURL {
		t.Fatalf("Normalize().Server.BaseURL = %v, want %q", config.Server.BaseURL, baseURL)
	}

	if config.Server.AuthMode != AuthModeLocalToken {
		t.Fatalf("Normalize().Server.AuthMode = %q, want %q", config.Server.AuthMode, AuthModeLocalToken)
	}

	if config.Server.LocalToken == nil || *config.Server.LocalToken != localToken {
		t.Fatalf("Normalize().Server.LocalToken = %v, want %q", config.Server.LocalToken, localToken)
	}

	if config.Scheduler.PollIntervalSeconds != 45 {
		t.Fatalf("Normalize().Scheduler.PollIntervalSeconds = %d, want %d", config.Scheduler.PollIntervalSeconds, 45)
	}

	if config.Scheduler.MaxConcurrentRuns != 3 {
		t.Fatalf("Normalize().Scheduler.MaxConcurrentRuns = %d, want default %d", config.Scheduler.MaxConcurrentRuns, 3)
	}

	if config.Agent.Vendor == nil || *config.Agent.Vendor != AgentVendorOpenCode {
		t.Fatalf("Normalize().Agent.Vendor = %v, want %q", config.Agent.Vendor, AgentVendorOpenCode)
	}

	if config.Agent.Model == nil || *config.Agent.Model != model {
		t.Fatalf("Normalize().Agent.Model = %v, want %q", config.Agent.Model, model)
	}

	if got := config.Agent.Params["reasoning"]; got != "medium" {
		t.Fatalf("Normalize().Agent.Params[reasoning] = %v, want %q", got, "medium")
	}

	if got := config.Agent.Env["OPENAI_API_KEY"]; got != "replace-me" {
		t.Fatalf("Normalize().Agent.Env[OPENAI_API_KEY] = %q, want %q", got, "replace-me")
	}

	if config.Logging.Level != LogLevelDebug {
		t.Fatalf("Normalize().Logging.Level = %q, want %q", config.Logging.Level, LogLevelDebug)
	}

	if config.Logging.MaxSizeMB != 10 {
		t.Fatalf("Normalize().Logging.MaxSizeMB = %d, want default %d", config.Logging.MaxSizeMB, 10)
	}

	if config.Logging.MaxFiles != 9 {
		t.Fatalf("Normalize().Logging.MaxFiles = %d, want %d", config.Logging.MaxFiles, 9)
	}

	if config.Notifications.InApp {
		t.Fatal("Normalize().Notifications.InApp = true, want false")
	}

	if config.Notifications.Osascript.Enabled {
		t.Fatal("Normalize().Notifications.Osascript.Enabled = true, want false")
	}

	if len(config.Notifications.Osascript.SoundForLevels) != 1 || config.Notifications.Osascript.SoundForLevels[0] != NotificationSoundLevelFailure {
		t.Fatalf("Normalize().Notifications.Osascript.SoundForLevels = %#v, want [%q]", config.Notifications.Osascript.SoundForLevels, NotificationSoundLevelFailure)
	}

	if config.Notifications.Osascript.ThrottleWindowSeconds != 120 {
		t.Fatalf("Normalize().Notifications.Osascript.ThrottleWindowSeconds = %d, want %d", config.Notifications.Osascript.ThrottleWindowSeconds, 120)
	}

	if config.Tools.BunPath == nil || *config.Tools.BunPath != bunPath {
		t.Fatalf("Normalize().Tools.BunPath = %v, want %q", config.Tools.BunPath, bunPath)
	}

	if config.Tools.GitPath != nil {
		t.Fatalf("Normalize().Tools.GitPath = %v, want nil", config.Tools.GitPath)
	}

	if config.Daemon.Mode != DaemonModeLaunchd {
		t.Fatalf("Normalize().Daemon.Mode = %q, want %q", config.Daemon.Mode, DaemonModeLaunchd)
	}

	if config.Daemon.LogDir != logDir {
		t.Fatalf("Normalize().Daemon.LogDir = %q, want %q", config.Daemon.LogDir, logDir)
	}

	if config.Daemon.WorkingDirectory != "/workspace" {
		t.Fatalf("Normalize().Daemon.WorkingDirectory = %q, want %q", config.Daemon.WorkingDirectory, "/workspace")
	}

	if got := config.Daemon.Environment["EXAMPLE_FLAG"]; got != "1" {
		t.Fatalf("Normalize().Daemon.Environment[EXAMPLE_FLAG] = %q, want %q", got, "1")
	}

	if config.Defaults.BaseBranch != baseBranch {
		t.Fatalf("Normalize().Defaults.BaseBranch = %q, want %q", config.Defaults.BaseBranch, baseBranch)
	}

	if !config.Defaults.AllowAutoCommit {
		t.Fatal("Normalize().Defaults.AllowAutoCommit = false, want true")
	}

	if !config.Defaults.AllowAutoApprove {
		t.Fatal("Normalize().Defaults.AllowAutoApprove = false, want true")
	}

	if config.Defaults.OpenPRStrategy != OpenPRStrategyAllDone {
		t.Fatalf("Normalize().Defaults.OpenPRStrategy = %q, want %q", config.Defaults.OpenPRStrategy, OpenPRStrategyAllDone)
	}

	if len(config.Projects) != 1 {
		t.Fatalf("Normalize().Projects len = %d, want 1", len(config.Projects))
	}

	if config.Projects[0].BaseBranch == nil || *config.Projects[0].BaseBranch != repoBaseBranch {
		t.Fatalf("Normalize().Projects[0].BaseBranch = %v, want %q", config.Projects[0].BaseBranch, repoBaseBranch)
	}

	if config.Projects[0].WorktreeRoot == nil || *config.Projects[0].WorktreeRoot != worktreeRoot {
		t.Fatalf("Normalize().Projects[0].WorktreeRoot = %v, want %q", config.Projects[0].WorktreeRoot, worktreeRoot)
	}
}

func TestNormalizeReplacesArraysAndClonesMaps(t *testing.T) {
	soundLevels := []NotificationSoundLevel{}
	projects := []ProjectRefConfig{{ID: "project-b", Name: "Project B", RepoPath: "/repos/project-b"}}
	params := map[string]any{"reasoning": "high"}
	env := map[string]string{"FOO": "bar"}
	environment := map[string]string{"BAR": "baz"}

	config, err := Normalize("/tmp", PartialConfig{
		Agent: &PartialAgentConfig{
			Params: params,
			Env:    env,
		},
		Notifications: &PartialNotificationConfig{
			Osascript: &PartialOsascriptNotificationConfig{SoundForLevels: &soundLevels},
		},
		Daemon:   &PartialDaemonConfig{Environment: environment},
		Projects: &projects,
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	params["reasoning"] = "low"
	env["FOO"] = "changed"
	environment["BAR"] = "changed"
	projects[0].Name = "Changed"

	if got := config.Agent.Params["reasoning"]; got != "high" {
		t.Fatalf("Normalize().Agent.Params[reasoning] = %v, want %q", got, "high")
	}

	if got := config.Agent.Env["FOO"]; got != "bar" {
		t.Fatalf("Normalize().Agent.Env[FOO] = %q, want %q", got, "bar")
	}

	if got := config.Daemon.Environment["BAR"]; got != "baz" {
		t.Fatalf("Normalize().Daemon.Environment[BAR] = %q, want %q", got, "baz")
	}

	if len(config.Notifications.Osascript.SoundForLevels) != 0 {
		t.Fatalf("Normalize().Notifications.Osascript.SoundForLevels len = %d, want 0", len(config.Notifications.Osascript.SoundForLevels))
	}

	if config.Projects[0].Name != "Project B" {
		t.Fatalf("Normalize().Projects[0].Name = %q, want %q", config.Projects[0].Name, "Project B")
	}
}

func TestNormalizeAppliesLayersInOrder(t *testing.T) {
	host := "0.0.0.0"
	port := 6000
	overriddenPort := 7000
	baseParams := map[string]any{"reasoning": map[string]any{"level": "high", "mode": "careful"}}
	overrideParams := map[string]any{"reasoning": map[string]any{"mode": "fast"}, "verbosity": "low"}
	projects := []ProjectRefConfig{}

	config, err := Normalize("/tmp/cwd",
		PartialConfig{
			Server: &PartialServerConfig{Host: &host, Port: &port},
			Agent:  &PartialAgentConfig{Params: baseParams},
		},
		PartialConfig{
			Server:   &PartialServerConfig{Port: &overriddenPort},
			Agent:    &PartialAgentConfig{Params: overrideParams},
			Projects: &projects,
		},
	)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	if config.Server.Host != host {
		t.Fatalf("Normalize().Server.Host = %q, want %q", config.Server.Host, host)
	}

	if config.Server.Port != overriddenPort {
		t.Fatalf("Normalize().Server.Port = %d, want %d", config.Server.Port, overriddenPort)
	}

	reasoning, ok := config.Agent.Params["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("Normalize().Agent.Params[reasoning] type = %T, want map[string]any", config.Agent.Params["reasoning"])
	}

	if got := reasoning["level"]; got != "high" {
		t.Fatalf("Normalize().Agent.Params[reasoning][level] = %v, want %q", got, "high")
	}

	if got := reasoning["mode"]; got != "fast" {
		t.Fatalf("Normalize().Agent.Params[reasoning][mode] = %v, want %q", got, "fast")
	}

	if got := config.Agent.Params["verbosity"]; got != "low" {
		t.Fatalf("Normalize().Agent.Params[verbosity] = %v, want %q", got, "low")
	}

	if len(config.Projects) != 0 {
		t.Fatalf("Normalize().Projects len = %d, want 0", len(config.Projects))
	}
}

func TestDefaultPathHelpersMatchTSLayout(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() error = %v", err)
	}

	configPath, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath() error = %v", err)
	}

	if configPath != filepath.Join(homeDir, ".looper", "config.json") {
		t.Fatalf("DefaultConfigPath() = %q, want %q", configPath, filepath.Join(homeDir, ".looper", "config.json"))
	}

	worktreeRoot, err := DefaultProjectWorktreeRoot("example-project", "/tmp/example-repo")
	if err != nil {
		t.Fatalf("DefaultProjectWorktreeRoot() error = %v", err)
	}

	wantPrefix := filepath.Join(homeDir, ".looper", "worktrees", ToRepoWorktreeDirectoryName("/tmp/example-repo"))
	if filepath.Dir(worktreeRoot) != wantPrefix {
		t.Fatalf("filepath.Dir(DefaultProjectWorktreeRoot()) = %q, want %q", filepath.Dir(worktreeRoot), wantPrefix)
	}

	if filepath.Base(worktreeRoot) != "example-project" {
		t.Fatalf("filepath.Base(DefaultProjectWorktreeRoot()) = %q, want %q", filepath.Base(worktreeRoot), "example-project")
	}
}

func TestProjectDirectoryNamingMatchesTSRules(t *testing.T) {
	longProjectID := strings.Repeat("a", 256)
	longProjectIDHash := sha256Hex(longProjectID)

	testCases := []struct {
		name      string
		projectID string
		want      string
	}{
		{name: "canonical", projectID: "example-project", want: "example-project"},
		{name: "relative traversal", projectID: "../tmp", want: legacyProjectIDPrefix + hex.EncodeToString([]byte("../tmp"))},
		{name: "mixed case", projectID: "Foo", want: legacyProjectIDPrefix + hex.EncodeToString([]byte("Foo"))},
		{name: "windows reserved name", projectID: "con", want: legacyProjectIDPrefix + hex.EncodeToString([]byte("con"))},
		{name: "empty", projectID: "", want: legacyProjectIDPrefix + "empty"},
		{name: "hashed fallback", projectID: longProjectID, want: legacyProjectIDPrefix + longProjectIDHash},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ToProjectWorktreeDirectoryName(testCase.projectID); got != testCase.want {
				t.Fatalf("ToProjectWorktreeDirectoryName(%q) = %q, want %q", testCase.projectID, got, testCase.want)
			}
		})
	}
}

func TestRepoWorktreeDirectoryNameCanonicalizesSymlinks(t *testing.T) {
	repoRoot := t.TempDir()
	symlinkPath := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(repoRoot, symlinkPath); err != nil {
		t.Skipf("os.Symlink() unavailable: %v", err)
	}

	canonicalName := ToRepoWorktreeDirectoryName(repoRoot)
	symlinkName := ToRepoWorktreeDirectoryName(symlinkPath)
	if canonicalName != symlinkName {
		t.Fatalf("ToRepoWorktreeDirectoryName(%q) = %q, want %q", symlinkPath, symlinkName, canonicalName)
	}
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}
