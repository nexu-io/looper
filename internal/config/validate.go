package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

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

	if config.Agent.Vendor != nil && !isValidAgentVendor(*config.Agent.Vendor) {
		issues = append(issues, ValidationIssue{Path: "agent.vendor", Message: fmt.Sprintf("must be one of: %s, %s, %s, %s", AgentVendorClaudeCode, AgentVendorCodex, AgentVendorOpenCode, AgentVendorCursorCLI)})
	}

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

	if config.Daemon.LogDir == "" {
		issues = append(issues, ValidationIssue{Path: "daemon.logDir", Message: "must be a non-empty path"})
	}

	if config.Daemon.ShutdownTimeoutMS < 1 {
		issues = append(issues, ValidationIssue{Path: "daemon.shutdownTimeoutMs", Message: "must be a positive integer"})
	}

	if config.Daemon.WorkingDirectory == "" {
		issues = append(issues, ValidationIssue{Path: "daemon.workingDirectory", Message: "must be a non-empty path"})
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

	if config.Reviewer.Loop.QuietPeriodSeconds < 0 {
		issues = append(issues, ValidationIssue{Path: "reviewer.loop.quietPeriodSeconds", Message: "must be an integer >= 0"})
	}
	if config.Reviewer.Loop.MaxIterationsPerPR < 1 {
		issues = append(issues, ValidationIssue{Path: "reviewer.loop.maxIterationsPerPR", Message: "must be a positive integer"})
	}
	if config.Reviewer.Loop.MaxIterationsPerHead < 1 {
		issues = append(issues, ValidationIssue{Path: "reviewer.loop.maxIterationsPerHead", Message: "must be a positive integer"})
	}
	if config.Reviewer.Loop.MaxWallClockSeconds < 1 {
		issues = append(issues, ValidationIssue{Path: "reviewer.loop.maxWallClockSeconds", Message: "must be a positive integer"})
	}
	if config.Reviewer.Loop.MaxConsecutiveFailures < 1 {
		issues = append(issues, ValidationIssue{Path: "reviewer.loop.maxConsecutiveFailures", Message: "must be a positive integer"})
	}
	if config.Reviewer.Loop.MaxAgentExecutionsPerPR < 1 {
		issues = append(issues, ValidationIssue{Path: "reviewer.loop.maxAgentExecutionsPerPR", Message: "must be a positive integer"})
	}
	if !isValidReviewerScope(config.Reviewer.Scope) {
		issues = append(issues, ValidationIssue{Path: "reviewer.scope", Message: fmt.Sprintf("must be one of: %s, %s, %s", ReviewerScopeFullPR, ReviewerScopeChangedFiles, ReviewerScopeChangedRanges)})
	}
	if config.Reviewer.PublishMode != ReviewerPublishModeSingleReview {
		issues = append(issues, ValidationIssue{Path: "reviewer.publishMode", Message: fmt.Sprintf("must be %s", ReviewerPublishModeSingleReview)})
	}
	if config.Reviewer.ReviewEvents.Clean != ReviewerReviewEventComment && config.Reviewer.ReviewEvents.Clean != ReviewerReviewEventApprove {
		issues = append(issues, ValidationIssue{Path: "reviewer.reviewEvents.clean", Message: fmt.Sprintf("must be one of: %s, %s", ReviewerReviewEventComment, ReviewerReviewEventApprove)})
	}
	if config.Reviewer.ReviewEvents.Blocking != ReviewerReviewEventComment && config.Reviewer.ReviewEvents.Blocking != ReviewerReviewEventRequestChanges {
		issues = append(issues, ValidationIssue{Path: "reviewer.reviewEvents.blocking", Message: fmt.Sprintf("must be one of: %s, %s", ReviewerReviewEventComment, ReviewerReviewEventRequestChanges)})
	}

	validateInstructions(config, &issues)
	validateIssueRoleTriggers(config.Roles.Planner.Triggers, "roles.planner.triggers", &issues)
	validateIssueRoleTriggers(config.Roles.Worker.Triggers, "roles.worker.triggers", &issues)
	validateReviewerRoleTriggers(config.Roles.Reviewer.Triggers, "roles.reviewer.triggers", &issues)
	validateFixerRoleTriggers(config.Roles.Fixer.Triggers, "roles.fixer.triggers", &issues)
	if config.Roles.Reviewer.SpecReview.IncludeReviewingLabel && strings.TrimSpace(config.Roles.Reviewer.SpecReview.ReviewingLabel) == "" {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.specReview.reviewingLabel", Message: "must be a non-empty string when includeReviewingLabel is true"})
	} else if config.Roles.Reviewer.SpecReview.ReviewingLabel != strings.TrimSpace(config.Roles.Reviewer.SpecReview.ReviewingLabel) {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.specReview.reviewingLabel", Message: "must not contain leading or trailing whitespace"})
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

		for role, text := range project.Instructions {
			path := fmt.Sprintf("%s.instructions.%s", prefix, role)
			validateInstructionText(path, role, text, config.Instructions.MaxBytes, &issues)
			validateAggregateInstructionBytes(path, roleInstructionText(config.Roles, role), text, config.Instructions.MaxBytes, &issues)
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
	default:
		return ""
	}
}

func validateInstructionText(path, role, text string, maxBytes int, issues *[]ValidationIssue) {
	if !isValidInstructionRole(role) {
		*issues = append(*issues, ValidationIssue{Path: path, Message: "role must be one of: planner, worker, reviewer, fixer"})
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
	case "planner", "worker", "reviewer", "fixer":
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
