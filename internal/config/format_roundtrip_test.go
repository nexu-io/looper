package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

func TestCanonicalDomainsRoundTripAcrossStructuredFormats(t *testing.T) {
	original := canonicalRoundTripFixtureConfig()
	domains := map[string]func(Config) any{
		"server":        func(cfg Config) any { return cfg.Server },
		"daemon":        func(cfg Config) any { return cfg.Daemon },
		"storage":       func(cfg Config) any { return cfg.Storage },
		"scheduler":     func(cfg Config) any { return cfg.Scheduler },
		"agent":         func(cfg Config) any { return cfg.Agent },
		"logging":       func(cfg Config) any { return cfg.Logging },
		"notifications": func(cfg Config) any { return cfg.Notifications },
		"disclosure":    func(cfg Config) any { return cfg.Disclosure },
		"tools":         func(cfg Config) any { return cfg.Tools },
		"package":       func(cfg Config) any { return cfg.Package },
		"defaults":      func(cfg Config) any { return cfg.Defaults },
		"instructions":  func(cfg Config) any { return cfg.Instructions },
		"roles":         func(cfg Config) any { return cfg.Roles },
		"projects":      func(cfg Config) any { return cfg.Projects },
	}

	for _, format := range []string{"json", "yaml", "toml"} {
		roundTripped := roundTripConfigViaStructuredFormat(t, original, format)
		for name, extract := range domains {
			t.Run(format+"/"+name, func(t *testing.T) {
				if !reflect.DeepEqual(extract(roundTripped), extract(original)) {
					t.Fatalf("%s domain mismatch after %s round trip\nactual: %#v\nwant: %#v", name, format, extract(roundTripped), extract(original))
				}
			})
		}
	}
}

func TestMarshalConfigFilePreservesIntegerTOMLNumbers(t *testing.T) {
	mode := WebhookModeTunnel
	listenPort := 17311
	publicBaseURL := "https://looper.example.test"
	fallback := 300
	partial := PartialConfig{Webhook: &PartialWebhookConfig{Enabled: fixtureBoolPtr(true), Mode: &mode, ListenPort: &listenPort, PublicBaseURL: &publicBaseURL, FallbackPollIntervalSeconds: &fallback}}

	raw, err := MarshalConfigFile("config.toml", partial)
	if err != nil {
		t.Fatalf("MarshalConfigFile() error = %v", err)
	}
	text := string(raw)
	for _, want := range []string{"fallbackPollIntervalSeconds = 300", "listenPort = 17311"} {
		if !strings.Contains(text, want) {
			t.Fatalf("MarshalConfigFile() missing %q:\n%s", want, text)
		}
	}
	for _, notWant := range []string{"fallbackPollIntervalSeconds = 300.0", "listenPort = 17311.0"} {
		if strings.Contains(text, notWant) {
			t.Fatalf("MarshalConfigFile() contained %q:\n%s", notWant, text)
		}
	}
}

