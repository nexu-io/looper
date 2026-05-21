package cliapp

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/spf13/cobra"
)

type configField struct {
	key       string
	valueType string
	env       string
	envAlias  string
	flag      string
	flagAlias string
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
	"instructions.enabled":        boolFieldWithAlias("instructions.enabled", "", "", "instructions-enabled", "no-custom-instructions", func(c config.Config) any { return c.Instructions.Enabled }, func(p *config.PartialConfig) **bool { return &ensurePartialInstructions(p).Enabled }),
	"package.autoUpgradeEnabled":  boolFieldWithAlias("package.autoUpgradeEnabled", "LOOPER_AUTO_UPGRADE_ENABLED", "", "package-auto-upgrade-enabled", "no-auto-upgrade", func(c config.Config) any { return c.Package.AutoUpgradeEnabled }, func(p *config.PartialConfig) **bool { return &ensurePartialPackage(p).AutoUpgradeEnabled }),
	"instructions.maxBytes":       positiveIntField("instructions.maxBytes", "", "", func(c config.Config) any { return c.Instructions.MaxBytes }, func(p *config.PartialConfig) **int { return &ensurePartialInstructions(p).MaxBytes }),
	"roles.reviewer.behavior.reviewEvents.clean": reviewerReviewEventField("roles.reviewer.behavior.reviewEvents.clean", "LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_CLEAN", "LOOPER_REVIEWER_REVIEW_EVENTS_CLEAN", "roles-reviewer-behavior-review-events-clean", "reviewer-clean-review-event", func(c config.Config) any { return c.Roles.Reviewer.Behavior.ReviewEvents.Clean }, func(p *config.PartialConfig) **config.ReviewerReviewEvent {
		return &ensurePartialReviewerReviewEvents(p).Clean
	}),
	"reviewer.reviewEvents.clean": reviewerReviewEventField("reviewer.reviewEvents.clean", "LOOPER_REVIEWER_REVIEW_EVENTS_CLEAN", "LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_CLEAN", "reviewer-clean-review-event", "roles-reviewer-behavior-review-events-clean", func(c config.Config) any { return c.Roles.Reviewer.Behavior.ReviewEvents.Clean }, func(p *config.PartialConfig) **config.ReviewerReviewEvent {
		return &ensurePartialReviewerReviewEvents(p).Clean
	}),
	"roles.reviewer.behavior.reviewEvents.blocking": reviewerReviewEventField("roles.reviewer.behavior.reviewEvents.blocking", "LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_BLOCKING", "LOOPER_REVIEWER_REVIEW_EVENTS_BLOCKING", "roles-reviewer-behavior-review-events-blocking", "reviewer-blocking-review-event", func(c config.Config) any { return c.Roles.Reviewer.Behavior.ReviewEvents.Blocking }, func(p *config.PartialConfig) **config.ReviewerReviewEvent {
		return &ensurePartialReviewerReviewEvents(p).Blocking
	}),
	"reviewer.reviewEvents.blocking": reviewerReviewEventField("reviewer.reviewEvents.blocking", "LOOPER_REVIEWER_REVIEW_EVENTS_BLOCKING", "LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_BLOCKING", "reviewer-blocking-review-event", "roles-reviewer-behavior-review-events-blocking", func(c config.Config) any { return c.Roles.Reviewer.Behavior.ReviewEvents.Blocking }, func(p *config.PartialConfig) **config.ReviewerReviewEvent {
		return &ensurePartialReviewerReviewEvents(p).Blocking
	}),
	"roles.reviewer.behavior.retry.enhancedTransientClassification": boolField("roles.reviewer.behavior.retry.enhancedTransientClassification", "", "", func(c config.Config) any {
		return c.Roles.Reviewer.Behavior.Retry.EnhancedTransientClassification
	}, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRetry(p).EnhancedTransientClassification
	}),
	"roles.reviewer.behavior.retry.extraTransientErrorPatterns": stringListField("roles.reviewer.behavior.retry.extraTransientErrorPatterns", "", func(c config.Config) any {
		return c.Roles.Reviewer.Behavior.Retry.ExtraTransientErrorPatterns
	}, func(p *config.PartialConfig) **[]string {
		return &ensurePartialReviewerRetry(p).ExtraTransientErrorPatterns
	}),
	"roles.reviewer.behavior.retry.recoverExistingMatchedFailures": boolField("roles.reviewer.behavior.retry.recoverExistingMatchedFailures", "", "", func(c config.Config) any {
		return c.Roles.Reviewer.Behavior.Retry.RecoverExistingMatchedFailures
	}, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRetry(p).RecoverExistingMatchedFailures
	}),
	"roles.reviewer.behavior.retry.autoRecoveryMaxAttempts": positiveIntField("roles.reviewer.behavior.retry.autoRecoveryMaxAttempts", "", "", func(c config.Config) any {
		return c.Roles.Reviewer.Behavior.Retry.AutoRecoveryMaxAttempts
	}, func(p *config.PartialConfig) **int {
		return &ensurePartialReviewerRetry(p).AutoRecoveryMaxAttempts
	}),
	"roles.reviewer.behavior.retry.maxDelayMs": positiveIntField("roles.reviewer.behavior.retry.maxDelayMs", "", "", func(c config.Config) any {
		return c.Roles.Reviewer.Behavior.Retry.MaxDelayMS
	}, func(p *config.PartialConfig) **int {
		return &ensurePartialReviewerRetry(p).MaxDelayMS
	}),
	"roles.planner.autoDiscovery":      boolField("roles.planner.autoDiscovery", "LOOPER_ROLES_PLANNER_AUTO_DISCOVERY", "", func(c config.Config) any { return c.Roles.Planner.AutoDiscovery }, func(p *config.PartialConfig) **bool { return &ensurePartialPlannerRole(p).AutoDiscovery }),
	"roles.planner.instructions":       stringField("roles.planner.instructions", "", "", func(c config.Config) any { return c.Roles.Planner.Instructions }, func(p *config.PartialConfig) **string { return &ensurePartialPlannerRole(p).Instructions }),
	"roles.planner.triggers.labels":    stringListField("roles.planner.triggers.labels", "LOOPER_ROLES_PLANNER_TRIGGERS_LABELS", func(c config.Config) any { return c.Roles.Planner.Triggers.Labels }, func(p *config.PartialConfig) **[]string { return &ensurePartialPlannerTriggers(p).Labels }),
	"roles.planner.triggers.labelMode": labelModeField("roles.planner.triggers.labelMode", "LOOPER_ROLES_PLANNER_TRIGGERS_LABEL_MODE", func(c config.Config) any { return c.Roles.Planner.Triggers.LabelMode }, func(p *config.PartialConfig) **config.LabelMode { return &ensurePartialPlannerTriggers(p).LabelMode }),
	"roles.planner.triggers.requireAssigneeCurrentUser": boolField("roles.planner.triggers.requireAssigneeCurrentUser", "LOOPER_ROLES_PLANNER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER", "", func(c config.Config) any { return c.Roles.Planner.Triggers.RequireAssigneeCurrentUser }, func(p *config.PartialConfig) **bool {
		return &ensurePartialPlannerTriggers(p).RequireAssigneeCurrentUser
	}),
	"roles.worker.autoDiscovery":      boolField("roles.worker.autoDiscovery", "LOOPER_ROLES_WORKER_AUTO_DISCOVERY", "", func(c config.Config) any { return c.Roles.Worker.AutoDiscovery }, func(p *config.PartialConfig) **bool { return &ensurePartialWorkerRole(p).AutoDiscovery }),
	"roles.worker.instructions":       stringField("roles.worker.instructions", "", "", func(c config.Config) any { return c.Roles.Worker.Instructions }, func(p *config.PartialConfig) **string { return &ensurePartialWorkerRole(p).Instructions }),
	"roles.worker.triggers.labels":    stringListField("roles.worker.triggers.labels", "LOOPER_ROLES_WORKER_TRIGGERS_LABELS", func(c config.Config) any { return c.Roles.Worker.Triggers.Labels }, func(p *config.PartialConfig) **[]string { return &ensurePartialWorkerTriggers(p).Labels }),
	"roles.worker.triggers.labelMode": labelModeField("roles.worker.triggers.labelMode", "LOOPER_ROLES_WORKER_TRIGGERS_LABEL_MODE", func(c config.Config) any { return c.Roles.Worker.Triggers.LabelMode }, func(p *config.PartialConfig) **config.LabelMode { return &ensurePartialWorkerTriggers(p).LabelMode }),
	"roles.worker.triggers.requireAssigneeCurrentUser": boolField("roles.worker.triggers.requireAssigneeCurrentUser", "LOOPER_ROLES_WORKER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER", "", func(c config.Config) any { return c.Roles.Worker.Triggers.RequireAssigneeCurrentUser }, func(p *config.PartialConfig) **bool {
		return &ensurePartialWorkerTriggers(p).RequireAssigneeCurrentUser
	}),
	"roles.reviewer.discovery.autoDiscovery": reviewerDiscoveryBoolFieldWithAlias("roles.reviewer.discovery.autoDiscovery", "LOOPER_ROLES_REVIEWER_DISCOVERY_AUTO_DISCOVERY", "LOOPER_ROLES_REVIEWER_AUTO_DISCOVERY", "", "", func(c config.Config) any { return c.Roles.Reviewer.Discovery.AutoDiscovery }, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleDiscovery(p).AutoDiscovery
	}, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRole(p).AutoDiscovery
	}),
	"roles.reviewer.autoMerge.enabled": boolFieldWithAlias("roles.reviewer.autoMerge.enabled", "", "", "roles-reviewer-auto-merge-enabled", "reviewer-auto-merge-enabled", func(c config.Config) any { return c.Roles.Reviewer.AutoMerge.Enabled }, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerAutoMerge(p).Enabled
	}),
	"roles.reviewer.autoMerge.strategy": reviewerAutoMergeStrategyField(),
	"roles.reviewer.autoMerge.requireBranchProtection": boolFieldWithAlias("roles.reviewer.autoMerge.requireBranchProtection", "", "", "roles-reviewer-auto-merge-require-branch-protection", "reviewer-auto-merge-require-branch-protection", func(c config.Config) any { return c.Roles.Reviewer.AutoMerge.RequireBranchProtection }, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerAutoMerge(p).RequireBranchProtection
	}),
	"roles.reviewer.autoMerge.transientRetries": positiveIntField("roles.reviewer.autoMerge.transientRetries", "", "roles-reviewer-auto-merge-transient-retries", func(c config.Config) any { return c.Roles.Reviewer.AutoMerge.TransientRetries }, func(p *config.PartialConfig) **int {
		return &ensurePartialReviewerAutoMerge(p).TransientRetries
	}),
	"roles.reviewer.autoMerge.scope": reviewerAutoMergeScopeField(),
	"roles.reviewer.autoDiscovery": reviewerDiscoveryBoolFieldWithAlias("roles.reviewer.autoDiscovery", "LOOPER_ROLES_REVIEWER_AUTO_DISCOVERY", "LOOPER_ROLES_REVIEWER_DISCOVERY_AUTO_DISCOVERY", "", "", func(c config.Config) any { return c.Roles.Reviewer.Discovery.AutoDiscovery }, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleDiscovery(p).AutoDiscovery
	}, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRole(p).AutoDiscovery
	}),
	"roles.reviewer.instructions": stringField("roles.reviewer.instructions", "", "", func(c config.Config) any { return c.Roles.Reviewer.Instructions }, func(p *config.PartialConfig) **string { return &ensurePartialReviewerRole(p).Instructions }),
	"roles.reviewer.discovery.triggers.includeDrafts": reviewerDiscoveryBoolFieldWithAlias("roles.reviewer.discovery.triggers.includeDrafts", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_INCLUDE_DRAFTS", "LOOPER_ROLES_REVIEWER_TRIGGERS_INCLUDE_DRAFTS", "", "", func(c config.Config) any { return c.Roles.Reviewer.Discovery.Triggers.IncludeDrafts }, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleDiscoveryTriggers(p).IncludeDrafts
	}, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleTriggers(p).IncludeDrafts
	}),
	"roles.reviewer.triggers.includeDrafts": reviewerDiscoveryBoolFieldWithAlias("roles.reviewer.triggers.includeDrafts", "LOOPER_ROLES_REVIEWER_TRIGGERS_INCLUDE_DRAFTS", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_INCLUDE_DRAFTS", "", "", func(c config.Config) any { return c.Roles.Reviewer.Discovery.Triggers.IncludeDrafts }, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleDiscoveryTriggers(p).IncludeDrafts
	}, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleTriggers(p).IncludeDrafts
	}),
	"roles.reviewer.discovery.triggers.requireReviewRequest": reviewerDiscoveryBoolFieldWithAlias("roles.reviewer.discovery.triggers.requireReviewRequest", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_REQUIRE_REVIEW_REQUEST", "LOOPER_ROLES_REVIEWER_TRIGGERS_REQUIRE_REVIEW_REQUEST", "", "", func(c config.Config) any { return c.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest }, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleDiscoveryTriggers(p).RequireReviewRequest
	}, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleTriggers(p).RequireReviewRequest
	}),
	"roles.reviewer.triggers.requireReviewRequest": reviewerDiscoveryBoolFieldWithAlias("roles.reviewer.triggers.requireReviewRequest", "LOOPER_ROLES_REVIEWER_TRIGGERS_REQUIRE_REVIEW_REQUEST", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_REQUIRE_REVIEW_REQUEST", "", "", func(c config.Config) any { return c.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest }, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleDiscoveryTriggers(p).RequireReviewRequest
	}, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleTriggers(p).RequireReviewRequest
	}),
	"roles.reviewer.discovery.triggers.enableSelfReview": reviewerDiscoveryBoolFieldWithAlias("roles.reviewer.discovery.triggers.enableSelfReview", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_ENABLE_SELF_REVIEW", "LOOPER_ROLES_REVIEWER_TRIGGERS_ENABLE_SELF_REVIEW", "roles-reviewer-discovery-triggers-enable-self-review", "reviewer-enable-self-review", func(c config.Config) any { return c.Roles.Reviewer.Discovery.Triggers.EnableSelfReview }, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleDiscoveryTriggers(p).EnableSelfReview
	}, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleTriggers(p).EnableSelfReview
	}),
	"roles.reviewer.triggers.enableSelfReview": reviewerDiscoveryBoolFieldWithAlias("roles.reviewer.triggers.enableSelfReview", "LOOPER_ROLES_REVIEWER_TRIGGERS_ENABLE_SELF_REVIEW", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_ENABLE_SELF_REVIEW", "reviewer-enable-self-review", "roles-reviewer-discovery-triggers-enable-self-review", func(c config.Config) any { return c.Roles.Reviewer.Discovery.Triggers.EnableSelfReview }, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleDiscoveryTriggers(p).EnableSelfReview
	}, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleTriggers(p).EnableSelfReview
	}),
	"roles.reviewer.discovery.triggers.labels": reviewerDiscoveryStringListFieldWithAlias("roles.reviewer.discovery.triggers.labels", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_LABELS", "LOOPER_ROLES_REVIEWER_TRIGGERS_LABELS", func(c config.Config) any { return c.Roles.Reviewer.Discovery.Triggers.Labels }, func(p *config.PartialConfig) **[]string {
		return &ensurePartialReviewerRoleDiscoveryTriggers(p).Labels
	}, func(p *config.PartialConfig) **[]string {
		return &ensurePartialReviewerRoleTriggers(p).Labels
	}),
	"roles.reviewer.triggers.labels": reviewerDiscoveryStringListFieldWithAlias("roles.reviewer.triggers.labels", "LOOPER_ROLES_REVIEWER_TRIGGERS_LABELS", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_LABELS", func(c config.Config) any { return c.Roles.Reviewer.Discovery.Triggers.Labels }, func(p *config.PartialConfig) **[]string {
		return &ensurePartialReviewerRoleDiscoveryTriggers(p).Labels
	}, func(p *config.PartialConfig) **[]string {
		return &ensurePartialReviewerRoleTriggers(p).Labels
	}),
	"roles.reviewer.discovery.triggers.labelMode": reviewerDiscoveryLabelModeFieldWithAlias("roles.reviewer.discovery.triggers.labelMode", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_LABEL_MODE", "LOOPER_ROLES_REVIEWER_TRIGGERS_LABEL_MODE", func(c config.Config) any { return c.Roles.Reviewer.Discovery.Triggers.LabelMode }, func(p *config.PartialConfig) **config.LabelMode {
		return &ensurePartialReviewerRoleDiscoveryTriggers(p).LabelMode
	}, func(p *config.PartialConfig) **config.LabelMode {
		return &ensurePartialReviewerRoleTriggers(p).LabelMode
	}),
	"roles.reviewer.triggers.labelMode": reviewerDiscoveryLabelModeFieldWithAlias("roles.reviewer.triggers.labelMode", "LOOPER_ROLES_REVIEWER_TRIGGERS_LABEL_MODE", "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_LABEL_MODE", func(c config.Config) any { return c.Roles.Reviewer.Discovery.Triggers.LabelMode }, func(p *config.PartialConfig) **config.LabelMode {
		return &ensurePartialReviewerRoleDiscoveryTriggers(p).LabelMode
	}, func(p *config.PartialConfig) **config.LabelMode {
		return &ensurePartialReviewerRoleTriggers(p).LabelMode
	}),
	"roles.reviewer.discovery.specReview.includeReviewingLabel": reviewerDiscoveryBoolFieldWithAlias("roles.reviewer.discovery.specReview.includeReviewingLabel", "LOOPER_ROLES_REVIEWER_DISCOVERY_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL", "LOOPER_ROLES_REVIEWER_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL", "", "", func(c config.Config) any { return c.Roles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel }, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleDiscoverySpecReview(p).IncludeReviewingLabel
	}, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerSpecReview(p).IncludeReviewingLabel
	}),
	"roles.reviewer.specReview.includeReviewingLabel": reviewerDiscoveryBoolFieldWithAlias("roles.reviewer.specReview.includeReviewingLabel", "LOOPER_ROLES_REVIEWER_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL", "LOOPER_ROLES_REVIEWER_DISCOVERY_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL", "", "", func(c config.Config) any { return c.Roles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel }, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerRoleDiscoverySpecReview(p).IncludeReviewingLabel
	}, func(p *config.PartialConfig) **bool {
		return &ensurePartialReviewerSpecReview(p).IncludeReviewingLabel
	}),
	"roles.reviewer.discovery.specReview.reviewingLabel": reviewerDiscoveryStringFieldWithAlias("roles.reviewer.discovery.specReview.reviewingLabel", "LOOPER_ROLES_REVIEWER_DISCOVERY_SPEC_REVIEW_REVIEWING_LABEL", "LOOPER_ROLES_REVIEWER_SPEC_REVIEW_REVIEWING_LABEL", "", "", func(c config.Config) any { return c.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel }, func(p *config.PartialConfig) **string {
		return &ensurePartialReviewerRoleDiscoverySpecReview(p).ReviewingLabel
	}, func(p *config.PartialConfig) **string {
		return &ensurePartialReviewerSpecReview(p).ReviewingLabel
	}),
	"roles.reviewer.specReview.reviewingLabel": reviewerDiscoveryStringFieldWithAlias("roles.reviewer.specReview.reviewingLabel", "LOOPER_ROLES_REVIEWER_SPEC_REVIEW_REVIEWING_LABEL", "LOOPER_ROLES_REVIEWER_DISCOVERY_SPEC_REVIEW_REVIEWING_LABEL", "", "", func(c config.Config) any { return c.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel }, func(p *config.PartialConfig) **string {
		return &ensurePartialReviewerRoleDiscoverySpecReview(p).ReviewingLabel
	}, func(p *config.PartialConfig) **string {
		return &ensurePartialReviewerSpecReview(p).ReviewingLabel
	}),
	"roles.fixer.autoDiscovery":          boolField("roles.fixer.autoDiscovery", "LOOPER_ROLES_FIXER_AUTO_DISCOVERY", "", func(c config.Config) any { return c.Roles.Fixer.AutoDiscovery }, func(p *config.PartialConfig) **bool { return &ensurePartialFixerRole(p).AutoDiscovery }),
	"roles.fixer.instructions":           stringField("roles.fixer.instructions", "", "", func(c config.Config) any { return c.Roles.Fixer.Instructions }, func(p *config.PartialConfig) **string { return &ensurePartialFixerRole(p).Instructions }),
	"roles.fixer.triggers.includeDrafts": boolField("roles.fixer.triggers.includeDrafts", "LOOPER_ROLES_FIXER_TRIGGERS_INCLUDE_DRAFTS", "", func(c config.Config) any { return c.Roles.Fixer.Triggers.IncludeDrafts }, func(p *config.PartialConfig) **bool { return &ensurePartialFixerRoleTriggers(p).IncludeDrafts }),
	"roles.fixer.triggers.labels":        stringListField("roles.fixer.triggers.labels", "LOOPER_ROLES_FIXER_TRIGGERS_LABELS", func(c config.Config) any { return c.Roles.Fixer.Triggers.Labels }, func(p *config.PartialConfig) **[]string { return &ensurePartialFixerRoleTriggers(p).Labels }),
	"roles.fixer.triggers.labelMode":     labelModeField("roles.fixer.triggers.labelMode", "LOOPER_ROLES_FIXER_TRIGGERS_LABEL_MODE", func(c config.Config) any { return c.Roles.Fixer.Triggers.LabelMode }, func(p *config.PartialConfig) **config.LabelMode { return &ensurePartialFixerRoleTriggers(p).LabelMode }),
	"roles.fixer.triggers.authorFilter":  fixerAuthorFilterField(),
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
		if field.overrideFromEnv() {
			source = "env"
		}
		if field.overrideFromFlag(cmd) {
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
		if err := r.writeDefaultConfigTemplate(loaded.Metadata.ConfigPath, loaded.Config); err != nil {
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

type configMigrationPlan struct {
	From          string
	To            string
	DryRun        bool
	Force         bool
	Overwrites    bool
	Changes       []string
	Preview       string
	SourcePresent bool
}

func (r *commandRuntime) configMigrate(cmd *cobra.Command, args []string) error {
	plan, err := r.buildConfigMigrationPlan(cmd)
	if err != nil {
		return err
	}

	if getBoolFlag(cmd, "json") {
		payload := map[string]any{
			"from":       plan.From,
			"to":         plan.To,
			"dryRun":     plan.DryRun,
			"force":      plan.Force,
			"overwrites": plan.Overwrites,
			"changes":    plan.Changes,
			"preview":    plan.Preview,
		}
		if plan.DryRun {
			if err := r.preflightMigratedConfigWrite(plan.To, plan.Preview, plan.Overwrites); err != nil {
				return err
			}
			payload["updated"] = false
			return writeJSON(cmd.OutOrStdout(), payload)
		}
		backupPath, err := r.writeMigratedConfig(plan.To, plan.Preview, plan.Overwrites)
		if err != nil {
			return err
		}
		payload["updated"] = true
		payload["sourcePreserved"] = true
		if backupPath != "" {
			payload["backupPath"] = backupPath
		}
		return writeJSON(cmd.OutOrStdout(), payload)
	}

	if plan.DryRun {
		if err := r.preflightMigratedConfigWrite(plan.To, plan.Preview, plan.Overwrites); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Preview migration: %s -> %s\n", plan.From, plan.To); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Dry run: would migrate config %s -> %s\n", plan.From, plan.To); err != nil {
			return err
		}
		if err := writeMigrationChangeSummary(cmd, plan.Changes); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "\n--- canonical preview ---\n%s", plan.Preview)
		return err
	}

	backupPath, err := r.writeMigratedConfig(plan.To, plan.Preview, plan.Overwrites)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Migrated config %s -> %s\n", plan.From, plan.To); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Migrated config: %s -> %s\n", plan.From, plan.To); err != nil {
		return err
	}
	if err := writeMigrationChangeSummary(cmd, plan.Changes); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Source preserved at %s\n", plan.From); err != nil {
		return err
	}
	if backupPath != "" {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Backup created at %s\n", backupPath)
		return err
	}
	return nil
}

