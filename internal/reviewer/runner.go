package reviewer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/powerformer/looper/internal/agent"
	"github.com/powerformer/looper/internal/bootstrap"
	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/diffanchor"
	"github.com/powerformer/looper/internal/disclosure"
	"github.com/powerformer/looper/internal/eventlog"
	"github.com/powerformer/looper/internal/infra/specpr"
	"github.com/powerformer/looper/internal/storage"
	"github.com/powerformer/looper/internal/version"
)

const (
	stepDiscover ReviewerStep = "discover"
	stepFilter   ReviewerStep = "filter"
	stepClaim    ReviewerStep = "claim"
	stepSnapshot ReviewerStep = "snapshot"
	stepWorktree ReviewerStep = "worktree"
	stepReview   ReviewerStep = "review"
	stepPublish  ReviewerStep = "publish"
)

var reviewerStepSequence = []ReviewerStep{
	stepDiscover,
	stepFilter,
	stepClaim,
	stepSnapshot,
	stepWorktree,
	stepReview,
	stepPublish,
}

var reviewMarkerCommentPattern = regexp.MustCompile(`(?is)<!--\s*looper:review\b.*?-->`)

type ReviewerStep string

type ReviewEvent string

const (
	ReviewEventApprove        ReviewEvent = "APPROVE"
	ReviewEventComment        ReviewEvent = "COMMENT"
	ReviewEventRequestChanges ReviewEvent = "REQUEST_CHANGES"
	reviewEventAgentNative    ReviewEvent = "AGENT_NATIVE"
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
	HeadRefName    string
	BaseRefName    string
	Author         string
	ReviewRequests []string
	ChecksSummary  string
	Diff           string
	Comments       []map[string]any
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
	Ref             string
	ExpectedHeadSHA string
	Remote          string
}

type PrepareWorktreeResult struct {
	HeadSHA string
	Clean   bool
}

type CleanupWorktreeInput struct {
	ProjectID         string
	RepoPath          string
	WorktreePath      string
	Branch            string
	ProtectedBranches []string
}

type transientFailure interface{ Temporary() bool }

type TransientCommandError struct{ Err error }

func (e TransientCommandError) Error() string   { return e.Err.Error() }
func (e TransientCommandError) Unwrap() error   { return e.Err }
func (e TransientCommandError) Temporary() bool { return true }

type ListOpenPullRequestsInput struct {
	Repo   string
	CWD    string
	Limit  int
	Label  string
	Labels []string
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

type VerifyReviewMarkerInput struct {
	Repo                string
	PRNumber            int64
	Marker              string
	AllowedReviewEvents []ReviewEvent
	AuthorLogin         string
	CWD                 string
}

type ReviewMarkerResult struct {
	Found               bool
	Outcome             string
	Event               ReviewEvent
	AuthorLogin         string
	Body                string
	InlineCommentBodies []string
}

type PullRequestReactionInput struct {
	Repo     string
	PRNumber int64
	Content  string
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
	FindReviewMarker(context.Context, VerifyReviewMarkerInput) (ReviewMarkerResult, error)
	AddPullRequestReaction(context.Context, PullRequestReactionInput) error
	RemovePullRequestReaction(context.Context, PullRequestReactionInput) error
	AddPullRequestLabels(context.Context, PullRequestLabelsInput) error
	RemovePullRequestLabels(context.Context, PullRequestLabelsInput) error
}

type GitGateway interface {
	CreateWorktree(context.Context, CreateWorktreeInput) (CreateWorktreeResult, error)
	PrepareWorktree(context.Context, PrepareWorktreeInput) (PrepareWorktreeResult, error)
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
	AllowAutoApprove        bool
	ReviewEvents            config.ReviewerReviewEventsConfig
	LoopConfig              config.ReviewerLoopConfig
	DiscoveryPolicy         DiscoveryPolicy
	Scope                   config.ReviewerScope
	DetectDuplicateFindings bool
	Disclosure              *config.DisclosureConfig
	CustomInstructions      *config.Config
	AgentRuntime            string
	AgentModel              *string
	LooperCLIPath           string
	RetryBaseDelay          time.Duration
	RetryMaxAttempts        int64
	OnAgentExecutionStarted AgentExecutionStartedFunc
}

type DiscoveryPolicy struct {
	AutoDiscovery             bool
	IncludeDrafts             bool
	RequireReviewRequest      bool
	Labels                    []string
	LabelMode                 config.LabelMode
	IncludeSpecReviewingLabel bool
	SpecReviewingLabel        string
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
	allowAutoApprove        bool
	reviewEvents            config.ReviewerReviewEventsConfig
	loopConfig              config.ReviewerLoopConfig
	discoveryPolicy         DiscoveryPolicy
	scope                   config.ReviewerScope
	detectDuplicateFindings bool
	disclosure              config.DisclosureConfig
	customInstructions      config.Config
	agentRuntime            string
	agentModel              string
	looperCLIPath           string
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
	Worktree       *checkpointWorktree      `json:"worktree,omitempty"`
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
	HeadRefName    string   `json:"headRefName,omitempty"`
	BaseRefName    string   `json:"baseRefName,omitempty"`
	Author         string   `json:"author,omitempty"`
	ReviewRequests []string `json:"reviewRequests,omitempty"`
}

type checkpointWorktree struct {
	Path       string `json:"path,omitempty"`
	Branch     string `json:"branch,omitempty"`
	BaseBranch string `json:"baseBranch,omitempty"`
	HeadSHA    string `json:"headSha,omitempty"`
	PreparedAt string `json:"preparedAt,omitempty"`
	CleanedAt  string `json:"cleanedAt,omitempty"`
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

type pendingReviewCheckpoint struct {
	HeadSHA                  string      `json:"headSha,omitempty"`
	IdempotencyKey           string      `json:"idempotencyKey,omitempty"`
	Event                    ReviewEvent `json:"event,omitempty"`
	Summary                  string      `json:"summary,omitempty"`
	ContentFingerprint       string      `json:"contentFingerprint,omitempty"`
	MarkerVerificationMisses int         `json:"markerVerificationMisses,omitempty"`
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
	disclosureCfg := config.DefaultDisclosureConfig()
	if options.Disclosure != nil {
		disclosureCfg = *options.Disclosure
	}
	loopConfig := options.LoopConfig
	if loopConfig.MaxIterationsPerPR == 0 {
		loopConfig = config.ReviewerLoopConfig{EnabledByDefault: false, QuietPeriodSeconds: 120, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 14400, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25, StopOnApproved: true, StopOnReadyLabel: true, StopOnIdenticalOutput: true}
	}
	scope := options.Scope
	if scope == "" {
		scope = config.ReviewerScopeChangedRanges
	}
	reviewEvents := options.ReviewEvents
	if reviewEvents.Clean == "" {
		reviewEvents.Clean = config.ReviewerReviewEventComment
	}
	if reviewEvents.Blocking == "" {
		reviewEvents.Blocking = config.ReviewerReviewEventComment
	}
	if options.AllowAutoApprove && options.ReviewEvents.Clean == "" {
		reviewEvents.Clean = config.ReviewerReviewEventApprove
	}
	policy := options.DiscoveryPolicy
	if !policy.AutoDiscovery && !policy.IncludeDrafts && !policy.RequireReviewRequest && len(policy.Labels) == 0 && policy.LabelMode == "" && !policy.IncludeSpecReviewingLabel && policy.SpecReviewingLabel == "" {
		policy = DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, RequireReviewRequest: true, Labels: []string{}, LabelMode: config.LabelModeAll, IncludeSpecReviewingLabel: true, SpecReviewingLabel: specpr.ReviewingLabel}
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
		allowAutoApprove:        options.AllowAutoApprove,
		reviewEvents:            reviewEvents,
		loopConfig:              loopConfig,
		discoveryPolicy:         policy,
		scope:                   scope,
		detectDuplicateFindings: options.DetectDuplicateFindings,
		disclosure:              disclosureCfg,
		customInstructions:      customInstructionConfig(options.CustomInstructions),
		agentRuntime:            strings.TrimSpace(options.AgentRuntime),
		agentModel:              derefString(options.AgentModel),
		looperCLIPath:           normalizeLooperCLIPath(options.LooperCLIPath),
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
	openPRs, err := r.listOpenPullRequestsForDiscovery(ctx, input.Repo, project.RepoPath, input.Limit)
	if err != nil {
		return DiscoveryResult{}, err
	}
	specPRs := []PullRequestSummary{}
	if r.discoveryPolicy.IncludeSpecReviewingLabel {
		var err error
		specPRs, err = r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: input.Repo, CWD: project.RepoPath, Limit: input.Limit, Label: r.discoveryPolicy.SpecReviewingLabel})
		if err != nil {
			return DiscoveryResult{}, err
		}
	}
	currentLogin := ""
	if r.discoveryPolicy.RequireReviewRequest {
		var err error
		currentLogin, err = r.github.GetCurrentUserLogin(ctx, project.RepoPath)
		if err != nil {
			return DiscoveryResult{}, err
		}
		currentLogin = normalizeLogin(currentLogin)
	}
	result := DiscoveryResult{}
	seen := map[string]struct{}{}
	enqueue := func(pr PullRequestSummary, existing *storage.LoopRecord) error {
		loopResult, loopErr := r.ensureLoopForPullRequest(ctx, *project, input.Repo, pr.Number, existing)
		if loopErr != nil {
			return loopErr
		}
		if terminalReviewerLoopReason(loopResult.record) != "" {
			result.Skipped++
			return nil
		}
		if loopResult.created {
			result.CreatedLoopIDs = append(result.CreatedLoopIDs, loopResult.record.ID)
		}
		meta := parseJSONObject(loopResult.record.MetadataJSON)
		_, hasPublishedHead := stringFromAny(meta["lastPublishedHeadSha"])
		if !r.loopEnabled(meta) && (!loopResult.created || hasPublishedHead) {
			result.Skipped++
			return nil
		}
		if last, ok := stringFromAny(meta["lastPublishedHeadSha"]); ok && last == pr.HeadSHA && pr.HeadSHA != "" {
			result.Skipped++
			return nil
		}
		availableAt := r.now()
		if _, reviewed := stringFromAny(meta["lastPublishedHeadSha"]); reviewed && r.loopConfig.QuietPeriodSeconds > 0 {
			availableAt = availableAt.Add(time.Duration(r.loopConfig.QuietPeriodSeconds) * time.Second)
		}
		queueItem, queueErr := r.enqueue(ctx, enqueueInput{
			ProjectID:   project.ID,
			LoopID:      loopResult.record.ID,
			Repo:        input.Repo,
			PRNumber:    pr.Number,
			HeadSHA:     pr.HeadSHA,
			AvailableAt: availableAt,
		})
		if queueErr != nil {
			return queueErr
		}
		if err := r.markLoopQueuedForReview(ctx, loopResult.record, queueItem.AvailableAt); err != nil {
			return err
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
		if !r.prEligibleForDiscovery(pr, currentLogin) {
			result.Skipped++
			continue
		}
		if err := seenEnqueue(pr); err != nil {
			return DiscoveryResult{}, err
		}
	}
	for _, pr := range specPRs {
		if !r.prEligibleForDiscovery(pr, currentLogin) {
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
		detail, viewErr := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: *loop.PRNumber, CWD: project.RepoPath})
		if viewErr != nil {
			result.Skipped++
			continue
		}
		if !r.discoveryPolicy.IncludeDrafts && detail.IsDraft {
			result.Skipped++
			continue
		}
		if normalizePRState(detail.State) != "open" {
			if err := r.terminateLoop(ctx, loop, "pr_closed_or_merged"); err != nil {
				return DiscoveryResult{}, err
			}
			result.Skipped++
			continue
		}
		if !isManualReviewerLoop(loop) && r.discoveryPolicy.RequireReviewRequest && !isCurrentUserRequested(detail.ReviewRequests, currentLogin) {
			result.Skipped++
			continue
		}
		if !isManualReviewerLoop(loop) && !labelsMatch(detail.Labels, r.discoveryPolicy.Labels, r.discoveryPolicy.LabelMode) {
			result.Skipped++
			continue
		}
		seen[key] = struct{}{}
		if err := enqueue(summaryFromDetail(detail), &loop); err != nil {
			return DiscoveryResult{}, err
		}
	}
	return result, nil
}

