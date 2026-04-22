package planner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/powerformer/looper/internal/agent"
	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/eventlog"
	"github.com/powerformer/looper/internal/infra/specpr"
	"github.com/powerformer/looper/internal/storage"
)

const (
	stepDiscoverIssues  PlannerStep = "discover-issues"
	stepPrepareWorktree PlannerStep = "prepare-worktree"
	stepWriteSpec       PlannerStep = "write-spec"
	stepPublish         PlannerStep = "publish"
	stepNotify          PlannerStep = "notify"

	discoveryLabel = "looper:plan"

	defaultAgentTimeout = 30 * time.Minute
	defaultClaimTTL     = 10 * time.Minute
	defaultRetryDelay   = 5 * time.Second
	defaultRetryMax     = 3
)

var plannerStepSequence = []PlannerStep{stepDiscoverIssues, stepPrepareWorktree, stepWriteSpec, stepPublish, stepNotify}

type PlannerStep string

type QueueFailureKind string

const (
	FailureRetryableTransient   QueueFailureKind = "retryable_transient"
	FailureRetryableAfterResume QueueFailureKind = "retryable_after_resume"
	FailureNonRetryable         QueueFailureKind = "non_retryable"
	FailureManualIntervention   QueueFailureKind = "manual_intervention"
)

type IssueSummary struct {
	Number    int64
	Title     string
	Body      string
	URL       string
	Assignees []string
	Labels    []string
}

type IssueDetail struct {
	Number    int64
	Title     string
	Body      string
	URL       string
	Assignees []string
	Labels    []string
}

type ListOpenIssuesInput struct {
	Repo     string
	CWD      string
	Limit    int
	Assignee string
	Label    string
}

type ViewIssueInput struct {
	Repo        string
	IssueNumber int64
	CWD         string
}

type CreatePullRequestInput struct {
	Repo       string
	HeadBranch string
	BaseBranch string
	Title      string
	Body       string
	CWD        string
}

type CreatePullRequestResult struct {
	Number int64
	URL    string
}

type PullRequestLabelsInput struct {
	Repo     string
	PRNumber int64
	Labels   []string
	CWD      string
}

type PullRequestReviewersInput struct {
	Repo      string
	PRNumber  int64
	Reviewers []string
	CWD       string
}

type GitHubGateway interface {
	ListOpenIssues(context.Context, ListOpenIssuesInput) ([]IssueSummary, error)
	ViewIssue(context.Context, ViewIssueInput) (IssueDetail, error)
	GetCurrentUserLogin(context.Context, string) (string, error)
	CreatePullRequest(context.Context, CreatePullRequestInput) (CreatePullRequestResult, error)
	AddPullRequestLabels(context.Context, PullRequestLabelsInput) error
	AddPullRequestReviewers(context.Context, PullRequestReviewersInput) error
}

type CreateWorktreeInput struct {
	ProjectID         string
	RepoPath          string
	WorktreeRoot      string
	Branch            string
	BaseBranch        string
	ProtectedBranches []string
}

type CreateWorktreeResult struct {
	ID           string
	WorktreePath string
	Branch       string
	BaseBranch   string
}

type PushInput struct {
	WorktreePath      string
	Branch            string
	Remote            string
	ProtectedBranches []string
}

type GitGateway interface {
	CreateWorktree(context.Context, CreateWorktreeInput) (CreateWorktreeResult, error)
	Push(context.Context, PushInput) error
}

type AgentRunInput struct {
	ExecutionID      string
	ProjectID        string
	LoopID           string
	RunID            string
	Prompt           string
	WorkingDirectory string
	Timeout          time.Duration
	Metadata         map[string]any
	IdempotencyKey   string
}

type AgentResult struct {
	Status  string
	Summary string
	Stdout  string
	Commits []string
}

type AgentExecution interface {
	Wait(context.Context) (AgentResult, error)
}

type AgentExecutor interface {
	Start(context.Context, AgentRunInput) (AgentExecution, error)
}

type AgentExecutionStartedInput struct {
	ExecutionID string
	ProjectID   string
	LoopID      string
	RunID       string
	Subtitle    string
	Body        string
	DedupeKey   string
}

type AgentExecutionStartedFunc func(context.Context, AgentExecutionStartedInput) error

type Options struct {
	DB                      *sql.DB
	Repos                   *storage.Repositories
	GitHub                  GitHubGateway
	Git                     GitGateway
	AgentExecutor           AgentExecutor
	Logger                  bootstrap.Logger
	Now                     func() time.Time
	AgentTimeout            time.Duration
	ClaimTTL                time.Duration
	AllowAutoPush           *bool
	RetryBaseDelay          time.Duration
	RetryMaxAttempts        int64
	OnAgentExecutionStarted AgentExecutionStartedFunc
}

type Runner struct {
	db                      *sql.DB
	repos                   *storage.Repositories
	github                  GitHubGateway
	git                     GitGateway
	agentExecutor           AgentExecutor
	logger                  bootstrap.Logger
	now                     func() time.Time
	agentTimeout            time.Duration
	claimTTL                time.Duration
	allowAutoPush           bool
	retryBaseDelay          time.Duration
	retryMaxAttempts        int64
	onAgentExecutionStarted AgentExecutionStartedFunc
}

type DiscoveryInput struct {
	ProjectID string
	Repo      string
	Limit     int
}

type DiscoveryResult struct {
	QueueItems     []storage.QueueItemRecord
	CreatedLoopIDs []string
	Skipped        int
}

type ProcessResult struct {
	LoopID            string
	RunID             string
	QueueItemID       string
	Status            string
	Summary           string
	FailureKind       QueueFailureKind
	PullRequestNumber int64
}

type plannerCheckpoint struct {
	ResumePolicy   string                  `json:"resumePolicy,omitempty"`
	Issue          *checkpointIssue        `json:"issue,omitempty"`
	ClaimedLockKey string                  `json:"claimedLockKey,omitempty"`
	Worktree       *checkpointWorktree     `json:"worktree,omitempty"`
	WriteSpec      *checkpointWriteSpec    `json:"writeSpec,omitempty"`
	Publish        *checkpointPublishState `json:"publish,omitempty"`
	Notify         *checkpointNotify       `json:"notify,omitempty"`
	SkipReason     string                  `json:"skipReason,omitempty"`
}