func (r *commandRuntime) buildConfigMigrationPlan(cmd *cobra.Command) (configMigrationPlan, error) {
	plan, err := r.resolveConfigMigrationPlan(cmd)
	if err != nil {
		return configMigrationPlan{}, err
	}
	partial, present, err := config.ReadPartialConfigFile(plan.From)
	if err != nil {
		return configMigrationPlan{}, err
	}
	if !present {
		return configMigrationPlan{}, fmt.Errorf("source config file not found: %s", plan.From)
	}
	plan.SourcePresent = true
	plan.Changes = detectConfigMigrationChanges(partial, plan.From, plan.To)
	if err := r.validatePartialConfig(partial); err != nil {
		return configMigrationPlan{}, err
	}
	canonicalPartial := config.CanonicalizePartialForMigration(partial)
	if err := r.validatePartialConfig(canonicalPartial); err != nil {
		return configMigrationPlan{}, err
	}
	preview, err := marshalCanonicalMigrationPreview(plan.To, canonicalPartial)
	if err != nil {
		return configMigrationPlan{}, err
	}
	plan.Preview = preview
	return plan, nil
}

func (r *commandRuntime) resolveConfigMigrationPlan(cmd *cobra.Command) (configMigrationPlan, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return configMigrationPlan{}, fmt.Errorf("determine current working directory: %w", err)
	}
	fromFlag := strings.TrimSpace(getStringFlag(cmd, "from"))
	toFlag := strings.TrimSpace(getStringFlag(cmd, "to"))
	from := ""
	if fromFlag != "" {
		from = config.ResolveConfigPath(fromFlag, cwd)
	} else {
		looperHome, homeErr := config.DefaultLooperHome()
		if homeErr != nil {
			return configMigrationPlan{}, fmt.Errorf("determine looper home: %w", homeErr)
		}
		defaultFrom := filepath.Join(looperHome, "config.json")
		if hasExplicitConfigPath(r.argv) || os.Getenv("LOOPER_CONFIG") != "" {
			from, err = resolveConfigPathFromArgs(r.argv, cwd)
			if err != nil {
				return configMigrationPlan{}, err
			}
		} else {
			from = defaultFrom
		}
	}
	to := ""
	if toFlag != "" {
		to = config.ResolveConfigPath(toFlag, cwd)
	} else {
		to = strings.TrimSuffix(from, filepath.Ext(from)) + ".toml"
	}
	fromCanonical, err := canonicalizeConfigMigrationPath(from)
	if err != nil {
		return configMigrationPlan{}, err
	}
	toCanonical, err := canonicalizeConfigMigrationPath(to)
	if err != nil {
		return configMigrationPlan{}, err
	}
	if fromCanonical == toCanonical {
		return configMigrationPlan{}, fmt.Errorf("source and destination must differ; source and destination config paths must differ; use --to to choose a different destination")
	}
	sameFile, err := pathsReferToSameFile(fromCanonical, toCanonical)
	if err != nil {
		return configMigrationPlan{}, err
	}
	if sameFile {
		return configMigrationPlan{}, fmt.Errorf("source and destination must differ")
	}
	if _, statErr := os.Stat(from); statErr != nil {
		if os.IsNotExist(statErr) {
			return configMigrationPlan{}, fmt.Errorf("source config file not found: %s", from)
		}
		return configMigrationPlan{}, fmt.Errorf("check source config file at %s: %w", from, statErr)
	}
	overwrites := false
	if info, lstatErr := os.Lstat(to); lstatErr == nil {
		if info.IsDir() {
			return configMigrationPlan{}, fmt.Errorf("destination config path points to a directory: %s; use --to to choose a file path", to)
		}
		overwrites = true
	} else if !os.IsNotExist(lstatErr) {
		return configMigrationPlan{}, fmt.Errorf("check destination config file at %s: %w", to, lstatErr)
	}
	if !strings.EqualFold(filepath.Ext(to), ".toml") {
		return configMigrationPlan{}, fmt.Errorf("destination config must use .toml extension: %s", to)
	}
	force := getBoolFlag(cmd, "force")
	if overwrites && !force && !getBoolFlag(cmd, "dry-run") {
		if fromFlag == "" && toFlag == "" && !hasExplicitConfigPath(r.argv) && os.Getenv("LOOPER_CONFIG") == "" {
			return configMigrationPlan{}, fmt.Errorf("default migration target already exists at %s; rerun with --force to overwrite it after creating a backup, or set --to to choose another path", to)
		}
		return configMigrationPlan{}, fmt.Errorf("destination already exists at %s; destination config file already exists there; rerun with --force to overwrite it after creating a backup, or set --to to choose another path", to)
	}
	return configMigrationPlan{
		From:       from,
		To:         to,
		DryRun:     getBoolFlag(cmd, "dry-run"),
		Force:      force,
		Overwrites: overwrites,
	}, nil
}

