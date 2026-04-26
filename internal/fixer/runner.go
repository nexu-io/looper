package fixer

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	stepDiscoverPR       FixerStep = "discover-pr"
	stepClaimPR          FixerStep = "claim-pr"
	stepCollectFixes     FixerStep = "collect-fixes"
	stepPrepareWorktree  FixerStep = "prepare-worktree"
	stepRepair           FixerStep = "repair"
	stepReconcileCommits FixerStep = "reconcile-commits"
	stepValidate         FixerStep = "validate"
	stepPush             FixerStep = "push"
	stepResolveComments  FixerStep = "resolve-comments"
	stepRecheck          FixerStep = "recheck"
)

var fixerStepSequence = []FixerStep{
	stepDiscoverPR,
	stepClaimPR,
	stepCollectFixes,
	stepPrepareWorktree,
	stepRepair,
	stepReconcileCommits,
	stepValidate,
	stepPush,
	stepResolveComments,
	stepRecheck,
}

type FixerStep string

type QueueFailureKind string

const (
	FailureRetryableTransient   QueueFailureKind = "retryable_transient"
	FailureRetryableAfterResume QueueFailureKind = "retryable_after_resume"
	FailureNonRetryable         QueueFailureKind = "non_retryable"
	FailureManualIntervention   QueueFailureKind = "manual_intervention"

	defaultAgentTimeout = 30 * time.Minute
	defaultClaimTTL     = 5 * time.Minute
	defaultRetryDelay   = 5 * time.Second
	defaultRetryMax     = 3
)

type FixItem struct {
	Type     string   `json:"type"`
	ID       string   `json:"id,omitempty"`
	ThreadID string   `json:"threadId,omitempty"`
	Name     string   `json:"name,omitempty"`
	Summary  string   `json:"summary,omitempty"`
	Files    []string `json:"files,omitempty"`
}

type PullRequestSummary struct {
	Number  int64
	State   string
	IsDraft bool
	HeadSHA string
}

type PullRequestDetail struct {
	Number         int64
	State          string
	IsDraft        bool
	Labels         []string
	HeadSHA        string
	HeadRefName    string
	BaseRefName    string
	BaseSHA        string
	ReviewDecision string
	Comments       []map[string]any
	Checks         []map[string]any
	HasConflicts   bool
}

type ListOpenPullRequestsInput struct {
	Repo  string
	CWD   string
	Limit int
}

type ViewPullRequestInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type ResolveReviewThreadInput struct {
	Repo     string
	ThreadID string
	CWD      string
}

type PullRequestLabelsInput struct {
	Repo     string
	PRNumber int64
	Labels   []string
	CWD      string
}

