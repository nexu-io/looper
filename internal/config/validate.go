package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var networkNodeNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

type ValidationIssue struct {
	Path    string
	Message string
}

type ConfigValidationError struct {
	Issues []ValidationIssue
}

type ValidateOptions struct {
	DefaultWorktreeRoot string
}

func (err *ConfigValidationError) Error() string {
	if err == nil || len(err.Issues) == 0 {
		return "config validation failed"
	}

	if len(err.Issues) == 1 {
		issue := err.Issues[0]
		return fmt.Sprintf("config validation failed: %s %s", issue.Path, issue.Message)
	}

	return fmt.Sprintf("config validation failed with %d issues", len(err.Issues))
}

func Validate(config Config) error {
	return ValidateWithOptions(config, ValidateOptions{})
}

func ValidateWithOptions(config Config, options ValidateOptions) error {
	issues := make([]ValidationIssue, 0)

	if config.Server.Host == "" {
		issues = append(issues, ValidationIssue{Path: "server.host", Message: "must be a non-empty string"})
	}

	if config.Server.Port < 1 || config.Server.Port > 65535 {
		issues = append(issues, ValidationIssue{Path: "server.port", Message: "must be an integer between 1 and 65535"})
	}

	if !isValidAuthMode(config.Server.AuthMode) {
		issues = append(issues, ValidationIssue{Path: "server.authMode", Message: fmt.Sprintf("must be one of: %s, %s", AuthModeNone, AuthModeLocalToken)})
	}

	if config.Server.AuthMode == AuthModeLocalToken && isNilOrEmptyString(config.Server.LocalToken) {
		issues = append(issues, ValidationIssue{Path: "server.localToken", Message: "is required when authMode is local-token"})
	}

	if config.Storage.Mode != "sqlite" {
		issues = append(issues, ValidationIssue{Path: "storage.mode", Message: "must be sqlite"})
	}

	if config.Storage.DBPath == "" {
		issues = append(issues, ValidationIssue{Path: "storage.dbPath", Message: "must be a non-empty path"})
	}

	if config.Scheduler.PollIntervalSeconds < 10 {
		issues = append(issues, ValidationIssue{Path: "scheduler.pollIntervalSeconds", Message: "must be an integer >= 10"})
	}

	if config.Scheduler.MaxConcurrentRuns < 1 {
		issues = append(issues, ValidationIssue{Path: "scheduler.maxConcurrentRuns", Message: "must be a positive integer"})
	}

	if config.Scheduler.RetryMaxAttempts < 1 {
		issues = append(issues, ValidationIssue{Path: "scheduler.retryMaxAttempts", Message: "must be a positive integer"})
	}

	if config.Scheduler.RetryBaseDelayMS < 1 {
		issues = append(issues, ValidationIssue{Path: "scheduler.retryBaseDelayMs", Message: "must be a positive integer"})
	}

	if config.Scheduler.SlowLaneWarnThresholdMS < 1 {
		issues = append(issues, ValidationIssue{Path: "scheduler.slowLaneWarnThresholdMs", Message: "must be a positive integer"})
	}

	if config.Webhook.FallbackPollIntervalSeconds < 60 {
		issues = append(issues, ValidationIssue{Path: "webhook.fallbackPollIntervalSeconds", Message: "must be an integer >= 60"})
	}
	if !isValidWebhookMode(config.Webhook.Mode) {
		issues = append(issues, ValidationIssue{Path: "webhook.mode", Message: fmt.Sprintf("must be one of: %s, %s", WebhookModeGHForward, WebhookModeTunnel)})
	}
	if config.Webhook.Enabled && webhookModeRequiresTunnelConfig(config, nil) {
		validateWebhookTunnelConfig(config.Webhook, "webhook", &issues)
	}

	if config.Agent.Vendor != nil && !isValidAgentVendor(*config.Agent.Vendor) {
		issues = append(issues, ValidationIssue{Path: "agent.vendor", Message: fmt.Sprintf("must be one of: %s, %s, %s, %s", AgentVendorClaudeCode, AgentVendorCodex, AgentVendorOpenCode, AgentVendorCursorCLI)})
	}
	validateAgentTimeouts(config.Agent.Timeouts, "agent.timeouts", &issues)

	if !isValidLogLevel(config.Logging.Level) {
		issues = append(issues, ValidationIssue{Path: "logging.level", Message: fmt.Sprintf("must be one of: %s, %s, %s, %s", LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError)})
	}

	if config.Logging.MaxSizeMB < 1 {
		issues = append(issues, ValidationIssue{Path: "logging.maxSizeMB", Message: "must be a positive integer"})
	}

	if config.Logging.MaxFiles < 1 {
		issues = append(issues, ValidationIssue{Path: "logging.maxFiles", Message: "must be a positive integer"})
	}

	if config.Notifications.Osascript.ThrottleWindowSeconds < 1 {
		issues = append(issues, ValidationIssue{Path: "notifications.osascript.throttleWindowSeconds", Message: "must be a positive integer"})
	}

	for _, level := range config.Notifications.Osascript.SoundForLevels {
		if !isValidNotificationSoundLevel(level) {
			issues = append(issues, ValidationIssue{Path: "notifications.osascript.soundForLevels", Message: fmt.Sprintf("contains unsupported value: %s", level)})
		}
	}

	if !isValidDaemonMode(config.Daemon.Mode) {
		issues = append(issues, ValidationIssue{Path: "daemon.mode", Message: fmt.Sprintf("must be one of: %s, %s", DaemonModeForeground, DaemonModeLaunchd)})
	}

	if !isValidDaemonRestartPolicy(config.Daemon.RestartPolicy) {
		issues = append(issues, ValidationIssue{Path: "daemon.restartPolicy", Message: fmt.Sprintf("must be one of: %s, %s, %s", DaemonRestartNever, DaemonRestartOnFailure, DaemonRestartAlways)})
	}

	if config.Daemon.RestartThrottleSeconds < 1 {
		issues = append(issues, ValidationIssue{Path: "daemon.restartThrottleSeconds", Message: "must be a positive integer"})
	}

	if config.Daemon.LogDir == "" {
		issues = append(issues, ValidationIssue{Path: "daemon.logDir", Message: "must be a non-empty path"})
	}

	if config.Daemon.ShutdownTimeoutMS < 1 {
		issues = append(issues, ValidationIssue{Path: "daemon.shutdownTimeoutMs", Message: "must be a positive integer"})
	}

	if config.Daemon.WorkingDirectory == "" {
		issues = append(issues, ValidationIssue{Path: "daemon.workingDirectory", Message: "must be a non-empty path"})
	}
	validateWorktreeCleanupConfig(config.Daemon.WorktreeCleanup, "daemon.worktreeCleanup", &issues)

	if strings.TrimSpace(config.Package.Distribution) == "" {
		issues = append(issues, ValidationIssue{Path: "package.distribution", Message: "must be a non-empty string"})
	}

	if config.Defaults.BaseBranch == "" {
		issues = append(issues, ValidationIssue{Path: "defaults.baseBranch", Message: "must be a non-empty string"})
	}

	if !isValidOpenPRStrategy(config.Defaults.OpenPRStrategy) {
		issues = append(issues, ValidationIssue{Path: "defaults.openPrStrategy", Message: fmt.Sprintf("must be one of: %s, %s, %s", OpenPRStrategyAllDone, OpenPRStrategyFirstCommit, OpenPRStrategyManual)})
	}

	if !isValidAddSnapshotMode(config.Defaults.AddSnapshotMode) {
		issues = append(issues, ValidationIssue{Path: "defaults.addSnapshotMode", Message: fmt.Sprintf("must be one of: %s, %s, %s", AddSnapshotModeAsync, AddSnapshotModeFull, AddSnapshotModeOff)})
	}

	if config.Roles.Reviewer.Behavior.Loop.QuietPeriodSeconds < 0 {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.loop.quietPeriodSeconds", Message: "must be an integer >= 0"})
	}
	if config.Roles.Reviewer.Behavior.Loop.MinPublishIntervalSeconds < 0 {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.loop.minPublishIntervalSeconds", Message: "must be an integer >= 0"})
	}
	if config.Roles.Reviewer.Behavior.Retry.AutoRecoveryMaxAttempts < 1 {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.retry.autoRecoveryMaxAttempts", Message: "must be a positive integer"})
	}
	if config.Roles.Reviewer.Behavior.Retry.MaxDelayMS < 1 {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.retry.maxDelayMs", Message: "must be a positive integer"})
	}
	for index, pattern := range config.Roles.Reviewer.Behavior.Retry.ExtraTransientErrorPatterns {
		if strings.TrimSpace(pattern) == "" {
			issues = append(issues, ValidationIssue{Path: fmt.Sprintf("roles.reviewer.behavior.retry.extraTransientErrorPatterns[%d]", index), Message: "must be a non-empty string"})
		}
	}
	if !isValidReviewerScope(config.Roles.Reviewer.Behavior.Scope) {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.scope", Message: fmt.Sprintf("must be one of: %s, %s, %s", ReviewerScopeFullPR, ReviewerScopeChangedFiles, ReviewerScopeChangedRanges)})
	}
	if config.Roles.Reviewer.Behavior.PublishMode != ReviewerPublishModeSingleReview {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.publishMode", Message: fmt.Sprintf("must be %s", ReviewerPublishModeSingleReview)})
	}
	if !isValidReviewerThreadResolutionMode(config.Roles.Reviewer.Behavior.ThreadResolution.Mode) {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.threadResolution.mode", Message: fmt.Sprintf("must be one of: %s, %s, %s, %s", ReviewerThreadResolutionModeReportOnly, ReviewerThreadResolutionModeCommentOnly, ReviewerThreadResolutionModeSuggestResolution, ReviewerThreadResolutionModeResolveObjective)})
	}
	if config.Roles.Reviewer.Behavior.ThreadResolution.Scope != ReviewerThreadResolutionScopeLooperAuthoredOnly {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.threadResolution.scope", Message: fmt.Sprintf("must be %s", ReviewerThreadResolutionScopeLooperAuthoredOnly)})
	}
	if config.Roles.Reviewer.Behavior.ThreadResolution.AutoResolve != ReviewerThreadResolutionAutoResolveObjectiveOnly {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.threadResolution.autoResolve", Message: fmt.Sprintf("must be %s", ReviewerThreadResolutionAutoResolveObjectiveOnly)})
	}
	if config.Roles.Reviewer.Behavior.ThreadResolution.MaxThreadsPerRun < 1 {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.threadResolution.maxThreadsPerRun", Message: "must be a positive integer"})
	}
	if config.Roles.Reviewer.Behavior.ThreadResolution.Mode == ReviewerThreadResolutionModeResolveObjective && !config.Roles.Reviewer.Behavior.ThreadResolution.RequireAuditComment {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.threadResolution.requireAuditComment", Message: "must be true when mode is resolve_objective"})
	}
	if !isValidReviewerAutoMergeStrategy(config.Roles.Reviewer.AutoMerge.Strategy) {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.autoMerge.strategy", Message: fmt.Sprintf("must be one of: %s, %s, %s", ReviewerAutoMergeStrategySquash, ReviewerAutoMergeStrategyMerge, ReviewerAutoMergeStrategyRebase)})
	}
	if config.Roles.Reviewer.AutoMerge.TransientRetries < 1 {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.autoMerge.transientRetries", Message: "must be a positive integer"})
	}
	if config.Roles.Reviewer.AutoMerge.Scope != ReviewerAutoMergeScopeLooperOnly {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.autoMerge.scope", Message: fmt.Sprintf("must be %s", ReviewerAutoMergeScopeLooperOnly)})
	}
	if config.Roles.Reviewer.Behavior.ReviewEvents.Clean != ReviewerReviewEventComment && config.Roles.Reviewer.Behavior.ReviewEvents.Clean != ReviewerReviewEventApprove {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.reviewEvents.clean", Message: fmt.Sprintf("must be one of: %s, %s", ReviewerReviewEventComment, ReviewerReviewEventApprove)})
	}
	if config.Roles.Reviewer.Behavior.ReviewEvents.Blocking != ReviewerReviewEventComment && config.Roles.Reviewer.Behavior.ReviewEvents.Blocking != ReviewerReviewEventRequestChanges {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.reviewEvents.blocking", Message: fmt.Sprintf("must be one of: %s, %s", ReviewerReviewEventComment, ReviewerReviewEventRequestChanges)})
	}

	validateInstructions(config, &issues)
	validateCoordinatorRoleConfig(config.Roles.Coordinator, "roles.coordinator", &issues)
	validateIssueRoleTriggers(config.Roles.Planner.Triggers, "roles.planner.triggers", &issues)
	validateIssueRoleTriggers(config.Roles.Worker.Triggers, "roles.worker.triggers", &issues)
	validateReviewerRoleTriggers(config.Roles.Reviewer.Discovery.Triggers, "roles.reviewer.discovery.triggers", &issues)
	validateFixerRoleTriggers(config.Roles.Fixer.Triggers, "roles.fixer.triggers", &issues)
	validateSweeperRoleConfig(config.Roles.Sweeper, "roles.sweeper", &issues)
	if config.Roles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel && strings.TrimSpace(config.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel) == "" {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.discovery.specReview.reviewingLabel", Message: "must be a non-empty string when includeReviewingLabel is true"})
	} else if config.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel != strings.TrimSpace(config.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel) {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.discovery.specReview.reviewingLabel", Message: "must not contain leading or trailing whitespace"})
	}

	projectIDs := make(map[string]struct{}, len(config.Projects))
	for index, project := range config.Projects {
		prefix := fmt.Sprintf("projects[%d]", index)

		if project.ID == "" {
			issues = append(issues, ValidationIssue{Path: prefix + ".id", Message: "must be a non-empty string"})
		} else if !isValidConfiguredProjectID(project.ID) {
			issues = append(issues, ValidationIssue{Path: prefix + ".id", Message: getConfigProjectIDValidationMessage()})
		} else {
			if _, exists := projectIDs[project.ID]; exists {
				issues = append(issues, ValidationIssue{Path: prefix + ".id", Message: fmt.Sprintf("duplicate project id: %s", project.ID)})
			} else {
				projectIDs[project.ID] = struct{}{}
			}
		}

		if project.Name == "" {
			issues = append(issues, ValidationIssue{Path: prefix + ".name", Message: "must be a non-empty string"})
		}

		if project.RepoPath == "" {
			issues = append(issues, ValidationIssue{Path: prefix + ".repoPath", Message: "must be a non-empty path"})
		}
		if project.Path != "" && project.RepoPath != "" && project.Path != project.RepoPath {
			issues = append(issues, ValidationIssue{Path: prefix + ".path", Message: "must match repoPath when both path and repoPath are set"})
		}
		if !isValidWebhookModeOrEmpty(project.Webhook.Mode) {
			issues = append(issues, ValidationIssue{Path: prefix + ".webhook.mode", Message: fmt.Sprintf("must be one of: %s, %s", WebhookModeGHForward, WebhookModeTunnel)})
		}
		if !isValidNetworkMode(project.Network.Mode) {
			issues = append(issues, ValidationIssue{Path: prefix + ".network.mode", Message: fmt.Sprintf("must be one of: %s, %s", NetworkModeOff, NetworkModeRouted)})
		}
		if config.Webhook.Enabled && webhookModeRequiresTunnelConfig(config, &project) {
			validateWebhookTunnelConfig(config.Webhook, "webhook", &issues)
		}

		validateProjectRoleOverrides(project.Roles, prefix+".roles", config.Instructions.MaxBytes, &issues)
		effectiveProjectRoles := ProjectRoleConfigs(config, project.ID)
		for _, roleInstruction := range roleInstructions(effectiveProjectRoles) {
			if !projectRoleInstructionsConfigured(project.Roles, roleInstruction.role) {
				continue
			}
			path := fmt.Sprintf("%s.roles.%s.instructions", prefix, roleInstruction.role)
			validateInstructionText(path, roleInstruction.role, roleInstruction.text, config.Instructions.MaxBytes, &issues)
		}
		if effectiveProjectRoles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel && strings.TrimSpace(effectiveProjectRoles.Reviewer.Discovery.SpecReview.ReviewingLabel) == "" {
			issues = append(issues, ValidationIssue{Path: prefix + ".roles.reviewer.discovery.specReview.reviewingLabel", Message: "must be a non-empty string when includeReviewingLabel is true"})
		}
		if project.Roles != nil && project.Roles.Sweeper != nil {
			validateSweeperRoleConfig(effectiveProjectRoles.Sweeper, prefix+".roles.sweeper", &issues)
		}
		if project.Roles != nil && project.Roles.Coordinator != nil {
			validateCoordinatorRoleConfig(effectiveProjectRoles.Coordinator, prefix+".roles.coordinator", &issues)
		}
		if normalizeNetworkMode(project.Network.Mode) == NetworkModeRouted {
			validateRoutedProjectPrerequisites(config, effectiveProjectRoles, prefix, &issues)
		}
	}

	if len(issues) > 0 {
		return &ConfigValidationError{Issues: issues}
	}

	defaultWorktreeRoot := options.DefaultWorktreeRoot
	if defaultWorktreeRoot == "" {
		resolvedDefaultWorktreeRoot, err := DefaultWorktreeRoot()
		if err != nil {
			return fmt.Errorf("determine default worktree root: %w", err)
		}

		defaultWorktreeRoot = resolvedDefaultWorktreeRoot
	}

	ensureWritablePath(config.Storage.DBPath, writablePathFileParent, &issues, "storage.dbPath")
	ensureWritablePath(config.Daemon.LogDir, writablePathDirectory, &issues, "daemon.logDir")
	ensureWritablePath(config.Daemon.WorkingDirectory, writablePathDirectory, &issues, "daemon.workingDirectory")
	ensureWritablePath(defaultWorktreeRoot, writablePathDirectory, &issues, "defaults.worktreeRoot")

	if len(issues) > 0 {
		return &ConfigValidationError{Issues: issues}
	}

	return nil
}

