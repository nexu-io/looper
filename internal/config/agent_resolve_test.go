package config

import (
	"reflect"
	"testing"
)

func mustDefaultConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	return cfg
}

func TestResolveAgent_ZeroDiffGlobalOnly(t *testing.T) {
	t.Parallel()

	vendor := AgentVendorCodex
	model := "gpt-5"
	cfg := mustDefaultConfig(t)
	cfg.Agent.Vendor = &vendor
	cfg.Agent.Model = &model

	var first ResolvedAgent
	for i, role := range []string{CodingRolePlanner, CodingRoleWorker, CodingRoleReviewer, CodingRoleFixer} {
		got, ok := ResolveAgent(cfg, "", role)
		if !ok {
			t.Fatalf("ResolveAgent(%q) ok=false, want true", role)
		}
		if got.Vendor != vendor {
			t.Fatalf("ResolveAgent(%q).Vendor = %q, want %q", role, got.Vendor, vendor)
		}
		if got.Model == nil || *got.Model != model {
			t.Fatalf("ResolveAgent(%q).Model = %v, want %q", role, got.Model, model)
		}
		if got.ProfileID != "" {
			t.Fatalf("ResolveAgent(%q).ProfileID = %q, want empty", role, got.ProfileID)
		}
		if i == 0 {
			first = got
			continue
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("ResolveAgent(%q) = %#v, want same as planner %#v", role, got, first)
		}
	}
}

func TestResolveAgent_RoleInlineOverride(t *testing.T) {
	t.Parallel()

	globalVendor := AgentVendorCodex
	globalModel := "global-model"
	roleVendor := AgentVendorClaudeCode
	roleModel := "role-model"
	cfg := mustDefaultConfig(t)
	cfg.Agent.Vendor = &globalVendor
	cfg.Agent.Model = &globalModel
	cfg.Roles.Worker.Agent = &RoleAgentConfig{
		Vendor: &roleVendor,
		Model:  &roleModel,
	}

	got, ok := ResolveAgent(cfg, "", CodingRoleWorker)
	if !ok {
		t.Fatal("ResolveAgent(worker) ok=false, want true")
	}
	if got.Vendor != roleVendor {
		t.Fatalf("Vendor = %q, want %q", got.Vendor, roleVendor)
	}
	if got.Model == nil || *got.Model != roleModel {
		t.Fatalf("Model = %v, want %q", got.Model, roleModel)
	}

	planner, ok := ResolveAgent(cfg, "", CodingRolePlanner)
	if !ok {
		t.Fatal("ResolveAgent(planner) ok=false, want true")
	}
	if planner.Vendor != globalVendor || planner.Model == nil || *planner.Model != globalModel {
		t.Fatalf("planner should inherit global, got vendor=%q model=%v", planner.Vendor, planner.Model)
	}
}

func TestResolveAgent_ProfileBasePlusInlineModel(t *testing.T) {
	t.Parallel()

	globalVendor := AgentVendorCodex
	globalModel := "global-model"
	profileVendor := AgentVendorOpenCode
	profileModel := "profile-model"
	inlineModel := "inline-model"
	cfg := mustDefaultConfig(t)
	cfg.Agent.Vendor = &globalVendor
	cfg.Agent.Model = &globalModel
	cfg.Agent.Profiles = map[string]AgentBindingConfig{
		"fast": {
			Vendor: &profileVendor,
			Model:  &profileModel,
		},
	}
	cfg.Roles.Reviewer.Agent = &RoleAgentConfig{
		Profile: stringPtr("fast"),
		Model:   &inlineModel,
	}

	got, ok := ResolveAgent(cfg, "", CodingRoleReviewer)
	if !ok {
		t.Fatal("ResolveAgent(reviewer) ok=false, want true")
	}
	if got.Vendor != profileVendor {
		t.Fatalf("Vendor = %q, want profile vendor %q", got.Vendor, profileVendor)
	}
	if got.Model == nil || *got.Model != inlineModel {
		t.Fatalf("Model = %v, want inline %q", got.Model, inlineModel)
	}
	if got.ProfileID != "fast" {
		t.Fatalf("ProfileID = %q, want fast", got.ProfileID)
	}
}