func marshalCanonicalMigrationPreview(path string, partial config.PartialConfig) (string, error) {
	raw, err := config.MarshalConfigFile(path, pruneEmptyMaps(normalizeConfigMigrationValue(partial)))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func normalizeConfigMigrationValue(partial config.PartialConfig) map[string]any {
	raw, err := json.Marshal(partial)
	if err != nil {
		return map[string]any{}
	}
	var normalized map[string]any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return map[string]any{}
	}
	return normalized
}

func (r *commandRuntime) prepareMigratedConfigFile(path string, preview string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}
	return r.prepareMigratedConfigValidationFile(filepath.Dir(path), filepath.Ext(path), preview)
}

func (r *commandRuntime) prepareMigratedConfigValidationFile(dir string, ext string, preview string) (string, error) {
	tmpPattern := ".config-*" + ext
	if ext == "" {
		tmpPattern = ".config-*.tmp"
	}
	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return "", fmt.Errorf("create temporary config: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(preview); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close temporary config: %w", err)
	}
	if err := r.validateMigratedConfigFile(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

func (r *commandRuntime) preflightMigratedConfigWrite(path string, preview string, overwrites bool) error {
	if err := preflightMigratedConfigDirectory(path); err != nil {
		return err
	}
	tmpPath, err := r.prepareMigratedConfigValidationFile(os.TempDir(), filepath.Ext(path), preview)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	if !overwrites {
		return nil
	}
	backupPath, err := backupConfigFileWithPath(path)
	if err != nil {
		return err
	}
	if backupPath != "" {
		if err := os.Remove(backupPath); err != nil {
			return fmt.Errorf("remove config backup created during dry-run: %w", err)
		}
	}
	return nil
}

func preflightMigratedConfigDirectory(path string) error {
	anchor := filepath.Dir(path)
	for {
		info, err := os.Stat(anchor)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("create config directory: %w", syscall.ENOTDIR)
			}
			if err := syscall.Access(anchor, 0x2); err != nil {
				return fmt.Errorf("create config directory: %w", err)
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("create config directory: %w", err)
		}
		parent := filepath.Dir(anchor)
		if parent == anchor {
			return fmt.Errorf("create config directory: no existing parent directory found for %s", path)
		}
		anchor = parent
	}
}