func validateRoutedProjectPrerequisites(config Config, roles RoleConfigs, prefix string, issues *[]ValidationIssue) {
	if !config.Network.Enrolled {
		*issues = append(*issues, ValidationIssue{Path: "network.enrolled", Message: fmt.Sprintf("must be true when %s.network.mode is %s; join a Network or set the project back to %s", prefix, NetworkModeRouted, NetworkModeOff)})
	}
	parsedLoopernetURL, err := url.Parse(strings.TrimSpace(config.Network.LoopernetBaseURL))
	if err != nil || parsedLoopernetURL.Scheme == "" || parsedLoopernetURL.Host == "" {
		*issues = append(*issues, ValidationIssue{Path: "network.loopernetBaseUrl", Message: fmt.Sprintf("must be an absolute URL with a host when %s.network.mode is %s", prefix, NetworkModeRouted)})
	}
	if err := validateNetworkNodeName(config.Network.NodeName); err != nil {
		*issues = append(*issues, ValidationIssue{Path: "network.nodeName", Message: fmt.Sprintf("%v when %s.network.mode is %s", err, prefix, NetworkModeRouted)})
	}
	if config.Network.GitHubUserID < 0 {
		*issues = append(*issues, ValidationIssue{Path: "network.githubUserId", Message: "must be a positive integer when configured"})
	}
	if strings.TrimSpace(config.Network.GitHubLogin) == "" {
		*issues = append(*issues, ValidationIssue{Path: "network.githubLogin", Message: fmt.Sprintf("must be configured when %s.network.mode is %s so routed claims can fall back when numeric GitHub IDs are unavailable", prefix, NetworkModeRouted)})
	}
	if roles.Planner.AutoDiscovery {
		*issues = append(*issues, ValidationIssue{Path: prefix + ".roles.planner.autoDiscovery", Message: "must be false for routed projects; planner routed execution is not supported yet"})
	}
	if roles.Fixer.AutoDiscovery {
		*issues = append(*issues, ValidationIssue{Path: prefix + ".roles.fixer.autoDiscovery", Message: "must be false for routed projects; fixer routed execution is not supported yet"})
	}
}

