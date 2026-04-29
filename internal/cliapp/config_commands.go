package cliapp

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/spf13/cobra"
)

type configField struct {
	key       string
	valueType string
	env       string
	flag      string
	get       func(config.Config) any
	set       func(*config.PartialConfig, string) error
	unset     func(*config.PartialConfig)
}

var configFieldRegistry = map[string]configField{
	"defaults.baseBranch":         stringField("defaults.baseBranch", "", "", func(c config.Config) any { return c.Defaults.BaseBranch }, func(p *config.PartialConfig) **string { return &ensurePartialDefaults(p).BaseBranch }),
	"defaults.allowAutoCommit":    boolField("defaults.allowAutoCommit", "LOOPER_ALLOW_AUTO_COMMIT", "", func(c config.Config) any { return c.Defaults.AllowAutoCommit }, func(p *config.PartialConfig) **bool { return &ensurePartialDefaults(p).AllowAutoCommit }),
	"defaults.allowAutoPush":      boolField("defaults.allowAutoPush", "LOOPER_ALLOW_AUTO_PUSH", "", func(c config.Config) any { return c.Defaults.AllowAutoPush }, func(p *config.PartialConfig) **bool { return &ensurePartialDefaults(p).AllowAutoPush }),
	"defaults.allowAutoApprove":   boolField("defaults.allowAutoApprove", "LOOPER_ALLOW_AUTO_APPROVE", "", func(c config.Config) any { return c.Defaults.AllowAutoApprove }, func(p *config.PartialConfig) **bool { return &ensurePartialDefaults(p).AllowAutoApprove }),
	"defaults.allowAutoMerge":     boolField("defaults.allowAutoMerge", "", "", func(c config.Config) any { return c.Defaults.AllowAutoMerge }, func(p *config.PartialConfig) **bool { return &ensurePartialDefaults(p).AllowAutoMerge }),
	"defaults.allowRiskyFixes":    boolField("defaults.allowRiskyFixes", "", "", func(c config.Config) any { return c.Defaults.AllowRiskyFixes }, func(p *config.PartialConfig) **bool { return &ensurePartialDefaults(p).AllowRiskyFixes }),
	"defaults.fixAllPullRequests": boolField("defaults.fixAllPullRequests", "LOOPER_FIX_ALL_PULL_REQUESTS", "fix-all-pull-requests", func(c config.Config) any { return c.Defaults.FixAllPullRequests }, func(p *config.PartialConfig) **bool { return &ensurePartialDefaults(p).FixAllPullRequests }),
	"defaults.openPrStrategy":     openPRStrategyField(),
}

func (r *commandRuntime) configGet(cmd *cobra.Command, args []string) error {
	field, err := lookupConfigField(args[0])
	if err != nil {
		return err
	}
	loaded, err := r.loadRawConfigForEdit()
	if err != nil {
		return err
	}
	value := field.get(loaded.Config)
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"key": field.key, "value": value})
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), value)
	return err
}

func (r *commandRuntime) configSet(cmd *cobra.Command, args []string) error {
	field, err := lookupConfigField(args[0])
	if err != nil {
		return err
	}
	loaded, err := r.loadRawConfigForEdit()
	if err != nil {
		return err
	}
	partial := loaded.Partial
	if err := field.set(&partial, args[1]); err != nil {
		return err
	}
	if err := r.writeConfigFile(loaded.Metadata.ConfigPath, partial); err != nil {
		return err
	}
	r.warnConfigOverrides(cmd, field)
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"key": field.key, "configPath": loaded.Metadata.ConfigPath, "updated": true})
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Set %s in %s\n", field.key, loaded.Metadata.ConfigPath)
	return err
}

func (r *commandRuntime) configUnset(cmd *cobra.Command, args []string) error {
	field, err := lookupConfigField(args[0])
	if err != nil {
		return err
	}
	loaded, err := r.loadRawConfigForEdit()
	if err != nil {
		return err
	}
	partial := loaded.Partial
	field.unset(&partial)
	if err := r.writeConfigFile(loaded.Metadata.ConfigPath, partial); err != nil {
		return err
	}
	r.warnConfigOverrides(cmd, field)
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"key": field.key, "configPath": loaded.Metadata.ConfigPath, "updated": true})
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Unset %s in %s\n", field.key, loaded.Metadata.ConfigPath)
	return err
}

func (r *commandRuntime) configValidate(cmd *cobra.Command, args []string) error {
	loaded, err := r.loadConfigForEdit()
	if err != nil {
		return err
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"configPath": loaded.Metadata.ConfigPath, "valid": true})
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Config valid: %s\n", loaded.Metadata.ConfigPath)
	return err
}