type checkpointIssue struct {
	Repo               string   `json:"repo,omitempty"`
	IssueNumber        int64    `json:"issueNumber,omitempty"`
	Title              string   `json:"title,omitempty"`
	Body               string   `json:"body,omitempty"`
	URL                string   `json:"url,omitempty"`
	Assignees          []string `json:"assignees,omitempty"`
	Labels             []string `json:"labels,omitempty"`
	CurrentUserLogin   string   `json:"currentUserLogin,omitempty"`
	SpecPath           string   `json:"specPath,omitempty"`
	RequestedReviewers []string `json:"requestedReviewers,omitempty"`
}

type checkpointWorktree struct {
	ID         string `json:"id,omitempty"`
	Path       string `json:"path,omitempty"`
	Branch     string `json:"branch,omitempty"`
	BaseBranch string `json:"baseBranch,omitempty"`
	SpecPath   string `json:"specPath,omitempty"`
}

type checkpointWriteSpec struct {
	Status  string   `json:"status,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Stdout  string   `json:"stdout,omitempty"`
	Commits []string `json:"commits,omitempty"`
}

type checkpointPullRequest struct {
	Number int64  `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
	Body   string `json:"body,omitempty"`
}

type checkpointPublishState struct {
	Pushed         bool                   `json:"pushed,omitempty"`
	PullRequest    *checkpointPullRequest `json:"pullRequest,omitempty"`
	LabelsAdded    []string               `json:"labelsAdded,omitempty"`
	ReviewersAdded []string               `json:"reviewersAdded,omitempty"`
}

type checkpointNotify struct {
	SentAt  string `json:"sentAt,omitempty"`
	Message string `json:"message,omitempty"`
}

type resumedRunContext struct {
	Run        storage.RunRecord
	StartStep  PlannerStep
	Checkpoint plannerCheckpoint
	Resumed    bool
}

type stepInput struct {
	Project    storage.ProjectRecord
	Loop       storage.LoopRecord
	Run        storage.RunRecord
	QueueItem  storage.QueueItemRecord
	Checkpoint plannerCheckpoint
}

type loopError struct {
	message string
	kind    QueueFailureKind
}

func (e *loopError) Error() string { return e.message }

type transientFailure interface{ Temporary() bool }

func New(options Options) *Runner {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	agentTimeout := options.AgentTimeout
	if agentTimeout <= 0 {
		agentTimeout = defaultAgentTimeout
	}
	claimTTL := options.ClaimTTL
	if claimTTL <= 0 {
		claimTTL = defaultClaimTTL
	}
	retryBaseDelay := options.RetryBaseDelay
	if retryBaseDelay <= 0 {
		retryBaseDelay = defaultRetryDelay
	}
	retryMax := options.RetryMaxAttempts
	if retryMax <= 0 {
		retryMax = defaultRetryMax
	}
	allowAutoPush := true
	if options.AllowAutoPush != nil {
		allowAutoPush = *options.AllowAutoPush
	}
	return &Runner{db: options.DB, repos: options.Repos, github: options.GitHub, git: options.Git, agentExecutor: options.AgentExecutor, logger: options.Logger, now: now, agentTimeout: agentTimeout, claimTTL: claimTTL, allowAutoPush: allowAutoPush, retryBaseDelay: retryBaseDelay, retryMaxAttempts: retryMax, onAgentExecutionStarted: options.OnAgentExecutionStarted}
}

func (r *Runner) DiscoverIssues(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	if r.repos == nil || r.repos.Projects == nil || r.repos.Loops == nil || r.repos.Queue == nil || r.repos.Runs == nil {
		return DiscoveryResult{}, fmt.Errorf("planner repositories are not configured")
	}
	project, err := r.repos.Projects.GetByID(ctx, input.ProjectID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	if project == nil {
		return DiscoveryResult{}, fmt.Errorf("project not found: %s", input.ProjectID)
	}
	if project.Archived {
		return DiscoveryResult{Skipped: 1}, nil
	}
	login, err := r.github.GetCurrentUserLogin(ctx, project.RepoPath)
	if err != nil {
		return DiscoveryResult{}, err
	}
	login = normalizeLogin(login)
	if login == "" {
		return DiscoveryResult{Skipped: 1}, nil
	}
	issues, err := r.github.ListOpenIssues(ctx, ListOpenIssuesInput{Repo: input.Repo, CWD: project.RepoPath, Limit: input.Limit, Assignee: login, Label: discoveryLabel})
	if err != nil {
		return DiscoveryResult{}, err
	}
	result := DiscoveryResult{}
	for _, issue := range issues {
		if !shouldClaimIssue(issue, login) {
			result.Skipped++
			continue
		}
		loopResult, err := r.ensureLoopForIssue(ctx, *project, input.Repo, issue)
		if err != nil {
			return DiscoveryResult{}, err
		}
		if loopResult.created {
			result.CreatedLoopIDs = append(result.CreatedLoopIDs, loopResult.record.ID)
		}
		if loopResult.record.Status == "paused" || loopResult.record.Status == "completed" {
			result.Skipped++
			continue
		}
		queueItem, err := r.enqueue(ctx, enqueueInput{ProjectID: project.ID, LoopID: loopResult.record.ID, Repo: input.Repo, IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number, "title": issue.Title, "body": issue.Body, "url": issue.URL, "assignees": issue.Assignees, "labels": issue.Labels, "currentUserLogin": login}})
		if err != nil {
			return DiscoveryResult{}, err
		}
		result.QueueItems = append(result.QueueItems, queueItem)
	}
	return result, nil
}

func (r *Runner) ProcessNext(ctx context.Context, claimedBy string) (*ProcessResult, error) {
	if r.repos == nil || r.repos.Queue == nil {
		return nil, fmt.Errorf("planner queue repository is not configured")
	}
	item, err := r.repos.Queue.ClaimNextOfType(ctx, r.nowISO(), claimedBy, "planner")
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, nil
	}
	return r.ProcessClaimedQueueItem(ctx, *item)
}

func (r *Runner) ProcessClaimedQueueItem(ctx context.Context, queueItem storage.QueueItemRecord) (*ProcessResult, error) {
	result, err := r.ProcessClaimedItem(ctx, queueItem)
	if err != nil {
		return r.recoverClaimedItem(ctx, queueItem, err)
	}
	return &result, nil
}

