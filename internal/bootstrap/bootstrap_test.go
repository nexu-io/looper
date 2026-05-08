package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
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

func TestBootstrapWaitForShutdownRegistersSignalsAndStopsRuntime(t *testing.T) {
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
	}

	logger := &recordingLogger{}
	runtime := &stubShutdownRuntime{shutdown: make(chan struct{})}
	notifier := &stubSignalNotifier{}

	done := make(chan Result, 1)
	errCh := make(chan error, 1)

	go func() {
		result, err := Bootstrap(context.Background(), Options{
			LoadConfig: func(config.LoadFileOptions) (config.LoadedFileConfig, error) {
				return loadedConfig, nil
			},
			CreateLogger: func(config.LoggingConfig, string, LoggerOptions) (Logger, error) {
				return logger, nil
			},
			StartRuntime: func(context.Context, RuntimeDependencies) (Runtime, error) {
				return runtime, nil
			},
			WaitForShutdown: true,
			SignalNotifier:  notifier,
		})
		if err != nil {
			errCh <- err
			return
		}
		done <- result
	}()

	registered := notifier.waitForRegistration(t)
	if !reflect.DeepEqual(registered, []os.Signal{os.Interrupt, syscall.SIGTERM}) {
		t.Fatalf("registered signals = %#v, want %#v", registered, []os.Signal{os.Interrupt, syscall.SIGTERM})
	}

	select {
	case <-done:
		t.Fatalf("Bootstrap() returned before runtime shutdown")
	case err := <-errCh:
		t.Fatalf("Bootstrap() error = %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	notifier.emit(syscall.SIGTERM)

	select {
	case err := <-errCh:
		t.Fatalf("Bootstrap() error = %v", err)
	case result := <-done:
		if result.Runtime != runtime {
			t.Fatalf("result.Runtime = %#v, want %#v", result.Runtime, runtime)
		}
	case <-time.After(time.Second):
		t.Fatal("Bootstrap() did not return after shutdown signal")
	}

	if !reflect.DeepEqual(runtime.stopReasons, []string{"SIGTERM"}) {
		t.Fatalf("runtime.Stop() reasons = %#v, want %#v", runtime.stopReasons, []string{"SIGTERM"})
	}
	if notifier.stopCalls != 1 {
		t.Fatalf("SignalNotifier.Stop() calls = %d, want 1", notifier.stopCalls)
	}

	if len(logger.infoEntries) != 2 {
		t.Fatalf("len(logger.infoEntries) = %d, want 2", len(logger.infoEntries))
	}
	if got := logger.infoEntries[1].message; got != "received shutdown signal" {
		t.Fatalf("logger.Info() message = %q, want %q", got, "received shutdown signal")
	}
	if got := logger.infoEntries[1].context["signal"]; got != "SIGTERM" {
		t.Fatalf("logger.Info() context[signal] = %#v, want %#v", got, "SIGTERM")
	}
}

func TestBootstrapWaitForShutdownRequiresShutdownRuntime(t *testing.T) {
	workingDir := t.TempDir()
	rootDir := t.TempDir()
	logDir := filepath.Join(rootDir, "runtime", "logs")
	dbPath := filepath.Join(rootDir, "data", "looper.sqlite")

	_, err := Bootstrap(context.Background(), Options{
		LoadConfig: func(config.LoadFileOptions) (config.LoadedFileConfig, error) {
			return config.LoadedFileConfig{
				Config: config.Config{
					Storage: config.StorageConfig{DBPath: dbPath},
					Logging: config.LoggingConfig{Level: config.LogLevelInfo, MaxSizeMB: 10, MaxFiles: 5},
					Daemon:  config.DaemonConfig{LogDir: logDir, WorkingDirectory: workingDir},
				},
			}, nil
		},
		CreateLogger: func(config.LoggingConfig, string, LoggerOptions) (Logger, error) {
			return &recordingLogger{}, nil
		},
		StartRuntime: func(context.Context, RuntimeDependencies) (Runtime, error) {
			return struct{}{}, nil
		},
		WaitForShutdown: true,
	})
	if err == nil {
		t.Fatal("Bootstrap() error = nil, want error")
	}
	if got, want := err.Error(), "runtime does not support shutdown coordination"; !contains(got, want) {
		t.Fatalf("Bootstrap() error = %q, want substring %q", got, want)
	}
}

type recordingLogger struct {
	infoEntries []logCall
}

type stubShutdownRuntime struct {
	stopReasons []string
	shutdown    chan struct{}
}

func (r *stubShutdownRuntime) Stop(reason string) {
	r.stopReasons = append(r.stopReasons, reason)
	select {
	case <-r.shutdown:
	default:
		close(r.shutdown)
	}
}

func (r *stubShutdownRuntime) WaitForShutdown() {
	<-r.shutdown
}

type stubSignalNotifier struct {
	ch         chan<- os.Signal
	signals    []os.Signal
	registered chan struct{}
	stopCalls  int
}

func (n *stubSignalNotifier) Notify(ch chan<- os.Signal, sig ...os.Signal) {
	n.ch = ch
	n.signals = append([]os.Signal(nil), sig...)
	if n.registered == nil {
		n.registered = make(chan struct{})
	}
	close(n.registered)
}

func (n *stubSignalNotifier) Stop(chan<- os.Signal) {
	n.stopCalls++
}

func (n *stubSignalNotifier) waitForRegistration(t *testing.T) []os.Signal {
	t.Helper()

	if n.registered == nil {
		n.registered = make(chan struct{})
	}

	select {
	case <-n.registered:
		return append([]os.Signal(nil), n.signals...)
	case <-time.After(time.Second):
		t.Fatal("SignalNotifier.Notify() was not called")
		return nil
	}
}

func (n *stubSignalNotifier) emit(sig os.Signal) {
	n.ch <- sig
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