func (r *commandRuntime) configShowSource(cmd *cobra.Command) error {
	loaded, err := r.loadConfigForEdit()
	if err != nil {
		return err
	}
	values := make(map[string]map[string]any, len(configFieldRegistry))
	for key, field := range configFieldRegistry {
		source := "default"
		if configFieldSet(loaded.Partial, key) {
			source = "config-file"
		}
		if field.env != "" {
			if _, ok := os.LookupEnv(field.env); ok {
				source = "env"
			}
		}
		if field.flag != "" && commandFlagChanged(cmd, field.flag) {
			source = "cli"
		}
		values[key] = map[string]any{"value": field.get(loaded.Config), "source": source}
	}
	return writeJSON(cmd.OutOrStdout(), map[string]any{"configPath": loaded.Metadata.ConfigPath, "fields": values})
}

func (r *commandRuntime) configEdit(cmd *cobra.Command, args []string) error {
	loaded, err := r.loadRawConfigForEdit()
	if err != nil {
		return err
	}
	if !loaded.Metadata.ConfigFilePresent {
		if err := r.writeConfigFile(loaded.Metadata.ConfigPath, loaded.Partial); err != nil {
			return err
		}
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		return fmt.Errorf("config edit requires VISUAL or EDITOR to be set")
	}
	if err := backupConfigFile(loaded.Metadata.ConfigPath); err != nil {
		return err
	}
	editCmd := exec.CommandContext(cmd.Context(), "sh", "-c", editor+" \"$1\"", "looper-editor", loaded.Metadata.ConfigPath)
	editCmd.Stdin = cmd.InOrStdin()
	editCmd.Stdout = cmd.OutOrStdout()
	editCmd.Stderr = cmd.ErrOrStderr()
	if err := editCmd.Run(); err != nil {
		return fmt.Errorf("run editor: %w", err)
	}
	loadedAfter, err := r.loadRawConfigForEdit()
	if err != nil {
		return err
	}
	if err := r.validateConfigFile(loadedAfter.Metadata.ConfigPath); err != nil {
		return err
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"configPath": loaded.Metadata.ConfigPath, "valid": true})
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Config valid: %s\n", loaded.Metadata.ConfigPath)
	return err
}

func (r *commandRuntime) loadConfigForEdit() (config.LoadedFileConfig, error) {
	return config.LoadFile(config.LoadFileOptions{Args: ExtractConfigArgs(r.argv), LookPath: r.lookPath()})
}

func (r *commandRuntime) loadRawConfigForEdit() (config.LoadedFileConfig, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return config.LoadedFileConfig{}, fmt.Errorf("determine current working directory: %w", err)
	}
	configPath, err := resolveConfigPathFromArgs(r.argv, cwd)
	if err != nil {
		return config.LoadedFileConfig{}, err
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return config.LoadedFileConfig{}, fmt.Errorf("failed to read config file at %s: %w", configPath, err)
		}
		full, normErr := config.Normalize(cwd)
		if normErr != nil {
			return config.LoadedFileConfig{}, normErr
		}
		return config.LoadedFileConfig{Config: full, Partial: config.PartialConfig{}, Metadata: config.LoadFileMetadata{ConfigPath: configPath, ConfigFilePresent: false}}, nil
	}
	var partial config.PartialConfig
	if err := json.Unmarshal(raw, &partial); err != nil {
		return config.LoadedFileConfig{}, fmt.Errorf("failed to read config file at %s: %w", configPath, err)
	}
	full, err := config.Normalize(cwd, partial)
	if err != nil {
		return config.LoadedFileConfig{}, err
	}
	return config.LoadedFileConfig{Config: full, Partial: partial, Metadata: config.LoadFileMetadata{ConfigPath: configPath, ConfigFilePresent: true}}, nil
}

func resolveConfigPathFromArgs(argv []string, cwd string) (string, error) {
	args := ExtractConfigArgs(argv)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--config" {
			if index+1 >= len(args) {
				return "", fmt.Errorf("missing value for --config")
			}
			return config.ResolveConfigPath(args[index+1], cwd), nil
		}
		if strings.HasPrefix(arg, "--config=") {
			return config.ResolveConfigPath(strings.TrimPrefix(arg, "--config="), cwd), nil
		}
	}
	if envPath, ok := os.LookupEnv("LOOPER_CONFIG"); ok {
		return config.ResolveConfigPath(envPath, cwd), nil
	}
	defaultPath, err := config.DefaultConfigPath()
	if err != nil {
		return "", fmt.Errorf("determine default config path: %w", err)
	}
	return config.ResolveConfigPath(defaultPath, cwd), nil
}

func (r *commandRuntime) writeConfigFile(path string, partial config.PartialConfig) error {
	if err := r.validatePartialConfig(partial); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	raw, err := json.MarshalIndent(partial, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := r.validateConfigFile(tmpPath); err != nil {
		return err
	}
	if err := backupConfigFile(path); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func (r *commandRuntime) validateConfigFile(path string) error {
	_, err := config.LoadFile(config.LoadFileOptions{Args: []string{"--config", path}, LookPath: r.lookPath()})
	return err
}

func (r *commandRuntime) validatePartialConfig(partial config.PartialConfig) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("determine current working directory: %w", err)
	}
	full, err := config.Normalize(cwd, partial)
	if err != nil {
		return err
	}
	return config.Validate(full)
}

func backupConfigFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config for backup: %w", err)
	}
	backupPath := fmt.Sprintf("%s.%s.bak", path, time.Now().UTC().Format("20060102150405.000000000"))
	if err := os.WriteFile(backupPath, raw, 0o600); err != nil {
		return fmt.Errorf("write config backup: %w", err)
	}
	return nil
}

func (r *commandRuntime) warnConfigOverrides(cmd *cobra.Command, field configField) {
	if field.env != "" {
		if _, ok := os.LookupEnv(field.env); ok {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s is set, so %s from the config file may not take effect\n", field.env, field.key)
		}
	}
	if field.flag != "" && commandFlagChanged(cmd, field.flag) {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: --%s is set, so %s from the config file may not take effect\n", field.flag, field.key)
	}
}

func commandFlagChanged(cmd *cobra.Command, name string) bool {
	flag := cmd.Flags().Lookup(name)
	if flag != nil && flag.Changed {
		return true
	}
	flag = cmd.InheritedFlags().Lookup(name)
	return flag != nil && flag.Changed
}

func lookupConfigField(key string) (configField, error) {
	field, ok := configFieldRegistry[key]
	if !ok {
		keys := make([]string, 0, len(configFieldRegistry))
		for registered := range configFieldRegistry {
			keys = append(keys, registered)
		}
		sort.Strings(keys)
		return configField{}, fmt.Errorf("unsupported config key %q; supported keys: %s", key, strings.Join(keys, ", "))
	}
	return field, nil
}

func boolField(key, env, flag string, get func(config.Config) any, target func(*config.PartialConfig) **bool) configField {
	return configField{key: key, valueType: "boolean", env: env, flag: flag, get: get, set: func(p *config.PartialConfig, raw string) error {
		value, err := parseConfigBool(raw)
		if err != nil {
			return fmt.Errorf("invalid value for %s: %q is not a boolean (use true or false)", key, raw)
		}
		*target(p) = &value
		return nil
	}, unset: func(p *config.PartialConfig) {
		if p.Defaults == nil {
			return
		}
		*target(p) = nil
	}}
}

func stringField(key, env, flag string, get func(config.Config) any, target func(*config.PartialConfig) **string) configField {
	return configField{key: key, valueType: "string", env: env, flag: flag, get: get, set: func(p *config.PartialConfig, raw string) error {
		if strings.TrimSpace(raw) == "" {
			return fmt.Errorf("invalid value for %s: must be a non-empty string", key)
		}
		*target(p) = &raw
		return nil
	}, unset: func(p *config.PartialConfig) {
		if p.Defaults == nil {
			return
		}
		*target(p) = nil
	}}
}

func openPRStrategyField() configField {
	return configField{key: "defaults.openPrStrategy", valueType: "string", get: func(c config.Config) any { return c.Defaults.OpenPRStrategy }, set: func(p *config.PartialConfig, raw string) error {
		switch config.OpenPRStrategy(raw) {
		case config.OpenPRStrategyAllDone, config.OpenPRStrategyFirstCommit, config.OpenPRStrategyManual:
			value := config.OpenPRStrategy(raw)
			ensurePartialDefaults(p).OpenPRStrategy = &value
			return nil
		default:
			return fmt.Errorf("invalid value for defaults.openPrStrategy: must be one of: %s, %s, %s", config.OpenPRStrategyAllDone, config.OpenPRStrategyFirstCommit, config.OpenPRStrategyManual)
		}
	}, unset: func(p *config.PartialConfig) {
		if p.Defaults != nil {
			p.Defaults.OpenPRStrategy = nil
		}
	}}
}

func parseConfigBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean")
	}
}

func ensurePartialDefaults(partial *config.PartialConfig) *config.PartialDefaultsConfig {
	if partial.Defaults == nil {
		partial.Defaults = &config.PartialDefaultsConfig{}
	}
	return partial.Defaults
}

func configFieldSet(partial config.PartialConfig, key string) bool {
	if partial.Defaults == nil {
		return false
	}
	switch key {
	case "defaults.baseBranch":
		return partial.Defaults.BaseBranch != nil
	case "defaults.allowAutoCommit":
		return partial.Defaults.AllowAutoCommit != nil
	case "defaults.allowAutoPush":
		return partial.Defaults.AllowAutoPush != nil
	case "defaults.allowAutoApprove":
		return partial.Defaults.AllowAutoApprove != nil
	case "defaults.allowAutoMerge":
		return partial.Defaults.AllowAutoMerge != nil
	case "defaults.allowRiskyFixes":
		return partial.Defaults.AllowRiskyFixes != nil
	case "defaults.fixAllPullRequests":
		return partial.Defaults.FixAllPullRequests != nil
	case "defaults.openPrStrategy":
		return partial.Defaults.OpenPRStrategy != nil
	default:
		return false
	}
}
