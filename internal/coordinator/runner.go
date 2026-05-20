package coordinator

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator/depgraph"
	"github.com/nexu-io/looper/internal/coordinator/dispatch"
	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/disclosure"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/specpr"
	"github.com/nexu-io/looper/internal/network/protocol"
	"github.com/nexu-io/looper/internal/storage"
)

const jsISOStringLayout = "2006-01-02T15:04:05.000Z"

const triageCommentMarker = "<!-- looper:coordinator:triage -->"
const dispatchFailureCommentMarker = "<!-- looper:coordinator:dispatch-failure -->"
const cycleCommentMarker = "<!-- looper:coordinator:cycle -->"
const mergeWatchCommentMarkerPrefix = "<!-- looper:coordinator:merge-watch"
const noEligibleNodeStatus = "no-eligible-node"

type DiscoveryInput struct {
	ProjectID string
	Repo      string
	Snapshot  *githubinfra.DiscoverySnapshot
}

type DiscoveryResult struct {
	Skipped bool
	Ticked  bool
}

type IssueSummary struct {
	Number int64
	Labels []string
}

type loadedCoordinatorIssue struct {
	summary       githubinfra.IssueSummary
	issue         triage.Issue
	dispatchIssue dispatch.Issue
}

type GitHubGateway interface {
	ListOpenIssues(context.Context, githubinfra.ListOpenIssuesInput) ([]githubinfra.IssueSummary, error)
	ListLinkedPullRequests(context.Context, githubinfra.LinkedPullRequestsInput) ([]githubinfra.LinkedPullRequest, error)
	ViewIssue(context.Context, githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error)
	ViewPullRequest(context.Context, githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error)
	ListIssueComments(context.Context, githubinfra.ViewIssueInput) ([]githubinfra.CommentInfo, error)
	ListIssueTimeline(context.Context, githubinfra.IssueTimelineInput) ([]map[string]any, error)
	ListIssueBlockedBy(context.Context, githubinfra.ListIssueBlockedByInput) ([]githubinfra.IssueDependency, error)
	GetIssueState(context.Context, githubinfra.ViewIssueInput) (githubinfra.IssueState, error)
	GetCurrentUserLogin(context.Context, string) (string, error)
	GetCurrentUserLoginForRepo(context.Context, string, string) (string, error)
	GetRepositoryPermission(context.Context, githubinfra.RepositoryPermissionInput) (string, error)
	ListBlockedByIssues(context.Context, githubinfra.ViewIssueInput) ([]githubinfra.DependencyIssue, error)
	ListSubIssues(context.Context, githubinfra.ViewIssueInput) ([]githubinfra.DependencyIssue, error)
	AddIssueAssignees(context.Context, githubinfra.IssueAssigneesInput) error
	AddIssueLabels(context.Context, githubinfra.IssueLabelsInput) error
	AddIssueReaction(context.Context, githubinfra.CreateIssueReactionInput) error
	RemoveIssueLabels(context.Context, githubinfra.IssueLabelsInput) error
	CreateIssueComment(context.Context, githubinfra.IssueCommentInput) (githubinfra.IssueCommentResult, error)
	UpdateIssueComment(context.Context, githubinfra.UpdateIssueCommentInput) error
	DeleteIssueComment(context.Context, githubinfra.DeleteIssueCommentInput) error
	AddPullRequestLabels(context.Context, githubinfra.PullRequestLabelsInput) error
	ViewPullRequestMergeWatch(context.Context, githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error)
	ListPullRequestCheckRuns(context.Context, githubinfra.PullRequestCheckRunsInput) (githubinfra.PullRequestCheckRuns, error)
	GetBranchProtection(context.Context, githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error)
}

type RepositoryInspector interface {
	Inspect(context.Context, string, triage.Issue) (triage.RepoContext, error)
}

type NetworkAdmissionGateway interface {
	Status(context.Context) (protocol.NodeStatusResponse, error)
	RevalidateLease(context.Context, protocol.CoordinatorLeaseRevalidateRequest) error
}

type Options struct {
	Repos      *storage.Repositories
	GitHub     GitHubGateway
	Config     *config.Config
	Logger     bootstrap.Logger
	Now        func() time.Time
	TriageLLM  triage.LLM
	Inspector  RepositoryInspector
	Disclosure *config.DisclosureConfig
	Network    NetworkAdmissionGateway
}

type Runner struct {
	repos      *storage.Repositories
	github     GitHubGateway
	config     *config.Config
	logger     bootstrap.Logger
	now        func() time.Time
	triageLLM  triage.LLM
	inspector  RepositoryInspector
	disclosure *config.DisclosureConfig
	network    NetworkAdmissionGateway

	mu                sync.Mutex
	lastTickByProject map[string]time.Time
	watchLocks        map[string]*sync.Mutex
}

type loadedIssue struct {
	summary     githubinfra.IssueSummary
	detail      githubinfra.IssueDetail
	issue       triage.Issue
	rawTimeline []map[string]any
}

type dependencyState struct {
	enabled              bool
	graph                depgraph.DependencyGraph
	readySet             map[depgraph.IssueRef]struct{}
	tracked              map[depgraph.IssueRef]loadedIssue
	cycleCommentByIssue  map[int64]string
	retriageIssueNumbers map[int64]struct{}
	parentOrderByIssue   map[int64]issueOrder
	trackedIssueByNumber map[int64]depgraph.IssueRef
}

type issueOrder struct {
	parentNumber int64
	index        int
}

type downstreamTriggerLabels struct {
	reviewer     []string
	reviewerMode config.LabelMode
	fixer        []string
	fixerMode    config.LabelMode
	worker       []string
	workerMode   config.LabelMode
}

func New(options Options) *Runner {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	inspector := options.Inspector
	if inspector == nil {
		inspector = localRepositoryInspector{}
	}
	return &Runner{
		repos:             options.Repos,
		github:            options.GitHub,
		config:            options.Config,
		logger:            options.Logger,
		now:               now,
		triageLLM:         options.TriageLLM,
		inspector:         inspector,
		disclosure:        options.Disclosure,
		network:           options.Network,
		lastTickByProject: map[string]time.Time{},
		watchLocks:        map[string]*sync.Mutex{},
	}
}

func (r *Runner) DiscoverIssues(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	ctx = githubinfra.ContextWithDiscoverySnapshot(ctx, input.Snapshot)
	if !r.shouldRunTick(input.ProjectID) {
		return DiscoveryResult{Skipped: true}, nil
	}
	if r.github == nil {
		return DiscoveryResult{Ticked: true}, nil
	}
	if r.repos == nil || r.repos.Projects == nil {
		return DiscoveryResult{}, fmt.Errorf("coordinator repositories are not configured")
	}
	project, roleCfg, sweeperCfg, err := r.projectConfig(ctx, input.ProjectID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	if project.Archived || !roleCfg.Enabled {
		return DiscoveryResult{Skipped: true}, nil
	}
	issues, err := r.github.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{Repo: input.Repo, CWD: project.RepoPath, Limit: 100})
	if err != nil {
		return DiscoveryResult{}, err
	}
	triageCfg := roleConfigToTriageConfig(roleCfg)
	projectRoles := config.ProjectRoleConfigs(*r.config, input.ProjectID)
	dispatchCfg := roleConfigToDispatchConfig(roleCfg, projectRoles)
	downstreamLabels := downstreamTriggerLabels{
		reviewer:     append([]string(nil), projectRoles.Reviewer.Discovery.Triggers.Labels...),
		reviewerMode: projectRoles.Reviewer.Discovery.Triggers.LabelMode,
		fixer:        append([]string(nil), projectRoles.Fixer.Triggers.Labels...),
		fixerMode:    projectRoles.Fixer.Triggers.LabelMode,
		worker:       append([]string(nil), projectRoles.Worker.Triggers.Labels...),
		workerMode:   projectRoles.Worker.Triggers.LabelMode,
	}
	loaded := make([]loadedIssue, 0, len(issues))
	for _, summary := range issues {
		if ShouldSkipIssue(IssueSummary{Number: summary.Number, Labels: summary.Labels}, roleCfg, sweeperCfg) {
			continue
		}
		issue, err := r.loadIssue(ctx, input.Repo, project.RepoPath, summary)
		if err != nil {
			return DiscoveryResult{}, err
		}
		loaded = append(loaded, issue)
	}
	mergeWatchRetriggers, err := r.applyMergeWatch(ctx, input.Repo, project.RepoPath, loaded, config.ProjectRoleConfigs(*r.config, input.ProjectID))
	if err != nil {
		return DiscoveryResult{}, err
	}
	activeLoaded := filterLoadedIssues(loaded, mergeWatchRetriggers)

	deps, err := r.buildDependencyState(ctx, input.Repo, project.RepoPath, activeLoaded, triageCfg, dispatchCfg, roleCfg.Dependencies)
	if err != nil {
		return DiscoveryResult{}, err
	}
	if err := r.applyDependencyActions(ctx, input.Repo, project.RepoPath, triageCfg, deps); err != nil {
		return DiscoveryResult{}, err
	}
	if err := r.applyDispatches(ctx, input.ProjectID, input.Repo, project.RepoPath, activeLoaded, triageCfg, dispatchCfg, deps, downstreamLabels); err != nil {
		return DiscoveryResult{}, err
	}

	processed := 0
	for _, loadedIssue := range loaded {
		if _, skip := mergeWatchRetriggers[loadedIssue.issue.Number]; skip {
			continue
		}
		if _, skip := deps.retriageIssueNumbers[loadedIssue.issue.Number]; skip {
			continue
		}
		if processed >= triageCfg.MaxPerTick {
			continue
		}
		if !mightNeedCoordinatorAction(loadedIssue.summary, triageCfg) {
			continue
		}
		if !triage.ShouldReTriage(loadedIssue.issue, triageCfg, r.now().UTC()) && !triage.ShouldTriage(loadedIssue.issue, triageCfg, r.now().UTC()) {
			continue
		}
		analysisStartedAt := r.now().UTC()
		processed++
		decision, err := r.decide(ctx, project.RepoPath, input.Repo, loadedIssue.issue, triageCfg)
		if err != nil {
			return DiscoveryResult{}, err
		}
		if decision.NoOp {
			continue
		}
		if err := r.applyDecision(ctx, input.Repo, project.RepoPath, loadedIssue.issue, triageCfg, analysisStartedAt, decision); err != nil {
			return DiscoveryResult{}, err
		}
	}
	return DiscoveryResult{Ticked: true}, nil
}

