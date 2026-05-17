package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

var supportedDefaultConfigNames = []string{"config.toml", "config.yaml", "config.yml", "config.json"}

var supportedConfigSuffixes = []string{".toml", ".yaml", ".yml", ".json"}

var defaultLooperHomeForNotices = DefaultLooperHome

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
	Warnings []string
	Notices  []string
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

	configPath := ""
	if parsedCLI.hasConfigPath {
		configPath = parsedCLI.configPath
	} else if envConfigPath, ok := lookupEnv("LOOPER_CONFIG"); ok {
		configPath = envConfigPath
	} else if options.ConfigPath != "" {
		configPath = options.ConfigPath
	} else if options.DefaultConfigPath != "" {
		configPath = options.DefaultConfigPath
	}
	if configPath == "" {
		defaultConfigPath, err := DiscoverDefaultConfigPath()
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

	fileWarnings := collectMixedSchemaWarnings(partialConfig)
	fileNotices, err := collectConfigLoadNotices(resolvedConfigPath, present)
	if err != nil {
		return LoadedFileConfig{}, err
	}
	envWarnings := collectDeprecatedEnvWarnings(lookupEnv)
	cliWarnings := collectDeprecatedCLIWarnings(options.Args)
	config, err := Normalize(cwd, partialConfig, envOverrides, parsedCLI.overrides)
	if err != nil {
		return LoadedFileConfig{}, err
	}
	applyGlobalReviewerEnableSelfReviewOverride(&config, envOverrides)
	applyGlobalReviewerEnableSelfReviewOverride(&config, parsedCLI.overrides)

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
		Config:   config,
		Partial:  partialConfig,
		Warnings: dedupeWarnings(fileWarnings, envWarnings, cliWarnings),
		Notices:  fileNotices,
		Metadata: LoadFileMetadata{
			ConfigPath:        resolvedConfigPath,
			ConfigFilePresent: present,
			ToolDetection:     toolDetection.Detection,
		},
	}, nil
}

func collectConfigLoadNotices(resolvedConfigPath string, present bool) ([]string, error) {
	if !present {
		return nil, nil
	}
	looperHome, err := defaultLooperHomeForNotices()
	if err != nil {
		return nil, nil
	}
	legacyDefaultPath := filepath.Join(looperHome, "config.json")
	canonicalDefaultPath := filepath.Join(looperHome, "config.toml")
	if filepath.Clean(resolvedConfigPath) != filepath.Clean(legacyDefaultPath) {
		return nil, nil
	}
	return []string{legacyDefaultConfigMigrationNote(legacyDefaultPath, canonicalDefaultPath)}, nil
}

func DiscoverDefaultConfigPath() (string, error) {
	looperHome, err := DefaultLooperHome()
	if err != nil {
		return "", err
	}

	candidates := make([]string, 0, len(supportedDefaultConfigNames))
	found := make([]string, 0, len(supportedDefaultConfigNames))
	for _, name := range supportedDefaultConfigNames {
		path := filepath.Join(looperHome, name)
		candidates = append(candidates, path)
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("check config file at %s: %w", path, err)
		}
		if info.IsDir() {
			continue
		}
		found = append(found, path)
	}

	if len(found) > 1 {
		canonicalTOMLPath := filepath.Join(looperHome, "config.toml")
		legacyJSONPath := filepath.Join(looperHome, "config.json")
		if len(found) == 2 && containsConfigPath(found, canonicalTOMLPath) && containsConfigPath(found, legacyJSONPath) {
			return canonicalTOMLPath, nil
		}
		return "", fmt.Errorf("multiple default config files found: %s; keep only one of %s", strings.Join(found, ", "), strings.Join(candidates, ", "))
	}
	if len(found) == 1 {
		return found[0], nil
	}

	return candidates[0], nil
}

func containsConfigPath(paths []string, target string) bool {
	cleanTarget := filepath.Clean(target)
	for _, path := range paths {
		if filepath.Clean(path) == cleanTarget {
			return true
		}
	}
	return false
}

func validateConfiguredToolPath(path *string, field string) error {
	if isNilOrEmptyString(path) {
		return nil
	}
	value := strings.TrimSpace(*path)
	if !filepath.IsAbs(value) && !strings.ContainsRune(value, os.PathSeparator) {
		return nil
	}
	info, err := os.Stat(value)
	if err == nil && !info.IsDir() {
		return nil
	}
	message := "must reference an existing executable file"
	if err == nil && info.IsDir() {
		message = "must reference a file, not a directory"
	}
	return &ConfigValidationError{Issues: []ValidationIssue{{Path: field, Message: message}}}
}

func applyGlobalReviewerEnableSelfReviewOverride(config *Config, partial PartialConfig) {
	if config == nil || partial.Roles == nil || partial.Roles.Reviewer == nil {
		return
	}
	var source *PartialReviewerRoleTriggersConfig
	if partial.Roles.Reviewer.Discovery != nil && partial.Roles.Reviewer.Discovery.Triggers != nil && partial.Roles.Reviewer.Discovery.Triggers.EnableSelfReview != nil {
		source = partial.Roles.Reviewer.Discovery.Triggers
	} else if partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.EnableSelfReview != nil {
		source = partial.Roles.Reviewer.Triggers
	}
	if source == nil || source.EnableSelfReview == nil {
		return
	}
	value := *source.EnableSelfReview
	config.Roles.Reviewer.Discovery.Triggers.EnableSelfReview = value
	for i := range config.Projects {
		if config.Projects[i].Roles == nil {
			continue
		}
		if config.Projects[i].Roles.Reviewer == nil {
			continue
		}
		if config.Projects[i].Roles.Reviewer.Discovery == nil {
			config.Projects[i].Roles.Reviewer.Discovery = &PartialReviewerRoleDiscoveryConfig{}
		}
		if config.Projects[i].Roles.Reviewer.Discovery.Triggers == nil {
			config.Projects[i].Roles.Reviewer.Discovery.Triggers = &PartialReviewerRoleTriggersConfig{}
		}
		config.Projects[i].Roles.Reviewer.Discovery.Triggers.EnableSelfReview = &value
	}
}

func readConfigFile(path string) (PartialConfig, bool, error) {
	if err := validateConfigFileSuffix(path); err != nil {
		return PartialConfig{}, false, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return PartialConfig{}, false, nil
		}

		return PartialConfig{}, false, fmt.Errorf("failed to read config file at %s: %w", path, err)
	}

	partialConfig, err := decodeConfigFile(path, raw)
	if err != nil {
		return PartialConfig{}, true, fmt.Errorf("failed to read config file at %s: %w", path, err)
	}

	return partialConfig, true, nil
}

