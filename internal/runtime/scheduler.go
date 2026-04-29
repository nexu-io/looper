package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/powerformer/looper/internal/agent"
	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
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

type workerIssueDiscoveryScheduler interface {
	DiscoverIssues(context.Context, worker.DiscoveryInput) (worker.DiscoveryResult, error)
}

type schedulerAsyncRunner interface {
	Go(func())
}

type defaultSchedulerTickInput struct {
	Repos             *storage.Repositories
	Logger            bootstrap.Logger
	Now               func() time.Time
	MaxConcurrentRuns int
	AsyncRunner       schedulerAsyncRunner
	Planner           plannerScheduler
	Reviewer          reviewerScheduler
	Fixer             fixerScheduler
	Worker            workerScheduler
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

type plannerGitHubAdapter struct{ gateway *githubinfra.Gateway }

func (a plannerGitHubAdapter) ListOpenIssues(ctx context.Context, input planner.ListOpenIssuesInput) ([]planner.IssueSummary, error) {
	issues, err := a.gateway.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Assignee: input.Assignee, Label: input.Label})
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
	pr, err := a.gateway.CreatePullRequest(ctx, githubinfra.CreatePullRequestInput{Repo: input.Repo, HeadBranch: input.HeadBranch, BaseBranch: input.BaseBranch, Title: input.Title, Body: input.Body, CWD: input.CWD})
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

type plannerGitAdapter struct{ gateway *gitinfra.Gateway }

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
	result, err := a.gateway.Commit(ctx, gitinfra.CommitInput{WorktreePath: input.WorktreePath, Message: input.Message})
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

type reviewerGitHubAdapter struct{ gateway *githubinfra.Gateway }

func (a reviewerGitHubAdapter) ListOpenPullRequests(ctx context.Context, input reviewer.ListOpenPullRequestsInput) ([]reviewer.PullRequestSummary, error) {
	pullRequests, err := a.gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Label: input.Label})
	if err != nil {
		return nil, err
	}
	result := make([]reviewer.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, reviewer.PullRequestSummary{Number: pr.Number, Title: pr.Title, State: pr.State, IsDraft: pr.IsDraft, ReviewDecision: pr.ReviewDecision, Labels: pr.Labels, HeadSHA: pr.HeadSHA, Author: pr.Author, ReviewRequests: pr.ReviewRequests})
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
	return reviewer.PullRequestDetail{Number: detail.Number, Title: detail.Title, Body: detail.Body, State: detail.State, IsDraft: detail.IsDraft, ReviewDecision: detail.ReviewDecision, Labels: detail.Labels, HeadSHA: detail.HeadSHA, BaseSHA: detail.BaseSHA, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, Author: detail.Author, ReviewRequests: detail.ReviewRequests, ChecksSummary: summarizeCheckStates(detail.Checks), Comments: detail.Comments}, nil
}

func (a reviewerGitHubAdapter) CapturePullRequestSnapshot(ctx context.Context, input reviewer.CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	return a.gateway.CapturePullRequestSnapshot(ctx, githubinfra.CapturePullRequestSnapshotInput{ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, CapturedAt: input.CapturedAt})
}

func (a reviewerGitHubAdapter) SubmitReview(ctx context.Context, input reviewer.SubmitReviewInput) error {
	comments := make([]githubinfra.ReviewComment, 0, len(input.Comments))
	for _, comment := range input.Comments {
		comments = append(comments, githubinfra.ReviewComment{Body: comment.Body, Path: comment.Path, Line: comment.Line, Side: comment.Side, StartLine: comment.StartLine, StartSide: comment.StartSide})
	}
	return a.gateway.SubmitReview(ctx, githubinfra.SubmitReviewInput{Repo: input.Repo, PRNumber: input.PRNumber, Event: string(input.Event), Body: input.Body, CommitID: input.CommitID, Comments: comments, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) AddPullRequestComment(ctx context.Context, input reviewer.PullRequestCommentInput) error {
	return a.gateway.AddPullRequestComment(ctx, githubinfra.PullRequestCommentInput{Repo: input.Repo, PRNumber: input.PRNumber, Body: input.Body, CWD: input.CWD})
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
	pullRequests, err := a.gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Author: input.Author})
	if err != nil {
		return nil, err
	}
	result := make([]fixer.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, fixer.PullRequestSummary{Number: pr.Number, State: pr.State, IsDraft: pr.IsDraft, HeadSHA: pr.HeadSHA, Author: pr.Author})
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

type fixerGitAdapter struct{ gateway *gitinfra.Gateway }

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
	result, err := a.gateway.Commit(ctx, gitinfra.CommitInput{WorktreePath: input.WorktreePath, Message: input.Message})
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

type workerGitHubAdapter struct{ gateway *githubinfra.Gateway }

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
	issues, err := a.gateway.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Assignee: input.Assignee, Label: input.Label})
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
	comment, err := a.gateway.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Body: input.Body, CWD: input.CWD})
	if err != nil {
		return worker.IssueCommentResult{}, err
	}
	return worker.IssueCommentResult{ID: comment.ID, URL: comment.URL}, nil
}

