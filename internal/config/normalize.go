package config

import "fmt"

func Normalize(cwd string, partials ...PartialConfig) (Config, error) {
	config, err := DefaultConfig(cwd)
	if err != nil {
		return Config{}, err
	}

	for _, partial := range partials {
		if issues := validateLegacyProjectInstructionRoleKeys(partial); len(issues) > 0 {
			return Config{}, &ConfigValidationError{Issues: issues}
		}
		mergeConfig(&config, normalizeLayerPartial(partial))
	}

	return config, nil
}

func CanonicalizePartialForMigration(partial PartialConfig) PartialConfig {
	normalized := normalizeLayerPartial(clonePartialConfig(partial))
	normalized.LegacyReviewer = nil

	if normalized.Defaults != nil {
		normalized.Defaults.AllowAutoApprove = nil
		normalized.Defaults.FixAllPullRequests = nil
	}

	if normalized.Roles != nil && normalized.Roles.Reviewer != nil {
		normalized.Roles.Reviewer.AutoDiscovery = nil
		normalized.Roles.Reviewer.Triggers = nil
		normalized.Roles.Reviewer.SpecReview = nil
	}

	if normalized.Projects != nil {
		projects := *normalized.Projects
		for i := range projects {
			projects[i].Path = ""
			projects[i].Instructions = nil
			if projects[i].Roles != nil && projects[i].Roles.Reviewer != nil {
				projects[i].Roles.Reviewer.AutoDiscovery = nil
				projects[i].Roles.Reviewer.Triggers = nil
				projects[i].Roles.Reviewer.SpecReview = nil
			}
		}
		normalized.Projects = &projects
	}

	return normalized
}

func normalizeLayerPartial(partial PartialConfig) PartialConfig {
	normalized := partial

	if normalized.LegacyReviewer != nil {
		reviewer := ensureReviewerRoleConfig(&normalized)
		reviewer.Behavior = mergePartialReviewerConfigWithCanonicalPriority(reviewer.Behavior, normalized.LegacyReviewer)
	}

	if normalized.Roles != nil && normalized.Roles.Reviewer != nil {
		normalizeReviewerRoleLegacyShape(normalized.Roles.Reviewer)
	}

	if normalized.Projects != nil {
		projects := *normalized.Projects
		for i := range projects {
			if projects[i].RepoPath == "" {
				projects[i].RepoPath = projects[i].Path
			}
			projects[i].Roles = mergeLegacyProjectInstructionsIntoRoles(projects[i].Roles, projects[i].Instructions)
			if projects[i].Roles != nil && projects[i].Roles.Reviewer != nil {
				normalizeReviewerRoleLegacyShape(projects[i].Roles.Reviewer)
			}
		}
		normalized.Projects = &projects
	}

	if normalized.Defaults != nil {
		if normalized.Defaults.AllowAutoApprove != nil {
			reviewEvents := ensureReviewerReviewEventsConfig(&normalized)
			if reviewEvents.Clean == nil {
				event := ReviewerReviewEventComment
				if *normalized.Defaults.AllowAutoApprove {
					event = ReviewerReviewEventApprove
				}
				reviewEvents.Clean = &event
			}
		}
		if normalized.Defaults.FixAllPullRequests != nil {
			triggers := ensureFixerRoleTriggersConfig(&normalized)
			if triggers.AuthorFilter == nil {
				authorFilter := FixerAuthorFilterCurrentUser
				if *normalized.Defaults.FixAllPullRequests {
					authorFilter = FixerAuthorFilterAny
				}
				triggers.AuthorFilter = &authorFilter
			}
		}
	}

	return normalized
}

func validateLegacyProjectInstructionRoleKeys(partial PartialConfig) []ValidationIssue {
	if partial.Projects == nil {
		return nil
	}

	issues := make([]ValidationIssue, 0)
	for index, project := range *partial.Projects {
		for role := range project.Instructions {
			if isValidInstructionRole(role) {
				continue
			}
			issues = append(issues, ValidationIssue{
				Path:    fmt.Sprintf("projects[%d].instructions.%s", index, role),
				Message: "role must be one of: planner, worker, reviewer, fixer, sweeper",
			})
		}
	}

	return issues
}

func normalizeReviewerRoleLegacyShape(reviewer *PartialReviewerRoleConfig) {
	if reviewer == nil {
		return
	}
	reviewer.Discovery = mergePartialReviewerRoleDiscoveryWithCanonicalPriority(reviewer.Discovery, reviewer.AutoDiscovery, reviewer.Triggers, reviewer.SpecReview)
}

func mergePartialReviewerConfigWithCanonicalPriority(canonical *PartialReviewerConfig, legacy *PartialReviewerConfig) *PartialReviewerConfig {
	if canonical == nil {
		return legacy
	}
	if legacy == nil {
		return canonical
	}
	if canonical.Loop == nil {
		canonical.Loop = legacy.Loop
	}
	if canonical.Scope == nil {
		canonical.Scope = legacy.Scope
	}
	if canonical.PublishMode == nil {
		canonical.PublishMode = legacy.PublishMode
	}
	if canonical.ReviewEvents == nil {
		canonical.ReviewEvents = legacy.ReviewEvents
	} else if legacy.ReviewEvents != nil {
		if canonical.ReviewEvents.Clean == nil {
			canonical.ReviewEvents.Clean = legacy.ReviewEvents.Clean
		}
		if canonical.ReviewEvents.Blocking == nil {
			canonical.ReviewEvents.Blocking = legacy.ReviewEvents.Blocking
		}
		if canonical.ReviewEvents.Clean == nil && canonical.ReviewEvents.Blocking == nil {
			canonical.ReviewEvents = legacy.ReviewEvents
		}
	}
	if canonical.DetectDuplicateFindings == nil {
		canonical.DetectDuplicateFindings = legacy.DetectDuplicateFindings
	}
	if canonical.DedupeFindings == nil {
		canonical.DedupeFindings = legacy.DedupeFindings
	}
	if canonical.NativeResume == nil {
		canonical.NativeResume = legacy.NativeResume
	}
	if canonical.ThreadResolution == nil {
		canonical.ThreadResolution = legacy.ThreadResolution
	}
	return canonical
}

func mergePartialReviewerRoleDiscoveryWithCanonicalPriority(canonical *PartialReviewerRoleDiscoveryConfig, legacyAutoDiscovery *bool, legacyTriggers *PartialReviewerRoleTriggersConfig, legacySpecReview *PartialReviewerSpecReviewConfig) *PartialReviewerRoleDiscoveryConfig {
	if canonical == nil && legacyAutoDiscovery == nil && legacyTriggers == nil && legacySpecReview == nil {
		return nil
	}
	if canonical == nil {
		canonical = &PartialReviewerRoleDiscoveryConfig{}
	}
	if canonical.AutoDiscovery == nil {
		canonical.AutoDiscovery = legacyAutoDiscovery
	}
	if canonical.Triggers == nil {
		canonical.Triggers = legacyTriggers
	} else if legacyTriggers != nil {
		if canonical.Triggers.IncludeDrafts == nil {
			canonical.Triggers.IncludeDrafts = legacyTriggers.IncludeDrafts
		}
		if canonical.Triggers.RequireReviewRequest == nil {
			canonical.Triggers.RequireReviewRequest = legacyTriggers.RequireReviewRequest
		}
		if canonical.Triggers.EnableSelfReview == nil {
			canonical.Triggers.EnableSelfReview = legacyTriggers.EnableSelfReview
		}
		if canonical.Triggers.Labels == nil {
			canonical.Triggers.Labels = legacyTriggers.Labels
		}
		if canonical.Triggers.LabelMode == nil {
			canonical.Triggers.LabelMode = legacyTriggers.LabelMode
		}
	}
	if canonical.SpecReview == nil {
		canonical.SpecReview = legacySpecReview
	} else if legacySpecReview != nil {
		if canonical.SpecReview.IncludeReviewingLabel == nil {
			canonical.SpecReview.IncludeReviewingLabel = legacySpecReview.IncludeReviewingLabel
		}
		if canonical.SpecReview.ReviewingLabel == nil {
			canonical.SpecReview.ReviewingLabel = legacySpecReview.ReviewingLabel
		}
	}
	return canonical
}

