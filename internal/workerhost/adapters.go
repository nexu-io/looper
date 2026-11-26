package workerhost

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/forge"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/networkpolicy"
	"github.com/nexu-io/looper/internal/worker"
)

func ForgejoClientForRepo(cfg *config.Config, repo string) (*forge.ForgejoClient, bool, error) {
	provider, ok, err := ForgejoProviderForRepo(cfg, repo)
	if !ok || err != nil {
		return nil, ok, err
	}
	client, err := forge.NewForgejoClientFromConfig(provider, strings.TrimSpace(repo))
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

// ForgejoReviewerDiscoveryLabelsForRepo returns configured reviewer trigger
// labels for a Forgejo project. Used as a discovery fallback when native
// requested_reviewers alone is insufficient (label-triggered discovery or
// instances without requested_reviewers support).
func ForgejoReviewerDiscoveryLabelsForRepo(cfg *config.Config, repo, cwd string) []string {
	if cfg == nil {
		return nil
	}
	repo = strings.TrimSpace(repo)
	if strings.TrimSpace(cwd) != "" {
		for _, project := range cfg.Projects {
			if CwdBelongsToProject(project, cwd) {
				return ForgejoReviewerDiscoveryLabelsForProject(*cfg, project)
			}
		}
	}
	var matched *config.ProjectRefConfig
	for _, project := range cfg.Projects {
		if !strings.EqualFold(strings.TrimSpace(project.Repo), repo) {
			continue
		}
		if matched != nil {
			return nil
		}
		projectCopy := project
		matched = &projectCopy
	}
	if matched != nil {
		return ForgejoReviewerDiscoveryLabelsForProject(*cfg, *matched)
	}
	return nil
}

func ForgejoReviewerDiscoveryLabelsForProject(cfg config.Config, project config.ProjectRefConfig) []string {
	if config.ResolvedProjectProviderKind(cfg, project) != config.ProviderKindForgejo {
		return nil
	}
	labels := config.ProjectRoleConfigs(cfg, project.ID).Reviewer.Discovery.Triggers.Labels
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label != "" {
			result = append(result, label)
		}
	}
	return result
}

// AddForgejoPullRequestReviewers requests native reviewers and, when the project
// is label-triggered, also applies discovery trigger labels so reviewer
// auto-discovery still matches. If native request fails but labels are
// configured, labels alone keep compatibility instances working.
// When native request succeeds on a native-request-triggered project
// (requireReviewRequest=true), label application is best-effort because
// requested_reviewers already makes the PR discoverable. Label failure remains
// fatal for label-triggered projects (requireReviewRequest=false with trigger
// labels): discovery still filters by labels, so a missing label would leave
// the handoff permanently undiscoverable if publish marked it done.
func AddForgejoPullRequestReviewers(ctx context.Context, client *forge.ForgejoClient, cfg *config.Config, repo string, prNumber int64, reviewers []string, cwd string) error {
	labels := ForgejoReviewerDiscoveryLabelsForRepo(cfg, repo, cwd)
	nativeErr := client.AddPullRequestReviewers(ctx, prNumber, reviewers)
	if nativeErr != nil && len(labels) == 0 {
		return nativeErr
	}
	if len(labels) > 0 {
		if _, err := client.AddIssueLabels(ctx, prNumber, labels); err != nil {
			if nativeErr != nil {
				return fmt.Errorf("forgejo native review request failed (%v); label fallback also failed: %w", nativeErr, err)
			}
			// Native request already succeeded. Only native-request-triggered
			// discovery can treat labels as best-effort; label-triggered
			// projects must keep the handoff retryable until labels land.
			if ForgejoReviewerRequireReviewRequestForRepo(cfg, repo, cwd) {
				return nil
			}
			return err
		}
	}
	return nil
}

// ForgejoReviewerRequireReviewRequestForRepo reports whether reviewer discovery
// for the matched Forgejo project requires a native review request. Defaults to
// true when no unique project match exists (labels are also empty in that case).
func ForgejoReviewerRequireReviewRequestForRepo(cfg *config.Config, repo, cwd string) bool {
	if cfg == nil {
		return true
	}
	repo = strings.TrimSpace(repo)
	if strings.TrimSpace(cwd) != "" {
		for _, project := range cfg.Projects {
			if CwdBelongsToProject(project, cwd) {
				return ForgejoReviewerRequireReviewRequestForProject(*cfg, project)
			}
		}
	}
	var matched *config.ProjectRefConfig
	for _, project := range cfg.Projects {
		if !strings.EqualFold(strings.TrimSpace(project.Repo), repo) {
			continue
		}
		if matched != nil {
			return true
		}
		projectCopy := project
		matched = &projectCopy
	}
	if matched != nil {
		return ForgejoReviewerRequireReviewRequestForProject(*cfg, *matched)
	}
	return true
}

