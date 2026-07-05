package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	coordinatorrole "github.com/nexu-io/looper/internal/coordinator"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/fixer"
	"github.com/nexu-io/looper/internal/forge"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/notify"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/network/protocol"
	"github.com/nexu-io/looper/internal/networkpolicy"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/webhookforward"
	"github.com/nexu-io/looper/internal/worker"
)

type plannerScheduler interface {
	DiscoverIssues(context.Context, planner.DiscoveryInput) (planner.DiscoveryResult, error)
	ProcessNext(context.Context, string) (*planner.ProcessResult, error)
	ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*planner.ProcessResult, error)
}

type coordinatorScheduler interface {
	DiscoverIssues(context.Context, coordinatorrole.DiscoveryInput) (coordinatorrole.DiscoveryResult, error)
}

type reviewerScheduler interface {
	DiscoverPullRequests(context.Context, reviewer.DiscoveryInput) (reviewer.DiscoveryResult, error)
	DiscoverPullRequest(context.Context, reviewer.TargetedDiscoveryInput) (reviewer.DiscoveryResult, error)
	ProcessNext(context.Context, string) (*reviewer.ProcessResult, error)
	ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*reviewer.ProcessResult, error)
}

type fixerScheduler interface {
	DiscoverPullRequests(context.Context, fixer.DiscoveryInput) (fixer.DiscoveryResult, error)
	DiscoverPullRequest(context.Context, fixer.TargetedDiscoveryInput) (fixer.DiscoveryResult, error)
	DiscoverPullRequestsForBaseBranchUpdate(context.Context, fixer.BaseBranchDiscoveryInput) (fixer.DiscoveryResult, error)
	ProcessNext(context.Context, string) (*fixer.ProcessResult, error)
	ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*fixer.ProcessResult, error)
}

type workerScheduler interface {
	ProcessNext(context.Context, string) (*worker.ProcessResult, error)
	ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*worker.ProcessResult, error)
}

type snapshotScheduler interface {
	CapturePullRequestSnapshot(context.Context, githubinfra.CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error)
}

type workerIssueDiscoveryScheduler interface {
	DiscoverIssues(context.Context, worker.DiscoveryInput) (worker.DiscoveryResult, error)
}

type schedulerAsyncRunner interface {
	Go(func())
}

type defaultSchedulerTickInput struct {
	Repos                    *storage.Repositories
	GitHubGateway            *githubinfra.Gateway
	Logger                   bootstrap.Logger
	Now                      func() time.Time
	MaxConcurrentRuns        int
	ClaimMu                  *sync.Mutex
	ReconcileStaleRuns       func(context.Context) (StaleRunReconcileSummary, error)
	AsyncRunner              schedulerAsyncRunner
	RequestSchedulerWake     func()
	Planner                  plannerScheduler
	Coordinator              coordinatorScheduler
	Reviewer                 reviewerScheduler
	Fixer                    fixerScheduler
	Worker                   workerScheduler
	Snapshotter              snapshotScheduler
	Config                   *config.Config
	PlannerDiscoveryEnabled  *bool
	CoordinatorEnabled       func(string) bool
	ReviewerDiscoveryEnabled *bool
	FixerDiscoveryEnabled    *bool
	WorkerDiscoveryEnabled   *bool
	// OnHITLAnswerDelivered, when set, is called after a Feishu HITL answer is
	// delivered to a loop, so the transport can mark the ask card resolved.
	OnHITLAnswerDelivered func(context.Context, string, string)
}

type defaultSchedulerHandlers struct {
	tick    RunSchedulerTickFunc
	claim   RunSchedulerTickFunc
	webhook WebhookForwarder
}

type schedulerTaskTracker struct{ wg sync.WaitGroup }

func (t *schedulerTaskTracker) Go(fn func()) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		fn()
	}()
}

func (t *schedulerTaskTracker) Wait() {
	t.wg.Wait()
}

type agentExecutionNotificationInput struct {
	ExecutionID string
	ProjectID   string
	LoopID      string
	RunID       string
	Title       string
	Subtitle    string
	Body        string
	DedupeKey   string
}

type workerRunCompletedNotificationInput struct {
	ProjectID         string
	LoopID            string
	RunID             string
	Subtitle          string
	Status            string
	Summary           string
	FailureKind       worker.QueueFailureKind
	PullRequestNumber int64
	PullRequestURL    string
}

type coordinatorNetworkGateway struct {
	statePath string
	client    *http.Client
}

func (g coordinatorNetworkGateway) Status(ctx context.Context) (protocol.NodeStatusResponse, error) {
	api, err := g.api()
	if err != nil {
		return protocol.NodeStatusResponse{}, err
	}
	return api.Status(ctx)
}

func (g coordinatorNetworkGateway) RevalidateLease(ctx context.Context, req protocol.CoordinatorLeaseRevalidateRequest) error {
	api, err := g.api()
	if err != nil {
		return err
	}
	return api.RevalidateLease(ctx, req)
}

func (g coordinatorNetworkGateway) api() (*networkclient.Client, error) {
	state, err := networkclient.LoadState(g.statePath)
	if err != nil {
		return nil, err
	}
	return networkclient.New(state.URL, state.NodeToken, g.client), nil
}

type plannerGitHubAdapter struct {
	gateway *githubinfra.Gateway
	stamper disclosure.Stamper
	config  *config.Config
}

func forgejoClientForRepo(cfg *config.Config, repo string) (*forge.ForgejoClient, bool, error) {
	provider, ok, err := forgejoProviderForRepo(cfg, repo)
	if !ok || err != nil {
		return nil, ok, err
	}
	client, err := forge.NewForgejoClientFromConfig(provider, strings.TrimSpace(repo))
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

func forgejoClientForCWD(cfg *config.Config, cwd string) (*forge.ForgejoClient, bool, error) {
	project, provider, ok, err := forgejoProjectProviderForCWD(cfg, cwd)
	if !ok || err != nil {
		return nil, ok, err
	}
	client, err := forge.NewForgejoClientFromConfig(provider, strings.TrimSpace(project.Repo))
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

func forgejoProviderForRepo(cfg *config.Config, repo string) (config.ProviderConfig, bool, error) {
	if cfg == nil {
		return config.ProviderConfig{}, false, nil
	}
	repo = strings.TrimSpace(repo)
	for _, project := range cfg.Projects {
		if strings.TrimSpace(project.Repo) != repo {
			continue
		}
		if config.ResolvedProjectProviderKind(*cfg, project) != config.ProviderKindForgejo {
			return config.ProviderConfig{}, false, nil
		}
		provider, ok := forgejoProviderByID(*cfg, project.Provider)
		if !ok {
			return config.ProviderConfig{}, false, fmt.Errorf("forgejo provider %q not configured for repo %s", project.Provider, repo)
		}
		return provider, true, nil
	}
	return config.ProviderConfig{}, false, nil
}

func forgejoReviewerDiscoveryLabelsForRepo(cfg *config.Config, repo string) []string {
	if cfg == nil {
		return nil
	}
	repo = strings.TrimSpace(repo)
	for _, project := range cfg.Projects {
		if strings.TrimSpace(project.Repo) != repo {
			continue
		}
		if config.ResolvedProjectProviderKind(*cfg, project) != config.ProviderKindForgejo {
			return nil
		}
		labels := config.ProjectRoleConfigs(*cfg, project.ID).Reviewer.Discovery.Triggers.Labels
		result := make([]string, 0, len(labels))
		for _, label := range labels {
			label = strings.TrimSpace(label)
			if label == "" {
				continue
			}
			result = append(result, label)
		}
		return result
	}
	return nil
}

func forgejoProjectProviderForCWD(cfg *config.Config, cwd string) (config.ProjectRefConfig, config.ProviderConfig, bool, error) {
	if cfg == nil {
		return config.ProjectRefConfig{}, config.ProviderConfig{}, false, nil
	}
	cwd = strings.TrimSpace(cwd)
	for _, project := range cfg.Projects {
		if strings.TrimSpace(project.RepoPath) != cwd {
			continue
		}
		if config.ResolvedProjectProviderKind(*cfg, project) != config.ProviderKindForgejo {
			return config.ProjectRefConfig{}, config.ProviderConfig{}, false, nil
		}
		provider, ok := forgejoProviderByID(*cfg, project.Provider)
		if !ok {
			return config.ProjectRefConfig{}, config.ProviderConfig{}, false, fmt.Errorf("forgejo provider %q not configured for project %s", project.Provider, project.ID)
		}
		if strings.TrimSpace(project.Repo) == "" {
			return config.ProjectRefConfig{}, config.ProviderConfig{}, false, fmt.Errorf("forgejo project %s is missing repo", project.ID)
		}
		return project, provider, true, nil
	}
	return config.ProjectRefConfig{}, config.ProviderConfig{}, false, nil
}

func forgejoProviderByID(cfg config.Config, providerID string) (config.ProviderConfig, bool) {
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

// planeClientForRepo returns a Plane task-source client for a project whose
// provider kind is "plane" and whose code repo (project.Repo, a GitHub repo)
// matches repo. The second return is false when repo is not a plane project, in
// which case callers fall through to the GitHub/forgejo path.
func planeClientForRepo(cfg *config.Config, repo string) (*forge.PlaneClient, bool, error) {
	provider, codeRepo, ok, err := planeProviderForRepo(cfg, repo)
	if !ok || err != nil {
		return nil, ok, err
	}
	client, err := forge.NewPlaneClientFromConfig(provider, codeRepo)
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

func planeClientForCWD(cfg *config.Config, cwd string) (*forge.PlaneClient, bool, error) {
	project, provider, ok, err := planeProjectProviderForCWD(cfg, cwd)
	if !ok || err != nil {
		return nil, ok, err
	}
	client, err := forge.NewPlaneClientFromConfig(provider, strings.TrimSpace(project.Repo))
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

func planeProviderForRepo(cfg *config.Config, repo string) (config.ProviderConfig, string, bool, error) {
	if cfg == nil {
		return config.ProviderConfig{}, "", false, nil
	}
	repo = strings.TrimSpace(repo)
	for _, project := range cfg.Projects {
		if strings.TrimSpace(project.Repo) != repo {
			continue
		}
		if config.ResolvedProjectProviderKind(*cfg, project) != config.ProviderKindPlane {
			return config.ProviderConfig{}, "", false, nil
		}
		provider, ok := forgejoProviderByID(*cfg, project.Provider)
		if !ok {
			return config.ProviderConfig{}, "", false, fmt.Errorf("plane provider %q not configured for repo %s", project.Provider, repo)
		}
		return provider, strings.TrimSpace(project.Repo), true, nil
	}
	return config.ProviderConfig{}, "", false, nil
}

func planeProjectProviderForCWD(cfg *config.Config, cwd string) (config.ProjectRefConfig, config.ProviderConfig, bool, error) {
	if cfg == nil {
		return config.ProjectRefConfig{}, config.ProviderConfig{}, false, nil
	}
	cwd = strings.TrimSpace(cwd)
	for _, project := range cfg.Projects {
		if strings.TrimSpace(project.RepoPath) != cwd {
			continue
		}
		if config.ResolvedProjectProviderKind(*cfg, project) != config.ProviderKindPlane {
			return config.ProjectRefConfig{}, config.ProviderConfig{}, false, nil
		}
		provider, ok := forgejoProviderByID(*cfg, project.Provider)
		if !ok {
			return config.ProjectRefConfig{}, config.ProviderConfig{}, false, fmt.Errorf("plane provider %q not configured for project %s", project.Provider, project.ID)
		}
		if strings.TrimSpace(project.Repo) == "" {
			return config.ProjectRefConfig{}, config.ProviderConfig{}, false, fmt.Errorf("plane project %s is missing repo", project.ID)
		}
		return project, provider, true, nil
	}
	return config.ProjectRefConfig{}, config.ProviderConfig{}, false, nil
}

func forgeIdentityLogins(identities []forge.Identity) []string {
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

func forgeLabelNames(labels []forge.Label) []string {
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

func appendLabels(label string, labels []string) []string {
	result := make([]string, 0, len(labels)+1)
	if label = strings.TrimSpace(label); label != "" {
		result = append(result, label)
	}
	result = append(result, labels...)
	return result
}

func forgeNetworkPolicyUsers(users []forge.Identity) []networkpolicy.GitHubUser {
	if users == nil {
		return nil
	}
	converted := make([]networkpolicy.GitHubUser, 0, len(users))
	for _, user := range users {
		converted = append(converted, networkpolicy.GitHubUser{Login: user.Login, ID: user.ID})
	}
	return converted
}

func (a plannerGitHubAdapter) forgejo(ctx context.Context, repo string) (*forge.ForgejoClient, bool, error) {
	client, ok, err := forgejoClientForRepo(a.config, repo)
	return client, ok, err
}

// plane returns a Plane task-source client when repo belongs to a plane-kind
// project. Issue-side reads/mutations for such projects are served by Plane;
// pull-request operations are left to the GitHub path (repo is the code repo).
func (a plannerGitHubAdapter) plane(ctx context.Context, repo string) (*forge.PlaneClient, bool, error) {
	client, ok, err := planeClientForRepo(a.config, repo)
	return client, ok, err
}

func (a plannerGitHubAdapter) ListOpenIssues(ctx context.Context, input planner.ListOpenIssuesInput) ([]planner.IssueSummary, error) {
	if client, ok, err := a.plane(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return nil, err
		}
		issues, err := client.ListOpenIssues(ctx, forge.ListIssuesInput{Labels: input.Labels, Assignee: input.Assignee, Limit: input.Limit})
		if err != nil {
			return nil, err
		}
		result := make([]planner.IssueSummary, 0, len(issues))
		for _, issue := range issues {
			result = append(result, planner.IssueSummary{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.HTMLURL, Assignees: forgeIdentityLogins(issue.Assignees), Labels: forgeLabelNames(issue.Labels)})
		}
		return result, nil
	}
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return nil, err
		}
		issues, err := client.ListOpenIssues(ctx, forge.ListIssuesInput{Labels: input.Labels, Assignee: input.Assignee, Limit: input.Limit})
		if err != nil {
			return nil, err
		}
		result := make([]planner.IssueSummary, 0, len(issues))
		for _, issue := range issues {
			result = append(result, planner.IssueSummary{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.HTMLURL, Assignees: forgeIdentityLogins(issue.Assignees), Labels: forgeLabelNames(issue.Labels)})
		}
		return result, nil
	}
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	issues, err := a.gateway.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Assignee: input.Assignee, Label: input.Label, Labels: input.Labels})
	if err != nil {
		return nil, err
	}
	result := make([]planner.IssueSummary, 0, len(issues))
	for _, issue := range issues {
		result = append(result, planner.IssueSummary{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, Assignees: issue.Assignees, Labels: issue.Labels})
	}
	return result, nil
}

func (a plannerGitHubAdapter) ViewIssue(ctx context.Context, input planner.ViewIssueInput) (planner.IssueDetail, error) {
	if client, ok, err := a.plane(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return planner.IssueDetail{}, err
		}
		issue, err := client.ViewIssue(ctx, input.IssueNumber)
		if err != nil {
			return planner.IssueDetail{}, err
		}
		return planner.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.HTMLURL, Assignees: forgeIdentityLogins(issue.Assignees), Labels: forgeLabelNames(issue.Labels)}, nil
	}
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return planner.IssueDetail{}, err
		}
		issue, err := client.ViewIssue(ctx, input.IssueNumber)
		if err != nil {
			return planner.IssueDetail{}, err
		}
		return planner.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.HTMLURL, Assignees: forgeIdentityLogins(issue.Assignees), Labels: forgeLabelNames(issue.Labels)}, nil
	}
	if a.gateway == nil {
		return planner.IssueDetail{}, fmt.Errorf("github gateway is not configured")
	}
	issue, err := a.gateway.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: input.Repo, IssueNumber: input.IssueNumber, CWD: input.CWD})
	if err != nil {
		return planner.IssueDetail{}, err
	}
	return planner.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, Assignees: issue.Assignees, Labels: issue.Labels}, nil
}

