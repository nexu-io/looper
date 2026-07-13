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
