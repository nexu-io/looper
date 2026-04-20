package config

type AgentVendor string

const (
	AgentVendorClaudeCode AgentVendor = "claude-code"
	AgentVendorCodex      AgentVendor = "codex"
	AgentVendorOpenCode   AgentVendor = "opencode"
	AgentVendorCursorCLI  AgentVendor = "cursor-cli"
)

type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

type AuthMode string

const (
	AuthModeNone       AuthMode = "none"
	AuthModeLocalToken AuthMode = "local-token"
)

type DaemonMode string

const (
	DaemonModeForeground DaemonMode = "foreground"
	DaemonModeLaunchd    DaemonMode = "launchd"
)

type OpenPRStrategy string

const (
	OpenPRStrategyAllDone     OpenPRStrategy = "all_done"
	OpenPRStrategyFirstCommit OpenPRStrategy = "first_commit"
	OpenPRStrategyManual      OpenPRStrategy = "manual"
)

type NotificationSoundLevel string

const (
	NotificationSoundLevelActionRequired NotificationSoundLevel = "action_required"
	NotificationSoundLevelFailure        NotificationSoundLevel = "failure"
)

type ServerConfig struct {
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	BaseURL    *string  `json:"baseUrl,omitempty"`
	AuthMode   AuthMode `json:"authMode"`
	LocalToken *string  `json:"localToken,omitempty"`
}

type StorageConfig struct {
	Mode      string  `json:"mode"`
	DBPath    string  `json:"dbPath"`
	BackupDir *string `json:"backupDir,omitempty"`
}

type SchedulerConfig struct {
	PollIntervalSeconds int `json:"pollIntervalSeconds"`
	MaxConcurrentRuns   int `json:"maxConcurrentRuns"`
	RetryMaxAttempts    int `json:"retryMaxAttempts"`
	RetryBaseDelayMS    int `json:"retryBaseDelayMs"`
}

type AgentConfig struct {
	Vendor *AgentVendor      `json:"vendor,omitempty"`
	Model  *string           `json:"model,omitempty"`
	Params map[string]any    `json:"params"`
	Env    map[string]string `json:"env"`
}

type NotificationConfig struct {
	InApp     bool                        `json:"inApp"`
	Osascript OsascriptNotificationConfig `json:"osascript"`
}

type OsascriptNotificationConfig struct {
	Enabled               bool                     `json:"enabled"`
	SoundForLevels        []NotificationSoundLevel `json:"soundForLevels"`
	ThrottleWindowSeconds int                      `json:"throttleWindowSeconds"`
}

type LoggingConfig struct {
	Level     LogLevel `json:"level"`
	MaxSizeMB int      `json:"maxSizeMB"`
	MaxFiles  int      `json:"maxFiles"`
}

type ToolPathsConfig struct {
	GitPath       *string `json:"gitPath,omitempty"`
	GHPath        *string `json:"ghPath,omitempty"`
	OsascriptPath *string `json:"osascriptPath,omitempty"`
}

type ToolDetectionStatus string

const (
	ToolDetectionStatusConfigured ToolDetectionStatus = "configured"
	ToolDetectionStatusDetected   ToolDetectionStatus = "detected"
	ToolDetectionStatusMissing    ToolDetectionStatus = "missing"
)

type DaemonConfig struct {
	Mode              DaemonMode        `json:"mode"`
	PlistPath         *string           `json:"plistPath,omitempty"`
	LogDir            string            `json:"logDir"`
	ShutdownTimeoutMS int               `json:"shutdownTimeoutMs"`
	WorkingDirectory  string            `json:"workingDirectory"`
	Environment       map[string]string `json:"environment"`
}

type PackageConfig struct {
	Distribution               string `json:"distribution"`
	AutoMigrateOnStartup       bool   `json:"autoMigrateOnStartup"`
	RequireBackupBeforeMigrate bool   `json:"requireBackupBeforeMigrate"`
}

type DefaultsConfig struct {
	BaseBranch       string         `json:"baseBranch"`
	AllowAutoCommit  bool           `json:"allowAutoCommit"`
	AllowAutoPush    bool           `json:"allowAutoPush"`
	AllowAutoApprove bool           `json:"allowAutoApprove"`
	AllowAutoMerge   bool           `json:"allowAutoMerge"`
	AllowRiskyFixes  bool           `json:"allowRiskyFixes"`
	OpenPRStrategy   OpenPRStrategy `json:"openPrStrategy"`
}

type ProjectRefConfig struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	RepoPath     string  `json:"repoPath"`
	BaseBranch   *string `json:"baseBranch,omitempty"`
	WorktreeRoot *string `json:"worktreeRoot,omitempty"`
}

