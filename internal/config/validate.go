package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