func mergeConfig(config *Config, partial PartialConfig) {
	if partial.Server != nil {
		mergeServerConfig(&config.Server, *partial.Server)
	}

	if partial.Storage != nil {
		mergeStorageConfig(&config.Storage, *partial.Storage)
	}

	if partial.Scheduler != nil {
		mergeSchedulerConfig(&config.Scheduler, *partial.Scheduler)
	}

	if partial.Webhook != nil {
		mergeWebhookConfig(&config.Webhook, *partial.Webhook)
	}

	if partial.Network != nil {
		mergeNetworkConfig(&config.Network, *partial.Network)
	}

	if partial.Agent != nil {
		mergeAgentConfig(&config.Agent, *partial.Agent)
	}

	if partial.Logging != nil {
		mergeLoggingConfig(&config.Logging, *partial.Logging)
	}

	if partial.Notifications != nil {
		mergeNotificationConfig(&config.Notifications, *partial.Notifications)
	}

	if partial.Disclosure != nil {
		mergeDisclosureConfig(&config.Disclosure, *partial.Disclosure)
	}

	if partial.Tools != nil {
		mergeToolPathsConfig(&config.Tools, *partial.Tools)
	}

	if partial.Daemon != nil {
		mergeDaemonConfig(&config.Daemon, *partial.Daemon)
	}

	if partial.Package != nil {
		mergePackageConfig(&config.Package, *partial.Package)
	}

	if partial.Defaults != nil {
		mergeDefaultsConfig(&config.Defaults, *partial.Defaults)
	}

	if partial.LegacyReviewer != nil {
		mergeReviewerConfig(&config.Roles.Reviewer.Behavior, *partial.LegacyReviewer)
	}

	if partial.Instructions != nil {
		mergeInstructionsConfig(&config.Instructions, *partial.Instructions)
	}

	if partial.Roles != nil {
		mergeRoleConfigs(&config.Roles, *partial.Roles)
	}

	if partial.Projects != nil {
		config.Projects = cloneProjects(*partial.Projects)
	}
}

func mergeServerConfig(config *ServerConfig, partial PartialServerConfig) {
	if partial.Host != nil {
		config.Host = *partial.Host
	}

	if partial.Port != nil {
		config.Port = *partial.Port
	}

	if partial.BaseURL != nil {
		config.BaseURL = stringPtr(*partial.BaseURL)
	}

	if partial.AuthMode != nil {
		config.AuthMode = *partial.AuthMode
	}

	if partial.LocalToken != nil {
		config.LocalToken = stringPtr(*partial.LocalToken)
	}
}

func mergeStorageConfig(config *StorageConfig, partial PartialStorageConfig) {
	if partial.Mode != nil {
		config.Mode = *partial.Mode
	}

	if partial.DBPath != nil {
		config.DBPath = *partial.DBPath
	}

	if partial.BackupDir != nil {
		config.BackupDir = stringPtr(*partial.BackupDir)
	}
}

func mergeSchedulerConfig(config *SchedulerConfig, partial PartialSchedulerConfig) {
	if partial.PollIntervalSeconds != nil {
		config.PollIntervalSeconds = *partial.PollIntervalSeconds
	}

	if partial.MaxConcurrentRuns != nil {
		config.MaxConcurrentRuns = *partial.MaxConcurrentRuns
	}

	if partial.RetryMaxAttempts != nil {
		config.RetryMaxAttempts = *partial.RetryMaxAttempts
	}

	if partial.RetryBaseDelayMS != nil {
		config.RetryBaseDelayMS = *partial.RetryBaseDelayMS
	}

	if partial.SlowLaneWarnThresholdMS != nil {
		config.SlowLaneWarnThresholdMS = *partial.SlowLaneWarnThresholdMS
	}
}

func mergeWebhookConfig(config *WebhookConfig, partial PartialWebhookConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}

	if partial.Mode != nil {
		config.Mode = *partial.Mode
	}

	if partial.ListenPort != nil {
		config.ListenPort = *partial.ListenPort
	}

	if partial.PublicBaseURL != nil {
		config.PublicBaseURL = *partial.PublicBaseURL
	}

	if partial.FallbackPollIntervalSeconds != nil {
		config.FallbackPollIntervalSeconds = *partial.FallbackPollIntervalSeconds
	}
}

func mergeNetworkConfig(config *NetworkConfig, partial PartialNetworkConfig) {
	if partial.Enrolled != nil {
		config.Enrolled = *partial.Enrolled
	}
	if partial.LoopernetBaseURL != nil {
		config.LoopernetBaseURL = *partial.LoopernetBaseURL
	}
	if partial.NodeName != nil {
		config.NodeName = *partial.NodeName
	}
	if partial.GitHubLogin != nil {
		config.GitHubLogin = *partial.GitHubLogin
	}
	if partial.GitHubUserID != nil {
		config.GitHubUserID = *partial.GitHubUserID
	}
}

func mergeAgentConfig(config *AgentConfig, partial PartialAgentConfig) {
	if partial.Vendor != nil {
		vendor := *partial.Vendor
		config.Vendor = &vendor
	}

	if partial.Model != nil {
		config.Model = stringPtr(*partial.Model)
	}

	if partial.Params != nil {
		config.Params = mergeAnyMap(config.Params, partial.Params)
	}

	if partial.Env != nil {
		config.Env = mergeStringMap(config.Env, partial.Env)
	}

	if partial.Timeouts != nil {
		mergeAgentTimeoutConfig(&config.Timeouts, *partial.Timeouts)
	}

	if partial.NativeResume != nil && partial.NativeResume.Enabled != nil {
		config.NativeResume.Enabled = *partial.NativeResume.Enabled
	}
}

