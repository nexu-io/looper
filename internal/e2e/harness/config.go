package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

type TestToolPaths struct {
	Git       string
	GH        string
	Looper    string
	Osascript string
}

type ConfigOptions struct {
	Port              int
	WorkingDir        string
	ToolPaths         TestToolPaths
	EnableOsascript   bool
	AgentVendor       *config.AgentVendor
	AgentCommand      string
	AgentEnv          map[string]string
	Projects          []config.ProjectRefConfig
	DisableDisclosure bool
}

func DefaultConfig(tb testing.TB, home TempHome, options ConfigOptions) config.Config {
	tb.Helper()
	workingDir := options.WorkingDir
	if workingDir == "" {
		workingDir = home.WorkingDir
	}
	tb.Setenv("HOME", home.HomeDir)
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		tb.Fatalf("default config: %v", err)
	}
	if options.Port > 0 {
		cfg.Server.Port = options.Port
	}
	cfg.Storage.DBPath = home.DBPath
	cfg.Storage.BackupDir = stringPtr(home.BackupDir)
	cfg.Daemon.LogDir = home.LogDir
	cfg.Daemon.WorkingDirectory = workingDir
	cfg.Notifications.Osascript.Enabled = options.EnableOsascript
	cfg.Defaults.OpenPRStrategy = config.OpenPRStrategyAllDone
	if options.DisableDisclosure {
		cfg.Disclosure.Enabled = false
	}
	if options.ToolPaths.Git != "" {
		cfg.Tools.GitPath = stringPtr(options.ToolPaths.Git)
	}
	if options.ToolPaths.GH != "" {
		cfg.Tools.GHPath = stringPtr(options.ToolPaths.GH)
	}
	if options.ToolPaths.Looper != "" {
		cfg.Tools.LooperPath = stringPtr(options.ToolPaths.Looper)
	}
	if options.ToolPaths.Osascript != "" {
		cfg.Tools.OsascriptPath = stringPtr(options.ToolPaths.Osascript)
	}
	if options.AgentVendor != nil {
		cfg.Agent.Vendor = options.AgentVendor
	}
	if options.AgentCommand != "" {
		if cfg.Agent.Params == nil {
			cfg.Agent.Params = map[string]any{}
		}
		cfg.Agent.Params["command"] = options.AgentCommand
	}
	if len(options.AgentEnv) > 0 {
		if cfg.Agent.Env == nil {
			cfg.Agent.Env = map[string]string{}
		}
		for key, value := range options.AgentEnv {
			cfg.Agent.Env[key] = value
		}
	}
	if options.Projects != nil {
		cfg.Projects = append([]config.ProjectRefConfig{}, options.Projects...)
	}
	return cfg
}

func WriteConfig(tb testing.TB, path string, cfg config.Config, rawOverrides map[string]any) {
	tb.Helper()
	payload, err := json.Marshal(cfg)
	if err != nil {
		tb.Fatalf("marshal config: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		tb.Fatalf("decode config map: %v", err)
	}
	deepMerge(doc, rawOverrides)
	formatted, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		tb.Fatalf("marshal config json: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(path, formatted, 0o644); err != nil {
		tb.Fatalf("write config: %v", err)
	}
}

func stringPtr(value string) *string { return &value }

func deepMerge(dst map[string]any, src map[string]any) {
	for key, value := range src {
		srcMap, srcOK := value.(map[string]any)
		dstMap, dstOK := dst[key].(map[string]any)
		if srcOK && dstOK {
			deepMerge(dstMap, srcMap)
			continue
		}
		dst[key] = value
	}
}
