package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultConfigReviewerSkillsExtend(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	if cfg.Roles.Reviewer.Skills.Mode != ReviewerSkillsModeExtend {
		t.Fatalf("default skills mode = %q, want %q", cfg.Roles.Reviewer.Skills.Mode, ReviewerSkillsModeExtend)
	}
	if len(cfg.Roles.Reviewer.Skills.Required) != 0 || len(cfg.Roles.Reviewer.Skills.Optional) != 0 {
		t.Fatalf("default skills lists = %#v, want empty", cfg.Roles.Reviewer.Skills)
	}
}

func TestLoadFileWithoutReviewerSkillsKeepsExtendDefault(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server":{"port":17310}}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
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
	skills := loaded.Config.Roles.Reviewer.Skills
	if skills.Mode != ReviewerSkillsModeExtend || len(skills.Required) != 0 || len(skills.Optional) != 0 {
		t.Fatalf("skills = %#v, want extend with empty lists", skills)
	}
}

func TestReviewerSkillsUnsetInheritProjectOverlayAndEmptyArrays(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.Skills = ReviewerSkillsConfig{
		Mode:     ReviewerSkillsModeExtend,
		Required: []string{"security-review"},
		Optional: []string{"perf-review"},
	}
	cfg.Projects = []ProjectRefConfig{
		{ID: "inherit", Name: "Inherit", RepoPath: "/tmp/inherit"},
		{ID: "overlay", Name: "Overlay", RepoPath: "/tmp/overlay", Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{Skills: &PartialReviewerSkillsConfig{
			Mode:     reviewerSkillsModePtr(ReviewerSkillsModeReplace),
			Required: stringSlicePtr("team-review"),
			Optional: stringSlicePtr(),
		}}}},
		{ID: "clear", Name: "Clear", RepoPath: "/tmp/clear", Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{Skills: &PartialReviewerSkillsConfig{
			Required: stringSlicePtr(),
			Optional: stringSlicePtr(),
		}}}},
	}

	inherited := ProjectRoleConfigs(cfg, "inherit").Reviewer.Skills
	if inherited.Mode != ReviewerSkillsModeExtend || !reflect.DeepEqual(inherited.Required, []string{"security-review"}) || !reflect.DeepEqual(inherited.Optional, []string{"perf-review"}) {
		t.Fatalf("inherit skills = %#v", inherited)
	}

	overlay := ProjectRoleConfigs(cfg, "overlay").Reviewer.Skills
	if overlay.Mode != ReviewerSkillsModeReplace || !reflect.DeepEqual(overlay.Required, []string{"team-review"}) || overlay.Optional == nil || len(overlay.Optional) != 0 {
		t.Fatalf("overlay skills = %#v", overlay)
	}

	cleared := ProjectRoleConfigs(cfg, "clear").Reviewer.Skills
	if cleared.Mode != ReviewerSkillsModeExtend || cleared.Required == nil || len(cleared.Required) != 0 || cleared.Optional == nil || len(cleared.Optional) != 0 {
		t.Fatalf("cleared skills = %#v", cleared)
	}

	unknown := ProjectRoleConfigs(cfg, "missing").Reviewer.Skills
	if unknown.Mode != ReviewerSkillsModeExtend || !reflect.DeepEqual(unknown.Required, []string{"security-review"}) {
		t.Fatalf("unknown project skills = %#v", unknown)
	}
}

func TestNormalizeReviewerSkillsExtendAndReplace(t *testing.T) {
	t.Parallel()

	extend, err := Normalize(t.TempDir(), PartialConfig{Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{Skills: &PartialReviewerSkillsConfig{
		Mode:     reviewerSkillsModePtr(ReviewerSkillsModeExtend),
		Required: stringSlicePtr("security-review"),
		Optional: stringSlicePtr("perf-review"),
	}}}})
	if err != nil {
		t.Fatalf("Normalize(extend) error = %v", err)
	}
	if extend.Roles.Reviewer.Skills.Mode != ReviewerSkillsModeExtend || !reflect.DeepEqual(extend.Roles.Reviewer.Skills.Required, []string{"security-review"}) || !reflect.DeepEqual(extend.Roles.Reviewer.Skills.Optional, []string{"perf-review"}) {
		t.Fatalf("extend skills = %#v", extend.Roles.Reviewer.Skills)
	}

	replace, err := Normalize(t.TempDir(), PartialConfig{Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{Skills: &PartialReviewerSkillsConfig{
		Mode:     reviewerSkillsModePtr(ReviewerSkillsModeReplace),
		Required: stringSlicePtr("./review-skills/team.md"),
	}}}})
	if err != nil {
		t.Fatalf("Normalize(replace) error = %v", err)
	}
	if replace.Roles.Reviewer.Skills.Mode != ReviewerSkillsModeReplace || !reflect.DeepEqual(replace.Roles.Reviewer.Skills.Required, []string{"./review-skills/team.md"}) {
		t.Fatalf("replace skills = %#v", replace.Roles.Reviewer.Skills)
	}
}

func TestValidateReviewerSkillsInvalidModeAndReplaceWithoutRequired(t *testing.T) {
	t.Parallel()

	invalid, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	invalid.Roles.Reviewer.Skills.Mode = "append"
	err = ValidateWithOptions(invalid, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	var invalidErr *ConfigValidationError
	if !errors.As(err, &invalidErr) {
		t.Fatalf("Validate(invalid mode) error = %v, want ConfigValidationError", err)
	}
	assertValidationIssue(t, invalidErr, "roles.reviewer.skills.mode", "must be one of: extend, replace")

	emptyMode, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	emptyMode.Roles.Reviewer.Skills.Mode = ""
	if err := ValidateWithOptions(emptyMode, ValidateOptions{DefaultWorktreeRoot: t.TempDir()}); err != nil {
		t.Fatalf("Validate(empty mode) error = %v, want nil", err)
	}

	replace, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	replace.Roles.Reviewer.Skills.Mode = ReviewerSkillsModeReplace
	replace.Roles.Reviewer.Skills.Required = nil
	err = ValidateWithOptions(replace, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	var replaceErr *ConfigValidationError
	if !errors.As(err, &replaceErr) {
		t.Fatalf("Validate(replace) error = %v, want ConfigValidationError", err)
	}
	assertValidationIssue(t, replaceErr, "roles.reviewer.skills.required", "must contain at least one skill when mode is replace")
}

func TestValidateProjectOverlayReviewerSkillsReplaceWithoutRequired(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	repo := t.TempDir()
	cfg.Projects = []ProjectRefConfig{{
		ID: "demo", Name: "Demo", RepoPath: repo,
		Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{Skills: &PartialReviewerSkillsConfig{
			Mode: reviewerSkillsModePtr(ReviewerSkillsModeReplace),
		}}},
	}}
	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("Validate() error = %v, want ConfigValidationError", err)
	}
	assertValidationIssue(t, validationErr, "projects[0].roles.reviewer.skills.required", "must contain at least one skill when mode is replace")
}

func reviewerSkillsModePtr(value ReviewerSkillsMode) *ReviewerSkillsMode {
	return &value
}

func stringSlicePtr(values ...string) *[]string {
	cloned := make([]string, len(values))
	copy(cloned, values)
	return &cloned
}