func mergeAgentTimeoutConfig(config *AgentTimeoutConfig, partial PartialAgentTimeoutConfig) {
	if partial.PlannerSeconds != nil {
		config.PlannerSeconds = *partial.PlannerSeconds
		config.PlannerMaxRuntimeSeconds = *partial.PlannerSeconds
	}
	if partial.WorkerSeconds != nil {
		config.WorkerSeconds = *partial.WorkerSeconds
		config.WorkerMaxRuntimeSeconds = *partial.WorkerSeconds
	}
	if partial.ReviewerSeconds != nil {
		config.ReviewerSeconds = *partial.ReviewerSeconds
		config.ReviewerMaxRuntimeSeconds = *partial.ReviewerSeconds
	}
	if partial.FixerSeconds != nil {
		config.FixerSeconds = *partial.FixerSeconds
		config.FixerMaxRuntimeSeconds = *partial.FixerSeconds
	}
	if partial.PlannerIdleTimeoutSeconds != nil {
		config.PlannerIdleTimeoutSeconds = *partial.PlannerIdleTimeoutSeconds
	}
	if partial.PlannerMaxRuntimeSeconds != nil {
		config.PlannerMaxRuntimeSeconds = *partial.PlannerMaxRuntimeSeconds
		config.PlannerSeconds = *partial.PlannerMaxRuntimeSeconds
	}
	if partial.WorkerIdleTimeoutSeconds != nil {
		config.WorkerIdleTimeoutSeconds = *partial.WorkerIdleTimeoutSeconds
	}
	if partial.WorkerMaxRuntimeSeconds != nil {
		config.WorkerMaxRuntimeSeconds = *partial.WorkerMaxRuntimeSeconds
		config.WorkerSeconds = *partial.WorkerMaxRuntimeSeconds
	}
	if partial.ReviewerIdleTimeoutSeconds != nil {
		config.ReviewerIdleTimeoutSeconds = *partial.ReviewerIdleTimeoutSeconds
	}
	if partial.ReviewerMaxRuntimeSeconds != nil {
		config.ReviewerMaxRuntimeSeconds = *partial.ReviewerMaxRuntimeSeconds
		config.ReviewerSeconds = *partial.ReviewerMaxRuntimeSeconds
	}
	if partial.FixerIdleTimeoutSeconds != nil {
		config.FixerIdleTimeoutSeconds = *partial.FixerIdleTimeoutSeconds
	}
	if partial.FixerMaxRuntimeSeconds != nil {
		config.FixerMaxRuntimeSeconds = *partial.FixerMaxRuntimeSeconds
		config.FixerSeconds = *partial.FixerMaxRuntimeSeconds
	}
}

func mergeLoggingConfig(config *LoggingConfig, partial PartialLoggingConfig) {
	if partial.Level != nil {
		config.Level = *partial.Level
	}

	if partial.MaxSizeMB != nil {
		config.MaxSizeMB = *partial.MaxSizeMB
	}

	if partial.MaxFiles != nil {
		config.MaxFiles = *partial.MaxFiles
	}
}

func mergeNotificationConfig(config *NotificationConfig, partial PartialNotificationConfig) {
	if partial.InApp != nil {
		config.InApp = *partial.InApp
	}

	if partial.Osascript != nil {
		mergeOsascriptNotificationConfig(&config.Osascript, *partial.Osascript)
	}
}

func mergeOsascriptNotificationConfig(config *OsascriptNotificationConfig, partial PartialOsascriptNotificationConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}

	if partial.SoundForLevels != nil {
		config.SoundForLevels = cloneSoundLevels(*partial.SoundForLevels)
	}

	if partial.ThrottleWindowSeconds != nil {
		config.ThrottleWindowSeconds = *partial.ThrottleWindowSeconds
	}
}

func mergeDisclosureConfig(config *DisclosureConfig, partial PartialDisclosureConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}

	if partial.IncludeAgent != nil {
		config.IncludeAgent = *partial.IncludeAgent
	}

	if partial.IncludeOS != nil {
		config.IncludeOS = *partial.IncludeOS
	}

	if partial.Channels != nil {
		mergeDisclosureChannelsConfig(&config.Channels, *partial.Channels)
	}
}

func mergeDisclosureChannelsConfig(config *DisclosureChannelsConfig, partial PartialDisclosureChannelsConfig) {
	if partial.GitCommit != nil {
		config.GitCommit = *partial.GitCommit
	}
	if partial.PullRequest != nil {
		config.PullRequest = *partial.PullRequest
	}
	if partial.IssueComment != nil {
		config.IssueComment = *partial.IssueComment
	}
	if partial.ReviewComment != nil {
		config.ReviewComment = *partial.ReviewComment
	}
	if partial.InlineCommentVisible != nil {
		config.InlineCommentVisible = *partial.InlineCommentVisible
	}
}

func mergeToolPathsConfig(config *ToolPathsConfig, partial PartialToolPathsConfig) {
	if partial.GitPath != nil {
		config.GitPath = stringPtr(*partial.GitPath)
	}

	if partial.GHPath != nil {
		config.GHPath = stringPtr(*partial.GHPath)
	}

	if partial.LooperPath != nil {
		config.LooperPath = stringPtr(*partial.LooperPath)
	}

	if partial.OsascriptPath != nil {
		config.OsascriptPath = stringPtr(*partial.OsascriptPath)
	}
}

func mergeDaemonConfig(config *DaemonConfig, partial PartialDaemonConfig) {
	if partial.Mode != nil {
		config.Mode = *partial.Mode
	}

	if partial.RestartPolicy != nil {
		config.RestartPolicy = *partial.RestartPolicy
	}

	if partial.RestartThrottleSeconds != nil {
		config.RestartThrottleSeconds = *partial.RestartThrottleSeconds
	}

	if partial.PlistPath != nil {
		config.PlistPath = stringPtr(*partial.PlistPath)
	}

	if partial.LogDir != nil {
		config.LogDir = *partial.LogDir
	}

	if partial.ShutdownTimeoutMS != nil {
		config.ShutdownTimeoutMS = *partial.ShutdownTimeoutMS
	}

	if partial.WorkingDirectory != nil {
		config.WorkingDirectory = *partial.WorkingDirectory
	}

	if partial.Environment != nil {
		config.Environment = mergeStringMap(config.Environment, partial.Environment)
	}
}

func mergePackageConfig(config *PackageConfig, partial PartialPackageConfig) {
	if partial.Distribution != nil {
		config.Distribution = *partial.Distribution
	}

	if partial.AutoUpgradeEnabled != nil {
		config.AutoUpgradeEnabled = *partial.AutoUpgradeEnabled
	}

	if partial.AutoMigrateOnStartup != nil {
		config.AutoMigrateOnStartup = *partial.AutoMigrateOnStartup
	}

	if partial.RequireBackupBeforeMigrate != nil {
		config.RequireBackupBeforeMigrate = *partial.RequireBackupBeforeMigrate
	}
}

func mergeDefaultsConfig(config *DefaultsConfig, partial PartialDefaultsConfig) {
	if partial.BaseBranch != nil {
		config.BaseBranch = *partial.BaseBranch
	}

	if partial.AllowAutoCommit != nil {
		config.AllowAutoCommit = *partial.AllowAutoCommit
	}

	if partial.AllowAutoPush != nil {
		config.AllowAutoPush = *partial.AllowAutoPush
	}

	if partial.AllowAutoApprove != nil {
		config.AllowAutoApprove = *partial.AllowAutoApprove
	}

	if partial.AllowAutoMerge != nil {
		config.AllowAutoMerge = *partial.AllowAutoMerge
	}

	if partial.AllowRiskyFixes != nil {
		config.AllowRiskyFixes = *partial.AllowRiskyFixes
	}

	if partial.FixAllPullRequests != nil {
		config.FixAllPullRequests = *partial.FixAllPullRequests
	}

	if partial.OpenPRStrategy != nil {
		config.OpenPRStrategy = *partial.OpenPRStrategy
	}

	if partial.AddSnapshotMode != nil {
		config.AddSnapshotMode = *partial.AddSnapshotMode
	}
}

