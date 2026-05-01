package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type EnvLookupFunc func(string) (string, bool)

type LoadFileMetadata struct {
	ConfigPath        string
	ConfigFilePresent bool
	ToolDetection     map[string]ToolDetectionStatus
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
	Args              []string
	LookupEnv         EnvLookupFunc
	LookPath          LookPathFunc
}

type parsedCLIArgs struct {
	configPath    string
	hasConfigPath bool
	overrides     PartialConfig
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

	lookupEnv := options.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	parsedCLI, err := parseCLIArgs(options.Args)
	if err != nil {
		return LoadedFileConfig{}, err
	}

	configPath := options.ConfigPath
	if parsedCLI.hasConfigPath {
		configPath = parsedCLI.configPath
	} else if envConfigPath, ok := lookupEnv("LOOPER_CONFIG"); ok {
		configPath = envConfigPath
	} else if configPath == "" {
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

	envOverrides, err := buildEnvOverrides(lookupEnv)
	if err != nil {
		return LoadedFileConfig{}, err
	}

	config, err := Normalize(cwd, partialConfig, envOverrides, parsedCLI.overrides)
	if err != nil {
		return LoadedFileConfig{}, err
	}

	toolDetection := DetectToolPaths(config.Tools, options.LookPath)
	config.Tools = toolDetection.Paths
	if config.Notifications.Osascript.Enabled && isNilOrEmptyString(config.Tools.OsascriptPath) {
		return LoadedFileConfig{}, &ConfigValidationError{Issues: []ValidationIssue{{
			Path:    "tools.osascriptPath",
			Message: "is required when notifications.osascript.enabled is true",
		}}}
	}

	if err := Validate(config); err != nil {
		return LoadedFileConfig{}, err
	}

	return LoadedFileConfig{
		Config:  config,
		Partial: partialConfig,
		Metadata: LoadFileMetadata{
			ConfigPath:        resolvedConfigPath,
			ConfigFilePresent: present,
			ToolDetection:     toolDetection.Detection,
		},
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

func parseCLIArgs(args []string) (parsedCLIArgs, error) {
	parsed := parsedCLIArgs{}

	takeValue := func(index int, flag string) (string, int, error) {
		current := args[index]
		if current == "" {
			return "", index, fmt.Errorf("missing value for %s", flag)
		}

		if _, value, ok := strings.Cut(current, "="); ok {
			return value, index, nil
		}

		nextIndex := index + 1
		if nextIndex >= len(args) || strings.HasPrefix(args[nextIndex], "--") {
			return "", index, fmt.Errorf("missing value for %s", flag)
		}

		return args[nextIndex], nextIndex, nil
	}

	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "--") {
			continue
		}

		switch {
		case matchesFlag(arg, "--config"):
			value, nextIndex, err := takeValue(index, "--config")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsed.configPath = value
			parsed.hasConfigPath = true
			index = nextIndex
		case matchesFlag(arg, "--host"):
			value, nextIndex, err := takeValue(index, "--host")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			ensureServerConfig(&parsed.overrides).Host = stringPtr(value)
			index = nextIndex
		case matchesFlag(arg, "--port"):
			value, nextIndex, err := takeValue(index, "--port")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseInteger(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --port: %q is not an integer", value)
			}
			ensureServerConfig(&parsed.overrides).Port = parsedValue
			index = nextIndex
		case matchesFlag(arg, "--db-path"):
			value, nextIndex, err := takeValue(index, "--db-path")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			ensureStorageConfig(&parsed.overrides).DBPath = stringPtr(value)
			index = nextIndex
		case matchesFlag(arg, "--log-dir"):
			value, nextIndex, err := takeValue(index, "--log-dir")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			ensureDaemonConfig(&parsed.overrides).LogDir = stringPtr(value)
			index = nextIndex
		case matchesFlag(arg, "--daemon-mode"):
			value, nextIndex, err := takeValue(index, "--daemon-mode")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			daemonMode := DaemonMode(value)
			ensureDaemonConfig(&parsed.overrides).Mode = &daemonMode
			index = nextIndex
		case matchesFlag(arg, "--git-path"):
			value, nextIndex, err := takeValue(index, "--git-path")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			ensureToolPathsConfig(&parsed.overrides).GitPath = stringPtr(value)
			index = nextIndex
		case matchesFlag(arg, "--gh-path"):
			value, nextIndex, err := takeValue(index, "--gh-path")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			ensureToolPathsConfig(&parsed.overrides).GHPath = stringPtr(value)
			index = nextIndex
		case matchesFlag(arg, "--looper-path"):
			value, nextIndex, err := takeValue(index, "--looper-path")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			ensureToolPathsConfig(&parsed.overrides).LooperPath = stringPtr(value)
			index = nextIndex
		case matchesFlag(arg, "--allow-auto-commit"):
			value, nextIndex, err := takeValue(index, "--allow-auto-commit")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseBoolean(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --allow-auto-commit: %q is not a boolean", value)
			}
			ensureDefaultsConfig(&parsed.overrides).AllowAutoCommit = parsedValue
			index = nextIndex
		case matchesFlag(arg, "--allow-auto-push"):
			value, nextIndex, err := takeValue(index, "--allow-auto-push")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseBoolean(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --allow-auto-push: %q is not a boolean", value)
			}
			ensureDefaultsConfig(&parsed.overrides).AllowAutoPush = parsedValue
			index = nextIndex
		case matchesFlag(arg, "--allow-auto-approve"):
			value, nextIndex, err := takeValue(index, "--allow-auto-approve")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseBoolean(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --allow-auto-approve: %q is not a boolean", value)
			}
			ensureDefaultsConfig(&parsed.overrides).AllowAutoApprove = parsedValue
			index = nextIndex
		case matchesFlag(arg, "--fix-all-pull-requests"):
			value, nextIndex, err := takeValue(index, "--fix-all-pull-requests")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseBoolean(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --fix-all-pull-requests: %q is not a boolean", value)
			}
			ensureDefaultsConfig(&parsed.overrides).FixAllPullRequests = parsedValue
			index = nextIndex
		case matchesFlag(arg, "--reviewer-loop-enabled"):
			value, nextIndex, err := takeValue(index, "--reviewer-loop-enabled")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseBoolean(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --reviewer-loop-enabled: %q is not a boolean", value)
			}
			ensureReviewerLoopConfig(&parsed.overrides).EnabledByDefault = parsedValue
			index = nextIndex
		case matchesFlag(arg, "--reviewer-quiet-period-seconds"):
			value, nextIndex, err := takeValue(index, "--reviewer-quiet-period-seconds")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseInteger(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --reviewer-quiet-period-seconds: %q is not an integer", value)
			}
			ensureReviewerLoopConfig(&parsed.overrides).QuietPeriodSeconds = parsedValue
			index = nextIndex
		case matchesFlag(arg, "--reviewer-max-iterations-per-pr"):
			value, nextIndex, err := takeValue(index, "--reviewer-max-iterations-per-pr")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseInteger(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --reviewer-max-iterations-per-pr: %q is not an integer", value)
			}
			ensureReviewerLoopConfig(&parsed.overrides).MaxIterationsPerPR = parsedValue
			index = nextIndex
		case matchesFlag(arg, "--reviewer-max-iterations-per-head"):
			value, nextIndex, err := takeValue(index, "--reviewer-max-iterations-per-head")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseInteger(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --reviewer-max-iterations-per-head: %q is not an integer", value)
			}
			ensureReviewerLoopConfig(&parsed.overrides).MaxIterationsPerHead = parsedValue
			index = nextIndex
		case matchesFlag(arg, "--osascript-path"):
			value, nextIndex, err := takeValue(index, "--osascript-path")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			ensureToolPathsConfig(&parsed.overrides).OsascriptPath = stringPtr(value)
			index = nextIndex
		default:
			return parsedCLIArgs{}, fmt.Errorf("Unknown looperd argument: %s", arg)
		}
	}

	return parsed, nil
}

func matchesFlag(arg string, flag string) bool {
	return arg == flag || strings.HasPrefix(arg, flag+"=")
}

func parseInteger(value string) (*int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return nil, err
	}

	return &parsed, nil
}