func isValidNetworkMode(mode NetworkMode) bool {
	return normalizeNetworkMode(mode) == NetworkModeOff || normalizeNetworkMode(mode) == NetworkModeRouted
}

func normalizeNetworkMode(mode NetworkMode) NetworkMode {
	switch strings.TrimSpace(string(mode)) {
	case "", string(NetworkModeOff):
		return NetworkModeOff
	case string(NetworkModeRouted):
		return NetworkModeRouted
	default:
		return mode
	}
}

func validateNetworkNodeName(nodeName string) error {
	trimmed := strings.TrimSpace(nodeName)
	if trimmed == "" {
		return fmt.Errorf("must be a non-empty string")
	}
	if trimmed != nodeName {
		return fmt.Errorf("must not contain leading or trailing whitespace")
	}
	if strings.Contains(trimmed, ":") {
		return fmt.Errorf("must not contain ':' so it can form looper:target:<node_name>")
	}
	if len(trimmed) > 32 {
		return fmt.Errorf("must be 32 characters or fewer so it can form looper:target:<node_name>")
	}
	if !networkNodeNamePattern.MatchString(trimmed) {
		return fmt.Errorf("must contain only letters, numbers, '.', '_' or '-' so it can form looper:target:<node_name>")
	}
	return nil
}