func validateConfigFileSuffix(path string) error {
	suffix := strings.ToLower(filepath.Ext(path))
	for _, supported := range supportedConfigSuffixes {
		if suffix == supported {
			return nil
		}
	}
	if suffix == "" {
		suffix = "<none>"
	}
	return fmt.Errorf("unsupported config file suffix %q at %s; supported suffixes: %s", suffix, path, strings.Join(supportedConfigSuffixes, ", "))
}

func decodeConfigFile(path string, raw []byte) (PartialConfig, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return decodeJSONConfigFile(raw)
	case ".yaml", ".yml":
		return decodeStructuredConfigFile(raw, func(target any) error { return yaml.Unmarshal(raw, target) })
	case ".toml":
		return decodeStructuredConfigFile(raw, func(target any) error { return toml.Unmarshal(raw, target) })
	default:
		return PartialConfig{}, fmt.Errorf("unsupported config file suffix %q", filepath.Ext(path))
	}
}

func decodeJSONConfigFile(raw []byte) (PartialConfig, error) {
	var partialConfig PartialConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decodeTopLevelConfigSections(decoder, &partialConfig); err != nil {
		return PartialConfig{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return PartialConfig{}, fmt.Errorf("trailing JSON value")
	}
	return partialConfig, nil
}

func decodeStructuredConfigFile(raw []byte, unmarshal func(any) error) (PartialConfig, error) {
	var decoded map[string]any
	if err := unmarshal(&decoded); err != nil {
		return PartialConfig{}, err
	}
	normalizedRaw, err := json.Marshal(decoded)
	if err != nil {
		return PartialConfig{}, fmt.Errorf("normalize structured config: %w", err)
	}
	return decodeJSONConfigFile(normalizedRaw)
}

func decodeTopLevelConfigSections(decoder *json.Decoder, partialConfig *PartialConfig) error {
	type topLevelConfigSection struct {
		key    string
		decode func(json.RawMessage) error
	}

	sections := []topLevelConfigSection{
		{key: "server", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "server", &partialConfig.Server)
		}},
		{key: "storage", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "storage", &partialConfig.Storage)
		}},
		{key: "scheduler", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "scheduler", &partialConfig.Scheduler)
		}},
		{key: "webhook", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "webhook", &partialConfig.Webhook)
		}},
		{key: "agent", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "agent", &partialConfig.Agent)
		}},
		{key: "logging", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "logging", &partialConfig.Logging)
		}},
		{key: "notifications", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "notifications", &partialConfig.Notifications)
		}},
		{key: "disclosure", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "disclosure", &partialConfig.Disclosure)
		}},
		{key: "tools", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "tools", &partialConfig.Tools)
		}},
		{key: "daemon", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "daemon", &partialConfig.Daemon)
		}},
		{key: "package", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "package", &partialConfig.Package)
		}},
		{key: "defaults", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "defaults", &partialConfig.Defaults)
		}},
		{key: "reviewer", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "reviewer", &partialConfig.LegacyReviewer)
		}},
		{key: "instructions", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "instructions", &partialConfig.Instructions)
		}},
		{key: "roles", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "roles", &partialConfig.Roles)
		}},
		{key: "projects", decode: func(raw json.RawMessage) error {
			return decodeTopLevelConfigSection(raw, "projects", &partialConfig.Projects)
		}},
	}

	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return nil
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("invalid JSON value for config: expected object")
	}

	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}

		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("invalid JSON value for config: expected object key")
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}

		matched := false
		for index := range sections {
			if strings.EqualFold(key, sections[index].key) {
				if err := sections[index].decode(value); err != nil {
					return err
				}
				matched = true
				break
			}
		}

		if !matched {
			continue
		}
	}

	if token, err = decoder.Token(); err != nil {
		return err
	} else if delimiter, ok := token.(json.Delim); !ok || delimiter != '}' {
		return fmt.Errorf("invalid JSON value for config: expected object terminator")
	}

	return nil
}

func decodeTopLevelConfigSection[T any](raw json.RawMessage, key string, target **T) error {
	if len(raw) == 0 {
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decodeTarget := any(target)
	if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if target != nil && *target != nil {
			decodeTarget = *target
		}
	}
	if err := decoder.Decode(decodeTarget); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("invalid JSON value for %q", key)
	}

	return nil
}