func (a plannerGitHubAdapter) GetCurrentUserLogin(ctx context.Context, cwd string) (string, error) {
	if client, ok, err := planeClientForCWD(a.config, cwd); ok || err != nil {
		if err != nil {
			return "", err
		}
		identity, err := client.CurrentUser(ctx)
		return identity.Login, err
	}
	if client, ok, err := forgejoClientForCWD(a.config, cwd); ok || err != nil {
		if err != nil {
			return "", err
		}
		identity, err := client.CurrentUser(ctx)
		return identity.Login, err
	}
	if a.gateway == nil {
		return "", fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.GetCurrentUserLogin(ctx, cwd)
}

func (a plannerGitHubAdapter) AddIssueAssignees(ctx context.Context, input planner.IssueAssigneesInput) error {
	if client, ok, err := a.plane(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		return client.AddIssueAssignees(ctx, input.IssueNumber, input.Assignees)
	}
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		return client.AddIssueAssignees(ctx, input.IssueNumber, input.Assignees)
	}
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.AddIssueAssignees(ctx, githubinfra.IssueAssigneesInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Assignees: input.Assignees, CWD: input.CWD})
}

func networkPolicyUsers(users []githubinfra.GitHubUser) []networkpolicy.GitHubUser {
	if users == nil {
		return nil
	}
	converted := make([]networkpolicy.GitHubUser, 0, len(users))
	for _, user := range users {
		converted = append(converted, networkpolicy.GitHubUser{Login: user.Login, ID: user.ID})
	}
	return converted
}

func (a plannerGitHubAdapter) ListOpenPullRequests(ctx context.Context, input planner.ListOpenPullRequestsInput) ([]planner.PullRequestSummary, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return nil, err
		}
		pullRequests, err := client.ListOpenPullRequests(ctx, forge.ListPullRequestsInput{Limit: input.Limit})
		if err != nil {
			return nil, err
		}
		result := make([]planner.PullRequestSummary, 0, len(pullRequests))
		for _, pr := range pullRequests {
			result = append(result, planner.PullRequestSummary{Number: pr.Number, URL: pr.HTMLURL, State: pr.State, HeadRefName: pr.Head.Name, BaseRefName: pr.Base.Name})
		}
		return result, nil
	}
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	pullRequests, err := a.gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit})
	if err != nil {
		return nil, err
	}
	result := make([]planner.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, planner.PullRequestSummary{Number: pr.Number, URL: pr.URL, State: pr.State, HeadRefName: pr.HeadRefName, BaseRefName: pr.BaseRefName})
	}
	return result, nil
}

func (a plannerGitHubAdapter) ViewPullRequest(ctx context.Context, input planner.ViewPullRequestInput) (planner.PullRequestDetail, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return planner.PullRequestDetail{}, err
		}
		pr, err := client.ViewPullRequest(ctx, input.PRNumber)
		if err != nil {
			return planner.PullRequestDetail{}, err
		}
		return planner.PullRequestDetail{Number: pr.Number, Title: pr.Title, Body: pr.Body, URL: pr.HTMLURL, State: pr.State, HeadRefName: pr.Head.Name, BaseRefName: pr.Base.Name}, nil
	}
	if a.gateway == nil {
		return planner.PullRequestDetail{}, fmt.Errorf("github gateway is not configured")
	}
	pr, err := a.gateway.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return planner.PullRequestDetail{}, err
	}
	return planner.PullRequestDetail{Number: pr.Number, Title: pr.Title, Body: pr.Body, URL: pr.URL, State: pr.State, HeadRefName: pr.HeadRefName, BaseRefName: pr.BaseRefName}, nil
}

func (a plannerGitHubAdapter) CreatePullRequest(ctx context.Context, input planner.CreatePullRequestInput) (planner.CreatePullRequestResult, error) {
	body := a.stamper.Markdown(input.Body, "planner", disclosure.ChannelPullRequest)
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return planner.CreatePullRequestResult{}, err
		}
		pr, err := client.CreatePullRequest(ctx, forge.CreatePullRequestInput{Title: input.Title, Body: body, Head: input.HeadBranch, Base: input.BaseBranch})
		if err != nil {
			return planner.CreatePullRequestResult{}, err
		}
		return planner.CreatePullRequestResult{Number: pr.Number, URL: pr.HTMLURL}, nil
	}
	if a.gateway == nil {
		return planner.CreatePullRequestResult{}, fmt.Errorf("github gateway is not configured")
	}
	pr, err := a.gateway.CreatePullRequest(ctx, githubinfra.CreatePullRequestInput{Repo: input.Repo, HeadBranch: input.HeadBranch, BaseBranch: input.BaseBranch, Title: input.Title, Body: body, CWD: input.CWD})
	if err != nil {
		return planner.CreatePullRequestResult{}, err
	}
	return planner.CreatePullRequestResult{Number: pr.Number, URL: pr.URL}, nil
}

func (a plannerGitHubAdapter) UpdatePullRequestBody(ctx context.Context, input planner.UpdatePullRequestBodyInput) error {
	body := a.stamper.Markdown(input.Body, "planner", disclosure.ChannelPullRequest)
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		_, err = client.UpdatePullRequest(ctx, forge.UpdatePullRequestInput{Number: input.PRNumber, Body: &body})
		return err
	}
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.UpdatePullRequestBody(ctx, githubinfra.UpdatePullRequestBodyInput{Repo: input.Repo, PRNumber: input.PRNumber, Body: body, CWD: input.CWD})
}

func (a plannerGitHubAdapter) AddPullRequestLabels(ctx context.Context, input planner.PullRequestLabelsInput) error {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		_, err = client.AddIssueLabels(ctx, input.PRNumber, input.Labels)
		return err
	}
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a plannerGitHubAdapter) AddPullRequestReviewers(ctx context.Context, input planner.PullRequestReviewersInput) error {
	if _, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		return nil
	}
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.AddPullRequestReviewers(ctx, githubinfra.PullRequestReviewersInput{Repo: input.Repo, PRNumber: input.PRNumber, Reviewers: input.Reviewers, CWD: input.CWD})
}

type plannerGitAdapter struct {
	gateway *gitinfra.Gateway
	stamper disclosure.Stamper
}

func (a plannerGitAdapter) CreateWorktree(ctx context.Context, input planner.CreateWorktreeInput) (planner.CreateWorktreeResult, error) {
	worktree, err := a.gateway.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, Branch: input.Branch, BaseBranch: input.BaseBranch, ProtectedBranches: input.ProtectedBranches})
	if err != nil {
		return planner.CreateWorktreeResult{}, err
	}
	return planner.CreateWorktreeResult{ID: worktree.ID, WorktreePath: worktree.WorktreePath, Branch: worktree.Branch, BaseBranch: derefString(worktree.BaseBranch)}, nil
}

func (a plannerGitAdapter) InspectHead(ctx context.Context, input planner.InspectHeadInput) (planner.InspectHeadResult, error) {
	result, err := a.gateway.InspectHead(ctx, gitinfra.InspectHeadInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, BaseRef: input.BaseRef})
	if err != nil {
		return planner.InspectHeadResult{}, err
	}
	return planner.InspectHeadResult{HeadSHA: result.HeadSHA, NewCommitSHAs: result.NewCommitSHAs, HasUncommittedChanges: result.HasUncommittedChanges, ChangedFiles: result.ChangedFiles}, nil
}

func (a plannerGitAdapter) Commit(ctx context.Context, input planner.CommitInput) (planner.CommitResult, error) {
	message := a.stamper.CommitMessage(input.Message, "planner")
	result, err := a.gateway.Commit(ctx, gitinfra.CommitInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Message: message})
	if err != nil {
		return planner.CommitResult{}, err
	}
	return planner.CommitResult{CommitSHA: result.CommitSHA}, nil
}

func (a plannerGitAdapter) Push(ctx context.Context, input planner.PushInput) error {
	return a.gateway.Push(ctx, gitinfra.PushInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, Remote: input.Remote, ProtectedBranches: input.ProtectedBranches})
}

type plannerAgentExecutorAdapter struct{ executor *agent.ConfiguredExecutor }
type plannerAgentExecutionAdapter struct{ execution agent.Execution }

func (a plannerAgentExecutorAdapter) Start(ctx context.Context, input planner.AgentRunInput) (planner.AgentExecution, error) {
	execution, err := a.executor.Start(ctx, agent.RunInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Prompt: input.Prompt, WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, HeartbeatTimeout: input.HeartbeatTimeout, Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	return plannerAgentExecutionAdapter{execution: execution}, nil
}

func (a plannerAgentExecutionAdapter) Wait(ctx context.Context) (planner.AgentResult, error) {
	result, err := a.execution.Wait(ctx)
	if err != nil {
		return planner.AgentResult{}, err
	}
	return planner.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, Commits: result.Commits, Lifecycle: result.Lifecycle, TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds, ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt}, nil
}

type reviewerGitHubAdapter struct {
	gateway *githubinfra.Gateway
	stamper disclosure.Stamper
	config  *config.Config
}

func (a reviewerGitHubAdapter) forgejo(ctx context.Context, repo string) (*forge.ForgejoClient, bool, error) {
	client, ok, err := forgejoClientForRepo(a.config, repo)
	return client, ok, err
}

func (a reviewerGitHubAdapter) ListOpenPullRequests(ctx context.Context, input reviewer.ListOpenPullRequestsInput) ([]reviewer.PullRequestSummary, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return nil, err
		}
		labels := append([]string(nil), input.Labels...)
		if len(labels) == 0 && strings.TrimSpace(input.Label) != "" {
			labels = []string{strings.TrimSpace(input.Label)}
		}
		pullRequests, err := client.ListOpenPullRequests(ctx, forge.ListPullRequestsInput{Labels: labels, Limit: input.Limit})
		if err != nil {
			return nil, err
		}
		result := make([]reviewer.PullRequestSummary, 0, len(pullRequests))
		for _, pr := range pullRequests {
			result = append(result, reviewer.PullRequestSummary{Number: pr.Number, Title: pr.Title, State: pr.State, IsDraft: pr.IsDraft, Labels: forgeLabelNames(pr.Labels), HeadSHA: pr.Head.SHA, BaseSHA: pr.Base.SHA, Author: pr.User.Login})
		}
		return result, nil
	}
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	pullRequests, err := a.gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Label: input.Label, Labels: input.Labels})
	if err != nil {
		return nil, err
	}
	result := make([]reviewer.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, reviewer.PullRequestSummary{Number: pr.Number, Title: pr.Title, State: pr.State, IsDraft: pr.IsDraft, ReviewDecision: pr.ReviewDecision, Labels: pr.Labels, HeadSHA: pr.HeadSHA, BaseSHA: pr.BaseSHA, HasConflicts: pr.HasConflicts, Author: pr.Author, ReviewRequests: pr.ReviewRequests, ReviewRequestUsers: networkPolicyUsers(pr.ReviewRequestUsers), Reviews: pr.Reviews})
	}
	return result, nil
}

func (a reviewerGitHubAdapter) ListReviewRequestedPullRequests(ctx context.Context, input reviewer.ListReviewRequestedPullRequestsInput) ([]reviewer.PullRequestSummary, error) {
	if _, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("forgejo reviewer does not support review-request discovery")
	}
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	pullRequests, err := a.gateway.ListReviewRequestedPullRequests(ctx, githubinfra.ListReviewRequestedPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Reviewer: input.Reviewer})
	if err != nil {
		return nil, err
	}
	result := make([]reviewer.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, reviewer.PullRequestSummary{Number: pr.Number, Title: pr.Title, State: pr.State, IsDraft: pr.IsDraft, ReviewDecision: pr.ReviewDecision, Labels: pr.Labels, HeadSHA: pr.HeadSHA, BaseSHA: pr.BaseSHA, HasConflicts: pr.HasConflicts, Author: pr.Author, ReviewRequests: pr.ReviewRequests, ReviewRequestUsers: networkPolicyUsers(pr.ReviewRequestUsers), Reviews: pr.Reviews})
	}
	return result, nil
}

func (a reviewerGitHubAdapter) GetCurrentUserLogin(ctx context.Context, cwd string) (string, error) {
	if client, ok, err := forgejoClientForCWD(a.config, cwd); ok || err != nil {
		if err != nil {
			return "", err
		}
		identity, err := client.CurrentUser(ctx)
		return identity.Login, err
	}
	if a.gateway == nil {
		return "", fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.GetCurrentUserLogin(ctx, cwd)
}

func (a reviewerGitHubAdapter) ViewPullRequest(ctx context.Context, input reviewer.ViewPullRequestInput) (reviewer.PullRequestDetail, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return reviewer.PullRequestDetail{}, err
		}
		pr, err := client.ViewPullRequest(ctx, input.PRNumber)
		if err != nil {
			return reviewer.PullRequestDetail{}, err
		}
		diff, err := client.PullRequestDiff(ctx, input.PRNumber)
		if err != nil {
			return reviewer.PullRequestDetail{}, err
		}
		comments, err := client.ListIssueComments(ctx, input.PRNumber)
		if err != nil {
			return reviewer.PullRequestDetail{}, err
		}
		return reviewer.PullRequestDetail{Number: pr.Number, Title: pr.Title, Body: pr.Body, State: pr.State, IsDraft: pr.IsDraft, Labels: forgeLabelNames(pr.Labels), HeadSHA: pr.Head.SHA, BaseSHA: pr.Base.SHA, HeadRefName: pr.Head.Name, BaseRefName: pr.Base.Name, Author: pr.User.Login, Diff: diff, IssueComments: forgeCommentsToObjects(comments)}, nil
	}
	if a.gateway == nil {
		return reviewer.PullRequestDetail{}, fmt.Errorf("github gateway is not configured")
	}
	detail, err := a.gateway.ViewPullRequestForReviewer(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return reviewer.PullRequestDetail{}, err
	}
	return reviewer.PullRequestDetail{Number: detail.Number, Title: detail.Title, Body: detail.Body, State: detail.State, IsDraft: detail.IsDraft, ReviewDecision: detail.ReviewDecision, Labels: detail.Labels, HeadSHA: detail.HeadSHA, BaseSHA: detail.BaseSHA, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, Author: detail.Author, ReviewRequests: detail.ReviewRequests, ReviewRequestUsers: networkPolicyUsers(detail.ReviewRequestUsers), HasConflicts: detail.HasConflicts, ChecksSummary: summarizeCheckStates(detail.Checks), Comments: detail.Comments, IssueComments: commentInfosToObjects(detail.IssueComments), Reviews: detail.Reviews}, nil
}

func (a reviewerGitHubAdapter) ViewIssue(ctx context.Context, input githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error) {
	return a.gateway.ViewIssue(ctx, input)
}

func (a reviewerGitHubAdapter) GetRepositorySettings(ctx context.Context, input githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error) {
	return a.gateway.GetRepositorySettings(ctx, input)
}

func (a reviewerGitHubAdapter) GetBranchProtection(ctx context.Context, input githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error) {
	return a.gateway.GetBranchProtection(ctx, input)
}

