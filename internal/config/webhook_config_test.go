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