func TestResolveAgent_ProfileOnlyOnOneRole(t *testing.T) {
	t.Parallel()

	globalVendor := AgentVendorCodex
	globalModel := "global-model"
	profileVendor := AgentVendorCursorCLI
	cfg := mustDefaultConfig(t)
	cfg.Agent.Vendor = &globalVendor
	cfg.Agent.Model = &globalModel
	cfg.Agent.Profiles = map[string]AgentBindingConfig{
		"cursor": {Vendor: &profileVendor},
	}
	cfg.Roles.Fixer.Agent = &RoleAgentConfig{Profile: stringPtr("cursor")}

	fixer, ok := ResolveAgent(cfg, "", CodingRoleFixer)
	if !ok {
		t.Fatal("ResolveAgent(fixer) ok=false")
	}
	if fixer.Vendor != profileVendor {
		t.Fatalf("fixer Vendor = %q, want %q", fixer.Vendor, profileVendor)
	}
	if fixer.Model == nil || *fixer.Model != globalModel {
		t.Fatalf("fixer should keep global model, got %v", fixer.Model)
	}

	worker, ok := ResolveAgent(cfg, "", CodingRoleWorker)
	if !ok {
		t.Fatal("ResolveAgent(worker) ok=false")
	}
	if worker.Vendor != globalVendor || worker.Model == nil || *worker.Model != globalModel {
		t.Fatalf("worker should inherit global, got vendor=%q model=%v", worker.Vendor, worker.Model)
	}
	if worker.ProfileID != "" {
		t.Fatalf("worker ProfileID = %q, want empty", worker.ProfileID)
	}
}

func TestResolveAgent_MissingVendorOkFalse(t *testing.T) {
	t.Parallel()

	roleVendor := AgentVendorClaudeCode
	cfg := mustDefaultConfig(t)
	// Global vendor nil; only worker has vendor.
	cfg.Roles.Worker.Agent = &RoleAgentConfig{Vendor: &roleVendor}

	if _, ok := ResolveAgent(cfg, "", CodingRolePlanner); ok {
		t.Fatal("planner should not resolve without vendor")
	}
	got, ok := ResolveAgent(cfg, "", CodingRoleWorker)
	if !ok {
		t.Fatal("worker should resolve with role vendor")
	}
	if got.Vendor != roleVendor {
		t.Fatalf("worker Vendor = %q, want %q", got.Vendor, roleVendor)
	}
}

func TestResolveAgent_EmptyModelSuppressesInherited(t *testing.T) {
	t.Parallel()

	globalVendor := AgentVendorCodex
	globalModel := "global-model"
	cfg := mustDefaultConfig(t)
	cfg.Agent.Vendor = &globalVendor
	cfg.Agent.Model = &globalModel
	cfg.Roles.Planner.Agent = &RoleAgentConfig{Model: stringPtr("")}

	got, ok := ResolveAgent(cfg, "", CodingRolePlanner)
	if !ok {
		t.Fatal("ResolveAgent(planner) ok=false")
	}
	if got.Vendor != globalVendor {
		t.Fatalf("Vendor = %q, want %q", got.Vendor, globalVendor)
	}
	if got.Model != nil {
		t.Fatalf("Model = %v, want nil after empty-string suppress", got.Model)
	}
}

func TestResolveAgent_ProjectIDDoesNotChangeResolve(t *testing.T) {
	t.Parallel()

	globalVendor := AgentVendorCodex
	projectVendor := AgentVendorClaudeCode
	cfg := mustDefaultConfig(t)
	cfg.Agent.Vendor = &globalVendor
	cfg.Projects = []ProjectRefConfig{{
		ID:       "demo",
		Name:     "Demo",
		RepoPath: "/repos/demo",
		Roles: &PartialRoleConfigs{
			Worker: &PartialWorkerRoleConfig{
				Agent: &RoleAgentConfig{Vendor: &projectVendor},
			},
		},
	}}

	global, ok := ResolveAgent(cfg, "", CodingRoleWorker)
	if !ok {
		t.Fatal("ResolveAgent global ok=false")
	}
	project, ok := ResolveAgent(cfg, "demo", CodingRoleWorker)
	if !ok {
		t.Fatal("ResolveAgent project ok=false")
	}
	if !reflect.DeepEqual(global, project) {
		t.Fatalf("projectID must not change resolve: global=%#v project=%#v", global, project)
	}
	if project.Vendor != globalVendor {
		t.Fatalf("project agent must not apply: got %q want %q", project.Vendor, globalVendor)
	}
}

