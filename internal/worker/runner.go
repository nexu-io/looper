package worker

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/powerformer/looper/internal/agent"
	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/eventlog"
	"github.com/powerformer/looper/internal/infra/shell"
	"github.com/powerformer/looper/internal/infra/specpr"
	"github.com/powerformer/looper/internal/storage"
)

const (
	stepPrepareWork     WorkerStep = "prepare-work"
	stepPrepareWorktree WorkerStep = "prepare-worktree"
	stepPlan            WorkerStep = "plan"
	stepExecute         WorkerStep = "execute"
	stepValidate        WorkerStep = "validate"
	stepOpenPR          WorkerStep = "open-pr"

	FailureRetryableTransient   QueueFailureKind = "retryable_transient"
	FailureRetryableAfterResume QueueFailureKind = "retryable_after_resume"
	FailureNonRetryable         QueueFailureKind = "non_retryable"
	FailureManualIntervention   QueueFailureKind = "manual_intervention"

	defaultAgentTimeout = 30 * time.Minute
	defaultClaimTTL     = 10 * time.Minute
	defaultRetryDelay   = 5 * time.Second
	defaultRetryMax     = 3

	workerBranchSlugMaxLength = 30
	workerBranchSlugMaxWords  = 5
	workerBranchHashLength    = 16
	workerPRDedupeLookupLimit = 1000
)

var workerStepSequence = []WorkerStep{
	stepPrepareWork,
	stepPrepareWorktree,
	stepPlan,
	stepExecute,
	stepValidate,
	stepOpenPR,
}

type WorkerStep string

type QueueFailureKind string

type PullRequestSummary struct {
	Number      int64
	URL         string
	State       string
	HeadRefName string
	BaseRefName string
}

type PullRequestDetail struct {
	Number         int64
	Title          string
	Body           string
	URL            string
	State          string
	HeadRefName    string
	BaseRefName    string
	HeadSHA        string
	ReviewRequests []string
}

