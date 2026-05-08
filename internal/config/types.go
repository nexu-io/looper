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

type DaemonRestartPolicy string

const (
	DaemonRestartNever     DaemonRestartPolicy = "never"
	DaemonRestartOnFailure DaemonRestartPolicy = "on-failure"
	DaemonRestartAlways    DaemonRestartPolicy = "always"
)

type OpenPRStrategy string

const (
	OpenPRStrategyAllDone     OpenPRStrategy = "all_done"
	OpenPRStrategyFirstCommit OpenPRStrategy = "first_commit"
	OpenPRStrategyManual      OpenPRStrategy = "manual"
)

type AddSnapshotMode string

const (
	AddSnapshotModeAsync AddSnapshotMode = "async"
	AddSnapshotModeFull  AddSnapshotMode = "full"
	AddSnapshotModeOff   AddSnapshotMode = "off"
)

type LabelMode string

const (
	LabelModeAll LabelMode = "all"
	LabelModeAny LabelMode = "any"
)

type FixerAuthorFilter string

const (
	FixerAuthorFilterCurrentUser FixerAuthorFilter = "current_user"
	FixerAuthorFilterAny         FixerAuthorFilter = "any"
)

type ReviewerScope string

const (
	ReviewerScopeFullPR        ReviewerScope = "full_pr"
	ReviewerScopeChangedFiles  ReviewerScope = "changed_files"
	ReviewerScopeChangedRanges ReviewerScope = "changed_ranges"
)

type ReviewerPublishMode string

const (
	ReviewerPublishModeSingleReview ReviewerPublishMode = "single_review"
)

type ReviewerThreadResolutionMode string

const (
	ReviewerThreadResolutionModeReportOnly        ReviewerThreadResolutionMode = "report_only"
	ReviewerThreadResolutionModeCommentOnly       ReviewerThreadResolutionMode = "comment_only"
	ReviewerThreadResolutionModeSuggestResolution ReviewerThreadResolutionMode = "suggest_resolution"
	ReviewerThreadResolutionModeResolveObjective  ReviewerThreadResolutionMode = "resolve_objective"
)

type ReviewerThreadResolutionScope string

const (
	ReviewerThreadResolutionScopeLooperAuthoredOnly ReviewerThreadResolutionScope = "looper_authored_only"
)

type ReviewerThreadResolutionAutoResolve string

const (
	ReviewerThreadResolutionAutoResolveObjectiveOnly ReviewerThreadResolutionAutoResolve = "objective_only"
)

type ReviewerReviewEvent string

const (
	ReviewerReviewEventComment        ReviewerReviewEvent = "COMMENT"
	ReviewerReviewEventApprove        ReviewerReviewEvent = "APPROVE"
	ReviewerReviewEventRequestChanges ReviewerReviewEvent = "REQUEST_CHANGES"
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
	Vendor       *AgentVendor            `json:"vendor,omitempty"`
	Model        *string                 `json:"model,omitempty"`
	Params       map[string]any          `json:"params"`
	Env          map[string]string       `json:"env"`
	Timeouts     AgentTimeoutConfig      `json:"timeouts"`
	NativeResume AgentNativeResumeConfig `json:"nativeResume"`
}

type AgentNativeResumeConfig struct {
	Enabled bool `json:"enabled"`
}

type AgentTimeoutConfig struct {
	PlannerSeconds  int `json:"plannerSeconds"`
	WorkerSeconds   int `json:"workerSeconds"`
	ReviewerSeconds int `json:"reviewerSeconds"`
	FixerSeconds    int `json:"fixerSeconds"`

	PlannerIdleTimeoutSeconds  int `json:"plannerIdleTimeoutSeconds"`
	PlannerMaxRuntimeSeconds   int `json:"plannerMaxRuntimeSeconds"`
	WorkerIdleTimeoutSeconds   int `json:"workerIdleTimeoutSeconds"`
	WorkerMaxRuntimeSeconds    int `json:"workerMaxRuntimeSeconds"`
	ReviewerIdleTimeoutSeconds int `json:"reviewerIdleTimeoutSeconds"`
	ReviewerMaxRuntimeSeconds  int `json:"reviewerMaxRuntimeSeconds"`
	FixerIdleTimeoutSeconds    int `json:"fixerIdleTimeoutSeconds"`
	FixerMaxRuntimeSeconds     int `json:"fixerMaxRuntimeSeconds"`
}