func mergeReviewerConfig(config *ReviewerConfig, partial PartialReviewerConfig) {
	if partial.Loop != nil {
		mergeReviewerLoopConfig(&config.Loop, *partial.Loop)
	}
	if partial.Scope != nil {
		config.Scope = *partial.Scope
	}
	if partial.PublishMode != nil {
		config.PublishMode = *partial.PublishMode
	}
	if partial.ReviewEvents != nil {
		mergeReviewerReviewEventsConfig(&config.ReviewEvents, *partial.ReviewEvents)
	}
	if partial.DetectDuplicateFindings != nil {
		config.DetectDuplicateFindings = *partial.DetectDuplicateFindings
	} else if partial.DedupeFindings != nil {
		config.DetectDuplicateFindings = *partial.DedupeFindings
	}
	if partial.NativeResume != nil {
		mergeReviewerNativeResumeConfig(&config.NativeResume, *partial.NativeResume)
	}
	if partial.ThreadResolution != nil {
		mergeReviewerThreadResolutionConfig(&config.ThreadResolution, *partial.ThreadResolution)
	}
}

func mergeReviewerNativeResumeConfig(config *ReviewerNativeResumeConfig, partial PartialReviewerNativeResumeConfig) {
	if partial.OnHeadChange != nil {
		config.OnHeadChange = *partial.OnHeadChange
	}
	if partial.ReReviewPromptOnHeadChange != nil {
		config.ReReviewPromptOnHeadChange = *partial.ReReviewPromptOnHeadChange
	}
}

func mergeReviewerThreadResolutionConfig(config *ReviewerThreadResolutionConfig, partial PartialReviewerThreadResolutionConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.Mode != nil {
		config.Mode = *partial.Mode
	}
	if partial.Scope != nil {
		config.Scope = *partial.Scope
	}
	if partial.AutoResolve != nil {
		config.AutoResolve = *partial.AutoResolve
	}
	if partial.RequireAuditComment != nil {
		config.RequireAuditComment = *partial.RequireAuditComment
	}
	if partial.RequireNewHeadSinceThread != nil {
		config.RequireNewHeadSinceThread = *partial.RequireNewHeadSinceThread
	}
	if partial.RequireCurrentReviewRequest != nil {
		config.RequireCurrentReviewRequest = *partial.RequireCurrentReviewRequest
	}
	if partial.MaxThreadsPerRun != nil {
		config.MaxThreadsPerRun = *partial.MaxThreadsPerRun
	}
}

func mergeReviewerReviewEventsConfig(config *ReviewerReviewEventsConfig, partial PartialReviewerReviewEventsConfig) {
	if partial.Clean != nil {
		config.Clean = *partial.Clean
	}
	if partial.Blocking != nil {
		config.Blocking = *partial.Blocking
	}
}

func mergeReviewerLoopConfig(config *ReviewerLoopConfig, partial PartialReviewerLoopConfig) {
	if partial.EnabledByDefault != nil {
		config.EnabledByDefault = *partial.EnabledByDefault
	}
	if partial.QuietPeriodSeconds != nil {
		config.QuietPeriodSeconds = *partial.QuietPeriodSeconds
	}
	if partial.MinPublishIntervalSeconds != nil {
		config.MinPublishIntervalSeconds = *partial.MinPublishIntervalSeconds
	}
	if partial.MaxIterationsPerPR != nil {
		config.MaxIterationsPerPR = *partial.MaxIterationsPerPR
	}
	if partial.MaxIterationsPerHead != nil {
		config.MaxIterationsPerHead = *partial.MaxIterationsPerHead
	}
	if partial.MaxWallClockSeconds != nil {
		config.MaxWallClockSeconds = *partial.MaxWallClockSeconds
	}
	if partial.MaxConsecutiveFailures != nil {
		config.MaxConsecutiveFailures = *partial.MaxConsecutiveFailures
	}
	if partial.MaxAgentExecutionsPerPR != nil {
		config.MaxAgentExecutionsPerPR = *partial.MaxAgentExecutionsPerPR
	}
	if partial.StopOnApproved != nil {
		config.StopOnApproved = *partial.StopOnApproved
	}
	if partial.StopOnReadyLabel != nil {
		config.StopOnReadyLabel = *partial.StopOnReadyLabel
	}
	if partial.StopOnIdenticalOutput != nil {
		config.StopOnIdenticalOutput = *partial.StopOnIdenticalOutput
	}
}

func mergeInstructionsConfig(config *InstructionsConfig, partial PartialInstructionsConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.MaxBytes != nil {
		config.MaxBytes = *partial.MaxBytes
	}
}

func mergeRoleConfigs(config *RoleConfigs, partial PartialRoleConfigs) {
	if partial.Coordinator != nil {
		mergeCoordinatorRoleConfig(&config.Coordinator, *partial.Coordinator)
	}
	if partial.Planner != nil {
		mergePlannerRoleConfig(&config.Planner, *partial.Planner)
	}
	if partial.Reviewer != nil {
		mergeReviewerRoleConfig(&config.Reviewer, *partial.Reviewer)
	}
	if partial.Fixer != nil {
		mergeFixerRoleConfig(&config.Fixer, *partial.Fixer)
	}
	if partial.Worker != nil {
		mergeWorkerRoleConfig(&config.Worker, *partial.Worker)
	}
	if partial.Sweeper != nil {
		mergeSweeperRoleConfig(&config.Sweeper, *partial.Sweeper)
	}
}

func mergeCoordinatorRoleConfig(config *CoordinatorRoleConfig, partial PartialCoordinatorRoleConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.PollInterval != nil {
		config.PollInterval = *partial.PollInterval
	}
	if partial.Triage != nil {
		mergeCoordinatorTriageConfig(&config.Triage, *partial.Triage)
	}
	if partial.Dispatch != nil {
		mergeCoordinatorDispatchConfig(&config.Dispatch, *partial.Dispatch)
	}
	if partial.Dependencies != nil {
		mergeCoordinatorDependenciesConfig(&config.Dependencies, *partial.Dependencies)
	}
	if partial.MergeWatch != nil {
		mergeCoordinatorMergeWatchConfig(&config.MergeWatch, *partial.MergeWatch)
	}
}

func mergeCoordinatorTriageConfig(config *CoordinatorTriageConfig, partial PartialCoordinatorTriageConfig) {
	if partial.TriagedLabel != nil {
		config.TriagedLabel = *partial.TriagedLabel
	}
	if partial.MaxIssueAgeDays != nil {
		config.MaxIssueAgeDays = *partial.MaxIssueAgeDays
	}
	if partial.MaxPerTick != nil {
		config.MaxPerTick = *partial.MaxPerTick
	}
	if partial.Disposition != nil {
		mergeCoordinatorTriageDispositionConfig(&config.Disposition, *partial.Disposition)
	}
}

func mergeCoordinatorTriageDispositionConfig(config *CoordinatorTriageDispositionConfig, partial PartialCoordinatorTriageDispositionConfig) {
	if partial.OutOfScopeLabel != nil {
		config.OutOfScopeLabel = *partial.OutOfScopeLabel
	}
	if partial.UnclearLabel != nil {
		config.UnclearLabel = *partial.UnclearLabel
	}
	if partial.ReTriageOnAuthorReply != nil {
		config.ReTriageOnAuthorReply = *partial.ReTriageOnAuthorReply
	}
}