func ForgejoReviewerRequireReviewRequestForProject(cfg config.Config, project config.ProjectRefConfig) bool {
	if config.ResolvedProjectProviderKind(cfg, project) != config.ProviderKindForgejo {
		return true
	}
	return config.ProjectRoleConfigs(cfg, project.ID).Reviewer.Discovery.Triggers.RequireReviewRequest
}

func ForgejoClientForCWD(cfg *config.Config, cwd string) (*forge.ForgejoClient, bool, error) {
	project, provider, ok, err := ForgejoProjectProviderForCWD(cfg, cwd)
	if !ok || err != nil {
		return nil, ok, err
	}
	client, err := forge.NewForgejoClientFromConfig(provider, strings.TrimSpace(project.Repo))
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

func ForgejoProviderForRepo(cfg *config.Config, repo string) (config.ProviderConfig, bool, error) {
	if cfg == nil {
		return config.ProviderConfig{}, false, nil
	}
	repo = strings.TrimSpace(repo)
	var matched *config.ProjectRefConfig
	for _, project := range cfg.Projects {
		if !strings.EqualFold(strings.TrimSpace(project.Repo), repo) {
			continue
		}
		if matched != nil {
			return config.ProviderConfig{}, false, fmt.Errorf("repository %s is bound to multiple projects; project path or id is required", repo)
		}
		projectCopy := project
		matched = &projectCopy
	}
	if matched != nil {
		if config.ResolvedProjectProviderKind(*cfg, *matched) != config.ProviderKindForgejo {
			return config.ProviderConfig{}, false, nil
		}
		provider, ok := ForgejoProviderByID(*cfg, matched.Provider)
		if !ok {
			return config.ProviderConfig{}, false, fmt.Errorf("forgejo provider %q not configured for repo %s", matched.Provider, repo)
		}
		return provider, true, nil
	}
	return config.ProviderConfig{}, false, nil
}

func ForgejoProjectProviderForCWD(cfg *config.Config, cwd string) (config.ProjectRefConfig, config.ProviderConfig, bool, error) {
	if cfg == nil {
		return config.ProjectRefConfig{}, config.ProviderConfig{}, false, nil
	}
	project, matched, err := ProjectForCWD(*cfg, cwd)
	if err != nil || !matched {
		return config.ProjectRefConfig{}, config.ProviderConfig{}, false, err
	}
	if config.ResolvedProjectProviderKind(*cfg, project) != config.ProviderKindForgejo {
		return config.ProjectRefConfig{}, config.ProviderConfig{}, false, nil
	}
	provider, ok := ForgejoProviderByID(*cfg, project.Provider)
	if !ok {
		return config.ProjectRefConfig{}, config.ProviderConfig{}, false, fmt.Errorf("forgejo provider %q not configured for project %s", project.Provider, project.ID)
	}
	if strings.TrimSpace(project.Repo) == "" {
		return config.ProjectRefConfig{}, config.ProviderConfig{}, false, fmt.Errorf("forgejo project %s is missing repo", project.ID)
	}
	return project, provider, true, nil
}

func ProjectForCWD(cfg config.Config, cwd string) (config.ProjectRefConfig, bool, error) {
	matches := make([]config.ProjectRefConfig, 0, 1)
	for _, project := range cfg.Projects {
		if CwdBelongsToProject(project, cwd) {
			matches = append(matches, project)
		}
	}
	if len(matches) == 0 {
		return config.ProjectRefConfig{}, false, nil
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, project := range matches {
			ids = append(ids, project.ID)
		}
		return config.ProjectRefConfig{}, false, fmt.Errorf("working directory %s matches multiple projects: %s", strings.TrimSpace(cwd), strings.Join(ids, ", "))
	}
	return matches[0], true, nil
}

func CwdBelongsToProject(project config.ProjectRefConfig, cwd string) bool {
	cwd = filepath.Clean(strings.TrimSpace(cwd))
	if cwd == "." || cwd == "" {
		return false
	}
	repoPath := filepath.Clean(strings.TrimSpace(project.RepoPath))
	if repoPath != "." && cwd == repoPath {
		return true
	}
	worktreeRoot := ""
	if project.WorktreeRoot != nil {
		worktreeRoot = strings.TrimSpace(*project.WorktreeRoot)
	}
	if worktreeRoot == "" {
		resolved, err := config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
		if err != nil {
			return false
		}
		worktreeRoot = resolved
	}
	worktreeRoot = filepath.Clean(worktreeRoot)
	relative, err := filepath.Rel(worktreeRoot, cwd)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func ForgejoProviderByID(cfg config.Config, providerID string) (config.ProviderConfig, bool) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return config.ProviderConfig{}, false
	}
	for _, provider := range cfg.Providers {
		if provider.ID == providerID {
			return provider, true
		}
	}
	return config.ProviderConfig{}, false
}

