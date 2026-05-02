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
	"reviewer.reviewEvents.clean": reviewerReviewEventField("reviewer.reviewEvents.clean", "LOOPER_REVIEWER_REVIEW_EVENTS_CLEAN", "reviewer-clean-review-event", func(c config.Config) any { return c.Reviewer.ReviewEvents.Clean }, func(p *config.PartialConfig) **config.ReviewerReviewEvent {
		return &ensurePartialReviewerReviewEvents(p).Clean
	}),
	"reviewer.reviewEvents.blocking": reviewerReviewEventField("reviewer.reviewEvents.blocking", "LOOPER_REVIEWER_REVIEW_EVENTS_BLOCKING", "reviewer-blocking-review-event", func(c config.Config) any { return c.Reviewer.ReviewEvents.Blocking }, func(p *config.PartialConfig) **config.ReviewerReviewEvent {
		return &ensurePartialReviewerReviewEvents(p).Blocking
	}),
	"roles.planner.autoDiscovery":      boolField("roles.planner.autoDiscovery", "LOOPER_ROLES_PLANNER_AUTO_DISCOVERY", "", func(c config.Config) any { return c.Roles.Planner.AutoDiscovery }, func(p *config.PartialConfig) **bool { return &ensurePartialPlannerRole(p).AutoDiscovery }),
	"roles.planner.triggers.labels":    stringListField("roles.planner.triggers.labels", "LOOPER_ROLES_PLANNER_TRIGGERS_LABELS", func(c config.Config) any { return c.Roles.Planner.Triggers.Labels }, func(p *config.PartialConfig) **[]string { return &ensurePartialPlannerTriggers(p).Labels }),
	"roles.planner.triggers.labelMode": labelModeField("roles.planner.triggers.labelMode", "LOOPER_ROLES_PLANNER_TRIGGERS_LABEL_MODE", func(c config.Config) any { return c.Roles.Planner.Triggers.LabelMode }, func(p *config.PartialConfig) **config.LabelMode { return &ensurePartialPlannerTriggers(p).LabelMode }),
	"roles.planner.triggers.requireAssigneeCurrentUser": boolField("roles.planner.triggers.requireAssigneeCurrentUser", "LOOPER_ROLES_PLANNER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER", "", func(c config.Config) any { return c.Roles.Planner.Triggers.RequireAssigneeCurrentUser }, func(p *config.PartialConfig) **bool {
		return &ensurePartialPlannerTriggers(p).RequireAssigneeCurrentUser
	}),
	"roles.worker.autoDiscovery":      boolField("roles.worker.autoDiscovery", "LOOPER_ROLES_WORKER_AUTO_DISCOVERY", "", func(c config.Config) any { return c.Roles.Worker.AutoDiscovery }, func(p *config.PartialConfig) **bool { return &ensurePartialWorkerRole(p).AutoDiscovery }),
	"roles.worker.triggers.labels":    stringListField("roles.worker.triggers.labels", "LOOPER_ROLES_WORKER_TRIGGERS_LABELS", func(c config.Config) any { return c.Roles.Worker.Triggers.Labels }, func(p *config.PartialConfig) **[]string { return &ensurePartialWorkerTriggers(p).Labels }),
	"roles.worker.triggers.labelMode": labelModeField("roles.worker.triggers.labelMode", "LOOPER_ROLES_WORKER_TRIGGERS_LABEL_MODE", func(c config.Config) any { return c.Roles.Worker.Triggers.LabelMode }, func(p *config.PartialConfig) **config.LabelMode { return &ensurePartialWorkerTriggers(p).LabelMode }),
	"roles.worker.triggers.requireAssigneeCurrentUser": boolField("roles.worker.triggers.requireAssigneeCurrentUser", "LOOPER_ROLES_WORKER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER", "", func(c config.Config) any { return c.Roles.Worker.Triggers.RequireAssigneeCurrentUser }, func(p *config.PartialConfig) **bool {
		return &ensurePartialWorkerTriggers(p).RequireAssigneeCurrentUser
	}),
	"roles.reviewer.autoDiscovery":          boolField("roles.reviewer.autoDiscovery", "LOOPER_ROLES_REVIEWER_AUTO_DISCOVERY", "", func(c config.Config) any { return c.Roles.Reviewer.AutoDiscovery }, func(p *config.PartialConfig) **bool { return &ensurePartialReviewerRole(p).AutoDiscovery }),
	"roles.reviewer.triggers.includeDrafts": boolField("roles.reviewer.triggers.includeDrafts", "LOOPER_ROLES_REVIEWER_TRIGGERS_INCLUDE_DRAFTS", "", func(c config.Config) any { return c.Roles.Reviewer.Triggers.IncludeDrafts }, func(p *config.PartialConfig) **bool { return &ensurePartialReviewerRoleTriggers(p).IncludeDrafts }),
	"roles.reviewer.triggers.requireReviewRequest": boolField("roles.reviewer.triggers.requireReviewRequest", "LOOPER_ROLES_REVIEWER_TRIGGERS_REQUIRE_REVIEW_REQUEST", "", func(c config.Config) any { return c.Roles.Reviewer.Triggers.RequireReviewRequest }, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleTriggers(p).RequireReviewRequest
	}),
	"roles.reviewer.triggers.labels": stringListField("roles.reviewer.triggers.labels", "LOOPER_ROLES_REVIEWER_TRIGGERS_LABELS", func(c config.Config) any { return c.Roles.Reviewer.Triggers.Labels }, func(p *config.PartialConfig) **[]string { return &ensurePartialReviewerRoleTriggers(p).Labels }),
	"roles.reviewer.triggers.labelMode": labelModeField("roles.reviewer.triggers.labelMode", "LOOPER_ROLES_REVIEWER_TRIGGERS_LABEL_MODE", func(c config.Config) any { return c.Roles.Reviewer.Triggers.LabelMode }, func(p *config.PartialConfig) **config.LabelMode {
		return &ensurePartialReviewerRoleTriggers(p).LabelMode
	}),
	"roles.reviewer.specReview.includeReviewingLabel": boolField("roles.reviewer.specReview.includeReviewingLabel", "LOOPER_ROLES_REVIEWER_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL", "", func(c config.Config) any { return c.Roles.Reviewer.SpecReview.IncludeReviewingLabel }, func(p *config.PartialConfig) **bool { return &ensurePartialReviewerSpecReview(p).IncludeReviewingLabel }),
	"roles.reviewer.specReview.reviewingLabel":        stringField("roles.reviewer.specReview.reviewingLabel", "LOOPER_ROLES_REVIEWER_SPEC_REVIEW_REVIEWING_LABEL", "", func(c config.Config) any { return c.Roles.Reviewer.SpecReview.ReviewingLabel }, func(p *config.PartialConfig) **string { return &ensurePartialReviewerSpecReview(p).ReviewingLabel }),
	"roles.fixer.autoDiscovery":                       boolField("roles.fixer.autoDiscovery", "LOOPER_ROLES_FIXER_AUTO_DISCOVERY", "", func(c config.Config) any { return c.Roles.Fixer.AutoDiscovery }, func(p *config.PartialConfig) **bool { return &ensurePartialFixerRole(p).AutoDiscovery }),
	"roles.fixer.triggers.includeDrafts":              boolField("roles.fixer.triggers.includeDrafts", "LOOPER_ROLES_FIXER_TRIGGERS_INCLUDE_DRAFTS", "", func(c config.Config) any { return c.Roles.Fixer.Triggers.IncludeDrafts }, func(p *config.PartialConfig) **bool { return &ensurePartialFixerRoleTriggers(p).IncludeDrafts }),
	"roles.fixer.triggers.labels":                     stringListField("roles.fixer.triggers.labels", "LOOPER_ROLES_FIXER_TRIGGERS_LABELS", func(c config.Config) any { return c.Roles.Fixer.Triggers.Labels }, func(p *config.PartialConfig) **[]string { return &ensurePartialFixerRoleTriggers(p).Labels }),
	"roles.fixer.triggers.labelMode":                  labelModeField("roles.fixer.triggers.labelMode", "LOOPER_ROLES_FIXER_TRIGGERS_LABEL_MODE", func(c config.Config) any { return c.Roles.Fixer.Triggers.LabelMode }, func(p *config.PartialConfig) **config.LabelMode { return &ensurePartialFixerRoleTriggers(p).LabelMode }),
	"roles.fixer.triggers.authorFilter":               fixerAuthorFilterField(),
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
		*target(p) = nil
	}}
}