type NotificationConfig struct {
	InApp     bool                        `json:"inApp"`
	Osascript OsascriptNotificationConfig `json:"osascript"`
}

type DisclosureConfig struct {
	Enabled      bool                     `json:"enabled"`
	IncludeAgent bool                     `json:"includeAgent"`
	IncludeOS    bool                     `json:"includeOS"`
	Channels     DisclosureChannelsConfig `json:"channels"`
}

type DisclosureChannelsConfig struct {
	GitCommit            bool `json:"gitCommit"`
	PullRequest          bool `json:"pullRequest"`
	IssueComment         bool `json:"issueComment"`
	ReviewComment        bool `json:"reviewComment"`
	InlineCommentVisible bool `json:"inlineCommentVisible"`
}

type InstructionsConfig struct {
	Enabled  bool `json:"enabled"`
	MaxBytes int  `json:"maxBytes"`
}

type RoleConfig struct {
	Instructions string `json:"instructions,omitempty"`
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
	LooperPath    *string `json:"looperPath,omitempty"`
	OsascriptPath *string `json:"osascriptPath,omitempty"`
}

type ToolDetectionStatus string

const (
	ToolDetectionStatusConfigured ToolDetectionStatus = "configured"
	ToolDetectionStatusDetected   ToolDetectionStatus = "detected"
	ToolDetectionStatusMissing    ToolDetectionStatus = "missing"
)

type DaemonConfig struct {
	Mode                   DaemonMode          `json:"mode"`
	RestartPolicy          DaemonRestartPolicy `json:"restartPolicy"`
	RestartThrottleSeconds int                 `json:"restartThrottleSeconds"`
	PlistPath              *string             `json:"plistPath,omitempty"`
	LogDir                 string              `json:"logDir"`
	ShutdownTimeoutMS      int                 `json:"shutdownTimeoutMs"`
	WorkingDirectory       string              `json:"workingDirectory"`
	Environment            map[string]string   `json:"environment"`
}

type PackageConfig struct {
	Distribution               string `json:"distribution"`
	AutoUpgradeEnabled         bool   `json:"autoUpgradeEnabled"`
	AutoMigrateOnStartup       bool   `json:"autoMigrateOnStartup"`
	RequireBackupBeforeMigrate bool   `json:"requireBackupBeforeMigrate"`
}

type DefaultsConfig struct {
	BaseBranch         string          `json:"baseBranch"`
	AllowAutoCommit    bool            `json:"allowAutoCommit"`
	AllowAutoPush      bool            `json:"allowAutoPush"`
	AllowAutoApprove   bool            `json:"allowAutoApprove"`
	AllowAutoMerge     bool            `json:"allowAutoMerge"`
	AllowRiskyFixes    bool            `json:"allowRiskyFixes"`
	FixAllPullRequests bool            `json:"fixAllPullRequests"`
	OpenPRStrategy     OpenPRStrategy  `json:"openPrStrategy"`
	AddSnapshotMode    AddSnapshotMode `json:"addSnapshotMode"`
}

type ReviewerLoopConfig struct {
	EnabledByDefault          bool `json:"enabledByDefault"`
	QuietPeriodSeconds        int  `json:"quietPeriodSeconds"`
	MinPublishIntervalSeconds int  `json:"minPublishIntervalSeconds"`
	MaxIterationsPerPR        int  `json:"maxIterationsPerPR"`
	MaxIterationsPerHead      int  `json:"maxIterationsPerHead"`
	MaxWallClockSeconds       int  `json:"maxWallClockSeconds"`
	MaxConsecutiveFailures    int  `json:"maxConsecutiveFailures"`
	MaxAgentExecutionsPerPR   int  `json:"maxAgentExecutionsPerPR"`
	StopOnApproved            bool `json:"stopOnApproved"`
	StopOnReadyLabel          bool `json:"stopOnReadyLabel"`
	StopOnIdenticalOutput     bool `json:"stopOnIdenticalOutput"`
}