func (r *Runner) recoverClaimedItem(ctx context.Context, queueItem storage.QueueItemRecord, err error) (*ProcessResult, error) {
	failure := r.classifyFailure(err)
	failedQueue, failErr := r.failQueueItem(ctx, queueItem, failure.kind, failure.message)
	if failErr != nil {
		return nil, failErr
	}
	if err := r.reconcileRecoveredLoop(ctx, queueItem, failedQueue, failure.kind); err != nil {
		return nil, err
	}
	return &ProcessResult{LoopID: derefString(queueItem.LoopID), QueueItemID: queueItem.ID, Status: "failed", Summary: failure.message, FailureKind: failure.kind}, nil
}

func (r *Runner) reconcileRecoveredLoop(ctx context.Context, queueItem storage.QueueItemRecord, failedQueue *storage.QueueItemRecord, failureKind QueueFailureKind) error {
	if queueItem.LoopID == nil {
		return nil
	}
	loop, err := r.repos.Loops.GetByID(ctx, *queueItem.LoopID)
	if err != nil {
		return err
	}
	if loop == nil || loop.Status != "running" {
		return nil
	}
	_, err = r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
		updated.LastRunAt = stringPtr(r.nowISO())
		if updated.Status == "paused" {
			updated.NextRunAt = nil
		} else if failedQueue != nil && failedQueue.Status == "queued" {
			updated.Status = "queued"
			updated.NextRunAt = stringPtr(failedQueue.AvailableAt)
		} else {
			if failureKind == FailureManualIntervention || (failedQueue != nil && failedQueue.Status == "cancelled") {
				updated.Status = "paused"
			} else {
				updated.Status = "failed"
			}
			updated.NextRunAt = nil
		}
	})
	return err
}

func (r *Runner) ProcessClaimedItem(ctx context.Context, queueItem storage.QueueItemRecord) (ProcessResult, error) {
	if queueItem.Type != "planner" {
		return ProcessResult{}, fmt.Errorf("unsupported queue item type: %s", queueItem.Type)
	}
	if queueItem.LoopID == nil {
		return ProcessResult{}, fmt.Errorf("planner queue item requires loopId")
	}
	loop, err := r.repos.Loops.GetByID(ctx, *queueItem.LoopID)
	if err != nil {
		return ProcessResult{}, err
	}
	if loop == nil {
		return ProcessResult{}, fmt.Errorf("loop not found: %s", *queueItem.LoopID)
	}
	project, err := r.repos.Projects.GetByID(ctx, loop.ProjectID)
	if err != nil {
		return ProcessResult{}, err
	}
	if project == nil {
		return ProcessResult{}, fmt.Errorf("project not found: %s", loop.ProjectID)
	}
	resumedRun, err := r.createRunContext(ctx, *loop)
	if err != nil {
		return ProcessResult{}, err
	}
	run := resumedRun.Run
	checkpoint := resumedRun.Checkpoint
	claimedLockKey := ""
	if resumedRun.StartStep != stepDiscoverIssues {
		claimedLockKey = checkpoint.ClaimedLockKey
	}
	acquiredClaimedLock := false
	if claimedLockKey != "" {
		reason := "planner-run-resume"
		nowISO := r.nowISO()
		acquired, err := r.repos.Locks.Acquire(ctx, storage.LockRecord{Key: claimedLockKey, Owner: queueItem.ID, Reason: &reason, ExpiresAt: eventlog.FormatJavaScriptISOString(r.now().Add(r.claimTTL)), CreatedAt: nowISO, UpdatedAt: nowISO})
		if err != nil {
			return ProcessResult{}, err
		}
		if !acquired {
			return ProcessResult{}, &loopError{message: fmt.Sprintf("Issue lock is already held for %s", claimedLockKey), kind: FailureRetryableTransient}
		}
		acquiredClaimedLock = true
	}
	defer func() {
		if acquiredClaimedLock && claimedLockKey != "" {
			_ = r.repos.Locks.Release(context.Background(), claimedLockKey)
		}
	}()
	if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
		updated.Status = "running"
		updated.LastRunAt = stringPtr(run.StartedAt)
		updated.NextRunAt = nil
	}); err != nil {
		return ProcessResult{}, err
	}
	r.appendEvent(ctx, eventInput{eventType: "loop.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "loop", entityID: loop.ID, payload: map[string]any{"queueItemId": queueItem.ID, "resumed": resumedRun.Resumed, "startStep": string(resumedRun.StartStep)}})
	r.appendEvent(ctx, eventInput{eventType: "run.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"queueItemId": queueItem.ID, "currentStep": string(resumedRun.StartStep)}})

	for _, step := range stepsFrom(resumedRun.StartStep) {
		run, err = r.persistStepStarted(ctx, run, step, checkpoint)
		if err != nil {
			return ProcessResult{}, err
		}
		r.appendEvent(ctx, eventInput{eventType: "loop.step.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"step": string(step)}})
		checkpoint, err = r.executeStep(ctx, step, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: queueItem, Checkpoint: checkpoint})
		if err != nil {
			failure := r.classifyFailure(err)
			latest := r.getLatestCheckpoint(ctx, run, checkpoint)
			resumePolicy := latest.ResumePolicy
			switch failure.kind {
			case FailureRetryableAfterResume:
				resumePolicy = "advance_from_checkpoint"
			case FailureManualIntervention:
				resumePolicy = "manual_intervention"
			default:
				if resumePolicy == "" {
					resumePolicy = "replay_step"
				}
			}
			latest.ResumePolicy = resumePolicy
			if _, err := r.completeRun(ctx, run, "failed", failure.message, failure.message, latest); err != nil {
				return ProcessResult{}, err
			}
			r.appendEvent(ctx, eventInput{eventType: "loop.step.failed", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"message": failure.message, "failureKind": string(failure.kind), "currentStep": derefString(run.CurrentStep)}})
			r.appendEvent(ctx, eventInput{eventType: "run.failed", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"summary": failure.message, "failureKind": string(failure.kind)}})
			failedQueue, err := r.failQueueItem(ctx, queueItem, failure.kind, failure.message)
			if err != nil {
				return ProcessResult{}, err
			}
			if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
				updated.LastRunAt = stringPtr(r.nowISO())
				if updated.Status == "paused" {
					updated.NextRunAt = nil
				} else if failedQueue != nil && failedQueue.Status == "queued" {
					updated.Status = "queued"
					updated.NextRunAt = stringPtr(failedQueue.AvailableAt)
				} else {
					if failure.kind == FailureManualIntervention || (failedQueue != nil && failedQueue.Status == "cancelled") {
						updated.Status = "paused"
					} else {
						updated.Status = "failed"
					}
					updated.NextRunAt = nil
				}
			}); err != nil {
				return ProcessResult{}, err
			}
			return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: "failed", Summary: failure.message, FailureKind: failure.kind}, nil
		}
		if step == stepDiscoverIssues {
			claimedLockKey = checkpoint.ClaimedLockKey
			acquiredClaimedLock = claimedLockKey != ""
		}
		run, err = r.persistStepCompleted(ctx, run, step, checkpoint)
		if err != nil {
			return ProcessResult{}, err
		}
		r.appendEvent(ctx, eventInput{eventType: "loop.step.completed", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"step": string(step)}})
		if checkpoint.SkipReason != "" {
			break
		}
	}

	summary := checkpoint.SkipReason
	if summary == "" {
		issue := checkpoint.Issue
		if issue != nil {
			summary = fmt.Sprintf("Opened spec PR for %s#%d", issue.Repo, issue.IssueNumber)
		} else {
			summary = "Completed planner run"
		}
	}
	if _, err := r.completeRun(ctx, run, "success", summary, "", checkpoint); err != nil {
		return ProcessResult{}, err
	}
	r.appendEvent(ctx, eventInput{eventType: "run.completed", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"summary": summary}})
	if err := r.repos.Queue.Complete(ctx, queueItem.ID, r.nowISO()); err != nil {
		return ProcessResult{}, err
	}
	if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
		updated.Status = "completed"
		updated.LastRunAt = stringPtr(r.nowISO())
		updated.NextRunAt = nil
	}); err != nil {
		return ProcessResult{}, err
	}
	status := "success"
	if checkpoint.SkipReason != "" {
		status = "skipped"
	}
	prNumber := int64(0)
	if checkpoint.Publish != nil && checkpoint.Publish.PullRequest != nil {
		prNumber = checkpoint.Publish.PullRequest.Number
	}
	return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: status, Summary: summary, PullRequestNumber: prNumber}, nil
}

