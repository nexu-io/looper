package config

import (
	"os"
	"path/filepath"
	"runtime"
)

const DefaultServerPort = 17310

func DefaultLooperHome() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(homeDir, ".looper"), nil
}

func DefaultConfigPath() (string, error) {
	looperHome, err := DefaultLooperHome()
	if err != nil {
		return "", err
	}

	return filepath.Join(looperHome, "config.json"), nil
}

func DefaultWorktreeRoot() (string, error) {
	looperHome, err := DefaultLooperHome()
	if err != nil {
		return "", err
	}

	return filepath.Join(looperHome, "worktrees"), nil
}

func DefaultProjectWorktreeRoot(projectID string, repoIdentity string) (string, error) {
	worktreeRoot, err := DefaultWorktreeRoot()
	if err != nil {
		return "", err
	}

	return filepath.Join(worktreeRoot, ToRepoWorktreeDirectoryName(repoIdentity), ToProjectWorktreeDirectoryName(projectID)), nil
}

func DefaultConfig(cwd string) (Config, error) {
	looperHome, err := DefaultLooperHome()
	if err != nil {
		return Config{}, err
	}

	backupDir := filepath.Join(looperHome, "backups")
	logDir := filepath.Join(looperHome, "logs")

	return Config{
		Server: ServerConfig{
			Host:     "127.0.0.1",
			Port:     DefaultServerPort,
			AuthMode: AuthModeNone,
		},
		Storage: StorageConfig{
			Mode:      "sqlite",
			DBPath:    filepath.Join(looperHome, "looper.sqlite"),
			BackupDir: stringPtr(backupDir),
		},
		Scheduler: SchedulerConfig{
			PollIntervalSeconds: 30,
			MaxConcurrentRuns:   3,
			RetryMaxAttempts:    5,
			RetryBaseDelayMS:    5000,
		},
		Agent: AgentConfig{
			Params: map[string]any{},
			Env:    map[string]string{},
			Timeouts: AgentTimeoutConfig{
				PlannerSeconds:  30 * 60,
				WorkerSeconds:   60 * 60,
				ReviewerSeconds: 30 * 60,
				FixerSeconds:    30 * 60,
			},
		},
		Logging: LoggingConfig{
			Level:     LogLevelInfo,
			MaxSizeMB: 10,
			MaxFiles:  5,
		},
		Notifications: NotificationConfig{
			InApp: true,
			Osascript: OsascriptNotificationConfig{
				Enabled: runtime.GOOS == "darwin",
				SoundForLevels: []NotificationSoundLevel{
					NotificationSoundLevelActionRequired,
					NotificationSoundLevelFailure,
				},
				ThrottleWindowSeconds: 60,
			},
		},
		Disclosure: DefaultDisclosureConfig(),
		Tools:      ToolPathsConfig{},
		Daemon: DaemonConfig{
			Mode:              DaemonModeForeground,
			LogDir:            logDir,
			ShutdownTimeoutMS: 1000,
			WorkingDirectory:  cwd,
			Environment:       map[string]string{},
		},
		Package: PackageConfig{
			Distribution:               "github-release",
			AutoMigrateOnStartup:       true,
			RequireBackupBeforeMigrate: false,
		},
		Defaults: DefaultsConfig{
			BaseBranch:         "main",
			AllowAutoCommit:    true,
			AllowAutoPush:      true,
			AllowAutoApprove:   false,
			AllowAutoMerge:     false,
			AllowRiskyFixes:    false,
			FixAllPullRequests: false,
			OpenPRStrategy:     OpenPRStrategyAllDone,
			AddSnapshotMode:    AddSnapshotModeAsync,
		},
		Reviewer: ReviewerConfig{
			Loop: ReviewerLoopConfig{
				EnabledByDefault:          true,
				QuietPeriodSeconds:        60,
				MinPublishIntervalSeconds: 300,
				MaxIterationsPerPR:        20,
				MaxIterationsPerHead:      1,
				MaxWallClockSeconds:       14400,
				MaxConsecutiveFailures:    3,
				MaxAgentExecutionsPerPR:   25,
				StopOnApproved:            true,
				StopOnReadyLabel:          true,
				StopOnIdenticalOutput:     true,
			},
			Scope:                   ReviewerScopeChangedRanges,
			PublishMode:             ReviewerPublishModeSingleReview,
			ReviewEvents:            ReviewerReviewEventsConfig{Clean: ReviewerReviewEventComment, Blocking: ReviewerReviewEventComment},
			DetectDuplicateFindings: true,
			ThreadResolution: ReviewerThreadResolutionConfig{
				Enabled:                     false,
				Mode:                        ReviewerThreadResolutionModeReportOnly,
				Scope:                       ReviewerThreadResolutionScopeLooperAuthoredOnly,
				AutoResolve:                 ReviewerThreadResolutionAutoResolveObjectiveOnly,
				RequireAuditComment:         true,
				RequireNewHeadSinceThread:   true,
				RequireCurrentReviewRequest: true,
				MaxThreadsPerRun:            10,
			},
		},
		Instructions: InstructionsConfig{Enabled: true, MaxBytes: 8192},
		Roles: RoleConfigs{
			Planner: PlannerRoleConfig{
				AutoDiscovery: true,
				Triggers: IssueRoleTriggersConfig{
					Labels:                     []string{"looper:plan"},
					LabelMode:                  LabelModeAll,
					RequireAssigneeCurrentUser: true,
				},
			},
			Reviewer: ReviewerRoleConfig{
				AutoDiscovery: true,
				Triggers: ReviewerRoleTriggersConfig{
					IncludeDrafts:        false,
					RequireReviewRequest: true,
					Labels:               []string{},
					LabelMode:            LabelModeAll,
				},
				SpecReview: ReviewerSpecReviewConfig{
					IncludeReviewingLabel: true,
					ReviewingLabel:        "looper:spec-reviewing",
				},
			},
			Fixer: FixerRoleConfig{
				AutoDiscovery: true,
				Triggers: FixerRoleTriggersConfig{
					IncludeDrafts: false,
					AuthorFilter:  FixerAuthorFilterCurrentUser,
					Labels:        []string{},
					LabelMode:     LabelModeAll,
				},
			},
			Worker: WorkerRoleConfig{
				AutoDiscovery: true,
				Triggers: IssueRoleTriggersConfig{
					Labels:                     []string{"looper:worker-ready"},
					LabelMode:                  LabelModeAll,
					RequireAssigneeCurrentUser: true,
				},
			},
		},
		Projects: []ProjectRefConfig{},
	}, nil
}

func DefaultDisclosureConfig() DisclosureConfig {
	return DisclosureConfig{
		Enabled:      true,
		IncludeAgent: true,
		IncludeOS:    false,
		Channels: DisclosureChannelsConfig{
			GitCommit:            true,
			PullRequest:          true,
			IssueComment:         true,
			ReviewComment:        true,
			InlineCommentVisible: false,
		},
	}
}

func stringPtr(value string) *string {
	return &value
}