func filterLoadedIssues(loaded []loadedIssue, skipped map[int64]struct{}) []loadedIssue {
	if len(skipped) == 0 {
		return loaded
	}
	filtered := make([]loadedIssue, 0, len(loaded))
	for _, item := range loaded {
		if _, skip := skipped[item.issue.Number]; skip {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func (r *Runner) buildDispatchDependencyGraph(ctx context.Context, repo, cwd string, depsCfg config.CoordinatorDependenciesConfig, dispatchCfg dispatch.Config, loaded []loadedCoordinatorIssue, now time.Time) (*depgraph.DependencyGraph, error) {
	if !depsCfg.Enabled {
		return nil, nil
	}
	candidates := dispatchDependencyCandidates(loaded, dispatchCfg, now)
	if len(candidates) == 0 {
		graph := depgraph.Build(nil, depgraph.Snapshot{})
		return &graph, nil
	}
	tracked := make([]depgraph.IssueRef, 0, len(candidates))
	snapshot := depgraph.Snapshot{
		BlockedBy:   make(map[depgraph.IssueRef][]depgraph.IssueRef, len(candidates)),
		Issues:      map[depgraph.IssueRef]depgraph.IssueState{},
		Unreachable: []depgraph.IssueRef{},
	}
	for _, issueNumber := range candidates {
		issueRef := depgraph.IssueRef{Repo: repo, Number: issueNumber}
		tracked = append(tracked, issueRef)
		blockedBy, err := r.listIssueBlockedByWithRetry(ctx, repo, cwd, issueNumber, depsCfg)
		if err != nil {
			return nil, err
		}
		for _, blocker := range blockedBy {
			blockerRef, blockerState, reachable := r.loadBlockerState(ctx, cwd, blocker, depsCfg)
			snapshot.BlockedBy[issueRef] = append(snapshot.BlockedBy[issueRef], blockerRef)
			if reachable {
				snapshot.Issues[blockerRef] = blockerState
				continue
			}
			snapshot.Unreachable = append(snapshot.Unreachable, blockerRef)
		}
	}
	graph := depgraph.Build(tracked, snapshot)
	return &graph, nil
}

func dispatchDependencyCandidates(loaded []loadedCoordinatorIssue, cfg dispatch.Config, now time.Time) []int64 {
	set := map[int64]struct{}{}
	for _, loadedIssue := range loaded {
		if !dispatch.NeedsDependencyGate(loadedIssue.dispatchIssue, cfg, now) {
			continue
		}
		set[loadedIssue.issue.Number] = struct{}{}
	}
	out := make([]int64, 0, len(set))
	for issueNumber := range set {
		out = append(out, issueNumber)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
func (r *Runner) listIssueBlockedByWithRetry(ctx context.Context, repo, cwd string, issueNumber int64, depsCfg config.CoordinatorDependenciesConfig) ([]githubinfra.IssueDependency, error) {
	var lastErr error
	attempts := maxDependencyAttempts(depsCfg.APIRetryAttempts)
	for attempt := 0; attempt < attempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, dependencyTimeout(depsCfg.APITimeoutSeconds))
		blockedBy, err := r.github.ListIssueBlockedBy(callCtx, githubinfra.ListIssueBlockedByInput{Repo: repo, IssueNumber: issueNumber, CWD: cwd})
		cancel()
		if err == nil {
			return blockedBy, nil
		}
		lastErr = err
		if !shouldRetryDependencyError(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (r *Runner) listBlockedByIssuesWithRetry(ctx context.Context, repo, cwd string, issueNumber int64, depsCfg config.CoordinatorDependenciesConfig) ([]githubinfra.DependencyIssue, error) {
	var lastErr error
	attempts := maxDependencyAttempts(depsCfg.APIRetryAttempts)
	for attempt := 0; attempt < attempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, dependencyTimeout(depsCfg.APITimeoutSeconds))
		blockedBy, err := r.github.ListBlockedByIssues(callCtx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: issueNumber, CWD: cwd})
		cancel()
		if err == nil {
			return blockedBy, nil
		}
		lastErr = err
		if !shouldRetryDependencyError(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (r *Runner) loadBlockerState(ctx context.Context, cwd string, blocker githubinfra.IssueDependency, depsCfg config.CoordinatorDependenciesConfig) (depgraph.IssueRef, depgraph.IssueState, bool) {
	blockerRef := depgraph.IssueRef{Repo: blocker.Repo, Number: blocker.Number}
	var lastErr error
	attempts := maxDependencyAttempts(depsCfg.APIRetryAttempts)
	for attempt := 0; attempt < attempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, dependencyTimeout(depsCfg.APITimeoutSeconds))
		state, err := r.github.GetIssueState(callCtx, githubinfra.ViewIssueInput{Repo: blocker.Repo, IssueNumber: blocker.Number, CWD: cwd})
		cancel()
		if err == nil {
			return blockerRef, depgraph.IssueState{State: strings.ToLower(strings.TrimSpace(state.State)), StateReason: strings.ToLower(strings.TrimSpace(state.StateReason))}, true
		}
		lastErr = err
		if !shouldRetryDependencyError(err) {
			break
		}
	}
	_ = lastErr
	return blockerRef, depgraph.IssueState{}, false
}

func dependencyTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

func maxDependencyAttempts(attempts int) int {
	if attempts <= 0 {
		return 1
	}
	return attempts
}

func shouldRetryDependencyError(err error) bool {
	if githubinfra.IsTransientError(err) {
		return true
	}
	message := strings.ToLower(githubinfra.ErrorMessage(err))
	return strings.Contains(message, "timed out") || strings.Contains(message, "context deadline exceeded")
}

func (r *Runner) hasDispatchWork(action dispatch.Action) bool {
	return action.ReactionCommentID != 0 || len(action.TriggerLabels) != 0 || action.FailureCommentBody != ""
}

// ShouldSkipIssue reserves the structural cross-role boundary with Sweeper.
// Future triage discovery must skip issues that Sweeper already marked pending,
// retired, or quarantined so the two roles never fight over authority.
func ShouldSkipIssue(issue IssueSummary, roleCfg config.CoordinatorRoleConfig, sweeperCfg config.SweeperRoleConfig) bool {
	_ = roleCfg
	return hasExactLabel(issue.Labels, sweeperCfg.Lifecycle.PendingLabel) ||
		hasExactLabel(issue.Labels, sweeperCfg.Lifecycle.ClosedLabel) ||
		hasExactLabel(issue.Labels, sweeperCfg.Security.QuarantineLabel)
}

func (r *Runner) decide(ctx context.Context, repoPath string, repo string, issue triage.Issue, cfg triage.Config) (triage.Decision, error) {
	reTriage := triage.ShouldReTriage(issue, cfg, r.now().UTC())
	if !reTriage && !triage.ShouldTriage(issue, cfg, r.now().UTC()) {
		return triage.NoOpDecision(), nil
	}
	repoCtx, err := r.inspector.Inspect(ctx, repoPath, issue)
	if err != nil {
		return triage.Decision{}, err
	}
	repoCtx.Repo = repo
	repoCtx.WorkingDirectory = repoPath
	return triage.Decide(ctx, r.triageLLM, triage.Input{Issue: issue, RepoContext: repoCtx, Config: cfg, Now: r.now().UTC()}), nil
}

func (r *Runner) applyDecision(ctx context.Context, repo string, cwd string, issue triage.Issue, cfg triage.Config, analysisStartedAt time.Time, decision triage.Decision) error {
	remainingLabels := append([]string(nil), issue.Labels...)
	hadTriaged := hasExactLabel(remainingLabels, cfg.TriagedLabel)
	if decision.MarkTriaged && hadTriaged {
		if err := r.removeIssueLabels(ctx, repo, cwd, issue.Number, remainingLabels, []string{cfg.TriagedLabel}); err != nil {
			return err
		}
		remainingLabels = removeExactLabels(remainingLabels, cfg.TriagedLabel)
	}
	clearNow, clearAfter := splitDelayedLabelPatterns(decision.ClearLabelPatterns, cfg.UnclearLabel, hadTriaged)
	if err := r.removeIssueLabels(ctx, repo, cwd, issue.Number, issue.Labels, clearNow); err != nil {
		return err
	}
	remainingLabels = removeMatchingLabels(remainingLabels, clearNow)
	removeNow, removeAfter := splitDelayedLabelPatterns(decision.RemoveLabels, cfg.UnclearLabel, hadTriaged)
	if err := r.removeIssueLabels(ctx, repo, cwd, issue.Number, issue.Labels, removeNow); err != nil {
		return err
	}
	remainingLabels = removeMatchingLabels(remainingLabels, removeNow)
	applyNow := removeExactLabels(decision.ApplyLabels, cfg.TriagedLabel)
	if len(applyNow) > 0 {
		if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: repo, IssueNumber: issue.Number, Labels: applyNow, CWD: cwd}); err != nil {
			return err
		}
	}
	clearAfter = removeAppliedLabelPatterns(clearAfter, decision.ApplyLabels)
	removeAfter = removeAppliedLabelPatterns(removeAfter, decision.ApplyLabels)
	commentPosted := true
	if strings.TrimSpace(decision.CommentBody) != "" {
		posted, err := r.postOrEditComment(ctx, repo, cwd, issue, analysisStartedAt, decision.CommentBody)
		if err != nil {
			return err
		}
		commentPosted = posted
	}
	shouldMarkTriaged := decision.MarkTriaged && (!hadTriaged || commentPosted)
	if shouldMarkTriaged && !hasExactLabel(remainingLabels, cfg.TriagedLabel) {
		if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: repo, IssueNumber: issue.Number, Labels: []string{cfg.TriagedLabel}, CWD: cwd}); err != nil {
			return err
		}
	}
	if commentPosted {
		if err := r.removeIssueLabels(ctx, repo, cwd, issue.Number, remainingLabels, clearAfter); err != nil {
			return err
		}
		if err := r.removeIssueLabels(ctx, repo, cwd, issue.Number, remainingLabels, removeAfter); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) applyDispatchAction(ctx context.Context, projectID string, repo string, cwd string, issue triage.Issue, action dispatch.Action, dispatchCfg dispatch.Config) (bool, error) {
	if strings.TrimSpace(action.FailureCommentBody) != "" {
		if err := r.postOrEditDispatchFailureComment(ctx, repo, cwd, issue.Number, action.FailureCommentBody); err != nil {
			return false, err
		}
		if action.ReactionCommentID != 0 && strings.TrimSpace(action.ReactionContent) != "" {
			if err := r.github.AddIssueReaction(ctx, githubinfra.CreateIssueReactionInput{Repo: repo, CommentID: action.ReactionCommentID, Content: action.ReactionContent, CWD: cwd}); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	if intent := r.workerAdmissionIntent(issue, action, dispatchCfg); intent.Active {
		if r.projectNetworkMode(projectID) == config.ProjectNetworkModeRouted {
			return r.applyRoutedWorkerAdmission(ctx, repo, cwd, issue, action, intent)
		}
		return r.applyLocalWorkerAdmission(ctx, repo, cwd, issue, action, intent.TriggerLabels)
	}
	return r.applyGenericDispatchAction(ctx, repo, cwd, issue, action)

}

func (r *Runner) applyGenericDispatchAction(ctx context.Context, repo string, cwd string, issue triage.Issue, action dispatch.Action) (bool, error) {
	mutated := false
	if strings.TrimSpace(action.AssignTo) != "" {
		if err := r.github.AddIssueAssignees(ctx, githubinfra.IssueAssigneesInput{Repo: repo, IssueNumber: issue.Number, Assignees: []string{action.AssignTo}, CWD: cwd}); err != nil {
			return false, err
		}
		mutated = true
	}
	labelsToAdd := removeExistingLabels(action.TriggerLabels, issue.Labels)
	if len(labelsToAdd) > 0 {
		if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: repo, IssueNumber: issue.Number, Labels: labelsToAdd, CWD: cwd}); err != nil {
			return false, err
		}
		mutated = true
	}
	if action.ReactionCommentID != 0 && strings.TrimSpace(action.ReactionContent) != "" {
		if err := r.github.AddIssueReaction(ctx, githubinfra.CreateIssueReactionInput{Repo: repo, CommentID: action.ReactionCommentID, Content: action.ReactionContent, CWD: cwd}); err != nil {
			return false, err
		}
		mutated = true
	}
	return mutated, nil
}

type workerAdmissionIntent struct {
	Active        bool
	TriggerLabels []string
}

func (r *Runner) workerAdmissionIntent(issue triage.Issue, action dispatch.Action, cfg dispatch.Config) workerAdmissionIntent {
	desired := append([]string(nil), cfg.WorkerTriggerLabels...)
	if len(desired) == 0 {
		return workerAdmissionIntent{}
	}
	if len(intersectExactLabels(action.TriggerLabels, desired)) > 0 {
		return workerAdmissionIntent{Active: true, TriggerLabels: desired}
	}
	if hasExactLabel(issue.Labels, dispatch.DispatchPlan) {
		return workerAdmissionIntent{}
	}
	if len(intersectExactLabels(issue.Labels, desired)) > 0 {
		return workerAdmissionIntent{Active: true, TriggerLabels: desired}
	}
	return workerAdmissionIntent{}
}

func (r *Runner) applyLocalWorkerAdmission(ctx context.Context, repo string, cwd string, issue triage.Issue, action dispatch.Action, triggerLabels []string) (bool, error) {
	localAction := action
	localAction.TriggerLabels = triggerLabels
	return r.applyGenericDispatchAction(ctx, repo, cwd, issue, localAction)
}

func (r *Runner) applyRoutedWorkerAdmission(ctx context.Context, repo string, cwd string, issue triage.Issue, action dispatch.Action, intent workerAdmissionIntent) (bool, error) {
	mutated := false
	if r.network == nil {
		return false, fmt.Errorf("coordinator network admission is not configured")
	}
	status, err := r.network.Status(ctx)
	if err != nil {
		return false, err
	}
	if !r.currentNodeHoldsLease(status) {
		return false, nil
	}
	selected, ok := selectEligibleWorkerNode(status.Memberships, r.now().UTC())
	if !ok {
		if err := r.postOrEditDispatchFailureComment(ctx, repo, cwd, issue.Number, fmt.Sprintf("Coordinator can’t route this implementation Issue right now because no eligible Worker Node is available (`%s`).", noEligibleNodeStatus)); err != nil {
			return false, err
		}
		mutated = true
		if action.ReactionCommentID != 0 {
			if err := r.github.AddIssueReaction(ctx, githubinfra.CreateIssueReactionInput{Repo: repo, CommentID: action.ReactionCommentID, Content: dispatch.ReactionFailure, CWD: cwd}); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	if err := r.revalidateCoordinatorLease(ctx, issue.URL, status.Lease.FencingToken); err != nil {
		if isStaleCoordinatorLeaseError(err) {
			return false, nil
		}
		return false, err
	}
	if login := strings.TrimSpace(selected.GitHub.Login); login != "" {
		if err := r.github.AddIssueAssignees(ctx, githubinfra.IssueAssigneesInput{Repo: repo, IssueNumber: issue.Number, Assignees: []string{login}, CWD: cwd}); err != nil {
			return false, err
		}
		mutated = true
	}
	labelsToAdd := removeExistingLabels(intent.TriggerLabels, issue.Labels)
	if len(labelsToAdd) > 0 {
		if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: repo, IssueNumber: issue.Number, Labels: labelsToAdd, CWD: cwd}); err != nil {
			return false, err
		}
		mutated = true
	}
	if err := r.revalidateCoordinatorLease(ctx, issue.URL, status.Lease.FencingToken); err != nil {
		if isStaleCoordinatorLeaseError(err) {
			return mutated, nil
		}
		return false, err
	}
	exactTarget := protocol.TargetLabelForNode(selected.NodeName)
	labelsToRemove := nonExactTargetLabels(issue.Labels, exactTarget)
	if len(labelsToRemove) > 0 {
		if err := r.github.RemoveIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: repo, IssueNumber: issue.Number, Labels: labelsToRemove, CWD: cwd}); err != nil {
			return false, err
		}
		mutated = true
	}
	if !hasExactLabel(issue.Labels, exactTarget) {
		if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: repo, IssueNumber: issue.Number, Labels: []string{exactTarget}, CWD: cwd}); err != nil {
			return false, err
		}
		mutated = true
	}
	if action.ReactionCommentID != 0 && strings.TrimSpace(action.ReactionContent) != "" {
		if err := r.github.AddIssueReaction(ctx, githubinfra.CreateIssueReactionInput{Repo: repo, CommentID: action.ReactionCommentID, Content: action.ReactionContent, CWD: cwd}); err != nil {
			return false, err
		}
		mutated = true
	}
	return mutated, nil
}

