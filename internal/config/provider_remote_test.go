package config

import (
	"strings"
	"testing"
)

func TestResolveProviderRef(t *testing.T) {
	cfg := Config{
		Providers: []ProviderConfig{
			{ID: "forgejo-main", Kind: ProviderKindForgejo, BaseURL: "https://code.powerformer.net"},
			{ID: "plane-main", Kind: ProviderKindPlane},
		},
	}

	id, ok, err := ResolveProviderRef(cfg, "forgejo-main")
	if err != nil || !ok || id != "forgejo-main" {
		t.Fatalf("ResolveProviderRef(id) = (%q, %v, %v), want forgejo-main", id, ok, err)
	}

	id, ok, err = ResolveProviderRef(cfg, "forgejo")
	if err != nil || !ok || id != "forgejo-main" {
		t.Fatalf("ResolveProviderRef(kind) = (%q, %v, %v), want forgejo-main", id, ok, err)
	}

	id, ok, err = ResolveProviderRef(cfg, "github")
	if err != nil || ok || id != "" {
		t.Fatalf("ResolveProviderRef(github) = (%q, %v, %v), want empty", id, ok, err)
	}

	if _, _, err := ResolveProviderRef(cfg, "missing"); err == nil {
		t.Fatal("ResolveProviderRef(missing) error = nil, want error")
	}

	cfg.Providers = append(cfg.Providers, ProviderConfig{ID: "forgejo-other", Kind: ProviderKindForgejo, BaseURL: "https://other.example"})
	if _, _, err := ResolveProviderRef(cfg, "forgejo"); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("ResolveProviderRef(ambiguous kind) error = %v, want multiple", err)
	}
}

func TestMatchForgejoProviderByRemoteHost(t *testing.T) {
	cfg := Config{
		Providers: []ProviderConfig{
			{ID: "forgejo-main", Kind: ProviderKindForgejo, BaseURL: "https://code.powerformer.net"},
			{ID: "github-default", Kind: ProviderKindGitHub, BaseURL: "https://github.com"},
		},
	}

	provider, ok := MatchForgejoProviderByRemoteHost(cfg, "ssh.code.powerformer.net")
	if !ok || provider.ID != "forgejo-main" {
		t.Fatalf("MatchForgejoProviderByRemoteHost(ssh host) = (%q, %v), want forgejo-main", provider.ID, ok)
	}

	provider, ok = MatchForgejoProviderByRemoteHost(cfg, "code.powerformer.net")
	if !ok || provider.ID != "forgejo-main" {
		t.Fatalf("MatchForgejoProviderByRemoteHost(api host) = (%q, %v), want forgejo-main", provider.ID, ok)
	}

	if _, ok := MatchForgejoProviderByRemoteHost(cfg, "github.com"); ok {
		t.Fatal("MatchForgejoProviderByRemoteHost(github.com) matched, want no match")
	}
	if _, ok := MatchForgejoProviderByRemoteHost(cfg, "other.example"); ok {
		t.Fatal("MatchForgejoProviderByRemoteHost(unknown) matched, want no match")
	}
}

func TestMatchForgejoProviderByRemoteHostAmbiguous(t *testing.T) {
	cfg := Config{
		Providers: []ProviderConfig{
			{ID: "a", Kind: ProviderKindForgejo, BaseURL: "https://code.example.com"},
			{ID: "b", Kind: ProviderKindForgejo, BaseURL: "https://code.example.com"},
		},
	}
	if _, ok := MatchForgejoProviderByRemoteHost(cfg, "code.example.com"); ok {
		t.Fatal("ambiguous providers should not match")
	}
}