func validateWebhookTunnelConfig(config WebhookConfig, path string, issues *[]ValidationIssue) {
	if config.ListenPort < 1024 || config.ListenPort > 65535 {
		*issues = append(*issues, ValidationIssue{Path: path + ".listenPort", Message: "must be an integer between 1024 and 65535 when webhook mode is tunnel"})
	}
	parsed, err := url.Parse(config.PublicBaseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		*issues = append(*issues, ValidationIssue{Path: path + ".publicBaseUrl", Message: "must be a valid https URL with a host when webhook mode is tunnel"})
	}
}

func validateWorktreeCleanupConfig(config WorktreeCleanupConfig, path string, issues *[]ValidationIssue) {
	if strings.TrimSpace(config.Interval) == "" {
		*issues = append(*issues, ValidationIssue{Path: path + ".interval", Message: "must be a non-empty duration string"})
	} else if duration, err := time.ParseDuration(config.Interval); err != nil || duration <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".interval", Message: "must be a positive duration"})
	}
	if config.RetentionDays < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".retentionDays", Message: "must be an integer >= 0"})
	}
	if config.MaxPerTick < 1 {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxPerTick", Message: "must be a positive integer"})
	}
}

func webhookModeRequiresTunnelConfig(config Config, project *ProjectRefConfig) bool {
	mode := config.Webhook.Mode
	if project != nil && project.Webhook.Mode != "" {
		mode = project.Webhook.Mode
	}
	return mode == WebhookModeTunnel
}

func validateAgentTimeouts(timeouts AgentTimeoutConfig, path string, issues *[]ValidationIssue) {
	validateAgentTimeoutSeconds(timeouts.PlannerSeconds, path+".plannerSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.WorkerSeconds, path+".workerSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.ReviewerSeconds, path+".reviewerSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.FixerSeconds, path+".fixerSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.PlannerIdleTimeoutSeconds, path+".plannerIdleTimeoutSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.PlannerMaxRuntimeSeconds, path+".plannerMaxRuntimeSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.WorkerIdleTimeoutSeconds, path+".workerIdleTimeoutSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.WorkerMaxRuntimeSeconds, path+".workerMaxRuntimeSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.ReviewerIdleTimeoutSeconds, path+".reviewerIdleTimeoutSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.ReviewerMaxRuntimeSeconds, path+".reviewerMaxRuntimeSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.FixerIdleTimeoutSeconds, path+".fixerIdleTimeoutSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.FixerMaxRuntimeSeconds, path+".fixerMaxRuntimeSeconds", issues)
}

func validateAgentTimeoutSeconds(seconds int, path string, issues *[]ValidationIssue) {
	if seconds < 1 {
		*issues = append(*issues, ValidationIssue{Path: path, Message: "must be a positive integer"})
		return
	}

	const maxDuration = time.Duration(1<<63 - 1)
	maxSeconds := int64(maxDuration / time.Second)
	if int64(seconds) > maxSeconds {
		*issues = append(*issues, ValidationIssue{Path: path, Message: "must fit within time.Duration when converted from seconds"})
	}
}

func validateProjectRoleOverrides(roles *PartialRoleConfigs, prefix string, maxInstructionBytes int, issues *[]ValidationIssue) {
	if roles == nil {
		return
	}
	if roles.Planner != nil {
		validateProjectRoleInstruction(prefix+".planner.instructions", "planner", roles.Planner.Instructions, maxInstructionBytes, issues)
		if roles.Planner.Triggers != nil {
			validateIssueRoleTriggers(partialIssueRoleTriggers(*roles.Planner.Triggers), prefix+".planner.triggers", issues)
		}
	}
	if roles.Worker != nil {
		validateProjectRoleInstruction(prefix+".worker.instructions", "worker", roles.Worker.Instructions, maxInstructionBytes, issues)
		if roles.Worker.Triggers != nil {
			validateIssueRoleTriggers(partialIssueRoleTriggers(*roles.Worker.Triggers), prefix+".worker.triggers", issues)
		}
	}
	if roles.Reviewer != nil {
		validateProjectRoleInstruction(prefix+".reviewer.instructions", "reviewer", roles.Reviewer.Instructions, maxInstructionBytes, issues)
		if roles.Reviewer.Discovery != nil {
			if roles.Reviewer.Discovery.Triggers != nil {
				validateReviewerRoleTriggers(partialReviewerRoleTriggers(*roles.Reviewer.Discovery.Triggers), prefix+".reviewer.discovery.triggers", issues)
			}
			if roles.Reviewer.Discovery.SpecReview != nil && roles.Reviewer.Discovery.SpecReview.ReviewingLabel != nil {
				label := *roles.Reviewer.Discovery.SpecReview.ReviewingLabel
				if label != "" && label != strings.TrimSpace(label) {
					*issues = append(*issues, ValidationIssue{Path: prefix + ".reviewer.discovery.specReview.reviewingLabel", Message: "must not contain leading or trailing whitespace"})
				}
			}
		}
		if roles.Reviewer.Triggers != nil {
			validateReviewerRoleTriggers(partialReviewerRoleTriggers(*roles.Reviewer.Triggers), prefix+".reviewer.triggers", issues)
		}
		if roles.Reviewer.SpecReview != nil && roles.Reviewer.SpecReview.ReviewingLabel != nil {
			label := *roles.Reviewer.SpecReview.ReviewingLabel
			if label != "" && label != strings.TrimSpace(label) {
				*issues = append(*issues, ValidationIssue{Path: prefix + ".reviewer.specReview.reviewingLabel", Message: "must not contain leading or trailing whitespace"})
			}
		}
		if roles.Reviewer.AutoMerge != nil {
			validatePartialReviewerAutoMerge(*roles.Reviewer.AutoMerge, prefix+".reviewer.autoMerge", issues)
		}
	}
	if roles.Fixer != nil {
		validateProjectRoleInstruction(prefix+".fixer.instructions", "fixer", roles.Fixer.Instructions, maxInstructionBytes, issues)
		if roles.Fixer.Triggers != nil {
			validateFixerRoleTriggers(partialFixerRoleTriggers(*roles.Fixer.Triggers), prefix+".fixer.triggers", issues)
		}
	}
	if roles.Sweeper != nil {
		validateProjectRoleInstruction(prefix+".sweeper.instructions", "sweeper", roles.Sweeper.Instructions, maxInstructionBytes, issues)
	}
	if roles.Coordinator != nil {
		if roles.Coordinator.PollInterval != nil && strings.TrimSpace(*roles.Coordinator.PollInterval) == "" {
			*issues = append(*issues, ValidationIssue{Path: prefix + ".coordinator.pollInterval", Message: "must be a non-empty duration string"})
		}
	}
}

func validateProjectRoleInstruction(path, role string, text *string, maxBytes int, issues *[]ValidationIssue) {
	if text == nil {
		return
	}
	validateInstructionText(path, role, *text, maxBytes, issues)
}

func partialIssueRoleTriggers(partial PartialIssueRoleTriggersConfig) IssueRoleTriggersConfig {
	config := IssueRoleTriggersConfig{LabelMode: LabelModeAll}
	mergeIssueRoleTriggersConfig(&config, partial)
	return config
}

func partialReviewerRoleTriggers(partial PartialReviewerRoleTriggersConfig) ReviewerRoleTriggersConfig {
	config := ReviewerRoleTriggersConfig{LabelMode: LabelModeAll}
	mergeReviewerRoleTriggersConfig(&config, partial)
	return config
}

func partialFixerRoleTriggers(partial PartialFixerRoleTriggersConfig) FixerRoleTriggersConfig {
	config := FixerRoleTriggersConfig{AuthorFilter: FixerAuthorFilterCurrentUser, LabelMode: LabelModeAll}
	mergeFixerRoleTriggersConfig(&config, partial)
	return config
}

func validateInstructions(config Config, issues *[]ValidationIssue) {
	if config.Instructions.MaxBytes < 1 {
		*issues = append(*issues, ValidationIssue{Path: "instructions.maxBytes", Message: "must be a positive integer"})
	}
	for _, roleInstruction := range roleInstructions(config.Roles) {
		validateInstructionText("roles."+roleInstruction.role+".instructions", roleInstruction.role, roleInstruction.text, config.Instructions.MaxBytes, issues)
		validateAggregateInstructionBytes("roles."+roleInstruction.role+".instructions", roleInstruction.text, "", config.Instructions.MaxBytes, issues)
	}
}

type roleInstruction struct {
	role string
	text string
}

func roleInstructions(roles RoleConfigs) []roleInstruction {
	return []roleInstruction{
		{role: "planner", text: roles.Planner.Instructions},
		{role: "worker", text: roles.Worker.Instructions},
		{role: "reviewer", text: roles.Reviewer.Instructions},
		{role: "fixer", text: roles.Fixer.Instructions},
		{role: "sweeper", text: roles.Sweeper.Instructions},
	}
}

func roleInstructionText(roles RoleConfigs, role string) string {
	switch role {
	case "planner":
		return roles.Planner.Instructions
	case "worker":
		return roles.Worker.Instructions
	case "reviewer":
		return roles.Reviewer.Instructions
	case "fixer":
		return roles.Fixer.Instructions
	case "sweeper":
		return roles.Sweeper.Instructions
	default:
		return ""
	}
}

func validateInstructionText(path, role, text string, maxBytes int, issues *[]ValidationIssue) {
	if !isValidInstructionRole(role) {
		*issues = append(*issues, ValidationIssue{Path: path, Message: "role must be one of: planner, worker, reviewer, fixer, sweeper"})
	}
	if maxBytes > 0 && len([]byte(text)) > maxBytes {
		*issues = append(*issues, ValidationIssue{Path: path, Message: fmt.Sprintf("must be at most %d bytes", maxBytes)})
	}
	if protected := protectedInstructionPhrase(text); protected != "" {
		*issues = append(*issues, ValidationIssue{Path: path, Message: fmt.Sprintf("must not attempt to override protected Looper contract %q", protected)})
	}
}

func validateAggregateInstructionBytes(path, globalText, projectText string, maxBytes int, issues *[]ValidationIssue) {
	if maxBytes <= 0 {
		return
	}
	bytes := len([]byte(strings.TrimSpace(globalText))) + len([]byte(strings.TrimSpace(projectText)))
	if bytes > maxBytes {
		*issues = append(*issues, ValidationIssue{Path: path, Message: fmt.Sprintf("combined custom instructions for this role must be at most %d bytes", maxBytes)})
	}
}

func isValidInstructionRole(role string) bool {
	switch role {
	case "planner", "worker", "reviewer", "fixer", "sweeper":
		return true
	default:
		return false
	}
}

func protectedInstructionPhrase(text string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(text), " "))
	protected := []string{"systemprompt", "system prompt", "__looper_result__", "completion marker", "git_pr_lifecycle", "summary field", "commits field", "result json", "allowautopush", "allowautoapprove", "allow auto push", "allow auto approve", "auto approve", "auto push", "pr creation policy", "review submission policy", "looper review submit", "review submit wrapper", "gh pr review", "disclosure stamping", "auth requirement", "permission boundary", "state transition", "state machine", "ignore lifecycle", "override lifecycle", "custom completion"}
	for _, phrase := range protected {
		if strings.Contains(normalized, phrase) {
			return phrase
		}
	}
	return ""
}

