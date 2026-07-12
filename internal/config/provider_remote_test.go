package config

import "testing"

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
	// Config-file projects keep authority.
	UpsertRuntimeProjectBinding(&cfg, "odcrew", "other", "forgejo-main", "core/other", "/tmp/other")
	if cfg.Projects[0].Repo != "core/odcrew" {
		t.Fatalf("config project was overwritten: %#v", cfg.Projects[0])
	}
}