type GitHubGateway interface {
	ListOpenPullRequests(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error)
	ViewPullRequest(context.Context, ViewPullRequestInput) (PullRequestDetail, error)
	ResolveReviewThread(context.Context, ResolveReviewThreadInput) error
	AddPullRequestLabels(context.Context, PullRequestLabelsInput) error
	RemovePullRequestLabels(context.Context, PullRequestLabelsInput) error
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
	HeadSHA      string
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

type InspectHeadInput struct {
	WorktreePath string
	BaseRef      string
}

type InspectHeadResult struct {
	HeadSHA               string
	NewCommitSHAs         []string
	HasUncommittedChanges bool
	ChangedFiles          []string
}

type CommitInput struct {
	WorktreePath string
	Message      string
}

type CommitResult struct{ CommitSHA string }

type PushInput struct {
	WorktreePath          string
	Branch                string
	Remote                string
	ExpectedRemoteHeadSHA string
	ProtectedBranches     []string
}

type CleanupWorktreeInput struct {
	ProjectID         string
	RepoPath          string
	WorktreePath      string
	Branch            string
	ProtectedBranches []string
}

type GitGateway interface {
	CreateWorktree(context.Context, CreateWorktreeInput) (CreateWorktreeResult, error)
	PrepareWorktree(context.Context, PrepareWorktreeInput) (PrepareWorktreeResult, error)
	InspectHead(context.Context, InspectHeadInput) (InspectHeadResult, error)
	Commit(context.Context, CommitInput) (CommitResult, error)
	Push(context.Context, PushInput) error
	CleanupWorktree(context.Context, CleanupWorktreeInput) error
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
	Status      string
	Summary     string
	Stdout      string
	Stderr      string
	ParseStatus string
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

type ValidationRunner func(context.Context, ValidationInput) (ValidationResult, error)

type ValidationInput struct {
	CWD      string
	Commands []string
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
	ValidationCommands      []string
	ValidationRunner        ValidationRunner
	AllowAutoCommit         bool
	AllowAutoPush           bool
	AllowRiskyFixes         bool
	Sleep                   func(time.Duration)
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
	allowRiskyFixes         bool
	sleep                   func(time.Duration)
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
	LoopID      string
	RunID       string
	QueueItemID string
	Status      string
	Summary     string
	FailureKind QueueFailureKind
}

type fixerCheckpoint struct {
	ResumePolicy     string                      `json:"resumePolicy,omitempty"`
	Detail           *checkpointDetail           `json:"detail,omitempty"`
	ClaimedLockKey   string                      `json:"claimedLockKey,omitempty"`
	FixItems         []FixItem                   `json:"fixItems,omitempty"`
	FixItemsHash     string                      `json:"fixItemsHash,omitempty"`
	Worktree         *checkpointWorktree         `json:"worktree,omitempty"`
	Repair           *checkpointRepair           `json:"repair,omitempty"`
	ReconcileCommits *checkpointReconcileCommits `json:"reconcileCommits,omitempty"`
	Validation       *ValidationResult           `json:"validation,omitempty"`
	Push             *checkpointPush             `json:"push,omitempty"`
	ResolvedComments *checkpointResolvedComments `json:"resolvedComments,omitempty"`
	Recheck          *checkpointRecheck          `json:"recheck,omitempty"`
	SkipReason       string                      `json:"skipReason,omitempty"`
}

type checkpointDetail struct {
	State          string           `json:"state,omitempty"`
	IsDraft        bool             `json:"isDraft,omitempty"`
	Labels         []string         `json:"labels,omitempty"`
	HeadSHA        string           `json:"headSha,omitempty"`
	HeadRefName    string           `json:"headRefName,omitempty"`
	BaseRefName    string           `json:"baseRefName,omitempty"`
	BaseSHA        string           `json:"baseSha,omitempty"`
	ReviewDecision string           `json:"reviewDecision,omitempty"`
	Comments       []map[string]any `json:"comments,omitempty"`
	Checks         []map[string]any `json:"checks,omitempty"`
	HasConflicts   bool             `json:"hasConflicts,omitempty"`
}

type checkpointWorktree struct {
	Path               string `json:"path,omitempty"`
	Branch             string `json:"branch,omitempty"`
	HeadSHA            string `json:"headSha,omitempty"`
	BaseHeadSHA        string `json:"baseHeadSha,omitempty"`
	PreparedAt         string `json:"preparedAt,omitempty"`
	CleanupAttemptedAt string `json:"cleanupAttemptedAt,omitempty"`
	CleanedAt          string `json:"cleanedAt,omitempty"`
}

type checkpointRepair struct {
	AgentExecutionID string `json:"agentExecutionId,omitempty"`
	Summary          string `json:"summary,omitempty"`
	HeadSHA          string `json:"headSha,omitempty"`
	ParseStatus      string `json:"parseStatus,omitempty"`
	CompletedAt      string `json:"completedAt,omitempty"`
}

type checkpointReconcileCommits struct {
	BaseHeadSHA      string   `json:"baseHeadSha,omitempty"`
	FinalHeadSHA     string   `json:"finalHeadSha,omitempty"`
	NewCommitSHAs    []string `json:"newCommitShas,omitempty"`
	CommittedByAgent bool     `json:"committedByAgent,omitempty"`
	CommittedByLoop  bool     `json:"committedByLooperd,omitempty"`
	WorkingTreeClean bool     `json:"workingTreeClean,omitempty"`
	ChangedFiles     []string `json:"changedFiles,omitempty"`
	CompletedAt      string   `json:"completedAt,omitempty"`
}

type checkpointPush struct {
	Pushed        bool   `json:"pushed"`
	Branch        string `json:"branch,omitempty"`
	Remote        string `json:"remote,omitempty"`
	PushedAt      string `json:"pushedAt,omitempty"`
	SkippedReason string `json:"skippedReason,omitempty"`
}

type checkpointResolvedComments struct {
	Items []checkpointResolvedComment `json:"items,omitempty"`
}

type checkpointResolvedComment struct {
	FixItemID string `json:"fixItemId,omitempty"`
	ThreadID  string `json:"threadId,omitempty"`
	Status    string `json:"status,omitempty"`
	Message   string `json:"message,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type checkpointRecheck struct {
	RemainingFixItems []FixItem `json:"remainingFixItems,omitempty"`
}

type resumedRunContext struct {
	Run        storage.RunRecord
	StartStep  FixerStep
	Checkpoint fixerCheckpoint
	Resumed    bool
}

type stepInput struct {
	Project    storage.ProjectRecord
	Loop       storage.LoopRecord
	Run        storage.RunRecord
	QueueItem  storage.QueueItemRecord
	Repo       string
	PRNumber   int64
	Checkpoint fixerCheckpoint
}

type loopError struct {
	message string
	kind    QueueFailureKind
}

func validateCompletedRepairCheckpoint(repair *checkpointRepair) error {
	if repair == nil {
		return nil
	}
	if repair.ParseStatus == "parsed" {
		return nil
	}
	return &loopError{
		message: firstNonEmpty(repair.Summary, fmt.Sprintf("Fixer agent completed without valid structured result (parse status: %s)", firstNonEmpty(repair.ParseStatus, "missing"))),
		kind:    FailureRetryableTransient,
	}
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
	retryMax := options.RetryMaxAttempts
	if retryMax <= 0 {
		retryMax = defaultRetryMax
	}
	sleep := options.Sleep
	if sleep == nil {
		sleep = time.Sleep
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
		allowRiskyFixes:         options.AllowRiskyFixes,
		sleep:                   sleep,
		retryBaseDelay:          retryBaseDelay,
		retryMaxAttempts:        retryMax,
		onAgentExecutionStarted: options.OnAgentExecutionStarted,
	}
}

func (r *Runner) DiscoverPullRequests(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	if r.repos == nil || r.repos.Projects == nil || r.repos.Loops == nil || r.repos.Queue == nil || r.repos.Runs == nil || r.repos.Locks == nil {
		return DiscoveryResult{}, fmt.Errorf("fixer repositories are not configured")
	}
	project, err := r.repos.Projects.GetByID(ctx, input.ProjectID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	if project == nil {
		return DiscoveryResult{}, fmt.Errorf("project not found: %s", input.ProjectID)
	}
	openPRs, err := r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: input.Repo, CWD: project.RepoPath, Limit: input.Limit})
	if err != nil {
		return DiscoveryResult{}, err
	}
	result := DiscoveryResult{}
	for _, pr := range openPRs {
		if pr.IsDraft || normalizePRState(pr.State) != "open" || r.hasActivePRLock(ctx, input.Repo, pr.Number) {
			result.Skipped++
			continue
		}
		detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: pr.Number, CWD: project.RepoPath})
		if err != nil {
			return DiscoveryResult{}, err
		}
		fixItems := collectFixItems(detail)
		if len(fixItems) == 0 {
			result.Skipped++
			continue
		}
		loopResult, err := r.ensureLoopForPullRequest(ctx, *project, input.Repo, pr.Number)
		if err != nil {
			return DiscoveryResult{}, err
		}
		if loopResult.record.Status == "paused" {
			result.Skipped++
			continue
		}
		if loopResult.created {
			result.CreatedLoopIDs = append(result.CreatedLoopIDs, loopResult.record.ID)
		}
		headSHA := detail.HeadSHA
		if headSHA == "" {
			headSHA = "unknown"
		}
		queueItem, err := r.enqueue(ctx, enqueueInput{
			ProjectID:    project.ID,
			LoopID:       loopResult.record.ID,
			Repo:         input.Repo,
			PRNumber:     pr.Number,
			HeadSHA:      headSHA,
			FixItemsHash: hashFixItems(fixItems),
		})
		if err != nil {
			return DiscoveryResult{}, err
		}
		result.QueueItems = append(result.QueueItems, queueItem)
	}
	return result, nil
}

func (r *Runner) ProcessNext(ctx context.Context, claimedBy string) (*ProcessResult, error) {
	if r.repos == nil || r.repos.Queue == nil {
		return nil, fmt.Errorf("fixer queue repository is not configured")
	}
	item, err := r.repos.Queue.ClaimNextOfType(ctx, r.nowISO(), claimedBy, "fixer")
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
		if failedQueue != nil && failedQueue.Status == "queued" {
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
	if queueItem.Type != "fixer" {
		return ProcessResult{}, fmt.Errorf("unsupported queue item type: %s", queueItem.Type)
	}
	if queueItem.LoopID == nil || queueItem.Repo == nil || queueItem.PRNumber == nil {
		return ProcessResult{}, fmt.Errorf("fixer queue item requires loopId, repo, and prNumber")
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
	if resumedRun.Resumed && resumedRun.StartStep != stepClaimPR {
		claimedLockKey = checkpoint.ClaimedLockKey
		if claimedLockKey == "" {
			claimedLockKey = derefString(queueItem.LockKey)
			if claimedLockKey == "" {
				claimedLockKey = buildPullRequestLockKey(queueItem)
			}
		}
		if claimedLockKey != "" {
			nowISO := r.nowISO()
			reason := "fixer-run-resume"
			acquired, err := r.repos.Locks.Acquire(ctx, storage.LockRecord{Key: claimedLockKey, Owner: queueItem.ID, Reason: &reason, ExpiresAt: eventlog.FormatJavaScriptISOString(r.now().Add(r.claimTTL)), CreatedAt: nowISO, UpdatedAt: nowISO})
			if err != nil {
				return ProcessResult{}, err
			}
			if !acquired {
				return ProcessResult{}, &loopError{message: fmt.Sprintf("Pull request lock is already held for %s", claimedLockKey), kind: FailureRetryableTransient}
			}
			acquiredClaimedLock = true
		}
	}
	defer func() {
		if (acquiredClaimedLock || claimedLockKey != "") && claimedLockKey != "" {
			if err := r.repos.Locks.Release(context.Background(), claimedLockKey); err != nil {
				r.logWarn("fixer business lock release failed", map[string]any{"lockKey": claimedLockKey, "error": err.Error()})
			}
		}
	}()
	if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
		updated.Status = "running"
		updated.LastRunAt = stringPtr(run.StartedAt)
		updated.NextRunAt = nil
	}); err != nil {
		return ProcessResult{}, err
	}
	if err := validateFixerResumeCheckpoint(resumedRun.StartStep, checkpoint); err != nil {
		failure := r.classifyFailure(err)
		latest := r.getLatestCheckpoint(ctx, run, checkpoint)
		if latest.ResumePolicy == "" {
			latest.ResumePolicy = "replay_step"
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
			if failedQueue != nil && failedQueue.Status == "queued" {
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
		if failedQueue == nil || failedQueue.Status != "queued" {
			r.cleanupFixerWorktreeIfTerminal(context.Background(), *project, &latest)
		}
		return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: "failed", Summary: failure.message, FailureKind: failure.kind}, nil
	}
	r.appendEvent(ctx, eventInput{eventType: "loop.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "loop", entityID: loop.ID, payload: map[string]any{"queueItemId": queueItem.ID, "resumed": resumedRun.Resumed, "startStep": string(resumedRun.StartStep)}})
	r.appendEvent(ctx, eventInput{eventType: "run.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"queueItemId": queueItem.ID, "currentStep": string(resumedRun.StartStep)}})
	r.logInfo("fixer loop started", map[string]any{"projectId": project.ID, "loopId": loop.ID, "runId": run.ID, "queueItemId": queueItem.ID, "currentStep": string(resumedRun.StartStep), "resumed": resumedRun.Resumed})
	r.logInfo("fixer run started", map[string]any{"projectId": project.ID, "loopId": loop.ID, "runId": run.ID, "queueItemId": queueItem.ID, "currentStep": string(resumedRun.StartStep)})
	for _, step := range stepsFrom(resumedRun.StartStep) {
		run, err = r.persistStepStarted(ctx, run, step, checkpoint)
		if err != nil {
			return ProcessResult{}, err
		}
		r.appendEvent(ctx, eventInput{eventType: "loop.step.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"step": string(step)}})
		r.logInfo("fixer step started", map[string]any{"projectId": project.ID, "loopId": loop.ID, "runId": run.ID, "queueItemId": queueItem.ID, "currentStep": string(step)})
		checkpoint, err = r.executeStep(ctx, step, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: queueItem, Repo: *queueItem.Repo, PRNumber: *queueItem.PRNumber, Checkpoint: checkpoint})
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
			r.logError("fixer run failed", map[string]any{"projectId": project.ID, "loopId": loop.ID, "runId": run.ID, "queueItemId": queueItem.ID, "currentStep": derefString(run.CurrentStep), "failureKind": string(failure.kind), "summary": failure.message})
			failedQueue, err := r.failQueueItem(ctx, queueItem, failure.kind, failure.message)
			if err != nil {
				return ProcessResult{}, err
			}
			if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
				updated.LastRunAt = stringPtr(r.nowISO())
				if failedQueue != nil && failedQueue.Status == "queued" {
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
			if failedQueue == nil || failedQueue.Status != "queued" {
				r.cleanupFixerWorktreeIfTerminal(context.Background(), *project, &latest)
			}
			return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: "failed", Summary: failure.message, FailureKind: failure.kind}, nil
		}
		if step == stepClaimPR {
			claimedLockKey = checkpoint.ClaimedLockKey
			acquiredClaimedLock = claimedLockKey != ""
		}
		run, err = r.persistStepCompleted(ctx, run, step, checkpoint)
		if err != nil {
			return ProcessResult{}, err
		}
		r.appendEvent(ctx, eventInput{eventType: "loop.step.completed", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"step": string(step)}})
		r.logInfo("fixer step completed", map[string]any{"projectId": project.ID, "loopId": loop.ID, "runId": run.ID, "queueItemId": queueItem.ID, "currentStep": string(step)})
		if checkpoint.SkipReason != "" {
			break
		}
	}
	summary := checkpoint.SkipReason
	if summary == "" {
		summary = fmt.Sprintf("Applied fixer run for %s#%d", *queueItem.Repo, *queueItem.PRNumber)
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
	r.cleanupFixerWorktreeIfTerminal(context.Background(), *project, &checkpoint)
	status := "success"
	if checkpoint.SkipReason != "" {
		status = "skipped"
	}
	return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: status, Summary: summary}, nil
}

func (r *Runner) executeStep(ctx context.Context, step FixerStep, input stepInput) (fixerCheckpoint, error) {
	switch step {
	case stepDiscoverPR:
		return r.runDiscoverPRStep(ctx, input)
	case stepClaimPR:
		return r.runClaimPRStep(ctx, input)
	case stepCollectFixes:
		return r.runCollectFixesStep(input)
	case stepPrepareWorktree:
		return r.runPrepareWorktreeStep(ctx, input)
	case stepRepair:
		return r.runRepairStep(ctx, input)
	case stepReconcileCommits:
		return r.runReconcileCommitsStep(ctx, input)
	case stepValidate:
		return r.runValidateStep(ctx, input)
	case stepPush:
		return r.runPushStep(ctx, input)
	case stepResolveComments:
		return r.runResolveCommentsStep(ctx, input)
	case stepRecheck:
		return r.runRecheckStep(ctx, input)
	default:
		return input.Checkpoint, fmt.Errorf("unsupported fixer step: %s", step)
	}
}

func (r *Runner) runDiscoverPRStep(ctx context.Context, input stepInput) (fixerCheckpoint, error) {
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return input.Checkpoint, err
	}
	checkpoint := input.Checkpoint
	checkpoint.Detail = &checkpointDetail{State: detail.State, IsDraft: detail.IsDraft, Labels: cloneStrings(detail.Labels), HeadSHA: detail.HeadSHA, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, BaseSHA: detail.BaseSHA, ReviewDecision: detail.ReviewDecision, Comments: cloneObjectSlice(detail.Comments), Checks: cloneObjectSlice(detail.Checks), HasConflicts: detail.HasConflicts}
	checkpoint.ResumePolicy = "replay_step"
	return checkpoint, nil
}

func (r *Runner) runClaimPRStep(ctx context.Context, input stepInput) (fixerCheckpoint, error) {
	lockKey := derefString(input.QueueItem.LockKey)
	if lockKey == "" {
		lockKey = buildPullRequestLockKey(input.QueueItem)
	}
	if lockKey == "" {
		return input.Checkpoint, fmt.Errorf("fixer queue item lock key is required")
	}
	nowISO := r.nowISO()
	reason := "fixer-claim"
	acquired, err := r.repos.Locks.Acquire(ctx, storage.LockRecord{Key: lockKey, Owner: input.QueueItem.ID, Reason: &reason, ExpiresAt: eventlog.FormatJavaScriptISOString(r.now().Add(r.claimTTL)), CreatedAt: nowISO, UpdatedAt: nowISO})
	if err != nil {
		return input.Checkpoint, err
	}
	if !acquired {
		return input.Checkpoint, &loopError{message: fmt.Sprintf("Pull request lock is already held for %s", lockKey), kind: FailureRetryableTransient}
	}
	checkpoint := input.Checkpoint
	checkpoint.ClaimedLockKey = lockKey
	return checkpoint, nil
}

func (r *Runner) runCollectFixesStep(input stepInput) (fixerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.Detail == nil {
		return checkpoint, &loopError{message: "Missing PR detail checkpoint for collect-fixes step", kind: FailureRetryableTransient}
	}
	if checkpoint.Detail.IsDraft || normalizePRState(checkpoint.Detail.State) != "open" {
		checkpoint.SkipReason = fmt.Sprintf("Skipped pull request %s#%d because it is not eligible", input.Repo, input.PRNumber)
		return checkpoint, nil
	}
	fixItems := collectFixItemsFromCheckpoint(checkpoint)
	checkpoint.FixItems = fixItems
	checkpoint.FixItemsHash = hashFixItems(fixItems)
	if len(fixItems) == 0 {
		checkpoint.SkipReason = fmt.Sprintf("Skipped %s#%d because no fix items remain", input.Repo, input.PRNumber)
		return checkpoint, nil
	}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	checkpoint.SkipReason = ""
	return checkpoint, nil
}

func (r *Runner) runPrepareWorktreeStep(ctx context.Context, input stepInput) (fixerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if checkpoint.Worktree != nil && checkpoint.Worktree.PreparedAt != "" {
		return checkpoint, nil
	}
	branch := detailHeadRefName(checkpoint.Detail)
	if branch == "" {
		return checkpoint, fmt.Errorf("detail.headRefName is required")
	}
	projectMetadata := parseJSONObject(input.Project.MetadataJSON)
	worktreeRoot, _ := stringFromAny(projectMetadata["worktreeRoot"])
	if worktreeRoot == "" {
		resolvedRoot, err := config.DefaultProjectWorktreeRoot(input.Project.ID, input.Project.RepoPath)
		if err != nil {
			return checkpoint, err
		}
		worktreeRoot = resolvedRoot
	}
	if shouldRebuildWorktree(checkpoint) && checkpoint.Worktree != nil && checkpoint.Worktree.Path != "" && checkpoint.Worktree.Branch != "" {
		if err := r.git.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: input.Project.ID, RepoPath: input.Project.RepoPath, WorktreePath: checkpoint.Worktree.Path, Branch: checkpoint.Worktree.Branch, ProtectedBranches: compactStrings([]string{detailBaseRefName(checkpoint.Detail), derefString(input.Project.BaseBranch)})}); err != nil {
			return checkpoint, err
		}
	}
	created, err := r.git.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: input.Project.ID, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, Branch: branch, BaseBranch: firstNonEmpty(detailBaseRefName(checkpoint.Detail), derefString(input.Project.BaseBranch), "main"), PRNumber: input.PRNumber, ProtectedBranches: compactStrings([]string{detailBaseRefName(checkpoint.Detail), derefString(input.Project.BaseBranch)}), CheckoutMode: "detached"})
	if err != nil {
		return checkpoint, err
	}
	prepared, err := r.git.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: created.WorktreePath, Branch: branch, ExpectedHeadSHA: detailHeadSHA(checkpoint.Detail)})
	if err != nil {
		return checkpoint, err
	}
	if !prepared.Clean {
		return checkpoint, &loopError{message: fmt.Sprintf("Fixer worktree is dirty for branch %s; manual intervention required", branch), kind: FailureManualIntervention}
	}
	preparedAt := r.nowISO()
	checkpoint.Worktree = &checkpointWorktree{Path: created.WorktreePath, Branch: branch, HeadSHA: prepared.HeadSHA, BaseHeadSHA: prepared.HeadSHA, PreparedAt: preparedAt}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	r.appendEvent(ctx, eventInput{eventType: "fixer.worktree.prepared", projectID: input.Project.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"branch": branch, "path": created.WorktreePath, "headSha": nilIfEmpty(prepared.HeadSHA), "preparedAt": preparedAt}})
	return checkpoint, nil
}

func (r *Runner) runRepairStep(ctx context.Context, input stepInput) (fixerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if checkpoint.Repair != nil {
		if err := validateCompletedRepairCheckpoint(checkpoint.Repair); err != nil {
			return checkpoint, err
		}
		return checkpoint, nil
	}
	if len(checkpoint.FixItems) == 0 {
		return checkpoint, &loopError{message: "Missing fix items checkpoint for repair step", kind: FailureRetryableTransient}
	}
	if !r.allowRiskyFixes {
		for _, item := range checkpoint.FixItems {
			if item.Type == "conflict" {
				checkpoint.SkipReason = fmt.Sprintf("Skipped %s#%d because risky conflict fixes require manual intervention", input.Repo, input.PRNumber)
				checkpoint.ResumePolicy = "manual_intervention"
				return checkpoint, nil
			}
		}
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	executionID := eventlog.NewEventID("agent")
	execution, err := r.agentExecutor.Start(ctx, AgentRunInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Prompt: buildFixerPrompt(input.Repo, input.PRNumber, detailHeadSHA(checkpoint.Detail), checkpoint.FixItems), WorkingDirectory: worktree.Path, Timeout: r.agentTimeout, Metadata: map[string]any{"loopType": "fixer", "repo": input.Repo, "prNumber": input.PRNumber, "step": "repair"}, IdempotencyKey: fmt.Sprintf("fixer:%s:%s:%s", input.Loop.ID, firstNonEmpty(checkpoint.FixItemsHash, "unknown"), firstNonEmpty(detailHeadSHA(checkpoint.Detail), "unknown"))})
	if err != nil {
		return checkpoint, err
	}
	if r.onAgentExecutionStarted != nil {
		if err := r.onAgentExecutionStarted(ctx, AgentExecutionStartedInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Subtitle: fmt.Sprintf("%s#%d", input.Repo, input.PRNumber), Body: "Fix started", DedupeKey: "runtime.agent.started:fixer:" + input.Run.ID}); err != nil {
			return checkpoint, err
		}
	}
	result, err := execution.Wait(ctx)
	if err != nil {
		return checkpoint, err
	}
	if !strings.EqualFold(result.Status, "completed") {
		message := firstNonEmpty(result.Summary, result.Stderr, "Fixer agent "+result.Status)
		kind := FailureRetryableTransient
		if agent.IsAgentSetupFailureMessage(message) {
			kind = FailureManualIntervention
		}
		return checkpoint, &loopError{message: message, kind: kind}
	}
	if err := validateCompletedRepairCheckpoint(&checkpointRepair{Summary: result.Summary, ParseStatus: result.ParseStatus}); err != nil {
		return checkpoint, err
	}
	checkpoint.Repair = &checkpointRepair{AgentExecutionID: executionID, Summary: result.Summary, HeadSHA: detailHeadSHA(checkpoint.Detail), ParseStatus: result.ParseStatus, CompletedAt: r.nowISO()}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runReconcileCommitsStep(ctx context.Context, input stepInput) (fixerCheckpoint, error) {
	checkpoint, err := r.reconcileCommits(ctx, input.Checkpoint, buildFixerCommitMessage(input.PRNumber))
	if err != nil {
		return input.Checkpoint, err
	}
	r.appendEvent(ctx, eventInput{eventType: "fixer.commits.reconciled", projectID: input.Project.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: checkpoint.ReconcileCommits})
	return checkpoint, nil
}

func (r *Runner) runValidateStep(ctx context.Context, input stepInput) (fixerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	result, err := r.runValidation(ctx, ValidationInput{CWD: worktree.Path, Commands: r.validationCommands})
	if err != nil {
		return checkpoint, err
	}
	if !result.Passed {
		return checkpoint, &loopError{message: firstNonEmpty(result.Summary, "Validation failed"), kind: FailureRetryableAfterResume}
	}
	inspect, err := r.git.InspectHead(ctx, InspectHeadInput{WorktreePath: worktree.Path, BaseRef: reconcileBaseHeadSHA(checkpoint.ReconcileCommits)})
	if err != nil {
		return checkpoint, err
	}
	if inspect.HasUncommittedChanges {
		if checkpoint.Validation != nil && strings.Contains(strings.ToLower(checkpoint.Validation.Summary), "extra reconcile") {
			return checkpoint, &loopError{message: "Validation keeps producing new modifications after an extra reconcile pass", kind: FailureRetryableAfterResume}
		}
		checkpoint, err = r.reconcileCommits(ctx, checkpoint, buildFixerCommitMessage(input.PRNumber))
		if err != nil {
			return input.Checkpoint, err
		}
		worktree, err = requireWorktree(checkpoint)
		if err != nil {
			return checkpoint, err
		}
		second, err := r.runValidation(ctx, ValidationInput{CWD: worktree.Path, Commands: r.validationCommands})
		if err != nil {
			return checkpoint, err
		}
		if !second.Passed {
			return checkpoint, &loopError{message: firstNonEmpty(second.Summary, "Validation failed after reconcile"), kind: FailureRetryableAfterResume}
		}
		finalInspect, err := r.git.InspectHead(ctx, InspectHeadInput{WorktreePath: worktree.Path, BaseRef: reconcileBaseHeadSHA(checkpoint.ReconcileCommits)})
		if err != nil {
			return checkpoint, err
		}
		if finalInspect.HasUncommittedChanges {
			return checkpoint, &loopError{message: "Validation keeps producing new modifications after an extra reconcile pass", kind: FailureRetryableAfterResume}
		}
		second.Summary = firstNonEmpty(second.Summary, "Validation passed after extra reconcile")
		checkpoint.Validation = &second
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		return checkpoint, nil
	}
	checkpoint.Validation = &result
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runPushStep(ctx context.Context, input stepInput) (fixerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if checkpoint.Push != nil && checkpoint.Push.Pushed {
		return checkpoint, nil
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	branch := firstNonEmpty(worktree.Branch, detailHeadRefName(checkpoint.Detail))
	if branch == "" {
		return checkpoint, &loopError{message: "Missing PR head branch for push step", kind: FailureRetryableAfterResume}
	}
	if !r.allowAutoPush {
		r.appendEvent(ctx, eventInput{eventType: "fixer.push.skipped", projectID: input.Project.ID, loopID: input.Loop.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"branch": branch, "reason": "auto_push_disabled"}})
		checkpoint.SkipReason = fmt.Sprintf("Auto push disabled; manual fix push required for branch %s", branch)
		checkpoint.ResumePolicy = "manual_intervention"
		return checkpoint, nil
	}
	if checkpoint.ReconcileCommits == nil {
		return checkpoint, &loopError{message: "Missing reconcile-commits checkpoint for push step", kind: FailureRetryableAfterResume}
	}
	if !checkpoint.ReconcileCommits.WorkingTreeClean {
		return checkpoint, &loopError{message: "Working tree must be clean before push", kind: FailureRetryableAfterResume}
	}
	if checkpoint.ReconcileCommits.FinalHeadSHA != "" && checkpoint.ReconcileCommits.FinalHeadSHA == worktree.BaseHeadSHA {
		r.appendEvent(ctx, eventInput{eventType: "fixer.push.skipped", projectID: input.Project.ID, loopID: input.Loop.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"branch": branch, "reason": "no_new_commits"}})
		checkpoint.Push = &checkpointPush{Pushed: false, Branch: branch, Remote: "origin", SkippedReason: "No new commits to push"}
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		return checkpoint, nil
	}
	if err := r.git.Push(ctx, PushInput{WorktreePath: worktree.Path, Branch: branch, ExpectedRemoteHeadSHA: worktree.BaseHeadSHA}); err != nil {
		message := err.Error()
		eventType := "fixer.push.retryable"
		if strings.Contains(strings.ToLower(message), "remote head changed") {
			eventType = "fixer.push.conflicted"
		}
		r.appendEvent(ctx, eventInput{eventType: eventType, projectID: input.Project.ID, loopID: input.Loop.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"branch": branch, "message": message}})
		return checkpoint, &loopError{message: message, kind: FailureRetryableAfterResume}
	}
	finalHeadSHA := checkpoint.ReconcileCommits.FinalHeadSHA
	if finalHeadSHA == "" {
		return checkpoint, &loopError{message: "reconcileCommits.finalHeadSha is required", kind: FailureRetryableAfterResume}
	}
	if err := r.waitForPullRequestHeadSHA(ctx, waitForPullRequestHeadSHAInput{Repo: input.Repo, PRNumber: input.PRNumber, ExpectedHeadSHA: finalHeadSHA, CWD: input.Project.RepoPath, Attempts: 5, Delay: time.Second, FailureMessage: func(actual string) string {
		return fmt.Sprintf("PR head did not update after push: expected %s, got %s", finalHeadSHA, firstNonEmpty(actual, "unknown"))
	}}); err != nil {
		return checkpoint, err
	}
	metadataJSON, err := mergeLoopMetadataJSON(input.Loop.MetadataJSON, map[string]any{"lastFixHeadSha": detailHeadSHA(checkpoint.Detail), "lastFixItemsHash": checkpoint.FixItemsHash, "lastFixPushedAt": r.nowISO()})
	if err != nil {
		return checkpoint, err
	}
	if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) { updated.MetadataJSON = stringPtr(metadataJSON) }); err != nil {
		return checkpoint, err
	}
	pushedAt := r.nowISO()
	r.appendEvent(ctx, eventInput{eventType: "pr.branch.pushed", projectID: input.Project.ID, loopID: input.Loop.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"branch": branch, "pushedAt": pushedAt, "headSha": nilIfEmpty(detailHeadSHA(checkpoint.Detail))}})
	checkpoint.Push = &checkpointPush{Pushed: true, Branch: branch, Remote: "origin", PushedAt: pushedAt}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runResolveCommentsStep(ctx context.Context, input stepInput) (fixerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if checkpoint.Validation == nil || !checkpoint.Validation.Passed {
		return checkpoint, &loopError{message: "resolve-comments requires successful validation", kind: FailureRetryableAfterResume}
	}
	if checkpoint.Push == nil {
		return checkpoint, &loopError{message: "resolve-comments requires push step to complete", kind: FailureRetryableAfterResume}
	}
	if checkpoint.ReconcileCommits != nil && checkpoint.ReconcileCommits.FinalHeadSHA != "" {
		if err := r.waitForPullRequestHeadSHA(ctx, waitForPullRequestHeadSHAInput{Repo: input.Repo, PRNumber: input.PRNumber, ExpectedHeadSHA: checkpoint.ReconcileCommits.FinalHeadSHA, CWD: input.Project.RepoPath, Attempts: 5, Delay: time.Second, FailureMessage: func(actual string) string {
			return fmt.Sprintf("PR head changed before resolving comments: expected %s, got %s", checkpoint.ReconcileCommits.FinalHeadSHA, firstNonEmpty(actual, "unknown"))
		}}); err != nil {
			return checkpoint, err
		}
	}
	if checkpoint.ResolvedComments == nil {
		checkpoint.ResolvedComments = &checkpointResolvedComments{Items: []checkpointResolvedComment{}}
	}
	failedCount := 0
	for _, item := range checkpoint.FixItems {
		if item.Type != "comment" {
			continue
		}
		if alreadyResolved(checkpoint.ResolvedComments.Items, item) {
			continue
		}
		if err := r.github.ResolveReviewThread(ctx, ResolveReviewThreadInput{Repo: input.Repo, ThreadID: item.ThreadID, CWD: input.Project.RepoPath}); err != nil {
			message := err.Error()
			status := "failed"
			if strings.Contains(strings.ToLower(message), "already") {
				status = "already_resolved"
			} else {
				failedCount++
			}
			upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Status: status, Message: message, UpdatedAt: r.nowISO()})
			continue
		}
		upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Status: "resolved", UpdatedAt: r.nowISO()})
	}
	r.appendEvent(ctx, eventInput{eventType: "fixer.comments.resolved", projectID: input.Project.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"items": checkpoint.ResolvedComments.Items}})
	if failedCount > 0 {
		return checkpoint, &loopError{message: fmt.Sprintf("Failed to resolve %d review thread(s)", failedCount), kind: FailureRetryableAfterResume}
	}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runRecheckStep(ctx context.Context, input stepInput) (fixerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	checkpointHadSpecReviewing := specpr.HasLabel(detailLabels(checkpoint.Detail), specpr.ReviewingLabel)
	if (specpr.HasLabel(detail.Labels, specpr.ReviewingLabel) || checkpointHadSpecReviewing) && isSpecReviewClean(detail) {
		if specpr.HasLabel(detail.Labels, specpr.ReviewingLabel) {
			if err := r.github.RemovePullRequestLabels(ctx, PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: []string{specpr.ReviewingLabel}, CWD: input.Project.RepoPath}); err != nil {
				return checkpoint, err
			}
		}
		if !specpr.HasLabel(detail.Labels, specpr.ReadyLabel) {
			if err := r.github.AddPullRequestLabels(ctx, PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: []string{specpr.ReadyLabel}, CWD: input.Project.RepoPath}); err != nil {
				return checkpoint, err
			}
		}
	}
	checkpoint.Recheck = &checkpointRecheck{RemainingFixItems: collectFixItems(detail)}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) createRunContext(ctx context.Context, loop storage.LoopRecord) (resumedRunContext, error) {
	latestRun, err := r.repos.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return resumedRunContext{}, err
	}
	checkpoint := parseCheckpoint(nil)
	lastCompleted := FixerStep("")
	failedStep := FixerStep("")
	if latestRun != nil {
		checkpoint = parseCheckpoint(latestRun.CheckpointJSON)
		lastCompleted = asFixerStep(derefString(latestRun.LastCompletedStep))
		failedStep = asFixerStep(derefString(latestRun.CurrentStep))
	}
	restartFromDiscover := false
	resumeFromPrepare := false
	if latestRun != nil {
		failureSummary := firstNonEmpty(derefString(latestRun.Summary), derefString(latestRun.ErrorMessage))
		restartFromDiscover = shouldRestartFromDiscover(latestRun.Status, failedStep, failureSummary)
		resumeFromPrepare = shouldResumeFromPrepare(latestRun.Status, failedStep, checkpoint)
	}
	startStep := stepDiscoverPR
	resumedCheckpoint := checkpoint
	if latestRun != nil && (latestRun.Status == "failed" || latestRun.Status == "interrupted") {
		switch {
		case restartFromDiscover:
			startStep = stepDiscoverPR
			resumedCheckpoint = fixerCheckpoint{ResumePolicy: "replay_step"}
		case resumeFromPrepare:
			startStep = stepPrepareWorktree
			resumedCheckpoint = rewindCheckpointForPrepareRetry(checkpoint)
		case lastCompleted != "":
			if next := nextFixerStep(lastCompleted); next != "" {
				startStep = next
			}
		}
	}
	resumed := latestRun != nil && (latestRun.Status == "failed" || latestRun.Status == "interrupted") && startStep != stepDiscoverPR
	initialCheckpoint := fixerCheckpoint{ResumePolicy: "replay_step"}
	if resumed {
		initialCheckpoint = resumedCheckpoint
		initialCheckpoint.ResumePolicy = "advance_from_checkpoint"
	}
	nowISO := r.nowISO()
	run := storage.RunRecord{ID: eventlog.NewEventID("run"), LoopID: loop.ID, Status: "running", CurrentStep: stringPtr(string(startStep)), StartedAt: nowISO, LastHeartbeatAt: stringPtr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO}
	if resumed {
		switch {
		case restartFromDiscover:
			run.LastCompletedStep = nil
		case resumeFromPrepare:
			if prev := previousFixerStep(startStep); prev != "" {
				run.LastCompletedStep = stringPtr(string(prev))
			}
		case lastCompleted != "":
			run.LastCompletedStep = stringPtr(string(lastCompleted))
		}
	}
	encoded := mustMarshalJSON(initialCheckpoint)
	run.CheckpointJSON = &encoded
	if err := r.repos.Runs.Upsert(ctx, run); err != nil {
		return resumedRunContext{}, err
	}
	return resumedRunContext{Run: run, StartStep: startStep, Checkpoint: initialCheckpoint, Resumed: resumed}, nil
}

func (r *Runner) persistStepStarted(ctx context.Context, run storage.RunRecord, step FixerStep, checkpoint fixerCheckpoint) (storage.RunRecord, error) {
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

func (r *Runner) persistStepCompleted(ctx context.Context, run storage.RunRecord, step FixerStep, checkpoint fixerCheckpoint) (storage.RunRecord, error) {
	updated := run
	nowISO := r.nowISO()
	if checkpoint.SkipReason != "" {
		updated.Status = "success"
		updated.Summary = stringPtr(checkpoint.SkipReason)
		endedAt := nowISO
		updated.EndedAt = &endedAt
	}
	if next := nextFixerStep(step); next != "" {
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

func (r *Runner) completeRun(ctx context.Context, run storage.RunRecord, status, summary, errorMessage string, checkpoint fixerCheckpoint) (storage.RunRecord, error) {
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

func (r *Runner) persistCheckpoint(ctx context.Context, runID string, step FixerStep, checkpoint fixerCheckpoint) error {
	run, err := r.repos.Runs.GetByID(ctx, runID)
	if err != nil || run == nil {
		return err
	}
	_, err = r.persistStepStarted(ctx, *run, step, checkpoint)
	return err
}

func (r *Runner) getLatestCheckpoint(ctx context.Context, run storage.RunRecord, fallback fixerCheckpoint) fixerCheckpoint {
	persisted, err := r.repos.Runs.GetByID(ctx, run.ID)
	if err != nil || persisted == nil {
		return fallback
	}
	return parseCheckpoint(persisted.CheckpointJSON)
}

type loopUpsertResult struct {
	record  storage.LoopRecord
	created bool
}

func (r *Runner) ensureLoopForPullRequest(ctx context.Context, project storage.ProjectRecord, repo string, prNumber int64) (loopUpsertResult, error) {
	nowISO := r.nowISO()
	loops, err := r.repos.Loops.List(ctx)
	if err != nil {
		return loopUpsertResult{}, err
	}
	for _, existing := range loops {
		if existing.Type == "fixer" && existing.ProjectID == project.ID && derefString(existing.Repo) == repo && derefInt64(existing.PRNumber) == prNumber {
			if existing.Status == "paused" {
				return loopUpsertResult{record: existing, created: false}, nil
			}
			updated := existing
			if active, err := r.hasActiveRunningRun(ctx, updated.ID); err == nil && active {
				updated.Status = "running"
			} else {
				updated.Status = "queued"
			}
			updated.NextRunAt = &nowISO
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
	targetID := buildPullRequestTargetID(repo, prNumber)
	loop := storage.LoopRecord{ID: eventlog.NewEventID("loop"), Seq: seq, ProjectID: project.ID, Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "queued", NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := r.repos.Loops.Upsert(ctx, loop); err != nil {
		return loopUpsertResult{}, err
	}
	return loopUpsertResult{record: loop, created: true}, nil
}

func (r *Runner) hasActiveRunningRun(ctx context.Context, loopID string) (bool, error) {
	runs, err := r.repos.Runs.ListByLoop(ctx, loopID)
	if err != nil {
		return false, err
	}
	for _, run := range runs {
		if run.Status == "running" {
			return true, nil
		}
	}
	return false, nil
}

type enqueueInput struct {
	ProjectID    string
	LoopID       string
	Repo         string
	PRNumber     int64
	HeadSHA      string
	FixItemsHash string
}

func (r *Runner) enqueue(ctx context.Context, input enqueueInput) (storage.QueueItemRecord, error) {
	dedupeKey := buildFixerDedupeKey(input.ProjectID, input.LoopID, input.Repo, input.PRNumber, input.HeadSHA, input.FixItemsHash)
	existing, err := r.repos.Queue.FindActiveByDedupe(ctx, dedupeKey)
	if err != nil {
		return storage.QueueItemRecord{}, err
	}
	if existing != nil {
		return *existing, nil
	}
	nowISO := r.nowISO()
	targetID := buildPullRequestTargetID(input.Repo, input.PRNumber)
	lockKey := fmt.Sprintf("pr:%s:%d", input.Repo, input.PRNumber)
	projectID := input.ProjectID
	loopID := input.LoopID
	queueItem := storage.QueueItemRecord{ID: eventlog.NewEventID("queue"), ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: targetID, Repo: &input.Repo, PRNumber: &input.PRNumber, DedupeKey: dedupeKey, Priority: storage.QueuePriorityFixer, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: r.retryMaxAttempts, LockKey: &lockKey, CreatedAt: nowISO, UpdatedAt: nowISO}
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

func (r *Runner) classifyFailure(err error) *loopError {
	var typed *loopError
	if errors.As(err, &typed) {
		return typed
	}
	message := err.Error()
	if strings.Contains(strings.ToLower(message), "remote head changed") {
		return &loopError{message: message, kind: FailureRetryableAfterResume}
	}
	return &loopError{message: message, kind: FailureNonRetryable}
}

func (r *Runner) reconcileCommits(ctx context.Context, checkpoint fixerCheckpoint, commitMessage string) (fixerCheckpoint, error) {
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if checkpoint.ReconcileCommits != nil && checkpoint.ReconcileCommits.CompletedAt != "" {
		return checkpoint, nil
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	baseHeadSHA := firstNonEmpty(reconcileBaseHeadSHA(checkpoint.ReconcileCommits), worktree.BaseHeadSHA, worktree.HeadSHA)
	initial, err := r.git.InspectHead(ctx, InspectHeadInput{WorktreePath: worktree.Path, BaseRef: baseHeadSHA})
	if err != nil {
		return checkpoint, err
	}
	committedByLoop := false
	if initial.HasUncommittedChanges {
		if !r.allowAutoCommit {
			return checkpoint, &loopError{message: fmt.Sprintf("Auto commit disabled but fixer worktree has uncommitted changes: %s", firstNonEmpty(strings.Join(initial.ChangedFiles, ", "), "unknown files")), kind: FailureManualIntervention}
		}
		if _, err := r.git.Commit(ctx, CommitInput{WorktreePath: worktree.Path, Message: commitMessage}); err != nil {
			return checkpoint, err
		}
		committedByLoop = true
	}
	final, err := r.git.InspectHead(ctx, InspectHeadInput{WorktreePath: worktree.Path, BaseRef: baseHeadSHA})
	if err != nil {
		return checkpoint, err
	}
	checkpoint.ReconcileCommits = &checkpointReconcileCommits{BaseHeadSHA: baseHeadSHA, FinalHeadSHA: final.HeadSHA, NewCommitSHAs: append([]string(nil), final.NewCommitSHAs...), CommittedByAgent: len(initial.NewCommitSHAs) > 0, CommittedByLoop: committedByLoop, WorkingTreeClean: !final.HasUncommittedChanges, ChangedFiles: append([]string(nil), final.ChangedFiles...), CompletedAt: r.nowISO()}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) cleanupFixerWorktreeIfTerminal(ctx context.Context, project storage.ProjectRecord, checkpoint *fixerCheckpoint) {
	if checkpoint == nil || checkpoint.Worktree == nil || checkpoint.Worktree.Path == "" || checkpoint.Worktree.Branch == "" || checkpoint.Worktree.CleanedAt != "" {
		return
	}
	checkpoint.Worktree.CleanupAttemptedAt = r.nowISO()
	if err := r.git.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: project.ID, RepoPath: project.RepoPath, WorktreePath: checkpoint.Worktree.Path, Branch: checkpoint.Worktree.Branch, ProtectedBranches: compactStrings([]string{derefString(project.BaseBranch)})}); err != nil {
		r.appendEvent(ctx, eventInput{eventType: "fixer.worktree.cleanup_failed", projectID: project.ID, entityType: "pull_request", entityID: project.ID, payload: map[string]any{"path": checkpoint.Worktree.Path, "branch": checkpoint.Worktree.Branch, "message": err.Error()}})
		r.logError("fixer worktree cleanup failed", map[string]any{"projectId": project.ID, "worktreePath": checkpoint.Worktree.Path, "branch": checkpoint.Worktree.Branch, "message": err.Error()})
		return
	}
	checkpoint.Worktree.CleanedAt = r.nowISO()
	r.appendEvent(ctx, eventInput{eventType: "fixer.worktree.cleaned", projectID: project.ID, entityType: "pull_request", entityID: project.ID, payload: map[string]any{"path": checkpoint.Worktree.Path, "branch": checkpoint.Worktree.Branch}})
}

type waitForPullRequestHeadSHAInput struct {
	Repo            string
	PRNumber        int64
	ExpectedHeadSHA string
	CWD             string
	Attempts        int
	Delay           time.Duration
	FailureMessage  func(string) string
}

func (r *Runner) waitForPullRequestHeadSHA(ctx context.Context, input waitForPullRequestHeadSHAInput) error {
	actual := ""
	for attempt := 0; attempt < input.Attempts; attempt++ {
		detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
		if err != nil {
			return &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		actual = detail.HeadSHA
		if actual == input.ExpectedHeadSHA {
			return nil
		}
		if attempt < input.Attempts-1 {
			r.sleep(input.Delay)
		}
	}
	return &loopError{message: input.FailureMessage(actual), kind: FailureRetryableAfterResume}
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
	_ = eventlog.Append(ctx, r.repos, eventlog.AppendInput{EventType: input.eventType, ProjectID: optionalString(input.projectID), LoopID: optionalString(input.loopID), RunID: optionalString(input.runID), EntityType: optionalString(input.entityType), EntityID: optionalString(input.entityID), ActorType: optionalString("system"), ActorID: optionalString("fixer-loop"), ActorDisplayName: optionalString("fixer-loop"), Payload: input.payload, CreatedAt: r.now()})
}

func (r *Runner) hasActivePRLock(ctx context.Context, repo string, prNumber int64) bool {
	lock, err := r.repos.Locks.Get(ctx, fmt.Sprintf("pr:%s:%d", repo, prNumber))
	if err != nil || lock == nil {
		return false
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, lock.ExpiresAt)
	if err != nil {
		return false
	}
	return expiresAt.After(r.now())
}

func (r *Runner) nowISO() string { return eventlog.FormatJavaScriptISOString(r.now()) }

func (r *Runner) logInfo(message string, context map[string]any) {
	if r.logger != nil {
		r.logger.Info(message, context)
	}
}

func (r *Runner) logWarn(message string, context map[string]any) {
	if r.logger != nil {
		r.logger.Warn(message, context)
	}
}

func (r *Runner) logError(message string, context map[string]any) {
	if r.logger != nil {
		r.logger.Error(message, context)
	}
}

func stepsFrom(start FixerStep) []FixerStep {
	startIndex := 0
	for i, step := range fixerStepSequence {
		if step == start {
			startIndex = i
			break
		}
	}
	return fixerStepSequence[startIndex:]
}

func nextFixerStep(step FixerStep) FixerStep {
	for i, candidate := range fixerStepSequence {
		if candidate == step && i+1 < len(fixerStepSequence) {
			return fixerStepSequence[i+1]
		}
	}
	return ""
}

func previousFixerStep(step FixerStep) FixerStep {
	for i, candidate := range fixerStepSequence {
		if candidate == step && i > 0 {
			return fixerStepSequence[i-1]
		}
	}
	return ""
}

func validateFixerResumeCheckpoint(startStep FixerStep, checkpoint fixerCheckpoint) error {
	switch startStep {
	case stepReconcileCommits, stepValidate, stepPush, stepResolveComments, stepRecheck:
		return validateCompletedRepairCheckpoint(checkpoint.Repair)
	default:
		return nil
	}
}

func asFixerStep(value string) FixerStep {
	for _, candidate := range fixerStepSequence {
		if string(candidate) == value {
			return candidate
		}
	}
	return ""
}

func parseCheckpoint(value *string) fixerCheckpoint {
	if value == nil || *value == "" {
		return fixerCheckpoint{}
	}
	var checkpoint fixerCheckpoint
	if err := json.Unmarshal([]byte(*value), &checkpoint); err != nil {
		return fixerCheckpoint{}
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

func collectFixItemsFromCheckpoint(checkpoint fixerCheckpoint) []FixItem {
	if checkpoint.Detail == nil {
		return nil
	}
	return normalizeFixItems(checkpoint.Detail.Comments, checkpoint.Detail.Checks, checkpoint.Detail.HasConflicts)
}

func collectFixItems(detail PullRequestDetail) []FixItem {
	return normalizeFixItems(detail.Comments, detail.Checks, detail.HasConflicts)
}

func normalizeFixItems(comments []map[string]any, checks []map[string]any, hasConflicts bool) []FixItem {
	result := make([]FixItem, 0)
	for index, comment := range comments {
		if isCommentResolved(comment) {
			continue
		}
		id, _ := stringFromAny(comment["id"])
		if id == "" {
			id = fmt.Sprintf("comment-%d", index)
		}
		threadID, _ := stringFromAny(comment["threadId"])
		if threadID == "" {
			threadID = id
		}
		summary, _ := stringFromAny(comment["body"])
		if summary == "" {
			summary, _ = stringFromAny(comment["state"])
		}
		if summary == "" {
			summary = "Unresolved review comment"
		}
		result = append(result, FixItem{Type: "comment", ID: id, ThreadID: threadID, Summary: summary})
	}
	for _, check := range checks {
		if !isFailingCheck(check) {
			continue
		}
		name, _ := stringFromAny(check["name"])
		if name == "" {
			name = "unnamed-check"
		}
		summary, _ := stringFromAny(check["conclusion"])
		if summary == "" {
			summary, _ = stringFromAny(check["state"])
		}
		if summary == "" {
			summary = "Failing check"
		}
		result = append(result, FixItem{Type: "check", Name: name, Summary: summary})
	}
	if hasConflicts {
		result = append(result, FixItem{Type: "conflict", Files: []string{}})
	}
	return result
}

func isCommentResolved(comment map[string]any) bool {
	if state, ok := stringFromAny(comment["state"]); ok && strings.EqualFold(state, "resolved") {
		return true
	}
	if resolved, ok := comment["isResolved"].(bool); ok && resolved {
		return true
	}
	return false
}

func isFailingCheck(check map[string]any) bool {
	state, _ := stringFromAny(check["conclusion"])
	if state == "" {
		state, _ = stringFromAny(check["state"])
	}
	state = strings.ToUpper(strings.TrimSpace(state))
	switch state {
	case "FAILURE", "FAILED", "ERROR", "TIMED_OUT", "ACTION_REQUIRED":
		return true
	default:
		return false
	}
}

func buildPullRequestTargetID(repo string, prNumber int64) string {
	return fmt.Sprintf("pr:%s:%d", repo, prNumber)
}

func buildFixerDedupeKey(projectID, loopID, repo string, prNumber int64, headSHA, fixItemsHash string) string {
	return fmt.Sprintf("fixer:%s:%s:%s:%d:%s:%s", projectID, loopID, repo, prNumber, headSHA, fixItemsHash)
}

func hashFixItems(items []FixItem) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		encoded, _ := json.Marshal(item)
		parts = append(parts, string(encoded))
	}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

func buildFixerPrompt(repo string, prNumber int64, headSHA string, fixItems []FixItem) string {
	parts := []string{fmt.Sprintf("Fix pull request %s#%d.", repo, prNumber)}
	if headSHA != "" {
		parts = append(parts, "Head SHA: "+headSHA)
	}
	encodedItems := make([]string, 0, len(fixItems))
	for _, item := range fixItems {
		encoded, _ := json.Marshal(item)
		encodedItems = append(encodedItems, "- "+string(encoded))
	}
	parts = append(parts,
		"Fix items:\n"+strings.Join(encodedItems, "\n"),
		"Only perform repair changes for the listed fix items.",
		"Avoid pushing branches or changing remote review state; Looper will handle follow-up repository actions after your edits.",
	)
	return agent.AppendCompletionInstruction(strings.Join(parts, "\n\n"))
}

func buildFixerCommitMessage(prNumber int64) string {
	return fmt.Sprintf("fixer: address PR #%d follow-up items", prNumber)
}

func requireWorktree(checkpoint fixerCheckpoint) (*checkpointWorktree, error) {
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path == "" || checkpoint.Worktree.Branch == "" {
		return nil, &loopError{message: "Missing worktree checkpoint for fixer step", kind: FailureRetryableAfterResume}
	}
	return checkpoint.Worktree, nil
}

func shouldResumeFromPrepare(status string, failedStep FixerStep, checkpoint fixerCheckpoint) bool {
	if status != "failed" && status != "interrupted" {
		return false
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.PreparedAt == "" {
		return false
	}
	switch failedStep {
	case stepRepair, stepReconcileCommits, stepValidate, stepPush:
		return true
	case stepResolveComments, stepRecheck:
		return validateCompletedRepairCheckpoint(checkpoint.Repair) != nil
	default:
		return false
	}
}

func shouldRestartFromDiscover(status string, failedStep FixerStep, failureSummary string) bool {
	if status != "failed" && status != "interrupted" {
		return false
	}
	if failedStep == stepPrepareWorktree {
		return true
	}
	if failedStep == stepPush {
		return strings.Contains(strings.ToLower(failureSummary), "remote head changed")
	}
	if failedStep != stepResolveComments {
		return false
	}
	if status == "interrupted" {
		return true
	}
	return strings.Contains(failureSummary, "PR head changed before resolving comments")
}

func shouldRebuildWorktree(checkpoint fixerCheckpoint) bool {
	return checkpoint.Worktree != nil && checkpoint.Worktree.Path != "" && checkpoint.Worktree.PreparedAt == ""
}

func rewindCheckpointForPrepareRetry(checkpoint fixerCheckpoint) fixerCheckpoint {
	checkpoint.SkipReason = ""
	if checkpoint.Worktree != nil {
		worktree := *checkpoint.Worktree
		worktree.HeadSHA = ""
		worktree.BaseHeadSHA = ""
		worktree.PreparedAt = ""
		worktree.CleanedAt = ""
		checkpoint.Worktree = &worktree
	}
	checkpoint.Repair = nil
	checkpoint.ReconcileCommits = nil
	checkpoint.Validation = nil
	checkpoint.Push = nil
	checkpoint.ResolvedComments = nil
	checkpoint.Recheck = nil
	return checkpoint
}

func upsertResolvedComment(items *[]checkpointResolvedComment, next checkpointResolvedComment) {
	for i := range *items {
		current := (*items)[i]
		if current.FixItemID == next.FixItemID || (current.ThreadID != "" && current.ThreadID == next.ThreadID) {
			(*items)[i] = next
			return
		}
	}
	*items = append(*items, next)
}

func alreadyResolved(items []checkpointResolvedComment, item FixItem) bool {
	for _, entry := range items {
		if entry.FixItemID == item.ID || (entry.ThreadID != "" && entry.ThreadID == item.ThreadID) {
			return entry.Status == "resolved" || entry.Status == "already_resolved"
		}
	}
	return false
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

func normalizePRState(value string) string {
	if strings.EqualFold(value, "open") {
		return "open"
	}
	return "other"
}

func isSpecReviewClean(detail PullRequestDetail) bool {
	return specpr.IsReviewClean(detail.ReviewDecision, detail.Comments)
}

func detailLabels(detail *checkpointDetail) []string {
	if detail == nil {
		return nil
	}
	return detail.Labels
}

func detailHeadSHA(detail *checkpointDetail) string {
	if detail == nil {
		return ""
	}
	return detail.HeadSHA
}

func detailHeadRefName(detail *checkpointDetail) string {
	if detail == nil {
		return ""
	}
	return detail.HeadRefName
}

func detailBaseRefName(detail *checkpointDetail) string {
	if detail == nil {
		return ""
	}
	return detail.BaseRefName
}

func reconcileBaseHeadSHA(reconcile *checkpointReconcileCommits) string {
	if reconcile == nil {
		return ""
	}
	return reconcile.BaseHeadSHA
}

func compactStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, value)
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

func cloneObjectSlice(values []map[string]any) []map[string]any {
	if values == nil {
		return nil
	}
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		clone := make(map[string]any, len(value))
		for k, v := range value {
			clone[k] = v
		}
		result = append(result, clone)
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func mustMarshalJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func stringFromAny(value any) (string, bool) {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
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

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func buildPullRequestLockKey(item storage.QueueItemRecord) string {
	if item.Repo == nil || item.PRNumber == nil {
		return ""
	}
	return fmt.Sprintf("pr:%s:%d", *item.Repo, *item.PRNumber)
}