type writablePathKind string

const (
	writablePathDirectory  writablePathKind = "directory"
	writablePathFileParent writablePathKind = "file-parent"
	writePermissionMode                     = 0x2
)

func ensureWritablePath(path string, kind writablePathKind, issues *[]ValidationIssue, field string) {
	target := path
	if kind == writablePathFileParent {
		target = filepath.Dir(path)
	}

	writableAnchor := target
	for {
		info, err := os.Stat(writableAnchor)
		if err == nil {
			if !info.IsDir() {
				*issues = append(*issues, ValidationIssue{Path: field, Message: fmt.Sprintf("%s is not a directory", writableAnchor)})
			}
			break
		}

		if errors.Is(err, syscall.ENOTDIR) {
			parent := filepath.Dir(writableAnchor)
			if parent == writableAnchor {
				*issues = append(*issues, ValidationIssue{Path: field, Message: fmt.Sprintf("%s cannot be created because no existing parent was found", target)})
				return
			}

			writableAnchor = parent
			continue
		}

		if !os.IsNotExist(err) {
			*issues = append(*issues, ValidationIssue{Path: field, Message: err.Error()})
			return
		}

		parent := filepath.Dir(writableAnchor)
		if parent == writableAnchor {
			*issues = append(*issues, ValidationIssue{Path: field, Message: fmt.Sprintf("%s cannot be created because no existing parent was found", target)})
			return
		}

		writableAnchor = parent
	}

	if hasIssueForField(*issues, field) {
		return
	}

	if err := syscall.Access(writableAnchor, writePermissionMode); err != nil {
		*issues = append(*issues, ValidationIssue{Path: field, Message: fmt.Sprintf("%s is not writable", writableAnchor)})
	}
}

func hasIssueForField(issues []ValidationIssue, field string) bool {
	for _, issue := range issues {
		if issue.Path == field {
			return true
		}
	}

	return false
}

func getConfigProjectIDValidationMessage() string {
	return "must not contain path separators, dot segments, or be an absolute path"
}

func isNilOrEmptyString(value *string) bool {
	return value == nil || *value == ""
}

