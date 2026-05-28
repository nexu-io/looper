package reviewer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/eventlog"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/specpr"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/loops/failureclass"
	"github.com/nexu-io/looper/internal/networkpolicy"
	"github.com/nexu-io/looper/internal/reviewer/automerge"
	"github.com/nexu-io/looper/internal/reviewer/criteria"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/version"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

const (
	stepDiscover         ReviewerStep = "discover"
	stepFilter           ReviewerStep = "filter"
	stepClaim            ReviewerStep = "claim"
	stepSnapshot         ReviewerStep = "snapshot"
	stepWorktree         ReviewerStep = "worktree"
	stepThreadResolution ReviewerStep = "thread_resolution"
	stepReview           ReviewerStep = "review"
	stepPublish          ReviewerStep = "publish"
)

var reviewerStepSequence = []ReviewerStep{
	stepDiscover,
	stepFilter,
	stepClaim,
	stepSnapshot,
	stepWorktree,
	stepThreadResolution,
	stepReview,
	stepPublish,
}

var reviewMarkerCommentPattern = regexp.MustCompile(`(?is)<!--\s*looper:review\b.*?-->`)
var reviewHumanHTMLCommentPattern = regexp.MustCompile(`(?s)<!--.*?-->`)
var reviewHumanReferenceDefinitionPattern = regexp.MustCompile(`(?m)^\s{0,3}\[[^\]\n]+\]:[^\n]*(?:\n[ \t]+[^\n]*)*`)
var reviewerIssueClosingReferencePattern = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+((?:[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#\d+)\b`)

const (
	reviewerNativeResumeMetadataKey      = "reviewerNativeResume"
	reviewerNativeResumeReasonHeadChange = "head_change"
)

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
	defaultAgentTimeout = 90 * time.Minute
	defaultClaimTTL     = 5 * time.Minute
	defaultRetryDelay   = 5 * time.Second
	defaultRetryMax     = 5
	maxRetryDelay       = 60 * time.Second
	retryJitterDivisor  = 4

	defaultHeadChangePollInterval = 15 * time.Second
)

var retryAfterPattern = regexp.MustCompile(`(?i)retry-after\s*[:=]\s*(\d+)`)

var reviewerDiscoveryLocks = newDiscoveryLockSet()

type discoveryLockSet struct {
	mu    sync.Mutex
	locks map[string]*discoveryLockRef
}

type discoveryLockRef struct {
	mu   sync.Mutex
	refs int
}

func newDiscoveryLockSet() *discoveryLockSet {
	return &discoveryLockSet{locks: map[string]*discoveryLockRef{}}
}

func (s *discoveryLockSet) With(key string, fn func() error) error {
	s.mu.Lock()
	ref := s.locks[key]
	if ref == nil {
		ref = &discoveryLockRef{}
		s.locks[key] = ref
	}
	ref.refs++
	s.mu.Unlock()

	ref.mu.Lock()
	defer ref.mu.Unlock()
	defer func() {
		s.mu.Lock()
		ref.refs--
		if ref.refs == 0 {
			delete(s.locks, key)
		}
		s.mu.Unlock()
	}()
	return fn()
}

type PullRequestSummary struct {
	Number             int64
	Title              string
	State              string
	IsDraft            bool
	ReviewDecision     string
	Labels             []string
	HeadSHA            string
	BaseSHA            string
	HasConflicts       bool
	Author             string
	ReviewRequests     []string
	ReviewRequestUsers []networkpolicy.GitHubUser
	Reviews            []map[string]any
}

type PullRequestDetail struct {
	Number             int64
	Title              string
	Body               string
	State              string
	IsDraft            bool
	ReviewDecision     string
	Labels             []string
	HeadSHA            string
	BaseSHA            string
	HeadRefName        string
	BaseRefName        string
	Author             string
	ReviewRequests     []string
	ReviewRequestUsers []networkpolicy.GitHubUser
	HasConflicts       bool
	ChecksSummary      string
	Diff               string
	Comments           []map[string]any
	IssueComments      []map[string]any
	Reviews            []map[string]any
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
	RepoPath        string
	WorktreeRoot    string
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
	WorktreeRoot      string
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
	AllowCleanComment   bool
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

type IssueCommentInput struct {
	Repo        string
	IssueNumber int64
	Body        string
	CWD         string
}

type IssueCommentResult struct {
	ID  int64
	URL string
}

type PullRequestLabelsInput struct {
	Repo     string
	PRNumber int64
	Labels   []string
	CWD      string
}

type ListReviewThreadsInput struct {
	Repo     string
	PRNumber int64
	CWD      string
	Limit    int
}

type ReviewThread struct {
	ID         string
	IsResolved bool
	Path       string
	Line       int64
	URL        string
	Comments   []ReviewThreadComment
}

type ReviewThreadComment struct {
	ID                string
	Body              string
	Author            string
	CreatedAt         string
	UpdatedAt         string
	Path              string
	Line              int64
	OriginalCommitOID string
	CommitOID         string
	URL               string
}

type AddReviewThreadReplyInput struct {
	Repo     string
	ThreadID string
	Body     string
	CWD      string
}

type ResolveReviewThreadInput struct {
	Repo     string
	ThreadID string
	CWD      string
}

type GitHubGateway interface {
	ListOpenPullRequests(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error)
	GetCurrentUserLogin(context.Context, string) (string, error)
	ViewPullRequest(context.Context, ViewPullRequestInput) (PullRequestDetail, error)
	ViewIssue(context.Context, githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error)
	GetPullRequestHeadSHA(context.Context, ViewPullRequestInput) (string, error)
	GetRepositorySettings(context.Context, githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error)
	GetBranchProtection(context.Context, githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error)
	CapturePullRequestSnapshot(context.Context, CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error)
	FindReviewMarker(context.Context, VerifyReviewMarkerInput) (ReviewMarkerResult, error)
	CreateIssueComment(context.Context, IssueCommentInput) (IssueCommentResult, error)
	SubmitReview(context.Context, githubinfra.SubmitReviewInput) error
	EnableAutoMerge(context.Context, githubinfra.EnableAutoMergeInput) error
	AddPullRequestReaction(context.Context, PullRequestReactionInput) error
	RemovePullRequestReaction(context.Context, PullRequestReactionInput) error
	AddPullRequestLabels(context.Context, PullRequestLabelsInput) error
	RemovePullRequestLabels(context.Context, PullRequestLabelsInput) error
	RemoveIssueLabels(context.Context, githubinfra.IssueLabelsInput) error
	ListReviewThreads(context.Context, ListReviewThreadsInput) ([]ReviewThread, error)
	AddReviewThreadReply(context.Context, AddReviewThreadReplyInput) error
	ResolveReviewThread(context.Context, ResolveReviewThreadInput) error
}

type GitGateway interface {
	CreateWorktree(context.Context, CreateWorktreeInput) (CreateWorktreeResult, error)
	PrepareWorktree(context.Context, PrepareWorktreeInput) (PrepareWorktreeResult, error)
	CleanupWorktree(context.Context, CleanupWorktreeInput) error
}

type AgentRunInput struct {
	ExecutionID        string
	ProjectID          string
	LoopID             string
	RunID              string
	Prompt             string
	NativeResumePrompt string
	WorkingDirectory   string
	Timeout            time.Duration
	HeartbeatTimeout   time.Duration
	Metadata           map[string]any
	IdempotencyKey     string
}

type AgentResult struct {
	Status                       string
	Summary                      string
	Stdout                       string
	Stderr                       string
	ParseStatus                  string
	TimeoutType                  string
	ConfiguredIdleTimeoutSeconds int64
	ConfiguredMaxRuntimeSeconds  int64
	ElapsedRuntimeSeconds        int64
	LastProgressAt               string
}

type AgentExecution interface {
	Wait(context.Context) (AgentResult, error)
	Kill(string) error
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
	AgentIdleTimeout        time.Duration
	ClaimTTL                time.Duration
	AllowAutoApprove        bool
	ReviewEvents            config.ReviewerReviewEventsConfig
	LoopConfig              config.ReviewerLoopConfig
	DiscoveryPolicy         DiscoveryPolicy
	Scope                   config.ReviewerScope
	DetectDuplicateFindings bool
	NativeResume            config.ReviewerNativeResumeConfig
	ThreadResolution        config.ReviewerThreadResolutionConfig
	Disclosure              *config.DisclosureConfig
	CustomInstructions      *config.Config
	CriteriaVerifier        criteria.Verifier
	AgentRuntime            string
	AgentModel              *string
	LooperCLIPath           string
	RetryBaseDelay          time.Duration
	RetryMaxAttempts        int64
	RetryPolicy             config.ReviewerRetryConfig
	HeadChangePollInterval  time.Duration
	OnAgentExecutionStarted AgentExecutionStartedFunc
	OnQueueItemEnqueued     func()
}

type DiscoveryPolicy struct {
	AutoDiscovery             bool
	IncludeDrafts             bool
	RequireReviewRequest      bool
	EnableSelfReview          bool
	Labels                    []string
	LabelMode                 config.LabelMode
	IncludeSpecReviewingLabel bool
	SpecReviewingLabel        string
	RoutedClaimPolicy         networkpolicy.ProjectPolicy
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
	agentIdleTimeout        time.Duration
	claimTTL                time.Duration
	allowAutoApprove        bool
	reviewEvents            config.ReviewerReviewEventsConfig
	loopConfig              config.ReviewerLoopConfig
	discoveryPolicy         DiscoveryPolicy
	scope                   config.ReviewerScope
	detectDuplicateFindings bool
	nativeResume            config.ReviewerNativeResumeConfig
	threadResolution        config.ReviewerThreadResolutionConfig
	disclosure              config.DisclosureConfig
	customInstructions      config.Config
	projectRoleConfig       *config.Config
	criteriaVerifier        criteria.Verifier
	agentRuntime            string
	agentModel              string
	looperCLIPath           string
	retryBaseDelay          time.Duration
	retryMaxAttempts        int64
	retryPolicy             config.ReviewerRetryConfig
	retryMaxDelay           time.Duration
	headChangePollInterval  time.Duration
	onAgentExecutionStarted AgentExecutionStartedFunc
	onQueueItemEnqueued     func()
}

type DiscoveryInput struct {
	ProjectID string
	Repo      string
	Limit     int
	Snapshot  *githubinfra.DiscoverySnapshot
}

type TargetedDiscoveryInput struct {
	ProjectID string
	Repo      string
	PRNumber  int64

	Snapshot *githubinfra.DiscoverySnapshot
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
	ResumePolicy                 string                      `json:"resumePolicy,omitempty"`
	Detail                       *checkpointDetail           `json:"detail,omitempty"`
	RoutedClaimMatchMode         string                      `json:"routedClaimMatchMode,omitempty"`
	ClaimedLockKey               string                      `json:"claimedLockKey,omitempty"`
	Snapshot                     *checkpointSnapshot         `json:"snapshot,omitempty"`
	Worktree                     *checkpointWorktree         `json:"worktree,omitempty"`
	ThreadResolution             *threadResolutionCheckpoint `json:"threadResolution,omitempty"`
	ThreadResolutionFollowUpOnly bool                        `json:"threadResolutionFollowUpOnly,omitempty"`
	PendingReview                *pendingReviewCheckpoint    `json:"pendingReview,omitempty"`
	SkipReason                   string                      `json:"skipReason,omitempty"`
	SkipKind                     string                      `json:"skipKind,omitempty"`
	SkipReviewerLogin            string                      `json:"skipReviewerLogin,omitempty"`
}

type checkpointDetail struct {
	Title              string                     `json:"title,omitempty"`
	State              string                     `json:"state,omitempty"`
	IsDraft            bool                       `json:"isDraft,omitempty"`
	ReviewDecision     string                     `json:"reviewDecision,omitempty"`
	Labels             []string                   `json:"labels,omitempty"`
	HeadSHA            string                     `json:"headSha,omitempty"`
	BaseSHA            string                     `json:"baseSha,omitempty"`
	HeadRefName        string                     `json:"headRefName,omitempty"`
	BaseRefName        string                     `json:"baseRefName,omitempty"`
	Author             string                     `json:"author,omitempty"`
	ReviewRequests     []string                   `json:"reviewRequests,omitempty"`
	ReviewRequestUsers []networkpolicy.GitHubUser `json:"reviewRequestUsers,omitempty"`
	HasConflicts       bool                       `json:"hasConflicts,omitempty"`
	CurrentLogin       string                     `json:"currentLogin,omitempty"`
	Reviews            []map[string]any           `json:"reviews,omitempty"`
	Comments           []map[string]any           `json:"comments,omitempty"`
	IssueComments      []map[string]any           `json:"issueComments,omitempty"`
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
	CleanNoop                bool        `json:"cleanNoop,omitempty"`
	MarkerVerificationMisses int         `json:"markerVerificationMisses,omitempty"`
}

type threadResolutionCheckpoint struct {
	HeadSHA   string `json:"headSha,omitempty"`
	Processed int    `json:"processed,omitempty"`
	Commented int    `json:"commented,omitempty"`
	Resolved  int    `json:"resolved,omitempty"`
	Reported  int    `json:"reported,omitempty"`
}

type resumedRunContext struct {
	Run        storage.RunRecord
	StartStep  ReviewerStep
	Checkpoint reviewerCheckpoint
	Resumed    bool
}

type loopError struct {
	message     string
	kind        QueueFailureKind
	interrupted bool
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
	agentIdleTimeout := options.AgentIdleTimeout
	if agentIdleTimeout <= 0 {
		agentIdleTimeout = 10 * time.Minute
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
	retryPolicy := config.NormalizeReviewerRetryConfig(options.RetryPolicy)
	retryMaxDelay := time.Duration(retryPolicy.MaxDelayMS) * time.Millisecond
	if retryMaxDelay <= 0 {
		retryMaxDelay = maxRetryDelay
	}
	headChangePollInterval := options.HeadChangePollInterval
	if headChangePollInterval <= 0 {
		headChangePollInterval = defaultHeadChangePollInterval
	}
	disclosureCfg := config.DefaultDisclosureConfig()
	if options.Disclosure != nil {
		disclosureCfg = *options.Disclosure
	}
	loopConfig := options.LoopConfig
	if loopConfig == (config.ReviewerLoopConfig{}) {
		loopConfig = config.ReviewerLoopConfig{EnabledByDefault: false, QuietPeriodSeconds: 60, MinPublishIntervalSeconds: 300, MaxIterationsPerPR: 20, MaxIterationsPerHead: 1, MaxWallClockSeconds: 0, MaxConsecutiveFailures: 3, MaxAgentExecutionsPerPR: 25, StopOnApproved: false, StopOnReadyLabel: true, StopOnIdenticalOutput: true}
	}
	scope := options.Scope
	if scope == "" {
		scope = config.ReviewerScopeChangedRanges
	}
	threadResolution := options.ThreadResolution
	if threadResolution.Mode == "" {
		threadResolution = config.ReviewerThreadResolutionConfig{Enabled: false, Mode: config.ReviewerThreadResolutionModeReportOnly, Scope: config.ReviewerThreadResolutionScopeLooperAuthoredOnly, AutoResolve: config.ReviewerThreadResolutionAutoResolveObjectiveOnly, RequireAuditComment: true, RequireNewHeadSinceThread: true, RequireCurrentReviewRequest: true, MaxThreadsPerRun: 10}
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
	if !policy.AutoDiscovery && !policy.IncludeDrafts && !policy.RequireReviewRequest && !policy.EnableSelfReview && len(policy.Labels) == 0 && policy.LabelMode == "" && !policy.IncludeSpecReviewingLabel && policy.SpecReviewingLabel == "" {
		policy = DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, RequireReviewRequest: true, EnableSelfReview: false, Labels: []string{}, LabelMode: config.LabelModeAll, IncludeSpecReviewingLabel: true, SpecReviewingLabel: specpr.ReviewingLabel}
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
		agentIdleTimeout:        agentIdleTimeout,
		claimTTL:                claimTTL,
		allowAutoApprove:        options.AllowAutoApprove,
		reviewEvents:            reviewEvents,
		loopConfig:              loopConfig,
		discoveryPolicy:         policy,
		scope:                   scope,
		detectDuplicateFindings: options.DetectDuplicateFindings,
		nativeResume:            options.NativeResume,
		threadResolution:        threadResolution,
		disclosure:              disclosureCfg,
		customInstructions:      customInstructionConfig(options.CustomInstructions),
		projectRoleConfig:       options.CustomInstructions,
		criteriaVerifier:        options.CriteriaVerifier,
		agentRuntime:            strings.TrimSpace(options.AgentRuntime),
		agentModel:              derefString(options.AgentModel),
		looperCLIPath:           normalizeLooperCLIPath(options.LooperCLIPath),
		retryBaseDelay:          retryBaseDelay,
		retryMaxAttempts:        retryMax,
		retryPolicy:             retryPolicy,
		retryMaxDelay:           retryMaxDelay,
		headChangePollInterval:  headChangePollInterval,
		onAgentExecutionStarted: options.OnAgentExecutionStarted,
		onQueueItemEnqueued:     options.OnQueueItemEnqueued,
	}
}

func (r *Runner) DiscoverPullRequests(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	ctx = githubinfra.ContextWithDiscoverySnapshot(ctx, input.Snapshot)
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
	policy := r.discoveryPolicyForProject(project.ID)
	if !policy.AutoDiscovery {
		return DiscoveryResult{Skipped: 1}, nil
	}
	openPRs, err := r.listOpenPullRequestsForDiscoveryWithPolicy(ctx, input.Repo, project.RepoPath, input.Limit, policy)
	if err != nil {
		return DiscoveryResult{}, err
	}
	specPRs := []PullRequestSummary{}
	if policy.IncludeSpecReviewingLabel {
		var err error
		specPRs, err = r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: input.Repo, CWD: project.RepoPath, Limit: input.Limit, Label: policy.SpecReviewingLabel})
		if err != nil {
			return DiscoveryResult{}, err
		}
	}
	currentLogin := ""
	if policy.RequireReviewRequest || !policy.EnableSelfReview {
		var err error
		currentLogin, err = r.github.GetCurrentUserLogin(ctx, project.RepoPath)
		if err != nil {
			return DiscoveryResult{}, err
		}
		currentLogin = normalizeLogin(currentLogin)
	}
	result := DiscoveryResult{}
	seen := map[string]struct{}{}
	resolveAuthor := func(pr PullRequestSummary) PullRequestSummary {
		if policy.EnableSelfReview || strings.TrimSpace(pr.Author) != "" {
			return pr
		}
		detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: pr.Number, CWD: project.RepoPath})
		if err != nil {
			return pr
		}
		pr.Author = detail.Author
		if pr.HeadSHA == "" {
			pr.HeadSHA = detail.HeadSHA
		}
		return pr
	}
	enqueue := func(pr PullRequestSummary, existing *storage.LoopRecord) error {

		return r.enqueueReviewerDiscoveryCandidate(ctx, *project, input.Repo, policy, &currentLogin, pr, existing, &result)

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
		pr = resolveAuthor(pr)
		if !prEligibleForDiscoveryPreclaim(pr, currentLogin, policy) {
			result.Skipped++
			continue
		}
		if networkpolicy.IsRouted(policy.RoutedClaimPolicy) {
			_, resolvedLogin, err := r.routedReviewerClaimDecisionWithCurrentLogin(ctx, project.RepoPath, policy, currentLogin, pr.Author, pr.Labels, pr.ReviewRequestUsers)
			if err != nil {
				return DiscoveryResult{}, err
			}
			currentLogin = resolvedLogin
		}
		if !prEligibleForDiscovery(pr, currentLogin, policy) {
			result.Skipped++
			continue
		}
		if err := seenEnqueue(pr); err != nil {
			return DiscoveryResult{}, err
		}
	}
	for _, pr := range specPRs {
		pr = resolveAuthor(pr)
		if !prEligibleForDiscoveryPreclaim(pr, currentLogin, policy) {
			result.Skipped++
			continue
		}
		if networkpolicy.IsRouted(policy.RoutedClaimPolicy) {
			_, resolvedLogin, err := r.routedReviewerClaimDecisionWithCurrentLogin(ctx, project.RepoPath, policy, currentLogin, pr.Author, pr.Labels, pr.ReviewRequestUsers)
			if err != nil {
				return DiscoveryResult{}, err
			}
			currentLogin = resolvedLogin
		}
		if !prEligibleForDiscovery(pr, currentLogin, policy) {
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
		queuedBefore := len(result.QueueItems)
		if err := r.discoverExistingReviewerLoop(ctx, *project, input.Repo, policy, &currentLogin, loop, detail, &result); err != nil {
			return DiscoveryResult{}, err
		}
		if len(result.QueueItems) > queuedBefore {
			seen[key] = struct{}{}
		}
	}
	return result, nil
}

func (r *Runner) DiscoverPullRequest(ctx context.Context, input TargetedDiscoveryInput) (DiscoveryResult, error) {

	ctx = githubinfra.ContextWithDiscoverySnapshot(ctx, input.Snapshot)

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
	policy := r.discoveryPolicyForProject(project.ID)
	if !policy.AutoDiscovery {
		return DiscoveryResult{Skipped: 1}, nil
	}

	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: project.RepoPath})
	if err != nil {
		return DiscoveryResult{}, err
	}

	currentLogin := ""
	if policy.RequireReviewRequest || !policy.EnableSelfReview {
		currentLogin, err = r.github.GetCurrentUserLogin(ctx, project.RepoPath)
		if err != nil {
			return DiscoveryResult{}, err
		}
		currentLogin = normalizeLogin(currentLogin)
	}
	pr := summaryFromDetail(detail)
	result := DiscoveryResult{}
	existingLoops, err := r.findReviewerLoopsByPR(ctx, project.ID, input.Repo, input.PRNumber)
	if err != nil {
		return DiscoveryResult{}, err
	}
	if len(existingLoops) > 0 {
		for _, loop := range existingLoops {
			queuedBefore := len(result.QueueItems)
			if err := r.discoverExistingReviewerLoop(ctx, *project, input.Repo, policy, &currentLogin, loop, detail, &result); err != nil {
				return DiscoveryResult{}, err
			}
			if len(result.QueueItems) > queuedBefore {
				return result, nil
			}
		}
		return result, nil
	}
	if !prEligibleForDiscoveryPreclaim(pr, currentLogin, policy) {
		result.Skipped++
		return result, nil
	}
	if networkpolicy.IsRouted(policy.RoutedClaimPolicy) {
		_, resolvedLogin, err := r.routedReviewerClaimDecisionWithCurrentLogin(ctx, project.RepoPath, policy, currentLogin, pr.Author, pr.Labels, pr.ReviewRequestUsers)
		if err != nil {
			return DiscoveryResult{}, err
		}
		currentLogin = resolvedLogin
	}
	if !prEligibleForDiscovery(pr, currentLogin, policy) {
		result.Skipped++
		return result, nil
	}
	if err := r.enqueueReviewerDiscoveryCandidate(ctx, *project, input.Repo, policy, &currentLogin, pr, nil, &result); err != nil {

		return DiscoveryResult{}, err
	}
	return result, nil
}