func parseCLIArgs(args []string) (parsedCLIArgs, error) {
	parsed := parsedCLIArgs{}
	canonicalInstructionsEnabledOverrideSet := false
	canonicalPackageAutoUpgradeOverrideSet := false
	canonicalReviewerCleanOverrideSet := false
	canonicalReviewerBlockingOverrideSet := false
	canonicalReviewerLoopEnabledOverrideSet := false
	canonicalReviewerEnableSelfReviewOverrideSet := false
	canonicalFixerAuthorFilterOverrideSet := false

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
		case matchesFlag(arg, "--no-custom-instructions"):
			disable := true
			if _, value, ok := strings.Cut(arg, "="); ok {
				parsedValue, err := parseBoolean(value)
				if err != nil {
					return parsedCLIArgs{}, fmt.Errorf("invalid value for --no-custom-instructions: %q is not a boolean", value)
				}
				disable = *parsedValue
			} else if index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
				if parsedValue, err := parseBoolean(args[index+1]); err == nil {
					disable = *parsedValue
					index++
				}
			}
			if !canonicalInstructionsEnabledOverrideSet {
				ensureInstructionsConfig(&parsed.overrides).Enabled = boolPtr(!disable)
			}
		case matchesFlag(arg, "--instructions-enabled"):
			value, nextIndex, err := takeValue(index, "--instructions-enabled")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseBoolean(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --instructions-enabled: %q is not a boolean", value)
			}
			ensureInstructionsConfig(&parsed.overrides).Enabled = parsedValue
			canonicalInstructionsEnabledOverrideSet = true
			index = nextIndex
		case matchesFlag(arg, "--no-auto-upgrade"):
			disable := true
			if _, value, ok := strings.Cut(arg, "="); ok {
				parsedValue, err := parseBoolean(value)
				if err != nil {
					return parsedCLIArgs{}, fmt.Errorf("invalid value for --no-auto-upgrade: %q is not a boolean", value)
				}
				disable = *parsedValue
			} else if index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
				if parsedValue, err := parseBoolean(args[index+1]); err == nil {
					disable = *parsedValue
					index++
				}
			}
			if !canonicalPackageAutoUpgradeOverrideSet {
				ensurePackageConfig(&parsed.overrides).AutoUpgradeEnabled = boolPtr(!disable)
			}
		case matchesFlag(arg, "--package-auto-upgrade-enabled"):
			value, nextIndex, err := takeValue(index, "--package-auto-upgrade-enabled")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseBoolean(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --package-auto-upgrade-enabled: %q is not a boolean", value)
			}
			ensurePackageConfig(&parsed.overrides).AutoUpgradeEnabled = parsedValue
			canonicalPackageAutoUpgradeOverrideSet = true
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
		case matchesFlag(arg, "--daemon-restart-policy"):
			value, nextIndex, err := takeValue(index, "--daemon-restart-policy")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			policy := DaemonRestartPolicy(value)
			ensureDaemonConfig(&parsed.overrides).RestartPolicy = &policy
			index = nextIndex
		case matchesFlag(arg, "--daemon-restart-throttle-seconds"):
			value, nextIndex, err := takeValue(index, "--daemon-restart-throttle-seconds")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseInteger(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --daemon-restart-throttle-seconds: %q is not an integer", value)
			}
			ensureDaemonConfig(&parsed.overrides).RestartThrottleSeconds = parsedValue
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
		case matchesFlag(arg, "--planner-agent-timeout-seconds"):
			value, nextIndex, err := takeValue(index, "--planner-agent-timeout-seconds")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseInteger(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --planner-agent-timeout-seconds: %q is not an integer", value)
			}
			ensureAgentTimeoutConfig(&parsed.overrides).PlannerSeconds = parsedValue
			index = nextIndex
		case matchesFlag(arg, "--worker-agent-timeout-seconds"):
			value, nextIndex, err := takeValue(index, "--worker-agent-timeout-seconds")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseInteger(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --worker-agent-timeout-seconds: %q is not an integer", value)
			}
			ensureAgentTimeoutConfig(&parsed.overrides).WorkerSeconds = parsedValue
			index = nextIndex
		case matchesFlag(arg, "--reviewer-agent-timeout-seconds"):
			value, nextIndex, err := takeValue(index, "--reviewer-agent-timeout-seconds")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseInteger(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --reviewer-agent-timeout-seconds: %q is not an integer", value)
			}
			ensureAgentTimeoutConfig(&parsed.overrides).ReviewerSeconds = parsedValue
			index = nextIndex
		case matchesFlag(arg, "--fixer-agent-timeout-seconds"):
			value, nextIndex, err := takeValue(index, "--fixer-agent-timeout-seconds")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseInteger(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --fixer-agent-timeout-seconds: %q is not an integer", value)
			}
			ensureAgentTimeoutConfig(&parsed.overrides).FixerSeconds = parsedValue
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
		case matchesAnyFlag(arg, "--roles-reviewer-behavior-review-events-clean", "--reviewer-clean-review-event"):
			value, nextIndex, err := takeValue(index, "--roles-reviewer-behavior-review-events-clean")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			event := ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(value)))
			if matchesFlag(arg, "--roles-reviewer-behavior-review-events-clean") {
				ensureReviewerReviewEventsConfig(&parsed.overrides).Clean = &event
				canonicalReviewerCleanOverrideSet = true
			} else if !canonicalReviewerCleanOverrideSet {
				ensureReviewerReviewEventsConfig(&parsed.overrides).Clean = &event
			}
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
			if !canonicalReviewerCleanOverrideSet {
				event := ReviewerReviewEventComment
				if *parsedValue {
					event = ReviewerReviewEventApprove
				}
				ensureReviewerReviewEventsConfig(&parsed.overrides).Clean = &event
			}
			index = nextIndex
		case matchesFlag(arg, "--roles-fixer-triggers-author-filter"):
			value, nextIndex, err := takeValue(index, "--roles-fixer-triggers-author-filter")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseFixerAuthorFilter(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --roles-fixer-triggers-author-filter: must be one of: %s, %s", FixerAuthorFilterCurrentUser, FixerAuthorFilterAny)
			}
			ensureFixerRoleTriggersConfig(&parsed.overrides).AuthorFilter = parsedValue
			canonicalFixerAuthorFilterOverrideSet = true
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
			if !canonicalFixerAuthorFilterOverrideSet {
				authorFilter := FixerAuthorFilterCurrentUser
				if *parsedValue {
					authorFilter = FixerAuthorFilterAny
				}
				ensureFixerRoleTriggersConfig(&parsed.overrides).AuthorFilter = &authorFilter
			}
			index = nextIndex
		case matchesFlag(arg, "--roles-reviewer-behavior-loop-enabled-by-default"):
			value, nextIndex, err := takeValue(index, "--roles-reviewer-behavior-loop-enabled-by-default")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseBoolean(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --roles-reviewer-behavior-loop-enabled-by-default: %q is not a boolean", value)
			}
			ensureReviewerLoopConfig(&parsed.overrides).EnabledByDefault = parsedValue
			canonicalReviewerLoopEnabledOverrideSet = true
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
			if !canonicalReviewerLoopEnabledOverrideSet {
				ensureReviewerLoopConfig(&parsed.overrides).EnabledByDefault = parsedValue
			}
			index = nextIndex
		case matchesFlag(arg, "--roles-reviewer-discovery-triggers-enable-self-review"):
			value, nextIndex, err := takeValue(index, "--roles-reviewer-discovery-triggers-enable-self-review")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseBoolean(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --roles-reviewer-discovery-triggers-enable-self-review: %q is not a boolean", value)
			}
			ensureReviewerRoleTriggersConfig(&parsed.overrides).EnableSelfReview = parsedValue
			canonicalReviewerEnableSelfReviewOverrideSet = true
			index = nextIndex
		case matchesFlag(arg, "--reviewer-enable-self-review"):
			value, nextIndex, err := takeValue(index, "--reviewer-enable-self-review")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseBoolean(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --reviewer-enable-self-review: %q is not a boolean", value)
			}
			if !canonicalReviewerEnableSelfReviewOverrideSet {
				ensureReviewerRoleTriggersConfig(&parsed.overrides).EnableSelfReview = parsedValue
			}
			index = nextIndex
		case matchesAnyFlag(arg, "--roles-reviewer-behavior-review-events-blocking", "--reviewer-blocking-review-event"):
			value, nextIndex, err := takeValue(index, "--roles-reviewer-behavior-review-events-blocking")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			event := ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(value)))
			if matchesFlag(arg, "--roles-reviewer-behavior-review-events-blocking") {
				ensureReviewerReviewEventsConfig(&parsed.overrides).Blocking = &event
				canonicalReviewerBlockingOverrideSet = true
			} else if !canonicalReviewerBlockingOverrideSet {
				ensureReviewerReviewEventsConfig(&parsed.overrides).Blocking = &event
			}
			index = nextIndex
		case matchesAnyFlag(arg, "--roles-reviewer-behavior-loop-quiet-period-seconds", "--reviewer-quiet-period-seconds"):
			value, nextIndex, err := takeValue(index, "--roles-reviewer-behavior-loop-quiet-period-seconds")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseInteger(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --reviewer-quiet-period-seconds: %q is not an integer", value)
			}
			ensureReviewerLoopConfig(&parsed.overrides).QuietPeriodSeconds = parsedValue
			index = nextIndex
		case matchesAnyFlag(arg, "--roles-reviewer-behavior-loop-min-publish-interval-seconds", "--reviewer-min-publish-interval-seconds"):
			value, nextIndex, err := takeValue(index, "--roles-reviewer-behavior-loop-min-publish-interval-seconds")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseInteger(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --reviewer-min-publish-interval-seconds: %q is not an integer", value)
			}
			ensureReviewerLoopConfig(&parsed.overrides).MinPublishIntervalSeconds = parsedValue
			index = nextIndex
		case matchesAnyFlag(arg, "--roles-reviewer-behavior-loop-max-iterations-per-pr", "--reviewer-max-iterations-per-pr"):
			value, nextIndex, err := takeValue(index, "--roles-reviewer-behavior-loop-max-iterations-per-pr")
			if err != nil {
				return parsedCLIArgs{}, err
			}
			parsedValue, err := parseInteger(value)
			if err != nil {
				return parsedCLIArgs{}, fmt.Errorf("invalid value for --reviewer-max-iterations-per-pr: %q is not an integer", value)
			}
			ensureReviewerLoopConfig(&parsed.overrides).MaxIterationsPerPR = parsedValue
			index = nextIndex
		case matchesAnyFlag(arg, "--roles-reviewer-behavior-loop-max-iterations-per-head", "--reviewer-max-iterations-per-head"):
			value, nextIndex, err := takeValue(index, "--roles-reviewer-behavior-loop-max-iterations-per-head")
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