func mergeCoordinatorDispatchConfig(config *CoordinatorDispatchConfig, partial PartialCoordinatorDispatchConfig) {
	if partial.Mode != nil {
		config.Mode = *partial.Mode
	}
	if partial.HumanGate != nil {
		mergeCoordinatorDispatchHumanGateConfig(&config.HumanGate, *partial.HumanGate)
	}
	if partial.Autonomous != nil {
		mergeCoordinatorDispatchAutonomousConfig(&config.Autonomous, *partial.Autonomous)
	}
	if partial.AssignTo != nil {
		config.AssignTo = *partial.AssignTo
	}
}

func mergeCoordinatorDispatchHumanGateConfig(config *CoordinatorDispatchHumanGateConfig, partial PartialCoordinatorDispatchHumanGateConfig) {
	if partial.SlashCommands != nil {
		config.SlashCommands = cloneStrings(*partial.SlashCommands)
	}
	if partial.AllowedUsers != nil {
		config.AllowedUsers = cloneStrings(*partial.AllowedUsers)
	}
}

func mergeCoordinatorDispatchAutonomousConfig(config *CoordinatorDispatchAutonomousConfig, partial PartialCoordinatorDispatchAutonomousConfig) {
	if partial.DelayMinutes != nil {
		config.DelayMinutes = *partial.DelayMinutes
	}
	if partial.HoldLabel != nil {
		config.HoldLabel = *partial.HoldLabel
	}
}

func mergeCoordinatorDependenciesConfig(config *CoordinatorDependenciesConfig, partial PartialCoordinatorDependenciesConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.APITimeoutSeconds != nil {
		config.APITimeoutSeconds = *partial.APITimeoutSeconds
	}
	if partial.APIRetryAttempts != nil {
		config.APIRetryAttempts = *partial.APIRetryAttempts
	}
}

func mergeCoordinatorMergeWatchConfig(config *CoordinatorMergeWatchConfig, partial PartialCoordinatorMergeWatchConfig) {
	if partial.TransientRetries != nil {
		config.TransientRetries = *partial.TransientRetries
	}
	if partial.MaxIndeterminateDuration != nil {
		config.MaxIndeterminateDuration = *partial.MaxIndeterminateDuration
	}
}

func mergePlannerRoleConfig(config *PlannerRoleConfig, partial PartialPlannerRoleConfig) {
	if partial.AutoDiscovery != nil {
		config.AutoDiscovery = *partial.AutoDiscovery
	}
	if partial.Triggers != nil {
		mergeIssueRoleTriggersConfig(&config.Triggers, *partial.Triggers)
	}
	if partial.Instructions != nil {
		config.Instructions = *partial.Instructions
	}
}

func mergeWorkerRoleConfig(config *WorkerRoleConfig, partial PartialWorkerRoleConfig) {
	if partial.AutoDiscovery != nil {
		config.AutoDiscovery = *partial.AutoDiscovery
	}
	if partial.Triggers != nil {
		mergeIssueRoleTriggersConfig(&config.Triggers, *partial.Triggers)
	}
	if partial.Instructions != nil {
		config.Instructions = *partial.Instructions
	}
}

func mergeReviewerRoleConfig(config *ReviewerRoleConfig, partial PartialReviewerRoleConfig) {
	if partial.AutoDiscovery != nil || partial.Triggers != nil || partial.SpecReview != nil {
		mergeReviewerRoleDiscoveryConfig(&config.Discovery, PartialReviewerRoleDiscoveryConfig{
			AutoDiscovery: partial.AutoDiscovery,
			Triggers:      partial.Triggers,
			SpecReview:    partial.SpecReview,
		})
	}
	if partial.Discovery != nil {
		mergeReviewerRoleDiscoveryConfig(&config.Discovery, *partial.Discovery)
	}
	if partial.Behavior != nil {
		mergeReviewerConfig(&config.Behavior, *partial.Behavior)
	}
	if partial.AutoMerge != nil {
		mergeReviewerAutoMergeConfig(&config.AutoMerge, *partial.AutoMerge)
	}
	if partial.Instructions != nil {
		config.Instructions = *partial.Instructions
	}
}

func mergeReviewerAutoMergeConfig(config *ReviewerAutoMergeConfig, partial PartialReviewerAutoMergeConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.Strategy != nil {
		config.Strategy = *partial.Strategy
	}
	if partial.RequireBranchProtection != nil {
		config.RequireBranchProtection = *partial.RequireBranchProtection
	}
	if partial.TransientRetries != nil {
		config.TransientRetries = *partial.TransientRetries
	}
	if partial.Scope != nil {
		config.Scope = *partial.Scope
	}
}

func mergeReviewerRoleDiscoveryConfig(config *ReviewerRoleDiscoveryConfig, partial PartialReviewerRoleDiscoveryConfig) {
	if partial.AutoDiscovery != nil {
		config.AutoDiscovery = *partial.AutoDiscovery
	}
	if partial.Triggers != nil {
		mergeReviewerRoleTriggersConfig(&config.Triggers, *partial.Triggers)
	}
	if partial.SpecReview != nil {
		mergeReviewerSpecReviewConfig(&config.SpecReview, *partial.SpecReview)
	}
}

func mergeFixerRoleConfig(config *FixerRoleConfig, partial PartialFixerRoleConfig) {
	if partial.AutoDiscovery != nil {
		config.AutoDiscovery = *partial.AutoDiscovery
	}
	if partial.Triggers != nil {
		mergeFixerRoleTriggersConfig(&config.Triggers, *partial.Triggers)
	}
	if partial.Instructions != nil {
		config.Instructions = *partial.Instructions
	}
}

func mergeSweeperRoleConfig(config *SweeperRoleConfig, partial PartialSweeperRoleConfig) {
	if partial.AutoDiscovery != nil {
		config.AutoDiscovery = *partial.AutoDiscovery
	}
	if partial.DryRun != nil {
		config.DryRun = *partial.DryRun
	}
	if partial.Triggers != nil {
		mergeSweeperTriggersConfig(&config.Triggers, *partial.Triggers)
	}
	if partial.Filter != nil {
		mergeSweeperFilterConfig(&config.Filter, *partial.Filter)
	}
	if partial.Proposer != nil {
		mergeSweeperProposerConfig(&config.Proposer, *partial.Proposer)
	}
	if partial.Lifecycle != nil {
		mergeSweeperLifecycleConfig(&config.Lifecycle, *partial.Lifecycle)
	}
	if partial.Limits != nil {
		mergeSweeperLimitsConfig(&config.Limits, *partial.Limits)
	}
	if partial.Categories != nil {
		mergeSweeperCategoriesConfig(&config.Categories, *partial.Categories)
	}
	if partial.Security != nil {
		mergeSweeperSecurityConfig(&config.Security, *partial.Security)
	}
	if partial.Reporting != nil {
		mergeSweeperReportingConfig(&config.Reporting, *partial.Reporting)
	}
	if partial.Instructions != nil {
		config.Instructions = *partial.Instructions
	}
}

