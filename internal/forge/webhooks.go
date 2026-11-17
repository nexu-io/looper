package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
)

// RepositoryHook is the common managed repository-hook response. Forgejo does
// not expose the signing secret or an insecure_ssl setting on reads.
type RepositoryHook struct {
	ID           int64    `json:"id"`
	Type         string   `json:"type,omitempty"`
	Active       bool     `json:"active"`
	Events       []string `json:"events"`
	BranchFilter string   `json:"branch_filter,omitempty"`
	Config       struct {
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
		InsecureSSL string `json:"insecure_ssl"`
		Secret      string `json:"secret"`
	} `json:"config"`
	LastResponse *struct {
		Code int `json:"code"`
	} `json:"last_response"`
}

// ForgejoWebhookEvents includes the expanded event set returned by Forgejo for
// the issues/pull_request subscriptions. Comparing only the umbrella names
// would cause an unnecessary PATCH on every reconciliation.
func ForgejoWebhookEvents() []string {
	return []string{
		"issues", "issue_assign", "issue_label", "issue_milestone", "issue_comment",
		"pull_request", "pull_request_assign", "pull_request_label", "pull_request_milestone",
		"pull_request_comment", "pull_request_review_approved", "pull_request_review_rejected",
		"pull_request_review_comment", "pull_request_sync", "pull_request_review_request",
		"push", "action_run_success", "action_run_failure", "action_run_recover",
	}
}

// NewForgejoWebhookClient resolves a managed record's full repository URL
// against configured providers. Project defaults own repository administration;
// role-specific identities remain scoped to their role's discovery and runs.
// Removed projects can still clean up their orphaned hooks with provider auth.
func NewForgejoWebhookClient(ctx context.Context, cfg config.Config, key string, options ...ForgejoOption) (*ForgejoClient, error) {
	// Mutations carry the signing secret. Direct HTTP must use the configured
	// endpoint without forwarding that body to a redirect destination.
	options = append(options, func(client *ForgejoClient) {
		copy := *client.httpClient
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client.httpClient = &copy
	})
	for _, provider := range cfg.Providers {
		if provider.Kind != config.ProviderKindForgejo {
			continue
		}
		base := strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
		repo, ok := strings.CutPrefix(key, base+"/")
		if !ok || strings.Count(repo, "/") != 1 {
			continue
		}
		if project, found := config.WebhookProject(cfg, key); found {
			if project.Provider != provider.ID {
				continue
			}
			if project.Identity != "" {
				definition, exists := cfg.Identities[project.Identity]
				if !exists {
					return nil, fmt.Errorf("webhook identity %q is not configured", project.Identity)
				}
				target, _ := config.ProjectRepositoryIdentity(cfg, project)
				bound, err := hostingidentity.BindResolved(ctx, config.ResolvedHostingIdentity{Name: project.Identity, Definition: definition, Target: target, ProjectID: project.ID, Role: "webhook"})
				if err != nil {
					return nil, err
				}
				return NewForgejoClientForContext(bound, provider, repo, options...)
			}
		}
		return NewForgejoClientFromConfig(provider, repo, options...)
	}
	return nil, fmt.Errorf("no configured Forgejo provider for webhook repository %q", key)
}

func (c *ForgejoClient) GetWebhook(ctx context.Context, id int64) (RepositoryHook, bool, error) {
	var hook RepositoryHook
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("repos/%s/hooks/%d", c.repo.Repo, id), nil, nil, &hook)
	var apiErr *ForgejoHTTPError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return RepositoryHook{}, false, nil
	}
	return hook, err == nil, err
}

func (c *ForgejoClient) CreateWebhook(ctx context.Context, targetURL, secret string) (RepositoryHook, error) {
	var hook RepositoryHook
	body := map[string]any{
		"type": "forgejo", "active": true, "events": ForgejoWebhookEvents(),
		"config": map[string]string{"url": targetURL, "content_type": "json", "secret": secret},
	}
	err := c.do(ctx, http.MethodPost, "repos/"+c.repo.Repo+"/hooks", nil, body, &hook)
	if err != nil && secret != "" && strings.Contains(err.Error(), secret) {
		var apiErr *ForgejoHTTPError
		if errors.As(err, &apiErr) {
			redacted := *apiErr
			redacted.Message = strings.ReplaceAll(redacted.Message, secret, "[REDACTED]")
			return hook, &redacted
		}
		return hook, errors.New(strings.ReplaceAll(err.Error(), secret, "[REDACTED]"))
	}
	if err == nil && hook.ID <= 0 {
		return hook, errors.New("Forgejo create webhook returned no hook ID")
	}
	return hook, err
}

// UpdateWebhook deliberately leaves the secret alone: Forgejo's EditHookOption
// does not update the stored secret. Rotation creates a replacement hook.
func (c *ForgejoClient) UpdateWebhook(ctx context.Context, id int64, targetURL string, active bool) (RepositoryHook, error) {
	var hook RepositoryHook
	body := map[string]any{"active": active, "events": ForgejoWebhookEvents(), "config": map[string]string{"url": targetURL, "content_type": "json"}}
	err := c.do(ctx, http.MethodPatch, fmt.Sprintf("repos/%s/hooks/%d", c.repo.Repo, id), nil, body, &hook)
	return hook, err
}

func (c *ForgejoClient) DeleteWebhook(ctx context.Context, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("repos/%s/hooks/%d", c.repo.Repo, id), nil, nil, nil)
	var apiErr *ForgejoHTTPError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}