type ReviewerConfig struct {
	Loop                    ReviewerLoopConfig             `json:"loop"`
	Scope                   ReviewerScope                  `json:"scope"`
	PublishMode             ReviewerPublishMode            `json:"publishMode"`
	ReviewEvents            ReviewerReviewEventsConfig     `json:"reviewEvents"`
	DetectDuplicateFindings bool                           `json:"detectDuplicateFindings"`
	ThreadResolution        ReviewerThreadResolutionConfig `json:"threadResolution"`
}

type ReviewerReviewEventsConfig struct {
	Clean    ReviewerReviewEvent `json:"clean"`
	Blocking ReviewerReviewEvent `json:"blocking"`
}

type ReviewerThreadResolutionConfig struct {
	Enabled                     bool                                `json:"enabled"`
	Mode                        ReviewerThreadResolutionMode        `json:"mode"`
	Scope                       ReviewerThreadResolutionScope       `json:"scope"`
	AutoResolve                 ReviewerThreadResolutionAutoResolve `json:"autoResolve"`
	RequireAuditComment         bool                                `json:"requireAuditComment"`
	RequireNewHeadSinceThread   bool                                `json:"requireNewHeadSinceThread"`
	RequireCurrentReviewRequest bool                                `json:"requireCurrentReviewRequest"`
	MaxThreadsPerRun            int                                 `json:"maxThreadsPerRun"`
}

type IssueRoleTriggersConfig struct {
	Labels                     []string  `json:"labels"`
	LabelMode                  LabelMode `json:"labelMode"`
	RequireAssigneeCurrentUser bool      `json:"requireAssigneeCurrentUser"`
}

type PullRequestRoleTriggersConfig struct {
	IncludeDrafts        bool `json:"includeDrafts"`
	RequireReviewRequest bool `json:"requireReviewRequest"`
}

type ReviewerRoleTriggersConfig struct {
	IncludeDrafts        bool      `json:"includeDrafts"`
	RequireReviewRequest bool      `json:"requireReviewRequest"`
	EnableSelfReview     bool      `json:"enableSelfReview,omitempty"`
	Labels               []string  `json:"labels"`
	LabelMode            LabelMode `json:"labelMode"`
}

type ReviewerSpecReviewConfig struct {
	IncludeReviewingLabel bool   `json:"includeReviewingLabel"`
	ReviewingLabel        string `json:"reviewingLabel"`
}

type FixerRoleTriggersConfig struct {
	IncludeDrafts bool              `json:"includeDrafts"`
	AuthorFilter  FixerAuthorFilter `json:"authorFilter"`
	Labels        []string          `json:"labels"`
	LabelMode     LabelMode         `json:"labelMode"`
}

type PlannerRoleConfig struct {
	AutoDiscovery bool                    `json:"autoDiscovery"`
	Triggers      IssueRoleTriggersConfig `json:"triggers"`
	Instructions  string                  `json:"instructions,omitempty"`
}

type WorkerRoleConfig struct {
	AutoDiscovery bool                    `json:"autoDiscovery"`
	Triggers      IssueRoleTriggersConfig `json:"triggers"`
	Instructions  string                  `json:"instructions,omitempty"`
}

type ReviewerRoleConfig struct {
	AutoDiscovery bool                       `json:"autoDiscovery"`
	Triggers      ReviewerRoleTriggersConfig `json:"triggers"`
	SpecReview    ReviewerSpecReviewConfig   `json:"specReview"`
	Instructions  string                     `json:"instructions,omitempty"`
}

type FixerRoleConfig struct {
	AutoDiscovery bool                    `json:"autoDiscovery"`
	Triggers      FixerRoleTriggersConfig `json:"triggers"`
	Instructions  string                  `json:"instructions,omitempty"`
}

