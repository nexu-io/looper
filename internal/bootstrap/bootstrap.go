package bootstrap

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nexu-io/looper/internal/config"
)

type Runtime any

type ShutdownRuntime interface {
	Stop(reason string)
	WaitForShutdown()
}

type RuntimeDependencies struct {
	Config        config.Config
	Metadata      config.LoadFileMetadata
	InitialConfig config.LoadedFileConfig
	ReloadConfig  func() (config.LoadedFileConfig, error)
	LoadConfigAt  func(string) (config.LoadedFileConfig, error)
	Logger        Logger
}

type LoadConfigFunc func(config.LoadFileOptions) (config.LoadedFileConfig, error)

type CreateLoggerFunc func(config.LoggingConfig, string, LoggerOptions) (Logger, error)

type StartRuntimeFunc func(context.Context, RuntimeDependencies) (Runtime, error)

type SignalNotifier interface {
	Notify(chan<- os.Signal, ...os.Signal)
	Stop(chan<- os.Signal)
}

type Options struct {
	Args            []string
	Env             map[string]string
	CWD             string
	Stdout          io.Writer
	Stderr          io.Writer
	LoadConfig      LoadConfigFunc
	CreateLogger    CreateLoggerFunc
	StartRuntime    StartRuntimeFunc
	WaitForShutdown bool
	SignalNotifier  SignalNotifier
}

type Result struct {
	Config   config.Config
	Metadata config.LoadFileMetadata
	Logger   Logger
	Runtime  Runtime
}

func Bootstrap(ctx context.Context, options Options) (Result, error) {
	loadConfig := options.LoadConfig
	if loadConfig == nil {
		loadConfig = config.LoadFile
	}
	cwd := strings.TrimSpace(options.CWD)
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return Result{}, fmt.Errorf("determine bootstrap working directory: %w", err)
		}
	}
	env := cloneEnvironment(options.Env)
	if options.Env == nil {
		env = currentEnvironment()
	}
	args := append([]string(nil), options.Args...)

	loadedConfig, err := loadConfig(config.LoadFileOptions{
		CWD:       cwd,
		Args:      args,
		LookupEnv: envLookupFromMap(env),
	})
	if err != nil {
		return Result{}, err
	}
	if err := validateConfiguredToolPaths(loadedConfig.Config, loadedConfig.Metadata.ToolDetection); err != nil {
		return Result{}, err
	}

	if err := ensureRuntimePaths(loadedConfig.Config); err != nil {
		return Result{}, err
	}

	createLogger := options.CreateLogger
	if createLogger == nil {
		createLogger = CreateLogger
	}

	logger, err := createLogger(loadedConfig.Config.Logging, loadedConfig.Config.Daemon.LogDir, LoggerOptions{
		Stdout: options.Stdout,
		Stderr: options.Stderr,
	})
	if err != nil {
		return Result{}, err
	}

	logger.Info("looperd bootstrap initialized", map[string]any{
		"configPath":        loadedConfig.Metadata.ConfigPath,
		"configFilePresent": loadedConfig.Metadata.ConfigFilePresent,
		"toolDetection":     loadedConfig.Metadata.ToolDetection,
	})

	result := Result{
		Config:   loadedConfig.Config,
		Metadata: loadedConfig.Metadata,
		Logger:   logger,
	}

	if options.StartRuntime == nil {
		return result, nil
	}

	reloadOptions := config.LoadFileOptions{
		CWD:                cwd,
		Args:               append([]string(nil), args...),
		LookupEnv:          envLookupFromMap(env),
		ConfigPathOverride: loadedConfig.Metadata.ConfigPath,
	}
	loadReloadCandidate := func(loadOptions config.LoadFileOptions) (config.LoadedFileConfig, error) {
		candidate, err := loadConfig(loadOptions)
		if err != nil {
			return config.LoadedFileConfig{}, err
		}
		if err := validateConfiguredToolPaths(candidate.Config, candidate.Metadata.ToolDetection); err != nil {
			return config.LoadedFileConfig{}, err
		}
		return candidate, nil
	}
	runtime, err := options.StartRuntime(ctx, RuntimeDependencies{
		Config:        loadedConfig.Config,
		Metadata:      loadedConfig.Metadata,
		InitialConfig: loadedConfig,
		ReloadConfig: func() (config.LoadedFileConfig, error) {
			return loadReloadCandidate(reloadOptions)
		},
		LoadConfigAt: func(path string) (config.LoadedFileConfig, error) {
			options := reloadOptions
			options.ConfigPathOverride = path
			return loadReloadCandidate(options)
		},
		Logger: logger,
	})
	if err != nil {
		return Result{}, err
	}

	result.Runtime = runtime

	if options.WaitForShutdown {
		shutdownRuntime, ok := runtime.(ShutdownRuntime)
		if !ok {
			return Result{}, fmt.Errorf("runtime does not support shutdown coordination")
		}

		waitForShutdownWithSignals(shutdownRuntime, logger, signalNotifierOrDefault(options.SignalNotifier))
	}

	return result, nil
}