func isValidAgentVendor(vendor AgentVendor) bool {
	switch vendor {
	case AgentVendorClaudeCode, AgentVendorCodex, AgentVendorOpenCode, AgentVendorCursorCLI:
		return true
	default:
		return false
	}
}

func isValidAuthMode(mode AuthMode) bool {
	switch mode {
	case AuthModeNone, AuthModeLocalToken:
		return true
	default:
		return false
	}
}

func isValidDaemonMode(mode DaemonMode) bool {
	switch mode {
	case DaemonModeForeground, DaemonModeLaunchd:
		return true
	default:
		return false
	}
}

func isValidDaemonRestartPolicy(policy DaemonRestartPolicy) bool {
	switch policy {
	case DaemonRestartNever, DaemonRestartOnFailure, DaemonRestartAlways:
		return true
	default:
		return false
	}
}

func isValidLogLevel(level LogLevel) bool {
	switch level {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
		return true
	default:
		return false
	}
}

func isValidNotificationSoundLevel(level NotificationSoundLevel) bool {
	switch level {
	case NotificationSoundLevelActionRequired, NotificationSoundLevelFailure:
		return true
	default:
		return false
	}
}

func isValidOpenPRStrategy(strategy OpenPRStrategy) bool {
	switch strategy {
	case OpenPRStrategyAllDone, OpenPRStrategyFirstCommit, OpenPRStrategyManual:
		return true
	default:
		return false
	}
}

func isValidAddSnapshotMode(mode AddSnapshotMode) bool {
	switch mode {
	case AddSnapshotModeAsync, AddSnapshotModeFull, AddSnapshotModeOff:
		return true
	default:
		return false
	}
}

func isValidWebhookMode(mode WebhookMode) bool {
	switch mode {
	case WebhookModeGHForward, WebhookModeTunnel:
		return true
	default:
		return false
	}
}

func isValidWebhookModeOrEmpty(mode WebhookMode) bool {
	return mode == "" || isValidWebhookMode(mode)
}

func isValidLabelMode(mode LabelMode) bool {
	switch mode {
	case LabelModeAll, LabelModeAny:
		return true
	default:
		return false
	}
}

func isValidFixerAuthorFilter(filter FixerAuthorFilter) bool {
	switch filter {
	case FixerAuthorFilterCurrentUser, FixerAuthorFilterAny:
		return true
	default:
		return false
	}
}

func isValidReviewerAutoMergeStrategy(strategy ReviewerAutoMergeStrategy) bool {
	switch strategy {
	case ReviewerAutoMergeStrategySquash, ReviewerAutoMergeStrategyMerge, ReviewerAutoMergeStrategyRebase:
		return true
	default:
		return false
	}
}

func validatePartialReviewerAutoMerge(partial PartialReviewerAutoMergeConfig, path string, issues *[]ValidationIssue) {
	if partial.Strategy != nil && !isValidReviewerAutoMergeStrategy(*partial.Strategy) {
		*issues = append(*issues, ValidationIssue{Path: path + ".strategy", Message: fmt.Sprintf("must be one of: %s, %s, %s", ReviewerAutoMergeStrategySquash, ReviewerAutoMergeStrategyMerge, ReviewerAutoMergeStrategyRebase)})
	}
	if partial.TransientRetries != nil && *partial.TransientRetries < 1 {
		*issues = append(*issues, ValidationIssue{Path: path + ".transientRetries", Message: "must be a positive integer"})
	}
	if partial.Scope != nil && *partial.Scope != ReviewerAutoMergeScopeLooperOnly {
		*issues = append(*issues, ValidationIssue{Path: path + ".scope", Message: fmt.Sprintf("must be %s", ReviewerAutoMergeScopeLooperOnly)})
	}
}

func validateIssueRoleTriggers(triggers IssueRoleTriggersConfig, path string, issues *[]ValidationIssue) {
	validateLabelTriggers(triggers.Labels, triggers.LabelMode, path, issues)
}

func validateReviewerRoleTriggers(triggers ReviewerRoleTriggersConfig, path string, issues *[]ValidationIssue) {
	validateLabelTriggers(triggers.Labels, triggers.LabelMode, path, issues)
}

func validateFixerRoleTriggers(triggers FixerRoleTriggersConfig, path string, issues *[]ValidationIssue) {
	validateLabelTriggers(triggers.Labels, triggers.LabelMode, path, issues)
	if !isValidFixerAuthorFilter(triggers.AuthorFilter) {
		*issues = append(*issues, ValidationIssue{Path: path + ".authorFilter", Message: fmt.Sprintf("must be one of: %s, %s", FixerAuthorFilterCurrentUser, FixerAuthorFilterAny)})
	}
}