type SweeperCategoryConfig struct {
	Enabled         bool `json:"enabled"`
	InactivityDays  int  `json:"inactivityDays,omitempty"`
	GracePeriodDays int  `json:"gracePeriodDays"`
	MinConfidence   int  `json:"minConfidence"`
}

type SweeperTriggersConfig struct {
	IncludeIssues             bool     `json:"includeIssues"`
	IncludePullRequests       bool     `json:"includePullRequests"`
	IncludeDrafts             bool     `json:"includeDrafts"`
	ExcludeLabels             []string `json:"excludeLabels"`
	ExcludeAuthors            []string `json:"excludeAuthors"`
	ExcludeAuthorAssociations []string `json:"excludeAuthorAssociations"`
	LooperInternalLabels      []string `json:"looperInternalLabels"`
	ReopenCooldownDays        int      `json:"reopenCooldownDays"`
	MaxPerTick                int      `json:"maxPerTick"`
}

type SweeperLimitsConfig struct {
	MaxWarningsPerRepoPerDay int  `json:"maxWarningsPerRepoPerDay"`
	MaxClosesPerRepoPerDay   int  `json:"maxClosesPerRepoPerDay"`
	GlobalKillSwitch         bool `json:"globalKillSwitch"`
}

type SweeperSecurityConfig struct {
	QuarantineLabel string   `json:"quarantineLabel"`
	NotifyAssignees []string `json:"notifyAssignees"`
}

type SweeperReportingConfig struct {
	DurableReportsDir string `json:"durableReportsDir"`
}

type SweeperLifecycleConfig struct {
	PendingLabel string `json:"pendingLabel"`
	ClosedLabel  string `json:"closedLabel"`
	KeepLabel    string `json:"keepLabel"`
}

type SweeperCategoriesConfig struct {
	Stale        SweeperCategoryConfig `json:"stale"`
	AlreadyFixed SweeperCategoryConfig `json:"alreadyFixed"`
	Unrelated    SweeperCategoryConfig `json:"unrelated"`
	Superseded   SweeperCategoryConfig `json:"superseded"`
	AbandonedPR  SweeperCategoryConfig `json:"abandonedPR"`
}

type SweeperRoleConfig struct {
	AutoDiscovery bool                    `json:"autoDiscovery"`
	DryRun        bool                    `json:"dryRun"`
	Triggers      SweeperTriggersConfig   `json:"triggers"`
	Lifecycle     SweeperLifecycleConfig  `json:"lifecycle"`
	Limits        SweeperLimitsConfig     `json:"limits"`
	Categories    SweeperCategoriesConfig `json:"categories"`
	Security      SweeperSecurityConfig   `json:"security"`
	Reporting     SweeperReportingConfig  `json:"reporting"`
	Instructions  string                  `json:"instructions,omitempty"`
}

type RoleConfigs struct {
	Planner  PlannerRoleConfig  `json:"planner"`
	Reviewer ReviewerRoleConfig `json:"reviewer"`
	Fixer    FixerRoleConfig    `json:"fixer"`
	Worker   WorkerRoleConfig   `json:"worker"`
	Sweeper  SweeperRoleConfig  `json:"sweeper"`
}

type ProjectRefConfig struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	RepoPath     string              `json:"repoPath"`
	Path         string              `json:"path,omitempty"`
	BaseBranch   *string             `json:"baseBranch,omitempty"`
	WorktreeRoot *string             `json:"worktreeRoot,omitempty"`
	Instructions map[string]string   `json:"instructions,omitempty"`
	Roles        *PartialRoleConfigs `json:"roles,omitempty"`
}

