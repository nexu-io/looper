package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoadFileUsesDefaultsWhenConfigMissing(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "missing.json")

	loaded, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: configPath,
		LookupEnv:  emptyEnvLookup,
		LookPath:   fakeLookPath(map[string]string{"git": "/detected/git", "gh": "/detected/gh", "osascript": "/detected/osascript"}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	if loaded.Metadata.ConfigPath != configPath {
		t.Fatalf("LoadFile().Metadata.ConfigPath = %q, want %q", loaded.Metadata.ConfigPath, configPath)
	}

	if loaded.Metadata.ConfigFilePresent {
		t.Fatal("LoadFile().Metadata.ConfigFilePresent = true, want false")
	}

	if loaded.Config.Server.Host != "127.0.0.1" {
		t.Fatalf("LoadFile().Config.Server.Host = %q, want default %q", loaded.Config.Server.Host, "127.0.0.1")
	}

	if loaded.Config.Daemon.WorkingDirectory != cwd {
		t.Fatalf("LoadFile().Config.Daemon.WorkingDirectory = %q, want %q", loaded.Config.Daemon.WorkingDirectory, cwd)
	}

	if loaded.Partial.Server != nil {
		t.Fatalf("LoadFile().Partial.Server = %#v, want nil", loaded.Partial.Server)
	}

	if got := loaded.Metadata.ToolDetection["gitPath"]; got != ToolDetectionStatusDetected {
		t.Fatalf("LoadFile().Metadata.ToolDetection[gitPath] = %q, want %q", got, ToolDetectionStatusDetected)
	}
}

func TestLoadFileUsesDefaultsWhenConfigFileIsTopLevelNull(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	if err := os.WriteFile(configPath, []byte(`null`), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: configPath,
		LookupEnv:  emptyEnvLookup,
		LookPath:   fakeLookPath(map[string]string{"git": "/detected/git", "gh": "/detected/gh", "osascript": "/detected/osascript"}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if !loaded.Metadata.ConfigFilePresent {
		t.Fatal("LoadFile().Metadata.ConfigFilePresent = false, want true")
	}
	if loaded.Partial != (PartialConfig{}) {
		t.Fatalf("LoadFile().Partial = %#v, want empty config", loaded.Partial)
	}
	if loaded.Config.Server.Host != "127.0.0.1" {
		t.Fatalf("LoadFile().Config.Server.Host = %q, want default %q", loaded.Config.Server.Host, "127.0.0.1")
	}
}

func TestReadConfigFileIgnoresUnknownTopLevelKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	contents := `{"futureSection":{"enabled":true},"server":{"host":"0.0.0.0"}}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	partial, present, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile() error = %v", err)
	}
	if !present {
		t.Fatal("readConfigFile() present = false, want true")
	}
	if partial.Server == nil || partial.Server.Host == nil || *partial.Server.Host != "0.0.0.0" {
		t.Fatalf("readConfigFile() server host = %#v, want 0.0.0.0", partial.Server)
	}
	if partial.Scheduler != nil {
		t.Fatalf("readConfigFile() scheduler = %#v, want nil", partial.Scheduler)
	}
}

func TestReadConfigFileAcceptsTopLevelNull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`null`), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	partial, present, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile() error = %v", err)
	}
	if !present {
		t.Fatal("readConfigFile() present = false, want true")
	}
	if partial != (PartialConfig{}) {
		t.Fatalf("readConfigFile() partial = %#v, want empty config", partial)
	}
}

func TestReadConfigFileRejectsUnknownNestedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	contents := `{"scheduler":{"pollIntervalSecond":5}}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, present, err := readConfigFile(path)
	if !present {
		t.Fatal("readConfigFile() present = false, want true")
	}
	if err == nil {
		t.Fatal("readConfigFile() error = nil, want unknown field error")
	}
	if !strings.Contains(err.Error(), `json: unknown field "pollIntervalSecond"`) {
		t.Fatalf("readConfigFile() error = %v, want unknown field pollIntervalSecond", err)
	}
}

func TestReadConfigFileMatchesTopLevelKeysCaseInsensitively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	contents := `{"Server":{"host":"0.0.0.0"},"SCHEDULER":{"pollIntervalSeconds":7}}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	partial, present, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile() error = %v", err)
	}
	if !present {
		t.Fatal("readConfigFile() present = false, want true")
	}
	if partial.Server == nil || partial.Server.Host == nil || *partial.Server.Host != "0.0.0.0" {
		t.Fatalf("readConfigFile() server host = %#v, want 0.0.0.0", partial.Server)
	}
	if partial.Scheduler == nil || partial.Scheduler.PollIntervalSeconds == nil || *partial.Scheduler.PollIntervalSeconds != 7 {
		t.Fatalf("readConfigFile() scheduler = %#v, want pollIntervalSeconds 7", partial.Scheduler)
	}
}

func TestReadConfigFileMergesDuplicateTopLevelSectionsInEncounterOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	contents := `{"SERVER":{"host":"0.0.0.0"},"Server":{"port":8123}}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	partial, present, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile() error = %v", err)
	}
	if !present {
		t.Fatal("readConfigFile() present = false, want true")
	}
	if partial.Server == nil || partial.Server.Host == nil || *partial.Server.Host != "0.0.0.0" {
		t.Fatalf("readConfigFile() server host = %#v, want 0.0.0.0", partial.Server)
	}
	if partial.Server.Port == nil || *partial.Server.Port != 8123 {
		t.Fatalf("readConfigFile() server port = %#v, want 8123", partial.Server)
	}
}

func TestReadConfigFileLaterNullClearsDuplicateKnownSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	contents := `{"server":{"host":"0.0.0.0"},"server":null}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	partial, present, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile() error = %v", err)
	}
	if !present {
		t.Fatal("readConfigFile() present = false, want true")
	}
	if partial.Server != nil {
		t.Fatalf("readConfigFile() server = %#v, want nil", partial.Server)
	}
}

func TestReadConfigFileRejectsInvalidEarlierDuplicateKnownSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	contents := `{"SERVER":{"bad":1},"Server":{"host":"127.0.0.2"}}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, present, err := readConfigFile(path)
	if !present {
		t.Fatal("readConfigFile() present = false, want true")
	}
	if err == nil {
		t.Fatal("readConfigFile() error = nil, want unknown field error")
	}
	if !strings.Contains(err.Error(), `json: unknown field "bad"`) {
		t.Fatalf("readConfigFile() error = %v, want unknown field bad", err)
	}
}

func TestRoleDefaultsMirrorCurrentDiscoveryPolicy(t *testing.T) {
	cfg, err := Normalize(t.TempDir())
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	if got := cfg.Agent.Timeouts; got.PlannerSeconds != 3600 || got.WorkerSeconds != 10800 || got.ReviewerSeconds != 5400 || got.FixerSeconds != 7200 || got.PlannerMaxRuntimeSeconds != 3600 || got.WorkerMaxRuntimeSeconds != 10800 || got.ReviewerMaxRuntimeSeconds != 5400 || got.FixerMaxRuntimeSeconds != 7200 || got.PlannerIdleTimeoutSeconds != 600 || got.WorkerIdleTimeoutSeconds != 900 || got.ReviewerIdleTimeoutSeconds != 600 || got.FixerIdleTimeoutSeconds != 600 {
		t.Fatalf("agent timeout defaults = %#v", got)
	}

	if got := cfg.Roles.Planner; !got.AutoDiscovery || got.Triggers.LabelMode != LabelModeAll || !got.Triggers.RequireAssigneeCurrentUser || !reflectStringSlicesEqual(got.Triggers.Labels, []string{"looper:plan"}) {
		t.Fatalf("planner role defaults = %#v", got)
	}
	if got := cfg.Roles.Reviewer; !got.Discovery.AutoDiscovery || got.Discovery.Triggers.IncludeDrafts || !got.Discovery.Triggers.RequireReviewRequest || got.Discovery.Triggers.LabelMode != LabelModeAll || len(got.Discovery.Triggers.Labels) != 0 || !got.Discovery.SpecReview.IncludeReviewingLabel || got.Discovery.SpecReview.ReviewingLabel != "looper:spec-reviewing" {
		t.Fatalf("reviewer role defaults = %#v", got)
	}
	if got := cfg.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventApprove {
		t.Fatalf("reviewer clean review event default = %q, want %q", got, ReviewerReviewEventApprove)
	}
	if got := cfg.Roles.Reviewer.Behavior.ReviewEvents.Blocking; got != ReviewerReviewEventRequestChanges {
		t.Fatalf("reviewer blocking review event default = %q, want %q", got, ReviewerReviewEventRequestChanges)
	}
	if got := reviewerEnableSelfReviewValue(t, cfg.Roles.Reviewer.Discovery.Triggers); got {
		t.Fatalf("reviewer enableSelfReview default = %v, want false", got)
	}
	if got := cfg.Roles.Fixer; !got.AutoDiscovery || got.Triggers.IncludeDrafts || got.Triggers.AuthorFilter != FixerAuthorFilterCurrentUser || got.Triggers.LabelMode != LabelModeAll || len(got.Triggers.Labels) != 0 {
		t.Fatalf("fixer role defaults = %#v", got)
	}
	if got := cfg.Roles.Worker; !got.AutoDiscovery || got.Triggers.LabelMode != LabelModeAll || !got.Triggers.RequireAssigneeCurrentUser || !reflectStringSlicesEqual(got.Triggers.Labels, []string{"looper:worker-ready"}) {
		t.Fatalf("worker role defaults = %#v", got)
	}
	if got := cfg.Roles.Reviewer.Behavior.Loop.MaxWallClockSeconds; got != 0 {
		t.Fatalf("reviewer loop max wall clock default = %d, want 0", got)
	}
}

func TestAgentTimeoutConfigOverrides(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{"agent": {"timeouts": {"plannerSeconds": 1200, "workerSeconds": 2400, "reviewerSeconds": 1500, "fixerSeconds": 900}}}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: configPath,
		Args:       []string{"--reviewer-agent-timeout-seconds", "2100", "--fixer-agent-timeout-seconds=1800"},
		LookupEnv: mapEnvLookup(map[string]string{
			"LOOPER_OSASCRIPT_ENABLED":               "false",
			"LOOPER_AGENT_TIMEOUTS_WORKER_SECONDS":   "4200",
			"LOOPER_AGENT_TIMEOUTS_REVIEWER_SECONDS": "3300",
		}),
		LookPath: fakeLookPath(map[string]string{}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	got := loaded.Config.Agent.Timeouts
	if got.PlannerSeconds != 1200 || got.PlannerMaxRuntimeSeconds != 1200 || got.WorkerSeconds != 4200 || got.WorkerMaxRuntimeSeconds != 4200 || got.ReviewerSeconds != 2100 || got.ReviewerMaxRuntimeSeconds != 2100 || got.FixerSeconds != 1800 || got.FixerMaxRuntimeSeconds != 1800 {
		t.Fatalf("agent timeouts = %#v", got)
	}
}

func TestValidateRejectsInvalidAgentTimeouts(t *testing.T) {
	cfg, err := Normalize(t.TempDir())
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	cfg.Agent.Timeouts.WorkerSeconds = 0

	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateWithOptions() error = %v, want ConfigValidationError", err)
	}
	if len(validationErr.Issues) != 1 || validationErr.Issues[0].Path != "agent.timeouts.workerSeconds" {
		t.Fatalf("validation issues = %#v", validationErr.Issues)
	}
}

func TestValidateRejectsAgentTimeoutDurationOverflow(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("overflow timeout value requires 64-bit int")
	}
	cfg, err := Normalize(t.TempDir())
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	const maxDuration = time.Duration(1<<63 - 1)
	cfg.Agent.Timeouts.ReviewerSeconds = int(maxDuration/time.Second) + 1

	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateWithOptions() error = %v, want ConfigValidationError", err)
	}
	if len(validationErr.Issues) != 1 || validationErr.Issues[0].Path != "agent.timeouts.reviewerSeconds" {
		t.Fatalf("validation issues = %#v", validationErr.Issues)
	}
}