func mergeIssueRoleTriggersConfig(config *IssueRoleTriggersConfig, partial PartialIssueRoleTriggersConfig) {
	if partial.Labels != nil {
		config.Labels = cloneStrings(*partial.Labels)
	}
	if partial.LabelMode != nil {
		config.LabelMode = *partial.LabelMode
	}
	if partial.RequireAssigneeCurrentUser != nil {
		config.RequireAssigneeCurrentUser = *partial.RequireAssigneeCurrentUser
	}
}

func mergePullRequestRoleTriggersConfig(config *PullRequestRoleTriggersConfig, partial PartialPullRequestRoleTriggersConfig) {
	if partial.IncludeDrafts != nil {
		config.IncludeDrafts = *partial.IncludeDrafts
	}
	if partial.RequireReviewRequest != nil {
		config.RequireReviewRequest = *partial.RequireReviewRequest
	}
}

func mergeReviewerRoleTriggersConfig(config *ReviewerRoleTriggersConfig, partial PartialReviewerRoleTriggersConfig) {
	if partial.IncludeDrafts != nil {
		config.IncludeDrafts = *partial.IncludeDrafts
	}
	if partial.RequireReviewRequest != nil {
		config.RequireReviewRequest = *partial.RequireReviewRequest
	}
	if partial.EnableSelfReview != nil {
		config.EnableSelfReview = *partial.EnableSelfReview
	}
	if partial.Labels != nil {
		config.Labels = cloneStrings(*partial.Labels)
	}
	if partial.LabelMode != nil {
		config.LabelMode = *partial.LabelMode
	}
}

func mergeReviewerSpecReviewConfig(config *ReviewerSpecReviewConfig, partial PartialReviewerSpecReviewConfig) {
	if partial.IncludeReviewingLabel != nil {
		config.IncludeReviewingLabel = *partial.IncludeReviewingLabel
	}
	if partial.ReviewingLabel != nil {
		config.ReviewingLabel = *partial.ReviewingLabel
	}
}

func mergeFixerRoleTriggersConfig(config *FixerRoleTriggersConfig, partial PartialFixerRoleTriggersConfig) {
	if partial.IncludeDrafts != nil {
		config.IncludeDrafts = *partial.IncludeDrafts
	}
	if partial.AuthorFilter != nil {
		config.AuthorFilter = *partial.AuthorFilter
	}
	if partial.Labels != nil {
		config.Labels = cloneStrings(*partial.Labels)
	}
	if partial.LabelMode != nil {
		config.LabelMode = *partial.LabelMode
	}
}

func mergeSweeperCategoryConfig(config *SweeperCategoryConfig, partial PartialSweeperCategoryConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.InactivityDays != nil {
		config.InactivityDays = *partial.InactivityDays
	}
	if partial.GracePeriodDays != nil {
		config.GracePeriodDays = *partial.GracePeriodDays
	}
	if partial.MinConfidence != nil {
		config.MinConfidence = *partial.MinConfidence
	}
}

func mergeSweeperTriggersConfig(config *SweeperTriggersConfig, partial PartialSweeperTriggersConfig) {
	if partial.IncludeIssues != nil {
		config.IncludeIssues = *partial.IncludeIssues
	}
	if partial.IncludePullRequests != nil {
		config.IncludePullRequests = *partial.IncludePullRequests
	}
	if partial.IncludeDrafts != nil {
		config.IncludeDrafts = *partial.IncludeDrafts
	}
	if partial.ExcludeLabels != nil {
		config.ExcludeLabels = cloneStrings(*partial.ExcludeLabels)
	}
	if partial.ExcludeAuthors != nil {
		config.ExcludeAuthors = cloneStrings(*partial.ExcludeAuthors)
	}
	if partial.ExcludeAuthorAssociations != nil {
		config.ExcludeAuthorAssociations = cloneStrings(*partial.ExcludeAuthorAssociations)
	}
	if partial.LooperInternalLabels != nil {
		config.LooperInternalLabels = cloneStrings(*partial.LooperInternalLabels)
	}
	if partial.ReopenCooldownDays != nil {
		config.ReopenCooldownDays = *partial.ReopenCooldownDays
	}
	if partial.MaxPerTick != nil {
		config.MaxPerTick = *partial.MaxPerTick
	}
}

func mergeSweeperLimitsConfig(config *SweeperLimitsConfig, partial PartialSweeperLimitsConfig) {
	if partial.MaxWarningsPerRepoPerDay != nil {
		config.MaxWarningsPerRepoPerDay = *partial.MaxWarningsPerRepoPerDay
	}
	if partial.MaxClosesPerRepoPerDay != nil {
		config.MaxClosesPerRepoPerDay = *partial.MaxClosesPerRepoPerDay
	}
	if partial.GlobalKillSwitch != nil {
		config.GlobalKillSwitch = *partial.GlobalKillSwitch
	}
}

func mergeSweeperSecurityConfig(config *SweeperSecurityConfig, partial PartialSweeperSecurityConfig) {
	if partial.QuarantineLabel != nil {
		config.QuarantineLabel = *partial.QuarantineLabel
	}
	if partial.NotifyAssignees != nil {
		config.NotifyAssignees = cloneStrings(*partial.NotifyAssignees)
	}
}

func mergeSweeperReportingConfig(config *SweeperReportingConfig, partial PartialSweeperReportingConfig) {
	if partial.DurableReportsDir != nil {
		config.DurableReportsDir = *partial.DurableReportsDir
	}
}

func mergeSweeperFilterConfig(config *SweeperFilterConfig, partial PartialSweeperFilterConfig) {
	if partial.Mode != nil {
		config.Mode = *partial.Mode
	}
}

func mergeSweeperProposerConfig(config *SweeperProposerConfig, partial PartialSweeperProposerConfig) {
	if partial.Mode != nil {
		config.Mode = *partial.Mode
	}
	if partial.Model != nil {
		config.Model = partial.Model
	}
	if partial.TimeoutSeconds != nil {
		config.TimeoutSeconds = *partial.TimeoutSeconds
	}
	if partial.SchemaVersion != nil {
		config.SchemaVersion = *partial.SchemaVersion
	}
	if partial.DiagnosticMode != nil {
		config.DiagnosticMode = *partial.DiagnosticMode
	}
	if partial.TimeoutRateDryRunThreshold != nil {
		config.TimeoutRateDryRunThreshold = *partial.TimeoutRateDryRunThreshold
	}
	if partial.TimeoutRateDryRunMinSamples != nil {
		config.TimeoutRateDryRunMinSamples = *partial.TimeoutRateDryRunMinSamples
	}
}

func mergeSweeperLifecycleConfig(config *SweeperLifecycleConfig, partial PartialSweeperLifecycleConfig) {
	if partial.PendingLabel != nil {
		config.PendingLabel = *partial.PendingLabel
	}
	if partial.ClosedLabel != nil {
		config.ClosedLabel = *partial.ClosedLabel
	}
	if partial.KeepLabel != nil {
		config.KeepLabel = *partial.KeepLabel
	}
}

func mergeSweeperCategoriesConfig(config *SweeperCategoriesConfig, partial PartialSweeperCategoriesConfig) {
	if partial.Stale != nil {
		mergeSweeperCategoryConfig(&config.Stale, *partial.Stale)
	}
	if partial.AlreadyFixed != nil {
		mergeSweeperCategoryConfig(&config.AlreadyFixed, *partial.AlreadyFixed)
	}
	if partial.Unrelated != nil {
		mergeSweeperCategoryConfig(&config.Unrelated, *partial.Unrelated)
	}
	if partial.Superseded != nil {
		mergeSweeperCategoryConfig(&config.Superseded, *partial.Superseded)
	}
	if partial.AbandonedPR != nil {
		mergeSweeperCategoryConfig(&config.AbandonedPR, *partial.AbandonedPR)
	}
}