func (r *Runner) listOpenPullRequestsForDiscovery(ctx context.Context, repo, cwd string, limit int) ([]PullRequestSummary, error) {
	labels := prQueryLabels(r.discoveryPolicy.Labels)
	effectiveLimit := defaultDiscoveryLimit(limit)
	if len(labels) == 0 {
		return r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: limit})
	}
	if len(labels) == 1 {
		return r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: limit, Label: labels[0]})
	}
	if r.discoveryPolicy.LabelMode == config.LabelModeAll {
		return r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: limit, Labels: labels})
	}

	result := []PullRequestSummary{}
	seen := map[int64]struct{}{}
	for _, label := range labels {
		if len(result) >= effectiveLimit {
			break
		}
		prs, err := r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: effectiveLimit, Label: label})
		if err != nil {
			return nil, err
		}
		for _, pr := range prs {
			if _, ok := seen[pr.Number]; ok {
				continue
			}
			seen[pr.Number] = struct{}{}
			result = append(result, pr)
			if len(result) >= effectiveLimit {
				break
			}
		}
	}
	return result, nil
}

func defaultDiscoveryLimit(limit int) int {
	if limit <= 0 {
		return 30
	}
	return limit
}

func (r *Runner) prEligibleForDiscovery(pr PullRequestSummary, currentLogin string) bool {
	if !r.discoveryPolicy.IncludeDrafts && pr.IsDraft {
		return false
	}
	if normalizePRState(pr.State) != "open" {
		return false
	}
	if r.discoveryPolicy.RequireReviewRequest && !isCurrentUserRequested(pr.ReviewRequests, currentLogin) {
		return false
	}
	if !labelsMatch(pr.Labels, r.discoveryPolicy.Labels, r.discoveryPolicy.LabelMode) {
		return false
	}
	return true
}

func prQueryLabels(labels []string) []string {
	result := []string{}
	seen := map[string]struct{}{}
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, label)
	}
	return result
}