func (a reviewerGitHubAdapter) GetPullRequestHeadSHA(ctx context.Context, input reviewer.ViewPullRequestInput) (string, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return "", err
		}
		pr, err := client.ViewPullRequest(ctx, input.PRNumber)
		return pr.Head.SHA, err
	}
	if a.gateway == nil {
		return "", fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.GetPullRequestHeadSHA(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) CapturePullRequestSnapshot(ctx context.Context, input reviewer.CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return storage.PullRequestSnapshotRecord{}, err
		}
		pr, err := client.ViewPullRequest(ctx, input.PRNumber)
		if err != nil {
			return storage.PullRequestSnapshotRecord{}, err
		}
		diff, err := client.PullRequestDiff(ctx, input.PRNumber)
		if err != nil {
			return storage.PullRequestSnapshotRecord{}, err
		}
		payloadJSON, err := json.Marshal(map[string]any{"diff": diff})
		if err != nil {
			return storage.PullRequestSnapshotRecord{}, err
		}
		baseSHA := strings.TrimSpace(pr.Base.SHA)
		return storage.PullRequestSnapshotRecord{
			ID:          fmt.Sprintf("snapshot:%d:%s", input.PRNumber, input.CapturedAt),
			ProjectID:   input.ProjectID,
			Repo:        input.Repo,
			PRNumber:    input.PRNumber,
			HeadSHA:     pr.Head.SHA,
			BaseSHA:     stringPtr(baseSHA),
			Title:       stringPtr(pr.Title),
			Body:        stringPtr(pr.Body),
			Author:      stringPtr(pr.User.Login),
			PayloadJSON: stringPtr(string(payloadJSON)),
			CapturedAt:  input.CapturedAt,
			CreatedAt:   input.CapturedAt,
		}, nil
	}
	if a.gateway == nil {
		return storage.PullRequestSnapshotRecord{}, fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.CapturePullRequestSnapshot(ctx, githubinfra.CapturePullRequestSnapshotInput{ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, CapturedAt: input.CapturedAt})
}

func (a reviewerGitHubAdapter) FindReviewMarker(ctx context.Context, input reviewer.VerifyReviewMarkerInput) (reviewer.ReviewMarkerResult, error) {
	allowedReviewEvents := make([]string, 0, len(input.AllowedReviewEvents))
	for _, event := range input.AllowedReviewEvents {
		allowedReviewEvents = append(allowedReviewEvents, string(event))
	}
	marker, err := a.gateway.FindReviewMarker(ctx, githubinfra.VerifyReviewMarkerInput{Repo: input.Repo, PRNumber: input.PRNumber, Marker: input.Marker, AllowedReviewEvents: allowedReviewEvents, AuthorLogin: input.AuthorLogin, AllowCleanComment: input.AllowCleanComment, CWD: input.CWD})
	if err != nil {
		return reviewer.ReviewMarkerResult{}, err
	}
	return reviewer.ReviewMarkerResult{Found: marker.Found, Outcome: marker.Outcome, Event: reviewer.ReviewEvent(marker.Event), AuthorLogin: marker.AuthorLogin, Body: marker.Body, InlineCommentBodies: append([]string(nil), marker.InlineCommentBodies...)}, nil
}

func (a reviewerGitHubAdapter) CreateIssueComment(ctx context.Context, input reviewer.IssueCommentInput) (reviewer.IssueCommentResult, error) {
	body := a.stamper.Markdown(input.Body, "reviewer", disclosure.ChannelIssueComment)
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return reviewer.IssueCommentResult{}, err
		}
		comment, err := client.CreateIssueComment(ctx, forge.CreateCommentInput{IssueNumber: input.IssueNumber, Body: body})
		if err != nil {
			return reviewer.IssueCommentResult{}, err
		}
		return reviewer.IssueCommentResult{ID: comment.ID, URL: comment.HTMLURL}, nil
	}
	if a.gateway == nil {
		return reviewer.IssueCommentResult{}, fmt.Errorf("github gateway is not configured")
	}
	comment, err := a.gateway.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Body: body, CWD: input.CWD})
	if err != nil {
		return reviewer.IssueCommentResult{}, err
	}
	return reviewer.IssueCommentResult{ID: comment.ID, URL: comment.URL}, nil
}

func (a reviewerGitHubAdapter) ListIssueComments(ctx context.Context, input reviewer.ViewPullRequestInput) ([]reviewer.IssueComment, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return nil, err
		}
		comments, err := client.ListIssueComments(ctx, input.PRNumber)
		if err != nil {
			return nil, err
		}
		out := make([]reviewer.IssueComment, 0, len(comments))
		for _, comment := range comments {
			out = append(out, reviewer.IssueComment{ID: comment.ID, Body: comment.Body})
		}
		return out, nil
	}
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	comments, err := a.gateway.ListIssueComments(ctx, githubinfra.ViewIssueInput{Repo: input.Repo, IssueNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return nil, err
	}
	out := make([]reviewer.IssueComment, 0, len(comments))
	for _, comment := range comments {
		out = append(out, reviewer.IssueComment{ID: comment.ID, Body: comment.Body})
	}
	return out, nil
}

func (a reviewerGitHubAdapter) UpdateIssueComment(ctx context.Context, input reviewer.UpdateIssueCommentInput) error {
	body := a.stamper.Markdown(input.Body, "reviewer", disclosure.ChannelIssueComment)
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		_, err = client.UpdateIssueComment(ctx, forge.UpdateCommentInput{CommentID: input.CommentID, Body: body})
		return err
	}
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: input.Repo, CommentID: input.CommentID, Body: body, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) SubmitReview(ctx context.Context, input githubinfra.SubmitReviewInput) error {
	return a.gateway.SubmitReview(ctx, input)
}

func (a reviewerGitHubAdapter) EnableAutoMerge(ctx context.Context, input githubinfra.EnableAutoMergeInput) error {
	return a.gateway.EnableAutoMerge(ctx, input)
}

func (a reviewerGitHubAdapter) AddPullRequestReaction(ctx context.Context, input reviewer.PullRequestReactionInput) error {
	return a.gateway.AddPullRequestReaction(ctx, githubinfra.PullRequestReactionInput{Repo: input.Repo, PRNumber: input.PRNumber, Content: input.Content, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) RemovePullRequestReaction(ctx context.Context, input reviewer.PullRequestReactionInput) error {
	return a.gateway.RemovePullRequestReaction(ctx, githubinfra.PullRequestReactionInput{Repo: input.Repo, PRNumber: input.PRNumber, Content: input.Content, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) AddPullRequestLabels(ctx context.Context, input reviewer.PullRequestLabelsInput) error {
	return a.gateway.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) RemovePullRequestLabels(ctx context.Context, input reviewer.PullRequestLabelsInput) error {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
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
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) RemoveIssueLabels(ctx context.Context, input githubinfra.IssueLabelsInput) error {
	return a.gateway.RemoveIssueLabels(ctx, input)
}

func (a reviewerGitHubAdapter) ListReviewThreads(ctx context.Context, input reviewer.ListReviewThreadsInput) ([]reviewer.ReviewThread, error) {
	threads, err := a.gateway.ListReviewThreads(ctx, githubinfra.ListReviewThreadsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, Limit: input.Limit})
	if err != nil {
		return nil, err
	}
	out := make([]reviewer.ReviewThread, 0, len(threads))
	for _, thread := range threads {
		converted := reviewer.ReviewThread{ID: thread.ID, IsResolved: thread.IsResolved, Path: thread.Path, Line: thread.Line, URL: thread.URL}
		for _, comment := range thread.Comments {
			converted.Comments = append(converted.Comments, reviewer.ReviewThreadComment{ID: comment.ID, Body: comment.Body, Author: comment.Author, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt, Path: comment.Path, Line: comment.Line, OriginalCommitOID: comment.OriginalCommitOID, CommitOID: comment.CommitOID, URL: comment.URL})
		}
		out = append(out, converted)
	}
	return out, nil
}

func (a reviewerGitHubAdapter) AddReviewThreadReply(ctx context.Context, input reviewer.AddReviewThreadReplyInput) error {
	body := a.stamper.ReviewComment(input.Body, "reviewer")
	return a.gateway.AddReviewThreadReply(ctx, githubinfra.AddReviewThreadReplyInput{Repo: input.Repo, ThreadID: input.ThreadID, Body: body, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) ResolveReviewThread(ctx context.Context, input reviewer.ResolveReviewThreadInput) error {
	return a.gateway.ResolveReviewThread(ctx, githubinfra.ResolveReviewThreadInput{Repo: input.Repo, ThreadID: input.ThreadID, CWD: input.CWD})
}

type reviewerAgentExecutorAdapter struct{ executor *agent.ConfiguredExecutor }
type reviewerAgentExecutionAdapter struct{ execution agent.Execution }

type reviewerGitAdapter struct{ gateway *gitinfra.Gateway }

func (a reviewerGitAdapter) CreateWorktree(ctx context.Context, input reviewer.CreateWorktreeInput) (reviewer.CreateWorktreeResult, error) {
	worktree, err := a.gateway.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, Branch: input.Branch, BaseBranch: input.BaseBranch, PRNumber: input.PRNumber, ProtectedBranches: input.ProtectedBranches, CheckoutMode: gitinfra.CheckoutMode(input.CheckoutMode)})
	if err != nil {
		return reviewer.CreateWorktreeResult{}, err
	}
	return reviewer.CreateWorktreeResult{WorktreePath: worktree.WorktreePath, Branch: worktree.Branch, HeadSHA: derefString(worktree.HeadSHA)}, nil
}

func (a reviewerGitAdapter) PrepareWorktree(ctx context.Context, input reviewer.PrepareWorktreeInput) (reviewer.PrepareWorktreeResult, error) {
	result, err := a.gateway.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, Ref: input.Ref, ExpectedHeadSHA: input.ExpectedHeadSHA, Remote: input.Remote})
	if err != nil {
		return reviewer.PrepareWorktreeResult{}, err
	}
	return reviewer.PrepareWorktreeResult{HeadSHA: result.HeadSHA, Clean: result.Clean}, nil
}

func (a reviewerGitAdapter) CleanupWorktree(ctx context.Context, input reviewer.CleanupWorktreeInput) error {
	return a.gateway.CleanupWorktree(ctx, gitinfra.CleanupWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, ProtectedBranches: input.ProtectedBranches})
}

func (a reviewerAgentExecutorAdapter) Start(ctx context.Context, input reviewer.AgentRunInput) (reviewer.AgentExecution, error) {
	execution, err := a.executor.Start(ctx, agent.RunInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Prompt: input.Prompt, NativeResumePrompt: input.NativeResumePrompt, WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, HeartbeatTimeout: input.HeartbeatTimeout, Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	return reviewerAgentExecutionAdapter{execution: execution}, nil
}

func (a reviewerAgentExecutionAdapter) Wait(ctx context.Context) (reviewer.AgentResult, error) {
	result, err := a.execution.Wait(ctx)
	if err != nil {
		return reviewer.AgentResult{}, err
	}
	return reviewer.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, ParseStatus: result.ParseStatus, TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds, ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt}, nil
}

func (a reviewerAgentExecutionAdapter) Kill(reason string) error {
	return a.execution.Kill(reason)
}

type fixerGitHubAdapter struct {
	gateway *githubinfra.Gateway
	stamper disclosure.Stamper
	config  *config.Config
}

func (a fixerGitHubAdapter) forgejo(ctx context.Context, repo string) (*forge.ForgejoClient, bool, error) {
	client, ok, err := forgejoClientForRepo(a.config, repo)
	return client, ok, err
}

func (a fixerGitHubAdapter) forgejoForCWD(ctx context.Context, cwd string) (*forge.ForgejoClient, bool, error) {
	client, ok, err := forgejoClientForCWD(a.config, cwd)
	return client, ok, err
}

func (a fixerGitHubAdapter) ListOpenPullRequests(ctx context.Context, input fixer.ListOpenPullRequestsInput) ([]fixer.PullRequestSummary, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(input.Author) != "" {
			return nil, fmt.Errorf("forgejo fixer does not support author-filter discovery")
		}
		pullRequests, err := client.ListOpenPullRequests(ctx, forge.ListPullRequestsInput{Limit: input.Limit, Labels: appendLabels(input.Label, input.Labels)})
		if err != nil {
			return nil, err
		}
		result := make([]fixer.PullRequestSummary, 0, len(pullRequests))
		for _, pr := range pullRequests {
			if input.BaseRefName != "" && pr.Base.Name != input.BaseRefName {
				continue
			}
			result = append(result, fixer.PullRequestSummary{Number: pr.Number, State: pr.State, IsDraft: pr.IsDraft, Labels: forgeLabelNames(pr.Labels), BaseRefName: pr.Base.Name, HeadSHA: pr.Head.SHA, Author: pr.User.Login})
		}
		return result, nil
	}
	pullRequests, err := a.gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Author: input.Author, Label: input.Label, Labels: input.Labels, BaseRefName: input.BaseRefName})
	if err != nil {
		return nil, err
	}
	result := make([]fixer.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, fixer.PullRequestSummary{Number: pr.Number, State: pr.State, IsDraft: pr.IsDraft, Labels: pr.Labels, BaseRefName: pr.BaseRefName, HeadSHA: pr.HeadSHA, Author: pr.Author})
	}
	return result, nil
}

func (a fixerGitHubAdapter) GetCurrentUserLogin(ctx context.Context, cwd string) (string, error) {
	if client, ok, err := a.forgejoForCWD(ctx, cwd); ok || err != nil {
		if err != nil {
			return "", err
		}
		identity, err := client.CurrentUser(ctx)
		return identity.Login, err
	}
	return a.gateway.GetCurrentUserLogin(ctx, cwd)
}

func (a fixerGitHubAdapter) GetPullRequestAuthor(ctx context.Context, input fixer.ViewPullRequestInput) (string, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return "", err
		}
		pr, err := client.ViewPullRequest(ctx, input.PRNumber)
		if err != nil {
			return "", err
		}
		return pr.User.Login, nil
	}
	return a.gateway.GetPullRequestAuthor(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
}

func (a fixerGitHubAdapter) ViewPullRequest(ctx context.Context, input fixer.ViewPullRequestInput) (fixer.PullRequestDetail, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return fixer.PullRequestDetail{}, err
		}
		pr, err := client.ViewPullRequest(ctx, input.PRNumber)
		if err != nil {
			return fixer.PullRequestDetail{}, err
		}
		comments, err := client.ListIssueComments(ctx, input.PRNumber)
		if err != nil {
			return fixer.PullRequestDetail{}, err
		}
		return fixer.PullRequestDetail{Number: pr.Number, State: pr.State, IsDraft: pr.IsDraft, Labels: forgeLabelNames(pr.Labels), HeadSHA: pr.Head.SHA, HeadRefName: pr.Head.Name, BaseRefName: pr.Base.Name, BaseSHA: pr.Base.SHA, IssueComments: forgeCommentsToObjects(comments), Author: pr.User.Login}, nil
	}
	detail, err := a.gateway.ViewPullRequestForFixer(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return fixer.PullRequestDetail{}, err
	}
	return fixer.PullRequestDetail{Number: detail.Number, State: detail.State, IsDraft: detail.IsDraft, Labels: detail.Labels, HeadSHA: detail.HeadSHA, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, BaseSHA: detail.BaseSHA, ReviewDecision: detail.ReviewDecision, Comments: detail.Comments, IssueComments: commentInfosToObjects(detail.IssueComments), Checks: detail.Checks, HasConflicts: detail.HasConflicts, Author: detail.Author}, nil
}

func forgeCommentsToObjects(items []forge.Comment) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{
			"id":        item.ID,
			"author":    map[string]any{"login": item.User.Login},
			"body":      item.Body,
			"updatedAt": item.UpdatedAt,
			"url":       item.HTMLURL,
		})
	}
	return out
}