type IssueDetail struct {
	Number int64
	Title  string
	Body   string
	URL    string
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

type ViewPullRequestInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type ViewIssueInput struct {
	Repo        string
	IssueNumber int64
	CWD         string
}

type ListOpenPullRequestsInput struct {
	Repo  string
	CWD   string
	Limit int
	Label string
}

type GitHubGateway interface {
	ListOpenPullRequests(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error)
	ViewPullRequest(context.Context, ViewPullRequestInput) (PullRequestDetail, error)
	ViewIssue(context.Context, ViewIssueInput) (IssueDetail, error)
	CreatePullRequest(context.Context, CreatePullRequestInput) (CreatePullRequestResult, error)
	RemovePullRequestLabels(context.Context, PullRequestLabelsInput) error
	AddPullRequestReviewers(context.Context, PullRequestReviewersInput) error
}

type CreateWorktreeInput struct {
	ProjectID         string
	RepoPath          string
	WorktreeRoot      string
	Branch            string
	BaseBranch        string
	PRNumber          int64
	ProtectedBranches []string
	CheckoutMode      string
}

type CreateWorktreeResult struct {
	WorktreePath string
	Branch       string
	BaseBranch   string
	HeadSHA      string
	WorktreeID   string
}

type PrepareWorktreeInput struct {
	WorktreePath    string
	Branch          string
	ExpectedHeadSHA string
	Remote          string
}

type PrepareWorktreeResult struct {
	HeadSHA string
	Clean   bool
}

type PushInput struct {
	WorktreePath      string
	Branch            string
	Remote            string
	ProtectedBranches []string
}

type GitGateway interface {
	CreateWorktree(context.Context, CreateWorktreeInput) (CreateWorktreeResult, error)
	PrepareWorktree(context.Context, PrepareWorktreeInput) (PrepareWorktreeResult, error)
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
	Status       string
	Summary      string
	Stdout       string
	ChangedFiles []string
	Commits      []string
}

type AgentExecution interface {
	Wait(context.Context) (AgentResult, error)
}

type AgentExecutor interface {
	Start(context.Context, AgentRunInput) (AgentExecution, error)
}

type ValidationResult struct {
	Passed  bool   `json:"passed"`
	Summary string `json:"summary,omitempty"`
	Output  string `json:"output,omitempty"`
}

type ValidationInput struct {
	CWD      string
	Commands []string
}

type ValidationRunner func(context.Context, ValidationInput) (ValidationResult, error)

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
	ValidationCommands      []string
	ValidationRunner        ValidationRunner
	AllowAutoCommit         bool
	AllowAutoPush           bool
	OpenPRStrategy          config.OpenPRStrategy
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
	validationCommands      []string
	validationRunner        ValidationRunner
	allowAutoCommit         bool
	allowAutoPush           bool
	openPRStrategy          config.OpenPRStrategy
	retryBaseDelay          time.Duration
	retryMaxAttempts        int64
	onAgentExecutionStarted AgentExecutionStartedFunc
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

type workerInput struct {
	Title         string   `json:"title,omitempty"`
	Prompt        string   `json:"prompt,omitempty"`
	SpecPath      string   `json:"specPath,omitempty"`
	Repo          string   `json:"repo,omitempty"`
	BaseBranch    string   `json:"baseBranch,omitempty"`
	ExecutionMode string   `json:"executionMode,omitempty"`
	IssueNumber   int64    `json:"issueNumber,omitempty"`
	IssueURL      string   `json:"issueUrl,omitempty"`
	PRNumber      int64    `json:"prNumber,omitempty"`
	Branch        string   `json:"branch,omitempty"`
	HeadSHA       string   `json:"headSha,omitempty"`
	Reviewers     []string `json:"reviewers,omitempty"`
}

type workerCheckpoint struct {
	ResumePolicy   string               `json:"resumePolicy,omitempty"`
	Work           *workerInput         `json:"work,omitempty"`
	ClaimedLockKey string               `json:"claimedLockKey,omitempty"`
	Worktree       *checkpointWorktree  `json:"worktree,omitempty"`
	Plan           *checkpointPlan      `json:"plan,omitempty"`
	Execution      *checkpointExecution `json:"execution,omitempty"`
	Validation     *ValidationResult    `json:"validation,omitempty"`
	PullRequest    *checkpointPullPR    `json:"pullRequest,omitempty"`
	SkipReason     string               `json:"skipReason,omitempty"`
}

type checkpointWorktree struct {
	ID         string `json:"id,omitempty"`
	Path       string `json:"path,omitempty"`
	Branch     string `json:"branch,omitempty"`
	BaseBranch string `json:"baseBranch,omitempty"`
	HeadSHA    string `json:"headSha,omitempty"`
}

type checkpointPlan struct {
	Summary string   `json:"summary,omitempty"`
	Items   []string `json:"items,omitempty"`
}

type checkpointExecution struct {
	Status       string   `json:"status,omitempty"`
	Summary      string   `json:"summary,omitempty"`
	ChangedFiles []string `json:"changedFiles,omitempty"`
	Commits      []string `json:"commits,omitempty"`
	Stdout       string   `json:"stdout,omitempty"`
}

type checkpointPullPR struct {
	Number int64  `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
}

type resumedRunContext struct {
	Run        storage.RunRecord
	StartStep  WorkerStep
	Checkpoint workerCheckpoint
	Resumed    bool
}

type stepInput struct {
	Project    storage.ProjectRecord
	Loop       storage.LoopRecord
	Run        storage.RunRecord
	QueueItem  storage.QueueItemRecord
	Checkpoint workerCheckpoint
}

type loopError struct {
	message string
	kind    QueueFailureKind
}

func (e *loopError) Error() string { return e.message }

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
	retryMaxAttempts := options.RetryMaxAttempts
	if retryMaxAttempts <= 0 {
		retryMaxAttempts = defaultRetryMax
	}
	strategy := options.OpenPRStrategy
	if strategy == "" {
		strategy = config.OpenPRStrategyManual
	}
	return &Runner{
		db:                      options.DB,
		repos:                   options.Repos,
		github:                  options.GitHub,
		git:                     options.Git,
		agentExecutor:           options.AgentExecutor,
		logger:                  options.Logger,
		now:                     now,
		agentTimeout:            agentTimeout,
		claimTTL:                claimTTL,
		validationCommands:      append([]string(nil), options.ValidationCommands...),
		validationRunner:        options.ValidationRunner,
		allowAutoCommit:         options.AllowAutoCommit,
		allowAutoPush:           options.AllowAutoPush,
		openPRStrategy:          strategy,
		retryBaseDelay:          retryBaseDelay,
		retryMaxAttempts:        retryMaxAttempts,
		onAgentExecutionStarted: options.OnAgentExecutionStarted,
	}
}

func (r *Runner) ProcessNext(ctx context.Context, claimedBy string) (*ProcessResult, error) {
	if r.repos == nil || r.repos.Queue == nil {
		return nil, fmt.Errorf("worker runner is not configured")
	}
	claimed, err := r.repos.Queue.ClaimNextOfType(ctx, r.nowISO(), claimedBy, "worker")
	if err != nil || claimed == nil {
		return nil, err
	}
	return r.ProcessClaimedQueueItem(ctx, *claimed)
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
	if queueItem.Type != "worker" {
		return ProcessResult{}, fmt.Errorf("unsupported queue item type: %s", queueItem.Type)
	}
	if queueItem.LoopID == nil {
		return ProcessResult{}, fmt.Errorf("worker queue item requires loopId")
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
	acquiredClaimedLock := false
	if resumedRun.StartStep != stepPrepareWork {
		claimedLockKey = checkpoint.ClaimedLockKey
	}
	if claimedLockKey != "" {
		nowISO := r.nowISO()
		reason := "worker-run-resume"
		acquired, err := r.repos.Locks.Acquire(ctx, storage.LockRecord{Key: claimedLockKey, Owner: queueItem.ID, Reason: &reason, ExpiresAt: eventlog.FormatJavaScriptISOString(r.now().Add(r.claimTTL)), CreatedAt: nowISO, UpdatedAt: nowISO})
		if err != nil {
			return ProcessResult{}, err
		}
		if !acquired {
			return ProcessResult{}, &loopError{message: fmt.Sprintf("Worker lock is already held for %s", claimedLockKey), kind: FailureRetryableTransient}
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

	for _, step := range stepsFrom(resumedRun.StartStep) {
		run, err = r.persistStepStarted(ctx, run, step, checkpoint)
		if err != nil {
			return ProcessResult{}, err
		}
		checkpoint, err = r.executeStep(ctx, step, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: queueItem, Checkpoint: checkpoint})
		if err != nil {
			failure := r.classifyFailure(err)
			latest := r.getLatestCheckpoint(ctx, run, checkpoint)
			switch failure.kind {
			case FailureRetryableAfterResume:
				latest.ResumePolicy = "advance_from_checkpoint"
			case FailureManualIntervention:
				latest.ResumePolicy = "manual_intervention"
			default:
				if latest.ResumePolicy == "" {
					latest.ResumePolicy = "replay_step"
				}
			}
			if _, err := r.completeRun(ctx, run, "failed", failure.message, failure.message, latest); err != nil {
				return ProcessResult{}, err
			}
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
		if step == stepPrepareWork {
			claimedLockKey = checkpoint.ClaimedLockKey
			acquiredClaimedLock = claimedLockKey != ""
		}
		run, err = r.persistStepCompleted(ctx, run, step, checkpoint)
		if err != nil {
			return ProcessResult{}, err
		}
		if checkpoint.SkipReason != "" {
			break
		}
	}

	summary := r.buildSuccessSummary(*loop, checkpoint)
	if _, err := r.completeRun(ctx, run, "success", summary, "", checkpoint); err != nil {
		return ProcessResult{}, err
	}
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
	return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: status, Summary: summary, PullRequestNumber: pullRequestNumber(checkpoint.PullRequest)}, nil
}

func (r *Runner) executeStep(ctx context.Context, step WorkerStep, input stepInput) (workerCheckpoint, error) {
	switch step {
	case stepPrepareWork:
		return r.runPrepareWorkStep(ctx, input)
	case stepPrepareWorktree:
		return r.runPrepareWorktreeStep(ctx, input)
	case stepPlan:
		return r.runPlanStep(input)
	case stepExecute:
		return r.runExecuteStep(ctx, input)
	case stepValidate:
		return r.runValidateStep(ctx, input)
	case stepOpenPR:
		return r.runOpenPRStep(ctx, input)
	default:
		return input.Checkpoint, fmt.Errorf("unsupported worker step: %s", step)
	}
}

func (r *Runner) runPrepareWorkStep(ctx context.Context, input stepInput) (workerCheckpoint, error) {
	checkpoint := input.Checkpoint
	work, err := r.resolveWorkerInput(ctx, input.Project, input.Loop, input.QueueItem, checkpoint)
	if err != nil {
		return checkpoint, err
	}
	lockKey := derefString(input.QueueItem.LockKey)
	if lockKey == "" {
		if work.ExecutionMode == "push-existing" && work.Repo != "" && work.PRNumber > 0 {
			lockKey = fmt.Sprintf("pr:%s:%d", work.Repo, work.PRNumber)
		} else {
			lockKey = fmt.Sprintf("worker:%s", input.Loop.ID)
		}
	}
	nowISO := r.nowISO()
	reason := "worker-run"
	acquired, err := r.repos.Locks.Acquire(ctx, storage.LockRecord{Key: lockKey, Owner: input.QueueItem.ID, Reason: &reason, ExpiresAt: eventlog.FormatJavaScriptISOString(r.now().Add(r.claimTTL)), CreatedAt: nowISO, UpdatedAt: nowISO})
	if err != nil {
		return checkpoint, err
	}
	if !acquired {
		return checkpoint, &loopError{message: fmt.Sprintf("Worker lock is already held for %s", lockKey), kind: FailureRetryableTransient}
	}
	if input.Loop.TargetType == "pull_request" && work.Repo != "" && work.PRNumber > 0 && r.github != nil {
		_ = r.github.RemovePullRequestLabels(ctx, PullRequestLabelsInput{Repo: work.Repo, PRNumber: work.PRNumber, Labels: []string{specpr.ReadyLabel}, CWD: input.Project.RepoPath})
	}
	checkpoint.Work = &work
	checkpoint.ClaimedLockKey = lockKey
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	checkpoint.SkipReason = ""
	return checkpoint, nil
}

func (r *Runner) runPrepareWorktreeStep(ctx context.Context, input stepInput) (workerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.Worktree != nil {
		return checkpoint, nil
	}
	work, err := requireWork(checkpoint)
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
	branch := work.Branch
	if branch == "" {
		if work.ExecutionMode == "push-existing" && work.PRNumber > 0 {
			branch = fmt.Sprintf("pr-%d", work.PRNumber)
		} else {
			branch = buildWorkerBranchName(work, input.Loop.ID)
		}
	}
	created, err := r.git.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:         input.Project.ID,
		RepoPath:          input.Project.RepoPath,
		WorktreeRoot:      worktreeRoot,
		Branch:            branch,
		BaseBranch:        work.BaseBranch,
		PRNumber:          work.PRNumber,
		ProtectedBranches: compactStrings([]string{work.BaseBranch}),
	})
	if err != nil {
		return checkpoint, err
	}
	if work.ExecutionMode == "push-existing" {
		prepared, err := r.git.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: created.WorktreePath, Branch: created.Branch, ExpectedHeadSHA: work.HeadSHA})
		if err != nil {
			return checkpoint, err
		}
		if !prepared.Clean {
			return checkpoint, &loopError{message: fmt.Sprintf("Worker worktree is dirty for branch %s", created.Branch), kind: FailureManualIntervention}
		}
		if prepared.HeadSHA != "" {
			created.HeadSHA = prepared.HeadSHA
		}
	}
	worktreeID := created.WorktreeID
	baseBranch := created.BaseBranch
	if baseBranch == "" {
		baseBranch = work.BaseBranch
	}
	metadataJSON, err := mergeLoopMetadataJSON(input.Loop.MetadataJSON, map[string]any{"worktreeId": worktreeID, "worktreePath": created.WorktreePath, "branch": created.Branch, "baseBranch": baseBranch})
	if err == nil {
		_, _ = r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) { updated.MetadataJSON = stringPtr(metadataJSON) })
	}
	checkpoint.Worktree = &checkpointWorktree{ID: worktreeID, Path: created.WorktreePath, Branch: created.Branch, BaseBranch: baseBranch, HeadSHA: created.HeadSHA}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runPlanStep(input stepInput) (workerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.Plan != nil {
		return checkpoint, nil
	}
	work, err := requireWork(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	items := compactStrings([]string{
		prefixIfPresent("Implement: ", work.Prompt),
		prefixIfPresent("Follow spec: ", work.SpecPath),
	})
	if len(items) == 0 {
		items = []string{work.Title}
	}
	checkpoint.Plan = &checkpointPlan{Summary: work.Title, Items: items}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runExecuteStep(ctx context.Context, input stepInput) (workerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.Execution != nil && checkpoint.Execution.Status == "completed" {
		return checkpoint, nil
	}
	if !r.allowAutoCommit {
		checkpoint.SkipReason = fmt.Sprintf("Auto commit disabled; manual execution required for worker %s", input.Loop.ID)
		checkpoint.ResumePolicy = "manual_intervention"
		return checkpoint, nil
	}
	work, err := requireWork(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	prompt, err := buildWorkerPrompt(worktree.Path, work, checkpoint.Plan, r.canAgentCreatePR(work))
	if err != nil {
		return checkpoint, err
	}
	executionID := eventlog.NewEventID("agent")
	execution, err := r.agentExecutor.Start(ctx, AgentRunInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Prompt: prompt, WorkingDirectory: worktree.Path, Timeout: r.agentTimeout, Metadata: map[string]any{"loopType": "worker", "title": work.Title, "repo": work.Repo, "baseBranch": work.BaseBranch}, IdempotencyKey: fmt.Sprintf("worker:%s", input.Loop.ID)})
	if err != nil {
		return checkpoint, err
	}
	if r.onAgentExecutionStarted != nil {
		_ = r.onAgentExecutionStarted(ctx, AgentExecutionStartedInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Subtitle: work.Title, Body: "Worker started", DedupeKey: fmt.Sprintf("runtime.agent.started:worker:%s", input.Run.ID)})
	}
	result, err := execution.Wait(ctx)
	if err != nil {
		return checkpoint, err
	}
	if result.Status != "completed" {
		return checkpoint, &loopError{message: firstNonEmpty(result.Summary, fmt.Sprintf("Worker agent %s", result.Status)), kind: FailureRetryableTransient}
	}
	checkpoint.Execution = &checkpointExecution{Status: result.Status, Summary: result.Summary, ChangedFiles: append([]string(nil), result.ChangedFiles...), Commits: append([]string(nil), result.Commits...), Stdout: result.Stdout}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runValidateStep(ctx context.Context, input stepInput) (workerCheckpoint, error) {
	checkpoint := input.Checkpoint
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	result, err := r.runValidation(ctx, ValidationInput{CWD: worktree.Path, Commands: r.validationCommands})
	if err != nil {
		return checkpoint, err
	}
	checkpoint.Validation = &result
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runOpenPRStep(ctx context.Context, input stepInput) (workerCheckpoint, error) {
	checkpoint := input.Checkpoint
	work, err := requireWork(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	if work.ExecutionMode == "create-pr" && (checkpoint.PullRequest != nil || input.Loop.PRNumber != nil) {
		if checkpoint.PullRequest == nil {
			checkpoint.PullRequest = &checkpointPullPR{Number: derefInt64(input.Loop.PRNumber), URL: stringFromAnyDefault(parseJSONObject(input.Loop.MetadataJSON)["prUrl"])}
		}
		return checkpoint, nil
	}
	if checkpoint.Validation != nil && !checkpoint.Validation.Passed {
		return checkpoint, &loopError{message: firstNonEmpty(checkpoint.Validation.Summary, "Validation failed"), kind: FailureManualIntervention}
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	if work.ExecutionMode == "push-existing" {
		if !r.allowAutoPush {
			checkpoint.SkipReason = fmt.Sprintf("Auto push disabled; manual PR opening required for worker %s", input.Loop.ID)
			checkpoint.ResumePolicy = "manual_intervention"
			return checkpoint, nil
		}
		if err := r.git.Push(ctx, PushInput{WorktreePath: worktree.Path, Branch: firstNonEmpty(work.Branch, worktree.Branch), ProtectedBranches: compactStrings([]string{work.BaseBranch})}); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		if len(work.Reviewers) > 0 && work.PRNumber > 0 && r.github != nil {
			_ = r.github.AddPullRequestReviewers(ctx, PullRequestReviewersInput{Repo: work.Repo, PRNumber: work.PRNumber, Reviewers: append([]string(nil), work.Reviewers...), CWD: input.Project.RepoPath})
		}
		prURL := stringFromAnyDefault(parseJSONObject(input.Loop.MetadataJSON)["prUrl"])
		checkpoint.PullRequest = &checkpointPullPR{Number: work.PRNumber, URL: prURL}
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		return checkpoint, nil
	}
	if r.openPRStrategy == config.OpenPRStrategyManual {
		checkpoint.SkipReason = fmt.Sprintf("Worker completed; PR opening is manual for %s", input.Loop.ID)
		checkpoint.ResumePolicy = "manual_intervention"
		return checkpoint, nil
	}
	if !r.allowAutoPush {
		checkpoint.SkipReason = fmt.Sprintf("Auto push disabled; manual PR opening required for worker %s", input.Loop.ID)
		checkpoint.ResumePolicy = "manual_intervention"
		return checkpoint, nil
	}
	aliases := buildWorkerBranchAliases(work, input.Loop.ID)
	if existing, err := r.findOpenPullRequestForBranch(ctx, work.Repo, aliases, work.BaseBranch, input.Project.RepoPath); err == nil && existing != nil {
		if err := r.git.Push(ctx, PushInput{WorktreePath: worktree.Path, Branch: firstNonEmpty(existing.HeadRefName, worktree.Branch), ProtectedBranches: compactStrings([]string{work.BaseBranch})}); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		_ = r.assignReviewersIfNeeded(ctx, work, existing.Number, input.Project.RepoPath)
		if err := r.persistPullRequestReference(ctx, input.Loop, work.Repo, checkpointPullPR{Number: existing.Number, URL: existing.URL}); err != nil {
			return checkpoint, err
		}
		checkpoint.PullRequest = &checkpointPullPR{Number: existing.Number, URL: existing.URL}
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		return checkpoint, nil
	}
	if err := r.git.Push(ctx, PushInput{WorktreePath: worktree.Path, Branch: worktree.Branch, ProtectedBranches: compactStrings([]string{work.BaseBranch})}); err != nil {
		return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	if existing, err := r.findOpenPullRequestForBranch(ctx, work.Repo, aliases, work.BaseBranch, input.Project.RepoPath); err == nil && existing != nil {
		_ = r.assignReviewersIfNeeded(ctx, work, existing.Number, input.Project.RepoPath)
		if err := r.persistPullRequestReference(ctx, input.Loop, work.Repo, checkpointPullPR{Number: existing.Number, URL: existing.URL}); err != nil {
			return checkpoint, err
		}
		checkpoint.PullRequest = &checkpointPullPR{Number: existing.Number, URL: existing.URL}
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		return checkpoint, nil
	}
	created, err := r.github.CreatePullRequest(ctx, CreatePullRequestInput{Repo: work.Repo, HeadBranch: worktree.Branch, BaseBranch: work.BaseBranch, Title: work.Title, Body: buildPullRequestBody(work, checkpoint.Plan, checkpoint.Execution), CWD: input.Project.RepoPath})
	if err != nil {
		return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	if created.Number <= 0 {
		return checkpoint, &loopError{message: "Worker create-pr requires a pull request number", kind: FailureRetryableAfterResume}
	}
	_ = r.assignReviewersIfNeeded(ctx, work, created.Number, input.Project.RepoPath)
	pr := checkpointPullPR{Number: created.Number, URL: created.URL}
	if err := r.persistPullRequestReference(ctx, input.Loop, work.Repo, pr); err != nil {
		return checkpoint, err
	}
	checkpoint.PullRequest = &pr
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) resolveWorkerInput(ctx context.Context, project storage.ProjectRecord, loop storage.LoopRecord, queueItem storage.QueueItemRecord, checkpoint workerCheckpoint) (workerInput, error) {
	if checkpoint.Work != nil {
		return *checkpoint.Work, nil
	}
	payload := parseJSONObject(queueItem.PayloadJSON)
	metadata := parseJSONObject(loop.MetadataJSON)
	workerMeta, _ := metadata["worker"].(map[string]any)
	source := map[string]any{}
	for k, v := range workerMeta {
		source[k] = v
	}
	for k, v := range payload {
		source[k] = v
	}
	executionMode := stringFromAnyDefault(source["executionMode"])
	if executionMode == "" {
		if loop.TargetType == "pull_request" {
			executionMode = "push-existing"
		} else {
			executionMode = "create-pr"
		}
	}
	projectMetadata := parseJSONObject(project.MetadataJSON)
	repo := firstNonEmpty(stringFromAnyDefault(source["repo"]), derefString(loop.Repo), stringFromAnyDefault(projectMetadata["repo"]))
	baseBranch := firstNonEmpty(stringFromAnyDefault(source["baseBranch"]), stringFromAnyDefault(metadata["baseBranch"]), derefString(project.BaseBranch), "main")
	work := workerInput{Title: firstNonEmpty(stringFromAnyDefault(source["title"]), "Worker run"), Prompt: stringFromAnyDefault(source["prompt"]), SpecPath: stringFromAnyDefault(source["specPath"]), Repo: repo, BaseBranch: baseBranch, ExecutionMode: executionMode, IssueNumber: int64FromAny(source["issueNumber"]), IssueURL: stringFromAnyDefault(source["issueUrl"]), PRNumber: int64FromAny(source["prNumber"]), Branch: stringFromAnyDefault(source["branch"]), HeadSHA: stringFromAnyDefault(source["headSha"]), Reviewers: stringSliceFromAny(source["reviewers"])}
	if work.Repo == "" {
		return workerInput{}, &loopError{message: "worker.repo is required", kind: FailureNonRetryable}
	}
	if work.BaseBranch == "" {
		return workerInput{}, &loopError{message: "worker.baseBranch is required", kind: FailureNonRetryable}
	}
	if work.ExecutionMode == "create-pr" && work.Prompt == "" && work.SpecPath == "" && work.IssueNumber == 0 {
		return workerInput{}, &loopError{message: "worker.prompt or worker.specPath is required", kind: FailureNonRetryable}
	}
	if work.ExecutionMode == "create-pr" && work.IssueNumber > 0 && work.Prompt == "" && work.SpecPath == "" && r.github != nil {
		issue, err := r.github.ViewIssue(ctx, ViewIssueInput{Repo: work.Repo, IssueNumber: work.IssueNumber, CWD: project.RepoPath})
		if err != nil {
			return workerInput{}, err
		}
		work = hydrateWorkerInputFromIssue(work, issue)
	}
	if loop.TargetType == "pull_request" {
		repo := firstNonEmpty(derefString(loop.Repo), work.Repo)
		prNumber := firstNonZero(derefInt64(loop.PRNumber), work.PRNumber)
		if repo == "" || prNumber == 0 {
			return workerInput{}, &loopError{message: "pull_request worker loop requires repo and prNumber", kind: FailureNonRetryable}
		}
		detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: project.RepoPath})
		if err != nil {
			return workerInput{}, err
		}
		work.Title = firstNonEmpty(detail.Title, work.Title)
		work.Repo = repo
		work.PRNumber = prNumber
		work.BaseBranch = firstNonEmpty(detail.BaseRefName, work.BaseBranch)
		work.Branch = firstNonEmpty(detail.HeadRefName, work.Branch)
		work.HeadSHA = firstNonEmpty(detail.HeadSHA, work.HeadSHA)
		work.ExecutionMode = "push-existing"
		work.SpecPath = firstNonEmpty(work.SpecPath, specpr.ParseSpecPathFromPullRequestBody(detail.Body))
		work.Reviewers = append([]string(nil), detail.ReviewRequests...)
		if work.SpecPath == "" {
			return workerInput{}, &loopError{message: fmt.Sprintf("No explicit spec path found for %s#%d", repo, prNumber), kind: FailureManualIntervention}
		}
	}
	return work, nil
}

func (r *Runner) createRunContext(ctx context.Context, loop storage.LoopRecord) (resumedRunContext, error) {
	latestRun, err := r.repos.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return resumedRunContext{}, err
	}
	checkpoint := workerCheckpoint{}
	var lastCompletedStep WorkerStep
	if latestRun != nil {
		checkpoint, err = parseCheckpoint(latestRun.CheckpointJSON)
		if err != nil {
			return resumedRunContext{}, err
		}
		lastCompletedStep = asWorkerStep(derefString(latestRun.LastCompletedStep))
		if derefString(latestRun.LastCompletedStep) != "" && lastCompletedStep == "" {
			return resumedRunContext{}, fmt.Errorf("unknown worker last completed step %q", derefString(latestRun.LastCompletedStep))
		}
	}
	startStep := stepPrepareWork
	if latestRun != nil && (latestRun.Status == "failed" || latestRun.Status == "interrupted") && lastCompletedStep != "" {
		if next := nextWorkerStep(lastCompletedStep); next != "" {
			startStep = next
		}
	}
	resumed := latestRun != nil && (latestRun.Status == "failed" || latestRun.Status == "interrupted") && startStep != stepPrepareWork
	nowISO := r.nowISO()
	encoded := mustMarshalJSON(workerCheckpoint{ResumePolicy: ternary(resumed, "advance_from_checkpoint", "replay_step"), Work: checkpoint.Work, ClaimedLockKey: checkpoint.ClaimedLockKey, Worktree: checkpoint.Worktree, Plan: checkpoint.Plan, Execution: checkpoint.Execution, Validation: checkpoint.Validation, PullRequest: checkpoint.PullRequest, SkipReason: checkpoint.SkipReason})
	run := storage.RunRecord{ID: eventlog.NewEventID("run"), LoopID: loop.ID, Status: "running", CurrentStep: stringPtr(string(startStep)), LastCompletedStep: nil, CheckpointJSON: &encoded, StartedAt: nowISO, LastHeartbeatAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if resumed && lastCompletedStep != "" {
		value := string(lastCompletedStep)
		run.LastCompletedStep = &value
	}
	if err := r.repos.Runs.Upsert(ctx, run); err != nil {
		return resumedRunContext{}, err
	}
	parsedCheckpoint, err := parseCheckpoint(run.CheckpointJSON)
	if err != nil {
		return resumedRunContext{}, err
	}
	return resumedRunContext{Run: run, StartStep: startStep, Checkpoint: parsedCheckpoint, Resumed: resumed}, nil
}

func (r *Runner) persistStepStarted(ctx context.Context, run storage.RunRecord, step WorkerStep, checkpoint workerCheckpoint) (storage.RunRecord, error) {
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

func (r *Runner) persistStepCompleted(ctx context.Context, run storage.RunRecord, step WorkerStep, checkpoint workerCheckpoint) (storage.RunRecord, error) {
	updated := run
	nowISO := r.nowISO()
	if next := nextWorkerStep(step); next != "" {
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

func (r *Runner) completeRun(ctx context.Context, run storage.RunRecord, status, summary, errorMessage string, checkpoint workerCheckpoint) (storage.RunRecord, error) {
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

func (r *Runner) getLatestCheckpoint(ctx context.Context, run storage.RunRecord, fallback workerCheckpoint) workerCheckpoint {
	persisted, err := r.repos.Runs.GetByID(ctx, run.ID)
	if err != nil || persisted == nil {
		return fallback
	}
	checkpoint, err := parseCheckpoint(persisted.CheckpointJSON)
	if err != nil {
		return fallback
	}
	return checkpoint
}

func (r *Runner) runValidation(ctx context.Context, input ValidationInput) (ValidationResult, error) {
	if r.validationRunner != nil {
		return r.validationRunner(ctx, input)
	}
	if len(input.Commands) == 0 {
		return ValidationResult{Passed: true, Summary: "No validation commands configured"}, nil
	}

	outputs := make([]string, 0, len(input.Commands)*2)
	for _, command := range input.Commands {
		result, err := shell.Run(ctx, shell.Options{Command: "/bin/sh", Args: []string{"-c", command}, CWD: input.CWD})
		if err != nil {
			output := "Unknown validation failure"
			var commandErr *shell.CommandExecutionError
			if errors.As(err, &commandErr) {
				output = strings.TrimSpace(strings.Join([]string{commandErr.Result.Stdout, commandErr.Result.Stderr}, "\n"))
			} else {
				output = err.Error()
			}
			return ValidationResult{Passed: false, Summary: fmt.Sprintf("Validation failed: %s", command), Output: output}, nil
		}
		if stdout := strings.TrimSpace(result.Stdout); stdout != "" {
			outputs = append(outputs, stdout)
		}
		if stderr := strings.TrimSpace(result.Stderr); stderr != "" {
			outputs = append(outputs, stderr)
		}
	}

	return ValidationResult{Passed: true, Summary: "Validation passed", Output: strings.Join(outputs, "\n")}, nil
}

func (r *Runner) findOpenPullRequestForBranch(ctx context.Context, repo string, branches []string, baseBranch, cwd string) (*PullRequestSummary, error) {
	if r.github == nil {
		return nil, nil
	}
	pullRequests, err := r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: workerPRDedupeLookupLimit})
	if err != nil {
		return nil, err
	}
	for _, pr := range pullRequests {
		if strings.EqualFold(pr.State, "open") && containsString(branches, pr.HeadRefName) && pr.BaseRefName == baseBranch {
			candidate := pr
			return &candidate, nil
		}
	}
	return nil, nil
}

func (r *Runner) assignReviewersIfNeeded(ctx context.Context, work workerInput, prNumber int64, cwd string) error {
	if r.github == nil || prNumber == 0 || len(work.Reviewers) == 0 {
		return nil
	}
	return r.github.AddPullRequestReviewers(ctx, PullRequestReviewersInput{Repo: work.Repo, PRNumber: prNumber, Reviewers: append([]string(nil), work.Reviewers...), CWD: cwd})
}

func (r *Runner) persistPullRequestReference(ctx context.Context, loop storage.LoopRecord, repo string, pr checkpointPullPR) error {
	metadataJSON, err := mergeLoopMetadataJSON(loop.MetadataJSON, map[string]any{"prUrl": pr.URL, "prNumber": pr.Number, "repo": repo})
	if err != nil {
		return err
	}
	_, err = r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		updated.Repo = stringPtr(repo)
		if updated.TargetType == "pull_request" {
			updated.PRNumber = int64Ptr(pr.Number)
		}
		updated.MetadataJSON = stringPtr(metadataJSON)
	})
	return err
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
	return r.repos.Queue.GetByID(ctx, queueItem.ID)
}

func (r *Runner) updateLoop(ctx context.Context, loop storage.LoopRecord, mutate func(*storage.LoopRecord)) (storage.LoopRecord, error) {
	current, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return storage.LoopRecord{}, err
	}
	updated := loop
	if current != nil {
		updated = *current
	}
	mutate(&updated)
	updated.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, updated); err != nil {
		return storage.LoopRecord{}, err
	}
	return updated, nil
}

func (r *Runner) buildSuccessSummary(loop storage.LoopRecord, checkpoint workerCheckpoint) string {
	if checkpoint.SkipReason != "" {
		return checkpoint.SkipReason
	}
	if checkpoint.PullRequest != nil && checkpoint.PullRequest.URL != "" {
		return fmt.Sprintf("Opened pull request for worker %s: %s", loop.ID, checkpoint.PullRequest.URL)
	}
	return fmt.Sprintf("Completed worker %s", loop.ID)
}

func (r *Runner) canAgentCreatePR(work workerInput) bool {
	return work.ExecutionMode == "create-pr" && r.openPRStrategy != config.OpenPRStrategyManual && r.allowAutoPush && len(r.validationCommands) == 0 && r.validationRunner == nil
}

func (r *Runner) classifyFailure(err error) *loopError {
	var typed *loopError
	if errors.As(err, &typed) {
		return typed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	return &loopError{message: err.Error(), kind: FailureNonRetryable}
}

func (r *Runner) nowISO() string { return eventlog.FormatJavaScriptISOString(r.now()) }

func stepsFrom(start WorkerStep) []WorkerStep {
	startIndex := 0
	for i, step := range workerStepSequence {
		if step == start {
			startIndex = i
			break
		}
	}
	return workerStepSequence[startIndex:]
}

func nextWorkerStep(step WorkerStep) WorkerStep {
	for i, candidate := range workerStepSequence {
		if candidate == step && i+1 < len(workerStepSequence) {
			return workerStepSequence[i+1]
		}
	}
	return ""
}

func asWorkerStep(value string) WorkerStep {
	for _, candidate := range workerStepSequence {
		if string(candidate) == value {
			return candidate
		}
	}
	return ""
}

func parseCheckpoint(value *string) (workerCheckpoint, error) {
	if value == nil || *value == "" {
		return workerCheckpoint{}, nil
	}
	var checkpoint workerCheckpoint
	if err := json.Unmarshal([]byte(*value), &checkpoint); err != nil {
		return workerCheckpoint{}, fmt.Errorf("parse worker checkpoint: %w", err)
	}
	return checkpoint, nil
}

func requireWork(checkpoint workerCheckpoint) (workerInput, error) {
	if checkpoint.Work == nil {
		return workerInput{}, &loopError{message: "missing worker input checkpoint", kind: FailureRetryableTransient}
	}
	return *checkpoint.Work, nil
}

func requireWorktree(checkpoint workerCheckpoint) (checkpointWorktree, error) {
	if checkpoint.Worktree == nil {
		return checkpointWorktree{}, &loopError{message: "missing worker worktree checkpoint", kind: FailureRetryableTransient}
	}
	return *checkpoint.Worktree, nil
}

func buildWorkerPrompt(repoRootPath string, work workerInput, plan *checkpointPlan, allowAgentPRCreation bool) (string, error) {
	parts := []string{}
	if work.ExecutionMode == "push-existing" {
		parts = append(parts, fmt.Sprintf("Continue implementing on existing pull request %s#%d.", work.Repo, work.PRNumber))
	} else {
		parts = append(parts, fmt.Sprintf("Create a pull request for: %s", work.Title))
	}
	if work.Prompt != "" {
		parts = append(parts, "User prompt:\n"+work.Prompt)
	}
	parts = append(parts, fmt.Sprintf("Repository: %s", work.Repo), fmt.Sprintf("Base branch: %s", work.BaseBranch))
	if work.ExecutionMode == "push-existing" && work.SpecPath != "" {
		parts = append(parts, fmt.Sprintf("Do not modify the spec file at %s.", work.SpecPath))
	}
	if specBlock, err := readSpecBlock(repoRootPath, work.SpecPath); err == nil && specBlock != "" {
		parts = append(parts, specBlock)
	}
	if plan != nil && len(plan.Items) > 0 {
		lines := []string{"Execution plan:"}
		for _, item := range plan.Items {
			lines = append(lines, "- "+item)
		}
		parts = append(parts, strings.Join(lines, "\n"))
	}
	if allowAgentPRCreation {
		parts = append(parts, buildAgentPullRequestInstruction(work))
		parts = append(parts, "Make the necessary code changes, validate them, and ensure the branch and pull request are left in a consistent state.")
	} else {
		parts = append(parts, "Make the necessary code changes, validate them, and leave the branch ready for PR creation.")
	}
	return agent.AppendCompletionInstruction(strings.Join(parts, "\n\n")), nil
}

func buildAgentPullRequestInstruction(work workerInput) string {
	parts := []string{
		"When the implementation is ready and validation passes, use the GitHub CLI (`gh`) to create the pull request yourself.",
		"Before creating a PR, check whether one already exists for the current branch and avoid duplicates.",
		"Write a concise, accurate PR title and a structured body that explains the actual changes and why they were made.",
		fmt.Sprintf("Target base branch: %s.", work.BaseBranch),
	}
	if work.IssueNumber > 0 {
		parts = append(parts, fmt.Sprintf("Include `Closes #%d` in the PR body.", work.IssueNumber))
	}
	return strings.Join(parts, "\n")
}