func labelsMatch(labels []string, required []string, mode config.LabelMode) bool {
	if len(required) == 0 {
		return true
	}
	if mode == config.LabelModeAny {
		for _, label := range required {
			if specpr.HasLabel(labels, label) {
				return true
			}
		}
		return false
	}
	for _, label := range required {
		if !specpr.HasLabel(labels, label) {
			return false
		}
	}
	return true
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
	if reason := terminalReviewerLoopReason(*loop); reason != "" {
		cancelReason := "loop_" + reason
		if _, err := r.repos.Queue.CancelByLoop(ctx, loop.ID, r.nowISO(), &cancelReason); err != nil {
			return ProcessResult{}, err
		}
		summary := fmt.Sprintf("Skipped terminal reviewer loop %s: %s", loop.ID, reason)
		return ProcessResult{LoopID: loop.ID, QueueItemID: queueItem.ID, Status: "skipped", Summary: summary}, nil
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
		metadataJSON, metaErr := r.recordLoopRunStartMetadata(updated.MetadataJSON)
		if metaErr == nil {
			updated.MetadataJSON = &metadataJSON
		}
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
			if checkpoint.ResumePolicy == "rerun_review" || hasPendingReviewMarkerMiss(checkpoint) {
				latest = checkpoint
			}
			if checkpoint.ResumePolicy == "restart_from_discover" {
				latest.ResumePolicy = checkpoint.ResumePolicy
			}
			resumePolicy := latest.ResumePolicy
			switch failure.kind {
			case FailureRetryableAfterResume:
				if resumePolicy != "restart_from_discover" && resumePolicy != "rerun_review" {
					resumePolicy = "advance_from_checkpoint"
				}
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
			terminalFailure := false
			_, loopErr := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
				updated.LastRunAt = stringPtr(r.nowISO())
				metadataJSON, metaErr := r.recordLoopFailureMetadata(updated.MetadataJSON, failure.message)
				if metaErr == nil {
					updated.MetadataJSON = &metadataJSON
				}
				if terminalReviewerLoopReason(*updated) == "failed" {
					terminalFailure = true
					updated.Status = "failed"
					updated.NextRunAt = nil
				} else if updated.Status == "paused" {
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
			if terminalFailure && failedQueue != nil && failedQueue.Status == "queued" {
				if err := r.repos.Queue.Fail(ctx, storage.QueueFailInput{ID: failedQueue.ID, FinishedAt: r.nowISO(), ErrorMessage: optionalString(failure.message), ErrorKind: string(failure.kind), UpdatedAt: r.nowISO()}); err != nil {
					return ProcessResult{}, err
				}
				failedQueue.Status = "failed"
			}
			if failedQueue == nil || failedQueue.Status != "queued" {
				r.cleanupReviewerWorktreeIfTerminal(context.Background(), *project, &latest)
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
	updatedLoop, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
		metadataJSON, metaErr := r.recordLoopSuccessMetadata(updated.MetadataJSON, checkpoint, summary)
		if metaErr == nil {
			updated.MetadataJSON = &metadataJSON
		}
		updated.Status = r.loopSuccessStatus(updated.Status, updated.MetadataJSON, checkpoint.SkipReason)
		updated.LastRunAt = stringPtr(r.nowISO())
		updated.NextRunAt = nil
	})
	if err != nil {
		return ProcessResult{}, err
	}
	if reason := terminalReviewerLoopReason(updatedLoop); reason != "" {
		if _, err := r.repos.Queue.CancelByLoop(ctx, updatedLoop.ID, r.nowISO(), &reason); err != nil {
			return ProcessResult{}, err
		}
	}
	r.cleanupReviewerWorktreeIfTerminal(context.Background(), *project, &checkpoint)
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
		return r.runFilterStep(ctx, input)
	case stepClaim:
		return r.runClaimStep(ctx, input)
	case stepSnapshot:
		return r.runSnapshotStep(ctx, input)
	case stepWorktree:
		return r.runPrepareWorktreeStep(ctx, input)
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
	checkpoint.Detail = &checkpointDetail{Title: detail.Title, State: detail.State, IsDraft: detail.IsDraft, ReviewDecision: detail.ReviewDecision, Labels: cloneStrings(detail.Labels), HeadSHA: detail.HeadSHA, BaseSHA: detail.BaseSHA, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, Author: detail.Author, ReviewRequests: cloneStrings(detail.ReviewRequests)}
	checkpoint.ResumePolicy = "replay_step"
	return checkpoint, nil
}

func (r *Runner) runFilterStep(ctx context.Context, input stepInput) (reviewerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.Detail == nil {
		return checkpoint, &loopError{message: "Missing PR detail checkpoint for filter step", kind: FailureRetryableTransient}
	}
	if !r.discoveryPolicy.IncludeDrafts && checkpoint.Detail.IsDraft {
		checkpoint.SkipReason = fmt.Sprintf("Skipped draft pull request %s#%d", input.Repo, input.PRNumber)
		return checkpoint, nil
	}
	if normalizePRState(checkpoint.Detail.State) != "open" {
		checkpoint.SkipReason = fmt.Sprintf("Skipped non-open pull request %s#%d", input.Repo, input.PRNumber)
		if err := r.terminateLoop(ctx, input.Loop, "pr_closed_or_merged"); err != nil {
			return checkpoint, err
		}
		return checkpoint, nil
	}
	if !isManualReviewerLoop(input.Loop) && r.loopConfig.StopOnApproved && strings.EqualFold(strings.TrimSpace(checkpoint.Detail.ReviewDecision), "APPROVED") {
		checkpoint.SkipReason = fmt.Sprintf("Terminated reviewer loop for approved pull request %s#%d", input.Repo, input.PRNumber)
		if err := r.terminateLoop(ctx, input.Loop, "approved"); err != nil {
			return checkpoint, err
		}
		return checkpoint, nil
	}
	if !isManualReviewerLoop(input.Loop) && r.loopConfig.StopOnReadyLabel && specpr.HasLabel(checkpoint.Detail.Labels, specpr.ReadyLabel) {
		checkpoint.SkipReason = fmt.Sprintf("Terminated reviewer loop for ready pull request %s#%d", input.Repo, input.PRNumber)
		if err := r.terminateLoop(ctx, input.Loop, "ready_label"); err != nil {
			return checkpoint, err
		}
		return checkpoint, nil
	}
	meta := parseJSONObject(input.Loop.MetadataJSON)
	if last, ok := stringFromAny(meta["lastPublishedHeadSha"]); ok && checkpoint.Detail.HeadSHA != "" && last == checkpoint.Detail.HeadSHA {
		checkpoint.SkipReason = fmt.Sprintf("Skipped already-reviewed head %s for %s#%d", checkpoint.Detail.HeadSHA, input.Repo, input.PRNumber)
		return checkpoint, nil
	}
	if r.loopEnabled(meta) {
		if reason := r.loopBudgetTerminationReason(input.Loop, checkpoint.Detail.HeadSHA); reason != "" {
			checkpoint.SkipReason = fmt.Sprintf("Terminated reviewer loop for %s#%d: %s", input.Repo, input.PRNumber, reason)
			if err := r.terminateLoop(ctx, input.Loop, reason); err != nil {
				return checkpoint, err
			}
			return checkpoint, nil
		}
	}
	if !isManualReviewerLoop(input.Loop) && r.discoveryPolicy.RequireReviewRequest {
		currentLogin, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableTransient}
		}
		if !isCurrentUserRequested(checkpoint.Detail.ReviewRequests, normalizeLogin(currentLogin)) {
			checkpoint.SkipReason = fmt.Sprintf("Skipped pull request %s#%d because current user is not requested for review", input.Repo, input.PRNumber)
			return checkpoint, nil
		}
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

func (r *Runner) runPrepareWorktreeStep(ctx context.Context, input stepInput) (reviewerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if reviewerWorktreePrepared(checkpoint) {
		return checkpoint, nil
	}
	if r.git == nil {
		return checkpoint, fmt.Errorf("reviewer git gateway is not configured")
	}
	if checkpoint.Detail == nil {
		return checkpoint, &loopError{message: "Missing PR detail checkpoint for worktree step", kind: FailureRetryableTransient}
	}
	if checkpoint.Snapshot == nil {
		return checkpoint, &loopError{message: "Missing PR snapshot checkpoint for worktree step", kind: FailureRetryableTransient}
	}
	branch := reviewerWorktreeBranch(input.PRNumber, checkpoint)
	baseBranch := firstNonEmpty(strings.TrimSpace(checkpoint.Detail.BaseRefName), derefString(input.Project.BaseBranch), "main")
	prRef := pullRequestHeadRef(input.PRNumber)
	projectMetadata := parseJSONObject(input.Project.MetadataJSON)
	worktreeRoot, _ := stringFromAny(projectMetadata["worktreeRoot"])
	if worktreeRoot == "" {
		resolvedRoot, err := config.DefaultProjectWorktreeRoot(input.Project.ID, input.Project.RepoPath)
		if err != nil {
			return checkpoint, err
		}
		worktreeRoot = resolvedRoot
	}
	protectedBranches := make([]string, 0, 2)
	if candidate := strings.TrimSpace(checkpoint.Detail.BaseRefName); candidate != "" {
		protectedBranches = append(protectedBranches, candidate)
	}
	if candidate := strings.TrimSpace(derefString(input.Project.BaseBranch)); candidate != "" {
		protectedBranches = append(protectedBranches, candidate)
	}
	created, err := r.git.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: input.Project.ID, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, Branch: branch, BaseBranch: baseBranch, PRNumber: input.PRNumber, ProtectedBranches: protectedBranches, CheckoutMode: "detached"})
	if err != nil {
		return checkpoint, err
	}
	checkpoint.Worktree = &checkpointWorktree{Path: created.WorktreePath, Branch: branch, BaseBranch: baseBranch}
	if input.Run.ID != "" {
		step := asReviewerStep(derefString(input.Run.CurrentStep))
		if step == "" {
			step = stepWorktree
		}
		if err := r.persistCheckpoint(ctx, input.Run.ID, step, checkpoint); err != nil {
			return checkpoint, err
		}
	}
	prepared, err := r.git.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: created.WorktreePath, Branch: branch, Ref: prRef, ExpectedHeadSHA: checkpoint.Snapshot.HeadSHA})
	if err != nil {
		return checkpoint, err
	}
	if !prepared.Clean {
		return checkpoint, &loopError{message: fmt.Sprintf("Reviewer worktree is dirty for branch %s; manual intervention required", branch), kind: FailureManualIntervention}
	}
	checkpoint.Worktree.HeadSHA = prepared.HeadSHA
	checkpoint.Worktree.PreparedAt = r.nowISO()
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runReviewStep(ctx context.Context, input stepInput) (reviewerCheckpoint, error) {
	checkpoint := input.Checkpoint
	var err error
	if checkpoint.PendingReview != nil {
		return checkpoint, nil
	}
	if checkpoint.Snapshot == nil {
		return checkpoint, &loopError{message: "Missing PR snapshot checkpoint for review step", kind: FailureRetryableTransient}
	}
	if !reviewerWorktreePrepared(checkpoint) {
		checkpoint, err = r.runPrepareWorktreeStep(ctx, input)
		if err != nil {
			return input.Checkpoint, err
		}
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepReview, checkpoint); err != nil {
			return checkpoint, err
		}
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	executionID := eventlog.NewEventID("agent")
	idempotencyKey := agentNativeReviewID(input.Loop.ID, checkpoint.Snapshot.HeadSHA)
	prompt, instructionBlock := buildReviewPromptWithInstructions(input.Project.ID, r.customInstructions, input.Repo, input.PRNumber, checkpoint, input.Run.ID, idempotencyKey, r.effectiveReviewEvents(input.Loop.MetadataJSON), isManualReviewerLoop(input.Loop), r.scope, r.disclosure, r.agentRuntime, r.agentModel, r.looperCLIPath)
	metadata := map[string]any{"loopType": "reviewer", "repo": input.Repo, "prNumber": input.PRNumber}
	for key, value := range config.CustomInstructionMetadata(instructionBlock, prompt) {
		metadata[key] = value
	}
	execution, err := r.agentExecutor.Start(ctx, AgentRunInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Prompt: prompt, WorkingDirectory: worktree.Path, Timeout: r.agentTimeout, Metadata: metadata, IdempotencyKey: idempotencyKey})
	if err != nil {
		return checkpoint, err
	}
	if err := r.recordAgentExecutionStarted(ctx, input.Loop.ID); err != nil {
		r.logWarn("reviewer agent execution start metadata update failed", map[string]any{"loopId": input.Loop.ID, "runId": input.Run.ID, "error": err.Error()})
	}
	if r.onAgentExecutionStarted != nil {
		if err := r.onAgentExecutionStarted(ctx, AgentExecutionStartedInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Subtitle: fmt.Sprintf("%s#%d", input.Repo, input.PRNumber), Body: "Review started", DedupeKey: "runtime.agent.started:reviewer:" + input.Run.ID}); err != nil && r.logger != nil {
			r.logger.Warn("reviewer agent start notification failed", map[string]any{"loopId": input.Loop.ID, "runId": input.Run.ID, "error": err.Error()})
		}
	}
	result, err := execution.Wait(ctx)
	if err != nil {
		return checkpoint, err
	}
	if result.Status != "completed" {
		if reason, ok := r.detectHeadChangeRequired(ctx, input, checkpoint); ok {
			checkpoint.ResumePolicy = "restart_from_discover"
			return checkpoint, &loopError{message: reason, kind: FailureRetryableAfterResume}
		}
		if found, err := r.verifyAgentNativeReviewMarker(ctx, input, checkpoint.Snapshot.HeadSHA, idempotencyKey); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		} else if found.Found {
			checkpoint.PendingReview = &pendingReviewCheckpoint{HeadSHA: checkpoint.Snapshot.HeadSHA, IdempotencyKey: idempotencyKey, Event: reviewEventAgentNative, Summary: result.Summary, ContentFingerprint: reviewMarkerFingerprint(found)}
			checkpoint.ResumePolicy = "advance_from_checkpoint"
			return checkpoint, nil
		}
		if reason, ok := r.detectRediscoveryRequired(ctx, input, checkpoint); ok {
			checkpoint.ResumePolicy = "restart_from_discover"
			return checkpoint, &loopError{message: reason, kind: FailureRetryableAfterResume}
		}
		message := firstNonEmpty(result.Summary, result.Stderr, fmt.Sprintf("Reviewer agent %s", result.Status))
		kind := FailureRetryableTransient
		if agent.IsAgentSetupFailureMessage(message) {
			kind = FailureManualIntervention
		}
		return checkpoint, &loopError{message: message, kind: kind}
	}
	if result.ParseStatus != "parsed" {
		if found, err := r.verifyAgentNativeReviewMarker(ctx, input, checkpoint.Snapshot.HeadSHA, idempotencyKey); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		} else if found.Found {
			checkpoint.PendingReview = &pendingReviewCheckpoint{HeadSHA: checkpoint.Snapshot.HeadSHA, IdempotencyKey: idempotencyKey, Event: reviewEventAgentNative, Summary: result.Summary, ContentFingerprint: reviewMarkerFingerprint(found)}
			checkpoint.ResumePolicy = "advance_from_checkpoint"
			return checkpoint, nil
		}
		return checkpoint, &loopError{message: "Reviewer agent did not report a valid completion marker after publishing review", kind: FailureNonRetryable}
	}
	checkpoint.PendingReview = &pendingReviewCheckpoint{HeadSHA: checkpoint.Snapshot.HeadSHA, IdempotencyKey: idempotencyKey, Event: reviewEventAgentNative, Summary: result.Summary}
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
	pending := *checkpoint.PendingReview
	meta := parseJSONObject(input.Loop.MetadataJSON)
	if last, ok := stringFromAny(meta["lastPublishedHeadSha"]); ok && last == pending.HeadSHA {
		checkpoint.SkipReason = fmt.Sprintf("Skipped already-published review for head %s", pending.HeadSHA)
		return checkpoint, nil
	}
	repo := input.Repo
	prNumber := input.PRNumber
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	if detail.HeadSHA != "" && pending.HeadSHA != "" && detail.HeadSHA != pending.HeadSHA {
		return checkpoint, &loopError{message: fmt.Sprintf("PR head changed before publish: expected %s, got %s", pending.HeadSHA, detail.HeadSHA), kind: FailureRetryableAfterResume}
	}
	markerResult := ReviewMarkerResult{}
	if pending.Event == reviewEventAgentNative {
		found, err := r.verifyAgentNativeReviewMarker(ctx, input, pending.HeadSHA, pending.IdempotencyKey)
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		markerResult = found
	} else {
		checkpoint.PendingReview = nil
		checkpoint.ResumePolicy = "rerun_review"
		return checkpoint, &loopError{message: "Legacy pending review checkpoint cannot be verified; rerunning review before marking publish success", kind: FailureRetryableAfterResume}
	}
	if !isManualReviewerLoop(input.Loop) && r.discoveryPolicy.RequireReviewRequest {
		currentLogin, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		if !isCurrentUserRequested(detail.ReviewRequests, normalizeLogin(currentLogin)) && !markerResult.Found {
			checkpoint.SkipReason = fmt.Sprintf("Skipped pull request %s#%d because current user is not requested for review", repo, prNumber)
			return checkpoint, nil
		}
	}
	if !markerResult.Found {
		if pending.MarkerVerificationMisses == 0 {
			pending.MarkerVerificationMisses = 1
			checkpoint.PendingReview = pending.clone()
			checkpoint.ResumePolicy = "advance_from_checkpoint"
			return checkpoint, &loopError{message: "Reviewer agent completed but no matching GitHub review marker was found; retrying marker verification before rerunning review", kind: FailureRetryableAfterResume}
		}
		checkpoint.PendingReview = nil
		checkpoint.ResumePolicy = "rerun_review"
		return checkpoint, &loopError{message: "Reviewer agent completed but no matching GitHub review marker was found", kind: FailureRetryableAfterResume}
	}
	checkpoint.PendingReview = pending.clone()
	if checkpoint.PendingReview.ContentFingerprint == "" {
		if fp := reviewMarkerFingerprint(markerResult); fp != "" {
			checkpoint.PendingReview.ContentFingerprint = fp
		}
	}
	if err := r.applyVerifiedReviewSideEffects(ctx, input, checkpoint, detail, markerResult); err != nil {
		return checkpoint, err
	}
	if err := r.recordPublishedReviewProgress(ctx, input, pending, pendingReviewEvent(pending)); err != nil {
		return checkpoint, err
	}
	return checkpoint, nil
}