func mergeAnyMap(base map[string]any, override map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		merged[key] = cloneAnyValue(value)
	}

	for key, value := range override {
		if baseValue, ok := merged[key]; ok {
			merged[key] = mergeAnyValue(baseValue, value)
			continue
		}

		merged[key] = cloneAnyValue(value)
	}

	return merged
}

func mergeAnyValue(base any, override any) any {
	baseMap, baseIsMap := base.(map[string]any)
	overrideMap, overrideIsMap := override.(map[string]any)
	if baseIsMap && overrideIsMap {
		return mergeAnyMap(baseMap, overrideMap)
	}

	return cloneAnyValue(override)
}

func mergeStringMap(base map[string]string, override map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(override))
	for key, value := range base {
		merged[key] = value
	}

	for key, value := range override {
		merged[key] = value
	}

	return merged
}

func cloneAnyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return mergeAnyMap(nil, typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneAnyValue(item)
		}

		return cloned
	default:
		return typed
	}
}

func cloneSoundLevels(levels []NotificationSoundLevel) []NotificationSoundLevel {
	if levels == nil {
		return nil
	}

	cloned := make([]NotificationSoundLevel, len(levels))
	copy(cloned, levels)
	return cloned
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func clonePartialConfig(partial PartialConfig) PartialConfig {
	cloned := partial
	if partial.Network != nil {
		network := *partial.Network
		cloned.Network = &network
	}
	if partial.Defaults != nil {
		defaults := *partial.Defaults
		cloned.Defaults = &defaults
	}
	if partial.LegacyReviewer != nil {
		cloned.LegacyReviewer = clonePartialReviewerConfig(partial.LegacyReviewer)
	}
	if partial.Roles != nil {
		cloned.Roles = clonePartialRoleConfigs(partial.Roles)
	}
	if partial.Projects != nil {
		projects := clonePartialProjects(*partial.Projects)
		cloned.Projects = &projects
	}
	return cloned
}

func clonePartialProjects(projects []PartialProjectRefConfig) []PartialProjectRefConfig {
	if projects == nil {
		return nil
	}
	cloned := make([]PartialProjectRefConfig, len(projects))
	for index, project := range projects {
		cloned[index] = PartialProjectRefConfig{
			ID:           project.ID,
			Name:         project.Name,
			RepoPath:     project.RepoPath,
			Path:         project.Path,
			BaseBranch:   cloneStringPtr(project.BaseBranch),
			WorktreeRoot: cloneStringPtr(project.WorktreeRoot),
			Network:      clonePartialProjectNetworkConfig(project.Network),
			Webhook:      clonePartialProjectWebhookConfig(project.Webhook),
			Instructions: cloneStringMap(project.Instructions),
			Roles:        clonePartialRoleConfigs(project.Roles),
		}
	}
	return cloned
}

func clonePartialProjectNetworkConfig(config *PartialProjectNetworkConfig) *PartialProjectNetworkConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	return &cloned
}

func clonePartialProjectWebhookConfig(config *PartialProjectWebhookConfig) *PartialProjectWebhookConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	return &cloned
}

func clonePartialReviewerConfig(config *PartialReviewerConfig) *PartialReviewerConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	if config.Loop != nil {
		loop := *config.Loop
		cloned.Loop = &loop
	}
	if config.ReviewEvents != nil {
		reviewEvents := *config.ReviewEvents
		cloned.ReviewEvents = &reviewEvents
	}
	if config.NativeResume != nil {
		nativeResume := *config.NativeResume
		cloned.NativeResume = &nativeResume
	}
	if config.ThreadResolution != nil {
		threadResolution := *config.ThreadResolution
		cloned.ThreadResolution = &threadResolution
	}
	return &cloned
}

func cloneProjects(projects []PartialProjectRefConfig) []ProjectRefConfig {
	if projects == nil {
		return nil
	}

	cloned := make([]ProjectRefConfig, len(projects))
	for index, project := range projects {
		roles := mergeLegacyProjectInstructionsIntoRoles(clonePartialRoleConfigs(project.Roles), project.Instructions)
		repoPath := firstNonEmpty(project.RepoPath, project.Path)

		cloned[index] = ProjectRefConfig{
			ID:       project.ID,
			Name:     project.Name,
			RepoPath: repoPath,
			Path:     project.Path,
			Network:  ProjectNetworkConfig{Mode: NetworkModeOff},
			Roles:    roles,
		}
		if project.Network != nil && project.Network.Mode != nil {
			cloned[index].Network.Mode = *project.Network.Mode
		}
		if project.Webhook != nil && project.Webhook.Mode != nil {
			cloned[index].Webhook.Mode = *project.Webhook.Mode
		}

		if project.BaseBranch != nil {
			cloned[index].BaseBranch = stringPtr(*project.BaseBranch)
		}

		if project.WorktreeRoot != nil {
			cloned[index].WorktreeRoot = stringPtr(*project.WorktreeRoot)
		}
	}

	return cloned
}

func cloneProjectNetworkConfig(config *ProjectNetworkConfig) *ProjectNetworkConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	return &cloned
}

func mergeLegacyProjectInstructionsIntoRoles(roles *PartialRoleConfigs, instructions map[string]string) *PartialRoleConfigs {
	if len(instructions) == 0 {
		return roles
	}
	if roles == nil {
		roles = &PartialRoleConfigs{}
	}
	for role, text := range instructions {
		switch role {
		case "planner":
			if roles.Planner == nil {
				roles.Planner = &PartialPlannerRoleConfig{}
			}
			if roles.Planner.Instructions == nil {
				roles.Planner.Instructions = stringPtr(text)
			}
		case "worker":
			if roles.Worker == nil {
				roles.Worker = &PartialWorkerRoleConfig{}
			}
			if roles.Worker.Instructions == nil {
				roles.Worker.Instructions = stringPtr(text)
			}
		case "reviewer":
			if roles.Reviewer == nil {
				roles.Reviewer = &PartialReviewerRoleConfig{}
			}
			if roles.Reviewer.Instructions == nil {
				roles.Reviewer.Instructions = stringPtr(text)
			}
		case "fixer":
			if roles.Fixer == nil {
				roles.Fixer = &PartialFixerRoleConfig{}
			}
			if roles.Fixer.Instructions == nil {
				roles.Fixer.Instructions = stringPtr(text)
			}
		case "sweeper":
			if roles.Sweeper == nil {
				roles.Sweeper = &PartialSweeperRoleConfig{}
			}
			if roles.Sweeper.Instructions == nil {
				roles.Sweeper.Instructions = stringPtr(text)
			}
		}
	}
	return roles
}