type Config struct {
	Server        ServerConfig       `json:"server"`
	Storage       StorageConfig      `json:"storage"`
	Scheduler     SchedulerConfig    `json:"scheduler"`
	Agent         AgentConfig        `json:"agent"`
	Logging       LoggingConfig      `json:"logging"`
	Notifications NotificationConfig `json:"notifications"`
	Disclosure    DisclosureConfig   `json:"disclosure"`
	Tools         ToolPathsConfig    `json:"tools"`
	Daemon        DaemonConfig       `json:"daemon"`
	Package       PackageConfig      `json:"package"`
	Defaults      DefaultsConfig     `json:"defaults"`
	Reviewer      ReviewerConfig     `json:"reviewer"`
	Instructions  InstructionsConfig `json:"instructions"`
	Roles         RoleConfigs        `json:"roles"`
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
	Vendor       *AgentVendor                    `json:"vendor,omitempty"`
	Model        *string                         `json:"model,omitempty"`
	Params       map[string]any                  `json:"params,omitempty"`
	Env          map[string]string               `json:"env,omitempty"`
	Timeouts     *PartialAgentTimeoutConfig      `json:"timeouts,omitempty"`
	NativeResume *PartialAgentNativeResumeConfig `json:"nativeResume,omitempty"`
}

type PartialAgentNativeResumeConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

type PartialAgentTimeoutConfig struct {
	PlannerSeconds  *int `json:"plannerSeconds,omitempty"`
	WorkerSeconds   *int `json:"workerSeconds,omitempty"`
	ReviewerSeconds *int `json:"reviewerSeconds,omitempty"`
	FixerSeconds    *int `json:"fixerSeconds,omitempty"`

	PlannerIdleTimeoutSeconds  *int `json:"plannerIdleTimeoutSeconds,omitempty"`
	PlannerMaxRuntimeSeconds   *int `json:"plannerMaxRuntimeSeconds,omitempty"`
	WorkerIdleTimeoutSeconds   *int `json:"workerIdleTimeoutSeconds,omitempty"`
	WorkerMaxRuntimeSeconds    *int `json:"workerMaxRuntimeSeconds,omitempty"`
	ReviewerIdleTimeoutSeconds *int `json:"reviewerIdleTimeoutSeconds,omitempty"`
	ReviewerMaxRuntimeSeconds  *int `json:"reviewerMaxRuntimeSeconds,omitempty"`
	FixerIdleTimeoutSeconds    *int `json:"fixerIdleTimeoutSeconds,omitempty"`
	FixerMaxRuntimeSeconds     *int `json:"fixerMaxRuntimeSeconds,omitempty"`
}

type PartialNotificationConfig struct {
	InApp     *bool                               `json:"inApp,omitempty"`
	Osascript *PartialOsascriptNotificationConfig `json:"osascript,omitempty"`
}

type PartialDisclosureConfig struct {
	Enabled      *bool                            `json:"enabled,omitempty"`
	IncludeAgent *bool                            `json:"includeAgent,omitempty"`
	IncludeOS    *bool                            `json:"includeOS,omitempty"`
	Channels     *PartialDisclosureChannelsConfig `json:"channels,omitempty"`
}

type PartialDisclosureChannelsConfig struct {
	GitCommit            *bool `json:"gitCommit,omitempty"`
	PullRequest          *bool `json:"pullRequest,omitempty"`
	IssueComment         *bool `json:"issueComment,omitempty"`
	ReviewComment        *bool `json:"reviewComment,omitempty"`
	InlineCommentVisible *bool `json:"inlineCommentVisible,omitempty"`
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
	LooperPath    *string `json:"looperPath,omitempty"`
	OsascriptPath *string `json:"osascriptPath,omitempty"`
}

type PartialDaemonConfig struct {
	Mode                   *DaemonMode          `json:"mode,omitempty"`
	RestartPolicy          *DaemonRestartPolicy `json:"restartPolicy,omitempty"`
	RestartThrottleSeconds *int                 `json:"restartThrottleSeconds,omitempty"`
	PlistPath              *string              `json:"plistPath,omitempty"`
	LogDir                 *string              `json:"logDir,omitempty"`
	ShutdownTimeoutMS      *int                 `json:"shutdownTimeoutMs,omitempty"`
	WorkingDirectory       *string              `json:"workingDirectory,omitempty"`
	Environment            map[string]string    `json:"environment,omitempty"`
}