// PlaneClientForRepo returns a Plane task-source client for a project whose
// provider kind is "plane" and whose code repo (project.Repo, a GitHub repo)
// matches repo. The second return is false when repo is not a plane project, in
// which case callers fall through to the GitHub/forgejo path.
func PlaneClientForRepo(cfg *config.Config, repo string) (*forge.PlaneClient, bool, error) {
	provider, codeRepo, ok, err := PlaneProviderForRepo(cfg, repo)
	if !ok || err != nil {
		return nil, ok, err
	}
	client, err := forge.NewPlaneClientFromConfig(provider, codeRepo)
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

func PlaneClientForCWD(cfg *config.Config, cwd string) (*forge.PlaneClient, bool, error) {
	project, provider, ok, err := PlaneProjectProviderForCWD(cfg, cwd)
	if !ok || err != nil {
		return nil, ok, err
	}
	client, err := forge.NewPlaneClientFromConfig(provider, strings.TrimSpace(project.Repo))
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

func PlaneProviderForRepo(cfg *config.Config, repo string) (config.ProviderConfig, string, bool, error) {
	if cfg == nil {
		return config.ProviderConfig{}, "", false, nil
	}
	repo = strings.TrimSpace(repo)
	var matched *config.ProjectRefConfig
	for _, project := range cfg.Projects {
		if !strings.EqualFold(strings.TrimSpace(project.Repo), repo) {
			continue
		}
		if matched != nil {
			return config.ProviderConfig{}, "", false, fmt.Errorf("repository %s is bound to multiple projects; project path or id is required", repo)
		}
		projectCopy := project
		matched = &projectCopy
	}
	if matched != nil {
		if config.ResolvedProjectProviderKind(*cfg, *matched) != config.ProviderKindPlane {
			return config.ProviderConfig{}, "", false, nil
		}
		provider, ok := ForgejoProviderByID(*cfg, matched.Provider)
		if !ok {
			return config.ProviderConfig{}, "", false, fmt.Errorf("plane provider %q not configured for repo %s", matched.Provider, repo)
		}
		return provider, strings.TrimSpace(matched.Repo), true, nil
	}
	return config.ProviderConfig{}, "", false, nil
}

func PlaneProjectProviderForCWD(cfg *config.Config, cwd string) (config.ProjectRefConfig, config.ProviderConfig, bool, error) {
	if cfg == nil {
		return config.ProjectRefConfig{}, config.ProviderConfig{}, false, nil
	}
	project, matched, err := ProjectForCWD(*cfg, cwd)
	if err != nil || !matched {
		return config.ProjectRefConfig{}, config.ProviderConfig{}, false, err
	}
	if config.ResolvedProjectProviderKind(*cfg, project) != config.ProviderKindPlane {
		return config.ProjectRefConfig{}, config.ProviderConfig{}, false, nil
	}
	provider, ok := ForgejoProviderByID(*cfg, project.Provider)
	if !ok {
		return config.ProjectRefConfig{}, config.ProviderConfig{}, false, fmt.Errorf("plane provider %q not configured for project %s", project.Provider, project.ID)
	}
	if strings.TrimSpace(project.Repo) == "" {
		return config.ProjectRefConfig{}, config.ProviderConfig{}, false, fmt.Errorf("plane project %s is missing repo", project.ID)
	}
	return project, provider, true, nil
}

func ForgeIdentityLogins(identities []forge.Identity) []string {
	if identities == nil {
		return nil
	}
	logins := make([]string, 0, len(identities))
	for _, identity := range identities {
		if login := strings.TrimSpace(identity.Login); login != "" {
			logins = append(logins, login)
		}
	}
	return logins
}

func ForgeLabelNames(labels []forge.Label) []string {
	if labels == nil {
		return nil
	}
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		if name := strings.TrimSpace(label.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func ForgeNetworkPolicyUsers(users []forge.Identity) []networkpolicy.GitHubUser {
	if users == nil {
		return nil
	}
	converted := make([]networkpolicy.GitHubUser, 0, len(users))
	for _, user := range users {
		converted = append(converted, networkpolicy.GitHubUser{Login: user.Login, ID: user.ID})
	}
	return converted
}

func ForgeClientForLocation[T any](cfg *config.Config, repo string, cwd []string, byCWD func(*config.Config, string) (*T, bool, error), byRepo func(*config.Config, string) (*T, bool, error)) (*T, bool, error) {
	if len(cwd) > 0 && strings.TrimSpace(cwd[0]) != "" {
		client, ok, err := byCWD(cfg, cwd[0])
		if ok || err != nil {
			return client, ok, err
		}
		// A configured project path is authoritative even when it belongs to
		// GitHub; do not fall through and select a same-slug Forgejo project.
		if cfg != nil {
			for _, project := range cfg.Projects {
				if CwdBelongsToProject(project, cwd[0]) {
					return nil, false, nil
				}
			}
		}
	}
	return byRepo(cfg, repo)
}

func NetworkPolicyUsers(users []githubinfra.GitHubUser) []networkpolicy.GitHubUser {
	if users == nil {
		return nil
	}
	converted := make([]networkpolicy.GitHubUser, 0, len(users))
	for _, user := range users {
		converted = append(converted, networkpolicy.GitHubUser{Login: user.Login, ID: user.ID})
	}
	return converted
}

type GitHub struct {
	Gateway *githubinfra.Gateway
	Stamper disclosure.Stamper
	Config  *config.Config
}

func (a GitHub) forgejo(ctx context.Context, repo string, cwd ...string) (*forge.ForgejoClient, bool, error) {
	client, ok, err := ForgeClientForLocation(a.Config, repo, cwd, ForgejoClientForCWD, ForgejoClientForRepo)
	return client, ok, err
}

// plane returns a Plane task-source client when repo belongs to a plane-kind
// project. The worker reads issues and posts issue-side comments/labels through
// Plane; pull-request creation stays on the GitHub code repo.
func (a GitHub) plane(ctx context.Context, repo string, cwd ...string) (*forge.PlaneClient, bool, error) {
	client, ok, err := ForgeClientForLocation(a.Config, repo, cwd, PlaneClientForCWD, PlaneClientForRepo)
	return client, ok, err
}

func (a GitHub) ListOpenPullRequests(ctx context.Context, input worker.ListOpenPullRequestsInput) ([]worker.PullRequestSummary, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return nil, err
		}
		pullRequests, err := client.ListOpenPullRequests(ctx, forge.ListPullRequestsInput{Limit: input.Limit})
		if err != nil {
			return nil, err
		}
		result := make([]worker.PullRequestSummary, 0, len(pullRequests))
		for _, pr := range pullRequests {
			result = append(result, worker.PullRequestSummary{Number: pr.Number, URL: pr.HTMLURL, State: pr.State, HeadRefName: pr.Head.Name, BaseRefName: pr.Base.Name})
		}
		return result, nil
	}
	if a.Gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	pullRequests, err := a.Gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{
		Repo:       input.Repo,
		CWD:        input.CWD,
		Limit:      input.Limit,
		Label:      input.Label,
		PreferREST: true,
	})
	if err != nil {
		return nil, err
	}
	result := make([]worker.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, worker.PullRequestSummary{Number: pr.Number, URL: pr.URL, State: pr.State, HeadRefName: pr.HeadRefName, BaseRefName: pr.BaseRefName})
	}
	return result, nil
}