func readSpecBlock(projectRepoPath, specPath string) (string, error) {
	if specPath == "" {
		return "", nil
	}
	resolved := specPath
	if !filepath.IsAbs(specPath) {
		resolved = filepath.Join(projectRepoPath, specPath)
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		return fmt.Sprintf("Spec path: %s", specPath), nil
	}
	return fmt.Sprintf("Spec (%s):\n%s", specPath, string(content)), nil
}

func buildPullRequestBody(work workerInput, plan *checkpointPlan, execution *checkpointExecution) string {
	lines := []string{"## Summary"}
	if plan != nil {
		for _, item := range plan.Items {
			lines = append(lines, "- "+item)
		}
	}
	if execution != nil && execution.Summary != "" {
		lines = append(lines, "", "## Agent Summary", execution.Summary)
	}
	if work.IssueNumber > 0 {
		lines = append(lines, "", fmt.Sprintf("Issue: %s#%d", work.Repo, work.IssueNumber))
	}
	if work.IssueURL != "" {
		lines = append(lines, fmt.Sprintf("Issue URL: %s", work.IssueURL))
	}
	if work.SpecPath != "" {
		lines = append(lines, "", fmt.Sprintf("Spec: %s", work.SpecPath))
	}
	if work.Prompt != "" {
		lines = append(lines, "", fmt.Sprintf("Prompt: %s", work.Prompt))
	}
	if work.IssueNumber > 0 {
		lines = append(lines, "", fmt.Sprintf("Closes #%d", work.IssueNumber))
	}
	return strings.Join(lines, "\n")
}