type PartialPackageConfig struct {
	Distribution               *string `json:"distribution,omitempty"`
	AutoUpgradeEnabled         *bool   `json:"autoUpgradeEnabled,omitempty"`
	AutoMigrateOnStartup       *bool   `json:"autoMigrateOnStartup,omitempty"`
	RequireBackupBeforeMigrate *bool   `json:"requireBackupBeforeMigrate,omitempty"`
}

type PartialDefaultsConfig struct {
	BaseBranch         *string          `json:"baseBranch,omitempty"`
	AllowAutoCommit    *bool            `json:"allowAutoCommit,omitempty"`
	AllowAutoPush      *bool            `json:"allowAutoPush,omitempty"`
	AllowAutoApprove   *bool            `json:"allowAutoApprove,omitempty"`
	AllowAutoMerge     *bool            `json:"allowAutoMerge,omitempty"`
	AllowRiskyFixes    *bool            `json:"allowRiskyFixes,omitempty"`
	FixAllPullRequests *bool            `json:"fixAllPullRequests,omitempty"`
	OpenPRStrategy     *OpenPRStrategy  `json:"openPrStrategy,omitempty"`
	AddSnapshotMode    *AddSnapshotMode `json:"addSnapshotMode,omitempty"`
}

type PartialReviewerLoopConfig struct {
	EnabledByDefault          *bool `json:"enabledByDefault,omitempty"`
	QuietPeriodSeconds        *int  `json:"quietPeriodSeconds,omitempty"`
	MinPublishIntervalSeconds *int  `json:"minPublishIntervalSeconds,omitempty"`
	MaxIterationsPerPR        *int  `json:"maxIterationsPerPR,omitempty"`
	MaxIterationsPerHead      *int  `json:"maxIterationsPerHead,omitempty"`
	MaxWallClockSeconds       *int  `json:"maxWallClockSeconds,omitempty"`
	MaxConsecutiveFailures    *int  `json:"maxConsecutiveFailures,omitempty"`
	MaxAgentExecutionsPerPR   *int  `json:"maxAgentExecutionsPerPR,omitempty"`
	StopOnApproved            *bool `json:"stopOnApproved,omitempty"`
	StopOnReadyLabel          *bool `json:"stopOnReadyLabel,omitempty"`
	StopOnIdenticalOutput     *bool `json:"stopOnIdenticalOutput,omitempty"`
}

type PartialReviewerConfig struct {
	Loop                    *PartialReviewerLoopConfig             `json:"loop,omitempty"`
	Scope                   *ReviewerScope                         `json:"scope,omitempty"`
	PublishMode             *ReviewerPublishMode                   `json:"publishMode,omitempty"`
	ReviewEvents            *PartialReviewerReviewEventsConfig     `json:"reviewEvents,omitempty"`
	DetectDuplicateFindings *bool                                  `json:"detectDuplicateFindings,omitempty"`
	DedupeFindings          *bool                                  `json:"dedupeFindings,omitempty"`
	ThreadResolution        *PartialReviewerThreadResolutionConfig `json:"threadResolution,omitempty"`
}

type PartialReviewerReviewEventsConfig struct {
	Clean    *ReviewerReviewEvent `json:"clean,omitempty"`
	Blocking *ReviewerReviewEvent `json:"blocking,omitempty"`
}

type PartialReviewerThreadResolutionConfig struct {
	Enabled                     *bool                                `json:"enabled,omitempty"`
	Mode                        *ReviewerThreadResolutionMode        `json:"mode,omitempty"`
	Scope                       *ReviewerThreadResolutionScope       `json:"scope,omitempty"`
	AutoResolve                 *ReviewerThreadResolutionAutoResolve `json:"autoResolve,omitempty"`
	RequireAuditComment         *bool                                `json:"requireAuditComment,omitempty"`
	RequireNewHeadSinceThread   *bool                                `json:"requireNewHeadSinceThread,omitempty"`
	RequireCurrentReviewRequest *bool                                `json:"requireCurrentReviewRequest,omitempty"`
	MaxThreadsPerRun            *int                                 `json:"maxThreadsPerRun,omitempty"`
}

type PartialInstructionsConfig struct {
	Enabled  *bool `json:"enabled,omitempty"`
	MaxBytes *int  `json:"maxBytes,omitempty"`
}