func TestLegacyFixAllPullRequestsMapsToFixerAuthorFilter(t *testing.T) {
	trueValue := true
	cfg, err := Normalize(t.TempDir(), PartialConfig{Defaults: &PartialDefaultsConfig{FixAllPullRequests: &trueValue}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if cfg.Roles.Fixer.Triggers.AuthorFilter != FixerAuthorFilterAny {
		t.Fatalf("fixer authorFilter = %q, want %q", cfg.Roles.Fixer.Triggers.AuthorFilter, FixerAuthorFilterAny)
	}

	currentUser := FixerAuthorFilterCurrentUser
	cfg, err = Normalize(t.TempDir(), PartialConfig{Defaults: &PartialDefaultsConfig{FixAllPullRequests: &trueValue}, Roles: &PartialRoleConfigs{Fixer: &PartialFixerRoleConfig{Triggers: &PartialFixerRoleTriggersConfig{AuthorFilter: &currentUser}}}})
	if err != nil {
		t.Fatalf("Normalize() with explicit role error = %v", err)
	}
	if cfg.Roles.Fixer.Triggers.AuthorFilter != FixerAuthorFilterCurrentUser {
		t.Fatalf("explicit fixer authorFilter = %q, want %q", cfg.Roles.Fixer.Triggers.AuthorFilter, FixerAuthorFilterCurrentUser)
	}
}

func TestLegacyAndCanonicalConfigSurfacesProduceEquivalentTargets(t *testing.T) {
	testCases := []struct {
		name      string
		legacy    string
		canonical string
		extract   func(t *testing.T, cfg Config) any
	}{
		{
			name:      "reviewer behavior root",
			legacy:    `{"reviewer":{"reviewEvents":{"clean":"APPROVE","blocking":"REQUEST_CHANGES"},"loop":{"enabledByDefault":true,"quietPeriodSeconds":45,"minPublishIntervalSeconds":120},"nativeResume":{"onHeadChange":true,"reReviewPromptOnHeadChange":false}}}`,
			canonical: `{"roles":{"reviewer":{"behavior":{"reviewEvents":{"clean":"APPROVE","blocking":"REQUEST_CHANGES"},"loop":{"enabledByDefault":true,"quietPeriodSeconds":45,"minPublishIntervalSeconds":120},"nativeResume":{"onHeadChange":true,"reReviewPromptOnHeadChange":false}}}}}`,
			extract: func(t *testing.T, cfg Config) any {
				t.Helper()
				return toJSONValue(t, cfg.Roles.Reviewer.Behavior)
			},
		},
		{
			name:      "allowAutoApprove alias",
			legacy:    `{"defaults":{"allowAutoApprove":true}}`,
			canonical: `{"roles":{"reviewer":{"behavior":{"reviewEvents":{"clean":"APPROVE"}}}}}`,
			extract: func(t *testing.T, cfg Config) any {
				t.Helper()
				return map[string]any{"clean": cfg.Roles.Reviewer.Behavior.ReviewEvents.Clean, "blocking": cfg.Roles.Reviewer.Behavior.ReviewEvents.Blocking}
			},
		},
		{
			name:      "fixAllPullRequests alias",
			legacy:    `{"defaults":{"fixAllPullRequests":true}}`,
			canonical: `{"roles":{"fixer":{"triggers":{"authorFilter":"any"}}}}`,
			extract: func(t *testing.T, cfg Config) any {
				t.Helper()
				return cfg.Roles.Fixer.Triggers.AuthorFilter
			},
		},
		{
			name:      "project path alias",
			legacy:    `{"projects":[{"id":"demo","name":"Demo","path":"/repos/demo"}]}`,
			canonical: `{"projects":[{"id":"demo","name":"Demo","repoPath":"/repos/demo"}]}`,
			extract: func(t *testing.T, cfg Config) any {
				t.Helper()
				if len(cfg.Projects) != 1 {
					t.Fatalf("len(cfg.Projects) = %d, want 1", len(cfg.Projects))
				}
				return cfg.Projects[0].RepoPath
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			legacy := loadConfigFromJSONFixture(t, tc.legacy)
			canonical := loadConfigFromJSONFixture(t, tc.canonical)

			legacyValue := tc.extract(t, legacy.Config)
			canonicalValue := tc.extract(t, canonical.Config)
			if !reflect.DeepEqual(legacyValue, canonicalValue) {
				t.Fatalf("legacy target = %#v, canonical target = %#v", legacyValue, canonicalValue)
			}
		})
	}
}

func TestLegacyAndCanonicalFixerEnvOverridesProduceEquivalentTargets(t *testing.T) {
	legacy := loadConfigWithEnvFixture(t, map[string]string{"LOOPER_FIX_ALL_PULL_REQUESTS": "true"})
	canonical := loadConfigWithEnvFixture(t, map[string]string{"LOOPER_ROLES_FIXER_TRIGGERS_AUTHOR_FILTER": "any"})

	if legacy.Config.Roles.Fixer.Triggers.AuthorFilter != canonical.Config.Roles.Fixer.Triggers.AuthorFilter {
		t.Fatalf("legacy fixer authorFilter = %q, canonical fixer authorFilter = %q", legacy.Config.Roles.Fixer.Triggers.AuthorFilter, canonical.Config.Roles.Fixer.Triggers.AuthorFilter)
	}
}

func TestCanonicalizePartialForMigrationRemovesLegacySurfacesAfterNormalization(t *testing.T) {
	clean := ReviewerReviewEventComment
	allowAutoApprove := true
	fixAllPullRequests := true
	autoDiscovery := true
	reviewerInstruction := "review this"
	partial := PartialConfig{
		LegacyReviewer: &PartialReviewerConfig{ReviewEvents: &PartialReviewerReviewEventsConfig{Clean: &clean}},
		Defaults:       &PartialDefaultsConfig{AllowAutoApprove: &allowAutoApprove, FixAllPullRequests: &fixAllPullRequests},
		Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{
			AutoDiscovery: &autoDiscovery,
			Instructions:  &reviewerInstruction,
			Triggers:      &PartialReviewerRoleTriggersConfig{EnableSelfReview: &autoDiscovery},
		}},
		Projects: &[]PartialProjectRefConfig{{
			ID:           "repo",
			Name:         "Repo",
			Path:         "/tmp/repo",
			Instructions: map[string]string{"reviewer": "project reviewer"},
		}},
	}

	canonical := CanonicalizePartialForMigration(partial)
	if canonical.LegacyReviewer != nil {
		t.Fatalf("LegacyReviewer = %#v, want nil", canonical.LegacyReviewer)
	}
	if canonical.Defaults == nil || canonical.Defaults.AllowAutoApprove != nil || canonical.Defaults.FixAllPullRequests != nil {
		t.Fatalf("Defaults = %#v, want deprecated aliases removed", canonical.Defaults)
	}
	if canonical.Roles == nil || canonical.Roles.Reviewer == nil || canonical.Roles.Reviewer.Behavior == nil || canonical.Roles.Reviewer.Behavior.ReviewEvents == nil || canonical.Roles.Reviewer.Behavior.ReviewEvents.Clean == nil || *canonical.Roles.Reviewer.Behavior.ReviewEvents.Clean != ReviewerReviewEventComment {
		t.Fatalf("Reviewer behavior = %#v, want migrated clean review event", canonical.Roles)
	}
	if canonical.Roles.Reviewer.AutoDiscovery != nil || canonical.Roles.Reviewer.Triggers != nil || canonical.Roles.Reviewer.SpecReview != nil {
		t.Fatalf("Reviewer legacy aliases = %#v, want nil", canonical.Roles.Reviewer)
	}
	if canonical.Projects == nil || len(*canonical.Projects) != 1 {
		t.Fatalf("Projects = %#v, want one migrated project", canonical.Projects)
	}
	project := (*canonical.Projects)[0]
	if project.Path != "" || len(project.Instructions) != 0 {
		t.Fatalf("Project legacy fields = %#v, want cleared", project)
	}
	if project.RepoPath != "/tmp/repo" {
		t.Fatalf("Project RepoPath = %q, want /tmp/repo", project.RepoPath)
	}
	if project.Roles == nil || project.Roles.Reviewer == nil || project.Roles.Reviewer.Instructions == nil || *project.Roles.Reviewer.Instructions != "project reviewer" {
		t.Fatalf("Project reviewer instructions = %#v, want migrated reviewer instruction", project.Roles)
	}
	if canonical.Roles == nil || canonical.Roles.Fixer == nil || canonical.Roles.Fixer.Triggers == nil || canonical.Roles.Fixer.Triggers.AuthorFilter == nil || *canonical.Roles.Fixer.Triggers.AuthorFilter != FixerAuthorFilterAny {
		t.Fatalf("Fixer triggers = %#v, want migrated author filter", canonical.Roles)
	}
}

func TestLegacyAndCanonicalReviewerEnvOverridesResolveIdenticallyAndBeatFileConfig(t *testing.T) {
	cleanFile := `{"roles":{"reviewer":{"behavior":{"reviewEvents":{"clean":"COMMENT"}}}}}`
	legacyClean := loadConfigFromJSONWithEnvAndArgsFixture(t, cleanFile, map[string]string{"LOOPER_ALLOW_AUTO_APPROVE": "true"}, nil)
	canonicalClean := loadConfigFromJSONWithEnvAndArgsFixture(t, cleanFile, map[string]string{"LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_CLEAN": "APPROVE"}, nil)

	if got := legacyClean.Config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventApprove {
		t.Fatalf("legacy env clean review event = %q, want %q", got, ReviewerReviewEventApprove)
	}
	if got := canonicalClean.Config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventApprove {
		t.Fatalf("canonical env clean review event = %q, want %q", got, ReviewerReviewEventApprove)
	}

	selfReviewFile := `{"roles":{"reviewer":{"discovery":{"triggers":{"enableSelfReview":false}}}}}`
	legacySelfReview := loadConfigFromJSONWithEnvAndArgsFixture(t, selfReviewFile, map[string]string{"LOOPER_ROLES_REVIEWER_TRIGGERS_ENABLE_SELF_REVIEW": "true"}, nil)
	canonicalSelfReview := loadConfigFromJSONWithEnvAndArgsFixture(t, selfReviewFile, map[string]string{"LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_ENABLE_SELF_REVIEW": "true"}, nil)

	if got := reviewerEnableSelfReviewValue(t, legacySelfReview.Config.Roles.Reviewer.Discovery.Triggers); !got {
		t.Fatalf("legacy env reviewer enableSelfReview = %v, want true", got)
	}
	if got := reviewerEnableSelfReviewValue(t, canonicalSelfReview.Config.Roles.Reviewer.Discovery.Triggers); !got {
		t.Fatalf("canonical env reviewer enableSelfReview = %v, want true", got)
	}
}

func TestLegacyAndCanonicalReviewerCLIOverridesResolveIdenticallyAndBeatFileConfig(t *testing.T) {
	cleanFile := `{"roles":{"reviewer":{"behavior":{"reviewEvents":{"clean":"COMMENT"}}}}}`
	legacyClean := loadConfigFromJSONWithEnvAndArgsFixture(t, cleanFile, nil, []string{"--allow-auto-approve=true"})
	canonicalClean := loadConfigFromJSONWithEnvAndArgsFixture(t, cleanFile, nil, []string{"--roles-reviewer-behavior-review-events-clean=APPROVE"})

	if got := legacyClean.Config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventApprove {
		t.Fatalf("legacy cli clean review event = %q, want %q", got, ReviewerReviewEventApprove)
	}
	if got := canonicalClean.Config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventApprove {
		t.Fatalf("canonical cli clean review event = %q, want %q", got, ReviewerReviewEventApprove)
	}

	selfReviewFile := `{"roles":{"reviewer":{"discovery":{"triggers":{"enableSelfReview":false}}}}}`
	legacySelfReview := loadConfigFromJSONWithEnvAndArgsFixture(t, selfReviewFile, nil, []string{"--reviewer-enable-self-review=true"})
	canonicalSelfReview := loadConfigFromJSONWithEnvAndArgsFixture(t, selfReviewFile, nil, []string{"--roles-reviewer-discovery-triggers-enable-self-review=true"})

	if got := reviewerEnableSelfReviewValue(t, legacySelfReview.Config.Roles.Reviewer.Discovery.Triggers); !got {
		t.Fatalf("legacy cli reviewer enableSelfReview = %v, want true", got)
	}
	if got := reviewerEnableSelfReviewValue(t, canonicalSelfReview.Config.Roles.Reviewer.Discovery.Triggers); !got {
		t.Fatalf("canonical cli reviewer enableSelfReview = %v, want true", got)
	}
}

func TestCanonicalReviewerCleanCLIOverrideWinsOverAllowAutoApproveRegardlessOfOrder(t *testing.T) {
	file := `{"roles":{"reviewer":{"behavior":{"reviewEvents":{"clean":"COMMENT"}}}}}`

	loaded := loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--roles-reviewer-behavior-review-events-clean=COMMENT",
		"--allow-auto-approve=true",
	})
	if got := loaded.Config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventComment {
		t.Fatalf("clean review event with canonical then legacy flags = %q, want %q", got, ReviewerReviewEventComment)
	}

	loaded = loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--allow-auto-approve=true",
		"--roles-reviewer-behavior-review-events-clean=COMMENT",
	})
	if got := loaded.Config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventComment {
		t.Fatalf("clean review event with legacy then canonical flags = %q, want %q", got, ReviewerReviewEventComment)
	}

	loaded = loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--roles-reviewer-behavior-review-events-clean=COMMENT",
		"--reviewer-clean-review-event=APPROVE",
	})
	if got := loaded.Config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventComment {
		t.Fatalf("clean review event with canonical then legacy alias = %q, want %q", got, ReviewerReviewEventComment)
	}

	loaded = loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--reviewer-clean-review-event=APPROVE",
		"--roles-reviewer-behavior-review-events-clean=COMMENT",
	})
	if got := loaded.Config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventComment {
		t.Fatalf("clean review event with legacy alias then canonical = %q, want %q", got, ReviewerReviewEventComment)
	}
}

func TestCanonicalFixerAuthorFilterCLIOverrideWinsOverFixAllPullRequestsRegardlessOfOrder(t *testing.T) {
	file := `{"roles":{"fixer":{"triggers":{"authorFilter":"current_user"}}}}`

	loaded := loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--roles-fixer-triggers-author-filter=current_user",
		"--fix-all-pull-requests=true",
	})
	if got := loaded.Config.Roles.Fixer.Triggers.AuthorFilter; got != FixerAuthorFilterCurrentUser {
		t.Fatalf("author filter with canonical then legacy flags = %q, want %q", got, FixerAuthorFilterCurrentUser)
	}

	loaded = loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--fix-all-pull-requests=true",
		"--roles-fixer-triggers-author-filter=current_user",
	})
	if got := loaded.Config.Roles.Fixer.Triggers.AuthorFilter; got != FixerAuthorFilterCurrentUser {
		t.Fatalf("author filter with legacy then canonical flags = %q, want %q", got, FixerAuthorFilterCurrentUser)
	}
}

func TestCanonicalReviewerEnableSelfReviewCLIOverrideWinsOverLegacyAliasRegardlessOfOrder(t *testing.T) {
	file := `{"roles":{"reviewer":{"discovery":{"triggers":{"enableSelfReview":true}}}}}`

	loaded := loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--roles-reviewer-discovery-triggers-enable-self-review=false",
		"--reviewer-enable-self-review=true",
	})
	if got := reviewerEnableSelfReviewValue(t, loaded.Config.Roles.Reviewer.Discovery.Triggers); got {
		t.Fatalf("enableSelfReview with canonical then legacy flags = %v, want false", got)
	}

	loaded = loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--reviewer-enable-self-review=true",
		"--roles-reviewer-discovery-triggers-enable-self-review=false",
	})
	if got := reviewerEnableSelfReviewValue(t, loaded.Config.Roles.Reviewer.Discovery.Triggers); got {
		t.Fatalf("enableSelfReview with legacy then canonical flags = %v, want false", got)
	}
}

func TestCanonicalReviewerBlockingCLIOverrideWinsOverLegacyAliasRegardlessOfOrder(t *testing.T) {
	file := `{"roles":{"reviewer":{"behavior":{"reviewEvents":{"blocking":"COMMENT"}}}}}`

	loaded := loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--roles-reviewer-behavior-review-events-blocking=COMMENT",
		"--reviewer-blocking-review-event=REQUEST_CHANGES",
	})
	if got := loaded.Config.Roles.Reviewer.Behavior.ReviewEvents.Blocking; got != ReviewerReviewEventComment {
		t.Fatalf("blocking review event with canonical then legacy flags = %q, want %q", got, ReviewerReviewEventComment)
	}

	loaded = loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--reviewer-blocking-review-event=REQUEST_CHANGES",
		"--roles-reviewer-behavior-review-events-blocking=COMMENT",
	})
	if got := loaded.Config.Roles.Reviewer.Behavior.ReviewEvents.Blocking; got != ReviewerReviewEventComment {
		t.Fatalf("blocking review event with legacy then canonical flags = %q, want %q", got, ReviewerReviewEventComment)
	}
}

func TestCanonicalReviewerLoopEnabledCLIOverrideWinsOverLegacyAliasRegardlessOfOrder(t *testing.T) {
	file := `{"roles":{"reviewer":{"behavior":{"loop":{"enabledByDefault":true}}}}}`

	loaded := loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--roles-reviewer-behavior-loop-enabled-by-default=false",
		"--reviewer-loop-enabled=true",
	})
	if got := loaded.Config.Roles.Reviewer.Behavior.Loop.EnabledByDefault; got {
		t.Fatalf("loop enabled with canonical then legacy flags = %v, want false", got)
	}

	loaded = loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--reviewer-loop-enabled=true",
		"--roles-reviewer-behavior-loop-enabled-by-default=false",
	})
	if got := loaded.Config.Roles.Reviewer.Behavior.Loop.EnabledByDefault; got {
		t.Fatalf("loop enabled with legacy then canonical flags = %v, want false", got)
	}
}

func TestCanonicalInstructionsEnabledCLIOverrideWinsOverLegacyAliasRegardlessOfOrder(t *testing.T) {
	file := `{"instructions":{"enabled":false}}`

	loaded := loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--instructions-enabled=true",
		"--no-custom-instructions=true",
	})
	if got := loaded.Config.Instructions.Enabled; !got {
		t.Fatal("instructions enabled with canonical then legacy flags = false, want true")
	}

	loaded = loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--no-custom-instructions=true",
		"--instructions-enabled=true",
	})
	if got := loaded.Config.Instructions.Enabled; !got {
		t.Fatal("instructions enabled with legacy then canonical flags = false, want true")
	}
}

func TestCanonicalPackageAutoUpgradeCLIOverrideWinsOverLegacyAliasRegardlessOfOrder(t *testing.T) {
	file := `{"package":{"autoUpgradeEnabled":false}}`

	loaded := loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--package-auto-upgrade-enabled=true",
		"--no-auto-upgrade=true",
	})
	if got := loaded.Config.Package.AutoUpgradeEnabled; !got {
		t.Fatal("package autoUpgradeEnabled with canonical then legacy flags = false, want true")
	}

	loaded = loadConfigFromJSONWithEnvAndArgsFixture(t, file, nil, []string{
		"--no-auto-upgrade=true",
		"--package-auto-upgrade-enabled=true",
	})
	if got := loaded.Config.Package.AutoUpgradeEnabled; !got {
		t.Fatal("package autoUpgradeEnabled with legacy then canonical flags = false, want true")
	}
}

func TestMixedSchemaConfigAcceptsDeterministicInputsWithCanonicalWinning(t *testing.T) {
	testCases := []struct {
		name         string
		fileName     string
		contents     string
		assertConfig func(t *testing.T, loaded LoadedFileConfig)
		wantWarnings []string
	}{
		{
			name:     "legacy reviewer root loses to canonical behavior",
			fileName: "config.json",
			contents: `{"reviewer":{"reviewEvents":{"clean":"APPROVE"}},"roles":{"reviewer":{"behavior":{"reviewEvents":{"clean":"COMMENT"}}}}}`,
			assertConfig: func(t *testing.T, loaded LoadedFileConfig) {
				t.Helper()
				if got := loaded.Config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventComment {
					t.Fatalf("clean review event = %q, want %q", got, ReviewerReviewEventComment)
				}
			},
			wantWarnings: []string{`deprecated config path "reviewer" is accepted for now; use "roles.reviewer.behavior" instead`},
		},
		{
			name:     "legacy reviewer discovery root loses to canonical discovery",
			fileName: "config.toml",
			contents: "[roles.reviewer]\nautoDiscovery = false\n\n[roles.reviewer.discovery]\nautoDiscovery = true\n",
			assertConfig: func(t *testing.T, loaded LoadedFileConfig) {
				t.Helper()
				if !loaded.Config.Roles.Reviewer.Discovery.AutoDiscovery {
					t.Fatal("reviewer discovery autoDiscovery = false, want true")
				}
			},
			wantWarnings: []string{`deprecated config path "roles.reviewer.autoDiscovery" is accepted for now; use "roles.reviewer.discovery.autoDiscovery" instead`},
		},
		{
			name:     "legacy defaults alias loses to canonical fixer target",
			fileName: "config.yaml",
			contents: "defaults:\n  fixAllPullRequests: true\nroles:\n  fixer:\n    triggers:\n      authorFilter: current_user\n",
			assertConfig: func(t *testing.T, loaded LoadedFileConfig) {
				t.Helper()
				if got := loaded.Config.Roles.Fixer.Triggers.AuthorFilter; got != FixerAuthorFilterCurrentUser {
					t.Fatalf("fixer authorFilter = %q, want %q", got, FixerAuthorFilterCurrentUser)
				}
			},
			wantWarnings: []string{`deprecated config path "defaults.fixAllPullRequests" is accepted for now; use "roles.fixer.triggers.authorFilter" instead`},
		},
		{
			name:     "legacy project instructions lose to canonical project role instructions",
			fileName: "config.json",
			contents: `{"projects":[{"id":"demo","name":"Demo","repoPath":"/repos/demo","instructions":{"worker":"legacy"},"roles":{"worker":{"instructions":"canonical"}}}]}`,
			assertConfig: func(t *testing.T, loaded LoadedFileConfig) {
				t.Helper()
				block := BuildCustomInstructionBlock(loaded.Config, "demo", "worker")
				if !strings.Contains(block.Text, "canonical") || strings.Contains(block.Text, "legacy") {
					t.Fatalf("custom instruction block = %q, want canonical project instructions to win", block.Text)
				}
			},
			wantWarnings: []string{`deprecated config path "projects[].instructions" is accepted for now; use "projects[].roles.<role>.instructions" instead`},
		},
		{
			name:     "legacy project reviewer discovery loses to canonical discovery",
			fileName: "config.toml",
			contents: "[[projects]]\nid = \"demo\"\nname = \"Demo\"\nrepoPath = \"/repos/demo\"\n\n[projects.roles.reviewer]\nautoDiscovery = false\n\n[projects.roles.reviewer.discovery]\nautoDiscovery = true\n",
			assertConfig: func(t *testing.T, loaded LoadedFileConfig) {
				t.Helper()
				if !ProjectRoleConfigs(loaded.Config, "demo").Reviewer.Discovery.AutoDiscovery {
					t.Fatal("project reviewer discovery autoDiscovery = false, want true")
				}
			},
			wantWarnings: []string{`deprecated config path "projects[].roles.reviewer.autoDiscovery" is accepted for now; use "projects[].roles.reviewer.discovery.autoDiscovery" instead`},
		},
		{
			name:     "legacy project path alias is preserved when it matches repoPath",
			fileName: "config.yaml",
			contents: "projects:\n  - id: demo\n    name: Demo\n    path: /repos/demo\n    repoPath: /repos/demo\n",
			assertConfig: func(t *testing.T, loaded LoadedFileConfig) {
				t.Helper()
				if got := loaded.Config.Projects[0].RepoPath; got != "/repos/demo" {
					t.Fatalf("project repoPath = %q, want %q", got, "/repos/demo")
				}
			},
			wantWarnings: []string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			loaded := loadConfigFixture(t, tc.fileName, tc.contents, nil, nil)
			tc.assertConfig(t, loaded)
			assertWarningsEqual(t, loaded.Warnings, tc.wantWarnings)
		})
	}
}

func TestMixedSchemaEnvAndCLIOverridesStillBeatFileBackedValues(t *testing.T) {
	file := `{"defaults":{"allowAutoApprove":true},"roles":{"reviewer":{"behavior":{"reviewEvents":{"clean":"COMMENT"}}}}}`

	envLoaded := loadConfigFixture(t, "config.json", file, map[string]string{"LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_CLEAN": "APPROVE"}, nil)
	if got := envLoaded.Config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventApprove {
		t.Fatalf("env clean review event = %q, want %q", got, ReviewerReviewEventApprove)
	}
	assertWarningsEqual(t, envLoaded.Warnings, []string{`deprecated config path "defaults.allowAutoApprove" is accepted for now; use "roles.reviewer.behavior.reviewEvents.clean" instead`})

	cliLoaded := loadConfigFixture(t, "config.json", file, nil, []string{"--roles-reviewer-behavior-review-events-clean=APPROVE"})
	if got := cliLoaded.Config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventApprove {
		t.Fatalf("cli clean review event = %q, want %q", got, ReviewerReviewEventApprove)
	}
	assertWarningsEqual(t, cliLoaded.Warnings, []string{`deprecated config path "defaults.allowAutoApprove" is accepted for now; use "roles.reviewer.behavior.reviewEvents.clean" instead`})
}