func (r *Runner) currentNodeHoldsLease(status protocol.NodeStatusResponse) bool {
	if status.Lease.FencingToken == 0 || strings.TrimSpace(status.Lease.HolderNodeID) == "" {
		return false
	}
	if status.Lease.ExpiresAt == nil || !status.Lease.ExpiresAt.After(r.now().UTC()) {
		return false
	}
	return strings.TrimSpace(status.Lease.HolderNodeID) == strings.TrimSpace(status.Membership.NodeID)
}

func (r *Runner) revalidateCoordinatorLease(ctx context.Context, issueURL string, fencingToken int64) error {
	if r.network == nil || fencingToken == 0 {
		return nil
	}
	return r.network.RevalidateLease(ctx, protocol.CoordinatorLeaseRevalidateRequest{FencingToken: fencingToken, URL: revalidateProbeURL(issueURL), Method: "GET"})
}

func selectEligibleWorkerNode(members []protocol.Membership, now time.Time) (protocol.Membership, bool) {
	eligible := make([]protocol.Membership, 0, len(members))
	for _, member := range members {
		if !memberHasRole(member, "worker") || member.DuplicateWarning || member.Capabilities.IdentityDrift {
			continue
		}
		if strings.TrimSpace(member.NodeName) == "" || strings.TrimSpace(member.GitHub.Login) == "" {
			continue
		}
		if member.LastHeartbeatAt == nil || member.LastHeartbeatAt.Before(now.Add(-2*protocol.DefaultLeaseTTL)) {
			continue
		}
		if !hasExactLabel(member.TargetLabels, protocol.TargetLabelForNode(member.NodeName)) {
			continue
		}
		eligible = append(eligible, member)
	}
	if len(eligible) == 0 {
		return protocol.Membership{}, false
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Capabilities.DynamicLoad != eligible[j].Capabilities.DynamicLoad {
			return eligible[i].Capabilities.DynamicLoad < eligible[j].Capabilities.DynamicLoad
		}
		return eligible[i].NodeName < eligible[j].NodeName
	})
	return eligible[0], true
}