type PartialIssueRoleTriggersConfig struct {
	Labels                     *[]string  `json:"labels,omitempty"`
	LabelMode                  *LabelMode `json:"labelMode,omitempty"`
	RequireAssigneeCurrentUser *bool      `json:"requireAssigneeCurrentUser,omitempty"`
}

type PartialPullRequestRoleTriggersConfig struct {
	IncludeDrafts        *bool `json:"includeDrafts,omitempty"`
	RequireReviewRequest *bool `json:"requireReviewRequest,omitempty"`
}

type PartialReviewerRoleTriggersConfig struct {
	IncludeDrafts        *bool      `json:"includeDrafts,omitempty"`
	RequireReviewRequest *bool      `json:"requireReviewRequest,omitempty"`
	EnableSelfReview     *bool      `json:"enableSelfReview,omitempty"`
	Labels               *[]string  `json:"labels,omitempty"`
	LabelMode            *LabelMode `json:"labelMode,omitempty"`
}

type PartialReviewerSpecReviewConfig struct {
	IncludeReviewingLabel *bool   `json:"includeReviewingLabel,omitempty"`
	ReviewingLabel        *string `json:"reviewingLabel,omitempty"`
}

type PartialFixerRoleTriggersConfig struct {
	IncludeDrafts *bool              `json:"includeDrafts,omitempty"`
	AuthorFilter  *FixerAuthorFilter `json:"authorFilter,omitempty"`
	Labels        *[]string          `json:"labels,omitempty"`
	LabelMode     *LabelMode         `json:"labelMode,omitempty"`
}

type PartialSweeperCategoryConfig struct {
	Enabled         *bool `json:"enabled,omitempty"`
	InactivityDays  *int  `json:"inactivityDays,omitempty"`
	GracePeriodDays *int  `json:"gracePeriodDays,omitempty"`
	MinConfidence   *int  `json:"minConfidence,omitempty"`
}

type PartialSweeperTriggersConfig struct {
	IncludeIssues             *bool     `json:"includeIssues,omitempty"`
	IncludePullRequests       *bool     `json:"includePullRequests,omitempty"`
	IncludeDrafts             *bool     `json:"includeDrafts,omitempty"`
	ExcludeLabels             *[]string `json:"excludeLabels,omitempty"`
	ExcludeAuthors            *[]string `json:"excludeAuthors,omitempty"`
	ExcludeAuthorAssociations *[]string `json:"excludeAuthorAssociations,omitempty"`
	LooperInternalLabels      *[]string `json:"looperInternalLabels,omitempty"`
	ReopenCooldownDays        *int      `json:"reopenCooldownDays,omitempty"`
	MaxPerTick                *int      `json:"maxPerTick,omitempty"`
}

type PartialSweeperLimitsConfig struct {
	MaxWarningsPerRepoPerDay *int  `json:"maxWarningsPerRepoPerDay,omitempty"`
	MaxClosesPerRepoPerDay   *int  `json:"maxClosesPerRepoPerDay,omitempty"`
	GlobalKillSwitch         *bool `json:"globalKillSwitch,omitempty"`
}

type PartialSweeperSecurityConfig struct {
	QuarantineLabel *string   `json:"quarantineLabel,omitempty"`
	NotifyAssignees *[]string `json:"notifyAssignees,omitempty"`
}

type PartialSweeperReportingConfig struct {
	DurableReportsDir *string `json:"durableReportsDir,omitempty"`
}

type PartialSweeperLifecycleConfig struct {
	PendingLabel *string `json:"pendingLabel,omitempty"`
	ClosedLabel  *string `json:"closedLabel,omitempty"`
	KeepLabel    *string `json:"keepLabel,omitempty"`
}

type PartialSweeperCategoriesConfig struct {
	Stale        *PartialSweeperCategoryConfig `json:"stale,omitempty"`
	AlreadyFixed *PartialSweeperCategoryConfig `json:"alreadyFixed,omitempty"`
	Unrelated    *PartialSweeperCategoryConfig `json:"unrelated,omitempty"`
	Superseded   *PartialSweeperCategoryConfig `json:"superseded,omitempty"`
	AbandonedPR  *PartialSweeperCategoryConfig `json:"abandonedPR,omitempty"`
}

