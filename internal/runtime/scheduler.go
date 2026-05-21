package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/notify"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/network/protocol"
	"github.com/nexu-io/looper/internal/networkpolicy"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/sweeper"
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

type sweeperScheduler interface {
	DiscoverIssues(context.Context, sweeper.DiscoveryInput) (sweeper.DiscoveryResult, error)
	DiscoverPullRequests(context.Context, sweeper.DiscoveryInput) (sweeper.DiscoveryResult, error)
	DiscoverReconcile(context.Context, sweeper.DiscoveryInput) (sweeper.DiscoveryResult, error)
	ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*sweeper.ProcessResult, error)
}

type sweeperOperatorStatsProvider interface {
	RepoOperatorStats(context.Context, string, string, int) (sweeper.RepoStats, error)
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
	AsyncRunner              schedulerAsyncRunner
	RequestSchedulerWake     func()
	Planner                  plannerScheduler
	Coordinator              coordinatorScheduler
	Reviewer                 reviewerScheduler
	Fixer                    fixerScheduler
	Worker                   workerScheduler
	Sweeper                  sweeperScheduler
	Snapshotter              snapshotScheduler
	Config                   *config.Config
	PlannerDiscoveryEnabled  *bool
	CoordinatorEnabled       func(string) bool
	ReviewerDiscoveryEnabled *bool
	FixerDiscoveryEnabled    *bool
	WorkerDiscoveryEnabled   *bool
	SweeperDiscoveryEnabled  *bool
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
}

func (a plannerGitHubAdapter) ListOpenIssues(ctx context.Context, input planner.ListOpenIssuesInput) ([]planner.IssueSummary, error) {
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
	issue, err := a.gateway.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: input.Repo, IssueNumber: input.IssueNumber, CWD: input.CWD})
	if err != nil {
		return planner.IssueDetail{}, err
	}
	return planner.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, Assignees: issue.Assignees, Labels: issue.Labels}, nil
}

func (a plannerGitHubAdapter) GetCurrentUserLogin(ctx context.Context, cwd string) (string, error) {
	return a.gateway.GetCurrentUserLogin(ctx, cwd)
}

func (a plannerGitHubAdapter) AddIssueAssignees(ctx context.Context, input planner.IssueAssigneesInput) error {
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
	pr, err := a.gateway.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return planner.PullRequestDetail{}, err
	}
	return planner.PullRequestDetail{Number: pr.Number, Title: pr.Title, Body: pr.Body, URL: pr.URL, State: pr.State, HeadRefName: pr.HeadRefName, BaseRefName: pr.BaseRefName}, nil
}

func (a plannerGitHubAdapter) CreatePullRequest(ctx context.Context, input planner.CreatePullRequestInput) (planner.CreatePullRequestResult, error) {
	body := a.stamper.Markdown(input.Body, "planner", disclosure.ChannelPullRequest)
	pr, err := a.gateway.CreatePullRequest(ctx, githubinfra.CreatePullRequestInput{Repo: input.Repo, HeadBranch: input.HeadBranch, BaseBranch: input.BaseBranch, Title: input.Title, Body: body, CWD: input.CWD})
	if err != nil {
		return planner.CreatePullRequestResult{}, err
	}
	return planner.CreatePullRequestResult{Number: pr.Number, URL: pr.URL}, nil
}

func (a plannerGitHubAdapter) UpdatePullRequestBody(ctx context.Context, input planner.UpdatePullRequestBodyInput) error {
	body := a.stamper.Markdown(input.Body, "planner", disclosure.ChannelPullRequest)
	return a.gateway.UpdatePullRequestBody(ctx, githubinfra.UpdatePullRequestBodyInput{Repo: input.Repo, PRNumber: input.PRNumber, Body: body, CWD: input.CWD})
}

func (a plannerGitHubAdapter) AddPullRequestLabels(ctx context.Context, input planner.PullRequestLabelsInput) error {
	return a.gateway.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a plannerGitHubAdapter) AddPullRequestReviewers(ctx context.Context, input planner.PullRequestReviewersInput) error {
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
}