type Config struct {
	Server        ServerConfig       `json:"server"`
	Storage       StorageConfig      `json:"storage"`
	Scheduler     SchedulerConfig    `json:"scheduler"`
	Agent         AgentConfig        `json:"agent"`
	Logging       LoggingConfig      `json:"logging"`
	Notifications NotificationConfig `json:"notifications"`
	Tools         ToolPathsConfig    `json:"tools"`
	Daemon        DaemonConfig       `json:"daemon"`
	Package       PackageConfig      `json:"package"`
	Defaults      DefaultsConfig     `json:"defaults"`
	Projects      []ProjectRefConfig `json:"projects"`
}

type PartialServerConfig struct {
	Host       *string   `json:"host,omitempty"`
	Port       *int      `json:"port,omitempty"`
	BaseURL    *string   `json:"baseUrl,omitempty"`
	AuthMode   *AuthMode `json:"authMode,omitempty"`
	LocalToken *string   `json:"localToken,omitempty"`
}

type PartialStorageConfig struct {
	Mode      *string `json:"mode,omitempty"`
	DBPath    *string `json:"dbPath,omitempty"`
	BackupDir *string `json:"backupDir,omitempty"`
}

type PartialSchedulerConfig struct {
	PollIntervalSeconds *int `json:"pollIntervalSeconds,omitempty"`
	MaxConcurrentRuns   *int `json:"maxConcurrentRuns,omitempty"`
	RetryMaxAttempts    *int `json:"retryMaxAttempts,omitempty"`
	RetryBaseDelayMS    *int `json:"retryBaseDelayMs,omitempty"`
}

type PartialAgentConfig struct {
	Vendor *AgentVendor      `json:"vendor,omitempty"`
	Model  *string           `json:"model,omitempty"`
	Params map[string]any    `json:"params,omitempty"`
	Env    map[string]string `json:"env,omitempty"`
}

type PartialNotificationConfig struct {
	InApp     *bool                               `json:"inApp,omitempty"`
	Osascript *PartialOsascriptNotificationConfig `json:"osascript,omitempty"`
}

type PartialOsascriptNotificationConfig struct {
	Enabled               *bool                     `json:"enabled,omitempty"`
	SoundForLevels        *[]NotificationSoundLevel `json:"soundForLevels,omitempty"`
	ThrottleWindowSeconds *int                      `json:"throttleWindowSeconds,omitempty"`
}

type PartialLoggingConfig struct {
	Level     *LogLevel `json:"level,omitempty"`
	MaxSizeMB *int      `json:"maxSizeMB,omitempty"`
	MaxFiles  *int      `json:"maxFiles,omitempty"`
}

type PartialToolPathsConfig struct {
	GitPath       *string `json:"gitPath,omitempty"`
	GHPath        *string `json:"ghPath,omitempty"`
	OsascriptPath *string `json:"osascriptPath,omitempty"`
}

type PartialDaemonConfig struct {
	Mode              *DaemonMode       `json:"mode,omitempty"`
	PlistPath         *string           `json:"plistPath,omitempty"`
	LogDir            *string           `json:"logDir,omitempty"`
	ShutdownTimeoutMS *int              `json:"shutdownTimeoutMs,omitempty"`
	WorkingDirectory  *string           `json:"workingDirectory,omitempty"`
	Environment       map[string]string `json:"environment,omitempty"`
}

type PartialPackageConfig struct {
	Distribution               *string `json:"distribution,omitempty"`
	AutoMigrateOnStartup       *bool   `json:"autoMigrateOnStartup,omitempty"`
	RequireBackupBeforeMigrate *bool   `json:"requireBackupBeforeMigrate,omitempty"`
}

type PartialDefaultsConfig struct {
	BaseBranch       *string         `json:"baseBranch,omitempty"`
	AllowAutoCommit  *bool           `json:"allowAutoCommit,omitempty"`
	AllowAutoPush    *bool           `json:"allowAutoPush,omitempty"`
	AllowAutoApprove *bool           `json:"allowAutoApprove,omitempty"`
	AllowAutoMerge   *bool           `json:"allowAutoMerge,omitempty"`
	AllowRiskyFixes  *bool           `json:"allowRiskyFixes,omitempty"`
	OpenPRStrategy   *OpenPRStrategy `json:"openPrStrategy,omitempty"`
}

type PartialConfig struct {
	Server        *PartialServerConfig       `json:"server,omitempty"`
	Storage       *PartialStorageConfig      `json:"storage,omitempty"`
	Scheduler     *PartialSchedulerConfig    `json:"scheduler,omitempty"`
	Agent         *PartialAgentConfig        `json:"agent,omitempty"`
	Logging       *PartialLoggingConfig      `json:"logging,omitempty"`
	Notifications *PartialNotificationConfig `json:"notifications,omitempty"`
	Tools         *PartialToolPathsConfig    `json:"tools,omitempty"`
	Daemon        *PartialDaemonConfig       `json:"daemon,omitempty"`
	Package       *PartialPackageConfig      `json:"package,omitempty"`
	Defaults      *PartialDefaultsConfig     `json:"defaults,omitempty"`
	Projects      *[]ProjectRefConfig        `json:"projects,omitempty"`
}