type osSignalNotifier struct{}

func (osSignalNotifier) Notify(ch chan<- os.Signal, sig ...os.Signal) {
	signal.Notify(ch, sig...)
}

func (osSignalNotifier) Stop(ch chan<- os.Signal) {
	signal.Stop(ch)
}

func signalNotifierOrDefault(notifier SignalNotifier) SignalNotifier {
	if notifier != nil {
		return notifier
	}

	return osSignalNotifier{}
}

func validateConfiguredToolPaths(cfg config.Config, detection map[string]config.ToolDetectionStatus) error {
	checks := []struct {
		statusKey string
		field     string
		path      *string
	}{
		{statusKey: "gitPath", field: "tools.gitPath", path: cfg.Tools.GitPath},
		{statusKey: "ghPath", field: "tools.ghPath", path: cfg.Tools.GHPath},
		{statusKey: "looperPath", field: "tools.looperPath", path: cfg.Tools.LooperPath},
		{statusKey: "osascriptPath", field: "tools.osascriptPath", path: cfg.Tools.OsascriptPath},
	}
	for _, check := range checks {
		if detection[check.statusKey] != config.ToolDetectionStatusConfigured || check.path == nil {
			continue
		}
		value := strings.TrimSpace(*check.path)
		if value == "" || !filepath.IsAbs(value) {
			continue
		}
		info, err := os.Stat(value)
		if err == nil && !info.IsDir() {
			if _, err := exec.LookPath(value); err == nil {
				continue
			}
		}
		message := "must reference an existing executable file"
		if err == nil && info.IsDir() {
			message = "must reference a file, not a directory"
		}
		return &config.ConfigValidationError{Issues: []config.ValidationIssue{{Path: check.field, Message: message}}}
	}
	return nil
}

func waitForShutdownWithSignals(runtime ShutdownRuntime, logger Logger, notifier SignalNotifier) {
	signals := make(chan os.Signal, 1)
	notifier.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer notifier.Stop(signals)

	listenerStopped := make(chan struct{})
	listenerDone := make(chan struct{})

	go func() {
		defer close(listenerDone)

		select {
		case sig := <-signals:
			if sig == nil {
				return
			}

			reason := signalReason(sig)
			logger.Info("received shutdown signal", map[string]any{"signal": reason})
			runtime.Stop(reason)
		case <-listenerStopped:
		}
	}()

	runtime.WaitForShutdown()
	close(listenerStopped)
	<-listenerDone
}

func signalReason(sig os.Signal) string {
	switch sig {
	case os.Interrupt:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	default:
		return sig.String()
	}
}

func ensureRuntimePaths(cfg config.Config) error {
	if err := ensureWritableDirectory(cfg.Daemon.LogDir, true); err != nil {
		return fmt.Errorf("ensure daemon log directory %s is writable: %w", cfg.Daemon.LogDir, err)
	}

	dbParentDir := filepath.Dir(cfg.Storage.DBPath)
	if err := ensureWritableDirectory(dbParentDir, true); err != nil {
		return fmt.Errorf("ensure storage database parent directory %s is writable: %w", dbParentDir, err)
	}

	if err := ensureWritableDirectory(cfg.Daemon.WorkingDirectory, false); err != nil {
		return fmt.Errorf("ensure daemon working directory %s is writable: %w", cfg.Daemon.WorkingDirectory, err)
	}

	return nil
}

func ensureWritableDirectory(path string, create bool) error {
	if create {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("path is not a directory")
	}

	probe, err := os.CreateTemp(path, ".looper-write-check-*")
	if err != nil {
		return err
	}

	probePath := probe.Name()
	if closeErr := probe.Close(); closeErr != nil {
		_ = os.Remove(probePath)
		return closeErr
	}

	if err := os.Remove(probePath); err != nil {
		return err
	}

	return nil
}

func envLookupFromMap(env map[string]string) config.EnvLookupFunc {
	if env == nil {
		return os.LookupEnv
	}

	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

func cloneEnvironment(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	cloned := make(map[string]string, len(env))
	for key, value := range env {
		cloned[key] = value
	}
	return cloned
}

func currentEnvironment() map[string]string {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	return env
}