func commentInfosToObjects(items []githubinfra.CommentInfo) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{
			"id":                item.ID,
			"author":            map[string]any{"login": item.Author},
			"authorAssociation": item.AuthorAssociation,
			"body":              item.Body,
			"createdAt":         item.CreatedAt,
			"updatedAt":         item.UpdatedAt,
			"url":               item.URL,
		})
	}
	return out
}

func (a fixerGitHubAdapter) ListReviewThreads(ctx context.Context, input fixer.ListReviewThreadsInput) ([]fixer.ReviewThread, error) {
	if _, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("forgejo fixer does not support native review threads")
	}
	threads, err := a.gateway.ListReviewThreads(ctx, githubinfra.ListReviewThreadsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, Limit: input.Limit})
	if err != nil {
		return nil, err
	}
	out := make([]fixer.ReviewThread, 0, len(threads))
	for _, thread := range threads {
		comments := make([]fixer.ReviewThreadComment, 0, len(thread.Comments))
		for _, comment := range thread.Comments {
			comments = append(comments, fixer.ReviewThreadComment{ID: comment.ID, Body: comment.Body, Author: comment.Author, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt})
		}
		out = append(out, fixer.ReviewThread{ID: thread.ID, IsResolved: thread.IsResolved, Comments: comments})
	}
	return out, nil
}

func (a fixerGitHubAdapter) ViewReviewThread(ctx context.Context, input fixer.ViewReviewThreadInput) (fixer.ReviewThread, error) {
	if _, ok, err := a.forgejoForCWD(ctx, input.CWD); ok || err != nil {
		if err != nil {
			return fixer.ReviewThread{}, err
		}
		return fixer.ReviewThread{}, fmt.Errorf("forgejo fixer does not support native review threads")
	}
	thread, err := a.gateway.ViewReviewThread(ctx, githubinfra.ViewReviewThreadInput{ThreadID: input.ThreadID, CWD: input.CWD})
	if err != nil {
		return fixer.ReviewThread{}, err
	}
	comments := make([]fixer.ReviewThreadComment, 0, len(thread.Comments))
	for _, comment := range thread.Comments {
		comments = append(comments, fixer.ReviewThreadComment{ID: comment.ID, Body: comment.Body, Author: comment.Author, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt})
	}
	return fixer.ReviewThread{ID: thread.ID, IsResolved: thread.IsResolved, Comments: comments}, nil
}

func (a fixerGitHubAdapter) ResolveReviewThread(ctx context.Context, input fixer.ResolveReviewThreadInput) error {
	if _, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("forgejo fixer does not support native review thread resolution")
	}
	return a.gateway.ResolveReviewThread(ctx, githubinfra.ResolveReviewThreadInput{Repo: input.Repo, ThreadID: input.ThreadID, CWD: input.CWD})
}

func (a fixerGitHubAdapter) AddReviewThreadReply(ctx context.Context, input fixer.AddReviewThreadReplyInput) error {
	if _, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("forgejo fixer does not support native review thread replies")
	}
	body := a.stamper.ReviewComment(input.Body, "fixer")
	return a.gateway.AddReviewThreadReply(ctx, githubinfra.AddReviewThreadReplyInput{Repo: input.Repo, ThreadID: input.ThreadID, Body: body, CWD: input.CWD})
}

func (a fixerGitHubAdapter) CompareCommits(ctx context.Context, input fixer.CompareCommitsInput) (fixer.CompareCommitsResult, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return fixer.CompareCommitsResult{}, err
		}
		out, err := client.CompareBranches(ctx, forge.CompareBranchesInput{Base: input.Base, Head: input.Head})
		if err != nil {
			return fixer.CompareCommitsResult{}, err
		}
		return fixer.CompareCommitsResult{Status: out.Status}, nil
	}
	out, err := a.gateway.CompareCommits(ctx, githubinfra.CompareCommitsInput{Repo: input.Repo, Base: input.Base, Head: input.Head, CWD: input.CWD})
	if err != nil {
		return fixer.CompareCommitsResult{}, err
	}
	return fixer.CompareCommitsResult{Status: out.Status}, nil
}

func (a fixerGitHubAdapter) CreateIssueComment(ctx context.Context, input fixer.IssueCommentInput) (fixer.IssueCommentResult, error) {
	body := a.stamper.Markdown(input.Body, "fixer", disclosure.ChannelIssueComment)
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return fixer.IssueCommentResult{}, err
		}
		comment, err := client.CreateIssueComment(ctx, forge.CreateCommentInput{IssueNumber: input.IssueNumber, Body: body})
		if err != nil {
			return fixer.IssueCommentResult{}, err
		}
		return fixer.IssueCommentResult{ID: comment.ID, URL: comment.HTMLURL}, nil
	}
	comment, err := a.gateway.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Body: body, CWD: input.CWD})
	if err != nil {
		return fixer.IssueCommentResult{}, err
	}
	return fixer.IssueCommentResult{ID: comment.ID, URL: comment.URL}, nil
}

func (a fixerGitHubAdapter) UpdateIssueComment(ctx context.Context, input fixer.UpdateIssueCommentInput) error {
	body := a.stamper.Markdown(input.Body, "fixer", disclosure.ChannelIssueComment)
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		_, err = client.UpdateIssueComment(ctx, forge.UpdateCommentInput{CommentID: input.CommentID, Body: body})
		return err
	}
	return a.gateway.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: input.Repo, CommentID: input.CommentID, Body: body, CWD: input.CWD})
}

func (a fixerGitHubAdapter) AddPullRequestLabels(ctx context.Context, input fixer.PullRequestLabelsInput) error {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		_, err = client.AddIssueLabels(ctx, input.PRNumber, input.Labels)
		return err
	}
	return a.gateway.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a fixerGitHubAdapter) RemovePullRequestLabels(ctx context.Context, input fixer.PullRequestLabelsInput) error {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
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
	return a.gateway.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a fixerGitHubAdapter) AddPullRequestReviewers(ctx context.Context, input fixer.PullRequestReviewersInput) error {
	return a.gateway.AddPullRequestReviewers(ctx, githubinfra.PullRequestReviewersInput{Repo: input.Repo, PRNumber: input.PRNumber, Reviewers: input.Reviewers, CWD: input.CWD})
}

func (a fixerGitHubAdapter) ListPullRequestReviews(ctx context.Context, input fixer.ViewPullRequestInput) ([]fixer.ReviewSummary, error) {
	reviews, err := a.gateway.ListPullRequestReviews(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return nil, err
	}
	out := make([]fixer.ReviewSummary, 0, len(reviews))
	for _, rv := range reviews {
		out = append(out, fixer.ReviewSummary{ID: rv.ID, State: rv.State, Author: rv.Author, Body: rv.Body})
	}
	return out, nil
}

func (a fixerGitHubAdapter) DismissReview(ctx context.Context, input fixer.DismissReviewInput) error {
	return a.gateway.DismissReview(ctx, githubinfra.DismissReviewInput{Repo: input.Repo, PRNumber: input.PRNumber, ReviewID: input.ReviewID, Message: input.Message, CWD: input.CWD})
}

type fixerGitAdapter struct {
	gateway *gitinfra.Gateway
	stamper disclosure.Stamper
}

func (a fixerGitAdapter) CreateWorktree(ctx context.Context, input fixer.CreateWorktreeInput) (fixer.CreateWorktreeResult, error) {
	worktree, err := a.gateway.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, Branch: input.Branch, BaseBranch: input.BaseBranch, PRNumber: input.PRNumber, ProtectedBranches: input.ProtectedBranches, CheckoutMode: gitinfra.CheckoutMode(input.CheckoutMode)})
	if err != nil {
		return fixer.CreateWorktreeResult{}, err
	}
	return fixer.CreateWorktreeResult{WorktreePath: worktree.WorktreePath, Branch: worktree.Branch, HeadSHA: derefString(worktree.HeadSHA)}, nil
}

func (a fixerGitAdapter) PrepareWorktree(ctx context.Context, input fixer.PrepareWorktreeInput) (fixer.PrepareWorktreeResult, error) {
	result, err := a.gateway.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, ExpectedHeadSHA: input.ExpectedHeadSHA, Remote: input.Remote})
	if err != nil {
		return fixer.PrepareWorktreeResult{}, err
	}
	return fixer.PrepareWorktreeResult{HeadSHA: result.HeadSHA, Clean: result.Clean}, nil
}

func (a fixerGitAdapter) InspectHead(ctx context.Context, input fixer.InspectHeadInput) (fixer.InspectHeadResult, error) {
	result, err := a.gateway.InspectHead(ctx, gitinfra.InspectHeadInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, BaseRef: input.BaseRef})
	if err != nil {
		return fixer.InspectHeadResult{}, err
	}
	return fixer.InspectHeadResult{HeadSHA: result.HeadSHA, NewCommitSHAs: result.NewCommitSHAs, HasUncommittedChanges: result.HasUncommittedChanges, ChangedFiles: result.ChangedFiles}, nil
}

func (a fixerGitAdapter) Commit(ctx context.Context, input fixer.CommitInput) (fixer.CommitResult, error) {
	message := a.stamper.CommitMessage(input.Message, "fixer")
	result, err := a.gateway.Commit(ctx, gitinfra.CommitInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Message: message})
	if err != nil {
		return fixer.CommitResult{}, err
	}
	return fixer.CommitResult{CommitSHA: result.CommitSHA}, nil
}

func (a fixerGitAdapter) Push(ctx context.Context, input fixer.PushInput) error {
	return a.gateway.Push(ctx, gitinfra.PushInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, Remote: input.Remote, ExpectedRemoteHeadSHA: input.ExpectedRemoteHeadSHA, ProtectedBranches: input.ProtectedBranches})
}

func (a fixerGitAdapter) FetchBranch(ctx context.Context, repoPath, remote, branch string) error {
	return a.gateway.FetchBranch(ctx, repoPath, remote, branch)
}

func (a fixerGitAdapter) MergeBaseIntoWorktree(ctx context.Context, input fixer.MergeBaseInput) (fixer.MergeBaseResult, error) {
	res, err := a.gateway.MergeBaseIntoWorktree(ctx, gitinfra.MergeBaseInput{WorktreePath: input.WorktreePath, Remote: input.Remote, BaseBranch: input.BaseBranch})
	if err != nil {
		return fixer.MergeBaseResult{}, err
	}
	return fixer.MergeBaseResult{AlreadyUpToDate: res.AlreadyUpToDate, Conflicted: res.Conflicted}, nil
}

func (a fixerGitAdapter) IsAncestor(ctx context.Context, repoPath, ancestor, descendant string) (bool, error) {
	return a.gateway.IsAncestor(ctx, repoPath, ancestor, descendant)
}

func (a fixerGitAdapter) CleanupWorktree(ctx context.Context, input fixer.CleanupWorktreeInput) error {
	return a.gateway.CleanupWorktree(ctx, gitinfra.CleanupWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, ProtectedBranches: input.ProtectedBranches})
}

type fixerAgentExecutorAdapter struct{ executor *agent.ConfiguredExecutor }
type fixerAgentExecutionAdapter struct{ execution agent.Execution }

func (a fixerAgentExecutorAdapter) Start(ctx context.Context, input fixer.AgentRunInput) (fixer.AgentExecution, error) {
	execution, err := a.executor.Start(ctx, agent.RunInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Prompt: input.Prompt, WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, HeartbeatTimeout: input.HeartbeatTimeout, Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	return fixerAgentExecutionAdapter{execution: execution}, nil
}

func (a fixerAgentExecutionAdapter) Wait(ctx context.Context) (fixer.AgentResult, error) {
	result, err := a.execution.Wait(ctx)
	if err != nil {
		return fixer.AgentResult{}, err
	}
	return fixer.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, ParseStatus: result.ParseStatus, Lifecycle: result.Lifecycle, TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds, ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt}, nil
}

type workerGitHubAdapter struct {
	gateway *githubinfra.Gateway
	stamper disclosure.Stamper
	config  *config.Config
}

func (a workerGitHubAdapter) forgejo(ctx context.Context, repo string) (*forge.ForgejoClient, bool, error) {
	client, ok, err := forgejoClientForRepo(a.config, repo)
	return client, ok, err
}

// plane returns a Plane task-source client when repo belongs to a plane-kind
// project. The worker reads issues and posts issue-side comments/labels through
// Plane; pull-request creation stays on the GitHub code repo.
func (a workerGitHubAdapter) plane(ctx context.Context, repo string) (*forge.PlaneClient, bool, error) {
	client, ok, err := planeClientForRepo(a.config, repo)
	return client, ok, err
}

func (a workerGitHubAdapter) ListOpenPullRequests(ctx context.Context, input worker.ListOpenPullRequestsInput) ([]worker.PullRequestSummary, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
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
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	pullRequests, err := a.gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Label: input.Label})
	if err != nil {
		return nil, err
	}
	result := make([]worker.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, worker.PullRequestSummary{Number: pr.Number, URL: pr.URL, State: pr.State, HeadRefName: pr.HeadRefName, BaseRefName: pr.BaseRefName})
	}
	return result, nil
}

func (a workerGitHubAdapter) ListOpenIssues(ctx context.Context, input worker.ListOpenIssuesInput) ([]worker.IssueSummary, error) {
	if client, ok, err := a.plane(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return nil, err
		}
		issues, err := client.ListOpenIssues(ctx, forge.ListIssuesInput{Labels: input.Labels, Assignee: input.Assignee, Limit: input.Limit})
		if err != nil {
			return nil, err
		}
		result := make([]worker.IssueSummary, 0, len(issues))
		for _, issue := range issues {
			result = append(result, worker.IssueSummary{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.HTMLURL, Assignees: forgeIdentityLogins(issue.Assignees), AssigneeUsers: forgeNetworkPolicyUsers(issue.Assignees), Labels: forgeLabelNames(issue.Labels)})
		}
		return result, nil
	}
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return nil, err
		}
		issues, err := client.ListOpenIssues(ctx, forge.ListIssuesInput{Labels: input.Labels, Assignee: input.Assignee, Limit: input.Limit})
		if err != nil {
			return nil, err
		}
		result := make([]worker.IssueSummary, 0, len(issues))
		for _, issue := range issues {
			result = append(result, worker.IssueSummary{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.HTMLURL, Assignees: forgeIdentityLogins(issue.Assignees), AssigneeUsers: forgeNetworkPolicyUsers(issue.Assignees), Labels: forgeLabelNames(issue.Labels)})
		}
		return result, nil
	}
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	issues, err := a.gateway.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Assignee: input.Assignee, Label: input.Label, Labels: input.Labels})
	if err != nil {
		return nil, err
	}
	result := make([]worker.IssueSummary, 0, len(issues))
	for _, issue := range issues {
		result = append(result, worker.IssueSummary{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, Author: issue.Author, Assignees: issue.Assignees, AssigneeUsers: networkPolicyUsers(issue.AssigneeUsers), Labels: issue.Labels})
	}
	return result, nil
}