func stringListField(key, env string, get func(config.Config) any, target func(*config.PartialConfig) **[]string) configField {
	return configField{key: key, valueType: "string-list", env: env, get: get, set: func(p *config.PartialConfig, raw string) error {
		items, err := parseConfigStringList(raw)
		if err != nil {
			return fmt.Errorf("invalid value for %s: %v", key, err)
		}
		*target(p) = &items
		return nil
	}, unset: func(p *config.PartialConfig) { *target(p) = nil }}
}

func labelModeField(key, env string, get func(config.Config) any, target func(*config.PartialConfig) **config.LabelMode) configField {
	return configField{key: key, valueType: "string", env: env, get: get, set: func(p *config.PartialConfig, raw string) error {
		mode := config.LabelMode(strings.TrimSpace(raw))
		switch mode {
		case config.LabelModeAll, config.LabelModeAny:
			*target(p) = &mode
			return nil
		default:
			return fmt.Errorf("invalid value for %s: must be one of: %s, %s", key, config.LabelModeAll, config.LabelModeAny)
		}
	}, unset: func(p *config.PartialConfig) { *target(p) = nil }}
}

func fixerAuthorFilterField() configField {
	return configField{key: "roles.fixer.triggers.authorFilter", valueType: "string", env: "LOOPER_ROLES_FIXER_TRIGGERS_AUTHOR_FILTER", get: func(c config.Config) any { return c.Roles.Fixer.Triggers.AuthorFilter }, set: func(p *config.PartialConfig, raw string) error {
		filter := config.FixerAuthorFilter(strings.TrimSpace(raw))
		switch filter {
		case config.FixerAuthorFilterCurrentUser, config.FixerAuthorFilterAny:
			ensurePartialFixerRoleTriggers(p).AuthorFilter = &filter
			return nil
		default:
			return fmt.Errorf("invalid value for roles.fixer.triggers.authorFilter: must be one of: %s, %s", config.FixerAuthorFilterCurrentUser, config.FixerAuthorFilterAny)
		}
	}, unset: func(p *config.PartialConfig) { ensurePartialFixerRoleTriggers(p).AuthorFilter = nil }}
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

func reviewerReviewEventField(key, env, flag string, get func(config.Config) any, target func(*config.PartialConfig) **config.ReviewerReviewEvent) configField {
	return configField{key: key, valueType: "string", env: env, flag: flag, get: get, set: func(p *config.PartialConfig, raw string) error {
		value := config.ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(raw)))
		switch key {
		case "reviewer.reviewEvents.clean":
			if value != config.ReviewerReviewEventComment && value != config.ReviewerReviewEventApprove {
				return fmt.Errorf("invalid value for %s: must be one of: %s, %s", key, config.ReviewerReviewEventComment, config.ReviewerReviewEventApprove)
			}
		case "reviewer.reviewEvents.blocking":
			if value != config.ReviewerReviewEventComment && value != config.ReviewerReviewEventRequestChanges {
				return fmt.Errorf("invalid value for %s: must be one of: %s, %s", key, config.ReviewerReviewEventComment, config.ReviewerReviewEventRequestChanges)
			}
		default:
			return fmt.Errorf("unsupported review event config key %q", key)
		}
		*target(p) = &value
		return nil
	}, unset: func(p *config.PartialConfig) {
		*target(p) = nil
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

func parseConfigStringList(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}, nil
	}
	parts := strings.Split(raw, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			return nil, fmt.Errorf("empty list item")
		}
		items = append(items, item)
	}
	return items, nil
}