func collectDeprecatedCLIWarnings(args []string) []string {
	deprecated := []deprecatedSurface{}
	add := func(flag string, replacement string) {
		deprecated = append(deprecated, deprecatedSurface{kind: deprecatedSurfaceCLIFlag, legacy: flag, replacement: replacement})
	}

	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			continue
		}

		switch {
		case matchesFlag(arg, "--allow-auto-approve"):
			add("--allow-auto-approve", "--roles-reviewer-behavior-review-events-clean")
		case matchesFlag(arg, "--fix-all-pull-requests"):
			add("--fix-all-pull-requests", "--roles-fixer-triggers-author-filter")
		case matchesFlag(arg, "--reviewer-loop-enabled"):
			add("--reviewer-loop-enabled", "--roles-reviewer-behavior-loop-enabled-by-default")
		case matchesFlag(arg, "--reviewer-enable-self-review"):
			add("--reviewer-enable-self-review", "--roles-reviewer-discovery-triggers-enable-self-review")
		case matchesFlag(arg, "--reviewer-clean-review-event"):
			add("--reviewer-clean-review-event", "--roles-reviewer-behavior-review-events-clean")
		case matchesFlag(arg, "--reviewer-blocking-review-event"):
			add("--reviewer-blocking-review-event", "--roles-reviewer-behavior-review-events-blocking")
		case matchesFlag(arg, "--reviewer-quiet-period-seconds"):
			add("--reviewer-quiet-period-seconds", "--roles-reviewer-behavior-loop-quiet-period-seconds")
		case matchesFlag(arg, "--reviewer-min-publish-interval-seconds"):
			add("--reviewer-min-publish-interval-seconds", "--roles-reviewer-behavior-loop-min-publish-interval-seconds")
		case matchesFlag(arg, "--reviewer-max-iterations-per-pr"):
			add("--reviewer-max-iterations-per-pr", "--roles-reviewer-behavior-loop-max-iterations-per-pr")
		case matchesFlag(arg, "--reviewer-max-iterations-per-head"):
			add("--reviewer-max-iterations-per-head", "--roles-reviewer-behavior-loop-max-iterations-per-head")
		}
	}

	return dedupeDeprecationWarnings(deprecated)
}

func matchesFlag(arg string, flag string) bool {
	return arg == flag || strings.HasPrefix(arg, flag+"=")
}

func matchesAnyFlag(arg string, flags ...string) bool {
	for _, flag := range flags {
		if matchesFlag(arg, flag) {
			return true
		}
	}
	return false
}

func parseInteger(value string) (*int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return nil, err
	}

	return &parsed, nil
}

func parseBoolean(value string) (*bool, error) {
	value = strings.TrimSpace(value)
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

func parseStringList(value string) (*[]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		parsed := []string{}
		return &parsed, nil
	}
	parts := strings.Split(value, ",")
	parsed := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			return nil, fmt.Errorf("list contains empty item")
		}
		parsed = append(parsed, item)
	}
	return &parsed, nil
}

func parseLabelMode(value string) (*LabelMode, error) {
	mode := LabelMode(strings.TrimSpace(value))
	if !isValidLabelMode(mode) {
		return nil, fmt.Errorf("invalid label mode")
	}
	return &mode, nil
}

func parseFixerAuthorFilter(value string) (*FixerAuthorFilter, error) {
	filter := FixerAuthorFilter(strings.TrimSpace(value))
	if !isValidFixerAuthorFilter(filter) {
		return nil, fmt.Errorf("invalid author filter")
	}
	return &filter, nil
}

