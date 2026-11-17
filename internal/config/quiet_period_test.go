package config

import (
	"strconv"
	"testing"
	"time"
)

func TestDefaultQuietPeriodValues(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	if cfg.Defaults.Loop.QuietPeriodSeconds != 60 {
		t.Fatalf("defaults.loop.quietPeriodSeconds = %d, want 60", cfg.Defaults.Loop.QuietPeriodSeconds)
	}
	if cfg.Roles.Reviewer.Behavior.Loop.QuietPeriodSeconds != 60 {
		t.Fatalf("reviewer quietPeriodSeconds = %d, want 60", cfg.Roles.Reviewer.Behavior.Loop.QuietPeriodSeconds)
	}
	if cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds != 0 {
		t.Fatalf("fixer quietPeriodSeconds = %d, want 0 (opt-in)", cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds)
	}
}

func TestQuietPeriodInheritanceFromDefaultsLoop(t *testing.T) {
	t.Parallel()
	quiet := 120
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Defaults: &PartialDefaultsConfig{
			Loop: &PartialDefaultsLoopConfig{QuietPeriodSeconds: &quiet},
		},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if cfg.Defaults.Loop.QuietPeriodSeconds != 120 {
		t.Fatalf("defaults.loop = %d, want 120", cfg.Defaults.Loop.QuietPeriodSeconds)
	}
	// Role fields unset → inherit defaults.loop.
	if cfg.Roles.Reviewer.Behavior.Loop.QuietPeriodSeconds != 120 {
		t.Fatalf("reviewer inherited = %d, want 120", cfg.Roles.Reviewer.Behavior.Loop.QuietPeriodSeconds)
	}
	if cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds != 120 {
		t.Fatalf("fixer inherited = %d, want 120", cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds)
	}
}