func (r *Runner) executeStep(ctx context.Context, step PlannerStep, input stepInput) (plannerCheckpoint, error) {
	switch step {
	case stepDiscoverIssues:
		return r.runDiscoverIssueStep(ctx, input)
	case stepPrepareWorktree:
		return r.runPrepareWorktreeStep(ctx, input)
	case stepWriteSpec:
		return r.runWriteSpecStep(ctx, input)
	case stepPublish:
		return r.runPublishStep(ctx, input)
	case stepNotify:
		return r.runNotifyStep(input)
	default:
		return input.Checkpoint, fmt.Errorf("unsupported planner step: %s", step)
	}
}

func (r *Runner) runDiscoverIssueStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	payload := parseJSONObject(input.QueueItem.PayloadJSON)
	repo := firstNonEmpty(derefString(input.QueueItem.Repo), derefString(input.Loop.Repo), projectRepo(input.Project))
	issueNumber := int64FromAny(payload["issueNumber"])
	if issueNumber == 0 {
		issueNumber = parseIssueNumberFromTargetID(derefString(input.Loop.TargetID))
	}
	if repo == "" || issueNumber == 0 {
		return input.Checkpoint, &loopError{message: "Planner queue item requires repo and issue number", kind: FailureNonRetryable}
	}
	detail, err := r.github.ViewIssue(ctx, ViewIssueInput{Repo: repo, IssueNumber: issueNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return input.Checkpoint, err
	}
	currentLogin := firstNonEmpty(stringFromAnyDefault(payload["currentUserLogin"]), input.CheckpointIssueLogin())
	if currentLogin == "" {
		login, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
		if err == nil {
			currentLogin = normalizeLogin(login)
		}
	}
	lockKey := firstNonEmpty(derefString(input.QueueItem.LockKey), buildIssueLockKey(repo, issueNumber))
	nowISO := r.nowISO()
	reason := "planner-run"
	acquired, err := r.repos.Locks.Acquire(ctx, storage.LockRecord{Key: lockKey, Owner: input.QueueItem.ID, Reason: &reason, ExpiresAt: eventlog.FormatJavaScriptISOString(r.now().Add(r.claimTTL)), CreatedAt: nowISO, UpdatedAt: nowISO})
	if err != nil {
		return input.Checkpoint, err
	}
	if !acquired {
		return input.Checkpoint, &loopError{message: fmt.Sprintf("Issue lock is already held for %s", lockKey), kind: FailureRetryableTransient}
	}
	checkpoint := input.Checkpoint
	checkpoint.Issue = &checkpointIssue{Repo: repo, IssueNumber: issueNumber, Title: detail.Title, Body: detail.Body, URL: detail.URL, Assignees: cloneStrings(detail.Assignees), Labels: cloneStrings(detail.Labels), CurrentUserLogin: currentLogin, SpecPath: buildSpecPath(r.now(), issueNumber, detail.Title), RequestedReviewers: resolveRequestedReviewers(input.Project, input.Loop, detail.Assignees, currentLogin)}
	checkpoint.ClaimedLockKey = lockKey
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	if currentLogin != "" && !includesLogin(detail.Assignees, currentLogin) {
		checkpoint.SkipReason = fmt.Sprintf("Issue %s#%d is no longer assigned to %s", repo, issueNumber, currentLogin)
		return checkpoint, nil
	}
	if !specpr.HasLabel(detail.Labels, discoveryLabel) {
		checkpoint.SkipReason = fmt.Sprintf("Issue %s#%d no longer has %s", repo, issueNumber, discoveryLabel)
		return checkpoint, nil
	}
	checkpoint.SkipReason = ""
	return checkpoint, nil
}

func (input stepInput) CheckpointIssueLogin() string {
	if input.Checkpoint.Issue == nil {
		return ""
	}
	return input.Checkpoint.Issue.CurrentUserLogin
}

