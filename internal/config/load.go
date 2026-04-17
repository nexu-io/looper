package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type LoadFileMetadata struct {
	ConfigPath        string
	ConfigFilePresent bool
}

type LoadedFileConfig struct {
	Config   Config
	Metadata LoadFileMetadata
	Partial  PartialConfig
}

type LoadFileOptions struct {
	CWD               string
	ConfigPath        string
	DefaultConfigPath string
}

func ResolveConfigPath(path string, cwd string) string {
	if filepath.IsAbs(path) {
		return path
	}

	return filepath.Join(cwd, path)
}

func LoadFile(options LoadFileOptions) (LoadedFileConfig, error) {
	cwd := options.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return LoadedFileConfig{}, fmt.Errorf("determine current working directory: %w", err)
		}
	}

	configPath := options.ConfigPath
	if configPath == "" {
		configPath = options.DefaultConfigPath
	}
	if configPath == "" {
		defaultConfigPath, err := DefaultConfigPath()
		if err != nil {
			return LoadedFileConfig{}, fmt.Errorf("determine default config path: %w", err)
		}

		configPath = defaultConfigPath
	}

	resolvedConfigPath := ResolveConfigPath(configPath, cwd)
	partialConfig, present, err := readConfigFile(resolvedConfigPath)
	if err != nil {
		return LoadedFileConfig{}, err
	}

	config, err := Normalize(cwd, partialConfig)
	if err != nil {
		return LoadedFileConfig{}, err
	}

	return LoadedFileConfig{
		Config:   config,
		Partial:  partialConfig,
		Metadata: LoadFileMetadata{ConfigPath: resolvedConfigPath, ConfigFilePresent: present},
	}, nil
}

func readConfigFile(path string) (PartialConfig, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return PartialConfig{}, false, nil
		}

		return PartialConfig{}, false, fmt.Errorf("failed to read config file at %s: %w", path, err)
	}

	var partialConfig PartialConfig
	if err := json.Unmarshal(raw, &partialConfig); err != nil {
		return PartialConfig{}, true, fmt.Errorf("failed to read config file at %s: %w", path, err)
	}

	return partialConfig, true, nil
}
