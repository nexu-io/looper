package runtime

import (
	"context"
	"net/http"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
)

type forgejoWebhookTunnelClient struct{ client *forge.ForgejoClient }

func (c forgejoWebhookTunnelClient) GetHook(ctx context.Context, _ string, id int64) (webhookTunnelGitHubHook, bool, error) {
	return c.client.GetWebhook(ctx, id)
}
func (c forgejoWebhookTunnelClient) CreateHook(ctx context.Context, _, targetURL, secret string, _ []string) (webhookTunnelGitHubHook, error) {
	return c.client.CreateWebhook(ctx, targetURL, secret)
}
func (c forgejoWebhookTunnelClient) UpdateHook(ctx context.Context, _ string, id int64, targetURL, _ string, _ []string, active bool) (webhookTunnelGitHubHook, error) {
	return c.client.UpdateWebhook(ctx, id, targetURL, active)
}
func (c forgejoWebhookTunnelClient) DeleteHook(ctx context.Context, _ string, id int64) error {
	return c.client.DeleteWebhook(ctx, id)
}

func isForgejoWebhookRepository(cfg config.Config, key string) bool {
	project, ok := config.WebhookProject(cfg, key)
	return ok && config.ResolvedProjectProviderKind(cfg, project) == config.ProviderKindForgejo
}

func (w *webhookRuntime) tunnelClientForRepository(ctx context.Context, cfg config.Config, key string) (webhookTunnelGitHubClient, error) {
	if isForgejoWebhookRepository(cfg, key) {
		client, err := forge.NewForgejoWebhookClient(ctx, cfg, key)
		if err != nil {
			return nil, err
		}
		return forgejoWebhookTunnelClient{client: client}, nil
	}
	return w.tunnelGitHubClient(), nil
}

func webhookTunnelNeedsGH(cfg config.Config) bool {
	if len(cfg.Projects) == 0 {
		return cfg.Webhook.Mode == config.WebhookModeTunnel
	}
	for _, project := range cfg.Projects {
		if config.ProjectWebhookMode(cfg, project) == config.WebhookModeTunnel && config.ResolvedProjectProviderKind(cfg, project) != config.ProviderKindForgejo {
			return true
		}
	}
	return false
}

func forgejoWebhookTargetForPath(cfg config.Config, path string) (config.ProjectRefConfig, bool) {
	for _, project := range cfg.Projects {
		if config.ResolvedProjectProviderKind(cfg, project) == config.ProviderKindForgejo && config.ProjectWebhookMode(cfg, project) == config.WebhookModeTunnel {
			// Compare decoded paths, matching net/http's URL.Path semantics even
			// when an existing project ID needs path escaping.
			if path == "/webhook/forgejo/project/"+project.ID {
				return project, true
			}
		}
	}
	return config.ProjectRefConfig{}, false
}

func forgejoWebhookHeaders(headers http.Header, secret string, body []byte) (string, string, bool) {
	// Forgejo also sends GitHub-compatible headers. The authenticated target,
	// never a payload or header claiming a provider, selects the protocol.
	for _, prefix := range []string{"X-Forgejo-", "X-Gitea-"} {
		if signature := headers.Get(prefix + "Signature"); signature != "" {
			event := headers.Get(prefix + "Event-Type")
			if event == "" {
				event = headers.Get(prefix + "Event")
			}
			return headers.Get(prefix + "Delivery"), event, validGitHubSignature(secret, body, "sha256="+signature)
		}
	}
	return "", "", false
}