func hydrateWorkerInputFromIssue(work workerInput, issue IssueDetail) workerInput {
	fallbackTitle := buildDefaultIssueWorkerTitle(work.Repo, issue.Number)
	if work.Title == "Worker run" || work.Title == fallbackTitle {
		work.Title = issue.Title
	}
	work.Prompt = buildIssuePrompt(work.Repo, issue)
	work.IssueURL = issue.URL
	return work
}

func buildIssuePrompt(repo string, issue IssueDetail) string {
	parts := []string{fmt.Sprintf("Implement GitHub issue %s#%d: %s", repo, issue.Number, issue.Title)}
	if issue.Body != "" {
		parts = append(parts, "Issue body:\n"+issue.Body)
	}
	if issue.URL != "" {
		parts = append(parts, "Issue URL: "+issue.URL)
	}
	return strings.Join(parts, "\n\n")
}

func buildWorkerBranchName(work workerInput, loopID string) string {
	loopHash := buildWorkerLoopHash(loopID)
	if work.IssueNumber > 0 {
		return fmt.Sprintf("looper/%d-%s-%s", work.IssueNumber, buildWorkerSlug(work.Title), loopHash)
	}
	return "looper/" + loopHash
}

func buildWorkerBranchAliases(work workerInput, loopID string) []string {
	branch := buildWorkerBranchName(work, loopID)
	legacy := strings.Replace(branch, "looper/", "looper/worker/", 1)
	if legacy == branch {
		return []string{branch}
	}
	return []string{branch, legacy}
}

