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

type ReviewerAutoMergeStrategy string

const (
	ReviewerAutoMergeStrategySquash ReviewerAutoMergeStrategy = "squash"
	ReviewerAutoMergeStrategyMerge  ReviewerAutoMergeStrategy = "merge"
	ReviewerAutoMergeStrategyRebase ReviewerAutoMergeStrategy = "rebase"
)

type ReviewerAutoMergeScope string

const (
	ReviewerAutoMergeScopeLooperOnly ReviewerAutoMergeScope = "looper-only"
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
	PollIntervalSeconds     int `json:"pollIntervalSeconds"`
	MaxConcurrentRuns       int `json:"maxConcurrentRuns"`
	RetryMaxAttempts        int `json:"retryMaxAttempts"`
	RetryBaseDelayMS        int `json:"retryBaseDelayMs"`
	SlowLaneWarnThresholdMS int `json:"slowLaneWarnThresholdMs"`
}

type WebhookConfig struct {
	Enabled                     bool        `json:"enabled"`
	Mode                        WebhookMode `json:"mode"`
	ListenPort                  int         `json:"listenPort"`
	PublicBaseURL               string      `json:"publicBaseUrl"`
	FallbackPollIntervalSeconds int         `json:"fallbackPollIntervalSeconds"`
}

type WebhookMode string

const (
	WebhookModeGHForward WebhookMode = "gh-forward"
	WebhookModeTunnel    WebhookMode = "tunnel"
)

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
	Mode                   DaemonMode            `json:"mode"`
	RestartPolicy          DaemonRestartPolicy   `json:"restartPolicy"`
	RestartThrottleSeconds int                   `json:"restartThrottleSeconds"`
	PlistPath              *string               `json:"plistPath,omitempty"`
	LogDir                 string                `json:"logDir"`
	ShutdownTimeoutMS      int                   `json:"shutdownTimeoutMs"`
	WorkingDirectory       string                `json:"workingDirectory"`
	Environment            map[string]string     `json:"environment"`
	WorktreeCleanup        WorktreeCleanupConfig `json:"worktreeCleanup"`
}

type WorktreeCleanupConfig struct {
	Enabled        bool   `json:"enabled"`
	Interval       string `json:"interval"`
	RetentionDays  int    `json:"retentionDays"`
	MaxPerTick     int    `json:"maxPerTick"`
	IncludeOrphans bool   `json:"includeOrphans"`
	DryRun         bool   `json:"dryRun"`
}

type PackageConfig struct {
	Distribution               string `json:"distribution"`
	AutoUpgradeEnabled         bool   `json:"autoUpgradeEnabled"`
	AutoMigrateOnStartup       bool   `json:"autoMigrateOnStartup"`
	RequireBackupBeforeMigrate bool   `json:"requireBackupBeforeMigrate"`
}

type NetworkMode string

const (
	NetworkModeOff    NetworkMode = "off"
	NetworkModeRouted NetworkMode = "routed"
)

type ProjectNetworkMode = NetworkMode

const (
	ProjectNetworkModeOff    = NetworkModeOff
	ProjectNetworkModeRouted = NetworkModeRouted
)

type NetworkConfig struct {
	Enrolled         bool   `json:"enrolled"`
	LoopernetBaseURL string `json:"loopernetBaseUrl"`
	NodeName         string `json:"nodeName"`
	GitHubLogin      string `json:"githubLogin"`
	GitHubUserID     int64  `json:"githubUserId,omitempty"`
}

type ProjectNetworkConfig struct {
	Mode NetworkMode `json:"mode,omitempty"`
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
	NativeResume            ReviewerNativeResumeConfig     `json:"nativeResume"`
	ThreadResolution        ReviewerThreadResolutionConfig `json:"threadResolution"`
}

type ReviewerReviewEventsConfig struct {
	Clean    ReviewerReviewEvent `json:"clean"`
	Blocking ReviewerReviewEvent `json:"blocking"`
}

