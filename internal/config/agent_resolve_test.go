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
	// Suppress stays a non-nil empty pointer so params filtering can strip
	// --model flags; nil would mean "unset" and preserve params-only models.
	if got.Model == nil || *got.Model != "" {
		t.Fatalf("Model = %v, want non-nil empty after empty-string suppress", got.Model)
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
	if got.Model == nil || *got.Model != "" {
		t.Fatalf("Model = %v, want non-nil empty after profile empty-string suppress", got.Model)
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