func parseBoolean(value string) (*bool, error) {
	if value == "" {
		return nil, fmt.Errorf("boolean value cannot be empty")
	}

	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		parsed := true
		return &parsed, nil
	case "0", "false", "no", "off":
		parsed := false
		return &parsed, nil
	default:
		return nil, fmt.Errorf("invalid boolean")
	}
}

func buildEnvOverrides(lookupEnv EnvLookupFunc) (PartialConfig, error) {
	var overrides PartialConfig

	if value, ok := lookupEnv("LOOPER_HOST"); ok {
		ensureServerConfig(&overrides).Host = stringPtr(value)
	}
	if value, ok := lookupEnv("LOOPER_PORT"); ok {
		parsed, err := parseInteger(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_PORT: %q is not an integer", value)
		}
		ensureServerConfig(&overrides).Port = parsed
	}
	if value, ok := lookupEnv("LOOPER_DB_PATH"); ok {
		ensureStorageConfig(&overrides).DBPath = stringPtr(value)
	}
	if value, ok := lookupEnv("LOOPER_LOG_DIR"); ok {
		ensureDaemonConfig(&overrides).LogDir = stringPtr(value)
	}
	if value, ok := lookupEnv("LOOPER_DAEMON_MODE"); ok {
		daemonMode := DaemonMode(value)
		ensureDaemonConfig(&overrides).Mode = &daemonMode
	}
	if value, ok := lookupEnv("LOOPER_WORKING_DIRECTORY"); ok {
		ensureDaemonConfig(&overrides).WorkingDirectory = stringPtr(value)
	}
	if value, ok := lookupEnv("LOOPER_IN_APP_NOTIFICATIONS"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_IN_APP_NOTIFICATIONS: %q is not a boolean", value)
		}
		ensureNotificationConfig(&overrides).InApp = parsed
	}
	if value, ok := lookupEnv("LOOPER_OSASCRIPT_ENABLED"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_OSASCRIPT_ENABLED: %q is not a boolean", value)
		}
		ensureOsascriptNotificationConfig(&overrides).Enabled = parsed
	}
	if value, ok := lookupEnv("LOOPER_ALLOW_AUTO_COMMIT"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_ALLOW_AUTO_COMMIT: %q is not a boolean", value)
		}
		ensureDefaultsConfig(&overrides).AllowAutoCommit = parsed
	}
	if value, ok := lookupEnv("LOOPER_ALLOW_AUTO_PUSH"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_ALLOW_AUTO_PUSH: %q is not a boolean", value)
		}
		ensureDefaultsConfig(&overrides).AllowAutoPush = parsed
	}
	if value, ok := lookupEnv("LOOPER_ALLOW_AUTO_APPROVE"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_ALLOW_AUTO_APPROVE: %q is not a boolean", value)
		}
		ensureDefaultsConfig(&overrides).AllowAutoApprove = parsed
	}
	if value, ok := lookupEnv("LOOPER_FIX_ALL_PULL_REQUESTS"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_FIX_ALL_PULL_REQUESTS: %q is not a boolean", value)
		}
		ensureDefaultsConfig(&overrides).FixAllPullRequests = parsed
	}
	if value, ok := lookupEnv("LOOPER_REVIEWER_LOOP_ENABLED"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_REVIEWER_LOOP_ENABLED: %q is not a boolean", value)
		}
		ensureReviewerLoopConfig(&overrides).EnabledByDefault = parsed
	}
	if value, ok := lookupEnv("LOOPER_REVIEWER_QUIET_PERIOD_SECONDS"); ok {
		parsed, err := parseInteger(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_REVIEWER_QUIET_PERIOD_SECONDS: %q is not an integer", value)
		}
		ensureReviewerLoopConfig(&overrides).QuietPeriodSeconds = parsed
	}
	if value, ok := lookupEnv("LOOPER_REVIEWER_MAX_ITERATIONS_PER_PR"); ok {
		parsed, err := parseInteger(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_REVIEWER_MAX_ITERATIONS_PER_PR: %q is not an integer", value)
		}
		ensureReviewerLoopConfig(&overrides).MaxIterationsPerPR = parsed
	}
	if value, ok := lookupEnv("LOOPER_REVIEWER_MAX_ITERATIONS_PER_HEAD"); ok {
		parsed, err := parseInteger(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_REVIEWER_MAX_ITERATIONS_PER_HEAD: %q is not an integer", value)
		}
		ensureReviewerLoopConfig(&overrides).MaxIterationsPerHead = parsed
	}
	if value, ok := lookupEnv("LOOPER_GIT_PATH"); ok {
		ensureToolPathsConfig(&overrides).GitPath = stringPtr(value)
	}
	if value, ok := lookupEnv("LOOPER_GH_PATH"); ok {
		ensureToolPathsConfig(&overrides).GHPath = stringPtr(value)
	}
	if value, ok := lookupEnv("LOOPER_LOOPER_PATH"); ok {
		ensureToolPathsConfig(&overrides).LooperPath = stringPtr(value)
	}
	if value, ok := lookupEnv("LOOPER_OSASCRIPT_PATH"); ok {
		ensureToolPathsConfig(&overrides).OsascriptPath = stringPtr(value)
	}

	return overrides, nil
}