func canonicalRoundTripFixtureConfig() Config {
	baseURL := "https://looper.example.test"
	localToken := "local-token"
	backupDir := "/tmp/looper/backups"
	gitPath := "/opt/bin/git"
	ghPath := "/opt/bin/gh"
	looperPath := "/opt/bin/looper"
	osascriptPath := "/usr/bin/osascript"
	baseBranch := "release"
	worktreeRoot := "/tmp/demo-worktrees"

	return Config{
		Server:        ServerConfig{Host: "0.0.0.0", Port: 18443, BaseURL: &baseURL, AuthMode: AuthModeLocalToken, LocalToken: &localToken},
		Daemon:        DaemonConfig{Mode: DaemonModeLaunchd, RestartPolicy: DaemonRestartAlways, RestartThrottleSeconds: 30, LogDir: "/tmp/looper/logs", ShutdownTimeoutMS: 2500, WorkingDirectory: "/tmp/looper/workdir", Environment: map[string]string{"LAUNCHD": "1"}},
		Storage:       StorageConfig{Mode: "sqlite", DBPath: "/tmp/looper/state.sqlite", BackupDir: &backupDir},
		Scheduler:     SchedulerConfig{PollIntervalSeconds: 45, MaxConcurrentRuns: 7, RetryMaxAttempts: 9, RetryBaseDelayMS: 1500, SlowLaneWarnThresholdMS: 2500, DiscoveryCacheTTLSeconds: 12},
		Agent:         AgentConfig{Vendor: fixtureVendorPtr(AgentVendorCodex), Model: stringPtr("gpt-5"), Params: map[string]any{"reasoning": map[string]any{"effort": "high"}}, Env: map[string]string{"OPENAI_API_KEY": "test"}, Timeouts: AgentTimeoutConfig{PlannerSeconds: 360, WorkerSeconds: 720, ReviewerSeconds: 540, FixerSeconds: 480, PlannerIdleTimeoutSeconds: 120, PlannerMaxRuntimeSeconds: 360, WorkerIdleTimeoutSeconds: 180, WorkerMaxRuntimeSeconds: 720, ReviewerIdleTimeoutSeconds: 150, ReviewerMaxRuntimeSeconds: 540, FixerIdleTimeoutSeconds: 90, FixerMaxRuntimeSeconds: 480}, NativeResume: AgentNativeResumeConfig{Enabled: true}},
		Logging:       LoggingConfig{Level: LogLevelDebug, MaxSizeMB: 25, MaxFiles: 9},
		Notifications: NotificationConfig{InApp: false, Osascript: OsascriptNotificationConfig{Enabled: true, SoundForLevels: []NotificationSoundLevel{NotificationSoundLevelFailure}, ThrottleWindowSeconds: 120}},
		Disclosure:    DisclosureConfig{Enabled: true, IncludeAgent: false, IncludeOS: true, Channels: DisclosureChannelsConfig{GitCommit: true, PullRequest: false, IssueComment: true, ReviewComment: false, InlineCommentVisible: true}},
		Tools:         ToolPathsConfig{GitPath: &gitPath, GHPath: &ghPath, LooperPath: &looperPath, OsascriptPath: &osascriptPath},
		Package:       PackageConfig{Distribution: "github-release", AutoUpgradeEnabled: false, AutoMigrateOnStartup: true, RequireBackupBeforeMigrate: true},
		Defaults:      DefaultsConfig{BaseBranch: baseBranch, AllowAutoCommit: false, AllowAutoPush: true, AllowAutoApprove: true, AllowAutoMerge: false, AllowRiskyFixes: true, FixAllPullRequests: false, OpenPRStrategy: OpenPRStrategyFirstCommit, AddSnapshotMode: AddSnapshotModeFull},
		Instructions:  InstructionsConfig{Enabled: true, MaxBytes: 12288},
		Roles: RoleConfigs{
			Planner:  PlannerRoleConfig{AutoDiscovery: true, Triggers: IssueRoleTriggersConfig{Labels: []string{"plan", "triage"}, LabelMode: LabelModeAny, RequireAssigneeCurrentUser: false}, Instructions: "Plan carefully"},
			Worker:   WorkerRoleConfig{AutoDiscovery: false, Triggers: IssueRoleTriggersConfig{Labels: []string{"ready"}, LabelMode: LabelModeAll, RequireAssigneeCurrentUser: true}, Instructions: "Ship code"},
			Reviewer: ReviewerRoleConfig{Instructions: "Review with context", Discovery: ReviewerRoleDiscoveryConfig{AutoDiscovery: true, Triggers: ReviewerRoleTriggersConfig{IncludeDrafts: true, RequireReviewRequest: false, EnableSelfReview: true, Labels: []string{"needs-review"}, LabelMode: LabelModeAny}, SpecReview: ReviewerSpecReviewConfig{IncludeReviewingLabel: true, ReviewingLabel: "looper:spec-reviewing"}}, Behavior: ReviewerConfig{Loop: ReviewerLoopConfig{EnabledByDefault: true, QuietPeriodSeconds: 5, MinPublishIntervalSeconds: 60, MaxIterationsPerPR: 3, MaxIterationsPerHead: 2, MaxWallClockSeconds: 300, MaxConsecutiveFailures: 2, MaxAgentExecutionsPerPR: 4, StopOnApproved: false, StopOnReadyLabel: true, StopOnIdenticalOutput: false}, Scope: ReviewerScopeChangedFiles, PublishMode: ReviewerPublishModeSingleReview, ReviewEvents: ReviewerReviewEventsConfig{Clean: ReviewerReviewEventApprove, Blocking: ReviewerReviewEventRequestChanges}, DetectDuplicateFindings: false, NativeResume: ReviewerNativeResumeConfig{OnHeadChange: true, ReReviewPromptOnHeadChange: true}, ThreadResolution: ReviewerThreadResolutionConfig{Enabled: true, Mode: ReviewerThreadResolutionModeResolveObjective, Scope: ReviewerThreadResolutionScopeLooperAuthoredOnly, AutoResolve: ReviewerThreadResolutionAutoResolveObjectiveOnly, RequireAuditComment: true, RequireNewHeadSinceThread: false, RequireCurrentReviewRequest: true, MaxThreadsPerRun: 2}}},
			Fixer:    FixerRoleConfig{AutoDiscovery: true, Triggers: FixerRoleTriggersConfig{IncludeDrafts: true, AuthorFilter: FixerAuthorFilterAny, Labels: []string{"fix-me"}, LabelMode: LabelModeAny}, Instructions: "Keep diffs small"},
			Sweeper:  SweeperRoleConfig{AutoDiscovery: true, DryRun: false, Triggers: SweeperTriggersConfig{IncludeIssues: true, IncludePullRequests: false, IncludeDrafts: true, ExcludeLabels: []string{"pinned"}, ExcludeAuthors: []string{"bot"}, ExcludeAuthorAssociations: []string{"OWNER"}, LooperInternalLabels: []string{"looper:plan"}, ReopenCooldownDays: 7, MaxPerTick: 3}, Lifecycle: SweeperLifecycleConfig{PendingLabel: "pending", ClosedLabel: "closed", KeepLabel: "keep"}, Limits: SweeperLimitsConfig{MaxWarningsPerRepoPerDay: 4, MaxClosesPerRepoPerDay: 2, GlobalKillSwitch: false}, Categories: SweeperCategoriesConfig{Stale: SweeperCategoryConfig{Enabled: true, InactivityDays: 30, GracePeriodDays: 3, MinConfidence: 80}, AlreadyFixed: SweeperCategoryConfig{Enabled: true, GracePeriodDays: 2, MinConfidence: 85}, Unrelated: SweeperCategoryConfig{Enabled: false, GracePeriodDays: 4, MinConfidence: 90}, Superseded: SweeperCategoryConfig{Enabled: true, GracePeriodDays: 5, MinConfidence: 88}, AbandonedPR: SweeperCategoryConfig{Enabled: true, InactivityDays: 14, GracePeriodDays: 2, MinConfidence: 70}}, Security: SweeperSecurityConfig{QuarantineLabel: "security", NotifyAssignees: []string{"team@example.com"}}, Reporting: SweeperReportingConfig{DurableReportsDir: "/tmp/looper/reports"}, Instructions: "Sweep carefully"},
		},
		Projects: []ProjectRefConfig{{ID: "demo", Name: "Demo", RepoPath: "/repos/demo", BaseBranch: &baseBranch, WorktreeRoot: &worktreeRoot, Roles: &PartialRoleConfigs{Planner: &PartialPlannerRoleConfig{Instructions: stringPtr("Project plan instructions")}, Reviewer: &PartialReviewerRoleConfig{Instructions: stringPtr("Project review instructions"), Discovery: &PartialReviewerRoleDiscoveryConfig{AutoDiscovery: fixtureBoolPtr(false), Triggers: &PartialReviewerRoleTriggersConfig{IncludeDrafts: fixtureBoolPtr(false), Labels: fixtureStringSlicePtr([]string{"project-review"}), LabelMode: fixtureLabelModePtr(LabelModeAll)}, SpecReview: &PartialReviewerSpecReviewConfig{IncludeReviewingLabel: fixtureBoolPtr(true), ReviewingLabel: stringPtr("project:spec")}}, Behavior: &PartialReviewerConfig{Loop: &PartialReviewerLoopConfig{QuietPeriodSeconds: fixtureIntPtr(15)}, ReviewEvents: &PartialReviewerReviewEventsConfig{Blocking: fixtureReviewerReviewEventPtr(ReviewerReviewEventComment)}}}}}},
	}
}

