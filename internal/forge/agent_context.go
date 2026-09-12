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

// HostingAgentContext is the complete bot-mode replacement for legacy gh/tea
// authentication instructions. The CLI and socket reveal no credentials.
// targetNumber is the issue number for planners and the PR number for other
// roles; zero means no remote target exists yet. Role prompts own seed checks.
func HostingAgentContext(kind config.HostingIdentityKind, role, repo string, targetNumber int64) string {
	parts := []string{
		fmt.Sprintf("Hosting context: role=%s repository=%s. Use the trusted absolute CLI from LOOPER_HOST_CLI and the current execution's socket for hosting reads. Authentication is daemon-owned; no token, key, personal gh/tea login or SSH agent is available to you.", role, repo),
		`Identity: "$LOOPER_HOST_CLI" host whoami. API commands below accept only repository-relative GET paths; they cannot select a different repository or host. --paginate returns one combined JSON collection with all supported pages, or an explicit error. A failed read or response-limit error is not an empty or clean result.`,
	}
	hasPR := role != "planner" && targetNumber > 0
	if role == "planner" && targetNumber > 0 {
		parts = append(parts, fmt.Sprintf(`Issue context: "$LOOPER_HOST_CLI" host api issues/%d. Conversation: "$LOOPER_HOST_CLI" host api issues/%d/comments --paginate. Read these when current issue context is needed.`, targetNumber, targetNumber))
	} else if hasPR {
		parts = append(parts,
			fmt.Sprintf(`PR metadata: "$LOOPER_HOST_CLI" host api pulls/%d. Patch: "$LOOPER_HOST_CLI" host api pulls/%d --diff. Read these when current PR context is needed, and inspect only relevant files.`, targetNumber, targetNumber),
			fmt.Sprintf(`Conversation: "$LOOPER_HOST_CLI" host api issues/%d/comments --paginate. Reviews: "$LOOPER_HOST_CLI" host api pulls/%d/reviews --paginate.`, targetNumber, targetNumber),
		)
	}
	parts = append(parts,
		`Use the prepared local checkout for local Git inspection, edits, add and commits. If refs are missing, use "$LOOPER_HOST_CLI" host git fetch <ref>, then inspect FETCH_HEAD locally. Do not clone another checkout, push, create/edit a PR, change labels/reviewers, or make remote review-state changes. After validation, Looper publishes local commits and applies the run's PR metadata. Native reviewer review publication, when explicitly authorized later, uses only the existing trusted review submit command.`,
		`Include the ordinary final __LOOPER_RESULT__ JSON with an accurate summary, changedFiles and commits when available. git_pr_lifecycle is optional; if included, report only local commits as agent actions and remote push/PR actions as none. Preserve any role-specific structured review or repair decisions.`,
	)
	if kind == config.HostingIdentityForgejoToken {
		if hasPR {
			parts = append(parts, fmt.Sprintf(`Forgejo native inline comments: "$LOOPER_HOST_CLI" host api pulls/%d/reviews/<review_id>/comments --paginate. These are individual native review comments, not GitHub GraphQL threads.`, targetNumber))
		}
		parts = append(parts,
			`Forgejo CI: host api "statuses/<head_sha>?sort=highestindex" --paginate and host api "actions/runs?head_sha=<head_sha>" --paginate; workflow_runs is an array. Use run.id for host api actions/runs/<run_id>/jobs --paginate (bare array), then host api actions/jobs/<job_id>/logs (optional ?attempt=N) for required failure diagnostics. Prefix every command with "$LOOPER_HOST_CLI".`,
		)
	} else {
		if hasPR {
			parts = append(parts, fmt.Sprintf(`GitHub inline comments: "$LOOPER_HOST_CLI" host api pulls/%d/comments --paginate. Complete thread snapshots: "$LOOPER_HOST_CLI" host threads %d, or host thread %d <thread_node_id>. The result preserves comment id (GraphQL node ID), updatedAt, body, author.login and thread resolution state. Raw GraphQL is unavailable.`, targetNumber, targetNumber, targetNumber))
			if role == "fixer" {
				parts = append(parts, `Use these IDs/timestamps for threadCommentsObserved; never substitute REST numeric IDs.`)
			}
		}
		parts = append(parts,
			`GitHub CI: "$LOOPER_HOST_CLI" host api commits/<head_sha>/check-runs --paginate and host api commits/<head_sha>/status. For diagnostics use host api actions/runs/<run_id>/jobs --paginate and host api actions/jobs/<job_id>/logs, always prefixed with "$LOOPER_HOST_CLI".`,
		)
	}
	return strings.Join(parts, "\n")
}