func buildWorkerSlug(title string) string {
	words := strings.Split(slugify(title), "-")
	filtered := []string{}
	for _, word := range words {
		if word != "" {
			filtered = append(filtered, word)
		}
	}
	if len(filtered) > workerBranchSlugMaxWords {
		filtered = filtered[:workerBranchSlugMaxWords]
	}
	value := strings.TrimRight(strings.Join(filtered, "-"), "-")
	if len(value) > workerBranchSlugMaxLength {
		value = strings.TrimRight(value[:workerBranchSlugMaxLength], "-")
	}
	if value == "" {
		return "update"
	}
	return value
}

func buildWorkerLoopHash(loopID string) string {
	sum := sha1.Sum([]byte(loopID))
	value := hex.EncodeToString(sum[:])
	if len(value) > workerBranchHashLength {
		value = value[:workerBranchHashLength]
	}
	if value == "" {
		return "worker"
	}
	return value
}

func buildDefaultIssueWorkerTitle(repo string, issueNumber int64) string {
	return fmt.Sprintf("Implement %s#%d", repo, issueNumber)
}

func mergeLoopMetadataJSON(current *string, updates map[string]any) (string, error) {
	metadata := parseJSONObject(current)
	for key, value := range updates {
		metadata[key] = value
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func parseJSONObject(raw *string) map[string]any {
	if raw == nil || *raw == "" {
		return map[string]any{}
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(*raw), &decoded); err != nil {
		return map[string]any{}
	}
	return decoded
}

func mustMarshalJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func slugify(value string) string {
	value = strings.ToLower(value)
	parts := make([]rune, 0, len(value))
	dash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			parts = append(parts, r)
			dash = false
			continue
		}
		if !dash {
			parts = append(parts, '-')
			dash = true
		}
	}
	return strings.Trim(string(parts), "-")
}