func (r *Runner) enqueueReviewerDiscoveryCandidate(ctx context.Context, project storage.ProjectRecord, repo string, policy DiscoveryPolicy, currentLogin *string, pr PullRequestSummary, existing *storage.LoopRecord, result *DiscoveryResult) error {
	loopResult, loopErr := r.ensureLoopForPullRequest(ctx, project, repo, pr.Number, existing)
	if loopErr != nil {
		return loopErr
	}
	if terminalReviewerLoopReason(loopResult.record) == "failed" {
		recovered, recoverErr := r.recoverFailedReviewerLoop(ctx, loopResult.record, pr)
		if recoverErr != nil {
			return recoverErr
		}
		if recovered != nil {
			loopResult.record = *recovered
		}
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
	if reviewerDiscoverySuppressedByLastSkip(meta, pr, *currentLogin, policy) {
		result.Skipped++
		return nil
	}
	if reviewerLastSkipNeedsCurrentLogin(meta, pr) && *currentLogin == "" {
		lookupLogin, lookupErr := r.github.GetCurrentUserLogin(ctx, project.RepoPath)
		if lookupErr != nil {
			lookupLogin = ""
		}
		*currentLogin = normalizeLogin(lookupLogin)
	}
	if reviewerDiscoverySuppressedByLastSkip(meta, pr, *currentLogin, policy) {
		result.Skipped++
		return nil
	}
	if last, ok := stringFromAny(meta["lastPublishedHeadSha"]); ok && last == pr.HeadSHA && pr.HeadSHA != "" {
		result.Skipped++
		return nil
	}
	availableAt := r.nextReviewAvailableAt(meta)
	queueItem, queueErr := r.enqueue(ctx, enqueueInput{
		ProjectID:   project.ID,
		LoopID:      loopResult.record.ID,
		Repo:        repo,
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

func (r *Runner) discoverExistingReviewerLoop(ctx context.Context, project storage.ProjectRecord, repo string, policy DiscoveryPolicy, currentLogin *string, loop storage.LoopRecord, detail PullRequestDetail, result *DiscoveryResult) error {
	if !policy.IncludeDrafts && detail.IsDraft {
		result.Skipped++
		return nil
	}
	if normalizePRState(detail.State) != "open" {
		if err := r.terminateLoop(ctx, loop, "pr_closed_or_merged"); err != nil {
			return err
		}
		result.Skipped++
		return nil
	}
	if !isManualReviewerLoop(loop) && isSelfAuthoredPR(detail.Author, *currentLogin, policy) {
		result.Skipped++
		return nil
	}
	requireReviewRequest := requireReviewRequestForLoop(loop, policy.RequireReviewRequest, detail.HeadSHA)
	if !networkpolicy.IsRouted(policy.RoutedClaimPolicy) && requireReviewRequest && !isCurrentUserRequested(detail.ReviewRequests, *currentLogin) && !r.hasThreadResolutionFollowUpCandidate(ctx, project.RepoPath, repo, detail.Number, detail.HeadSHA, *currentLogin) {
		result.Skipped++
		return nil
	}
	if !isManualReviewerLoop(loop) && !labelsMatch(detail.Labels, policy.Labels, policy.LabelMode) {
		result.Skipped++
		return nil
	}
	return r.enqueueReviewerDiscoveryCandidate(ctx, project, repo, policy, currentLogin, summaryFromDetail(detail), &loop, result)
}

func (r *Runner) findReviewerLoopsByPR(ctx context.Context, projectID, repo string, prNumber int64) ([]storage.LoopRecord, error) {
	loops, err := r.repos.Loops.List(ctx)
	if err != nil {
		return nil, err
	}
	matched := []storage.LoopRecord{}
	for _, loop := range loops {
		if loop.Type == "reviewer" && loop.ProjectID == projectID && derefString(loop.Repo) == repo && derefInt64(loop.PRNumber) == prNumber {
			matched = append(matched, loop)
		}
	}
	return matched, nil

}

func (r *Runner) listOpenPullRequestsForDiscovery(ctx context.Context, repo, cwd string, limit int) ([]PullRequestSummary, error) {
	return r.listOpenPullRequestsForDiscoveryWithPolicy(ctx, repo, cwd, limit, r.discoveryPolicy)
}

func (r *Runner) listOpenPullRequestsForDiscoveryWithPolicy(ctx context.Context, repo, cwd string, limit int, policy DiscoveryPolicy) ([]PullRequestSummary, error) {
	labels := prQueryLabels(policy.Labels)
	effectiveLimit := defaultDiscoveryLimit(limit)
	if len(labels) == 0 {
		return r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: limit})
	}
	if len(labels) == 1 {
		return r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: limit, Label: labels[0]})
	}
	if policy.LabelMode == config.LabelModeAll {
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
	return prEligibleForDiscovery(pr, currentLogin, r.discoveryPolicy)
}

func prEligibleForDiscovery(pr PullRequestSummary, currentLogin string, policy DiscoveryPolicy) bool {
	if !prEligibleForDiscoveryPreclaim(pr, currentLogin, policy) {
		return false
	}
	if networkpolicy.IsRouted(policy.RoutedClaimPolicy) {
		decision := routedReviewerClaimDecision(policy, currentLogin, pr.Author, pr.Labels, pr.ReviewRequestUsers)
		if !decision.Allowed {
			return false
		}
	}
	return true
}

func prEligibleForDiscoveryPreclaim(pr PullRequestSummary, currentLogin string, policy DiscoveryPolicy) bool {
	if !policy.IncludeDrafts && pr.IsDraft {
		return false
	}
	if normalizePRState(pr.State) != "open" {
		return false
	}
	if isSelfAuthoredPR(pr.Author, currentLogin, policy) {
		return false
	}
	if !networkpolicy.IsRouted(policy.RoutedClaimPolicy) && policy.RequireReviewRequest && !isCurrentUserRequested(pr.ReviewRequests, currentLogin) {
		return false
	}
	if !labelsMatch(pr.Labels, policy.Labels, policy.LabelMode) {
		return false
	}
	return true
}

func (r *Runner) discoveryPolicyForProject(projectID string) DiscoveryPolicy {
	if r.projectRoleConfig == nil {
		return r.discoveryPolicy
	}
	roles := config.ProjectRoleConfigs(*r.projectRoleConfig, projectID)
	return DiscoveryPolicy{AutoDiscovery: roles.Reviewer.Discovery.AutoDiscovery, IncludeDrafts: roles.Reviewer.Discovery.Triggers.IncludeDrafts, RequireReviewRequest: roles.Reviewer.Discovery.Triggers.RequireReviewRequest, EnableSelfReview: roles.Reviewer.Discovery.Triggers.EnableSelfReview, Labels: append([]string(nil), roles.Reviewer.Discovery.Triggers.Labels...), LabelMode: roles.Reviewer.Discovery.Triggers.LabelMode, IncludeSpecReviewingLabel: roles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel, SpecReviewingLabel: roles.Reviewer.Discovery.SpecReview.ReviewingLabel, RoutedClaimPolicy: networkpolicy.ProjectPolicyForProject(*r.projectRoleConfig, projectID)}
}

func (r *Runner) reviewerAutoMergeConfigForProject(projectID string) config.ReviewerAutoMergeConfig {
	if r.projectRoleConfig == nil {
		return config.ReviewerAutoMergeConfig{}
	}
	roles := config.ProjectRoleConfigs(*r.projectRoleConfig, projectID)
	return roles.Reviewer.AutoMerge
}

func (r *Runner) retryPolicyForProject(projectID string) config.ReviewerRetryConfig {
	policy := config.NormalizeReviewerRetryConfig(r.retryPolicy)
	if r.projectRoleConfig == nil {
		return policy
	}
	projectID = strings.TrimSpace(projectID)
	for _, project := range r.projectRoleConfig.Projects {
		if strings.TrimSpace(project.ID) != projectID {
			continue
		}
		if project.Roles == nil || project.Roles.Reviewer == nil || project.Roles.Reviewer.Behavior == nil || project.Roles.Reviewer.Behavior.Retry == nil {
			return policy
		}
		return mergeProjectReviewerRetryPolicy(policy, *project.Roles.Reviewer.Behavior.Retry)
	}
	return policy
}

func mergeProjectReviewerRetryPolicy(policy config.ReviewerRetryConfig, partial config.PartialReviewerRetryConfig) config.ReviewerRetryConfig {
	if partial.EnhancedTransientClassification != nil {
		policy.EnhancedTransientClassification = *partial.EnhancedTransientClassification
	}
	if partial.ExtraTransientErrorPatterns != nil {
		policy.ExtraTransientErrorPatterns = append([]string(nil), (*partial.ExtraTransientErrorPatterns)...)
	}
	if partial.RecoverExistingMatchedFailures != nil {
		policy.RecoverExistingMatchedFailures = *partial.RecoverExistingMatchedFailures
	}
	if partial.AutoRecoveryMaxAttempts != nil {
		policy.AutoRecoveryMaxAttempts = *partial.AutoRecoveryMaxAttempts
	}
	if partial.MaxDelayMS != nil {
		policy.MaxDelayMS = *partial.MaxDelayMS
	}
	return config.NormalizeReviewerRetryConfig(policy)
}

func (r *Runner) retryMaxDelayForProject(projectID string) time.Duration {
	if r.projectRoleConfig == nil && r.retryMaxDelay > 0 {
		return r.retryMaxDelay
	}
	policy := r.retryPolicyForProject(projectID)
	delay := time.Duration(policy.MaxDelayMS) * time.Millisecond
	if delay <= 0 {
		return maxRetryDelay
	}
	return delay
}

func isSelfAuthoredPR(author string, currentLogin string, policy DiscoveryPolicy) bool {
	if policy.EnableSelfReview {
		return false
	}
	return normalizeLogin(author) != "" && normalizeLogin(author) == normalizeLogin(currentLogin)
}

func routedReviewerClaimDecision(policy DiscoveryPolicy, currentLogin string, author string, labels []string, reviewRequests []networkpolicy.GitHubUser) networkpolicy.ClaimDecision {
	decision := networkpolicy.EvaluateReviewer(policy.RoutedClaimPolicy, labels, reviewRequests)
	if decision.Allowed {
		return decision
	}
	if !policy.EnableSelfReview {
		return decision
	}
	if decision.Reason != "local GitHub identity is not requested for review" {
		return decision
	}
	if normalizeLogin(currentLogin) == "" {
		return decision
	}
	if normalizeLogin(author) == "" || normalizeLogin(author) != normalizeLogin(currentLogin) {
		return decision
	}
	return networkpolicy.ClaimDecision{Allowed: true, Reason: "", MatchMode: decision.MatchMode, TargetLabel: decision.TargetLabel}
}

func (r *Runner) routedReviewerClaimDecisionWithCurrentLogin(ctx context.Context, cwd string, policy DiscoveryPolicy, currentLogin string, author string, labels []string, reviewRequests []networkpolicy.GitHubUser) (networkpolicy.ClaimDecision, string, error) {
	decision := networkpolicy.EvaluateReviewer(policy.RoutedClaimPolicy, labels, reviewRequests)
	if decision.Allowed || !policy.EnableSelfReview || decision.Reason != "local GitHub identity is not requested for review" {
		return decision, currentLogin, nil
	}
	if normalizeLogin(currentLogin) == "" {
		if r.github == nil {
			return decision, currentLogin, nil
		}
		lookupLogin, err := r.github.GetCurrentUserLogin(ctx, cwd)
		if err != nil {
			return decision, currentLogin, nil
		}
		currentLogin = normalizeLogin(lookupLogin)
	}
	return routedReviewerClaimDecision(policy, currentLogin, author, labels, reviewRequests), currentLogin, nil
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
	failure := r.classifyFailureForProject(derefString(queueItem.ProjectID), cause)
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
			if loops.ShouldPauseLoopAfterFailure(string(failure.kind), failedQueue, "") {
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
	if err := r.revalidateRoutedReviewerClaim(ctx, *project, queueItem); err != nil {
		return ProcessResult{}, err
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
		stepStartedAt := r.now()
		run, err = r.persistStepStarted(ctx, run, step, checkpoint)
		if err != nil {
			return ProcessResult{}, err
		}
		r.appendEvent(ctx, eventInput{eventType: "loop.step.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"step": string(step), "startedAt": eventlog.FormatJavaScriptISOString(stepStartedAt.UTC())}})
		checkpoint, err = r.executeStep(ctx, step, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: queueItem, Repo: *queueItem.Repo, PRNumber: *queueItem.PRNumber, Checkpoint: checkpoint})
		if err != nil {
			stepElapsedSeconds := durationSeconds(r.now().Sub(stepStartedAt))
			failure := r.classifyFailureForProjectAndBoundary(project.ID, err, reviewerFailureBoundaryForStep(step))
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
				if resumePolicy != loops.ResumePolicyRestartFromDiscover && resumePolicy != "rerun_review" {
					resumePolicy = loops.ResumePolicyAdvanceFromCheckpoint
				}
			case FailureManualIntervention:
				resumePolicy = loops.ResumePolicyManualIntervention
			default:
				if resumePolicy == "" {
					resumePolicy = loops.ResumePolicyReplayStep
				}
			}
			latest.ResumePolicy = resumePolicy
			runStatus := "failed"
			stepEventType := "loop.step.failed"
			runEventType := "run.failed"
			if failure.interrupted {
				runStatus = "interrupted"
				stepEventType = "loop.step.interrupted"
				runEventType = "run.interrupted"
			}
			if _, err := r.completeRun(ctx, run, runStatus, failure.message, failure.message, latest); err != nil {
				return ProcessResult{}, err
			}
			r.appendEvent(ctx, eventInput{eventType: stepEventType, projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"message": failure.message, "failureKind": string(failure.kind), "currentStep": derefString(run.CurrentStep), "elapsedSeconds": stepElapsedSeconds}})
			r.appendEvent(ctx, eventInput{eventType: runEventType, projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"summary": failure.message, "failureKind": string(failure.kind)}})
			if failure.interrupted {
				r.logInfo("reviewer run interrupted", map[string]any{"projectId": project.ID, "loopId": loop.ID, "runId": run.ID, "queueItemId": queueItem.ID, "currentStep": derefString(run.CurrentStep), "elapsedSeconds": stepElapsedSeconds, "failureKind": string(failure.kind), "summary": failure.message})
			} else {
				r.logError("reviewer run failed", map[string]any{"projectId": project.ID, "loopId": loop.ID, "runId": run.ID, "queueItemId": queueItem.ID, "currentStep": derefString(run.CurrentStep), "elapsedSeconds": stepElapsedSeconds, "failureKind": string(failure.kind), "summary": failure.message})
			}
			failedQueue, queueErr := r.failQueueItem(ctx, queueItem, failure.kind, failure.message)
			if queueErr != nil {
				return ProcessResult{}, queueErr
			}
			terminalFailure := false
			_, loopErr := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
				updated.LastRunAt = stringPtr(r.nowISO())
				if !failure.interrupted {
					metadataJSON, metaErr := r.recordLoopFailureMetadata(updated.MetadataJSON, failure.message)
					if metaErr == nil {
						updated.MetadataJSON = &metadataJSON
					}
				}
				if !failure.interrupted && terminalReviewerLoopReason(*updated) == "failed" {
					terminalFailure = true
					updated.Status = "failed"
					updated.NextRunAt = nil
				} else if updated.Status == "paused" {
					updated.NextRunAt = nil
				} else if failedQueue != nil && failedQueue.Status == "queued" {
					updated.Status = "queued"
					updated.NextRunAt = stringPtr(failedQueue.AvailableAt)
				} else {
					if loops.ShouldPauseLoopAfterFailure(string(failure.kind), failedQueue, latest.ResumePolicy) {
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
				if err := r.repos.Queue.Fail(ctx, storage.QueueFailInput{ID: failedQueue.ID, Attempts: failedQueue.Attempts, FinishedAt: r.nowISO(), ErrorMessage: optionalString(failure.message), ErrorKind: string(failure.kind), UpdatedAt: r.nowISO()}); err != nil {
					return ProcessResult{}, err
				}
				failedQueue.Status = "failed"
			}
			if failedQueue == nil || failedQueue.Status != "queued" {
				r.cleanupReviewerWorktreeIfTerminal(context.Background(), *project, &latest)
			}
			return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: runStatus, Summary: failure.message, FailureKind: failure.kind}, nil
		}
		if step == stepClaim {
			claimedLockKey = checkpoint.ClaimedLockKey
			acquiredClaimedLock = claimedLockKey != ""
		}
		run, err = r.persistStepCompleted(ctx, run, step, checkpoint)
		if err != nil {
			return ProcessResult{}, err
		}
		stepElapsedSeconds := durationSeconds(r.now().Sub(stepStartedAt))
		r.appendEvent(ctx, eventInput{eventType: "loop.step.completed", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"step": string(step), "elapsedSeconds": stepElapsedSeconds}})
		r.logInfo("reviewer step completed", map[string]any{"projectId": project.ID, "loopId": loop.ID, "runId": run.ID, "queueItemId": queueItem.ID, "step": string(step), "elapsedSeconds": stepElapsedSeconds})
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

func (r *Runner) revalidateRoutedReviewerClaim(ctx context.Context, project storage.ProjectRecord, queueItem storage.QueueItemRecord) error {
	policy := r.discoveryPolicyForProject(project.ID)
	if !networkpolicy.IsRouted(policy.RoutedClaimPolicy) || r.github == nil || queueItem.Repo == nil || queueItem.PRNumber == nil {
		return nil
	}
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: *queueItem.Repo, PRNumber: *queueItem.PRNumber, CWD: project.RepoPath})
	if err != nil {
		return err
	}
	decision := networkpolicy.EvaluateReviewer(policy.RoutedClaimPolicy, detail.Labels, detail.ReviewRequestUsers)
	if !decision.Allowed && policy.EnableSelfReview && decision.Reason == "local GitHub identity is not requested for review" {
		currentLogin, lookupErr := r.github.GetCurrentUserLogin(ctx, project.RepoPath)
		if lookupErr != nil {
			return &loopError{message: lookupErr.Error(), kind: FailureRetryableTransient}
		}
		decision = routedReviewerClaimDecision(policy, currentLogin, detail.Author, detail.Labels, detail.ReviewRequestUsers)
	}
	if !decision.Allowed {
		return &loopError{message: fmt.Sprintf("Skipped routed pull request %s#%d: %s", *queueItem.Repo, *queueItem.PRNumber, decision.Reason), kind: FailureManualIntervention}
	}
	return nil
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
	if reviewerStepSupportsTransientExternalRetry(step) {
		return r.executeStepWithTransientExternalRetry(ctx, step, input)
	}
	return r.executeStepOnce(ctx, step, input)
}

func (r *Runner) executeStepOnce(ctx context.Context, step ReviewerStep, input stepInput) (reviewerCheckpoint, error) {
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
	case stepThreadResolution:
		return r.runThreadResolutionStep(ctx, input)
	case stepReview:
		return r.runReviewStep(ctx, input)
	case stepPublish:
		return r.runPublishStep(ctx, input)
	default:
		return input.Checkpoint, fmt.Errorf("unsupported reviewer step: %s", step)
	}
}

func (r *Runner) executeStepWithTransientExternalRetry(ctx context.Context, step ReviewerStep, input stepInput) (reviewerCheckpoint, error) {
	if _, ok := ctx.Deadline(); !ok && r.agentTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.agentTimeout)
		defer cancel()
	}
	maxAttempts := r.retryMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultRetryMax
	}
	retryMaxDelay := r.retryMaxDelayForProject(input.Project.ID)
	checkpoint := input.Checkpoint
	var err error
	for attempt := int64(1); attempt <= maxAttempts; attempt++ {
		input.Checkpoint = checkpoint
		checkpoint, err = r.executeStepOnce(ctx, step, input)
		if err == nil {
			if attempt > 1 {
				r.logInfo("reviewer transient external retry succeeded", map[string]any{"projectId": input.Project.ID, "loopId": input.Loop.ID, "runId": input.Run.ID, "queueItemId": input.QueueItem.ID, "step": string(step), "attempt": attempt, "maxAttempts": maxAttempts})
			}
			return checkpoint, nil
		}
		if attempt >= maxAttempts || !r.isTransientExternalFailureForProject(input.Project.ID, err) {
			break
		}
		delay := retryDelay(r.retryBaseDelay, attempt, err, retryMaxDelay)
		if r.shouldSkipTransientRetryDelayForNativeResume(ctx, input.Loop.ID, err) {
			delay = 0
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			r.logWarn("reviewer transient external retry budget exhausted", map[string]any{"projectId": input.Project.ID, "loopId": input.Loop.ID, "runId": input.Run.ID, "queueItemId": input.QueueItem.ID, "step": string(step), "attempt": attempt, "maxAttempts": maxAttempts, "retryDelay": delay.String(), "error": err.Error()})
			break
		}
		r.logInfo("reviewer transient external failure retrying", map[string]any{"projectId": input.Project.ID, "loopId": input.Loop.ID, "runId": input.Run.ID, "queueItemId": input.QueueItem.ID, "step": string(step), "attempt": attempt, "nextAttempt": attempt + 1, "maxAttempts": maxAttempts, "retryDelay": delay.String(), "error": err.Error()})
		if sleepErr := sleepWithContext(ctx, delay); sleepErr != nil {
			return checkpoint, errors.Join(err, sleepErr)
		}
	}
	return checkpoint, err
}

func (r *Runner) runDiscoverStep(ctx context.Context, input stepInput) (reviewerCheckpoint, error) {
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
	if err != nil {
		if githubinfra.IsPullRequestNotFoundError(err) {
			checkpoint := input.Checkpoint
			checkpoint.SkipReason = fmt.Sprintf("Skipped missing pull request %s#%d: %s", input.Repo, input.PRNumber, githubinfra.ErrorMessage(err))
			checkpoint.SkipKind = "pr_not_found"
			checkpoint.ResumePolicy = ""
			if terminateErr := r.terminateLoop(ctx, input.Loop, "pr_not_found"); terminateErr != nil {
				return checkpoint, terminateErr
			}
			return checkpoint, nil
		}
		return input.Checkpoint, err
	}
	checkpoint := input.Checkpoint
	checkpoint.Detail = checkpointDetailFromDetail(detail)
	checkpoint.ResumePolicy = "replay_step"
	return checkpoint, nil
}

func checkpointDetailFromDetail(detail PullRequestDetail) *checkpointDetail {
	return &checkpointDetail{Title: detail.Title, State: detail.State, IsDraft: detail.IsDraft, ReviewDecision: detail.ReviewDecision, Labels: cloneStrings(detail.Labels), HeadSHA: detail.HeadSHA, BaseSHA: detail.BaseSHA, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, Author: detail.Author, ReviewRequests: cloneStrings(detail.ReviewRequests), ReviewRequestUsers: cloneGitHubUsers(detail.ReviewRequestUsers), HasConflicts: detail.HasConflicts, Comments: cloneObjectSlice(detail.Comments), IssueComments: cloneObjectSlice(detail.IssueComments), Reviews: cloneObjectSlice(detail.Reviews)}
}

func (r *Runner) runFilterStep(ctx context.Context, input stepInput) (reviewerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.Detail == nil {
		return checkpoint, &loopError{message: "Missing PR detail checkpoint for filter step", kind: FailureRetryableTransient}
	}
	policy := r.discoveryPolicyForProject(input.Project.ID)
	if !policy.IncludeDrafts && checkpoint.Detail.IsDraft {
		checkpoint.SkipReason = fmt.Sprintf("Skipped draft pull request %s#%d", input.Repo, input.PRNumber)
		checkpoint.SkipKind = "draft"
		return checkpoint, nil
	}
	if normalizePRState(checkpoint.Detail.State) != "open" {
		checkpoint.SkipReason = fmt.Sprintf("Skipped non-open pull request %s#%d", input.Repo, input.PRNumber)
		checkpoint.SkipKind = "non_open"
		if err := r.terminateLoop(ctx, input.Loop, "pr_closed_or_merged"); err != nil {
			return checkpoint, err
		}
		return checkpoint, nil
	}
	currentLogin := ""
	ensureCurrentLogin := func() error {
		if currentLogin != "" {
			return nil
		}
		lookupLogin, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
		if err != nil {
			return err
		}
		currentLogin = normalizeLogin(lookupLogin)
		checkpoint.Detail.CurrentLogin = currentLogin
		return nil
	}
	if !isManualReviewerLoop(input.Loop) && r.loopConfig.StopOnReadyLabel && specpr.HasLabel(checkpoint.Detail.Labels, specpr.ReadyLabel) {
		checkpoint.SkipReason = fmt.Sprintf("Terminated reviewer loop for ready pull request %s#%d", input.Repo, input.PRNumber)
		checkpoint.SkipKind = "ready_label"
		if err := r.terminateLoop(ctx, input.Loop, "ready_label"); err != nil {
			return checkpoint, err
		}
		return checkpoint, nil
	}
	if checkpoint.Detail.HasConflicts {
		checkpoint.SkipReason = fmt.Sprintf("Skipped conflicted pull request %s#%d", input.Repo, input.PRNumber)
		checkpoint.SkipKind = "conflicted"
		if err := r.notifyConflictedPullRequest(ctx, input, checkpoint); err != nil {
			r.logWarn("conflicted pull request notification failed; preserving skip", map[string]any{"repo": input.Repo, "prNumber": input.PRNumber, "headSha": checkpoint.Detail.HeadSHA, "error": err.Error()})
		}
		return checkpoint, nil
	}
	if !isManualReviewerLoop(input.Loop) && !policy.EnableSelfReview {
		if err := ensureCurrentLogin(); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableTransient}
		}
		if isSelfAuthoredPR(checkpoint.Detail.Author, currentLogin, policy) {
			checkpoint.SkipReason = fmt.Sprintf("Skipped self-authored pull request %s#%d for reviewer %s", input.Repo, input.PRNumber, currentLogin)
			checkpoint.SkipKind = "self_authored"
			checkpoint.SkipReviewerLogin = currentLogin
			return checkpoint, nil
		}
	}
	if !isManualReviewerLoop(input.Loop) && len(checkpoint.Detail.Reviews) > 0 && r.loopConfig.StopOnApproved {
		if err := ensureCurrentLogin(); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableTransient}
		}
		if r.loopConfig.StopOnApproved && hasApprovedReviewByAuthorForHead(checkpoint.Detail.Reviews, currentLogin, checkpoint.Detail.HeadSHA) {
			checkpoint.SkipReason = fmt.Sprintf("Terminated reviewer loop for approved pull request %s#%d", input.Repo, input.PRNumber)
			checkpoint.SkipKind = "approved"
			if err := r.terminateLoop(ctx, input.Loop, "approved"); err != nil {
				return checkpoint, err
			}
			return checkpoint, nil
		}
	}
	if !isManualReviewerLoop(input.Loop) && len(checkpoint.Detail.Reviews) > 0 {
		if err := ensureCurrentLogin(); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableTransient}
		}
		if hasReviewByAuthorForHead(checkpoint.Detail.Reviews, currentLogin, checkpoint.Detail.HeadSHA) {
			checkpoint.SkipReason = fmt.Sprintf("Skipped pull request %s#%d because current user already reviewed head %s", input.Repo, input.PRNumber, checkpoint.Detail.HeadSHA)
			checkpoint.SkipKind = "already_reviewed_by_current_user"
			checkpoint.SkipReviewerLogin = currentLogin
			return checkpoint, nil
		}
	}
	meta := parseJSONObject(input.Loop.MetadataJSON)
	if last, ok := stringFromAny(meta["lastPublishedHeadSha"]); ok && checkpoint.Detail.HeadSHA != "" && last == checkpoint.Detail.HeadSHA {
		checkpoint.SkipReason = fmt.Sprintf("Skipped already-reviewed head %s for %s#%d", checkpoint.Detail.HeadSHA, input.Repo, input.PRNumber)
		checkpoint.SkipKind = "already_published_head"
		return checkpoint, nil
	}
	requireReviewRequest := requireReviewRequestForLoop(input.Loop, policy.RequireReviewRequest, checkpoint.Detail.HeadSHA)
	if networkpolicy.IsRouted(policy.RoutedClaimPolicy) {
		decision, resolvedLogin, err := r.routedReviewerClaimDecisionWithCurrentLogin(ctx, input.Project.RepoPath, policy, currentLogin, checkpoint.Detail.Author, checkpoint.Detail.Labels, checkpoint.Detail.ReviewRequestUsers)
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableTransient}
		}
		currentLogin = resolvedLogin
		checkpoint.Detail.CurrentLogin = currentLogin
		if !decision.Allowed {
			checkpoint.SkipReason = fmt.Sprintf("Skipped routed pull request %s#%d: %s", input.Repo, input.PRNumber, decision.Reason)
			checkpoint.SkipKind = "routed_claim_ineligible"
			return checkpoint, nil
		}
		checkpoint.RoutedClaimMatchMode = string(decision.MatchMode)
	}
	if requireReviewRequest && !networkpolicy.IsRouted(policy.RoutedClaimPolicy) {
		if err := ensureCurrentLogin(); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableTransient}
		}
		if !isCurrentUserRequested(checkpoint.Detail.ReviewRequests, currentLogin) {
			if r.hasThreadResolutionFollowUpCandidate(ctx, input.Project.RepoPath, input.Repo, input.PRNumber, checkpoint.Detail.HeadSHA, currentLogin) {
				checkpoint.ThreadResolutionFollowUpOnly = true
				return checkpoint, nil
			}
			checkpoint.SkipReason = fmt.Sprintf("Skipped pull request %s#%d because current user is not requested for review", input.Repo, input.PRNumber)
			checkpoint.SkipKind = "not_requested"
			return checkpoint, nil
		}
	}
	return checkpoint, nil
}