func ensurePartialDefaults(partial *config.PartialConfig) *config.PartialDefaultsConfig {
	if partial.Defaults == nil {
		partial.Defaults = &config.PartialDefaultsConfig{}
	}
	return partial.Defaults
}

func ensurePartialReviewerReviewEvents(partial *config.PartialConfig) *config.PartialReviewerReviewEventsConfig {
	if partial.Reviewer == nil {
		partial.Reviewer = &config.PartialReviewerConfig{}
	}
	if partial.Reviewer.ReviewEvents == nil {
		partial.Reviewer.ReviewEvents = &config.PartialReviewerReviewEventsConfig{}
	}
	return partial.Reviewer.ReviewEvents
}

func ensurePartialRoles(partial *config.PartialConfig) *config.PartialRoleConfigs {
	if partial.Roles == nil {
		partial.Roles = &config.PartialRoleConfigs{}
	}
	return partial.Roles
}

func ensurePartialPlannerRole(partial *config.PartialConfig) *config.PartialPlannerRoleConfig {
	roles := ensurePartialRoles(partial)
	if roles.Planner == nil {
		roles.Planner = &config.PartialPlannerRoleConfig{}
	}
	return roles.Planner
}

func ensurePartialPlannerTriggers(partial *config.PartialConfig) *config.PartialIssueRoleTriggersConfig {
	planner := ensurePartialPlannerRole(partial)
	if planner.Triggers == nil {
		planner.Triggers = &config.PartialIssueRoleTriggersConfig{}
	}
	return planner.Triggers
}

