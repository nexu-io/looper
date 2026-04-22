package reviewer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/powerformer/looper/internal/agent"
	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/eventlog"
	"github.com/powerformer/looper/internal/infra/specpr"
	"github.com/powerformer/looper/internal/storage"
)

const (
	stepDiscover ReviewerStep = "discover"
	stepFilter   ReviewerStep = "filter"
	stepClaim    ReviewerStep = "claim"
	stepSnapshot ReviewerStep = "snapshot"
	stepReview   ReviewerStep = "review"
	stepPublish  ReviewerStep = "publish"
)

var reviewerStepSequence = []ReviewerStep{
	stepDiscover,
	stepFilter,
	stepClaim,
	stepSnapshot,
	stepReview,
	stepPublish,
}

type ReviewerStep string

type ReviewEvent string

const (
	ReviewEventApprove ReviewEvent = "APPROVE"
	ReviewEventComment ReviewEvent = "COMMENT"
)

type QueueFailureKind string

const (
	FailureRetryableTransient   QueueFailureKind = "retryable_transient"
	FailureRetryableAfterResume QueueFailureKind = "retryable_after_resume"
	FailureNonRetryable         QueueFailureKind = "non_retryable"
	FailureManualIntervention   QueueFailureKind = "manual_intervention"
)

const (
	defaultAgentTimeout = 15 * time.Minute
	defaultClaimTTL     = 5 * time.Minute
	defaultRetryDelay   = 5 * time.Second
	defaultRetryMax     = 3
)

type PullRequestSummary struct {
	Number         int64
	Title          string
	State          string
	IsDraft        bool
	ReviewDecision string
	Labels         []string
	HeadSHA        string
	Author         string
	ReviewRequests []string
}

type PullRequestDetail struct {
	Number         int64
	Title          string
	Body           string
	State          string
	IsDraft        bool
	ReviewDecision string
	Labels         []string
	HeadSHA        string
	BaseSHA        string
	Author         string
	ReviewRequests []string
	ChecksSummary  string
	Diff           string
	Comments       []map[string]any
}

type transientFailure interface{ Temporary() bool }

type TransientCommandError struct{ Err error }

func (e TransientCommandError) Error() string   { return e.Err.Error() }
func (e TransientCommandError) Unwrap() error   { return e.Err }
func (e TransientCommandError) Temporary() bool { return true }

type ReviewComment struct {
	Body      string `json:"body"`
	Path      string `json:"path,omitempty"`
	Line      int64  `json:"line,omitempty"`
	Side      string `json:"side,omitempty"`
	StartLine int64  `json:"startLine,omitempty"`
	StartSide string `json:"startSide,omitempty"`
}

type ListOpenPullRequestsInput struct {
	Repo  string
	CWD   string
	Limit int
	Label string
}

type ViewPullRequestInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type CapturePullRequestSnapshotInput struct {
	ProjectID  string
	Repo       string
	PRNumber   int64
	CWD        string
	CapturedAt string
}

type SubmitReviewInput struct {
	Repo     string
	PRNumber int64
	Event    ReviewEvent
	Body     string
	CommitID string
	Comments []ReviewComment
	CWD      string
}

type PullRequestReactionInput struct {
	Repo     string
	PRNumber int64
	Content  string
	CWD      string
}

type PullRequestCommentInput struct {
	Repo     string
	PRNumber int64
	Body     string
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
	GetCurrentUserLogin(context.Context, string) (string, error)
	ViewPullRequest(context.Context, ViewPullRequestInput) (PullRequestDetail, error)
	CapturePullRequestSnapshot(context.Context, CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error)
	SubmitReview(context.Context, SubmitReviewInput) error
	AddPullRequestComment(context.Context, PullRequestCommentInput) error
	AddPullRequestReaction(context.Context, PullRequestReactionInput) error
	RemovePullRequestReaction(context.Context, PullRequestReactionInput) error
	AddPullRequestLabels(context.Context, PullRequestLabelsInput) error
	RemovePullRequestLabels(context.Context, PullRequestLabelsInput) error
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
	ParseStatus string
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
	AgentExecutor           AgentExecutor
	Logger                  bootstrap.Logger
	Now                     func() time.Time
	AgentTimeout            time.Duration
	ClaimTTL                time.Duration
	AllowAutoApprove        bool
	RetryBaseDelay          time.Duration
	RetryMaxAttempts        int64
	OnAgentExecutionStarted AgentExecutionStartedFunc
}