func TestProjectRoleConfigs_DoesNotApplyProjectAgent(t *testing.T) {
	t.Parallel()

	globalVendor := AgentVendorCodex
	projectVendor := AgentVendorOpenCode
	cfg := mustDefaultConfig(t)
	cfg.Agent.Vendor = &globalVendor
	cfg.Roles.Worker.Agent = &RoleAgentConfig{Vendor: &globalVendor}
	falseValue := false
	cfg.Projects = []ProjectRefConfig{{
		ID:       "demo",
		Name:     "Demo",
		RepoPath: "/repos/demo",
		Roles: &PartialRoleConfigs{
			Worker: &PartialWorkerRoleConfig{
				AutoDiscovery: &falseValue,
				Agent:         &RoleAgentConfig{Vendor: &projectVendor},
			},
		},
	}}

	roles := ProjectRoleConfigs(cfg, "demo")
	if roles.Worker.AutoDiscovery {
		t.Fatal("project autoDiscovery override should still apply")
	}
	if roles.Worker.Agent == nil || roles.Worker.Agent.Vendor == nil || *roles.Worker.Agent.Vendor != globalVendor {
		t.Fatalf("project agent must not merge; got %#v", roles.Worker.Agent)
	}

	got, ok := ResolveAgent(cfg, "demo", CodingRoleWorker)
	if !ok {
		t.Fatal("ResolveAgent ok=false")
	}
	if got.Vendor != globalVendor {
		t.Fatalf("ResolveAgent vendor = %q, want global %q", got.Vendor, globalVendor)
	}
}

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

func TestNormalize_EmptyRoleAgentBecomesNil(t *testing.T) {
	t.Parallel()

	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Roles: &PartialRoleConfigs{
			Planner: &PartialPlannerRoleConfig{
				Agent: &RoleAgentConfig{Profile: stringPtr("   ")},
			},
			Worker: &PartialWorkerRoleConfig{
				Agent: &RoleAgentConfig{},
			},
		},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if cfg.Roles.Planner.Agent != nil {
		t.Fatalf("planner agent = %#v, want nil after empty canonicalize", cfg.Roles.Planner.Agent)
	}
	if cfg.Roles.Worker.Agent != nil {
		t.Fatalf("worker agent = %#v, want nil after empty canonicalize", cfg.Roles.Worker.Agent)
	}
}