func (r *Runner) verifyAgentNativeReviewMarker(ctx context.Context, input stepInput, headSHA string, idempotencyKey string) (ReviewMarkerResult, error) {
	currentLogin, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
	if err != nil {
		return ReviewMarkerResult{}, err
	}
	marker := agentNativeReviewMarker(input.Loop.ID, headSHA, idempotencyKey)
	return r.github.FindReviewMarker(ctx, VerifyReviewMarkerInput{Repo: input.Repo, PRNumber: input.PRNumber, Marker: marker, AllowedReviewEvents: r.allowedReviewEventsForPolicy(r.effectiveReviewEvents(input.Loop.MetadataJSON)), AuthorLogin: currentLogin, CWD: input.Project.RepoPath})
}

func (r *Runner) applyVerifiedReviewSideEffects(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint, detail PullRequestDetail, marker ReviewMarkerResult) error {
	if !marker.Found {
		return &loopError{message: "Cannot apply review side effects without a verified review marker", kind: FailureRetryableAfterResume}
	}
	outcome := strings.ToLower(strings.TrimSpace(marker.Outcome))
	reaction := PullRequestReactionInput{Repo: input.Repo, PRNumber: input.PRNumber, Content: "+1", CWD: input.Project.RepoPath}
	switch outcome {
	case "clean":
		if err := r.github.AddPullRequestReaction(ctx, reaction); err != nil {
			return &loopError{message: fmt.Sprintf("Failed to add clean-review reaction before marking publish success: %v", err), kind: FailureRetryableAfterResume}
		}
		specReviewingLabel := r.specReviewingLabel()
		checkpointHadSpecReviewing := specpr.HasLabel(detailLabels(checkpoint.Detail), specReviewingLabel)
		reviewEvents := r.effectiveReviewEvents(input.Loop.MetadataJSON)
		if reviewEvents.Clean == config.ReviewerReviewEventApprove && marker.Event == ReviewEventApprove && (checkpointHadSpecReviewing || specpr.HasLabel(detail.Labels, specReviewingLabel)) {
			freshDetail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
			if err != nil {
				return &loopError{message: fmt.Sprintf("Failed to refresh pull request review state before spec-ready transition: %v", err), kind: FailureRetryableAfterResume}
			}
			if detail.HeadSHA != "" && freshDetail.HeadSHA != "" && detail.HeadSHA != freshDetail.HeadSHA {
				return &loopError{message: fmt.Sprintf("PR head changed before spec-ready transition: expected %s, got %s", detail.HeadSHA, freshDetail.HeadSHA), kind: FailureRetryableAfterResume}
			}
			detail = freshDetail
			if !specpr.IsReviewClean(detail.ReviewDecision, detail.Comments) {
				return nil
			}
			if specpr.HasLabel(detail.Labels, specReviewingLabel) {
				if err := r.github.RemovePullRequestLabels(ctx, PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: []string{specReviewingLabel}, CWD: input.Project.RepoPath}); err != nil {
					return &loopError{message: fmt.Sprintf("Failed to remove spec-reviewing label before marking publish success: %v", err), kind: FailureRetryableAfterResume}
				}
			}
			if !specpr.HasLabel(detail.Labels, specpr.ReadyLabel) {
				if err := r.github.AddPullRequestLabels(ctx, PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: []string{specpr.ReadyLabel}, CWD: input.Project.RepoPath}); err != nil {
					return &loopError{message: fmt.Sprintf("Failed to add spec-ready label before marking publish success: %v", err), kind: FailureRetryableAfterResume}
				}
			}
		}
	case "actionable", "non_blocking", "blocking":
		if err := r.github.RemovePullRequestReaction(ctx, reaction); err != nil {
			return &loopError{message: fmt.Sprintf("Failed to remove stale clean-review reaction before marking publish success: %v", err), kind: FailureRetryableAfterResume}
		}
	default:
		return &loopError{message: "Verified review marker is missing outcome=clean|non_blocking|blocking|actionable; cannot validate review side effects", kind: FailureRetryableAfterResume}
	}
	return nil
}

func (r *Runner) specReviewingLabel() string {
	if label := strings.TrimSpace(r.discoveryPolicy.SpecReviewingLabel); label != "" {
		return label
	}
	return specpr.ReviewingLabel
}

func pendingReviewEvent(pending pendingReviewCheckpoint) ReviewEvent {
	if pending.Event != "" {
		return pending.Event
	}
	return reviewEventAgentNative
}

func hasPendingReviewMarkerMiss(checkpoint reviewerCheckpoint) bool {
	return checkpoint.PendingReview != nil && checkpoint.PendingReview.MarkerVerificationMisses > 0
}

func (r *Runner) allowedAgentNativeReviewEvents() []ReviewEvent {
	return r.allowedReviewEventsForPolicy(r.reviewEvents)
}

func (r *Runner) allowedReviewEventsForPolicy(policy config.ReviewerReviewEventsConfig) []ReviewEvent {
	events := []ReviewEvent{ReviewEventComment}
	if policy.Clean == config.ReviewerReviewEventApprove {
		events = append(events, ReviewEventApprove)
	}
	if policy.Blocking == config.ReviewerReviewEventRequestChanges {
		events = append(events, ReviewEventRequestChanges)
	}
	return events
}

func (r *Runner) effectiveReviewEvents(metadataJSON *string) config.ReviewerReviewEventsConfig {
	policy := r.reviewEvents
	meta := parseJSONObject(metadataJSON)
	if reviewEvents, ok := meta["reviewEvents"].(map[string]any); ok {
		if clean, ok := stringFromAny(reviewEvents["clean"]); ok && strings.TrimSpace(clean) != "" {
			if isValidCleanReviewEvent(clean) {
				policy.Clean = config.ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(clean)))
			}
		}
		if blocking, ok := stringFromAny(reviewEvents["blocking"]); ok && strings.TrimSpace(blocking) != "" {
			if isValidBlockingReviewEvent(blocking) {
				policy.Blocking = config.ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(blocking)))
			}
		}
	}
	return policy
}

func isValidCleanReviewEvent(value string) bool {
	switch config.ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(value))) {
	case config.ReviewerReviewEventComment, config.ReviewerReviewEventApprove:
		return true
	default:
		return false
	}
}

func isValidBlockingReviewEvent(value string) bool {
	switch config.ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(value))) {
	case config.ReviewerReviewEventComment, config.ReviewerReviewEventRequestChanges:
		return true
	default:
		return false
	}
}

