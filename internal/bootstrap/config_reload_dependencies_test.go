package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestBootstrapSuppliesExactValidatedReloadLoaders(t *testing.T) {
	root := t.TempDir()
	bootstrapCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(bootstrapCWD) })
	loaded := config.LoadedFileConfig{
		Config: config.Config{
			Storage: config.StorageConfig{DBPath: filepath.Join(root, "data", "looper.sqlite")},
			Logging: config.LoggingConfig{Level: config.LogLevelInfo, MaxSizeMB: 1, MaxFiles: 1},
			Daemon: config.DaemonConfig{
				LogDir:           filepath.Join(root, "logs"),
				WorkingDirectory: root,
			},
		},
		Metadata: config.LoadFileMetadata{ConfigPath: filepath.Join(root, "config.toml")},
	}
	wantArgs := []string{"--port", "19100"}
	args := append([]string(nil), wantArgs...)
	env := map[string]string{"LOOPER_MAX_CONCURRENT_RUNS": "7"}
	loadCalls := 0

	_, err = Bootstrap(context.Background(), Options{
		Args: args,
		Env:  env,
		LoadConfig: func(options config.LoadFileOptions) (config.LoadedFileConfig, error) {
			loadCalls++
			if !reflect.DeepEqual(options.Args, wantArgs) {
				t.Fatalf("LoadFileOptions.Args = %#v, want frozen %#v", options.Args, wantArgs)
			}
			if got, ok := options.LookupEnv("LOOPER_MAX_CONCURRENT_RUNS"); !ok || got != "7" {
				t.Fatalf("LookupEnv() = (%q, %v), want frozen (7, true)", got, ok)
			}
			if options.CWD != bootstrapCWD {
				t.Fatalf("LoadFileOptions.CWD = %q, want frozen %q", options.CWD, bootstrapCWD)
			}
			if loadCalls == 1 && options.ConfigPathOverride != "" {
				t.Fatalf("initial ConfigPathOverride = %q, want empty", options.ConfigPathOverride)
			}
			if loadCalls == 2 && options.ConfigPathOverride != loaded.Metadata.ConfigPath {
				t.Fatalf("reload ConfigPathOverride = %q, want pinned %q", options.ConfigPathOverride, loaded.Metadata.ConfigPath)
			}
			candidate := loaded
			if options.ConfigPathOverride != "" && options.ConfigPathOverride != loaded.Metadata.ConfigPath {
				candidate.Metadata.ConfigPath = options.ConfigPathOverride
				return candidate, nil
			}
			if loadCalls > 1 {
				missing := filepath.Join(root, "missing-osascript")
				candidate.Config.Tools.OsascriptPath = &missing
				candidate.Metadata.ToolDetection = map[string]config.ToolDetectionStatus{
					"osascriptPath": config.ToolDetectionStatusConfigured,
				}
			}
			return candidate, nil
		},
		CreateLogger: func(config.LoggingConfig, string, LoggerOptions) (Logger, error) {
			return &recordingLogger{}, nil
		},
		StartRuntime: func(_ context.Context, deps RuntimeDependencies) (Runtime, error) {
			if deps.ReloadConfig == nil || deps.LoadConfigAt == nil {
				t.Fatal("dynamic config loaders were not supplied")
			}
			if !reflect.DeepEqual(deps.InitialConfig, loaded) {
				t.Fatalf("InitialConfig = %#v, want %#v", deps.InitialConfig, loaded)
			}
			args[0] = "--host"
			env["LOOPER_MAX_CONCURRENT_RUNS"] = "99"
			if err := os.Chdir(t.TempDir()); err != nil {
				t.Fatalf("Chdir() error = %v", err)
			}
			_, reloadErr := deps.ReloadConfig()
			var validationErr *config.ConfigValidationError
			if !errors.As(reloadErr, &validationErr) || len(validationErr.Issues) != 1 || validationErr.Issues[0].Path != "tools.osascriptPath" {
				t.Fatalf("ReloadConfig() error = %#v, want tools.osascriptPath validation", reloadErr)
			}
			candidatePath := filepath.Join(root, "candidate.toml")
			candidate, loadErr := deps.LoadConfigAt(candidatePath)
			if loadErr != nil {
				t.Fatalf("LoadConfigAt() error = %v", loadErr)
			}
			if candidate.Metadata.ConfigPath != candidatePath {
				t.Fatalf("LoadConfigAt().ConfigPath = %q, want %q", candidate.Metadata.ConfigPath, candidatePath)
			}
			return struct{}{}, nil
		},
	})
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if loadCalls != 3 {
		t.Fatalf("LoadConfig() calls = %d, want initial, reload, and candidate", loadCalls)
	}
}

func TestValidateConfiguredToolPathsRejectsMissingHotTool(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing-tool")
	for _, testCase := range []struct {
		name      string
		field     string
		statusKey string
		configure func(*config.Config)
	}{
		{name: "looper", field: "tools.looperPath", statusKey: "looperPath", configure: func(cfg *config.Config) { cfg.Tools.LooperPath = &missing }},
		{name: "osascript", field: "tools.osascriptPath", statusKey: "osascriptPath", configure: func(cfg *config.Config) { cfg.Tools.OsascriptPath = &missing }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := config.Config{}
			testCase.configure(&cfg)
			err := validateConfiguredToolPaths(cfg, map[string]config.ToolDetectionStatus{testCase.statusKey: config.ToolDetectionStatusConfigured})
			var validationErr *config.ConfigValidationError
			if !errors.As(err, &validationErr) || len(validationErr.Issues) != 1 || validationErr.Issues[0].Path != testCase.field {
				t.Fatalf("validateConfiguredToolPaths() error = %#v, want %s validation", err, testCase.field)
			}
		})
	}
}