func validateCoordinatorRoleConfig(config CoordinatorRoleConfig, path string, issues *[]ValidationIssue) {
	if strings.TrimSpace(config.PollInterval) == "" {
		*issues = append(*issues, ValidationIssue{Path: path + ".pollInterval", Message: "must be a non-empty duration string"})
	} else if duration, err := time.ParseDuration(strings.TrimSpace(config.PollInterval)); err != nil {
		*issues = append(*issues, ValidationIssue{Path: path + ".pollInterval", Message: "must be a valid time.Duration string"})
	} else if duration <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".pollInterval", Message: "must be greater than 0"})
	}
	if config.Triage.MaxIssueAgeDays <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".triage.maxIssueAgeDays", Message: "must be a positive integer"})
	}
	if config.Triage.MaxPerTick <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".triage.maxPerTick", Message: "must be a positive integer"})
	}
	if !isNonEmptyTrimmed(config.Triage.TriagedLabel) {
		*issues = append(*issues, ValidationIssue{Path: path + ".triage.triagedLabel", Message: "must be a non-empty string without leading or trailing whitespace"})
	}
	if !isNonEmptyTrimmed(config.Triage.Disposition.OutOfScopeLabel) {
		*issues = append(*issues, ValidationIssue{Path: path + ".triage.disposition.outOfScopeLabel", Message: "must be a non-empty string without leading or trailing whitespace"})
	}
	if !isNonEmptyTrimmed(config.Triage.Disposition.UnclearLabel) {
		*issues = append(*issues, ValidationIssue{Path: path + ".triage.disposition.unclearLabel", Message: "must be a non-empty string without leading or trailing whitespace"})
	}
	if config.Dispatch.Mode != "human-gated" && config.Dispatch.Mode != "autonomous" {
		*issues = append(*issues, ValidationIssue{Path: path + ".dispatch.mode", Message: "must be one of: human-gated, autonomous"})
	}
	validateStringList(config.Dispatch.HumanGate.SlashCommands, path+".dispatch.humanGate.slashCommands", issues)
	validateStringList(config.Dispatch.HumanGate.AllowedUsers, path+".dispatch.humanGate.allowedUsers", issues)
	if len(config.Dispatch.HumanGate.SlashCommands) == 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".dispatch.humanGate.slashCommands", Message: "must contain at least one slash command"})
	}
	for _, command := range config.Dispatch.HumanGate.SlashCommands {
		if command != "/plan" && command != "/implement" {
			*issues = append(*issues, ValidationIssue{Path: path + ".dispatch.humanGate.slashCommands", Message: fmt.Sprintf("contains unsupported slash command: %s", command)})
		}
	}
	if config.Dispatch.Autonomous.DelayMinutes <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".dispatch.autonomous.delayMinutes", Message: "must be a positive integer"})
	}
	if !isNonEmptyTrimmed(config.Dispatch.Autonomous.HoldLabel) {
		*issues = append(*issues, ValidationIssue{Path: path + ".dispatch.autonomous.holdLabel", Message: "must be a non-empty string without leading or trailing whitespace"})
	}
	if config.Dispatch.AssignTo != strings.TrimSpace(config.Dispatch.AssignTo) {
		*issues = append(*issues, ValidationIssue{Path: path + ".dispatch.assignTo", Message: "must not contain leading or trailing whitespace"})
	}
	if config.Dependencies.Enabled {
		if config.Dependencies.APITimeoutSeconds <= 0 {
			*issues = append(*issues, ValidationIssue{Path: path + ".dependencies.apiTimeoutSeconds", Message: "must be a positive integer when dependencies are enabled"})
		}
		if config.Dependencies.APIRetryAttempts <= 0 {
			*issues = append(*issues, ValidationIssue{Path: path + ".dependencies.apiRetryAttempts", Message: "must be a positive integer when dependencies are enabled"})
		}
	}
	if config.MergeWatch.TransientRetries <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".mergeWatch.transientRetries", Message: "must be a positive integer"})
	}
	if strings.TrimSpace(config.MergeWatch.MaxIndeterminateDuration) == "" {
		*issues = append(*issues, ValidationIssue{Path: path + ".mergeWatch.maxIndeterminateDuration", Message: "must be a non-empty duration string"})
	} else if duration, err := time.ParseDuration(strings.TrimSpace(config.MergeWatch.MaxIndeterminateDuration)); err != nil {
		*issues = append(*issues, ValidationIssue{Path: path + ".mergeWatch.maxIndeterminateDuration", Message: "must be a valid time.Duration string"})
	} else if duration <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".mergeWatch.maxIndeterminateDuration", Message: "must be greater than 0"})
	}
	validateDistinctLabels([]labelPathValue{
		{Path: path + ".triage.triagedLabel", Value: config.Triage.TriagedLabel},
		{Path: path + ".triage.disposition.outOfScopeLabel", Value: config.Triage.Disposition.OutOfScopeLabel},
		{Path: path + ".triage.disposition.unclearLabel", Value: config.Triage.Disposition.UnclearLabel},
		{Path: path + ".dispatch.autonomous.holdLabel", Value: config.Dispatch.Autonomous.HoldLabel},
	}, issues)
}

func isNonEmptyTrimmed(value string) bool {
	return strings.TrimSpace(value) != "" && value == strings.TrimSpace(value)
}

func validateSweeperRoleConfig(config SweeperRoleConfig, path string, issues *[]ValidationIssue) {
	validateStringList(config.Triggers.ExcludeLabels, path+".triggers.excludeLabels", issues)
	validateStringList(config.Triggers.ExcludeAuthors, path+".triggers.excludeAuthors", issues)
	validateStringList(config.Triggers.LooperInternalLabels, path+".triggers.looperInternalLabels", issues)
	validateStringList(config.Security.NotifyAssignees, path+".security.notifyAssignees", issues)
	validateSweeperAssociations(config.Triggers.ExcludeAuthorAssociations, path+".triggers.excludeAuthorAssociations", issues)

	if config.Triggers.MaxPerTick <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".triggers.maxPerTick", Message: "must be a positive integer"})
	}
	if config.Filter.Mode != SweeperFilterModeDeterministic {
		*issues = append(*issues, ValidationIssue{Path: path + ".filter.mode", Message: fmt.Sprintf("must be %q", SweeperFilterModeDeterministic)})
	}
	if config.Proposer.Mode != SweeperProposerModeAgentApply && config.Proposer.Mode != SweeperProposerModeHeuristicFallback {
		*issues = append(*issues, ValidationIssue{Path: path + ".proposer.mode", Message: fmt.Sprintf("must be one of: %s, %s", SweeperProposerModeAgentApply, SweeperProposerModeHeuristicFallback)})
	}
	if config.Proposer.TimeoutSeconds <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".proposer.timeoutSeconds", Message: "must be a positive integer"})
	}
	if config.Proposer.TimeoutRateDryRunThreshold < 0 || config.Proposer.TimeoutRateDryRunThreshold > 1 {
		*issues = append(*issues, ValidationIssue{Path: path + ".proposer.timeoutRateDryRunThreshold", Message: "must be between 0 and 1"})
	}
	if config.Proposer.TimeoutRateDryRunMinSamples < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".proposer.timeoutRateDryRunMinSamples", Message: "must be greater than or equal to 0"})
	}
	if config.Proposer.SchemaVersion != 1 && config.Proposer.SchemaVersion != 2 {
		*issues = append(*issues, ValidationIssue{Path: path + ".proposer.schemaVersion", Message: "must be 1 or 2"})
	}
	if config.Proposer.Model != nil && strings.TrimSpace(*config.Proposer.Model) == "" {
		*issues = append(*issues, ValidationIssue{Path: path + ".proposer.model", Message: "must be a non-empty string when set"})
	}
	if config.Triggers.ReopenCooldownDays <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".triggers.reopenCooldownDays", Message: "must be a positive integer"})
	}
	if config.AutoDiscovery && !config.Triggers.IncludeIssues && !config.Triggers.IncludePullRequests {
		*issues = append(*issues, ValidationIssue{Path: path + ".triggers.includeIssues", Message: "includeIssues and includePullRequests cannot both be false when sweeper autoDiscovery is enabled"})
	}
	if config.Limits.MaxWarningsPerRepoPerDay < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".limits.maxWarningsPerRepoPerDay", Message: "must be greater than or equal to 0"})
	}
	if config.Limits.MaxClosesPerRepoPerDay < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".limits.maxClosesPerRepoPerDay", Message: "must be greater than or equal to 0"})
	}

	validateSweeperLabel(config.Lifecycle.PendingLabel, path+".lifecycle.pendingLabel", issues)
	validateSweeperLabel(config.Lifecycle.ClosedLabel, path+".lifecycle.closedLabel", issues)
	validateSweeperLabel(config.Lifecycle.KeepLabel, path+".lifecycle.keepLabel", issues)
	validateSweeperLabel(config.Security.QuarantineLabel, path+".security.quarantineLabel", issues)
	validateDistinctLabels([]labelPathValue{
		{Path: path + ".lifecycle.pendingLabel", Value: config.Lifecycle.PendingLabel},
		{Path: path + ".lifecycle.closedLabel", Value: config.Lifecycle.ClosedLabel},
		{Path: path + ".lifecycle.keepLabel", Value: config.Lifecycle.KeepLabel},
		{Path: path + ".security.quarantineLabel", Value: config.Security.QuarantineLabel},
	}, issues)
	validateNoLabelOverlap(
		[]labelPathValue{
			{Path: path + ".lifecycle.pendingLabel", Value: config.Lifecycle.PendingLabel},
			{Path: path + ".lifecycle.closedLabel", Value: config.Lifecycle.ClosedLabel},
			{Path: path + ".security.quarantineLabel", Value: config.Security.QuarantineLabel},
		},
		config.Triggers.ExcludeLabels,
		path+".triggers.excludeLabels",
		issues,
	)

	validateSweeperCategory(config.Categories.Stale, path+".categories.stale", true, issues)
	validateSweeperCategory(config.Categories.AlreadyFixed, path+".categories.alreadyFixed", false, issues)
	validateSweeperCategory(config.Categories.Unrelated, path+".categories.unrelated", false, issues)
	validateSweeperCategory(config.Categories.Superseded, path+".categories.superseded", false, issues)
	validateSweeperCategory(config.Categories.AbandonedPR, path+".categories.abandonedPR", true, issues)
}