func buildEnvOverrides(lookupEnv EnvLookupFunc) (PartialConfig, error) {
	var overrides PartialConfig
	envValue := func(primary string, aliases ...string) (string, bool) {
		if value, ok := lookupEnv(primary); ok {
			return value, true
		}
		for _, alias := range aliases {
			if value, ok := lookupEnv(alias); ok {
				return value, true
			}
		}
		return "", false
	}

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
	if value, ok := lookupEnv("LOOPER_DAEMON_RESTART_POLICY"); ok {
		policy := DaemonRestartPolicy(value)
		ensureDaemonConfig(&overrides).RestartPolicy = &policy
	}
	if value, ok := lookupEnv("LOOPER_DAEMON_RESTART_THROTTLE_SECONDS"); ok {
		parsed, err := parseInteger(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_DAEMON_RESTART_THROTTLE_SECONDS: %q is not an integer", value)
		}
		ensureDaemonConfig(&overrides).RestartThrottleSeconds = parsed
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
	if value, ok := lookupEnv("LOOPER_AUTO_UPGRADE_ENABLED"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_AUTO_UPGRADE_ENABLED: %q is not a boolean", value)
		}
		ensurePackageConfig(&overrides).AutoUpgradeEnabled = parsed
	}
	if err := applyAgentTimeoutEnvOverrides(&overrides, lookupEnv); err != nil {
		return PartialConfig{}, err
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
		event := ReviewerReviewEventComment
		if *parsed {
			event = ReviewerReviewEventApprove
		}
		ensureReviewerReviewEventsConfig(&overrides).Clean = &event
	}
	if value, ok := lookupEnv("LOOPER_FIX_ALL_PULL_REQUESTS"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_FIX_ALL_PULL_REQUESTS: %q is not a boolean", value)
		}
		ensureDefaultsConfig(&overrides).FixAllPullRequests = parsed
		authorFilter := FixerAuthorFilterCurrentUser
		if *parsed {
			authorFilter = FixerAuthorFilterAny
		}
		ensureFixerRoleTriggersConfig(&overrides).AuthorFilter = &authorFilter
	}
	if value, ok := envValue("LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_ENABLED_BY_DEFAULT", "LOOPER_REVIEWER_LOOP_ENABLED"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_ENABLED_BY_DEFAULT: %q is not a boolean", value)
		}
		ensureReviewerLoopConfig(&overrides).EnabledByDefault = parsed
	}
	if value, ok := envValue("LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_CLEAN", "LOOPER_REVIEWER_REVIEW_EVENTS_CLEAN"); ok {
		event := ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(value)))
		ensureReviewerReviewEventsConfig(&overrides).Clean = &event
	}
	if value, ok := envValue("LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_BLOCKING", "LOOPER_REVIEWER_REVIEW_EVENTS_BLOCKING"); ok {
		event := ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(value)))
		ensureReviewerReviewEventsConfig(&overrides).Blocking = &event
	}
	if value, ok := envValue("LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_QUIET_PERIOD_SECONDS", "LOOPER_REVIEWER_QUIET_PERIOD_SECONDS"); ok {
		parsed, err := parseInteger(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_QUIET_PERIOD_SECONDS: %q is not an integer", value)
		}
		ensureReviewerLoopConfig(&overrides).QuietPeriodSeconds = parsed
	}
	if value, ok := envValue("LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_MIN_PUBLISH_INTERVAL_SECONDS", "LOOPER_REVIEWER_MIN_PUBLISH_INTERVAL_SECONDS"); ok {
		parsed, err := parseInteger(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_MIN_PUBLISH_INTERVAL_SECONDS: %q is not an integer", value)
		}
		ensureReviewerLoopConfig(&overrides).MinPublishIntervalSeconds = parsed
	}
	if value, ok := envValue("LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_MAX_ITERATIONS_PER_PR", "LOOPER_REVIEWER_MAX_ITERATIONS_PER_PR"); ok {
		parsed, err := parseInteger(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_MAX_ITERATIONS_PER_PR: %q is not an integer", value)
		}
		ensureReviewerLoopConfig(&overrides).MaxIterationsPerPR = parsed
	}
	if value, ok := envValue("LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_MAX_ITERATIONS_PER_HEAD", "LOOPER_REVIEWER_MAX_ITERATIONS_PER_HEAD"); ok {
		parsed, err := parseInteger(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_MAX_ITERATIONS_PER_HEAD: %q is not an integer", value)
		}
		ensureReviewerLoopConfig(&overrides).MaxIterationsPerHead = parsed
	}
	if value, ok := envValue("LOOPER_ROLES_REVIEWER_BEHAVIOR_NATIVE_RESUME_ON_HEAD_CHANGE", "LOOPER_REVIEWER_NATIVE_RESUME_ON_HEAD_CHANGE"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_ROLES_REVIEWER_BEHAVIOR_NATIVE_RESUME_ON_HEAD_CHANGE: %q is not a boolean", value)
		}
		ensureReviewerNativeResumeConfig(&overrides).OnHeadChange = parsed
	}
	if value, ok := envValue("LOOPER_ROLES_REVIEWER_BEHAVIOR_NATIVE_RESUME_RE_REVIEW_PROMPT_ON_HEAD_CHANGE", "LOOPER_REVIEWER_NATIVE_RESUME_REREVIEW_PROMPT_ON_HEAD_CHANGE"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_ROLES_REVIEWER_BEHAVIOR_NATIVE_RESUME_RE_REVIEW_PROMPT_ON_HEAD_CHANGE: %q is not a boolean", value)
		}
		ensureReviewerNativeResumeConfig(&overrides).ReReviewPromptOnHeadChange = parsed
	}
	if value, ok := envValue("LOOPER_ROLES_REVIEWER_BEHAVIOR_THREAD_RESOLUTION_ENABLED", "LOOPER_REVIEWER_THREAD_RESOLUTION_ENABLED"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_ROLES_REVIEWER_BEHAVIOR_THREAD_RESOLUTION_ENABLED: %q is not a boolean", value)
		}
		ensureReviewerThreadResolutionConfig(&overrides).Enabled = parsed
	}
	if value, ok := envValue("LOOPER_ROLES_REVIEWER_BEHAVIOR_THREAD_RESOLUTION_MODE", "LOOPER_REVIEWER_THREAD_RESOLUTION_MODE"); ok {
		mode := ReviewerThreadResolutionMode(value)
		ensureReviewerThreadResolutionConfig(&overrides).Mode = &mode
	}
	if value, ok := envValue("LOOPER_ROLES_REVIEWER_BEHAVIOR_THREAD_RESOLUTION_MAX_THREADS_PER_RUN", "LOOPER_REVIEWER_THREAD_RESOLUTION_MAX_THREADS_PER_RUN"); ok {
		parsed, err := parseInteger(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_ROLES_REVIEWER_BEHAVIOR_THREAD_RESOLUTION_MAX_THREADS_PER_RUN: %q is not an integer", value)
		}
		ensureReviewerThreadResolutionConfig(&overrides).MaxThreadsPerRun = parsed
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
	if value, ok := lookupEnv("LOOPER_AGENT_NATIVE_RESUME_ENABLED"); ok {
		parsed, err := parseBoolean(value)
		if err != nil {
			return PartialConfig{}, fmt.Errorf("invalid value for LOOPER_AGENT_NATIVE_RESUME_ENABLED: %q is not a boolean", value)
		}
		ensureAgentNativeResumeConfig(&overrides).Enabled = parsed
	}
	if err := applyRoleEnvOverrides(&overrides, lookupEnv); err != nil {
		return PartialConfig{}, err
	}

	return overrides, nil
}