func TestLoadFileRejectsMismatchedProjectPathAndRepoPathAfterNormalization(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{"projects":[{"id":"demo","name":"Demo","path":"/repos/legacy","repoPath":"/repos/canonical"}]}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want config validation error")
	}

	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("LoadFile() error = %T, want *ConfigValidationError", err)
	}

	assertValidationIssue(t, validationErr, "projects[0].path", "must match repoPath when both path and repoPath are set")
}

func TestDeprecatedAliasWarningsDeduplicateAndUseExactReplacementNames(t *testing.T) {
	loaded := loadConfigFixture(t, "config.json", `{
		"projects": [
			{"id":"demo-a","name":"Demo A","repoPath":"/repos/demo-a","instructions":{"worker":"legacy a"}},
			{"id":"demo-b","name":"Demo B","repoPath":"/repos/demo-b","instructions":{"worker":"legacy b"}}
		]
	}`, map[string]string{
		"LOOPER_ROLES_REVIEWER_TRIGGERS_ENABLE_SELF_REVIEW": "true",
	}, []string{
		"--reviewer-enable-self-review=true",
		"--reviewer-enable-self-review=false",
	})

	assertWarningsEqual(t, loaded.Warnings, []string{
		`deprecated config path "projects[].instructions" is accepted for now; use "projects[].roles.<role>.instructions" instead`,
		`deprecated environment variable "LOOPER_ROLES_REVIEWER_TRIGGERS_ENABLE_SELF_REVIEW" is accepted for now; use "LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_ENABLE_SELF_REVIEW" instead`,
		`deprecated CLI flag "--reviewer-enable-self-review" is accepted for now; use "--roles-reviewer-discovery-triggers-enable-self-review" instead`,
	})
}

func TestLoadFileRejectsUnsupportedConfigSuffixWithExactMessage(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.txt")
	if err := os.WriteFile(configPath, []byte("server:\n  port: 6101\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want error")
	}

	want := fmt.Sprintf("unsupported config file suffix %q at %s; supported suffixes: .toml, .yaml, .yml, .json", ".txt", configPath)
	if got := err.Error(); got != want {
		t.Fatalf("LoadFile() error = %q, want %q", got, want)
	}
}

func TestLegacyReviewerConfigStillValidatesAgainstCanonicalRules(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{"reviewer":{"scope":"bad-scope","loop":{"quietPeriodSeconds":-1}}}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want config validation error")
	}

	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("LoadFile() error = %T, want *ConfigValidationError", err)
	}

	assertValidationIssue(t, validationErr, "roles.reviewer.behavior.loop.quietPeriodSeconds", "must be an integer >= 0")
	assertValidationIssue(t, validationErr, "roles.reviewer.behavior.scope", "must be one of: full_pr, changed_files, changed_ranges")
}

func TestMixedSchemaConfigRejectsStructurallyIncompatibleTargets(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{"reviewer":{"reviewEvents":{"clean":"APPROVE"}},"roles":{"reviewer":{"behavior":{"reviewEvents":"COMMENT"}}}}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want error")
	}
	if !strings.Contains(err.Error(), `reviewEvents`) {
		t.Fatalf("LoadFile() error = %q, want reviewEvents incompatibility", err)
	}
}

func TestNormalizeLayersProduceEquivalentEffectiveConfigAcrossCanonicalLegacyAndMixedInputs(t *testing.T) {
	legacyFile := PartialConfig{
		Defaults: &PartialDefaultsConfig{
			AllowAutoApprove:   boolPtr(true),
			FixAllPullRequests: boolPtr(true),
		},
		LegacyReviewer: &PartialReviewerConfig{
			Loop: &PartialReviewerLoopConfig{EnabledByDefault: boolPtr(true)},
		},
		Roles: &PartialRoleConfigs{
			Reviewer: &PartialReviewerRoleConfig{
				AutoDiscovery: boolPtr(false),
				Triggers:      &PartialReviewerRoleTriggersConfig{Labels: &[]string{"legacy-file"}, LabelMode: labelModePtr(LabelModeAll)},
			},
		},
		Projects: &[]PartialProjectRefConfig{{
			ID:           "demo",
			Name:         "Demo",
			Path:         "/repos/demo",
			Instructions: map[string]string{"worker": "legacy worker instructions"},
			Roles:        &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{AutoDiscovery: boolPtr(false)}},
		}},
	}
	canonicalFile := PartialConfig{
		Defaults: &PartialDefaultsConfig{
			AllowAutoApprove:   boolPtr(true),
			FixAllPullRequests: boolPtr(true),
		},
		Roles: &PartialRoleConfigs{
			Reviewer: &PartialReviewerRoleConfig{
				Behavior:  &PartialReviewerConfig{Loop: &PartialReviewerLoopConfig{EnabledByDefault: boolPtr(true)}},
				Discovery: &PartialReviewerRoleDiscoveryConfig{AutoDiscovery: boolPtr(false), Triggers: &PartialReviewerRoleTriggersConfig{Labels: &[]string{"legacy-file"}, LabelMode: labelModePtr(LabelModeAll)}},
			},
		},
		Projects: &[]PartialProjectRefConfig{{
			ID:       "demo",
			Name:     "Demo",
			RepoPath: "/repos/demo",
			Roles: &PartialRoleConfigs{
				Worker:   &PartialWorkerRoleConfig{Instructions: stringPtr("legacy worker instructions")},
				Reviewer: &PartialReviewerRoleConfig{Discovery: &PartialReviewerRoleDiscoveryConfig{AutoDiscovery: boolPtr(false)}},
			},
		}},
	}

	legacyEnv, err := buildEnvOverrides(mapEnvLookup(map[string]string{
		"LOOPER_ALLOW_AUTO_APPROVE":                         "false",
		"LOOPER_ROLES_REVIEWER_TRIGGERS_ENABLE_SELF_REVIEW": "true",
	}))
	if err != nil {
		t.Fatalf("buildEnvOverrides(legacy) error = %v", err)
	}
	canonicalEnv, err := buildEnvOverrides(mapEnvLookup(map[string]string{
		"LOOPER_ROLES_REVIEWER_BEHAVIOR_REVIEW_EVENTS_CLEAN":          "COMMENT",
		"LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_ENABLE_SELF_REVIEW": "true",
	}))
	if err != nil {
		t.Fatalf("buildEnvOverrides(canonical) error = %v", err)
	}

	legacyCLI, err := parseCLIArgs([]string{"--reviewer-loop-enabled=true", "--reviewer-enable-self-review=false", "--fix-all-pull-requests=false"})
	if err != nil {
		t.Fatalf("parseCLIArgs(legacy) error = %v", err)
	}
	canonicalCLI, err := parseCLIArgs([]string{"--roles-reviewer-behavior-loop-enabled-by-default=true", "--roles-reviewer-discovery-triggers-enable-self-review=false", "--roles-fixer-triggers-author-filter=current_user"})
	if err != nil {
		t.Fatalf("parseCLIArgs(canonical) error = %v", err)
	}

	cwd := t.TempDir()
	legacyConfig, err := Normalize(cwd, legacyFile, legacyEnv, legacyCLI.overrides)
	if err != nil {
		t.Fatalf("Normalize(legacy layers) error = %v", err)
	}
	canonicalConfig, err := Normalize(cwd, canonicalFile, canonicalEnv, canonicalCLI.overrides)
	if err != nil {
		t.Fatalf("Normalize(canonical layers) error = %v", err)
	}
	mixedConfig, err := Normalize(cwd, legacyFile, canonicalEnv, legacyCLI.overrides)
	if err != nil {
		t.Fatalf("Normalize(mixed layers) error = %v", err)
	}

	legacyEffective := map[string]any{
		"clean":             legacyConfig.Roles.Reviewer.Behavior.ReviewEvents.Clean,
		"fixerAuthorFilter": legacyConfig.Roles.Fixer.Triggers.AuthorFilter,
		"loopEnabled":       legacyConfig.Roles.Reviewer.Behavior.Loop.EnabledByDefault,
		"enableSelfReview":  legacyConfig.Roles.Reviewer.Discovery.Triggers.EnableSelfReview,
		"reviewerLabels":    legacyConfig.Roles.Reviewer.Discovery.Triggers.Labels,
		"projectRepoPath":   legacyConfig.Projects[0].RepoPath,
		"projectWorkerText": *legacyConfig.Projects[0].Roles.Worker.Instructions,
	}
	canonicalEffective := map[string]any{
		"clean":             canonicalConfig.Roles.Reviewer.Behavior.ReviewEvents.Clean,
		"fixerAuthorFilter": canonicalConfig.Roles.Fixer.Triggers.AuthorFilter,
		"loopEnabled":       canonicalConfig.Roles.Reviewer.Behavior.Loop.EnabledByDefault,
		"enableSelfReview":  canonicalConfig.Roles.Reviewer.Discovery.Triggers.EnableSelfReview,
		"reviewerLabels":    canonicalConfig.Roles.Reviewer.Discovery.Triggers.Labels,
		"projectRepoPath":   canonicalConfig.Projects[0].RepoPath,
		"projectWorkerText": *canonicalConfig.Projects[0].Roles.Worker.Instructions,
	}
	mixedEffective := map[string]any{
		"clean":             mixedConfig.Roles.Reviewer.Behavior.ReviewEvents.Clean,
		"fixerAuthorFilter": mixedConfig.Roles.Fixer.Triggers.AuthorFilter,
		"loopEnabled":       mixedConfig.Roles.Reviewer.Behavior.Loop.EnabledByDefault,
		"enableSelfReview":  mixedConfig.Roles.Reviewer.Discovery.Triggers.EnableSelfReview,
		"reviewerLabels":    mixedConfig.Roles.Reviewer.Discovery.Triggers.Labels,
		"projectRepoPath":   mixedConfig.Projects[0].RepoPath,
		"projectWorkerText": *mixedConfig.Projects[0].Roles.Worker.Instructions,
	}

	if !reflect.DeepEqual(legacyEffective, canonicalEffective) {
		t.Fatalf("legacy effective = %#v, canonical effective = %#v", legacyEffective, canonicalEffective)
	}
	if !reflect.DeepEqual(mixedEffective, canonicalEffective) {
		t.Fatalf("mixed effective = %#v, canonical effective = %#v", mixedEffective, canonicalEffective)
	}
	if got := canonicalConfig.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventComment {
		t.Fatalf("clean review event = %q, want %q", got, ReviewerReviewEventComment)
	}
	if got := canonicalConfig.Roles.Fixer.Triggers.AuthorFilter; got != FixerAuthorFilterCurrentUser {
		t.Fatalf("fixer authorFilter = %q, want %q", got, FixerAuthorFilterCurrentUser)
	}
	if got := canonicalConfig.Roles.Reviewer.Discovery.Triggers.EnableSelfReview; got {
		t.Fatalf("reviewer enableSelfReview = %v, want false", got)
	}
	if got := canonicalConfig.Projects[0].RepoPath; got != "/repos/demo" {
		t.Fatalf("project repoPath = %q, want %q", got, "/repos/demo")
	}
	if got := canonicalConfig.Projects[0].Roles.Worker.Instructions; got == nil || *got != "legacy worker instructions" {
		t.Fatalf("project worker instructions = %v, want %q", got, "legacy worker instructions")
	}
}

func TestCanonicalizePartialForMigrationDoesNotMutateCaller(t *testing.T) {
	t.Parallel()

	original := PartialConfig{
		Defaults: &PartialDefaultsConfig{
			AllowAutoApprove: boolPtr(true),
		},
		Projects: &[]PartialProjectRefConfig{{
			ID:           "demo",
			Name:         "Demo",
			Path:         "/repos/demo",
			Instructions: map[string]string{"worker": "legacy worker instructions"},
		}},
	}
	wantOriginal := PartialConfig{
		Defaults: &PartialDefaultsConfig{
			AllowAutoApprove: boolPtr(true),
		},
		Projects: &[]PartialProjectRefConfig{{
			ID:           "demo",
			Name:         "Demo",
			Path:         "/repos/demo",
			Instructions: map[string]string{"worker": "legacy worker instructions"},
		}},
	}

	canonical := CanonicalizePartialForMigration(original)

	if !reflect.DeepEqual(original, wantOriginal) {
		t.Fatalf("CanonicalizePartialForMigration() mutated caller = %#v, want %#v", original, wantOriginal)
	}
	if original.Projects == canonical.Projects {
		t.Fatal("CanonicalizePartialForMigration() reused original projects slice")
	}
	if canonical.Defaults == nil || canonical.Defaults.AllowAutoApprove != nil {
		t.Fatalf("canonical defaults = %#v, want allowAutoApprove removed", canonical.Defaults)
	}
	if canonical.Projects == nil || len(*canonical.Projects) != 1 {
		t.Fatalf("canonical projects = %#v, want one project", canonical.Projects)
	}
	project := (*canonical.Projects)[0]
	if project.Path != "" || project.RepoPath != "/repos/demo" {
		t.Fatalf("canonical project paths = %#v, want path migrated to repoPath", project)
	}
	if len(project.Instructions) != 0 {
		t.Fatalf("canonical project instructions = %#v, want cleared legacy instructions", project.Instructions)
	}
	if project.Roles == nil || project.Roles.Worker == nil || project.Roles.Worker.Instructions == nil || *project.Roles.Worker.Instructions != "legacy worker instructions" {
		t.Fatalf("canonical project roles = %#v, want migrated worker instructions", project.Roles)
	}
	if original.Defaults == canonical.Defaults {
		t.Fatal("CanonicalizePartialForMigration() reused original defaults struct")
	}
}

func TestNormalizeLayersKeepDeepMergeForObjectsAndArrayReplacementForArrays(t *testing.T) {
	config, err := Normalize(t.TempDir(),
		PartialConfig{
			Agent: &PartialAgentConfig{Params: map[string]any{"shared": map[string]any{"file": true}, "fileOnly": "file"}},
			Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{Triggers: &PartialReviewerRoleTriggersConfig{Labels: &[]string{"file-a", "file-b"}, LabelMode: labelModePtr(LabelModeAll)}}},
		},
		PartialConfig{
			Agent: &PartialAgentConfig{Params: map[string]any{"shared": map[string]any{"env": true}, "envOnly": "env"}},
			Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{Discovery: &PartialReviewerRoleDiscoveryConfig{Triggers: &PartialReviewerRoleTriggersConfig{Labels: &[]string{"env-only"}, LabelMode: labelModePtr(LabelModeAny)}}}},
		},
		PartialConfig{
			Agent: &PartialAgentConfig{Params: map[string]any{"shared": map[string]any{"cli": true}, "cliOnly": "cli"}},
		},
	)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	shared, ok := config.Agent.Params["shared"].(map[string]any)
	if !ok {
		t.Fatalf("shared params type = %T, want map[string]any", config.Agent.Params["shared"])
	}
	if !reflect.DeepEqual(shared, map[string]any{"file": true, "env": true, "cli": true}) {
		t.Fatalf("shared params = %#v", shared)
	}
	if got := config.Agent.Params["fileOnly"]; got != "file" {
		t.Fatalf("fileOnly = %#v, want %q", got, "file")
	}
	if got := config.Agent.Params["envOnly"]; got != "env" {
		t.Fatalf("envOnly = %#v, want %q", got, "env")
	}
	if got := config.Agent.Params["cliOnly"]; got != "cli" {
		t.Fatalf("cliOnly = %#v, want %q", got, "cli")
	}
	if !reflect.DeepEqual(config.Roles.Reviewer.Discovery.Triggers.Labels, []string{"env-only"}) {
		t.Fatalf("reviewer labels = %#v, want %#v", config.Roles.Reviewer.Discovery.Triggers.Labels, []string{"env-only"})
	}
	if got := config.Roles.Reviewer.Discovery.Triggers.LabelMode; got != LabelModeAny {
		t.Fatalf("reviewer labelMode = %q, want %q", got, LabelModeAny)
	}
}

