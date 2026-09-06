package forge

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

func configuredForgejoProvider(cfg config.Config, projectID, repo string) (config.ProviderConfig, bool) {
	for _, project := range cfg.Projects {
		if project.ID != strings.TrimSpace(projectID) || !strings.EqualFold(strings.TrimSpace(project.Repo), strings.TrimSpace(repo)) || config.ResolvedProjectProviderKind(cfg, project) != config.ProviderKindForgejo {
			continue
		}
		for _, provider := range cfg.Providers {
			if provider.ID == project.Provider && provider.Kind == config.ProviderKindForgejo {
				return provider, true
			}
		}
	}
	return config.ProviderConfig{}, false
}

// ConfiguredPullRequestURL supplies older checkpoints with the configured
// Forgejo web URL. Fresh provider-returned HTML URLs take precedence at callers.
func ConfiguredPullRequestURL(cfg config.Config, projectID, repo string, prNumber int64) string {
	provider, ok := configuredForgejoProvider(cfg, projectID, repo)
	if !ok {
		return ""
	}
	base, err := url.Parse(provider.BaseURL)
	if err != nil || base.Host == "" {
		return ""
	}
	base.User, base.RawQuery, base.Fragment = nil, "", ""
	return base.JoinPath(repo, "pulls", fmt.Sprint(prNumber)).String()
}

// ForgejoAgentContext describes the configured transport without reading or
// copying credentials. Both runners use the same provider-specific read APIs.
func ForgejoAgentContext(cfg config.Config, projectID, repo string, prNumber int64) string {
	provider, ok := configuredForgejoProvider(cfg, projectID, repo)
	if !ok {
		return ""
	}
	prURL := ConfiguredPullRequestURL(cfg, projectID, repo, prNumber)
	base, err := url.Parse(provider.BaseURL)
	if err != nil || base.Host == "" {
		return ""
	}
	base.User, base.RawQuery, base.Fragment = nil, "", ""
	apiBase := strings.TrimRight(base.String(), "/") + "/api/v1"
	apiRepo := "/repos/" + strings.Trim(repo, "/")
	auth := ""
	if config.EffectiveProviderAuth(provider) == config.ProviderAuthTea {
		teaPath, login := "tea", ""
		if provider.TeaPath != nil && strings.TrimSpace(*provider.TeaPath) != "" {
			teaPath = strings.TrimSpace(*provider.TeaPath)
		}
		if provider.TeaLogin != nil {
			login = strings.TrimSpace(*provider.TeaLogin)
		}
		auth = fmt.Sprintf("Read API command: %s api --login %s -i <endpoint>. Use this explicit login. tea can exit zero on HTTP errors: inspect the HTTP status printed with -i and require 2xx.", agentContextShellQuote(teaPath), agentContextShellQuote(login))
	} else {
		envName := ""
		if provider.TokenEnv != nil {
			envName = strings.TrimSpace(*provider.TokenEnv)
		}
		auth = fmt.Sprintf("Authentication uses the configured environment variable %q. Read it only inside your HTTP client and set the Authorization header to token plus that value. Never print its value, dump the environment, or enable shell tracing. Require an HTTP 2xx response.", envName)
	}
	return strings.Join([]string{
		"Forgejo repository context: " + prURL + ". API base: " + apiBase + ".",
		auth,
		"Use the prepared local worktree for Git inspection and validation. Use Forgejo API reads for mutable PR context; gh commands target the wrong provider. Follow the role-specific publishing instructions for all writes.",
		fmt.Sprintf("PR metadata: GET %s/pulls/%d (head.sha, base.sha, head.ref, base.ref, state, draft, requested_reviewers). Patch: GET %s/pulls/%d.diff. Conversation: GET %s/issues/%d/comments.", apiRepo, prNumber, apiRepo, prNumber, apiRepo, prNumber),
		fmt.Sprintf("Native reviews: GET %s/pulls/%d/reviews, then GET %s/pulls/%d/reviews/<review_id>/comments for inline findings. These are not GitHub GraphQL threads; Forgejo currently has no supported resolve endpoint.", apiRepo, prNumber, apiRepo, prNumber),
		"Paginate list reads with page=1&limit=50, following Link rel=next / X-Total-Pages; retain all relevant pages. A failed read is not an empty list or clean result.",
		fmt.Sprintf("CI: GET %s/statuses/<head_sha>?sort=highestindex and retain the newest status per context. Actions: GET %s/actions/runs?head_sha=<head_sha> (do not combine event with head_sha); response.workflow_runs is an array. Use run.id for API paths, not index_in_repo or the UI run number. For each relevant newest workflow run, GET %s/actions/runs/<run_id>/jobs returns a bare array. Failed-job plaintext logs: GET %s/actions/jobs/<job_id>/logs (optional ?attempt=<attempt>). Fetch logs only for diagnostics needed in this run.", apiRepo, apiRepo, apiRepo, apiRepo),
	}, "\n")
}

func agentContextShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