func memberHasRole(member protocol.Membership, want string) bool {
	for _, role := range member.Capabilities.Roles {
		if strings.EqualFold(strings.TrimSpace(role), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func intersectExactLabels(left []string, right []string) []string {
	if len(left) == 0 || len(right) == 0 {
		return nil
	}
	set := map[string]struct{}{}
	for _, label := range right {
		set[label] = struct{}{}
	}
	out := []string{}
	for _, label := range left {
		if _, ok := set[label]; ok {
			out = append(out, label)
		}
	}
	return out
}

func revalidateProbeURL(issueURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(issueURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimSpace(issueURL)
	}
	parsed.Path = "/"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func isStaleCoordinatorLeaseError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "stale coordinator lease token") || strings.Contains(message, "coordinator lease is already held")
}

func (r *Runner) buildDependencyState(ctx context.Context, repo, cwd string, loaded []loadedIssue, triageCfg triage.Config, dispatchCfg dispatch.Config, depsCfg config.CoordinatorDependenciesConfig) (dependencyState, error) {
	state := dependencyState{
		enabled:              depsCfg.Enabled,
		readySet:             map[depgraph.IssueRef]struct{}{},
		tracked:              map[depgraph.IssueRef]loadedIssue{},
		cycleCommentByIssue:  map[int64]string{},
		retriageIssueNumbers: map[int64]struct{}{},
		parentOrderByIssue:   map[int64]issueOrder{},
		trackedIssueByNumber: map[int64]depgraph.IssueRef{},
	}
	if !depsCfg.Enabled {
		return state, nil
	}

	snapshot := depgraph.Snapshot{BlockedBy: map[depgraph.IssueRef][]depgraph.IssueRef{}, Issues: map[depgraph.IssueRef]depgraph.IssueState{}}
	trackedRefs := make([]depgraph.IssueRef, 0, len(loaded))
	for _, item := range loaded {
		if !dependencyTrackedIssue(item.issue, triageCfg.TriagedLabel, dispatchCfg) {
			continue
		}
		ref := depgraph.IssueRef{Repo: normalizeDependencyRepo(repo), Number: item.issue.Number}
		trackedRefs = append(trackedRefs, ref)
		state.tracked[ref] = item
		state.trackedIssueByNumber[item.issue.Number] = ref
		snapshot.Issues[ref] = depgraph.IssueState{State: item.detail.State, StateReason: item.detail.StateReason}
		blockers, err := r.listBlockedByIssuesWithRetry(ctx, repo, cwd, item.issue.Number, depsCfg)
		if err != nil {
			return dependencyState{}, err
		}
		for _, blocker := range blockers {
			blockerRef := dependencyIssueRef(repo, blocker)
			snapshot.BlockedBy[ref] = append(snapshot.BlockedBy[ref], blockerRef)
			stateInfo := depgraph.IssueState{State: strings.ToLower(strings.TrimSpace(blocker.State)), StateReason: strings.ToLower(strings.TrimSpace(blocker.StateReason))}
			if stateInfo.State == "" {
				resolvedRef, resolvedState, reachable := r.loadBlockerState(ctx, cwd, githubinfra.IssueDependency{Number: blocker.Number, Repo: blockerRef.Repo}, depsCfg)
				if reachable {
					blockerRef = resolvedRef
					stateInfo = resolvedState
				}
			}
			snapshot.Issues[blockerRef] = stateInfo
		}
	}
	if len(trackedRefs) == 0 {
		return state, nil
	}

	state.graph = depgraph.Build(trackedRefs, snapshot)
	for _, ref := range state.graph.ReadySet() {
		state.readySet[ref] = struct{}{}
	}
	for _, cycle := range state.graph.Cycles() {
		comment := cycleCommentBody(cycle)
		for _, ref := range cycle[:len(cycle)-1] {
			state.cycleCommentByIssue[ref.Number] = comment
			state.retriageIssueNumbers[ref.Number] = struct{}{}
		}
	}
	for _, ref := range trackedRefs {
		for _, blocker := range state.graph.BlockersOf(ref) {
			if blocker.RequiresReTriage {
				state.retriageIssueNumbers[ref.Number] = struct{}{}
				break
			}
		}
	}
	if err := r.populateParentOrdering(ctx, repo, cwd, loaded, &state); err != nil {
		return dependencyState{}, err
	}
	return state, nil
}

func (r *Runner) populateParentOrdering(ctx context.Context, repo, cwd string, loaded []loadedIssue, deps *dependencyState) error {
	if deps == nil || len(deps.readySet) < 2 {
		return nil
	}
	readyNumbers := map[int64]struct{}{}
	for ref := range deps.readySet {
		readyNumbers[ref.Number] = struct{}{}
	}
	for _, item := range loaded {
		subIssues, err := r.github.ListSubIssues(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: item.issue.Number, CWD: cwd})
		if err != nil {
			continue
		}
		for index, subIssue := range subIssues {
			if _, ok := readyNumbers[subIssue.Number]; !ok {
				continue
			}
			deps.parentOrderByIssue[subIssue.Number] = issueOrder{parentNumber: item.issue.Number, index: index}
		}
	}
	return nil
}

func (r *Runner) applyDependencyActions(ctx context.Context, repo, cwd string, triageCfg triage.Config, deps dependencyState) error {
	refs := make([]depgraph.IssueRef, 0, len(deps.tracked))
	for ref := range deps.tracked {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Number < refs[j].Number
	})
	for _, ref := range refs {
		item := deps.tracked[ref]
		if _, ok := deps.retriageIssueNumbers[ref.Number]; !ok {
			continue
		}
		if err := r.removeIssueLabels(ctx, repo, cwd, item.issue.Number, item.issue.Labels, []string{triageCfg.TriagedLabel, "dispatch/*"}); err != nil {
			return err
		}
		if commentBody, ok := deps.cycleCommentByIssue[item.issue.Number]; ok {
			if err := r.postCycleComment(ctx, repo, cwd, item.issue.Number, commentBody); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runner) applyDispatches(ctx context.Context, projectID, repo, cwd string, loaded []loadedIssue, triageCfg triage.Config, dispatchCfg dispatch.Config, deps dependencyState, downstreamLabels downstreamTriggerLabels) error {
	if dispatchCfg.Mode == dispatch.ModeAutonomous {
		return r.applyAutonomousDispatches(ctx, projectID, repo, cwd, loaded, triageCfg, dispatchCfg, deps, downstreamLabels)
	}
	for _, item := range loaded {
		if _, skip := deps.retriageIssueNumbers[item.issue.Number]; skip {
			continue
		}
		dispatchIssue, err := r.dispatchIssue(ctx, repo, cwd, item.issue, triageCfg.TriagedLabel, dispatchCfg)
		if err != nil {
			return err
		}
		action := dispatch.Decide(dispatchIssue, dispatchCfg, r.now().UTC(), &deps.graph)
		action = applyHumanDependencyGate(action, item.issue.Number, deps)
		if r.hasDispatchWork(action) || r.workerAdmissionIntent(item.issue, action, dispatchCfg).Active {
			if _, err := r.applyDispatchAction(ctx, projectID, repo, cwd, item.issue, action, dispatchCfg); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runner) applyAutonomousDispatches(ctx context.Context, projectID, repo, cwd string, loaded []loadedIssue, triageCfg triage.Config, dispatchCfg dispatch.Config, deps dependencyState, downstreamLabels downstreamTriggerLabels) error {
	ready := make([]autonomousDispatchCandidate, 0, len(loaded))
	for _, item := range loaded {
		if _, skip := deps.retriageIssueNumbers[item.issue.Number]; skip {
			continue
		}
		dispatchIssue, err := r.dispatchIssue(ctx, repo, cwd, item.issue, triageCfg.TriagedLabel, dispatchCfg)
		if err != nil {
			return err
		}
		action := dispatch.Decide(dispatchIssue, dispatchCfg, r.now().UTC(), &deps.graph)
		if strings.TrimSpace(action.FailureCommentBody) != "" {
			continue
		}
		if !r.hasDispatchWork(action) && !r.workerAdmissionIntent(item.issue, action, dispatchCfg).Active {
			continue
		}
		if deps.enabled {
			ref, tracked := deps.trackedIssueByNumber[item.issue.Number]
			if tracked {
				if _, ok := deps.readySet[ref]; !ok {
					continue
				}
			}
		}
		ready = append(ready, autonomousDispatchCandidate{issue: item.issue, action: action, order: deps.parentOrderByIssue[item.issue.Number], worker: isWorkerDispatch(item.issue)})
	}
	sortAutonomousDispatchCandidates(ready)
	budget, preemptWorkers, err := r.dispatchBudget(ctx, projectID, repo, cwd, loaded, ready, downstreamLabels)
	if err != nil {
		return err
	}
	dispatched := 0
	for _, candidate := range ready {
		if preemptWorkers && candidate.worker {
			continue
		}
		if dispatched >= budget {
			break
		}
		mutated, err := r.applyDispatchAction(ctx, projectID, repo, cwd, candidate.issue, candidate.action, dispatchCfg)
		if err != nil {
			return err
		}
		if mutated {
			dispatched++
		}
	}
	return nil
}

type autonomousDispatchCandidate struct {
	issue  triage.Issue
	action dispatch.Action
	order  issueOrder
	worker bool
}

func sortAutonomousDispatchCandidates(candidates []autonomousDispatchCandidate) {
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.order.parentNumber != 0 && left.order.parentNumber == right.order.parentNumber && left.order.index != right.order.index {
			return left.order.index < right.order.index
		}
		return left.issue.Number < right.issue.Number
	})
}

func (r *Runner) dispatchBudget(ctx context.Context, projectID, repo, cwd string, loaded []loadedIssue, ready []autonomousDispatchCandidate, downstreamLabels downstreamTriggerLabels) (int, bool, error) {
	if r == nil || r.config == nil || r.config.Scheduler.MaxConcurrentRuns <= 0 {
		return int(^uint(0) >> 1), false, nil
	}
	maxConcurrentRuns := r.config.Scheduler.MaxConcurrentRuns
	running, err := r.runningQueueItems(ctx)
	if err != nil {
		return 0, false, err
	}
	if running >= maxConcurrentRuns {
		return 0, false, nil
	}
	budget := maxConcurrentRuns - running
	readyWorkers := 0
	for _, candidate := range ready[:min(len(ready), budget)] {
		if candidate.worker {
			readyWorkers++
		}
	}
	if readyWorkers > 0 && running+readyWorkers >= maxConcurrentRuns {
		pending, err := r.hasPendingReviewerOrFixerWork(ctx, projectID, repo, cwd, loaded, downstreamLabels)
		if err != nil {
			return 0, false, err
		}
		if pending {
			return budget, true, nil
		}
	}
	return budget, false, nil
}

func (r *Runner) runningQueueItems(ctx context.Context) (int, error) {
	if r == nil || r.repos == nil || r.repos.Queue == nil {
		return 0, nil
	}
	count, err := r.repos.Queue.CountByStatus(ctx, "running")
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

func (r *Runner) hasPendingReviewerOrFixerWork(ctx context.Context, projectID, repo, cwd string, loaded []loadedIssue, downstreamLabels downstreamTriggerLabels) (bool, error) {
	if r == nil || r.config == nil || r.repos == nil || r.github == nil {
		return false, nil
	}
	roles := config.ProjectRoleConfigs(*r.config, projectID)
	reviewerConfig := roles.Reviewer.Discovery.Triggers
	fixerConfig := roles.Fixer.Triggers
	reviewerLabels := downstreamLabels.reviewer
	fixerLabels := downstreamLabels.fixer
	active, err := r.activeQueueItemsByPR(ctx)
	if err != nil {
		return false, err
	}
	currentLogin := ""
	loadedCurrentLogin := false
	for _, issue := range loaded {
		prs, err := r.github.ListLinkedPullRequests(ctx, githubinfra.LinkedPullRequestsInput{Repo: repo, IssueNumber: issue.issue.Number, CWD: cwd})
		if err != nil {
			return false, err
		}
		for _, pr := range prs {
			if !strings.EqualFold(strings.TrimSpace(pr.State), "OPEN") {
				continue
			}
			detail, err := r.github.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: pr.Number, CWD: cwd})
			if err != nil {
				return false, err
			}
			prKey := queuePullRequestKey(repo, pr.Number)
			if !loadedCurrentLogin && (reviewerConfig.RequireReviewRequest || !reviewerConfig.EnableSelfReview || fixerConfig.AuthorFilter != config.FixerAuthorFilterAny) {
				lookupLogin, err := r.github.GetCurrentUserLoginForRepo(ctx, repo, cwd)
				if err != nil {
					return false, err
				}
				currentLogin = normalizeLogin(lookupLogin)
				loadedCurrentLogin = true
			}
			if !active["reviewer"][prKey] {
				if reviewerWorkPending(detail, currentLogin, reviewerConfig, reviewerLabels, downstreamLabels.reviewerMode) {
					return true, nil
				}
			}
			if !active["fixer"][prKey] && fixerWorkPending(detail, currentLogin, fixerConfig, fixerLabels, downstreamLabels.fixerMode) {
				return true, nil
			}
		}
	}
	return false, nil
}

func reviewerWorkPending(detail githubinfra.PullRequestDetail, currentLogin string, trigger config.ReviewerRoleTriggersConfig, requiredLabels []string, labelMode config.LabelMode) bool {
	if !trigger.IncludeDrafts && detail.IsDraft {
		return false
	}
	if !trigger.EnableSelfReview && normalizeLogin(detail.Author) != "" && normalizeLogin(detail.Author) == normalizeLogin(currentLogin) {
		return false
	}
	if trigger.RequireReviewRequest && !isCurrentUserRequested(detail.ReviewRequests, currentLogin) {
		return false
	}
	return labelsMatch(detail.Labels, requiredLabels, labelMode)
}

func fixerWorkPending(detail githubinfra.PullRequestDetail, currentLogin string, trigger config.FixerRoleTriggersConfig, requiredLabels []string, labelMode config.LabelMode) bool {
	if !trigger.IncludeDrafts && detail.IsDraft {
		return false
	}
	if trigger.AuthorFilter != config.FixerAuthorFilterAny && normalizeLogin(detail.Author) != "" && normalizeLogin(detail.Author) != normalizeLogin(currentLogin) {
		return false
	}
	if !labelsMatch(detail.Labels, requiredLabels, labelMode) {
		return false
	}
	if detail.HasConflicts {
		return true
	}
	for _, comment := range detail.Comments {
		if !commentResolved(comment) {
			return true
		}
	}
	for _, check := range detail.Checks {
		if failingCheck(check) {
			return true
		}
	}
	return false
}

func (r *Runner) activeQueueItemsByPR(ctx context.Context) (map[string]map[string]bool, error) {
	active := map[string]map[string]bool{"reviewer": {}, "fixer": {}}
	if r == nil || r.repos == nil || r.repos.Queue == nil {
		return active, nil
	}
	items, err := r.repos.Queue.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.Status != "queued" && item.Status != "running" {
			continue
		}
		if item.Repo == nil || item.PRNumber == nil {
			continue
		}
		if _, ok := active[item.Type]; !ok {
			continue
		}
		active[item.Type][queuePullRequestKey(*item.Repo, *item.PRNumber)] = true
	}
	return active, nil
}