func pruneEmptyMaps(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		pruned := make(map[string]any, len(typed))
		for key, item := range typed {
			next := pruneEmptyMaps(item)
			if next == nil {
				continue
			}
			pruned[key] = next
		}
		if len(pruned) == 0 {
			return nil
		}
		return pruned
	case []any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			next := pruneEmptyMaps(item)
			if next == nil {
				continue
			}
			items = append(items, next)
		}
		return items
	default:
		return value
	}
}

type configWriteFile interface {
	WriteString(s string) (n int, err error)
	Close() error
}

var openExclusiveConfigWriteFile = func(path string) (configWriteFile, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
}

func (r *commandRuntime) writeMigratedConfig(path string, preview string, overwrites bool) (string, error) {
	tmpPath, err := r.prepareMigratedConfigFile(path, preview)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmpPath)
	if !overwrites {
		file, err := openExclusiveConfigWriteFile(path)
		if err != nil {
			if os.IsExist(err) {
				return "", fmt.Errorf("destination already exists at %s; destination config file already exists there; rerun with --force to overwrite it after creating a backup, or set --to to choose another path", path)
			}
			return "", fmt.Errorf("create config without overwrite: %w", err)
		}
		if _, err := file.WriteString(preview); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return "", fmt.Errorf("create config without overwrite: %w", err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(path)
			return "", fmt.Errorf("create config without overwrite: %w", err)
		}
		return "", nil
	}
	backupPath := ""
	if overwrites {
		backupPath, err = backupConfigFileWithPath(path)
		if err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("replace config: %w", err)
	}
	return backupPath, nil
}