func (a workerGitHubAdapter) GetCurrentUserLogin(ctx context.Context, cwd string) (string, error) {
	if client, ok, err := planeClientForCWD(a.config, cwd); ok || err != nil {
		if err != nil {
			return "", err
		}
		identity, err := client.CurrentUser(ctx)
		return identity.Login, err
	}
	if client, ok, err := forgejoClientForCWD(a.config, cwd); ok || err != nil {
		if err != nil {
			return "", err
		}
		identity, err := client.CurrentUser(ctx)
		return identity.Login, err
	}
	if a.gateway == nil {
		return "", fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.GetCurrentUserLogin(ctx, cwd)
}

func (a workerGitHubAdapter) AddIssueAssignees(ctx context.Context, input worker.IssueAssigneesInput) error {
	if client, ok, err := a.plane(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		return client.AddIssueAssignees(ctx, input.IssueNumber, input.Assignees)
	}
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		return client.AddIssueAssignees(ctx, input.IssueNumber, input.Assignees)
	}
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.AddIssueAssignees(ctx, githubinfra.IssueAssigneesInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Assignees: input.Assignees, CWD: input.CWD})
}

func (a workerGitHubAdapter) ViewPullRequest(ctx context.Context, input worker.ViewPullRequestInput) (worker.PullRequestDetail, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return worker.PullRequestDetail{}, err
		}
		pr, err := client.ViewPullRequest(ctx, input.PRNumber)
		if err != nil {
			return worker.PullRequestDetail{}, err
		}
		return worker.PullRequestDetail{Number: pr.Number, Title: pr.Title, Body: pr.Body, URL: pr.HTMLURL, State: pr.State, HeadRefName: pr.Head.Name, BaseRefName: pr.Base.Name, HeadSHA: pr.Head.SHA}, nil
	}
	if a.gateway == nil {
		return worker.PullRequestDetail{}, fmt.Errorf("github gateway is not configured")
	}
	detail, err := a.gateway.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return worker.PullRequestDetail{}, err
	}
	return worker.PullRequestDetail{Number: detail.Number, Title: detail.Title, Body: detail.Body, URL: detail.URL, State: detail.State, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, HeadSHA: detail.HeadSHA, ReviewRequests: detail.ReviewRequests, ReviewRequestUsers: networkPolicyUsers(detail.ReviewRequestUsers)}, nil
}

func (a workerGitHubAdapter) ViewIssue(ctx context.Context, input worker.ViewIssueInput) (worker.IssueDetail, error) {
	if client, ok, err := a.plane(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return worker.IssueDetail{}, err
		}
		issue, err := client.ViewIssue(ctx, input.IssueNumber)
		if err != nil {
			return worker.IssueDetail{}, err
		}
		return worker.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.HTMLURL, State: issue.State, AssigneeUsers: forgeNetworkPolicyUsers(issue.Assignees), Labels: forgeLabelNames(issue.Labels)}, nil
	}
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return worker.IssueDetail{}, err
		}
		issue, err := client.ViewIssue(ctx, input.IssueNumber)
		if err != nil {
			return worker.IssueDetail{}, err
		}
		return worker.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.HTMLURL, State: issue.State, AssigneeUsers: forgeNetworkPolicyUsers(issue.Assignees), Labels: forgeLabelNames(issue.Labels)}, nil
	}
	if a.gateway == nil {
		return worker.IssueDetail{}, fmt.Errorf("github gateway is not configured")
	}
	issue, err := a.gateway.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: input.Repo, IssueNumber: input.IssueNumber, CWD: input.CWD})
	if err != nil {
		return worker.IssueDetail{}, err
	}
	return worker.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, State: issue.State, IsPullRequest: issue.IsPullRequest, AssigneeUsers: networkPolicyUsers(issue.AssigneeUsers), Labels: issue.Labels}, nil
}

func (a workerGitHubAdapter) CreateIssueComment(ctx context.Context, input worker.IssueCommentInput) (worker.IssueCommentResult, error) {
	body := a.stamper.Markdown(input.Body, "worker", disclosure.ChannelIssueComment)
	if client, ok, err := a.plane(ctx, input.Repo); ok || err != nil {
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
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return worker.IssueCommentResult{}, err
		}
		comment, err := client.CreateIssueComment(ctx, forge.CreateCommentInput{IssueNumber: input.IssueNumber, Body: body})
		if err != nil {
			return worker.IssueCommentResult{}, err
		}
		return worker.IssueCommentResult{ID: comment.ID, URL: comment.HTMLURL}, nil
	}
	if a.gateway == nil {
		return worker.IssueCommentResult{}, fmt.Errorf("github gateway is not configured")
	}
	comment, err := a.gateway.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Body: body, CWD: input.CWD})
	if err != nil {
		return worker.IssueCommentResult{}, err
	}
	return worker.IssueCommentResult{ID: comment.ID, URL: comment.URL}, nil
}

func (a workerGitHubAdapter) UpdateIssueComment(ctx context.Context, input worker.UpdateIssueCommentInput) error {
	body := a.stamper.Markdown(input.Body, "worker", disclosure.ChannelIssueComment)
	if _, ok, err := a.plane(ctx, input.Repo); ok || err != nil {
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
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		_, err = client.UpdateIssueComment(ctx, forge.UpdateCommentInput{CommentID: input.CommentID, Body: body})
		return err
	}
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: input.Repo, CommentID: input.CommentID, Body: body, CWD: input.CWD})
}

func (a workerGitHubAdapter) CreatePullRequest(ctx context.Context, input worker.CreatePullRequestInput) (worker.CreatePullRequestResult, error) {
	body := a.stamper.Markdown(input.Body, "worker", disclosure.ChannelPullRequest)
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return worker.CreatePullRequestResult{}, err
		}
		pr, err := client.CreatePullRequest(ctx, forge.CreatePullRequestInput{Title: input.Title, Body: body, Head: input.HeadBranch, Base: input.BaseBranch})
		if err != nil {
			return worker.CreatePullRequestResult{}, err
		}
		return worker.CreatePullRequestResult{Number: pr.Number, URL: pr.HTMLURL}, nil
	}
	if a.gateway == nil {
		return worker.CreatePullRequestResult{}, fmt.Errorf("github gateway is not configured")
	}
	pr, err := a.gateway.CreatePullRequest(ctx, githubinfra.CreatePullRequestInput{Repo: input.Repo, HeadBranch: input.HeadBranch, BaseBranch: input.BaseBranch, Title: input.Title, Body: body, Draft: input.Draft, CWD: input.CWD})
	if err != nil {
		return worker.CreatePullRequestResult{}, err
	}
	return worker.CreatePullRequestResult{Number: pr.Number, URL: pr.URL}, nil
}

func (a workerGitHubAdapter) AddPullRequestLabels(ctx context.Context, input worker.PullRequestLabelsInput) error {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		_, err := client.AddIssueLabels(ctx, input.PRNumber, input.Labels)
		return err
	}
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

// providerHasGitHubPullRequests reports whether a project of this provider kind
// has its pull requests on GitHub — so the coordinator/fixer PR-follow-up lanes
// apply. GitHub projects obviously do; Plane projects too (Plane is the task
// source, but the code + PRs live on the bound GitHub repo).
func providerHasGitHubPullRequests(kind config.ProviderKind) bool {
	return kind == config.ProviderKindGitHub || kind == config.ProviderKindPlane
}

// hitlGitHubSettings maps the HITL GitHub config into the worker's settings.
func hitlGitHubSettings(cfg *config.HITLGitHubConfig) worker.HITLGitHubSettings {
	if cfg == nil {
		return worker.HITLGitHubSettings{}
	}
	return worker.HITLGitHubSettings{
		AwaitingLabel: cfg.AwaitingLabel,
		MentionLogins: append([]string(nil), cfg.MentionLogins...),
	}
}

func (a workerGitHubAdapter) CompareBranches(ctx context.Context, input worker.CompareBranchesInput) (worker.CompareBranchesResult, error) {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return worker.CompareBranchesResult{}, err
		}
		comparison, err := client.CompareBranches(ctx, forge.CompareBranchesInput{Base: input.BaseBranch, Head: input.HeadBranch})
		if err != nil {
			return worker.CompareBranchesResult{}, err
		}
		return worker.CompareBranchesResult{AheadBy: comparison.AheadBy, BehindBy: comparison.BehindBy, Status: comparison.Status, TotalCommits: comparison.TotalCommits}, nil
	}
	if a.gateway == nil {
		return worker.CompareBranchesResult{}, fmt.Errorf("github gateway is not configured")
	}
	comparison, err := a.gateway.CompareBranches(ctx, githubinfra.CompareBranchesInput{Repo: input.Repo, BaseBranch: input.BaseBranch, HeadBranch: input.HeadBranch, CWD: input.CWD})
	if err != nil {
		return worker.CompareBranchesResult{}, err
	}
	return worker.CompareBranchesResult{AheadBy: comparison.AheadBy, BehindBy: comparison.BehindBy, Status: comparison.Status, TotalCommits: comparison.TotalCommits}, nil
}

func (a workerGitHubAdapter) UpdatePullRequestBody(ctx context.Context, input worker.UpdatePullRequestBodyInput) error {
	body := a.stamper.Markdown(input.Body, "worker", disclosure.ChannelPullRequest)
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		_, err = client.UpdatePullRequest(ctx, forge.UpdatePullRequestInput{Number: input.PRNumber, Body: &body})
		return err
	}
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.UpdatePullRequestBody(ctx, githubinfra.UpdatePullRequestBodyInput{Repo: input.Repo, PRNumber: input.PRNumber, Body: body, CWD: input.CWD})
}

func (a workerGitHubAdapter) UpdatePullRequestTitle(ctx context.Context, input worker.UpdatePullRequestTitleInput) error {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		_, err = client.UpdatePullRequest(ctx, forge.UpdatePullRequestInput{Number: input.PRNumber, Title: &input.Title})
		return err
	}
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.UpdatePullRequestTitle(ctx, githubinfra.UpdatePullRequestTitleInput{Repo: input.Repo, PRNumber: input.PRNumber, Title: input.Title, CWD: input.CWD})
}

func (a workerGitHubAdapter) RemovePullRequestLabels(ctx context.Context, input worker.PullRequestLabelsInput) error {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
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
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a workerGitHubAdapter) AddPullRequestReviewers(ctx context.Context, input worker.PullRequestReviewersInput) error {
	if client, ok, err := a.forgejo(ctx, input.Repo); ok || err != nil {
		if err != nil {
			return err
		}
		labels := forgejoReviewerDiscoveryLabelsForRepo(a.config, input.Repo)
		if len(labels) > 0 {
			if _, err := client.AddIssueLabels(ctx, input.PRNumber, labels); err != nil {
				return err
			}
		}
		return nil
	}
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.AddPullRequestReviewers(ctx, githubinfra.PullRequestReviewersInput{Repo: input.Repo, PRNumber: input.PRNumber, Reviewers: input.Reviewers, CWD: input.CWD})
}

type workerGitAdapter struct {
	gateway *gitinfra.Gateway
	stamper disclosure.Stamper
}

func (a workerGitAdapter) CreateWorktree(ctx context.Context, input worker.CreateWorktreeInput) (worker.CreateWorktreeResult, error) {
	worktree, err := a.gateway.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, Branch: input.Branch, BaseBranch: input.BaseBranch, PRNumber: input.PRNumber, ProtectedBranches: input.ProtectedBranches, CheckoutMode: gitinfra.CheckoutMode(input.CheckoutMode)})
	if err != nil {
		return worker.CreateWorktreeResult{}, err
	}
	return worker.CreateWorktreeResult{WorktreePath: worktree.WorktreePath, Branch: worktree.Branch, BaseBranch: derefString(worktree.BaseBranch), HeadSHA: derefString(worktree.HeadSHA), WorktreeID: worktree.ID}, nil
}

func (a workerGitAdapter) RestoreWorktree(ctx context.Context, input worker.RestoreWorktreeInput) (*worker.RestoreWorktreeResult, error) {
	worktree, err := a.gateway.RestoreWorktree(ctx, gitinfra.RestoreWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, Branch: input.Branch, WorktreeRoot: input.WorktreeRoot, CheckoutMode: gitinfra.CheckoutMode(input.CheckoutMode), ExpectedWorktreePath: input.ExpectedWorktreePath})
	if err != nil || worktree == nil {
		return nil, err
	}
	return &worker.RestoreWorktreeResult{WorktreePath: worktree.WorktreePath, Branch: worktree.Branch, BaseBranch: derefString(worktree.BaseBranch), HeadSHA: derefString(worktree.HeadSHA), WorktreeID: worktree.ID}, nil
}

func (a workerGitAdapter) PrepareWorktree(ctx context.Context, input worker.PrepareWorktreeInput) (worker.PrepareWorktreeResult, error) {
	result, err := a.gateway.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, ExpectedHeadSHA: input.ExpectedHeadSHA, Remote: input.Remote})
	if err != nil {
		return worker.PrepareWorktreeResult{}, err
	}
	return worker.PrepareWorktreeResult{HeadSHA: result.HeadSHA, Clean: result.Clean}, nil
}

func (a workerGitAdapter) InspectHead(ctx context.Context, input worker.InspectHeadInput) (worker.InspectHeadResult, error) {
	result, err := a.gateway.InspectHead(ctx, gitinfra.InspectHeadInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, BaseRef: input.BaseRef})
	if err != nil {
		return worker.InspectHeadResult{}, err
	}
	return worker.InspectHeadResult{HeadSHA: result.HeadSHA, NewCommitSHAs: result.NewCommitSHAs, HasUncommittedChanges: result.HasUncommittedChanges, ChangedFiles: result.ChangedFiles}, nil
}

func (a workerGitAdapter) Commit(ctx context.Context, input worker.CommitInput) (worker.CommitResult, error) {
	message := a.stamper.CommitMessage(input.Message, "worker")
	result, err := a.gateway.Commit(ctx, gitinfra.CommitInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Message: message})
	if err != nil {
		return worker.CommitResult{}, err
	}
	return worker.CommitResult{CommitSHA: result.CommitSHA}, nil
}

func (a workerGitAdapter) Push(ctx context.Context, input worker.PushInput) error {
	return a.gateway.Push(ctx, gitinfra.PushInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, Remote: input.Remote, ProtectedBranches: input.ProtectedBranches})
}

type workerAgentExecutorAdapter struct {
	executor *agent.ConfiguredExecutor
	registry *ActiveExecutionRegistry
}
type workerAgentExecutionAdapter struct {
	execution agent.Execution
}

func (a workerAgentExecutorAdapter) Start(ctx context.Context, input worker.AgentRunInput) (worker.AgentExecution, error) {
	execution, err := a.executor.Start(ctx, agent.RunInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Prompt: input.Prompt, NativeResumePrompt: input.NativeResumePrompt, NativeSessionID: input.NativeSessionID, WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, HeartbeatTimeout: input.HeartbeatTimeout, Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	unregister := func() {}
	if a.registry != nil {
		unregister = a.registry.Register(input.LoopID, input.RunID, input.ExecutionID, execution)
	}
	go func() {
		_, _ = execution.Wait(context.Background())
		unregister()
	}()
	return workerAgentExecutionAdapter{execution: execution}, nil
}

func (a workerAgentExecutionAdapter) Wait(ctx context.Context) (worker.AgentResult, error) {
	result, err := a.execution.Wait(ctx)
	if err != nil {
		return worker.AgentResult{}, err
	}
	return worker.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, ParseStatus: result.ParseStatus, ChangedFiles: result.ChangedFiles, Commits: result.Commits, Lifecycle: result.Lifecycle, TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds, ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt}, nil
}

func (a workerAgentExecutionAdapter) Kill(reason string) error {
	return a.execution.Kill(reason)
}