type Runner struct {
	db                      *sql.DB
	repos                   *storage.Repositories
	github                  GitHubGateway
	agentExecutor           AgentExecutor
	logger                  bootstrap.Logger
	now                     func() time.Time
	agentTimeout            time.Duration
	claimTTL                time.Duration
	allowAutoApprove        bool
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

type reviewerCheckpoint struct {
	ResumePolicy   string                   `json:"resumePolicy,omitempty"`
	Detail         *checkpointDetail        `json:"detail,omitempty"`
	ClaimedLockKey string                   `json:"claimedLockKey,omitempty"`
	Snapshot       *checkpointSnapshot      `json:"snapshot,omitempty"`
	PendingReview  *pendingReviewCheckpoint `json:"pendingReview,omitempty"`
	SkipReason     string                   `json:"skipReason,omitempty"`
}

type checkpointDetail struct {
	Title          string   `json:"title,omitempty"`
	State          string   `json:"state,omitempty"`
	IsDraft        bool     `json:"isDraft,omitempty"`
	ReviewDecision string   `json:"reviewDecision,omitempty"`
	Labels         []string `json:"labels,omitempty"`
	HeadSHA        string   `json:"headSha,omitempty"`
	BaseSHA        string   `json:"baseSha,omitempty"`
	Author         string   `json:"author,omitempty"`
}

type checkpointSnapshot struct {
	ID                    string `json:"id,omitempty"`
	HeadSHA               string `json:"headSha,omitempty"`
	CapturedAt            string `json:"capturedAt,omitempty"`
	Title                 string `json:"title,omitempty"`
	Body                  string `json:"body,omitempty"`
	Author                string `json:"author,omitempty"`
	ChecksSummary         string `json:"checksSummary,omitempty"`
	UnresolvedThreadCount *int64 `json:"unresolvedThreadCount,omitempty"`
	PayloadJSON           string `json:"payloadJson,omitempty"`
}

type reviewFeedbackComment struct {
	Body      string `json:"body"`
	Path      string `json:"path,omitempty"`
	Line      int64  `json:"line,omitempty"`
	Side      string `json:"side,omitempty"`
	StartLine int64  `json:"startLine,omitempty"`
	StartSide string `json:"startSide,omitempty"`
}

type publishState struct {
	ReviewSubmitted        bool `json:"reviewSubmitted,omitempty"`
	TopLevelCommentsPosted int  `json:"topLevelCommentsPosted,omitempty"`
}

type pendingReviewCheckpoint struct {
	HeadSHA      string                  `json:"headSha,omitempty"`
	Event        ReviewEvent             `json:"event,omitempty"`
	Body         string                  `json:"body,omitempty"`
	Summary      string                  `json:"summary,omitempty"`
	Comments     []reviewFeedbackComment `json:"comments,omitempty"`
	Clean        bool                    `json:"clean,omitempty"`
	PublishState *publishState           `json:"publishState,omitempty"`
}

type parsedReviewFeedback struct {
	Body     string
	Comments []reviewFeedbackComment
	Clean    bool
}

type resumedRunContext struct {
	Run        storage.RunRecord
	StartStep  ReviewerStep
	Checkpoint reviewerCheckpoint
	Resumed    bool
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
	retryMax := options.RetryMaxAttempts
	if retryMax <= 0 {
		retryMax = defaultRetryMax
	}
	return &Runner{
		db:                      options.DB,
		repos:                   options.Repos,
		github:                  options.GitHub,
		agentExecutor:           options.AgentExecutor,
		logger:                  options.Logger,
		now:                     now,
		agentTimeout:            agentTimeout,
		claimTTL:                claimTTL,
		allowAutoApprove:        options.AllowAutoApprove,
		retryBaseDelay:          retryBaseDelay,
		retryMaxAttempts:        retryMax,
		onAgentExecutionStarted: options.OnAgentExecutionStarted,
	}
}

func (r *Runner) DiscoverPullRequests(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	if r.repos == nil || r.repos.Projects == nil || r.repos.Loops == nil || r.repos.Queue == nil || r.repos.Runs == nil {
		return DiscoveryResult{}, fmt.Errorf("reviewer repositories are not configured")
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
	specPRs, err := r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: input.Repo, CWD: project.RepoPath, Limit: input.Limit, Label: specpr.ReviewingLabel})
	if err != nil {
		return DiscoveryResult{}, err
	}
	currentLogin, _ := r.github.GetCurrentUserLogin(ctx, project.RepoPath)
	currentLogin = normalizeLogin(currentLogin)
	result := DiscoveryResult{}
	seen := map[string]struct{}{}
	enqueue := func(pr PullRequestSummary, existing *storage.LoopRecord) error {
		loopResult, loopErr := r.ensureLoopForPullRequest(ctx, *project, input.Repo, pr.Number, existing)
		if loopErr != nil {
			return loopErr
		}
		if loopResult.record.Status == "paused" {
			result.Skipped++
			return nil
		}
		if loopResult.created {
			result.CreatedLoopIDs = append(result.CreatedLoopIDs, loopResult.record.ID)
		}
		meta := parseJSONObject(loopResult.record.MetadataJSON)
		if last, ok := stringFromAny(meta["lastPublishedHeadSha"]); ok && last == pr.HeadSHA && pr.HeadSHA != "" {
			result.Skipped++
			return nil
		}
		queueItem, queueErr := r.enqueue(ctx, enqueueInput{
			ProjectID: project.ID,
			LoopID:    loopResult.record.ID,
			Repo:      input.Repo,
			PRNumber:  pr.Number,
		})
		if queueErr != nil {
			return queueErr
		}
		result.QueueItems = append(result.QueueItems, queueItem)
		return nil
	}
	seenEnqueue := func(pr PullRequestSummary) error {
		key := fmt.Sprintf("%s#%d", input.Repo, pr.Number)
		if _, ok := seen[key]; ok {
			return nil
		}
		seen[key] = struct{}{}
		return enqueue(pr, nil)
	}
	for _, pr := range openPRs {
		if pr.IsDraft || normalizePRState(pr.State) != "open" || currentLogin == "" || !isCurrentUserRequested(pr.ReviewRequests, currentLogin) {
			result.Skipped++
			continue
		}
		if err := seenEnqueue(pr); err != nil {
			return DiscoveryResult{}, err
		}
	}
	for _, pr := range specPRs {
		if pr.IsDraft || normalizePRState(pr.State) != "open" {
			result.Skipped++
			continue
		}
		if err := seenEnqueue(pr); err != nil {
			return DiscoveryResult{}, err
		}
	}
	followUpLoops, err := r.listFollowUpLoops(ctx, project.ID, input.Repo)
	if err != nil {
		return DiscoveryResult{}, err
	}
	for _, loop := range followUpLoops {
		if loop.PRNumber == nil {
			continue
		}
		key := fmt.Sprintf("%s#%d", input.Repo, *loop.PRNumber)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		detail, viewErr := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: *loop.PRNumber, CWD: project.RepoPath})
		if viewErr != nil {
			result.Skipped++
			continue
		}
		if detail.IsDraft || normalizePRState(detail.State) != "open" {
			result.Skipped++
			continue
		}
		if err := enqueue(summaryFromDetail(detail), &loop); err != nil {
			return DiscoveryResult{}, err
		}
	}
	return result, nil
}

func (r *Runner) ProcessNext(ctx context.Context, claimedBy string) (*ProcessResult, error) {
	if r.repos == nil || r.repos.Queue == nil {
		return nil, fmt.Errorf("reviewer queue repository is not configured")
	}
	item, err := r.repos.Queue.ClaimNextOfType(ctx, r.nowISO(), claimedBy, "reviewer")
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
		if cleanupErr := r.finalizeClaimSetupFailure(ctx, queueItem, err); cleanupErr != nil {
			return nil, errors.Join(err, cleanupErr)
		}
		return nil, err
	}
	return &result, nil
}