func (a GitHub) ListOpenIssues(ctx context.Context, input worker.ListOpenIssuesInput) ([]worker.IssueSummary, error) {
	if client, ok, err := a.plane(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return nil, err
		}
		issues, err := client.ListStrictDispatchIssues(ctx, forge.ListIssuesInput{Labels: input.Labels, Assignee: input.Assignee, Limit: input.Limit}, "worker")
		if err != nil {
			return nil, err
		}
		result := make([]worker.IssueSummary, 0, len(issues))
		for _, issue := range issues {
			result = append(result, worker.IssueSummary{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.HTMLURL, Assignees: ForgeIdentityLogins(issue.Assignees), AssigneeUsers: ForgeNetworkPolicyUsers(issue.Assignees), Labels: ForgeLabelNames(issue.Labels), StrictDispatchID: issue.StrictDispatchID})
		}
		return result, nil
	}
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return nil, err
		}
		issues, err := client.ListOpenIssues(ctx, forge.ListIssuesInput{Labels: input.Labels, Assignee: input.Assignee, Limit: input.Limit})
		if err != nil {
			return nil, err
		}
		result := make([]worker.IssueSummary, 0, len(issues))
		for _, issue := range issues {
			result = append(result, worker.IssueSummary{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.HTMLURL, Assignees: ForgeIdentityLogins(issue.Assignees), AssigneeUsers: ForgeNetworkPolicyUsers(issue.Assignees), Labels: ForgeLabelNames(issue.Labels)})
		}
		return result, nil
	}
	if a.Gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	issues, err := a.Gateway.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Assignee: input.Assignee, Label: input.Label, Labels: input.Labels})
	if err != nil {
		return nil, err
	}
	result := make([]worker.IssueSummary, 0, len(issues))
	for _, issue := range issues {
		result = append(result, worker.IssueSummary{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, Author: issue.Author, Assignees: issue.Assignees, AssigneeUsers: NetworkPolicyUsers(issue.AssigneeUsers), Labels: issue.Labels})
	}
	return result, nil
}