func roundTripConfigViaStructuredFormat(t *testing.T, cfg Config, format string) Config {
	t.Helper()

	base := asGenericMap(t, cfg)
	encoded := marshalStructured(t, format, base)
	decoded := unmarshalStructured(t, format, encoded)
	raw, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("json.Marshal(%s decoded map) error = %v", format, err)
	}

	var roundTripped Config
	if err := json.Unmarshal(raw, &roundTripped); err != nil {
		t.Fatalf("json.Unmarshal(%s decoded map) error = %v", format, err)
	}
	return roundTripped
}

func asGenericMap(t *testing.T, value any) map[string]any {
	t.Helper()

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	return decoded
}

func marshalStructured(t *testing.T, format string, value map[string]any) []byte {
	t.Helper()

	switch format {
	case "json":
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		return raw
	case "yaml":
		raw, err := yaml.Marshal(value)
		if err != nil {
			t.Fatalf("yaml.Marshal() error = %v", err)
		}
		return raw
	case "toml":
		raw, err := toml.Marshal(value)
		if err != nil {
			t.Fatalf("toml.Marshal() error = %v", err)
		}
		return raw
	default:
		t.Fatalf("unsupported format %q", format)
		return nil
	}
}

func unmarshalStructured(t *testing.T, format string, raw []byte) map[string]any {
	t.Helper()

	decoded := map[string]any{}
	switch format {
	case "json":
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
	case "yaml":
		if err := yaml.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("yaml.Unmarshal() error = %v", err)
		}
	case "toml":
		if err := toml.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("toml.Unmarshal() error = %v", err)
		}
	default:
		t.Fatalf("unsupported format %q", format)
	}
	return decoded
}

func fixtureBoolPtr(value bool) *bool { return &value }

func fixtureIntPtr(value int) *int { return &value }

func fixtureLabelModePtr(value LabelMode) *LabelMode { return &value }

func fixtureReviewerReviewEventPtr(value ReviewerReviewEvent) *ReviewerReviewEvent { return &value }

func fixtureStringSlicePtr(value []string) *[]string { return &value }

func fixtureVendorPtr(value AgentVendor) *AgentVendor { return &value }