func (r *Runner) finalizeClaimSetupFailure(ctx context.Context, queueItem storage.QueueItemRecord, cause error) error {
	failure := r.classifyFailure(cause)
	failedQueue, err := r.failQueueItem(ctx, queueItem, failure.kind, failure.message)
	if err != nil {
		return err
	}
	if queueItem.LoopID == nil {
		return nil
	}
	loop, err := r.repos.Loops.GetByID(ctx, *queueItem.LoopID)
	if err != nil {
		return err
	}
	if loop == nil {
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
			if failure.kind == FailureManualIntervention || (failedQueue != nil && failedQueue.Status == "cancelled") {
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
	if queueItem.Type != "reviewer" {
		return ProcessResult{}, fmt.Errorf("unsupported queue item type: %s", queueItem.Type)
	}
	if queueItem.LoopID == nil || queueItem.Repo == nil || queueItem.PRNumber == nil {
		return ProcessResult{}, fmt.Errorf("reviewer queue item requires loopId, repo, and prNumber")
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
	if resumedRun.Resumed && resumedRun.StartStep != stepClaim {
		claimedLockKey = checkpoint.ClaimedLockKey
		if claimedLockKey == "" {
			claimedLockKey = derefString(queueItem.LockKey)
			if claimedLockKey == "" {
				claimedLockKey = buildPullRequestLockKey(queueItem)
			}
		}
		if claimedLockKey != "" {
			nowISO := r.nowISO()
			reason := "reviewer-run-resume"
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
				r.logWarn("reviewer business lock release failed", map[string]any{"lockKey": claimedLockKey, "error": err.Error()})
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
	r.appendEvent(ctx, eventInput{eventType: "loop.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "loop", entityID: loop.ID, payload: map[string]any{"queueItemId": queueItem.ID, "resumed": resumedRun.Resumed, "startStep": string(resumedRun.StartStep)}})
	r.appendEvent(ctx, eventInput{eventType: "run.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"queueItemId": queueItem.ID, "currentStep": string(resumedRun.StartStep)}})
	r.logInfo("reviewer run started", map[string]any{"projectId": project.ID, "loopId": loop.ID, "runId": run.ID, "queueItemId": queueItem.ID, "currentStep": string(resumedRun.StartStep), "resumed": resumedRun.Resumed})
	for _, step := range stepsFrom(resumedRun.StartStep) {
		run, err = r.persistStepStarted(ctx, run, step, checkpoint)
		if err != nil {
			return ProcessResult{}, err
		}
		r.appendEvent(ctx, eventInput{eventType: "loop.step.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"step": string(step)}})
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
			r.logError("reviewer run failed", map[string]any{"projectId": project.ID, "loopId": loop.ID, "runId": run.ID, "queueItemId": queueItem.ID, "currentStep": derefString(run.CurrentStep), "failureKind": string(failure.kind), "summary": failure.message})
			failedQueue, queueErr := r.failQueueItem(ctx, queueItem, failure.kind, failure.message)
			if queueErr != nil {
				return ProcessResult{}, queueErr
			}
			_, loopErr := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
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
			})
			if loopErr != nil {
				return ProcessResult{}, loopErr
			}
			return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: "failed", Summary: failure.message, FailureKind: failure.kind}, nil
		}
		if step == stepClaim {
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
		summary = fmt.Sprintf("Published review for %s#%d", *queueItem.Repo, *queueItem.PRNumber)
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
	return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: status, Summary: summary}, nil
}

type stepInput struct {
	Project    storage.ProjectRecord
	Loop       storage.LoopRecord
	Run        storage.RunRecord
	QueueItem  storage.QueueItemRecord
	Repo       string
	PRNumber   int64
	Checkpoint reviewerCheckpoint
}

func (r *Runner) executeStep(ctx context.Context, step ReviewerStep, input stepInput) (reviewerCheckpoint, error) {
	switch step {
	case stepDiscover:
		return r.runDiscoverStep(ctx, input)
	case stepFilter:
		return r.runFilterStep(input)
	case stepClaim:
		return r.runClaimStep(ctx, input)
	case stepSnapshot:
		return r.runSnapshotStep(ctx, input)
	case stepReview:
		return r.runReviewStep(ctx, input)
	case stepPublish:
		return r.runPublishStep(ctx, input)
	default:
		return input.Checkpoint, fmt.Errorf("unsupported reviewer step: %s", step)
	}
}

func (r *Runner) runDiscoverStep(ctx context.Context, input stepInput) (reviewerCheckpoint, error) {
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return input.Checkpoint, err
	}
	checkpoint := input.Checkpoint
	checkpoint.Detail = &checkpointDetail{Title: detail.Title, State: detail.State, IsDraft: detail.IsDraft, ReviewDecision: detail.ReviewDecision, Labels: cloneStrings(detail.Labels), HeadSHA: detail.HeadSHA, BaseSHA: detail.BaseSHA, Author: detail.Author}
	checkpoint.ResumePolicy = "replay_step"
	return checkpoint, nil
}

func (r *Runner) runFilterStep(input stepInput) (reviewerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.Detail == nil {
		return checkpoint, &loopError{message: "Missing PR detail checkpoint for filter step", kind: FailureRetryableTransient}
	}
	if checkpoint.Detail.IsDraft {
		checkpoint.SkipReason = fmt.Sprintf("Skipped draft pull request %s#%d", input.Repo, input.PRNumber)
		return checkpoint, nil
	}
	if normalizePRState(checkpoint.Detail.State) != "open" {
		checkpoint.SkipReason = fmt.Sprintf("Skipped non-open pull request %s#%d", input.Repo, input.PRNumber)
		return checkpoint, nil
	}
	meta := parseJSONObject(input.Loop.MetadataJSON)
	if last, ok := stringFromAny(meta["lastPublishedHeadSha"]); ok && checkpoint.Detail.HeadSHA != "" && last == checkpoint.Detail.HeadSHA {
		checkpoint.SkipReason = fmt.Sprintf("Skipped already-reviewed head %s for %s#%d", checkpoint.Detail.HeadSHA, input.Repo, input.PRNumber)
		return checkpoint, nil
	}
	return checkpoint, nil
}

func (r *Runner) runClaimStep(ctx context.Context, input stepInput) (reviewerCheckpoint, error) {
	lockKey := derefString(input.QueueItem.LockKey)
	if lockKey == "" {
		lockKey = buildPullRequestLockKey(input.QueueItem)
	}
	if lockKey == "" {
		return input.Checkpoint, fmt.Errorf("reviewer queue item lock key is required")
	}
	nowISO := r.nowISO()
	reason := "reviewer-claim"
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

func (r *Runner) runSnapshotStep(ctx context.Context, input stepInput) (reviewerCheckpoint, error) {
	snapshot, err := r.github.CapturePullRequestSnapshot(ctx, CapturePullRequestSnapshotInput{ProjectID: input.Project.ID, Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath, CapturedAt: r.nowISO()})
	if err != nil {
		return input.Checkpoint, err
	}
	if err := r.repos.PullRequestSnapshots.Upsert(ctx, snapshot); err != nil {
		return input.Checkpoint, err
	}
	checkpoint := input.Checkpoint
	checkpoint.Snapshot = &checkpointSnapshot{ID: snapshot.ID, HeadSHA: snapshot.HeadSHA, CapturedAt: snapshot.CapturedAt, Title: derefString(snapshot.Title), Body: derefString(snapshot.Body), Author: derefString(snapshot.Author), ChecksSummary: derefString(snapshot.ChecksSummary), UnresolvedThreadCount: snapshot.UnresolvedThreadCount, PayloadJSON: derefString(snapshot.PayloadJSON)}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runReviewStep(ctx context.Context, input stepInput) (reviewerCheckpoint, error) {
	if input.Checkpoint.PendingReview != nil {
		return input.Checkpoint, nil
	}
	if input.Checkpoint.Snapshot == nil {
		return input.Checkpoint, &loopError{message: "Missing PR snapshot checkpoint for review step", kind: FailureRetryableTransient}
	}
	executionID := eventlog.NewEventID("agent")
	execution, err := r.agentExecutor.Start(ctx, AgentRunInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Prompt: buildReviewPrompt(input.Repo, input.PRNumber, input.Checkpoint), WorkingDirectory: input.Project.RepoPath, Timeout: r.agentTimeout, Metadata: map[string]any{"loopType": "reviewer", "repo": input.Repo, "prNumber": input.PRNumber}, IdempotencyKey: fmt.Sprintf("reviewer:%s:%s", input.Loop.ID, input.Checkpoint.Snapshot.HeadSHA)})
	if err != nil {
		return input.Checkpoint, err
	}
	if r.onAgentExecutionStarted != nil {
		if err := r.onAgentExecutionStarted(ctx, AgentExecutionStartedInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Subtitle: fmt.Sprintf("%s#%d", input.Repo, input.PRNumber), Body: "Review started", DedupeKey: "runtime.agent.started:reviewer:" + input.Run.ID}); err != nil && r.logger != nil {
			r.logger.Warn("reviewer agent start notification failed", map[string]any{"loopId": input.Loop.ID, "runId": input.Run.ID, "error": err.Error()})
		}
	}
	_ = r.tryAddReaction(ctx, input, "eyes")
	result, err := execution.Wait(ctx)
	if err != nil {
		_ = r.tryRemoveReaction(ctx, input, "eyes")
		return input.Checkpoint, err
	}
	if result.Status != "completed" {
		_ = r.tryRemoveReaction(ctx, input, "eyes")
		return input.Checkpoint, &loopError{message: firstNonEmpty(result.Summary, fmt.Sprintf("Reviewer agent %s", result.Status)), kind: FailureRetryableTransient}
	}
	feedback := parseReviewFeedback(result)
	if !feedback.Clean && len(feedback.Comments) == 0 {
		_ = r.tryRemoveReaction(ctx, input, "eyes")
		kind := FailureRetryableTransient
		if result.ParseStatus == "invalid_json" {
			kind = FailureNonRetryable
		}
		return input.Checkpoint, &loopError{message: "Reviewer agent produced no actionable review comments", kind: kind}
	}
	checkpoint := input.Checkpoint
	checkpoint.PendingReview = &pendingReviewCheckpoint{HeadSHA: input.Checkpoint.Snapshot.HeadSHA, Event: ternaryReviewEvent(feedback.Clean), Body: feedback.Body, Summary: result.Summary, Comments: feedback.Comments, Clean: feedback.Clean}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runPublishStep(ctx context.Context, input stepInput) (reviewerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if checkpoint.PendingReview == nil {
		return checkpoint, &loopError{message: "Missing pending review checkpoint for publish step", kind: FailureRetryableAfterResume}
	}
	pending := normalizePendingReviewCheckpoint(*checkpoint.PendingReview)
	meta := parseJSONObject(input.Loop.MetadataJSON)
	if last, ok := stringFromAny(meta["lastPublishedHeadSha"]); ok && last == pending.HeadSHA {
		checkpoint.SkipReason = fmt.Sprintf("Skipped already-published review for head %s", pending.HeadSHA)
		return checkpoint, nil
	}
	reviewEvent := pending.Event
	if reviewEvent == ReviewEventApprove && !r.allowAutoApprove {
		reviewEvent = ReviewEventComment
	}
	reviewBody := pending.Body
	if pending.Clean && reviewEvent == ReviewEventComment {
		reviewBody = firstNonEmpty(strings.TrimSpace(reviewBody), strings.TrimSpace(pending.Summary))
	}
	repo := input.Repo
	prNumber := input.PRNumber
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	phase := resolvePullRequestPhase(detail.Labels)
	checkpointPhase := resolvePullRequestPhase(detailLabels(input.Checkpoint.Detail))
	if detail.HeadSHA != "" && detail.HeadSHA != pending.HeadSHA {
		return checkpoint, &loopError{message: fmt.Sprintf("PR head changed before publish: expected %s, got %s", pending.HeadSHA, detail.HeadSHA), kind: FailureRetryableAfterResume}
	}
	if pending.Clean {
		if !pending.PublishState.ReviewSubmitted && (reviewEvent == ReviewEventApprove || reviewBody != "") {
			if err := r.github.SubmitReview(ctx, SubmitReviewInput{Repo: repo, PRNumber: prNumber, Event: reviewEvent, Body: reviewBody, CWD: input.Project.RepoPath}); err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
			}
			pending.PublishState.ReviewSubmitted = true
			checkpoint.PendingReview = pending.clone()
			if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
				return checkpoint, err
			}
		}
	} else {
		if !pending.PublishState.ReviewSubmitted && (hasInlineComments(pending.Comments) || strings.TrimSpace(pending.Body) != "") {
			inline := inlineComments(pending.Comments)
			err := r.github.SubmitReview(ctx, SubmitReviewInput{Repo: repo, PRNumber: prNumber, Event: reviewEvent, Body: pending.Body, CommitID: pending.HeadSHA, Comments: toGitHubReviewComments(inline), CWD: input.Project.RepoPath})
			if err != nil {
				if len(inline) == 0 || !isInlineReviewAnchorFailure(err) {
					return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
				}
				r.logWarn("reviewer inline anchors rejected; falling back to top-level comments", map[string]any{"projectId": input.Project.ID, "loopId": input.Loop.ID, "runId": input.Run.ID, "repo": repo, "prNumber": prNumber, "inlineCommentCount": len(inline)})
				for i := range pending.Comments {
					if pending.Comments[i].Path != "" && pending.Comments[i].Line > 0 && pending.Comments[i].Side != "" {
						pending.Comments[i] = downgradeInlineCommentToTopLevel(pending.Comments[i])
					}
				}
				checkpoint.PendingReview = pending.clone()
				if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
					return checkpoint, err
				}
				fallbackBody := firstNonEmpty(strings.TrimSpace(pending.Body), strings.TrimSpace(pending.Summary))
				if fallbackBody != "" {
					if err := r.github.SubmitReview(ctx, SubmitReviewInput{Repo: repo, PRNumber: prNumber, Event: reviewEvent, Body: fallbackBody, CWD: input.Project.RepoPath}); err != nil {
						return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
					}
				}
			}
			pending.PublishState.ReviewSubmitted = true
			checkpoint.PendingReview = pending.clone()
			if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
				return checkpoint, err
			}
		}
		topLevel := topLevelComments(pending.Comments)
		for pending.PublishState.TopLevelCommentsPosted < len(topLevel) {
			comment := topLevel[pending.PublishState.TopLevelCommentsPosted]
			if err := r.github.AddPullRequestComment(ctx, PullRequestCommentInput{Repo: repo, PRNumber: prNumber, Body: comment.Body, CWD: input.Project.RepoPath}); err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
			}
			pending.PublishState.TopLevelCommentsPosted++
			checkpoint.PendingReview = pending.clone()
			if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
				return checkpoint, err
			}
		}
	}
	if pending.Clean {
		_ = r.tryAddReaction(ctx, input, "+1")
	} else {
		_ = r.tryRemoveReaction(ctx, input, "+1")
	}
	_ = r.tryRemoveReaction(ctx, input, "eyes")
	postSubmitDetail := detail
	if reviewEvent == ReviewEventApprove && (phase == "spec" || checkpointPhase == "spec") {
		postSubmitDetail, err = r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: input.Project.RepoPath})
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
	}
	if reviewEvent == ReviewEventApprove && (phase == "spec" || checkpointPhase == "spec") && isSpecReviewClean(postSubmitDetail) {
		if specpr.HasLabel(postSubmitDetail.Labels, specpr.ReviewingLabel) {
			if err := r.github.RemovePullRequestLabels(ctx, PullRequestLabelsInput{Repo: repo, PRNumber: prNumber, Labels: []string{specpr.ReviewingLabel}, CWD: input.Project.RepoPath}); err != nil {
				return checkpoint, err
			}
		}
		if !specpr.HasLabel(postSubmitDetail.Labels, specpr.ReadyLabel) {
			if err := r.github.AddPullRequestLabels(ctx, PullRequestLabelsInput{Repo: repo, PRNumber: prNumber, Labels: []string{specpr.ReadyLabel}, CWD: input.Project.RepoPath}); err != nil {
				return checkpoint, err
			}
		}
	}
	metadataJSON, err := mergeLoopMetadataJSON(input.Loop.MetadataJSON, map[string]any{"lastPublishedHeadSha": pending.HeadSHA, "lastReviewEvent": string(reviewEvent), "lastReviewSummary": pending.Summary, "lastPublishedAt": r.nowISO()})
	if err != nil {
		return checkpoint, err
	}
	if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) { updated.MetadataJSON = stringPtr(metadataJSON) }); err != nil {
		return checkpoint, err
	}
	r.appendEvent(ctx, eventInput{eventType: "pr.review.posted", projectID: input.Project.ID, loopID: input.Loop.ID, runID: input.Run.ID, entityType: "pull_request", entityID: fmt.Sprintf("%s#%d", repo, prNumber), payload: map[string]any{"repo": repo, "prNumber": prNumber, "event": string(reviewEvent), "headSha": pending.HeadSHA}})
	return checkpoint, nil
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
	_ = eventlog.Append(ctx, r.repos, eventlog.AppendInput{EventType: input.eventType, ProjectID: optionalString(input.projectID), LoopID: optionalString(input.loopID), RunID: optionalString(input.runID), EntityType: optionalString(input.entityType), EntityID: optionalString(input.entityID), ActorType: optionalString("system"), ActorID: optionalString("reviewer-loop"), ActorDisplayName: optionalString("reviewer-loop"), Payload: input.payload, CreatedAt: r.now()})
}