func queuePullRequestKey(repo string, prNumber int64) string {
	return fmt.Sprintf("%s#%d", repo, prNumber)
}

func isWorkerDispatch(issue triage.Issue) bool {
	return specpr.HasLabel(issue.Labels, dispatch.DispatchImplement)
}

func labelsMatch(labels, expected []string, mode config.LabelMode) bool {
	if len(expected) == 0 {
		return true
	}
	if mode == config.LabelModeAny {
		for _, label := range expected {
			if specpr.HasLabel(labels, label) {
				return true
			}
		}
		return false
	}
	for _, label := range expected {
		if !specpr.HasLabel(labels, label) {
			return false
		}
	}
	return true
}

func normalizeLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

func isCurrentUserRequested(requested []string, currentLogin string) bool {
	currentLogin = normalizeLogin(currentLogin)
	if currentLogin == "" {
		return false
	}
	for _, login := range requested {
		if normalizeLogin(login) == currentLogin {
			return true
		}
	}
	return false
}

func commentResolved(comment map[string]any) bool {
	if state, ok := comment["state"].(string); ok && strings.EqualFold(strings.TrimSpace(state), "resolved") {
		return true
	}
	if resolved, ok := comment["isResolved"].(bool); ok && resolved {
		return true
	}
	return false
}

