package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/powerformer/looper/internal/agent"
	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/disclosure"
	"github.com/powerformer/looper/internal/fixer"
	gitinfra "github.com/powerformer/looper/internal/infra/git"
	githubinfra "github.com/powerformer/looper/internal/infra/github"
	"github.com/powerformer/looper/internal/infra/notify"
	"github.com/powerformer/looper/internal/planner"
	"github.com/powerformer/looper/internal/reviewer"
	"github.com/powerformer/looper/internal/storage"
	"github.com/powerformer/looper/internal/worker"
)

type plannerScheduler interface {
	DiscoverIssues(context.Context, planner.DiscoveryInput) (planner.DiscoveryResult, error)
	ProcessNext(context.Context, string) (*planner.ProcessResult, error)
	ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*planner.ProcessResult, error)
}

type reviewerScheduler interface {
	DiscoverPullRequests(context.Context, reviewer.DiscoveryInput) (reviewer.DiscoveryResult, error)
	ProcessNext(context.Context, string) (*reviewer.ProcessResult, error)
	ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*reviewer.ProcessResult, error)
}

type fixerScheduler interface {
	DiscoverPullRequests(context.Context, fixer.DiscoveryInput) (fixer.DiscoveryResult, error)
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
	Logger                   bootstrap.Logger
	Now                      func() time.Time
	MaxConcurrentRuns        int
	AsyncRunner              schedulerAsyncRunner
	Planner                  plannerScheduler
	Reviewer                 reviewerScheduler
	Fixer                    fixerScheduler
	Worker                   workerScheduler
	Snapshotter              snapshotScheduler
	PlannerDiscoveryEnabled  *bool
	ReviewerDiscoveryEnabled *bool
	FixerDiscoveryEnabled    *bool
	WorkerDiscoveryEnabled   *bool
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
	return planner.PullRequestDetail{Number: pr.Number, URL: pr.URL, State: pr.State, HeadRefName: pr.HeadRefName, BaseRefName: pr.BaseRefName}, nil
}

func (a plannerGitHubAdapter) CreatePullRequest(ctx context.Context, input planner.CreatePullRequestInput) (planner.CreatePullRequestResult, error) {
	body := a.stamper.Markdown(input.Body, "planner", disclosure.ChannelPullRequest)
	pr, err := a.gateway.CreatePullRequest(ctx, githubinfra.CreatePullRequestInput{Repo: input.Repo, HeadBranch: input.HeadBranch, BaseBranch: input.BaseBranch, Title: input.Title, Body: body, CWD: input.CWD})
	if err != nil {
		return planner.CreatePullRequestResult{}, err
	}
	return planner.CreatePullRequestResult{Number: pr.Number, URL: pr.URL}, nil
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
	result, err := a.gateway.InspectHead(ctx, gitinfra.InspectHeadInput{WorktreePath: input.WorktreePath, BaseRef: input.BaseRef})
	if err != nil {
		return planner.InspectHeadResult{}, err
	}
	return planner.InspectHeadResult{HeadSHA: result.HeadSHA, NewCommitSHAs: result.NewCommitSHAs, HasUncommittedChanges: result.HasUncommittedChanges, ChangedFiles: result.ChangedFiles}, nil
}

func (a plannerGitAdapter) Commit(ctx context.Context, input planner.CommitInput) (planner.CommitResult, error) {
	message := a.stamper.CommitMessage(input.Message, "planner")
	result, err := a.gateway.Commit(ctx, gitinfra.CommitInput{WorktreePath: input.WorktreePath, Message: message})
	if err != nil {
		return planner.CommitResult{}, err
	}
	return planner.CommitResult{CommitSHA: result.CommitSHA}, nil
}

func (a plannerGitAdapter) Push(ctx context.Context, input planner.PushInput) error {
	return a.gateway.Push(ctx, gitinfra.PushInput{WorktreePath: input.WorktreePath, Branch: input.Branch, Remote: input.Remote, ProtectedBranches: input.ProtectedBranches})
}

type plannerAgentExecutorAdapter struct{ executor *agent.ConfiguredExecutor }
type plannerAgentExecutionAdapter struct{ execution agent.Execution }

