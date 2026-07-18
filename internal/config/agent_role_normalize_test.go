package config

import (
	"testing"
)

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