func buildDefaultSchedulerHandlers(cfg config.Config, logger bootstrap.Logger, coordinator *storage.SQLiteCoordinator, repos *storage.Repositories, gitGateway *gitinfra.Gateway, githubGateway *githubinfra.Gateway, activeExecutions *ActiveExecutionRegistry, asyncRunner func() schedulerAsyncRunner, requestWake func(), now func() time.Time, reconcileStaleRuns func(context.Context) (StaleRunReconcileSummary, error)) defaultSchedulerHandlers {
	if now == nil {
		now = time.Now
	}
	if repos == nil || coordinator == nil {
		fail := func(context.Context, Services) error {
			return fmt.Errorf("default scheduler dependencies are not configured")
		}
		return defaultSchedulerHandlers{tick: fail, claim: fail}
	}
	if cfg.Agent.Vendor == nil {
		noop := func(context.Context, Services) error { return nil }
		return defaultSchedulerHandlers{tick: noop, claim: noop}
	}
	notificationGateway := notify.NewGateway(notify.Options{
		Config:        cfg.Notifications,
		OsascriptPath: derefString(cfg.Tools.OsascriptPath),
		LogFilePath:   filepath.Join(cfg.Daemon.LogDir, "looperd.log"),
		Repositories:  repos,
		Now:           now,
	})
	// refreshFeishuAnchor re-renders a loop's thread-anchor card to reflect its
	// CURRENT status (colour + label), without disturbing the retained live tail.
	// The anchor is otherwise only patched opportunistically by the progress ticker
	// (OnProgress) while an agent runs — so a loop that finishes quickly, or leaves
	// awaiting_human on resume, would leave a stale "🔧 处理中 / ⏸ 等你定夺" header
	// forever. Calling this on every run start/finish makes the header converge to
	// the real status. App-mode only; a no-op otherwise.
	refreshFeishuAnchor := func(ctx context.Context, loopID string) {
		if strings.TrimSpace(loopID) == "" || !strings.EqualFold(strings.TrimSpace(cfg.Notifications.Webhook.Mode), "app") {
			return
		}
		notificationGateway.RefreshThreadHeader(ctx, loopID, nil, 0)
	}
	notifyAgentExecutionStarted := func(ctx context.Context, input agentExecutionNotificationInput) error {
		notificationGateway.Notify(ctx, notify.SystemNotificationPayload{
			ID:         input.ExecutionID,
			ProjectID:  input.ProjectID,
			LoopID:     input.LoopID,
			RunID:      input.RunID,
			Level:      "info",
			Title:      input.Title,
			Subtitle:   input.Subtitle,
			Body:       input.Body,
			EntityType: "agent_execution",
			EntityID:   input.ExecutionID,
			DedupeKey:  input.DedupeKey,
		})
		refreshFeishuAnchor(ctx, input.LoopID)
		return nil
	}
	notifyWorkerRunCompleted := func(ctx context.Context, input workerRunCompletedNotificationInput) error {
		workerNotificationKeyID := runtimeFirstNonEmpty(input.RunID, input.LoopID)
		payload := notify.SystemNotificationPayload{
			ProjectID:  input.ProjectID,
			LoopID:     input.LoopID,
			RunID:      input.RunID,
			Subtitle:   input.Subtitle,
			EntityType: "run",
			EntityID:   input.RunID,
		}
		switch {
		case input.Status == "failed" && input.FailureKind == worker.FailureManualIntervention:
			payload.Level = "action_required"
			payload.Title = "Looper Worker Needs Attention"
			payload.Body = input.Summary
			payload.DedupeKey = fmt.Sprintf("runtime.worker.action_required:%s", workerNotificationKeyID)
		case input.Status == "failed":
			payload.Level = "failure"
			payload.Title = "Looper Worker Failed"
			payload.Body = input.Summary
			payload.DedupeKey = fmt.Sprintf("runtime.worker.failed:%s", workerNotificationKeyID)
		case input.Status == "skipped":
			payload.Level = "action_required"
			payload.Title = "Looper Worker Needs Attention"
			payload.Body = input.Summary
			payload.DedupeKey = fmt.Sprintf("runtime.worker.action_required:%s", workerNotificationKeyID)
		case input.PullRequestNumber > 0:
			payload.Level = "action_required"
			payload.Title = "Looper Worker Opened a PR"
			payload.Body = fmt.Sprintf("PR #%d is ready: %s", input.PullRequestNumber, runtimeFirstNonEmpty(input.PullRequestURL, input.Summary))
			payload.DedupeKey = fmt.Sprintf("runtime.worker.pr_ready:%s", input.RunID)
		default:
			payload.Level = "success"
			payload.Title = "Looper Worker Completed"
			payload.Body = runtimeFirstNonEmpty(input.Summary, "Worker completed successfully")
			payload.DedupeKey = fmt.Sprintf("runtime.worker.completed:%s", input.RunID)
		}
		notificationGateway.Notify(ctx, payload)
		// Log the outcome to the loop's story so the anchor reads as a narrative.
		if strings.TrimSpace(input.LoopID) != "" && strings.EqualFold(strings.TrimSpace(cfg.Notifications.Webhook.Mode), "app") {
			switch {
			case input.PullRequestNumber > 0:
				link := runtimeFirstNonEmpty(input.PullRequestURL, "")
				if link != "" {
					notificationGateway.RecordMilestone(ctx, input.LoopID, fmt.Sprintf("🔀 已开 [PR #%d](%s)", input.PullRequestNumber, link))
				} else {
					notificationGateway.RecordMilestone(ctx, input.LoopID, fmt.Sprintf("🔀 已开 PR #%d", input.PullRequestNumber))
				}
			case input.Status == "failed" && input.FailureKind == worker.FailureManualIntervention:
				notificationGateway.RecordMilestone(ctx, input.LoopID, "⏸ 需要人处理")
			case input.Status == "failed":
				notificationGateway.RecordMilestone(ctx, input.LoopID, "⚠️ 本轮失败,重试中")
			case input.Status != "skipped":
				notificationGateway.RecordMilestone(ctx, input.LoopID, "✅ 完成")
			}
		} else {
			refreshFeishuAnchor(ctx, input.LoopID)
		}
		return nil
	}

	var plannerRunner plannerScheduler
	var coordinatorRunner coordinatorScheduler
	var reviewerRunner reviewerScheduler
	var fixerRunner fixerScheduler
	var workerRunner workerScheduler

	agentExecutor := agent.New(agent.ExecutorOptions{
		Config: agent.ExecutorConfig{
			Vendor:              *cfg.Agent.Vendor,
			Model:               cfg.Agent.Model,
			Params:              cfg.Agent.Params,
			Env:                 cfg.Agent.Env,
			NativeResumeEnabled: cfg.Agent.NativeResume.Enabled,
			// Env-gated (not a config field yet) so it stays zero-risk to the schema
			// / parity fixtures until the codex --json path is proven end-to-end.
			LiveToolEvents: strings.EqualFold(strings.TrimSpace(os.Getenv("LOOPER_CODEX_JSON_EVENTS")), "1"),
		},
		Repos:  repos,
		LogDir: cfg.Daemon.LogDir,
		Now:    now,
		// Live progress → Feishu anchor card. Vendor-agnostic (works off the agent
		// subprocess's stdout tail). Only wired when the Feishu app-bot transport is
		// configured; a no-op otherwise.
		OnProgress: func(ctx context.Context, p agent.ProgressUpdate) {
			if p.LoopID == "" || !strings.EqualFold(strings.TrimSpace(cfg.Notifications.Webhook.Mode), "app") {
				return
			}
			// In-memory only — never writes the loop record, so it can't race the
			// scheduler's loop/run writes.
			notificationGateway.RefreshThreadHeader(ctx, p.LoopID, p.TailLines, p.ElapsedSeconds)
		},
	})
	retryBaseDelay := time.Duration(cfg.Scheduler.RetryBaseDelayMS) * time.Millisecond
	stamper := disclosure.FromConfig(cfg)
	agentRuntime := ""
	if cfg.Agent.Vendor != nil {
		agentRuntime = string(*cfg.Agent.Vendor)
	}
	plannerRunner = planner.New(planner.Options{
		DB:                 coordinator.DB(),
		Repos:              repos,
		GitHub:             plannerGitHubAdapter{gateway: githubGateway, stamper: stamper, config: &cfg},
		Git:                plannerGitAdapter{gateway: gitGateway, stamper: stamper},
		AgentExecutor:      plannerAgentExecutorAdapter{executor: agentExecutor},
		Logger:             logger,
		Now:                now,
		AllowAutoPush:      boolPtr(cfg.Defaults.AllowAutoPush),
		Disclosure:         &cfg.Disclosure,
		AgentRuntime:       agentRuntime,
		CustomInstructions: &cfg,
		AgentModel:         cfg.Agent.Model,
		AgentTimeout:       time.Duration(cfg.Agent.Timeouts.PlannerMaxRuntimeSeconds) * time.Second,
		AgentIdleTimeout:   time.Duration(cfg.Agent.Timeouts.PlannerIdleTimeoutSeconds) * time.Second,
		DiscoveryPolicy: planner.DiscoveryPolicy{
			AutoDiscovery:              cfg.Roles.Planner.AutoDiscovery,
			Labels:                     append([]string(nil), cfg.Roles.Planner.Triggers.Labels...),
			LabelMode:                  cfg.Roles.Planner.Triggers.LabelMode,
			RequireAssigneeCurrentUser: cfg.Roles.Planner.Triggers.RequireAssigneeCurrentUser,
		},
		RetryBaseDelay:      retryBaseDelay,
		RetryMaxAttempts:    int64(cfg.Scheduler.RetryMaxAttempts),
		OnQueueItemEnqueued: requestWake,
		OnAgentExecutionStarted: func(ctx context.Context, input planner.AgentExecutionStartedInput) error {
			return notifyAgentExecutionStarted(ctx, agentExecutionNotificationInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Title: "Looper Planner", Subtitle: input.Subtitle, Body: input.Body, DedupeKey: input.DedupeKey})
		},
	})
	coordinatorRunner = coordinatorrole.New(coordinatorrole.Options{
		Repos:   repos,
		GitHub:  githubGateway,
		Config:  &cfg,
		Logger:  logger,
		Now:     now,
		Network: coordinatorrole.NewLoopernetGateway(networkclient.DefaultStatePath(runtimeHomeDirOrEmpty())),
		TriageLLM: coordinatorrole.NewAgentLLM(agentExecutor, now,
			time.Duration(cfg.Agent.Timeouts.PlannerMaxRuntimeSeconds)*time.Second,
			time.Duration(cfg.Agent.Timeouts.PlannerIdleTimeoutSeconds)*time.Second,
		),
	})
	reviewerRunner = reviewer.New(reviewer.Options{
		DB:               coordinator.DB(),
		Repos:            repos,
		GitHub:           reviewerGitHubAdapter{gateway: githubGateway, stamper: stamper, config: &cfg},
		Git:              reviewerGitAdapter{gateway: gitGateway},
		AgentExecutor:    reviewerAgentExecutorAdapter{executor: agentExecutor},
		Logger:           logger,
		Now:              now,
		AllowAutoApprove: cfg.Defaults.AllowAutoApprove,
		ReviewEvents:     cfg.Roles.Reviewer.Behavior.ReviewEvents,
		LoopConfig:       cfg.Roles.Reviewer.Behavior.Loop,
		DiscoveryPolicy: reviewer.DiscoveryPolicy{
			AutoDiscovery:             cfg.Roles.Reviewer.Discovery.AutoDiscovery,
			IncludeDrafts:             cfg.Roles.Reviewer.Discovery.Triggers.IncludeDrafts,
			RequireReviewRequest:      cfg.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest,
			EnableSelfReview:          cfg.Roles.Reviewer.Discovery.Triggers.EnableSelfReview,
			Labels:                    append([]string(nil), cfg.Roles.Reviewer.Discovery.Triggers.Labels...),
			LabelMode:                 cfg.Roles.Reviewer.Discovery.Triggers.LabelMode,
			IncludeSpecReviewingLabel: cfg.Roles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel,
			SpecReviewingLabel:        cfg.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel,
		},
		Scope:                   cfg.Roles.Reviewer.Behavior.Scope,
		DetectDuplicateFindings: cfg.Roles.Reviewer.Behavior.DetectDuplicateFindings,
		NativeResume:            cfg.Roles.Reviewer.Behavior.NativeResume,
		ThreadResolution:        cfg.Roles.Reviewer.Behavior.ThreadResolution,
		Disclosure:              &cfg.Disclosure,
		AgentRuntime:            agentRuntime,
		CustomInstructions:      &cfg,
		LooperCLIPath:           derefString(cfg.Tools.LooperPath),
		AgentModel:              cfg.Agent.Model,
		AgentTimeout:            time.Duration(cfg.Agent.Timeouts.ReviewerMaxRuntimeSeconds) * time.Second,
		AgentIdleTimeout:        time.Duration(cfg.Agent.Timeouts.ReviewerIdleTimeoutSeconds) * time.Second,
		RetryBaseDelay:          retryBaseDelay,
		RetryMaxAttempts:        int64(cfg.Scheduler.RetryMaxAttempts),
		RetryPolicy:             cfg.Roles.Reviewer.Behavior.Retry,
		OnQueueItemEnqueued:     requestWake,
		OnAgentExecutionStarted: func(ctx context.Context, input reviewer.AgentExecutionStartedInput) error {
			return notifyAgentExecutionStarted(ctx, agentExecutionNotificationInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Title: "Looper Reviewer", Subtitle: input.Subtitle, Body: input.Body, DedupeKey: input.DedupeKey})
		},
	})
	fixerRunner = fixer.New(fixer.Options{
		DB:                 coordinator.DB(),
		Repos:              repos,
		GitHub:             fixerGitHubAdapter{gateway: githubGateway, stamper: stamper, config: &cfg},
		Git:                fixerGitAdapter{gateway: gitGateway, stamper: stamper},
		AgentExecutor:      fixerAgentExecutorAdapter{executor: agentExecutor},
		Logger:             logger,
		Now:                now,
		AllowAutoCommit:    cfg.Defaults.AllowAutoCommit,
		AllowAutoPush:      cfg.Defaults.AllowAutoPush,
		AllowRiskyFixes:    cfg.Defaults.AllowRiskyFixes,
		FixAllPullRequests: cfg.Defaults.FixAllPullRequests,
		DiscoveryPolicy: fixer.DiscoveryPolicy{
			AutoDiscovery: cfg.Roles.Fixer.AutoDiscovery,
			IncludeDrafts: cfg.Roles.Fixer.Triggers.IncludeDrafts,
			AuthorFilter:  cfg.Roles.Fixer.Triggers.AuthorFilter,
			Labels:        append([]string(nil), cfg.Roles.Fixer.Triggers.Labels...),
			LabelMode:     cfg.Roles.Fixer.Triggers.LabelMode,
		},
		Disclosure:          &cfg.Disclosure,
		AgentRuntime:        agentRuntime,
		CustomInstructions:  &cfg,
		AgentModel:          cfg.Agent.Model,
		AgentTimeout:        time.Duration(cfg.Agent.Timeouts.FixerMaxRuntimeSeconds) * time.Second,
		AgentIdleTimeout:    time.Duration(cfg.Agent.Timeouts.FixerIdleTimeoutSeconds) * time.Second,
		RetryBaseDelay:      retryBaseDelay,
		RetryMaxAttempts:    int64(cfg.Scheduler.RetryMaxAttempts),
		OnQueueItemEnqueued: requestWake,
		OnAgentExecutionStarted: func(ctx context.Context, input fixer.AgentExecutionStartedInput) error {
			return notifyAgentExecutionStarted(ctx, agentExecutionNotificationInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Title: "Looper Fixer", Subtitle: input.Subtitle, Body: input.Body, DedupeKey: input.DedupeKey})
		},
	})
	workerRunner = worker.New(worker.Options{
		DB:     coordinator.DB(),
		Repos:  repos,
		GitHub: workerGitHubAdapter{gateway: githubGateway, stamper: stamper, config: &cfg},
		GitHubCLIAutoPROpeningAvailable: func(ctx context.Context, repo, cwd string) bool {
			return githubCLIAutoPROpeningAvailable(ctx, cfg, githubGateway, logger, repo, cwd)
		},
		Git:             workerGitAdapter{gateway: gitGateway, stamper: stamper},
		AgentExecutor:   workerAgentExecutorAdapter{executor: agentExecutor, registry: activeExecutions},
		Logger:          logger,
		Now:             now,
		AllowAutoCommit: cfg.Defaults.AllowAutoCommit,
		AllowAutoPush:   cfg.Defaults.AllowAutoPush,
		OpenPRStrategy:  cfg.Defaults.OpenPRStrategy,
		DiscoveryPolicy: worker.DiscoveryPolicy{
			AutoDiscovery:              cfg.Roles.Worker.AutoDiscovery,
			Labels:                     append([]string(nil), cfg.Roles.Worker.Triggers.Labels...),
			LabelMode:                  cfg.Roles.Worker.Triggers.LabelMode,
			RequireAssigneeCurrentUser: cfg.Roles.Worker.Triggers.RequireAssigneeCurrentUser,
		},
		Disclosure:          &cfg.Disclosure,
		AgentRuntime:        agentRuntime,
		CustomInstructions:  &cfg,
		Network:             coordinatorNetworkGateway{statePath: networkclient.DefaultStatePath(runtimeHomeDirOrEmpty()), client: &http.Client{Timeout: 10 * time.Second}},
		AgentModel:          cfg.Agent.Model,
		AgentTimeout:        time.Duration(cfg.Agent.Timeouts.WorkerMaxRuntimeSeconds) * time.Second,
		AgentIdleTimeout:    time.Duration(cfg.Agent.Timeouts.WorkerIdleTimeoutSeconds) * time.Second,
		RetryBaseDelay:      retryBaseDelay,
		RetryMaxAttempts:    int64(cfg.Scheduler.RetryMaxAttempts),
		OnQueueItemEnqueued: requestWake,
		OnRunCompleted: func(ctx context.Context, input worker.RunCompletedInput) error {
			return notifyWorkerRunCompleted(ctx, workerRunCompletedNotificationInput{ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Subtitle: input.Subtitle, Status: input.Status, Summary: input.Summary, FailureKind: input.FailureKind, PullRequestNumber: input.PullRequestNumber, PullRequestURL: input.PullRequestURL})
		},
		HITLEnabled:         cfg.HITL.Enabled,
		HITLAnswerTransport: cfg.HITL.AnswerTransport,
		HITLGitHub:          hitlGitHubSettings(cfg.HITL.GitHub),
		HITLNotify: func(ctx context.Context, ask worker.HITLAskNotification) error {
			return notificationGateway.SendHITLAsk(ctx, notify.HITLAskCard{
				ProjectID: ask.ProjectID, LoopID: ask.LoopID, LoopSeq: ask.LoopSeq,
				Repo: ask.Repo, Title: ask.Title, Question: ask.Question, Options: ask.Options,
				SourceType: ask.SourceType, SourceRef: ask.SourceRef, SourceURL: ask.SourceURL,
				TriggerLogin:      ask.TriggerLogin,
				Recommendation:    ask.Recommendation,
				RecommendedOption: ask.RecommendedOption,
				Consequences:      ask.Consequences,
				Confidence:        ask.Confidence,
			})
		},
	})
	claimMu := &sync.Mutex{}

	inputForServices := func(services Services) defaultSchedulerTickInput {
		var runner schedulerAsyncRunner
		if asyncRunner != nil {
			runner = asyncRunner()
		}
		return defaultSchedulerTickInput{
			Repos:                    services.Repositories,
			GitHubGateway:            githubGateway,
			Logger:                   logger,
			Now:                      now,
			MaxConcurrentRuns:        cfg.Scheduler.MaxConcurrentRuns,
			ClaimMu:                  claimMu,
			ReconcileStaleRuns:       reconcileStaleRuns,
			AsyncRunner:              runner,
			RequestSchedulerWake:     requestWake,
			Planner:                  plannerRunner,
			Coordinator:              coordinatorRunner,
			Reviewer:                 reviewerRunner,
			Fixer:                    fixerRunner,
			Worker:                   workerRunner,
			Snapshotter:              githubGateway,
			Config:                   &cfg,
			PlannerDiscoveryEnabled:  boolPtr(config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "planner")),
			CoordinatorEnabled:       func(projectID string) bool { return config.ProjectRoleConfigs(cfg, projectID).Coordinator.Enabled },
			ReviewerDiscoveryEnabled: boolPtr(config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "reviewer")),
			FixerDiscoveryEnabled:    boolPtr(config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "fixer")),
			WorkerDiscoveryEnabled:   boolPtr(config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "worker")),
			OnHITLAnswerDelivered:    notificationGateway.MarkAskAnswered,
		}
	}

	return defaultSchedulerHandlers{
		tick: func(ctx context.Context, services Services) error {
			return runDefaultSchedulerTick(ctx, inputForServices(services))
		},
		claim: func(ctx context.Context, services Services) error {
			return runIndependentClaimPass(ctx, inputForServices(services))
		},
		webhook: webhookforward.New(webhookforward.Options{
			Repos:    repos,
			Config:   cfg,
			Reviewer: reviewerRunner,
			Fixer:    fixerRunner,
			Logger:   logger,
			Now:      now,
		}),
	}
}