func writeMigrationChangeSummary(cmd *cobra.Command, changes []string) error {
	if len(changes) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "Changes: none; source already uses canonical schema/format")
		return err
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Changes:"); err != nil {
		return err
	}
	for _, change := range changes {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "- %s\n", change); err != nil {
			return err
		}
	}
	return nil
}

func detectConfigMigrationChanges(partial config.PartialConfig, from string, to string) []string {
	changes := make([]string, 0, 8)
	fromExt := strings.ToLower(filepath.Ext(from))
	toExt := strings.ToLower(filepath.Ext(to))
	if fromExt != toExt {
		changes = append(changes, fmt.Sprintf("rewrite config format from %s to %s", strings.TrimPrefix(fromExt, "."), strings.TrimPrefix(toExt, ".")))
	}
	if partial.LegacyReviewer != nil {
		changes = append(changes, "move legacy top-level reviewer.* settings to roles.reviewer.behavior.*")
	}
	if partial.Defaults != nil && partial.Defaults.AllowAutoApprove != nil {
		changes = append(changes, "replace defaults.allowAutoApprove with roles.reviewer.behavior.reviewEvents.clean")
		changes = append(changes, "defaults.allowAutoApprove -> roles.reviewer.behavior.reviewEvents.clean")
	}
	if partial.Defaults != nil && partial.Defaults.FixAllPullRequests != nil {
		changes = append(changes, "replace defaults.fixAllPullRequests with roles.fixer.triggers.authorFilter")
		changes = append(changes, "defaults.fixAllPullRequests -> roles.fixer.triggers.authorFilter")
	}
	if hasLegacyReviewerDiscoveryAliases(partial.Roles) {
		changes = append(changes, "move legacy reviewer discovery aliases to roles.reviewer.discovery.*")
	}
	if partial.Projects != nil {
		for index, project := range *partial.Projects {
			if project.Path != "" {
				changes = append(changes, "replace projects[].path with projects[].repoPath")
				changes = append(changes, fmt.Sprintf("projects[%d].path -> projects[%d].repoPath", index, index))
				break
			}
		}
		hasProjectInstructions := false
		hasProjectReviewerAliases := false
		for index, project := range *partial.Projects {
			if len(project.Instructions) > 0 {
				hasProjectInstructions = true
				changes = append(changes, fmt.Sprintf("projects[%d].instructions -> projects[%d].roles.<role>.instructions", index, index))
			}
			if project.Roles != nil && hasLegacyReviewerDiscoveryAliases(project.Roles) {
				hasProjectReviewerAliases = true
			}
		}
		if hasProjectInstructions {
			changes = append(changes, "move projects[].instructions.<role> entries to projects[].roles.<role>.instructions")
		}
		if hasProjectReviewerAliases {
			changes = append(changes, "move legacy project reviewer discovery aliases to projects[].roles.reviewer.discovery.*")
		}
	}
	return changes
}

func hasLegacyReviewerDiscoveryAliases(roles *config.PartialRoleConfigs) bool {
	if roles == nil || roles.Reviewer == nil {
		return false
	}
	reviewer := roles.Reviewer
	return reviewer.AutoDiscovery != nil || reviewer.Triggers != nil || reviewer.SpecReview != nil
}

func (r *commandRuntime) loadConfigForEdit() (config.LoadedFileConfig, error) {
	loaded, err := config.LoadFile(config.LoadFileOptions{Args: ExtractConfigArgs(r.argv), LookPath: r.lookPath()})
	if err != nil {
		return config.LoadedFileConfig{}, err
	}
	r.emitConfigLoadNotices(r.filterConfigLoadNoticesForCommand(loaded))
	return loaded, nil
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
	partial, present, err := config.ReadPartialConfigFile(configPath)
	if err != nil {
		return config.LoadedFileConfig{}, err
	}
	if !present {
		full, normErr := config.Normalize(cwd)
		if normErr != nil {
			return config.LoadedFileConfig{}, normErr
		}
		return config.LoadedFileConfig{Config: full, Partial: config.PartialConfig{}, Metadata: config.LoadFileMetadata{ConfigPath: configPath, ConfigFilePresent: false}}, nil
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
	defaultPath, err := config.DiscoverDefaultConfigPath()
	if err != nil {
		return "", fmt.Errorf("determine default config path: %w", err)
	}
	return config.ResolveConfigPath(defaultPath, cwd), nil
}

func hasExplicitConfigPath(argv []string) bool {
	args := ExtractConfigArgs(argv)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--config" {
			return true
		}
		if strings.HasPrefix(arg, "--config=") {
			return true
		}
	}
	return false
}

func (r *commandRuntime) writeConfigFile(path string, partial config.PartialConfig) error {
	if err := r.validatePartialConfig(partial); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	raw, err := config.MarshalConfigFile(path, partial)
	if err != nil {
		return err
	}
	tmpPattern := ".config-*" + filepath.Ext(path)
	if filepath.Ext(path) == "" {
		tmpPattern = ".config-*.tmp"
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), tmpPattern)
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

func (r *commandRuntime) writeDefaultConfigTemplate(path string, cfg config.Config) error {
	if err := config.Validate(cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	templateValue, err := canonicalConfigTemplateValue(cfg)
	if err != nil {
		return err
	}
	raw, err := config.MarshalConfigFile(path, templateValue)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func canonicalConfigTemplateValue(cfg config.Config) (map[string]any, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("normalize config template: %w", err)
	}
	var normalized map[string]any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return nil, fmt.Errorf("normalize config template: %w", err)
	}
	defaults, _ := normalized["defaults"].(map[string]any)
	delete(defaults, "allowAutoApprove")
	delete(defaults, "fixAllPullRequests")
	return normalized, nil
}

func (r *commandRuntime) validateConfigFile(path string) error {
	_, err := config.LoadFile(config.LoadFileOptions{Args: []string{"--config", path}, LookPath: r.lookPath()})
	return err
}

func (r *commandRuntime) validateMigratedConfigFile(path string) error {
	_, err := config.LoadFile(config.LoadFileOptions{
		Args:      []string{"--config", path},
		LookupEnv: func(string) (string, bool) { return "", false },
		LookPath:  r.lookPath(),
	})
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
	_, err := backupConfigFileWithPath(path)
	return err
}

var backupConfigFileWithPath = func(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read config for backup: %w", err)
	}
	backupPath := fmt.Sprintf("%s.%s.bak", path, time.Now().UTC().Format("20060102150405.000000000"))
	if info.Mode()&os.ModeSymlink != 0 {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				target, linkErr := os.Readlink(path)
				if linkErr != nil {
					return "", fmt.Errorf("read config symlink for backup: %w", linkErr)
				}
				if err := os.Symlink(target, backupPath); err != nil {
					return "", fmt.Errorf("write config backup: %w", err)
				}
				return backupPath, nil
			}
			return "", fmt.Errorf("read config for backup: %w", readErr)
		}
		if err := os.WriteFile(backupPath, raw, 0o600); err != nil {
			return "", fmt.Errorf("write config backup: %w", err)
		}
		return backupPath, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read config for backup: %w", err)
	}
	if err := os.WriteFile(backupPath, raw, 0o600); err != nil {
		return "", fmt.Errorf("write config backup: %w", err)
	}
	return backupPath, nil
}

func canonicalizeConfigMigrationPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config migration path %q: %w", path, err)
	}
	canonical := filepath.Clean(abs)
	resolved, err := filepath.EvalSymlinks(canonical)
	if err != nil {
		if os.IsNotExist(err) {
			return canonical, nil
		}
		return "", fmt.Errorf("resolve config migration path %q: %w", path, err)
	}
	return filepath.Clean(resolved), nil
}

func pathsReferToSameFile(pathA string, pathB string) (bool, error) {
	infoA, err := os.Stat(pathA)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat config migration path %q: %w", pathA, err)
	}
	infoB, err := os.Stat(pathB)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat config migration path %q: %w", pathB, err)
	}
	return os.SameFile(infoA, infoB), nil
}