func (a plannerAgentExecutorAdapter) Start(ctx context.Context, input planner.AgentRunInput) (planner.AgentExecution, error) {
	execution, err := a.executor.Start(ctx, agent.RunInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Prompt: input.Prompt, WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey})
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
	return planner.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, Commits: result.Commits, Lifecycle: result.Lifecycle}, nil
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
		result = append(result, reviewer.PullRequestSummary{Number: pr.Number, Title: pr.Title, State: pr.State, IsDraft: pr.IsDraft, ReviewDecision: pr.ReviewDecision, Labels: pr.Labels, HeadSHA: pr.HeadSHA, BaseSHA: pr.BaseSHA, HasConflicts: pr.HasConflicts, Author: pr.Author, ReviewRequests: pr.ReviewRequests, Reviews: pr.Reviews})
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
	return reviewer.PullRequestDetail{Number: detail.Number, Title: detail.Title, Body: detail.Body, State: detail.State, IsDraft: detail.IsDraft, ReviewDecision: detail.ReviewDecision, Labels: detail.Labels, HeadSHA: detail.HeadSHA, BaseSHA: detail.BaseSHA, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, Author: detail.Author, ReviewRequests: detail.ReviewRequests, HasConflicts: detail.HasConflicts, ChecksSummary: summarizeCheckStates(detail.Checks), Comments: detail.Comments, Reviews: detail.Reviews}, nil
}

func (a reviewerGitHubAdapter) CapturePullRequestSnapshot(ctx context.Context, input reviewer.CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	return a.gateway.CapturePullRequestSnapshot(ctx, githubinfra.CapturePullRequestSnapshotInput{ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, CapturedAt: input.CapturedAt})
}

func (a reviewerGitHubAdapter) FindReviewMarker(ctx context.Context, input reviewer.VerifyReviewMarkerInput) (reviewer.ReviewMarkerResult, error) {
	allowedReviewEvents := make([]string, 0, len(input.AllowedReviewEvents))
	for _, event := range input.AllowedReviewEvents {
		allowedReviewEvents = append(allowedReviewEvents, string(event))
	}
	marker, err := a.gateway.FindReviewMarker(ctx, githubinfra.VerifyReviewMarkerInput{Repo: input.Repo, PRNumber: input.PRNumber, Marker: input.Marker, AllowedReviewEvents: allowedReviewEvents, AuthorLogin: input.AuthorLogin, CWD: input.CWD})
	if err != nil {
		return reviewer.ReviewMarkerResult{}, err
	}
	return reviewer.ReviewMarkerResult{Found: marker.Found, Outcome: marker.Outcome, Event: reviewer.ReviewEvent(marker.Event), AuthorLogin: marker.AuthorLogin, Body: marker.Body, InlineCommentBodies: append([]string(nil), marker.InlineCommentBodies...)}, nil
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
	result, err := a.gateway.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{WorktreePath: input.WorktreePath, Branch: input.Branch, Ref: input.Ref, ExpectedHeadSHA: input.ExpectedHeadSHA, Remote: input.Remote})
	if err != nil {
		return reviewer.PrepareWorktreeResult{}, err
	}
	return reviewer.PrepareWorktreeResult{HeadSHA: result.HeadSHA, Clean: result.Clean}, nil
}

func (a reviewerGitAdapter) CleanupWorktree(ctx context.Context, input reviewer.CleanupWorktreeInput) error {
	return a.gateway.CleanupWorktree(ctx, gitinfra.CleanupWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreePath: input.WorktreePath, Branch: input.Branch, ProtectedBranches: input.ProtectedBranches})
}

func (a reviewerAgentExecutorAdapter) Start(ctx context.Context, input reviewer.AgentRunInput) (reviewer.AgentExecution, error) {
	execution, err := a.executor.Start(ctx, agent.RunInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Prompt: input.Prompt, WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey})
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
	return reviewer.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, ParseStatus: result.ParseStatus}, nil
}

type fixerGitHubAdapter struct{ gateway *githubinfra.Gateway }

func (a fixerGitHubAdapter) ListOpenPullRequests(ctx context.Context, input fixer.ListOpenPullRequestsInput) ([]fixer.PullRequestSummary, error) {
	pullRequests, err := a.gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Author: input.Author, Label: input.Label, Labels: input.Labels})
	if err != nil {
		return nil, err
	}
	result := make([]fixer.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, fixer.PullRequestSummary{Number: pr.Number, State: pr.State, IsDraft: pr.IsDraft, Labels: pr.Labels, HeadSHA: pr.HeadSHA, Author: pr.Author})
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
	return fixer.PullRequestDetail{Number: detail.Number, State: detail.State, IsDraft: detail.IsDraft, Labels: detail.Labels, HeadSHA: detail.HeadSHA, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, BaseSHA: detail.BaseSHA, ReviewDecision: detail.ReviewDecision, Comments: detail.Comments, Checks: detail.Checks, HasConflicts: detail.HasConflicts, Author: detail.Author}, nil
}