func (a reviewerGitHubAdapter) ListOpenPullRequests(ctx context.Context, input reviewer.ListOpenPullRequestsInput) ([]reviewer.PullRequestSummary, error) {
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

func (a reviewerGitHubAdapter) GetCurrentUserLogin(ctx context.Context, cwd string) (string, error) {
	return a.gateway.GetCurrentUserLogin(ctx, cwd)
}

func (a reviewerGitHubAdapter) ViewPullRequest(ctx context.Context, input reviewer.ViewPullRequestInput) (reviewer.PullRequestDetail, error) {
	detail, err := a.gateway.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
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
	return a.gateway.GetPullRequestHeadSHA(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) CapturePullRequestSnapshot(ctx context.Context, input reviewer.CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
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
	comment, err := a.gateway.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Body: body, CWD: input.CWD})
	if err != nil {
		return reviewer.IssueCommentResult{}, err
	}
	return reviewer.IssueCommentResult{ID: comment.ID, URL: comment.URL}, nil
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

type sweeperAgentExecutorAdapter struct{ executor *agent.ConfiguredExecutor }
type sweeperAgentExecutionAdapter struct{ execution agent.Execution }

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

func (a sweeperAgentExecutorAdapter) Start(ctx context.Context, input sweeper.AgentRunInput) (sweeper.AgentExecution, error) {
	execution, err := a.executor.Start(ctx, agent.RunInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Prompt: input.Prompt, WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, HeartbeatTimeout: input.HeartbeatTimeout, Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	return sweeperAgentExecutionAdapter{execution: execution}, nil
}

func (a sweeperAgentExecutionAdapter) Wait(ctx context.Context) (sweeper.AgentResult, error) {
	result, err := a.execution.Wait(ctx)
	if err != nil {
		return sweeper.AgentResult{}, err
	}
	return sweeper.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, ParseStatus: result.ParseStatus, TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds, ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt}, nil
}

func (a sweeperAgentExecutionAdapter) Kill(reason string) error {
	return a.execution.Kill(reason)
}

type fixerGitHubAdapter struct {
	gateway *githubinfra.Gateway
	stamper disclosure.Stamper
}

func (a fixerGitHubAdapter) ListOpenPullRequests(ctx context.Context, input fixer.ListOpenPullRequestsInput) ([]fixer.PullRequestSummary, error) {
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
	return a.gateway.GetCurrentUserLogin(ctx, cwd)
}

func (a fixerGitHubAdapter) GetPullRequestAuthor(ctx context.Context, input fixer.ViewPullRequestInput) (string, error) {
	return a.gateway.GetPullRequestAuthor(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
}

func (a fixerGitHubAdapter) ViewPullRequest(ctx context.Context, input fixer.ViewPullRequestInput) (fixer.PullRequestDetail, error) {
	detail, err := a.gateway.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return fixer.PullRequestDetail{}, err
	}
	return fixer.PullRequestDetail{Number: detail.Number, State: detail.State, IsDraft: detail.IsDraft, Labels: detail.Labels, HeadSHA: detail.HeadSHA, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, BaseSHA: detail.BaseSHA, ReviewDecision: detail.ReviewDecision, Comments: detail.Comments, IssueComments: commentInfosToObjects(detail.IssueComments), Checks: detail.Checks, HasConflicts: detail.HasConflicts, Author: detail.Author}, nil
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
	return a.gateway.ResolveReviewThread(ctx, githubinfra.ResolveReviewThreadInput{Repo: input.Repo, ThreadID: input.ThreadID, CWD: input.CWD})
}

func (a fixerGitHubAdapter) AddReviewThreadReply(ctx context.Context, input fixer.AddReviewThreadReplyInput) error {
	body := a.stamper.ReviewComment(input.Body, "fixer")
	return a.gateway.AddReviewThreadReply(ctx, githubinfra.AddReviewThreadReplyInput{Repo: input.Repo, ThreadID: input.ThreadID, Body: body, CWD: input.CWD})
}

func (a fixerGitHubAdapter) CompareCommits(ctx context.Context, input fixer.CompareCommitsInput) (fixer.CompareCommitsResult, error) {
	out, err := a.gateway.CompareCommits(ctx, githubinfra.CompareCommitsInput{Repo: input.Repo, Base: input.Base, Head: input.Head, CWD: input.CWD})
	if err != nil {
		return fixer.CompareCommitsResult{}, err
	}
	return fixer.CompareCommitsResult{Status: out.Status}, nil
}

func (a fixerGitHubAdapter) CreateIssueComment(ctx context.Context, input fixer.IssueCommentInput) (fixer.IssueCommentResult, error) {
	body := a.stamper.Markdown(input.Body, "fixer", disclosure.ChannelIssueComment)
	comment, err := a.gateway.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Body: body, CWD: input.CWD})
	if err != nil {
		return fixer.IssueCommentResult{}, err
	}
	return fixer.IssueCommentResult{ID: comment.ID, URL: comment.URL}, nil
}