func failingCheck(check map[string]any) bool {
	state, _ := check["conclusion"].(string)
	if strings.TrimSpace(state) == "" {
		state, _ = check["state"].(string)
	}
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "FAILURE", "FAILED", "ERROR", "TIMED_OUT", "ACTION_REQUIRED":
		return true
	default:
		return false
	}
}

func applyHumanDependencyGate(action dispatch.Action, issueNumber int64, deps dependencyState) dispatch.Action {
	if !deps.enabled || action.ReactionCommentID == 0 || len(action.TriggerLabels) == 0 || strings.TrimSpace(action.FailureCommentBody) != "" {
		return action
	}
	ref, ok := deps.trackedIssueByNumber[issueNumber]
	if !ok {
		return action
	}
	if _, ready := deps.readySet[ref]; ready {
		return action
	}
	action.NoOp = true
	action.TriggerLabels = nil
	action.AssignTo = ""
	action.ReactionContent = dispatch.ReactionFailure
	action.FailureCommentBody = dependencyFailureCommentBody(deps.graph.BlockersOf(ref))
	return action
}

func dependencyFailureCommentBody(blockers []depgraph.Blocker) string {
	parts := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		state := blocker.State
		switch {
		case blocker.Unreachable:
			state = "unreachable"
		case strings.TrimSpace(blocker.StateReason) != "":
			state = blocker.StateReason
		case strings.TrimSpace(state) == "":
			state = "blocking"
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", blocker.Issue.String(), state))
	}
	if len(parts) == 0 {
		return "Coordinator can't dispatch until the dependency gate releases."
	}
	return "Coordinator can't dispatch until the dependency gate releases. Blocked by: " + strings.Join(parts, ", ") + "."
}

func cycleCommentBody(cycle depgraph.Cycle) string {
	parts := make([]string, 0, len(cycle))
	for _, ref := range cycle {
		parts = append(parts, ref.String())
	}
	return fmt.Sprintf("This Issue is part of a `blocked_by` cycle: %s. Coordinator has returned the cycle members to the re-Triage candidate set. Resolve the cycle by editing the `blocked_by` relationships, then re-Triage will form a fresh Disposition.", strings.Join(parts, " → "))
}

func dependencyTrackedIssue(issue triage.Issue, triagedLabel string, dispatchCfg dispatch.Config) bool {
	if !hasExactLabel(issue.Labels, triagedLabel) {
		return false
	}
	dispatchLabel, ok := issueDispatchLabel(issue.Labels)
	if !ok {
		return false
	}
	triggerLabels := configuredTriggerLabels(dispatchLabel, dispatchCfg)
	if len(triggerLabels) == 0 {
		return false
	}
	return len(removeExistingLabels(triggerLabels, issue.Labels)) > 0
}

func issueDispatchLabel(labels []string) (string, bool) {
	match := ""
	for _, label := range labels {
		if !strings.HasPrefix(label, "dispatch/") {
			continue
		}
		if match != "" {
			return "", false
		}
		match = label
	}
	return match, match != ""
}

func configuredTriggerLabels(dispatchLabel string, cfg dispatch.Config) []string {
	switch dispatchLabel {
	case dispatch.DispatchPlan:
		return append([]string(nil), cfg.PlannerTriggerLabels...)
	case dispatch.DispatchImplement:
		return append([]string(nil), cfg.WorkerTriggerLabels...)
	default:
		return nil
	}
}

func dependencyIssueRef(defaultRepo string, issue githubinfra.DependencyIssue) depgraph.IssueRef {
	repo := strings.TrimSpace(issue.Repository.FullName)
	if repo == "" {
		repo = defaultRepo
	}
	return depgraph.IssueRef{Repo: normalizeDependencyRepo(repo), Number: issue.Number}
}

func normalizeDependencyRepo(repo string) string {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	parts := strings.Split(repo, "/")
	if len(parts) >= 3 {
		return strings.Join(parts[len(parts)-2:], "/")
	}
	return repo
}

func (r *Runner) postOrEditComment(ctx context.Context, repo, cwd string, issue triage.Issue, analysisStartedAt time.Time, body string) (bool, error) {
	comments, err := r.github.ListIssueComments(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: issue.Number, CWD: cwd})
	if err != nil {
		return false, err
	}
	currentLogin, err := r.github.GetCurrentUserLoginForRepo(ctx, repo, cwd)
	if err != nil {
		return false, err
	}
	existing := findMarkerComment(comments, currentLogin)
	if hasNewHumanComment(comments, knownCommentIDs(issue.Comments), analysisStartedAt) {
		return false, nil
	}
	commentBody := triageCommentMarker + "\n\n" + body
	stamper := disclosure.FromConfig(*r.config)
	commentBody = stamper.Markdown(commentBody, "coordinator", disclosure.ChannelIssueComment)
	if existing != nil {
		return true, r.github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: repo, CommentID: existing.ID, Body: commentBody, CWD: cwd})
	}
	_, err = r.github.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: repo, IssueNumber: issue.Number, Body: commentBody, CWD: cwd})
	return err == nil, err
}

