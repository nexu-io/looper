package config

import "testing"

func TestDefaultConfigDisablesWebhookMode(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	if cfg.Webhook.Enabled {
		t.Fatal("DefaultConfig().Webhook.Enabled = true, want false")
	}
	if cfg.Webhook.FallbackPollIntervalSeconds != 300 {
		t.Fatalf("DefaultConfig().Webhook.FallbackPollIntervalSeconds = %d, want 300", cfg.Webhook.FallbackPollIntervalSeconds)
	}
	if cfg.Webhook.Mode != WebhookModeGHForward {
		t.Fatalf("DefaultConfig().Webhook.Mode = %q, want %q", cfg.Webhook.Mode, WebhookModeGHForward)
	}
	if cfg.Webhook.ListenPort != 0 {
		t.Fatalf("DefaultConfig().Webhook.ListenPort = %d, want 0", cfg.Webhook.ListenPort)
	}
	if cfg.Webhook.PublicBaseURL != "" {
		t.Fatalf("DefaultConfig().Webhook.PublicBaseURL = %q, want empty", cfg.Webhook.PublicBaseURL)
	}
}

func TestValidateRejectsWebhookFallbackBelowMinimum(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Webhook.FallbackPollIntervalSeconds = 59
	err = Validate(cfg)
	if err == nil {
		t.Fatal("Validate() error = nil, want validation error")
	}
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("Validate() error = %T, want *ConfigValidationError", err)
	}
	if len(validationErr.Issues) != 1 || validationErr.Issues[0].Path != "webhook.fallbackPollIntervalSeconds" {
		t.Fatalf("Validate() issues = %#v, want webhook fallback issue", validationErr.Issues)
	}
}

func TestValidateRejectsUnsupportedWebhookModes(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Webhook.Mode = WebhookMode("invalid")
	cfg.Projects = []ProjectRefConfig{{ID: "p1", Name: "P1", RepoPath: t.TempDir(), Webhook: ProjectWebhookConfig{Mode: WebhookMode("bad")}}}
	err = Validate(cfg)
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("Validate() error = %T, want *ConfigValidationError", err)
	}
	if len(validationErr.Issues) != 2 {
		t.Fatalf("Validate() issues = %#v, want 2 mode issues", validationErr.Issues)
	}
}

func TestValidateRequiresTunnelSettingsForGlobalTunnelMode(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Webhook.Enabled = true
	cfg.Webhook.Mode = WebhookModeTunnel
	err = Validate(cfg)
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("Validate() error = %T, want *ConfigValidationError", err)
	}
	if len(validationErr.Issues) != 2 {
		t.Fatalf("Validate() issues = %#v, want tunnel listen/public issues", validationErr.Issues)
	}
}

func TestValidateRequiresTunnelSettingsForProjectTunnelOverride(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Webhook.Enabled = true
	cfg.Projects = []ProjectRefConfig{{ID: "p1", Name: "P1", RepoPath: t.TempDir(), Webhook: ProjectWebhookConfig{Mode: WebhookModeTunnel}}}
	err = Validate(cfg)
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("Validate() error = %T, want *ConfigValidationError", err)
	}
	if len(validationErr.Issues) != 2 {
		t.Fatalf("Validate() issues = %#v, want tunnel listen/public issues", validationErr.Issues)
	}
}

func TestValidateRejectsHostlessTunnelPublicBaseURL(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Webhook.Enabled = true
	cfg.Webhook.Mode = WebhookModeTunnel
	cfg.Webhook.ListenPort = 8443
	cfg.Webhook.PublicBaseURL = "https://"

	err = Validate(cfg)
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("Validate() error = %T, want *ConfigValidationError", err)
	}
	if len(validationErr.Issues) != 1 || validationErr.Issues[0].Path != "webhook.publicBaseUrl" {
		t.Fatalf("Validate() issues = %#v, want webhook publicBaseUrl issue", validationErr.Issues)
	}
}

func TestValidateRejectsTunnelPublicBaseURLWithQueryOrFragment(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		baseURL string
	}{
		{name: "query", baseURL: "https://hooks.example.test?t=dev"},
		{name: "fragment", baseURL: "https://hooks.example.test#dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatalf("DefaultConfig() error = %v", err)
			}
			cfg.Webhook.Enabled = true
			cfg.Webhook.Mode = WebhookModeTunnel
			cfg.Webhook.ListenPort = 8443
			cfg.Webhook.PublicBaseURL = tc.baseURL

			err = Validate(cfg)
			validationErr, ok := err.(*ConfigValidationError)
			if !ok {
				t.Fatalf("Validate() error = %T, want *ConfigValidationError", err)
			}
			if len(validationErr.Issues) != 1 || validationErr.Issues[0].Path != "webhook.publicBaseUrl" {
				t.Fatalf("Validate() issues = %#v, want webhook publicBaseUrl issue", validationErr.Issues)
			}
		})
	}
}

func TestNormalizeMergesPartialWebhookTunnelFields(t *testing.T) {
	t.Parallel()
	mode := WebhookModeTunnel
	port := 8443
	baseURL := "https://hooks.example.test"
	projectMode := WebhookModeTunnel
	cfg, err := Normalize(t.TempDir(), PartialConfig{Webhook: &PartialWebhookConfig{Mode: &mode, ListenPort: &port, PublicBaseURL: &baseURL}, Projects: &[]PartialProjectRefConfig{{ID: "p1", Name: "P1", RepoPath: "/tmp/repo", Webhook: &PartialProjectWebhookConfig{Mode: &projectMode}}}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if cfg.Webhook.Mode != WebhookModeTunnel || cfg.Webhook.ListenPort != 8443 || cfg.Webhook.PublicBaseURL != baseURL {
		t.Fatalf("Normalize() webhook = %#v", cfg.Webhook)
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0].Webhook.Mode != WebhookModeTunnel {
		t.Fatalf("Normalize() projects = %#v", cfg.Projects)
	}
}
