package config

import (
	"testing"
)

func TestValidate_AgentProfilesAndRoleBindings(t *testing.T) {
	t.Parallel()

	t.Run("unknown profile ref", func(t *testing.T) {
		t.Parallel()
		cfg := mustDefaultConfig(t)
		cfg.Roles.Planner.Agent = &RoleAgentConfig{Profile: stringPtr("missing")}
		err := ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
		var validationErr *ConfigValidationError
		if !asConfigValidationError(err, &validationErr) {
			t.Fatalf("Validate() error = %v, want ConfigValidationError", err)
		}
		assertValidationIssue(t, validationErr, "roles.planner.agent.profile", "references unknown agent profile: missing")
	})

	t.Run("invalid profile id with dots", func(t *testing.T) {
		t.Parallel()
		vendor := AgentVendorCodex
		cfg := mustDefaultConfig(t)
		cfg.Agent.Profiles = map[string]AgentBindingConfig{
			"bad.id": {Vendor: &vendor},
		}
		err := ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
		var validationErr *ConfigValidationError
		if !asConfigValidationError(err, &validationErr) {
			t.Fatalf("Validate() error = %v, want ConfigValidationError", err)
		}
		assertValidationIssue(t, validationErr, "agent.profiles.bad.id", "profile id must be non-empty, trimmed, and match [A-Za-z0-9_-]+")
	})

	t.Run("empty profile object", func(t *testing.T) {
		t.Parallel()
		cfg := mustDefaultConfig(t)
		cfg.Agent.Profiles = map[string]AgentBindingConfig{
			"empty": {},
		}
		err := ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
		var validationErr *ConfigValidationError
		if !asConfigValidationError(err, &validationErr) {
			t.Fatalf("Validate() error = %v, want ConfigValidationError", err)
		}
		assertValidationIssue(t, validationErr, "agent.profiles.empty", "must set at least one of vendor or model")
	})

	t.Run("project-level agent binding", func(t *testing.T) {
		t.Parallel()
		vendor := AgentVendorCodex
		cfg := mustDefaultConfig(t)
		cfg.Projects = []ProjectRefConfig{{
			ID:       "demo",
			Name:     "Demo",
			RepoPath: t.TempDir(),
			Roles: &PartialRoleConfigs{
				Planner: &PartialPlannerRoleConfig{
					Agent: &RoleAgentConfig{Vendor: &vendor},
				},
			},
		}}
		err := ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
		var validationErr *ConfigValidationError
		if !asConfigValidationError(err, &validationErr) {
			t.Fatalf("Validate() error = %v, want ConfigValidationError", err)
		}
		assertValidationIssue(t, validationErr, "projects[0].roles.planner.agent", "project-level agent bindings are not supported")
	})
}

func TestValidate_EmptyProfileModelCountsAsSet(t *testing.T) {
	t.Parallel()

	// Non-nil empty model is a valid suppress binding for a profile.
	cfg := mustDefaultConfig(t)
	cfg.Agent.Profiles = map[string]AgentBindingConfig{
		"suppress": {Model: stringPtr("")},
	}
	if err := ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()}); err != nil {
		t.Fatalf("ValidateWithOptions() error = %v, want nil (empty model counts as set)", err)
	}
}

func TestValidate_InvalidVendorInProfileAndRoleBinding(t *testing.T) {
	t.Parallel()

	invalid := AgentVendor("not-a-vendor")
	cfg := mustDefaultConfig(t)
	cfg.Agent.Profiles = map[string]AgentBindingConfig{
		"bad": {Vendor: &invalid},
	}
	cfg.Roles.Worker.Agent = &RoleAgentConfig{Vendor: &invalid}

	err := ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	if err == nil {
		t.Fatal("ValidateWithOptions() error = nil, want invalid vendor issues")
	}
	var validationErr *ConfigValidationError
	if !asConfigValidationError(err, &validationErr) {
		t.Fatalf("error type = %T, want *ConfigValidationError", err)
	}
	wantMsg := agentVendorValidationMessage()
	assertValidationIssue(t, validationErr, "agent.profiles.bad.vendor", wantMsg)
	assertValidationIssue(t, validationErr, "roles.worker.agent.vendor", wantMsg)
}

func asConfigValidationError(err error, target **ConfigValidationError) bool {
	if err == nil {
		return false
	}
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		return false
	}
	*target = validationErr
	return true
}
