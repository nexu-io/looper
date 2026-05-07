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
	if got := cfg.Roles.Reviewer; !got.AutoDiscovery || got.Triggers.IncludeDrafts || !got.Triggers.RequireReviewRequest || got.Triggers.LabelMode != LabelModeAll || len(got.Triggers.Labels) != 0 || !got.SpecReview.IncludeReviewingLabel || got.SpecReview.ReviewingLabel != "looper:spec-reviewing" {
		t.Fatalf("reviewer role defaults = %#v", got)
	}
	if got := reviewerEnableSelfReviewValue(t, cfg.Roles.Reviewer.Triggers); got {
		t.Fatalf("reviewer enableSelfReview default = %v, want false", got)
	}
	if got := cfg.Roles.Fixer; !got.AutoDiscovery || got.Triggers.IncludeDrafts || got.Triggers.AuthorFilter != FixerAuthorFilterCurrentUser || got.Triggers.LabelMode != LabelModeAll || len(got.Triggers.Labels) != 0 {
		t.Fatalf("fixer role defaults = %#v", got)
	}
	if got := cfg.Roles.Worker; !got.AutoDiscovery || got.Triggers.LabelMode != LabelModeAll || !got.Triggers.RequireAssigneeCurrentUser || !reflectStringSlicesEqual(got.Triggers.Labels, []string{"looper:worker-ready"}) {
		t.Fatalf("worker role defaults = %#v", got)
	}
	if got := cfg.Reviewer.Loop.MaxWallClockSeconds; got != 0 {
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
	if !loaded.Config.Roles.Reviewer.Triggers.IncludeDrafts || loaded.Config.Roles.Reviewer.Triggers.RequireReviewRequest {
		t.Fatalf("reviewer triggers = %#v", loaded.Config.Roles.Reviewer.Triggers)
	}
	if !reflectStringSlicesEqual(loaded.Config.Roles.Reviewer.Triggers.Labels, []string{"needs-review", "spec"}) || loaded.Config.Roles.Reviewer.SpecReview.IncludeReviewingLabel {
		t.Fatalf("reviewer config = %#v", loaded.Config.Roles.Reviewer)
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
	if !strings.Contains(block.Text, "Keep specs scoped.") || !strings.Contains(block.Text, "Respect local config precedence.") {
		t.Fatalf("custom block missing instructions: %s", block.Text)
	}
}

func TestProjectRoleConfigOverridesGlobalRoleConfig(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	contents := `{
		"roles": {
			"worker": {"autoDiscovery": false, "triggers": {"labels": ["global"], "labelMode": "all", "requireAssigneeCurrentUser": true}, "instructions": "Global worker guidance."},
			"reviewer": {"triggers": {"includeDrafts": false, "requireReviewRequest": true, "enableSelfReview": false, "labels": ["review"], "labelMode": "all"}}
		},
		"projects": [{
			"id": "demo",
			"name": "Demo",
			"repoPath": "/repos/demo",
			"roles": {
				"worker": {"autoDiscovery": true, "triggers": {"labels": ["project"], "labelMode": "any", "requireAssigneeCurrentUser": false}, "instructions": "Project worker guidance."},
				"reviewer": {"triggers": {"includeDrafts": true, "enableSelfReview": true}}
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
	if global.Worker.AutoDiscovery || !reflectStringSlicesEqual(global.Worker.Triggers.Labels, []string{"global"}) || !global.Worker.Triggers.RequireAssigneeCurrentUser {
		t.Fatalf("global worker roles = %#v", global.Worker)
	}

	projectRoles := ProjectRoleConfigs(loaded.Config, "demo")
	if !projectRoles.Worker.AutoDiscovery || !reflectStringSlicesEqual(projectRoles.Worker.Triggers.Labels, []string{"project"}) || projectRoles.Worker.Triggers.LabelMode != LabelModeAny || projectRoles.Worker.Triggers.RequireAssigneeCurrentUser {
		t.Fatalf("project worker roles = %#v", projectRoles.Worker)
	}
	if !projectRoles.Reviewer.Triggers.IncludeDrafts || !projectRoles.Reviewer.Triggers.RequireReviewRequest || !reflectStringSlicesEqual(projectRoles.Reviewer.Triggers.Labels, []string{"review"}) {
		t.Fatalf("project reviewer roles = %#v", projectRoles.Reviewer)
	}
	if got := reviewerEnableSelfReviewValue(t, global.Reviewer.Triggers); got {
		t.Fatalf("global reviewer enableSelfReview = %v, want false", got)
	}
	if got := reviewerEnableSelfReviewValue(t, projectRoles.Reviewer.Triggers); !got {
		t.Fatalf("project reviewer enableSelfReview = %v, want true", got)
	}
	if !AnyProjectRoleAutoDiscoveryEnabled(loaded.Config, "worker") {
		t.Fatal("AnyProjectRoleAutoDiscoveryEnabled(worker) = false, want true from project override")
	}

	block := BuildCustomInstructionBlock(loaded.Config, "demo", "worker")
	if strings.Contains(block.Text, "Global worker guidance.") || !strings.Contains(block.Text, "Project worker guidance.") {
		t.Fatalf("custom instruction block did not use project role override: %s", block.Text)
	}
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

	if got := reviewerEnableSelfReviewValue(t, loaded.Config.Roles.Reviewer.Triggers); !got {
		t.Fatalf("global reviewer enableSelfReview = %v, want true", got)
	}
	if got := reviewerEnableSelfReviewValue(t, ProjectRoleConfigs(loaded.Config, "demo").Reviewer.Triggers); got {
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

	if got := reviewerEnableSelfReviewValue(t, loaded.Config.Roles.Reviewer.Triggers); !got {
		t.Fatalf("global reviewer enableSelfReview = %v, want true", got)
	}
	if got := reviewerEnableSelfReviewValue(t, ProjectRoleConfigs(loaded.Config, "demo").Reviewer.Triggers); !got {
		t.Fatalf("project reviewer enableSelfReview = %v, want true from env override", got)
	}
}

func TestProjectRoleInstructionsCanClearGlobalInstructions(t *testing.T) {
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Roles:    &PartialRoleConfigs{Worker: &PartialWorkerRoleConfig{Instructions: stringPtr("Global worker guidance.")}},
		Projects: &[]ProjectRefConfig{{ID: "demo", Name: "Demo", RepoPath: "/repos/demo", Roles: &PartialRoleConfigs{Worker: &PartialWorkerRoleConfig{Instructions: stringPtr("")}}}},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	block := BuildCustomInstructionBlock(cfg, "demo", "worker")
	if block.Text != "" {
		t.Fatalf("custom instruction block = %q, want empty because project role cleared global instructions", block.Text)
	}
}

func TestValidateProjectRoleInstructionAggregateBytes(t *testing.T) {
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Instructions: &PartialInstructionsConfig{MaxBytes: intPtr(8)},
		Projects:     &[]ProjectRefConfig{{ID: "demo", Name: "Demo", RepoPath: "/repos/demo", Roles: &PartialRoleConfigs{Worker: &PartialWorkerRoleConfig{Instructions: stringPtr("12345")}}, Instructions: map[string]string{"worker": "6789"}}},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	var validationErr *ConfigValidationError
	if err == nil || !errors.As(err, &validationErr) || !hasValidationIssue(validationErr.Issues, "projects[0].instructions.worker") {
		t.Fatalf("ValidateWithOptions() error = %v, issues = %#v, want aggregate project instruction validation", err, validationErr)
	}
}

func TestValidateProjectReviewerLabelCanBeClearedWhenDisabled(t *testing.T) {
	falseValue := false
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Projects: &[]ProjectRefConfig{{ID: "demo", Name: "Demo", RepoPath: "/repos/demo", Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{SpecReview: &PartialReviewerSpecReviewConfig{IncludeReviewingLabel: &falseValue, ReviewingLabel: stringPtr("")}}}}},
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
	loop := loaded.Config.Reviewer.Loop
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
	if got := loaded.Config.Reviewer.ReviewEvents.Clean; got != ReviewerReviewEventApprove {
		t.Fatalf("clean review event = %q, want %q", got, ReviewerReviewEventApprove)
	}
	if got := loaded.Config.Reviewer.ReviewEvents.Blocking; got != ReviewerReviewEventRequestChanges {
		t.Fatalf("blocking review event = %q, want %q", got, ReviewerReviewEventRequestChanges)
	}
}

func TestNormalizeAllowAutoApproveLegacyAliasRespectsExplicitReviewerCleanEvent(t *testing.T) {
	trueValue := true
	comment := ReviewerReviewEventComment
	config, err := Normalize("/tmp", PartialConfig{Defaults: &PartialDefaultsConfig{AllowAutoApprove: &trueValue}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if got := config.Reviewer.ReviewEvents.Clean; got != ReviewerReviewEventApprove {
		t.Fatalf("legacy clean review event = %q, want %q", got, ReviewerReviewEventApprove)
	}

	config, err = Normalize("/tmp", PartialConfig{Defaults: &PartialDefaultsConfig{AllowAutoApprove: &trueValue}, Reviewer: &PartialReviewerConfig{ReviewEvents: &PartialReviewerReviewEventsConfig{Clean: &comment}}})
	if err != nil {
		t.Fatalf("Normalize(explicit) error = %v", err)
	}
	if got := config.Reviewer.ReviewEvents.Clean; got != ReviewerReviewEventComment {
		t.Fatalf("explicit clean review event = %q, want %q", got, ReviewerReviewEventComment)
	}
}

func TestValidateRejectsInvalidReviewerReviewEvents(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Reviewer.ReviewEvents.Clean = ReviewerReviewEventRequestChanges
	cfg.Reviewer.ReviewEvents.Blocking = ReviewerReviewEventApprove
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
	assertValidationIssue(t, validationErr, "reviewer.loop.quietPeriodSeconds", "must be an integer >= 0")
	assertValidationIssue(t, validationErr, "reviewer.loop.minPublishIntervalSeconds", "must be an integer >= 0")
	assertValidationIssue(t, validationErr, "reviewer.scope", "must be one of: full_pr, changed_files, changed_ranges")
	assertValidationIssue(t, validationErr, "reviewer.publishMode", "must be single_review")
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

	projects := []ProjectRefConfig{{
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
	projects := []ProjectRefConfig{{ID: "project-b", Name: "Project B", RepoPath: "/repos/project-b"}}
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
	projects := []ProjectRefConfig{}

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

	if configPath != filepath.Join(homeDir, ".looper", "config.json") {
		t.Fatalf("DefaultConfigPath() = %q, want %q", configPath, filepath.Join(homeDir, ".looper", "config.json"))
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

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}

func mapEnvLookup(values map[string]string) EnvLookupFunc {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
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