func clonePartialRoleConfigs(configs *PartialRoleConfigs) *PartialRoleConfigs {
	if configs == nil {
		return nil
	}
	cloned := PartialRoleConfigs{}
	if configs.Planner != nil {
		planner := *configs.Planner
		if configs.Planner.Triggers != nil {
			triggers := *configs.Planner.Triggers
			if triggers.Labels != nil {
				labels := cloneStrings(*triggers.Labels)
				triggers.Labels = &labels
			}
			planner.Triggers = &triggers
		}
		cloned.Planner = &planner
	}
	if configs.Worker != nil {
		worker := *configs.Worker
		if configs.Worker.Triggers != nil {
			triggers := *configs.Worker.Triggers
			if triggers.Labels != nil {
				labels := cloneStrings(*triggers.Labels)
				triggers.Labels = &labels
			}
			worker.Triggers = &triggers
		}
		cloned.Worker = &worker
	}
	if configs.Coordinator != nil {
		coordinator := *configs.Coordinator
		if configs.Coordinator.Triage != nil {
			triage := *configs.Coordinator.Triage
			if configs.Coordinator.Triage.Disposition != nil {
				disposition := *configs.Coordinator.Triage.Disposition
				triage.Disposition = &disposition
			}
			coordinator.Triage = &triage
		}
		if configs.Coordinator.Dispatch != nil {
			dispatch := *configs.Coordinator.Dispatch
			if configs.Coordinator.Dispatch.HumanGate != nil {
				humanGate := *configs.Coordinator.Dispatch.HumanGate
				if humanGate.SlashCommands != nil {
					slashCommands := cloneStrings(*humanGate.SlashCommands)
					humanGate.SlashCommands = &slashCommands
				}
				if humanGate.AllowedUsers != nil {
					allowedUsers := cloneStrings(*humanGate.AllowedUsers)
					humanGate.AllowedUsers = &allowedUsers
				}
				dispatch.HumanGate = &humanGate
			}
			if configs.Coordinator.Dispatch.Autonomous != nil {
				autonomous := *configs.Coordinator.Dispatch.Autonomous
				dispatch.Autonomous = &autonomous
			}
			coordinator.Dispatch = &dispatch
		}
		cloned.Coordinator = &coordinator
	}
	if configs.Reviewer != nil {
		reviewer := *configs.Reviewer
		if configs.Reviewer.Discovery != nil {
			discovery := *configs.Reviewer.Discovery
			if configs.Reviewer.Discovery.Triggers != nil {
				triggers := *configs.Reviewer.Discovery.Triggers
				if triggers.Labels != nil {
					labels := cloneStrings(*triggers.Labels)
					triggers.Labels = &labels
				}
				discovery.Triggers = &triggers
			}
			if configs.Reviewer.Discovery.SpecReview != nil {
				specReview := *configs.Reviewer.Discovery.SpecReview
				discovery.SpecReview = &specReview
			}
			reviewer.Discovery = &discovery
		}
		if configs.Reviewer.Triggers != nil {
			triggers := *configs.Reviewer.Triggers
			if triggers.Labels != nil {
				labels := cloneStrings(*triggers.Labels)
				triggers.Labels = &labels
			}
			reviewer.Triggers = &triggers
		}
		if configs.Reviewer.SpecReview != nil {
			specReview := *configs.Reviewer.SpecReview
			reviewer.SpecReview = &specReview
		}
		if configs.Reviewer.Behavior != nil {
			behavior := *configs.Reviewer.Behavior
			if configs.Reviewer.Behavior.Loop != nil {
				loop := *configs.Reviewer.Behavior.Loop
				behavior.Loop = &loop
			}
			if configs.Reviewer.Behavior.ReviewEvents != nil {
				reviewEvents := *configs.Reviewer.Behavior.ReviewEvents
				behavior.ReviewEvents = &reviewEvents
			}
			if configs.Reviewer.Behavior.NativeResume != nil {
				nativeResume := *configs.Reviewer.Behavior.NativeResume
				behavior.NativeResume = &nativeResume
			}
			if configs.Reviewer.Behavior.ThreadResolution != nil {
				threadResolution := *configs.Reviewer.Behavior.ThreadResolution
				behavior.ThreadResolution = &threadResolution
			}
			reviewer.Behavior = &behavior
		}
		cloned.Reviewer = &reviewer
	}
	if configs.Fixer != nil {
		fixer := *configs.Fixer
		if configs.Fixer.Triggers != nil {
			triggers := *configs.Fixer.Triggers
			if triggers.Labels != nil {
				labels := cloneStrings(*triggers.Labels)
				triggers.Labels = &labels
			}
			fixer.Triggers = &triggers
		}
		cloned.Fixer = &fixer
	}
	if configs.Sweeper != nil {
		sweeper := *configs.Sweeper
		if configs.Sweeper.Triggers != nil {
			triggers := *configs.Sweeper.Triggers
			if triggers.ExcludeLabels != nil {
				labels := cloneStrings(*triggers.ExcludeLabels)
				triggers.ExcludeLabels = &labels
			}
			if triggers.ExcludeAuthors != nil {
				authors := cloneStrings(*triggers.ExcludeAuthors)
				triggers.ExcludeAuthors = &authors
			}
			if triggers.ExcludeAuthorAssociations != nil {
				associations := cloneStrings(*triggers.ExcludeAuthorAssociations)
				triggers.ExcludeAuthorAssociations = &associations
			}
			if triggers.LooperInternalLabels != nil {
				labels := cloneStrings(*triggers.LooperInternalLabels)
				triggers.LooperInternalLabels = &labels
			}
			sweeper.Triggers = &triggers
		}
		if configs.Sweeper.Filter != nil {
			filter := *configs.Sweeper.Filter
			sweeper.Filter = &filter
		}
		if configs.Sweeper.Proposer != nil {
			proposer := *configs.Sweeper.Proposer
			if configs.Sweeper.Proposer.Model != nil {
				model := *configs.Sweeper.Proposer.Model
				proposer.Model = &model
			}
			sweeper.Proposer = &proposer
		}
		if configs.Sweeper.Lifecycle != nil {
			lifecycle := *configs.Sweeper.Lifecycle
			sweeper.Lifecycle = &lifecycle
		}
		if configs.Sweeper.Limits != nil {
			limits := *configs.Sweeper.Limits
			sweeper.Limits = &limits
		}
		if configs.Sweeper.Categories != nil {
			categories := *configs.Sweeper.Categories
			copyCategory := func(category *PartialSweeperCategoryConfig) *PartialSweeperCategoryConfig {
				if category == nil {
					return nil
				}
				clonedCategory := *category
				return &clonedCategory
			}
			categories.Stale = copyCategory(configs.Sweeper.Categories.Stale)
			categories.AlreadyFixed = copyCategory(configs.Sweeper.Categories.AlreadyFixed)
			categories.Unrelated = copyCategory(configs.Sweeper.Categories.Unrelated)
			categories.Superseded = copyCategory(configs.Sweeper.Categories.Superseded)
			categories.AbandonedPR = copyCategory(configs.Sweeper.Categories.AbandonedPR)
			sweeper.Categories = &categories
		}
		if configs.Sweeper.Security != nil {
			security := *configs.Sweeper.Security
			if security.NotifyAssignees != nil {
				assignees := cloneStrings(*security.NotifyAssignees)
				security.NotifyAssignees = &assignees
			}
			sweeper.Security = &security
		}
		if configs.Sweeper.Reporting != nil {
			reporting := *configs.Sweeper.Reporting
			sweeper.Reporting = &reporting
		}
		cloned.Sweeper = &sweeper
	}
	return &cloned
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