type ReviewerNativeResumeConfig struct {
	OnHeadChange               bool `json:"onHeadChange"`
	ReReviewPromptOnHeadChange bool `json:"reReviewPromptOnHeadChange"`
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

type ReviewerAutoMergeConfig struct {
	Enabled                 bool                      `json:"enabled"`
	Strategy                ReviewerAutoMergeStrategy `json:"strategy"`
	RequireBranchProtection bool                      `json:"requireBranchProtection"`
	TransientRetries        int                       `json:"transientRetries"`
	Scope                   ReviewerAutoMergeScope    `json:"scope"`
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

type ReviewerRoleDiscoveryConfig struct {
	AutoDiscovery bool                       `json:"autoDiscovery"`
	Triggers      ReviewerRoleTriggersConfig `json:"triggers"`
	SpecReview    ReviewerSpecReviewConfig   `json:"specReview"`
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
	Discovery    ReviewerRoleDiscoveryConfig `json:"discovery"`
	Behavior     ReviewerConfig              `json:"behavior"`
	AutoMerge    ReviewerAutoMergeConfig     `json:"autoMerge"`
	Instructions string                      `json:"instructions,omitempty"`
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

type SweeperProposerMode string

const (
	SweeperProposerModeAgentApply        SweeperProposerMode = "agent_apply"
	SweeperProposerModeHeuristicFallback SweeperProposerMode = "heuristic_fallback"
)

type SweeperFilterMode string

const (
	SweeperFilterModeDeterministic SweeperFilterMode = "deterministic"
)

type SweeperProposerConfig struct {
	Mode                        SweeperProposerMode `json:"mode"`
	Model                       *string             `json:"model,omitempty"`
	TimeoutSeconds              int                 `json:"timeoutSeconds"`
	SchemaVersion               int                 `json:"schemaVersion"`
	DiagnosticMode              bool                `json:"diagnosticMode"`
	TimeoutRateDryRunThreshold  float64             `json:"timeoutRateDryRunThreshold"`
	TimeoutRateDryRunMinSamples int                 `json:"timeoutRateDryRunMinSamples"`
}

type SweeperFilterConfig struct {
	Mode SweeperFilterMode `json:"mode"`
}

type SweeperRoleConfig struct {
	AutoDiscovery bool                    `json:"autoDiscovery"`
	DryRun        bool                    `json:"dryRun"`
	Triggers      SweeperTriggersConfig   `json:"triggers"`
	Filter        SweeperFilterConfig     `json:"filter"`
	Proposer      SweeperProposerConfig   `json:"proposer"`
	Lifecycle     SweeperLifecycleConfig  `json:"lifecycle"`
	Limits        SweeperLimitsConfig     `json:"limits"`
	Categories    SweeperCategoriesConfig `json:"categories"`
	Security      SweeperSecurityConfig   `json:"security"`
	Reporting     SweeperReportingConfig  `json:"reporting"`
	Instructions  string                  `json:"instructions,omitempty"`
}

type CoordinatorTriageDispositionConfig struct {
	OutOfScopeLabel       string `json:"outOfScopeLabel"`
	UnclearLabel          string `json:"unclearLabel"`
	ReTriageOnAuthorReply bool   `json:"reTriageOnAuthorReply"`
}

type CoordinatorTriageConfig struct {
	TriagedLabel    string                             `json:"triagedLabel"`
	MaxIssueAgeDays int                                `json:"maxIssueAgeDays"`
	MaxPerTick      int                                `json:"maxPerTick"`
	Disposition     CoordinatorTriageDispositionConfig `json:"disposition"`
}

type CoordinatorDispatchHumanGateConfig struct {
	SlashCommands []string `json:"slashCommands"`
	AllowedUsers  []string `json:"allowedUsers"`
}

type CoordinatorDispatchAutonomousConfig struct {
	DelayMinutes int    `json:"delayMinutes"`
	HoldLabel    string `json:"holdLabel"`
}

type CoordinatorDispatchConfig struct {
	Mode       string                              `json:"mode"`
	HumanGate  CoordinatorDispatchHumanGateConfig  `json:"humanGate"`
	Autonomous CoordinatorDispatchAutonomousConfig `json:"autonomous"`
	AssignTo   string                              `json:"assignTo"`
}

type CoordinatorDependenciesConfig struct {
	Enabled           bool `json:"enabled"`
	APITimeoutSeconds int  `json:"apiTimeoutSeconds"`
	APIRetryAttempts  int  `json:"apiRetryAttempts"`
}

type CoordinatorMergeWatchConfig struct {
	TransientRetries         int    `json:"transientRetries"`
	MaxIndeterminateDuration string `json:"maxIndeterminateDuration"`
}

type CoordinatorRoleConfig struct {
	Enabled      bool                          `json:"enabled"`
	PollInterval string                        `json:"pollInterval"`
	Triage       CoordinatorTriageConfig       `json:"triage"`
	Dispatch     CoordinatorDispatchConfig     `json:"dispatch"`
	Dependencies CoordinatorDependenciesConfig `json:"dependencies"`
	MergeWatch   CoordinatorMergeWatchConfig   `json:"mergeWatch"`
}

type RoleConfigs struct {
	Planner     PlannerRoleConfig     `json:"planner"`
	Reviewer    ReviewerRoleConfig    `json:"reviewer"`
	Fixer       FixerRoleConfig       `json:"fixer"`
	Worker      WorkerRoleConfig      `json:"worker"`
	Sweeper     SweeperRoleConfig     `json:"sweeper"`
	Coordinator CoordinatorRoleConfig `json:"coordinator"`
}

type ProjectRefConfig struct {
	ID           string               `json:"id"`
	Name         string               `json:"name"`
	RepoPath     string               `json:"repoPath"`
	Path         string               `json:"path,omitempty"`
	BaseBranch   *string              `json:"baseBranch,omitempty"`
	WorktreeRoot *string              `json:"worktreeRoot,omitempty"`
	Network      ProjectNetworkConfig `json:"network,omitempty"`
	Webhook      ProjectWebhookConfig `json:"webhook,omitempty"`
	Roles        *PartialRoleConfigs  `json:"roles,omitempty"`
}

type ProjectWebhookConfig struct {
	Mode WebhookMode `json:"mode,omitempty"`
}

type PartialProjectRefConfig struct {
	ID           string                       `json:"id"`
	Name         string                       `json:"name"`
	RepoPath     string                       `json:"repoPath"`
	Path         string                       `json:"path,omitempty"`
	BaseBranch   *string                      `json:"baseBranch,omitempty"`
	WorktreeRoot *string                      `json:"worktreeRoot,omitempty"`
	Network      *PartialProjectNetworkConfig `json:"network,omitempty"`
	Webhook      *PartialProjectWebhookConfig `json:"webhook,omitempty"`
	Instructions map[string]string            `json:"instructions,omitempty"`
	Roles        *PartialRoleConfigs          `json:"roles,omitempty"`
}

type PartialProjectNetworkConfig struct {
	Mode *NetworkMode `json:"mode,omitempty"`
}

type PartialProjectWebhookConfig struct {
	Mode *WebhookMode `json:"mode,omitempty"`
}

type Config struct {
	Server        ServerConfig       `json:"server"`
	Storage       StorageConfig      `json:"storage"`
	Scheduler     SchedulerConfig    `json:"scheduler"`
	Webhook       WebhookConfig      `json:"webhook"`
	Network       NetworkConfig      `json:"network"`
	Agent         AgentConfig        `json:"agent"`
	Logging       LoggingConfig      `json:"logging"`
	Notifications NotificationConfig `json:"notifications"`
	Disclosure    DisclosureConfig   `json:"disclosure"`
	Tools         ToolPathsConfig    `json:"tools"`
	Daemon        DaemonConfig       `json:"daemon"`
	Package       PackageConfig      `json:"package"`
	Defaults      DefaultsConfig     `json:"defaults"`
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
	PollIntervalSeconds     *int `json:"pollIntervalSeconds,omitempty"`
	MaxConcurrentRuns       *int `json:"maxConcurrentRuns,omitempty"`
	RetryMaxAttempts        *int `json:"retryMaxAttempts,omitempty"`
	RetryBaseDelayMS        *int `json:"retryBaseDelayMs,omitempty"`
	SlowLaneWarnThresholdMS *int `json:"slowLaneWarnThresholdMs,omitempty"`
}

type PartialWebhookConfig struct {
	Enabled                     *bool        `json:"enabled,omitempty"`
	Mode                        *WebhookMode `json:"mode,omitempty"`
	ListenPort                  *int         `json:"listenPort,omitempty"`
	PublicBaseURL               *string      `json:"publicBaseUrl,omitempty"`
	FallbackPollIntervalSeconds *int         `json:"fallbackPollIntervalSeconds,omitempty"`
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
	Mode                   *DaemonMode                   `json:"mode,omitempty"`
	RestartPolicy          *DaemonRestartPolicy          `json:"restartPolicy,omitempty"`
	RestartThrottleSeconds *int                          `json:"restartThrottleSeconds,omitempty"`
	PlistPath              *string                       `json:"plistPath,omitempty"`
	LogDir                 *string                       `json:"logDir,omitempty"`
	ShutdownTimeoutMS      *int                          `json:"shutdownTimeoutMs,omitempty"`
	WorkingDirectory       *string                       `json:"workingDirectory,omitempty"`
	Environment            map[string]string             `json:"environment,omitempty"`
	WorktreeCleanup        *PartialWorktreeCleanupConfig `json:"worktreeCleanup,omitempty"`
}

type PartialWorktreeCleanupConfig struct {
	Enabled        *bool   `json:"enabled,omitempty"`
	Interval       *string `json:"interval,omitempty"`
	RetentionDays  *int    `json:"retentionDays,omitempty"`
	MaxPerTick     *int    `json:"maxPerTick,omitempty"`
	IncludeOrphans *bool   `json:"includeOrphans,omitempty"`
	DryRun         *bool   `json:"dryRun,omitempty"`
}

type PartialPackageConfig struct {
	Distribution               *string `json:"distribution,omitempty"`
	AutoUpgradeEnabled         *bool   `json:"autoUpgradeEnabled,omitempty"`
	AutoMigrateOnStartup       *bool   `json:"autoMigrateOnStartup,omitempty"`
	RequireBackupBeforeMigrate *bool   `json:"requireBackupBeforeMigrate,omitempty"`
}

type PartialNetworkConfig struct {
	Enrolled         *bool   `json:"enrolled,omitempty"`
	LoopernetBaseURL *string `json:"loopernetBaseUrl,omitempty"`
	NodeName         *string `json:"nodeName,omitempty"`
	GitHubLogin      *string `json:"githubLogin,omitempty"`
	GitHubUserID     *int64  `json:"githubUserId,omitempty"`
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
	NativeResume            *PartialReviewerNativeResumeConfig     `json:"nativeResume,omitempty"`
	ThreadResolution        *PartialReviewerThreadResolutionConfig `json:"threadResolution,omitempty"`
}

type PartialReviewerReviewEventsConfig struct {
	Clean    *ReviewerReviewEvent `json:"clean,omitempty"`
	Blocking *ReviewerReviewEvent `json:"blocking,omitempty"`
}

type PartialReviewerNativeResumeConfig struct {
	OnHeadChange               *bool `json:"onHeadChange,omitempty"`
	ReReviewPromptOnHeadChange *bool `json:"reReviewPromptOnHeadChange,omitempty"`
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

type PartialReviewerAutoMergeConfig struct {
	Enabled                 *bool                      `json:"enabled,omitempty"`
	Strategy                *ReviewerAutoMergeStrategy `json:"strategy,omitempty"`
	RequireBranchProtection *bool                      `json:"requireBranchProtection,omitempty"`
	TransientRetries        *int                       `json:"transientRetries,omitempty"`
	Scope                   *ReviewerAutoMergeScope    `json:"scope,omitempty"`
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

type PartialReviewerRoleDiscoveryConfig struct {
	AutoDiscovery *bool                              `json:"autoDiscovery,omitempty"`
	Triggers      *PartialReviewerRoleTriggersConfig `json:"triggers,omitempty"`
	SpecReview    *PartialReviewerSpecReviewConfig   `json:"specReview,omitempty"`
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

type PartialSweeperProposerConfig struct {
	Mode                        *SweeperProposerMode `json:"mode,omitempty"`
	Model                       *string              `json:"model,omitempty"`
	TimeoutSeconds              *int                 `json:"timeoutSeconds,omitempty"`
	SchemaVersion               *int                 `json:"schemaVersion,omitempty"`
	DiagnosticMode              *bool                `json:"diagnosticMode,omitempty"`
	TimeoutRateDryRunThreshold  *float64             `json:"timeoutRateDryRunThreshold,omitempty"`
	TimeoutRateDryRunMinSamples *int                 `json:"timeoutRateDryRunMinSamples,omitempty"`
}

type PartialSweeperFilterConfig struct {
	Mode *SweeperFilterMode `json:"mode,omitempty"`
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
	Discovery    *PartialReviewerRoleDiscoveryConfig `json:"discovery,omitempty"`
	Behavior     *PartialReviewerConfig              `json:"behavior,omitempty"`
	AutoMerge    *PartialReviewerAutoMergeConfig     `json:"autoMerge,omitempty"`
	Instructions *string                             `json:"instructions,omitempty"`

	AutoDiscovery *bool                              `json:"autoDiscovery,omitempty"`
	Triggers      *PartialReviewerRoleTriggersConfig `json:"triggers,omitempty"`
	SpecReview    *PartialReviewerSpecReviewConfig   `json:"specReview,omitempty"`
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
	Filter        *PartialSweeperFilterConfig     `json:"filter,omitempty"`
	Proposer      *PartialSweeperProposerConfig   `json:"proposer,omitempty"`
	Lifecycle     *PartialSweeperLifecycleConfig  `json:"lifecycle,omitempty"`
	Limits        *PartialSweeperLimitsConfig     `json:"limits,omitempty"`
	Categories    *PartialSweeperCategoriesConfig `json:"categories,omitempty"`
	Security      *PartialSweeperSecurityConfig   `json:"security,omitempty"`
	Reporting     *PartialSweeperReportingConfig  `json:"reporting,omitempty"`
	Instructions  *string                         `json:"instructions,omitempty"`
}

type PartialCoordinatorTriageDispositionConfig struct {
	OutOfScopeLabel       *string `json:"outOfScopeLabel,omitempty"`
	UnclearLabel          *string `json:"unclearLabel,omitempty"`
	ReTriageOnAuthorReply *bool   `json:"reTriageOnAuthorReply,omitempty"`
}

type PartialCoordinatorTriageConfig struct {
	TriagedLabel    *string                                    `json:"triagedLabel,omitempty"`
	MaxIssueAgeDays *int                                       `json:"maxIssueAgeDays,omitempty"`
	MaxPerTick      *int                                       `json:"maxPerTick,omitempty"`
	Disposition     *PartialCoordinatorTriageDispositionConfig `json:"disposition,omitempty"`
}

type PartialCoordinatorDispatchHumanGateConfig struct {
	SlashCommands *[]string `json:"slashCommands,omitempty"`
	AllowedUsers  *[]string `json:"allowedUsers,omitempty"`
}

type PartialCoordinatorDispatchAutonomousConfig struct {
	DelayMinutes *int    `json:"delayMinutes,omitempty"`
	HoldLabel    *string `json:"holdLabel,omitempty"`
}

type PartialCoordinatorDispatchConfig struct {
	Mode       *string                                     `json:"mode,omitempty"`
	HumanGate  *PartialCoordinatorDispatchHumanGateConfig  `json:"humanGate,omitempty"`
	Autonomous *PartialCoordinatorDispatchAutonomousConfig `json:"autonomous,omitempty"`
	AssignTo   *string                                     `json:"assignTo,omitempty"`
}

type PartialCoordinatorDependenciesConfig struct {
	Enabled           *bool `json:"enabled,omitempty"`
	APITimeoutSeconds *int  `json:"apiTimeoutSeconds,omitempty"`
	APIRetryAttempts  *int  `json:"apiRetryAttempts,omitempty"`
}

type PartialCoordinatorMergeWatchConfig struct {
	TransientRetries         *int    `json:"transientRetries,omitempty"`
	MaxIndeterminateDuration *string `json:"maxIndeterminateDuration,omitempty"`
}

type PartialCoordinatorRoleConfig struct {
	Enabled      *bool                                 `json:"enabled,omitempty"`
	PollInterval *string                               `json:"pollInterval,omitempty"`
	Triage       *PartialCoordinatorTriageConfig       `json:"triage,omitempty"`
	Dispatch     *PartialCoordinatorDispatchConfig     `json:"dispatch,omitempty"`
	Dependencies *PartialCoordinatorDependenciesConfig `json:"dependencies,omitempty"`
	MergeWatch   *PartialCoordinatorMergeWatchConfig   `json:"mergeWatch,omitempty"`
}

type PartialRoleConfigs struct {
	Planner     *PartialPlannerRoleConfig     `json:"planner,omitempty"`
	Reviewer    *PartialReviewerRoleConfig    `json:"reviewer,omitempty"`
	Fixer       *PartialFixerRoleConfig       `json:"fixer,omitempty"`
	Worker      *PartialWorkerRoleConfig      `json:"worker,omitempty"`
	Sweeper     *PartialSweeperRoleConfig     `json:"sweeper,omitempty"`
	Coordinator *PartialCoordinatorRoleConfig `json:"coordinator,omitempty"`
}

type PartialConfig struct {
	Server         *PartialServerConfig       `json:"server,omitempty"`
	Storage        *PartialStorageConfig      `json:"storage,omitempty"`
	Scheduler      *PartialSchedulerConfig    `json:"scheduler,omitempty"`
	Webhook        *PartialWebhookConfig      `json:"webhook,omitempty"`
	Network        *PartialNetworkConfig      `json:"network,omitempty"`
	Agent          *PartialAgentConfig        `json:"agent,omitempty"`
	Logging        *PartialLoggingConfig      `json:"logging,omitempty"`
	Notifications  *PartialNotificationConfig `json:"notifications,omitempty"`
	Disclosure     *PartialDisclosureConfig   `json:"disclosure,omitempty"`
	Tools          *PartialToolPathsConfig    `json:"tools,omitempty"`
	Daemon         *PartialDaemonConfig       `json:"daemon,omitempty"`
	Package        *PartialPackageConfig      `json:"package,omitempty"`
	Defaults       *PartialDefaultsConfig     `json:"defaults,omitempty"`
	LegacyReviewer *PartialReviewerConfig     `json:"reviewer,omitempty"`
	Instructions   *PartialInstructionsConfig `json:"instructions,omitempty"`
	Roles          *PartialRoleConfigs        `json:"roles,omitempty"`
	Projects       *[]PartialProjectRefConfig `json:"projects,omitempty"`
}