func (a fixerGitHubAdapter) ResolveReviewThread(ctx context.Context, input fixer.ResolveReviewThreadInput) error {
	return a.gateway.ResolveReviewThread(ctx, githubinfra.ResolveReviewThreadInput{Repo: input.Repo, ThreadID: input.ThreadID, CWD: input.CWD})
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
	result, err := a.gateway.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{WorktreePath: input.WorktreePath, Branch: input.Branch, ExpectedHeadSHA: input.ExpectedHeadSHA, Remote: input.Remote})
	if err != nil {
		return fixer.PrepareWorktreeResult{}, err
	}
	return fixer.PrepareWorktreeResult{HeadSHA: result.HeadSHA, Clean: result.Clean}, nil
}

func (a fixerGitAdapter) InspectHead(ctx context.Context, input fixer.InspectHeadInput) (fixer.InspectHeadResult, error) {
	result, err := a.gateway.InspectHead(ctx, gitinfra.InspectHeadInput{WorktreePath: input.WorktreePath, BaseRef: input.BaseRef})
	if err != nil {
		return fixer.InspectHeadResult{}, err
	}
	return fixer.InspectHeadResult{HeadSHA: result.HeadSHA, NewCommitSHAs: result.NewCommitSHAs, HasUncommittedChanges: result.HasUncommittedChanges, ChangedFiles: result.ChangedFiles}, nil
}

func (a fixerGitAdapter) Commit(ctx context.Context, input fixer.CommitInput) (fixer.CommitResult, error) {
	message := a.stamper.CommitMessage(input.Message, "fixer")
	result, err := a.gateway.Commit(ctx, gitinfra.CommitInput{WorktreePath: input.WorktreePath, Message: message})
	if err != nil {
		return fixer.CommitResult{}, err
	}
	return fixer.CommitResult{CommitSHA: result.CommitSHA}, nil
}

func (a fixerGitAdapter) Push(ctx context.Context, input fixer.PushInput) error {
	return a.gateway.Push(ctx, gitinfra.PushInput{WorktreePath: input.WorktreePath, Branch: input.Branch, Remote: input.Remote, ExpectedRemoteHeadSHA: input.ExpectedRemoteHeadSHA, ProtectedBranches: input.ProtectedBranches})
}

func (a fixerGitAdapter) CleanupWorktree(ctx context.Context, input fixer.CleanupWorktreeInput) error {
	return a.gateway.CleanupWorktree(ctx, gitinfra.CleanupWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreePath: input.WorktreePath, Branch: input.Branch, ProtectedBranches: input.ProtectedBranches})
}

type fixerAgentExecutorAdapter struct{ executor *agent.ConfiguredExecutor }
type fixerAgentExecutionAdapter struct{ execution agent.Execution }

func (a fixerAgentExecutorAdapter) Start(ctx context.Context, input fixer.AgentRunInput) (fixer.AgentExecution, error) {
	execution, err := a.executor.Start(ctx, agent.RunInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Prompt: input.Prompt, WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey})
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
	return fixer.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, ParseStatus: result.ParseStatus, Lifecycle: result.Lifecycle}, nil
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
		result = append(result, worker.IssueSummary{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, Assignees: issue.Assignees, Labels: issue.Labels})
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
	return worker.PullRequestDetail{Number: detail.Number, Title: detail.Title, Body: detail.Body, URL: detail.URL, State: detail.State, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, HeadSHA: detail.HeadSHA, ReviewRequests: detail.ReviewRequests}, nil
}

func (a workerGitHubAdapter) ViewIssue(ctx context.Context, input worker.ViewIssueInput) (worker.IssueDetail, error) {
	issue, err := a.gateway.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: input.Repo, IssueNumber: input.IssueNumber, CWD: input.CWD})
	if err != nil {
		return worker.IssueDetail{}, err
	}
	return worker.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, State: issue.State, IsPullRequest: issue.IsPullRequest}, nil
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