func (a GitHub) TransitionStrictDispatch(ctx context.Context, input worker.StrictDispatchTransitionInput) error {
	client, ok, err := a.plane(ctx, input.Repo, input.CWD)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("strict dispatch requires a Plane provider")
	}
	return client.TransitionStrictDispatch(ctx, input.DispatchID, input.State, input.WaitKind)
}

func (a GitHub) GetCurrentUserLogin(ctx context.Context, cwd string) (string, error) {
	if client, ok, err := PlaneClientForCWD(a.Config, cwd); ok || err != nil {
		if err != nil {
			return "", err
		}
		identity, err := client.CurrentUser(ctx)
		return identity.Login, err
	}
	if client, ok, err := ForgejoClientForCWD(a.Config, cwd); ok || err != nil {
		if err != nil {
			return "", err
		}
		identity, err := client.CurrentUser(ctx)
		return identity.Login, err
	}
	if a.Gateway == nil {
		return "", fmt.Errorf("github gateway is not configured")
	}
	return a.Gateway.GetCurrentUserLogin(ctx, cwd)
}

func (a GitHub) AddIssueAssignees(ctx context.Context, input worker.IssueAssigneesInput) error {
	if client, ok, err := a.plane(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return err
		}
		return client.AddIssueAssignees(ctx, input.IssueNumber, input.Assignees)
	}
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return err
		}
		return client.AddIssueAssignees(ctx, input.IssueNumber, input.Assignees)
	}
	if a.Gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.Gateway.AddIssueAssignees(ctx, githubinfra.IssueAssigneesInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Assignees: input.Assignees, CWD: input.CWD})
}

func (a GitHub) ViewPullRequest(ctx context.Context, input worker.ViewPullRequestInput) (worker.PullRequestDetail, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return worker.PullRequestDetail{}, err
		}
		pr, err := client.ViewPullRequest(ctx, input.PRNumber)
		if err != nil {
			return worker.PullRequestDetail{}, err
		}
		return worker.PullRequestDetail{Number: pr.Number, Title: pr.Title, Body: pr.Body, URL: pr.HTMLURL, State: pr.State, HeadRefName: pr.Head.Name, BaseRefName: pr.Base.Name, HeadSHA: pr.Head.SHA, Labels: ForgeLabelNames(pr.Labels)}, nil
	}
	if a.Gateway == nil {
		return worker.PullRequestDetail{}, fmt.Errorf("github gateway is not configured")
	}
	detail, err := a.Gateway.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return worker.PullRequestDetail{}, err
	}
	return worker.PullRequestDetail{Number: detail.Number, Title: detail.Title, Body: detail.Body, URL: detail.URL, State: detail.State, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, HeadSHA: detail.HeadSHA, Labels: append([]string(nil), detail.Labels...), ReviewRequests: detail.ReviewRequests, ReviewRequestUsers: NetworkPolicyUsers(detail.ReviewRequestUsers)}, nil
}