func (r *Runner) runPrepareWorktreeStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" || checkpoint.Worktree != nil {
		return checkpoint, nil
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	projectMetadata := parseJSONObject(input.Project.MetadataJSON)
	worktreeRoot := stringFromAnyDefault(projectMetadata["worktreeRoot"])
	if worktreeRoot == "" {
		worktreeRoot, err = config.DefaultProjectWorktreeRoot(input.Project.ID, input.Project.RepoPath)
		if err != nil {
			return checkpoint, err
		}
	}
	baseBranch := firstNonEmpty(derefString(input.Project.BaseBranch), "main")
	branch := buildPlannerBranch(issue.IssueNumber, issue.Title)
	created, err := r.git.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: input.Project.ID, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, Branch: branch, BaseBranch: baseBranch, ProtectedBranches: []string{baseBranch}})
	if err != nil {
		return checkpoint, err
	}
	checkpoint.Worktree = &checkpointWorktree{ID: created.ID, Path: created.WorktreePath, Branch: created.Branch, BaseBranch: firstNonEmpty(created.BaseBranch, baseBranch), SpecPath: issue.SpecPath}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runWriteSpecStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if checkpoint.WriteSpec != nil && strings.EqualFold(checkpoint.WriteSpec.Status, "completed") {
		return checkpoint, nil
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	executionID := eventlog.NewEventID("agent")
	prompt := buildPlannerPrompt(input.Project, issue, worktree)
	execution, err := r.agentExecutor.Start(ctx, AgentRunInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Prompt: prompt, WorkingDirectory: worktree.Path, Timeout: r.agentTimeout, Metadata: map[string]any{"loopType": "planner", "repo": issue.Repo, "issueNumber": issue.IssueNumber, "specPath": issue.SpecPath}, IdempotencyKey: fmt.Sprintf("planner:%s", input.Loop.ID)})
	if err != nil {
		return checkpoint, err
	}
	if r.onAgentExecutionStarted != nil {
		if err := r.onAgentExecutionStarted(ctx, AgentExecutionStartedInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Subtitle: fmt.Sprintf("%s#%d", issue.Repo, issue.IssueNumber), Body: fmt.Sprintf("Planner started for %s", issue.Title), DedupeKey: "runtime.agent.started:planner:" + input.Run.ID}); err != nil && r.logger != nil {
			r.logger.Warn("planner agent start notification failed", map[string]any{"loopId": input.Loop.ID, "runId": input.Run.ID, "error": err.Error()})
		}
	}
	result, err := execution.Wait(ctx)
	if err != nil {
		return checkpoint, err
	}
	if !strings.EqualFold(result.Status, "completed") {
		return checkpoint, &loopError{message: firstNonEmpty(result.Summary, "Planner agent "+result.Status), kind: FailureRetryableTransient}
	}
	checkpoint.WriteSpec = &checkpointWriteSpec{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Commits: append([]string(nil), result.Commits...)}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runPublishStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if !r.allowAutoPush {
		checkpoint.SkipReason = fmt.Sprintf("Auto push disabled; manual publish required for planner %s", input.Loop.ID)
		checkpoint.ResumePolicy = "manual_intervention"
		return checkpoint, nil
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	if checkpoint.Publish == nil {
		checkpoint.Publish = &checkpointPublishState{}
	}
	if checkpoint.Publish.LabelsAdded == nil {
		checkpoint.Publish.LabelsAdded = []string{}
	}
	if checkpoint.Publish.ReviewersAdded == nil {
		checkpoint.Publish.ReviewersAdded = []string{}
	}
	if !checkpoint.Publish.Pushed {
		if err := r.git.Push(ctx, PushInput{WorktreePath: worktree.Path, Branch: worktree.Branch, ProtectedBranches: []string{worktree.BaseBranch}}); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		checkpoint.Publish.Pushed = true
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
	}
	if checkpoint.Publish.PullRequest == nil {
		body := buildPullRequestBody(*issue, *worktree, checkpoint.WriteSpec)
		pr, err := r.github.CreatePullRequest(ctx, CreatePullRequestInput{Repo: issue.Repo, HeadBranch: worktree.Branch, BaseBranch: worktree.BaseBranch, Title: "Spec: " + issue.Title, Body: body, CWD: input.Project.RepoPath})
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		if pr.Number == 0 {
			return checkpoint, &loopError{message: "Planner publish requires a pull request number", kind: FailureRetryableAfterResume}
		}
		checkpoint.Publish.PullRequest = &checkpointPullRequest{Number: pr.Number, URL: pr.URL, Body: body}
		if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
			updated.Repo = stringPtr(issue.Repo)
			updated.PRNumber = &pr.Number
		}); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
		metadataJSON, err := mergeLoopMetadataJSON(input.Loop.MetadataJSON, map[string]any{"issueNumber": issue.IssueNumber, "issueUrl": issue.URL, "issueTitle": issue.Title, "specPath": issue.SpecPath, "branch": worktree.Branch, "prUrl": pr.URL, "prNumber": pr.Number, "requestedReviewers": issue.RequestedReviewers})
		if err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
		if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) { updated.MetadataJSON = stringPtr(metadataJSON) }); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
	}
	pr := checkpoint.Publish.PullRequest
	if pr == nil || pr.Number == 0 {
		return checkpoint, &loopError{message: "Planner publish requires a pull request number", kind: FailureRetryableAfterResume}
	}
	if !stringInSlice(specpr.ReviewingLabel, checkpoint.Publish.LabelsAdded) {
		if err := r.github.AddPullRequestLabels(ctx, PullRequestLabelsInput{Repo: issue.Repo, PRNumber: pr.Number, Labels: []string{specpr.ReviewingLabel}, CWD: input.Project.RepoPath}); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		checkpoint.Publish.LabelsAdded = append(checkpoint.Publish.LabelsAdded, specpr.ReviewingLabel)
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
	}
	pendingReviewers := make([]string, 0)
	for _, reviewer := range issue.RequestedReviewers {
		if !stringInSlice(reviewer, checkpoint.Publish.ReviewersAdded) {
			pendingReviewers = append(pendingReviewers, reviewer)
		}
	}
	if len(pendingReviewers) > 0 {
		if err := r.github.AddPullRequestReviewers(ctx, PullRequestReviewersInput{Repo: issue.Repo, PRNumber: pr.Number, Reviewers: pendingReviewers, CWD: input.Project.RepoPath}); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		checkpoint.Publish.ReviewersAdded = append(checkpoint.Publish.ReviewersAdded, pendingReviewers...)
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
	}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runNotifyStep(input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" || checkpoint.Notify != nil {
		return checkpoint, nil
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	message := fmt.Sprintf("Planner completed for %s#%d", issue.Repo, issue.IssueNumber)
	if checkpoint.Publish != nil && checkpoint.Publish.PullRequest != nil && checkpoint.Publish.PullRequest.URL != "" {
		message = "Spec PR ready for review: " + checkpoint.Publish.PullRequest.URL
	}
	checkpoint.Notify = &checkpointNotify{SentAt: r.nowISO(), Message: message}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) createRunContext(ctx context.Context, loop storage.LoopRecord) (resumedRunContext, error) {
	latestRun, err := r.repos.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return resumedRunContext{}, err
	}
	check := parseCheckpoint(nil)
	lastCompleted := PlannerStep("")
	if latestRun != nil {
		check = parseCheckpoint(latestRun.CheckpointJSON)
		lastCompleted = asPlannerStep(derefString(latestRun.LastCompletedStep))
	}
	shouldResume := latestRun != nil && (latestRun.Status == "failed" || latestRun.Status == "interrupted") && check.ResumePolicy != "manual_intervention" && lastCompleted != ""
	startStep := stepDiscoverIssues
	if shouldResume {
		if next := nextPlannerStep(lastCompleted); next != "" {
			startStep = next
		}
	}
	resumed := shouldResume && startStep != stepDiscoverIssues
	initialCheckpoint := plannerCheckpoint{ResumePolicy: "replay_step"}
	if resumed {
		initialCheckpoint = check
		initialCheckpoint.ResumePolicy = "advance_from_checkpoint"
	}
	nowISO := r.nowISO()
	run := storage.RunRecord{ID: eventlog.NewEventID("run"), LoopID: loop.ID, Status: "running", CurrentStep: stringPtr(string(startStep)), StartedAt: nowISO, LastHeartbeatAt: stringPtr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO}
	if resumed && lastCompleted != "" {
		run.LastCompletedStep = stringPtr(string(lastCompleted))
	}
	encoded := mustMarshalJSON(initialCheckpoint)
	run.CheckpointJSON = &encoded
	if err := r.repos.Runs.Upsert(ctx, run); err != nil {
		return resumedRunContext{}, err
	}
	return resumedRunContext{Run: run, StartStep: startStep, Checkpoint: initialCheckpoint, Resumed: resumed}, nil
}