func (r *Runner) recordPublishedReviewProgress(ctx context.Context, input stepInput, pending pendingReviewCheckpoint, reviewEvent ReviewEvent) error {
	if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
		metadataJSON, err := mergeLoopMetadataJSON(updated.MetadataJSON, map[string]any{"lastPublishedHeadSha": pending.HeadSHA, "lastReviewEvent": string(reviewEvent), "lastReviewSummary": pending.Summary, "lastPublishedAt": r.nowISO()})
		if err == nil {
			updated.MetadataJSON = stringPtr(metadataJSON)
		}
	}); err != nil {
		return err
	}
	r.appendEvent(ctx, eventInput{eventType: "pr.review.posted", projectID: input.Project.ID, loopID: input.Loop.ID, runID: input.Run.ID, entityType: "pull_request", entityID: fmt.Sprintf("%s#%d", input.Repo, input.PRNumber), payload: map[string]any{"repo": input.Repo, "prNumber": input.PRNumber, "event": string(reviewEvent), "headSha": pending.HeadSHA}})
	return nil
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
	rerunReview := false
	if latestRun != nil {
		failureSummary := firstNonEmpty(derefString(latestRun.Summary), derefString(latestRun.ErrorMessage))
		restartFromDiscover = checkpoint.ResumePolicy == "restart_from_discover" || shouldRestartFromDiscover(latestRun.Status, failedStep, failureSummary)
		rerunReview = checkpoint.ResumePolicy == "rerun_review"
	}
	startStep := stepDiscover
	if latestRun != nil && (latestRun.Status == "failed" || latestRun.Status == "interrupted") {
		if restartFromDiscover {
			startStep = stepDiscover
		} else if rerunReview && !isManualReviewerLoop(loop) {
			startStep = stepDiscover
		} else if rerunReview {
			startStep = stepReview
		} else if lastCompleted != "" {
			if next := nextReviewerStep(lastCompleted); next != "" {
				startStep = next
			}
		}
	}
	if startStep != stepDiscover && !isManualReviewerLoop(loop) && needsReviewerEligibilityRediscovery(checkpoint, startStep) {
		startStep = stepDiscover
		restartFromDiscover = true
	}
	resumed := latestRun != nil && (latestRun.Status == "failed" || latestRun.Status == "interrupted") && startStep != stepDiscover
	initialCheckpoint := reviewerCheckpoint{ResumePolicy: "replay_step"}
	if resumed {
		initialCheckpoint = checkpoint
		if restartFromDiscover {
			initialCheckpoint = reviewerCheckpoint{ResumePolicy: "replay_step"}
		} else {
			initialCheckpoint.ResumePolicy = "advance_from_checkpoint"
			if startStep == stepReview && initialCheckpoint.Worktree != nil {
				initialCheckpoint.Worktree.PreparedAt = ""
			}
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
		if terminalReviewerLoopReason(*existing) != "" {
			return loopUpsertResult{record: *existing, created: false}, nil
		}
		updated := *existing
		metadataJSONSource := updated.MetadataJSON
		meta := parseJSONObject(updated.MetadataJSON)
		if loopEnabledMetadataMissing(meta) {
			meta["followUpdates"] = false
			encoded, err := json.Marshal(meta)
			if err != nil {
				return loopUpsertResult{}, err
			}
			text := string(encoded)
			metadataJSONSource = &text
		}
		metadataJSON, err := r.ensureLoopMetadataJSON(metadataJSONSource, repo, prNumber)
		if err != nil {
			return loopUpsertResult{}, err
		}
		updated.MetadataJSON = &metadataJSON
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
	metadataJSON, err := r.ensureLoopMetadataJSON(nil, repo, prNumber)
	if err != nil {
		return loopUpsertResult{}, err
	}
	loop := storage.LoopRecord{ID: eventlog.NewEventID("loop"), Seq: seq, ProjectID: project.ID, Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "queued", NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &metadataJSON}
	if err := r.repos.Loops.Upsert(ctx, loop); err != nil {
		return loopUpsertResult{}, err
	}
	r.appendEvent(ctx, eventInput{eventType: "loop.created", projectID: project.ID, loopID: loop.ID, entityType: "loop", entityID: loop.ID, payload: map[string]any{"type": "reviewer", "repo": repo, "prNumber": prNumber}})
	return loopUpsertResult{record: loop, created: true}, nil
}

func (r *Runner) markLoopQueuedForReview(ctx context.Context, loop storage.LoopRecord, availableAt string) error {
	if terminalReviewerLoopReason(loop) != "" {
		return nil
	}
	_, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		if active, activeErr := r.hasActiveRunningRun(ctx, updated.ID); activeErr == nil && active {
			updated.Status = "running"
			updated.NextRunAt = nil
			return
		}
		updated.Status = "queued"
		updated.NextRunAt = stringPtr(availableAt)
	})
	return err
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
		if loop.Type != "reviewer" || loop.ProjectID != projectID || derefString(loop.Repo) != repo || loop.PRNumber == nil || loop.Status == "paused" || loop.Status == "failed" || terminalReviewerLoopReason(loop) != "" {
			continue
		}
		meta := parseJSONObject(loop.MetadataJSON)
		if r.loopEnabled(meta) {
			result = append(result, loop)
		}
	}
	return result, nil
}

func isManualReviewerLoop(loop storage.LoopRecord) bool {
	meta := parseJSONObject(loop.MetadataJSON)
	manual, _ := meta["manual"].(bool)
	return manual
}

func needsReviewerEligibilityRediscovery(checkpoint reviewerCheckpoint, startStep ReviewerStep) bool {
	if checkpoint.PendingReview != nil && (startStep == stepReview || startStep == stepPublish) {
		return false
	}
	return checkpoint.Detail == nil || checkpoint.Detail.ReviewRequests == nil
}

type enqueueInput struct {
	ProjectID   string
	LoopID      string
	Repo        string
	PRNumber    int64
	HeadSHA     string
	AvailableAt time.Time
}

func (r *Runner) enqueue(ctx context.Context, input enqueueInput) (storage.QueueItemRecord, error) {
	dedupeKey := buildReviewerDedupeKey(input.ProjectID, input.LoopID, input.Repo, input.PRNumber)
	existing, err := r.repos.Queue.FindActiveByDedupe(ctx, dedupeKey)
	if err != nil {
		return storage.QueueItemRecord{}, err
	}
	availableAt := r.nowISO()
	if !input.AvailableAt.IsZero() {
		availableAt = eventlog.FormatJavaScriptISOString(input.AvailableAt.UTC())
	}
	payloadJSON := ""
	if strings.TrimSpace(input.HeadSHA) != "" {
		payload, err := json.Marshal(map[string]any{"headSha": input.HeadSHA})
		if err != nil {
			return storage.QueueItemRecord{}, err
		}
		payloadJSON = string(payload)
	}
	if existing != nil {
		if existing.Status == "queued" && strings.TrimSpace(input.HeadSHA) != "" {
			existingPayload := parseJSONObject(existing.PayloadJSON)
			existingHeadSHA, _ := stringFromAny(existingPayload["headSha"])
			if existingHeadSHA != input.HeadSHA && isoTimeAfter(availableAt, existing.AvailableAt) {
				updated := *existing
				updated.AvailableAt = availableAt
				updated.UpdatedAt = r.nowISO()
				updated.PayloadJSON = &payloadJSON
				if err := r.repos.Queue.Upsert(ctx, updated); err != nil {
					return storage.QueueItemRecord{}, err
				}
				return updated, nil
			}
		}
		return *existing, nil
	}
	nowISO := r.nowISO()
	targetID := fmt.Sprintf("pr:%s:%d", input.Repo, input.PRNumber)
	lockKey := fmt.Sprintf("pr:%s:%d", input.Repo, input.PRNumber)
	projectID := input.ProjectID
	loopID := input.LoopID
	queueItem := storage.QueueItemRecord{ID: eventlog.NewEventID("queue"), ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: targetID, Repo: &input.Repo, PRNumber: &input.PRNumber, DedupeKey: dedupeKey, Priority: storage.QueuePriorityReviewer, Status: "queued", AvailableAt: availableAt, Attempts: 0, MaxAttempts: r.retryMaxAttempts, LockKey: &lockKey, CreatedAt: nowISO, UpdatedAt: nowISO}
	if payloadJSON != "" {
		queueItem.PayloadJSON = &payloadJSON
	}
	if err := r.repos.Queue.Upsert(ctx, queueItem); err != nil {
		return storage.QueueItemRecord{}, err
	}
	return queueItem, nil
}

func isoTimeAfter(candidate, current string) bool {
	candidateTime, candidateErr := time.Parse(time.RFC3339Nano, candidate)
	currentTime, currentErr := time.Parse(time.RFC3339Nano, current)
	if candidateErr == nil && currentErr == nil {
		return candidateTime.After(currentTime)
	}
	return candidate > current
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
	if failedStep != stepPublish && failedStep != stepReview {
		return false
	}
	return strings.Contains(failureSummary, "PR head changed before publish") || strings.Contains(failureSummary, "review request removed before publish")
}

func (r *Runner) detectRediscoveryRequired(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint) (string, bool) {
	if checkpoint.Snapshot == nil {
		return "", false
	}
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return "", false
	}
	if detail.HeadSHA != "" && checkpoint.Snapshot.HeadSHA != "" && detail.HeadSHA != checkpoint.Snapshot.HeadSHA {
		return fmt.Sprintf("PR head changed before publish: expected %s, got %s", checkpoint.Snapshot.HeadSHA, detail.HeadSHA), true
	}
	if isManualReviewerLoop(input.Loop) || !r.discoveryPolicy.RequireReviewRequest {
		return "", false
	}
	currentLogin, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
	if err != nil {
		return "", false
	}
	if !isCurrentUserRequested(detail.ReviewRequests, normalizeLogin(currentLogin)) {
		return "review request removed before publish", true
	}
	return "", false
}

func (r *Runner) detectHeadChangeRequired(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint) (string, bool) {
	if checkpoint.Snapshot == nil {
		return "", false
	}
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return "", false
	}
	if detail.HeadSHA != "" && checkpoint.Snapshot.HeadSHA != "" && detail.HeadSHA != checkpoint.Snapshot.HeadSHA {
		return fmt.Sprintf("PR head changed before publish: expected %s, got %s", checkpoint.Snapshot.HeadSHA, detail.HeadSHA), true
	}
	return "", false
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

func (r *Runner) loopEnabled(meta map[string]any) bool {
	if enabled, ok := meta["followUpdates"].(bool); ok {
		return enabled
	}
	if loopMeta, ok := meta["loop"].(map[string]any); ok {
		if enabled, ok := loopMeta["enabled"].(bool); ok {
			return enabled
		}
	}
	return false
}

func loopEnabledMetadataMissing(meta map[string]any) bool {
	if _, ok := meta["followUpdates"].(bool); ok {
		return false
	}
	if loopMeta, ok := meta["loop"].(map[string]any); ok {
		if _, ok := loopMeta["enabled"].(bool); ok {
			return false
		}
	}
	return true
}

func (r *Runner) ensureLoopMetadataJSON(current *string, repo string, prNumber int64) (string, error) {
	meta := parseJSONObject(current)
	loopMeta, _ := meta["loop"].(map[string]any)
	if loopMeta == nil {
		loopMeta = map[string]any{}
	}
	if _, ok := meta["followUpdates"].(bool); !ok {
		if enabled, ok := loopMeta["enabled"].(bool); ok {
			meta["followUpdates"] = enabled
		} else {
			meta["followUpdates"] = r.loopConfig.EnabledByDefault
		}
	}
	if _, ok := loopMeta["enabled"].(bool); !ok {
		loopMeta["enabled"] = r.loopEnabled(meta)
	}
	if _, ok := loopMeta["status"].(string); !ok {
		loopMeta["status"] = "active"
	}
	if _, ok := loopMeta["startTime"].(string); !ok {
		loopMeta["startTime"] = r.nowISO()
	}
	loopMeta["repo"] = repo
	loopMeta["prNumber"] = prNumber
	loopMeta["scope"] = string(r.scope)
	loopMeta["quietPeriodSeconds"] = r.loopConfig.QuietPeriodSeconds
	loopMeta["maxIterationsPerPR"] = r.loopConfig.MaxIterationsPerPR
	loopMeta["maxIterationsPerHead"] = r.loopConfig.MaxIterationsPerHead
	loopMeta["maxWallClockSeconds"] = r.loopConfig.MaxWallClockSeconds
	loopMeta["maxConsecutiveFailures"] = r.loopConfig.MaxConsecutiveFailures
	loopMeta["maxAgentExecutionsPerPR"] = r.loopConfig.MaxAgentExecutionsPerPR
	reviewEventsRaw, hasReviewEvents := meta["reviewEvents"]
	reviewEventsMeta, _ := reviewEventsRaw.(map[string]any)
	if hasReviewEvents && reviewEventsMeta == nil {
		return "", fmt.Errorf("reviewEvents must be a JSON object")
	}
	if reviewEventsMeta == nil {
		reviewEventsMeta = map[string]any{}
	}
	if cleanRaw, present := reviewEventsMeta["clean"]; present {
		clean, ok := cleanRaw.(string)
		if !ok {
			return "", fmt.Errorf("reviewEvents.clean must be COMMENT or APPROVE")
		}
		if !isValidCleanReviewEvent(clean) {
			return "", fmt.Errorf("reviewEvents.clean must be COMMENT or APPROVE")
		}
	} else {
		reviewEventsMeta["clean"] = string(r.reviewEvents.Clean)
	}
	if blockingRaw, present := reviewEventsMeta["blocking"]; present {
		blocking, ok := blockingRaw.(string)
		if !ok {
			return "", fmt.Errorf("reviewEvents.blocking must be COMMENT or REQUEST_CHANGES")
		}
		if !isValidBlockingReviewEvent(blocking) {
			return "", fmt.Errorf("reviewEvents.blocking must be COMMENT or REQUEST_CHANGES")
		}
	} else {
		reviewEventsMeta["blocking"] = string(r.reviewEvents.Blocking)
	}
	meta["reviewEvents"] = reviewEventsMeta
	meta["loop"] = loopMeta
	encoded, err := json.Marshal(meta)
	return string(encoded), err
}