func TestUpsertRuntimeProjectBindingAppliesForgejoProfile(t *testing.T) {
	cfg := Config{
		Providers: []ProviderConfig{
			{ID: "forgejo-main", Kind: ProviderKindForgejo, BaseURL: "https://code.example.com"},
		},
	}
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	UpsertRuntimeProjectBinding(&cfg, "odcrew", "odcrew", "forgejo-main", "core/odcrew", "/tmp/odcrew")
	if len(cfg.Projects) != 1 {
		t.Fatalf("Projects len = %d, want 1", len(cfg.Projects))
	}
	project := cfg.Projects[0]
	if project.Provider != "forgejo-main" || project.Repo != "core/odcrew" {
		t.Fatalf("project = %#v, want forgejo binding", project)
	}
	if project.Roles == nil || project.Roles.Reviewer == nil || project.Roles.Reviewer.Discovery == nil || project.Roles.Reviewer.Discovery.Triggers == nil {
		t.Fatalf("forgejo profile roles missing: %#v", project.Roles)
	}
	if project.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest == nil || *project.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest {
		t.Fatalf("RequireReviewRequest = %#v, want false", project.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest)
	}
	effectiveRoles := ProjectRoleConfigs(cfg, "odcrew")
	if effectiveRoles.Coordinator.Enabled {
		t.Fatal("runtime forgejo coordinator enabled = true, want false")
	}
	if effectiveRoles.Coordinator.Dependencies.Enabled {
		t.Fatal("runtime forgejo coordinator dependencies enabled = true, want false")
	}
	// Runtime projects are updated when the API registers the same ID again.
	UpsertRuntimeProjectBinding(&cfg, "odcrew", "other", "forgejo-main", "core/other", "/tmp/other")
	if cfg.Projects[0].Repo != "core/other" {
		t.Fatalf("runtime project was not updated: %#v", cfg.Projects[0])
	}
}

func TestUpsertRuntimeProjectBindingKeepsConfigFileProjectAuthority(t *testing.T) {
	cfg := Config{
		Providers: []ProviderConfig{
			{ID: "forgejo-main", Kind: ProviderKindForgejo, BaseURL: "https://code.example.com"},
		},
		Projects: []ProjectRefConfig{{
			ID:       "odcrew",
			Name:     "Config file",
			Provider: "forgejo-main",
			Repo:     "core/configured",
			RepoPath: "/tmp/configured",
		}},
	}

	UpsertRuntimeProjectBinding(&cfg, "odcrew", "runtime", "forgejo-main", "core/runtime", "/tmp/runtime")

	if len(cfg.Projects) != 1 {
		t.Fatalf("Projects len = %d, want 1", len(cfg.Projects))
	}
	if cfg.Projects[0].Repo != "core/configured" || cfg.Projects[0].RepoPath != "/tmp/configured" {
		t.Fatalf("config project was overwritten: %#v", cfg.Projects[0])
	}
}

func TestUpsertRuntimeProjectBindingRejectsDuplicateRepo(t *testing.T) {
	cfg := Config{Projects: []ProjectRefConfig{{
		ID: "configured", Provider: "forgejo-a", Repo: "Core/Looper", RepoPath: "/tmp/configured",
	}}}

	UpsertRuntimeProjectBinding(&cfg, "runtime", "runtime", "forgejo-b", "core/looper", "/tmp/runtime")

	if len(cfg.Projects) != 1 {
		t.Fatalf("Projects = %#v, want duplicate runtime repo rejected", cfg.Projects)
	}
}

func TestValidateRuntimeProjectBindingRejectsDuplicateRepo(t *testing.T) {
	cfg := Config{Projects: []ProjectRefConfig{{ID: "configured", Repo: "Core/Looper"}}}

	err := ValidateRuntimeProjectBinding(cfg, "runtime", "core/looper")

	if err == nil || !strings.Contains(err.Error(), "already bound to project configured") {
		t.Fatalf("ValidateRuntimeProjectBinding() error = %v, want duplicate repo error", err)
	}
}

func TestUpsertRuntimeProjectBindingRemovesRuntimeBindingWhenProviderIsCleared(t *testing.T) {
	cfg := Config{}
	UpsertRuntimeProjectBinding(&cfg, "odcrew", "odcrew", "forgejo-main", "core/odcrew", "/tmp/odcrew")

	UpsertRuntimeProjectBinding(&cfg, "odcrew", "odcrew", "", "core/odcrew", "/tmp/odcrew")

	if len(cfg.Projects) != 0 {
		t.Fatalf("Projects = %#v, want runtime binding removed", cfg.Projects)
	}
	if cfg.hasRuntimeProjectBinding("odcrew") {
		t.Fatal("runtime binding marker remains after provider was cleared")
	}
}

func TestUpsertRuntimeProjectBindingDoesNotRemoveConfigFileProject(t *testing.T) {
	cfg := Config{Projects: []ProjectRefConfig{{ID: "odcrew", Provider: "forgejo-main"}}}

	UpsertRuntimeProjectBinding(&cfg, "odcrew", "odcrew", "", "core/odcrew", "/tmp/odcrew")

	if len(cfg.Projects) != 1 || cfg.Projects[0].Provider != "forgejo-main" {
		t.Fatalf("Projects = %#v, want config-file project preserved", cfg.Projects)
	}
}