const conflictedPRNotificationChannel = "github_pr_comment"

func (r *Runner) notifyConflictedPullRequest(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint) error {
	if r.github == nil || r.repos == nil || r.repos.Notifications == nil || checkpoint.Detail == nil {
		return nil
	}
	headSHA := strings.TrimSpace(checkpoint.Detail.HeadSHA)
	dedupeKey := fmt.Sprintf("reviewer.conflicted_pr:%s:%d:%s", input.Repo, input.PRNumber, firstNonEmpty(headSHA, "unknown"))
	marker := conflictNoticeMarker(dedupeKey)
	lockKey := "notification:" + dedupeKey
	if r.repos.Locks != nil {
		acquired, err := r.repos.Locks.Acquire(ctx, storage.LockRecord{Key: lockKey, Owner: firstNonEmpty(input.Run.ID, input.QueueItem.ID, "reviewer"), Reason: stringPtr("conflicted-pr-notification"), ExpiresAt: eventlog.FormatJavaScriptISOString(r.now().Add(5 * time.Minute)), CreatedAt: r.nowISO(), UpdatedAt: r.nowISO()})
		if err != nil {
			return err
		}
		if !acquired {
			return nil
		}
		defer func() { _ = r.repos.Locks.Release(context.Background(), lockKey) }()
	}
	latest, err := r.repos.Notifications.GetLatestByDedupe(ctx, conflictedPRNotificationChannel, dedupeKey)
	if err != nil {
		return err
	}
	if latest != nil && latest.Status == "sent" {
		return nil
	}

	nowISO := r.now().UTC().Format(time.RFC3339Nano)
	notificationID := eventlog.NewEventID("notification")
	if latest != nil && latest.ID != "" {
		notificationID = latest.ID
	}
	body := buildConflictedPullRequestNotice(input.Repo, input.PRNumber, checkpoint.Detail.Author, checkpoint.Detail.BaseRefName, marker)
	entityID := fmt.Sprintf("%s#%d", input.Repo, input.PRNumber)
	title := "PR review skipped due to merge conflicts"
	pending := storage.NotificationRecord{
		ID:         notificationID,
		ProjectID:  optionalString(input.Project.ID),
		LoopID:     optionalString(input.Loop.ID),
		RunID:      optionalString(input.Run.ID),
		EntityType: stringPtr("pull_request"),
		EntityID:   &entityID,
		Channel:    conflictedPRNotificationChannel,
		Level:      "action_required",
		Title:      title,
		Subtitle:   &entityID,
		Body:       body,
		Status:     "pending",
		DedupeKey:  &dedupeKey,
		CreatedAt:  firstNonEmpty(latestCreatedAt(latest), nowISO),
		UpdatedAt:  nowISO,
	}
	if err := r.repos.Notifications.Upsert(ctx, pending); err != nil {
		return err
	}
	if conflictNoticeAlreadyPosted(checkpoint.Detail.IssueComments, marker) {
		payload, _ := json.Marshal(map[string]any{"dedupedBy": "existing_comment", "headSha": headSHA})
		sentAt := r.now().UTC().Format(time.RFC3339Nano)
		sent := pending
		sent.Status = "sent"
		sent.PayloadJSON = optionalString(string(payload))
		sent.SentAt = &sentAt
		sent.UpdatedAt = sentAt
		return r.repos.Notifications.Upsert(ctx, sent)
	}

	comment, err := r.github.CreateIssueComment(ctx, IssueCommentInput{Repo: input.Repo, IssueNumber: input.PRNumber, Body: body, CWD: input.Project.RepoPath})
	if err != nil {
		failed := pending
		failed.Status = "failed"
		failed.ErrorMessage = optionalString(err.Error())
		failed.UpdatedAt = r.now().UTC().Format(time.RFC3339Nano)
		_ = r.repos.Notifications.Upsert(ctx, failed)
		return err
	}
	payload, _ := json.Marshal(map[string]any{"commentId": comment.ID, "commentUrl": comment.URL, "headSha": headSHA})
	sentAt := r.now().UTC().Format(time.RFC3339Nano)
	sent := pending
	sent.Status = "sent"
	sent.PayloadJSON = optionalString(string(payload))
	sent.SentAt = &sentAt
	sent.UpdatedAt = sentAt
	if err := r.repos.Notifications.Upsert(ctx, sent); err != nil {
		r.logWarn("conflicted pull request notification status update failed after posting comment", map[string]any{"repo": input.Repo, "prNumber": input.PRNumber, "headSha": headSHA, "error": err.Error()})
	}
	return nil
}

func buildConflictedPullRequestNotice(repo string, prNumber int64, author string, baseRefName string, marker string) string {
	author = strings.TrimPrefix(strings.TrimSpace(author), "@")
	mention := "PR author"
	if author != "" {
		mention = "@" + author
	}
	base := strings.TrimSpace(baseRefName)
	if base == "" {
		base = "the base branch"
	}
	return fmt.Sprintf("%s I'm holding off on generating review comments for %s#%d because this pull request has merge conflicts right now.\n\nPlease resolve the conflicts with %s and push the updated branch. Once that's done, request or wait for the review to run again and I'll take another look.\n\n%s", mention, repo, prNumber, base, marker)
}

func conflictNoticeMarker(dedupeKey string) string {
	sum := sha256.Sum256([]byte(dedupeKey))
	return fmt.Sprintf("<!-- looper:conflict-notice id=%s -->", hex.EncodeToString(sum[:])[:16])
}

func conflictNoticeAlreadyPosted(comments []map[string]any, marker string) bool {
	if marker == "" {
		return false
	}
	for _, comment := range comments {
		body, _ := stringFromAny(comment["body"])
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

func latestCreatedAt(record *storage.NotificationRecord) string {
	if record == nil {
		return ""
	}
	return record.CreatedAt
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
	if checkpoint.Worktree != nil {
		if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: checkpoint.Worktree.Path, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot}); err != nil {
			checkpoint.Worktree = nil
			checkpoint.ResumePolicy = "advance_from_checkpoint"
		} else if reviewerWorktreePrepared(checkpoint) {
			return checkpoint, nil
		}
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
	prepared, err := r.git.PrepareWorktree(ctx, PrepareWorktreeInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: created.WorktreePath, Branch: branch, Ref: prRef, ExpectedHeadSHA: checkpoint.Snapshot.HeadSHA})
	if err != nil {
		var remoteHeadChanged *gitinfra.RemoteHeadChangedError
		if errors.As(err, &remoteHeadChanged) {
			return markReviewerRunStale(checkpoint, remoteHeadChanged.Error()), nil
		}
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

type threadResolutionAgentDecision struct {
	ThreadID   string `json:"threadId"`
	Decision   string `json:"decision"`
	Evidence   string `json:"evidence"`
	Confidence string `json:"confidence"`
}

type threadResolutionAgentOutput struct {
	Decisions []threadResolutionAgentDecision `json:"decisions"`
}

func (r *Runner) runThreadResolutionStep(ctx context.Context, input stepInput) (reviewerCheckpoint, error) {
	checkpoint := input.Checkpoint
	policy := r.threadResolution
	if checkpoint.SkipReason != "" || !policy.Enabled {
		return checkpoint, nil
	}
	if checkpoint.ThreadResolution != nil && checkpoint.Snapshot != nil && checkpoint.ThreadResolution.HeadSHA == checkpoint.Snapshot.HeadSHA {
		return checkpoint, nil
	}
	if checkpoint.Detail == nil || checkpoint.Snapshot == nil {
		return checkpoint, &loopError{message: "Missing PR detail or snapshot checkpoint for thread resolution step", kind: FailureRetryableTransient}
	}
	if r.hasPendingHeadChangeNativeResume(ctx, input.Loop.ID) {
		return checkpoint, nil
	}
	if normalizePRState(checkpoint.Detail.State) != "open" {
		return checkpoint, nil
	}
	currentLogin, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
	if err != nil {
		return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	currentLogin = normalizeLogin(currentLogin)
	limit := policy.MaxThreadsPerRun
	if limit <= 0 {
		limit = 10
	}
	fetchLimit := limit * 5
	if fetchLimit < 100 {
		fetchLimit = 100
	}
	threads, err := r.github.ListReviewThreads(ctx, ListReviewThreadsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath, Limit: fetchLimit})
	if err != nil {
		return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	candidates := make([]ReviewThread, 0, len(threads))
	for _, thread := range threads {
		if len(candidates) >= limit {
			break
		}
		if r.threadResolutionCandidate(thread, checkpoint.Snapshot.HeadSHA, currentLogin, policy) {
			candidates = append(candidates, thread)
		}
	}
	result := &threadResolutionCheckpoint{HeadSHA: checkpoint.Snapshot.HeadSHA, Reported: len(candidates)}
	if len(candidates) == 0 {
		checkpoint.ThreadResolution = result
		return checkpoint, nil
	}
	decisions, err := r.classifyReviewThreads(ctx, input, checkpoint, candidates)
	if err != nil {
		return checkpoint, err
	}
	decisionByID := map[string]threadResolutionAgentDecision{}
	for _, decision := range decisions {
		decisionByID[decision.ThreadID] = decision
	}
	for _, thread := range candidates {
		decision, ok := decisionByID[thread.ID]
		if !ok {
			decision = threadResolutionAgentDecision{ThreadID: thread.ID, Decision: "needs_human", Evidence: "no classifier decision returned", Confidence: "low"}
		}
		result.Processed++
		auditedForHead := hasSufficientThreadResolutionAuditForDecision(policy, thread, checkpoint.Snapshot.HeadSHA, decision)
		commented := false
		resolved := false
		skippedReason := ""
		if !auditedForHead && r.threadResolutionShouldComment(policy, decision) {
			latestThread, refreshedDetail, err := r.refreshThreadResolutionCandidate(ctx, input, checkpoint.Snapshot.HeadSHA, currentLogin, policy, thread.ID, fetchLimit)
			if err != nil {
				checkpoint = markThreadResolutionRediscoveryOnRefreshError(checkpoint, err)
				return checkpoint, err
			}
			if latestThread == nil {
				skippedReason = "candidate_no_longer_eligible"
				r.appendThreadResolutionEvent(ctx, input, checkpoint.Snapshot.HeadSHA, strings.TrimSpace(decision.Decision), strings.TrimSpace(decision.Evidence), thread.ID, "skipped", skippedReason)
				continue
			}
			checkpoint.Detail.ReviewRequests = cloneStrings(refreshedDetail.ReviewRequests)
			body := r.buildThreadResolutionReply(thread.ID, checkpoint.Snapshot.HeadSHA, decision, policy)
			if err := r.github.AddReviewThreadReply(ctx, AddReviewThreadReplyInput{Repo: input.Repo, ThreadID: thread.ID, Body: body, CWD: input.Project.RepoPath}); err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableTransient}
			}
			result.Commented++
			commented = true
		}
		if r.threadResolutionShouldResolve(policy, decision) {
			latestThread, refreshedDetail, err := r.refreshThreadResolutionCandidate(ctx, input, checkpoint.Snapshot.HeadSHA, currentLogin, policy, thread.ID, fetchLimit)
			if err != nil {
				checkpoint = markThreadResolutionRediscoveryOnRefreshError(checkpoint, err)
				return checkpoint, err
			}
			if latestThread == nil {
				skippedReason = "candidate_no_longer_eligible"
				continue
			}
			checkpoint.Detail.ReviewRequests = cloneStrings(refreshedDetail.ReviewRequests)
			if !hasObjectiveThreadResolutionAuditForHead(*latestThread, thread.ID, checkpoint.Snapshot.HeadSHA) && !commented {
				skippedReason = "missing_objective_audit_comment"
				continue
			}
			if err := r.github.ResolveReviewThread(ctx, ResolveReviewThreadInput{Repo: input.Repo, ThreadID: thread.ID, CWD: input.Project.RepoPath}); err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableTransient}
			}
			result.Resolved++
			resolved = true
		}
		action := "reported"
		if resolved {
			action = "resolved"
		} else if commented {
			action = "commented"
		} else if skippedReason != "" {
			action = "skipped"
		}
		r.appendThreadResolutionEvent(ctx, input, checkpoint.Snapshot.HeadSHA, strings.TrimSpace(decision.Decision), strings.TrimSpace(decision.Evidence), thread.ID, action, skippedReason)
	}
	checkpoint.ThreadResolution = result
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func markThreadResolutionRediscoveryOnRefreshError(checkpoint reviewerCheckpoint, err error) reviewerCheckpoint {
	var typed *loopError
	if errors.As(err, &typed) && typed.kind == FailureRetryableAfterResume && strings.Contains(typed.message, "PR changed during thread reconciliation") {
		checkpoint.ResumePolicy = "restart_from_discover"
	}
	return checkpoint
}

func (r *Runner) hasThreadResolutionFollowUpCandidate(ctx context.Context, cwd, repo string, prNumber int64, headSHA, currentLogin string) bool {
	policy := r.threadResolution
	if !policy.Enabled || policy.RequireCurrentReviewRequest || r.github == nil || strings.TrimSpace(headSHA) == "" || strings.TrimSpace(currentLogin) == "" {
		return false
	}
	limit := policy.MaxThreadsPerRun
	if limit <= 0 {
		limit = 10
	}
	fetchLimit := limit * 5
	if fetchLimit < 100 {
		fetchLimit = 100
	}
	threads, err := r.github.ListReviewThreads(ctx, ListReviewThreadsInput{Repo: repo, PRNumber: prNumber, CWD: cwd, Limit: fetchLimit})
	if err != nil {
		r.logWarn("reviewer thread resolution follow-up candidate lookup failed", map[string]any{"repo": repo, "prNumber": prNumber, "error": err.Error()})
		return false
	}
	for _, thread := range threads {
		if r.threadResolutionCandidate(thread, headSHA, currentLogin, policy) {
			return true
		}
	}
	return false
}

func (r *Runner) threadResolutionCandidate(thread ReviewThread, headSHA, currentLogin string, policy config.ReviewerThreadResolutionConfig) bool {
	if thread.IsResolved || thread.ID == "" || len(thread.Comments) == 0 {
		return false
	}
	first := thread.Comments[0]
	if policy.Scope == config.ReviewerThreadResolutionScopeLooperAuthoredOnly && normalizeLogin(first.Author) != currentLogin {
		return false
	}
	if policy.Scope == config.ReviewerThreadResolutionScopeLooperAuthoredOnly && !isLooperAuthoredThread(thread) {
		return false
	}
	if policy.RequireNewHeadSinceThread && headSHA != "" {
		threadSHA := latestThreadFeedbackCommitOID(thread)
		if threadSHA == "" || threadSHA == headSHA {
			return false
		}
	}
	if policy.Mode != config.ReviewerThreadResolutionModeResolveObjective && hasThreadResolutionAuditForHead(thread, headSHA) {
		return false
	}
	return true
}

func (r *Runner) refreshThreadResolutionCandidate(ctx context.Context, input stepInput, headSHA, currentLogin string, policy config.ReviewerThreadResolutionConfig, threadID string, limit int) (*ReviewThread, PullRequestDetail, error) {
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return nil, PullRequestDetail{}, &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	if normalizePRState(detail.State) != "open" || detail.HeadSHA != headSHA {
		return nil, PullRequestDetail{}, &loopError{message: "PR changed during thread reconciliation", kind: FailureRetryableAfterResume}
	}
	if policy.RequireCurrentReviewRequest && !isCurrentUserRequested(detail.ReviewRequests, currentLogin) {
		return nil, detail, nil
	}
	latest, err := r.github.ListReviewThreads(ctx, ListReviewThreadsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath, Limit: limit})
	if err != nil {
		return nil, detail, &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	for i := range latest {
		if latest[i].ID == threadID {
			if !r.threadResolutionCandidate(latest[i], headSHA, currentLogin, policy) {
				return nil, detail, nil
			}
			return &latest[i], detail, nil
		}
	}
	return nil, detail, nil
}

func (r *Runner) classifyReviewThreads(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint, threads []ReviewThread) ([]threadResolutionAgentDecision, error) {
	if r.agentExecutor == nil {
		return nil, &loopError{message: "reviewer agent executor is not configured", kind: FailureRetryableTransient}
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return nil, err
	}
	executionID := eventlog.NewEventID("agent")
	candidateIDs := make([]string, 0, len(threads))
	for _, thread := range threads {
		candidateIDs = append(candidateIDs, thread.ID)
	}
	slices.Sort(candidateIDs)
	idempotencyKey := fmt.Sprintf("reviewer-thread-resolution:%s:%d:%s:%s", input.Repo, input.PRNumber, checkpoint.Snapshot.HeadSHA, strings.Join(candidateIDs, ","))
	prompt := buildThreadResolutionPrompt(input.Repo, input.PRNumber, checkpoint.Snapshot.HeadSHA, threads)
	if r.hasPendingNativeResume(ctx, input.Loop.ID) {
		prompt = nativeResumeContinuationPrompt("thread-resolution", input.Repo, input.PRNumber, checkpoint.Snapshot.HeadSHA, idempotencyKey)
	}
	execution, err := r.agentExecutor.Start(ctx, AgentRunInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Prompt: prompt, WorkingDirectory: worktree.Path, Timeout: r.agentTimeout, HeartbeatTimeout: r.agentIdleTimeout, Metadata: map[string]any{"loopType": "reviewer", "phase": "thread_resolution", "repo": input.Repo, "prNumber": input.PRNumber}, IdempotencyKey: idempotencyKey})
	if err != nil {
		return nil, err
	}
	r.appendReviewerAgentEvent(ctx, input, "reviewer.agent.started", "thread_resolution", executionID, map[string]any{"promptBytes": len(prompt), "threadCount": len(threads), "timeoutSeconds": durationSeconds(r.agentTimeout), "idleTimeoutSeconds": durationSeconds(r.agentIdleTimeout)})
	agentStartedAt := r.now()
	result, err := execution.Wait(ctx)
	if err != nil {
		r.appendReviewerAgentEvent(ctx, input, "reviewer.agent.failed", "thread_resolution", executionID, reviewerAgentWaitErrorPayload(err, r.now().Sub(agentStartedAt)))
		r.logWarn("reviewer agent wait failed", map[string]any{"projectId": input.Project.ID, "loopId": input.Loop.ID, "runId": input.Run.ID, "repo": input.Repo, "prNumber": input.PRNumber, "phase": "thread_resolution", "executionId": executionID, "elapsedSeconds": durationSeconds(r.now().Sub(agentStartedAt)), "error": err.Error()})
		r.markAgentExecutionNativeResumePendingForTransientProvider(ctx, executionID, err.Error())
		return nil, err
	}
	r.appendReviewerAgentEvent(ctx, input, reviewerAgentTerminalEvent(result), "thread_resolution", executionID, reviewerAgentResultPayload(result, r.now().Sub(agentStartedAt)))
	r.logInfo("reviewer agent completed", map[string]any{"projectId": input.Project.ID, "loopId": input.Loop.ID, "runId": input.Run.ID, "repo": input.Repo, "prNumber": input.PRNumber, "phase": "thread_resolution", "executionId": executionID, "status": result.Status, "timeoutType": result.TimeoutType, "elapsedSeconds": elapsedSeconds(result, r.now().Sub(agentStartedAt)), "parseStatus": result.ParseStatus})
	if result.Status != "completed" {
		message := reviewerAgentFailureMessage("thread resolution", result, "thread resolution classifier failed")
		r.markAgentExecutionNativeResumePendingForTransientProvider(ctx, executionID, message)
		return nil, &loopError{message: message, kind: FailureRetryableTransient}
	}
	parsed, err := parseThreadResolutionOutput(result.Stdout)
	if err == nil {
		return parsed.Decisions, nil
	}
	if message := transientProviderMessageFromAgentResult(result); message != "" {
		r.markAgentExecutionNativeResumePendingForTransientProvider(ctx, executionID, message)
		return nil, &loopError{message: message, kind: FailureRetryableTransient}
	}
	return nil, &loopError{message: err.Error(), kind: FailureNonRetryable}
}

func buildThreadResolutionPrompt(repo string, prNumber int64, headSHA string, threads []ReviewThread) string {
	payload, _ := json.MarshalIndent(map[string]any{"repo": repo, "prNumber": prNumber, "headSHA": headSHA, "threads": threads}, "", "  ")
	return strings.TrimSpace(`You are running Looper's reviewer thread reconciliation phase.

Inspect the current worktree and the unresolved pull request review threads in the JSON payload below. Classify whether each requested change is objectively addressed at the current head.

Safety rules:
- The current working directory is Looper's prepared reviewer worktree for this PR and is the canonical local checkout. Reuse it for git fetch, git checkout, diff inspection, and local verification. Do not run gh repo clone, git clone, or create any additional checkout for this PR's base or head repository unless the provided worktree is missing or unusable.
- Return objectively_fixed only for concrete, verifiable code or documentation changes that are present in the worktree.
- Return needs_human for subjective, product, design, security-sensitive, ambiguous, or partially addressed feedback.
- Do not treat an author reply like "fixed" as evidence by itself.
- Do not call GitHub APIs and do not post comments.

Output only valid JSON in this exact shape:
{"decisions":[{"threadId":"<id>","decision":"objectively_fixed|needs_human|not_fixed","evidence":"brief concrete evidence","confidence":"high|medium|low"}]}

Payload:
` + string(payload))
}

func parseThreadResolutionOutput(stdout string) (threadResolutionAgentOutput, error) {
	trimmed := strings.TrimSpace(stdout)
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end < start {
		return threadResolutionAgentOutput{}, fmt.Errorf("thread resolution classifier did not return JSON")
	}
	var parsed threadResolutionAgentOutput
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &parsed); err != nil {
		return threadResolutionAgentOutput{}, fmt.Errorf("parse thread resolution classifier output: %w", err)
	}
	return parsed, nil
}