func (r *commandRuntime) warnConfigOverrides(cmd *cobra.Command, field configField) {
	if env := field.activeOverrideEnv(); env != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s is set, so %s from the config file may not take effect\n", env, field.key)
	}
	if flag := field.activeOverrideFlag(cmd); flag != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: --%s is set, so %s from the config file may not take effect\n", flag, field.key)
	}
}

func (f configField) activeOverrideEnv() string {
	if f.env != "" {
		if _, ok := os.LookupEnv(f.env); ok {
			return f.env
		}
	}
	if f.envAlias != "" {
		if _, ok := os.LookupEnv(f.envAlias); ok {
			return f.envAlias
		}
	}
	return ""
}

func (f configField) overrideFromEnv() bool {
	return f.activeOverrideEnv() != ""
}

func (f configField) activeOverrideFlag(cmd *cobra.Command) string {
	if f.flag != "" && commandFlagChanged(cmd, f.flag) {
		return f.flag
	}
	if f.flagAlias != "" && commandFlagChanged(cmd, f.flagAlias) {
		return f.flagAlias
	}
	return ""
}

func (f configField) overrideFromFlag(cmd *cobra.Command) bool {
	return f.activeOverrideFlag(cmd) != ""
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
	return boolFieldWithAlias(key, env, "", flag, "", get, target)
}

