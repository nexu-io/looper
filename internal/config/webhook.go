package config

import "strings"

// ProjectWebhookMode preserves polling for Forgejo projects inheriting the
// GitHub-only forwarder. Forgejo opts into tunnel mode globally or per project.
func ProjectWebhookMode(cfg Config, project ProjectRefConfig) WebhookMode {
	if project.Webhook.Mode != "" {
		return project.Webhook.Mode
	}
	mode := cfg.Webhook.Mode
	if mode == "" {
		mode = WebhookModeGHForward
	}
	if ResolvedProjectProviderKind(cfg, project) == ProviderKindForgejo && mode == WebhookModeGHForward {
		return ""
	}
	return mode
}

// WebhookRepositoryKey keeps legacy GitHub records addressable by owner/repo.
// Forgejo records use the repository URL so identical slugs on different
// hosting instances cannot share hook IDs or signing secrets.
func WebhookRepositoryKey(cfg Config, project ProjectRefConfig) string {
	if identity, ok := ProjectRepositoryIdentity(cfg, project); ok && identity.Kind == ProviderKindForgejo {
		return strings.TrimRight(identity.BaseURL, "/") + "/" + identity.Repo
	}
	return strings.TrimSpace(project.Repo)
}

func WebhookProject(cfg Config, key string) (ProjectRefConfig, bool) {
	for _, project := range cfg.Projects {
		if WebhookRepositoryKey(cfg, project) == key {
			return project, true
		}
	}
	return ProjectRefConfig{}, false
}

func WebhookNeedsGHForward(cfg Config) bool {
	if len(cfg.Projects) == 0 {
		return cfg.Webhook.Mode == "" || cfg.Webhook.Mode == WebhookModeGHForward
	}
	for _, project := range cfg.Projects {
		if ProjectWebhookMode(cfg, project) == WebhookModeGHForward {
			return true
		}
	}
	return false
}