func (r *Runner) createRunContext(ctx context.Context, loop storage.LoopRecord) (resumedRunContext, error) {
	latestRun, err := r.repos.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return resumedRunContext{}, err
	}
	checkpoint := parseCheckpoint(nil)
	lastCompleted := ReviewerStep("")
	failedStep := ReviewerStep("")
	if latestRun != nil {
		checkpoint = parseCheckpoint(latestRun.CheckpointJSON)
		lastCompleted = asReviewerStep(derefString(latestRun.LastCompletedStep))
		failedStep = asReviewerStep(derefString(latestRun.CurrentStep))
	}
	restartFromDiscover := false
	if latestRun != nil {
		failureSummary := firstNonEmpty(derefString(latestRun.Summary), derefString(latestRun.ErrorMessage))
		restartFromDiscover = shouldRestartFromDiscover(latestRun.Status, failedStep, failureSummary)
	}
	startStep := stepDiscover
	if latestRun != nil && (latestRun.Status == "failed" || latestRun.Status == "interrupted") {
		if restartFromDiscover {
			startStep = stepDiscover
		} else if lastCompleted != "" {
			if next := nextReviewerStep(lastCompleted); next != "" {
				startStep = next
			}
		}
	}
	resumed := latestRun != nil && (latestRun.Status == "failed" || latestRun.Status == "interrupted") && startStep != stepDiscover
	initialCheckpoint := reviewerCheckpoint{ResumePolicy: "replay_step"}
	if resumed {
		initialCheckpoint = checkpoint
		if restartFromDiscover {
			initialCheckpoint = reviewerCheckpoint{ResumePolicy: "replay_step"}
		} else {
			initialCheckpoint.ResumePolicy = "advance_from_checkpoint"
		}
	}
	nowISO := r.nowISO()
	run := storage.RunRecord{ID: eventlog.NewEventID("run"), LoopID: loop.ID, Status: "running", CurrentStep: stringPtr(string(startStep)), CheckpointJSON: stringPtr(mustMarshalJSON(initialCheckpoint)), StartedAt: nowISO, LastHeartbeatAt: stringPtr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO}
	if resumed && !restartFromDiscover && lastCompleted != "" {
		run.LastCompletedStep = stringPtr(string(lastCompleted))
	}
	if err := r.repos.Runs.Upsert(ctx, run); err != nil {
		return resumedRunContext{}, err
	}
	return resumedRunContext{Run: run, StartStep: startStep, Checkpoint: initialCheckpoint, Resumed: resumed}, nil
}