func (r *Runner) postOrEditDispatchFailureComment(ctx context.Context, repo, cwd string, issueNumber int64, body string) error {
	comments, err := r.github.ListIssueComments(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: issueNumber, CWD: cwd})
	if err != nil {
		return err
	}
	currentLogin, err := r.github.GetCurrentUserLoginForRepo(ctx, repo, cwd)
	if err != nil {
		return err
	}
	existing := findDispatchFailureComment(comments, currentLogin)
	commentBody := dispatchFailureCommentMarker + "\n\n" + strings.TrimSpace(body)
	stamper := disclosure.FromConfig(*r.config)
	commentBody = stamper.Markdown(commentBody, "coordinator", disclosure.ChannelIssueComment)
	if existing != nil {
		return r.github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: repo, CommentID: existing.ID, Body: commentBody, CWD: cwd})
	}
	_, err = r.github.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: repo, IssueNumber: issueNumber, Body: commentBody, CWD: cwd})
	return err
}

func (r *Runner) removeIssueLabels(ctx context.Context, repo, cwd string, issueNumber int64, existing []string, patterns []string) error {
	labels := matchingLabels(existing, patterns)
	if len(labels) == 0 {
		return nil
	}
	return r.github.RemoveIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: repo, IssueNumber: issueNumber, Labels: labels, CWD: cwd})
}

func (r *Runner) loadIssue(ctx context.Context, repo, cwd string, summary githubinfra.IssueSummary) (loadedIssue, error) {
	detail, err := r.github.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: summary.Number, CWD: cwd})
	if err != nil {
		return loadedIssue{}, err
	}
	timeline, err := r.github.ListIssueTimeline(ctx, githubinfra.IssueTimelineInput{Repo: repo, IssueNumber: summary.Number, CWD: cwd})
	if err != nil {
		return loadedIssue{}, err
	}
	issue := triage.Issue{
		Number:    detail.Number,
		Title:     detail.Title,
		Body:      detail.Body,
		URL:       detail.URL,
		Author:    detail.Author,
		CreatedAt: detail.CreatedAt,
		UpdatedAt: detail.UpdatedAt,
		Labels:    append([]string(nil), detail.Labels...),
		Comments:  make([]triage.Comment, 0, len(detail.Comments)),
		Timeline:  make([]triage.TimelineEvent, 0, len(timeline)),
	}
	for _, comment := range detail.Comments {
		issue.Comments = append(issue.Comments, triage.Comment{ID: comment.ID, Author: comment.Author, AuthorAssociation: comment.AuthorAssociation, Body: comment.Body, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt})
	}
	for _, event := range timeline {
		issue.Timeline = append(issue.Timeline, triage.TimelineEvent{Event: strings.TrimSpace(asString(event["event"])), CreatedAt: firstNonEmpty(asString(event["created_at"]), asString(event["createdAt"])), Label: timelineLabelName(event)})
	}
	return loadedIssue{summary: summary, detail: detail, issue: issue, rawTimeline: append([]map[string]any(nil), timeline...)}, nil
}

func roleConfigToDispatchConfig(roleCfg config.CoordinatorRoleConfig, roles config.RoleConfigs) dispatch.Config {
	return dispatch.Config{
		Mode:                 roleCfg.Dispatch.Mode,
		TriagedLabel:         roleCfg.Triage.TriagedLabel,
		HoldLabel:            roleCfg.Dispatch.Autonomous.HoldLabel,
		AutonomousDelay:      time.Duration(roleCfg.Dispatch.Autonomous.DelayMinutes) * time.Minute,
		AllowedUsers:         append([]string(nil), roleCfg.Dispatch.HumanGate.AllowedUsers...),
		SlashCommands:        append([]string(nil), roleCfg.Dispatch.HumanGate.SlashCommands...),
		AssignTo:             roleCfg.Dispatch.AssignTo,
		PlannerTriggerLabels: requiredTriggerLabels(roles.Planner.Triggers),
		WorkerTriggerLabels:  requiredTriggerLabels(roles.Worker.Triggers),
	}
}

func requiredTriggerLabels(cfg config.IssueRoleTriggersConfig) []string {
	if cfg.LabelMode == config.LabelModeAll {
		return append([]string(nil), cfg.Labels...)
	}
	if len(cfg.Labels) == 0 {
		return nil
	}
	return []string{cfg.Labels[0]}
}

func (r *Runner) dispatchIssue(ctx context.Context, repo, cwd string, issue triage.Issue, triagedLabel string, cfg dispatch.Config) (dispatch.Issue, error) {
	out := dispatch.Issue{Number: issue.Number, Labels: append([]string(nil), issue.Labels...), Comments: make([]dispatch.Comment, 0, len(issue.Comments))}
	permissionCache := map[string]bool{}
	for _, comment := range issue.Comments {
		createdAt, _ := parseCoordinatorTime(comment.CreatedAt)
		hasWriteAccess := false
		if cfg.Mode == dispatch.ModeHumanGated {
			if _, ok := dispatch.ParseSlashCommand(comment.Body, cfg.SlashCommands); ok {
				allowed, err := r.commentHasWriteAccess(ctx, repo, cwd, comment.Author, permissionCache, cfg)
				if err != nil {
					return dispatch.Issue{}, err
				}
				hasWriteAccess = allowed
			}
		}
		out.Comments = append(out.Comments, dispatch.Comment{ID: comment.ID, Author: comment.Author, AuthorAssociation: comment.AuthorAssociation, HasWriteAccess: hasWriteAccess, Body: comment.Body, CreatedAt: createdAt})
	}
	for _, event := range issue.Timeline {
		if event.Event != "labeled" || event.Label != triagedLabel {
			continue
		}
		when, ok := parseCoordinatorTime(event.CreatedAt)
		if ok && (out.TriagedAt.IsZero() || when.After(out.TriagedAt)) {
			out.TriagedAt = when
		}
	}
	return out, nil
}

func (r *Runner) commentHasWriteAccess(ctx context.Context, repo, cwd, author string, cache map[string]bool, cfg dispatch.Config) (bool, error) {
	author = strings.TrimSpace(author)
	if author == "" {
		return false, nil
	}
	for _, allowed := range cfg.AllowedUsers {
		if strings.EqualFold(strings.TrimSpace(allowed), author) {
			return true, nil
		}
	}
	if allowed, ok := cache[strings.ToLower(author)]; ok {
		return allowed, nil
	}
	permission, err := r.github.GetRepositoryPermission(ctx, githubinfra.RepositoryPermissionInput{Repo: repo, User: author, CWD: cwd})
	if err != nil {
		return false, err
	}
	allowed := permission == "admin" || permission == "maintain" || permission == "write"
	cache[strings.ToLower(author)] = allowed
	return allowed, nil
}

func (r *Runner) projectConfig(ctx context.Context, projectID string) (*storage.ProjectRecord, config.CoordinatorRoleConfig, config.SweeperRoleConfig, error) {
	project, err := r.repos.Projects.GetByID(ctx, projectID)
	if err != nil {
		return nil, config.CoordinatorRoleConfig{}, config.SweeperRoleConfig{}, err
	}
	if project == nil {
		return nil, config.CoordinatorRoleConfig{}, config.SweeperRoleConfig{}, fmt.Errorf("project %q not found", projectID)
	}
	if r.config == nil {
		return nil, config.CoordinatorRoleConfig{}, config.SweeperRoleConfig{}, fmt.Errorf("coordinator config is not configured")
	}
	roles := config.ProjectRoleConfigs(*r.config, projectID)
	return project, roles.Coordinator, roles.Sweeper, nil
}

func (r *Runner) projectNetworkMode(projectID string) config.ProjectNetworkMode {
	if r == nil || r.config == nil {
		return config.ProjectNetworkModeOff
	}
	for _, project := range r.config.Projects {
		if project.ID != projectID {
			continue
		}
		if project.Network.Mode != "" {
			return project.Network.Mode
		}
		break
	}
	return config.ProjectNetworkModeOff
}