func (a workerGitHubAdapter) UpdateIssueComment(ctx context.Context, input worker.UpdateIssueCommentInput) error {
	return a.gateway.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: input.Repo, CommentID: input.CommentID, Body: input.Body, CWD: input.CWD})
}

func (a workerGitHubAdapter) CreatePullRequest(ctx context.Context, input worker.CreatePullRequestInput) (worker.CreatePullRequestResult, error) {
	pr, err := a.gateway.CreatePullRequest(ctx, githubinfra.CreatePullRequestInput{Repo: input.Repo, HeadBranch: input.HeadBranch, BaseBranch: input.BaseBranch, Title: input.Title, Body: input.Body, CWD: input.CWD})
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

type workerGitAdapter struct{ gateway *gitinfra.Gateway }

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
	result, err := a.gateway.Commit(ctx, gitinfra.CommitInput{WorktreePath: input.WorktreePath, Message: input.Message})
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
		Repos: repos,
		Now:   now,
	})
	retryBaseDelay := time.Duration(cfg.Scheduler.RetryBaseDelayMS) * time.Millisecond
	plannerRunner = planner.New(planner.Options{
		DB:               coordinator.DB(),
		Repos:            repos,
		GitHub:           plannerGitHubAdapter{gateway: githubGateway},
		Git:              plannerGitAdapter{gateway: gitGateway},
		AgentExecutor:    plannerAgentExecutorAdapter{executor: agentExecutor},
		Logger:           logger,
		Now:              now,
		AllowAutoPush:    boolPtr(cfg.Defaults.AllowAutoPush),
		RetryBaseDelay:   retryBaseDelay,
		RetryMaxAttempts: int64(cfg.Scheduler.RetryMaxAttempts),
		OnAgentExecutionStarted: func(ctx context.Context, input planner.AgentExecutionStartedInput) error {
			return notifyAgentExecutionStarted(ctx, agentExecutionNotificationInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Title: "Looper Planner", Subtitle: input.Subtitle, Body: input.Body, DedupeKey: input.DedupeKey})
		},
	})
	reviewerRunner = reviewer.New(reviewer.Options{
		DB:               coordinator.DB(),
		Repos:            repos,
		GitHub:           reviewerGitHubAdapter{gateway: githubGateway},
		Git:              reviewerGitAdapter{gateway: gitGateway},
		AgentExecutor:    reviewerAgentExecutorAdapter{executor: agentExecutor},
		Logger:           logger,
		Now:              now,
		AllowAutoApprove: cfg.Defaults.AllowAutoApprove,
		RetryBaseDelay:   retryBaseDelay,
		RetryMaxAttempts: int64(cfg.Scheduler.RetryMaxAttempts),
		OnAgentExecutionStarted: func(ctx context.Context, input reviewer.AgentExecutionStartedInput) error {
			return notifyAgentExecutionStarted(ctx, agentExecutionNotificationInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Title: "Looper Reviewer", Subtitle: input.Subtitle, Body: input.Body, DedupeKey: input.DedupeKey})
		},
	})
	fixerRunner = fixer.New(fixer.Options{
		DB:                 coordinator.DB(),
		Repos:              repos,
		GitHub:             fixerGitHubAdapter{gateway: githubGateway},
		Git:                fixerGitAdapter{gateway: gitGateway},
		AgentExecutor:      fixerAgentExecutorAdapter{executor: agentExecutor},
		Logger:             logger,
		Now:                now,
		AllowAutoCommit:    cfg.Defaults.AllowAutoCommit,
		AllowAutoPush:      cfg.Defaults.AllowAutoPush,
		AllowRiskyFixes:    cfg.Defaults.AllowRiskyFixes,
		FixAllPullRequests: cfg.Defaults.FixAllPullRequests,
		RetryBaseDelay:     retryBaseDelay,
		RetryMaxAttempts:   int64(cfg.Scheduler.RetryMaxAttempts),
		OnAgentExecutionStarted: func(ctx context.Context, input fixer.AgentExecutionStartedInput) error {
			return notifyAgentExecutionStarted(ctx, agentExecutionNotificationInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Title: "Looper Fixer", Subtitle: input.Subtitle, Body: input.Body, DedupeKey: input.DedupeKey})
		},
	})
	workerRunner = worker.New(worker.Options{
		DB:     coordinator.DB(),
		Repos:  repos,
		GitHub: workerGitHubAdapter{gateway: githubGateway},
		GitHubCLIAutoPROpeningAvailable: func(ctx context.Context, repo, cwd string) bool {
			return githubCLIAutoPROpeningAvailable(ctx, cfg, githubGateway, logger, repo, cwd)
		},
		Git:              workerGitAdapter{gateway: gitGateway},
		AgentExecutor:    workerAgentExecutorAdapter{executor: agentExecutor, registry: activeExecutions},
		Logger:           logger,
		Now:              now,
		AllowAutoCommit:  cfg.Defaults.AllowAutoCommit,
		AllowAutoPush:    cfg.Defaults.AllowAutoPush,
		OpenPRStrategy:   cfg.Defaults.OpenPRStrategy,
		RetryBaseDelay:   retryBaseDelay,
		RetryMaxAttempts: int64(cfg.Scheduler.RetryMaxAttempts),
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
			Repos:             services.Repositories,
			Logger:            logger,
			Now:               now,
			MaxConcurrentRuns: cfg.Scheduler.MaxConcurrentRuns,
			AsyncRunner:       runner,
			Planner:           plannerRunner,
			Reviewer:          reviewerRunner,
			Fixer:             fixerRunner,
			Worker:            workerRunner,
		})
	}
}