func ensureServerConfig(partial *PartialConfig) *PartialServerConfig {
	if partial.Server == nil {
		partial.Server = &PartialServerConfig{}
	}

	return partial.Server
}

func ensureStorageConfig(partial *PartialConfig) *PartialStorageConfig {
	if partial.Storage == nil {
		partial.Storage = &PartialStorageConfig{}
	}

	return partial.Storage
}

func ensureNotificationConfig(partial *PartialConfig) *PartialNotificationConfig {
	if partial.Notifications == nil {
		partial.Notifications = &PartialNotificationConfig{}
	}

	return partial.Notifications
}

func ensureOsascriptNotificationConfig(partial *PartialConfig) *PartialOsascriptNotificationConfig {
	notifications := ensureNotificationConfig(partial)
	if notifications.Osascript == nil {
		notifications.Osascript = &PartialOsascriptNotificationConfig{}
	}

	return notifications.Osascript
}

func ensureToolPathsConfig(partial *PartialConfig) *PartialToolPathsConfig {
	if partial.Tools == nil {
		partial.Tools = &PartialToolPathsConfig{}
	}

	return partial.Tools
}

func ensureDaemonConfig(partial *PartialConfig) *PartialDaemonConfig {
	if partial.Daemon == nil {
		partial.Daemon = &PartialDaemonConfig{}
	}

	return partial.Daemon
}

func ensureDefaultsConfig(partial *PartialConfig) *PartialDefaultsConfig {
	if partial.Defaults == nil {
		partial.Defaults = &PartialDefaultsConfig{}
	}

	return partial.Defaults
}

func ensureReviewerConfig(partial *PartialConfig) *PartialReviewerConfig {
	if partial.Reviewer == nil {
		partial.Reviewer = &PartialReviewerConfig{}
	}
	return partial.Reviewer
}

func ensureReviewerLoopConfig(partial *PartialConfig) *PartialReviewerLoopConfig {
	reviewer := ensureReviewerConfig(partial)
	if reviewer.Loop == nil {
		reviewer.Loop = &PartialReviewerLoopConfig{}
	}
	return reviewer.Loop
}