func (a workerGitAdapter) PrepareWorktree(ctx context.Context, input worker.PrepareWorktreeInput) (worker.PrepareWorktreeResult, error) {
	result, err := a.gateway.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{WorktreePath: input.WorktreePath, Branch: input.Branch, ExpectedHeadSHA: input.ExpectedHeadSHA, Remote: input.Remote})
	if err != nil {
		return worker.PrepareWorktreeResult{}, err
	}
	return worker.PrepareWorktreeResult{HeadSHA: result.HeadSHA, Clean: result.Clean}, nil
}

func (a workerGitAdapter) InspectHead(ctx context.Context, input worker.InspectHeadInput) (worker.InspectHeadResult, error) {
	result, err := a.gateway.InspectHead(ctx, gitinfra.InspectHeadInput{WorktreePath: input.WorktreePath, BaseRef: input.BaseRef})
	if err != nil {
		return worker.InspectHeadResult{}, err
	}
	return worker.InspectHeadResult{HeadSHA: result.HeadSHA, NewCommitSHAs: result.NewCommitSHAs, HasUncommittedChanges: result.HasUncommittedChanges, ChangedFiles: result.ChangedFiles}, nil
}

func (a workerGitAdapter) Commit(ctx context.Context, input worker.CommitInput) (worker.CommitResult, error) {
	message := a.stamper.CommitMessage(input.Message, "worker")
	result, err := a.gateway.Commit(ctx, gitinfra.CommitInput{WorktreePath: input.WorktreePath, Message: message})
	if err != nil {
		return worker.CommitResult{}, err
	}
	return worker.CommitResult{CommitSHA: result.CommitSHA}, nil
}

func (a workerGitAdapter) Push(ctx context.Context, input worker.PushInput) error {
	return a.gateway.Push(ctx, gitinfra.PushInput{WorktreePath: input.WorktreePath, Branch: input.Branch, Remote: input.Remote, ProtectedBranches: input.ProtectedBranches})
}

type workerAgentExecutorAdapter struct {
	executor *agent.ConfiguredExecutor
	registry *ActiveExecutionRegistry
}
type workerAgentExecutionAdapter struct {
	execution agent.Execution
}

func (a workerAgentExecutorAdapter) Start(ctx context.Context, input worker.AgentRunInput) (worker.AgentExecution, error) {
	execution, err := a.executor.Start(ctx, agent.RunInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Prompt: input.Prompt, WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey})
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
	return worker.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, ParseStatus: result.ParseStatus, ChangedFiles: result.ChangedFiles, Commits: result.Commits, Lifecycle: result.Lifecycle}, nil
}

func (a workerAgentExecutionAdapter) Kill(reason string) error {
	return a.execution.Kill(reason)
}