func (r *Runner) recordLoopRunStartMetadata(current *string) (string, error) {
	meta := parseJSONObject(current)
	loopMeta := reviewerLoopMetadata(meta)
	loopMeta["status"] = "active"
	loopMeta["lastStatus"] = "running"
	meta["loop"] = loopMeta
	encoded, err := json.Marshal(meta)
	return string(encoded), err
}

func (r *Runner) recordAgentExecutionStarted(ctx context.Context, loopID string) error {
	current, err := r.repos.Loops.GetByID(ctx, loopID)
	if err != nil || current == nil {
		return err
	}
	_, err = r.updateLoop(ctx, storage.LoopRecord{ID: loopID}, func(updated *storage.LoopRecord) {
		meta := parseJSONObject(updated.MetadataJSON)
		loopMeta := reviewerLoopMetadata(meta)
		loopMeta["agentExecutionCount"] = intFromAny(loopMeta["agentExecutionCount"]) + 1
		meta["loop"] = loopMeta
		if encoded, marshalErr := json.Marshal(meta); marshalErr == nil {
			text := string(encoded)
			updated.MetadataJSON = &text
		}
	})
	return err
}

func (r *Runner) loopSuccessStatus(currentStatus string, metadataJSON *string, skipReason string) string {
	meta := parseJSONObject(metadataJSON)
	loopMeta := reviewerLoopMetadata(meta)
	if status, ok := loopMeta["status"].(string); ok && isTerminalReviewerLoopStatus(status) {
		return status
	}
	if isTerminalReviewerLoopStatus(currentStatus) {
		return currentStatus
	}
	if r.loopEnabled(meta) && skipReason == "" {
		return "waiting"
	}
	return "completed"
}

func isTerminalReviewerLoopStatus(status string) bool {
	switch status {
	case "terminated", "failed", "paused", "stopped":
		return true
	default:
		return false
	}
}

func terminalReviewerLoopReason(loop storage.LoopRecord) string {
	if isTerminalReviewerLoopStatus(loop.Status) {
		return loop.Status
	}
	meta := parseJSONObject(loop.MetadataJSON)
	loopMeta := reviewerLoopMetadata(meta)
	if status, ok := loopMeta["status"].(string); ok && isTerminalReviewerLoopStatus(status) {
		return status
	}
	return ""
}

func (r *Runner) recordLoopFailureMetadata(current *string, message string) (string, error) {
	meta := parseJSONObject(current)
	loopMeta := reviewerLoopMetadata(meta)
	failures := intFromAny(loopMeta["failureCount"])
	consecutive := intFromAny(loopMeta["consecutiveFailures"])
	loopMeta["failureCount"] = failures + 1
	loopMeta["consecutiveFailures"] = consecutive + 1
	loopMeta["lastStatus"] = "failed"
	loopMeta["lastFailure"] = message
	if consecutive+1 >= r.loopConfig.MaxConsecutiveFailures {
		loopMeta["status"] = "failed"
		loopMeta["terminationReason"] = "max_consecutive_failures"
	}
	meta["loop"] = loopMeta
	encoded, err := json.Marshal(meta)
	return string(encoded), err
}

func (r *Runner) recordLoopSuccessMetadata(current *string, checkpoint reviewerCheckpoint, summary string) (string, error) {
	meta := parseJSONObject(current)
	loopMeta := reviewerLoopMetadata(meta)
	head := ""
	if checkpoint.Snapshot != nil {
		head = checkpoint.Snapshot.HeadSHA
	}
	iterations := intFromAny(loopMeta["iterationCount"])
	loopMeta["lastStatus"] = "success"
	loopMeta["consecutiveFailures"] = 0
	reviewCompleted := checkpoint.SkipReason == "" && checkpoint.PendingReview != nil
	if reviewCompleted {
		loopMeta["iterationCount"] = iterations + 1
	}
	if reviewCompleted && head != "" {
		loopMeta["lastReviewedHeadSha"] = head
		byHead, _ := loopMeta["iterationsByHead"].(map[string]any)
		if byHead == nil {
			byHead = map[string]any{}
		}
		byHead[head] = intFromAny(byHead[head]) + 1
		loopMeta["iterationsByHead"] = byHead
	}
	fp := ""
	if reviewCompleted {
		fp = loopSuccessOutputFingerprint(checkpoint, summary)
	}
	if fp != "" {
		previous, _ := loopMeta["lastOutputFingerprint"].(string)
		if previous == fp && r.loopConfig.StopOnIdenticalOutput {
			loopMeta["identicalOutputCount"] = intFromAny(loopMeta["identicalOutputCount"]) + 1
			loopMeta["terminationReason"] = "identical_output"
			loopMeta["status"] = "terminated"
		} else {
			loopMeta["identicalOutputCount"] = 1
		}
		loopMeta["lastOutputFingerprint"] = fp
		fingerprints, _ := loopMeta["publishedFindingFingerprints"].([]any)
		if !containsAnyString(fingerprints, fp) {
			loopMeta["publishedFindingFingerprints"] = append(fingerprints, fp)
		} else if r.detectDuplicateFindings {
			loopMeta["duplicateFindingsDetected"] = intFromAny(loopMeta["duplicateFindingsDetected"]) + 1
		}
	}
	if loopMeta["terminationReason"] == nil {
		loopMeta["status"] = "waiting"
	}
	meta["loop"] = loopMeta
	encoded, err := json.Marshal(meta)
	return string(encoded), err
}

func loopSuccessOutputFingerprint(checkpoint reviewerCheckpoint, summary string) string {
	if checkpoint.PendingReview != nil {
		if fp := strings.TrimSpace(checkpoint.PendingReview.ContentFingerprint); fp != "" {
			return fp
		}
		if fp := normalizedFindingFingerprint(checkpoint.PendingReview.Summary); fp != "" {
			return fp
		}
	}
	return normalizedFindingFingerprint(summary)
}

func reviewMarkerFingerprint(found ReviewMarkerResult) string {
	parts := make([]string, 0, 1+len(found.InlineCommentBodies))
	if strings.TrimSpace(found.Body) != "" {
		parts = append(parts, found.Body)
	}
	for _, body := range found.InlineCommentBodies {
		if strings.TrimSpace(body) != "" {
			parts = append(parts, body)
		}
	}
	return normalizedFindingFingerprint(strings.Join(parts, "\n"))
}

func (r *Runner) loopBudgetTerminationReason(loop storage.LoopRecord, headSHA string) string {
	meta := parseJSONObject(loop.MetadataJSON)
	loopMeta := reviewerLoopMetadata(meta)
	if intFromAny(loopMeta["iterationCount"]) >= r.loopConfig.MaxIterationsPerPR {
		return "max_iterations_per_pr"
	}
	if intFromAny(loopMeta["agentExecutionCount"]) >= r.loopConfig.MaxAgentExecutionsPerPR {
		return "max_agent_executions_per_pr"
	}
	if intFromAny(loopMeta["consecutiveFailures"]) >= r.loopConfig.MaxConsecutiveFailures {
		return "max_consecutive_failures"
	}
	if headSHA != "" {
		byHead, _ := loopMeta["iterationsByHead"].(map[string]any)
		if intFromAny(byHead[headSHA]) >= r.loopConfig.MaxIterationsPerHead {
			return "max_iterations_per_head"
		}
	}
	if start, ok := loopMeta["startTime"].(string); ok && start != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, start); err == nil && r.now().Sub(parsed) >= time.Duration(r.loopConfig.MaxWallClockSeconds)*time.Second {
			return "max_wall_clock"
		}
	}
	return ""
}

func (r *Runner) terminateLoop(ctx context.Context, loop storage.LoopRecord, reason string) error {
	_, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		updated.Status = "terminated"
		updated.NextRunAt = nil
		meta := parseJSONObject(updated.MetadataJSON)
		loopMeta := reviewerLoopMetadata(meta)
		loopMeta["status"] = "terminated"
		loopMeta["terminationReason"] = reason
		loopMeta["lastStatus"] = "terminated"
		meta["loop"] = loopMeta
		if encoded, marshalErr := json.Marshal(meta); marshalErr == nil {
			text := string(encoded)
			updated.MetadataJSON = &text
		}
	})
	if err != nil {
		return err
	}
	_, err = r.repos.Queue.CancelByLoop(ctx, loop.ID, r.nowISO(), &reason)
	return err
}

func reviewerLoopMetadata(meta map[string]any) map[string]any {
	loopMeta, _ := meta["loop"].(map[string]any)
	if loopMeta == nil {
		loopMeta = map[string]any{}
	}
	return loopMeta
}

func intFromAny(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func normalizedFindingFingerprint(text string) string {
	text = reviewMarkerCommentPattern.ReplaceAllString(text, "")
	normalized := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.ToLower(normalized)))
	return hex.EncodeToString(sum[:])
}