func (r *Runner) shouldRunTick(projectID string) bool {
	interval := r.pollInterval(projectID)
	if interval <= 0 {
		return true
	}
	now := r.now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	lastRun, ok := r.lastTickByProject[projectID]
	if ok && now.Sub(lastRun) < interval {
		return false
	}
	r.lastTickByProject[projectID] = now
	return true
}

func (r *Runner) pollInterval(projectID string) time.Duration {
	if r == nil || r.config == nil {
		return 0
	}
	roleCfg := config.ProjectRoleConfigs(*r.config, projectID).Coordinator
	interval, err := time.ParseDuration(strings.TrimSpace(roleCfg.PollInterval))
	if err != nil {
		return 0
	}
	return interval
}

func roleConfigToTriageConfig(roleCfg config.CoordinatorRoleConfig) triage.Config {
	return triage.Config{
		TriagedLabel:          roleCfg.Triage.TriagedLabel,
		MaxIssueAgeDays:       roleCfg.Triage.MaxIssueAgeDays,
		MaxPerTick:            roleCfg.Triage.MaxPerTick,
		OutOfScopeLabel:       roleCfg.Triage.Disposition.OutOfScopeLabel,
		UnclearLabel:          roleCfg.Triage.Disposition.UnclearLabel,
		ReTriageOnAuthorReply: roleCfg.Triage.Disposition.ReTriageOnAuthorReply,
	}
}

func mightNeedCoordinatorAction(issue githubinfra.IssueSummary, cfg triage.Config) bool {
	return !hasExactLabel(issue.Labels, cfg.TriagedLabel) || hasExactLabel(issue.Labels, cfg.UnclearLabel)
}

func matchingLabels(existing []string, patterns []string) []string {
	matched := []string{}
	for _, label := range existing {
		for _, pattern := range patterns {
			if labelMatchesPattern(label, pattern) {
				matched = append(matched, label)
				break
			}
		}
	}
	return matched
}

func removeExactLabels(labels []string, target string) []string {
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		if label != target {
			result = append(result, label)
		}
	}
	return result
}

func removeMatchingLabels(labels []string, patterns []string) []string {
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		if !matchesAnyLabelPattern(label, patterns) {
			result = append(result, label)
		}
	}
	return result
}

func splitDelayedLabelPatterns(patterns []string, delayedLabel string, delay bool) ([]string, []string) {
	if !delay {
		return append([]string(nil), patterns...), nil
	}
	now := make([]string, 0, len(patterns))
	after := []string{}
	for _, pattern := range patterns {
		if pattern == delayedLabel {
			after = append(after, pattern)
			continue
		}
		now = append(now, pattern)
	}
	return now, after
}

func removeAppliedLabelPatterns(patterns []string, applied []string) []string {
	if len(patterns) == 0 || len(applied) == 0 {
		return patterns
	}
	result := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if matchesAnyLabelPattern(pattern, applied) {
			continue
		}
		result = append(result, pattern)
	}
	return result
}

func removeExistingLabels(labels []string, existing []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if !hasExactLabel(existing, label) {
			out = append(out, label)
		}
	}
	return out
}

func nonExactTargetLabels(labels []string, keep string) []string {
	prefix := strings.ToLower(protocol.TargetLabelForNode(""))
	out := []string{}
	for _, label := range labels {
		if !strings.HasPrefix(strings.ToLower(label), prefix) || strings.EqualFold(label, keep) {
			continue
		}
		out = append(out, label)
	}
	return out
}

func matchesAnyLabelPattern(label string, patterns []string) bool {
	for _, pattern := range patterns {
		if labelMatchesPattern(label, pattern) {
			return true
		}
	}
	return false
}

func labelMatchesPattern(label string, pattern string) bool {
	if strings.HasSuffix(pattern, "/*") {
		return strings.HasPrefix(label, strings.TrimSuffix(pattern, "*"))
	}
	return label == pattern
}

func hasExactLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func findMarkerComment(comments []githubinfra.CommentInfo, currentLogin string) *githubinfra.CommentInfo {
	return findCoordinatorComment(comments, currentLogin, triageCommentMarker)
}

func findCoordinatorComment(comments []githubinfra.CommentInfo, currentLogin string, marker string) *githubinfra.CommentInfo {
	for index := range comments {
		if !strings.Contains(comments[index].Body, marker) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(comments[index].Author), strings.TrimSpace(currentLogin)) {
			return &comments[index]
		}
	}
	return nil
}

func findDispatchFailureComment(comments []githubinfra.CommentInfo, currentLogin string) *githubinfra.CommentInfo {
	return findCoordinatorComment(comments, currentLogin, dispatchFailureCommentMarker)
}

func (r *Runner) postCycleComment(ctx context.Context, repo, cwd string, issueNumber int64, body string) error {
	comments, err := r.github.ListIssueComments(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: issueNumber, CWD: cwd})
	if err != nil {
		return err
	}
	currentLogin, err := r.github.GetCurrentUserLoginForRepo(ctx, repo, cwd)
	if err != nil {
		return err
	}
	if findCoordinatorComment(comments, currentLogin, cycleCommentMarker) != nil {
		commentBody := cycleCommentMarker + "\n\n" + strings.TrimSpace(body)
		commentBody = disclosure.FromConfig(*r.config).Markdown(commentBody, "coordinator", disclosure.ChannelIssueComment)
		existing := findCoordinatorComment(comments, currentLogin, cycleCommentMarker)
		return r.github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: repo, CommentID: existing.ID, Body: commentBody, CWD: cwd})
	}
	commentBody := cycleCommentMarker + "\n\n" + strings.TrimSpace(body)
	commentBody = disclosure.FromConfig(*r.config).Markdown(commentBody, "coordinator", disclosure.ChannelIssueComment)
	_, err = r.github.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: repo, IssueNumber: issueNumber, Body: commentBody, CWD: cwd})
	return err
}

func knownCommentIDs(comments []triage.Comment) map[int64]struct{} {
	known := make(map[int64]struct{}, len(comments))
	for _, comment := range comments {
		if comment.ID == 0 {
			continue
		}
		known[comment.ID] = struct{}{}
	}
	return known
}

func hasNewHumanComment(comments []githubinfra.CommentInfo, known map[int64]struct{}, since time.Time) bool {
	since = since.UTC().Truncate(time.Second)
	for _, comment := range comments {
		if _, ok := known[comment.ID]; ok {
			continue
		}
		if strings.Contains(comment.Body, triageCommentMarker) || disclosure.HasMarkdownStamp(comment.Body) {
			continue
		}
		when, ok := parseCoordinatorTime(comment.CreatedAt)
		if ok && !when.Before(since) {
			return true
		}
	}
	return false
}

func timelineLabelName(event map[string]any) string {
	label, _ := event["label"].(map[string]any)
	return strings.TrimSpace(asString(label["name"]))
}

func parseCoordinatorTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, jsISOStringLayout} {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func asString(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprint(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type localRepositoryInspector struct{}

func (localRepositoryInspector) Inspect(_ context.Context, repoPath string, issue triage.Issue) (triage.RepoContext, error) {
	ctx := triage.RepoContext{WorkingDirectory: repoPath}
	tokens := triage.SearchTokens(issue)
	if repoPath == "" {
		return ctx, nil
	}
	_ = filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(ctx.Paths) >= 12 && len(ctx.Symbols) >= 12 {
			return filepath.SkipAll
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext != ".go" && ext != ".md" && ext != ".txt" && ext != ".json" && ext != ".yaml" && ext != ".yml" && ext != ".toml" {
			return nil
		}
		rel, relErr := filepath.Rel(repoPath, path)
		if relErr != nil {
			rel = path
		}
		lowerRel := strings.ToLower(rel)
		for _, token := range tokens {
			if strings.Contains(lowerRel, token) {
				if len(ctx.Paths) < 12 {
					ctx.Paths = append(ctx.Paths, rel)
				}
				break
			}
		}
		if len(ctx.Paths) >= 12 && len(ctx.Symbols) >= 12 {
			return filepath.SkipAll
		}
		if len(ctx.Symbols) >= 12 {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr == nil && info.Size() > 256*1024 {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, line := range strings.Split(string(content), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "func ") && !strings.HasPrefix(trimmed, "type ") && !strings.HasPrefix(trimmed, "const ") && !strings.HasPrefix(trimmed, "var ") {
				continue
			}
			lowerLine := strings.ToLower(trimmed)
			for _, token := range tokens {
				if strings.Contains(lowerLine, token) {
					ctx.Symbols = append(ctx.Symbols, rel+": "+trimmed)
					return nil
				}
			}
		}
		return nil
	})
	return ctx, nil
}
