package config

func Normalize(cwd string, partials ...PartialConfig) (Config, error) {
	config, err := DefaultConfig(cwd)
	if err != nil {
		return Config{}, err
	}

	for _, partial := range partials {
		mergeConfig(&config, partial)
	}

	return config, nil
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
	if partial.DetectDuplicateFindings != nil {
		config.DetectDuplicateFindings = *partial.DetectDuplicateFindings
	} else if partial.DedupeFindings != nil {
		config.DetectDuplicateFindings = *partial.DedupeFindings
	}
}

func mergeReviewerLoopConfig(config *ReviewerLoopConfig, partial PartialReviewerLoopConfig) {
	if partial.EnabledByDefault != nil {
		config.EnabledByDefault = *partial.EnabledByDefault
	}
	if partial.QuietPeriodSeconds != nil {
		config.QuietPeriodSeconds = *partial.QuietPeriodSeconds
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

func cloneProjects(projects []ProjectRefConfig) []ProjectRefConfig {
	if projects == nil {
		return nil
	}

	cloned := make([]ProjectRefConfig, len(projects))
	for index, project := range projects {
		cloned[index] = ProjectRefConfig{
			ID:       project.ID,
			Name:     project.Name,
			RepoPath: project.RepoPath,
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