func ensurePartialWorkerRole(partial *config.PartialConfig) *config.PartialWorkerRoleConfig {
	roles := ensurePartialRoles(partial)
	if roles.Worker == nil {
		roles.Worker = &config.PartialWorkerRoleConfig{}
	}
	return roles.Worker
}

func ensurePartialWorkerTriggers(partial *config.PartialConfig) *config.PartialIssueRoleTriggersConfig {
	worker := ensurePartialWorkerRole(partial)
	if worker.Triggers == nil {
		worker.Triggers = &config.PartialIssueRoleTriggersConfig{}
	}
	return worker.Triggers
}

func ensurePartialReviewerRole(partial *config.PartialConfig) *config.PartialReviewerRoleConfig {
	roles := ensurePartialRoles(partial)
	if roles.Reviewer == nil {
		roles.Reviewer = &config.PartialReviewerRoleConfig{}
	}
	return roles.Reviewer
}

func ensurePartialReviewerRoleTriggers(partial *config.PartialConfig) *config.PartialReviewerRoleTriggersConfig {
	reviewer := ensurePartialReviewerRole(partial)
	if reviewer.Triggers == nil {
		reviewer.Triggers = &config.PartialReviewerRoleTriggersConfig{}
	}
	return reviewer.Triggers
}

func ensurePartialReviewerSpecReview(partial *config.PartialConfig) *config.PartialReviewerSpecReviewConfig {
	reviewer := ensurePartialReviewerRole(partial)
	if reviewer.SpecReview == nil {
		reviewer.SpecReview = &config.PartialReviewerSpecReviewConfig{}
	}
	return reviewer.SpecReview
}

func ensurePartialFixerRole(partial *config.PartialConfig) *config.PartialFixerRoleConfig {
	roles := ensurePartialRoles(partial)
	if roles.Fixer == nil {
		roles.Fixer = &config.PartialFixerRoleConfig{}
	}
	return roles.Fixer
}

func ensurePartialFixerRoleTriggers(partial *config.PartialConfig) *config.PartialFixerRoleTriggersConfig {
	fixer := ensurePartialFixerRole(partial)
	if fixer.Triggers == nil {
		fixer.Triggers = &config.PartialFixerRoleTriggersConfig{}
	}
	return fixer.Triggers
}