func (a GitHub) ViewIssue(ctx context.Context, input worker.ViewIssueInput) (worker.IssueDetail, error) {
	if client, ok, err := a.plane(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return worker.IssueDetail{}, err
		}
		issue, err := client.ViewIssue(ctx, input.IssueNumber)
		if err != nil {
			return worker.IssueDetail{}, err
		}
		return worker.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.HTMLURL, State: issue.State, AssigneeUsers: ForgeNetworkPolicyUsers(issue.Assignees), Labels: ForgeLabelNames(issue.Labels)}, nil
	}
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return worker.IssueDetail{}, err
		}
		issue, err := client.ViewIssue(ctx, input.IssueNumber)
		if err != nil {
			return worker.IssueDetail{}, err
		}
		return worker.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.HTMLURL, State: issue.State, AssigneeUsers: ForgeNetworkPolicyUsers(issue.Assignees), Labels: ForgeLabelNames(issue.Labels)}, nil
	}
	if a.Gateway == nil {
		return worker.IssueDetail{}, fmt.Errorf("github gateway is not configured")
	}
	issue, err := a.Gateway.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: input.Repo, IssueNumber: input.IssueNumber, CWD: input.CWD})
	if err != nil {
		return worker.IssueDetail{}, err
	}
	return worker.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, State: issue.State, IsPullRequest: issue.IsPullRequest, AssigneeUsers: NetworkPolicyUsers(issue.AssigneeUsers), Labels: issue.Labels}, nil
}

func (a GitHub) CreateIssueComment(ctx context.Context, input worker.IssueCommentInput) (worker.IssueCommentResult, error) {
	body := a.Stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "worker", disclosure.ChannelIssueComment)
	if client, ok, err := a.plane(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return worker.IssueCommentResult{}, err
		}
		comment, err := client.CreateIssueComment(ctx, forge.CreateCommentInput{IssueNumber: input.IssueNumber, Body: body})
		if err != nil {
			return worker.IssueCommentResult{}, err
		}
		// Plane comment ids are UUIDs and do not fit looper's int64 comment id, so
		// ID stays 0; the URL points at the work-item web page.
		return worker.IssueCommentResult{ID: comment.ID, URL: comment.HTMLURL}, nil
	}
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return worker.IssueCommentResult{}, err
		}
		comment, err := client.CreateIssueComment(ctx, forge.CreateCommentInput{IssueNumber: input.IssueNumber, Body: body})
		if err != nil {
			return worker.IssueCommentResult{}, err
		}
		return worker.IssueCommentResult{ID: comment.ID, URL: comment.HTMLURL}, nil
	}
	if a.Gateway == nil {
		return worker.IssueCommentResult{}, fmt.Errorf("github gateway is not configured")
	}
	comment, err := a.Gateway.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Body: body, CWD: input.CWD})
	if err != nil {
		return worker.IssueCommentResult{}, err
	}
	return worker.IssueCommentResult{ID: comment.ID, URL: comment.URL}, nil
}

func (a GitHub) UpdateIssueComment(ctx context.Context, input worker.UpdateIssueCommentInput) error {
	body := a.Stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "worker", disclosure.ChannelIssueComment)
	if _, ok, err := a.plane(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return err
		}
		// Plane comment ids are UUIDs, which do not round-trip through looper's
		// int64 comment id, so in-place updates are not supported. This is a
		// no-op: the worker's CreateIssueComment path (which returns id 0) posts a
		// fresh progress comment on each status transition instead.
		// TODO(plane): track the Plane comment UUID to enable true updates.
		return nil
	}
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return err
		}
		_, err = client.UpdateIssueComment(ctx, forge.UpdateCommentInput{CommentID: input.CommentID, Body: body})
		return err
	}
	if a.Gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.Gateway.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: input.Repo, CommentID: input.CommentID, Body: body, CWD: input.CWD})
}

func (a GitHub) CreatePullRequest(ctx context.Context, input worker.CreatePullRequestInput) (worker.CreatePullRequestResult, error) {
	body := a.Stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "worker", disclosure.ChannelPullRequest)
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return worker.CreatePullRequestResult{}, err
		}
		pr, err := client.CreatePullRequest(ctx, forge.CreatePullRequestInput{Title: input.Title, Body: body, Head: input.HeadBranch, Base: input.BaseBranch})
		if err != nil {
			return worker.CreatePullRequestResult{}, err
		}
		return worker.CreatePullRequestResult{Number: pr.Number, URL: pr.HTMLURL}, nil
	}
	if a.Gateway == nil {
		return worker.CreatePullRequestResult{}, fmt.Errorf("github gateway is not configured")
	}
	pr, err := a.Gateway.CreatePullRequest(ctx, githubinfra.CreatePullRequestInput{Repo: input.Repo, HeadBranch: input.HeadBranch, BaseBranch: input.BaseBranch, Title: input.Title, Body: body, Draft: input.Draft, CWD: input.CWD})
	if err != nil {
		return worker.CreatePullRequestResult{}, err
	}
	return worker.CreatePullRequestResult{Number: pr.Number, URL: pr.URL}, nil
}