func backoffDelay(base time.Duration, attempts int64) time.Duration {
	if attempts <= 0 {
		attempts = 1
	}
	return time.Duration(attempts) * base
}

func isRetryableFailure(kind QueueFailureKind) bool {
	return kind == FailureRetryableTransient || kind == FailureRetryableAfterResume
}

func compactStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, strings.TrimSpace(value))
		}
	}
	return result
}

func stringSliceFromAny(value any) []string {
	array, ok := value.([]any)
	if !ok {
		stringsValue, ok := value.([]string)
		if ok {
			return append([]string(nil), stringsValue...)
		}
		return nil
	}
	result := make([]string, 0, len(array))
	for _, item := range array {
		if text, ok := item.(string); ok && text != "" {
			result = append(result, text)
		}
	}
	return result
}

func int64FromAny(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	default:
		return 0
	}
}

func stringFromAnyDefault(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func prefixIfPresent(prefix, value string) string {
	if value == "" {
		return ""
	}
	return prefix + value
}

func pullRequestNumber(pr *checkpointPullPR) int64 {
	if pr == nil {
		return 0
	}
	return pr.Number
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func stringPtr(value string) *string { return &value }

func int64Ptr(value int64) *int64 { return &value }

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func ternary[T any](condition bool, whenTrue, whenFalse T) T {
	if condition {
		return whenTrue
	}
	return whenFalse
}