func buildDefaultSchedulerTick(cfg config.Config, logger bootstrap.Logger, coordinator *storage.SQLiteCoordinator, repos *storage.Repositories, gitGateway *gitinfra.Gateway, githubGateway *githubinfra.Gateway, activeExecutions *ActiveExecutionRegistry, asyncRunner func() schedulerAsyncRunner, now func() time.Time) RunSchedulerTickFunc {
	if now == nil {
		now = time.Now
	}
	if repos == nil || coordinator == nil {
		return func(context.Context, Services) error {
			return fmt.Errorf("default scheduler dependencies are not configured")
		}
	}
	if cfg.Agent.Vendor == nil {
		return func(context.Context, Services) error { return nil }
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
	var reviewerRunner reviewerScheduler
	var fixerRunner fixerScheduler
	var workerRunner workerScheduler

	agentExecutor := agent.New(agent.ExecutorOptions{
		Config: agent.ExecutorConfig{
			Vendor: *cfg.Agent.Vendor,
			Model:  cfg.Agent.Model,
			Params: cfg.Agent.Params,
			Env:    cfg.Agent.Env,
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
		DiscoveryPolicy: planner.DiscoveryPolicy{
			AutoDiscovery:              cfg.Roles.Planner.AutoDiscovery,
			Labels:                     append([]string(nil), cfg.Roles.Planner.Triggers.Labels...),
			LabelMode:                  cfg.Roles.Planner.Triggers.LabelMode,
			RequireAssigneeCurrentUser: cfg.Roles.Planner.Triggers.RequireAssigneeCurrentUser,
		},
		RetryBaseDelay:   retryBaseDelay,
		RetryMaxAttempts: int64(cfg.Scheduler.RetryMaxAttempts),
		OnAgentExecutionStarted: func(ctx context.Context, input planner.AgentExecutionStartedInput) error {
			return notifyAgentExecutionStarted(ctx, agentExecutionNotificationInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Title: "Looper Planner", Subtitle: input.Subtitle, Body: input.Body, DedupeKey: input.DedupeKey})
		},
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
		ReviewEvents:     cfg.Reviewer.ReviewEvents,
		LoopConfig:       cfg.Reviewer.Loop,
		DiscoveryPolicy: reviewer.DiscoveryPolicy{
			AutoDiscovery:             cfg.Roles.Reviewer.AutoDiscovery,
			IncludeDrafts:             cfg.Roles.Reviewer.Triggers.IncludeDrafts,
			RequireReviewRequest:      cfg.Roles.Reviewer.Triggers.RequireReviewRequest,
			Labels:                    append([]string(nil), cfg.Roles.Reviewer.Triggers.Labels...),
			LabelMode:                 cfg.Roles.Reviewer.Triggers.LabelMode,
			IncludeSpecReviewingLabel: cfg.Roles.Reviewer.SpecReview.IncludeReviewingLabel,
			SpecReviewingLabel:        cfg.Roles.Reviewer.SpecReview.ReviewingLabel,
		},
		Scope:                   cfg.Reviewer.Scope,
		DetectDuplicateFindings: cfg.Reviewer.DetectDuplicateFindings,
		ThreadResolution:        cfg.Reviewer.ThreadResolution,
		Disclosure:              &cfg.Disclosure,
		AgentRuntime:            agentRuntime,
		CustomInstructions:      &cfg,
		LooperCLIPath:           derefString(cfg.Tools.LooperPath),
		AgentModel:              cfg.Agent.Model,
		RetryBaseDelay:          retryBaseDelay,
		RetryMaxAttempts:        int64(cfg.Scheduler.RetryMaxAttempts),
		OnAgentExecutionStarted: func(ctx context.Context, input reviewer.AgentExecutionStartedInput) error {
			return notifyAgentExecutionStarted(ctx, agentExecutionNotificationInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Title: "Looper Reviewer", Subtitle: input.Subtitle, Body: input.Body, DedupeKey: input.DedupeKey})
		},
	})
	fixerRunner = fixer.New(fixer.Options{
		DB:                 coordinator.DB(),
		Repos:              repos,
		GitHub:             fixerGitHubAdapter{gateway: githubGateway},
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
		Disclosure:         &cfg.Disclosure,
		AgentRuntime:       agentRuntime,
		CustomInstructions: &cfg,
		AgentModel:         cfg.Agent.Model,
		RetryBaseDelay:     retryBaseDelay,
		RetryMaxAttempts:   int64(cfg.Scheduler.RetryMaxAttempts),
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
		Disclosure:         &cfg.Disclosure,
		AgentRuntime:       agentRuntime,
		CustomInstructions: &cfg,
		AgentModel:         cfg.Agent.Model,
		RetryBaseDelay:     retryBaseDelay,
		RetryMaxAttempts:   int64(cfg.Scheduler.RetryMaxAttempts),
		OnRunCompleted: func(ctx context.Context, input worker.RunCompletedInput) error {
			return notifyWorkerRunCompleted(ctx, workerRunCompletedNotificationInput{ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Subtitle: input.Subtitle, Status: input.Status, Summary: input.Summary, FailureKind: input.FailureKind, PullRequestNumber: input.PullRequestNumber, PullRequestURL: input.PullRequestURL})
		},
	})

	return func(ctx context.Context, services Services) error {
		var runner schedulerAsyncRunner
		if asyncRunner != nil {
			runner = asyncRunner()
		}
		return runDefaultSchedulerTick(ctx, defaultSchedulerTickInput{
			Repos:                    services.Repositories,
			Logger:                   logger,
			Now:                      now,
			MaxConcurrentRuns:        cfg.Scheduler.MaxConcurrentRuns,
			AsyncRunner:              runner,
			Planner:                  plannerRunner,
			Reviewer:                 reviewerRunner,
			Fixer:                    fixerRunner,
			Worker:                   workerRunner,
			Snapshotter:              githubGateway,
			PlannerDiscoveryEnabled:  boolPtr(config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "planner")),
			ReviewerDiscoveryEnabled: boolPtr(config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "reviewer")),
			FixerDiscoveryEnabled:    boolPtr(config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "fixer")),
			WorkerDiscoveryEnabled:   boolPtr(config.AnyProjectRoleAutoDiscoveryEnabled(cfg, "worker")),
		})
	}
}