func TestNormalize_RoleAgentProfileAndModelMerge(t *testing.T) {
	t.Parallel()

	baseVendor := AgentVendorCodex
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Agent: &PartialAgentConfig{
			Vendor: &baseVendor,
			Profiles: map[string]AgentBindingConfig{
				"fast": {Vendor: &baseVendor},
			},
		},
		Roles: &PartialRoleConfigs{
			Worker: &PartialWorkerRoleConfig{
				Agent: &RoleAgentConfig{
					Profile: stringPtr("fast"),
					Model:   stringPtr("worker-model"),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if cfg.Agent.Profiles["fast"].Vendor == nil || *cfg.Agent.Profiles["fast"].Vendor != baseVendor {
		t.Fatalf("profile vendor not set: %#v", cfg.Agent.Profiles["fast"])
	}
	if cfg.Roles.Worker.Agent == nil || cfg.Roles.Worker.Agent.Profile == nil || *cfg.Roles.Worker.Agent.Profile != "fast" {
		t.Fatalf("worker agent profile not merged: %#v", cfg.Roles.Worker.Agent)
	}
	if cfg.Roles.Worker.Agent.Model == nil || *cfg.Roles.Worker.Agent.Model != "worker-model" {
		t.Fatalf("worker agent model not merged: %#v", cfg.Roles.Worker.Agent)
	}
}

func TestAnyCodingRoleAgentConfigured_RoleOnlyVendor(t *testing.T) {
	t.Parallel()

	roleVendor := AgentVendorOpenCode
	cfg := mustDefaultConfig(t)
	// Global vendor nil.
	cfg.Roles.Worker.Agent = &RoleAgentConfig{Vendor: &roleVendor}

	if !AnyCodingRoleAgentConfigured(cfg) {
		t.Fatal("AnyCodingRoleAgentConfigured = false, want true when worker has vendor")
	}
	if !CodingRoleAgentConfigured(cfg, CodingRoleWorker) {
		t.Fatal("CodingRoleAgentConfigured(worker) = false, want true")
	}
	if CodingRoleAgentConfigured(cfg, CodingRolePlanner) {
		t.Fatal("CodingRoleAgentConfigured(planner) = true, want false")
	}
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

func TestResolveAgent_ProfileEmptyModelSuppressesInherited(t *testing.T) {
	t.Parallel()

	globalVendor := AgentVendorCodex
	globalModel := "global-model"
	cfg := mustDefaultConfig(t)
	cfg.Agent.Vendor = &globalVendor
	cfg.Agent.Model = &globalModel
	cfg.Agent.Profiles = map[string]AgentBindingConfig{
		"suppress": {Model: stringPtr("")},
	}
	cfg.Roles.Reviewer.Agent = &RoleAgentConfig{Profile: stringPtr("suppress")}

	got, ok := ResolveAgent(cfg, "", CodingRoleReviewer)
	if !ok {
		t.Fatal("ResolveAgent(reviewer) ok=false, want true")
	}
	if got.Vendor != globalVendor {
		t.Fatalf("Vendor = %q, want %q", got.Vendor, globalVendor)
	}
	if got.Model != nil {
		t.Fatalf("Model = %v, want nil after profile empty-string suppress", got.Model)
	}
	if got.ProfileID != "suppress" {
		t.Fatalf("ProfileID = %q, want suppress", got.ProfileID)
	}
}

func TestResolveAgent_UnknownAndCoordinatorRolesFailClosed(t *testing.T) {
	t.Parallel()

	vendor := AgentVendorCodex
	cfg := mustDefaultConfig(t)
	cfg.Agent.Vendor = &vendor

	for _, role := range []string{"coordinator", "unknown", ""} {
		got, ok := ResolveAgent(cfg, "", role)
		if ok {
			t.Fatalf("ResolveAgent(%q) ok=true, want false", role)
		}
		if got != (ResolvedAgent{}) {
			t.Fatalf("ResolveAgent(%q) = %#v, want zero value", role, got)
		}
		if CodingRoleAgentConfigured(cfg, role) {
			t.Fatalf("CodingRoleAgentConfigured(%q) = true, want false", role)
		}
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

func TestNormalize_EmptyProjectRoleAgentBecomesNil(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Projects: &[]PartialProjectRefConfig{{
			ID:       "demo",
			Name:     "Demo",
			RepoPath: repo,
			Roles: &PartialRoleConfigs{
				Planner:  &PartialPlannerRoleConfig{Agent: &RoleAgentConfig{}},
				Worker:   &PartialWorkerRoleConfig{Agent: &RoleAgentConfig{}},
				Reviewer: &PartialReviewerRoleConfig{Agent: &RoleAgentConfig{}},
				Fixer:    &PartialFixerRoleConfig{Agent: &RoleAgentConfig{}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0].Roles == nil {
		t.Fatalf("projects = %#v", cfg.Projects)
	}
	roles := cfg.Projects[0].Roles
	if roles.Planner == nil || roles.Planner.Agent != nil {
		t.Fatalf("planner.agent = %#v, want nil", roles.Planner)
	}
	if roles.Worker == nil || roles.Worker.Agent != nil {
		t.Fatalf("worker.agent = %#v, want nil", roles.Worker)
	}
	if roles.Reviewer == nil || roles.Reviewer.Agent != nil {
		t.Fatalf("reviewer.agent = %#v, want nil", roles.Reviewer)
	}
	if roles.Fixer == nil || roles.Fixer.Agent != nil {
		t.Fatalf("fixer.agent = %#v, want nil", roles.Fixer)
	}
	if err := ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()}); err != nil {
		t.Fatalf("empty project agent objects should validate after canonicalize: %v", err)
	}
}

func TestNormalize_AgentProfilesFieldOverlayByKey(t *testing.T) {
	t.Parallel()

	baseVendor := AgentVendorCodex
	baseModel := "base"
	overlayModel := "overlay"
	cfg, err := Normalize(t.TempDir(),
		PartialConfig{Agent: &PartialAgentConfig{
			Profiles: map[string]AgentBindingConfig{
				"fast": {Vendor: &baseVendor, Model: &baseModel},
			},
		}},
		PartialConfig{Agent: &PartialAgentConfig{
			Profiles: map[string]AgentBindingConfig{
				"fast": {Model: &overlayModel},
			},
		}},
	)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	binding := cfg.Agent.Profiles["fast"]
	if binding.Vendor == nil || *binding.Vendor != baseVendor {
		t.Fatalf("vendor should remain after key overlay: %#v", binding)
	}
	if binding.Model == nil || *binding.Model != overlayModel {
		t.Fatalf("model should overlay: %#v", binding)
	}
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