func (a fixerGitHubAdapter) UpdateIssueComment(ctx context.Context, input fixer.UpdateIssueCommentInput) error {
	body := a.stamper.Markdown(input.Body, "fixer", disclosure.ChannelIssueComment)
	return a.gateway.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: input.Repo, CommentID: input.CommentID, Body: body, CWD: input.CWD})
}

func (a fixerGitHubAdapter) AddPullRequestLabels(ctx context.Context, input fixer.PullRequestLabelsInput) error {
	return a.gateway.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a fixerGitHubAdapter) RemovePullRequestLabels(ctx context.Context, input fixer.PullRequestLabelsInput) error {
	return a.gateway.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
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
}

func (a workerGitHubAdapter) ListOpenPullRequests(ctx context.Context, input worker.ListOpenPullRequestsInput) ([]worker.PullRequestSummary, error) {
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
	issues, err := a.gateway.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Assignee: input.Assignee, Label: input.Label, Labels: input.Labels})
	if err != nil {
		return nil, err
	}
	result := make([]worker.IssueSummary, 0, len(issues))
	for _, issue := range issues {
		result = append(result, worker.IssueSummary{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, Assignees: issue.Assignees, AssigneeUsers: networkPolicyUsers(issue.AssigneeUsers), Labels: issue.Labels})
	}
	return result, nil
}

func (a workerGitHubAdapter) GetCurrentUserLogin(ctx context.Context, cwd string) (string, error) {
	return a.gateway.GetCurrentUserLogin(ctx, cwd)
}

func (a workerGitHubAdapter) AddIssueAssignees(ctx context.Context, input worker.IssueAssigneesInput) error {
	return a.gateway.AddIssueAssignees(ctx, githubinfra.IssueAssigneesInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Assignees: input.Assignees, CWD: input.CWD})
}

func (a workerGitHubAdapter) ViewPullRequest(ctx context.Context, input worker.ViewPullRequestInput) (worker.PullRequestDetail, error) {
	detail, err := a.gateway.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return worker.PullRequestDetail{}, err
	}
	return worker.PullRequestDetail{Number: detail.Number, Title: detail.Title, Body: detail.Body, URL: detail.URL, State: detail.State, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, HeadSHA: detail.HeadSHA, ReviewRequests: detail.ReviewRequests, ReviewRequestUsers: networkPolicyUsers(detail.ReviewRequestUsers)}, nil
}

func (a workerGitHubAdapter) ViewIssue(ctx context.Context, input worker.ViewIssueInput) (worker.IssueDetail, error) {
	issue, err := a.gateway.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: input.Repo, IssueNumber: input.IssueNumber, CWD: input.CWD})
	if err != nil {
		return worker.IssueDetail{}, err
	}
	return worker.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, State: issue.State, IsPullRequest: issue.IsPullRequest, AssigneeUsers: networkPolicyUsers(issue.AssigneeUsers), Labels: issue.Labels}, nil
}

func (a workerGitHubAdapter) CreateIssueComment(ctx context.Context, input worker.IssueCommentInput) (worker.IssueCommentResult, error) {
	body := a.stamper.Markdown(input.Body, "worker", disclosure.ChannelIssueComment)
	comment, err := a.gateway.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Body: body, CWD: input.CWD})
	if err != nil {
		return worker.IssueCommentResult{}, err
	}
	return worker.IssueCommentResult{ID: comment.ID, URL: comment.URL}, nil
}