func boolFieldWithAlias(key, env, envAlias, flag, flagAlias string, get func(config.Config) any, target func(*config.PartialConfig) **bool) configField {
	return configField{key: key, valueType: "boolean", env: env, flag: flag, get: get, set: func(p *config.PartialConfig, raw string) error {
		value, err := parseConfigBool(raw)
		if err != nil {
			return fmt.Errorf("invalid value for %s: %q is not a boolean (use true or false)", key, raw)
		}
		*target(p) = &value
		return nil
	}, unset: func(p *config.PartialConfig) {
		*target(p) = nil
	}, envAlias: envAlias, flagAlias: flagAlias}
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

func reviewerDiscoveryBoolField(key, env, flag string, get func(config.Config) any, canonicalTarget func(*config.PartialConfig) **bool, legacyTarget func(*config.PartialConfig) **bool) configField {
	return reviewerDiscoveryBoolFieldWithAlias(key, env, "", flag, "", get, canonicalTarget, legacyTarget)
}

func reviewerDiscoveryBoolFieldWithAlias(key, env, envAlias, flag, flagAlias string, get func(config.Config) any, canonicalTarget func(*config.PartialConfig) **bool, legacyTarget func(*config.PartialConfig) **bool) configField {
	return configField{key: key, valueType: "boolean", env: env, envAlias: envAlias, flag: flag, flagAlias: flagAlias, get: get, set: func(p *config.PartialConfig, raw string) error {
		value, err := parseConfigBool(raw)
		if err != nil {
			return fmt.Errorf("invalid value for %s: %q is not a boolean (use true or false)", key, raw)
		}
		*canonicalTarget(p) = &value
		if legacyTarget != nil {
			*legacyTarget(p) = nil
		}
		return nil
	}, unset: func(p *config.PartialConfig) {
		*canonicalTarget(p) = nil
		if legacyTarget != nil {
			*legacyTarget(p) = nil
		}
	}}
}

func reviewerDiscoveryStringField(key, env, flag string, get func(config.Config) any, canonicalTarget func(*config.PartialConfig) **string, legacyTarget func(*config.PartialConfig) **string) configField {
	return reviewerDiscoveryStringFieldWithAlias(key, env, "", flag, "", get, canonicalTarget, legacyTarget)
}

func reviewerDiscoveryStringFieldWithAlias(key, env, envAlias, flag, flagAlias string, get func(config.Config) any, canonicalTarget func(*config.PartialConfig) **string, legacyTarget func(*config.PartialConfig) **string) configField {
	return configField{key: key, valueType: "string", env: env, flag: flag, get: get, set: func(p *config.PartialConfig, raw string) error {
		if strings.TrimSpace(raw) == "" {
			return fmt.Errorf("invalid value for %s: must be a non-empty string", key)
		}
		*canonicalTarget(p) = &raw
		if legacyTarget != nil {
			*legacyTarget(p) = nil
		}
		return nil
	}, unset: func(p *config.PartialConfig) {
		*canonicalTarget(p) = nil
		if legacyTarget != nil {
			*legacyTarget(p) = nil
		}
	}, envAlias: envAlias, flagAlias: flagAlias}
}

func positiveIntField(key, env, flag string, get func(config.Config) any, target func(*config.PartialConfig) **int) configField {
	return configField{key: key, valueType: "integer", env: env, flag: flag, get: get, set: func(p *config.PartialConfig, raw string) error {
		value, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || value < 1 {
			return fmt.Errorf("invalid value for %s: must be a positive integer", key)
		}
		*target(p) = &value
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

func reviewerDiscoveryStringListField(key, env string, get func(config.Config) any, canonicalTarget func(*config.PartialConfig) **[]string, legacyTarget func(*config.PartialConfig) **[]string) configField {
	return reviewerDiscoveryStringListFieldWithAlias(key, env, "", get, canonicalTarget, legacyTarget)
}

func reviewerDiscoveryStringListFieldWithAlias(key, env, envAlias string, get func(config.Config) any, canonicalTarget func(*config.PartialConfig) **[]string, legacyTarget func(*config.PartialConfig) **[]string) configField {
	return configField{key: key, valueType: "string-list", env: env, get: get, set: func(p *config.PartialConfig, raw string) error {
		items, err := parseConfigStringList(raw)
		if err != nil {
			return fmt.Errorf("invalid value for %s: %v", key, err)
		}
		*canonicalTarget(p) = &items
		if legacyTarget != nil {
			*legacyTarget(p) = nil
		}
		return nil
	}, unset: func(p *config.PartialConfig) {
		*canonicalTarget(p) = nil
		if legacyTarget != nil {
			*legacyTarget(p) = nil
		}
	}, envAlias: envAlias}
}

func reviewerDiscoveryLabelModeField(key, env string, get func(config.Config) any, canonicalTarget func(*config.PartialConfig) **config.LabelMode, legacyTarget func(*config.PartialConfig) **config.LabelMode) configField {
	return reviewerDiscoveryLabelModeFieldWithAlias(key, env, "", get, canonicalTarget, legacyTarget)
}

func reviewerDiscoveryLabelModeFieldWithAlias(key, env, envAlias string, get func(config.Config) any, canonicalTarget func(*config.PartialConfig) **config.LabelMode, legacyTarget func(*config.PartialConfig) **config.LabelMode) configField {
	return configField{key: key, valueType: "string", env: env, get: get, set: func(p *config.PartialConfig, raw string) error {
		mode := config.LabelMode(strings.TrimSpace(raw))
		switch mode {
		case config.LabelModeAll, config.LabelModeAny:
			*canonicalTarget(p) = &mode
			if legacyTarget != nil {
				*legacyTarget(p) = nil
			}
			return nil
		default:
			return fmt.Errorf("invalid value for %s: must be one of: %s, %s", key, config.LabelModeAll, config.LabelModeAny)
		}
	}, unset: func(p *config.PartialConfig) {
		*canonicalTarget(p) = nil
		if legacyTarget != nil {
			*legacyTarget(p) = nil
		}
	}, envAlias: envAlias}
}

func fixerAuthorFilterField() configField {
	return configField{key: "roles.fixer.triggers.authorFilter", valueType: "string", env: "LOOPER_ROLES_FIXER_TRIGGERS_AUTHOR_FILTER", flag: "roles-fixer-triggers-author-filter", flagAlias: "fix-all-pull-requests", get: func(c config.Config) any { return c.Roles.Fixer.Triggers.AuthorFilter }, set: func(p *config.PartialConfig, raw string) error {
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

func reviewerAutoMergeStrategyField() configField {
	return configField{key: "roles.reviewer.autoMerge.strategy", valueType: "string", flag: "roles-reviewer-auto-merge-strategy", flagAlias: "reviewer-auto-merge-strategy", get: func(c config.Config) any { return c.Roles.Reviewer.AutoMerge.Strategy }, set: func(p *config.PartialConfig, raw string) error {
		value := config.ReviewerAutoMergeStrategy(strings.TrimSpace(raw))
		switch value {
		case config.ReviewerAutoMergeStrategySquash, config.ReviewerAutoMergeStrategyMerge, config.ReviewerAutoMergeStrategyRebase:
			ensurePartialReviewerAutoMerge(p).Strategy = &value
			return nil
		default:
			return fmt.Errorf("invalid value for roles.reviewer.autoMerge.strategy: must be one of: %s, %s, %s", config.ReviewerAutoMergeStrategySquash, config.ReviewerAutoMergeStrategyMerge, config.ReviewerAutoMergeStrategyRebase)
		}
	}, unset: func(p *config.PartialConfig) { ensurePartialReviewerAutoMerge(p).Strategy = nil }}
}

func reviewerAutoMergeScopeField() configField {
	return configField{key: "roles.reviewer.autoMerge.scope", valueType: "string", flag: "roles-reviewer-auto-merge-scope", flagAlias: "reviewer-auto-merge-scope", get: func(c config.Config) any { return c.Roles.Reviewer.AutoMerge.Scope }, set: func(p *config.PartialConfig, raw string) error {
		value := config.ReviewerAutoMergeScope(strings.TrimSpace(raw))
		if value != config.ReviewerAutoMergeScopeLooperOnly {
			return fmt.Errorf("invalid value for roles.reviewer.autoMerge.scope: must be %s", config.ReviewerAutoMergeScopeLooperOnly)
		}
		ensurePartialReviewerAutoMerge(p).Scope = &value
		return nil
	}, unset: func(p *config.PartialConfig) { ensurePartialReviewerAutoMerge(p).Scope = nil }}
}

func reviewerReviewEventField(key, env, envAlias, flag, flagAlias string, get func(config.Config) any, target func(*config.PartialConfig) **config.ReviewerReviewEvent) configField {
	return configField{key: key, valueType: "string", env: env, envAlias: envAlias, flag: flag, flagAlias: flagAlias, get: get, set: func(p *config.PartialConfig, raw string) error {
		value := config.ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(raw)))
		switch key {
		case "roles.reviewer.behavior.reviewEvents.clean", "reviewer.reviewEvents.clean":
			if value != config.ReviewerReviewEventComment && value != config.ReviewerReviewEventApprove {
				return fmt.Errorf("invalid value for %s: must be one of: %s, %s", key, config.ReviewerReviewEventComment, config.ReviewerReviewEventApprove)
			}
		case "roles.reviewer.behavior.reviewEvents.blocking", "reviewer.reviewEvents.blocking":
			if value != config.ReviewerReviewEventComment && value != config.ReviewerReviewEventRequestChanges {
				return fmt.Errorf("invalid value for %s: must be one of: %s, %s", key, config.ReviewerReviewEventComment, config.ReviewerReviewEventRequestChanges)
			}
		default:
			return fmt.Errorf("unsupported review event config key %q", key)
		}
		clearReviewerReviewEventField(p, key)
		*target(p) = &value
		return nil
	}, unset: func(p *config.PartialConfig) {
		clearReviewerReviewEventField(p, key)
	}}
}

func clearReviewerReviewEventField(partial *config.PartialConfig, key string) {
	switch key {
	case "roles.reviewer.behavior.reviewEvents.clean", "reviewer.reviewEvents.clean":
		if partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Behavior != nil && partial.Roles.Reviewer.Behavior.ReviewEvents != nil {
			partial.Roles.Reviewer.Behavior.ReviewEvents.Clean = nil
		}
		if partial.LegacyReviewer != nil && partial.LegacyReviewer.ReviewEvents != nil {
			partial.LegacyReviewer.ReviewEvents.Clean = nil
		}
	case "roles.reviewer.behavior.reviewEvents.blocking", "reviewer.reviewEvents.blocking":
		if partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Behavior != nil && partial.Roles.Reviewer.Behavior.ReviewEvents != nil {
			partial.Roles.Reviewer.Behavior.ReviewEvents.Blocking = nil
		}
		if partial.LegacyReviewer != nil && partial.LegacyReviewer.ReviewEvents != nil {
			partial.LegacyReviewer.ReviewEvents.Blocking = nil
		}
	}
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
	reviewer := ensurePartialReviewerRole(partial)
	if reviewer.Behavior == nil {
		reviewer.Behavior = &config.PartialReviewerConfig{}
	}
	if reviewer.Behavior.ReviewEvents == nil {
		reviewer.Behavior.ReviewEvents = &config.PartialReviewerReviewEventsConfig{}
	}
	return reviewer.Behavior.ReviewEvents
}

func ensurePartialReviewerRetry(partial *config.PartialConfig) *config.PartialReviewerRetryConfig {
	reviewer := ensurePartialReviewerRole(partial)
	if reviewer.Behavior == nil {
		reviewer.Behavior = &config.PartialReviewerConfig{}
	}
	if reviewer.Behavior.Retry == nil {
		reviewer.Behavior.Retry = &config.PartialReviewerRetryConfig{}
	}
	return reviewer.Behavior.Retry
}

func ensurePartialInstructions(partial *config.PartialConfig) *config.PartialInstructionsConfig {
	if partial.Instructions == nil {
		partial.Instructions = &config.PartialInstructionsConfig{}
	}
	return partial.Instructions
}

func ensurePartialPackage(partial *config.PartialConfig) *config.PartialPackageConfig {
	if partial.Package == nil {
		partial.Package = &config.PartialPackageConfig{}
	}
	return partial.Package
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

func ensurePartialReviewerRoleDiscovery(partial *config.PartialConfig) *config.PartialReviewerRoleDiscoveryConfig {
	reviewer := ensurePartialReviewerRole(partial)
	if reviewer.Discovery == nil {
		reviewer.Discovery = &config.PartialReviewerRoleDiscoveryConfig{}
	}
	return reviewer.Discovery
}

func ensurePartialReviewerAutoMerge(partial *config.PartialConfig) *config.PartialReviewerAutoMergeConfig {
	reviewer := ensurePartialReviewerRole(partial)
	if reviewer.AutoMerge == nil {
		reviewer.AutoMerge = &config.PartialReviewerAutoMergeConfig{}
	}
	return reviewer.AutoMerge
}

func ensurePartialReviewerRoleTriggers(partial *config.PartialConfig) *config.PartialReviewerRoleTriggersConfig {
	reviewer := ensurePartialReviewerRole(partial)
	if reviewer.Triggers == nil {
		reviewer.Triggers = &config.PartialReviewerRoleTriggersConfig{}
	}
	return reviewer.Triggers
}

func ensurePartialReviewerRoleDiscoveryTriggers(partial *config.PartialConfig) *config.PartialReviewerRoleTriggersConfig {
	discovery := ensurePartialReviewerRoleDiscovery(partial)
	if discovery.Triggers == nil {
		discovery.Triggers = &config.PartialReviewerRoleTriggersConfig{}
	}
	return discovery.Triggers
}

func ensurePartialReviewerSpecReview(partial *config.PartialConfig) *config.PartialReviewerSpecReviewConfig {
	reviewer := ensurePartialReviewerRole(partial)
	if reviewer.SpecReview == nil {
		reviewer.SpecReview = &config.PartialReviewerSpecReviewConfig{}
	}
	return reviewer.SpecReview
}

func ensurePartialReviewerRoleDiscoverySpecReview(partial *config.PartialConfig) *config.PartialReviewerSpecReviewConfig {
	discovery := ensurePartialReviewerRoleDiscovery(partial)
	if discovery.SpecReview == nil {
		discovery.SpecReview = &config.PartialReviewerSpecReviewConfig{}
	}
	return discovery.SpecReview
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
	case "instructions.enabled":
		return partial.Instructions != nil && partial.Instructions.Enabled != nil
	case "package.autoUpgradeEnabled":
		return partial.Package != nil && partial.Package.AutoUpgradeEnabled != nil
	case "instructions.maxBytes":
		return partial.Instructions != nil && partial.Instructions.MaxBytes != nil
	case "roles.reviewer.behavior.reviewEvents.clean", "reviewer.reviewEvents.clean":
		return (partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Behavior != nil && partial.Roles.Reviewer.Behavior.ReviewEvents != nil && partial.Roles.Reviewer.Behavior.ReviewEvents.Clean != nil) ||
			(partial.LegacyReviewer != nil && partial.LegacyReviewer.ReviewEvents != nil && partial.LegacyReviewer.ReviewEvents.Clean != nil)
	case "roles.reviewer.behavior.reviewEvents.blocking", "reviewer.reviewEvents.blocking":
		return (partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Behavior != nil && partial.Roles.Reviewer.Behavior.ReviewEvents != nil && partial.Roles.Reviewer.Behavior.ReviewEvents.Blocking != nil) ||
			(partial.LegacyReviewer != nil && partial.LegacyReviewer.ReviewEvents != nil && partial.LegacyReviewer.ReviewEvents.Blocking != nil)
	case "roles.reviewer.behavior.retry.enhancedTransientClassification":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Behavior != nil && partial.Roles.Reviewer.Behavior.Retry != nil && partial.Roles.Reviewer.Behavior.Retry.EnhancedTransientClassification != nil
	case "roles.reviewer.behavior.retry.extraTransientErrorPatterns":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Behavior != nil && partial.Roles.Reviewer.Behavior.Retry != nil && partial.Roles.Reviewer.Behavior.Retry.ExtraTransientErrorPatterns != nil
	case "roles.reviewer.behavior.retry.recoverExistingMatchedFailures":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Behavior != nil && partial.Roles.Reviewer.Behavior.Retry != nil && partial.Roles.Reviewer.Behavior.Retry.RecoverExistingMatchedFailures != nil
	case "roles.reviewer.behavior.retry.autoRecoveryMaxAttempts":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Behavior != nil && partial.Roles.Reviewer.Behavior.Retry != nil && partial.Roles.Reviewer.Behavior.Retry.AutoRecoveryMaxAttempts != nil
	case "roles.reviewer.behavior.retry.maxDelayMs":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Behavior != nil && partial.Roles.Reviewer.Behavior.Retry != nil && partial.Roles.Reviewer.Behavior.Retry.MaxDelayMS != nil
	case "roles.planner.autoDiscovery":
		return partial.Roles != nil && partial.Roles.Planner != nil && partial.Roles.Planner.AutoDiscovery != nil
	case "roles.planner.instructions":
		return partial.Roles != nil && partial.Roles.Planner != nil && partial.Roles.Planner.Instructions != nil
	case "roles.planner.triggers.labels":
		return partial.Roles != nil && partial.Roles.Planner != nil && partial.Roles.Planner.Triggers != nil && partial.Roles.Planner.Triggers.Labels != nil
	case "roles.planner.triggers.labelMode":
		return partial.Roles != nil && partial.Roles.Planner != nil && partial.Roles.Planner.Triggers != nil && partial.Roles.Planner.Triggers.LabelMode != nil
	case "roles.planner.triggers.requireAssigneeCurrentUser":
		return partial.Roles != nil && partial.Roles.Planner != nil && partial.Roles.Planner.Triggers != nil && partial.Roles.Planner.Triggers.RequireAssigneeCurrentUser != nil
	case "roles.worker.autoDiscovery":
		return partial.Roles != nil && partial.Roles.Worker != nil && partial.Roles.Worker.AutoDiscovery != nil
	case "roles.worker.instructions":
		return partial.Roles != nil && partial.Roles.Worker != nil && partial.Roles.Worker.Instructions != nil
	case "roles.worker.triggers.labels":
		return partial.Roles != nil && partial.Roles.Worker != nil && partial.Roles.Worker.Triggers != nil && partial.Roles.Worker.Triggers.Labels != nil
	case "roles.worker.triggers.labelMode":
		return partial.Roles != nil && partial.Roles.Worker != nil && partial.Roles.Worker.Triggers != nil && partial.Roles.Worker.Triggers.LabelMode != nil
	case "roles.worker.triggers.requireAssigneeCurrentUser":
		return partial.Roles != nil && partial.Roles.Worker != nil && partial.Roles.Worker.Triggers != nil && partial.Roles.Worker.Triggers.RequireAssigneeCurrentUser != nil
	case "roles.reviewer.discovery.autoDiscovery", "roles.reviewer.autoDiscovery":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && ((partial.Roles.Reviewer.Discovery != nil && partial.Roles.Reviewer.Discovery.AutoDiscovery != nil) || partial.Roles.Reviewer.AutoDiscovery != nil)
	case "roles.reviewer.autoMerge.enabled":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.AutoMerge != nil && partial.Roles.Reviewer.AutoMerge.Enabled != nil
	case "roles.reviewer.autoMerge.strategy":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.AutoMerge != nil && partial.Roles.Reviewer.AutoMerge.Strategy != nil
	case "roles.reviewer.autoMerge.requireBranchProtection":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.AutoMerge != nil && partial.Roles.Reviewer.AutoMerge.RequireBranchProtection != nil
	case "roles.reviewer.autoMerge.transientRetries":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.AutoMerge != nil && partial.Roles.Reviewer.AutoMerge.TransientRetries != nil
	case "roles.reviewer.autoMerge.scope":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.AutoMerge != nil && partial.Roles.Reviewer.AutoMerge.Scope != nil
	case "roles.reviewer.instructions":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && partial.Roles.Reviewer.Instructions != nil
	case "roles.reviewer.discovery.triggers.includeDrafts", "roles.reviewer.triggers.includeDrafts":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && ((partial.Roles.Reviewer.Discovery != nil && partial.Roles.Reviewer.Discovery.Triggers != nil && partial.Roles.Reviewer.Discovery.Triggers.IncludeDrafts != nil) || (partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.IncludeDrafts != nil))
	case "roles.reviewer.discovery.triggers.requireReviewRequest", "roles.reviewer.triggers.requireReviewRequest":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && ((partial.Roles.Reviewer.Discovery != nil && partial.Roles.Reviewer.Discovery.Triggers != nil && partial.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest != nil) || (partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.RequireReviewRequest != nil))
	case "roles.reviewer.discovery.triggers.enableSelfReview", "roles.reviewer.triggers.enableSelfReview":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && ((partial.Roles.Reviewer.Discovery != nil && partial.Roles.Reviewer.Discovery.Triggers != nil && partial.Roles.Reviewer.Discovery.Triggers.EnableSelfReview != nil) || (partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.EnableSelfReview != nil))
	case "roles.reviewer.discovery.triggers.labels", "roles.reviewer.triggers.labels":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && ((partial.Roles.Reviewer.Discovery != nil && partial.Roles.Reviewer.Discovery.Triggers != nil && partial.Roles.Reviewer.Discovery.Triggers.Labels != nil) || (partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.Labels != nil))
	case "roles.reviewer.discovery.triggers.labelMode", "roles.reviewer.triggers.labelMode":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && ((partial.Roles.Reviewer.Discovery != nil && partial.Roles.Reviewer.Discovery.Triggers != nil && partial.Roles.Reviewer.Discovery.Triggers.LabelMode != nil) || (partial.Roles.Reviewer.Triggers != nil && partial.Roles.Reviewer.Triggers.LabelMode != nil))
	case "roles.reviewer.discovery.specReview.includeReviewingLabel", "roles.reviewer.specReview.includeReviewingLabel":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && ((partial.Roles.Reviewer.Discovery != nil && partial.Roles.Reviewer.Discovery.SpecReview != nil && partial.Roles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel != nil) || (partial.Roles.Reviewer.SpecReview != nil && partial.Roles.Reviewer.SpecReview.IncludeReviewingLabel != nil))
	case "roles.reviewer.discovery.specReview.reviewingLabel", "roles.reviewer.specReview.reviewingLabel":
		return partial.Roles != nil && partial.Roles.Reviewer != nil && ((partial.Roles.Reviewer.Discovery != nil && partial.Roles.Reviewer.Discovery.SpecReview != nil && partial.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel != nil) || (partial.Roles.Reviewer.SpecReview != nil && partial.Roles.Reviewer.SpecReview.ReviewingLabel != nil))
	case "roles.fixer.autoDiscovery":
		return partial.Roles != nil && partial.Roles.Fixer != nil && partial.Roles.Fixer.AutoDiscovery != nil
	case "roles.fixer.instructions":
		return partial.Roles != nil && partial.Roles.Fixer != nil && partial.Roles.Fixer.Instructions != nil
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