func TestRoleEnvironmentOverrides(t *testing.T) {
	cwd := t.TempDir()
	loaded, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: filepath.Join(cwd, "missing.json"),
		LookupEnv: mapEnvLookup(map[string]string{
			"LOOPER_OSASCRIPT_ENABLED":                                    "false",
			"LOOPER_ROLES_PLANNER_AUTO_DISCOVERY":                         "false",
			"LOOPER_ROLES_PLANNER_TRIGGERS_LABELS":                        "needs-plan,team:alpha",
			"LOOPER_ROLES_PLANNER_TRIGGERS_LABEL_MODE":                    "any",
			"LOOPER_ROLES_PLANNER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER": "false",
			"LOOPER_ROLES_REVIEWER_TRIGGERS_INCLUDE_DRAFTS":               "true",
			"LOOPER_ROLES_REVIEWER_TRIGGERS_REQUIRE_REVIEW_REQUEST":       "false",
			"LOOPER_ROLES_REVIEWER_TRIGGERS_LABELS":                       "needs-review,spec",
			"LOOPER_ROLES_REVIEWER_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL":   "false",
			"LOOPER_ROLES_FIXER_TRIGGERS_AUTHOR_FILTER":                   "any",
			"LOOPER_ROLES_FIXER_TRIGGERS_LABELS":                          "bugfix",
		}),
		LookPath: fakeLookPath(map[string]string{}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if loaded.Config.Roles.Planner.AutoDiscovery {
		t.Fatal("planner autoDiscovery = true, want false")
	}
	if !reflectStringSlicesEqual(loaded.Config.Roles.Planner.Triggers.Labels, []string{"needs-plan", "team:alpha"}) {
		t.Fatalf("planner labels = %#v", loaded.Config.Roles.Planner.Triggers.Labels)
	}
	if loaded.Config.Roles.Planner.Triggers.LabelMode != LabelModeAny || loaded.Config.Roles.Planner.Triggers.RequireAssigneeCurrentUser {
		t.Fatalf("planner triggers = %#v", loaded.Config.Roles.Planner.Triggers)
	}
	if !loaded.Config.Roles.Reviewer.Discovery.Triggers.IncludeDrafts || loaded.Config.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest {
		t.Fatalf("reviewer triggers = %#v", loaded.Config.Roles.Reviewer.Discovery.Triggers)
	}
	if !reflectStringSlicesEqual(loaded.Config.Roles.Reviewer.Discovery.Triggers.Labels, []string{"needs-review", "spec"}) || loaded.Config.Roles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel {
		t.Fatalf("reviewer discovery = %#v", loaded.Config.Roles.Reviewer)
	}
	if loaded.Config.Roles.Fixer.Triggers.AuthorFilter != FixerAuthorFilterAny || !reflectStringSlicesEqual(loaded.Config.Roles.Fixer.Triggers.Labels, []string{"bugfix"}) {
		t.Fatalf("fixer config = %#v", loaded.Config.Roles.Fixer)
	}
}

func reflectStringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func loadConfigFromJSONFixture(t *testing.T, contents string) LoadedFileConfig {
	t.Helper()
	return loadConfigFixture(t, "config.json", contents, nil, nil)
}

func loadConfigFixture(t *testing.T, fileName string, contents string, env map[string]string, args []string) LoadedFileConfig {
	t.Helper()

	cwd := t.TempDir()
	configPath := filepath.Join(cwd, fileName)
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	lookupEnv := emptyEnvLookup
	if env != nil {
		lookupEnv = mapEnvLookup(env)
	}

	loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: lookupEnv, Args: args, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	return loaded
}

func loadConfigWithEnvFixture(t *testing.T, env map[string]string) LoadedFileConfig {
	t.Helper()

	cwd := t.TempDir()
	loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: filepath.Join(cwd, "missing.json"), LookupEnv: mapEnvLookup(env), LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	return loaded
}

func loadConfigFromJSONWithEnvAndArgsFixture(t *testing.T, contents string, env map[string]string, args []string) LoadedFileConfig {
	t.Helper()
	return loadConfigFixture(t, "config.json", contents, env, args)
}

func assertWarningsEqual(t *testing.T, got []string, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("warnings = %#v, want %#v", got, want)
	}
}

func assertNoticesEqual(t *testing.T, got []string, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("notices = %#v, want %#v", got, want)
	}
}

func roleConfigMapFromRoles(t *testing.T, roles any, role string) (map[string]any, bool) {
	t.Helper()
	raw := toJSONValue(t, roles)
	roleMap, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("role map has unexpected type %T", raw)
	}
	value, ok := roleMap[role]
	if !ok {
		return nil, false
	}
	config, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	return config, true
}

func roleMapForRole(t *testing.T, roles any, role string) map[string]any {
	t.Helper()
	value, ok := roleConfigMapFromRoles(t, roles, role)
	if !ok {
		t.Fatalf("role config missing for %q", role)
	}
	return value
}

func roleMapFromRoleSection(t *testing.T, role map[string]any, section string) map[string]any {
	t.Helper()
	raw, ok := role[section]
	if !ok {
		t.Fatalf("role section %q missing", section)
	}
	sectionMap, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("role section %q has unexpected type %T", section, raw)
	}
	return sectionMap
}

func boolValueFromRoleMap(role map[string]any, key string) (bool, bool) {
	value, ok := role[key]
	if !ok {
		return false, false
	}
	b, ok := value.(bool)
	if !ok {
		return false, false
	}
	return b, true
}

func labelsFromRoleMap(t *testing.T, role map[string]any, key string) []string {
	t.Helper()
	raw, ok := role[key]
	if !ok {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("labels field %q has unexpected type %T", key, raw)
	}
	labels := make([]string, 0, len(items))
	for _, item := range items {
		label, ok := item.(string)
		if !ok {
			t.Fatalf("labels field %q contains unexpected item type %T", key, item)
		}
		labels = append(labels, label)
	}
	return labels
}

func reviewerEnableSelfReviewValue(t *testing.T, triggers any) bool {
	t.Helper()
	field := reflect.ValueOf(triggers).FieldByName("EnableSelfReview")
	if !field.IsValid() {
		t.Fatal("ReviewerRoleTriggersConfig.EnableSelfReview is missing")
	}
	if field.Kind() != reflect.Bool {
		t.Fatalf("ReviewerRoleTriggersConfig.EnableSelfReview kind = %s, want bool", field.Kind())
	}
	return field.Bool()
}