func (r *Runner) threadResolutionShouldComment(policy config.ReviewerThreadResolutionConfig, decision threadResolutionAgentDecision) bool {
	switch policy.Mode {
	case config.ReviewerThreadResolutionModeCommentOnly, config.ReviewerThreadResolutionModeSuggestResolution:
		return true
	case config.ReviewerThreadResolutionModeResolveObjective:
		return policy.RequireAuditComment && isObjectiveThreadResolutionDecision(decision)
	default:
		return false
	}
}

func (r *Runner) threadResolutionShouldResolve(policy config.ReviewerThreadResolutionConfig, decision threadResolutionAgentDecision) bool {
	return policy.Mode == config.ReviewerThreadResolutionModeResolveObjective && policy.AutoResolve == config.ReviewerThreadResolutionAutoResolveObjectiveOnly && policy.RequireAuditComment && isObjectiveThreadResolutionDecision(decision)
}

func isObjectiveThreadResolutionDecision(decision threadResolutionAgentDecision) bool {
	return strings.EqualFold(strings.TrimSpace(decision.Decision), "objectively_fixed") && strings.EqualFold(strings.TrimSpace(decision.Confidence), "high")
}

func (r *Runner) buildThreadResolutionReply(threadID, headSHA string, decision threadResolutionAgentDecision, policy config.ReviewerThreadResolutionConfig) string {
	evidence := strings.TrimSpace(decision.Evidence)
	if evidence == "" {
		evidence = "the current head"
	}
	decisionValue := strings.ToLower(strings.TrimSpace(decision.Decision))
	if decisionValue == "" {
		decisionValue = "needs_human"
	}
	marker := fmt.Sprintf("<!-- looper:thread-resolution thread=%s head=%s decision=%s -->", threadID, headSHA, decisionValue)
	if isObjectiveThreadResolutionDecision(decision) {
		if policy.Mode == config.ReviewerThreadResolutionModeSuggestResolution {
			return fmt.Sprintf("Looper checked this thread against head `%s`. The requested change appears objectively addressed by %s. Please resolve this thread if you agree.\n%s", headSHA, evidence, marker)
		}
		if policy.Mode == config.ReviewerThreadResolutionModeResolveObjective {
			return fmt.Sprintf("Looper checked this thread against head `%s`. The requested change appears objectively addressed by %s, so I’m resolving this thread. Reopen if this still needs discussion.\n%s", headSHA, evidence, marker)
		}
		return fmt.Sprintf("Looper checked this thread against head `%s`. The requested change appears objectively addressed by %s.\n%s", headSHA, evidence, marker)
	}
	return fmt.Sprintf("Looper checked this thread against head `%s`. I could not verify that this thread is objectively resolved: %s.\n%s", headSHA, evidence, marker)
}

func hasThreadResolutionAuditForHead(thread ReviewThread, headSHA string) bool {
	needle := "looper:thread-resolution"
	headNeedle := "head=" + headSHA
	for _, comment := range thread.Comments {
		if strings.Contains(comment.Body, needle) && (headSHA == "" || strings.Contains(comment.Body, headNeedle)) {
			return true
		}
	}
	return false
}

func hasSufficientThreadResolutionAuditForDecision(policy config.ReviewerThreadResolutionConfig, thread ReviewThread, headSHA string, decision threadResolutionAgentDecision) bool {
	if policy.Mode == config.ReviewerThreadResolutionModeResolveObjective && isObjectiveThreadResolutionDecision(decision) {
		return hasObjectiveThreadResolutionAuditForHead(thread, thread.ID, headSHA)
	}
	return hasThreadResolutionAuditForHead(thread, headSHA)
}

func hasObjectiveThreadResolutionAuditForHead(thread ReviewThread, threadID, headSHA string) bool {
	for _, comment := range thread.Comments {
		body := comment.Body
		if strings.Contains(body, "looper:thread-resolution") && strings.Contains(body, "thread="+threadID) && strings.Contains(body, "head="+headSHA) && strings.Contains(body, "decision=objectively_fixed") {
			return true
		}
	}
	return false
}

func isLooperAuthoredThread(thread ReviewThread) bool {
	if len(thread.Comments) == 0 {
		return false
	}
	body := thread.Comments[0].Body
	return strings.Contains(body, "looper:stamp") || strings.Contains(body, "looper:review")
}

func latestThreadFeedbackCommitOID(thread ReviewThread) string {
	for i := len(thread.Comments) - 1; i >= 0; i-- {
		comment := thread.Comments[i]
		if strings.Contains(comment.Body, "looper:thread-resolution") {
			continue
		}
		if oid := firstNonEmpty(comment.CommitOID, comment.OriginalCommitOID); oid != "" {
			return oid
		}
	}
	return ""
}

type reviewerHeadChangeMonitor struct {
	cancel func()
	done   chan struct{}
	signal chan reviewerHeadChangeSignal
}

type reviewerHeadChangeSignal struct {
	OldHeadSHA string
	NewHeadSHA string
	Reason     string
}

func (m reviewerHeadChangeMonitor) stop() reviewerHeadChangeSignal {
	if m.cancel == nil {
		return reviewerHeadChangeSignal{}
	}
	m.cancel()
	<-m.done
	select {
	case signal := <-m.signal:
		return signal
	default:
		return reviewerHeadChangeSignal{}
	}
}

func (r *Runner) startReviewerHeadChangeMonitor(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint, execution AgentExecution, executionID string) reviewerHeadChangeMonitor {
	expectedHead := snapshotHeadSHA(checkpoint)
	if expectedHead == "" || r.github == nil || execution == nil || r.headChangePollInterval <= 0 {
		return reviewerHeadChangeMonitor{}
	}
	monitorCtx, cancel := context.WithCancel(ctx)
	monitor := reviewerHeadChangeMonitor{cancel: cancel, done: make(chan struct{}), signal: make(chan reviewerHeadChangeSignal, 1)}
	go func() {
		defer close(monitor.done)
		ticker := time.NewTicker(r.headChangePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-monitorCtx.Done():
				return
			case <-ticker.C:
				headSHA, err := r.github.GetPullRequestHeadSHA(monitorCtx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
				if err != nil {
					if monitorCtx.Err() == nil {
						r.logWarn("reviewer head change poll failed", map[string]any{"projectId": input.Project.ID, "loopId": input.Loop.ID, "runId": input.Run.ID, "repo": input.Repo, "prNumber": input.PRNumber, "executionId": executionID, "error": err.Error()})
					}
					continue
				}
				if headSHA == "" || headSHA == expectedHead {
					continue
				}
				reason := fmt.Sprintf("PR head changed while reviewer was running: expected %s, got %s", expectedHead, headSHA)
				signal := reviewerHeadChangeSignal{OldHeadSHA: expectedHead, NewHeadSHA: headSHA, Reason: reason}
				r.appendReviewerAgentEvent(context.Background(), input, "reviewer.agent.interrupted", "review", executionID, map[string]any{"headSha": expectedHead, "newHeadSha": headSHA, "reason": reason})
				r.logInfo("reviewer agent interrupted for newer PR head", map[string]any{"projectId": input.Project.ID, "loopId": input.Loop.ID, "runId": input.Run.ID, "repo": input.Repo, "prNumber": input.PRNumber, "executionId": executionID, "expectedHeadSha": expectedHead, "actualHeadSha": headSHA})
				select {
				case monitor.signal <- signal:
				default:
				}
				if err := execution.Kill(reason); err != nil {
					r.logWarn("reviewer agent interrupt signal failed", map[string]any{"projectId": input.Project.ID, "loopId": input.Loop.ID, "runId": input.Run.ID, "repo": input.Repo, "prNumber": input.PRNumber, "executionId": executionID, "error": err.Error()})
				}
				return
			}
		}
	}()
	return monitor
}

func (r *Runner) appendThreadResolutionEvent(ctx context.Context, input stepInput, headSHA, decision, evidence, threadID, action, skippedReason string) {
	r.appendEvent(ctx, eventInput{eventType: "reviewer.thread_resolution", projectID: input.Project.ID, loopID: input.Loop.ID, runID: input.Run.ID, entityType: "pull_request", entityID: fmt.Sprintf("%s#%d", input.Repo, input.PRNumber), payload: map[string]any{"repo": input.Repo, "prNumber": input.PRNumber, "threadId": threadID, "headSha": headSHA, "decision": decision, "evidence": evidence, "action": action, "skippedReason": skippedReason}})
}