func (r *Runner) persistStepStarted(ctx context.Context, run storage.RunRecord, step PlannerStep, checkpoint plannerCheckpoint) (storage.RunRecord, error) {
	updated := run
	nowISO := r.nowISO()
	updated.CurrentStep = stringPtr(string(step))
	encoded := mustMarshalJSON(checkpoint)
	updated.CheckpointJSON = &encoded
	updated.LastHeartbeatAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := r.repos.Runs.Upsert(ctx, updated); err != nil {
		return storage.RunRecord{}, err
	}
	return updated, nil
}

func (r *Runner) persistStepCompleted(ctx context.Context, run storage.RunRecord, step PlannerStep, checkpoint plannerCheckpoint) (storage.RunRecord, error) {
	updated := run
	nowISO := r.nowISO()
	if next := nextPlannerStep(step); next != "" {
		updated.CurrentStep = stringPtr(string(next))
	} else {
		updated.CurrentStep = nil
	}
	updated.LastCompletedStep = stringPtr(string(step))
	encoded := mustMarshalJSON(checkpoint)
	updated.CheckpointJSON = &encoded
	updated.LastHeartbeatAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := r.repos.Runs.Upsert(ctx, updated); err != nil {
		return storage.RunRecord{}, err
	}
	return updated, nil
}

func (r *Runner) completeRun(ctx context.Context, run storage.RunRecord, status, summary, errorMessage string, checkpoint plannerCheckpoint) (storage.RunRecord, error) {
	updated := run
	endedAt := r.nowISO()
	updated.Status = status
	if summary != "" {
		updated.Summary = stringPtr(summary)
	}
	if errorMessage != "" {
		updated.ErrorMessage = stringPtr(errorMessage)
	}
	encoded := mustMarshalJSON(checkpoint)
	updated.CheckpointJSON = &encoded
	updated.EndedAt = &endedAt
	updated.LastHeartbeatAt = &endedAt
	updated.UpdatedAt = endedAt
	if err := r.repos.Runs.Upsert(ctx, updated); err != nil {
		return storage.RunRecord{}, err
	}
	return updated, nil
}

func (r *Runner) persistCheckpoint(ctx context.Context, runID string, step PlannerStep, checkpoint plannerCheckpoint) error {
	run, err := r.repos.Runs.GetByID(ctx, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("run not found: %s", runID)
	}
	_, err = r.persistStepStarted(ctx, *run, step, checkpoint)
	return err
}

func (r *Runner) getLatestCheckpoint(ctx context.Context, run storage.RunRecord, fallback plannerCheckpoint) plannerCheckpoint {
	persisted, err := r.repos.Runs.GetByID(ctx, run.ID)
	if err != nil || persisted == nil {
		return fallback
	}
	return parseCheckpoint(persisted.CheckpointJSON)
}

type eventInput struct {
	eventType  string
	projectID  string
	loopID     string
	runID      string
	entityType string
	entityID   string
	payload    any
}

func (r *Runner) appendEvent(ctx context.Context, input eventInput) {
	if r.repos == nil || r.repos.Events == nil {
		return
	}
	_ = eventlog.Append(ctx, r.repos, eventlog.AppendInput{EventType: input.eventType, ProjectID: optionalString(input.projectID), LoopID: optionalString(input.loopID), RunID: optionalString(input.runID), EntityType: optionalString(input.entityType), EntityID: optionalString(input.entityID), ActorType: optionalString("system"), ActorID: optionalString("planner-loop"), ActorDisplayName: optionalString("planner-loop"), Payload: input.payload, CreatedAt: r.now()})
}

type loopUpsertResult struct {
	record  storage.LoopRecord
	created bool
}