type PartialPlannerRoleConfig struct {
	AutoDiscovery *bool                           `json:"autoDiscovery,omitempty"`
	Triggers      *PartialIssueRoleTriggersConfig `json:"triggers,omitempty"`
	Instructions  *string                         `json:"instructions,omitempty"`
}

type PartialWorkerRoleConfig struct {
	AutoDiscovery *bool                           `json:"autoDiscovery,omitempty"`
	Triggers      *PartialIssueRoleTriggersConfig `json:"triggers,omitempty"`
	Instructions  *string                         `json:"instructions,omitempty"`
}

type PartialReviewerRoleConfig struct {
	AutoDiscovery *bool                              `json:"autoDiscovery,omitempty"`
	Triggers      *PartialReviewerRoleTriggersConfig `json:"triggers,omitempty"`
	SpecReview    *PartialReviewerSpecReviewConfig   `json:"specReview,omitempty"`
	Instructions  *string                            `json:"instructions,omitempty"`
}

type PartialFixerRoleConfig struct {
	AutoDiscovery *bool                           `json:"autoDiscovery,omitempty"`
	Triggers      *PartialFixerRoleTriggersConfig `json:"triggers,omitempty"`
	Instructions  *string                         `json:"instructions,omitempty"`
}

type PartialSweeperRoleConfig struct {
	AutoDiscovery *bool                           `json:"autoDiscovery,omitempty"`
	DryRun        *bool                           `json:"dryRun,omitempty"`
	Triggers      *PartialSweeperTriggersConfig   `json:"triggers,omitempty"`
	Lifecycle     *PartialSweeperLifecycleConfig  `json:"lifecycle,omitempty"`
	Limits        *PartialSweeperLimitsConfig     `json:"limits,omitempty"`
	Categories    *PartialSweeperCategoriesConfig `json:"categories,omitempty"`
	Security      *PartialSweeperSecurityConfig   `json:"security,omitempty"`
	Reporting     *PartialSweeperReportingConfig  `json:"reporting,omitempty"`
	Instructions  *string                         `json:"instructions,omitempty"`
}

type PartialRoleConfigs struct {
	Planner  *PartialPlannerRoleConfig  `json:"planner,omitempty"`
	Reviewer *PartialReviewerRoleConfig `json:"reviewer,omitempty"`
	Fixer    *PartialFixerRoleConfig    `json:"fixer,omitempty"`
	Worker   *PartialWorkerRoleConfig   `json:"worker,omitempty"`
	Sweeper  *PartialSweeperRoleConfig  `json:"sweeper,omitempty"`
}

type PartialConfig struct {
	Server        *PartialServerConfig       `json:"server,omitempty"`
	Storage       *PartialStorageConfig      `json:"storage,omitempty"`
	Scheduler     *PartialSchedulerConfig    `json:"scheduler,omitempty"`
	Agent         *PartialAgentConfig        `json:"agent,omitempty"`
	Logging       *PartialLoggingConfig      `json:"logging,omitempty"`
	Notifications *PartialNotificationConfig `json:"notifications,omitempty"`
	Disclosure    *PartialDisclosureConfig   `json:"disclosure,omitempty"`
	Tools         *PartialToolPathsConfig    `json:"tools,omitempty"`
	Daemon        *PartialDaemonConfig       `json:"daemon,omitempty"`
	Package       *PartialPackageConfig      `json:"package,omitempty"`
	Defaults      *PartialDefaultsConfig     `json:"defaults,omitempty"`
	Reviewer      *PartialReviewerConfig     `json:"reviewer,omitempty"`
	Instructions  *PartialInstructionsConfig `json:"instructions,omitempty"`
	Roles         *PartialRoleConfigs        `json:"roles,omitempty"`
	Projects      *[]ProjectRefConfig        `json:"projects,omitempty"`
}