func (a workerGitHubAdapter) UpdateIssueComment(ctx context.Context, input worker.UpdateIssueCommentInput) error {
	body := a.stamper.Markdown(input.Body, "worker", disclosure.ChannelIssueComment)
	return a.gateway.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: input.Repo, CommentID: input.CommentID, Body: body, CWD: input.CWD})
}

func (a workerGitHubAdapter) CreatePullRequest(ctx context.Context, input worker.CreatePullRequestInput) (worker.CreatePullRequestResult, error) {
	body := a.stamper.Markdown(input.Body, "worker", disclosure.ChannelPullRequest)
	pr, err := a.gateway.CreatePullRequest(ctx, githubinfra.CreatePullRequestInput{Repo: input.Repo, HeadBranch: input.HeadBranch, BaseBranch: input.BaseBranch, Title: input.Title, Body: body, CWD: input.CWD})
	if err != nil {
		return worker.CreatePullRequestResult{}, err
	}
	return worker.CreatePullRequestResult{Number: pr.Number, URL: pr.URL}, nil
}

func (a workerGitHubAdapter) CompareBranches(ctx context.Context, input worker.CompareBranchesInput) (worker.CompareBranchesResult, error) {
	comparison, err := a.gateway.CompareBranches(ctx, githubinfra.CompareBranchesInput{Repo: input.Repo, BaseBranch: input.BaseBranch, HeadBranch: input.HeadBranch, CWD: input.CWD})
	if err != nil {
		return worker.CompareBranchesResult{}, err
	}
	return worker.CompareBranchesResult{AheadBy: comparison.AheadBy, BehindBy: comparison.BehindBy, Status: comparison.Status, TotalCommits: comparison.TotalCommits}, nil
}

func (a workerGitHubAdapter) UpdatePullRequestBody(ctx context.Context, input worker.UpdatePullRequestBodyInput) error {
	body := a.stamper.Markdown(input.Body, "worker", disclosure.ChannelPullRequest)
	return a.gateway.UpdatePullRequestBody(ctx, githubinfra.UpdatePullRequestBodyInput{Repo: input.Repo, PRNumber: input.PRNumber, Body: body, CWD: input.CWD})
}

func (a workerGitHubAdapter) UpdatePullRequestTitle(ctx context.Context, input worker.UpdatePullRequestTitleInput) error {
	return a.gateway.UpdatePullRequestTitle(ctx, githubinfra.UpdatePullRequestTitleInput{Repo: input.Repo, PRNumber: input.PRNumber, Title: input.Title, CWD: input.CWD})
}

func (a workerGitHubAdapter) RemovePullRequestLabels(ctx context.Context, input worker.PullRequestLabelsInput) error {
	return a.gateway.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a workerGitHubAdapter) AddPullRequestReviewers(ctx context.Context, input worker.PullRequestReviewersInput) error {
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
	execution, err := a.executor.Start(ctx, agent.RunInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Prompt: input.Prompt, WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, HeartbeatTimeout: input.HeartbeatTimeout, Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey})
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