func (r *Runner) runReviewStep(ctx context.Context, input stepInput) (reviewerCheckpoint, error) {
	checkpoint := input.Checkpoint
	var err error
	if checkpoint.PendingReview != nil {
		return checkpoint, nil
	}
	if skipped, next, err := r.skipThreadResolutionFollowUpReview(ctx, input, checkpoint); skipped || err != nil {
		return next, err
	}
	if checkpoint.Snapshot == nil {
		return checkpoint, &loopError{message: "Missing PR snapshot checkpoint for review step", kind: FailureRetryableTransient}
	}
	if reviewerWorktreePrepared(checkpoint) {
		worktreeRoot, rootErr := reviewerWorktreeRoot(input.Project)
		if rootErr != nil {
			return checkpoint, rootErr
		}
		if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: checkpoint.Worktree.Path, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot}); err != nil {
			checkpoint.Worktree = nil
			checkpoint.ResumePolicy = "advance_from_checkpoint"
		}
	}
	if !reviewerWorktreePrepared(checkpoint) {
		checkpoint, err = r.runPrepareWorktreeStep(ctx, input)
		if err != nil {
			return input.Checkpoint, err
		}
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepReview, checkpoint); err != nil {
			return checkpoint, err
		}
		if checkpoint.SkipReason != "" {
			return checkpoint, nil
		}
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	executionID := eventlog.NewEventID("agent")
	idempotencyKey := agentNativeReviewID(input.Loop.ID, checkpoint.Snapshot.HeadSHA)
	policy := r.discoveryPolicyForProject(input.Project.ID)
	requireReviewRequest := requireReviewRequestForLoop(input.Loop, policy.RequireReviewRequest, checkpoint.Snapshot.HeadSHA)
	reviewRequestBypassReason := ""
	if !requireReviewRequest && policy.RequireReviewRequest && reviewerFollowUpHasNewHead(input.Loop, checkpoint.Snapshot.HeadSHA) {
		reviewRequestBypassReason = "follow_up_new_head"
	}
	prompt, instructionBlock := buildReviewPromptWithInstructions(input.Project.ID, r.customInstructions, input.Repo, input.PRNumber, checkpoint, input.Run.ID, idempotencyKey, r.effectiveReviewEvents(input.Loop.MetadataJSON), isManualReviewerLoop(input.Loop), requireReviewRequest, reviewRequestBypassReason, r.scope, r.disclosure, r.agentRuntime, r.agentModel, r.looperCLIPath, r.reviewerAutoMergeConfigForProject(input.Project.ID).Enabled)
	nativeResumePrompt := r.nativeResumePromptForReview(ctx, input, checkpoint.Snapshot.HeadSHA, idempotencyKey)
	metadata := map[string]any{"loopType": "reviewer", "repo": input.Repo, "prNumber": input.PRNumber}
	for key, value := range config.CustomInstructionMetadata(instructionBlock, prompt) {
		metadata[key] = value
	}
	execution, err := r.agentExecutor.Start(ctx, AgentRunInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Prompt: prompt, NativeResumePrompt: nativeResumePrompt, WorkingDirectory: worktree.Path, Timeout: r.agentTimeout, HeartbeatTimeout: r.agentIdleTimeout, Metadata: metadata, IdempotencyKey: idempotencyKey})
	if err != nil {
		return checkpoint, err
	}
	r.appendReviewerAgentEvent(ctx, input, "reviewer.agent.started", "review", executionID, map[string]any{"promptBytes": len(prompt), "timeoutSeconds": durationSeconds(r.agentTimeout), "idleTimeoutSeconds": durationSeconds(r.agentIdleTimeout), "scope": string(r.scope), "headSha": checkpoint.Snapshot.HeadSHA})
	agentStartedAt := r.now()
	if err := r.recordAgentExecutionStarted(ctx, input.Loop.ID); err != nil {
		r.logWarn("reviewer agent execution start metadata update failed", map[string]any{"loopId": input.Loop.ID, "runId": input.Run.ID, "error": err.Error()})
	}
	if r.onAgentExecutionStarted != nil {
		if err := r.onAgentExecutionStarted(ctx, AgentExecutionStartedInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Subtitle: fmt.Sprintf("%s#%d", input.Repo, input.PRNumber), Body: "Review started", DedupeKey: "runtime.agent.started:reviewer:" + input.Run.ID}); err != nil && r.logger != nil {
			r.logger.Warn("reviewer agent start notification failed", map[string]any{"loopId": input.Loop.ID, "runId": input.Run.ID, "error": err.Error()})
		}
	}
	headMonitor := r.startReviewerHeadChangeMonitor(ctx, input, checkpoint, execution, executionID)
	result, err := execution.Wait(ctx)
	headChange := headMonitor.stop()
	if headChange.Reason != "" {
		if r.nativeResume.OnHeadChange {
			r.markAgentExecutionNativeResumePendingForHeadChange(ctx, executionID, input, headChange)
		}
		checkpoint.PendingReview = nil
		checkpoint.ResumePolicy = "restart_from_discover"
		return checkpoint, &loopError{message: headChange.Reason, kind: FailureRetryableAfterResume, interrupted: true}
	}
	if err != nil {
		r.appendReviewerAgentEvent(ctx, input, "reviewer.agent.failed", "review", executionID, reviewerAgentWaitErrorPayload(err, r.now().Sub(agentStartedAt)))
		r.logWarn("reviewer agent wait failed", map[string]any{"projectId": input.Project.ID, "loopId": input.Loop.ID, "runId": input.Run.ID, "repo": input.Repo, "prNumber": input.PRNumber, "phase": "review", "executionId": executionID, "elapsedSeconds": durationSeconds(r.now().Sub(agentStartedAt)), "error": err.Error()})
		r.markAgentExecutionNativeResumePendingForTransientProvider(ctx, executionID, err.Error())
		return checkpoint, err
	}
	r.appendReviewerAgentEvent(ctx, input, reviewerAgentTerminalEvent(result), "review", executionID, reviewerAgentResultPayload(result, r.now().Sub(agentStartedAt)))
	r.logInfo("reviewer agent completed", map[string]any{"projectId": input.Project.ID, "loopId": input.Loop.ID, "runId": input.Run.ID, "repo": input.Repo, "prNumber": input.PRNumber, "phase": "review", "executionId": executionID, "status": result.Status, "timeoutType": result.TimeoutType, "elapsedSeconds": elapsedSeconds(result, r.now().Sub(agentStartedAt)), "parseStatus": result.ParseStatus})
	if result.Status != "completed" {
		if reason, ok := r.detectHeadChangeRequired(ctx, input, checkpoint); ok {
			return markReviewerRunStale(checkpoint, reason), nil
		}
		if found, err := r.verifyAgentNativeReviewMarker(ctx, input, checkpoint.Snapshot.HeadSHA, idempotencyKey, cleanReviewAuthorLogin(checkpoint, PullRequestDetail{})); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		} else if found.Found {
			checkpoint.PendingReview = &pendingReviewCheckpoint{HeadSHA: checkpoint.Snapshot.HeadSHA, IdempotencyKey: idempotencyKey, Event: reviewEventAgentNative, Summary: result.Summary, ContentFingerprint: reviewMarkerFingerprint(found)}
			checkpoint.ResumePolicy = "advance_from_checkpoint"
			return checkpoint, nil
		}
		if reason, ok := r.detectRediscoveryRequired(ctx, input, checkpoint); ok {
			return markReviewerRunStale(checkpoint, reason), nil
		}
		message := reviewerAgentFailureMessage("review", result, fmt.Sprintf("Reviewer agent %s", result.Status))
		kind := FailureRetryableTransient
		if isGitHubSelfApprovalFailure(message) {
			kind = FailureNonRetryable
		} else if agent.IsAgentSetupFailureMessage(message) {
			kind = FailureRetryableTransient
		}
		if kind == FailureRetryableTransient {
			r.markAgentExecutionNativeResumePendingForTransientProvider(ctx, executionID, message)
		}
		return checkpoint, &loopError{message: message, kind: kind}
	}
	if result.ParseStatus != "parsed" {
		if found, err := r.verifyAgentNativeReviewMarker(ctx, input, checkpoint.Snapshot.HeadSHA, idempotencyKey, cleanReviewAuthorLogin(checkpoint, PullRequestDetail{})); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		} else if found.Found {
			checkpoint.PendingReview = &pendingReviewCheckpoint{HeadSHA: checkpoint.Snapshot.HeadSHA, IdempotencyKey: idempotencyKey, Event: reviewEventAgentNative, Summary: result.Summary, ContentFingerprint: reviewMarkerFingerprint(found)}
			checkpoint.ResumePolicy = "advance_from_checkpoint"
			return checkpoint, nil
		}
		if reason, ok := rediscoverySignalFromAgentResult(result, requireReviewRequest); ok {
			return markReviewerRunStale(checkpoint, reason), nil
		}
		if reason, ok := r.detectRediscoveryRequired(ctx, input, checkpoint); ok {
			return markReviewerRunStale(checkpoint, reason), nil
		}
		if message := transientProviderMessageFromAgentResult(result); message != "" {
			r.markAgentExecutionNativeResumePendingForTransientProvider(ctx, executionID, message)
			return checkpoint, &loopError{message: message, kind: FailureRetryableTransient}
		}
		checkpoint.PendingReview = &pendingReviewCheckpoint{HeadSHA: checkpoint.Snapshot.HeadSHA, IdempotencyKey: idempotencyKey, Event: reviewEventAgentNative, Summary: result.Summary, MarkerVerificationMisses: 1}
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		return checkpoint, &loopError{message: "Reviewer agent did not report a valid completion marker after publishing review", kind: FailureRetryableAfterResume}
	}
	if cleanReviewNoopSummary(result.Summary) {
		policy := r.effectiveReviewEvents(input.Loop.MetadataJSON)
		if policy.Clean == config.ReviewerReviewEventApprove && r.reviewerAutoMergeConfigForProject(input.Project.ID).Enabled && resolvePullRequestPhase(detailLabels(checkpoint.Detail)) != "spec" {
			checkpoint.PendingReview = &pendingReviewCheckpoint{HeadSHA: checkpoint.Snapshot.HeadSHA, IdempotencyKey: idempotencyKey, Event: reviewEventAgentNative, Summary: result.Summary, CleanNoop: true}
			checkpoint.ResumePolicy = "advance_from_checkpoint"
			return checkpoint, nil
		}
		if policy.Clean == config.ReviewerReviewEventApprove {
			if found, err := r.verifyAgentNativeReviewMarker(ctx, input, checkpoint.Snapshot.HeadSHA, idempotencyKey, cleanReviewAuthorLogin(checkpoint, PullRequestDetail{})); err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
			} else if cleanReviewMarkerSatisfiesCleanPolicy(found, cleanReviewAuthorLogin(checkpoint, PullRequestDetail{})) {
				if err := validateCleanApprovedReviewMarkerBody(found, cleanReviewAuthorLogin(checkpoint, PullRequestDetail{})); err != nil {
					return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
				}
				checkpoint.PendingReview = &pendingReviewCheckpoint{HeadSHA: checkpoint.Snapshot.HeadSHA, IdempotencyKey: idempotencyKey, Event: reviewEventAgentNative, Summary: result.Summary, ContentFingerprint: reviewMarkerFingerprint(found), CleanNoop: true}
				checkpoint.ResumePolicy = "advance_from_checkpoint"
				return checkpoint, nil
			}
			return checkpoint, &loopError{message: "Reviewer agent reported a clean summary-only result, but clean review policy requires an APPROVED review marker; submit the APPROVE review through the trusted wrapper or exit non-zero", kind: FailureRetryableAfterResume}
		}
		checkpoint.PendingReview = &pendingReviewCheckpoint{HeadSHA: checkpoint.Snapshot.HeadSHA, IdempotencyKey: idempotencyKey, Event: reviewEventAgentNative, Summary: result.Summary, CleanNoop: true}
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		return checkpoint, nil
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
	if pending.CleanNoop {
		detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		if detail.HeadSHA != "" && pending.HeadSHA != "" && detail.HeadSHA != pending.HeadSHA {
			return markReviewerRunStale(checkpoint, fmt.Sprintf("PR head changed before publish: expected %s, got %s", pending.HeadSHA, detail.HeadSHA)), nil
		}
		if reason := reviewerPublishDriftReason(input, checkpoint, detail); reason != "" {
			return markReviewerRunStale(checkpoint, reason), nil
		}
		policy := r.discoveryPolicyForProject(input.Project.ID)
		requireReviewRequest := requireReviewRequestForLoop(input.Loop, policy.RequireReviewRequest, pending.HeadSHA)
		if requireReviewRequest {
			currentLogin, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
			if err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
			}
			if !isCurrentUserRequested(detail.ReviewRequests, normalizeLogin(currentLogin)) {
				checkpoint.SkipReason = fmt.Sprintf("Skipped pull request %s#%d because current user is not requested for review", input.Repo, input.PRNumber)
				return checkpoint, nil
			}
		}
		if criteriaResult, err := r.maybePublishCriteriaAnchoredCleanReview(ctx, input, checkpoint, pending, detail); err != nil {
			return checkpoint, err
		} else if criteriaResult != nil {
			if err := r.recordPublishedReviewProgress(ctx, input, pending, criteriaResult.reviewEvent); err != nil {
				return checkpoint, err
			}
			return checkpoint, nil
		}
		if r.effectiveReviewEvents(input.Loop.MetadataJSON).Clean == config.ReviewerReviewEventApprove {
			found, err := r.verifyAgentNativeReviewMarker(ctx, input, pending.HeadSHA, pending.IdempotencyKey, cleanReviewAuthorLogin(checkpoint, detail))
			if err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
			}
			if !cleanReviewMarkerSatisfiesCleanPolicy(found, cleanReviewAuthorLogin(checkpoint, detail)) {
				return checkpoint, &loopError{message: "Reviewer agent reported a clean summary-only result, but clean review policy requires an APPROVED review marker or a self-authored clean COMMENT fallback with a valid human approval body; submit the APPROVE review through the trusted wrapper or exit non-zero", kind: FailureRetryableAfterResume}
			}
			if err := validateCleanApprovedReviewMarkerBody(found, cleanReviewAuthorLogin(checkpoint, detail)); err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
			}
			checkpoint.PendingReview = pending.clone()
			if checkpoint.PendingReview.ContentFingerprint == "" {
				if fp := reviewMarkerFingerprint(found); fp != "" {
					checkpoint.PendingReview.ContentFingerprint = fp
				}
			}
			if err := r.applyVerifiedReviewSideEffects(ctx, input, checkpoint, detail, found); err != nil {
				return checkpoint, err
			}
			if err := r.recordPublishedReviewProgress(ctx, input, pending, pendingReviewEvent(pending)); err != nil {
				return checkpoint, err
			}
			return checkpoint, nil
		}
		if err := r.applyCleanNoopReviewSideEffects(ctx, input, checkpoint, detail); err != nil {
			return checkpoint, err
		}
		if err := r.recordPublishedReviewProgress(ctx, input, pending, ReviewEventComment); err != nil {
			return checkpoint, err
		}
		return checkpoint, nil
	}
	repo := input.Repo
	prNumber := input.PRNumber
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	if detail.HeadSHA != "" && pending.HeadSHA != "" && detail.HeadSHA != pending.HeadSHA {
		return markReviewerRunStale(checkpoint, fmt.Sprintf("PR head changed before publish: expected %s, got %s", pending.HeadSHA, detail.HeadSHA)), nil
	}
	if reason := reviewerPublishDriftReason(input, checkpoint, detail); reason != "" {
		return markReviewerRunStale(checkpoint, reason), nil
	}
	checkpoint.Detail = checkpointDetailFromDetail(detail)
	if skipped, next, err := r.skipThreadResolutionFollowUpReview(ctx, input, checkpoint); skipped || err != nil {
		return next, err
	}
	markerResult := ReviewMarkerResult{}
	if pending.Event == reviewEventAgentNative {
		found, err := r.verifyAgentNativeReviewMarker(ctx, input, pending.HeadSHA, pending.IdempotencyKey, cleanReviewAuthorLogin(checkpoint, detail))
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		markerResult = found
	} else {
		checkpoint.PendingReview = nil
		checkpoint.ResumePolicy = "rerun_review"
		return checkpoint, &loopError{message: "Legacy pending review checkpoint cannot be verified; rerunning review before marking publish success", kind: FailureRetryableAfterResume}
	}
	policy := r.discoveryPolicyForProject(input.Project.ID)
	requireReviewRequest := requireReviewRequestForLoop(input.Loop, policy.RequireReviewRequest, pending.HeadSHA)
	if requireReviewRequest && !markerResult.Found {
		currentLogin, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		if !isCurrentUserRequested(detail.ReviewRequests, normalizeLogin(currentLogin)) {
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
	reviewPolicy := r.effectiveReviewEvents(input.Loop.MetadataJSON)
	if cleanReviewNoopSummary(pending.Summary) && reviewPolicy.Clean == config.ReviewerReviewEventApprove && !cleanReviewMarkerSatisfiesCleanPolicy(markerResult, cleanReviewAuthorLogin(checkpoint, detail)) {
		return checkpoint, &loopError{message: "Reviewer agent reported a clean summary-only result, but clean review policy requires an APPROVED review marker or a self-authored clean COMMENT fallback with a valid human approval body; submit the APPROVE review through the trusted wrapper or exit non-zero", kind: FailureRetryableAfterResume}
	}
	if cleanApprovedReviewMarker(markerResult) || (reviewPolicy.Clean == config.ReviewerReviewEventApprove && cleanReviewMarkerSatisfiesCleanPolicy(markerResult, cleanReviewAuthorLogin(checkpoint, detail))) {
		if err := validateCleanApprovedReviewMarkerBody(markerResult, cleanReviewAuthorLogin(checkpoint, detail)); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
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

func (r *Runner) skipThreadResolutionFollowUpReview(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint) (bool, reviewerCheckpoint, error) {
	if !checkpoint.ThreadResolutionFollowUpOnly {
		return false, checkpoint, nil
	}
	policy := r.discoveryPolicyForProject(input.Project.ID)
	if isManualReviewerLoop(input.Loop) || !policy.RequireReviewRequest || checkpoint.Detail == nil {
		return false, checkpoint, nil
	}
	currentLogin := strings.TrimSpace(checkpoint.Detail.CurrentLogin)
	if currentLogin == "" {
		lookupLogin, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
		if err != nil {
			return false, checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		currentLogin = normalizeLogin(lookupLogin)
		checkpoint.Detail.CurrentLogin = currentLogin
	}
	if isCurrentUserRequested(checkpoint.Detail.ReviewRequests, currentLogin) {
		return false, checkpoint, nil
	}
	checkpoint.SkipReason = fmt.Sprintf("Skipped pull request %s#%d because current user is not requested for review", input.Repo, input.PRNumber)
	checkpoint.SkipKind = "not_requested"
	checkpoint.PendingReview = nil
	return true, checkpoint, nil
}

func (r *Runner) verifyAgentNativeReviewMarker(ctx context.Context, input stepInput, headSHA string, idempotencyKey string, prAuthorLogin string) (ReviewMarkerResult, error) {
	currentLogin, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
	if err != nil {
		return ReviewMarkerResult{}, err
	}
	marker := agentNativeReviewMarker(input.Loop.ID, headSHA, idempotencyKey)
	allowedEvents := r.allowedReviewEventsForPolicy(r.effectiveReviewEvents(input.Loop.MetadataJSON))
	allowCleanComment := sameReviewAuthorLogin(currentLogin, prAuthorLogin)
	found, err := r.github.FindReviewMarker(ctx, VerifyReviewMarkerInput{Repo: input.Repo, PRNumber: input.PRNumber, Marker: marker, AllowedReviewEvents: allowedEvents, AuthorLogin: currentLogin, AllowCleanComment: allowCleanComment, CWD: input.Project.RepoPath})
	if err != nil || found.Found {
		return found, err
	}
	loopMarker := agentNativeLoopReviewMarker(input.Loop.ID, headSHA)
	found, err = r.github.FindReviewMarker(ctx, VerifyReviewMarkerInput{Repo: input.Repo, PRNumber: input.PRNumber, Marker: loopMarker, AllowedReviewEvents: allowedEvents, AuthorLogin: currentLogin, AllowCleanComment: allowCleanComment, CWD: input.Project.RepoPath})
	if err != nil || found.Found {
		return found, err
	}
	return found, nil
}

func sameReviewAuthorLogin(a string, b string) bool {
	a = strings.TrimSpace(strings.TrimPrefix(a, "@"))
	b = strings.TrimSpace(strings.TrimPrefix(b, "@"))
	return a != "" && strings.EqualFold(a, b)
}

func cleanReviewMarkerSatisfiesCleanPolicy(marker ReviewMarkerResult, prAuthorLogin string) bool {
	if cleanApprovedReviewMarker(marker) {
		return true
	}
	return marker.Found && marker.Event == ReviewEventComment && strings.EqualFold(strings.TrimSpace(marker.Outcome), "clean") && len(marker.InlineCommentBodies) == 0 && sameReviewAuthorLogin(marker.AuthorLogin, prAuthorLogin)
}

func markReviewerRunStale(checkpoint reviewerCheckpoint, reason string) reviewerCheckpoint {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "PR drift detected before publish"
	}
	checkpoint.SkipReason = reason
	checkpoint.SkipKind = "stale"
	checkpoint.PendingReview = nil
	checkpoint.ResumePolicy = ""
	return checkpoint
}

func reviewerPublishDriftReason(input stepInput, checkpoint reviewerCheckpoint, detail PullRequestDetail) string {
	if state := normalizePRState(detail.State); state != "" && state != "open" {
		observed := strings.ToUpper(strings.TrimSpace(detail.State))
		if observed == "" {
			observed = strings.ToUpper(state)
		}
		return fmt.Sprintf("PR drift detected before publish: expected PR state OPEN, observed %s for %s#%d", observed, input.Repo, input.PRNumber)
	}
	if checkpoint.Detail != nil {
		if checkpoint.Detail.IsDraft != detail.IsDraft {
			return fmt.Sprintf("PR drift detected before publish: draft status changed from %t to %t for %s#%d", checkpoint.Detail.IsDraft, detail.IsDraft, input.Repo, input.PRNumber)
		}
		if checkpoint.Detail.BaseRefName != "" && detail.BaseRefName != "" && checkpoint.Detail.BaseRefName != detail.BaseRefName {
			return fmt.Sprintf("PR base branch changed before publish: expected %s, got %s", checkpoint.Detail.BaseRefName, detail.BaseRefName)
		}
		if checkpoint.Detail.HeadRefName != "" && detail.HeadRefName != "" && checkpoint.Detail.HeadRefName != detail.HeadRefName {
			return fmt.Sprintf("PR head branch changed before publish: expected %s, got %s", checkpoint.Detail.HeadRefName, detail.HeadRefName)
		}
	}
	return ""
}

func (r *Runner) appendReviewerAgentEvent(ctx context.Context, input stepInput, eventType, phase, executionID string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["repo"] = input.Repo
	payload["prNumber"] = input.PRNumber
	payload["phase"] = phase
	payload["executionId"] = executionID
	r.appendEvent(ctx, eventInput{eventType: eventType, projectID: input.Project.ID, loopID: input.Loop.ID, runID: input.Run.ID, entityType: "pull_request", entityID: fmt.Sprintf("%s#%d", input.Repo, input.PRNumber), payload: payload})
}

func reviewerAgentTerminalEvent(result AgentResult) string {
	if result.Status == "timeout" {
		return "reviewer.agent.timed_out"
	}
	if result.Status != "completed" {
		return "reviewer.agent.failed"
	}
	return "reviewer.agent.completed"
}

func reviewerAgentResultPayload(result AgentResult, observedElapsed time.Duration) map[string]any {
	return map[string]any{
		"status":                       result.Status,
		"timeoutType":                  result.TimeoutType,
		"configuredIdleTimeoutSeconds": result.ConfiguredIdleTimeoutSeconds,
		"configuredMaxRuntimeSeconds":  result.ConfiguredMaxRuntimeSeconds,
		"elapsedRuntimeSeconds":        elapsedSeconds(result, observedElapsed),
		"lastProgressAt":               result.LastProgressAt,
		"parseStatus":                  result.ParseStatus,
		"summary":                      firstNonEmpty(result.Summary, summarizeAgentStderr(result.Stderr)),
	}
}

func reviewerAgentWaitErrorPayload(err error, observedElapsed time.Duration) map[string]any {
	summary := ""
	if err != nil {
		summary = err.Error()
	}
	return map[string]any{
		"status":                "wait_error",
		"elapsedRuntimeSeconds": durationSeconds(observedElapsed),
		"summary":               summary,
	}
}

func reviewerAgentFailureMessage(phase string, result AgentResult, fallback string) string {
	base := firstNonEmpty(result.Summary, result.Stderr, fallback)
	if result.Status != "timeout" {
		return base
	}
	timeoutType := result.TimeoutType
	if timeoutType == "" {
		timeoutType = "unknown"
	}
	parts := []string{fmt.Sprintf("Reviewer %s agent timed out (%s)", phase, timeoutType)}
	if result.ElapsedRuntimeSeconds > 0 {
		parts = append(parts, fmt.Sprintf("after %ds", result.ElapsedRuntimeSeconds))
	}
	if result.ConfiguredMaxRuntimeSeconds > 0 || result.ConfiguredIdleTimeoutSeconds > 0 {
		parts = append(parts, fmt.Sprintf("configured max runtime %ds, idle timeout %ds", result.ConfiguredMaxRuntimeSeconds, result.ConfiguredIdleTimeoutSeconds))
	}
	if result.LastProgressAt != "" {
		parts = append(parts, "last progress at "+result.LastProgressAt)
	}
	if base != "" {
		parts = append(parts, "summary: "+base)
	}
	return strings.Join(parts, "; ")
}

func isGitHubSelfApprovalFailure(message string) bool {
	normalized := strings.ToLower(message)
	return strings.Contains(normalized, "can not approve your own pull request") ||
		strings.Contains(normalized, "cannot approve your own pull request")
}

func summarizeAgentStderr(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if len(stderr) <= 240 {
		return stderr
	}
	return strings.TrimSpace(stderr[:240]) + "…"
}

func elapsedSeconds(result AgentResult, observedElapsed time.Duration) int64 {
	if result.ElapsedRuntimeSeconds > 0 {
		return result.ElapsedRuntimeSeconds
	}
	return durationSeconds(observedElapsed)
}

func durationSeconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	seconds := int64(duration / time.Second)
	if seconds == 0 {
		return 1
	}
	return seconds
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
		policy := r.effectiveReviewEvents(input.Loop.MetadataJSON)
		shouldTransitionSpecLabels := cleanSpecLabelTransitionAllowed(policy, marker.Event, outcome)
		if err := r.applyCleanSpecLabelTransition(ctx, input, checkpoint, detail, shouldTransitionSpecLabels); err != nil {
			return err
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

func (r *Runner) applyCleanNoopReviewSideEffects(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint, detail PullRequestDetail) error {
	reaction := PullRequestReactionInput{Repo: input.Repo, PRNumber: input.PRNumber, Content: "+1", CWD: input.Project.RepoPath}
	if err := r.github.AddPullRequestReaction(ctx, reaction); err != nil {
		return &loopError{message: fmt.Sprintf("Failed to add clean-review reaction before marking publish success: %v", err), kind: FailureRetryableAfterResume}
	}
	policy := r.effectiveReviewEvents(input.Loop.MetadataJSON)
	shouldTransitionSpecLabels := cleanSpecLabelTransitionAllowed(policy, cleanReviewEventForPolicy(policy), "clean")
	return r.applyCleanSpecLabelTransition(ctx, input, checkpoint, detail, shouldTransitionSpecLabels)
}

func cleanSpecLabelTransitionAllowed(policy config.ReviewerReviewEventsConfig, event ReviewEvent, outcome string) bool {
	return strings.EqualFold(strings.TrimSpace(outcome), "clean") && policy.Clean == config.ReviewerReviewEventApprove && event == ReviewEventApprove
}

func cleanReviewEventForPolicy(policy config.ReviewerReviewEventsConfig) ReviewEvent {
	if policy.Clean == config.ReviewerReviewEventApprove {
		return ReviewEventApprove
	}
	return ReviewEventComment
}

func (r *Runner) applyCleanSpecLabelTransition(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint, detail PullRequestDetail, enabled bool) error {
	if !enabled {
		return nil
	}
	specReviewingLabel := r.specReviewingLabel(input.Project.ID)
	checkpointHadSpecReviewing := specpr.HasLabel(detailLabels(checkpoint.Detail), specReviewingLabel)
	if !checkpointHadSpecReviewing && !specpr.HasLabel(detail.Labels, specReviewingLabel) {
		return nil
	}
	freshDetail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return &loopError{message: fmt.Sprintf("Failed to refresh pull request review state before spec-ready transition: %v", err), kind: FailureRetryableAfterResume}
	}
	if detail.HeadSHA != "" && freshDetail.HeadSHA != "" && detail.HeadSHA != freshDetail.HeadSHA {
		return &loopError{message: fmt.Sprintf("PR head changed before spec-ready transition: expected %s, got %s", detail.HeadSHA, freshDetail.HeadSHA), kind: FailureRetryableAfterResume}
	}
	if !specpr.IsReviewClean(freshDetail.ReviewDecision, freshDetail.Comments) {
		return nil
	}
	if specpr.HasLabel(freshDetail.Labels, specReviewingLabel) {
		if err := r.github.RemovePullRequestLabels(ctx, PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: []string{specReviewingLabel}, CWD: input.Project.RepoPath}); err != nil {
			return &loopError{message: fmt.Sprintf("Failed to remove spec-reviewing label before marking publish success: %v", err), kind: FailureRetryableAfterResume}
		}
	}
	if !specpr.HasLabel(freshDetail.Labels, specpr.ReadyLabel) {
		if err := r.github.AddPullRequestLabels(ctx, PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: []string{specpr.ReadyLabel}, CWD: input.Project.RepoPath}); err != nil {
			return &loopError{message: fmt.Sprintf("Failed to add spec-ready label before marking publish success: %v", err), kind: FailureRetryableAfterResume}
		}
	}
	return nil
}

type linkedIssueReference struct {
	Repo    string
	Number  int64
	Tracked bool
}

type criteriaPublishResult struct {
	reviewEvent ReviewEvent
	marker      ReviewMarkerResult
	recordOnly  bool
}

const (
	criteriaFailCommentMarker     = "<!-- looper:reviewer:criteria-fail -->"
	autoMergeRefusedCommentMarker = "<!-- looper:reviewer:automerge-refused -->"
	criteriaVerificationHeading   = "### Acceptance criteria verification"
)

func (r *Runner) maybePublishCriteriaAnchoredCleanReview(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint, pending pendingReviewCheckpoint, detail PullRequestDetail) (*criteriaPublishResult, error) {
	if resolvePullRequestPhase(detail.Labels) == "spec" {
		return nil, nil
	}
	if r.effectiveReviewEvents(input.Loop.MetadataJSON).Clean != config.ReviewerReviewEventApprove {
		return nil, nil
	}
	autoMergeCfg := r.reviewerAutoMergeConfigForProject(input.Project.ID)
	if !autoMergeCfg.Enabled {
		return nil, nil
	}
	issueRef, issue, ok, err := r.resolveLinkedIssueForCriteria(ctx, input, detail)
	if err != nil {
		return nil, err
	}
	if !ok {
		return r.publishCleanReviewWithoutCriteria(ctx, input, checkpoint, pending, detail)
	}
	extracted := criteria.Extract(issue.Body)
	if len(extracted) == 0 {
		return r.publishCleanReviewWithoutCriteria(ctx, input, checkpoint, pending, detail)
	}
	verification, err := r.verifyAcceptanceCriteria(criteriaVerificationDiff(checkpoint, detail), extracted)
	if err != nil {
		return nil, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	if verification.Disposition != criteria.DispositionPass {
		return r.publishCriteriaFailureReview(ctx, input, detail, pending, issueRef, issue, verification)
	}
	return r.publishCriteriaApprovedReview(ctx, input, checkpoint, pending, detail, autoMergeCfg, issueRef, verification)
}

func (r *Runner) publishCleanReviewWithoutCriteria(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint, pending pendingReviewCheckpoint, detail PullRequestDetail) (*criteriaPublishResult, error) {
	policy := r.effectiveReviewEvents(input.Loop.MetadataJSON)
	if policy.Clean != config.ReviewerReviewEventApprove {
		if err := r.applyCleanNoopReviewSideEffects(ctx, input, checkpoint, detail); err != nil {
			return nil, err
		}
		return &criteriaPublishResult{reviewEvent: ReviewEventComment, recordOnly: true}, nil
	}
	body := stampReviewBody(r.disclosure, buildCleanApprovalBody(cleanReviewAuthorLogin(checkpoint, detail), criteriaVerificationHeading, nil, "No explicit acceptance criteria were stated on the linked issue, so this review follows the standard clean-review path."), "reviewer")
	marker, err := r.submitOrReuseReview(ctx, input, detail, pending, ReviewEventApprove, "clean", body)
	if err != nil {
		return nil, err
	}
	if err := r.applyVerifiedReviewSideEffects(ctx, input, checkpoint, detail, marker); err != nil {
		return nil, err
	}
	return &criteriaPublishResult{reviewEvent: marker.Event, marker: marker}, nil
}

func (r *Runner) publishCriteriaApprovedReview(ctx context.Context, input stepInput, checkpoint reviewerCheckpoint, pending pendingReviewCheckpoint, detail PullRequestDetail, autoMergeCfg config.ReviewerAutoMergeConfig, issueRef linkedIssueReference, verification criteria.VerificationResult) (*criteriaPublishResult, error) {
	body := stampReviewBody(r.disclosure, buildCleanApprovalBody(cleanReviewAuthorLogin(checkpoint, detail), criteriaVerificationHeading, verification.Criteria, "I verified each stated acceptance criterion against the current PR diff before approving."), "reviewer")
	marker, err := r.submitOrReuseReview(ctx, input, detail, pending, ReviewEventApprove, "clean", body)
	if err != nil {
		return nil, err
	}
	if err := r.applyVerifiedReviewSideEffects(ctx, input, checkpoint, detail, marker); err != nil {
		return nil, err
	}
	if marker.Event != ReviewEventApprove {
		return &criteriaPublishResult{reviewEvent: marker.Event, marker: marker}, nil
	}
	decision, err := r.decideAutoMerge(ctx, input, detail, autoMergeCfg, issueRef)
	if err != nil {
		return nil, err
	}
	if decision.Reason == "" {
		if err := r.github.EnableAutoMerge(ctx, githubinfra.EnableAutoMergeInput{Repo: input.Repo, PRNumber: input.PRNumber, Strategy: decision.Strategy, HeadSHA: pending.HeadSHA, CWD: input.Project.RepoPath}); err != nil && !isAlreadyEnabledAutoMergeError(err) {
			return nil, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		return &criteriaPublishResult{reviewEvent: marker.Event, marker: marker}, nil
	}
	if err := r.postStampedPRCommentIfMissing(ctx, input, detail, autoMergeRefusedCommentMarker, fmt.Sprintf("Auto-merge opt-in was refused for this PR: %s.", decision.Reason)); err != nil {
		return nil, err
	}
	return &criteriaPublishResult{reviewEvent: marker.Event, marker: marker}, nil
}

func (r *Runner) publishCriteriaFailureReview(ctx context.Context, input stepInput, detail PullRequestDetail, pending pendingReviewCheckpoint, issueRef linkedIssueReference, issue githubinfra.IssueDetail, verification criteria.VerificationResult) (*criteriaPublishResult, error) {
	body := stampReviewBody(r.disclosure, buildCriteriaFailureBody(verification.Criteria), "reviewer")
	marker, err := r.submitOrReuseReview(ctx, input, detail, pending, ReviewEventComment, "non_blocking", body)
	if err != nil {
		return nil, err
	}
	if err := r.github.RemoveIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: issueRef.Repo, IssueNumber: issueRef.Number, Labels: criteriaFailureLabels(issue.Labels), CWD: input.Project.RepoPath}); err != nil {
		return nil, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	if err := r.github.RemovePullRequestReaction(ctx, PullRequestReactionInput{Repo: input.Repo, PRNumber: input.PRNumber, Content: "+1", CWD: input.Project.RepoPath}); err != nil {
		return nil, &loopError{message: fmt.Sprintf("Failed to remove stale clean-review reaction before marking publish success: %v", err), kind: FailureRetryableAfterResume}
	}
	return &criteriaPublishResult{reviewEvent: ReviewEventComment, marker: marker}, nil
}

func (r *Runner) resolveLinkedIssueForCriteria(ctx context.Context, input stepInput, detail PullRequestDetail) (linkedIssueReference, githubinfra.IssueDetail, bool, error) {
	if ref, ok := parseClosingReference(input.Repo, detail.Body); ok {
		issue, err := r.github.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: ref.Repo, IssueNumber: ref.Number, CWD: input.Project.RepoPath})
		if err != nil {
			if linkedIssueLookupUnavailable(err) {
				return linkedIssueReference{}, githubinfra.IssueDetail{}, false, nil
			}
			return linkedIssueReference{}, githubinfra.IssueDetail{}, false, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		if issue.IsPullRequest {
			return linkedIssueReference{}, githubinfra.IssueDetail{}, false, nil
		}
		ref.Tracked = issueHasCoordinatorTracking(issue.Labels)
		return ref, issue, true, nil
	}
	return linkedIssueReference{}, githubinfra.IssueDetail{}, false, nil
}

func linkedIssueLookupUnavailable(err error) bool {
	if err == nil || githubinfra.IsTransientError(err) {
		return false
	}
	if githubinfra.IsNotFoundError(err) {
		return true
	}
	message := strings.ToLower(githubinfra.ErrorMessage(err))
	return strings.Contains(message, "resource not accessible") || strings.Contains(message, "http 403") || strings.Contains(message, "http 410")
}

func (r *Runner) verifyAcceptanceCriteria(rawDiff string, extracted []criteria.AcceptanceCriterion) (criteria.VerificationResult, error) {
	verifier := r.criteriaVerifier
	if verifier == nil {
		verifier = criteria.NewDefaultVerifier()
	}
	return criteria.Verify(extracted, parseCriteriaPRDiff(rawDiff), verifier)
}

func (r *Runner) decideAutoMerge(ctx context.Context, input stepInput, detail PullRequestDetail, autoMergeCfg config.ReviewerAutoMergeConfig, issueRef linkedIssueReference) (automerge.AutoMergeDecision, error) {
	decision := automerge.Decide(
		automerge.PRSnapshot{Labels: append([]string(nil), detail.Labels...), HasTrackedIssueLink: issueRef.Tracked},
		autoMergeCfg,
		automerge.BranchProtectionSnapshot{},
		automerge.RepoSettingsSnapshot{},
	)
	if decision.Reason == automerge.RefusalReasonDisabled || decision.Reason == automerge.RefusalReasonScope {
		return decision, nil
	}
	settings, err := r.github.GetRepositorySettings(ctx, githubinfra.RepositorySettingsInput{Repo: input.Repo, CWD: input.Project.RepoPath})
	if err != nil {
		return automerge.AutoMergeDecision{}, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	protection := githubinfra.BranchProtection{}
	if autoMergeCfg.RequireBranchProtection {
		protection, err = r.github.GetBranchProtection(ctx, githubinfra.BranchProtectionInput{Repo: input.Repo, Branch: firstNonEmpty(detail.BaseRefName, "main"), CWD: input.Project.RepoPath})
		if err != nil {
			return automerge.AutoMergeDecision{}, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
	}
	return automerge.Decide(
		automerge.PRSnapshot{Labels: append([]string(nil), detail.Labels...), HasTrackedIssueLink: issueRef.Tracked},
		autoMergeCfg,
		automerge.BranchProtectionSnapshot{Exists: protection.Enabled, HasRequiredChecks: protection.HasRequiredChecks},
		automerge.RepoSettingsSnapshot{AllowSquashMerge: settings.AllowSquashMerge, AllowMergeCommit: settings.AllowMergeCommit, AllowRebaseMerge: settings.AllowRebaseMerge, AllowAutoMerge: settings.AllowAutoMerge},
	), nil
}

func (r *Runner) submitOrReuseReview(ctx context.Context, input stepInput, detail PullRequestDetail, pending pendingReviewCheckpoint, event ReviewEvent, outcome string, body string) (ReviewMarkerResult, error) {
	body = appendReviewMarker(body, agentNativeReviewMarker(input.Loop.ID, pending.HeadSHA, pending.IdempotencyKey), outcome)
	currentLogin, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
	if err != nil {
		return ReviewMarkerResult{}, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	selfApprovalFallback := event == ReviewEventApprove && sameReviewAuthorLogin(detail.Author, currentLogin)
	found, err := r.verifyAgentNativeReviewMarker(ctx, input, pending.HeadSHA, pending.IdempotencyKey, cleanReviewAuthorLogin(reviewerCheckpoint{Detail: checkpointDetailFromDetail(detail)}, detail))
	if err != nil {
		return ReviewMarkerResult{}, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	if found.Found && strings.EqualFold(strings.TrimSpace(found.Outcome), strings.TrimSpace(outcome)) && (found.Event == event || (selfApprovalFallback && found.Event == ReviewEventComment)) {
		return found, nil
	}
	submitEvent := event
	if selfApprovalFallback {
		submitEvent = ReviewEventComment
	}
	if err := r.github.SubmitReview(ctx, githubinfra.SubmitReviewInput{Repo: input.Repo, PRNumber: input.PRNumber, Event: string(submitEvent), Body: body, CommitID: pending.HeadSHA, Disclosure: r.disclosure, CWD: input.Project.RepoPath}); err != nil {
		return ReviewMarkerResult{}, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	marker, err := r.verifyAgentNativeReviewMarker(ctx, input, pending.HeadSHA, pending.IdempotencyKey, cleanReviewAuthorLogin(reviewerCheckpoint{Detail: checkpointDetailFromDetail(detail)}, detail))
	if err != nil {
		return ReviewMarkerResult{}, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	if !marker.Found || !strings.EqualFold(strings.TrimSpace(marker.Outcome), strings.TrimSpace(outcome)) || (marker.Event != submitEvent && !(submitEvent == ReviewEventComment && event == ReviewEventApprove && sameReviewAuthorLogin(detail.Author, currentLogin))) {
		return ReviewMarkerResult{}, &loopError{message: fmt.Sprintf("Reviewer submitted %s review for outcome=%s but could not verify the idempotency marker", submitEvent, outcome), kind: FailureRetryableAfterResume}
	}
	return marker, nil
}

func appendReviewMarker(body string, marker string, outcome string) string {
	if strings.Contains(body, marker) {
		return body
	}
	markerBody := fmt.Sprintf("<!-- %s outcome=%s -->", strings.TrimSpace(marker), strings.TrimSpace(outcome))
	trimmed := strings.TrimRight(body, "\n")
	if trimmed == "" {
		return markerBody
	}
	return trimmed + "\n\n" + markerBody
}

func (r *Runner) postStampedPRCommentIfMissing(ctx context.Context, input stepInput, detail PullRequestDetail, marker string, visible string) error {
	if stampedCommentAlreadyPosted(detail.IssueComments, marker) {
		return nil
	}
	body := stampIssueComment(r.disclosure, visible+"\n\n"+marker, "reviewer")
	if _, err := r.github.CreateIssueComment(ctx, IssueCommentInput{Repo: input.Repo, IssueNumber: input.PRNumber, Body: body, CWD: input.Project.RepoPath}); err != nil {
		return &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
	}
	return nil
}

func stampedCommentAlreadyPosted(comments []map[string]any, marker string) bool {
	if marker == "" {
		return false
	}
	for _, comment := range comments {
		body, _ := stringFromAny(comment["body"])
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

func parseCriteriaPRDiff(raw string) criteria.PRDiff {
	files := []criteria.DiffFile{}
	var current *criteria.DiffFile
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			if current != nil {
				files = append(files, *current)
			}
			path := parseDiffFilePath(line)
			current = &criteria.DiffFile{Path: path}
			continue
		}
		if current == nil {
			continue
		}
		if current.Patch == "" {
			current.Patch = line
		} else {
			current.Patch += "\n" + line
		}
	}
	if current != nil {
		files = append(files, *current)
	}
	return criteria.PRDiff{Files: files}
}

func criteriaVerificationDiff(checkpoint reviewerCheckpoint, detail PullRequestDetail) string {
	if strings.TrimSpace(detail.Diff) != "" {
		return detail.Diff
	}
	if checkpoint.Snapshot == nil || strings.TrimSpace(checkpoint.Snapshot.PayloadJSON) == "" {
		return ""
	}
	payload := parseJSONObject(&checkpoint.Snapshot.PayloadJSON)
	if diff, ok := payload["diff"].(string); ok {
		return diff
	}
	return ""
}

func parseDiffFilePath(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return ""
	}
	path := strings.TrimPrefix(fields[3], "b/")
	if path == fields[3] {
		path = strings.TrimPrefix(fields[2], "a/")
	}
	return strings.Trim(path, `"`)
}

func parseClosingReference(defaultRepo, body string) (linkedIssueReference, bool) {
	match := reviewerIssueClosingReferencePattern.FindStringSubmatch(body)
	if len(match) != 2 {
		return linkedIssueReference{}, false
	}
	reference := strings.TrimSpace(match[1])
	repo, number, ok := splitIssueReference(defaultRepo, reference)
	if !ok {
		return linkedIssueReference{}, false
	}
	return linkedIssueReference{Repo: repo, Number: number}, true
}

func splitIssueReference(defaultRepo, reference string) (string, int64, bool) {
	parts := strings.Split(reference, "#")
	if len(parts) != 2 {
		return "", 0, false
	}
	number, err := parseInt64(strings.TrimSpace(parts[1]))
	if err != nil || number <= 0 {
		return "", 0, false
	}
	repo := strings.TrimSpace(parts[0])
	if repo == "" {
		repo = strings.TrimSpace(defaultRepo)
	}
	if repo == "" {
		return "", 0, false
	}
	return repo, number, true
}

func parseInt64(raw string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

func buildCleanApprovalBody(authorLogin string, heading string, results []criteria.CriterionResult, intro string) string {
	parts := []string{fmt.Sprintf("%s Thanks for the update — I reviewed the current PR head and it's ready to move forward.", cleanReviewAuthorMention(authorLogin))}
	if strings.TrimSpace(intro) != "" {
		parts = append(parts, intro)
	}
	if heading != "" {
		parts = append(parts, heading, formatCriteriaResults(results, true))
	}
	parts = append(parts, "Happy to see this tightened up — nice work.")
	return strings.Join(parts, "\n\n")
}

func buildCriteriaFailureBody(results []criteria.CriterionResult) string {
	return strings.Join([]string{"Acceptance criteria could not be fully verified for this PR head.", criteriaVerificationHeading, formatCriteriaResults(results, false), criteriaFailCommentMarker}, "\n\n")
}

func formatCriteriaResults(results []criteria.CriterionResult, includeOnlyPass bool) string {
	if len(results) == 0 {
		if includeOnlyPass {
			return "- No explicit acceptance criteria were available to verify."
		}
		return "- No acceptance criteria results were recorded."
	}
	lines := make([]string, 0, len(results))
	for _, result := range results {
		if includeOnlyPass && result.Verdict != criteria.VerdictPass {
			continue
		}
		line := fmt.Sprintf("- **%s** — %s", result.Criterion, strings.ToUpper(string(result.Verdict)))
		if pointers := formatEvidencePointers(result.Evidence); pointers != "" {
			line += " (" + pointers + ")"
		}
		if justification := strings.TrimSpace(result.Justification); justification != "" {
			line += ": " + justification
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 && includeOnlyPass {
		return "- No passing acceptance criteria were recorded."
	}
	return strings.Join(lines, "\n")
}

func formatEvidencePointers(evidence []criteria.Evidence) string {
	parts := make([]string, 0, len(evidence))
	for _, entry := range evidence {
		if entry.FilePath == "" || entry.StartLine < 1 {
			continue
		}
		if entry.EndLine > entry.StartLine {
			parts = append(parts, fmt.Sprintf("%s:%d-%d", entry.FilePath, entry.StartLine, entry.EndLine))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%d", entry.FilePath, entry.StartLine))
	}
	return strings.Join(parts, ", ")
}

func criteriaFailureLabels(labels []string) []string {
	toRemove := []string{}
	for _, label := range labels {
		normalized := strings.ToLower(strings.TrimSpace(label))
		if normalized == "triaged" || strings.HasPrefix(normalized, "dispatch/") {
			toRemove = append(toRemove, label)
		}
	}
	return toRemove
}

func issueHasCoordinatorTracking(labels []string) bool {
	for _, label := range labels {
		normalized := strings.ToLower(strings.TrimSpace(label))
		if normalized == "triaged" || strings.HasPrefix(normalized, "dispatch/") {
			return true
		}
	}
	return false
}

func isAlreadyEnabledAutoMergeError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "auto-merge is already enabled") || strings.Contains(message, "already enabled for this pull request")
}

func stampReviewBody(cfg config.DisclosureConfig, body string, runner string) string {
	return disclosure.Stamper{Config: cfg, Version: version.Current().Version}.Markdown(body, runner, disclosure.ChannelReviewComment)
}

func stampIssueComment(cfg config.DisclosureConfig, body string, runner string) string {
	return disclosure.Stamper{Config: cfg, Version: version.Current().Version}.Markdown(body, runner, disclosure.ChannelIssueComment)
}

func cleanReviewNoopSummary(summary string) bool {
	normalized := strings.ToLower(strings.TrimSpace(summary))
	if normalized == "" {
		return false
	}
	return strings.HasPrefix(normalized, "no actionable findings")
}

func cleanApprovedReviewMarker(found ReviewMarkerResult) bool {
	return found.Found && found.Event == ReviewEventApprove && strings.EqualFold(strings.TrimSpace(found.Outcome), "clean") && len(found.InlineCommentBodies) == 0
}

func reviewHumanVisibleBody(body string) string {
	cleaned := reviewMarkerCommentPattern.ReplaceAllString(body, "")
	cleaned = disclosure.StripMarkdownStamp(cleaned)
	cleaned = reviewHumanHTMLCommentPattern.ReplaceAllString(cleaned, "")
	cleaned = reviewHumanReferenceDefinitionPattern.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func validateCleanApprovedReviewMarkerBody(marker ReviewMarkerResult, authorLogin string) error {
	visible := reviewHumanVisibleBody(marker.Body)
	mention := cleanReviewAuthorMention(authorLogin)
	if mention == "" {
		return fmt.Errorf("clean APPROVE review body requires the PR author login for @mention validation")
	}
	fields := strings.Fields(visible)
	if len(fields) == 0 || !strings.EqualFold(fields[0], mention) {
		return fmt.Errorf("clean APPROVE review body must start with PR author mention %s", mention)
	}
	if len(fields) < 12 {
		return fmt.Errorf("clean APPROVE review body must include a short human summary and friendly acknowledgement, not only markers or disclosure")
	}
	return nil
}

func cleanReviewAuthorLogin(checkpoint reviewerCheckpoint, detail PullRequestDetail) string {
	if author := strings.TrimSpace(detail.Author); author != "" {
		return author
	}
	if checkpoint.Detail != nil && strings.TrimSpace(checkpoint.Detail.Author) != "" {
		return checkpoint.Detail.Author
	}
	if checkpoint.Snapshot != nil && strings.TrimSpace(checkpoint.Snapshot.Author) != "" {
		return checkpoint.Snapshot.Author
	}
	return ""
}

func cleanReviewAuthorMention(login string) string {
	login = strings.TrimSpace(strings.TrimPrefix(login, "@"))
	if login == "" {
		return ""
	}
	return "@" + login
}

func (r *Runner) specReviewingLabel(projectID string) string {
	if label := strings.TrimSpace(r.discoveryPolicyForProject(projectID).SpecReviewingLabel); label != "" {
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
		budgetTerminationReason := deprecatedReviewerLoopBudgetTerminationReason(*existing)
		if terminalReviewerLoopReason(*existing) != "" && budgetTerminationReason == "" {
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
		if budgetTerminationReason != "" && updated.Status == "terminated" {
			updated.Status = "queued"
			updated.NextRunAt = &nowISO
		}
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

func (r *Runner) recoverFailedReviewerLoop(ctx context.Context, loop storage.LoopRecord, pr PullRequestSummary) (*storage.LoopRecord, error) {
	eligible, queueID, reason, err := r.failedReviewerLoopRecoveryEligibility(ctx, loop, pr)
	if err != nil {
		return nil, err
	}
	if !eligible {
		r.logInfo("reviewer auto-recovery skipped", map[string]any{"loopId": loop.ID, "reason": reason})
		r.appendEvent(ctx, eventInput{eventType: "reviewer.auto_recovery.skipped", projectID: loop.ProjectID, loopID: loop.ID, entityType: "loop", entityID: loop.ID, payload: map[string]any{"reason": reason}})
		return nil, nil
	}
	nowISO := r.nowISO()
	latestQueue, err := r.repos.Queue.GetByID(ctx, queueID)
	if err != nil {
		return nil, err
	}
	requeued, err := r.requeueFailedReviewerQueueItem(ctx, loop.ID, queueID, nowISO, latestQueue, reason)
	if err != nil {
		return nil, err
	}
	if requeued == 0 {
		active, activeErr := r.repos.Queue.FindActiveByLoopID(ctx, loop.ID)
		if activeErr != nil {
			return nil, activeErr
		}
		if active == nil {
			return nil, fmt.Errorf("reviewer auto-recovery did not requeue failed queue item %s for loop %s", queueID, loop.ID)
		}
	}
	updated, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		updated.Status = "queued"
		updated.NextRunAt = stringPtr(nowISO)
		meta := parseJSONObject(updated.MetadataJSON)
		loopMeta := reviewerLoopMetadata(meta)
		loopMeta["status"] = "active"
		loopMeta["lastStatus"] = "auto_recovered"
		loopMeta["autoRecoveryAttempts"] = intFromAny(loopMeta["autoRecoveryAttempts"]) + 1
		loopMeta["lastAutoRecoveryReason"] = reason
		delete(loopMeta, "terminationReason")
		removeDeprecatedReviewerLoopBudgetMetadata(loopMeta)
		meta["loop"] = loopMeta
		if encoded, marshalErr := json.Marshal(meta); marshalErr == nil {
			text := string(encoded)
			updated.MetadataJSON = &text
		}
	})
	if err != nil {
		return nil, err
	}
	r.logInfo("reviewer auto-recovered failed loop", map[string]any{"loopId": loop.ID, "reason": reason})
	r.appendEvent(ctx, eventInput{eventType: "reviewer.auto_recovery.requeued", projectID: loop.ProjectID, loopID: loop.ID, entityType: "loop", entityID: loop.ID, payload: map[string]any{"reason": reason, "attempt": intFromAny(reviewerLoopMetadata(parseJSONObject(updated.MetadataJSON))["autoRecoveryAttempts"])}})
	return &updated, nil
}

func (r *Runner) requeueFailedReviewerQueueItem(ctx context.Context, loopID, queueID, queuedAt string, queue *storage.QueueItemRecord, reason string) (int64, error) {
	if queue != nil && (isRetryableTransientWithRemainingAttempts(*queue) || reason == "enhanced_transient_match_attempts_remaining") {
		return r.repos.Queue.RequeueFailedByIDWithAttempts(ctx, loopID, queueID, queuedAt, queue.Attempts)
	}
	return r.repos.Queue.RequeueFailedByID(ctx, loopID, queueID, queuedAt)
}

func (r *Runner) failedReviewerLoopRecoveryEligibility(ctx context.Context, loop storage.LoopRecord, pr PullRequestSummary) (bool, string, string, error) {
	if loop.Type != "reviewer" || loop.Status != "failed" || isManualReviewerLoop(loop) {
		return false, "", "not_failed_reviewer_loop", nil
	}
	retryPolicy := r.retryPolicyForProject(loop.ProjectID)
	if normalizePRState(pr.State) != "open" {
		return false, "", "pr_not_open", nil
	}
	if !r.discoveryPolicyForProject(loop.ProjectID).IncludeDrafts && pr.IsDraft {
		return false, "", "draft_pr", nil
	}
	if r.loopConfig.StopOnReadyLabel && specpr.HasLabel(pr.Labels, specpr.ReadyLabel) {
		return false, "", "ready_label", nil
	}
	meta := parseJSONObject(loop.MetadataJSON)
	if !r.loopEnabled(meta) {
		return false, "", "loop_disabled", nil
	}
	loopMeta := reviewerLoopMetadata(meta)
	if reason, _ := stringFromAny(loopMeta["terminationReason"]); reason != "" && !isDeprecatedReviewerLoopBudgetReason(reason) {
		return false, "", reason, nil
	}
	if intFromAny(loopMeta["autoRecoveryAttempts"]) >= retryPolicy.AutoRecoveryMaxAttempts {
		return false, "", "auto_recovery_attempt_cap", nil
	}
	latestRun, err := r.repos.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return false, "", "", err
	}
	latestQueue, err := r.repos.Queue.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return false, "", "", err
	}
	if latestQueue == nil || latestQueue.Status != "failed" {
		return false, "", "latest_queue_not_failed", nil
	}
	if latestRun == nil || latestRun.Status != "failed" {
		return false, "", "latest_run_not_failed", nil
	}
	checkpoint := reviewerCheckpoint{}
	checkpoint = parseCheckpoint(latestRun.CheckpointJSON)
	queueKind := ""
	if latestQueue.LastErrorKind != nil {
		queueKind = *latestQueue.LastErrorKind
	}
	resumePolicy := loops.NormalizeResumePolicy(queueKind, checkpoint.ResumePolicy)
	if loops.SuppressesAutonomousRecovery(queueKind, resumePolicy) {
		return false, "", "manual_intervention", nil
	}
	approvedByCurrentUser := func() (bool, error) {
		if !r.loopConfig.StopOnApproved || len(pr.Reviews) == 0 {
			return false, nil
		}
		currentLogin, err := r.currentLoginForLoop(ctx, loop)
		if err != nil {
			return false, err
		}
		return hasApprovedReviewByAuthorForHead(pr.Reviews, currentLogin, pr.HeadSHA), nil
	}
	if queueKind == string(FailureRetryableAfterResume) && (resumePolicy == loops.ResumePolicyRestartFromDiscover || resumePolicy == "rerun_review") {
		approved, err := approvedByCurrentUser()
		if err != nil {
			return false, "", "", err
		}
		if approved {
			return false, "", "approved", nil
		}
		return true, latestQueue.ID, "retryable_after_resume_" + checkpoint.ResumePolicy, nil
	}
	if isRetryableTransientWithRemainingAttempts(*latestQueue) {
		approved, err := approvedByCurrentUser()
		if err != nil {
			return false, "", "", err
		}
		if approved {
			return false, "", "approved", nil
		}
		return true, latestQueue.ID, "retryable_transient_attempts_remaining", nil
	}
	latestMessage := firstNonEmpty(derefString(latestRun.Summary), derefString(latestRun.ErrorMessage))
	if latestMessage == "" {
		latestMessage = derefString(latestQueue.LastError)
	}
	if retryPolicy.RecoverExistingMatchedFailures && queueHasRemainingAttempts(*latestQueue) && r.isEnhancedTransientMessageForPolicy(retryPolicy, latestMessage) {
		approved, err := approvedByCurrentUser()
		if err != nil {
			return false, "", "", err
		}
		if approved {
			return false, "", "approved", nil
		}
		return true, latestQueue.ID, "enhanced_transient_match_attempts_remaining", nil
	}
	if isKnownReviewerRediscoveryGuardrail(latestMessage) && isReviewerRediscoveryRunStep(latestRun) {
		approved, err := approvedByCurrentUser()
		if err != nil {
			return false, "", "", err
		}
		if approved {
			return false, "", "approved", nil
		}
		return true, latestQueue.ID, "historical_guardrail", nil
	}
	return false, "", "not_whitelisted", nil
}

func isRetryableTransientWithRemainingAttempts(queue storage.QueueItemRecord) bool {
	if derefString(queue.LastErrorKind) != string(FailureRetryableTransient) {
		return false
	}
	return queueHasRemainingAttempts(queue)
}

func queueHasRemainingAttempts(queue storage.QueueItemRecord) bool {
	nextAttempts := queue.Attempts + 1
	return queue.MaxAttempts > 0 && nextAttempts < queue.MaxAttempts
}

func isKnownReviewerRediscoveryGuardrail(message string) bool {
	return strings.Contains(message, "PR head changed before publish") || strings.Contains(message, "review request removed before publish")
}

func isReviewerRediscoveryRunStep(run *storage.RunRecord) bool {
	if run == nil || run.CurrentStep == nil {
		return false
	}
	step := strings.TrimSpace(*run.CurrentStep)
	return step == string(stepPublish) || step == string(stepReview) || step == string(stepThreadResolution)
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

func requireReviewRequestForLoop(loop storage.LoopRecord, requireReviewRequest bool, headSHA string) bool {
	if isManualReviewerLoop(loop) {
		return false
	}
	if !requireReviewRequest {
		return false
	}
	return !reviewerFollowUpHasNewHead(loop, headSHA)
}

func reviewerFollowUpHasNewHead(loop storage.LoopRecord, headSHA string) bool {
	if strings.TrimSpace(headSHA) == "" {
		return false
	}
	meta := parseJSONObject(loop.MetadataJSON)
	if enabled, ok := meta["followUpdates"].(bool); !ok || !enabled {
		return false
	}
	loopMeta := reviewerLoopMetadata(meta)
	if enabled, ok := loopMeta["enabled"].(bool); ok && !enabled {
		return false
	}
	lastPublishedHeadSHA, ok := stringFromAny(meta["lastPublishedHeadSha"])
	return ok && lastPublishedHeadSHA != "" && lastPublishedHeadSHA != headSHA
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
				persisted, _, err := r.repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, updated)
				if err != nil {
					return storage.QueueItemRecord{}, err
				}
				updated = persisted
				r.wakeSchedulerAfterEnqueue()
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
	persisted, created, err := r.repos.Queue.CreateOrGetActiveByDedupe(ctx, queueItem)
	if err != nil {
		return storage.QueueItemRecord{}, err
	}
	if created {
		r.wakeSchedulerAfterEnqueue()
	}
	return persisted, nil
}

func (r *Runner) wakeSchedulerAfterEnqueue() {
	if r.onQueueItemEnqueued != nil {
		r.onQueueItemEnqueued()
	}
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
		delay := backoffDelay(r.retryBaseDelay, nextAttempts, r.retryMaxDelayForProject(derefString(queueItem.ProjectID)))
		retryAt := eventlog.FormatJavaScriptISOString(r.now().Add(delay))
		if err := r.repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: queueItem.ID, AvailableAt: retryAt, Attempts: nextAttempts, ErrorMessage: optionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
			return nil, err
		}
	} else {
		if err := r.repos.Queue.Fail(ctx, storage.QueueFailInput{ID: queueItem.ID, Attempts: nextAttempts, FinishedAt: nowISO, ErrorMessage: optionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
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
	return r.classifyFailureForProject("", err)
}

func (r *Runner) classifyFailureForProject(projectID string, err error) *loopError {
	return r.classifyFailureForProjectAndBoundary(projectID, err, failureclass.BoundaryUnknown)
}

func (r *Runner) classifyFailureForProjectAndBoundary(projectID string, err error, boundary failureclass.Boundary) *loopError {
	var typed *loopError
	if errors.As(err, &typed) {
		return typed
	}
	var remoteHeadChanged *gitinfra.RemoteHeadChangedError
	if errors.As(err, &remoteHeadChanged) {
		return &loopError{message: remoteHeadChanged.Error(), kind: FailureRetryableAfterResume}
	}
	var transient transientFailure
	if errors.As(err, &transient) && transient.Temporary() {
		return &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	if githubinfra.IsTransientError(err) {
		return &loopError{message: githubinfra.ErrorMessage(err), kind: FailureRetryableTransient}
	}
	if isTransientModelProviderError(err) {
		return &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	if r.isEnhancedTransientFailureForPolicy(r.retryPolicyForProject(projectID), err) {
		return &loopError{message: githubinfra.ErrorMessage(err), kind: FailureRetryableTransient}
	}
	return &loopError{message: err.Error(), kind: reviewerFailureKind(failureclass.Classify(err, failureclass.Context{Runner: failureclass.RunnerReviewer, Boundary: boundary}))}
}

func reviewerFailureBoundaryForStep(step ReviewerStep) failureclass.Boundary {
	switch step {
	case stepDiscover, stepFilter, stepClaim, stepSnapshot, stepThreadResolution, stepPublish:
		return failureclass.BoundaryGitHubAPI
	case stepWorktree:
		return failureclass.BoundaryGitRemote
	case stepReview:
		return failureclass.BoundaryModelProvider
	default:
		return failureclass.BoundaryUnknown
	}
}

func reviewerFailureKind(kind failureclass.Kind) QueueFailureKind {
	switch kind {
	case failureclass.RetryableTransient:
		return FailureRetryableTransient
	case failureclass.RetryableAfterResume:
		return FailureRetryableAfterResume
	case failureclass.ManualIntervention:
		return FailureManualIntervention
	default:
		return FailureNonRetryable
	}
}

func (r *Runner) isTransientExternalFailure(err error) bool {
	return r.isTransientExternalFailureForProject("", err)
}

func (r *Runner) isTransientExternalFailureForProject(projectID string, err error) bool {
	if err == nil {
		return false
	}
	retryPolicy := r.retryPolicyForProject(projectID)
	if githubinfra.IsTransientError(err) || isTransientModelProviderError(err) {
		return true
	}
	if r.isEnhancedTransientFailureForPolicy(retryPolicy, err) {
		return true
	}
	var loopErr *loopError
	if errors.As(err, &loopErr) {
		return loopErr.kind == FailureRetryableTransient && (isTransientModelProviderMessage(loopErr.message) || r.isEnhancedTransientMessageForPolicy(retryPolicy, loopErr.message))
	}
	return false
}

func (r *Runner) isEnhancedTransientFailure(err error) bool {
	return r.isEnhancedTransientFailureForPolicy(r.retryPolicyForProject(""), err)
}

func (r *Runner) isEnhancedTransientFailureForPolicy(policy config.ReviewerRetryConfig, err error) bool {
	if err == nil {
		return false
	}
	return r.isEnhancedTransientMessageForPolicy(policy, err.Error()) || config.ReviewerRetryMessageMatches(policy, githubinfra.ErrorMessage(err))
}

func (r *Runner) isEnhancedTransientMessage(message string) bool {
	return r.isEnhancedTransientMessageForPolicy(r.retryPolicyForProject(""), message)
}

func (r *Runner) isEnhancedTransientMessageForPolicy(policy config.ReviewerRetryConfig, message string) bool {
	return config.ReviewerRetryMessageMatches(policy, message)
}

func (r *Runner) markAgentExecutionNativeResumePendingForTransientProvider(ctx context.Context, executionID string, message string) bool {
	if strings.TrimSpace(executionID) == "" || !isTransientModelProviderMessage(message) || r.repos == nil || r.repos.AgentExecutions == nil {
		return false
	}
	record, err := r.repos.AgentExecutions.GetByID(ctx, executionID)
	if err != nil {
		r.logWarn("reviewer native resume source lookup failed", map[string]any{"executionId": executionID, "error": err.Error()})
		return false
	}
	if record == nil || record.NativeSessionID == nil || strings.TrimSpace(*record.NativeSessionID) == "" {
		return false
	}
	if record.NativeResumeStatus != nil && *record.NativeResumeStatus == "pending" {
		return true
	}
	record.NativeResumeMode = stringPtr("native_resume")
	record.NativeResumeStatus = stringPtr("pending")
	record.NativeResumeError = stringPtr(strings.TrimSpace(message))
	record.UpdatedAt = r.nowISO()
	if err := r.repos.AgentExecutions.Upsert(ctx, *record); err != nil {
		r.logWarn("reviewer native resume source mark failed", map[string]any{"executionId": executionID, "error": err.Error()})
		return false
	}
	r.logInfo("reviewer native resume retry queued", map[string]any{"executionId": executionID, "loopId": derefString(record.LoopID), "runId": derefString(record.RunID)})
	return true
}

func (r *Runner) markAgentExecutionNativeResumePendingForHeadChange(ctx context.Context, executionID string, input stepInput, signal reviewerHeadChangeSignal) bool {
	if strings.TrimSpace(executionID) == "" || strings.TrimSpace(signal.Reason) == "" || r.repos == nil || r.repos.AgentExecutions == nil {
		return false
	}
	record, err := r.repos.AgentExecutions.GetByID(ctx, executionID)
	if err != nil {
		r.logWarn("reviewer native resume head-change source lookup failed", map[string]any{"executionId": executionID, "error": err.Error()})
		return false
	}
	if record == nil || record.NativeSessionID == nil || strings.TrimSpace(*record.NativeSessionID) == "" {
		return false
	}
	currentVendor := config.AgentVendor(strings.TrimSpace(r.agentRuntime))
	if currentVendor != "" && (!nativeResumeSupportedForReviewer(currentVendor) || record.Vendor != string(currentVendor)) {
		return false
	}
	metadata := parseJSONObject(record.MetadataJSON)
	metadata[reviewerNativeResumeMetadataKey] = map[string]any{
		"reason":     reviewerNativeResumeReasonHeadChange,
		"phase":      "review",
		"repo":       input.Repo,
		"prNumber":   input.PRNumber,
		"oldHeadSha": signal.OldHeadSHA,
		"newHeadSha": signal.NewHeadSHA,
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		r.logWarn("reviewer native resume head-change metadata marshal failed", map[string]any{"executionId": executionID, "error": err.Error()})
		return false
	}
	record.MetadataJSON = stringPtr(string(metadataJSON))
	record.NativeResumeMode = stringPtr("native_resume")
	record.NativeResumeStatus = stringPtr("pending")
	record.NativeResumeError = stringPtr(strings.TrimSpace(signal.Reason))
	record.UpdatedAt = r.nowISO()
	if err := r.repos.AgentExecutions.Upsert(ctx, *record); err != nil {
		r.logWarn("reviewer native resume head-change source mark failed", map[string]any{"executionId": executionID, "error": err.Error()})
		return false
	}
	r.logInfo("reviewer native resume re-review queued", map[string]any{"executionId": executionID, "loopId": derefString(record.LoopID), "runId": derefString(record.RunID), "oldHeadSha": signal.OldHeadSHA, "newHeadSha": signal.NewHeadSHA})
	return true
}

func (r *Runner) pendingNativeResume(ctx context.Context, loopID string) *storage.AgentExecutionRecord {
	if strings.TrimSpace(loopID) == "" || r.repos == nil || r.repos.AgentExecutions == nil {
		return nil
	}
	latest, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loopID)
	if err != nil {
		r.logWarn("reviewer native resume pending lookup failed", map[string]any{"loopId": loopID, "error": err.Error()})
		return nil
	}
	if !r.isResumableNativeSession(latest) {
		return nil
	}
	return latest
}

func (r *Runner) hasPendingNativeResume(ctx context.Context, loopID string) bool {
	return r.pendingNativeResume(ctx, loopID) != nil
}

func (r *Runner) hasPendingHeadChangeNativeResume(ctx context.Context, loopID string) bool {
	if !r.nativeResume.OnHeadChange && !r.nativeResume.ReReviewPromptOnHeadChange {
		return false
	}
	record := r.pendingNativeResume(ctx, loopID)
	if record == nil {
		return false
	}
	_, ok := reviewerNativeResumeHeadChange(record)
	return ok
}

func (r *Runner) nativeResumePromptForReview(ctx context.Context, input stepInput, currentHeadSHA string, idempotencyKey string) string {
	record := r.pendingNativeResume(ctx, input.Loop.ID)
	if record == nil {
		return ""
	}
	if r.nativeResume.ReReviewPromptOnHeadChange {
		if headChange, ok := reviewerNativeResumeHeadChange(record); ok && headChange.matches(input.Repo, input.PRNumber) {
			return nativeResumeReReviewPrompt(input.Repo, input.PRNumber, headChange.OldHeadSHA, headChange.NewHeadSHA, currentHeadSHA, idempotencyKey)
		}
	}
	return nativeResumeContinuationPrompt("review", input.Repo, input.PRNumber, currentHeadSHA, idempotencyKey)
}

func (r *Runner) shouldSkipTransientRetryDelayForNativeResume(ctx context.Context, loopID string, err error) bool {
	return isTransientModelProviderOverloadFailure(err) && r.hasPendingNativeResume(ctx, loopID)
}

func isTransientModelProviderOverloadFailure(err error) bool {
	if err == nil || githubinfra.IsTransientError(err) {
		return false
	}
	var loopErr *loopError
	if errors.As(err, &loopErr) {
		return loopErr.kind == FailureRetryableTransient && isTransientModelProviderOverloadMessage(loopErr.message)
	}
	return isTransientModelProviderOverloadMessage(err.Error())
}

func isTransientModelProviderOverloadMessage(message string) bool {
	message = strings.ToLower(message)
	for _, fragment := range []string{
		"server_is_overloaded",
		"service_unavailable_error",
		"overloaded_error",
		"server is overloaded",
		"overloaded",
		"http 529",
		"status code: 529",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func (r *Runner) isResumableNativeSession(latest *storage.AgentExecutionRecord) bool {
	if latest == nil || latest.NativeSessionID == nil || strings.TrimSpace(*latest.NativeSessionID) == "" {
		return false
	}
	if !isRecoverableReviewerNativeResumeSource(latest.Status, latest.NativeResumeStatus) {
		return false
	}
	currentVendor := config.AgentVendor(strings.TrimSpace(r.agentRuntime))
	if currentVendor == "" {
		return true
	}
	return nativeResumeSupportedForReviewer(currentVendor) && latest.Vendor == string(currentVendor)
}

func nativeResumeSupportedForReviewer(vendor config.AgentVendor) bool {
	switch vendor {
	case config.AgentVendorClaudeCode, config.AgentVendorCodex, config.AgentVendorOpenCode, config.AgentVendorCursorCLI:
		return true
	default:
		return false
	}
}

func isRecoverableReviewerNativeResumeSource(status string, resumeStatus *string) bool {
	if resumeStatus == nil || *resumeStatus != "pending" {
		return false
	}
	switch status {
	case "running", "cancelling", "killed", "timeout", "failed", "completed":
		return true
	default:
		return false
	}
}

type reviewerNativeResumeHeadChangeInfo struct {
	Repo       string
	PRNumber   int64
	OldHeadSHA string
	NewHeadSHA string
}

func (h reviewerNativeResumeHeadChangeInfo) matches(repo string, prNumber int64) bool {
	return strings.TrimSpace(h.Repo) == strings.TrimSpace(repo) && h.PRNumber == prNumber
}

func reviewerNativeResumeHeadChange(record *storage.AgentExecutionRecord) (reviewerNativeResumeHeadChangeInfo, bool) {
	if record == nil {
		return reviewerNativeResumeHeadChangeInfo{}, false
	}
	metadata := parseJSONObject(record.MetadataJSON)
	raw, _ := metadata[reviewerNativeResumeMetadataKey].(map[string]any)
	if raw == nil {
		return reviewerNativeResumeHeadChangeInfo{}, false
	}
	reason, _ := stringFromAny(raw["reason"])
	if reason != reviewerNativeResumeReasonHeadChange {
		return reviewerNativeResumeHeadChangeInfo{}, false
	}
	repo, _ := stringFromAny(raw["repo"])
	oldHeadSHA, _ := stringFromAny(raw["oldHeadSha"])
	newHeadSHA, _ := stringFromAny(raw["newHeadSha"])
	prNumber := int64(intFromAny(raw["prNumber"]))
	if repo == "" || prNumber == 0 || oldHeadSHA == "" || newHeadSHA == "" {
		return reviewerNativeResumeHeadChangeInfo{}, false
	}
	return reviewerNativeResumeHeadChangeInfo{Repo: repo, PRNumber: prNumber, OldHeadSHA: oldHeadSHA, NewHeadSHA: newHeadSHA}, true
}

func nativeResumeContinuationPrompt(phase string, repo string, prNumber int64, headSHA string, idempotencyKey string) string {
	return fmt.Sprintf(`Continue the existing Looper reviewer %s task in this resumed native session.

Do not restart from scratch or ask for more context. Reuse the prior session context and continue from the transient provider interruption.

Before any GitHub side effect, re-check the current PR/head/idempotency guards from the existing instructions:
- PR: %s#%d
- expected head SHA: %s
- idempotency key: %s

If the review or thread-resolution result was already posted, report the existing completion marker instead of posting a duplicate.`, strings.TrimSpace(phase), repo, prNumber, headSHA, idempotencyKey)
}

func nativeResumeReReviewPrompt(repo string, prNumber int64, oldHeadSHA string, interruptedHeadSHA string, currentHeadSHA string, idempotencyKey string) string {
	return fmt.Sprintf(`Continue the existing Looper reviewer review task in this resumed native session, but treat it as a PR update re-review.

The pull request changed while the prior review was running:
- PR: %s#%d
- previous reviewed head SHA: %s
- head SHA observed at interruption: %s
- current expected head SHA for this run: %s
- idempotency key for this run: %s

Use the prior session context only as background. Re-review the current expected head before publishing anything. Discard findings, assumptions, anchors, or conclusions that only apply to the previous head. Keep only findings that are still concrete, actionable, and valid against the current head.

Before any GitHub side effect, re-check that the PR is open, the current head SHA still matches the current expected head SHA, and the current-user review-request/idempotency guards from the existing instructions still pass.

If a matching review for the current expected head and idempotency key was already posted, report the existing completion marker instead of posting a duplicate.`, repo, prNumber, oldHeadSHA, interruptedHeadSHA, currentHeadSHA, idempotencyKey)
}

func transientProviderMessageFromAgentResult(result AgentResult) string {
	for _, candidate := range []string{result.Summary, result.Stderr} {
		candidate = strings.TrimSpace(candidate)
		if isTransientModelProviderMessage(candidate) {
			return candidate
		}
	}
	return ""
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
	if failedStep != stepPublish && failedStep != stepReview && failedStep != stepThreadResolution {
		return false
	}
	return strings.Contains(failureSummary, "PR head changed before publish") || strings.Contains(failureSummary, "PR head changed while reviewer was running") || strings.Contains(failureSummary, "review request removed before publish") || strings.Contains(failureSummary, "PR changed during thread reconciliation")
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
	if !requireReviewRequestForLoop(input.Loop, r.discoveryPolicyForProject(input.Project.ID).RequireReviewRequest, checkpoint.Snapshot.HeadSHA) {
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

func hasReviewByAuthorForHead(reviews []map[string]any, login string, headSHA string) bool {
	return hasReviewByAuthorForHeadMatchingState(reviews, login, headSHA, isSubmittedReviewState)
}

func hasApprovedReviewByAuthorForHead(reviews []map[string]any, login string, headSHA string) bool {
	return hasReviewByAuthorForHeadMatchingState(reviews, login, headSHA, func(state string) bool {
		return strings.EqualFold(strings.TrimSpace(state), "APPROVED")
	})
}

func hasReviewByAuthorForHeadMatchingState(reviews []map[string]any, login string, headSHA string, stateMatches func(string) bool) bool {
	login = normalizeLogin(login)
	headSHA = strings.TrimSpace(headSHA)
	if login == "" || headSHA == "" || stateMatches == nil {
		return false
	}
	for _, review := range reviews {
		author, ok := review["author"].(map[string]any)
		if !ok {
			continue
		}
		if authorLogin, ok := stringFromAny(author["login"]); !ok || normalizeLogin(authorLogin) != login {
			continue
		}
		state, _ := stringFromAny(review["state"])
		if !stateMatches(state) {
			continue
		}
		commit, ok := review["commit"].(map[string]any)
		if !ok {
			continue
		}
		if oid, ok := stringFromAny(commit["oid"]); ok && strings.TrimSpace(oid) == headSHA {
			return true
		}
	}
	return false
}

func (r *Runner) currentLoginForLoop(ctx context.Context, loop storage.LoopRecord) (string, error) {
	project, err := r.repos.Projects.GetByID(ctx, loop.ProjectID)
	if err != nil {
		return "", err
	}
	cwd := ""
	if project != nil {
		cwd = project.RepoPath
	}
	login, err := r.github.GetCurrentUserLogin(ctx, cwd)
	if err != nil {
		return "", err
	}
	return normalizeLogin(login), nil
}

func isSubmittedReviewState(state string) bool {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "APPROVED", "CHANGES_REQUESTED", "COMMENTED":
		return true
	default:
		return false
	}
}

func rediscoverySignalFromAgentResult(result AgentResult, allowReviewRequestSignal bool) (string, bool) {
	for _, candidate := range []string{result.Summary, result.Stdout, result.Stderr} {
		for _, line := range strings.Split(candidate, "\n") {
			line = strings.TrimSpace(line)
			searchable := stripMarkdownCodeSpans(line)
			switch {
			case strings.Contains(searchable, "PR head changed before publish"):
				return extractRediscoverySignal(searchable, "PR head changed before publish"), true
			case allowReviewRequestSignal && strings.Contains(searchable, "review request removed before publish"):
				return extractRediscoverySignal(searchable, "review request removed before publish"), true
			}
		}
	}
	return "", false
}

func stripMarkdownCodeSpans(line string) string {
	for {
		start := strings.Index(line, "`")
		if start < 0 {
			return line
		}
		end := strings.Index(line[start+1:], "`")
		if end < 0 {
			return line
		}
		end += start + 1
		line = line[:start] + line[end+1:]
	}
}

func extractRediscoverySignal(line, signal string) string {
	index := strings.Index(line, signal)
	if index < 0 {
		return strings.TrimSpace(line)
	}
	return strings.TrimSpace(line[index:])
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

func reviewerDiscoverySuppressedByLastSkip(meta map[string]any, pr PullRequestSummary, currentLogin string, policy DiscoveryPolicy) bool {
	raw, _ := meta["lastFilterSkip"].(map[string]any)
	if raw == nil {
		return false
	}
	kind, _ := stringFromAny(raw["kind"])
	if !isDiscoverySuppressingSkipKind(kind) {
		return false
	}
	headSHA, _ := stringFromAny(raw["headSha"])
	if headSHA == "" || pr.HeadSHA == "" || headSHA != pr.HeadSHA {
		return false
	}
	if kind == "already_reviewed_by_current_user" {
		reviewerLogin, _ := stringFromAny(raw["reviewerLogin"])
		if normalizeLogin(reviewerLogin) == "" || normalizeLogin(currentLogin) == "" || normalizeLogin(reviewerLogin) != normalizeLogin(currentLogin) {
			return false
		}
	}
	switch kind {
	case "conflicted":
		if !pr.HasConflicts {
			return false
		}
	case "self_authored":
		if policy.EnableSelfReview {
			return false
		}
		authorLogin, _ := stringFromAny(raw["authorLogin"])
		reviewerLogin, _ := stringFromAny(raw["reviewerLogin"])
		if normalizeLogin(authorLogin) == "" || normalizeLogin(pr.Author) == "" || normalizeLogin(authorLogin) != normalizeLogin(pr.Author) {
			return false
		}
		if normalizeLogin(reviewerLogin) == "" || normalizeLogin(currentLogin) == "" || normalizeLogin(reviewerLogin) != normalizeLogin(currentLogin) {
			return false
		}
	case "ready_label":
		if label, ok := stringFromAny(raw["requiredLabel"]); ok && label != "" && !specpr.HasLabel(pr.Labels, label) {
			return false
		}
	case "approved":
		if decision, ok := stringFromAny(raw["reviewDecision"]); ok && decision != "" && !strings.EqualFold(strings.TrimSpace(pr.ReviewDecision), decision) {
			return false
		}
	case "draft":
		if draft, ok := raw["isDraft"].(bool); ok && draft != pr.IsDraft {
			return false
		}
	}
	return true
}

func reviewerLastSkipNeedsCurrentLogin(meta map[string]any, pr PullRequestSummary) bool {
	raw, _ := meta["lastFilterSkip"].(map[string]any)
	if raw == nil {
		return false
	}
	kind, _ := stringFromAny(raw["kind"])
	if kind != "already_reviewed_by_current_user" && kind != "self_authored" {
		return false
	}
	headSHA, _ := stringFromAny(raw["headSha"])
	return headSHA != "" && pr.HeadSHA != "" && headSHA == pr.HeadSHA
}

func isDiscoverySuppressingSkipKind(kind string) bool {
	switch kind {
	case "conflicted", "already_reviewed_by_current_user", "already_published_head", "draft", "approved", "ready_label", "self_authored":
		return true
	default:
		return false
	}
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

func (r *Runner) nextReviewAvailableAt(meta map[string]any) time.Time {
	availableAt := r.now()
	if _, reviewed := stringFromAny(meta["lastPublishedHeadSha"]); reviewed && r.loopConfig.QuietPeriodSeconds > 0 {
		availableAt = availableAt.Add(time.Duration(r.loopConfig.QuietPeriodSeconds) * time.Second)
	}
	if r.loopConfig.MinPublishIntervalSeconds > 0 {
		if lastPublishedAt, ok := stringFromAny(meta["lastPublishedAt"]); ok {
			if parsed, err := time.Parse(time.RFC3339Nano, lastPublishedAt); err == nil {
				minimum := parsed.Add(time.Duration(r.loopConfig.MinPublishIntervalSeconds) * time.Second)
				if minimum.After(availableAt) {
					availableAt = minimum
				}
			}
		}
	}
	return availableAt
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
	loopMeta["minPublishIntervalSeconds"] = r.loopConfig.MinPublishIntervalSeconds
	removeDeprecatedReviewerLoopBudgetMetadata(loopMeta)
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

func deprecatedReviewerLoopBudgetTerminationReason(loop storage.LoopRecord) string {
	meta := parseJSONObject(loop.MetadataJSON)
	loopMeta := reviewerLoopMetadata(meta)
	reason, _ := stringFromAny(loopMeta["terminationReason"])
	if isDeprecatedReviewerLoopBudgetReason(reason) {
		return reason
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
	removeDeprecatedReviewerLoopBudgetMetadata(loopMeta)
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
	removeDeprecatedReviewerLoopBudgetMetadata(loopMeta)
	if checkpoint.SkipReason != "" {
		delete(meta, "lastFilterSkip")
		if skip := filterSkipMetadata(checkpoint, r.nowISO()); skip != nil {
			meta["lastFilterSkip"] = skip
		}
	} else {
		delete(meta, "lastFilterSkip")
	}
	meta["loop"] = loopMeta
	encoded, err := json.Marshal(meta)
	return string(encoded), err
}

func filterSkipMetadata(checkpoint reviewerCheckpoint, recordedAt string) map[string]any {
	if checkpoint.Detail == nil || !isDiscoverySuppressingSkipKind(checkpoint.SkipKind) {
		return nil
	}
	metadata := map[string]any{
		"kind":       checkpoint.SkipKind,
		"reason":     checkpoint.SkipReason,
		"recordedAt": recordedAt,
	}
	if checkpoint.Detail.HeadSHA != "" {
		metadata["headSha"] = checkpoint.Detail.HeadSHA
	}
	if checkpoint.SkipKind == "draft" && checkpoint.Detail.IsDraft {
		metadata["isDraft"] = true
	}
	if checkpoint.SkipKind == "approved" && checkpoint.Detail.ReviewDecision != "" {
		metadata["reviewDecision"] = strings.TrimSpace(checkpoint.Detail.ReviewDecision)
	}
	if checkpoint.SkipKind == "conflicted" && checkpoint.Detail.HasConflicts {
		metadata["hasConflicts"] = true
	}
	if checkpoint.SkipKind == "ready_label" {
		metadata["requiredLabel"] = specpr.ReadyLabel
	}
	if checkpoint.SkipKind == "already_reviewed_by_current_user" && checkpoint.SkipReviewerLogin != "" {
		metadata["reviewerLogin"] = normalizeLogin(checkpoint.SkipReviewerLogin)
	}
	if checkpoint.SkipKind == "self_authored" {
		if author := normalizeLogin(checkpoint.Detail.Author); author != "" {
			metadata["authorLogin"] = author
		}
		if checkpoint.SkipReviewerLogin != "" {
			metadata["reviewerLogin"] = normalizeLogin(checkpoint.SkipReviewerLogin)
		}
	}
	return metadata
}

func loopSuccessOutputFingerprint(checkpoint reviewerCheckpoint, summary string) string {
	if checkpoint.PendingReview != nil {
		if checkpoint.PendingReview.CleanNoop {
			return ""
		}
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

func removeDeprecatedReviewerLoopBudgetMetadata(loopMeta map[string]any) {
	for _, key := range deprecatedReviewerLoopBudgetMetadataKeys {
		delete(loopMeta, key)
	}
	if reason, _ := stringFromAny(loopMeta["terminationReason"]); isDeprecatedReviewerLoopBudgetReason(reason) {
		delete(loopMeta, "terminationReason")
		if status, _ := stringFromAny(loopMeta["status"]); status == "failed" || status == "terminated" {
			loopMeta["status"] = "active"
		}
	}
}

var deprecatedReviewerLoopBudgetMetadataKeys = []string{
	"maxIterationsPerPR",
	"maxIterationsPerHead",
	"maxWallClockSeconds",
	"maxConsecutiveFailures",
	"maxAgentExecutionsPerPR",
}

func isDeprecatedReviewerLoopBudgetReason(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "max_iterations_per_pr", "max_iterations_per_head", "max_wall_clock", "max_consecutive_failures", "max_agent_executions_per_pr":
		return true
	default:
		return false
	}
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
	return PullRequestSummary{Number: detail.Number, Title: detail.Title, State: detail.State, IsDraft: detail.IsDraft, ReviewDecision: detail.ReviewDecision, Labels: cloneStrings(detail.Labels), HeadSHA: detail.HeadSHA, BaseSHA: detail.BaseSHA, HasConflicts: detail.HasConflicts, Author: detail.Author, ReviewRequests: cloneStrings(detail.ReviewRequests), ReviewRequestUsers: cloneGitHubUsers(detail.ReviewRequestUsers), Reviews: cloneObjectSlice(detail.Reviews)}
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
	prompt, _ := buildReviewPromptWithInstructions("", cfg, repo, prNumber, checkpoint, runID, idempotencyKey, reviewEvents, manual, true, "", scope, disclosureCfg, agentRuntime, agentModel, looperCLIPath, false)
	return prompt
}

func buildReviewerMinimalPRSeed(repo string, prNumber int64, checkpoint reviewerCheckpoint, scope config.ReviewerScope) string {
	seed := map[string]any{
		"repo":           repo,
		"pr_number":      prNumber,
		"url":            seededPullRequestURL(repo, prNumber),
		"head_sha":       snapshotHeadSHA(checkpoint),
		"expected_state": "OPEN",
		"expected_draft": false,
		"task_intent":    "review_pull_request",
		"scope": map[string]any{
			"review_scope": scope,
		},
	}
	if checkpoint.Detail != nil {
		seed["base_ref"] = checkpoint.Detail.BaseRefName
		seed["head_ref"] = checkpoint.Detail.HeadRefName
		seed["expected_state"] = firstNonEmpty(strings.ToUpper(strings.TrimSpace(checkpoint.Detail.State)), "OPEN")
		seed["expected_draft"] = checkpoint.Detail.IsDraft
	} else {
		seed["base_ref"] = ""
		seed["head_ref"] = ""
	}
	encoded, _ := json.MarshalIndent(seed, "", "  ")
	return "Minimal PR seed (authoritative handoff fields; fetch all mutable PR details yourself):\n" + string(encoded)
}

func seededPullRequestURL(repo string, prNumber int64) string {
	host, path := seededPullRequestRepoParts(repo)
	return fmt.Sprintf("https://%s/%s/pull/%d", host, path, prNumber)
}

func seededPullRequestRepoParts(repo string) (host string, path string) {
	const defaultHost = "github.com"
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return defaultHost, ""
	}
	if parsed, err := url.Parse(repo); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname(), strings.Trim(strings.TrimSpace(parsed.Path), "/")
	}
	if at := strings.Index(repo, "@"); at >= 0 {
		repo = repo[at+1:]
	}
	if colon := strings.Index(repo, ":"); colon > 0 {
		host = strings.TrimSpace(repo[:colon])
		path = strings.Trim(strings.TrimSpace(repo[colon+1:]), "/")
		if host != "" && path != "" {
			return host, path
		}
	}
	parts := strings.Split(repo, "/")
	if len(parts) >= 3 && strings.TrimSpace(parts[0]) != "" {
		return strings.TrimSpace(parts[0]), strings.Join(parts[1:], "/")
	}
	return defaultHost, strings.Trim(repo, "/")
}

func reviewerAgentSideGitHubFetchContract() string {
	return strings.Join([]string{
		"Agent-side GitHub fetch contract: use the minimal PR seed above as the stable handoff. Do not assume PR title, body, full diff, full comment dumps, reviews, or checks from this prompt are complete or fresh.",
		"Local checkout contract: the current working directory is Looper's prepared reviewer worktree for this PR and is the canonical local checkout for verification. Reuse this worktree for git fetch, git checkout, diff inspection, and any local validation. Do not run `gh repo clone`, `git clone`, or create any additional checkout for this PR's base or head repository unless the provided worktree is missing or unusable.",
		"Before acting and again before final conclusions or publishing, run `gh pr view <pr-url> -R <repo> --json number,title,body,state,isDraft,baseRefName,headRefName,headRefOid,url,labels` using the seeded PR URL or number plus repository, and validate `headRefOid` equals the seeded `head_sha`, `baseRefName` equals the seeded `base_ref` when present, and state/draft status match the seed. Fail fast on drift.",
		"Fetch scoped data on demand with `gh pr diff <pr-url> -R <repo> --name-only` before selecting files. For relevant file diffs, use a supported workflow such as fetching the full patch with `gh pr diff <pr-url> -R <repo> --patch` and filtering locally, or fetching refs and running `git diff <base>...<head> -- <path>`. Run `gh pr checks <pr-url> -R <repo>` only when CI status matters.",
		"When review feedback context matters, do not rely only on `gh pr view --comments`; collect all review feedback with pagination: `gh api repos/{owner}/{repo}/pulls/{number}/comments --paginate`, `gh api repos/{owner}/{repo}/pulls/{number}/reviews --paginate`, and `gh api repos/{owner}/{repo}/issues/{number}/comments --paginate`.",
		"If `gh` fails for authentication, network, rate-limit, or PR drift reasons, stop and return a structured error with `type` set to one of `auth`, `network`, `rate_limit`, or `pr_drift`, plus a short `message` and any observed PR metadata. Do not proceed on stale PR data.",
	}, "\n")
}

func buildReviewPromptWithInstructions(projectID string, instructionConfig config.Config, repo string, prNumber int64, checkpoint reviewerCheckpoint, runID string, idempotencyKey string, reviewEvents config.ReviewerReviewEventsConfig, manual bool, requireReviewRequest bool, reviewRequestBypassReason string, scope config.ReviewerScope, disclosureCfg config.DisclosureConfig, agentRuntime string, agentModel string, looperCLIPath string, autoMergeEnabled bool) (string, config.CustomInstructionBlock) {
	looperCLIPath = normalizeLooperCLIPath(looperCLIPath)
	looperCLICommand := shellQuote(looperCLIPath)
	phase := resolvePullRequestPhase(detailLabels(checkpoint.Detail))
	phaseInstruction := "This is an implementation review. Focus on code correctness, safety, tests, and maintainability."
	if phase == "spec" {
		phaseInstruction = "This is a spec review. Focus on scope, correctness, feasibility, risks, and validation. Do not review implementation details beyond whether the spec is actionable."
	}
	publishInstruction := "For actionable findings, you must publish the GitHub review yourself by calling looper's enforced review-submit wrapper from the shell. For no-actionable-finding results, follow the clean-result publishing instructions for this run. Do not return review JSON for looper to parse; looper will not parse review content or post GitHub comments for you after the agent exits."
	if looperCLIPath == "" {
		publishInstruction = "A trusted Looper CLI review-submit wrapper is unavailable for this run, so fail closed: do not publish any GitHub review, do not add or remove any GitHub reaction, and exit non-zero with the exact message `trusted looper review submit wrapper unavailable`."
	}
	outcomeInstruction := "Use outcome=clean only when there are no blocking or non-blocking findings, outcome=non_blocking for actionable feedback that should not block merge, and outcome=blocking for findings that should block merge. Legacy outcome=actionable may be treated as comment-only compatibility, but prefer non_blocking or blocking. For no-actionable-finding results, follow the clean-result instructions for this run and finish with a completion summary that starts with `No actionable findings`."
	cleanResultCompletionInstruction := "Prefer 3 deeply specific comments over 10 shallow comments. Group related findings by file, subsystem, function, or rule in a single review round instead of splitting adjacent concerns across multiple small reviews. If there is no concrete actionable feedback, follow the clean-result instructions for this run and finish successfully with a summary beginning `No actionable findings`. Do not invent feedback."
	if looperCLIPath == "" {
		outcomeInstruction = "Use outcome=clean only when there are no blocking or non-blocking findings, outcome=non_blocking for actionable feedback that should not block merge, and outcome=blocking for findings that should block merge. Legacy outcome=actionable may be treated as comment-only compatibility, but prefer non_blocking or blocking. For no-actionable-finding results, do not report clean success because the trusted review-submit wrapper is unavailable; exit non-zero with the exact message `trusted looper review submit wrapper unavailable`."
		cleanResultCompletionInstruction = "Prefer 3 deeply specific comments over 10 shallow comments. Group related findings by file, subsystem, function, or rule in a single review round instead of splitting adjacent concerns across multiple small reviews. If there is no concrete actionable feedback, do not finish successfully or add a clean signal because the trusted wrapper is unavailable; exit non-zero with the exact message `trusted looper review submit wrapper unavailable`. Do not invent feedback."
	}
	parts := []string{fmt.Sprintf("Review pull request %s#%d.", repo, prNumber), buildReviewerMinimalPRSeed(repo, prNumber, checkpoint, scope), reviewerAgentSideGitHubFetchContract(), "Phase: " + phase, phaseInstruction, reviewerScopeInstruction(scope), publishInstruction, fmt.Sprintf("Review idempotency marker prefix: <!-- looper:review id=%s head=%s outcome=clean|non_blocking|blocking -->", idempotencyKey, snapshotHeadSHA(checkpoint)), outcomeInstruction, "Run ID for logging only, not for idempotency: " + runID}
	if checkpoint.Detail != nil && len(checkpoint.Detail.Labels) > 0 {
		parts = append(parts, "Current labels: "+strings.Join(checkpoint.Detail.Labels, ", "))
	}
	if checkpoint.Snapshot != nil {
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
	}
	instructionBlock := config.BuildCustomInstructionBlock(instructionConfig, projectID, "reviewer")
	if instructionBlock.Text != "" {
		parts = append(parts, instructionBlock.Text)
	}
	cleanReviewAuthorMention := cleanReviewAuthorTarget(checkpoint)
	cleanNoopInstruction := "For no-actionable-finding results when the clean review policy is COMMENT, do not submit a clean COMMENT or APPROVE review; finish successfully with the `No actionable findings` summary only. After Looper validates that no clean review marker was required for this run, the runner will reconcile the clean-signal +1 reaction."
	cleanInstruction := cleanNoopInstruction
	if autoMergeEnabled && phase != "spec" {
		cleanInstruction = "For no-actionable-finding results on this implementation PR, do not submit a clean GitHub review yourself. Finish successfully with a summary beginning `No actionable findings`; the runner will verify the linked issue's acceptance criteria and decide whether to publish APPROVE, COMMENT, or no clean review."
	}
	if looperCLIPath == "" {
		cleanInstruction = "For no-actionable-finding results, do not use the clean COMMENT no-op path and do not finish successfully because the trusted review-submit wrapper is unavailable; exit non-zero with the exact message `trusted looper review submit wrapper unavailable`."
	}
	blockingInstruction := "Submit blocking and non-blocking finding reviews as COMMENT."
	specLabelInstruction := "Do not transition spec-review labels yourself. Looper may transition spec-review labels only after it validates a matching APPROVED clean review marker for the current head."
	policyFlags := fmt.Sprintf("--clean-review-event %s --blocking-review-event %s", reviewEvents.Clean, reviewEvents.Blocking)
	actionableReviewSubmitCommand := fmt.Sprintf("`%s review submit %s#%d --event COMMENT --commit-id %s %s`", looperCLICommand, repo, prNumber, snapshotHeadSHA(checkpoint), policyFlags)
	if reviewEvents.Clean == config.ReviewerReviewEventApprove && looperCLIPath != "" && !(autoMergeEnabled && phase != "spec") {
		cleanInstruction = fmt.Sprintf("For no-actionable-finding results when the clean review policy is APPROVE, submit exactly one APPROVE review through the trusted Looper CLI wrapper with `outcome=clean`, no inline `comments`, and no extra PR conversation comment: `%s review submit %s#%d --event APPROVE --commit-id %s %s`. The APPROVE review body must not be empty or disclosure-only: the visible body must start with `%s`, briefly summarize what changed or what you verified, and include a warm, friendly, encouraging acknowledgement of the author's work. Then include exactly one clean review marker and any required Looper disclosure. Do not use a bare LGTM or marker/disclosure-only body; the wrapper rejects clean APPROVE reviews that do not start with an @mention or lack enough human-written summary text. If the authenticated GitHub user authored the pull request, the wrapper will downgrade the submission to a COMMENT because GitHub rejects self-approval. After Looper validates the matching APPROVED clean review marker, or the self-authored clean COMMENT fallback, the runner will reconcile the clean-signal +1 reaction and any eligible spec label transition.", looperCLICommand, repo, prNumber, snapshotHeadSHA(checkpoint), policyFlags, cleanReviewAuthorMention)
		specLabelInstruction = "Do not transition spec-review labels yourself. Looper may transition spec-review labels only after a new matching APPROVED clean review is validated for this head, or when idempotency finds an existing matching APPROVED clean review for this head."
	}
	if reviewEvents.Blocking == config.ReviewerReviewEventRequestChanges {
		blockingInstruction = "If the review has blocking findings, submit REQUEST_CHANGES with outcome=blocking. If findings are non-blocking, submit COMMENT with outcome=non_blocking. Never submit REQUEST_CHANGES for non-blocking findings."
		actionableReviewSubmitCommand = fmt.Sprintf("`%s review submit %s#%d --event COMMENT --commit-id %s %s` for non-blocking findings or `%s review submit %s#%d --event REQUEST_CHANGES --commit-id %s %s` for blocking findings", looperCLICommand, repo, prNumber, snapshotHeadSHA(checkpoint), policyFlags, looperCLICommand, repo, prNumber, snapshotHeadSHA(checkpoint), policyFlags)
	}
	existingMarkerEventInstruction := "Idempotency outcome matching is strict: only treat an existing `outcome=clean` marker as satisfied when it is on a COMMENTED review if clean policy is COMMENT, or on an APPROVED review if clean policy is APPROVE. A COMMENTED `outcome=clean` marker is also valid when the authenticated GitHub user authored the pull request and the trusted wrapper downgraded self-approval. Only treat an existing `outcome=blocking` marker as satisfied when it is on a CHANGES_REQUESTED review if blocking policy is REQUEST_CHANGES. Treat `outcome=non_blocking` or legacy `outcome=actionable` markers as satisfied only when they are on a COMMENTED review. Ignore matching markers on disallowed review states and publish the correct review for this run instead."
	reviewRequestInstruction := "Before posting, confirm the current GitHub user is still requested for review. If not requested, do not post a review; exit non-zero with the exact message `review request removed before publish`."
	if manual {
		reviewRequestInstruction = "This is a manual reviewer run, so a current-user review request is not required before posting."
	} else if !requireReviewRequest && reviewRequestBypassReason == "follow_up_new_head" {
		reviewRequestInstruction = "This is an enabled reviewer follow-up loop for a PR head that differs from the last published review head, so a fresh current-user review request is not required before posting for this follow-up pass."
	} else if !requireReviewRequest {
		reviewRequestInstruction = "This reviewer configuration does not require a current-user review request before posting."
	}
	githubOperationContract := fmt.Sprintf("GitHub operation contract: when there are actionable findings, submit exactly one PR review for this run through the trusted Looper CLI at %s, with the review JSON on stdin. The wrapper validates inline anchors against the live PR diff before it calls GitHub; do not use PATH-based `looper`, repository-local `go run ./cmd/looper`, `gh api repos/%s/pulls/%d/reviews`, or `gh pr review` directly for the review submission.", actionableReviewSubmitCommand, repo, prNumber)
	submitPayloadInstruction := fmt.Sprintf("When submitting through `%s review submit`, pass stdin JSON with `body` and optional `comments` entries using GitHub's review comment fields: `path`, `line`, `side` (`RIGHT` for new diff lines, `LEFT` for old diff lines), optional `start_line` and `start_side` for multiline ranges, and `body` for the actionable feedback.", looperCLICommand)
	if looperCLIPath == "" {
		githubOperationContract = "GitHub operation contract: a trusted Looper CLI path was not detected for this reviewer run, so you cannot safely publish a GitHub review. Do not call PATH-based `looper`, repository-local `go run ./cmd/looper`, `gh api repos/.../pulls/.../reviews`, or `gh pr review` directly; exit non-zero with the exact message `trusted looper review submit wrapper unavailable`."
		submitPayloadInstruction = ""
	}
	parts = append(parts,
		"Idempotency requirement: before posting anything, use `gh api` to list existing PR reviews for this PR. Only treat an existing marker as satisfying this run when the review body contains the exact idempotency id and expected head SHA, and the review state matches the required outcome-specific policy for this run. If such a matching review already exists, do not post another review. Instead, rely on Looper to validate that marker after the agent exits and to reconcile clean-signal reactions/spec label transitions as needed. If the marker exists but the outcome/review-state combination does not satisfy this run, ignore it and publish the correct review for this run instead.",
		existingMarkerEventInstruction,
		githubOperationContract,
		"Review pass contract: complete one full review pass before publishing. Collect PR metadata, changed-file list, live diffs, prior unresolved feedback, and necessary surrounding context; then scan every changed file/range in scope. Do not stop after the first issue. If a blocking issue is visible in the current PR head and review context, include it in this review rather than deferring it to a later pass.",
		"Finding accumulator contract: accumulate candidate findings internally before publishing. For each candidate, track location, severity, evidence, why it matters, and a suggested fix. Before submitting, deduplicate, merge same-root-cause findings, group repeated patterns into systemic comments with representative examples, and prefer fewer deep comments over many shallow ones. If there are more than 15 blocking findings or more than 25 total comments, avoid comment flooding by publishing grouped systemic blockers instead of many repetitive inline comments.",
		"Severity rubric: mark a finding as BLOCKING only when it can realistically cause incorrect behavior, data loss/corruption, security exposure, broken public API/protocol/config/migration/backward compatibility, failing existing or necessary tests, race/deadlock/resource leak, transaction/lifecycle inconsistency, clear production risk, or failure to satisfy the PR's stated goal. Mark actionable but merge-safe improvements as NON_BLOCKING. Mark tiny style, naming, wording, formatting, or subjective preferences as NIT; NITs must not block merge.",
		"Finalization gate before submit: verify that the scoped changed files/ranges were reviewed, all observed blocking findings are included, repeated patterns are consolidated, non-blocking/nit feedback is not escalated, every published finding has concrete evidence and a suggested fix, and the review outcome matches the highest published severity.",
		"Before posting, use `gh` to confirm the PR is still open and the head SHA still matches the expected head SHA. If it changed, do not post a review and exit non-zero with the exact message `PR head changed before publish`.",
		reviewRequestInstruction,
		"Review body style contract: the visible body must be human-authored review prose only. Never post terminal/tool output, ANSI escape sequences, file-read traces, command logs, JSON parsing artifacts, or your internal scratch work as the GitHub review body. If you have actionable findings but do not have concrete actionable prose yet, exit non-zero instead of posting logs. For a clean APPROVE review, write the required author mention, change/verification summary, and warm acknowledgement; never use an LGTM, empty, or disclosure-only clean body as a fallback.",
		"Every review body you post must include exactly one stable idempotency marker with id, head, and outcome fields: `<!-- looper:review id=... head=... outcome=clean|non_blocking|blocking -->`.",
		reviewDisclosureInstruction(disclosureCfg, agentRuntime, agentModel),
		"Before posting, validate every inline review comment's `path`, `line`, `side`, `start_line`, and `start_side` against the live PR diff fetched with `gh pr diff`. Preserve exact anchors that fit the live diff. If an otherwise useful comment is outside the live diff's anchorable locations, safely downgrade it to top-level review body feedback that starts with clear fallback location text instead of submitting an invalid inline anchor.",
		"Do not add or remove the PR main-conversation +1 reaction yourself. After Looper validates the resulting review marker or accepted clean no-op outcome for this run, the runner will reconcile clean-signal reactions automatically.",
		specLabelInstruction,
		cleanInstruction,
		blockingInstruction,
		cleanResultCompletionInstruction,
		"When follow-up findings target the same subsystem or topic as an existing unresolved thread, reply to that thread where possible instead of opening a separate top-level review round.",
		"For complex linting/parsing logic, prefer recommending fixture-matrix tests over isolated one-regression tests. For CSS linting specifically, consider coverage for multiple style blocks, inline styles, comments, at-rules, cascade order, custom properties, var() fallbacks, theme scopes, and px/em/rem unit handling.",
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
		"If the review is clean, do not write or publish a bare LGTM review body. For clean COMMENT policy, avoid adding PR conversation noise for no-actionable-finding outcomes. For clean APPROVE policy, the APPROVE review must include the required author mention, concise summary, and friendly acknowledgement.",
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

func cleanReviewAuthorTarget(checkpoint reviewerCheckpoint) string {
	mention := cleanReviewAuthorMention(cleanReviewAuthorLogin(checkpoint, PullRequestDetail{}))
	if mention == "" {
		return "@<PR-author-login>"
	}
	return mention
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
		return "Review scope: full_pr. Use the full PR context, including title, body, checks, discussion metadata, and the complete diff fetched through `gh` according to the agent-side GitHub fetch contract. You may report actionable issues anywhere in the PR diff when they are supported by the fetched context."
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
		return reviewBodyInstruction + " Every inline review comment you post must also use looper's configured visible inline disclosure style: include the hidden stamp marker `" + disclosure.Marker + "` immediately followed by the visible Markdown footer `" + strings.TrimPrefix(stamper.MarkdownStamp("reviewer"), disclosure.Marker+"\n") + "`. Do not write the footer as plain paragraph text."
	}
	return reviewBodyInstruction + " Every inline review comment you post must include only the hidden looper stamp marker `" + disclosure.Marker + "` as its disclosure. Do not add visible looper disclosure footers to inline review comments."
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

func agentNativeLoopReviewMarker(loopID string, headSHA string) string {
	return fmt.Sprintf("looper:review id_prefix=reviewer:%s: head=%s", loopID, headSHA)
}

func (r *Runner) cleanupReviewerWorktreeIfTerminal(ctx context.Context, project storage.ProjectRecord, checkpoint *reviewerCheckpoint) {
	if r.git == nil || checkpoint == nil || checkpoint.Worktree == nil || checkpoint.Worktree.Path == "" || checkpoint.Worktree.Branch == "" || checkpoint.Worktree.CleanedAt != "" {
		return
	}
	protectedBranches := []string{}
	if baseBranch := strings.TrimSpace(derefString(project.BaseBranch)); baseBranch != "" {
		protectedBranches = append(protectedBranches, baseBranch)
	}
	worktreeRoot, rootErr := reviewerWorktreeRoot(project)
	if rootErr != nil {
		r.logWarn("reviewer worktree cleanup skipped", map[string]any{"projectId": project.ID, "worktreePath": checkpoint.Worktree.Path, "branch": checkpoint.Worktree.Branch, "error": rootErr.Error()})
		return
	}
	if err := r.git.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: project.ID, RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: checkpoint.Worktree.Path, Branch: checkpoint.Worktree.Branch, ProtectedBranches: protectedBranches}); err != nil {
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

func reviewerWorktreeRoot(project storage.ProjectRecord) (string, error) {
	projectMetadata := parseJSONObject(project.MetadataJSON)
	worktreeRoot, _ := stringFromAny(projectMetadata["worktreeRoot"])
	if worktreeRoot != "" {
		return worktreeRoot, nil
	}
	return config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
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

func backoffDelay(base time.Duration, attempts int64, maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		maxDelay = maxRetryDelay
	}
	delay := base
	for i := int64(1); i < attempts; i++ {
		delay *= 2
	}
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func retryDelay(base time.Duration, attempts int64, err error, maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		maxDelay = maxRetryDelay
	}
	if delay, ok := retryAfterDelay(err); ok {
		if delay > maxDelay {
			return maxDelay
		}
		return delay
	}
	return jitterDelay(backoffDelay(base, attempts, maxDelay), maxDelay)
}

func jitterDelay(delay time.Duration, maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		maxDelay = maxRetryDelay
	}
	if delay <= 0 || delay >= maxDelay {
		return delay
	}
	maxJitter := delay / retryJitterDivisor
	if maxJitter <= 0 {
		return delay
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxJitter)+1))
	if err != nil {
		return delay
	}
	delay += time.Duration(n.Int64())
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func retryAfterDelay(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	matches := retryAfterPattern.FindStringSubmatch(err.Error())
	if len(matches) != 2 {
		return 0, false
	}
	seconds, parseErr := time.ParseDuration(matches[1] + "s")
	if parseErr != nil || seconds < 0 {
		return 0, false
	}
	return seconds, true
}

func reviewerStepSupportsTransientExternalRetry(step ReviewerStep) bool {
	switch step {
	case stepDiscover, stepSnapshot, stepThreadResolution, stepReview:
		return true
	default:
		return false
	}
}

func isTransientModelProviderError(err error) bool {
	if err == nil {
		return false
	}
	return isTransientModelProviderMessage(err.Error())
}

func isTransientModelProviderMessage(message string) bool {
	message = strings.ToLower(message)
	for _, fragment := range []string{
		"server_is_overloaded",
		"service_unavailable_error",
		"overloaded_error",
		"server is overloaded",
		"service unavailable",
		"overloaded",
		"unexpected eof",
		"tls handshake timeout",
		"connection reset by peer",
		"connection refused",
		"connection timed out",
		"i/o timeout",
		"temporary failure in name resolution",
		"network is unreachable",
		"http 408",
		"http 429",
		"http 500",
		"http 502",
		"http 503",
		"http 504",
		"http 529",
		"status code: 408",
		"status code: 429",
		"status code: 500",
		"status code: 502",
		"status code: 503",
		"status code: 504",
		"status code: 529",
		"408 request timeout",
		"429 too many requests",
		"500 internal server error",
		"502 bad gateway",
		"503 service unavailable",
		"504 gateway timeout",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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

func cloneGitHubUsers(values []networkpolicy.GitHubUser) []networkpolicy.GitHubUser {
	if values == nil {
		return nil
	}
	return append([]networkpolicy.GitHubUser(nil), values...)
}

func cloneObjectSlice(values []map[string]any) []map[string]any {
	if values == nil {
		return nil
	}
	cloned := make([]map[string]any, 0, len(values))
	for _, value := range values {
		clonedValue := make(map[string]any, len(value))
		for key, inner := range value {
			clonedValue[key] = inner
		}
		cloned = append(cloned, clonedValue)
	}
	return cloned
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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