func githubCLIAutoPROpeningAvailable(ctx context.Context, cfg config.Config, githubGateway *githubinfra.Gateway, logger bootstrap.Logger, repo, cwd string) bool {
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

func runDefaultSchedulerTick(ctx context.Context, input defaultSchedulerTickInput) error {
	if input.Repos == nil || input.Repos.Projects == nil {
		return nil
	}

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

	availableSlots, err := schedulerAvailableSlots(ctx, input.Repos, input.MaxConcurrentRuns)
	if err != nil {
		appendErr(err)
		availableSlots = 0
	}
	if availableSlots > 0 && input.Repos.Queue != nil {
		appendErr(claimAndRunScheduledQueueItems(ctx, availableSlots, input))
	}

	projectsList, err := input.Repos.Projects.List(ctx)
	if err != nil {
		appendErr(err)
		return errors.Join(errs...)
	}
	for _, project := range projectsList {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if project.Archived {
			continue
		}
		repo := repoFromProjectMetadata(project.MetadataJSON)
		if repo == "" {
			if input.Logger != nil {
				input.Logger.Warn("scheduler skipped project without repo metadata", map[string]any{"projectId": project.ID})
			}
			continue
		}
		if input.Planner != nil && discoveryEnabled(input.PlannerDiscoveryEnabled) {
			_, err := input.Planner.DiscoverIssues(ctx, planner.DiscoveryInput{ProjectID: project.ID, Repo: repo})
			appendErr(wrapSchedulerError("planner discovery", project.ID, repo, err))
		} else if input.Planner != nil && input.Logger != nil {
			input.Logger.Debug("planner auto-discovery disabled", map[string]any{"projectId": project.ID, "repo": repo})
		}
		if input.Reviewer != nil && discoveryEnabled(input.ReviewerDiscoveryEnabled) {
			_, err := input.Reviewer.DiscoverPullRequests(ctx, reviewer.DiscoveryInput{ProjectID: project.ID, Repo: repo})
			appendErr(wrapSchedulerError("reviewer discovery", project.ID, repo, err))
		} else if input.Reviewer != nil && input.Logger != nil {
			input.Logger.Debug("reviewer auto-discovery disabled", map[string]any{"projectId": project.ID, "repo": repo})
		}
		if input.Fixer != nil && discoveryEnabled(input.FixerDiscoveryEnabled) {
			_, err := input.Fixer.DiscoverPullRequests(ctx, fixer.DiscoveryInput{ProjectID: project.ID, Repo: repo})
			appendErr(wrapSchedulerError("fixer discovery", project.ID, repo, err))
		} else if input.Fixer != nil && input.Logger != nil {
			input.Logger.Debug("fixer auto-discovery disabled", map[string]any{"projectId": project.ID, "repo": repo})
		}
		if discoverer, ok := input.Worker.(workerIssueDiscoveryScheduler); ok && discoveryEnabled(input.WorkerDiscoveryEnabled) {
			_, err := discoverer.DiscoverIssues(ctx, worker.DiscoveryInput{ProjectID: project.ID, Repo: repo})
			appendErr(wrapSchedulerError("worker issue discovery", project.ID, repo, err))
		} else if input.Worker != nil && input.Logger != nil && !discoveryEnabled(input.WorkerDiscoveryEnabled) {
			input.Logger.Debug("worker auto-discovery disabled", map[string]any{"projectId": project.ID, "repo": repo})
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

func discoveryEnabled(value *bool) bool {
	return value == nil || *value
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

func claimAndRunScheduledQueueItems(ctx context.Context, availableSlots int, input defaultSchedulerTickInput) error {
	if availableSlots <= 0 || input.Repos == nil || input.Repos.Queue == nil {
		return nil
	}
	now := input.Now
	if now == nil {
		now = time.Now
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	queueItems := make([]storage.QueueItemRecord, 0, availableSlots)
	for i := 0; i < availableSlots; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		item, err := input.Repos.Queue.ClaimNext(ctx, nowISO, "scheduler")
		if err != nil {
			return err
		}
		if item == nil {
			break
		}
		queueItems = append(queueItems, *item)
	}
	return runScheduledQueueItems(ctx, queueItems, input)
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
	return input.Repos.Queue.Fail(ctx, storage.QueueFailInput{ID: item.ID, FinishedAt: nowISO, ErrorMessage: &message, ErrorKind: kind, UpdatedAt: nowISO})
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