func assertValidationIssueForPaths(t *testing.T, issues []ValidationIssue, wanted []string) {
	t.Helper()
	for _, path := range wanted {
		found := false
		for _, issue := range issues {
			if issue.Path == path {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing validation issue for path %q in %#v", path, issues)
		}
	}
}

func assertValidationIssueForPathPrefix(t *testing.T, issues []ValidationIssue, prefix string) {
	t.Helper()
	for _, issue := range issues {
		if strings.HasPrefix(issue.Path, prefix) {
			return
		}
	}
	t.Fatalf("missing validation issue with path prefix %q in %#v", prefix, issues)
}

func TestLoadFileResolvesRelativePathsAgainstCWD(t *testing.T) {
	cwd := t.TempDir()
	relativePath := filepath.Join("configs", "looper.json")
	configPath := filepath.Join(cwd, relativePath)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}

	contents := `{
		"server": {"port": 6123},
		"projects": [{"id": "demo", "name": "Demo", "repoPath": "/repos/demo"}]
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: relativePath, LookupEnv: emptyEnvLookup})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	if loaded.Metadata.ConfigPath != configPath {
		t.Fatalf("LoadFile().Metadata.ConfigPath = %q, want %q", loaded.Metadata.ConfigPath, configPath)
	}

	if !loaded.Metadata.ConfigFilePresent {
		t.Fatal("LoadFile().Metadata.ConfigFilePresent = false, want true")
	}

	if loaded.Config.Server.Port != 6123 {
		t.Fatalf("LoadFile().Config.Server.Port = %d, want %d", loaded.Config.Server.Port, 6123)
	}

	if len(loaded.Config.Projects) != 1 || loaded.Config.Projects[0].ID != "demo" {
		t.Fatalf("LoadFile().Config.Projects = %#v, want single demo project", loaded.Config.Projects)
	}

	if loaded.Partial.Server == nil || loaded.Partial.Server.Port == nil || *loaded.Partial.Server.Port != 6123 {
		t.Fatalf("LoadFile().Partial.Server.Port = %#v, want 6123", loaded.Partial.Server)
	}
}

func TestLoadFileReturnsClearErrorForInvalidJSON(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	if err := os.WriteFile(configPath, []byte("{"), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want error")
	}

	if !strings.Contains(err.Error(), "failed to read config file at "+configPath) {
		t.Fatalf("LoadFile() error = %q, want path context", err)
	}
}

func TestLoadFileIgnoresUnknownConfigFields(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{
		"futureFeature": {"enabled": true},
		"server": {"port": 6123}
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	if loaded.Config.Server.Port != 6123 {
		t.Fatalf("LoadFile().Config.Server.Port = %d, want %d", loaded.Config.Server.Port, 6123)
	}

	if loaded.Partial.Server == nil || loaded.Partial.Server.Port == nil || *loaded.Partial.Server.Port != 6123 {
		t.Fatalf("LoadFile().Partial.Server.Port = %#v, want 6123", loaded.Partial.Server)
	}
}

func TestLoadFileSupportsCustomInstructions(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{
		"instructions": {"enabled": true, "maxBytes": 128},
		"roles": {"planner": {"instructions": "Keep specs scoped."}},
		"projects": [{"id": "demo", "name": "Demo", "path": "/repos/demo", "instructions": {"planner": "Respect local config precedence."}}]
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if got := loaded.Config.Roles.Planner.Instructions; got != "Keep specs scoped." {
		t.Fatalf("planner instructions = %q", got)
	}
	if got := loaded.Config.Projects[0].RepoPath; got != "/repos/demo" {
		t.Fatalf("project repo path = %q", got)
	}
	block := BuildCustomInstructionBlock(loaded.Config, "demo", "planner")
	if strings.Contains(block.Text, "Keep specs scoped.") || !strings.Contains(block.Text, "Respect local config precedence.") {
		t.Fatalf("custom block missing instructions: %s", block.Text)
	}
}

func TestProjectRoleConfigOverridesGlobalRoleConfig(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{
		"roles": {
			"planner": {"autoDiscovery": false, "triggers": {"labels": ["needs-plan"], "labelMode": "all", "requireAssigneeCurrentUser": true}, "instructions": "Global planner guidance."},
			"worker": {"autoDiscovery": false, "triggers": {"labels": ["global"], "labelMode": "all", "requireAssigneeCurrentUser": true}, "instructions": "Global worker guidance."},
			"reviewer": {"triggers": {"includeDrafts": false, "requireReviewRequest": true, "enableSelfReview": false, "labels": ["review"], "labelMode": "all"}},
			"fixer": {"autoDiscovery": false, "triggers": {"includeDrafts": false, "authorFilter": "current_user", "labels": ["needs-fix"], "labelMode": "all"}, "instructions": "Global fixer guidance."}
		},
		"projects": [{
			"id": "demo",
			"name": "Demo",
			"repoPath": "/repos/demo",
			"roles": {
				"planner": {"autoDiscovery": true, "triggers": {"labels": ["project-plan"], "labelMode": "any", "requireAssigneeCurrentUser": false}, "instructions": "Project planner guidance."},
				"worker": {"autoDiscovery": true, "triggers": {"labels": ["project"], "labelMode": "any", "requireAssigneeCurrentUser": false}, "instructions": "Project worker guidance."},
				"reviewer": {"triggers": {"includeDrafts": true, "enableSelfReview": true}},
				"fixer": {"autoDiscovery": true, "triggers": {"includeDrafts": true, "authorFilter": "any", "labels": ["project-fix"], "labelMode": "any"}, "instructions": "Project fixer guidance."}
			}
		}]
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	global := ProjectRoleConfigs(loaded.Config, "missing")
	if global.Planner.AutoDiscovery || !reflectStringSlicesEqual(global.Planner.Triggers.Labels, []string{"needs-plan"}) || !global.Planner.Triggers.RequireAssigneeCurrentUser {
		t.Fatalf("global planner roles = %#v", global.Planner)
	}
	if global.Worker.AutoDiscovery || !reflectStringSlicesEqual(global.Worker.Triggers.Labels, []string{"global"}) || !global.Worker.Triggers.RequireAssigneeCurrentUser {
		t.Fatalf("global worker roles = %#v", global.Worker)
	}
	if global.Fixer.AutoDiscovery || global.Fixer.Triggers.AuthorFilter != FixerAuthorFilterCurrentUser || !reflectStringSlicesEqual(global.Fixer.Triggers.Labels, []string{"needs-fix"}) {
		t.Fatalf("global fixer roles = %#v", global.Fixer)
	}

	projectRoles := ProjectRoleConfigs(loaded.Config, "demo")
	if !projectRoles.Planner.AutoDiscovery || !reflectStringSlicesEqual(projectRoles.Planner.Triggers.Labels, []string{"project-plan"}) || projectRoles.Planner.Triggers.LabelMode != LabelModeAny || projectRoles.Planner.Triggers.RequireAssigneeCurrentUser {
		t.Fatalf("project planner roles = %#v", projectRoles.Planner)
	}
	if !projectRoles.Worker.AutoDiscovery || !reflectStringSlicesEqual(projectRoles.Worker.Triggers.Labels, []string{"project"}) || projectRoles.Worker.Triggers.LabelMode != LabelModeAny || projectRoles.Worker.Triggers.RequireAssigneeCurrentUser {
		t.Fatalf("project worker roles = %#v", projectRoles.Worker)
	}
	if !projectRoles.Reviewer.Discovery.Triggers.IncludeDrafts || !projectRoles.Reviewer.Discovery.Triggers.RequireReviewRequest || !reflectStringSlicesEqual(projectRoles.Reviewer.Discovery.Triggers.Labels, []string{"review"}) {
		t.Fatalf("project reviewer triggers = %#v", projectRoles.Reviewer)
	}
	if !projectRoles.Fixer.AutoDiscovery || projectRoles.Fixer.Triggers.AuthorFilter != FixerAuthorFilterAny || !reflectStringSlicesEqual(projectRoles.Fixer.Triggers.Labels, []string{"project-fix"}) || projectRoles.Fixer.Triggers.LabelMode != LabelModeAny {
		t.Fatalf("project fixer roles = %#v", projectRoles.Fixer)
	}
	if got := reviewerEnableSelfReviewValue(t, global.Reviewer.Discovery.Triggers); got {
		t.Fatalf("global reviewer enableSelfReview = %v, want false", got)
	}
	if got := reviewerEnableSelfReviewValue(t, projectRoles.Reviewer.Discovery.Triggers); !got {
		t.Fatalf("project reviewer enableSelfReview = %v, want true", got)
	}
	if !AnyProjectRoleAutoDiscoveryEnabled(loaded.Config, "worker") {
		t.Fatal("AnyProjectRoleAutoDiscoveryEnabled(worker) = false, want true from project override")
	}

	block := BuildCustomInstructionBlock(loaded.Config, "demo", "worker")
	if strings.Contains(block.Text, "Global worker guidance.") || !strings.Contains(block.Text, "Project worker guidance.") {
		t.Fatalf("custom instruction block did not use project role override: %s", block.Text)
	}
	plannerBlock := BuildCustomInstructionBlock(loaded.Config, "demo", "planner")
	if strings.Contains(plannerBlock.Text, "Global planner guidance.") || !strings.Contains(plannerBlock.Text, "Project planner guidance.") {
		t.Fatalf("planner custom instruction block did not use project role override: %s", plannerBlock.Text)
	}
	fixerBlock := BuildCustomInstructionBlock(loaded.Config, "demo", "fixer")
	if strings.Contains(fixerBlock.Text, "Global fixer guidance.") || !strings.Contains(fixerBlock.Text, "Project fixer guidance.") {
		t.Fatalf("fixer custom instruction block did not use project role override: %s", fixerBlock.Text)
	}
}

func TestSweeperRoleAutoDiscoveryAndProjectOverrideAffectRoleHelpers(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{
		"roles": {
			"sweeper": {
				"autoDiscovery": false,
				"triggers": {"maxPerTick": 10, "excludeLabels": ["keep-open"]},
				"lifecycle": {"pendingLabel": "looper:sweep-pending", "closedLabel": "looper:swept", "keepLabel": "looper:sweep-keep"},
				"security": {"quarantineLabel": "looper:sweeper-route-security"}
			}
		},
		"projects": [{
			"id": "demo",
			"name": "Demo",
			"repoPath": "/repos/demo",
			"roles": {
				"sweeper": {"autoDiscovery": true, "triggers": {"excludeLabels": ["project:keep-open"], "maxPerTick": 12}}
			}
		}]
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err != nil {
		skipIfSweeperConfigUnsupported(t, err)
	}

	if _, ok := roleConfigMapFromRoles(t, loaded.Config.Roles, "sweeper"); !ok {
		t.Skip("sweeper role support is not yet present in RoleConfigs")
	}

	globalRoles := ProjectRoleConfigs(loaded.Config, "missing")
	globalSweeper := roleMapForRole(t, globalRoles, "sweeper")
	globalAuto, ok := boolValueFromRoleMap(globalSweeper, "autoDiscovery")
	if !ok || globalAuto {
		t.Fatalf("global sweeper autoDiscovery = %v, want false", globalAuto)
	}

	globalExcludeLabels := labelsFromRoleMap(t, roleMapFromRoleSection(t, globalSweeper, "triggers"), "excludeLabels")
	if !reflectStringSlicesEqual(globalExcludeLabels, []string{"keep-open"}) {
		t.Fatalf("global sweeper excludeLabels = %#v, want %v", globalExcludeLabels, []string{"keep-open"})
	}

	projectRoles := ProjectRoleConfigs(loaded.Config, "demo")
	projectSweeper := roleMapForRole(t, projectRoles, "sweeper")
	projectAuto, ok := boolValueFromRoleMap(projectSweeper, "autoDiscovery")
	if !ok || !projectAuto {
		t.Fatalf("project sweeper autoDiscovery = %v, want true", projectAuto)
	}
	projectExcludeLabels := labelsFromRoleMap(t, roleMapFromRoleSection(t, projectSweeper, "triggers"), "excludeLabels")
	if !reflectStringSlicesEqual(projectExcludeLabels, []string{"project:keep-open"}) {
		t.Fatalf("project sweeper excludeLabels = %#v, want %v", projectExcludeLabels, []string{"project:keep-open"})
	}

	if !AnyProjectRoleAutoDiscoveryEnabled(loaded.Config, "sweeper") {
		t.Fatal("AnyProjectRoleAutoDiscoveryEnabled(sweeper) = false, want true from project override")
	}
}

func TestLoadFileSupportsSweeperCustomInstructions(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{
		"instructions": {"enabled": true},
		"roles": {"sweeper": {"instructions": "Global sweeper instruction."}},
		"projects": [{
			"id": "demo",
			"name": "Demo",
			"repoPath": "/repos/demo",
			"roles": {"sweeper": {"instructions": "Project sweeper role instruction."}},
			"instructions": {"sweeper": "Project sweeper map instruction."}
		}]
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err != nil {
		skipIfSweeperConfigUnsupported(t, err)
	}

	if _, ok := roleConfigMapFromRoles(t, loaded.Config.Roles, "sweeper"); !ok {
		t.Skip("sweeper role support is not yet present in RoleConfigs")
	}

	block := BuildCustomInstructionBlock(loaded.Config, "demo", "sweeper")
	if !strings.Contains(block.Text, "Project demo sweeper role instruction") {
		t.Fatalf("custom instruction block did not include project sweeper role override: %q", block.Text)
	}
	if strings.Contains(block.Text, "Project demo sweeper instructions") || strings.Contains(block.Text, "Project sweeper map instruction.") {
		t.Fatalf("custom instruction block kept deprecated project instruction map instead of canonical project role instructions: %q", block.Text)
	}
}

func TestCoordinatorRoleProjectOverrideAffectsRoleHelpers(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{
		"roles": {
			"coordinator": {
				"enabled": false,
				"pollInterval": "5m",
				"triage": {
					"triagedLabel": "triaged",
					"maxIssueAgeDays": 7,
					"maxPerTick": 5,
					"disposition": {
						"outOfScopeLabel": "wontfix",
						"unclearLabel": "needs-info",
						"reTriageOnAuthorReply": true
					}
				},
				"dispatch": {
					"mode": "human-gated",
					"humanGate": {"slashCommands": ["/plan"], "allowedUsers": []},
					"autonomous": {"delayMinutes": 30, "holdLabel": "looper:hold"},
					"assignTo": ""
				}
			}
		},
		"projects": [{
			"id": "demo",
			"name": "Demo",
			"repoPath": "/repos/demo",
			"roles": {
				"coordinator": {
					"enabled": true,
					"pollInterval": "2m",
					"triage": {"maxPerTick": 3},
					"dispatch": {"autonomous": {"holdLabel": "project:hold"}}
				}
			}
		}]
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	globalRoles := ProjectRoleConfigs(loaded.Config, "missing")
	if globalRoles.Coordinator.Enabled {
		t.Fatal("global coordinator enabled = true, want false")
	}
	if globalRoles.Coordinator.PollInterval != "5m" || globalRoles.Coordinator.Triage.MaxPerTick != 5 {
		t.Fatalf("global coordinator = %#v, want configured global defaults", globalRoles.Coordinator)
	}

	projectRoles := ProjectRoleConfigs(loaded.Config, "demo")
	if !projectRoles.Coordinator.Enabled {
		t.Fatal("project coordinator enabled = false, want true from project override")
	}
	if projectRoles.Coordinator.PollInterval != "2m" || projectRoles.Coordinator.Triage.MaxPerTick != 3 || projectRoles.Coordinator.Dispatch.Autonomous.HoldLabel != "project:hold" {
		t.Fatalf("project coordinator = %#v, want project overrides merged", projectRoles.Coordinator)
	}
	if !AnyProjectRoleAutoDiscoveryEnabled(loaded.Config, "coordinator") {
		t.Fatal("AnyProjectRoleAutoDiscoveryEnabled(coordinator) = false, want true from project enablement")
	}
}

func TestValidateRejectsInvalidCoordinatorConfig(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{
		"roles": {
			"coordinator": {
				"enabled": true,
				"pollInterval": "",
				"triage": {
					"triagedLabel": "",
					"maxIssueAgeDays": 0,
					"maxPerTick": 0,
					"disposition": {
						"outOfScopeLabel": "",
						"unclearLabel": " needs-info "
					}
				},
				"dispatch": {
					"mode": "robot",
					"humanGate": {"slashCommands": [], "allowedUsers": [""]},
					"autonomous": {"delayMinutes": 0, "holdLabel": ""},
					"assignTo": " octo "
				}
			}
		},
		"projects": [{"id": "demo", "name": "Demo", "repoPath": "/repos/demo"}]
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want validation error")
	}
	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("LoadFile() error = %v, want *ConfigValidationError", err)
	}
	assertValidationIssueForPaths(t, validationErr.Issues, []string{
		"roles.coordinator.pollInterval",
		"roles.coordinator.triage.triagedLabel",
		"roles.coordinator.triage.maxIssueAgeDays",
		"roles.coordinator.triage.maxPerTick",
		"roles.coordinator.triage.disposition.outOfScopeLabel",
		"roles.coordinator.triage.disposition.unclearLabel",
		"roles.coordinator.dispatch.mode",
		"roles.coordinator.dispatch.humanGate.slashCommands",
		"roles.coordinator.dispatch.autonomous.delayMinutes",
		"roles.coordinator.dispatch.autonomous.holdLabel",
		"roles.coordinator.dispatch.assignTo",
	})
	assertValidationIssueForPathPrefix(t, validationErr.Issues, "roles.coordinator.dispatch.humanGate.allowedUsers")
}

func TestValidateRejectsInvalidSweeperTriggerThresholdAndAssociationConfig(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{
		"roles": {
			"sweeper": {
				"autoDiscovery": true,
				"triggers": {
					"includeIssues": false,
					"includePullRequests": false,
					"maxPerTick": 0,
					"reopenCooldownDays": 0,
					"excludeAuthorAssociations": ["BOGUS"],
					"excludeLabels": ["", ""],
					"looperInternalLabels": ["looper:spec-reviewing", "looper:sweep-pending"]
				},
				"lifecycle": {
					"pendingLabel": "looper:sweep-pending",
					"closedLabel": "looper:sweep-pending",
					"keepLabel": "looper:sweep-keep"
				},
				"security": {
					"quarantineLabel": "looper:sweep-keep"
				},
				"limits": {
					"maxWarningsPerRepoPerDay": -1,
					"maxClosesPerRepoPerDay": -1,
					"globalKillSwitch": false
				},
				"categories": {
					"stale": {"enabled": true, "inactivityDays": 0, "gracePeriodDays": 0, "minConfidence": 110}
				}
			}
		},
		"projects": [{"id": "demo", "name": "Demo", "repoPath": "/repos/demo"}]
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want validation error")
	}
	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("LoadFile() error = %v, want *ConfigValidationError", err)
	}

	assertValidationIssueForPaths(t, validationErr.Issues, []string{
		"roles.sweeper.triggers.maxPerTick",
		"roles.sweeper.triggers.reopenCooldownDays",
		"roles.sweeper.triggers.includeIssues",
		"roles.sweeper.limits.maxWarningsPerRepoPerDay",
		"roles.sweeper.limits.maxClosesPerRepoPerDay",
		"roles.sweeper.categories.stale.inactivityDays",
		"roles.sweeper.categories.stale.gracePeriodDays",
		"roles.sweeper.categories.stale.minConfidence",
		"roles.sweeper.lifecycle.closedLabel",
		"roles.sweeper.security.quarantineLabel",
	})
	assertValidationIssueForPathPrefix(t, validationErr.Issues, "roles.sweeper.triggers.excludeLabels")
	assertValidationIssueForPathPrefix(t, validationErr.Issues, "roles.sweeper.triggers.excludeAuthorAssociations")
}

func TestLoadFileSupportsReviewerEnableSelfReviewOverride(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{
		"roles": {"reviewer": {"triggers": {"enableSelfReview": true}}},
		"projects": [{
			"id": "demo",
			"name": "Demo",
			"repoPath": "/repos/demo",
			"roles": {"reviewer": {"triggers": {"enableSelfReview": false}}}
		}]
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	if got := reviewerEnableSelfReviewValue(t, loaded.Config.Roles.Reviewer.Discovery.Triggers); !got {
		t.Fatalf("global reviewer enableSelfReview = %v, want true", got)
	}
	if got := reviewerEnableSelfReviewValue(t, ProjectRoleConfigs(loaded.Config, "demo").Reviewer.Discovery.Triggers); got {
		t.Fatalf("project reviewer enableSelfReview = %v, want false", got)
	}
}

func TestEnvOverrideReviewerEnableSelfReviewBeatsProjectConfig(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{
		"roles": {"reviewer": {"triggers": {"enableSelfReview": false}}},
		"projects": [{
			"id": "demo",
			"name": "Demo",
			"repoPath": "/repos/demo",
			"roles": {"reviewer": {"triggers": {"enableSelfReview": false}}}
		}]
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: configPath,
		LookupEnv: mapEnvLookup(map[string]string{
			"LOOPER_ROLES_REVIEWER_TRIGGERS_ENABLE_SELF_REVIEW": "true",
		}),
		LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	if got := reviewerEnableSelfReviewValue(t, loaded.Config.Roles.Reviewer.Discovery.Triggers); !got {
		t.Fatalf("global reviewer enableSelfReview = %v, want true", got)
	}
	if got := reviewerEnableSelfReviewValue(t, ProjectRoleConfigs(loaded.Config, "demo").Reviewer.Discovery.Triggers); !got {
		t.Fatalf("project reviewer enableSelfReview = %v, want true", got)
	}
}

func TestProjectRoleInstructionsCanClearGlobalInstructions(t *testing.T) {
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Roles:    &PartialRoleConfigs{Worker: &PartialWorkerRoleConfig{Instructions: stringPtr("Global worker guidance.")}},
		Projects: &[]PartialProjectRefConfig{{ID: "demo", Name: "Demo", RepoPath: "/repos/demo", Roles: &PartialRoleConfigs{Worker: &PartialWorkerRoleConfig{Instructions: stringPtr("")}}}},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	block := BuildCustomInstructionBlock(cfg, "demo", "worker")
	if block.Text != "" {
		t.Fatalf("custom instruction block = %q, want empty because project role cleared global instructions", block.Text)
	}
}

func TestLegacyProjectInstructionsNormalizeToCanonicalProjectRoleInstructions(t *testing.T) {
	loaded := loadConfigFixture(t, "config.json", `{
		"instructions": {"enabled": true, "maxBytes": 128},
		"projects": [{
			"id": "demo",
			"name": "Demo",
			"repoPath": "/repos/demo",
			"instructions": {"worker": "Legacy project worker guidance."}
		}]
	}`, nil, nil)

	block := BuildCustomInstructionBlock(loaded.Config, "demo", "worker")
	if !strings.Contains(block.Text, "Legacy project worker guidance.") {
		t.Fatalf("custom instruction block = %q, want normalized legacy project instruction", block.Text)
	}
	assertWarningsEqual(t, loaded.Warnings, []string{`deprecated config path "projects[].instructions" is accepted for now; use "projects[].roles.<role>.instructions" instead`})
}

func TestCanonicalProjectRoleInstructionsBeatLegacyProjectInstructionMap(t *testing.T) {
	loaded := loadConfigFixture(t, "config.json", `{
		"instructions": {"enabled": true, "maxBytes": 128},
		"projects": [{
			"id": "demo",
			"name": "Demo",
			"repoPath": "/repos/demo",
			"instructions": {"worker": "Legacy worker guidance."},
			"roles": {"worker": {"instructions": "Canonical worker guidance."}}
		}]
	}`, nil, nil)

	block := BuildCustomInstructionBlock(loaded.Config, "demo", "worker")
	if !strings.Contains(block.Text, "Canonical worker guidance.") || strings.Contains(block.Text, "Legacy worker guidance.") {
		t.Fatalf("custom instruction block = %q, want canonical project role instructions to win", block.Text)
	}
	assertWarningsEqual(t, loaded.Warnings, []string{`deprecated config path "projects[].instructions" is accepted for now; use "projects[].roles.<role>.instructions" instead`})
}

func TestLoadFileRejectsUnknownLegacyProjectInstructionRole(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{"projects":[{"id":"demo","name":"Demo","repoPath":"/repos/demo","instructions":{"reveiwer":"legacy"}}]}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want config validation error")
	}

	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("LoadFile() error = %T, want *ConfigValidationError", err)
	}

	assertValidationIssue(t, validationErr, "projects[0].instructions.reveiwer", "role must be one of: planner, worker, reviewer, fixer, sweeper")
}

func TestValidateProjectReviewerLabelCanBeClearedWhenDisabled(t *testing.T) {
	falseValue := false
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Projects: &[]PartialProjectRefConfig{{ID: "demo", Name: "Demo", RepoPath: "/repos/demo", Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{SpecReview: &PartialReviewerSpecReviewConfig{IncludeReviewingLabel: &falseValue, ReviewingLabel: stringPtr("")}}}}},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if err := ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()}); err != nil {
		t.Fatalf("ValidateWithOptions() error = %v, want nil", err)
	}
}

func TestValidateRejectsOversizedAndProtectedInstructions(t *testing.T) {
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Instructions: &PartialInstructionsConfig{MaxBytes: intPtr(8)},
		Roles: &PartialRoleConfigs{
			Worker: &PartialWorkerRoleConfig{Instructions: stringPtr("this is too long")},
			Fixer:  &PartialFixerRoleConfig{Instructions: stringPtr("Change the __LOOPER_RESULT__ completion marker")},
		},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	if err == nil {
		t.Fatal("ValidateWithOptions() error = nil, want validation error")
	}
	message := err.Error()
	if !strings.Contains(message, "config validation failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNoCustomInstructionsCLIOverrideDisablesInstructions(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{"roles": {"worker": {"instructions": "Prefer minimal changes."}}}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, Args: []string{"--no-custom-instructions"}, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if loaded.Config.Instructions.Enabled {
		t.Fatal("instructions enabled = true, want false")
	}
	if block := BuildCustomInstructionBlock(loaded.Config, "", "worker"); block.Text != "" {
		t.Fatalf("disabled custom instruction block = %q", block.Text)
	}
}

func TestNoCustomInstructionsCLIOverrideAcceptsExplicitFalse(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{"instructions": {"enabled": false}, "roles": {"worker": {"instructions": "Prefer minimal changes."}}}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, Args: []string{"--no-custom-instructions", "false"}, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if !loaded.Config.Instructions.Enabled {
		t.Fatal("instructions enabled = false, want true")
	}
	if block := BuildCustomInstructionBlock(loaded.Config, "", "worker"); !strings.Contains(block.Text, "Prefer minimal changes.") {
		t.Fatalf("enabled custom instruction block = %q", block.Text)
	}
}

func TestLoadFileUsesDefaultConfigPathWhenUnset(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("LOOPER_CONFIG", "")
	looperHome := filepath.Join(homeDir, ".looper")
	if err := os.MkdirAll(looperHome, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD:      t.TempDir(),
		LookPath: fakeLookPath(map[string]string{"git": "/detected/git", "gh": "/detected/gh", "osascript": "/detected/osascript"}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	defaultConfigPath, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath() error = %v", err)
	}

	if loaded.Metadata.ConfigPath != defaultConfigPath {
		t.Fatalf("LoadFile().Metadata.ConfigPath = %q, want %q", loaded.Metadata.ConfigPath, defaultConfigPath)
	}
}

func TestLoadFileLoadsSupportedConfigFormats(t *testing.T) {
	cwd := t.TempDir()
	lookPath := fakeLookPath(map[string]string{"git": "/detected/git", "gh": "/detected/gh", "osascript": "/detected/osascript"})

	tests := []struct {
		name     string
		suffix   string
		contents string
		port     int
	}{
		{name: "json", suffix: ".json", contents: `{"server":{"port":6101}}`, port: 6101},
		{name: "yaml", suffix: ".yaml", contents: "server:\n  port: 6102\n", port: 6102},
		{name: "yml", suffix: ".yml", contents: "server:\n  port: 6103\n", port: 6103},
		{name: "toml", suffix: ".toml", contents: "[server]\nport = 6104\n", port: 6104},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(cwd, "config"+tt.suffix)
			if err := os.WriteFile(configPath, []byte(tt.contents), 0o644); err != nil {
				t.Fatalf("os.WriteFile() error = %v", err)
			}

			loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup, LookPath: lookPath})
			if err != nil {
				t.Fatalf("LoadFile() error = %v", err)
			}

			if loaded.Config.Server.Port != tt.port {
				t.Fatalf("LoadFile().Config.Server.Port = %d, want %d", loaded.Config.Server.Port, tt.port)
			}
			if !loaded.Metadata.ConfigFilePresent {
				t.Fatal("LoadFile().Metadata.ConfigFilePresent = false, want true")
			}
		})
	}
}

func TestLoadFileNullEmptyAndOmittedShapesProduceEquivalentDefaultsAcrossFormats(t *testing.T) {
	lookPath := fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})
	cwd := t.TempDir()
	tests := []struct {
		name     string
		fileName string
		contents string
	}{
		{name: "json null", fileName: "config.json", contents: `null`},
		{name: "json empty object", fileName: "config.json", contents: `{}`},
		{name: "yaml null", fileName: "config.yaml", contents: "null\n"},
		{name: "yaml empty object", fileName: "config.yaml", contents: "{}\n"},
		{name: "yaml omitted", fileName: "config.yaml", contents: ""},
		{name: "toml omitted", fileName: "config.toml", contents: ""},
	}

	var baselineConfig any
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(cwd, tt.fileName)
			if err := os.WriteFile(configPath, []byte(tt.contents), 0o644); err != nil {
				t.Fatalf("os.WriteFile() error = %v", err)
			}

			loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup, LookPath: lookPath})
			if err != nil {
				t.Fatalf("LoadFile() error = %v", err)
			}
			if !loaded.Metadata.ConfigFilePresent {
				t.Fatal("LoadFile().Metadata.ConfigFilePresent = false, want true")
			}
			if loaded.Partial != (PartialConfig{}) {
				t.Fatalf("LoadFile().Partial = %#v, want empty partial config", loaded.Partial)
			}

			current := toJSONValue(t, loaded.Config)
			if baselineConfig == nil {
				baselineConfig = current
				return
			}
			if !reflect.DeepEqual(current, baselineConfig) {
				t.Fatalf("effective config mismatch\ncurrent: %#v\nbaseline: %#v", current, baselineConfig)
			}
		})
	}
}

func TestLoadFileAutoDetectsMissingToolPathsAfterApplyingOverrides(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")

	contents := `{
		"tools": {"gitPath": "/file/git"}
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: configPath,
		LookupEnv: mapEnvLookup(map[string]string{
			"LOOPER_GH_PATH": "/env/gh",
		}),
		LookPath: fakeLookPath(map[string]string{"osascript": "/detected/osascript"}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	if loaded.Config.Tools.GitPath == nil || *loaded.Config.Tools.GitPath != "/file/git" {
		t.Fatalf("LoadFile().Config.Tools.GitPath = %v, want %q", loaded.Config.Tools.GitPath, "/file/git")
	}

	if loaded.Config.Tools.GHPath == nil || *loaded.Config.Tools.GHPath != "/env/gh" {
		t.Fatalf("LoadFile().Config.Tools.GHPath = %v, want %q", loaded.Config.Tools.GHPath, "/env/gh")
	}

	if loaded.Config.Tools.OsascriptPath == nil || *loaded.Config.Tools.OsascriptPath != "/detected/osascript" {
		t.Fatalf("LoadFile().Config.Tools.OsascriptPath = %v, want %q", loaded.Config.Tools.OsascriptPath, "/detected/osascript")
	}

	if got := loaded.Metadata.ToolDetection["gitPath"]; got != ToolDetectionStatusConfigured {
		t.Fatalf("LoadFile().Metadata.ToolDetection[gitPath] = %q, want %q", got, ToolDetectionStatusConfigured)
	}

	if got := loaded.Metadata.ToolDetection["ghPath"]; got != ToolDetectionStatusConfigured {
		t.Fatalf("LoadFile().Metadata.ToolDetection[ghPath] = %q, want %q", got, ToolDetectionStatusConfigured)
	}

	if got := loaded.Metadata.ToolDetection["osascriptPath"]; got != ToolDetectionStatusDetected {
		t.Fatalf("LoadFile().Metadata.ToolDetection[osascriptPath] = %q, want %q", got, ToolDetectionStatusDetected)
	}
}

func TestDetectToolPathsLeavesMissingEntriesUnset(t *testing.T) {
	configuredGitPath := "/configured/git"

	result := DetectToolPaths(ToolPathsConfig{
		GitPath: &configuredGitPath,
	}, fakeLookPath(map[string]string{}))

	if result.Paths.GitPath == nil || *result.Paths.GitPath != configuredGitPath {
		t.Fatalf("DetectToolPaths().Paths.GitPath = %v, want %q", result.Paths.GitPath, configuredGitPath)
	}

	if result.Paths.GHPath != nil {
		t.Fatalf("DetectToolPaths().Paths.GHPath = %v, want nil", result.Paths.GHPath)
	}

	if got := result.Detection["gitPath"]; got != ToolDetectionStatusConfigured {
		t.Fatalf("DetectToolPaths().Detection[gitPath] = %q, want %q", got, ToolDetectionStatusConfigured)
	}

	if got := result.Detection["ghPath"]; got != ToolDetectionStatusMissing {
		t.Fatalf("DetectToolPaths().Detection[ghPath] = %q, want %q", got, ToolDetectionStatusMissing)
	}

	if got := result.Detection["osascriptPath"]; got != ToolDetectionStatusMissing {
		t.Fatalf("DetectToolPaths().Detection[osascriptPath] = %q, want %q", got, ToolDetectionStatusMissing)
	}
}

func TestLoadFileAppliesFileEnvAndCLIOverridesInPriorityOrder(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	logDir := filepath.Join(cwd, "logs")
	dbPath := filepath.Join(cwd, "looper.sqlite")

	contents := fmt.Sprintf(`{
		"server": {"host": "0.0.0.0", "port": 5555},
		"daemon": {"logDir": %q, "workingDirectory": %q},
		"storage": {"dbPath": %q},
		"notifications": {"osascript": {"enabled": true, "throttleWindowSeconds": 60}},
		"tools": {"gitPath": "/file/git", "ghPath": "/file/gh"},
		"defaults": {"allowAutoCommit": false}
	}`, logDir, cwd, dbPath)
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD: cwd,
		Args: []string{
			"--config", configPath,
			"--port", "7000",
			"--git-path", "/cli/git",
			"--allow-auto-commit", "false",
			"--allow-auto-push", "false",
		},
		LookupEnv: mapEnvLookup(map[string]string{
			"LOOPER_HOST":                 "127.0.0.2",
			"LOOPER_DB_PATH":              filepath.Join(cwd, "env.sqlite"),
			"LOOPER_LOG_DIR":              filepath.Join(cwd, "env-logs"),
			"LOOPER_OSASCRIPT_ENABLED":    "false",
			"LOOPER_IN_APP_NOTIFICATIONS": "false",
			"LOOPER_GH_PATH":              "/env/gh",
			"LOOPER_ALLOW_AUTO_COMMIT":    "true",
			"LOOPER_ALLOW_AUTO_APPROVE":   "true",
			"LOOPER_WORKING_DIRECTORY":    filepath.Join(cwd, "env-workspace"),
			"LOOPER_OSASCRIPT_PATH":       "/env/osascript",
		}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	if loaded.Config.Server.Host != "127.0.0.2" {
		t.Fatalf("LoadFile().Config.Server.Host = %q, want %q", loaded.Config.Server.Host, "127.0.0.2")
	}

	if loaded.Config.Server.Port != 7000 {
		t.Fatalf("LoadFile().Config.Server.Port = %d, want %d", loaded.Config.Server.Port, 7000)
	}

	if loaded.Config.Storage.DBPath != filepath.Join(cwd, "env.sqlite") {
		t.Fatalf("LoadFile().Config.Storage.DBPath = %q, want %q", loaded.Config.Storage.DBPath, filepath.Join(cwd, "env.sqlite"))
	}

	if loaded.Config.Daemon.LogDir != filepath.Join(cwd, "env-logs") {
		t.Fatalf("LoadFile().Config.Daemon.LogDir = %q, want %q", loaded.Config.Daemon.LogDir, filepath.Join(cwd, "env-logs"))
	}

	if loaded.Config.Daemon.WorkingDirectory != filepath.Join(cwd, "env-workspace") {
		t.Fatalf("LoadFile().Config.Daemon.WorkingDirectory = %q, want %q", loaded.Config.Daemon.WorkingDirectory, filepath.Join(cwd, "env-workspace"))
	}

	if loaded.Config.Tools.GitPath == nil || *loaded.Config.Tools.GitPath != "/cli/git" {
		t.Fatalf("LoadFile().Config.Tools.GitPath = %v, want %q", loaded.Config.Tools.GitPath, "/cli/git")
	}

	if loaded.Config.Tools.GHPath == nil || *loaded.Config.Tools.GHPath != "/env/gh" {
		t.Fatalf("LoadFile().Config.Tools.GHPath = %v, want %q", loaded.Config.Tools.GHPath, "/env/gh")
	}

	if loaded.Config.Tools.OsascriptPath == nil || *loaded.Config.Tools.OsascriptPath != "/env/osascript" {
		t.Fatalf("LoadFile().Config.Tools.OsascriptPath = %v, want %q", loaded.Config.Tools.OsascriptPath, "/env/osascript")
	}

	if loaded.Config.Defaults.AllowAutoCommit {
		t.Fatal("LoadFile().Config.Defaults.AllowAutoCommit = true, want false")
	}

	if loaded.Config.Defaults.AllowAutoPush {
		t.Fatal("LoadFile().Config.Defaults.AllowAutoPush = true, want false")
	}

	if !loaded.Config.Defaults.AllowAutoApprove {
		t.Fatal("LoadFile().Config.Defaults.AllowAutoApprove = false, want true")
	}

	if loaded.Config.Notifications.InApp {
		t.Fatal("LoadFile().Config.Notifications.InApp = true, want false")
	}

	if loaded.Config.Notifications.Osascript.Enabled {
		t.Fatal("LoadFile().Config.Notifications.Osascript.Enabled = true, want false")
	}

	if !loaded.Metadata.ConfigFilePresent {
		t.Fatal("LoadFile().Metadata.ConfigFilePresent = false, want true")
	}
}

func TestLoadFileConfigPathSelectionPrefersCLIThenEnvThenOptions(t *testing.T) {
	cwd := t.TempDir()
	cliConfigPath := filepath.Join(cwd, "cli.json")
	envConfigPath := filepath.Join(cwd, "env.json")
	optionConfigPath := filepath.Join(cwd, "option.json")

	for path, port := range map[string]int{cliConfigPath: 6100, envConfigPath: 6200, optionConfigPath: 6300} {
		contents := fmt.Sprintf(`{"server": {"port": %d}}`, port)
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%q) error = %v", path, err)
		}
	}

	t.Run("cli beats env and options", func(t *testing.T) {
		loaded, err := LoadFile(LoadFileOptions{
			CWD:        cwd,
			ConfigPath: optionConfigPath,
			Args:       []string{"--config", cliConfigPath},
			LookupEnv:  mapEnvLookup(map[string]string{"LOOPER_CONFIG": envConfigPath}),
		})
		if err != nil {
			t.Fatalf("LoadFile() error = %v", err)
		}

		if loaded.Metadata.ConfigPath != cliConfigPath {
			t.Fatalf("LoadFile().Metadata.ConfigPath = %q, want %q", loaded.Metadata.ConfigPath, cliConfigPath)
		}

		if loaded.Config.Server.Port != 6100 {
			t.Fatalf("LoadFile().Config.Server.Port = %d, want %d", loaded.Config.Server.Port, 6100)
		}
	})

	t.Run("env beats options when cli absent", func(t *testing.T) {
		loaded, err := LoadFile(LoadFileOptions{
			CWD:        cwd,
			ConfigPath: optionConfigPath,
			LookupEnv:  mapEnvLookup(map[string]string{"LOOPER_CONFIG": envConfigPath}),
		})
		if err != nil {
			t.Fatalf("LoadFile() error = %v", err)
		}

		if loaded.Metadata.ConfigPath != envConfigPath {
			t.Fatalf("LoadFile().Metadata.ConfigPath = %q, want %q", loaded.Metadata.ConfigPath, envConfigPath)
		}

		if loaded.Config.Server.Port != 6200 {
			t.Fatalf("LoadFile().Config.Server.Port = %d, want %d", loaded.Config.Server.Port, 6200)
		}
	})
}

func TestLoadFileConfigPathSelectionPrefersCLIThenEnvThenDiscoveredDefault(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	looperHome := filepath.Join(homeDir, ".looper")
	if err := os.MkdirAll(looperHome, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}

	cwd := t.TempDir()
	cliConfigPath := filepath.Join(cwd, "cli.yaml")
	envConfigPath := filepath.Join(cwd, "env.json")
	defaultConfigPath := filepath.Join(looperHome, "config.toml")

	for path, contents := range map[string]string{
		cliConfigPath:     "server:\n  port: 7100\n",
		envConfigPath:     `{"server":{"port":7200}}`,
		defaultConfigPath: "[server]\nport = 7300\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%q) error = %v", path, err)
		}
	}

	t.Run("cli beats env and discovered default", func(t *testing.T) {
		loaded, err := LoadFile(LoadFileOptions{
			CWD:       cwd,
			Args:      []string{"--config", cliConfigPath},
			LookupEnv: mapEnvLookup(map[string]string{"LOOPER_CONFIG": envConfigPath}),
		})
		if err != nil {
			t.Fatalf("LoadFile() error = %v", err)
		}

		if loaded.Metadata.ConfigPath != cliConfigPath {
			t.Fatalf("LoadFile().Metadata.ConfigPath = %q, want %q", loaded.Metadata.ConfigPath, cliConfigPath)
		}
		if loaded.Config.Server.Port != 7100 {
			t.Fatalf("LoadFile().Config.Server.Port = %d, want %d", loaded.Config.Server.Port, 7100)
		}
	})

	t.Run("env beats discovered default when cli absent", func(t *testing.T) {
		loaded, err := LoadFile(LoadFileOptions{
			CWD:       cwd,
			LookupEnv: mapEnvLookup(map[string]string{"LOOPER_CONFIG": envConfigPath}),
		})
		if err != nil {
			t.Fatalf("LoadFile() error = %v", err)
		}

		if loaded.Metadata.ConfigPath != envConfigPath {
			t.Fatalf("LoadFile().Metadata.ConfigPath = %q, want %q", loaded.Metadata.ConfigPath, envConfigPath)
		}
		if loaded.Config.Server.Port != 7200 {
			t.Fatalf("LoadFile().Config.Server.Port = %d, want %d", loaded.Config.Server.Port, 7200)
		}
	})

	t.Run("discovered default used when cli and env absent", func(t *testing.T) {
		loaded, err := LoadFile(LoadFileOptions{CWD: cwd, LookupEnv: emptyEnvLookup})
		if err != nil {
			t.Fatalf("LoadFile() error = %v", err)
		}

		if loaded.Metadata.ConfigPath != defaultConfigPath {
			t.Fatalf("LoadFile().Metadata.ConfigPath = %q, want %q", loaded.Metadata.ConfigPath, defaultConfigPath)
		}
		if loaded.Config.Server.Port != 7300 {
			t.Fatalf("LoadFile().Config.Server.Port = %d, want %d", loaded.Config.Server.Port, 7300)
		}
	})
}