func (a GitHub) AddPullRequestLabels(ctx context.Context, input worker.PullRequestLabelsInput) error {
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return err
		}
		_, err := client.AddIssueLabels(ctx, input.PRNumber, input.Labels)
		return err
	}
	if a.Gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.Gateway.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a GitHub) CompareBranches(ctx context.Context, input worker.CompareBranchesInput) (worker.CompareBranchesResult, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return worker.CompareBranchesResult{}, err
		}
		comparison, err := client.CompareBranches(ctx, forge.CompareBranchesInput{Base: input.BaseBranch, Head: input.HeadBranch})
		if err != nil {
			return worker.CompareBranchesResult{}, err
		}
		return worker.CompareBranchesResult{AheadBy: comparison.AheadBy, BehindBy: comparison.BehindBy, Status: comparison.Status, TotalCommits: comparison.TotalCommits}, nil
	}
	if a.Gateway == nil {
		return worker.CompareBranchesResult{}, fmt.Errorf("github gateway is not configured")
	}
	comparison, err := a.Gateway.CompareBranches(ctx, githubinfra.CompareBranchesInput{Repo: input.Repo, BaseBranch: input.BaseBranch, HeadBranch: input.HeadBranch, CWD: input.CWD})
	if err != nil {
		return worker.CompareBranchesResult{}, err
	}
	return worker.CompareBranchesResult{AheadBy: comparison.AheadBy, BehindBy: comparison.BehindBy, Status: comparison.Status, TotalCommits: comparison.TotalCommits}, nil
}

func (a GitHub) UpdatePullRequestBody(ctx context.Context, input worker.UpdatePullRequestBodyInput) error {
	body := a.Stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "worker", disclosure.ChannelPullRequest)
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return err
		}
		_, err = client.UpdatePullRequest(ctx, forge.UpdatePullRequestInput{Number: input.PRNumber, Body: &body})
		return err
	}
	if a.Gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.Gateway.UpdatePullRequestBody(ctx, githubinfra.UpdatePullRequestBodyInput{Repo: input.Repo, PRNumber: input.PRNumber, Body: body, CWD: input.CWD})
}

func (a GitHub) UpdatePullRequestTitle(ctx context.Context, input worker.UpdatePullRequestTitleInput) error {
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return err
		}
		_, err = client.UpdatePullRequest(ctx, forge.UpdatePullRequestInput{Number: input.PRNumber, Title: &input.Title})
		return err
	}
	if a.Gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.Gateway.UpdatePullRequestTitle(ctx, githubinfra.UpdatePullRequestTitleInput{Repo: input.Repo, PRNumber: input.PRNumber, Title: input.Title, CWD: input.CWD})
}

func (a GitHub) RemovePullRequestLabels(ctx context.Context, input worker.PullRequestLabelsInput) error {
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return err
		}
		for _, label := range input.Labels {
			if err := client.RemoveIssueLabel(ctx, input.PRNumber, label); err != nil {
				return err
			}
		}
		return nil
	}
	if a.Gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.Gateway.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a GitHub) AddPullRequestReviewers(ctx context.Context, input worker.PullRequestReviewersInput) error {
	if client, ok, err := a.forgejo(ctx, input.Repo, input.CWD); ok || err != nil {
		if err != nil {
			return err
		}
		return AddForgejoPullRequestReviewers(ctx, client, a.Config, input.Repo, input.PRNumber, input.Reviewers, input.CWD)
	}
	if a.Gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.Gateway.AddPullRequestReviewers(ctx, githubinfra.PullRequestReviewersInput{Repo: input.Repo, PRNumber: input.PRNumber, Reviewers: input.Reviewers, CWD: input.CWD})
}

type Git struct {
	Gateway *gitinfra.Gateway
	Stamper disclosure.Stamper
}