func containsAnyString(values []any, target string) bool {
	for _, value := range values {
		if text, ok := value.(string); ok && text == target {
			return true
		}
	}
	return false
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

func buildPullRequestLockKey(item storage.QueueItemRecord) string {
	if item.Repo == nil || item.PRNumber == nil {
		return ""
	}
	return fmt.Sprintf("pr:%s:%d", *item.Repo, *item.PRNumber)
}

func buildReviewPrompt(repo string, prNumber int64, checkpoint reviewerCheckpoint, runID string, idempotencyKey string, reviewEvents config.ReviewerReviewEventsConfig, manual bool, scope config.ReviewerScope, disclosureCfg config.DisclosureConfig, agentRuntime string, agentModel string, looperCLIPath string) string {
	cfg, _ := config.Normalize("")
	cfg.Instructions.Enabled = false
	prompt, _ := buildReviewPromptWithInstructions("", cfg, repo, prNumber, checkpoint, runID, idempotencyKey, reviewEvents, manual, scope, disclosureCfg, agentRuntime, agentModel, looperCLIPath)
	return prompt
}

func buildReviewPromptWithInstructions(projectID string, instructionConfig config.Config, repo string, prNumber int64, checkpoint reviewerCheckpoint, runID string, idempotencyKey string, reviewEvents config.ReviewerReviewEventsConfig, manual bool, scope config.ReviewerScope, disclosureCfg config.DisclosureConfig, agentRuntime string, agentModel string, looperCLIPath string) (string, config.CustomInstructionBlock) {
	looperCLIPath = normalizeLooperCLIPath(looperCLIPath)
	looperCLICommand := shellQuote(looperCLIPath)
	phase := resolvePullRequestPhase(detailLabels(checkpoint.Detail))
	phaseInstruction := "This is an implementation review. Focus on code correctness, safety, tests, and maintainability."
	if phase == "spec" {
		phaseInstruction = "This is a spec review. Focus on scope, correctness, feasibility, risks, and validation. Do not review implementation details beyond whether the spec is actionable."
	}
	publishInstruction := "You must publish the GitHub review yourself by calling looper's enforced review-submit wrapper from the shell. Do not return review JSON for looper to parse; looper will not parse review content or post GitHub comments for you after the agent exits."
	if looperCLIPath == "" {
		publishInstruction = "A trusted Looper CLI review-submit wrapper is unavailable for this run, so do not publish a GitHub review yourself; exit non-zero with the exact message `trusted looper review submit wrapper unavailable`."
	}
	parts := []string{fmt.Sprintf("Review pull request %s#%d.", repo, prNumber), "Phase: " + phase, phaseInstruction, reviewerScopeInstruction(scope), publishInstruction, fmt.Sprintf("Review idempotency marker prefix: <!-- looper:review id=%s head=%s outcome=clean|non_blocking|blocking -->", idempotencyKey, snapshotHeadSHA(checkpoint)), "Use outcome=clean only when there are no blocking or non-blocking findings, outcome=non_blocking for actionable feedback that should not block merge, and outcome=blocking for findings that should block merge. Legacy outcome=actionable may be treated as comment-only compatibility, but prefer non_blocking or blocking.", "Run ID for logging only, not for idempotency: " + runID}
	if checkpoint.Detail != nil && len(checkpoint.Detail.Labels) > 0 {
		parts = append(parts, "Current labels: "+strings.Join(checkpoint.Detail.Labels, ", "))
	}
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
			parts = append(parts, diffanchor.Parse(diff).FormatPromptSection(80))
			parts = append(parts, "Diff:\n"+diff)
		}
	}
	instructionBlock := config.BuildCustomInstructionBlock(instructionConfig, projectID, "reviewer")
	if instructionBlock.Text != "" {
		parts = append(parts, instructionBlock.Text)
	}
	approveInstruction := "Submit clean reviews as COMMENT."
	blockingInstruction := "Submit blocking and non-blocking finding reviews as COMMENT."
	specLabelInstruction := "Do not transition spec-review labels when clean reviews are submitted as COMMENT."
	policyFlags := fmt.Sprintf("--clean-review-event %s --blocking-review-event %s", reviewEvents.Clean, reviewEvents.Blocking)
	reviewSubmitCommand := fmt.Sprintf("`%s review submit %s#%d --event COMMENT --commit-id %s %s`", looperCLICommand, repo, prNumber, snapshotHeadSHA(checkpoint), policyFlags)
	if reviewEvents.Clean == config.ReviewerReviewEventApprove {
		approveInstruction = "If the review is clean, submit an APPROVE review."
		specLabelInstruction = "If this is a clean spec review and the PR currently has label `looper:spec-reviewing`, use `gh` to remove `looper:spec-reviewing` and add `looper:spec-ready` after the APPROVE review is posted. Do not change labels for COMMENT, non-blocking, or blocking finding reviews."
		reviewSubmitCommand = fmt.Sprintf("`%s review submit %s#%d --event COMMENT --commit-id %s %s` for finding reviews or `%s review submit %s#%d --event APPROVE --commit-id %s %s` for clean reviews", looperCLICommand, repo, prNumber, snapshotHeadSHA(checkpoint), policyFlags, looperCLICommand, repo, prNumber, snapshotHeadSHA(checkpoint), policyFlags)
	}
	if reviewEvents.Blocking == config.ReviewerReviewEventRequestChanges {
		blockingInstruction = "If the review has blocking findings, submit REQUEST_CHANGES with outcome=blocking. If findings are non-blocking, submit COMMENT with outcome=non_blocking. Never submit REQUEST_CHANGES for non-blocking findings."
		if reviewEvents.Clean == config.ReviewerReviewEventApprove {
			reviewSubmitCommand = fmt.Sprintf("`%s review submit %s#%d --event COMMENT --commit-id %s %s` for non-blocking findings, `%s review submit %s#%d --event REQUEST_CHANGES --commit-id %s %s` for blocking findings, or `%s review submit %s#%d --event APPROVE --commit-id %s %s` for clean reviews", looperCLICommand, repo, prNumber, snapshotHeadSHA(checkpoint), policyFlags, looperCLICommand, repo, prNumber, snapshotHeadSHA(checkpoint), policyFlags, looperCLICommand, repo, prNumber, snapshotHeadSHA(checkpoint), policyFlags)
		} else {
			reviewSubmitCommand = fmt.Sprintf("`%s review submit %s#%d --event COMMENT --commit-id %s %s` for clean or non-blocking reviews, or `%s review submit %s#%d --event REQUEST_CHANGES --commit-id %s %s` for blocking findings", looperCLICommand, repo, prNumber, snapshotHeadSHA(checkpoint), policyFlags, looperCLICommand, repo, prNumber, snapshotHeadSHA(checkpoint), policyFlags)
		}
	}
	existingMarkerEventInstruction := "Only treat an existing marker as satisfying idempotency when that marker is on a COMMENTED PR review. Ignore matching markers on APPROVED reviews and post a new COMMENT review instead."
	if reviewEvents.Clean == config.ReviewerReviewEventApprove || reviewEvents.Blocking == config.ReviewerReviewEventRequestChanges {
		existingMarkerEventInstruction = "Only treat an existing marker as satisfying idempotency when that marker is on a COMMENTED, APPROVED, or CHANGES_REQUESTED PR review allowed by this run's review event policy."
	}
	reviewRequestInstruction := "Before posting, confirm the current GitHub user is still requested for review. If not requested, do not post a review; exit non-zero with the exact message `review request removed before publish`."
	if manual {
		reviewRequestInstruction = "This is a manual reviewer run, so a current-user review request is not required before posting."
	}
	githubOperationContract := fmt.Sprintf("GitHub operation contract: submit exactly one PR review for this run through the trusted Looper CLI at %s, with the review JSON on stdin. The wrapper validates inline anchors against the live PR diff before it calls GitHub; do not use PATH-based `looper`, repository-local `go run ./cmd/looper`, `gh api repos/%s/pulls/%d/reviews`, or `gh pr review` directly for the review submission.", reviewSubmitCommand, repo, prNumber)
	submitPayloadInstruction := fmt.Sprintf("When submitting through `%s review submit`, pass stdin JSON with `body` and optional `comments` entries using GitHub's review comment fields: `path`, `line`, `side` (`RIGHT` for new diff lines, `LEFT` for old diff lines), optional `start_line` and `start_side` for multiline ranges, and `body` for the actionable feedback.", looperCLICommand)
	if looperCLIPath == "" {
		githubOperationContract = "GitHub operation contract: a trusted Looper CLI path was not detected for this reviewer run, so you cannot safely publish a GitHub review. Do not call PATH-based `looper`, repository-local `go run ./cmd/looper`, `gh api repos/.../pulls/.../reviews`, or `gh pr review` directly; exit non-zero with the exact message `trusted looper review submit wrapper unavailable`."
		submitPayloadInstruction = ""
	}
	parts = append(parts,
		"Idempotency requirement: before posting anything, use `gh api` to list existing PR reviews for this PR. If any existing review body already contains `looper:review id=` with the idempotency id and `head=` with the expected head SHA, do not post another review. Instead, inspect the marker's `outcome` and review state: outcome=clean means ensure +1 reaction and spec-ready label transition only when the matching review is APPROVED; outcome=non_blocking, outcome=blocking, or legacy outcome=actionable means remove any stale +1 reaction from the current user. Then exit successfully after printing the normal completion marker.",
		existingMarkerEventInstruction,
		githubOperationContract,
		"Before posting, use `gh` to confirm the PR is still open and the head SHA still matches the expected head SHA. If it changed, do not post a review and exit non-zero with the exact message `PR head changed before publish`.",
		reviewRequestInstruction,
		"Review body style contract: the visible body must be human-authored review prose only. Never post terminal/tool output, ANSI escape sequences, file-read traces, command logs, JSON parsing artifacts, or your internal scratch work as the GitHub review body. If you do not have concrete prose yet, write a concise clean LGTM body or exit non-zero instead of posting logs.",
		"Every review body you post must include exactly one stable idempotency marker with id, head, and outcome fields: `<!-- looper:review id=... head=... outcome=clean|non_blocking|blocking -->`.",
		reviewDisclosureInstruction(disclosureCfg, agentRuntime, agentModel),
		"Before posting, validate every inline review comment's `path`, `line`, `side`, `start_line`, and `start_side` against the full PR diff's anchorable locations. Use ANCHORABLE DIFF LOCATIONS as a summary of known ranges, but if that summary is truncated, the full PR diff remains authoritative. Preserve exact anchors that fit the full diff. If an otherwise useful comment is outside the full diff's anchorable locations, safely downgrade it to top-level review body feedback that starts with clear fallback location text instead of submitting an invalid inline anchor.",
		fmt.Sprintf("For clean reviews, also add a +1 reaction to the PR main conversation with `gh api repos/%s/issues/%d/reactions --method POST -H 'Accept: application/vnd.github+json' -f content=+1`.", repo, prNumber),
		"For non-blocking or blocking finding reviews, use `gh` to remove any existing +1 reaction from the current GitHub user on the PR main conversation so stale clean signals do not remain after a new head needs changes.",
		specLabelInstruction,
		approveInstruction,
		blockingInstruction,
		"Prefer 3 deeply specific comments over 10 shallow comments. If there is no concrete actionable feedback, post a clean review. Do not invent feedback.",
		"Every comment MUST include: (1) a location via inline anchor or exact file/section/symbol reference, (2) the concrete problem, (3) why it matters, (4) evidence from the changed lines or spec section, and (5) a specific suggested change.",
		"Prefer inline comments for specific code-level feedback when you can anchor them confidently to the diff using the changed file path and file line numbers shown in the PR diff.",
		"Use top-level comments without path/line only for architectural, cross-cutting, or otherwise unanchorable feedback; top-level comment bodies must still name the exact file, section, symbol, or behavior they refer to.",
		"Flag any top-level actionable comment that lacks exact file, section, symbol, or behavior context as a follow-up quality-gating failure; do not publish vague unlocated feedback.",
		"Do not repeat the overall body/summary as a comment; comments must add distinct actionable feedback.",
		"Resolvable inline review comments are required for code-anchored actionable feedback: when a finding refers to specific changed lines and you can identify the changed file path plus RIGHT/LEFT line numbers from the diff, submit it as an inline review comment in the PR review `comments` array, not in the review body and not as a separate issue/PR conversation comment.",
		"Inline review comments posted through the PR review `comments` array create resolvable GitHub review threads. Top-level review bodies and issue comments are not resolvable; use them only for clean summaries or genuinely cross-cutting/unanchorable feedback.",
		"For non-blocking or blocking finding reviews with any anchorable findings, the review body should be a short overview plus markers/disclosure; the detailed findings must live in inline `comments` so maintainers can resolve them individually.",
		"For multiline inline comments, `start_line`/`start_side` must identify the first line and `line`/`side` the last line; omit `start_line`/`start_side` for single-line comments.",
		"Write substantially more detail than a brief summary; every comment should explain the problem, why it matters, and the concrete change to make.",
		"A comment is invalid if it only names a category (for example, 'gaps around X', 'issues with Y', or 'concerns about Z'), says only 'add tests' without naming the behavior and where the test belongs, lacks a concrete location or section reference, asks a question without proposing a resolution path, or compresses multiple unrelated concerns into one vague summary.",
		"Bad comment example: 'Spec review found actionable gaps around role-specific trigger schema, auto-discovery gating boundaries, and exact env/config-source behavior.' This is bad because it has no file, line, section, concrete missing requirement, evidence, or suggested wording.",
		"Good spec/docs comment example: {\"severity\":\"major\",\"category\":\"spec\",\"body\":\"Define the role trigger schema before implementation starts\",\"problem\":\"The spec introduces role-specific triggers but does not define the schema fields or validation rules.\",\"why\":\"Implementers cannot know which fields are required, how defaults behave, or how invalid trigger definitions should fail.\",\"evidence\":\"The Role triggers section describes behavior but does not list fields, defaults, or invalid examples.\",\"suggestedChange\":\"Add a schema table defining role, event, enabled, conditions, defaults, and validation errors, plus one valid and one invalid example.\",\"path\":\"docs/reviewer.md\",\"line\":42,\"side\":\"RIGHT\"}",
		"Implementation review rubric: check correctness, error handling, tests, concurrency, config compatibility, security, resource lifecycle, observability, migrations, and backward compatibility. Only report issues that are concrete and actionable.",
		"Spec/docs review rubric: check whether every requirement is testable, schemas are typed/defaulted/validated, config precedence is explicit, failure modes are defined, rollout/backward compatibility is covered, acceptance criteria are present, and ambiguous terms are resolved. For missing spec details, suggest exact wording, section, table, or example content.",
		"If the review is clean, write a warm, specific LGTM review body that briefly praises what is good about this PR. Keep it concise, genuine, and varied; do not use a generic template if you can reference the actual PR content.",
	)
	if submitPayloadInstruction != "" {
		parts = append(parts, submitPayloadInstruction)
	}
	return agent.AppendCompletionInstruction(strings.Join(parts, "\n\n")), instructionBlock
}