func TestLoadFileRejectsMultipleDefaultConfigFiles(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	looperHome := filepath.Join(homeDir, ".looper")
	if err := os.MkdirAll(looperHome, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}

	for _, name := range []string{"config.toml", "config.yaml"} {
		if err := os.WriteFile(filepath.Join(looperHome, name), []byte("{}"), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%q) error = %v", name, err)
		}
	}

	_, err := LoadFile(LoadFileOptions{CWD: t.TempDir(), LookupEnv: emptyEnvLookup})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want error")
	}

	if got := err.Error(); !strings.Contains(got, "multiple default config files found") || !strings.Contains(got, "config.toml") || !strings.Contains(got, "config.yaml") {
		t.Fatalf("LoadFile() error = %q, want multiple-default-files error", got)
	}
}

func TestLoadFilePrefersCanonicalTOMLWhenLegacyJSONAlsoExists(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	looperHome := filepath.Join(homeDir, ".looper")
	if err := os.MkdirAll(looperHome, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	tomlPath := filepath.Join(looperHome, "config.toml")
	jsonPath := filepath.Join(looperHome, "config.json")
	if err := os.WriteFile(tomlPath, []byte("[server]\nport = 7410\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(config.toml) error = %v", err)
	}
	if err := os.WriteFile(jsonPath, []byte(`{"server":{"port":7420}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile(config.json) error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{CWD: t.TempDir(), LookupEnv: emptyEnvLookup})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if loaded.Metadata.ConfigPath != tomlPath {
		t.Fatalf("LoadFile().Metadata.ConfigPath = %q, want %q", loaded.Metadata.ConfigPath, tomlPath)
	}
	if loaded.Config.Server.Port != 7410 {
		t.Fatalf("LoadFile().Config.Server.Port = %d, want 7410", loaded.Config.Server.Port)
	}
}

func TestLoadFileLegacyDefaultConfigJSONEmitsMigrationNote(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	looperHome := filepath.Join(homeDir, ".looper")
	if err := os.MkdirAll(looperHome, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	legacyDefaultPath := filepath.Join(looperHome, "config.json")
	canonicalDefaultPath := filepath.Join(looperHome, "config.toml")
	if err := os.WriteFile(legacyDefaultPath, []byte(`{"server":{"port":7400}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	wantNotices := []string{legacyDefaultConfigMigrationNote(legacyDefaultPath, canonicalDefaultPath)}

	t.Run("discovered default path", func(t *testing.T) {
		loaded, err := LoadFile(LoadFileOptions{CWD: t.TempDir(), LookupEnv: emptyEnvLookup})
		if err != nil {
			t.Fatalf("LoadFile() error = %v", err)
		}
		assertNoticesEqual(t, loaded.Notices, wantNotices)
	})

	t.Run("explicit env path", func(t *testing.T) {
		loaded, err := LoadFile(LoadFileOptions{CWD: t.TempDir(), LookupEnv: mapEnvLookup(map[string]string{"LOOPER_CONFIG": legacyDefaultPath})})
		if err != nil {
			t.Fatalf("LoadFile() error = %v", err)
		}
		assertNoticesEqual(t, loaded.Notices, wantNotices)
	})

	t.Run("explicit cli path", func(t *testing.T) {
		loaded, err := LoadFile(LoadFileOptions{CWD: t.TempDir(), Args: []string{"--config", legacyDefaultPath}, LookupEnv: emptyEnvLookup})
		if err != nil {
			t.Fatalf("LoadFile() error = %v", err)
		}
		assertNoticesEqual(t, loaded.Notices, wantNotices)
	})
}

func TestLoadFileIgnoresConfigLoadNoticeHomeResolutionErrors(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server":{"port":7400}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	original := defaultLooperHomeForNotices
	defaultLooperHomeForNotices = func() (string, error) {
		return "", fmt.Errorf("home unavailable")
	}
	defer func() {
		defaultLooperHomeForNotices = original
	}()

	loaded, err := LoadFile(LoadFileOptions{CWD: t.TempDir(), ConfigPath: configPath, LookupEnv: emptyEnvLookup})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if got, want := loaded.Config.Server.Port, 7400; got != want {
		t.Fatalf("LoadFile().Config.Server.Port = %d, want %d", got, want)
	}
	assertNoticesEqual(t, loaded.Notices, nil)
}

func TestLoadFileDoesNotEmitMigrationNoteForNonLegacyDefaultJSON(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	looperHome := filepath.Join(homeDir, ".looper")
	if err := os.MkdirAll(looperHome, 0o755); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	otherJSONPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(otherJSONPath, []byte(`{"server":{"port":7500}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{CWD: t.TempDir(), ConfigPath: otherJSONPath, LookupEnv: emptyEnvLookup})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	assertNoticesEqual(t, loaded.Notices, nil)
}

func TestLoadFileReviewerLoopPrecedenceDefaultsFileEnvCLI(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"reviewer":{"loop":{"enabledByDefault":false,"quietPeriodSeconds":30,"minPublishIntervalSeconds":900,"maxIterationsPerPR":7,"maxIterationsPerHead":2}}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD: cwd,
		Args: []string{
			"--config", configPath,
			"--reviewer-loop-enabled", "true",
			"--reviewer-max-iterations-per-pr", "11",
		},
		LookupEnv: mapEnvLookup(map[string]string{
			"LOOPER_REVIEWER_QUIET_PERIOD_SECONDS":         "45",
			"LOOPER_REVIEWER_MIN_PUBLISH_INTERVAL_SECONDS": "1200",
			"LOOPER_REVIEWER_MAX_ITERATIONS_PER_HEAD":      "3",
		}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	loop := loaded.Config.Roles.Reviewer.Behavior.Loop
	if !loop.EnabledByDefault || loop.QuietPeriodSeconds != 45 || loop.MinPublishIntervalSeconds != 1200 || loop.MaxIterationsPerPR != 11 || loop.MaxIterationsPerHead != 3 {
		t.Fatalf("reviewer loop config = %#v, want cli/env/file precedence applied", loop)
	}
}

func TestLoadFileReviewerReviewEventsPrecedenceDefaultsFileEnvCLI(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"reviewer":{"reviewEvents":{"clean":"COMMENT","blocking":"COMMENT"}}}`), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD: cwd,
		Args: []string{
			"--config", configPath,
			"--reviewer-blocking-review-event", "REQUEST_CHANGES",
		},
		LookupEnv: mapEnvLookup(map[string]string{
			"LOOPER_REVIEWER_REVIEW_EVENTS_CLEAN": "APPROVE",
		}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if got := loaded.Config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventApprove {
		t.Fatalf("clean review event = %q, want %q", got, ReviewerReviewEventApprove)
	}
	if got := loaded.Config.Roles.Reviewer.Behavior.ReviewEvents.Blocking; got != ReviewerReviewEventRequestChanges {
		t.Fatalf("blocking review event = %q, want %q", got, ReviewerReviewEventRequestChanges)
	}
}

func TestLoadFileReviewerNativeResumeOnHeadChangeEnvOverride(t *testing.T) {
	cwd := t.TempDir()
	loaded, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: filepath.Join(cwd, "missing.json"),
		LookupEnv:  mapEnvLookup(map[string]string{"LOOPER_REVIEWER_NATIVE_RESUME_ON_HEAD_CHANGE": "true"}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if !loaded.Config.Roles.Reviewer.Behavior.NativeResume.OnHeadChange {
		t.Fatalf("reviewer.nativeResume.onHeadChange = false, want true")
	}
	if loaded.Config.Roles.Reviewer.Behavior.NativeResume.ReReviewPromptOnHeadChange {
		t.Fatalf("reviewer.nativeResume.reReviewPromptOnHeadChange = true, want false")
	}
}

func TestLoadFileReviewerNativeResumeReReviewPromptOnHeadChangeEnvOverride(t *testing.T) {
	cwd := t.TempDir()
	loaded, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: filepath.Join(cwd, "missing.json"),
		LookupEnv:  mapEnvLookup(map[string]string{"LOOPER_REVIEWER_NATIVE_RESUME_REREVIEW_PROMPT_ON_HEAD_CHANGE": "true"}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if !loaded.Config.Roles.Reviewer.Behavior.NativeResume.ReReviewPromptOnHeadChange {
		t.Fatalf("reviewer.nativeResume.reReviewPromptOnHeadChange = false, want true")
	}
	if loaded.Config.Roles.Reviewer.Behavior.NativeResume.OnHeadChange {
		t.Fatalf("reviewer.nativeResume.onHeadChange = true, want false")
	}
}

func TestNormalizeAllowAutoApproveLegacyAliasRespectsExplicitReviewerCleanEvent(t *testing.T) {
	trueValue := true
	comment := ReviewerReviewEventComment
	config, err := Normalize("/tmp", PartialConfig{Defaults: &PartialDefaultsConfig{AllowAutoApprove: &trueValue}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if got := config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventApprove {
		t.Fatalf("legacy clean review event = %q, want %q", got, ReviewerReviewEventApprove)
	}

	config, err = Normalize("/tmp", PartialConfig{Defaults: &PartialDefaultsConfig{AllowAutoApprove: &trueValue}, LegacyReviewer: &PartialReviewerConfig{ReviewEvents: &PartialReviewerReviewEventsConfig{Clean: &comment}}})
	if err != nil {
		t.Fatalf("Normalize(explicit) error = %v", err)
	}
	if got := config.Roles.Reviewer.Behavior.ReviewEvents.Clean; got != ReviewerReviewEventComment {
		t.Fatalf("explicit clean review event = %q, want %q", got, ReviewerReviewEventComment)
	}
}

func TestValidateRejectsInvalidReviewerReviewEvents(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.Behavior.ReviewEvents.Clean = ReviewerReviewEventRequestChanges
	cfg.Roles.Reviewer.Behavior.ReviewEvents.Blocking = ReviewerReviewEventApprove
	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "config validation failed") {
		t.Fatalf("ValidateWithOptions() error = %v, want validation failure", err)
	}
}

func TestLoadFileRejectsUnknownCLIFlagsInsteadOfPrefixMatchingThem(t *testing.T) {
	_, err := LoadFile(LoadFileOptions{Args: []string{"--hostfoo", "127.0.0.99"}, LookupEnv: emptyEnvLookup})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want error")
	}

	if err.Error() != "Unknown looperd argument: --hostfoo" {
		t.Fatalf("LoadFile() error = %q, want %q", err, "Unknown looperd argument: --hostfoo")
	}
}

func TestLoadFileRejectsInvalidCLIPortOverride(t *testing.T) {
	_, err := LoadFile(LoadFileOptions{Args: []string{"--port", "abc"}, LookupEnv: emptyEnvLookup})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want error")
	}

	if got, want := err.Error(), `invalid value for --port: "abc" is not an integer`; got != want {
		t.Fatalf("LoadFile() error = %q, want %q", got, want)
	}
}

func TestLoadFileRejectsInvalidEnvPortOverride(t *testing.T) {
	_, err := LoadFile(LoadFileOptions{LookupEnv: mapEnvLookup(map[string]string{"LOOPER_PORT": "abc"})})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want error")
	}

	if got, want := err.Error(), `invalid value for LOOPER_PORT: "abc" is not an integer`; got != want {
		t.Fatalf("LoadFile() error = %q, want %q", got, want)
	}
}

func TestLoadFileRejectsInvalidCLIBooleanOverride(t *testing.T) {
	_, err := LoadFile(LoadFileOptions{Args: []string{"--allow-auto-push", "maybe"}, LookupEnv: emptyEnvLookup})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want error")
	}

	if got, want := err.Error(), `invalid value for --allow-auto-push: "maybe" is not a boolean`; got != want {
		t.Fatalf("LoadFile() error = %q, want %q", got, want)
	}
}

func TestLoadFileRejectsInvalidEnvBooleanOverride(t *testing.T) {
	_, err := LoadFile(LoadFileOptions{LookupEnv: mapEnvLookup(map[string]string{"LOOPER_ALLOW_AUTO_PUSH": "tru"})})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want error")
	}

	if got, want := err.Error(), `invalid value for LOOPER_ALLOW_AUTO_PUSH: "tru" is not a boolean`; got != want {
		t.Fatalf("LoadFile() error = %q, want %q", got, want)
	}
}

func TestLoadFileReturnsConfigValidationErrorForUnsupportedConfig(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")

	contents := `{
		"server": {"port": 0, "authMode": "local-token"},
		"storage": {"mode": "memory"},
		"scheduler": {"pollIntervalSeconds": 2},
		"logging": {"level": "verbose", "maxFiles": 0},
		"daemon": {"mode": "invalid", "shutdownTimeoutMs": 0},
		"defaults": {"openPrStrategy": "unsupported"},
		"reviewer": {"loop": {"quietPeriodSeconds": -1, "minPublishIntervalSeconds": -1, "maxIterationsPerPR": 0, "maxWallClockSeconds": -1}, "scope": "wide", "publishMode": "stream"},
		"notifications": {"osascript": {"soundForLevels": ["ring"]}},
		"projects": [{"id": "../../tmp", "name": "bad", "repoPath": "/repos/bad"}]
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: configPath, LookupEnv: emptyEnvLookup})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want config validation error")
	}

	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("LoadFile() error = %T, want *ConfigValidationError", err)
	}

	assertValidationIssue(t, validationErr, "server.port", "must be an integer between 1 and 65535")
	assertValidationIssue(t, validationErr, "server.localToken", "is required when authMode is local-token")
	assertValidationIssue(t, validationErr, "storage.mode", "must be sqlite")
	assertValidationIssue(t, validationErr, "scheduler.pollIntervalSeconds", "must be an integer >= 10")
	assertValidationIssue(t, validationErr, "logging.level", "must be one of: debug, info, warn, error")
	assertValidationIssue(t, validationErr, "logging.maxFiles", "must be a positive integer")
	assertValidationIssue(t, validationErr, "daemon.mode", "must be one of: foreground, launchd")
	assertValidationIssue(t, validationErr, "daemon.shutdownTimeoutMs", "must be a positive integer")
	assertValidationIssue(t, validationErr, "defaults.openPrStrategy", "must be one of: all_done, first_commit, manual")
	assertValidationIssue(t, validationErr, "roles.reviewer.behavior.loop.quietPeriodSeconds", "must be an integer >= 0")
	assertValidationIssue(t, validationErr, "roles.reviewer.behavior.loop.minPublishIntervalSeconds", "must be an integer >= 0")
	assertValidationIssue(t, validationErr, "roles.reviewer.behavior.scope", "must be one of: full_pr, changed_files, changed_ranges")
	assertValidationIssue(t, validationErr, "roles.reviewer.behavior.publishMode", "must be single_review")
	assertValidationIssue(t, validationErr, "notifications.osascript.soundForLevels", "contains unsupported value: ring")
	assertValidationIssue(t, validationErr, "projects[0].id", "must not contain path separators, dot segments, or be an absolute path")
}

func TestLoadFileRejectsEnabledOsascriptNotificationsWithoutResolvedPath(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	logDir := filepath.Join(cwd, "logs")
	dbPath := filepath.Join(cwd, "looper.sqlite")

	contents := fmt.Sprintf(`{
		"daemon": {"logDir": %q, "workingDirectory": %q},
		"storage": {"dbPath": %q},
		"notifications": {"osascript": {"enabled": true, "throttleWindowSeconds": 60}}
	}`, logDir, cwd, dbPath)
	if err := os.WriteFile(configPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	_, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: configPath,
		LookupEnv:  emptyEnvLookup,
		LookPath:   fakeLookPath(map[string]string{}),
	})
	if err == nil {
		t.Fatal("LoadFile() error = nil, want config validation error")
	}

	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("LoadFile() error = %T, want *ConfigValidationError", err)
	}

	assertValidationIssue(t, validationErr, "tools.osascriptPath", "is required when notifications.osascript.enabled is true")
}

func TestValidateAllowsLegacyProjectIDsForUpgradeCompatibility(t *testing.T) {
	config, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	config.Projects = []ProjectRefConfig{{
		ID:       "legacy-id-Li4vdG1w",
		Name:     "legacy-project",
		RepoPath: "/repos/legacy-project",
	}}

	if err := Validate(config); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestValidateDaemonSupervisionConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*Config)
		wantPath  string
		wantError string
	}{
		{
			name: "valid launchd restart policy",
			mutate: func(cfg *Config) {
				cfg.Daemon.Mode = DaemonModeLaunchd
				cfg.Daemon.RestartPolicy = DaemonRestartAlways
				cfg.Daemon.RestartThrottleSeconds = 30
			},
		},
		{
			name: "invalid restart policy",
			mutate: func(cfg *Config) {
				cfg.Daemon.RestartPolicy = DaemonRestartPolicy("sometimes")
			},
			wantPath:  "daemon.restartPolicy",
			wantError: "must be one of: never, on-failure, always",
		},
		{
			name: "invalid throttle",
			mutate: func(cfg *Config) {
				cfg.Daemon.RestartThrottleSeconds = 0
			},
			wantPath:  "daemon.restartThrottleSeconds",
			wantError: "must be a positive integer",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatalf("DefaultConfig() error = %v", err)
			}
			tt.mutate(&cfg)
			err = Validate(cfg)
			if tt.wantPath == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() error = nil, want validation error")
			}
			var validationErr *ConfigValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("Validate() error = %T, want *ConfigValidationError", err)
			}
			assertValidationIssue(t, validationErr, tt.wantPath, tt.wantError)
		})
	}
}

func TestValidateRejectsDuplicateAndIncompleteProjects(t *testing.T) {
	config, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	config.Projects = []ProjectRefConfig{
		{ID: "demo", Name: "Demo", RepoPath: "/repos/demo"},
		{ID: "demo", Name: "", RepoPath: ""},
	}

	err = Validate(config)
	if err == nil {
		t.Fatal("Validate() error = nil, want config validation error")
	}

	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("Validate() error = %T, want *ConfigValidationError", err)
	}

	assertValidationIssue(t, validationErr, "projects[1].id", "duplicate project id: demo")
	assertValidationIssue(t, validationErr, "projects[1].name", "must be a non-empty string")
	assertValidationIssue(t, validationErr, "projects[1].repoPath", "must be a non-empty path")
}

func TestValidateRejectsRuntimePathsWhenNoWritableDirectoryCanBeFound(t *testing.T) {
	rootDir := t.TempDir()
	config, err := DefaultConfig(rootDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	blockingRoot := filepath.Join(rootDir, "blocked-root")
	if err := os.WriteFile(blockingRoot, []byte("not-a-directory"), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	config.Storage.DBPath = filepath.Join(blockingRoot, "looper.sqlite")
	config.Daemon.LogDir = filepath.Join(blockingRoot, "logs")
	config.Daemon.WorkingDirectory = filepath.Join(blockingRoot, "workspace")
	config.Projects = []ProjectRefConfig{{ID: "demo", Name: "Demo", RepoPath: "/repos/demo"}}

	err = ValidateWithOptions(config, ValidateOptions{
		DefaultWorktreeRoot: filepath.Join(blockingRoot, "worktrees"),
	})
	if err == nil {
		t.Fatal("ValidateWithOptions() error = nil, want config validation error")
	}

	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateWithOptions() error = %T, want *ConfigValidationError", err)
	}

	assertValidationIssue(t, validationErr, "storage.dbPath", blockingRoot+" is not a directory")
	assertValidationIssue(t, validationErr, "daemon.logDir", blockingRoot+" is not a directory")
	assertValidationIssue(t, validationErr, "daemon.workingDirectory", blockingRoot+" is not a directory")
	assertValidationIssue(t, validationErr, "defaults.worktreeRoot", blockingRoot+" is not a directory")
}

func TestValidateAcceptsWritableRuntimePaths(t *testing.T) {
	rootDir := t.TempDir()
	config, err := DefaultConfig(rootDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	logDir := filepath.Join(rootDir, "logs")
	workingDir := filepath.Join(rootDir, "workspace")
	worktreeRoot := filepath.Join(rootDir, "worktrees")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll(logDir) error = %v", err)
	}
	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll(workingDir) error = %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("os.MkdirAll(worktreeRoot) error = %v", err)
	}

	config.Storage.DBPath = filepath.Join(rootDir, "state", "looper.sqlite")
	config.Daemon.LogDir = logDir
	config.Daemon.WorkingDirectory = workingDir
	config.Projects = []ProjectRefConfig{{ID: "demo", Name: "Demo", RepoPath: "/repos/demo"}}

	if err := ValidateWithOptions(config, ValidateOptions{DefaultWorktreeRoot: worktreeRoot}); err != nil {
		t.Fatalf("ValidateWithOptions() error = %v, want nil", err)
	}
}

func TestDefaultConfigMatchesDaemonDefaults(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() error = %v", err)
	}

	config, err := DefaultConfig("/tmp/looper-cwd")
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	if config.Server.Host != "127.0.0.1" {
		t.Fatalf("DefaultConfig().Server.Host = %q, want %q", config.Server.Host, "127.0.0.1")
	}

	if config.Server.Port != DefaultServerPort {
		t.Fatalf("DefaultConfig().Server.Port = %d, want %d", config.Server.Port, DefaultServerPort)
	}

	if config.Server.AuthMode != AuthModeNone {
		t.Fatalf("DefaultConfig().Server.AuthMode = %q, want %q", config.Server.AuthMode, AuthModeNone)
	}

	if config.Storage.Mode != "sqlite" {
		t.Fatalf("DefaultConfig().Storage.Mode = %q, want %q", config.Storage.Mode, "sqlite")
	}

	if config.Storage.DBPath != filepath.Join(homeDir, ".looper", "looper.sqlite") {
		t.Fatalf("DefaultConfig().Storage.DBPath = %q, want %q", config.Storage.DBPath, filepath.Join(homeDir, ".looper", "looper.sqlite"))
	}

	if config.Storage.BackupDir == nil || *config.Storage.BackupDir != filepath.Join(homeDir, ".looper", "backups") {
		t.Fatalf("DefaultConfig().Storage.BackupDir = %v, want %q", config.Storage.BackupDir, filepath.Join(homeDir, ".looper", "backups"))
	}

	if config.Scheduler.MaxConcurrentRuns != 3 {
		t.Fatalf("DefaultConfig().Scheduler.MaxConcurrentRuns = %d, want %d", config.Scheduler.MaxConcurrentRuns, 3)
	}
	if config.Scheduler.SlowLaneWarnThresholdMS != 5000 {
		t.Fatalf("DefaultConfig().Scheduler.SlowLaneWarnThresholdMS = %d, want %d", config.Scheduler.SlowLaneWarnThresholdMS, 5000)
	}

	if config.Logging.Level != LogLevelInfo {
		t.Fatalf("DefaultConfig().Logging.Level = %q, want %q", config.Logging.Level, LogLevelInfo)
	}

	if !config.Notifications.InApp {
		t.Fatal("DefaultConfig().Notifications.InApp = false, want true")
	}

	if got, want := config.Notifications.Osascript.Enabled, runtime.GOOS == "darwin"; got != want {
		t.Fatalf("DefaultConfig().Notifications.Osascript.Enabled = %v, want %v", got, want)
	}

	if config.Daemon.Mode != DaemonModeForeground {
		t.Fatalf("DefaultConfig().Daemon.Mode = %q, want %q", config.Daemon.Mode, DaemonModeForeground)
	}

	if config.Daemon.LogDir != filepath.Join(homeDir, ".looper", "logs") {
		t.Fatalf("DefaultConfig().Daemon.LogDir = %q, want %q", config.Daemon.LogDir, filepath.Join(homeDir, ".looper", "logs"))
	}

	if config.Daemon.ShutdownTimeoutMS != 1000 {
		t.Fatalf("DefaultConfig().Daemon.ShutdownTimeoutMS = %d, want %d", config.Daemon.ShutdownTimeoutMS, 1000)
	}

	if config.Daemon.WorkingDirectory != "/tmp/looper-cwd" {
		t.Fatalf("DefaultConfig().Daemon.WorkingDirectory = %q, want %q", config.Daemon.WorkingDirectory, "/tmp/looper-cwd")
	}

	if config.Defaults.OpenPRStrategy != OpenPRStrategyAllDone {
		t.Fatalf("DefaultConfig().Defaults.OpenPRStrategy = %q, want %q", config.Defaults.OpenPRStrategy, OpenPRStrategyAllDone)
	}

	if config.Defaults.AddSnapshotMode != AddSnapshotModeAsync {
		t.Fatalf("DefaultConfig().Defaults.AddSnapshotMode = %q, want %q", config.Defaults.AddSnapshotMode, AddSnapshotModeAsync)
	}

	if len(config.Agent.Params) != 0 || len(config.Agent.Env) != 0 {
		t.Fatalf("DefaultConfig().Agent maps = %#v / %#v, want empty maps", config.Agent.Params, config.Agent.Env)
	}

	if config.Roles.Reviewer.Behavior.NativeResume.OnHeadChange {
		t.Fatal("DefaultConfig().Reviewer.NativeResume.OnHeadChange = true, want false")
	}
	if config.Roles.Reviewer.Behavior.NativeResume.ReReviewPromptOnHeadChange {
		t.Fatal("DefaultConfig().Reviewer.NativeResume.ReReviewPromptOnHeadChange = true, want false")
	}

	if len(config.Projects) != 0 {
		t.Fatalf("DefaultConfig().Projects len = %d, want 0", len(config.Projects))
	}
}

func TestNormalizeAppliesOverridesWithoutDroppingDefaults(t *testing.T) {
	openPRStrategy := OpenPRStrategyAllDone
	authMode := AuthModeLocalToken
	level := LogLevelDebug
	daemonMode := DaemonModeLaunchd
	trueValue := true
	falseValue := false
	port := 7000
	pollInterval := 45
	throttleWindow := 120
	maxFiles := 9
	localToken := "secret"
	baseURL := "http://127.0.0.1:9999"
	vendor := AgentVendorOpenCode
	model := "gpt-5.4"
	logDir := "/var/log/looper"
	baseBranch := "develop"
	repoBaseBranch := "stable"
	worktreeRoot := "/tmp/worktrees/project-a"

	projects := []PartialProjectRefConfig{{
		ID:           "project-a",
		Name:         "Project A",
		RepoPath:     "/repos/project-a",
		BaseBranch:   &repoBaseBranch,
		WorktreeRoot: &worktreeRoot,
	}}

	config, err := Normalize("/tmp/original-cwd", PartialConfig{
		Server: &PartialServerConfig{
			Port:       &port,
			BaseURL:    &baseURL,
			AuthMode:   &authMode,
			LocalToken: &localToken,
		},
		Scheduler: &PartialSchedulerConfig{
			PollIntervalSeconds: &pollInterval,
		},
		Agent: &PartialAgentConfig{
			Vendor: &vendor,
			Model:  &model,
			Params: map[string]any{"reasoning": "medium"},
			Env:    map[string]string{"OPENAI_API_KEY": "replace-me"},
		},
		Logging: &PartialLoggingConfig{
			Level:    &level,
			MaxFiles: &maxFiles,
		},
		Notifications: &PartialNotificationConfig{
			InApp: &falseValue,
			Osascript: &PartialOsascriptNotificationConfig{
				Enabled:               &falseValue,
				SoundForLevels:        &[]NotificationSoundLevel{NotificationSoundLevelFailure},
				ThrottleWindowSeconds: &throttleWindow,
			},
		},
		Tools: &PartialToolPathsConfig{
			GitPath: stringPtr("/custom/bin/git"),
		},
		Daemon: &PartialDaemonConfig{
			Mode:              &daemonMode,
			LogDir:            &logDir,
			ShutdownTimeoutMS: intPtr(2500),
			WorkingDirectory:  stringPtr("/workspace"),
			Environment:       map[string]string{"EXAMPLE_FLAG": "1"},
		},
		Defaults: &PartialDefaultsConfig{
			BaseBranch:       &baseBranch,
			AllowAutoApprove: &trueValue,
			OpenPRStrategy:   &openPRStrategy,
		},
		Projects: &projects,
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	if config.Server.Host != "127.0.0.1" {
		t.Fatalf("Normalize().Server.Host = %q, want default %q", config.Server.Host, "127.0.0.1")
	}

	if config.Server.Port != 7000 {
		t.Fatalf("Normalize().Server.Port = %d, want %d", config.Server.Port, 7000)
	}

	if config.Server.BaseURL == nil || *config.Server.BaseURL != baseURL {
		t.Fatalf("Normalize().Server.BaseURL = %v, want %q", config.Server.BaseURL, baseURL)
	}

	if config.Server.AuthMode != AuthModeLocalToken {
		t.Fatalf("Normalize().Server.AuthMode = %q, want %q", config.Server.AuthMode, AuthModeLocalToken)
	}

	if config.Server.LocalToken == nil || *config.Server.LocalToken != localToken {
		t.Fatalf("Normalize().Server.LocalToken = %v, want %q", config.Server.LocalToken, localToken)
	}

	if config.Scheduler.PollIntervalSeconds != 45 {
		t.Fatalf("Normalize().Scheduler.PollIntervalSeconds = %d, want %d", config.Scheduler.PollIntervalSeconds, 45)
	}

	if config.Scheduler.MaxConcurrentRuns != 3 {
		t.Fatalf("Normalize().Scheduler.MaxConcurrentRuns = %d, want default %d", config.Scheduler.MaxConcurrentRuns, 3)
	}

	if config.Scheduler.SlowLaneWarnThresholdMS != 5000 {
		t.Fatalf("Normalize().Scheduler.SlowLaneWarnThresholdMS = %d, want default %d", config.Scheduler.SlowLaneWarnThresholdMS, 5000)
	}

	if config.Agent.Vendor == nil || *config.Agent.Vendor != AgentVendorOpenCode {
		t.Fatalf("Normalize().Agent.Vendor = %v, want %q", config.Agent.Vendor, AgentVendorOpenCode)
	}

	if config.Agent.Model == nil || *config.Agent.Model != model {
		t.Fatalf("Normalize().Agent.Model = %v, want %q", config.Agent.Model, model)
	}

	if got := config.Agent.Params["reasoning"]; got != "medium" {
		t.Fatalf("Normalize().Agent.Params[reasoning] = %v, want %q", got, "medium")
	}

	if got := config.Agent.Env["OPENAI_API_KEY"]; got != "replace-me" {
		t.Fatalf("Normalize().Agent.Env[OPENAI_API_KEY] = %q, want %q", got, "replace-me")
	}

	if config.Logging.Level != LogLevelDebug {
		t.Fatalf("Normalize().Logging.Level = %q, want %q", config.Logging.Level, LogLevelDebug)
	}

	if config.Logging.MaxSizeMB != 10 {
		t.Fatalf("Normalize().Logging.MaxSizeMB = %d, want default %d", config.Logging.MaxSizeMB, 10)
	}

	if config.Logging.MaxFiles != 9 {
		t.Fatalf("Normalize().Logging.MaxFiles = %d, want %d", config.Logging.MaxFiles, 9)
	}

	if config.Notifications.InApp {
		t.Fatal("Normalize().Notifications.InApp = true, want false")
	}

	if config.Notifications.Osascript.Enabled {
		t.Fatal("Normalize().Notifications.Osascript.Enabled = true, want false")
	}

	if len(config.Notifications.Osascript.SoundForLevels) != 1 || config.Notifications.Osascript.SoundForLevels[0] != NotificationSoundLevelFailure {
		t.Fatalf("Normalize().Notifications.Osascript.SoundForLevels = %#v, want [%q]", config.Notifications.Osascript.SoundForLevels, NotificationSoundLevelFailure)
	}

	if config.Notifications.Osascript.ThrottleWindowSeconds != 120 {
		t.Fatalf("Normalize().Notifications.Osascript.ThrottleWindowSeconds = %d, want %d", config.Notifications.Osascript.ThrottleWindowSeconds, 120)
	}

	if config.Tools.GitPath == nil || *config.Tools.GitPath != "/custom/bin/git" {
		t.Fatalf("Normalize().Tools.GitPath = %v, want %q", config.Tools.GitPath, "/custom/bin/git")
	}

	if config.Daemon.Mode != DaemonModeLaunchd {
		t.Fatalf("Normalize().Daemon.Mode = %q, want %q", config.Daemon.Mode, DaemonModeLaunchd)
	}

	if config.Daemon.LogDir != logDir {
		t.Fatalf("Normalize().Daemon.LogDir = %q, want %q", config.Daemon.LogDir, logDir)
	}

	if config.Daemon.ShutdownTimeoutMS != 2500 {
		t.Fatalf("Normalize().Daemon.ShutdownTimeoutMS = %d, want %d", config.Daemon.ShutdownTimeoutMS, 2500)
	}

	if config.Daemon.WorkingDirectory != "/workspace" {
		t.Fatalf("Normalize().Daemon.WorkingDirectory = %q, want %q", config.Daemon.WorkingDirectory, "/workspace")
	}

	if got := config.Daemon.Environment["EXAMPLE_FLAG"]; got != "1" {
		t.Fatalf("Normalize().Daemon.Environment[EXAMPLE_FLAG] = %q, want %q", got, "1")
	}

	if config.Defaults.BaseBranch != baseBranch {
		t.Fatalf("Normalize().Defaults.BaseBranch = %q, want %q", config.Defaults.BaseBranch, baseBranch)
	}

	if !config.Defaults.AllowAutoCommit {
		t.Fatal("Normalize().Defaults.AllowAutoCommit = false, want true")
	}

	if !config.Defaults.AllowAutoApprove {
		t.Fatal("Normalize().Defaults.AllowAutoApprove = false, want true")
	}

	if config.Defaults.OpenPRStrategy != OpenPRStrategyAllDone {
		t.Fatalf("Normalize().Defaults.OpenPRStrategy = %q, want %q", config.Defaults.OpenPRStrategy, OpenPRStrategyAllDone)
	}

	if len(config.Projects) != 1 {
		t.Fatalf("Normalize().Projects len = %d, want 1", len(config.Projects))
	}

	if config.Projects[0].BaseBranch == nil || *config.Projects[0].BaseBranch != repoBaseBranch {
		t.Fatalf("Normalize().Projects[0].BaseBranch = %v, want %q", config.Projects[0].BaseBranch, repoBaseBranch)
	}

	if config.Projects[0].WorktreeRoot == nil || *config.Projects[0].WorktreeRoot != worktreeRoot {
		t.Fatalf("Normalize().Projects[0].WorktreeRoot = %v, want %q", config.Projects[0].WorktreeRoot, worktreeRoot)
	}
}

func TestNormalizeReplacesArraysAndClonesMaps(t *testing.T) {
	soundLevels := []NotificationSoundLevel{}
	projects := []PartialProjectRefConfig{{ID: "project-b", Name: "Project B", RepoPath: "/repos/project-b"}}
	params := map[string]any{"reasoning": "high"}
	env := map[string]string{"FOO": "bar"}
	environment := map[string]string{"BAR": "baz"}

	config, err := Normalize("/tmp", PartialConfig{
		Agent: &PartialAgentConfig{
			Params: params,
			Env:    env,
		},
		Notifications: &PartialNotificationConfig{
			Osascript: &PartialOsascriptNotificationConfig{SoundForLevels: &soundLevels},
		},
		Daemon:   &PartialDaemonConfig{Environment: environment},
		Projects: &projects,
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	params["reasoning"] = "low"
	env["FOO"] = "changed"
	environment["BAR"] = "changed"
	projects[0].Name = "Changed"

	if got := config.Agent.Params["reasoning"]; got != "high" {
		t.Fatalf("Normalize().Agent.Params[reasoning] = %v, want %q", got, "high")
	}

	if got := config.Agent.Env["FOO"]; got != "bar" {
		t.Fatalf("Normalize().Agent.Env[FOO] = %q, want %q", got, "bar")
	}

	if got := config.Daemon.Environment["BAR"]; got != "baz" {
		t.Fatalf("Normalize().Daemon.Environment[BAR] = %q, want %q", got, "baz")
	}

	if len(config.Notifications.Osascript.SoundForLevels) != 0 {
		t.Fatalf("Normalize().Notifications.Osascript.SoundForLevels len = %d, want 0", len(config.Notifications.Osascript.SoundForLevels))
	}

	if config.Projects[0].Name != "Project B" {
		t.Fatalf("Normalize().Projects[0].Name = %q, want %q", config.Projects[0].Name, "Project B")
	}
}

func TestNormalizeAppliesLayersInOrder(t *testing.T) {
	host := "0.0.0.0"
	port := 6000
	overriddenPort := 7000
	baseParams := map[string]any{"reasoning": map[string]any{"level": "high", "mode": "careful"}}
	overrideParams := map[string]any{"reasoning": map[string]any{"mode": "fast"}, "verbosity": "low"}
	projects := []PartialProjectRefConfig{}

	config, err := Normalize("/tmp/cwd",
		PartialConfig{
			Server: &PartialServerConfig{Host: &host, Port: &port},
			Agent:  &PartialAgentConfig{Params: baseParams},
		},
		PartialConfig{
			Server:   &PartialServerConfig{Port: &overriddenPort},
			Agent:    &PartialAgentConfig{Params: overrideParams},
			Projects: &projects,
		},
	)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	if config.Server.Host != host {
		t.Fatalf("Normalize().Server.Host = %q, want %q", config.Server.Host, host)
	}

	if config.Server.Port != overriddenPort {
		t.Fatalf("Normalize().Server.Port = %d, want %d", config.Server.Port, overriddenPort)
	}

	reasoning, ok := config.Agent.Params["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("Normalize().Agent.Params[reasoning] type = %T, want map[string]any", config.Agent.Params["reasoning"])
	}

	if got := reasoning["level"]; got != "high" {
		t.Fatalf("Normalize().Agent.Params[reasoning][level] = %v, want %q", got, "high")
	}

	if got := reasoning["mode"]; got != "fast" {
		t.Fatalf("Normalize().Agent.Params[reasoning][mode] = %v, want %q", got, "fast")
	}

	if got := config.Agent.Params["verbosity"]; got != "low" {
		t.Fatalf("Normalize().Agent.Params[verbosity] = %v, want %q", got, "low")
	}

	if len(config.Projects) != 0 {
		t.Fatalf("Normalize().Projects len = %d, want 0", len(config.Projects))
	}
}

func TestDefaultPathHelpersMatchTSLayout(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() error = %v", err)
	}

	configPath, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath() error = %v", err)
	}

	if configPath != filepath.Join(homeDir, ".looper", "config.toml") {
		t.Fatalf("DefaultConfigPath() = %q, want %q", configPath, filepath.Join(homeDir, ".looper", "config.toml"))
	}

	worktreeRoot, err := DefaultProjectWorktreeRoot("example-project", "/tmp/example-repo")
	if err != nil {
		t.Fatalf("DefaultProjectWorktreeRoot() error = %v", err)
	}

	wantPrefix := filepath.Join(homeDir, ".looper", "worktrees", ToRepoWorktreeDirectoryName("/tmp/example-repo"))
	if filepath.Dir(worktreeRoot) != wantPrefix {
		t.Fatalf("filepath.Dir(DefaultProjectWorktreeRoot()) = %q, want %q", filepath.Dir(worktreeRoot), wantPrefix)
	}

	if filepath.Base(worktreeRoot) != "example-project" {
		t.Fatalf("filepath.Base(DefaultProjectWorktreeRoot()) = %q, want %q", filepath.Base(worktreeRoot), "example-project")
	}
}

func TestProjectDirectoryNamingMatchesTSRules(t *testing.T) {
	longProjectID := strings.Repeat("a", 256)
	longProjectIDHash := sha256Hex(longProjectID)

	testCases := []struct {
		name      string
		projectID string
		want      string
	}{
		{name: "canonical", projectID: "example-project", want: "example-project"},
		{name: "relative traversal", projectID: "../tmp", want: legacyProjectIDPrefix + hex.EncodeToString([]byte("../tmp"))},
		{name: "mixed case", projectID: "Foo", want: legacyProjectIDPrefix + hex.EncodeToString([]byte("Foo"))},
		{name: "windows reserved name", projectID: "con", want: legacyProjectIDPrefix + hex.EncodeToString([]byte("con"))},
		{name: "empty", projectID: "", want: legacyProjectIDPrefix + "empty"},
		{name: "hashed fallback", projectID: longProjectID, want: legacyProjectIDPrefix + longProjectIDHash},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ToProjectWorktreeDirectoryName(testCase.projectID); got != testCase.want {
				t.Fatalf("ToProjectWorktreeDirectoryName(%q) = %q, want %q", testCase.projectID, got, testCase.want)
			}
		})
	}
}

func TestRepoWorktreeDirectoryNameCanonicalizesSymlinks(t *testing.T) {
	repoRoot := t.TempDir()
	symlinkPath := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(repoRoot, symlinkPath); err != nil {
		t.Skipf("os.Symlink() unavailable: %v", err)
	}

	canonicalName := ToRepoWorktreeDirectoryName(repoRoot)
	symlinkName := ToRepoWorktreeDirectoryName(symlinkPath)
	if canonicalName != symlinkName {
		t.Fatalf("ToRepoWorktreeDirectoryName(%q) = %q, want %q", symlinkPath, symlinkName, canonicalName)
	}
}

func TestDefaultConfigSetsWebhookDefaults(t *testing.T) {
	t.Parallel()

	config, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	if config.Webhook.Enabled {
		t.Fatal("DefaultConfig().Webhook.Enabled = true, want false")
	}
	if config.Webhook.FallbackPollIntervalSeconds != 300 {
		t.Fatalf("DefaultConfig().Webhook.FallbackPollIntervalSeconds = %d, want 300", config.Webhook.FallbackPollIntervalSeconds)
	}
}

func TestValidateRejectsShortWebhookFallbackPollInterval(t *testing.T) {
	t.Parallel()

	config, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	config.Webhook.FallbackPollIntervalSeconds = 59

	validation := Validate(config)
	validationErr, ok := validation.(*ConfigValidationError)
	if !ok || validationErr == nil {
		t.Fatalf("Validate() error = %T %v, want *ConfigValidationError", validation, validation)
	}
	assertValidationIssue(t, validationErr, "webhook.fallbackPollIntervalSeconds", "must be an integer >= 60")
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}

func TestValidateCoordinatorDependenciesRequiresPositiveBoundsWhenEnabled(t *testing.T) {
	cwd := t.TempDir()
	cfg, err := DefaultConfig(cwd)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	cfg.Roles.Coordinator.Dependencies.APITimeoutSeconds = 0
	cfg.Roles.Coordinator.Dependencies.APIRetryAttempts = 0

	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	if err == nil {
		t.Fatal("ValidateWithOptions() error = nil, want validation error")
	}
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("ValidateWithOptions() error = %T, want *ConfigValidationError", err)
	}
	assertValidationIssue(t, validationErr, "roles.coordinator.dependencies.apiTimeoutSeconds", "must be a positive integer when dependencies are enabled")
	assertValidationIssue(t, validationErr, "roles.coordinator.dependencies.apiRetryAttempts", "must be a positive integer when dependencies are enabled")
}

func mapEnvLookup(values map[string]string) EnvLookupFunc {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func skipIfSweeperConfigUnsupported(t *testing.T, err error) {
	t.Helper()
	if strings.Contains(err.Error(), "json: unknown field \"sweeper\"") {
		t.Skip("sweeper role support is not yet present in config schema")
	}
	t.Fatalf("LoadFile() error = %v", err)
}

func fakeLookPath(values map[string]string) LookPathFunc {
	return func(name string) (string, error) {
		value, ok := values[name]
		if !ok {
			return "", errors.New("not found")
		}

		return value, nil
	}
}

func assertValidationIssue(t *testing.T, err *ConfigValidationError, path string, message string) {
	t.Helper()

	for _, issue := range err.Issues {
		if issue.Path == path && issue.Message == message {
			return
		}
	}

	t.Fatalf("ConfigValidationError missing issue {%q, %q}: %#v", path, message, err.Issues)
}

func hasValidationIssue(issues []ValidationIssue, path string) bool {
	for _, issue := range issues {
		if issue.Path == path {
			return true
		}
	}
	return false
}

func emptyEnvLookup(string) (string, bool) {
	return "", false
}

func intPtr(value int) *int {
	return &value
}

func labelModePtr(value LabelMode) *LabelMode {
	return &value
}
