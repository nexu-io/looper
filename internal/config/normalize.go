package config

func Normalize(cwd string, partials ...PartialConfig) (Config, error) {
	config, err := DefaultConfig(cwd)
	if err != nil {
		return Config{}, err
	}

	explicitReviewerCleanReviewEvent := hasExplicitReviewerCleanReviewEvent(partials)
	fixerAuthorFilterExplicit := false
	for _, partial := range partials {
		if partial.Roles != nil && partial.Roles.Fixer != nil && partial.Roles.Fixer.Triggers != nil && partial.Roles.Fixer.Triggers.AuthorFilter != nil {
			fixerAuthorFilterExplicit = true
		}
		mergeConfig(&config, partial)
	}
	if config.Defaults.AllowAutoApprove && !explicitReviewerCleanReviewEvent {
		config.Reviewer.ReviewEvents.Clean = ReviewerReviewEventApprove
	}
	if !fixerAuthorFilterExplicit && config.Defaults.FixAllPullRequests {
		config.Roles.Fixer.Triggers.AuthorFilter = FixerAuthorFilterAny
	}

	return config, nil
}

func hasExplicitReviewerCleanReviewEvent(partials []PartialConfig) bool {
	for _, partial := range partials {
		if partial.Reviewer != nil && partial.Reviewer.ReviewEvents != nil && partial.Reviewer.ReviewEvents.Clean != nil {
			return true
		}
	}
	return false
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

	if partial.Reviewer != nil {
		mergeReviewerConfig(&config.Reviewer, *partial.Reviewer)
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
	if partial.ThreadResolution != nil {
		mergeReviewerThreadResolutionConfig(&config.ThreadResolution, *partial.ThreadResolution)
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
	if partial.AutoDiscovery != nil {
		config.AutoDiscovery = *partial.AutoDiscovery
	}
	if partial.Triggers != nil {
		mergeReviewerRoleTriggersConfig(&config.Triggers, *partial.Triggers)
	}
	if partial.SpecReview != nil {
		mergeReviewerSpecReviewConfig(&config.SpecReview, *partial.SpecReview)
	}
	if partial.Instructions != nil {
		config.Instructions = *partial.Instructions
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

func cloneProjects(projects []ProjectRefConfig) []ProjectRefConfig {
	if projects == nil {
		return nil
	}

	cloned := make([]ProjectRefConfig, len(projects))
	for index, project := range projects {
		cloned[index] = ProjectRefConfig{
			ID:           project.ID,
			Name:         project.Name,
			RepoPath:     firstNonEmpty(project.RepoPath, project.Path),
			Path:         project.Path,
			Instructions: cloneStringMap(project.Instructions),
			Roles:        clonePartialRoleConfigs(project.Roles),
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
	if configs.Reviewer != nil {
		reviewer := *configs.Reviewer
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