func (r *Runner) ensureLoopForIssue(ctx context.Context, project storage.ProjectRecord, repo string, issue IssueSummary) (loopUpsertResult, error) {
	nowISO := r.nowISO()
	targetID := buildIssueTargetID(repo, issue.Number)
	loops, err := r.repos.Loops.List(ctx)
	if err != nil {
		return loopUpsertResult{}, err
	}
	for _, existing := range loops {
		if existing.Type == "planner" && existing.ProjectID == project.ID && existing.TargetType == "issue" && derefString(existing.TargetID) == targetID {
			pausedOrCompleted := existing.Status == "paused" || existing.Status == "completed"
			updated := existing
			updated.Repo = stringPtr(repo)
			if !pausedOrCompleted && updated.Status != "running" {
				updated.Status = "queued"
				updated.NextRunAt = &nowISO
			}
			metadataJSON, err := mergeLoopMetadataJSON(existing.MetadataJSON, map[string]any{"issueTitle": issue.Title, "issueURL": issue.URL, "issueNumber": issue.Number, "specPath": buildSpecPath(r.now(), issue.Number, issue.Title)})
			if err == nil {
				updated.MetadataJSON = stringPtr(metadataJSON)
			}
			updated.UpdatedAt = nowISO
			if err := r.repos.Loops.Upsert(ctx, updated); err != nil {
				return loopUpsertResult{}, err
			}
			return loopUpsertResult{record: updated, created: false}, nil
		}
	}
	seq, err := r.repos.Loops.AllocateSeq(ctx)
	if err != nil {
		return loopUpsertResult{}, err
	}
	meta := mustMarshalJSON(map[string]any{"issueTitle": issue.Title, "issueURL": issue.URL, "issueNumber": issue.Number, "specPath": buildSpecPath(r.now(), issue.Number, issue.Title)})
	loop := storage.LoopRecord{ID: eventlog.NewEventID("loop"), Seq: seq, ProjectID: project.ID, Type: "planner", TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "queued", MetadataJSON: &meta, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := r.repos.Loops.Upsert(ctx, loop); err != nil {
		return loopUpsertResult{}, err
	}
	r.appendEvent(ctx, eventInput{eventType: "loop.created", projectID: project.ID, loopID: loop.ID, entityType: "loop", entityID: loop.ID, payload: map[string]any{"type": "planner", "repo": repo, "issueNumber": issue.Number}})
	return loopUpsertResult{record: loop, created: true}, nil
}

type enqueueInput struct {
	ProjectID   string
	LoopID      string
	Repo        string
	IssueNumber int64
	Payload     map[string]any
}

func (r *Runner) enqueue(ctx context.Context, input enqueueInput) (storage.QueueItemRecord, error) {
	dedupeKey := buildPlannerDedupeKey(input.ProjectID, input.LoopID, input.Repo, input.IssueNumber)
	existing, err := r.repos.Queue.FindActiveByDedupe(ctx, dedupeKey)
	if err != nil {
		return storage.QueueItemRecord{}, err
	}
	if existing != nil {
		return *existing, nil
	}
	nowISO := r.nowISO()
	targetID := buildIssueTargetID(input.Repo, input.IssueNumber)
	lockKey := buildIssueLockKey(input.Repo, input.IssueNumber)
	projectID := input.ProjectID
	loopID := input.LoopID
	payload := mustMarshalJSON(input.Payload)
	queueItem := storage.QueueItemRecord{ID: eventlog.NewEventID("queue"), ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: targetID, Repo: &input.Repo, DedupeKey: dedupeKey, Priority: storage.QueuePriorityPlanner, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: r.retryMaxAttempts, LockKey: &lockKey, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := r.repos.Queue.Upsert(ctx, queueItem); err != nil {
		return storage.QueueItemRecord{}, err
	}
	return queueItem, nil
}

func (r *Runner) failQueueItem(ctx context.Context, queueItem storage.QueueItemRecord, kind QueueFailureKind, message string) (*storage.QueueItemRecord, error) {
	nextAttempts := queueItem.Attempts + 1
	nowISO := r.nowISO()
	if isRetryableFailure(kind) && nextAttempts < queueItem.MaxAttempts {
		retryAt := eventlog.FormatJavaScriptISOString(r.now().Add(backoffDelay(r.retryBaseDelay, nextAttempts)))
		if err := r.repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: queueItem.ID, AvailableAt: retryAt, Attempts: nextAttempts, ErrorMessage: optionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
			return nil, err
		}
	} else {
		if err := r.repos.Queue.Fail(ctx, storage.QueueFailInput{ID: queueItem.ID, FinishedAt: nowISO, ErrorMessage: optionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
			return nil, err
		}
	}
	updated, err := r.repos.Queue.GetByID(ctx, queueItem.ID)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (r *Runner) updateLoop(ctx context.Context, loop storage.LoopRecord, mutate func(*storage.LoopRecord)) (storage.LoopRecord, error) {
	current, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return storage.LoopRecord{}, err
	}
	if current == nil {
		return storage.LoopRecord{}, fmt.Errorf("loop not found: %s", loop.ID)
	}
	updated := *current
	mutate(&updated)
	updated.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, updated); err != nil {
		return storage.LoopRecord{}, err
	}
	return updated, nil
}

func (r *Runner) classifyFailure(err error) *loopError {
	var typed *loopError
	if errors.As(err, &typed) {
		return typed
	}
	var transient transientFailure
	if errors.As(err, &transient) && transient.Temporary() {
		return &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	return &loopError{message: err.Error(), kind: FailureNonRetryable}
}

func (r *Runner) nowISO() string { return eventlog.FormatJavaScriptISOString(r.now()) }

func stepsFrom(start PlannerStep) []PlannerStep {
	startIndex := 0
	for i, step := range plannerStepSequence {
		if step == start {
			startIndex = i
			break
		}
	}
	return plannerStepSequence[startIndex:]
}

func nextPlannerStep(step PlannerStep) PlannerStep {
	for i, candidate := range plannerStepSequence {
		if candidate == step && i+1 < len(plannerStepSequence) {
			return plannerStepSequence[i+1]
		}
	}
	return ""
}

func asPlannerStep(value string) PlannerStep {
	for _, candidate := range plannerStepSequence {
		if string(candidate) == value {
			return candidate
		}
	}
	return ""
}

func parseCheckpoint(value *string) plannerCheckpoint {
	if value == nil || *value == "" {
		return plannerCheckpoint{}
	}
	var checkpoint plannerCheckpoint
	if err := json.Unmarshal([]byte(*value), &checkpoint); err != nil {
		return plannerCheckpoint{}
	}
	return checkpoint
}

func parseJSONObject(value *string) map[string]any {
	if value == nil || *value == "" {
		return map[string]any{}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(*value), &parsed); err != nil {
		return map[string]any{}
	}
	return parsed
}

func mergeLoopMetadataJSON(current *string, updates map[string]any) (string, error) {
	parsed := parseJSONObject(current)
	for key, value := range updates {
		parsed[key] = value
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func requireIssue(checkpoint plannerCheckpoint) (*checkpointIssue, error) {
	if checkpoint.Issue == nil {
		return nil, &loopError{message: "Missing issue checkpoint for planner step", kind: FailureRetryableTransient}
	}
	return checkpoint.Issue, nil
}

func requireWorktree(checkpoint plannerCheckpoint) (*checkpointWorktree, error) {
	if checkpoint.Worktree == nil {
		return nil, &loopError{message: "Missing worktree checkpoint for planner step", kind: FailureRetryableTransient}
	}
	return checkpoint.Worktree, nil
}

func buildPlannerPrompt(project storage.ProjectRecord, issue *checkpointIssue, worktree *checkpointWorktree) string {
	parts := []string{
		fmt.Sprintf("Write a planning spec for GitHub issue %s#%d.", issue.Repo, issue.IssueNumber),
		"Repository: " + issue.Repo,
		"Base branch: " + worktree.BaseBranch,
		"Spec path: " + issue.SpecPath,
		"Issue title: " + issue.Title,
	}
	if strings.TrimSpace(issue.Body) != "" {
		parts = append(parts, "Issue body:\n"+issue.Body)
	}
	if strings.TrimSpace(issue.URL) != "" {
		parts = append(parts, "Issue URL: "+issue.URL)
	}
	if agentsBlock := readAgentsBlock(project.RepoPath); agentsBlock != "" {
		parts = append(parts, agentsBlock)
	}
	parts = append(parts, strings.Join([]string{
		"Requirements:",
		"- Create or update the spec at " + issue.SpecPath,
		"- Use Markdown with clear problem, goals, approach, risks, and validation sections",
		"- Keep the implementation scope aligned to the issue",
		"- Commit the spec changes on the current branch so the PR can be opened",
	}, "\n"))
	return agent.AppendCompletionInstruction(strings.Join(parts, "\n\n"))
}

func buildPullRequestBody(issue checkpointIssue, worktree checkpointWorktree, writeSpec *checkpointWriteSpec) string {
	lines := []string{"## Summary", fmt.Sprintf("- Adds the planning spec for %s#%d", issue.Repo, issue.IssueNumber), "- Spec path: " + issue.SpecPath, "- Planner branch: " + worktree.Branch}
	if issue.URL != "" {
		lines = append(lines, "- Source issue: "+issue.URL)
	}
	if writeSpec != nil && strings.TrimSpace(writeSpec.Summary) != "" {
		lines = append(lines, "", "## Agent Summary", writeSpec.Summary)
	}
	lines = append(lines, "", "Spec: "+issue.SpecPath, fmt.Sprintf("Issue: %s#%d", issue.Repo, issue.IssueNumber))
	return strings.Join(lines, "\n")
}

func readAgentsBlock(projectRepoPath string) string {
	content, err := os.ReadFile(filepath.Join(projectRepoPath, "AGENTS.md"))
	if err != nil {
		return ""
	}
	return "AGENTS.md:\n" + string(content)
}

func normalizeLogin(login string) string { return strings.ToLower(strings.TrimSpace(login)) }

func includesLogin(values []string, target string) bool {
	target = normalizeLogin(target)
	for _, value := range values {
		if normalizeLogin(value) == target {
			return true
		}
	}
	return false
}

func shouldClaimIssue(issue IssueSummary, login string) bool {
	return includesLogin(issue.Assignees, login) && specpr.HasLabel(issue.Labels, discoveryLabel)
}

func resolveRequestedReviewers(project storage.ProjectRecord, loop storage.LoopRecord, assignees []string, currentLogin string) []string {
	requested := make([]string, 0)
	projectMetadata := parseJSONObject(project.MetadataJSON)
	loopConfig := parseJSONObject(loop.ConfigJSON)
	for _, source := range []any{loopConfig["reviewers"], projectMetadata["reviewers"]} {
		for _, value := range toStrings(source) {
			login := normalizeLogin(value)
			if login != "" && login != normalizeLogin(currentLogin) && !stringInSlice(login, requested) {
				requested = append(requested, login)
			}
		}
	}
	for _, assignee := range assignees {
		login := normalizeLogin(assignee)
		if login != "" && login != normalizeLogin(currentLogin) && !stringInSlice(login, requested) {
			requested = append(requested, login)
		}
	}
	return requested
}

func buildIssueTargetID(repo string, issueNumber int64) string {
	return fmt.Sprintf("issue:%s:%d", repo, issueNumber)
}
func buildPlannerDedupeKey(projectID, loopID, repo string, issueNumber int64) string {
	return fmt.Sprintf("planner:%s:%s:%s:%d", projectID, loopID, repo, issueNumber)
}
func buildIssueLockKey(repo string, issueNumber int64) string {
	return fmt.Sprintf("issue:%s:%d", repo, issueNumber)
}

func parseIssueNumberFromTargetID(targetID string) int64 {
	if targetID == "" {
		return 0
	}
	parts := strings.Split(targetID, ":")
	if len(parts) != 3 || parts[0] != "issue" {
		return 0
	}
	number, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return 0
	}
	return number
}

func buildPlannerBranch(issueNumber int64, title string) string {
	return fmt.Sprintf("looper/planner/%d-%s", issueNumber, buildPlannerSlug(title))
}

func buildSpecPath(now time.Time, issueNumber int64, title string) string {
	return fmt.Sprintf("specs/%s-%d-%s.md", now.UTC().Format("2006-01-02"), issueNumber, buildPlannerSlug(title))
}

func buildPlannerSlug(title string) string {
	normalized := strings.ToLower(title)
	replaced := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, normalized)
	parts := strings.FieldsFunc(replaced, func(r rune) bool { return r == '-' })
	if len(parts) > 4 {
		parts = parts[:4]
	}
	if len(parts) == 0 {
		return "issue"
	}
	return strings.Join(parts, "-")
}

func projectRepo(project storage.ProjectRecord) string {
	meta := parseJSONObject(project.MetadataJSON)
	return stringFromAnyDefault(meta["repo"])
}

func int64FromAny(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case int:
		return int64(v)
	}
	return 0
}

func toStrings(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if direct, ok := value.([]string); ok {
			return append([]string(nil), direct...)
		}
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			result = append(result, text)
		}
	}
	return result
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func backoffDelay(base time.Duration, attempts int64) time.Duration {
	delay := base
	for i := int64(1); i < attempts; i++ {
		delay *= 2
	}
	return delay
}

func isRetryableFailure(kind QueueFailureKind) bool {
	return kind == FailureRetryableTransient || kind == FailureRetryableAfterResume
}

func wrapRetryableAfterResume(err error) error {
	if err == nil {
		return nil
	}
	return &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
}

func mustMarshalJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func stringFromAnyDefault(value any) string {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return ""
	}
	return strings.TrimSpace(text)
}

func stringInSlice(value string, values []string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func stringPtr(value string) *string { return &value }

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