func githubCLIAutoPROpeningAvailable(ctx context.Context, cfg config.Config, githubGateway *githubinfra.Gateway, logger bootstrap.Logger, repo, cwd string) bool {
	if githubGateway == nil {
		return false
	}
	authenticated, err := githubGateway.IsAuthenticated(ctx, cwd, githubAuthHostname(repo))
	if err != nil {
		if logger != nil {
			logger.Warn("github cli auth check failed; disabling automatic PR opening", map[string]any{"error": err.Error(), "repo": repo, "hostname": githubAuthHostname(repo)})
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

	projectsList, err := input.Repos.Projects.List(ctx)
	if err != nil {
		return err
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
		if input.Planner != nil {
			_, err := input.Planner.DiscoverIssues(ctx, planner.DiscoveryInput{ProjectID: project.ID, Repo: repo})
			appendErr(wrapSchedulerError("planner discovery", project.ID, repo, err))
		}
		if input.Reviewer != nil {
			_, err := input.Reviewer.DiscoverPullRequests(ctx, reviewer.DiscoveryInput{ProjectID: project.ID, Repo: repo})
			appendErr(wrapSchedulerError("reviewer discovery", project.ID, repo, err))
		}
		if input.Fixer != nil {
			_, err := input.Fixer.DiscoverPullRequests(ctx, fixer.DiscoveryInput{ProjectID: project.ID, Repo: repo})
			appendErr(wrapSchedulerError("fixer discovery", project.ID, repo, err))
		}
		if discoverer, ok := input.Worker.(workerIssueDiscoveryScheduler); ok {
			_, err := discoverer.DiscoverIssues(ctx, worker.DiscoveryInput{ProjectID: project.ID, Repo: repo})
			appendErr(wrapSchedulerError("worker issue discovery", project.ID, repo, err))
		}
	}

	if err := ctx.Err(); err != nil {
		return errors.Join(append(errs, err)...)
	}
	availableSlots, err := schedulerAvailableSlots(ctx, input.Repos, input.MaxConcurrentRuns)
	if err != nil {
		appendErr(err)
		availableSlots = 0
	}
	if availableSlots > 0 && input.Repos.Queue != nil {
		appendErr(claimAndRunScheduledQueueItems(ctx, availableSlots, input))
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
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
	default:
		return nil, fmt.Errorf("unsupported queue item type %q", item.Type)
	}
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