func (r *Runner) persistStepStarted(ctx context.Context, run storage.RunRecord, step ReviewerStep, checkpoint reviewerCheckpoint) (storage.RunRecord, error) {
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

func (r *Runner) persistStepCompleted(ctx context.Context, run storage.RunRecord, step ReviewerStep, checkpoint reviewerCheckpoint) (storage.RunRecord, error) {
	updated := run
	nowISO := r.nowISO()
	next := nextReviewerStep(step)
	if next != "" {
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

func (r *Runner) completeRun(ctx context.Context, run storage.RunRecord, status, summary, errorMessage string, checkpoint reviewerCheckpoint) (storage.RunRecord, error) {
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

func (r *Runner) persistCheckpoint(ctx context.Context, runID string, step ReviewerStep, checkpoint reviewerCheckpoint) error {
	run, err := r.repos.Runs.GetByID(ctx, runID)
	if err != nil || run == nil {
		return err
	}
	_, err = r.persistStepStarted(ctx, *run, step, checkpoint)
	return err
}

func (r *Runner) getLatestCheckpoint(ctx context.Context, run storage.RunRecord, fallback reviewerCheckpoint) reviewerCheckpoint {
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

func (r *Runner) ensureLoopForPullRequest(ctx context.Context, project storage.ProjectRecord, repo string, prNumber int64, existing *storage.LoopRecord) (loopUpsertResult, error) {
	nowISO := r.nowISO()
	if existing == nil {
		loops, err := r.repos.Loops.List(ctx)
		if err != nil {
			return loopUpsertResult{}, err
		}
		for _, loop := range loops {
			if loop.Type == "reviewer" && loop.ProjectID == project.ID && derefString(loop.Repo) == repo && derefInt64(loop.PRNumber) == prNumber {
				existing = &loop
				break
			}
		}
	}
	if existing != nil {
		if existing.Status == "paused" {
			return loopUpsertResult{record: *existing, created: false}, nil
		}
		updated := *existing
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
	seq, err := r.repos.Loops.AllocateSeq(ctx)
	if err != nil {
		return loopUpsertResult{}, err
	}
	targetID := fmt.Sprintf("pr:%s:%d", repo, prNumber)
	loop := storage.LoopRecord{ID: eventlog.NewEventID("loop"), Seq: seq, ProjectID: project.ID, Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "queued", NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := r.repos.Loops.Upsert(ctx, loop); err != nil {
		return loopUpsertResult{}, err
	}
	r.appendEvent(ctx, eventInput{eventType: "loop.created", projectID: project.ID, loopID: loop.ID, entityType: "loop", entityID: loop.ID, payload: map[string]any{"type": "reviewer", "repo": repo, "prNumber": prNumber}})
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

func (r *Runner) listFollowUpLoops(ctx context.Context, projectID, repo string) ([]storage.LoopRecord, error) {
	loops, err := r.repos.Loops.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]storage.LoopRecord, 0)
	for _, loop := range loops {
		if loop.Type != "reviewer" || loop.ProjectID != projectID || derefString(loop.Repo) != repo || loop.PRNumber == nil || loop.Status == "paused" {
			continue
		}
		meta := parseJSONObject(loop.MetadataJSON)
		if follow, ok := meta["followUpdates"].(bool); ok && follow {
			result = append(result, loop)
		}
	}
	return result, nil
}

type enqueueInput struct {
	ProjectID string
	LoopID    string
	Repo      string
	PRNumber  int64
}

func (r *Runner) enqueue(ctx context.Context, input enqueueInput) (storage.QueueItemRecord, error) {
	dedupeKey := buildReviewerDedupeKey(input.ProjectID, input.LoopID, input.Repo, input.PRNumber)
	existing, err := r.repos.Queue.FindActiveByDedupe(ctx, dedupeKey)
	if err != nil {
		return storage.QueueItemRecord{}, err
	}
	if existing != nil {
		return *existing, nil
	}
	nowISO := r.nowISO()
	targetID := fmt.Sprintf("pr:%s:%d", input.Repo, input.PRNumber)
	lockKey := fmt.Sprintf("pr:%s:%d", input.Repo, input.PRNumber)
	projectID := input.ProjectID
	loopID := input.LoopID
	queueItem := storage.QueueItemRecord{ID: eventlog.NewEventID("queue"), ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: targetID, Repo: &input.Repo, PRNumber: &input.PRNumber, DedupeKey: dedupeKey, Priority: storage.QueuePriorityReviewer, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: r.retryMaxAttempts, LockKey: &lockKey, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := r.repos.Queue.Upsert(ctx, queueItem); err != nil {
		return storage.QueueItemRecord{}, err
	}
	return queueItem, nil
}

func buildReviewerDedupeKey(projectID, loopID, repo string, prNumber int64) string {
	return fmt.Sprintf("reviewer:%s:%s:%s:%d", projectID, loopID, repo, prNumber)
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
	var transient transientFailure
	if errors.As(err, &transient) && transient.Temporary() {
		return &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	return &loopError{message: err.Error(), kind: FailureNonRetryable}
}

func (r *Runner) tryAddReaction(ctx context.Context, input stepInput, content string) error {
	if err := r.github.AddPullRequestReaction(ctx, PullRequestReactionInput{Repo: input.Repo, PRNumber: input.PRNumber, Content: content, CWD: input.Project.RepoPath}); err != nil {
		r.logWarn("reviewer reaction add failed", map[string]any{"projectId": input.Project.ID, "loopId": input.Loop.ID, "runId": input.Run.ID, "repo": input.Repo, "prNumber": input.PRNumber, "content": content, "error": err.Error()})
		return err
	}
	return nil
}

func (r *Runner) tryRemoveReaction(ctx context.Context, input stepInput, content string) error {
	if err := r.github.RemovePullRequestReaction(ctx, PullRequestReactionInput{Repo: input.Repo, PRNumber: input.PRNumber, Content: content, CWD: input.Project.RepoPath}); err != nil {
		r.logWarn("reviewer reaction removal failed", map[string]any{"projectId": input.Project.ID, "loopId": input.Loop.ID, "runId": input.Run.ID, "repo": input.Repo, "prNumber": input.PRNumber, "content": content, "error": err.Error()})
		return err
	}
	return nil
}

func (r *Runner) nowISO() string {
	return eventlog.FormatJavaScriptISOString(r.now())
}

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

func shouldRestartFromDiscover(status string, failedStep ReviewerStep, failureSummary string) bool {
	if status != "failed" && status != "interrupted" {
		return false
	}
	return failedStep == stepPublish && strings.Contains(failureSummary, "PR head changed before publish")
}

func stepsFrom(start ReviewerStep) []ReviewerStep {
	startIndex := 0
	for i, step := range reviewerStepSequence {
		if step == start {
			startIndex = i
			break
		}
	}
	return reviewerStepSequence[startIndex:]
}

func nextReviewerStep(step ReviewerStep) ReviewerStep {
	for i, candidate := range reviewerStepSequence {
		if candidate == step && i+1 < len(reviewerStepSequence) {
			return reviewerStepSequence[i+1]
		}
	}
	return ""
}

func asReviewerStep(value string) ReviewerStep {
	for _, candidate := range reviewerStepSequence {
		if string(candidate) == value {
			return candidate
		}
	}
	return ""
}

func parseCheckpoint(value *string) reviewerCheckpoint {
	if value == nil || *value == "" {
		return reviewerCheckpoint{}
	}
	var checkpoint reviewerCheckpoint
	if err := json.Unmarshal([]byte(*value), &checkpoint); err != nil {
		return reviewerCheckpoint{}
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

func summaryFromDetail(detail PullRequestDetail) PullRequestSummary {
	return PullRequestSummary{Number: detail.Number, Title: detail.Title, State: detail.State, IsDraft: detail.IsDraft, ReviewDecision: detail.ReviewDecision, Labels: cloneStrings(detail.Labels), HeadSHA: detail.HeadSHA, Author: detail.Author, ReviewRequests: cloneStrings(detail.ReviewRequests)}
}

func normalizePRState(value string) string {
	if strings.EqualFold(value, "open") {
		return "open"
	}
	return "other"
}

func normalizeLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

func isCurrentUserRequested(requested []string, currentLogin string) bool {
	for _, login := range requested {
		if normalizeLogin(login) == currentLogin {
			return true
		}
	}
	return false
}

func buildPullRequestLockKey(item storage.QueueItemRecord) string {
	if item.Repo == nil || item.PRNumber == nil {
		return ""
	}
	return fmt.Sprintf("pr:%s:%d", *item.Repo, *item.PRNumber)
}

func buildReviewPrompt(repo string, prNumber int64, checkpoint reviewerCheckpoint) string {
	phase := resolvePullRequestPhase(detailLabels(checkpoint.Detail))
	phaseInstruction := "This is an implementation review. Focus on code correctness, safety, tests, and maintainability."
	if phase == "spec" {
		phaseInstruction = "This is a spec review. Focus on scope, correctness, feasibility, risks, and validation. Do not review implementation details beyond whether the spec is actionable."
	}
	parts := []string{fmt.Sprintf("Review pull request %s#%d.", repo, prNumber), "Phase: " + phase, phaseInstruction}
	if checkpoint.Snapshot != nil {
		if checkpoint.Snapshot.Title != "" {
			parts = append(parts, "Title: "+checkpoint.Snapshot.Title)
		}
		if checkpoint.Snapshot.Body != "" {
			parts = append(parts, "Body:\n"+checkpoint.Snapshot.Body)
		}
		parts = append(parts, "Head SHA: "+checkpoint.Snapshot.HeadSHA)
		if checkpoint.Detail != nil && checkpoint.Detail.Author != "" {
			parts = append(parts, "Author: "+checkpoint.Detail.Author)
		}
		if checkpoint.Snapshot.ChecksSummary != "" {
			parts = append(parts, "Checks: "+checkpoint.Snapshot.ChecksSummary)
		}
		if checkpoint.Snapshot.UnresolvedThreadCount != nil {
			parts = append(parts, fmt.Sprintf("Unresolved threads: %d", *checkpoint.Snapshot.UnresolvedThreadCount))
		}
		payload := parseJSONObject(optionalString(checkpoint.Snapshot.PayloadJSON))
		if diff, ok := stringFromAny(payload["diff"]); ok && diff != "" {
			parts = append(parts, "Diff:\n"+diff)
		}
	}
	parts = append(parts,
		"Return detailed GitHub review feedback as raw JSON with this exact shape:\n{\"verdict\":\"clean\"|\"actionable\",\"body\":\"optional overall summary\",\"comments\":[{\"body\":\"required comment text\",\"path\":\"src/file.ts optional for inline comments\",\"line\":123,\"side\":\"RIGHT\",\"startLine\":120,\"startSide\":\"RIGHT\"}]}",
		"Use verdict=actionable whenever there is any actionable advice.",
		"Prefer inline comments for specific code-level feedback when you can anchor them confidently to the diff using the changed file path and file line numbers shown in the PR diff.",
		"Use top-level comments without path/line only for architectural, cross-cutting, or otherwise unanchorable feedback.",
		"For multiline inline comments, startLine/startSide must identify the first line and line/side the last line; omit startLine/startSide for single-line comments.",
		"Write substantially more detail than a brief summary; every comment should explain the problem, why it matters, and the concrete change to make.",
		"Do not approve. If the review is clean, return verdict=clean with comments=[].",
	)
	return agent.AppendCompletionInstruction(strings.Join(parts, "\n\n"))
}

func parseReviewFeedback(result AgentResult) parsedReviewFeedback {
	output := extractReviewOutput(result.Stdout)
	if structured, ok := parseStructuredReviewOutput(output); ok {
		return structured
	}
	fallback := strings.TrimSpace(firstNonEmpty(result.Summary, summarizeLogs(result.Stdout)))
	if fallback == "" {
		return parsedReviewFeedback{}
	}
	return parsedReviewFeedback{Body: fallback, Comments: []reviewFeedbackComment{{Body: fallback}}, Clean: false}
}

func extractReviewOutput(stdout string) string {
	lines := make([]string, 0)
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, agent.CompletionMarkerPrefix) {
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func parseStructuredReviewOutput(output string) (parsedReviewFeedback, bool) {
	if strings.TrimSpace(output) == "" {
		return parsedReviewFeedback{}, false
	}
	var payload struct {
		Verdict  string                  `json:"verdict"`
		Body     string                  `json:"body"`
		Comments []reviewFeedbackComment `json:"comments"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		return parsedReviewFeedback{}, false
	}
	comments := make([]reviewFeedbackComment, 0, len(payload.Comments))
	for _, comment := range payload.Comments {
		normalized := normalizeReviewFeedbackComment(comment)
		if normalized.Body != "" {
			comments = append(comments, normalized)
		}
	}
	clean := strings.EqualFold(payload.Verdict, "clean") && len(comments) == 0
	if !clean && len(comments) == 0 {
		return parsedReviewFeedback{}, false
	}
	return parsedReviewFeedback{Body: strings.TrimSpace(payload.Body), Comments: comments, Clean: clean}, true
}

func normalizeReviewFeedbackComment(comment reviewFeedbackComment) reviewFeedbackComment {
	comment.Body = strings.TrimSpace(comment.Body)
	comment.Path = strings.TrimSpace(comment.Path)
	comment.Side = strings.ToUpper(strings.TrimSpace(comment.Side))
	comment.StartSide = strings.ToUpper(strings.TrimSpace(comment.StartSide))
	if comment.Body == "" {
		return reviewFeedbackComment{}
	}
	if (comment.Path != "" && (comment.Line <= 0 || (comment.Side != "LEFT" && comment.Side != "RIGHT"))) || (comment.Path == "" && (comment.Line > 0 || comment.Side != "" || comment.StartLine > 0 || comment.StartSide != "")) {
		return reviewFeedbackComment{Body: comment.Body}
	}
	if (comment.StartLine > 0 || comment.StartSide != "") && (comment.StartLine <= 0 || (comment.StartSide != "LEFT" && comment.StartSide != "RIGHT")) {
		return reviewFeedbackComment{Body: comment.Body}
	}
	if comment.StartLine > 0 && comment.Line > 0 && comment.StartLine >= comment.Line {
		return reviewFeedbackComment{Body: comment.Body}
	}
	return comment
}

func normalizePendingReviewCheckpoint(pending pendingReviewCheckpoint) pendingReviewCheckpoint {
	if pending.PublishState == nil {
		pending.PublishState = &publishState{}
	}
	if pending.Comments == nil {
		pending.Comments = []reviewFeedbackComment{}
	}
	return pending
}

func (p pendingReviewCheckpoint) clone() *pendingReviewCheckpoint {
	copyValue := p
	copyValue.Comments = append([]reviewFeedbackComment(nil), p.Comments...)
	if p.PublishState != nil {
		stateCopy := *p.PublishState
		copyValue.PublishState = &stateCopy
	}
	return &copyValue
}

func summarizeLogs(stdout string) string {
	lines := make([]string, 0)
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, agent.CompletionMarkerPrefix) {
			continue
		}
		lines = append(lines, trimmed)
	}
	return strings.Join(lines, "\n")
}

func hasInlineComments(comments []reviewFeedbackComment) bool {
	for _, comment := range comments {
		if comment.Path != "" && comment.Line > 0 && comment.Side != "" {
			return true
		}
	}
	return false
}

func inlineComments(comments []reviewFeedbackComment) []reviewFeedbackComment {
	result := make([]reviewFeedbackComment, 0)
	for _, comment := range comments {
		if comment.Path != "" && comment.Line > 0 && comment.Side != "" {
			result = append(result, comment)
		}
	}
	return result
}

func topLevelComments(comments []reviewFeedbackComment) []reviewFeedbackComment {
	result := make([]reviewFeedbackComment, 0)
	for _, comment := range comments {
		if comment.Path == "" || comment.Line <= 0 || comment.Side == "" {
			result = append(result, comment)
		}
	}
	return result
}

func toGitHubReviewComments(comments []reviewFeedbackComment) []ReviewComment {
	result := make([]ReviewComment, 0, len(comments))
	for _, comment := range comments {
		result = append(result, ReviewComment{Body: comment.Body, Path: comment.Path, Line: comment.Line, Side: comment.Side, StartLine: comment.StartLine, StartSide: comment.StartSide})
	}
	return result
}

func downgradeInlineCommentToTopLevel(comment reviewFeedbackComment) reviewFeedbackComment {
	location := comment.Path
	if comment.StartLine > 0 && comment.Line > 0 && comment.StartLine != comment.Line {
		location = fmt.Sprintf("%s:%d-%d", comment.Path, comment.StartLine, comment.Line)
	} else if comment.Line > 0 {
		location = fmt.Sprintf("%s:%d", comment.Path, comment.Line)
	}
	if location == "" {
		return reviewFeedbackComment{Body: comment.Body}
	}
	return reviewFeedbackComment{Body: fmt.Sprintf("Inline comment fallback (%s):\n\n%s", location, comment.Body)}
}

func isInlineReviewAnchorFailure(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "validation failed") && strings.Contains(message, "pull request review thread") && (strings.Contains(message, "line") || strings.Contains(message, "path") || strings.Contains(message, "diff") || strings.Contains(message, "side"))
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

func ternaryReviewEvent(clean bool) ReviewEvent {
	if clean {
		return ReviewEventApprove
	}
	return ReviewEventComment
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

func resolvePullRequestPhase(labels []string) string {
	if specpr.ResolvePullRequestPhase(labels) == specpr.PhaseSpec {
		return "spec"
	}
	return "implementation"
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