func githubCLIAutoPROpeningAvailable(ctx context.Context, cfg config.Config, githubGateway *githubinfra.Gateway, logger bootstrap.Logger, repo, cwd string) bool {
	if configuredPath := strings.TrimSpace(derefString(cfg.Tools.GHPath)); configuredPath != "" {
		githubGateway = githubinfra.New(githubinfra.Options{GHPath: configuredPath, CWD: cwd})
	}
	if githubGateway == nil {
		return false
	}
	hostname := githubAuthHostname(repo)
	authenticated, err := githubGateway.IsAuthenticated(ctx, cwd, hostname)
	if err != nil {
		if logger != nil {
			logger.Warn("github cli auth check failed; disabling automatic PR opening", map[string]any{"error": err.Error(), "repo": repo, "hostname": hostname})
		}
		return false
	}
	return authenticated
}

func githubAuthHostname(repo string) string {
	const defaultHost = "github.com"
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return defaultHost
	}
	if parsed, err := url.Parse(repo); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	if at := strings.Index(repo, "@"); at >= 0 {
		repo = repo[at+1:]
	}
	if colon := strings.Index(repo, ":"); colon > 0 {
		return strings.TrimSpace(repo[:colon])
	}
	parts := strings.Split(repo, "/")
	if len(parts) == 3 && strings.TrimSpace(parts[0]) != "" {
		return strings.TrimSpace(parts[0])
	}
	return defaultHost
}

func runDefaultSchedulerTick(ctx context.Context, input defaultSchedulerTickInput) (retErr error) {
	if input.Repos == nil || input.Repos.Projects == nil {
		return nil
	}

	startedAt := time.Now()
	claimStats := schedulerClaimStats{}
	if input.Logger != nil {
		input.Logger.Debug("scheduler tick start", nil)
	}
	defer func() {
		if input.Logger == nil {
			return
		}
		fields := map[string]any{"durationMs": time.Since(startedAt).Milliseconds(), "claimedCount": claimStats.claimedCount, "availableSlots": claimStats.availableSlots}
		if retErr != nil {
			fields["error"] = retErr.Error()
		}
		input.Logger.Debug("scheduler tick end", fields)
		input.Logger.Info("scheduler tick summary", fields)
	}()

	now := input.Now
	if now == nil {
		now = time.Now
	}

	errs := make([]error, 0)
	appendErr := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	discoveredRunnableIDs := make(map[string]struct{})
	trackRunnableDiscovery := func(queueItems []storage.QueueItemRecord) {
		for _, id := range runnableSchedulerQueueItemIDs(queueItems, now) {
			discoveredRunnableIDs[id] = struct{}{}
		}
	}
	recordClaim := func(claimedCount, availableSlots int, err error) {
		claimStats.record(claimedCount, availableSlots)
		appendErr(err)
	}

	claimedCount, availableSlots, err := executeClaimPhase(ctx, "pre_discovery", input, discoveredRunnableIDs, true)
	recordClaim(claimedCount, availableSlots, err)

	projectsList, err := input.Repos.Projects.List(ctx)
	if err != nil {
		appendErr(err)
		retErr = errors.Join(errs...)
		return retErr
	}
	tickDiscoveryState := githubinfra.NewDiscoveryTickState()
	projectSnapshots := map[string]*githubinfra.DiscoverySnapshot{}
	projectSnapshot := func(projectID string) *githubinfra.DiscoverySnapshot {
		if input.GitHubGateway == nil {
			return nil
		}
		if snapshot, ok := projectSnapshots[projectID]; ok {
			return snapshot
		}
		snapshot := githubinfra.NewDiscoverySnapshot(input.GitHubGateway, tickDiscoveryState, projectDiscoverySnapshotOptions(input, projectID))
		projectSnapshots[projectID] = snapshot
		return snapshot
	}
	for _, project := range projectsList {
		if err := ctx.Err(); err != nil {
			retErr = errors.Join(append(errs, err)...)
			return retErr
		}
		if project.Archived {
			continue
		}
		providerKind := config.ProviderKindGitHub
		if input.Config != nil {
			providerKind = runtimeProjectProviderKind(*input.Config, project.ID)
		}
		repo := repoFromProjectMetadata(project.MetadataJSON)
		var snapshot *githubinfra.DiscoverySnapshot
		if providerKind == config.ProviderKindGitHub {
			snapshot = projectSnapshot(project.ID)
		}
		if repo == "" {
			if input.Logger != nil {
				input.Logger.Warn("scheduler skipped project without repo metadata", map[string]any{"projectId": project.ID})
			}
			continue
		}
		if input.Planner != nil && discoveryEnabled(input.PlannerDiscoveryEnabled) {
			appendErr(runSchedulerLane(input, "planner discovery", project.ID, repo, func() error {
				result, err := input.Planner.DiscoverIssues(ctx, planner.DiscoveryInput{ProjectID: project.ID, Repo: repo, Snapshot: snapshot})
				trackRunnableDiscovery(result.QueueItems)
				return wrapSchedulerError("planner discovery", project.ID, repo, err)
			}))
			claimedCount, availableSlots, err = executeClaimPhase(ctx, "post_planner_discovery", input, discoveredRunnableIDs, true)
			recordClaim(claimedCount, availableSlots, err)
		} else if input.Planner != nil && input.Logger != nil {
			input.Logger.Debug("planner auto-discovery disabled", map[string]any{"projectId": project.ID, "repo": repo})
		}
		if input.Coordinator != nil && coordinatorEnabledForProject(input, project.ID) {
			if !providerHasGitHubPullRequests(providerKind) {
				if input.Logger != nil {
					input.Logger.Debug("scheduler skipped unsupported provider lane", map[string]any{"lane": "coordinator discovery", "projectId": project.ID, "repo": repo, "provider": providerKind})
				}
			} else {
				appendErr(runSchedulerLane(input, "coordinator discovery", project.ID, repo, func() error {
					_, err := input.Coordinator.DiscoverIssues(ctx, coordinatorrole.DiscoveryInput{ProjectID: project.ID, Repo: repo, Snapshot: snapshot})
					return wrapSchedulerError("coordinator discovery", project.ID, repo, err)
				}))
				claimedCount, availableSlots, err = executeClaimPhase(ctx, "post_coordinator_discovery", input, discoveredRunnableIDs, true)
				recordClaim(claimedCount, availableSlots, err)
			}
		}
		if input.Reviewer != nil && discoveryEnabled(input.ReviewerDiscoveryEnabled) {
			appendErr(runSchedulerLane(input, "reviewer discovery", project.ID, repo, func() error {
				result, err := input.Reviewer.DiscoverPullRequests(ctx, reviewer.DiscoveryInput{ProjectID: project.ID, Repo: repo, Snapshot: snapshot})
				trackRunnableDiscovery(result.QueueItems)
				return wrapSchedulerError("reviewer discovery", project.ID, repo, err)
			}))
			claimedCount, availableSlots, err = executeClaimPhase(ctx, "post_reviewer_discovery", input, discoveredRunnableIDs, true)
			recordClaim(claimedCount, availableSlots, err)
		} else if input.Reviewer != nil && input.Logger != nil {
			input.Logger.Debug("reviewer auto-discovery disabled", map[string]any{"projectId": project.ID, "repo": repo})
		}
		if input.Fixer != nil && discoveryEnabled(input.FixerDiscoveryEnabled) {
			if !providerHasGitHubPullRequests(providerKind) {
				if input.Logger != nil {
					input.Logger.Debug("scheduler skipped unsupported provider lane", map[string]any{"lane": "fixer discovery", "projectId": project.ID, "repo": repo, "provider": providerKind})
				}
			} else {
				appendErr(runSchedulerLane(input, "fixer discovery", project.ID, repo, func() error {
					result, err := input.Fixer.DiscoverPullRequests(ctx, fixer.DiscoveryInput{ProjectID: project.ID, Repo: repo, Snapshot: snapshot})
					trackRunnableDiscovery(result.QueueItems)
					return wrapSchedulerError("fixer discovery", project.ID, repo, err)
				}))
				claimedCount, availableSlots, err = executeClaimPhase(ctx, "post_fixer_discovery", input, discoveredRunnableIDs, true)
				recordClaim(claimedCount, availableSlots, err)
			}
		} else if input.Fixer != nil && input.Logger != nil {
			input.Logger.Debug("fixer auto-discovery disabled", map[string]any{"projectId": project.ID, "repo": repo})
		}
		if discoverer, ok := input.Worker.(workerIssueDiscoveryScheduler); ok && discoveryEnabled(input.WorkerDiscoveryEnabled) {
			appendErr(runSchedulerLane(input, "worker issue discovery", project.ID, repo, func() error {
				result, err := discoverer.DiscoverIssues(ctx, worker.DiscoveryInput{ProjectID: project.ID, Repo: repo, Snapshot: snapshot})
				trackRunnableDiscovery(result.QueueItems)
				return wrapSchedulerError("worker issue discovery", project.ID, repo, err)
			}))
			claimedCount, availableSlots, err = executeClaimPhase(ctx, "post_worker_discovery", input, discoveredRunnableIDs, true)
			recordClaim(claimedCount, availableSlots, err)
		} else if input.Worker != nil && input.Logger != nil && !discoveryEnabled(input.WorkerDiscoveryEnabled) {
			input.Logger.Debug("worker auto-discovery disabled", map[string]any{"projectId": project.ID, "repo": repo})
		}

		// HITL (github transport): deliver any human answers posted on this
		// project's awaiting_human PRs so those loops resume.
		runGitHubHITLPoll(ctx, input, project)
	}

	// HITL (feishu transport): poll the shared Cloudflare inbox once per tick and
	// deliver any answers for this looper's awaiting loops.
	runFeishuHITLPoll(ctx, input)

	claimedCount, availableSlots, err = executeClaimPhase(ctx, "post_discovery", input, discoveredRunnableIDs, true)
	recordClaim(claimedCount, availableSlots, err)

	if len(errs) == 0 {
		return nil
	}
	retErr = errors.Join(errs...)
	return retErr
}

func discoveryEnabled(value *bool) bool {
	return value == nil || *value
}