func configFieldSet(partial config.PartialConfig, key string) bool {
	switch key {
	case "defaults.baseBranch":
		if partial.Defaults == nil {
			return false
		}
		return partial.Defaults.BaseBranch != nil
	case "defaults.allowAutoCommit":
		if partial.Defaults == nil {
			return false
		}
		return partial.Defaults.AllowAutoCommit != nil
	case "defaults.allowAutoPush":
		if partial.Defaults == nil {
			return false
		}
		return partial.Defaults.AllowAutoPush != nil
	case "defaults.allowAutoApprove":
		if partial.Defaults == nil {
			return false
		}
		return partial.Defaults.AllowAutoApprove != nil
	case "defaults.allowAutoMerge":
		if partial.Defaults == nil {
			return false
		}
		return partial.Defaults.AllowAutoMerge != nil
	case "defaults.allowRiskyFixes":
		if partial.Defaults == nil {
			return false
		}
		return partial.Defaults.AllowRiskyFixes != nil
	case "defaults.fixAllPullRequests":
		if partial.Defaults == nil {
			return false
		}
		return partial.Defaults.FixAllPullRequests != nil
	case "defaults.openPrStrategy":
		if partial.Defaults == nil {
			return false
		}
		return partial.Defaults.OpenPRStrategy != nil
	case "reviewer.reviewEvents.clean":
		return partial.Reviewer != nil && partial.Reviewer.ReviewEvents != nil && partial.Reviewer.ReviewEvents.Clean != nil
	case "reviewer.reviewEvents.blocking":
		return partial.Reviewer != nil && partial.Reviewer.ReviewEvents != nil && partial.Reviewer.ReviewEvents.Blocking != nil
	case "roles.planner.autoDiscovery":
		return partial.Roles != nil && partial.Roles.Planner != nil && partial.Roles.Planner.AutoDiscovery != nil
	case "roles.planner.triggers.labels":
		return partial.Roles != nil && partial.Roles.Planner != nil && partial.Roles.Planner.Triggers != nil && partial.Roles.Planner.Triggers.Labels != nil
	case "roles.planner.triggers.labelMode":
		return partial.Roles != nil && partial.Roles.Planner != nil && partial.Roles.Planner.Triggers != nil && partial.Roles.Planner.Triggers.LabelMode != nil
	case "roles.planner.triggers.requireAssigneeCurrentUser":
		return partial.Roles != nil && partial.Roles.Planner != nil && partial.Roles.Planner.Triggers != nil && partial.Roles.Planner.Triggers.RequireAssigneeCurrentUser != nil
	case "roles.worker.autoDiscovery":
		return partial.Roles != nil && partial.Roles.Worker != nil && partial.Roles.Worker.AutoDiscovery != nil
	case "roles.worker.triggers.labels":
		return partial.Roles != nil && partial.Roles.Worker != nil && partial.Roles.Worker.Triggers != nil && partial.Roles.Worker.Triggers.Labels != nil
	case "roles.worker.triggers.labelMode":
		return partial.Roles != nil && partial.Roles.Worker != nil && partial.Roles.Worker.Triggers != nil && partial.Roles.Worker.Triggers.LabelMode != nil
	case "roles.worker.triggers.requireAssigneeCurrentUser":
		return partial.Roles != nil && partial.Roles.Worker != nil && partial.Roles.Worker.Triggers != nil && partial.Roles.Worker.Triggers.RequireAssigneeCurrentUser != nil
	case "roles.reviewer.autoDiscovery":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.AutoDiscovery != nil
	case "roles.reviewer.triggers.includeDrafts":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.IncludeDrafts != nil
	case "roles.reviewer.triggers.requireReviewRequest":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.RequireReviewRequest != nil
	case "roles.reviewer.triggers.labels":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.Labels != nil
	case "roles.reviewer.triggers.labelMode":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.LabelMode != nil
	case "roles.reviewer.specReview.includeReviewingLabel":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.SpecReview != nil && partial.Roles.Reviewer.SpecReview.IncludeReviewingLabel != nil
	case "roles.reviewer.specReview.reviewingLabel":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.SpecReview != nil && partial.Roles.Reviewer.SpecReview.ReviewingLabel != nil
	case "roles.fixer.autoDiscovery":
		return partial.Roles != nil && partial.Roles.Fixer != nil && partial.Roles.Fixer.AutoDiscovery != nil
	case "roles.fixer.triggers.includeDrafts":
		return partial.Roles != nil && partial.Roles.Fixer != nil && partial.Roles.Fixer.Triggers != nil && partial.Roles.Fixer.Triggers.IncludeDrafts != nil
	case "roles.fixer.triggers.labels":
		return partial.Roles != nil && partial.Roles.Fixer != nil && partial.Roles.Fixer.Triggers != nil && partial.Roles.Fixer.Triggers.Labels != nil
	case "roles.fixer.triggers.labelMode":
		return partial.Roles != nil && partial.Roles.Fixer != nil && partial.Roles.Fixer.Triggers != nil && partial.Roles.Fixer.Triggers.LabelMode != nil
	case "roles.fixer.triggers.authorFilter":
		return partial.Roles != nil && partial.Roles.Fixer != nil && partial.Roles.Fixer.Triggers != nil && partial.Roles.Fixer.Triggers.AuthorFilter != nil
	default:
		return false
	}
}
