package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFileMetadataReportsCanonicalFieldSources(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	raw := `{
  "agent": {
    "model": "file-model",
    "env": {"TOKEN": "must-not-appear-in-metadata"},
    "timeouts": {"plannerSeconds": 12}
  },
  "defaults": {"allowAutoPush": false},
  "notifications": {"webhook": {"mentionOpenIds": []}}
}`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: configPath,
		Args:       []string{"--planner-agent-timeout-seconds=99"},
		LookupEnv: mapEnvLookup(map[string]string{
			"LOOPER_ALLOW_AUTO_PUSH":                "true",
			"LOOPER_AGENT_TIMEOUTS_PLANNER_SECONDS": "88",
		}),
		LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	want := map[string]ValueSource{
		"agent.model":                             ValueSourceConfigFile,
		"agent.env":                               ValueSourceConfigFile,
		"agent.env.TOKEN":                         ValueSourceConfigFile,
		"agent.timeouts.plannerSeconds":           ValueSourceCLI,
		"agent.timeouts.plannerMaxRuntimeSeconds": ValueSourceCLI,
		"defaults.allowAutoPush":                  ValueSourceEnv,
		"notifications.webhook.mentionOpenIds":    ValueSourceConfigFile,
		"server.port":                             ValueSourceDefault,
		"server.localToken":                       ValueSourceDefault,
		"agent.vendor":                            ValueSourceDefault,
		"hitl.github.awaitingLabel":               ValueSourceDefault,
		"roles.reviewer.discovery.autoDiscovery":  ValueSourceDefault,
	}
	for path, source := range want {
		if got := loaded.Metadata.FieldSources[path]; got != source {
			t.Errorf("FieldSources[%q] = %q, want %q", path, got, source)
		}
	}
	for path := range loaded.Metadata.FieldSources {
		if path == "must-not-appear-in-metadata" {
			t.Fatalf("FieldSources exposed a configured value: %#v", loaded.Metadata.FieldSources)
		}
	}
}

func TestLoadFileMetadataCanonicalizesLegacyOverridePaths(t *testing.T) {
	t.Parallel()

	loaded, err := LoadFile(LoadFileOptions{
		CWD:        t.TempDir(),
		ConfigPath: filepath.Join(t.TempDir(), "missing.json"),
		LookupEnv: mapEnvLookup(map[string]string{
			"LOOPER_ROLES_REVIEWER_AUTO_DISCOVERY": "false",
			"LOOPER_REVIEWER_LOOP_ENABLED":         "false",
		}),
		LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	if got := loaded.Metadata.FieldSources["roles.reviewer.discovery.autoDiscovery"]; got != ValueSourceEnv {
		t.Fatalf("canonical reviewer discovery source = %q, want env", got)
	}
	if got := loaded.Metadata.FieldSources["roles.reviewer.behavior.loop.enabledByDefault"]; got != ValueSourceEnv {
		t.Fatalf("canonical reviewer loop source = %q, want env", got)
	}
	if _, exists := loaded.Metadata.FieldSources["roles.reviewer.autoDiscovery"]; exists {
		t.Fatalf("FieldSources contains legacy reviewer path: %#v", loaded.Metadata.FieldSources)
	}
	if _, exists := loaded.Metadata.FieldSources["reviewer.loop.enabledByDefault"]; exists {
		t.Fatalf("FieldSources contains legacy top-level reviewer path: %#v", loaded.Metadata.FieldSources)
	}
}

func TestLoadFileConfigPathOverrideWinsSelectionButKeepsOverrides(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	forcedPath := filepath.Join(cwd, "forced.json")
	cliPath := filepath.Join(cwd, "cli.json")
	envPath := filepath.Join(cwd, "env.json")
	optionPath := filepath.Join(cwd, "option.json")
	files := map[string]string{
		forcedPath: `{"server":{"host":"forced"}}`,
		cliPath:    `{"server":{"host":"cli"}}`,
		envPath:    `{"server":{"host":"env"}}`,
		optionPath: `{"server":{"host":"option"}}`,
	}
	for path, raw := range files {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatalf("os.WriteFile(%q) error = %v", path, err)
		}
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD:                cwd,
		ConfigPath:         optionPath,
		ConfigPathOverride: forcedPath,
		Args:               []string{"--config", cliPath, "--allow-auto-push=false"},
		LookupEnv: mapEnvLookup(map[string]string{
			"LOOPER_CONFIG":          envPath,
			"LOOPER_PORT":            "19000",
			"LOOPER_ALLOW_AUTO_PUSH": "true",
		}),
		LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if loaded.Metadata.ConfigPath != forcedPath || loaded.Config.Server.Host != "forced" {
		t.Fatalf("LoadFile() selected (%q, %q), want forced path", loaded.Metadata.ConfigPath, loaded.Config.Server.Host)
	}
	if loaded.Config.Server.Port != 19000 || loaded.Config.Defaults.AllowAutoPush {
		t.Fatalf("LoadFile() overrides = port %d, allow push %v; want env port and CLI bool", loaded.Config.Server.Port, loaded.Config.Defaults.AllowAutoPush)
	}
	if loaded.Metadata.FieldSources["server.host"] != ValueSourceConfigFile || loaded.Metadata.FieldSources["server.port"] != ValueSourceEnv || loaded.Metadata.FieldSources["defaults.allowAutoPush"] != ValueSourceCLI {
		t.Fatalf("LoadFile() FieldSources = %#v, want file/env/cli precedence", loaded.Metadata.FieldSources)
	}
}