func coordinatorEnabledForProject(input defaultSchedulerTickInput, projectID string) bool {
	if input.CoordinatorEnabled == nil {
		return false
	}
	return input.CoordinatorEnabled(projectID)
}

func projectDiscoverySnapshotOptions(input defaultSchedulerTickInput, projectID string) githubinfra.DiscoverySnapshotOptions {
	prLimit := 30
	issueLimit := 30
	if input.Coordinator != nil && coordinatorEnabledForProject(input, projectID) {
		issueLimit = maxInt(issueLimit, 100)
	}
	return githubinfra.DiscoverySnapshotOptions{PullRequestLimit: prLimit, IssueLimit: issueLimit}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func runnableSchedulerQueueItemIDs(queueItems []storage.QueueItemRecord, now func() time.Time) []string {
	if len(queueItems) == 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	ids := make([]string, 0, len(queueItems))
	for _, item := range queueItems {
		if item.Status == "queued" && item.AvailableAt <= nowISO {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

func requestWakeForClaimedDiscovery(claimedItems []storage.QueueItemRecord, discoveredRunnableIDs map[string]struct{}, requestWake func()) {
	if requestWake == nil || len(claimedItems) == 0 || len(discoveredRunnableIDs) == 0 {
		return
	}
	for _, item := range claimedItems {
		if _, ok := discoveredRunnableIDs[item.ID]; ok {
			requestWake()
			return
		}
	}
}

type schedulerClaimStats struct {
	claimedCount   int
	availableSlots int
}

func (s *schedulerClaimStats) record(claimedCount, availableSlots int) {
	s.claimedCount += claimedCount
	s.availableSlots = availableSlots
}

func runIndependentClaimPass(ctx context.Context, input defaultSchedulerTickInput) error {
	_, _, err := executeClaimPhase(ctx, "claim_pump", input, nil, false)
	return err
}

func executeClaimPhase(ctx context.Context, phase string, input defaultSchedulerTickInput, discoveredRunnableIDs map[string]struct{}, alwaysLog bool) (int, int, error) {
	if input.ClaimMu != nil {
		input.ClaimMu.Lock()
		defer input.ClaimMu.Unlock()
	}
	start := time.Now()
	availableSlots, err := schedulerAvailableSlots(ctx, input.Repos, input.MaxConcurrentRuns)
	if err != nil {
		logClaimPhase(input.Logger, phase, 0, 0, time.Since(start), err)
		return 0, 0, err
	}
	if availableSlots == 0 && input.ReconcileStaleRuns != nil {
		if _, err := input.ReconcileStaleRuns(ctx); err != nil {
			logClaimPhase(input.Logger, phase, 0, 0, time.Since(start), err)
			return 0, 0, err
		}
		availableSlots, err = schedulerAvailableSlots(ctx, input.Repos, input.MaxConcurrentRuns)
		if err != nil {
			logClaimPhase(input.Logger, phase, 0, 0, time.Since(start), err)
			return 0, 0, err
		}
	}
	claimedItems := make([]storage.QueueItemRecord, 0)
	if availableSlots > 0 && input.Repos != nil && input.Repos.Queue != nil {
		claimedItems, err = claimAndRunScheduledQueueItems(ctx, availableSlots, input)
		if err == nil {
			requestWakeForClaimedDiscovery(claimedItems, discoveredRunnableIDs, input.RequestSchedulerWake)
		}
	}
	claimedCount := len(claimedItems)
	if alwaysLog || availableSlots > 0 || claimedCount > 0 || err != nil {
		logClaimPhase(input.Logger, phase, availableSlots, claimedCount, time.Since(start), err)
	}
	return claimedCount, availableSlots, err
}

func logClaimPhase(logger bootstrap.Logger, phase string, availableSlots, claimedCount int, duration time.Duration, err error) {
	if logger == nil {
		return
	}
	fields := map[string]any{"phase": phase, "availableSlots": availableSlots, "claimedCount": claimedCount, "durationMs": duration.Milliseconds()}
	if err != nil {
		fields["error"] = err.Error()
	}
	logger.Debug("scheduler claim phase", fields)
}

func runSchedulerLane(input defaultSchedulerTickInput, laneName, projectID, repo string, fn func() error) error {
	start := time.Now()
	if input.Logger != nil {
		input.Logger.Debug("scheduler lane start", map[string]any{"lane": laneName, "projectId": projectID, "repo": repo})
	}
	err := fn()
	if input.Logger != nil {
		fields := map[string]any{"lane": laneName, "projectId": projectID, "repo": repo, "durationMs": time.Since(start).Milliseconds()}
		if err != nil {
			fields["error"] = err.Error()
		}
		input.Logger.Debug("scheduler lane end", fields)
		if threshold := schedulerSlowLaneWarnThreshold(input); threshold > 0 && time.Since(start) >= threshold {
			input.Logger.Warn("scheduler lane slow", fields)
		}
	}
	return err
}

func schedulerSlowLaneWarnThreshold(input defaultSchedulerTickInput) time.Duration {
	if input.Config == nil || input.Config.Scheduler.SlowLaneWarnThresholdMS <= 0 {
		return 5 * time.Second
	}
	return time.Duration(input.Config.Scheduler.SlowLaneWarnThresholdMS) * time.Millisecond
}

func schedulerAvailableSlots(ctx context.Context, repos *storage.Repositories, maxConcurrentRuns int) (int, error) {
	if repos == nil || repos.Queue == nil {
		return 0, nil
	}
	if maxConcurrentRuns <= 0 {
		return 0, nil
	}
	runningCount, err := repos.Queue.CountByStatus(ctx, "running")
	if err != nil {
		return 0, err
	}
	available := maxConcurrentRuns - int(runningCount)
	if available < 0 {
		return 0, nil
	}
	return available, nil
}

func claimAndRunScheduledQueueItems(ctx context.Context, availableSlots int, input defaultSchedulerTickInput) ([]storage.QueueItemRecord, error) {
	if availableSlots <= 0 || input.Repos == nil || input.Repos.Queue == nil {
		return nil, nil
	}
	now := input.Now
	if now == nil {
		now = time.Now
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	queueItems := make([]storage.QueueItemRecord, 0, availableSlots)
	for i := 0; i < availableSlots; i++ {
		if err := ctx.Err(); err != nil {
			return queueItems, err
		}
		item, err := input.Repos.Queue.ClaimNextNonLongTermRetry(ctx, nowISO, "scheduler")
		if err != nil {
			return queueItems, err
		}
		if item == nil {
			break
		}
		queueItems = append(queueItems, *item)
	}
	for len(queueItems) < availableSlots {
		if err := ctx.Err(); err != nil {
			return queueItems, err
		}
		item, err := input.Repos.Queue.ClaimNextLongTermRetry(ctx, nowISO, "scheduler")
		if err != nil {
			return queueItems, err
		}
		if item == nil {
			break
		}
		queueItems = append(queueItems, *item)
	}
	return queueItems, runScheduledQueueItems(ctx, queueItems, input)
}

// schedulerLoopParked reports whether a claimed queue item's loop was parked
// (human takeover / paused) — a state the scheduler may observe AFTER the claim
// due to a race, and must then decline to run.
func schedulerLoopParked(ctx context.Context, item storage.QueueItemRecord, input defaultSchedulerTickInput) bool {
	if item.LoopID == nil || input.Repos == nil || input.Repos.Loops == nil {
		return false
	}
	loop, err := input.Repos.Loops.GetByID(ctx, *item.LoopID)
	if err != nil || loop == nil {
		return false
	}
	switch loop.Status {
	case "human_takeover", "paused":
		return true
	default:
		return false
	}
}

func runScheduledQueueItems(ctx context.Context, queueItems []storage.QueueItemRecord, input defaultSchedulerTickInput) error {
	if len(queueItems) == 0 {
		return nil
	}

	now := input.Now
	if now == nil {
		now = time.Now
	}
	errList := make([]error, 0)
	for _, item := range queueItems {
		if err := ctx.Err(); err != nil {
			return err
		}

		// A loop parked for human takeover (or paused) must never be run, even if a
		// queue item survived a race with the parking and got claimed. Release the
		// claim (so the slot frees) and skip — only an explicit handback re-arms it.
		if schedulerLoopParked(ctx, item, input) {
			if input.Repos != nil && input.Repos.Queue != nil {
				_ = input.Repos.Queue.Complete(ctx, item.ID, formatJavaScriptISOString(now().UTC()))
			}
			if input.Logger != nil {
				loopID := ""
				if item.LoopID != nil {
					loopID = *item.LoopID
				}
				input.Logger.Info("scheduler released claimed item for parked loop", map[string]any{"queueItemId": item.ID, "loopId": loopID})
			}
			continue
		}

		process, err := schedulerQueueProcessor(item, input)
		if err != nil {
			errList = append(errList, err)
			continue
		}
		item := item
		processFn := process

		if input.AsyncRunner != nil {
			input.AsyncRunner.Go(func() {
				if err := processFn(ctx); err != nil && input.Logger != nil {
					input.Logger.Warn("scheduler queue item failed", map[string]any{"type": item.Type, "queueItemId": item.ID, "error": err.Error()})
				}
			})
			continue
		}
		go func() {
			if err := processFn(ctx); err != nil && input.Logger != nil {
				input.Logger.Warn("scheduler queue item failed", map[string]any{"type": item.Type, "queueItemId": item.ID, "error": err.Error()})
			}
		}()
	}
	if len(errList) == 0 {
		return nil
	}
	return errors.Join(errList...)
}

func schedulerQueueProcessor(item storage.QueueItemRecord, input defaultSchedulerTickInput) (func(context.Context) error, error) {
	switch item.Type {
	case "planner":
		if input.Planner == nil {
			return nil, fmt.Errorf("planner runner is not configured")
		}
		return func(ctx context.Context) error {
			_, err := input.Planner.ProcessClaimedQueueItem(ctx, item)
			return wrapSchedulerQueueError(item.Type, err)
		}, nil
	case "reviewer":
		if input.Reviewer == nil {
			return nil, fmt.Errorf("reviewer runner is not configured")
		}
		return func(ctx context.Context) error {
			_, err := input.Reviewer.ProcessClaimedQueueItem(ctx, item)
			return wrapSchedulerQueueError(item.Type, err)
		}, nil
	case "fixer":
		if input.Fixer == nil {
			return nil, fmt.Errorf("fixer runner is not configured")
		}
		return func(ctx context.Context) error {
			_, err := input.Fixer.ProcessClaimedQueueItem(ctx, item)
			return wrapSchedulerQueueError(item.Type, err)
		}, nil
	case "worker":
		if input.Worker == nil {
			return nil, fmt.Errorf("worker runner is not configured")
		}
		return func(ctx context.Context) error {
			_, err := input.Worker.ProcessClaimedQueueItem(ctx, item)
			return wrapSchedulerQueueError(item.Type, err)
		}, nil
	case "snapshot":
		if input.Snapshotter == nil || input.Repos == nil || input.Repos.Queue == nil || input.Repos.PullRequestSnapshots == nil {
			return nil, fmt.Errorf("snapshot runner is not configured")
		}
		return func(ctx context.Context) error {
			return processSnapshotQueueItem(ctx, item, input)
		}, nil
	default:
		return nil, fmt.Errorf("unsupported queue item type %q", item.Type)
	}
}

func processSnapshotQueueItem(ctx context.Context, item storage.QueueItemRecord, input defaultSchedulerTickInput) error {
	if item.ProjectID == nil || strings.TrimSpace(*item.ProjectID) == "" || item.Repo == nil || strings.TrimSpace(*item.Repo) == "" || item.PRNumber == nil {
		return failSnapshotQueueItem(ctx, item, input, "invalid snapshot queue item", "non_retryable")
	}
	project, err := input.Repos.Projects.GetByID(ctx, *item.ProjectID)
	if err != nil {
		return failSnapshotQueueItem(ctx, item, input, err.Error(), "retryable_transient")
	}
	if project == nil {
		return failSnapshotQueueItem(ctx, item, input, "project not found", "non_retryable")
	}
	cwd := project.RepoPath
	if item.PayloadJSON != nil && strings.TrimSpace(*item.PayloadJSON) != "" {
		payload := map[string]any{}
		if err := json.Unmarshal([]byte(*item.PayloadJSON), &payload); err == nil {
			if payloadCWD, _ := payload["cwd"].(string); strings.TrimSpace(payloadCWD) != "" {
				cwd = strings.TrimSpace(payloadCWD)
			}
		}
	}
	now := input.Now
	if now == nil {
		now = time.Now
	}
	snapshot, err := input.Snapshotter.CapturePullRequestSnapshot(ctx, githubinfra.CapturePullRequestSnapshotInput{ProjectID: project.ID, Repo: *item.Repo, PRNumber: *item.PRNumber, CWD: cwd, CapturedAt: formatJavaScriptISOString(now().UTC())})
	if err != nil {
		return failSnapshotQueueItem(ctx, item, input, err.Error(), "retryable_transient")
	}
	if err := input.Repos.PullRequestSnapshots.Upsert(ctx, snapshot); err != nil {
		return failSnapshotQueueItem(ctx, item, input, err.Error(), "retryable_transient")
	}
	if err := input.Repos.Queue.Complete(ctx, item.ID, formatJavaScriptISOString(now().UTC())); err != nil {
		if errors.Is(err, storage.ErrQueueItemNotActive) {
			return nil
		}
		return failSnapshotQueueItem(ctx, item, input, err.Error(), "retryable_transient")
	}
	return nil
}

func failSnapshotQueueItem(ctx context.Context, item storage.QueueItemRecord, input defaultSchedulerTickInput, message, kind string) error {
	now := input.Now
	if now == nil {
		now = time.Now
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	nextAttempts := item.Attempts + 1
	if kind == "retryable_transient" && (item.MaxAttempts < 0 || (item.MaxAttempts > 0 && nextAttempts < item.MaxAttempts)) {
		retryAt := formatJavaScriptISOString(now().UTC().Add(time.Minute * time.Duration(cappedRetryDelayAttempt(nextAttempts, item.MaxAttempts))))
		return input.Repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: item.ID, AvailableAt: retryAt, Attempts: nextAttempts, ErrorMessage: &message, ErrorKind: kind, UpdatedAt: nowISO})
	}
	return input.Repos.Queue.Fail(ctx, storage.QueueFailInput{ID: item.ID, Attempts: nextAttempts, FinishedAt: nowISO, ErrorMessage: &message, ErrorKind: kind, UpdatedAt: nowISO})
}

func cappedRetryDelayAttempt(attempts, maxAttempts int64) int64 {
	if attempts <= 0 {
		return 1
	}
	if maxAttempts > 0 && attempts > maxAttempts {
		return maxAttempts
	}
	return attempts
}

func repoFromProjectMetadata(metadataJSON *string) string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return ""
	}
	metadata := map[string]any{}
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil {
		return ""
	}
	repo, _ := metadata["repo"].(string)
	return strings.TrimSpace(repo)
}

func wrapSchedulerError(action, projectID, repo string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s failed for project %s (%s): %w", action, projectID, repo, err)
}

func wrapSchedulerQueueError(queueType string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s processing failed: %w", queueType, err)
}

func runtimeFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func boolPtr(value bool) *bool {
	return &value
}

func summarizeCheckStates(checks []map[string]any) string {
	if len(checks) == 0 {
		return ""
	}
	states := make([]string, 0, len(checks))
	for _, check := range checks {
		state, _ := check["conclusion"].(string)
		if strings.TrimSpace(state) == "" {
			state, _ = check["state"].(string)
		}
		state = strings.TrimSpace(state)
		if state == "" {
			continue
		}
		states = append(states, state)
	}
	return strings.Join(states, ", ")
}