func TestQuietPeriodRoleOverrideWinsOverDefaultsLoop(t *testing.T) {
	t.Parallel()
	defaultsQuiet := 120
	reviewerQuiet := 30
	fixerQuiet := 0
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Defaults: &PartialDefaultsConfig{
			Loop: &PartialDefaultsLoopConfig{QuietPeriodSeconds: &defaultsQuiet},
		},
		Roles: &PartialRoleConfigs{
			Reviewer: &PartialReviewerRoleConfig{
				Behavior: &PartialReviewerConfig{
					Loop: &PartialReviewerLoopConfig{QuietPeriodSeconds: &reviewerQuiet},
				},
			},
			Fixer: &PartialFixerRoleConfig{
				Behavior: &PartialFixerBehaviorConfig{
					Loop: &PartialFixerLoopConfig{QuietPeriodSeconds: &fixerQuiet},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if cfg.Roles.Reviewer.Behavior.Loop.QuietPeriodSeconds != 30 {
		t.Fatalf("reviewer override = %d, want 30", cfg.Roles.Reviewer.Behavior.Loop.QuietPeriodSeconds)
	}
	if cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds != 0 {
		t.Fatalf("fixer override = %d, want 0", cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds)
	}
}

func TestQuietPeriodProjectOverlayOverridesFixer(t *testing.T) {
	t.Parallel()
	globalQuiet := 60
	projectQuiet := 120
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Roles: &PartialRoleConfigs{
			Fixer: &PartialFixerRoleConfig{
				Behavior: &PartialFixerBehaviorConfig{
					Loop: &PartialFixerLoopConfig{QuietPeriodSeconds: &globalQuiet},
				},
			},
		},
		Projects: &[]PartialProjectRefConfig{
			{
				ID:       "demo",
				Name:     "Demo",
				RepoPath: t.TempDir(),
				Roles: &PartialRoleConfigs{
					Fixer: &PartialFixerRoleConfig{
						Behavior: &PartialFixerBehaviorConfig{
							Loop: &PartialFixerLoopConfig{QuietPeriodSeconds: &projectQuiet},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds != 60 {
		t.Fatalf("global fixer quiet = %d, want 60", cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds)
	}
	roles := ProjectRoleConfigs(cfg, "demo")
	if roles.Fixer.Behavior.Loop.QuietPeriodSeconds != 120 {
		t.Fatalf("project fixer quiet = %d, want 120", roles.Fixer.Behavior.Loop.QuietPeriodSeconds)
	}
	// Unknown project falls back to global.
	fallback := ProjectRoleConfigs(cfg, "missing")
	if fallback.Fixer.Behavior.Loop.QuietPeriodSeconds != 60 {
		t.Fatalf("fallback fixer quiet = %d, want 60", fallback.Fixer.Behavior.Loop.QuietPeriodSeconds)
	}
}

func TestQuietPeriodValidationRejectsNegative(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Defaults.Loop.QuietPeriodSeconds = -1
	cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds = -2
	err = Validate(cfg)
	if err == nil {
		t.Fatal("Validate() = nil, want validation error")
	}
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("Validate() error type = %T, want *ConfigValidationError", err)
	}
	assertValidationIssue(t, validationErr, "defaults.loop.quietPeriodSeconds", "must be an integer >= 0")
	assertValidationIssue(t, validationErr, "roles.fixer.behavior.loop.quietPeriodSeconds", "must be an integer >= 0")
}

func TestQuietPeriodValidationRejectsDurationOverflow(t *testing.T) {
	t.Parallel()
	if strconv.IntSize < 64 {
		t.Skip("overflow quiet-period value requires 64-bit int")
	}
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	const maxDuration = time.Duration(1<<63 - 1)
	overflow := int(maxDuration/time.Second) + 1
	cfg.Defaults.Loop.QuietPeriodSeconds = overflow
	cfg.Roles.Reviewer.Behavior.Loop.QuietPeriodSeconds = overflow
	cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds = overflow
	err = Validate(cfg)
	if err == nil {
		t.Fatal("Validate() = nil, want validation error for duration overflow")
	}
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("Validate() error type = %T, want *ConfigValidationError", err)
	}
	msg := "must fit within time.Duration when converted from seconds"
	assertValidationIssue(t, validationErr, "defaults.loop.quietPeriodSeconds", msg)
	assertValidationIssue(t, validationErr, "roles.reviewer.behavior.loop.quietPeriodSeconds", msg)
	assertValidationIssue(t, validationErr, "roles.fixer.behavior.loop.quietPeriodSeconds", msg)
}

func TestQuietPeriodValidationRejectsDurationOverflowProjectOverride(t *testing.T) {
	t.Parallel()
	if strconv.IntSize < 64 {
		t.Skip("overflow quiet-period value requires 64-bit int")
	}
	const maxDuration = time.Duration(1<<63 - 1)
	overflow := int(maxDuration/time.Second) + 1
	repoPath := t.TempDir()
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Projects: &[]PartialProjectRefConfig{
			{
				ID:       "demo",
				Name:     "Demo",
				RepoPath: repoPath,
				Roles: &PartialRoleConfigs{
					Fixer: &PartialFixerRoleConfig{
						Behavior: &PartialFixerBehaviorConfig{
							Loop: &PartialFixerLoopConfig{QuietPeriodSeconds: &overflow},
						},
					},
					Reviewer: &PartialReviewerRoleConfig{
						Behavior: &PartialReviewerConfig{
							Loop: &PartialReviewerLoopConfig{QuietPeriodSeconds: &overflow},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	err = Validate(cfg)
	if err == nil {
		t.Fatal("Validate() = nil, want validation error for project quiet-period overflow")
	}
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("Validate() error type = %T, want *ConfigValidationError", err)
	}
	msg := "must fit within time.Duration when converted from seconds"
	assertValidationIssue(t, validationErr, "projects[0].roles.fixer.behavior.loop.quietPeriodSeconds", msg)
	assertValidationIssue(t, validationErr, "projects[0].roles.reviewer.behavior.loop.quietPeriodSeconds", msg)
}

func TestQuietPeriodValidationRejectsNegativeProjectOverride(t *testing.T) {
	t.Parallel()
	repoPath := t.TempDir()
	negativeQuiet := -5
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Projects: &[]PartialProjectRefConfig{
			{
				ID:       "demo",
				Name:     "Demo",
				RepoPath: repoPath,
				Roles: &PartialRoleConfigs{
					Fixer: &PartialFixerRoleConfig{
						Behavior: &PartialFixerBehaviorConfig{
							Loop: &PartialFixerLoopConfig{QuietPeriodSeconds: &negativeQuiet},
						},
					},
					Reviewer: &PartialReviewerRoleConfig{
						Behavior: &PartialReviewerConfig{
							Loop: &PartialReviewerLoopConfig{QuietPeriodSeconds: &negativeQuiet},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	// Global values remain valid; only project overrides are negative.
	if cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds < 0 {
		t.Fatalf("global fixer quiet = %d, want non-negative before project validation", cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds)
	}
	err = Validate(cfg)
	if err == nil {
		t.Fatal("Validate() = nil, want validation error for project quiet-period overrides")
	}
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("Validate() error type = %T, want *ConfigValidationError", err)
	}
	assertValidationIssue(t, validationErr, "projects[0].roles.fixer.behavior.loop.quietPeriodSeconds", "must be an integer >= 0")
	assertValidationIssue(t, validationErr, "projects[0].roles.reviewer.behavior.loop.quietPeriodSeconds", "must be an integer >= 0")
}

func TestQuietPeriodNoInheritanceWhenDefaultsLoopUnset(t *testing.T) {
	t.Parallel()
	// Without an explicit defaults.loop partial, role defaults stay at DefaultConfig values.
	cfg, err := Normalize(t.TempDir(), PartialConfig{})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if cfg.Roles.Reviewer.Behavior.Loop.QuietPeriodSeconds != 60 {
		t.Fatalf("reviewer = %d, want 60", cfg.Roles.Reviewer.Behavior.Loop.QuietPeriodSeconds)
	}
	if cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds != 0 {
		t.Fatalf("fixer = %d, want 0", cfg.Roles.Fixer.Behavior.Loop.QuietPeriodSeconds)
	}
}
