package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/powerformer/looper/internal/config"
)

func TestBootstrapLoadsConfigEnsuresPathsCreatesLoggerAndStartsRuntime(t *testing.T) {
	workingDir := t.TempDir()
	rootDir := t.TempDir()
	logDir := filepath.Join(rootDir, "runtime", "logs")
	dbPath := filepath.Join(rootDir, "data", "looper.sqlite")

	loadedConfig := config.LoadedFileConfig{
		Config: config.Config{
			Storage: config.StorageConfig{DBPath: dbPath},
			Logging: config.LoggingConfig{Level: config.LogLevelInfo, MaxSizeMB: 10, MaxFiles: 5},
			Daemon:  config.DaemonConfig{LogDir: logDir, WorkingDirectory: workingDir},
		},
		Metadata: config.LoadFileMetadata{
			ConfigPath:        "/tmp/looper.json",
			ConfigFilePresent: true,
			ToolDetection: map[string]config.ToolDetectionStatus{
				"git": config.ToolDetectionStatusDetected,
			},
		},
	}

	logger := &recordingLogger{}
	runtimeValue := struct{ Name string }{Name: "runtime"}
	startCalled := false

	result, err := Bootstrap(context.Background(), Options{
		Args: []string{"--port", "9999"},
		Env:  map[string]string{"LOOPER_CONFIG": "/tmp/override.json"},
		LoadConfig: func(options config.LoadFileOptions) (config.LoadedFileConfig, error) {
			if options.CWD != "" {
				t.Fatalf("LoadConfigOptions.CWD = %q, want empty string", options.CWD)
			}
			if got, ok := options.LookupEnv("LOOPER_CONFIG"); !ok || got != "/tmp/override.json" {
				t.Fatalf("LookupEnv(LOOPER_CONFIG) = (%q, %t), want (/tmp/override.json, true)", got, ok)
			}
			return loadedConfig, nil
		},
		CreateLogger: func(cfg config.LoggingConfig, gotLogDir string, _ LoggerOptions) (Logger, error) {
			if gotLogDir != logDir {
				t.Fatalf("CreateLogger() logDir = %q, want %q", gotLogDir, logDir)
			}
			if cfg != loadedConfig.Config.Logging {
				t.Fatalf("CreateLogger() cfg = %#v, want %#v", cfg, loadedConfig.Config.Logging)
			}
			return logger, nil
		},
		StartRuntime: func(_ context.Context, deps RuntimeDependencies) (Runtime, error) {
			startCalled = true
			if !reflect.DeepEqual(deps.Config, loadedConfig.Config) {
				t.Fatalf("StartRuntime() config = %#v, want %#v", deps.Config, loadedConfig.Config)
			}
			if !reflect.DeepEqual(deps.Metadata, loadedConfig.Metadata) {
				t.Fatalf("StartRuntime() metadata = %#v, want %#v", deps.Metadata, loadedConfig.Metadata)
			}
			if deps.Logger != logger {
				t.Fatalf("StartRuntime() logger = %#v, want %#v", deps.Logger, logger)
			}
			assertDirectoryExists(t, logDir)
			assertDirectoryExists(t, filepath.Dir(dbPath))
			return runtimeValue, nil
		},
	})
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}

	if !startCalled {
		t.Fatalf("StartRuntime() was not called")
	}
	if !reflect.DeepEqual(result.Config, loadedConfig.Config) {
		t.Fatalf("result.Config = %#v, want %#v", result.Config, loadedConfig.Config)
	}
	if !reflect.DeepEqual(result.Metadata, loadedConfig.Metadata) {
		t.Fatalf("result.Metadata = %#v, want %#v", result.Metadata, loadedConfig.Metadata)
	}
	if result.Logger != logger {
		t.Fatalf("result.Logger = %#v, want %#v", result.Logger, logger)
	}
	if result.Runtime != runtimeValue {
		t.Fatalf("result.Runtime = %#v, want %#v", result.Runtime, runtimeValue)
	}

	if len(logger.infoEntries) != 1 {
		t.Fatalf("len(logger.infoEntries) = %d, want 1", len(logger.infoEntries))
	}
	entry := logger.infoEntries[0]
	if entry.message != "looperd bootstrap initialized" {
		t.Fatalf("logger.Info() message = %q, want %q", entry.message, "looperd bootstrap initialized")
	}
	if got := entry.context["configPath"]; got != loadedConfig.Metadata.ConfigPath {
		t.Fatalf("logger.Info() context[configPath] = %#v, want %#v", got, loadedConfig.Metadata.ConfigPath)
	}
	if got := entry.context["configFilePresent"]; got != loadedConfig.Metadata.ConfigFilePresent {
		t.Fatalf("logger.Info() context[configFilePresent] = %#v, want %#v", got, loadedConfig.Metadata.ConfigFilePresent)
	}
	if got := entry.context["toolDetection"]; got == nil {
		t.Fatalf("logger.Info() context[toolDetection] = nil, want map")
	}
}

func TestBootstrapRequiresExistingWritableWorkingDirectory(t *testing.T) {
	missingWorkingDir := filepath.Join(t.TempDir(), "missing-working-directory")
	logDir := filepath.Join(t.TempDir(), "logs")
	dbPath := filepath.Join(t.TempDir(), "data", "looper.sqlite")

	_, err := Bootstrap(context.Background(), Options{
		LoadConfig: func(config.LoadFileOptions) (config.LoadedFileConfig, error) {
			return config.LoadedFileConfig{
				Config: config.Config{
					Storage: config.StorageConfig{DBPath: dbPath},
					Logging: config.LoggingConfig{Level: config.LogLevelInfo, MaxSizeMB: 10, MaxFiles: 5},
					Daemon:  config.DaemonConfig{LogDir: logDir, WorkingDirectory: missingWorkingDir},
				},
			}, nil
		},
	})
	if err == nil {
		t.Fatalf("Bootstrap() error = nil, want error")
	}
	if got, want := err.Error(), "ensure daemon working directory "+missingWorkingDir+" is writable"; !contains(got, want) {
		t.Fatalf("Bootstrap() error = %q, want substring %q", got, want)
	}
	assertDirectoryExists(t, logDir)
	assertDirectoryExists(t, filepath.Dir(dbPath))
}

func TestBootstrapPropagatesLoadConfigError(t *testing.T) {
	wantErr := errors.New("boom")

	_, err := Bootstrap(context.Background(), Options{
		LoadConfig: func(config.LoadFileOptions) (config.LoadedFileConfig, error) {
			return config.LoadedFileConfig{}, wantErr
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Bootstrap() error = %v, want %v", err, wantErr)
	}
}

type recordingLogger struct {
	infoEntries []logCall
}

type logCall struct {
	message string
	context map[string]any
}

func (l *recordingLogger) Debug(string, map[string]any) {}

func (l *recordingLogger) Info(message string, context map[string]any) {
	l.infoEntries = append(l.infoEntries, logCall{message: message, context: context})
}

func (l *recordingLogger) Warn(string, map[string]any) {}

func (l *recordingLogger) Error(string, map[string]any) {}

func assertDirectoryExists(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("os.Stat(%q) error = %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", path)
	}
}

func contains(value string, wantSubstring string) bool {
	return strings.Contains(value, wantSubstring)
}