func collectDeprecatedEnvWarnings(lookupEnv EnvLookupFunc) []string {
	deprecated := []deprecatedSurface{}
	add := func(name string, replacement string) {
		if _, ok := lookupEnv(name); !ok {
			return
		}
		deprecated = append(deprecated, deprecatedSurface{kind: deprecatedSurfaceEnvVar, legacy: name, replacement: replacement})
	}

	add("LOOPER_ALLOW_AUTO_APPROVE", "LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_CLEAN")
	add("LOOPER_FIX_ALL_PULL_REQUESTS", "LOOPER_ROLES_FIXER_TRIGGERS_AUTHOR_FILTER")
	add("LOOPER_REVIEWER_LOOP_ENABLED", "LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_ENABLED_BY_DEFAULT")
	add("LOOPER_REVIEWER_REVIEW_EVENTS_CLEAN", "LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_CLEAN")
	add("LOOPER_REVIEWER_REVIEW_EVENTS_BLOCKING", "LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_BLOCKING")
	add("LOOPER_REVIEWER_QUIET_PERIOD_SECONDS", "LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_QUIET_PERIOD_SECONDS")
	add("LOOPER_REVIEWER_MIN_PUBLISH_INTERVAL_SECONDS", "LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_MIN_PUBLISH_INTERVAL_SECONDS")
	add("LOOPER_REVIEWER_MAX_ITERATIONS_PER_PR", "LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_MAX_ITERATIONS_PER_PR")
	add("LOOPER_REVIEWER_MAX_ITERATIONS_PER_HEAD", "LOOPER_ROLES_REVIEWER_BEHAVIOR_LOOP_MAX_ITERATIONS_PER_HEAD")
	add("LOOPER_REVIEWER_NATIVE_RESUME_ON_HEAD_CHANGE", "LOOPER_ROLES_REVIEWER_BEHAVIOR_NATIVE_RESUME_ON_HEAD_CHANGE")
	add("LOOPER_REVIEWER_NATIVE_RESUME_REREVIEW_PROMPT_ON_HEAD_CHANGE", "LOOPER_ROLES_REVIEWER_BEHAVIOR_NATIVE_RESUME_RE_REVIEW_PROMPT_ON_HEAD_CHANGE")
	add("LOOPER_REVIEWER_THREAD_RESOLUTION_ENABLED", "LOOPER_ROLES_REVIEWER_BEHAVIOR_THREAD_RESOLUTION_ENABLED")
	add("LOOPER_REVIEWER_THREAD_RESOLUTION_MODE", "LOOPER_ROLES_REVIEWER_BEHAVIOR_THREAD_RESOLUTION_MODE")
	add("LOOPER_REVIEWER_THREAD_RESOLUTION_MAX_THREADS_PER_RUN", "LOOPER_ROLES_REVIEWER_BEHAVIOR_THREAD_RESOLUTION_MAX_THREADS_PER_RUN")
	add("LOOPER_ROLES_REVIEWER_AUTO_DISCOVERY", "LOOPER_ROLES_REVIEWER_DISCOVERY_AUTO_DISCOVERY")
	add("LOOPER_ROLES_REVIEWER_TRIGGERS_INCLUDE_DRAFTS", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_INCLUDE_DRAFTS")
	add("LOOPER_ROLES_REVIEWER_TRIGGERS_REQUIRE_REVIEW_REQUEST", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_REQUIRE_REVIEW_REQUEST")
	add("LOOPER_ROLES_REVIEWER_TRIGGERS_ENABLE_SELF_REVIEW", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_ENABLE_SELF_REVIEW")
	add("LOOPER_ROLES_REVIEWER_TRIGGERS_LABELS", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_LABELS")
	add("LOOPER_ROLES_REVIEWER_TRIGGERS_LABEL_MODE", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_LABEL_MODE")
	add("LOOPER_ROLES_REVIEWER_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL", "LOOPER_ROLES_REVIEWER_DISCOVERY_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL")
	add("LOOPER_ROLES_REVIEWER_SPEC_REVIEW_REVIEWING_LABEL", "LOOPER_ROLES_REVIEWER_DISCOVERY_SPEC_REVIEW_REVIEWING_LABEL")

	return dedupeDeprecationWarnings(deprecated)
}

func applyAgentTimeoutEnvOverrides(overrides *PartialConfig, lookupEnv EnvLookupFunc) error {
	integerEnv := func(name string, set func(*int)) error {
		value, ok := lookupEnv(name)
		if !ok {
			return nil
		}
		parsed, err := parseInteger(value)
		if err != nil {
			return fmt.Errorf("invalid value for %s: %q is not an integer", name, value)
		}
		set(parsed)
		return nil
	}

	if err := integerEnv("LOOPER_AGENT_TIMEOUTS_PLANNER_SECONDS", func(v *int) { ensureAgentTimeoutConfig(overrides).PlannerSeconds = v }); err != nil {
		return err
	}
	if err := integerEnv("LOOPER_AGENT_TIMEOUTS_WORKER_SECONDS", func(v *int) { ensureAgentTimeoutConfig(overrides).WorkerSeconds = v }); err != nil {
		return err
	}
	if err := integerEnv("LOOPER_AGENT_TIMEOUTS_REVIEWER_SECONDS", func(v *int) { ensureAgentTimeoutConfig(overrides).ReviewerSeconds = v }); err != nil {
		return err
	}
	if err := integerEnv("LOOPER_AGENT_TIMEOUTS_FIXER_SECONDS", func(v *int) { ensureAgentTimeoutConfig(overrides).FixerSeconds = v }); err != nil {
		return err
	}
	if err := integerEnv("LOOPER_AGENT_TIMEOUTS_PLANNER_IDLE_TIMEOUT_SECONDS", func(v *int) { ensureAgentTimeoutConfig(overrides).PlannerIdleTimeoutSeconds = v }); err != nil {
		return err
	}
	if err := integerEnv("LOOPER_AGENT_TIMEOUTS_PLANNER_MAX_RUNTIME_SECONDS", func(v *int) { ensureAgentTimeoutConfig(overrides).PlannerMaxRuntimeSeconds = v }); err != nil {
		return err
	}
	if err := integerEnv("LOOPER_AGENT_TIMEOUTS_WORKER_IDLE_TIMEOUT_SECONDS", func(v *int) { ensureAgentTimeoutConfig(overrides).WorkerIdleTimeoutSeconds = v }); err != nil {
		return err
	}
	if err := integerEnv("LOOPER_AGENT_TIMEOUTS_WORKER_MAX_RUNTIME_SECONDS", func(v *int) { ensureAgentTimeoutConfig(overrides).WorkerMaxRuntimeSeconds = v }); err != nil {
		return err
	}
	if err := integerEnv("LOOPER_AGENT_TIMEOUTS_REVIEWER_IDLE_TIMEOUT_SECONDS", func(v *int) { ensureAgentTimeoutConfig(overrides).ReviewerIdleTimeoutSeconds = v }); err != nil {
		return err
	}
	if err := integerEnv("LOOPER_AGENT_TIMEOUTS_REVIEWER_MAX_RUNTIME_SECONDS", func(v *int) { ensureAgentTimeoutConfig(overrides).ReviewerMaxRuntimeSeconds = v }); err != nil {
		return err
	}
	if err := integerEnv("LOOPER_AGENT_TIMEOUTS_FIXER_IDLE_TIMEOUT_SECONDS", func(v *int) { ensureAgentTimeoutConfig(overrides).FixerIdleTimeoutSeconds = v }); err != nil {
		return err
	}
	if err := integerEnv("LOOPER_AGENT_TIMEOUTS_FIXER_MAX_RUNTIME_SECONDS", func(v *int) { ensureAgentTimeoutConfig(overrides).FixerMaxRuntimeSeconds = v }); err != nil {
		return err
	}
	return nil
}

func applyRoleEnvOverrides(overrides *PartialConfig, lookupEnv EnvLookupFunc) error {
	envValue := func(primary string, aliases ...string) (string, bool) {
		if value, ok := lookupEnv(primary); ok {
			return value, true
		}
		for _, alias := range aliases {
			if value, ok := lookupEnv(alias); ok {
				return value, true
			}
		}
		return "", false
	}
	boolEnv := func(name string, set func(*bool), aliases ...string) error {
		value, ok := envValue(name, aliases...)
		if !ok {
			return nil
		}
		parsed, err := parseBoolean(value)
		if err != nil {
			return fmt.Errorf("invalid value for %s: %q is not a boolean", name, value)
		}
		set(parsed)
		return nil
	}
	listEnv := func(name string, set func(*[]string), aliases ...string) error {
		value, ok := envValue(name, aliases...)
		if !ok {
			return nil
		}
		parsed, err := parseStringList(value)
		if err != nil {
			return fmt.Errorf("invalid value for %s: %q is not a comma-separated string list", name, value)
		}
		set(parsed)
		return nil
	}
	labelModeEnv := func(name string, set func(*LabelMode), aliases ...string) error {
		value, ok := envValue(name, aliases...)
		if !ok {
			return nil
		}
		parsed, err := parseLabelMode(value)
		if err != nil {
			return fmt.Errorf("invalid value for %s: must be one of: %s, %s", name, LabelModeAll, LabelModeAny)
		}
		set(parsed)
		return nil
	}

	if err := boolEnv("LOOPER_ROLES_PLANNER_AUTO_DISCOVERY", func(v *bool) { ensurePlannerRoleConfig(overrides).AutoDiscovery = v }); err != nil {
		return err
	}
	if err := listEnv("LOOPER_ROLES_PLANNER_TRIGGERS_LABELS", func(v *[]string) { ensurePlannerRoleTriggersConfig(overrides).Labels = v }); err != nil {
		return err
	}
	if err := labelModeEnv("LOOPER_ROLES_PLANNER_TRIGGERS_LABEL_MODE", func(v *LabelMode) { ensurePlannerRoleTriggersConfig(overrides).LabelMode = v }); err != nil {
		return err
	}
	if err := boolEnv("LOOPER_ROLES_PLANNER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER", func(v *bool) { ensurePlannerRoleTriggersConfig(overrides).RequireAssigneeCurrentUser = v }); err != nil {
		return err
	}

	if err := boolEnv("LOOPER_ROLES_WORKER_AUTO_DISCOVERY", func(v *bool) { ensureWorkerRoleConfig(overrides).AutoDiscovery = v }); err != nil {
		return err
	}
	if err := listEnv("LOOPER_ROLES_WORKER_TRIGGERS_LABELS", func(v *[]string) { ensureWorkerRoleTriggersConfig(overrides).Labels = v }); err != nil {
		return err
	}
	if err := labelModeEnv("LOOPER_ROLES_WORKER_TRIGGERS_LABEL_MODE", func(v *LabelMode) { ensureWorkerRoleTriggersConfig(overrides).LabelMode = v }); err != nil {
		return err
	}
	if err := boolEnv("LOOPER_ROLES_WORKER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER", func(v *bool) { ensureWorkerRoleTriggersConfig(overrides).RequireAssigneeCurrentUser = v }); err != nil {
		return err
	}

	if err := boolEnv("LOOPER_ROLES_REVIEWER_DISCOVERY_AUTO_DISCOVERY", func(v *bool) { ensureReviewerRoleConfig(overrides).AutoDiscovery = v }, "LOOPER_ROLES_REVIEWER_AUTO_DISCOVERY"); err != nil {
		return err
	}
	if err := boolEnv("LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_INCLUDE_DRAFTS", func(v *bool) { ensureReviewerRoleTriggersConfig(overrides).IncludeDrafts = v }, "LOOPER_ROLES_REVIEWER_TRIGGERS_INCLUDE_DRAFTS"); err != nil {
		return err
	}
	if err := boolEnv("LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_REQUIRE_REVIEW_REQUEST", func(v *bool) { ensureReviewerRoleTriggersConfig(overrides).RequireReviewRequest = v }, "LOOPER_ROLES_REVIEWER_TRIGGERS_REQUIRE_REVIEW_REQUEST"); err != nil {
		return err
	}
	if err := boolEnv("LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_ENABLE_SELF_REVIEW", func(v *bool) { ensureReviewerRoleTriggersConfig(overrides).EnableSelfReview = v }, "LOOPER_ROLES_REVIEWER_TRIGGERS_ENABLE_SELF_REVIEW"); err != nil {
		return err
	}
	if err := listEnv("LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_LABELS", func(v *[]string) { ensureReviewerRoleTriggersConfig(overrides).Labels = v }, "LOOPER_ROLES_REVIEWER_TRIGGERS_LABELS"); err != nil {
		return err
	}
	if err := labelModeEnv("LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_LABEL_MODE", func(v *LabelMode) { ensureReviewerRoleTriggersConfig(overrides).LabelMode = v }, "LOOPER_ROLES_REVIEWER_TRIGGERS_LABEL_MODE"); err != nil {
		return err
	}
	if err := boolEnv("LOOPER_ROLES_REVIEWER_DISCOVERY_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL", func(v *bool) { ensureReviewerSpecReviewConfig(overrides).IncludeReviewingLabel = v }, "LOOPER_ROLES_REVIEWER_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL"); err != nil {
		return err
	}
	if value, ok := envValue("LOOPER_ROLES_REVIEWER_DISCOVERY_SPEC_REVIEW_REVIEWING_LABEL", "LOOPER_ROLES_REVIEWER_SPEC_REVIEW_REVIEWING_LABEL"); ok {
		ensureReviewerSpecReviewConfig(overrides).ReviewingLabel = stringPtr(strings.TrimSpace(value))
	}

	if err := boolEnv("LOOPER_ROLES_FIXER_AUTO_DISCOVERY", func(v *bool) { ensureFixerRoleConfig(overrides).AutoDiscovery = v }); err != nil {
		return err
	}
	if err := boolEnv("LOOPER_ROLES_FIXER_TRIGGERS_INCLUDE_DRAFTS", func(v *bool) { ensureFixerRoleTriggersConfig(overrides).IncludeDrafts = v }); err != nil {
		return err
	}
	if err := listEnv("LOOPER_ROLES_FIXER_TRIGGERS_LABELS", func(v *[]string) { ensureFixerRoleTriggersConfig(overrides).Labels = v }); err != nil {
		return err
	}
	if err := labelModeEnv("LOOPER_ROLES_FIXER_TRIGGERS_LABEL_MODE", func(v *LabelMode) { ensureFixerRoleTriggersConfig(overrides).LabelMode = v }); err != nil {
		return err
	}
	if value, ok := lookupEnv("LOOPER_ROLES_FIXER_TRIGGERS_AUTHOR_FILTER"); ok {
		parsed, err := parseFixerAuthorFilter(value)
		if err != nil {
			return fmt.Errorf("invalid value for LOOPER_ROLES_FIXER_TRIGGERS_AUTHOR_FILTER: must be one of: %s, %s", FixerAuthorFilterCurrentUser, FixerAuthorFilterAny)
		}
		ensureFixerRoleTriggersConfig(overrides).AuthorFilter = parsed
	}
	return nil
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

func ensureAgentConfig(partial *PartialConfig) *PartialAgentConfig {
	if partial.Agent == nil {
		partial.Agent = &PartialAgentConfig{}
	}

	return partial.Agent
}

func ensureAgentTimeoutConfig(partial *PartialConfig) *PartialAgentTimeoutConfig {
	agent := ensureAgentConfig(partial)
	if agent.Timeouts == nil {
		agent.Timeouts = &PartialAgentTimeoutConfig{}
	}

	return agent.Timeouts
}

func ensureAgentNativeResumeConfig(partial *PartialConfig) *PartialAgentNativeResumeConfig {
	agent := ensureAgentConfig(partial)
	if agent.NativeResume == nil {
		agent.NativeResume = &PartialAgentNativeResumeConfig{}
	}

	return agent.NativeResume
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

func ensurePackageConfig(partial *PartialConfig) *PartialPackageConfig {
	if partial.Package == nil {
		partial.Package = &PartialPackageConfig{}
	}

	return partial.Package
}

func ensureDefaultsConfig(partial *PartialConfig) *PartialDefaultsConfig {
	if partial.Defaults == nil {
		partial.Defaults = &PartialDefaultsConfig{}
	}

	return partial.Defaults
}

func ensureReviewerConfig(partial *PartialConfig) *PartialReviewerConfig {
	reviewer := ensureReviewerRoleConfig(partial)
	if reviewer.Behavior == nil {
		reviewer.Behavior = &PartialReviewerConfig{}
	}
	return reviewer.Behavior
}

func ensureReviewerLoopConfig(partial *PartialConfig) *PartialReviewerLoopConfig {
	reviewer := ensureReviewerConfig(partial)
	if reviewer.Loop == nil {
		reviewer.Loop = &PartialReviewerLoopConfig{}
	}
	return reviewer.Loop
}

func ensureReviewerThreadResolutionConfig(partial *PartialConfig) *PartialReviewerThreadResolutionConfig {
	reviewer := ensureReviewerConfig(partial)
	if reviewer.ThreadResolution == nil {
		reviewer.ThreadResolution = &PartialReviewerThreadResolutionConfig{}
	}
	return reviewer.ThreadResolution
}

func ensureReviewerReviewEventsConfig(partial *PartialConfig) *PartialReviewerReviewEventsConfig {
	reviewer := ensureReviewerConfig(partial)
	if reviewer.ReviewEvents == nil {
		reviewer.ReviewEvents = &PartialReviewerReviewEventsConfig{}
	}
	return reviewer.ReviewEvents
}

func ensureReviewerNativeResumeConfig(partial *PartialConfig) *PartialReviewerNativeResumeConfig {
	reviewer := ensureReviewerConfig(partial)
	if reviewer.NativeResume == nil {
		reviewer.NativeResume = &PartialReviewerNativeResumeConfig{}
	}
	return reviewer.NativeResume
}

func ensureInstructionsConfig(partial *PartialConfig) *PartialInstructionsConfig {
	if partial.Instructions == nil {
		partial.Instructions = &PartialInstructionsConfig{}
	}
	return partial.Instructions
}

func boolPtr(value bool) *bool { return &value }

func ensureRoleConfigs(partial *PartialConfig) *PartialRoleConfigs {
	if partial.Roles == nil {
		partial.Roles = &PartialRoleConfigs{}
	}
	return partial.Roles
}

func ensurePlannerRoleConfig(partial *PartialConfig) *PartialPlannerRoleConfig {
	roles := ensureRoleConfigs(partial)
	if roles.Planner == nil {
		roles.Planner = &PartialPlannerRoleConfig{}
	}
	return roles.Planner
}

func ensurePlannerRoleTriggersConfig(partial *PartialConfig) *PartialIssueRoleTriggersConfig {
	planner := ensurePlannerRoleConfig(partial)
	if planner.Triggers == nil {
		planner.Triggers = &PartialIssueRoleTriggersConfig{}
	}
	return planner.Triggers
}

func ensureWorkerRoleConfig(partial *PartialConfig) *PartialWorkerRoleConfig {
	roles := ensureRoleConfigs(partial)
	if roles.Worker == nil {
		roles.Worker = &PartialWorkerRoleConfig{}
	}
	return roles.Worker
}

func ensureWorkerRoleTriggersConfig(partial *PartialConfig) *PartialIssueRoleTriggersConfig {
	worker := ensureWorkerRoleConfig(partial)
	if worker.Triggers == nil {
		worker.Triggers = &PartialIssueRoleTriggersConfig{}
	}
	return worker.Triggers
}

func ensureReviewerRoleConfig(partial *PartialConfig) *PartialReviewerRoleConfig {
	roles := ensureRoleConfigs(partial)
	if roles.Reviewer == nil {
		roles.Reviewer = &PartialReviewerRoleConfig{}
	}
	return roles.Reviewer
}

func ensureReviewerRoleTriggersConfig(partial *PartialConfig) *PartialReviewerRoleTriggersConfig {
	reviewer := ensureReviewerRoleConfig(partial)
	if reviewer.Triggers == nil {
		reviewer.Triggers = &PartialReviewerRoleTriggersConfig{}
	}
	return reviewer.Triggers
}

func ensureReviewerSpecReviewConfig(partial *PartialConfig) *PartialReviewerSpecReviewConfig {
	reviewer := ensureReviewerRoleConfig(partial)
	if reviewer.SpecReview == nil {
		reviewer.SpecReview = &PartialReviewerSpecReviewConfig{}
	}
	return reviewer.SpecReview
}

func ensureFixerRoleConfig(partial *PartialConfig) *PartialFixerRoleConfig {
	roles := ensureRoleConfigs(partial)
	if roles.Fixer == nil {
		roles.Fixer = &PartialFixerRoleConfig{}
	}
	return roles.Fixer
}

func ensureFixerRoleTriggersConfig(partial *PartialConfig) *PartialFixerRoleTriggersConfig {
	fixer := ensureFixerRoleConfig(partial)
	if fixer.Triggers == nil {
		fixer.Triggers = &PartialFixerRoleTriggersConfig{}
	}
	return fixer.Triggers
}