func buildDefaultSchedulerHandlers(cfg config.Config, logger bootstrap.Logger, coordinator *storage.SQLiteCoordinator, repos *storage.Repositories, gitGateway *gitinfra.Gateway, githubGateway *githubinfra.Gateway, activeExecutions *ActiveExecutionRegistry, asyncRunner func() schedulerAsyncRunner, requestWake func(), now func() time.Time) defaultSchedulerHandlers {
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
		return nil
	}

	var plannerRunner plannerScheduler
	var coordinatorRunner coordinatorScheduler
	var reviewerRunner reviewerScheduler
	var fixerRunner fixerScheduler
	var workerRunner workerScheduler
	var sweeperRunner sweeperScheduler

	agentExecutor := agent.New(agent.ExecutorOptions{
		Config: agent.ExecutorConfig{
			Vendor:              *cfg.Agent.Vendor,
			Model:               cfg.Agent.Model,
			Params:              cfg.Agent.Params,
			Env:                 cfg.Agent.Env,
			NativeResumeEnabled: cfg.Agent.NativeResume.Enabled,
		},
		Repos:  repos,
		LogDir: cfg.Daemon.LogDir,
		Now:    now,
	})
	sweeperAgentModel := cfg.Roles.Sweeper.Proposer.Model
	if sweeperAgentModel == nil || strings.TrimSpace(*sweeperAgentModel) == "" {
		sweeperAgentModel = cfg.Agent.Model
	}
	sweeperAgentExecutor := agent.New(agent.ExecutorOptions{
		Config: agent.ExecutorConfig{
			Vendor:              *cfg.Agent.Vendor,
			Model:               sweeperAgentModel,
			Params:              cfg.Agent.Params,
			Env:                 cfg.Agent.Env,
			NativeResumeEnabled: cfg.Agent.NativeResume.Enabled,
		},
		Repos:  repos,
		LogDir: cfg.Daemon.LogDir,
		Now:    now,
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
		GitHub:             plannerGitHubAdapter{gateway: githubGateway, stamper: stamper},
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
		GitHub:           reviewerGitHubAdapter{gateway: githubGateway, stamper: stamper},
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
		OnQueueItemEnqueued:     requestWake,
		OnAgentExecutionStarted: func(ctx context.Context, input reviewer.AgentExecutionStartedInput) error {
			return notifyAgentExecutionStarted(ctx, agentExecutionNotificationInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Title: "Looper Reviewer", Subtitle: input.Subtitle, Body: input.Body, DedupeKey: input.DedupeKey})
		},
	})
	fixerRunner = fixer.New(fixer.Options{
		DB:                 coordinator.DB(),
		Repos:              repos,
		GitHub:             fixerGitHubAdapter{gateway: githubGateway, stamper: stamper},
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
		GitHub: workerGitHubAdapter{gateway: githubGateway, stamper: stamper},
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
	})
	sweeperRunner = sweeper.New(sweeper.Options{Repos: repos, GitHub: githubGateway, Agent: sweeperAgentExecutorAdapter{executor: sweeperAgentExecutor}, Logger: logger, Now: now, Config: &cfg, AgentRuntime: agentRuntime, AgentModel: sweeperAgentModel, OnQueueItemEnqueued: requestWake})
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
			AsyncRunner:              runner,
			RequestSchedulerWake:     requestWake,
			Planner:                  plannerRunner,
			Coordinator:              coordinatorRunner,
			Reviewer:                 reviewerRunner,
			Fixer:                    fixerRunner,
			Worker:                   workerRunner,
			Sweeper:                  sweeperRunner,
			Snapshotter:              githubGateway,
			Config:                   &cfg,
			PlannerDiscoveryEnabled:  boolPtr(config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "planner")),
			CoordinatorEnabled:       func(projectID string) bool { return config.ProjectRoleConfigs(cfg, projectID).Coordinator.Enabled },
			ReviewerDiscoveryEnabled: boolPtr(config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "reviewer")),
			FixerDiscoveryEnabled:    boolPtr(config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "fixer")),
			WorkerDiscoveryEnabled:   boolPtr(config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "worker")),
			SweeperDiscoveryEnabled:  boolPtr(config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "sweeper")),
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
		repo := repoFromProjectMetadata(project.MetadataJSON)
		snapshot := projectSnapshot(project.ID)
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
			appendErr(runSchedulerLane(input, "coordinator discovery", project.ID, repo, func() error {
				_, err := input.Coordinator.DiscoverIssues(ctx, coordinatorrole.DiscoveryInput{ProjectID: project.ID, Repo: repo, Snapshot: snapshot})
				return wrapSchedulerError("coordinator discovery", project.ID, repo, err)
			}))
			claimedCount, availableSlots, err = executeClaimPhase(ctx, "post_coordinator_discovery", input, discoveredRunnableIDs, true)
			recordClaim(claimedCount, availableSlots, err)
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
			appendErr(runSchedulerLane(input, "fixer discovery", project.ID, repo, func() error {
				result, err := input.Fixer.DiscoverPullRequests(ctx, fixer.DiscoveryInput{ProjectID: project.ID, Repo: repo, Snapshot: snapshot})
				trackRunnableDiscovery(result.QueueItems)
				return wrapSchedulerError("fixer discovery", project.ID, repo, err)
			}))
			claimedCount, availableSlots, err = executeClaimPhase(ctx, "post_fixer_discovery", input, discoveredRunnableIDs, true)
			recordClaim(claimedCount, availableSlots, err)
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
	}

	claimedCount, availableSlots, err = executeClaimPhase(ctx, "post_discovery", input, discoveredRunnableIDs, true)
	recordClaim(claimedCount, availableSlots, err)

	for _, project := range projectsList {
		if err := ctx.Err(); err != nil {
			retErr = errors.Join(append(errs, err)...)
			return retErr
		}
		if project.Archived {
			continue
		}
		repo := repoFromProjectMetadata(project.MetadataJSON)
		snapshot := projectSnapshot(project.ID)
		if repo == "" {
			continue
		}
		if input.Sweeper != nil && discoveryEnabled(input.SweeperDiscoveryEnabled) {
			appendErr(applySweeperBackpressure(ctx, input, project, repo))
			appendErr(runSchedulerLane(input, "sweeper issue discovery", project.ID, repo, func() error {
				_, err := input.Sweeper.DiscoverIssues(ctx, sweeper.DiscoveryInput{ProjectID: project.ID, Repo: repo, Snapshot: snapshot})
				return wrapSchedulerError("sweeper issue discovery", project.ID, repo, err)
			}))
			appendErr(runSchedulerLane(input, "sweeper pull request discovery", project.ID, repo, func() error {
				_, err := input.Sweeper.DiscoverPullRequests(ctx, sweeper.DiscoveryInput{ProjectID: project.ID, Repo: repo, Snapshot: snapshot})
				return wrapSchedulerError("sweeper pull request discovery", project.ID, repo, err)
			}))
			appendErr(runSchedulerLane(input, "sweeper reconciliation discovery", project.ID, repo, func() error {
				_, err := input.Sweeper.DiscoverReconcile(ctx, sweeper.DiscoveryInput{ProjectID: project.ID, Repo: repo})
				return wrapSchedulerError("sweeper reconciliation discovery", project.ID, repo, err)
			}))
			claimedCount, availableSlots, err = executeClaimPhase(ctx, "post_sweeper_discovery", input, discoveredRunnableIDs, true)
			recordClaim(claimedCount, availableSlots, err)
		} else if input.Sweeper != nil && input.Logger != nil {
			input.Logger.Debug("sweeper auto-discovery disabled", map[string]any{"projectId": project.ID, "repo": repo})
		}
	}

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
	if input.Config != nil && input.Sweeper != nil && discoveryEnabled(input.SweeperDiscoveryEnabled) {
		roles := config.ProjectRoleConfigs(*input.Config, projectID)
		sweeperLimit := maxInt(roles.Sweeper.Triggers.MaxPerTick*6, 30)
		prLimit = maxInt(prLimit, sweeperLimit)
		issueLimit = maxInt(issueLimit, sweeperLimit)
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
		item, err := input.Repos.Queue.ClaimNext(ctx, nowISO, "scheduler")
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

func runScheduledQueueItems(ctx context.Context, queueItems []storage.QueueItemRecord, input defaultSchedulerTickInput) error {
	if len(queueItems) == 0 {
		return nil
	}

	errList := make([]error, 0)
	for _, item := range queueItems {
		if err := ctx.Err(); err != nil {
			return err
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
	case "sweeper", "sweeper:warn", "sweeper:close", "sweeper:reconcile":
		if input.Sweeper == nil {
			return nil, fmt.Errorf("sweeper runner is not configured")
		}
		return func(ctx context.Context) error {
			_, err := input.Sweeper.ProcessClaimedQueueItem(ctx, item)
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
	if kind == "retryable_transient" && nextAttempts < item.MaxAttempts {
		return input.Repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: item.ID, AvailableAt: nowISO, Attempts: nextAttempts, ErrorMessage: &message, ErrorKind: kind, UpdatedAt: nowISO})
	}
	return input.Repos.Queue.Fail(ctx, storage.QueueFailInput{ID: item.ID, Attempts: nextAttempts, FinishedAt: nowISO, ErrorMessage: &message, ErrorKind: kind, UpdatedAt: nowISO})
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

func applySweeperBackpressure(ctx context.Context, input defaultSchedulerTickInput, project storage.ProjectRecord, repo string) error {
	if input.Config == nil || input.Sweeper == nil || input.Repos == nil || input.Repos.Projects == nil {
		return nil
	}
	statsProvider, ok := input.Sweeper.(sweeperOperatorStatsProvider)
	if !ok {
		return nil
	}
	roleCfg := config.ProjectRoleConfigs(*input.Config, project.ID).Sweeper
	threshold := roleCfg.Proposer.TimeoutRateDryRunThreshold
	minSamples := roleCfg.Proposer.TimeoutRateDryRunMinSamples
	if threshold <= 0 {
		return nil
	}
	meta := parseSchedulerProjectMetadata(project.MetadataJSON)
	if meta.Sweeper.AutoDryRun {
		return nil
	}
	stats, err := statsProvider.RepoOperatorStats(ctx, project.ID, repo, 1000)
	if err != nil {
		return fmt.Errorf("compute sweeper backpressure stats: %w", err)
	}
	if stats.AgentProposalCount < minSamples || stats.AgentTimeoutRate < threshold {
		return nil
	}
	nowFn := input.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	nowISO := formatJavaScriptISOString(nowFn().UTC())
	reason := fmt.Sprintf("agent timeout rate %.2f exceeded threshold %.2f across %d agent proposals", stats.AgentTimeoutRate, threshold, stats.AgentProposalCount)
	updatedMetadata, err := mergeSchedulerProjectMetadata(project.MetadataJSON, map[string]any{"sweeper": map[string]any{"autoDryRun": true, "autoDryRunReason": reason, "autoDryRunSetAt": nowISO}})
	if err != nil {
		return fmt.Errorf("update sweeper backpressure metadata: %w", err)
	}
	updated := project
	updated.MetadataJSON = &updatedMetadata
	updated.UpdatedAt = nowISO
	if err := input.Repos.Projects.Upsert(ctx, updated); err != nil {
		return fmt.Errorf("persist sweeper backpressure override: %w", err)
	}
	if input.Repos.Notifications != nil {
		dedupeKey := fmt.Sprintf("runtime.sweeper.auto_dry_run:%s:%s", project.ID, repo)
		payloadJSON := fmt.Sprintf(`{"repo":%q,"reason":%q}`, repo, reason)
		_ = input.Repos.Notifications.Upsert(ctx, storage.NotificationRecord{ID: dedupeKey, ProjectID: stringPtr(project.ID), EntityType: stringPtr("project"), EntityID: stringPtr(project.ID), Channel: "in_app", Level: "warning", Title: "Looper Sweeper Auto Dry-Run", Subtitle: stringPtr(repo), Body: reason, Status: "sent", DedupeKey: &dedupeKey, PayloadJSON: &payloadJSON, SentAt: stringPtr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO})
	}
	if input.Logger != nil {
		input.Logger.Warn("sweeper auto-enabled dry-run backpressure", map[string]any{"projectId": project.ID, "repo": repo, "reason": reason})
	}
	return nil
}

type schedulerProjectMetadata struct {
	Sweeper struct {
		AutoDryRun bool `json:"autoDryRun"`
	} `json:"sweeper"`
}

func parseSchedulerProjectMetadata(metadataJSON *string) schedulerProjectMetadata {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return schedulerProjectMetadata{}
	}
	var metadata schedulerProjectMetadata
	if err := json.Unmarshal([]byte(strings.TrimSpace(*metadataJSON)), &metadata); err != nil {
		return schedulerProjectMetadata{}
	}
	return metadata
}

func mergeSchedulerProjectMetadata(current *string, updates map[string]any) (string, error) {
	metadata := map[string]any{}
	if current != nil && strings.TrimSpace(*current) != "" {
		if err := json.Unmarshal([]byte(strings.TrimSpace(*current)), &metadata); err != nil {
			return "", err
		}
	}
	for key, value := range updates {
		metadata[key] = value
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
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
