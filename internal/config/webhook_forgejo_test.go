package config

import (
	"strings"
	"testing"
)

func TestForgejoWebhookModeAndIdentity(t *testing.T) {
	t.Parallel()
	cfg, err := Normalize(t.TempDir(), PartialConfig{
		Providers: &[]PartialProviderConfig{{ID: "fj", Kind: providerKindPtr(ProviderKindForgejo), BaseURL: stringPtr("https://code.example/team"), TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  &[]PartialProjectRefConfig{{ID: "fj", Name: "Forgejo", Provider: stringPtr("fj"), Repo: stringPtr("Acme/App"), RepoPath: "/tmp/fj"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := ProjectWebhookMode(cfg, cfg.Projects[0]); got != "" || WebhookNeedsGHForward(cfg) {
		t.Fatalf("Forgejo inherits polling from gh-forward, got %q", got)
	}
	if key := WebhookRepositoryKey(cfg, cfg.Projects[0]); key != "https://code.example/team/acme/app" {
		t.Fatalf("repository key = %q", key)
	}
	cfg.Webhook.Enabled = true
	cfg.Webhook.ListenPort = 8443
	cfg.Webhook.PublicBaseURL = "https://hooks.example/base"
	for _, global := range []bool{false, true} {
		candidate := cfg
		candidate.Projects = append([]ProjectRefConfig(nil), cfg.Projects...)
		if global {
			candidate.Webhook.Mode = WebhookModeTunnel
		} else {
			candidate.Projects[0].Webhook.Mode = WebhookModeTunnel
		}
		if err := Validate(candidate); err != nil {
			t.Fatalf("global=%t: %v", global, err)
		}
		if ProjectWebhookMode(candidate, candidate.Projects[0]) != WebhookModeTunnel || WebhookNeedsGHForward(candidate) {
			t.Fatalf("Forgejo tunnel must not need gh: %#v", candidate.Webhook)
		}
		candidate.Webhook.PublicBaseURL = ""
		if err := Validate(candidate); err == nil {
			t.Fatal("tunnel without public URL accepted")
		}
	}
	cfg.Projects[0].Webhook.Mode = WebhookModeGHForward
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "GitHub-only") {
		t.Fatalf("explicit gh-forward error = %v", err)
	}
}
