package bootstrap

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/powerformer/looper/internal/config"
)

type Runtime interface{}

type RuntimeDependencies struct {
	Config   config.Config
	Metadata config.LoadFileMetadata
	Logger   Logger
}

type LoadConfigFunc func(config.LoadFileOptions) (config.LoadedFileConfig, error)

type CreateLoggerFunc func(config.LoggingConfig, string, LoggerOptions) (Logger, error)

type StartRuntimeFunc func(context.Context, RuntimeDependencies) (Runtime, error)

type Options struct {
	Args         []string
	Env          map[string]string
	CWD          string
	Stdout       io.Writer
	Stderr       io.Writer
	LoadConfig   LoadConfigFunc
	CreateLogger CreateLoggerFunc
	StartRuntime StartRuntimeFunc
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

	loadedConfig, err := loadConfig(config.LoadFileOptions{
		CWD:       options.CWD,
		Args:      options.Args,
		LookupEnv: envLookupFromMap(options.Env),
	})
	if err != nil {
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

	runtime, err := options.StartRuntime(ctx, RuntimeDependencies{
		Config:   loadedConfig.Config,
		Metadata: loadedConfig.Metadata,
		Logger:   logger,
	})
	if err != nil {
		return Result{}, err
	}

	result.Runtime = runtime
	return result, nil
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