func customInstructionConfig(value *config.Config) config.Config {
	if value == nil {
		cfg, _ := config.Normalize("")
		cfg.Instructions.Enabled = false
		return cfg
	}
	return *value
}

func normalizeLooperCLIPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		return abs
	}
	return path
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func reviewerScopeInstruction(scope config.ReviewerScope) string {
	switch scope {
	case config.ReviewerScopeFullPR:
		return "Review scope: full_pr. Use the full PR context, including title, body, checks, discussion metadata, and the complete diff payload below. You may report actionable issues anywhere in the PR diff when they are supported by the included context."
	case config.ReviewerScopeChangedFiles:
		return "Review scope: changed_files. Limit actionable findings to files changed by this PR. Use unchanged hunks only as context for changed files, and do not request changes in unrelated files unless the changed-file behavior cannot be fixed locally."
	case config.ReviewerScopeChangedRanges:
		return "Review scope: changed_ranges. Limit actionable findings to changed diff ranges. Use surrounding unchanged lines only to understand the change, and prefer resolvable inline comments anchored to RIGHT/LEFT lines in the diff."
	default:
		return reviewerScopeInstruction(config.ReviewerScopeChangedRanges)
	}
}

func reviewDisclosureInstruction(disclosureCfg config.DisclosureConfig, agentRuntime string, agentModel string) string {
	if !disclosureCfg.Enabled {
		return "Looper disclosure stamping is disabled by configuration; do not add looper Markdown disclosure footers or hidden looper stamp markers to GitHub review bodies or inline review comments."
	}
	if !disclosureCfg.Channels.ReviewComment {
		return "Looper review disclosure stamping is disabled by configuration; do not add looper Markdown disclosure footers or hidden looper stamp markers to GitHub review bodies or inline review comments."
	}
	stamper := disclosure.Stamper{Config: disclosureCfg, Version: version.Current().Version, Agent: agentRuntime, Model: agentModel}
	reviewBodyInstruction := "Every GitHub review body you post must use looper's configured disclosure style: include the hidden stamp marker `" + disclosure.Marker + "` immediately followed by the visible Markdown footer `" + strings.TrimPrefix(stamper.MarkdownStamp("reviewer"), disclosure.Marker+"\n") + "`. Do not write the footer as plain paragraph text."
	if disclosureCfg.Channels.InlineCommentVisible {
		return reviewBodyInstruction + " Every inline review comment must include the hidden stamp marker immediately followed by the same visible Markdown footer."
	}
	return reviewBodyInstruction + " Inline review comments must use only the hidden `" + disclosure.Marker + "` marker, without a visible disclosure footer."
}

func snapshotHeadSHA(checkpoint reviewerCheckpoint) string {
	if checkpoint.Snapshot != nil {
		return checkpoint.Snapshot.HeadSHA
	}
	return ""
}

func agentNativeReviewID(loopID string, headSHA string) string {
	return fmt.Sprintf("reviewer:%s:%s", loopID, headSHA)
}

func agentNativeReviewMarker(loopID string, headSHA string, idempotencyKey string) string {
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("reviewer:%s:%s", loopID, headSHA)
	}
	return fmt.Sprintf("looper:review id=%s head=%s", idempotencyKey, headSHA)
}

func (r *Runner) cleanupReviewerWorktreeIfTerminal(ctx context.Context, project storage.ProjectRecord, checkpoint *reviewerCheckpoint) {
	if r.git == nil || checkpoint == nil || checkpoint.Worktree == nil || checkpoint.Worktree.Path == "" || checkpoint.Worktree.Branch == "" || checkpoint.Worktree.CleanedAt != "" {
		return
	}
	protectedBranches := []string{}
	if baseBranch := strings.TrimSpace(derefString(project.BaseBranch)); baseBranch != "" {
		protectedBranches = append(protectedBranches, baseBranch)
	}
	if err := r.git.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: project.ID, RepoPath: project.RepoPath, WorktreePath: checkpoint.Worktree.Path, Branch: checkpoint.Worktree.Branch, ProtectedBranches: protectedBranches}); err != nil {
		r.logWarn("reviewer worktree cleanup failed", map[string]any{"projectId": project.ID, "worktreePath": checkpoint.Worktree.Path, "branch": checkpoint.Worktree.Branch, "error": err.Error()})
		return
	}
	checkpoint.Worktree.CleanedAt = r.nowISO()
}

func requireWorktree(checkpoint reviewerCheckpoint) (*checkpointWorktree, error) {
	if checkpoint.Worktree == nil {
		return nil, &loopError{message: "Missing reviewer worktree checkpoint for review step", kind: FailureRetryableTransient}
	}
	return checkpoint.Worktree, nil
}

func reviewerWorktreePrepared(checkpoint reviewerCheckpoint) bool {
	if reviewerWorktreeNeedsPrepare(checkpoint) {
		return false
	}
	return checkpoint.Worktree.PreparedAt != ""
}

func reviewerWorktreeNeedsPrepare(checkpoint reviewerCheckpoint) bool {
	if checkpoint.Worktree == nil {
		return true
	}
	worktree := checkpoint.Worktree
	if strings.TrimSpace(worktree.Path) == "" || strings.TrimSpace(worktree.Branch) == "" || worktree.CleanedAt != "" {
		return true
	}
	_, err := os.Stat(worktree.Path)
	return err != nil
}

func reviewerWorktreeBranch(prNumber int64, checkpoint reviewerCheckpoint) string {
	if checkpoint.Worktree != nil {
		if branch := strings.TrimSpace(checkpoint.Worktree.Branch); branch != "" {
			return branch
		}
	}
	return fmt.Sprintf("pr-%d-head", prNumber)
}

func pullRequestHeadRef(prNumber int64) string {
	return fmt.Sprintf("refs/pull/%d/head", prNumber)
}

func (p pendingReviewCheckpoint) clone() *pendingReviewCheckpoint {
	copyValue := p
	return &copyValue
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

func detailLabels(detail *checkpointDetail) []string {
	if detail == nil {
		return nil
	}
	return detail.Labels
}