func (a Git) CreateWorktree(ctx context.Context, input worker.CreateWorktreeInput) (worker.CreateWorktreeResult, error) {
	worktree, err := a.Gateway.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, Branch: input.Branch, BaseBranch: input.BaseBranch, PRNumber: input.PRNumber, ProtectedBranches: input.ProtectedBranches, CheckoutMode: gitinfra.CheckoutMode(input.CheckoutMode)})
	if err != nil {
		return worker.CreateWorktreeResult{}, err
	}
	return worker.CreateWorktreeResult{WorktreePath: worktree.WorktreePath, Branch: worktree.Branch, BaseBranch: derefString(worktree.BaseBranch), HeadSHA: derefString(worktree.HeadSHA), WorktreeID: worktree.ID}, nil
}

func (a Git) RestoreWorktree(ctx context.Context, input worker.RestoreWorktreeInput) (*worker.RestoreWorktreeResult, error) {
	worktree, err := a.Gateway.RestoreWorktree(ctx, gitinfra.RestoreWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, Branch: input.Branch, WorktreeRoot: input.WorktreeRoot, CheckoutMode: gitinfra.CheckoutMode(input.CheckoutMode), ExpectedWorktreePath: input.ExpectedWorktreePath})
	if err != nil || worktree == nil {
		return nil, err
	}
	return &worker.RestoreWorktreeResult{WorktreePath: worktree.WorktreePath, Branch: worktree.Branch, BaseBranch: derefString(worktree.BaseBranch), HeadSHA: derefString(worktree.HeadSHA), WorktreeID: worktree.ID}, nil
}

func (a Git) PrepareWorktree(ctx context.Context, input worker.PrepareWorktreeInput) (worker.PrepareWorktreeResult, error) {
	result, err := a.Gateway.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, ExpectedHeadSHA: input.ExpectedHeadSHA, Remote: input.Remote})
	if err != nil {
		return worker.PrepareWorktreeResult{}, err
	}
	return worker.PrepareWorktreeResult{HeadSHA: result.HeadSHA, Clean: result.Clean}, nil
}

func (a Git) InspectHead(ctx context.Context, input worker.InspectHeadInput) (worker.InspectHeadResult, error) {
	result, err := a.Gateway.InspectHead(ctx, gitinfra.InspectHeadInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, BaseRef: input.BaseRef})
	if err != nil {
		return worker.InspectHeadResult{}, err
	}
	return worker.InspectHeadResult{HeadSHA: result.HeadSHA, NewCommitSHAs: result.NewCommitSHAs, HasUncommittedChanges: result.HasUncommittedChanges, ChangedFiles: result.ChangedFiles}, nil
}

func (a Git) Commit(ctx context.Context, input worker.CommitInput) (worker.CommitResult, error) {
	message := a.Stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).CommitMessage(input.Message, "worker")
	result, err := a.Gateway.Commit(ctx, gitinfra.CommitInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Message: message})
	if err != nil {
		return worker.CommitResult{}, err
	}
	return worker.CommitResult{CommitSHA: result.CommitSHA}, nil
}

func (a Git) Push(ctx context.Context, input worker.PushInput) error {
	return a.Gateway.Push(ctx, gitinfra.PushInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, Remote: input.Remote, ProtectedBranches: input.ProtectedBranches})
}

type Executor struct {
	Executor *agent.ConfiguredExecutor
}

type WorkerAgentExecutionAdapter struct {
	execution agent.Execution
}

func (a Executor) Start(ctx context.Context, input worker.AgentRunInput) (worker.AgentExecution, error) {
	// Ownership is acquired at the common executor boundary (AdmitSpawn +
	// BindHandle), not via post-spawn role-adapter registration (#576 / not #572).
	execution, err := a.Executor.Start(ctx, agent.RunInput{
		ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID,
		Prompt: input.Prompt, NativeResumePrompt: input.NativeResumePrompt, NativeSessionID: input.NativeSessionID,
		WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, HeartbeatTimeout: input.HeartbeatTimeout,
		Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey,
		UseSnapshot: input.UseSnapshot, SnapshotVendor: input.SnapshotVendor, SnapshotModel: input.SnapshotModel,
	})
	if err != nil {
		return nil, err
	}
	return WorkerAgentExecutionAdapter{execution: execution}, nil
}

func (a WorkerAgentExecutionAdapter) Wait(ctx context.Context) (worker.AgentResult, error) {
	result, err := a.execution.Wait(ctx)
	if err != nil {
		return worker.AgentResult{}, err
	}
	return worker.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, ParseStatus: result.ParseStatus, ChangedFiles: result.ChangedFiles, Commits: result.Commits, Lifecycle: result.Lifecycle, TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds, ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt}, nil
}

func (a WorkerAgentExecutionAdapter) Kill(reason string) error {
	return a.execution.Kill(reason)
}
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