type labelPathValue struct {
	Path  string
	Value string
}

func validateSweeperCategory(config SweeperCategoryConfig, path string, requiresInactivity bool, issues *[]ValidationIssue) {
	if requiresInactivity && config.Enabled && config.InactivityDays <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".inactivityDays", Message: "must be a positive integer when category is enabled"})
	}
	if config.GracePeriodDays <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".gracePeriodDays", Message: "must be a positive integer"})
	}
	if config.MinConfidence < 0 || config.MinConfidence > 100 {
		*issues = append(*issues, ValidationIssue{Path: path + ".minConfidence", Message: "must be between 0 and 100"})
	}
}

func validateSweeperLabel(value, path string, issues *[]ValidationIssue) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		*issues = append(*issues, ValidationIssue{Path: path, Message: "must be a non-empty string"})
		return
	}
	if value != trimmed {
		*issues = append(*issues, ValidationIssue{Path: path, Message: "must not contain leading or trailing whitespace"})
	}
}

func validateDistinctLabels(labels []labelPathValue, issues *[]ValidationIssue) {
	seen := map[string]string{}
	for _, label := range labels {
		trimmed := strings.TrimSpace(label.Value)
		if trimmed == "" {
			continue
		}
		if firstPath, ok := seen[trimmed]; ok {
			*issues = append(*issues, ValidationIssue{Path: label.Path, Message: fmt.Sprintf("duplicates %s", firstPath)})
			continue
		}
		seen[trimmed] = label.Path
	}
}

func validateNoLabelOverlap(labels []labelPathValue, excluded []string, excludePath string, issues *[]ValidationIssue) {
	excludedSet := map[string]struct{}{}
	for _, label := range excluded {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			continue
		}
		excludedSet[trimmed] = struct{}{}
	}
	for _, label := range labels {
		trimmed := strings.TrimSpace(label.Value)
		if trimmed == "" {
			continue
		}
		if _, ok := excludedSet[trimmed]; ok {
			*issues = append(*issues, ValidationIssue{Path: excludePath, Message: fmt.Sprintf("must not contain lifecycle/security label %q configured at %s", trimmed, label.Path)})
		}
	}
}

func validateSweeperAssociations(values []string, path string, issues *[]ValidationIssue) {
	allowed := map[string]struct{}{
		"OWNER": {}, "MEMBER": {}, "COLLABORATOR": {}, "CONTRIBUTOR": {},
		"FIRST_TIME_CONTRIBUTOR": {}, "FIRST_TIMER": {}, "MANNEQUIN": {}, "NONE": {},
	}
	seen := map[string]struct{}{}
	for index, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("%s[%d]", path, index), Message: "must be a non-empty string"})
			continue
		}
		if value != trimmed {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("%s[%d]", path, index), Message: "must not contain leading or trailing whitespace"})
			continue
		}
		if _, ok := allowed[value]; !ok {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("%s[%d]", path, index), Message: "must be one of: OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, FIRST_TIMER, MANNEQUIN, NONE"})
			continue
		}
		if _, ok := seen[value]; ok {
			*issues = append(*issues, ValidationIssue{Path: path, Message: fmt.Sprintf("contains duplicate value: %s", value)})
			continue
		}
		seen[value] = struct{}{}
	}
}

func validateStringList(values []string, path string, issues *[]ValidationIssue) {
	seen := map[string]struct{}{}
	for index, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("%s[%d]", path, index), Message: "must be a non-empty string"})
			continue
		}
		if value != trimmed {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("%s[%d]", path, index), Message: "must not contain leading or trailing whitespace"})
			continue
		}
		if _, ok := seen[value]; ok {
			*issues = append(*issues, ValidationIssue{Path: path, Message: fmt.Sprintf("contains duplicate value: %s", value)})
			continue
		}
		seen[value] = struct{}{}
	}
}

func validateLabelTriggers(labels []string, mode LabelMode, path string, issues *[]ValidationIssue) {
	if !isValidLabelMode(mode) {
		*issues = append(*issues, ValidationIssue{Path: path + ".labelMode", Message: fmt.Sprintf("must be one of: %s, %s", LabelModeAll, LabelModeAny)})
	}
	seen := map[string]struct{}{}
	for index, label := range labels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("%s.labels[%d]", path, index), Message: "must be a non-empty string"})
			continue
		}
		if label != trimmed {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("%s.labels[%d]", path, index), Message: "must not contain leading or trailing whitespace"})
			continue
		}
		if _, ok := seen[label]; ok {
			*issues = append(*issues, ValidationIssue{Path: path + ".labels", Message: fmt.Sprintf("contains duplicate label: %s", label)})
			continue
		}
		seen[label] = struct{}{}
	}
}

func isValidReviewerScope(scope ReviewerScope) bool {
	switch scope {
	case ReviewerScopeFullPR, ReviewerScopeChangedFiles, ReviewerScopeChangedRanges:
		return true
	default:
		return false
	}
}

func isValidReviewerThreadResolutionMode(mode ReviewerThreadResolutionMode) bool {
	switch mode {
	case ReviewerThreadResolutionModeReportOnly, ReviewerThreadResolutionModeCommentOnly, ReviewerThreadResolutionModeSuggestResolution, ReviewerThreadResolutionModeResolveObjective:
		return true
	default:
		return false
	}
}

func isValidConfiguredProjectID(projectID string) bool {
	return projectID != "" && projectID != "." && projectID != ".." && !containsProjectPathSeparator(projectID) && !isAbsoluteProjectPath(projectID)
}

func containsProjectPathSeparator(projectID string) bool {
	for _, char := range projectID {
		if char == '/' || char == '\\' {
			return true
		}
	}

	return false
}

func isAbsoluteProjectPath(projectID string) bool {
	if len(projectID) >= 1 && projectID[0] == '/' {
		return true
	}

	if len(projectID) >= 3 {
		drive := projectID[0]
		separator := projectID[2]
		if ((drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z')) && projectID[1] == ':' && (separator == '/' || separator == '\\') {
			return true
		}
	}

	if len(projectID) >= 2 && projectID[0] == '\\' && projectID[1] == '\\' {
		return true
	}

	return false
}
