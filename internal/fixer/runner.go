package fixer

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/infra/specpr"
	"github.com/nexu-io/looper/internal/lifecycle"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
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

	noopResolveManualIntervention = "resolve-comments left review threads unresolved because fixer produced no new commits to push"
	riskyConflictManualHold       = "risky conflict fixes require manual intervention"

	defaultAgentTimeout = 30 * time.Minute
	defaultClaimTTL     = 5 * time.Minute
	// defaultLegacyMarkerlessRunGrace protects running fixer runs that predate
	// durable start/pre-start checkpoint markers during rollout. It must be much
	// longer than claim TTL because non-agent fixer steps may legitimately run
	// longer than a queue claim window without an active agent execution.
	defaultLegacyMarkerlessRunGrace = 24 * time.Hour
	defaultRetryDelay               = 5 * time.Second
	defaultRetryMax                 = 3
)

var fixerDiscoveryLocks = newFixerDiscoveryLockSet()

type fixerDiscoveryLockSet struct {
	mu    sync.Mutex
	locks map[string]*fixerDiscoveryLockRef
}

type fixerDiscoveryLockRef struct {
	mu   sync.Mutex
	refs int
}

func newFixerDiscoveryLockSet() *fixerDiscoveryLockSet {
	return &fixerDiscoveryLockSet{locks: map[string]*fixerDiscoveryLockRef{}}
}

func (s *fixerDiscoveryLockSet) With(key string, fn func() error) error {
	s.mu.Lock()
	ref := s.locks[key]
	if ref == nil {
		ref = &fixerDiscoveryLockRef{}
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

type FixItem struct {
	Type              string   `json:"type"`
	ID                string   `json:"id,omitempty"`
	ThreadID          string   `json:"threadId,omitempty"`
	ThreadFingerprint string   `json:"threadFingerprint,omitempty"`
	Name              string   `json:"name,omitempty"`
	Summary           string   `json:"summary,omitempty"`
	Files             []string `json:"files,omitempty"`
	Author            string   `json:"author,omitempty"`
	URL               string   `json:"url,omitempty"`
	Path              string   `json:"path,omitempty"`
	Line              int64    `json:"line,omitempty"`
}

type PullRequestSummary struct {
	Number  int64
	State   string
	IsDraft bool
	Labels  []string
	HeadSHA string
	Author  string
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
	IssueComments  []map[string]any
	Checks         []map[string]any
	HasConflicts   bool
	Author         string
}

type ListOpenPullRequestsInput struct {
	Repo   string
	CWD    string
	Limit  int
	Author string
	Label  string
	Labels []string
}

type ViewPullRequestInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type ListReviewThreadsInput struct {
	Repo     string
	PRNumber int64
	CWD      string
	Limit    int
}

type ViewReviewThreadInput struct {
	ThreadID string
	CWD      string
}

type ReviewThread struct {
	ID         string
	IsResolved bool
	Comments   []ReviewThreadComment
}

type ReviewThreadComment struct {
	ID        string
	Body      string
	Author    string
	CreatedAt string
	UpdatedAt string
}

type ResolveReviewThreadInput struct {
	Repo     string
	ThreadID string
	CWD      string
}

type AddReviewThreadReplyInput struct {
	Repo     string
	ThreadID string
	Body     string
	CWD      string
}

// CompareCommitsInput asks the gateway to compare two commits on a remote
// repository (e.g. via the GitHub compare API). Used by the fixer to detect
// whether a previously-pushed fix commit is still reachable from the live PR
// head, which distinguishes safe upstream additions from history-rewriting
// rebases / force-pushes that erase the fix.
type CompareCommitsInput struct {
	Repo string
	Base string
	Head string
	CWD  string
}

// CompareCommitsResult mirrors the GitHub compare API status field.
// Valid values: "identical", "ahead", "behind", "diverged".
type CompareCommitsResult struct {
	Status string
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

type UpdateIssueCommentInput struct {
	Repo      string
	CommentID int64
	Body      string
	CWD       string
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
	GetPullRequestAuthor(context.Context, ViewPullRequestInput) (string, error)
	ViewPullRequest(context.Context, ViewPullRequestInput) (PullRequestDetail, error)
	ListReviewThreads(context.Context, ListReviewThreadsInput) ([]ReviewThread, error)
	ViewReviewThread(context.Context, ViewReviewThreadInput) (ReviewThread, error)
	ResolveReviewThread(context.Context, ResolveReviewThreadInput) error
	AddReviewThreadReply(context.Context, AddReviewThreadReplyInput) error
	CompareCommits(context.Context, CompareCommitsInput) (CompareCommitsResult, error)
	CreateIssueComment(context.Context, IssueCommentInput) (IssueCommentResult, error)
	UpdateIssueComment(context.Context, UpdateIssueCommentInput) error
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
	RepoPath        string
	WorktreeRoot    string
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
	RepoPath     string
	WorktreeRoot string
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
	RepoPath     string
	WorktreeRoot string
	WorktreePath string
	Message      string
}

type CommitResult struct{ CommitSHA string }

type PushInput struct {
	RepoPath              string
	WorktreeRoot          string
	WorktreePath          string
	Branch                string
	Remote                string
	ExpectedRemoteHeadSHA string
	ProtectedBranches     []string
}

type CleanupWorktreeInput struct {
	ProjectID         string
	RepoPath          string
	WorktreeRoot      string
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
	FetchBranch(context.Context, string, string, string) error
	IsAncestor(context.Context, string, string, string) (bool, error)
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
	HeartbeatTimeout time.Duration
	Metadata         map[string]any
	IdempotencyKey   string
}

type AgentResult struct {
	Status                       string
	Summary                      string
	Stdout                       string
	Stderr                       string
	ParseStatus                  string
	Lifecycle                    *lifecycle.State
	TimeoutType                  string
	ConfiguredIdleTimeoutSeconds int64
	ConfiguredMaxRuntimeSeconds  int64
	ElapsedRuntimeSeconds        int64
	LastProgressAt               string
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
	HeadSHA string `json:"headSha,omitempty"`
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
	AgentIdleTimeout        time.Duration
	ClaimTTL                time.Duration
	ValidationCommands      []string
	ValidationRunner        ValidationRunner
	AllowAutoCommit         bool
	AllowAutoPush           bool
	AllowRiskyFixes         bool
	FixAllPullRequests      bool
	DiscoveryPolicy         DiscoveryPolicy
	Disclosure              *config.DisclosureConfig
	AgentRuntime            string
	CustomInstructions      *config.Config
	AgentModel              *string
	Sleep                   func(time.Duration)
	RetryBaseDelay          time.Duration
	RetryMaxAttempts        int64
	OnAgentExecutionStarted AgentExecutionStartedFunc
	OnQueueItemEnqueued     func()
}

type DiscoveryPolicy struct {
	AutoDiscovery bool
	IncludeDrafts bool
	AuthorFilter  config.FixerAuthorFilter
	Labels        []string
	LabelMode     config.LabelMode
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
	validationCommands      []string
	validationRunner        ValidationRunner
	allowAutoCommit         bool
	allowAutoPush           bool
	allowRiskyFixes         bool
	fixAllPullRequests      bool
	discoveryPolicy         DiscoveryPolicy
	disclosure              config.DisclosureConfig
	agentRuntime            string
	customInstructions      config.Config
	projectRoleConfig       *config.Config
	agentModel              string
	sleep                   func(time.Duration)
	retryBaseDelay          time.Duration
	retryMaxAttempts        int64
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
	Snapshot  *githubinfra.DiscoverySnapshot
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

type replyAction string

const (
	replyActionFixed    replyAction = "fixed"
	replyActionDeclined replyAction = "declined"
)

type fixerFollowupReason string

const (
	fixerFollowupReasonMissingEvidence     fixerFollowupReason = "missing_evidence"
	fixerFollowupReasonMissingConfirmation fixerFollowupReason = "missing_confirmation"
	fixerFollowupReasonManualIntervention  fixerFollowupReason = "manual_intervention"
)

var fixerFollowupBackoffSchedule = []time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
	4 * time.Hour,
}

type fixerFollowupState struct {
	Reason                 string   `json:"reason,omitempty"`
	HeadSHA                string   `json:"headSha,omitempty"`
	FixItemsStateHash      string   `json:"fixItemsStateHash,omitempty"`
	UnresolvedThreadIDs    []string `json:"unresolvedThreadIds,omitempty"`
	AttemptsForFingerprint int      `json:"attemptsForFingerprint,omitempty"`
	LastAttemptAt          string   `json:"lastAttemptAt,omitempty"`
	NextEligibleAt         string   `json:"nextEligibleAt,omitempty"`
	Terminal               bool     `json:"terminal,omitempty"`
}

type declinedThreadRecord struct {
	RecordedAt string `json:"recordedAt,omitempty"`
	ThreadID   string `json:"threadId,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type zeroProgressState struct {
	HeadSHA           string `json:"headSha,omitempty"`
	FixItemsHash      string `json:"fixItemsHash,omitempty"`
	FixItemsStateHash string `json:"fixItemsStateHash,omitempty"`
	ConsecutiveCount  int    `json:"consecutiveCount,omitempty"`
	RecordedAt        string `json:"recordedAt,omitempty"`
}

type rediscoveryAction string

const (
	rediscoveryActionEnqueue  rediscoveryAction = "enqueue"
	rediscoveryActionDefer    rediscoveryAction = "defer"
	rediscoveryActionSuppress rediscoveryAction = "suppress"
)

type rediscoveryDecision struct {
	Action         rediscoveryAction
	Reason         string
	NextEligibleAt string
}

type fixerCheckpoint struct {
	ResumePolicy     string                      `json:"resumePolicy,omitempty"`
	RunStartedAt     string                      `json:"runStartedAt,omitempty"`
	RunStartedRunID  string                      `json:"runStartedRunId,omitempty"`
	RunPreStartAt    string                      `json:"runPreStartAt,omitempty"`
	RunPreStartRunID string                      `json:"runPreStartRunId,omitempty"`
	Detail           *checkpointDetail           `json:"detail,omitempty"`
	ClaimedLockKey   string                      `json:"claimedLockKey,omitempty"`
	FixItems         []FixItem                   `json:"fixItems,omitempty"`
	FixItemsHash     string                      `json:"fixItemsHash,omitempty"`
	Worktree         *checkpointWorktree         `json:"worktree,omitempty"`
	Repair           *checkpointRepair           `json:"repair,omitempty"`
	Lifecycle        *lifecycle.State            `json:"gitPrLifecycle,omitempty"`
	ReconcileCommits *checkpointReconcileCommits `json:"reconcileCommits,omitempty"`
	Validation       *ValidationResult           `json:"validation,omitempty"`
	Push             *checkpointPush             `json:"push,omitempty"`
	ResolvedComments *checkpointResolvedComments `json:"resolvedComments,omitempty"`
	SummaryComment   *checkpointSummaryComment   `json:"summaryComment,omitempty"`
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
	IssueComments  []map[string]any `json:"issueComments,omitempty"`
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
	AgentExecutionID             string                  `json:"agentExecutionId,omitempty"`
	Summary                      string                  `json:"summary,omitempty"`
	HeadSHA                      string                  `json:"headSha,omitempty"`
	ParseStatus                  string                  `json:"parseStatus,omitempty"`
	Lifecycle                    *lifecycle.State        `json:"gitPrLifecycle,omitempty"`
	CompletedAt                  string                  `json:"completedAt,omitempty"`
	Status                       string                  `json:"status,omitempty"`
	TimeoutType                  string                  `json:"timeoutType,omitempty"`
	ConfiguredIdleTimeoutSeconds int64                   `json:"configuredIdleTimeoutSeconds,omitempty"`
	ConfiguredMaxRuntimeSeconds  int64                   `json:"configuredMaxRuntimeSeconds,omitempty"`
	ElapsedRuntimeSeconds        int64                   `json:"elapsedRuntimeSeconds,omitempty"`
	LastProgressAt               string                  `json:"lastProgressAt,omitempty"`
	ReplyExplanations            []replyExplanationEntry `json:"replyExplanations,omitempty"`
}

// replyExplanationEntry holds the agent's per-fix-item explanation for the
// auto-reply posted before resolving the review thread. Stored on the repair
// checkpoint so resume/retry reuses the same explanation if it still maps to
// the current fix items snapshot. Fixed decisions must include
// ThreadCommentsObserved so resolve-comments can verify the live thread still
// contains exactly the same target review comment IDs before auto-resolving,
// excluding only prior Looper fixer replies.
type replyExplanationEntry struct {
	FixItemID              string `json:"fixItemId"`
	ThreadID               string `json:"threadId,omitempty"`
	Action                 string `json:"action,omitempty"`
	Explanation            string `json:"explanation"`
	ThreadCommentsObserved string `json:"threadCommentsObserved,omitempty"`
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
	Pushed        bool         `json:"pushed"`
	Branch        string       `json:"branch,omitempty"`
	Remote        string       `json:"remote,omitempty"`
	HeadSHA       string       `json:"headSha,omitempty"`
	PushedAt      string       `json:"pushedAt,omitempty"`
	SkippedReason string       `json:"skippedReason,omitempty"`
	Evidence      *fixEvidence `json:"evidence,omitempty"`
}

type fixEvidence struct {
	Valid              bool                 `json:"valid,omitempty"`
	Source             string               `json:"source,omitempty"`
	HeadSHA            string               `json:"headSha,omitempty"`
	CommitSHAs         []string             `json:"commitShas,omitempty"`
	BaseHeadSHA        string               `json:"baseHeadSha,omitempty"`
	FixItemsHash       string               `json:"fixItemsHash,omitempty"`
	CommentRecords     []fixCommentEvidence `json:"commentRecords,omitempty"`
	ProducedNewCommits bool                 `json:"producedNewCommits,omitempty"`
	PushedAt           string               `json:"pushedAt,omitempty"`
}

type fixCommentEvidence struct {
	FixItemID         string `json:"fixItemId,omitempty"`
	ThreadID          string `json:"threadId,omitempty"`
	ThreadFingerprint string `json:"threadFingerprint,omitempty"`
	CommitSHA         string `json:"commitSha,omitempty"`
	Explanation       string `json:"explanation,omitempty"`
}

type fixEvidenceStoreV2 struct {
	Version int                            `json:"version"`
	Threads map[string][]threadFixEvidence `json:"threads"`
}

type threadFixEvidence struct {
	ThreadID           string   `json:"threadId"`
	ThreadFingerprint  string   `json:"threadFingerprint"`
	EvidenceHeadSHA    string   `json:"evidenceHeadSha"`
	CommitSHA          string   `json:"commitSha,omitempty"`
	CommitSHAs         []string `json:"commitShas,omitempty"`
	ValidationHeadSHA  string   `json:"validationHeadSha,omitempty"`
	ProducedNewCommits bool     `json:"producedNewCommits"`
	FixItemsHash       string   `json:"fixItemsHash,omitempty"`
	Source             string   `json:"source,omitempty"`
	RunID              string   `json:"runId,omitempty"`
	PushedAt           string   `json:"pushedAt,omitempty"`
	Explanation        string   `json:"explanation,omitempty"`
	ReplyState         string   `json:"replyState,omitempty"`
	ResolveState       string   `json:"resolveState,omitempty"`
}

type checkpointResolvedComments struct {
	Items []checkpointResolvedComment `json:"items,omitempty"`
}

type checkpointResolvedComment struct {
	FixItemID  string `json:"fixItemId,omitempty"`
	ThreadID   string `json:"threadId,omitempty"`
	Action     string `json:"action,omitempty"`
	Status     string `json:"status,omitempty"`
	Message    string `json:"message,omitempty"`
	UpdatedAt  string `json:"updatedAt,omitempty"`
	ReplyState string `json:"replyState,omitempty"`
	ReplyError string `json:"replyError,omitempty"`
}

type checkpointSummaryComment struct {
	CommentID    int64  `json:"commentId,omitempty"`
	URL          string `json:"url,omitempty"`
	HeadSHA      string `json:"headSha,omitempty"`
	FixItemsHash string `json:"fixItemsHash,omitempty"`
	State        string `json:"state,omitempty"`
	Error        string `json:"error,omitempty"`
	UpdatedAt    string `json:"updatedAt,omitempty"`
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

func (e *loopError) Error() string { return e.message }

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

func checkpointRepairFromAgentResult(executionID, headSHA string, result AgentResult, nowISO string) *checkpointRepair {
	return &checkpointRepair{AgentExecutionID: executionID, Status: result.Status, Summary: result.Summary, HeadSHA: headSHA, ParseStatus: result.ParseStatus, Lifecycle: result.Lifecycle, CompletedAt: nowISO, TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds, ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt}
}

// maxReplyExplanationLength caps each agent-supplied explanation. Replies are
// posted on review threads where verbose bodies create noise; the cap also
// limits prompt-injection blast radius if a malicious reviewer plants payload.
const maxReplyExplanationLength = 500

const (
	agentMissingThreadDecisionExplanation = "Agent did not provide a decision for this thread"
	agentInvalidThreadDecisionExplanation = "Agent provided an unrecognized decision for this thread"
	agentDeclinedThreadWithoutReason      = "Agent declined this thread without a substantive reason"
	maxDeclinedThreadRecords              = 200
	zeroProgressPauseReason               = "agent_zero_progress"
)

// parseReplyExplanations extracts the optional review_thread_replies array from
// the final __LOOPER_RESULT__ JSON line. Failure to parse is not an error: the
// runner simply treats the affected threads as lacking agent confirmation for
// auto-reply/resolve. Entries are filtered against the current fixItems snapshot
// (keyed by fixItemId, with a defensive threadId cross-check), deduplicated
// keeping the first valid occurrence, and truncated. Disclosure markers,
// @mentions, and HTML tags are stripped so the adapter remains the only path
// that stamps and templates the reply.
func parseReplyExplanations(stdout, stderr string, fixItems []FixItem) []replyExplanationEntry {
	combined := stdout + "\n" + stderr
	if strings.TrimSpace(combined) == "" {
		return nil
	}
	itemsByID := make(map[string]FixItem, len(fixItems))
	for _, item := range fixItems {
		if item.Type != "comment" {
			continue
		}
		if item.ID == "" {
			continue
		}
		itemsByID[item.ID] = item
	}
	if len(itemsByID) == 0 {
		return nil
	}
	payload := extractCompletionMarkerPayload(combined)
	if payload == "" {
		return nil
	}
	var parsed struct {
		ReviewThreadReplies []struct {
			FixItemID              string `json:"fixItemId"`
			ThreadID               string `json:"threadId"`
			Action                 string `json:"action"`
			Explanation            string `json:"explanation"`
			ThreadCommentsObserved string `json:"threadCommentsObserved"`
		} `json:"review_thread_replies"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return nil
	}
	if len(parsed.ReviewThreadReplies) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(parsed.ReviewThreadReplies))
	out := make([]replyExplanationEntry, 0, len(parsed.ReviewThreadReplies))
	for _, raw := range parsed.ReviewThreadReplies {
		fixItemID := strings.TrimSpace(raw.FixItemID)
		if fixItemID == "" {
			continue
		}
		item, ok := itemsByID[fixItemID]
		if !ok {
			continue
		}
		threadID := strings.TrimSpace(raw.ThreadID)
		if threadID != "" && item.ThreadID != "" && threadID != item.ThreadID {
			continue
		}
		action := canonicalizeReplyAction(raw.Action)
		if action == "" {
			action = string(replyActionFixed)
		}
		explanation := sanitizeReplyExplanation(raw.Explanation)
		if explanation == "" {
			continue
		}
		if _, dup := seen[fixItemID]; dup {
			continue
		}
		seen[fixItemID] = struct{}{}
		out = append(out, replyExplanationEntry{FixItemID: fixItemID, ThreadID: item.ThreadID, Action: action, Explanation: explanation, ThreadCommentsObserved: normalizeThreadCommentsObserved(raw.ThreadCommentsObserved)})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeReplyAction(raw string) string {
	switch replyAction(strings.ToLower(strings.TrimSpace(raw))) {
	case replyActionFixed:
		return string(replyActionFixed)
	case replyActionDeclined:
		return string(replyActionDeclined)
	default:
		return ""
	}
}

func parseReplyAction(raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return "", true
	}
	action := normalizeReplyAction(raw)
	if action == "" {
		return "", false
	}
	return action, true
}

func canonicalizeReplyAction(raw string) string {
	action, ok := parseReplyAction(raw)
	if ok {
		if action == "" {
			return string(replyActionFixed)
		}
		return action
	}
	return strings.TrimSpace(raw)
}

// extractCompletionMarkerPayload mirrors the agent core's last-line scan but
// without coupling fixer code to internal/agent's parser. It returns the JSON
// payload after the final __LOOPER_RESULT__= line, or "" if absent.
func extractCompletionMarkerPayload(combined string) string {
	prefix := agent.CompletionMarkerPrefix
	lines := strings.Split(combined, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, prefix) {
			payload := strings.TrimPrefix(line, prefix)
			if isTemplateCompletionPayload(payload) {
				continue
			}
			return payload
		}
	}
	return ""
}

func isTemplateCompletionPayload(payload string) bool {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return false
	}
	summary, _ := parsed["summary"].(string)
	if strings.TrimSpace(summary) != "<one-sentence summary>" {
		return false
	}
	if len(parsed) != 1 {
		return false
	}
	_, ok := parsed["summary"]
	return ok
}

func sanitizeReplyExplanation(raw string) string {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return ""
	}
	// Drop any disclosure marker/footer the agent might have echoed; the adapter
	// is the only place allowed to stamp.
	cleaned = disclosure.StripMarkdownStamp(cleaned)
	// Strip leading mention clusters first so leftover punctuation from copied
	// greetings does not dominate the sanitized explanation.
	cleaned = leadingMentionPattern.ReplaceAllString(cleaned, "")
	// Strip @mentions anywhere; runner controls whom to ping.
	cleaned = inlineMentionPattern.ReplaceAllString(cleaned, `${1}${2}`)
	// Strip HTML tags conservatively; review thread bodies are markdown.
	cleaned = htmlTagPattern.ReplaceAllString(cleaned, "")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return ""
	}
	if len([]rune(cleaned)) > maxReplyExplanationLength {
		runes := []rune(cleaned)
		cleaned = strings.TrimSpace(string(runes[:maxReplyExplanationLength])) + "…"
	}
	return cleaned
}

var (
	leadingMentionPattern = regexp.MustCompile(`(?m)^(?:@[A-Za-z0-9][A-Za-z0-9-]{0,38}(?:[,:;]+)?\s*)+`)
	inlineMentionPattern  = regexp.MustCompile(`(^|[^[:alnum:]])@[A-Za-z0-9][A-Za-z0-9-]{0,38}([^[:alnum:]-]|$)`)
	htmlTagPattern        = regexp.MustCompile(`</?[A-Za-z][^>]*>`)
)

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
	sleep := options.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	disclosureCfg := config.DefaultDisclosureConfig()
	if options.Disclosure != nil {
		disclosureCfg = *options.Disclosure
	}
	policy := options.DiscoveryPolicy
	if policy.AuthorFilter == "" {
		policy = DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false, AuthorFilter: config.FixerAuthorFilterCurrentUser, Labels: []string{}, LabelMode: config.LabelModeAll}
		if options.FixAllPullRequests {
			policy.AuthorFilter = config.FixerAuthorFilterAny
		}
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
		validationCommands:      append([]string(nil), options.ValidationCommands...),
		validationRunner:        options.ValidationRunner,
		allowAutoCommit:         options.AllowAutoCommit,
		allowAutoPush:           options.AllowAutoPush,
		allowRiskyFixes:         options.AllowRiskyFixes,
		fixAllPullRequests:      options.FixAllPullRequests,
		discoveryPolicy:         policy,
		disclosure:              disclosureCfg,
		agentRuntime:            strings.TrimSpace(options.AgentRuntime),
		customInstructions:      customInstructionConfig(options.CustomInstructions),
		projectRoleConfig:       options.CustomInstructions,
		agentModel:              derefString(options.AgentModel),
		sleep:                   sleep,
		retryBaseDelay:          retryBaseDelay,
		retryMaxAttempts:        retryMax,
		onAgentExecutionStarted: options.OnAgentExecutionStarted,
		onQueueItemEnqueued:     options.OnQueueItemEnqueued,
	}
}

func (r *Runner) DiscoverPullRequests(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	ctx = githubinfra.ContextWithDiscoverySnapshot(ctx, input.Snapshot)
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
	policy := r.discoveryPolicyForProject(project.ID)
	if !policy.AutoDiscovery {
		return DiscoveryResult{Skipped: 1}, nil
	}
	currentUser := ""
	if policy.AuthorFilter != config.FixerAuthorFilterAny {
		currentUser, err = r.github.GetCurrentUserLogin(ctx, project.RepoPath)
		if err != nil {
			return DiscoveryResult{}, err
		}
		currentUser = strings.TrimSpace(currentUser)
	}
	recoveredQueueItems, err := r.recoverLegacyNoopFollowupLoops(ctx, *project, input.Repo, policy, currentUser)
	if err != nil {
		return DiscoveryResult{}, err
	}
	openPRs, err := r.listOpenPullRequestsForDiscoveryWithPolicy(ctx, input.Repo, project.RepoPath, input.Limit, currentUser, policy)
	if err != nil {
		return DiscoveryResult{}, err
	}
	result := DiscoveryResult{}
	for _, item := range recoveredQueueItems {
		appendDiscoveryQueueItem(&result.QueueItems, item)
	}
	for _, pr := range openPRs {

		if !r.pullRequestEligibleForDiscovery(ctx, pr, input.Repo, currentUser, policy) {
			result.Skipped++
			continue
		}
		detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: pr.Number, CWD: project.RepoPath})
		if err != nil {
			return DiscoveryResult{}, err
		}
		if err := r.discoverPullRequestFromDetail(ctx, *project, input.Repo, detail, &result); err != nil {
			return DiscoveryResult{}, err
		}
	}
	return result, nil
}

func (r *Runner) DiscoverPullRequest(ctx context.Context, input TargetedDiscoveryInput) (DiscoveryResult, error) {
	if input.PRNumber <= 0 {
		return DiscoveryResult{}, fmt.Errorf("prNumber must be positive")
	}
	ctx = githubinfra.ContextWithDiscoverySnapshot(ctx, input.Snapshot)
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
	policy := r.discoveryPolicyForProject(project.ID)
	if !policy.AutoDiscovery {
		return DiscoveryResult{Skipped: 1}, nil
	}
	currentUser := ""
	if policy.AuthorFilter != config.FixerAuthorFilterAny {
		currentUser, err = r.github.GetCurrentUserLogin(ctx, project.RepoPath)
		if err != nil {
			return DiscoveryResult{}, err
		}
		currentUser = strings.TrimSpace(currentUser)
	}
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: project.RepoPath})
	if err != nil {
		return DiscoveryResult{}, err
	}
	pr := PullRequestSummary{Number: detail.Number, State: detail.State, IsDraft: detail.IsDraft, Labels: append([]string(nil), detail.Labels...), HeadSHA: detail.HeadSHA, Author: detail.Author}
	result := DiscoveryResult{}
	if !r.pullRequestEligibleForDiscovery(ctx, pr, input.Repo, currentUser, policy) {
		result.Skipped++
		return result, nil
	}
	if err := r.discoverPullRequestFromDetail(ctx, *project, input.Repo, detail, &result); err != nil {
		return DiscoveryResult{}, err
	}
	return result, nil
}

func appendDiscoveryQueueItem(items *[]storage.QueueItemRecord, item storage.QueueItemRecord) {
	for i, existing := range *items {
		if existing.ID == item.ID {
			(*items)[i] = item
			return
		}
	}
	*items = append(*items, item)
}

func (r *Runner) listOpenPullRequestsForDiscovery(ctx context.Context, repo, cwd string, limit int, author string) ([]PullRequestSummary, error) {
	return r.listOpenPullRequestsForDiscoveryWithPolicy(ctx, repo, cwd, limit, author, r.discoveryPolicy)
}

func (r *Runner) listOpenPullRequestsForDiscoveryWithPolicy(ctx context.Context, repo, cwd string, limit int, author string, policy DiscoveryPolicy) ([]PullRequestSummary, error) {
	labels := prQueryLabels(policy.Labels)
	effectiveLimit := defaultDiscoveryLimit(limit)
	if len(labels) == 0 {
		return r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: limit, Author: author})
	}
	if len(labels) == 1 {
		return r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: limit, Author: author, Label: labels[0]})
	}
	if policy.LabelMode == config.LabelModeAll {
		return r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: limit, Author: author, Labels: labels})
	}

	result := []PullRequestSummary{}
	seen := map[int64]struct{}{}
	for _, label := range labels {
		if len(result) >= effectiveLimit {
			break
		}
		prs, err := r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: effectiveLimit, Author: author, Label: label})
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

func (r *Runner) discoveryPolicyForProject(projectID string) DiscoveryPolicy {
	if r.projectRoleConfig == nil {
		return r.discoveryPolicy
	}
	roles := config.ProjectRoleConfigs(*r.projectRoleConfig, projectID)
	return DiscoveryPolicy{AutoDiscovery: roles.Fixer.AutoDiscovery, IncludeDrafts: roles.Fixer.Triggers.IncludeDrafts, AuthorFilter: roles.Fixer.Triggers.AuthorFilter, Labels: append([]string(nil), roles.Fixer.Triggers.Labels...), LabelMode: roles.Fixer.Triggers.LabelMode}
}

func defaultDiscoveryLimit(limit int) int {
	if limit <= 0 {
		return 30
	}
	return limit
}

func (r *Runner) pullRequestEligibleForDiscovery(ctx context.Context, pr PullRequestSummary, repo, currentUser string, policy DiscoveryPolicy) bool {
	if (!policy.IncludeDrafts && pr.IsDraft) || normalizePRState(pr.State) != "open" || r.hasActivePRLock(ctx, repo, pr.Number) {
		return false
	}
	if policy.AuthorFilter != config.FixerAuthorFilterAny && !sameGitHubLogin(pr.Author, currentUser) {
		return false
	}
	if !labelsMatch(pr.Labels, policy.Labels, policy.LabelMode) {
		return false
	}
	return true
}

func (r *Runner) discoverPullRequestFromDetail(ctx context.Context, project storage.ProjectRecord, repo string, detail PullRequestDetail, result *DiscoveryResult) error {
	allFixItems := collectFixItems(detail)
	if len(allFixItems) == 0 {
		if err := r.clearFixerFollowupStateForPR(ctx, project.ID, repo, detail.Number); err != nil {
			return err
		}
		result.Skipped++
		return nil
	}
	allFixItemsStateHash := hashFixItemsState(allFixItems)
	fixItems := suppressDeclinedFixItems(loopMetadataForPR(ctx, r, project.ID, repo, detail.Number), detail.HeadSHA, allFixItems)
	if len(fixItems) == 0 {
		if err := r.resumePausedZeroProgressLoopIfStateChanged(ctx, project.ID, repo, detail.Number, detail.HeadSHA, allFixItemsStateHash); err != nil {
			return err
		}
		result.Skipped++
		return nil
	}
	fixItemsHash := hashFixItems(fixItems)
	fixItemsStateHash := allFixItemsStateHash
	unresolvedThreadIDs := unresolvedThreadIDs(fixItems)
	if len(unresolvedThreadIDs) == 0 {
		if err := r.clearFixerFollowupMetadataForPR(ctx, project.ID, repo, detail.Number); err != nil {
			return err
		}
	}
	loopResult, err := r.ensureLoopForPullRequest(ctx, project, repo, detail.Number, detail.HeadSHA, fixItemsHash, fixItemsStateHash, fixItems, unresolvedThreadIDs)
	if err != nil {
		return err
	}
	if loopResult.record.Status == "paused" || loopResult.record.Status == "failed" || loopResult.skipped {
		result.Skipped++
		return nil
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
		Repo:         repo,
		PRNumber:     detail.Number,
		HeadSHA:      headSHA,
		FixItemsHash: fixItemsStateHash,
		AvailableAt:  loopResult.availableAt,
	})
	if err != nil {
		return err
	}
	appendDiscoveryQueueItem(&result.QueueItems, queueItem)
	return nil
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
	var activeErr *activeRunError
	var failedQueue *storage.QueueItemRecord
	var failErr error
	if errors.As(err, &activeErr) {
		failedQueue, failErr = r.requeueQueueItem(ctx, queueItem, failure.kind, failure.message, queueItem.Attempts)
	} else {
		failedQueue, failErr = r.failQueueItem(ctx, queueItem, failure.kind, failure.message)
	}
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
			if loops.ShouldPauseLoopAfterFailure(string(failureKind), failedQueue, "") {
				updated.Status = "paused"
			} else {
				updated.Status = "failed"
				stampFixerFailedDiscoveryFingerprint(updated, queueItem)
			}
			updated.NextRunAt = nil
		}
	})
	return err
}

func (r *Runner) ProcessClaimedItem(ctx context.Context, queueItem storage.QueueItemRecord) (result ProcessResult, retErr error) {
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
	runCreated := true
	defer func() {
		if retErr == nil || !runCreated {
			return
		}
		persisted, err := r.repos.Runs.GetByID(context.Background(), run.ID)
		if err != nil || persisted == nil || persisted.Status != "running" {
			return
		}
		failure := r.classifyFailure(retErr)
		latest := r.getLatestCheckpoint(context.Background(), run, checkpoint)
		latest.ResumePolicy = loops.NormalizeResumePolicy(string(failure.kind), latest.ResumePolicy)
		completed, err := r.completeRun(context.Background(), *persisted, "failed", failure.message, failure.message, latest)
		if err != nil {
			r.logWarn("fixer run cleanup after pre-start failure failed", map[string]any{"runId": run.ID, "queueItemId": queueItem.ID, "error": err.Error()})
			return
		}
		run = completed
	}()
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
		latest.ResumePolicy = loops.NormalizeResumePolicy(string(failure.kind), latest.ResumePolicy)
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
				if loops.ShouldPauseLoopAfterFailure(string(failure.kind), failedQueue, latest.ResumePolicy) {
					updated.Status = "paused"
				} else {
					updated.Status = "failed"
					stampFixerFailedDiscoveryFingerprint(updated, queueItem)
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
	checkpoint.RunPreStartAt = r.nowISO()
	checkpoint.RunPreStartRunID = run.ID
	if err := r.persistCheckpoint(ctx, run.ID, resumedRun.StartStep, checkpoint); err != nil {
		return ProcessResult{}, err
	}
	persistedRun, err := r.repos.Runs.GetByID(ctx, run.ID)
	if err != nil {
		return ProcessResult{}, err
	}
	if persistedRun == nil {
		return ProcessResult{}, fmt.Errorf("run not found after start checkpoint: %s", resumedRun.Run.ID)
	}
	run = *persistedRun
	if reason, err := r.pullRequestOwnershipSkipReason(ctx, project.ID, project.RepoPath, *queueItem.Repo, *queueItem.PRNumber); err != nil {
		return ProcessResult{}, err
	} else if reason != "" {
		checkpoint.SkipReason = reason
		if _, err := r.completeRun(ctx, run, "success", reason, "", checkpoint); err != nil {
			return ProcessResult{}, err
		}
		r.appendEvent(ctx, eventInput{eventType: "run.completed", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"summary": reason}})
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
		return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: "skipped", Summary: reason}, nil
	}
	checkpoint.RunStartedAt = r.nowISO()
	checkpoint.RunStartedRunID = run.ID
	checkpoint.RunPreStartAt = ""
	checkpoint.RunPreStartRunID = ""
	if err := r.persistCheckpoint(ctx, run.ID, resumedRun.StartStep, checkpoint); err != nil {
		return ProcessResult{}, err
	}
	persistedRun, err = r.repos.Runs.GetByID(ctx, run.ID)
	if err != nil {
		return ProcessResult{}, err
	}
	if persistedRun == nil {
		return ProcessResult{}, fmt.Errorf("run not found after start checkpoint: %s", resumedRun.Run.ID)
	}
	run = *persistedRun
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
			latest := checkpoint
			latest.ResumePolicy = loops.NormalizeResumePolicy(string(failure.kind), latest.ResumePolicy)
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
					if loops.ShouldPauseLoopAfterFailure(string(failure.kind), failedQueue, latest.ResumePolicy) {
						updated.Status = "paused"
					} else {
						updated.Status = "failed"
						stampFixerFailedDiscoveryFingerprint(updated, queueItem)
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
	if hasProgressed(checkpoint) {
		if _, err := r.clearZeroProgressMetadata(ctx, *loop); err != nil {
			return ProcessResult{}, err
		}
	} else {
		paused, err := r.recordZeroProgressSuccess(ctx, *loop, checkpoint)
		if err != nil {
			return ProcessResult{}, err
		}
		if paused {
			r.cleanupFixerWorktreeIfTerminal(context.Background(), *project, &checkpoint)
			return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: statusForSkip(checkpoint.SkipReason), Summary: summary}, nil
		}
	}
	if scheduled, err := r.scheduleFollowupRetryAfterSuccess(ctx, *loop, *queueItem.Repo, *queueItem.PRNumber, checkpoint.SkipReason == ""); err != nil {
		return ProcessResult{}, err
	} else if !scheduled {
		if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
			updated.Status = "completed"
			updated.LastRunAt = stringPtr(r.nowISO())
			updated.NextRunAt = nil
		}); err != nil {
			return ProcessResult{}, err
		}
	}
	r.cleanupFixerWorktreeIfTerminal(context.Background(), *project, &checkpoint)
	status := statusForSkip(checkpoint.SkipReason)
	return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: status, Summary: summary}, nil
}

func statusForSkip(skipReason string) string {
	if skipReason != "" {
		return "skipped"
	}
	return "success"
}

func (r *Runner) scheduleFollowupRetryAfterSuccess(ctx context.Context, loop storage.LoopRecord, repo string, prNumber int64, allow bool) (bool, error) {
	if !allow {
		return false, nil
	}
	current, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return false, err
	}
	if current == nil {
		return false, nil
	}
	followup, ok := parseFixerFollowupState(parseJSONObject(current.MetadataJSON))
	if !ok || followup.Terminal {
		return false, nil
	}
	availableAt := parseRFC3339OrZero(followup.NextEligibleAt)
	if availableAt.IsZero() {
		return false, nil
	}
	updatedLoop, err := r.updateLoop(ctx, *current, func(updated *storage.LoopRecord) {
		updated.Status = "queued"
		updated.LastRunAt = stringPtr(r.nowISO())
		availableAtISO := eventlog.FormatJavaScriptISOString(availableAt.UTC())
		updated.NextRunAt = &availableAtISO
	})
	if err != nil {
		return false, err
	}
	_, err = r.enqueue(ctx, enqueueInput{ProjectID: updatedLoop.ProjectID, LoopID: updatedLoop.ID, Repo: repo, PRNumber: prNumber, HeadSHA: followup.HeadSHA, FixItemsHash: followup.FixItemsStateHash, AvailableAt: availableAt})
	if err != nil {
		return false, err
	}
	return true, nil
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
	checkpoint.Detail = &checkpointDetail{State: detail.State, IsDraft: detail.IsDraft, Labels: cloneStrings(detail.Labels), HeadSHA: detail.HeadSHA, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, BaseSHA: detail.BaseSHA, ReviewDecision: detail.ReviewDecision, Comments: cloneObjectSlice(detail.Comments), IssueComments: cloneObjectSlice(detail.IssueComments), Checks: cloneObjectSlice(detail.Checks), HasConflicts: detail.HasConflicts}
	checkpoint.ResumePolicy = "replay_step"
	return checkpoint, nil
}

func (r *Runner) pullRequestOwnershipSkipReason(ctx context.Context, projectID, cwd, repo string, prNumber int64) (string, error) {
	if r.discoveryPolicyForProject(projectID).AuthorFilter == config.FixerAuthorFilterAny {
		return "", nil
	}
	currentUser, err := r.github.GetCurrentUserLogin(ctx, cwd)
	if err != nil {
		return "", err
	}
	author, err := r.github.GetPullRequestAuthor(ctx, ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		return "", err
	}
	if sameGitHubLogin(author, currentUser) {
		return "", nil
	}
	return fmt.Sprintf("Skipped fixer run for %s#%d because PR author %q does not match fixer owner %q", repo, prNumber, strings.TrimSpace(author), strings.TrimSpace(currentUser)), nil
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
	policy := r.discoveryPolicyForProject(input.Project.ID)
	if (!policy.IncludeDrafts && checkpoint.Detail.IsDraft) || normalizePRState(checkpoint.Detail.State) != "open" {
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
	if checkpoint.Worktree != nil {
		if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: checkpoint.Worktree.Path, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot}); err != nil {
			checkpoint.Worktree = nil
			checkpoint.ResumePolicy = "advance_from_checkpoint"
		} else if checkpoint.Worktree.PreparedAt != "" {
			return checkpoint, nil
		}
	}
	if shouldRebuildWorktree(checkpoint) && checkpoint.Worktree != nil && checkpoint.Worktree.Path != "" && checkpoint.Worktree.Branch != "" {
		if err := r.git.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: input.Project.ID, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: checkpoint.Worktree.Path, Branch: checkpoint.Worktree.Branch, ProtectedBranches: compactStrings([]string{detailBaseRefName(checkpoint.Detail), derefString(input.Project.BaseBranch)})}); err != nil {
			return checkpoint, err
		}
	}
	created, err := r.git.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: input.Project.ID, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, Branch: branch, BaseBranch: firstNonEmpty(detailBaseRefName(checkpoint.Detail), derefString(input.Project.BaseBranch), "main"), PRNumber: input.PRNumber, ProtectedBranches: compactStrings([]string{detailBaseRefName(checkpoint.Detail), derefString(input.Project.BaseBranch)}), CheckoutMode: "detached"})
	if err != nil {
		return checkpoint, err
	}
	prepared, err := r.git.PrepareWorktree(ctx, PrepareWorktreeInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: created.WorktreePath, Branch: branch, ExpectedHeadSHA: detailHeadSHA(checkpoint.Detail)})
	if err != nil {
		return checkpoint, err
	}
	if !prepared.Clean {
		checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
		return checkpoint, &loopError{message: fmt.Sprintf("Fixer worktree is dirty for branch %s; manual intervention required", branch), kind: FailureManualIntervention}
	}
	preparedAt := r.nowISO()
	checkpoint.Worktree = &checkpointWorktree{Path: created.WorktreePath, Branch: branch, HeadSHA: prepared.HeadSHA, BaseHeadSHA: prepared.HeadSHA, PreparedAt: preparedAt}
	checkpoint.ensureLifecycle("fixer", branch, firstNonEmpty(detailBaseRefName(checkpoint.Detail), derefString(input.Project.BaseBranch), "main"), false)
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
				checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
				return checkpoint, &loopError{message: fmt.Sprintf("Skipped %s#%d because risky conflict fixes require manual intervention", input.Repo, input.PRNumber), kind: FailureManualIntervention}
			}
		}
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktreeRoot, rootErr := fixerWorktreeRoot(input.Project)
	if rootErr != nil {
		return checkpoint, rootErr
	}
	if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: worktree.Path, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot}); err != nil {
		checkpoint.Worktree = nil
		checkpoint.Repair = nil
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		input.Checkpoint = checkpoint
		checkpoint, err = r.runPrepareWorktreeStep(ctx, input)
		if err != nil {
			return checkpoint, err
		}
		worktree, err = requireWorktree(checkpoint)
		if err != nil {
			return checkpoint, err
		}
		if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: worktree.Path, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot}); err != nil {
			return checkpoint, err
		}
	}
	executionID := eventlog.NewEventID("agent")
	prompt, instructionBlock := buildFixerPrompt(input.Project.ID, r.customInstructions, input.Repo, input.PRNumber, checkpoint.Detail, checkpoint.FixItems, r.allowAutoPush, r.disclosure, r.agentRuntime, r.agentModel)
	metadata := map[string]any{"loopType": "fixer", "repo": input.Repo, "prNumber": input.PRNumber, "step": "repair"}
	for key, value := range config.CustomInstructionMetadata(instructionBlock, prompt) {
		metadata[key] = value
	}
	execution, err := r.agentExecutor.Start(ctx, AgentRunInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Prompt: prompt, WorkingDirectory: worktree.Path, Timeout: r.agentTimeout, HeartbeatTimeout: r.agentIdleTimeout, Metadata: metadata, IdempotencyKey: fmt.Sprintf("fixer:%s:%s:%s", input.Loop.ID, firstNonEmpty(checkpoint.FixItemsHash, "unknown"), firstNonEmpty(detailHeadSHA(checkpoint.Detail), "unknown"))})
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
		checkpoint.Repair = checkpointRepairFromAgentResult(executionID, detailHeadSHA(checkpoint.Detail), result, r.nowISO())
		checkpoint.ResumePolicy = "retry_from_timeout_context"
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepRepair, checkpoint); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		message := firstNonEmpty(result.Summary, result.Stderr, "Fixer agent "+result.Status)
		return checkpoint, &loopError{message: message, kind: FailureRetryableTransient}
	}
	if err := validateCompletedRepairCheckpoint(&checkpointRepair{Summary: result.Summary, ParseStatus: result.ParseStatus}); err != nil {
		return checkpoint, err
	}
	checkpoint.Repair = checkpointRepairFromAgentResult(executionID, detailHeadSHA(checkpoint.Detail), result, r.nowISO())
	checkpoint.Repair.ReplyExplanations = normalizeReplyExplanationActions(parseReplyExplanations(result.Stdout, result.Stderr, checkpoint.FixItems))
	checkpoint.ensureLifecycle("fixer", worktree.Branch, detailBaseRefName(checkpoint.Detail), false)
	if result.Lifecycle != nil {
		checkpoint.Lifecycle.MergeAgent(result.Lifecycle, r.nowISO())
	}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runReconcileCommitsStep(ctx context.Context, input stepInput) (fixerCheckpoint, error) {
	checkpoint, err := r.reconcileCommits(ctx, input.Project, input.Checkpoint, buildFixerCommitMessage(input.PRNumber))
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
	worktreeRoot, rootErr := fixerWorktreeRoot(input.Project)
	if rootErr != nil {
		return checkpoint, rootErr
	}
	result, err := r.runValidation(ctx, ValidationInput{CWD: worktree.Path, Commands: r.validationCommands})
	if err != nil {
		return checkpoint, err
	}
	if !result.Passed {
		return checkpoint, &loopError{message: firstNonEmpty(result.Summary, "Validation failed"), kind: FailureRetryableAfterResume}
	}
	inspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: reconcileBaseHeadSHA(checkpoint.ReconcileCommits)})
	if err != nil {
		return checkpoint, err
	}
	if inspect.HasUncommittedChanges {
		if checkpoint.Validation != nil && strings.Contains(strings.ToLower(checkpoint.Validation.Summary), "extra reconcile") {
			return checkpoint, &loopError{message: "Validation keeps producing new modifications after an extra reconcile pass", kind: FailureRetryableAfterResume}
		}
		checkpoint, err = r.reconcileCommits(ctx, input.Project, checkpoint, buildFixerCommitMessage(input.PRNumber))
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
		finalInspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: reconcileBaseHeadSHA(checkpoint.ReconcileCommits)})
		if err != nil {
			return checkpoint, err
		}
		if finalInspect.HasUncommittedChanges {
			return checkpoint, &loopError{message: "Validation keeps producing new modifications after an extra reconcile pass", kind: FailureRetryableAfterResume}
		}
		second.HeadSHA = finalInspect.HeadSHA
		second.Summary = firstNonEmpty(second.Summary, "Validation passed after extra reconcile")
		checkpoint.Validation = &second
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		return checkpoint, nil
	}
	result.HeadSHA = inspect.HeadSHA
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
	worktreeRoot, rootErr := fixerWorktreeRoot(input.Project)
	if rootErr != nil {
		return checkpoint, rootErr
	}
	branch := firstNonEmpty(worktree.Branch, detailHeadRefName(checkpoint.Detail))
	if branch == "" {
		return checkpoint, &loopError{message: "Missing PR head branch for push step", kind: FailureRetryableAfterResume}
	}
	if !r.allowAutoPush {
		r.appendEvent(ctx, eventInput{eventType: "fixer.push.skipped", projectID: input.Project.ID, loopID: input.Loop.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"branch": branch, "reason": "auto_push_disabled"}})
		checkpoint.Push = &checkpointPush{Pushed: false, Branch: branch, Remote: "origin", SkippedReason: "Auto push disabled"}
		checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
		return checkpoint, &loopError{message: fmt.Sprintf("Auto push disabled; manual fix push required for branch %s", branch), kind: FailureManualIntervention}
	}
	if checkpoint.ReconcileCommits == nil {
		return checkpoint, &loopError{message: "Missing reconcile-commits checkpoint for push step", kind: FailureRetryableAfterResume}
	}
	if !checkpoint.ReconcileCommits.WorkingTreeClean {
		return checkpoint, &loopError{message: "Working tree must be clean before push", kind: FailureRetryableAfterResume}
	}
	adopted, updatedCheckpoint, err := r.adoptLifecyclePushEvidence(ctx, input, checkpoint, branch)
	if err != nil {
		return updatedCheckpoint, err
	}
	if adopted {
		return updatedCheckpoint, nil
	}
	if checkpoint.ReconcileCommits.FinalHeadSHA != "" && checkpoint.ReconcileCommits.FinalHeadSHA == worktree.BaseHeadSHA {
		r.appendEvent(ctx, eventInput{eventType: "fixer.push.skipped", projectID: input.Project.ID, loopID: input.Loop.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"branch": branch, "reason": "no_new_commits"}})
		checkpoint.Push = &checkpointPush{Pushed: false, Branch: branch, Remote: "origin", SkippedReason: "No new commits to push", Evidence: resolveFixEvidence(checkpoint, input.Loop.MetadataJSON, checkpoint.FixItemsHash)}
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		return checkpoint, nil
	}
	if err := r.git.Push(ctx, PushInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, Branch: branch, ExpectedRemoteHeadSHA: worktree.BaseHeadSHA}); err != nil {
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
	pushedAt := r.nowISO()
	pushedHeadSHA := resolveCommentsExpectedHeadSHA(checkpoint)
	evidence := &fixEvidence{Valid: pushedHeadSHA != "" && checkpoint.FixItemsHash != "", HeadSHA: pushedHeadSHA, CommitSHAs: cloneStrings(checkpoint.ReconcileCommits.NewCommitSHAs), BaseHeadSHA: checkpoint.ReconcileCommits.BaseHeadSHA, FixItemsHash: checkpoint.FixItemsHash, CommentRecords: buildFixCommentEvidenceRecords(checkpoint, lastNonEmptyString(checkpoint.ReconcileCommits.NewCommitSHAs, pushedHeadSHA)), Source: "fallback_push", ProducedNewCommits: roundProducedNewCommits(&checkpoint), PushedAt: pushedAt}
	checkpoint.Push = &checkpointPush{Pushed: true, Branch: branch, Remote: "origin", HeadSHA: pushedHeadSHA, PushedAt: pushedAt, Evidence: evidence}
	if err := r.persistCheckpoint(ctx, input.Run.ID, stepPush, checkpoint); err != nil {
		return checkpoint, err
	}
	store, err := r.mergedFixEvidenceStoreV2(ctx, input.Loop, buildFixEvidenceStoreV2(checkpoint, evidence, input.Run.ID))
	if err != nil {
		return checkpoint, err
	}
	if _, err := r.mergeLoopMetadata(ctx, input.Loop, map[string]any{"lastFixHeadSha": pushedHeadSHA, "lastFixItemsHash": checkpoint.FixItemsHash, "lastFixPushedAt": pushedAt, "lastFixEvidence": evidence, "fixEvidenceStoreV2": store}); err != nil {
		return checkpoint, err
	}
	liveDetail, err := r.waitForPullRequestHeadSHA(ctx, waitForPullRequestHeadSHAInput{Repo: input.Repo, PRNumber: input.PRNumber, ExpectedHeadSHA: finalHeadSHA, CWD: input.Project.RepoPath, Attempts: 5, Delay: time.Second, FailureMessage: func(actual string) string {
		return fmt.Sprintf("PR head did not update after push: expected %s, got %s", finalHeadSHA, firstNonEmpty(actual, "unknown"))
	}})
	if err != nil {
		return checkpoint, err
	}
	checkpoint = refreshCheckpointHeadAfterPush(checkpoint, liveDetail)
	pushedHeadSHA = resolveCommentsExpectedHeadSHA(checkpoint)
	checkpoint.Push.HeadSHA = pushedHeadSHA
	checkpoint.Push.Evidence.HeadSHA = pushedHeadSHA
	if err := r.persistCheckpoint(ctx, input.Run.ID, stepPush, checkpoint); err != nil {
		return checkpoint, err
	}
	store, err = r.mergedFixEvidenceStoreV2(ctx, input.Loop, buildFixEvidenceStoreV2(checkpoint, evidence, input.Run.ID))
	if err != nil {
		return checkpoint, err
	}
	if _, err := r.mergeLoopMetadata(ctx, input.Loop, map[string]any{"lastFixHeadSha": pushedHeadSHA, "lastFixItemsHash": checkpoint.FixItemsHash, "lastFixPushedAt": pushedAt, "lastFixEvidence": evidence, "fixEvidenceStoreV2": store}); err != nil {
		return checkpoint, err
	}
	r.appendEvent(ctx, eventInput{eventType: "pr.branch.pushed", projectID: input.Project.ID, loopID: input.Loop.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"branch": branch, "pushedAt": pushedAt, "headSha": nilIfEmpty(pushedHeadSHA)}})
	checkpoint.ensureLifecycle("fixer", branch, detailBaseRefName(checkpoint.Detail), false)
	checkpoint.Lifecycle.Pushed = true
	checkpoint.Lifecycle.Actions.Push = lifecycle.ActionSourceFallback
	checkpoint.Lifecycle.PRNumber = input.PRNumber
	checkpoint.Lifecycle.PRAdopted = true
	checkpoint.Lifecycle.Actions.PR = lifecycle.ActionSourceFallback
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) adoptLifecyclePushEvidence(ctx context.Context, input stepInput, checkpoint fixerCheckpoint, branch string) (bool, fixerCheckpoint, error) {
	if checkpoint.Repair == nil || checkpoint.Repair.Status != "completed" {
		return false, checkpoint, nil
	}
	lc := checkpoint.Repair.Lifecycle
	if lc == nil {
		lc = checkpoint.Lifecycle
	}
	if lc == nil {
		return false, checkpoint, nil
	}
	if !lc.Pushed || lc.Actions.Push != lifecycle.ActionSourceAgent || len(lc.CommitSHAs) == 0 {
		return false, checkpoint, nil
	}
	adoptedHead := strings.TrimSpace(lc.CommitSHAs[len(lc.CommitSHAs)-1])
	if adoptedHead == "" || adoptedHead == checkpoint.Worktree.BaseHeadSHA {
		return false, checkpoint, nil
	}
	if lc.PRNumber > 0 && lc.PRNumber != input.PRNumber {
		return false, checkpoint, nil
	}
	if strings.TrimSpace(lc.Branch) != "" && strings.TrimSpace(lc.Branch) != branch {
		return false, checkpoint, nil
	}
	// PRNumber/Branch/HeadRefName/HeadSHA are validated independently
	// against the live PR detail below; we deliberately do not gate
	// adoption on a fix-items hash match. New review threads arriving
	// during repair routinely shift the snapshot hash without
	// invalidating the agent's claim that it pushed the right commit.
	liveDetail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return false, checkpoint, err
	}
	if strings.TrimSpace(liveDetail.HeadRefName) != branch || strings.TrimSpace(liveDetail.HeadSHA) != adoptedHead {
		return false, checkpoint, nil
	}
	if checkpoint.Validation == nil || !checkpoint.Validation.Passed || checkpoint.Validation.HeadSHA != adoptedHead {
		worktreeRoot, rootErr := fixerWorktreeRoot(input.Project)
		if rootErr != nil {
			return false, checkpoint, rootErr
		}
		prepared, err := r.git.PrepareWorktree(ctx, PrepareWorktreeInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: checkpoint.Worktree.Path, Branch: branch, ExpectedHeadSHA: adoptedHead})
		if err != nil {
			return false, checkpoint, err
		}
		if !prepared.Clean {
			return false, checkpoint, &loopError{message: fmt.Sprintf("Fixer worktree is dirty after adopting agent-pushed head %s", adoptedHead), kind: FailureRetryableAfterResume}
		}
		validation, err := r.runValidation(ctx, ValidationInput{CWD: checkpoint.Worktree.Path, Commands: r.validationCommands})
		if err != nil {
			return false, checkpoint, err
		}
		inspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: checkpoint.Worktree.Path, BaseRef: checkpoint.ReconcileCommits.BaseHeadSHA})
		if err != nil {
			return false, checkpoint, err
		}
		validation.HeadSHA = inspect.HeadSHA
		checkpoint.Validation = &validation
		if inspect.HasUncommittedChanges {
			return false, checkpoint, &loopError{message: "Validation produced uncommitted changes after adopting agent-pushed head", kind: FailureRetryableAfterResume}
		}
		if !validation.Passed || validation.HeadSHA != adoptedHead {
			return false, checkpoint, &loopError{message: firstNonEmpty(validation.Summary, "Validation failed for adopted agent-pushed head"), kind: FailureRetryableAfterResume}
		}
		checkpoint.Worktree.HeadSHA = adoptedHead
	}
	checkpoint.Detail = mergeCheckpointDetailPreservingLabels(checkpoint.Detail, liveDetail)
	pushedAt := r.nowISO()
	evidence := &fixEvidence{Valid: true, Source: "agent_push", HeadSHA: adoptedHead, CommitSHAs: cloneStrings(lc.CommitSHAs), BaseHeadSHA: checkpoint.ReconcileCommits.BaseHeadSHA, FixItemsHash: checkpoint.FixItemsHash, CommentRecords: buildFixCommentEvidenceRecords(checkpoint, lastNonEmptyString(lc.CommitSHAs, adoptedHead)), ProducedNewCommits: true, PushedAt: pushedAt}
	checkpoint.Push = &checkpointPush{Pushed: true, Branch: branch, Remote: "origin", HeadSHA: adoptedHead, PushedAt: pushedAt, Evidence: evidence}
	if err := r.persistCheckpoint(ctx, input.Run.ID, stepPush, checkpoint); err != nil {
		return false, checkpoint, err
	}
	store, err := r.mergedFixEvidenceStoreV2(ctx, input.Loop, buildFixEvidenceStoreV2(checkpoint, evidence, input.Run.ID))
	if err != nil {
		return false, checkpoint, err
	}
	if _, err := r.mergeLoopMetadata(ctx, input.Loop, map[string]any{"lastFixHeadSha": adoptedHead, "lastFixItemsHash": checkpoint.FixItemsHash, "lastFixPushedAt": pushedAt, "lastFixEvidence": evidence, "fixEvidenceStoreV2": store}); err != nil {
		return false, checkpoint, err
	}
	checkpoint.ensureLifecycle("fixer", branch, detailBaseRefName(checkpoint.Detail), false)
	checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, adoptedHead)
	checkpoint.Lifecycle.Pushed = true
	checkpoint.Lifecycle.Actions.Push = lifecycle.ActionSourceAgent
	checkpoint.Lifecycle.PRNumber = input.PRNumber
	checkpoint.Lifecycle.PRAdopted = true
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	r.appendEvent(ctx, eventInput{eventType: "fixer.push.adopted", projectID: input.Project.ID, loopID: input.Loop.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"branch": branch, "headSha": adoptedHead}})
	return true, checkpoint, nil
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
	liveDetail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return checkpoint, err
	}
	// Ancestor guard: if we previously pushed a fix commit, make sure the
	// live PR head still descends from it. If a collaborator force-pushed or
	// rebased and dropped our commit, replying and resolving threads would
	// be acknowledging code that no longer exists in the PR. Bail out and
	// let the next discover round re-derive everything from scratch.
	//
	// "identical" or "ahead" means the fix commit is still reachable from
	// the live head (possibly with extra commits stacked on top from CI
	// bots, the author, or other collaborators) — that is harmless and we
	// proceed. "behind" or "diverged" means history was rewritten such
	// that the fix commit is no longer an ancestor of the head; we must
	// abandon this round.
	if expectedHead := resolveCommentsExpectedHeadSHA(checkpoint); expectedHead != "" && liveDetail.HeadSHA != "" && liveDetail.HeadSHA != expectedHead {
		cmp, cmpErr := r.github.CompareCommits(ctx, CompareCommitsInput{Repo: input.Repo, Base: expectedHead, Head: liveDetail.HeadSHA, CWD: input.Project.RepoPath})
		if cmpErr != nil {
			checkpoint.ResumePolicy = loops.ResumePolicyRestartFromDiscover
			return checkpoint, &loopError{message: fmt.Sprintf("Failed to verify fix commit %s is reachable from PR head %s: %v", expectedHead, liveDetail.HeadSHA, cmpErr), kind: FailureRetryableAfterResume}
		}
		switch strings.ToLower(strings.TrimSpace(cmp.Status)) {
		case "identical", "ahead":
			// fix commit still in head's history; safe to continue
		default:
			checkpoint.ResumePolicy = loops.ResumePolicyRestartFromDiscover
			return checkpoint, &loopError{message: fmt.Sprintf("PR head %s no longer descends from fix commit %s (compare status %q); will rediscover", liveDetail.HeadSHA, expectedHead, cmp.Status), kind: FailureRetryableAfterResume}
		}
	}
	checkpoint.Detail = mergeCheckpointDetailPreservingLabels(checkpoint.Detail, liveDetail)
	fixItems := collectFixItems(liveDetail)
	checkpoint.FixItems = fixItems
	checkpoint.FixItemsHash = hashFixItems(fixItems)
	if checkpoint.ResolvedComments == nil {
		checkpoint.ResolvedComments = &checkpointResolvedComments{Items: []checkpointResolvedComment{}}
	}
	resolvedCount := 0
	contractViolationCount := 0
	declinedUpdates := map[string]declinedThreadRecord{}
	commentItems := make([]FixItem, 0, len(fixItems))
	for _, item := range fixItems {
		if item.Type == "comment" {
			commentItems = append(commentItems, item)
		}
	}
	repliesByItemID := agentResolveRepliesByFixItemID(checkpoint)
	repliesByThreadID := agentResolveRepliesByThreadID(checkpoint)
	commitSHA := resolveCommentCommitSHA(checkpoint, nil, false)
	if commitSHA == "" {
		commitSHA = resolveCommentsExpectedHeadSHA(checkpoint)
	}
	driftCount := 0
	mutationFailureCount := 0
	// Drift detection must be anchored to when the agent recorded the
	// reply explanations, not when the current (possibly retried) run
	// started. Otherwise a reviewer comment posted between the original
	// repair and a replay/retry would be classified as "old" and the
	// stale explanation would be used to resolve fresh feedback.
	driftSince := input.Run.StartedAt
	if checkpoint.Repair != nil && strings.TrimSpace(checkpoint.Repair.CompletedAt) != "" {
		driftSince = checkpoint.Repair.CompletedAt
	}
	// Per-thread drift detection (hasNonLooperCommentSince below) handles
	// the case where a reviewer added or edited a comment on an existing
	// thread after the agent recorded its decisions. New threads that
	// appeared after the snapshot are not present in the agent's payload.
	// Treat those missing/invalid per-thread decisions as contract
	// violations, while still honoring any valid "fixed"/"declined"
	// decisions the agent did provide for other threads. Invalidating the
	// entire reply set on a fix-items hash mismatch threw away every
	// successful "fixed" decision whenever a single new thread appeared,
	// which on PRs receiving a steady stream of bot comments produced an
	// unbreakable drift loop.
	for _, item := range commentItems {
		if alreadyResolved(checkpoint.ResolvedComments.Items, item) {
			continue
		}
		decision, ok := repliesByItemID[item.ID]
		if !ok && item.ThreadID != "" {
			decision, ok = repliesByThreadID[item.ThreadID]
		}
		decisionIssueStatus := ""
		decisionIssueMessage := ""
		if !ok {
			decisionIssueStatus = "skipped_missing_agent_decision"
			decisionIssueMessage = agentMissingThreadDecisionExplanation
		} else if _, validAction := parseReplyAction(decision.Action); !validAction {
			decisionIssueStatus = "skipped_invalid_agent_decision"
			decisionIssueMessage = agentInvalidThreadDecisionExplanation
		} else if normalizeReplyAction(decision.Action) == string(replyActionDeclined) && !hasSubstantiveDeclineExplanation(decision.Explanation) {
			decisionIssueStatus = "skipped_invalid_agent_decision"
			decisionIssueMessage = agentDeclinedThreadWithoutReason
		}
		thread, err := r.github.ViewReviewThread(ctx, ViewReviewThreadInput{ThreadID: item.ThreadID, CWD: input.Project.RepoPath})
		if err != nil {
			return checkpoint, err
		}
		if thread.IsResolved {
			upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Status: "already_resolved", UpdatedAt: r.nowISO()})
			continue
		}
		if hasNonLooperCommentSince(thread, driftSince) {
			driftCount++
			upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Status: "skipped_thread_drift", Message: "New human comment was added to this thread after the fixer run started", UpdatedAt: r.nowISO()})
			continue
		}
		if decisionIssueStatus != "" {
			contractViolationCount++
			upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Status: decisionIssueStatus, Message: decisionIssueMessage, UpdatedAt: r.nowISO()})
			continue
		}
		if fixedDecisionMissingThreadSnapshot(decision) || threadCommentsObservedDrifted(decision, thread) {
			driftCount++
			upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Status: "skipped_thread_drift", Message: "Review thread snapshot was missing or changed since the fixer inspected it", UpdatedAt: r.nowISO()})
			continue
		}
		switch normalizeReplyAction(decision.Action) {
		case string(replyActionDeclined):
			decisionFingerprint := buildDeclinedThreadFingerprint(item, liveDetail.HeadSHA)
			replyState, replyError := r.replyToDeclinedComment(ctx, input, item, decisionFingerprint, decision.Explanation, checkpoint.ResolvedComments.Items)
			if replyState == "failed" {
				mutationFailureCount++
				upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Action: string(replyActionDeclined), Status: "failed_mutation_retry", Message: decision.Explanation, UpdatedAt: r.nowISO(), ReplyState: replyState, ReplyError: replyError})
				continue
			}
			if err := r.persistCheckpoint(ctx, input.Run.ID, stepResolveComments, checkpoint); err != nil {
				return checkpoint, err
			}
			if err := r.github.ResolveReviewThread(ctx, ResolveReviewThreadInput{Repo: input.Repo, ThreadID: item.ThreadID, CWD: input.Project.RepoPath}); err != nil {
				message := err.Error()
				if strings.Contains(strings.ToLower(message), "already") {
					if replyState == "sent" {
						declinedUpdates[decisionFingerprint] = declinedThreadRecord{RecordedAt: r.nowISO(), ThreadID: item.ThreadID, Reason: decision.Explanation}
					}
					upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Action: string(replyActionDeclined), Status: "already_resolved", Message: message, UpdatedAt: r.nowISO(), ReplyState: replyState, ReplyError: replyError})
					continue
				}
				mutationFailureCount++
				upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Action: string(replyActionDeclined), Status: "failed_mutation_retry", Message: message, UpdatedAt: r.nowISO(), ReplyState: replyState, ReplyError: replyError})
				continue
			}
			resolvedCount++
			if replyState == "sent" {
				declinedUpdates[decisionFingerprint] = declinedThreadRecord{RecordedAt: r.nowISO(), ThreadID: item.ThreadID, Reason: decision.Explanation}
			}
			upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Action: string(replyActionDeclined), Status: "agent_declined", Message: decision.Explanation, UpdatedAt: r.nowISO(), ReplyState: replyState, ReplyError: replyError})
		default:
			replyState, replyError := r.replyToFixedComment(ctx, input, item, commitSHA, decision.Explanation, checkpoint.ResolvedComments.Items)
			if err := r.persistCheckpoint(ctx, input.Run.ID, stepResolveComments, checkpoint); err != nil {
				return checkpoint, err
			}
			if err := r.github.ResolveReviewThread(ctx, ResolveReviewThreadInput{Repo: input.Repo, ThreadID: item.ThreadID, CWD: input.Project.RepoPath}); err != nil {
				message := err.Error()
				if strings.Contains(strings.ToLower(message), "already") {
					upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Action: string(replyActionFixed), Status: "already_resolved", Message: message, UpdatedAt: r.nowISO(), ReplyState: replyState, ReplyError: replyError})
					continue
				}
				mutationFailureCount++
				upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Action: string(replyActionFixed), Status: "failed_mutation_retry", Message: message, UpdatedAt: r.nowISO(), ReplyState: replyState, ReplyError: replyError})
				continue
			}
			resolvedCount++
			upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Action: string(replyActionFixed), Status: "resolved", UpdatedAt: r.nowISO(), ReplyState: replyState, ReplyError: replyError})
		}
	}
	if contractViolationCount > 0 {
		if _, err := r.incrementContractViolationCount(ctx, input.Loop, contractViolationCount); err != nil {
			return checkpoint, err
		}
	}
	if len(declinedUpdates) > 0 {
		if _, err := r.persistDeclinedThreadRecords(ctx, input.Loop, declinedUpdates); err != nil {
			return checkpoint, err
		}
	}
	r.appendEvent(ctx, eventInput{eventType: "fixer.comments.resolved", projectID: input.Project.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"items": checkpoint.ResolvedComments.Items}})
	if resolvedCount > 0 {
		r.publishRoundSummaryComment(ctx, input, &checkpoint, fixItems, commitSHA, lookupReplyExplanations(checkpoint))
	}
	if driftCount > 0 {
		checkpoint.ResumePolicy = loops.ResumePolicyRestartFromDiscover
		return checkpoint, &loopError{message: fmt.Sprintf("Skipped %d review thread(s) because review thread content changed during the fixer run", driftCount), kind: FailureRetryableAfterResume}
	}
	if contractViolationCount > 0 {
		checkpoint.ResumePolicy = loops.ResumePolicyRestartFromDiscover
		return checkpoint, &loopError{message: fmt.Sprintf("Skipped %d review thread(s) because the fixer response omitted or invalidated thread decisions", contractViolationCount), kind: FailureRetryableAfterResume}
	}
	if mutationFailureCount > 0 {
		checkpoint.ResumePolicy = loops.ResumePolicyReplayStep
		return checkpoint, &loopError{message: fmt.Sprintf("Failed to resolve %d review thread(s); will retry on next run", mutationFailureCount), kind: FailureRetryableAfterResume}
	}
	if _, err := r.clearFixerFollowupMetadata(ctx, input.Loop); err != nil {
		return checkpoint, err
	}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

// replyToFixedComment posts a reply on the review thread acknowledging the fix
// before the thread is resolved. Reply failures are recorded so the caller can
// retry before resolving the thread. The disclosure stamper applied by the
// GitHub adapter ensures every reply carries the looper exposure marker, so
// automated replies can be filtered downstream.
func (r *Runner) replyToFixedComment(ctx context.Context, input stepInput, item FixItem, commitSHA, explanation string, existing []checkpointResolvedComment) (string, string) {
	if item.ThreadID == "" {
		return "skipped_no_thread", ""
	}
	for _, entry := range existing {
		if entry.FixItemID == item.ID || (entry.ThreadID != "" && entry.ThreadID == item.ThreadID) {
			if entry.ReplyState == "sent" || entry.ReplyState == "skipped_self_author" || entry.ReplyState == "skipped_no_thread" {
				return entry.ReplyState, entry.ReplyError
			}
		}
	}
	body := buildFixerReplyBody(item, commitSHA, explanation)
	existingRemoteReply, err := r.hasExistingFixerReply(ctx, input, item, commitSHA)
	if err != nil {
		return "failed", err.Error()
	}
	if existingRemoteReply {
		return "sent", ""
	}
	if err := r.github.AddReviewThreadReply(ctx, AddReviewThreadReplyInput{Repo: input.Repo, ThreadID: item.ThreadID, Body: body, CWD: input.Project.RepoPath}); err != nil {
		return "failed", err.Error()
	}
	return "sent", ""
}

func (r *Runner) replyToDeclinedComment(ctx context.Context, input stepInput, item FixItem, decisionFingerprint, explanation string, existing []checkpointResolvedComment) (string, string) {
	if item.ThreadID == "" {
		return "skipped_no_thread", ""
	}
	for _, entry := range existing {
		if entry.Action != string(replyActionDeclined) {
			continue
		}
		if entry.FixItemID == item.ID || (entry.ThreadID != "" && entry.ThreadID == item.ThreadID) {
			if entry.ReplyState == "sent" || entry.ReplyState == "skipped_self_author" || entry.ReplyState == "skipped_no_thread" {
				return entry.ReplyState, entry.ReplyError
			}
		}
	}
	body := buildFixerDeclinedReplyBody(item, explanation, decisionFingerprint)
	existingRemoteReply, err := r.hasExistingFixerDeclinedReply(ctx, input, item, decisionFingerprint)
	if err != nil {
		return "failed", err.Error()
	}
	if existingRemoteReply {
		return "sent", ""
	}
	if err := r.github.AddReviewThreadReply(ctx, AddReviewThreadReplyInput{Repo: input.Repo, ThreadID: item.ThreadID, Body: body, CWD: input.Project.RepoPath}); err != nil {
		return "failed", err.Error()
	}
	return "sent", ""
}

func (r *Runner) hasExistingFixerReply(ctx context.Context, input stepInput, item FixItem, commitSHA string) (bool, error) {
	marker := fixerReplyMarker(item.ThreadID, commitSHA)
	if marker == "" {
		return false, nil
	}
	thread, err := r.github.ViewReviewThread(ctx, ViewReviewThreadInput{ThreadID: item.ThreadID, CWD: input.Project.RepoPath})
	if err != nil {
		return false, err
	}
	for _, comment := range thread.Comments {
		if strings.Contains(comment.Body, marker) {
			return true, nil
		}
	}
	return false, nil
}

func (r *Runner) hasExistingFixerDeclinedReply(ctx context.Context, input stepInput, item FixItem, decisionFingerprint string) (bool, error) {
	marker := fixerDeclinedReplyMarker(item.ThreadID, decisionFingerprint)
	if marker == "" {
		return false, nil
	}
	thread, err := r.github.ViewReviewThread(ctx, ViewReviewThreadInput{ThreadID: item.ThreadID, CWD: input.Project.RepoPath})
	if err != nil {
		return false, err
	}
	for _, comment := range thread.Comments {
		if strings.Contains(comment.Body, marker) {
			return true, nil
		}
	}
	return false, nil
}

func (r *Runner) refreshResolveCommentState(ctx context.Context, input stepInput, checkpoint fixerCheckpoint, evidence threadFixEvidence, item FixItem) (string, PullRequestDetail, error) {
	liveDetail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return "", PullRequestDetail{}, err
	}
	refreshCheckpoint := checkpoint
	refreshCheckpoint.Detail = mergeCheckpointDetailPreservingLabels(refreshCheckpoint.Detail, liveDetail)
	refreshCheckpoint.FixItems = collectFixItems(liveDetail)
	refreshCheckpoint.FixItemsHash = hashFixItems(refreshCheckpoint.FixItems)
	verified, err := r.verifyThreadEvidence(ctx, input, refreshCheckpoint, liveDetail, evidence)
	if err != nil {
		return "", PullRequestDetail{}, err
	}
	if !verified {
		return "stale", liveDetail, nil
	}
	for _, liveItem := range refreshCheckpoint.FixItems {
		if strings.TrimSpace(liveItem.ThreadID) != strings.TrimSpace(item.ThreadID) {
			continue
		}
		if !threadFixEvidenceMatchesItem(evidence, liveItem) {
			return "stale", liveDetail, nil
		}
		return "ok", liveDetail, nil
	}
	return "already_resolved", liveDetail, nil
}

// lookupReplyExplanations returns a map of fixItemId → sanitized agent
// explanation. Per-thread drift (a new human comment posted after the
// agent's decision) is detected separately by hasNonLooperCommentSince.
// We deliberately do not invalidate the whole reply set when the
// fix-items hash differs from the snapshot the agent saw: keeping the
// per-item explanations available preserves round summary text and
// evidence records for the threads the agent actually decided about,
// without affecting how new (unknown) threads are handled downstream.
func lookupReplyExplanations(checkpoint fixerCheckpoint) map[string]string {
	if checkpoint.Repair == nil || len(checkpoint.Repair.ReplyExplanations) == 0 {
		return nil
	}
	out := make(map[string]string, len(checkpoint.Repair.ReplyExplanations))
	for _, entry := range checkpoint.Repair.ReplyExplanations {
		if entry.FixItemID == "" || entry.Explanation == "" {
			continue
		}
		out[entry.FixItemID] = entry.Explanation
	}
	return out
}

func normalizeReplyExplanationActions(entries []replyExplanationEntry) []replyExplanationEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]replyExplanationEntry, 0, len(entries))
	for _, entry := range entries {
		entry.Action = canonicalizeReplyAction(entry.Action)
		out = append(out, entry)
	}
	return out
}

func normalizeThreadCommentsObserved(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// agentResolveReplyExplanationsValid reports whether the agent-provided
// reply explanations exist on the checkpoint and can be consulted as the
// per-item reply authority. Per-thread drift (a new human comment posted
// after the agent's decision) is detected separately by
// hasNonLooperCommentSince. We deliberately do not invalidate the whole
// reply set when the fix-items hash differs from the snapshot the agent
// saw: new threads fall through to the contract-violation retry path,
// while previously decided threads continue to be replied/resolved as the
// agent intended.
func agentResolveReplyExplanationsValid(checkpoint fixerCheckpoint) bool {
	if checkpoint.Repair == nil || len(checkpoint.Repair.ReplyExplanations) == 0 {
		return false
	}
	return true
}

func agentResolveRepliesByFixItemID(checkpoint fixerCheckpoint) map[string]replyExplanationEntry {
	if !agentResolveReplyExplanationsValid(checkpoint) {
		return nil
	}
	out := make(map[string]replyExplanationEntry, len(checkpoint.Repair.ReplyExplanations))
	for _, entry := range checkpoint.Repair.ReplyExplanations {
		fixItemID := strings.TrimSpace(entry.FixItemID)
		explanation := strings.TrimSpace(entry.Explanation)
		if fixItemID == "" || explanation == "" {
			continue
		}
		if _, exists := out[fixItemID]; !exists {
			entry.Action = canonicalizeReplyAction(entry.Action)
			out[fixItemID] = entry
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func agentResolveRepliesByThreadID(checkpoint fixerCheckpoint) map[string]replyExplanationEntry {
	if !agentResolveReplyExplanationsValid(checkpoint) {
		return nil
	}
	out := make(map[string]replyExplanationEntry, len(checkpoint.Repair.ReplyExplanations))
	for _, entry := range checkpoint.Repair.ReplyExplanations {
		threadID := strings.TrimSpace(entry.ThreadID)
		explanation := strings.TrimSpace(entry.Explanation)
		if threadID == "" || explanation == "" {
			continue
		}
		if _, exists := out[threadID]; !exists {
			entry.Action = canonicalizeReplyAction(entry.Action)
			out[threadID] = entry
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func threadCommentsObservedDrifted(decision replyExplanationEntry, thread ReviewThread) bool {
	observed := normalizeThreadCommentsObserved(decision.ThreadCommentsObserved)
	if observed == "" {
		return false
	}
	return observed != hashReviewThreadComments(thread)
}

func fixedDecisionMissingThreadSnapshot(decision replyExplanationEntry) bool {
	action := normalizeReplyAction(decision.Action)
	if action == string(replyActionDeclined) {
		return false
	}
	return normalizeThreadCommentsObserved(decision.ThreadCommentsObserved) == ""
}

func hashReviewThreadComments(thread ReviewThread) string {
	type observedThreadComment struct {
		ID        string `json:"id"`
		UpdatedAt string `json:"updatedAt,omitempty"`
	}
	comments := make([]observedThreadComment, 0, len(thread.Comments))
	for _, comment := range thread.Comments {
		if isLooperFixerReplyComment(comment) {
			continue
		}
		id := strings.TrimSpace(comment.ID)
		if id == "" {
			continue
		}
		comments = append(comments, observedThreadComment{
			ID:        id,
			UpdatedAt: strings.TrimSpace(comment.UpdatedAt),
		})
	}
	payload, _ := json.Marshal(comments)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func hasNonLooperCommentSince(thread ReviewThread, rawSince string) bool {
	since := parseRFC3339OrZero(rawSince)
	if since.IsZero() {
		return false
	}
	for _, comment := range thread.Comments {
		createdAt := parseRFC3339OrZero(comment.CreatedAt)
		updatedAt := parseRFC3339OrZero(comment.UpdatedAt)
		latest := createdAt
		if updatedAt.After(latest) {
			latest = updatedAt
		}
		if latest.IsZero() || !latest.After(since) {
			continue
		}
		if isLooperReviewThreadComment(comment) {
			continue
		}
		if isBotReviewThreadComment(comment) {
			continue
		}
		return true
	}
	return false
}

func isLooperReviewThreadComment(comment ReviewThreadComment) bool {
	body := strings.TrimSpace(comment.Body)
	if body == "" {
		return false
	}
	return disclosure.HasMarkdownStamp(body) || strings.Contains(body, "looper-fixer-reply") || strings.Contains(body, "looper:fixer-round")
}

func isLooperFixerReplyComment(comment ReviewThreadComment) bool {
	body := strings.TrimSpace(comment.Body)
	if body == "" {
		return false
	}
	return strings.Contains(body, "looper-fixer-reply") || strings.Contains(body, "looper:fixer-round")
}

// isBotReviewThreadComment reports whether the comment was authored by a
// GitHub bot account (e.g. chatgpt-codex-connector[bot], coderabbitai[bot],
// github-actions[bot]). Bot comments must not be treated as new human
// reviewer feedback for drift detection.
func isBotReviewThreadComment(comment ReviewThreadComment) bool {
	login := strings.ToLower(strings.TrimSpace(comment.Author))
	if login == "" {
		return false
	}
	return strings.HasSuffix(login, "[bot]")
}

func buildFixerReplyBody(item FixItem, commitSHA, explanation string) string {
	var b strings.Builder
	mention := strings.TrimSpace(item.Author)
	if mention != "" {
		b.WriteString("@")
		b.WriteString(mention)
		b.WriteString(" ")
	}
	b.WriteString("This has been fixed")
	if shortSHA := shortCommitSHA(commitSHA); shortSHA != "" {
		b.WriteString(" in ")
		b.WriteString(shortSHA)
	}
	b.WriteString(".")
	if explanation = strings.TrimSpace(explanation); explanation != "" {
		b.WriteString("\n\n")
		b.WriteString(explanation)
	} else if summary := summarizeFixItem(item); summary != "" {
		b.WriteString("\n\n")
		b.WriteString(summary)
	}
	if marker := fixerReplyMarker(item.ThreadID, commitSHA); marker != "" {
		b.WriteString("\n\n")
		b.WriteString(marker)
	}
	return b.String()
}

func buildFixerDeclinedReplyBody(item FixItem, explanation, decisionFingerprint string) string {
	var b strings.Builder
	mention := strings.TrimSpace(item.Author)
	if mention != "" {
		b.WriteString("@")
		b.WriteString(mention)
		b.WriteString(" ")
	}
	b.WriteString("I'm not making a code change for this thread.")
	if explanation = strings.TrimSpace(explanation); explanation != "" {
		b.WriteString("\n\n")
		b.WriteString(explanation)
	}
	if marker := fixerDeclinedReplyMarker(item.ThreadID, decisionFingerprint); marker != "" {
		b.WriteString("\n\n")
		b.WriteString(marker)
	}
	return b.String()
}

func hasSubstantiveDeclineExplanation(explanation string) bool {
	explanation = strings.TrimSpace(explanation)
	if explanation == "" {
		return false
	}
	switch explanation {
	case agentMissingThreadDecisionExplanation, agentInvalidThreadDecisionExplanation, agentDeclinedThreadWithoutReason:
		return false
	default:
		return true
	}
}

func fixerReplyMarker(threadID, commitSHA string) string {
	threadID = strings.TrimSpace(threadID)
	commitSHA = strings.TrimSpace(commitSHA)
	if threadID == "" || commitSHA == "" {
		return ""
	}
	return fmt.Sprintf("<!-- looper-fixer-reply thread:%s commit:%s -->", threadID, commitSHA)
}

func fixerDeclinedReplyMarker(threadID, decisionFingerprint string) string {
	threadID = strings.TrimSpace(threadID)
	decisionFingerprint = strings.TrimSpace(decisionFingerprint)
	if threadID == "" || decisionFingerprint == "" {
		return ""
	}
	return fmt.Sprintf("<!-- looper-fixer-reply-declined thread:%s fingerprint:%s -->", threadID, decisionFingerprint)
}

func summarizeFixItem(item FixItem) string {
	summary := sanitizeReplyExplanation(item.Summary)
	if summary == "" {
		return ""
	}
	if len([]rune(summary)) > 240 {
		runes := []rune(summary)
		summary = strings.TrimSpace(string(runes[:240])) + "…"
	}
	lines := strings.Split(summary, "\n")
	for index, line := range lines {
		lines[index] = "> " + strings.TrimSpace(line)
	}
	return strings.Join(lines, "\n")
}

func shortCommitSHA(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 7 {
		return value
	}
	return value[:7]
}

const fixerSummaryMarkerPrefix = "<!-- looper:fixer-round head="

// fixerRoundSummaryMarker returns a hidden marker keyed by head SHA. The marker
// lets future fixer runs find and edit the existing summary instead of posting
// a duplicate when the same round retries (resume after transient failure,
// scheduler re-claim, etc.).
func fixerRoundSummaryMarker(headSHA string) string {
	if headSHA == "" {
		return ""
	}
	return fixerSummaryMarkerPrefix + headSHA + " -->"
}

// publishRoundSummaryComment posts (or edits) a single PR conversation comment
// summarizing this fixer round: the commit, every fix item with its outcome,
// agent explanations when present, and links to each thread. The summary is
// best-effort and never blocks the resolve step.
func (r *Runner) publishRoundSummaryComment(ctx context.Context, input stepInput, checkpoint *fixerCheckpoint, fixItems []FixItem, commitSHA string, explanationByID map[string]string) {
	headSHA := commitSHA
	if headSHA == "" && checkpoint.ReconcileCommits != nil {
		headSHA = checkpoint.ReconcileCommits.FinalHeadSHA
	}
	if headSHA == "" {
		headSHA = detailHeadSHA(checkpoint.Detail)
	}
	if headSHA == "" {
		return
	}
	evidence := resolveFixEvidence(*checkpoint, input.Loop.MetadataJSON, checkpoint.FixItemsHash)
	if evidence == nil || !evidence.Valid || evidence.HeadSHA == "" {
		return
	}
	if !roundProducedNewCommits(checkpoint) && (checkpoint.ReconcileCommits == nil || evidence.HeadSHA == reconcileBaseHeadSHA(checkpoint.ReconcileCommits)) {
		return
	}
	headSHA = evidence.HeadSHA
	commentItems := summaryCommentItems(fixItems, checkpoint, explanationByID)
	if len(commentItems) == 0 {
		return
	}
	body := buildFixerSummaryCommentBody(input.Repo, input.PRNumber, headSHA, commitSHA, commentItems)
	if checkpoint.SummaryComment != nil && checkpoint.SummaryComment.HeadSHA == headSHA && checkpoint.SummaryComment.CommentID != 0 {
		if err := r.github.UpdateIssueComment(ctx, UpdateIssueCommentInput{Repo: input.Repo, CommentID: checkpoint.SummaryComment.CommentID, Body: body, CWD: input.Project.RepoPath}); err != nil {
			checkpoint.SummaryComment.State = "update_failed"
			checkpoint.SummaryComment.Error = err.Error()
			checkpoint.SummaryComment.UpdatedAt = r.nowISO()
			return
		}
		checkpoint.SummaryComment.State = "updated"
		checkpoint.SummaryComment.Error = ""
		checkpoint.SummaryComment.FixItemsHash = checkpoint.FixItemsHash
		checkpoint.SummaryComment.UpdatedAt = r.nowISO()
		return
	}
	trustedLogin := ""
	if login, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath); err == nil {
		trustedLogin = login
	}
	if existingID, existingURL := findExistingFixerSummaryCommentID(checkpoint.Detail, headSHA, trustedLogin); existingID != 0 {
		if err := r.github.UpdateIssueComment(ctx, UpdateIssueCommentInput{Repo: input.Repo, CommentID: existingID, Body: body, CWD: input.Project.RepoPath}); err != nil {
			checkpoint.SummaryComment = &checkpointSummaryComment{CommentID: existingID, URL: existingURL, HeadSHA: headSHA, FixItemsHash: checkpoint.FixItemsHash, State: "update_failed", Error: err.Error(), UpdatedAt: r.nowISO()}
			return
		}
		checkpoint.SummaryComment = &checkpointSummaryComment{CommentID: existingID, URL: existingURL, HeadSHA: headSHA, FixItemsHash: checkpoint.FixItemsHash, State: "updated", UpdatedAt: r.nowISO()}
		return
	}
	created, err := r.github.CreateIssueComment(ctx, IssueCommentInput{Repo: input.Repo, IssueNumber: input.PRNumber, Body: body, CWD: input.Project.RepoPath})
	if err != nil {
		checkpoint.SummaryComment = &checkpointSummaryComment{HeadSHA: headSHA, FixItemsHash: checkpoint.FixItemsHash, State: "create_failed", Error: err.Error(), UpdatedAt: r.nowISO()}
		return
	}
	checkpoint.SummaryComment = &checkpointSummaryComment{CommentID: created.ID, URL: created.URL, HeadSHA: headSHA, FixItemsHash: checkpoint.FixItemsHash, State: "created", UpdatedAt: r.nowISO()}
	r.appendEvent(ctx, eventInput{eventType: "fixer.summary.posted", projectID: input.Project.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"commentId": created.ID, "url": created.URL, "headSha": headSHA, "items": len(commentItems)}})
}

// roundProducedNewCommits returns true when the current fixer round actually
// pushed at least one new commit. The summary is suppressed for no-op runs so
// PR conversations aren't spammed with empty heartbeats.
func roundProducedNewCommits(checkpoint *fixerCheckpoint) bool {
	if checkpoint == nil || checkpoint.ReconcileCommits == nil {
		return false
	}
	rc := checkpoint.ReconcileCommits
	if len(rc.NewCommitSHAs) > 0 {
		return true
	}
	if rc.FinalHeadSHA != "" && rc.BaseHeadSHA != "" && rc.FinalHeadSHA != rc.BaseHeadSHA {
		return true
	}
	return false
}

// fixerSummaryItem is the per-fix-item view rendered into the summary body.
// Resolved (or already-resolved) comments display as ✅; failed/conflict/check
// items keep their outcome visible so reviewers can see what's still open.
type fixerSummaryItem struct {
	FixItem     FixItem
	Status      string
	Explanation string
	ThreadURL   string
	ReplyState  string
}

func summaryCommentItems(fixItems []FixItem, checkpoint *fixerCheckpoint, explanationByID map[string]string) []fixerSummaryItem {
	resolvedByID := map[string]checkpointResolvedComment{}
	resolvedByThread := map[string]checkpointResolvedComment{}
	if checkpoint.ResolvedComments != nil {
		for _, item := range checkpoint.ResolvedComments.Items {
			if item.FixItemID != "" {
				resolvedByID[item.FixItemID] = item
			}
			if item.ThreadID != "" {
				resolvedByThread[item.ThreadID] = item
			}
		}
	}
	out := make([]fixerSummaryItem, 0, len(fixItems))
	for _, item := range fixItems {
		entry := fixerSummaryItem{FixItem: item, ThreadURL: item.URL, Status: item.Type}
		if item.Type == "comment" {
			entry.Explanation = explanationByID[item.ID]
			resolved, ok := resolvedByID[item.ID]
			if !ok && item.ThreadID != "" {
				resolved, ok = resolvedByThread[item.ThreadID]
			}
			if ok {
				entry.Status = resolved.Status
				entry.ReplyState = resolved.ReplyState
				if entry.Explanation == "" && strings.TrimSpace(resolved.Message) != "" {
					entry.Explanation = resolved.Message
				}
			} else {
				entry.Status = "pending"
			}
		}
		out = append(out, entry)
	}
	return out
}

// buildFixerSummaryCommentBody renders the round summary body. The hidden
// `<!-- looper:fixer-round head=… -->` marker on the first line is what
// findExistingFixerSummaryCommentID looks up for edit-on-retry behavior; the
// adapter still appends the disclosure stamp/footer on top.
func buildFixerSummaryCommentBody(repo string, prNumber int64, headSHA, commitSHA string, items []fixerSummaryItem) string {
	var b strings.Builder
	if marker := fixerRoundSummaryMarker(headSHA); marker != "" {
		b.WriteString(marker)
		b.WriteString("\n")
	}
	b.WriteString("**Looper fixer round complete**")
	if shortSHA := shortCommitSHA(commitSHA); shortSHA != "" {
		b.WriteString(" — ")
		b.WriteString(shortSHA)
	}
	b.WriteString("\n\n")
	for _, item := range items {
		b.WriteString(formatFixerSummaryBullet(item))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatFixerSummaryBullet(item fixerSummaryItem) string {
	icon := summaryStatusIcon(item.Status)
	label := summaryItemLabel(item.FixItem)
	threadURL := item.ThreadURL
	if threadURL == "" {
		threadURL = item.FixItem.URL
	}
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(icon)
	b.WriteString(" ")
	b.WriteString(label)
	if item.FixItem.Author != "" && item.FixItem.Type == "comment" {
		b.WriteString(" (@")
		b.WriteString(item.FixItem.Author)
		b.WriteString(")")
	}
	if threadURL != "" && item.FixItem.Type == "comment" {
		b.WriteString(" — [thread](")
		b.WriteString(threadURL)
		b.WriteString(")")
	}
	if explanation := strings.TrimSpace(item.Explanation); explanation != "" {
		b.WriteString("\n  - ")
		b.WriteString(strings.ReplaceAll(explanation, "\n", "\n    "))
	} else if item.FixItem.Type == "comment" {
		if summary := summarizeFixItem(item.FixItem); summary != "" {
			// summarizeFixItem already prefixes lines with "> "; in the bullet
			// context we want a plain nested bullet for readability.
			plain := strings.ReplaceAll(summary, "> ", "")
			plain = strings.TrimSpace(plain)
			if plain != "" {
				b.WriteString("\n  - ")
				b.WriteString(strings.ReplaceAll(plain, "\n", " "))
			}
		}
	}
	if item.ReplyState != "" && item.ReplyState != "sent" {
		b.WriteString("\n  - reply: ")
		b.WriteString(item.ReplyState)
	}
	return b.String()
}

func summaryItemLabel(item FixItem) string {
	switch item.Type {
	case "comment":
		if item.Path != "" {
			if item.Line > 0 {
				return fmt.Sprintf("Review comment on `%s:%d`", item.Path, item.Line)
			}
			return fmt.Sprintf("Review comment on `%s`", item.Path)
		}
		return "Review comment"
	case "check":
		if item.Name != "" {
			return "Failing check `" + item.Name + "`"
		}
		return "Failing check"
	case "conflict":
		return "Merge conflict"
	default:
		if item.Name != "" {
			return item.Name
		}
		return strings.TrimSpace(item.Type)
	}
}

func summaryStatusIcon(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "resolved", "already_resolved":
		return "✅"
	case "agent_declined":
		return "⏸️"
	case "failed":
		return "⚠️"
	case "pending":
		return "🟡"
	case "conflict":
		return "🔀"
	case "check":
		return "🧪"
	default:
		return "•"
	}
}

// findExistingFixerSummaryCommentID scans the PR's issue comments for a prior
// summary keyed by the same head SHA. Used for edit-on-retry when our local
// checkpoint was wiped (resume across daemon restarts, scheduler re-claim).
func findExistingFixerSummaryCommentID(detail *checkpointDetail, headSHA, trustedLogin string) (int64, string) {
	if detail == nil || headSHA == "" {
		return 0, ""
	}
	marker := fixerRoundSummaryMarker(headSHA)
	if marker == "" {
		return 0, ""
	}
	for _, comment := range detail.IssueComments {
		body, _ := stringFromAny(comment["body"])
		if !strings.Contains(body, marker) {
			continue
		}
		if !isTrustedFixerSummaryComment(comment, trustedLogin, body) {
			continue
		}
		id := issueCommentDatabaseID(comment)
		if id == 0 {
			continue
		}
		url, _ := stringFromAny(comment["url"])
		return id, url
	}
	return 0, ""
}

func isTrustedFixerSummaryComment(comment map[string]any, trustedLogin, _ string) bool {
	return sameGitHubLogin(issueCommentAuthorLogin(comment), trustedLogin)
}

func issueCommentAuthorLogin(comment map[string]any) string {
	for _, key := range []string{"author", "user"} {
		author, _ := comment[key].(map[string]any)
		login, _ := stringFromAny(author["login"])
		if strings.TrimSpace(login) != "" {
			return login
		}
	}
	return ""
}

func issueCommentDatabaseID(comment map[string]any) int64 {
	if id := int64FromAny(comment["id"]); id != 0 {
		return id
	}
	for _, key := range []string{"databaseId", "databaseID", "database_id"} {
		if id := int64FromAny(comment[key]); id != 0 {
			return id
		}
	}
	for _, key := range []string{"url", "html_url", "htmlUrl"} {
		raw, _ := stringFromAny(comment[key])
		if id := issueCommentIDFromURL(raw); id != 0 {
			return id
		}
	}
	return 0
}

func issueCommentIDFromURL(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	if fragment := strings.TrimSpace(parsed.Fragment); fragment != "" {
		if id := int64FromAny(strings.TrimPrefix(fragment, "issuecomment-")); id != 0 {
			return id
		}
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) >= 3 && parts[len(parts)-3] == "issues" && parts[len(parts)-2] == "comments" {
		return int64FromAny(parts[len(parts)-1])
	}
	return 0
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
	verifiedNoPushHead := resolveCommentsVerifiedNoPushHeadSHA(checkpoint.Push, input.Loop.MetadataJSON, checkpoint.FixItemsHash)
	hasVerifiedNoPushHead := verifiedNoPushHead != "" && strings.TrimSpace(detail.HeadSHA) == verifiedNoPushHead
	if shouldBlockResolveWithoutFix(checkpoint, checkpoint.Recheck.RemainingFixItems, hasVerifiedNoPushHead) {
		checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
		return checkpoint, &loopError{message: "resolve-comments left review threads unresolved because fixer produced no new commits to push", kind: FailureManualIntervention}
	}
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
		if latestRun.Status == "running" {
			if err := r.recoverOrphanPreStartRun(ctx, *latestRun); err != nil {
				return resumedRunContext{}, err
			}
			latestRun, err = r.repos.Runs.GetLatestByLoopID(ctx, loop.ID)
			if err != nil {
				return resumedRunContext{}, err
			}
		}
	}
	if latestRun != nil {
		checkpoint = parseCheckpoint(latestRun.CheckpointJSON)
		lastCompleted = asFixerStep(derefString(latestRun.LastCompletedStep))
		failedStep = asFixerStep(derefString(latestRun.CurrentStep))
	}
	restartFromDiscover := false
	resumeFromPrepare := false
	if latestRun != nil {
		failureSummary := firstNonEmpty(derefString(latestRun.Summary), derefString(latestRun.ErrorMessage))
		if strings.TrimSpace(derefString(latestRun.ErrorMessage)) == noopResolveManualIntervention {
			failureSummary = noopResolveManualIntervention
		}
		restartFromDiscover = shouldRestartFromDiscover(latestRun.Status, failedStep, failureSummary) || loops.ShouldRestartFromDiscover(latestRun.Status, checkpoint.ResumePolicy)
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
	initialCheckpoint.RunStartedAt = ""
	initialCheckpoint.RunStartedRunID = ""
	nowISO := r.nowISO()
	run := storage.RunRecord{ID: eventlog.NewEventID("run"), LoopID: loop.ID, Status: "running", CurrentStep: stringPtr(string(startStep)), StartedAt: nowISO, LastHeartbeatAt: stringPtr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO}
	initialCheckpoint.RunPreStartAt = nowISO
	initialCheckpoint.RunPreStartRunID = run.ID
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
		if hasRunning, checkErr := r.repos.Runs.HasRunningByLoopID(ctx, loop.ID); checkErr == nil && hasRunning {
			return resumedRunContext{}, activeFixerRunError(fmt.Sprintf("loop %s already has a running fixer run", loop.ID))
		}
		return resumedRunContext{}, err
	}
	return resumedRunContext{Run: run, StartStep: startStep, Checkpoint: initialCheckpoint, Resumed: resumed}, nil
}

func (r *Runner) recoverOrphanPreStartRun(ctx context.Context, run storage.RunRecord) error {
	if r.repos.AgentExecutions != nil {
		execution, err := r.repos.AgentExecutions.GetLatestActiveByRunID(ctx, run.ID)
		if err != nil {
			return err
		}
		if execution != nil {
			return activeFixerRunError(fmt.Sprintf("loop %s already has a running fixer run %s with agent execution %s", run.LoopID, run.ID, execution.ID))
		}
	}
	checkpoint := parseCheckpoint(run.CheckpointJSON)
	if checkpointStartedCurrentRun(checkpoint, run) {
		return activeFixerRunError(fmt.Sprintf("loop %s already has a running fixer run %s", run.LoopID, run.ID))
	}
	if r.preStartMarkerActive(checkpoint, run) {
		return activeFixerRunError(fmt.Sprintf("loop %s already has a running fixer run %s in pre-start checks", run.LoopID, run.ID))
	}
	if r.markerlessRunningRunActive(checkpoint, run) {
		return activeFixerRunError(fmt.Sprintf("loop %s already has a markerless running fixer run %s", run.LoopID, run.ID))
	}
	if checkpoint.ResumePolicy == "" {
		checkpoint.ResumePolicy = loops.ResumePolicyReplayStep
	}
	_, err := r.completeRun(ctx, run, "interrupted", "Interrupted orphaned fixer run before start", "Interrupted orphaned fixer run before start", checkpoint)
	return err
}

func activeFixerRunError(message string) error {
	return &activeRunError{loopError: &loopError{message: message, kind: FailureRetryableTransient}}
}

type activeRunError struct {
	*loopError
}

func (e *activeRunError) Unwrap() error {
	return e.loopError
}

func checkpointStartedCurrentRun(checkpoint fixerCheckpoint, run storage.RunRecord) bool {
	if checkpoint.RunStartedAt == "" {
		return false
	}
	if checkpoint.RunStartedRunID != "" {
		return checkpoint.RunStartedRunID == run.ID
	}
	return !timestampBefore(checkpoint.RunStartedAt, firstNonEmpty(run.CreatedAt, run.StartedAt))
}

func (r *Runner) preStartMarkerActive(checkpoint fixerCheckpoint, run storage.RunRecord) bool {
	if checkpoint.RunPreStartAt == "" || checkpoint.RunPreStartRunID != run.ID {
		return false
	}
	return timestampWithin(checkpoint.RunPreStartAt, r.now(), r.claimTTL)
}

func (r *Runner) markerlessRunningRunActive(checkpoint fixerCheckpoint, run storage.RunRecord) bool {
	if checkpoint.RunStartedAt != "" || checkpoint.RunStartedRunID != "" || checkpoint.RunPreStartAt != "" || checkpoint.RunPreStartRunID != "" {
		return false
	}
	return timestampWithin(freshestTimestamp(derefString(run.LastHeartbeatAt), run.UpdatedAt, run.StartedAt, run.CreatedAt), r.now(), defaultLegacyMarkerlessRunGrace)
}

func freshestTimestamp(values ...string) string {
	var freshest time.Time
	var freshestRaw string
	for _, raw := range values {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		timestamp, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			continue
		}
		if freshestRaw == "" || timestamp.After(freshest) {
			freshest = timestamp
			freshestRaw = raw
		}
	}
	return freshestRaw
}

func timestampWithin(raw string, now time.Time, ttl time.Duration) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || ttl <= 0 {
		return false
	}
	timestamp, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return false
	}
	return !timestamp.After(now) && now.Sub(timestamp) <= ttl
}

func timestampBefore(raw, floor string) bool {
	raw = strings.TrimSpace(raw)
	floor = strings.TrimSpace(floor)
	if raw == "" || floor == "" {
		return false
	}
	timestamp, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return false
	}
	floorTimestamp, err := time.Parse(time.RFC3339Nano, floor)
	if err != nil {
		return false
	}
	return timestamp.Before(floorTimestamp)
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
	if r.repos == nil || r.repos.Runs == nil || strings.TrimSpace(runID) == "" {
		return nil
	}
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
	record      storage.LoopRecord
	created     bool
	skipped     bool
	availableAt time.Time
}

func (r *Runner) ensureLoopForPullRequest(ctx context.Context, project storage.ProjectRecord, repo string, prNumber int64, headSHA, fixItemsHash, fixItemsStateHash string, fixItems []FixItem, unresolvedThreadIDs []string) (loopUpsertResult, error) {
	nowISO := r.nowISO()
	now := r.now()
	existingLoops, err := r.repos.Loops.List(ctx)
	if err != nil {
		return loopUpsertResult{}, err
	}
	for _, existing := range existingLoops {
		if existing.Type == "fixer" && existing.ProjectID == project.ID && derefString(existing.Repo) == repo && derefInt64(existing.PRNumber) == prNumber {
			if existing.Status == "paused" {
				if resumed, updated, err := r.resumePausedZeroProgressLoop(ctx, existing, headSHA, fixItemsStateHash); err != nil {
					return loopUpsertResult{}, err
				} else if resumed {
					existing = updated
				} else if resumed, updated, err := r.resumePausedNoopResolveLoop(ctx, existing, headSHA, fixItemsStateHash, unresolvedThreadIDs); err != nil {
					return loopUpsertResult{}, err
				} else if resumed {
					existing = updated
				} else if resumed, updated, err := r.resumePausedRiskyConflictLoop(ctx, existing, headSHA, fixItemsStateHash); err != nil {
					return loopUpsertResult{}, err
				} else if !resumed {
					return loopUpsertResult{record: existing, created: false}, nil
				} else {
					existing = updated
				}
			}
			if loops.ShouldSuppressFailedRediscovery(existing.Status, loops.LastFailedDiscoveryFingerprint(existing.MetadataJSON), buildFixerDiscoveryFingerprint(repo, prNumber, headSHA, fixItemsStateHash)) {
				return loopUpsertResult{record: existing, created: false, skipped: true}, nil
			}
			decision := decideRediscoveryAfterNoopResolve(existing, headSHA, fixItemsHash, fixItemsStateHash, fixItems, unresolvedThreadIDs, now)
			if decision.Action == rediscoveryActionSuppress {
				return loopUpsertResult{record: existing, created: false, skipped: true}, nil
			}
			availableAt := now
			if decision.Action == rediscoveryActionDefer {
				availableAt = parseRFC3339OrZero(decision.NextEligibleAt)
				if availableAt.IsZero() {
					availableAt = now
				}
			}
			availableAtISO := eventlog.FormatJavaScriptISOString(availableAt.UTC())
			updated := existing
			if active, err := r.hasActiveRunningRun(ctx, updated.ID); err == nil && active {
				updated.Status = "running"
				updated.NextRunAt = nil
			} else {
				updated.Status = "queued"
				updated.NextRunAt = &availableAtISO
			}
			updated.UpdatedAt = nowISO
			if err := r.repos.Loops.Upsert(ctx, updated); err != nil {
				return loopUpsertResult{}, err
			}
			return loopUpsertResult{record: updated, created: false, availableAt: availableAt}, nil
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
	return loopUpsertResult{record: loop, created: true, availableAt: now}, nil
}

func (r *Runner) resumePausedZeroProgressLoopIfStateChanged(ctx context.Context, projectID, repo string, prNumber int64, headSHA, fixItemsStateHash string) error {
	loop, err := r.findFixerLoopByPR(ctx, projectID, repo, prNumber)
	if err != nil || loop == nil {
		return err
	}
	_, _, err = r.resumePausedZeroProgressLoop(ctx, *loop, headSHA, fixItemsStateHash)
	return err
}

func (r *Runner) resumePausedZeroProgressLoop(ctx context.Context, loop storage.LoopRecord, headSHA, fixItemsStateHash string) (bool, storage.LoopRecord, error) {
	if loop.Status != "paused" {
		return false, loop, nil
	}
	metadata := parseJSONObject(loop.MetadataJSON)
	pauseReason, _ := stringFromAny(metadata["pauseReason"])
	if pauseReason != zeroProgressPauseReason {
		return false, loop, nil
	}
	state, ok := parseZeroProgressState(metadata)
	if !ok {
		return false, loop, nil
	}
	if state.HeadSHA == strings.TrimSpace(headSHA) && state.FixItemsStateHash == strings.TrimSpace(fixItemsStateHash) {
		return false, loop, nil
	}
	updated, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		updated.Status = "queued"
		nextRunAt := r.nowISO()
		updated.NextRunAt = &nextRunAt
	})
	if err != nil {
		return false, storage.LoopRecord{}, err
	}
	updated, err = r.clearZeroProgressMetadata(ctx, updated)
	if err != nil {
		return false, storage.LoopRecord{}, err
	}
	return true, updated, nil
}

func (r *Runner) resumePausedNoopResolveLoop(ctx context.Context, loop storage.LoopRecord, headSHA, fixItemsStateHash string, unresolvedThreadIDs []string) (bool, storage.LoopRecord, error) {
	if loop.Status != "paused" || r.repos == nil || r.repos.Runs == nil {
		return false, loop, nil
	}
	latestRun, err := r.repos.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return false, storage.LoopRecord{}, err
	}
	if latestRun == nil || latestRun.Status != "failed" || strings.TrimSpace(derefString(latestRun.ErrorMessage)) != noopResolveManualIntervention {
		return false, loop, nil
	}
	checkpoint := parseCheckpoint(latestRun.CheckpointJSON)
	previousHeadSHA := ""
	if checkpoint.Detail != nil {
		previousHeadSHA = strings.TrimSpace(checkpoint.Detail.HeadSHA)
	}
	previousStateHash := hashFixItemsState(checkpoint.FixItems)
	if len(checkpoint.FixItems) == 0 {
		previousStateHash = strings.TrimSpace(checkpoint.FixItemsHash)
	}
	previousThreadIDs := unresolvedThreadIDsFromCheckpoint(checkpoint, loop.MetadataJSON, previousHeadSHA)
	if previousHeadSHA == strings.TrimSpace(headSHA) && previousStateHash == strings.TrimSpace(fixItemsStateHash) && sameStringSlices(previousThreadIDs, unresolvedThreadIDs) {
		return false, loop, nil
	}
	updated, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		updated.Status = "queued"
		nextRunAt := r.nowISO()
		updated.NextRunAt = &nextRunAt
	})
	if err != nil {
		return false, storage.LoopRecord{}, err
	}
	return true, updated, nil
}

func (r *Runner) resumePausedRiskyConflictLoop(ctx context.Context, loop storage.LoopRecord, headSHA, fixItemsStateHash string) (bool, storage.LoopRecord, error) {
	if loop.Status != "paused" || r.repos == nil || r.repos.Runs == nil {
		return false, loop, nil
	}
	latestRun, err := r.repos.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return false, storage.LoopRecord{}, err
	}
	if latestRun == nil || latestRun.Status != "failed" || !strings.Contains(strings.TrimSpace(derefString(latestRun.ErrorMessage)), riskyConflictManualHold) {
		return false, loop, nil
	}
	checkpoint := parseCheckpoint(latestRun.CheckpointJSON)
	previousHeadSHA := ""
	if checkpoint.Detail != nil {
		previousHeadSHA = strings.TrimSpace(checkpoint.Detail.HeadSHA)
	}
	previousStateHash := hashFixItemsState(checkpoint.FixItems)
	if len(checkpoint.FixItems) == 0 {
		previousStateHash = strings.TrimSpace(checkpoint.FixItemsHash)
	}
	if previousHeadSHA == strings.TrimSpace(headSHA) && previousStateHash == strings.TrimSpace(fixItemsStateHash) {
		return false, loop, nil
	}
	updated, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		updated.Status = "queued"
		nextRunAt := r.nowISO()
		updated.NextRunAt = &nextRunAt
	})
	if err != nil {
		return false, storage.LoopRecord{}, err
	}
	return true, updated, nil
}

func unresolvedThreadIDsFromCheckpoint(checkpoint fixerCheckpoint, loopMetadataJSON *string, headSHA string) []string {
	if checkpoint.Recheck != nil {
		ids := unresolvedThreadIDs(suppressDeclinedFixItems(loopMetadataJSON, headSHA, checkpoint.Recheck.RemainingFixItems))
		if len(ids) > 0 {
			return ids
		}
	}
	return unresolvedThreadIDs(suppressDeclinedFixItems(loopMetadataJSON, headSHA, checkpoint.FixItems))
}

func (r *Runner) recoverLegacyNoopFollowupLoops(ctx context.Context, project storage.ProjectRecord, repo string, policy DiscoveryPolicy, currentUser string) ([]storage.QueueItemRecord, error) {
	loopsList, err := r.repos.Loops.List(ctx)
	if err != nil {
		return nil, err
	}
	queueItems := make([]storage.QueueItemRecord, 0)
	seenTargets := make(map[string]struct{})
	busyTargets := make(map[string]bool)
	for _, loop := range loopsList {
		if loop.Type != "fixer" || loop.ProjectID != project.ID || derefString(loop.Repo) != repo || loop.PRNumber == nil {
			continue
		}
		targetKey := buildPullRequestTargetID(repo, *loop.PRNumber)
		if _, seen := seenTargets[targetKey]; seen {
			continue
		}
		busy, ok := busyTargets[targetKey]
		if !ok {
			busy, err = r.legacyRecoveryTargetBusy(ctx, loopsList, project.ID, repo, *loop.PRNumber)
			if err != nil {
				return nil, err
			}
			busyTargets[targetKey] = busy
		}
		if busy {
			seenTargets[targetKey] = struct{}{}
			continue
		}
		if loop.Status == "paused" || loop.Status == "running" {
			continue
		}
		if r.hasActivePRLock(ctx, repo, *loop.PRNumber) {
			continue
		}
		metadata := parseJSONObject(loop.MetadataJSON)
		if _, ok := parseFixerFollowupState(metadata); ok {
			continue
		}
		legacy, ok := parseLegacyFixerNoopFollowup(loop, metadata, r.now())
		if !ok {
			continue
		}
		activeQueue, err := r.repos.Queue.FindActiveByLoopID(ctx, loop.ID)
		if err != nil {
			return nil, err
		}
		if activeQueue != nil {
			seenTargets[targetKey] = struct{}{}
			continue
		}
		if activeRun, err := r.hasActiveRunningRun(ctx, loop.ID); err != nil {
			return nil, err
		} else if activeRun {
			seenTargets[targetKey] = struct{}{}
			continue
		}
		detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: repo, PRNumber: *loop.PRNumber, CWD: project.RepoPath})
		if err != nil {
			return nil, err
		}
		if normalizePRState(detail.State) != "open" {
			seenTargets[targetKey] = struct{}{}
			if _, err := r.clearFixerFollowupMetadata(ctx, loop); err != nil {
				return nil, err
			}
			continue
		}
		if (!policy.IncludeDrafts && detail.IsDraft) || normalizePRState(detail.State) != "open" {
			seenTargets[targetKey] = struct{}{}
			continue
		}
		if policy.AuthorFilter != config.FixerAuthorFilterAny && !sameGitHubLogin(detail.Author, currentUser) {
			seenTargets[targetKey] = struct{}{}
			continue
		}
		if !labelsMatch(detail.Labels, policy.Labels, policy.LabelMode) {
			seenTargets[targetKey] = struct{}{}
			continue
		}
		fixItems := collectFixItems(detail)
		threadIDs := unresolvedThreadIDs(fixItems)
		if len(threadIDs) == 0 {
			seenTargets[targetKey] = struct{}{}
			if _, err := r.clearFixerFollowupMetadata(ctx, loop); err != nil {
				return nil, err
			}
			continue
		}
		liveStateHash := hashFixItemsState(fixItems)
		liveFixItemsHash := hashFixItems(fixItems)
		availableAt := r.now()
		updatedLoop := loop
		if detail.HeadSHA == legacy.HeadSHA && (legacy.FixItemsStateHash == liveStateHash || legacy.FixItemsStateHash == liveFixItemsHash) {
			availableAt = legacy.AvailableAt
			followup := fixerFollowupState{
				Reason:                 string(fixerFollowupReasonMissingEvidence),
				HeadSHA:                detail.HeadSHA,
				FixItemsStateHash:      liveStateHash,
				UnresolvedThreadIDs:    threadIDs,
				AttemptsForFingerprint: 1,
				LastAttemptAt:          legacy.LastAttemptAt,
				NextEligibleAt:         eventlog.FormatJavaScriptISOString(availableAt.UTC()),
			}
			updatedLoop, err = r.persistFixerFollowupState(ctx, loop, followup)
			if err != nil {
				return nil, err
			}
		} else {
			updatedLoop, err = r.clearFixerFollowupMetadata(ctx, loop)
			if err != nil {
				return nil, err
			}
		}
		availableAtISO := eventlog.FormatJavaScriptISOString(availableAt.UTC())
		updatedLoop, err = r.updateLoop(ctx, updatedLoop, func(updated *storage.LoopRecord) {
			updated.Status = "queued"
			updated.NextRunAt = &availableAtISO
		})
		if err != nil {
			return nil, err
		}
		queueItem, err := r.enqueue(ctx, enqueueInput{ProjectID: updatedLoop.ProjectID, LoopID: updatedLoop.ID, Repo: repo, PRNumber: *updatedLoop.PRNumber, HeadSHA: detail.HeadSHA, FixItemsHash: liveStateHash, AvailableAt: availableAt})
		if err != nil {
			return nil, err
		}
		seenTargets[targetKey] = struct{}{}
		queueItems = append(queueItems, queueItem)
	}
	return queueItems, nil
}

func (r *Runner) legacyRecoveryTargetBusy(ctx context.Context, loopsList []storage.LoopRecord, projectID, repo string, prNumber int64) (bool, error) {
	for _, loop := range loopsList {
		if loop.Type != "fixer" || loop.ProjectID != projectID || derefString(loop.Repo) != repo || derefInt64(loop.PRNumber) != prNumber {
			continue
		}
		if loop.Status == "paused" || loop.Status == "running" {
			return true, nil
		}
		activeQueue, err := r.repos.Queue.FindActiveByLoopID(ctx, loop.ID)
		if err != nil {
			return false, err
		}
		if activeQueue != nil {
			return true, nil
		}
		activeRun, err := r.hasActiveRunningRun(ctx, loop.ID)
		if err != nil {
			return false, err
		}
		if activeRun {
			return true, nil
		}
	}
	return false, nil
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
	AvailableAt  time.Time
}

func (r *Runner) enqueue(ctx context.Context, input enqueueInput) (storage.QueueItemRecord, error) {
	dedupeKey := buildFixerDedupeKey(input.ProjectID, input.LoopID, input.Repo, input.PRNumber, input.HeadSHA, input.FixItemsHash)
	existing, err := r.repos.Queue.FindActiveByDedupe(ctx, dedupeKey)
	if err != nil {
		return storage.QueueItemRecord{}, err
	}
	availableAt := r.nowISO()
	if !input.AvailableAt.IsZero() {
		availableAt = eventlog.FormatJavaScriptISOString(input.AvailableAt.UTC())
	}
	if existing != nil {
		if existing.Status == "queued" && isoTimeBefore(availableAt, existing.AvailableAt) {
			updated := *existing
			updated.AvailableAt = availableAt
			updated.UpdatedAt = r.nowISO()
			persisted, _, err := r.repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, updated)
			if err != nil {
				return storage.QueueItemRecord{}, err
			}
			updated = persisted
			r.wakeSchedulerAfterEnqueue()
			return updated, nil
		}
		return *existing, nil
	}
	payload := mustMarshalJSON(map[string]any{"discoveryFingerprint": buildFixerDiscoveryFingerprint(input.Repo, input.PRNumber, input.HeadSHA, input.FixItemsHash)})
	activeForLoop, err := r.repos.Queue.FindActiveByLoopID(ctx, input.LoopID)
	if err != nil {
		return storage.QueueItemRecord{}, err
	}
	if activeForLoop != nil {
		if activeForLoop.Status == "queued" {
			updated := *activeForLoop
			updated.DedupeKey = dedupeKey
			updated.AvailableAt = availableAt
			updated.PayloadJSON = &payload
			updated.UpdatedAt = r.nowISO()
			persisted, _, err := r.repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, updated)
			if err != nil {
				return storage.QueueItemRecord{}, err
			}
			updated = persisted
			r.wakeSchedulerAfterEnqueue()
			return updated, nil
		}
		return *activeForLoop, nil
	}
	nowISO := r.nowISO()
	targetID := buildPullRequestTargetID(input.Repo, input.PRNumber)
	lockKey := fmt.Sprintf("pr:%s:%d", input.Repo, input.PRNumber)
	projectID := input.ProjectID
	loopID := input.LoopID
	queueItem := storage.QueueItemRecord{ID: eventlog.NewEventID("queue"), ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: targetID, Repo: &input.Repo, PRNumber: &input.PRNumber, DedupeKey: dedupeKey, Priority: storage.QueuePriorityFixer, Status: "queued", AvailableAt: availableAt, Attempts: 0, MaxAttempts: r.retryMaxAttempts, LockKey: &lockKey, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}
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

func (r *Runner) failQueueItem(ctx context.Context, queueItem storage.QueueItemRecord, kind QueueFailureKind, message string) (*storage.QueueItemRecord, error) {
	nextAttempts := queueItem.Attempts + 1
	return r.requeueOrFailQueueItem(ctx, queueItem, kind, message, nextAttempts)
}

func (r *Runner) requeueQueueItem(ctx context.Context, queueItem storage.QueueItemRecord, kind QueueFailureKind, message string, attempts int64) (*storage.QueueItemRecord, error) {
	nowISO := r.nowISO()
	retryAt := eventlog.FormatJavaScriptISOString(r.now().Add(backoffDelay(r.retryBaseDelay, attempts+1)))
	if err := r.repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: queueItem.ID, AvailableAt: retryAt, Attempts: attempts, ErrorMessage: optionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
		return nil, err
	}
	return r.repos.Queue.GetByID(ctx, queueItem.ID)
}

func (r *Runner) requeueOrFailQueueItem(ctx context.Context, queueItem storage.QueueItemRecord, kind QueueFailureKind, message string, nextAttempts int64) (*storage.QueueItemRecord, error) {
	nowISO := r.nowISO()
	if isRetryableFailure(kind) && nextAttempts < queueItem.MaxAttempts {
		retryAt := eventlog.FormatJavaScriptISOString(r.now().Add(backoffDelay(r.retryBaseDelay, nextAttempts)))
		if err := r.repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: queueItem.ID, AvailableAt: retryAt, Attempts: nextAttempts, ErrorMessage: optionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
			return nil, err
		}
	} else {
		if err := r.repos.Queue.Fail(ctx, storage.QueueFailInput{ID: queueItem.ID, Attempts: nextAttempts, FinishedAt: nowISO, ErrorMessage: optionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
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

func (r *Runner) findFixerLoopByPR(ctx context.Context, projectID, repo string, prNumber int64) (*storage.LoopRecord, error) {
	loops, err := r.repos.Loops.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, loop := range loops {
		if loop.Type == "fixer" && loop.ProjectID == projectID && derefString(loop.Repo) == repo && derefInt64(loop.PRNumber) == prNumber {
			matched := loop
			return &matched, nil
		}
	}
	return nil, nil
}

func loopMetadataForPR(ctx context.Context, runner *Runner, projectID, repo string, prNumber int64) *string {
	if runner == nil {
		return nil
	}
	loop, err := runner.findFixerLoopByPR(ctx, projectID, repo, prNumber)
	if err != nil || loop == nil {
		return nil
	}
	return loop.MetadataJSON
}

func (r *Runner) clearFixerFollowupStateForPR(ctx context.Context, projectID, repo string, prNumber int64) error {
	loop, err := r.findFixerLoopByPR(ctx, projectID, repo, prNumber)
	if err != nil || loop == nil {
		return err
	}
	cleared, err := r.clearFixerFollowupMetadata(ctx, *loop)
	if err != nil {
		return err
	}
	return r.cancelQueuedFixerItemsForLoop(ctx, cleared.ID)
}

func (r *Runner) clearFixerFollowupMetadataForPR(ctx context.Context, projectID, repo string, prNumber int64) error {
	loop, err := r.findFixerLoopByPR(ctx, projectID, repo, prNumber)
	if err != nil || loop == nil {
		return err
	}
	_, err = r.clearFixerFollowupMetadata(ctx, *loop)
	return err
}

func (r *Runner) cancelQueuedFixerItemsForLoop(ctx context.Context, loopID string) error {
	items, err := r.repos.Queue.List(ctx)
	if err != nil {
		return err
	}
	finishedAt := r.nowISO()
	for _, item := range items {
		if derefString(item.LoopID) != loopID || item.Status != "queued" {
			continue
		}
		if err := r.repos.Queue.Complete(ctx, item.ID, finishedAt); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) clearFixerFollowupMetadata(ctx context.Context, loop storage.LoopRecord) (storage.LoopRecord, error) {
	apply := func(updated *storage.LoopRecord) error {
		meta := parseJSONObject(updated.MetadataJSON)
		delete(meta, "fixerFollowup")
		delete(meta, "lastNoopResolveHeadSha")
		delete(meta, "lastNoopResolveFixItemsHash")
		delete(meta, "lastNoopResolveStateHash")
		delete(meta, "lastNoopResolveAt")
		encoded, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		metadataJSON := string(encoded)
		updated.MetadataJSON = &metadataJSON
		return nil
	}
	if r.repos == nil || r.repos.Loops == nil || strings.TrimSpace(loop.ID) == "" {
		updated := loop
		if err := apply(&updated); err != nil {
			return storage.LoopRecord{}, err
		}
		return updated, nil
	}
	var mutateErr error
	updated, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		mutateErr = apply(updated)
	})
	if mutateErr != nil {
		return storage.LoopRecord{}, mutateErr
	}
	return updated, err
}

func (r *Runner) clearZeroProgressMetadata(ctx context.Context, loop storage.LoopRecord) (storage.LoopRecord, error) {
	apply := func(updated *storage.LoopRecord) error {
		meta := parseJSONObject(updated.MetadataJSON)
		delete(meta, "fixerZeroProgress")
		delete(meta, "pauseReason")
		encoded, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		metadataJSON := string(encoded)
		updated.MetadataJSON = &metadataJSON
		return nil
	}
	var mutateErr error
	updated, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		mutateErr = apply(updated)
	})
	if mutateErr != nil {
		return storage.LoopRecord{}, mutateErr
	}
	return updated, err
}

func (r *Runner) persistFixerFollowupState(ctx context.Context, loop storage.LoopRecord, state fixerFollowupState) (storage.LoopRecord, error) {
	apply := func(updated *storage.LoopRecord) error {
		meta := parseJSONObject(updated.MetadataJSON)
		state.UnresolvedThreadIDs = canonicalizeStringSlice(state.UnresolvedThreadIDs)
		meta["fixerFollowup"] = state
		meta["lastNoopResolveHeadSha"] = state.HeadSHA
		meta["lastNoopResolveFixItemsHash"] = state.FixItemsStateHash
		meta["lastNoopResolveStateHash"] = state.FixItemsStateHash
		meta["lastNoopResolveAt"] = state.LastAttemptAt
		encoded, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		metadataJSON := string(encoded)
		updated.MetadataJSON = &metadataJSON
		return nil
	}
	if r.repos == nil || r.repos.Loops == nil || strings.TrimSpace(loop.ID) == "" {
		updated := loop
		if err := apply(&updated); err != nil {
			return storage.LoopRecord{}, err
		}
		return updated, nil
	}
	var mutateErr error
	updated, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		mutateErr = apply(updated)
	})
	if mutateErr != nil {
		return storage.LoopRecord{}, mutateErr
	}
	return updated, err
}

func (r *Runner) recordFixerFollowupState(ctx context.Context, loop storage.LoopRecord, reason fixerFollowupReason, headSHA, fixItemsStateHash string, unresolvedThreadIDs []string, now time.Time) (storage.LoopRecord, error) {
	if r.repos != nil && r.repos.Loops != nil && strings.TrimSpace(loop.ID) != "" {
		if current, err := r.repos.Loops.GetByID(ctx, loop.ID); err != nil {
			return storage.LoopRecord{}, err
		} else if current != nil {
			loop = *current
		}
	}
	meta := parseJSONObject(loop.MetadataJSON)
	previous, ok := parseFixerFollowupState(meta)
	attempts := 1
	if ok && !previous.Terminal && previous.HeadSHA == headSHA && previous.FixItemsStateHash == fixItemsStateHash && sameStringSlices(previous.UnresolvedThreadIDs, unresolvedThreadIDs) {
		attempts = previous.AttemptsForFingerprint + 1
	}
	state := fixerFollowupState{Reason: string(reason), HeadSHA: headSHA, FixItemsStateHash: fixItemsStateHash, UnresolvedThreadIDs: canonicalizeStringSlice(unresolvedThreadIDs), AttemptsForFingerprint: attempts, LastAttemptAt: eventlog.FormatJavaScriptISOString(now.UTC())}
	if attempts > len(fixerFollowupBackoffSchedule) {
		state.Reason = string(fixerFollowupReasonManualIntervention)
		state.Terminal = true
	} else {
		state.NextEligibleAt = eventlog.FormatJavaScriptISOString(now.Add(fixerFollowupBackoffSchedule[attempts-1]).UTC())
	}
	return r.persistFixerFollowupState(ctx, loop, state)
}

func (r *Runner) mergeLoopMetadata(ctx context.Context, loop storage.LoopRecord, updates map[string]any) (storage.LoopRecord, error) {
	if r.repos == nil || r.repos.Loops == nil || strings.TrimSpace(loop.ID) == "" {
		updated := loop
		metadataJSON, err := mergeLoopMetadataJSON(updated.MetadataJSON, updates)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		updated.MetadataJSON = stringPtr(metadataJSON)
		return updated, nil
	}
	current, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return storage.LoopRecord{}, err
	}
	updated := loop
	if current != nil {
		updated = *current
	}
	metadataJSON, err := mergeLoopMetadataJSON(updated.MetadataJSON, updates)
	if err != nil {
		return storage.LoopRecord{}, err
	}
	updated.MetadataJSON = stringPtr(metadataJSON)
	updated.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, updated); err != nil {
		return storage.LoopRecord{}, err
	}
	return updated, nil
}

func (r *Runner) incrementContractViolationCount(ctx context.Context, loop storage.LoopRecord, count int) (storage.LoopRecord, error) {
	metadata := parseJSONObject(loop.MetadataJSON)
	current := int(int64FromAny(metadata["fixerContractViolationCount"]))
	return r.mergeLoopMetadata(ctx, loop, map[string]any{"fixerContractViolationCount": current + count})
}

func (r *Runner) persistDeclinedThreadRecords(ctx context.Context, loop storage.LoopRecord, updates map[string]declinedThreadRecord) (storage.LoopRecord, error) {
	metadata := parseJSONObject(loop.MetadataJSON)
	records := parseDeclinedThreadRecords(metadata)
	if records == nil {
		records = map[string]declinedThreadRecord{}
	}
	for fingerprint, record := range updates {
		records[fingerprint] = record
	}
	records = trimDeclinedThreadRecords(records, maxDeclinedThreadRecords)
	return r.mergeLoopMetadata(ctx, loop, map[string]any{"declinedThreads": records})
}

func (r *Runner) recordZeroProgressSuccess(ctx context.Context, loop storage.LoopRecord, checkpoint fixerCheckpoint) (bool, error) {
	if r.repos != nil && r.repos.Loops != nil && strings.TrimSpace(loop.ID) != "" {
		current, err := r.repos.Loops.GetByID(ctx, loop.ID)
		if err != nil {
			return false, err
		}
		if current != nil {
			loop = *current
		}
	}
	metadata := parseJSONObject(loop.MetadataJSON)
	previous, _ := parseZeroProgressState(metadata)
	current := zeroProgressState{
		HeadSHA:           detailHeadSHA(checkpoint.Detail),
		FixItemsHash:      strings.TrimSpace(checkpoint.FixItemsHash),
		FixItemsStateHash: hashFixItemsState(checkpoint.FixItems),
		ConsecutiveCount:  1,
		RecordedAt:        r.nowISO(),
	}
	if previous.HeadSHA == current.HeadSHA && previous.FixItemsHash == current.FixItemsHash && previous.FixItemsStateHash == current.FixItemsStateHash {
		current.ConsecutiveCount = previous.ConsecutiveCount + 1
	}
	updatedLoop, err := r.mergeLoopMetadata(ctx, loop, map[string]any{"fixerZeroProgress": current})
	if err != nil {
		return false, err
	}
	if current.ConsecutiveCount < 3 {
		_ = updatedLoop
		return false, nil
	}
	updatedLoop, err = r.mergeLoopMetadata(ctx, updatedLoop, map[string]any{"pauseReason": zeroProgressPauseReason})
	if err != nil {
		return false, err
	}
	_, err = r.updateLoop(ctx, updatedLoop, func(updated *storage.LoopRecord) {
		updated.Status = "paused"
		updated.NextRunAt = nil
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func hasProgressed(checkpoint fixerCheckpoint) bool {
	if checkpoint.ReconcileCommits != nil && len(checkpoint.ReconcileCommits.NewCommitSHAs) > 0 {
		return true
	}
	if checkpoint.ResolvedComments == nil {
		return false
	}
	for _, item := range checkpoint.ResolvedComments.Items {
		if item.Status == "resolved" || item.Status == "already_resolved" || item.Status == "agent_declined" || item.Action == string(replyActionFixed) {
			return true
		}
	}
	return false
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
	if githubinfra.IsTransientError(err) {
		return &loopError{message: message, kind: FailureRetryableTransient}
	}
	return &loopError{message: message, kind: FailureNonRetryable}
}

func (r *Runner) reconcileCommits(ctx context.Context, project storage.ProjectRecord, checkpoint fixerCheckpoint, commitMessage string) (fixerCheckpoint, error) {
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
	worktreeRoot, rootErr := fixerWorktreeRoot(project)
	if rootErr != nil {
		return checkpoint, rootErr
	}
	initial, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: baseHeadSHA})
	if err != nil {
		return checkpoint, err
	}
	committedByLoop := false
	if initial.HasUncommittedChanges {
		if !r.allowAutoCommit {
			checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
			return checkpoint, &loopError{message: fmt.Sprintf("Auto commit disabled but fixer worktree has uncommitted changes: %s", firstNonEmpty(strings.Join(initial.ChangedFiles, ", "), "unknown files")), kind: FailureManualIntervention}
		}
		if _, err := r.git.Commit(ctx, CommitInput{RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, Message: commitMessage}); err != nil {
			return checkpoint, err
		}
		committedByLoop = true
	}
	final, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: baseHeadSHA})
	if err != nil {
		return checkpoint, err
	}
	checkpoint.ReconcileCommits = &checkpointReconcileCommits{BaseHeadSHA: baseHeadSHA, FinalHeadSHA: final.HeadSHA, NewCommitSHAs: append([]string(nil), final.NewCommitSHAs...), CommittedByAgent: len(initial.NewCommitSHAs) > 0, CommittedByLoop: committedByLoop, WorkingTreeClean: !final.HasUncommittedChanges, ChangedFiles: append([]string(nil), final.ChangedFiles...), CompletedAt: r.nowISO()}
	checkpoint.ensureLifecycle("fixer", worktree.Branch, "", false)
	checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, final.NewCommitSHAs...)
	if len(final.NewCommitSHAs) > 0 {
		if committedByLoop {
			checkpoint.Lifecycle.Actions.Commit = lifecycle.ActionSourceFallback
		} else if checkpoint.Lifecycle.Actions.Commit == lifecycle.ActionSourceNone {
			checkpoint.Lifecycle.Actions.Commit = lifecycle.ActionSourceAgent
		}
	}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) cleanupFixerWorktreeIfTerminal(ctx context.Context, project storage.ProjectRecord, checkpoint *fixerCheckpoint) {
	if checkpoint == nil || checkpoint.Worktree == nil || checkpoint.Worktree.Path == "" || checkpoint.Worktree.Branch == "" || checkpoint.Worktree.CleanedAt != "" {
		return
	}
	checkpoint.Worktree.CleanupAttemptedAt = r.nowISO()
	worktreeRoot, rootErr := fixerWorktreeRoot(project)
	if rootErr != nil {
		r.logError("fixer worktree cleanup skipped", map[string]any{"projectId": project.ID, "worktreePath": checkpoint.Worktree.Path, "branch": checkpoint.Worktree.Branch, "message": rootErr.Error()})
		return
	}
	if err := r.git.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: project.ID, RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: checkpoint.Worktree.Path, Branch: checkpoint.Worktree.Branch, ProtectedBranches: compactStrings([]string{derefString(project.BaseBranch)})}); err != nil {
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

func (r *Runner) waitForPullRequestHeadSHA(ctx context.Context, input waitForPullRequestHeadSHAInput) (PullRequestDetail, error) {
	actual := ""
	var latest PullRequestDetail
	for attempt := 0; attempt < input.Attempts; attempt++ {
		detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
		if err != nil {
			return PullRequestDetail{}, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		latest = detail
		actual = detail.HeadSHA
		if actual == input.ExpectedHeadSHA {
			return detail, nil
		}
		if attempt < input.Attempts-1 {
			r.sleep(input.Delay)
		}
	}
	return latest, &loopError{message: input.FailureMessage(actual), kind: FailureRetryableAfterResume}
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

func parseDeclinedThreadRecords(metadata map[string]any) map[string]declinedThreadRecord {
	raw, ok := metadata["declinedThreads"]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var records map[string]declinedThreadRecord
	if err := json.Unmarshal(encoded, &records); err != nil {
		return nil
	}
	return records
}

func parseZeroProgressState(metadata map[string]any) (zeroProgressState, bool) {
	raw, ok := metadata["fixerZeroProgress"]
	if !ok || raw == nil {
		return zeroProgressState{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return zeroProgressState{}, false
	}
	var state zeroProgressState
	if err := json.Unmarshal(encoded, &state); err != nil {
		return zeroProgressState{}, false
	}
	if state.HeadSHA == "" && state.FixItemsHash == "" && state.FixItemsStateHash == "" {
		return zeroProgressState{}, false
	}
	return state, true
}

func trimDeclinedThreadRecords(records map[string]declinedThreadRecord, max int) map[string]declinedThreadRecord {
	if len(records) <= max || max <= 0 {
		return records
	}
	type declinedEntry struct {
		fingerprint string
		recordedAt  time.Time
	}
	entries := make([]declinedEntry, 0, len(records))
	for fingerprint, record := range records {
		entries = append(entries, declinedEntry{fingerprint: fingerprint, recordedAt: parseRFC3339OrZero(record.RecordedAt)})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].recordedAt.Equal(entries[j].recordedAt) {
			return entries[i].fingerprint < entries[j].fingerprint
		}
		return entries[i].recordedAt.After(entries[j].recordedAt)
	})
	trimmed := make(map[string]declinedThreadRecord, max)
	for _, entry := range entries[:max] {
		trimmed[entry.fingerprint] = records[entry.fingerprint]
	}
	return trimmed
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
		author, _ := stringFromAny(comment["author"])
		url, _ := stringFromAny(comment["url"])
		path, _ := stringFromAny(comment["path"])
		threadFingerprint, _ := stringFromAny(comment["threadFingerprint"])
		threadFingerprint = normalizeThreadFingerprint(threadFingerprint, threadID, id)
		var line int64
		switch v := comment["line"].(type) {
		case float64:
			line = int64(v)
		case int64:
			line = v
		case int:
			line = int64(v)
		}
		result = append(result, FixItem{Type: "comment", ID: id, ThreadID: threadID, ThreadFingerprint: threadFingerprint, Summary: summary, Author: author, URL: url, Path: path, Line: line})
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
		if item.Type == "comment" {
			item.ThreadFingerprint = ""
		}
		encoded, _ := json.Marshal(item)
		parts = append(parts, string(encoded))
	}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

func hashFixItemsState(items []FixItem) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if item.Type == "comment" {
			item.ThreadFingerprint = strings.TrimSpace(item.ThreadFingerprint)
			if strings.HasPrefix(item.ThreadFingerprint, "legacy:") {
				item.ThreadFingerprint = ""
			}
		}
		encoded, _ := json.Marshal(item)
		parts = append(parts, string(encoded))
	}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

func buildDeclinedThreadFingerprint(item FixItem, headSHA string) string {
	payload := strings.Join([]string{
		strings.TrimSpace(item.ThreadID),
		normalizeThreadFingerprint(item.ThreadFingerprint, item.ThreadID, item.ID),
		strings.TrimSpace(headSHA),
	}, "|")
	sum := sha1.Sum([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func suppressDeclinedFixItems(loopMetadataJSON *string, headSHA string, fixItems []FixItem) []FixItem {
	records := parseDeclinedThreadRecords(parseJSONObject(loopMetadataJSON))
	if len(records) == 0 {
		return fixItems
	}
	filtered := make([]FixItem, 0, len(fixItems))
	for _, item := range fixItems {
		if item.Type == "comment" {
			if _, ok := records[buildDeclinedThreadFingerprint(item, headSHA)]; ok {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func buildFixerMinimalPRSeed(repo string, prNumber int64, detail *checkpointDetail, fixItems []FixItem) string {
	seed := map[string]any{
		"repo":           repo,
		"pr_number":      prNumber,
		"url":            seededPullRequestURL(repo, prNumber),
		"base_ref":       "",
		"head_ref":       "",
		"head_sha":       detailHeadSHA(detail),
		"expected_state": "OPEN",
		"expected_draft": false,
		"task_intent":    "repair_pull_request_feedback",
		"scope": map[string]any{
			"fix_item_ids": fixItemIDs(fixItems),
		},
	}
	if detail != nil {
		seed["base_ref"] = detail.BaseRefName
		seed["head_ref"] = detail.HeadRefName
		seed["expected_state"] = firstNonEmpty(strings.ToUpper(strings.TrimSpace(detail.State)), "OPEN")
		seed["expected_draft"] = detail.IsDraft
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

func fixItemIDs(items []FixItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, firstNonEmpty(item.ID, item.ThreadID, item.Name, item.Type))
	}
	return ids
}

func fixerAgentSideGitHubFetchContract() string {
	return strings.Join([]string{
		"Agent-side GitHub fetch contract: use the minimal PR seed above as the stable handoff. Do not assume full PR diffs, full comment dumps, reviews, checks, or thread state from this prompt are complete or fresh.",
		"Before editing and again before final conclusions or pushing, run `gh pr view <pr-url> -R <repo> --json number,title,body,state,isDraft,baseRefName,headRefName,headRefOid,url,labels` using the seeded PR URL or number plus repository, and validate `headRefOid` equals the seeded `head_sha`, `baseRefName` equals the seeded `base_ref` when present, and state/draft status match the seed. Fail fast on drift.",
		"Fetch scoped data on demand with `gh pr diff <pr-url> -R <repo> --name-only` before selecting files. For relevant file diffs, use a supported workflow such as fetching the full patch with `gh pr diff <pr-url> -R <repo> --patch` and filtering locally, or fetching refs and running `git diff <base>...<head> -- <path>`. Run `gh pr checks <pr-url> -R <repo>` only when CI status matters.",
		"When review feedback context matters, do not rely only on `gh pr view --comments`; collect all review feedback with pagination: `gh api repos/{owner}/{repo}/pulls/{number}/comments --paginate`, `gh api repos/{owner}/{repo}/pulls/{number}/reviews --paginate`, and `gh api repos/{owner}/{repo}/issues/{number}/comments --paginate`.",
		"If `gh` fails for authentication, network, rate-limit, or PR drift reasons, stop and return a structured error with `type` set to one of `auth`, `network`, `rate_limit`, or `pr_drift`, plus a short `message` and any observed PR metadata. Do not proceed on stale PR data.",
	}, "\n")
}

func buildFixerPrompt(projectID string, instructionConfig config.Config, repo string, prNumber int64, detail *checkpointDetail, fixItems []FixItem, allowAutoPush bool, disclosureCfg config.DisclosureConfig, agentRuntime string, agentModel string) (string, config.CustomInstructionBlock) {
	parts := []string{fmt.Sprintf("Fix pull request %s#%d.", repo, prNumber), buildFixerMinimalPRSeed(repo, prNumber, detail, fixItems), fixerAgentSideGitHubFetchContract()}
	if headSHA := detailHeadSHA(detail); headSHA != "" {
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
	)
	if instruction := buildFixerReplyExplanationInstruction(fixItems); instruction != "" {
		parts = append(parts, instruction)
	}
	instructionBlock := config.BuildCustomInstructionBlock(instructionConfig, projectID, "fixer")
	if instructionBlock.Text != "" {
		parts = append(parts, instructionBlock.Text)
	}
	if allowAutoPush {
		parts = append(parts, "Commit and push the repair changes to the current PR branch when you can do so safely; Looper will reconcile any missing repository actions after your edits.")
		parts = append(parts, lifecycle.PromptInstruction("fixer", "", "", true, false, disclosureCfg, agentRuntime, agentModel))
	} else {
		parts = append(parts, "Do not push the branch or update remote pull request state; leave repository publishing for Looper/manual follow-up after your edits.")
		parts = append(parts, noRemoteLifecyclePromptInstruction("fixer", "", "", disclosureCfg, agentRuntime, agentModel))
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

// buildFixerReplyExplanationInstruction returns the prompt fragment that asks
// the agent to provide per-fix-item explanations Looper will use as the body of
// the auto-reply posted before resolving the review thread. The agent supplies
// only the explanation; Looper owns the @mention, commit reference, and
// disclosure stamping.
func buildFixerReplyExplanationInstruction(fixItems []FixItem) string {
	hasComment := false
	for _, item := range fixItems {
		if item.Type == "comment" && item.ID != "" && item.ThreadID != "" {
			hasComment = true
			break
		}
	}
	if !hasComment {
		return ""
	}
	return strings.Join([]string{
		"For EVERY comment-type fix item, you MUST include exactly one entry in a top-level `review_thread_replies` array on the final " + agent.CompletionMarker + " JSON line.",
		"Each entry must be an object with these fields:",
		`  - "fixItemId": the exact "id" of the fix item`,
		`  - "threadId": the exact "threadId" of the same fix item`,
		`  - "action": "fixed" or "declined"`,
		`  - "explanation": one or two sentences (max ~500 chars). If action is "fixed", say what you changed and where. If action is "declined", give a concrete reason why you are not acting. No greetings, no @mentions, no markdown headings, no HTML, no disclosure markers.`,
		`  - "threadCommentsObserved": sha256 of the JSON array of review-thread comments you observed in thread order, where each element is {"id","updatedAt"}. Include target reviewer comments even when they contain a Looper stamp. Exclude only prior Looper fixer replies/round comments.`,
		"Before including an entry, re-read the relevant review thread/comment context.",
		"Use \"fixed\" only when you can confidently confirm the current branch state actually addresses the thread; in other words, only include items you can confidently confirm are actually addressed by the current branch state. Use \"declined\" if you deliberately are not acting, including cases such as: already implemented on this branch, out of scope for this PR, reviewer request is incorrect, or you cannot safely complete it.",
		"Do not omit any comment-type fix item. Do not use vague explanations like \"looks fine\" or \"no change needed\".",
		"Read-only GitHub fetches are allowed for that verification. Do not post replies, resolve threads, submit reviews, edit PR metadata, or perform any other mutating GitHub API action; Looper owns those remote review-state changes after validation and push. Do not invent URLs.",
	}, "\n")
}

func noRemoteLifecyclePromptInstruction(runner, branch, baseBranch string, disclosureCfg config.DisclosureConfig, agentRuntime string, agentModel string) string {
	return strings.Join([]string{
		"Agent-managed git/PR lifecycle policy: remote actions disabled by Looper configuration.",
		"Before finishing: inspect git status, staged and unstaged diffs, untracked files, and recent commit style; commit only relevant non-secret changes if needed; do not push branches, create pull requests, update pull request metadata, or otherwise change remote review state.",
		lifecycle.DisclosurePromptInstruction(runner, disclosureCfg, agentRuntime, agentModel),
		"Because remote PR actions are disabled for this run, do not create or update PR bodies; any PR disclosure stamping can only happen during a later Looper-managed remote reconciliation step.",
		"Include a git_pr_lifecycle object in the final " + "__LOOPER_RESULT__" + " JSON with branch, baseBranch, commitShas, pushed, prNumber, prUrl, prAdopted, and actions {commit,push,pr}; use action source \"agent\" only for local commits you completed and \"none\" for disabled remote actions.",
		fmt.Sprintf("Expected lifecycle runner=%q branch=%q baseBranch=%q expectPush=%t expectPR=%t fallbackAllowed=%t.", runner, branch, baseBranch, false, false, true),
	}, "\n")
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

func fixerWorktreeRoot(project storage.ProjectRecord) (string, error) {
	projectMetadata := parseJSONObject(project.MetadataJSON)
	worktreeRoot, _ := stringFromAny(projectMetadata["worktreeRoot"])
	if worktreeRoot != "" {
		return worktreeRoot, nil
	}
	return config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
}

func (c *fixerCheckpoint) ensureLifecycle(runner, branch, baseBranch string, expectPR bool) {
	if c.Lifecycle == nil {
		c.Lifecycle = lifecycle.NewState(lifecycle.AgentManagedWithFallbackPolicy(runner, expectPR), branch, baseBranch)
		return
	}
	c.Lifecycle.Normalize()
	if c.Lifecycle.Branch == "" {
		c.Lifecycle.Branch = strings.TrimSpace(branch)
	}
	if c.Lifecycle.BaseBranch == "" {
		c.Lifecycle.BaseBranch = strings.TrimSpace(baseBranch)
	}
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
	if failedStep == stepRepair && strings.Contains(failureSummary, riskyConflictManualHold) {
		return true
	}
	if failedStep == stepRecheck && strings.TrimSpace(failureSummary) == noopResolveManualIntervention {
		return true
	}
	if failedStep != stepResolveComments {
		return false
	}
	return status == "interrupted"
}

func shouldRebuildWorktree(checkpoint fixerCheckpoint) bool {
	return checkpoint.Worktree != nil && checkpoint.Worktree.Path != "" && checkpoint.Worktree.PreparedAt == ""
}

func shouldBlockResolveWithoutFix(checkpoint fixerCheckpoint, fixItems []FixItem, hasVerifiedNoPushHead bool) bool {
	if hasVerifiedNoPushHead {
		return false
	}
	if checkpoint.Push == nil || checkpoint.Push.Pushed {
		return false
	}
	if checkpoint.ReconcileCommits == nil {
		return false
	}
	if len(checkpoint.ReconcileCommits.NewCommitSHAs) > 0 {
		return false
	}
	if checkpoint.ReconcileCommits.FinalHeadSHA != "" && checkpoint.ReconcileCommits.BaseHeadSHA != "" && checkpoint.ReconcileCommits.FinalHeadSHA != checkpoint.ReconcileCommits.BaseHeadSHA {
		return false
	}
	for _, item := range fixItems {
		if item.Type == "comment" {
			return true
		}
	}
	return false
}

func skippedFollowupThreadIDs(fixItems []FixItem, resolvedComments []checkpointResolvedComment) ([]string, fixerFollowupReason) {
	threadIDs := make([]string, 0)
	reason := fixerFollowupReason("")
	for _, item := range fixItems {
		if item.Type != "comment" {
			continue
		}
		matched := false
		for _, resolved := range resolvedComments {
			if resolved.FixItemID != item.ID && (resolved.ThreadID == "" || resolved.ThreadID != item.ThreadID) {
				continue
			}
			switch resolved.Status {
			case "skipped_no_evidence":
				matched = true
				reason = fixerFollowupReasonMissingEvidence
			case "skipped_no_confirmation":
				matched = true
				if reason == "" {
					reason = fixerFollowupReasonMissingConfirmation
				}
			}
			break
		}
		if matched {
			threadIDs = append(threadIDs, item.ThreadID)
		}
	}
	return canonicalizeStringSlice(threadIDs), reason
}

func skippedNoEvidenceThreadIDs(fixItems []FixItem, resolvedComments []checkpointResolvedComment) []string {
	threadIDs := make([]string, 0)
	for _, item := range fixItems {
		if item.Type != "comment" {
			continue
		}
		for _, resolved := range resolvedComments {
			if resolved.FixItemID != item.ID && (resolved.ThreadID == "" || resolved.ThreadID != item.ThreadID) {
				continue
			}
			if resolved.Status == "skipped_no_evidence" {
				threadIDs = append(threadIDs, item.ThreadID)
			}
			break
		}
	}
	return canonicalizeStringSlice(threadIDs)
}

func resolveCommentCommitSHA(checkpoint fixerCheckpoint, evidence *fixEvidence, verifiedEvidence bool) string {
	commitSHA := ""
	if checkpoint.ReconcileCommits != nil {
		if len(checkpoint.ReconcileCommits.NewCommitSHAs) > 0 {
			commitSHA = checkpoint.ReconcileCommits.NewCommitSHAs[len(checkpoint.ReconcileCommits.NewCommitSHAs)-1]
		} else {
			commitSHA = checkpoint.ReconcileCommits.FinalHeadSHA
		}
	}
	if commitSHA == "" || (verifiedEvidence && commitSHA == reconcileBaseHeadSHA(checkpoint.ReconcileCommits)) {
		commitSHA = firstNonEmpty(evidenceHeadSHA(evidence), commitSHA)
	}
	return commitSHA
}

func buildResolveReplyExplanations(checkpoint fixerCheckpoint, evidence *fixEvidence) map[string]string {
	out := lookupReplyExplanations(checkpoint)
	for _, record := range evidenceCommentRecords(evidence) {
		if strings.TrimSpace(record.FixItemID) == "" || strings.TrimSpace(record.Explanation) == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		if _, exists := out[record.FixItemID]; !exists {
			out[record.FixItemID] = record.Explanation
		}
	}
	return out
}

func buildThreadResolveReplyExplanations(store *fixEvidenceStoreV2, items []FixItem) map[string]string {
	if store == nil || len(store.Threads) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, item := range items {
		entry, ok := findThreadFixEvidence(store, item)
		if !ok || strings.TrimSpace(item.ID) == "" || strings.TrimSpace(entry.Explanation) == "" {
			continue
		}
		if _, exists := out[item.ID]; !exists {
			out[item.ID] = entry.Explanation
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneFixItems(items []FixItem) []FixItem {
	if len(items) == 0 {
		return nil
	}
	cloned := make([]FixItem, len(items))
	copy(cloned, items)
	for i := range cloned {
		cloned[i].Files = cloneStrings(cloned[i].Files)
	}
	return cloned
}

func evidenceRecordForItem(records map[string]fixCommentEvidence, item FixItem, evidenceFixItemsHash, currentFixItemsHash string) (fixCommentEvidence, bool) {
	if len(records) == 0 {
		return fixCommentEvidence{}, false
	}
	if record, ok := records[strings.TrimSpace(item.ThreadID)]; ok {
		if evidenceRecordMatchesItem(record, item, evidenceFixItemsHash, currentFixItemsHash) {
			return record, true
		}
	}
	if record, ok := records[strings.TrimSpace(item.ID)]; ok {
		if evidenceRecordMatchesItem(record, item, evidenceFixItemsHash, currentFixItemsHash) {
			return record, true
		}
	}
	return fixCommentEvidence{}, false
}

func evidenceRecordMatchesItem(record fixCommentEvidence, item FixItem, evidenceFixItemsHash, currentFixItemsHash string) bool {
	if strings.TrimSpace(record.ThreadID) != "" && strings.TrimSpace(item.ThreadID) != "" && strings.TrimSpace(record.ThreadID) != strings.TrimSpace(item.ThreadID) {
		return false
	}
	if strings.TrimSpace(record.FixItemID) != "" && strings.TrimSpace(item.ID) != "" && strings.TrimSpace(record.FixItemID) != strings.TrimSpace(item.ID) {
		return false
	}
	rawRecordFingerprint := strings.TrimSpace(record.ThreadFingerprint)
	recordFingerprint := normalizeThreadFingerprint(record.ThreadFingerprint, record.ThreadID, record.FixItemID)
	itemFingerprint := normalizeThreadFingerprint(item.ThreadFingerprint, item.ThreadID, item.ID)
	if rawRecordFingerprint == "" {
		latestID := threadFingerprintLatestCommentID(item.ThreadFingerprint)
		if latestID == "" {
			latestID = item.ID
		}
		if latestID == "" || latestID != strings.TrimSpace(record.FixItemID) {
			return false
		}
		if strings.TrimSpace(evidenceFixItemsHash) == "" {
			return false
		}
		if evidenceFixItemsHash == currentFixItemsHash {
			return true
		}
		return evidenceFixItemsHash == hashFixItems([]FixItem{item})
	}
	if itemFingerprint == "" {
		return false
	}
	return recordFingerprint == itemFingerprint
}

func evidenceFixItemsHash(evidence *fixEvidence) string {
	if evidence == nil {
		return ""
	}
	return strings.TrimSpace(evidence.FixItemsHash)
}

func normalizeThreadFingerprint(fingerprint, threadID, itemID string) string {
	if fingerprint = strings.TrimSpace(fingerprint); fingerprint != "" {
		return fingerprint
	}
	threadID = strings.TrimSpace(threadID)
	itemID = strings.TrimSpace(itemID)
	if threadID == "" || itemID == "" {
		return ""
	}
	return fmt.Sprintf("legacy:%s:%s", threadID, itemID)
}

func threadFingerprintLatestCommentID(fingerprint string) string {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return ""
	}
	if strings.HasPrefix(fingerprint, "latest=") {
		parts := strings.Split(fingerprint, "|")
		return strings.TrimPrefix(parts[0], "latest=")
	}
	if strings.Contains(fingerprint, "@") {
		parts := strings.Split(fingerprint, "|")
		last := strings.TrimSpace(parts[len(parts)-1])
		commentParts := strings.SplitN(last, "@", 2)
		return strings.TrimSpace(commentParts[0])
	}
	if strings.HasPrefix(fingerprint, "legacy:") {
		parts := strings.Split(fingerprint, ":")
		if len(parts) == 3 {
			return parts[2]
		}
	}
	return ""
}

func evidenceCommentRecordsByThread(evidence *fixEvidence) map[string]fixCommentEvidence {
	items := evidenceCommentRecords(evidence)
	if len(items) == 0 {
		return nil
	}
	out := make(map[string]fixCommentEvidence, len(items)*2)
	for _, item := range items {
		if key := strings.TrimSpace(item.ThreadID); key != "" {
			out[key] = item
		}
		if key := strings.TrimSpace(item.FixItemID); key != "" {
			if _, exists := out[key]; !exists {
				out[key] = item
			}
		}
	}
	return out
}

func roundEvidenceCommitSHAs(evidence *fixEvidence) []string {
	if evidence == nil {
		return nil
	}
	if len(evidence.CommitSHAs) > 0 {
		return cloneStrings(evidence.CommitSHAs)
	}
	if strings.TrimSpace(evidence.HeadSHA) == "" {
		return nil
	}
	return []string{evidence.HeadSHA}
}

func isSameRoundPushEvidence(evidence *fixEvidence) bool {
	if evidence == nil {
		return false
	}
	switch strings.TrimSpace(evidence.Source) {
	case "fallback_push", "agent_push":
		return true
	default:
		return false
	}
}

func hasCommentFixItems(fixItems []FixItem) bool {
	for _, item := range fixItems {
		if item.Type == "comment" {
			return true
		}
	}
	return false
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

func sameGitHubLogin(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left != "" && right != "" && strings.EqualFold(left, right)
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

func isSpecReviewClean(detail PullRequestDetail) bool {
	return specpr.IsReviewClean(detail.ReviewDecision, detail.Comments)
}

func detailLabels(detail *checkpointDetail) []string {
	if detail == nil {
		return nil
	}
	return detail.Labels
}

func mergeCheckpointDetailPreservingLabels(existing *checkpointDetail, live PullRequestDetail) *checkpointDetail {
	merged := &checkpointDetail{State: live.State, IsDraft: live.IsDraft, Labels: cloneStrings(live.Labels), HeadSHA: live.HeadSHA, HeadRefName: live.HeadRefName, BaseRefName: live.BaseRefName, BaseSHA: live.BaseSHA, ReviewDecision: live.ReviewDecision, Comments: cloneObjectSlice(live.Comments), IssueComments: cloneObjectSlice(live.IssueComments), Checks: cloneObjectSlice(live.Checks), HasConflicts: live.HasConflicts}
	if existing != nil {
		merged.Labels = cloneStrings(existing.Labels)
	}
	return merged
}

func refreshCheckpointHeadAfterPush(checkpoint fixerCheckpoint, live PullRequestDetail) fixerCheckpoint {
	if live.HeadSHA == "" {
		return checkpoint
	}
	checkpoint.Detail = mergeCheckpointDetailPreservingLabels(checkpoint.Detail, live)
	if checkpoint.Worktree != nil {
		checkpoint.Worktree.HeadSHA = live.HeadSHA
	}
	if checkpoint.ReconcileCommits != nil {
		checkpoint.ReconcileCommits.FinalHeadSHA = live.HeadSHA
	}
	return checkpoint
}

func resolveCommentsExpectedHeadSHA(checkpoint fixerCheckpoint) string {
	if checkpoint.Push != nil && checkpoint.Push.Pushed {
		if headSHA := checkpoint.Push.HeadSHA; headSHA != "" {
			return headSHA
		}
	}
	if checkpoint.ReconcileCommits != nil && checkpoint.ReconcileCommits.FinalHeadSHA != "" {
		return checkpoint.ReconcileCommits.FinalHeadSHA
	}
	return detailHeadSHA(checkpoint.Detail)
}

func resolveFixEvidence(checkpoint fixerCheckpoint, loopMetadataJSON *string, fixItemsHash string) *fixEvidence {
	if checkpoint.Push != nil && checkpoint.Push.Evidence != nil {
		evidence := *checkpoint.Push.Evidence
		if evidence.HeadSHA == "" {
			evidence.HeadSHA = checkpoint.Push.HeadSHA
		}
		if evidence.Valid && evidence.HeadSHA != "" && evidence.ProducedNewCommits {
			if len(evidence.CommentRecords) == 0 && canBackfillEvidenceRecords(checkpoint, evidence.FixItemsHash, fixItemsHash) {
				evidence.CommentRecords = buildFixCommentEvidenceRecords(checkpoint, resolveCommentCommitSHA(checkpoint, &evidence, true))
			}
			if !evidenceSafeForCurrentFixItems(&evidence, fixItemsHash) {
				return nil
			}
			return &evidence
		}
		return nil
	}
	if checkpoint.Push != nil && checkpoint.Push.Pushed && checkpoint.Push.HeadSHA != "" {
		producedNewCommits := roundProducedNewCommits(&checkpoint)
		if !producedNewCommits {
			return nil
		}
		evidence := &fixEvidence{Valid: true, Source: "prior_verified_push", HeadSHA: checkpoint.Push.HeadSHA, CommitSHAs: cloneStrings(commitEvidenceSHAs(checkpoint)), FixItemsHash: fixItemsHash, ProducedNewCommits: producedNewCommits, PushedAt: checkpoint.Push.PushedAt}
		if canBackfillEvidenceRecords(checkpoint, fixItemsHash, fixItemsHash) {
			evidence.CommentRecords = buildFixCommentEvidenceRecords(checkpoint, resolveCommentCommitSHA(checkpoint, nil, false))
		}
		if !evidenceSafeForCurrentFixItems(evidence, fixItemsHash) {
			return nil
		}
		return evidence
	}
	if persisted := persistedFixEvidence(loopMetadataJSON); persisted != nil && persisted.Valid && persisted.HeadSHA != "" && persisted.ProducedNewCommits {
		if !evidenceSafeForCurrentFixItems(persisted, fixItemsHash) {
			return nil
		}
		return persisted
	}
	if fixItemsHash == "" {
		return nil
	}
	if headSHA := resolveCommentsVerifiedNoPushHeadSHA(checkpoint.Push, loopMetadataJSON, fixItemsHash); headSHA != "" {
		evidence := &fixEvidence{Valid: true, Source: "prior_verified_push", HeadSHA: headSHA, FixItemsHash: fixItemsHash, ProducedNewCommits: true}
		if canBackfillEvidenceRecords(checkpoint, fixItemsHash, fixItemsHash) {
			evidence.CommentRecords = buildFixCommentEvidenceRecords(checkpoint, resolveCommentCommitSHA(checkpoint, nil, false))
		}
		if !evidenceSafeForCurrentFixItems(evidence, fixItemsHash) {
			return nil
		}
		return evidence
	}
	return nil
}

func buildFixEvidenceStoreV2(checkpoint fixerCheckpoint, evidence *fixEvidence, runID string) *fixEvidenceStoreV2 {
	if evidence == nil || !evidence.Valid || strings.TrimSpace(evidence.HeadSHA) == "" {
		return nil
	}
	store := &fixEvidenceStoreV2{Version: 2, Threads: map[string][]threadFixEvidence{}}
	defaultCommitSHA := resolveCommentCommitSHA(checkpoint, evidence, true)
	explanationByID := buildResolveReplyExplanations(checkpoint, evidence)
	recordsByThread := evidenceCommentRecordsByThread(evidence)
	for _, item := range checkpoint.FixItems {
		if item.Type != "comment" || strings.TrimSpace(item.ThreadID) == "" {
			continue
		}
		record, _ := evidenceRecordForItem(recordsByThread, item, evidenceFixItemsHash(evidence), checkpoint.FixItemsHash)
		entry := threadFixEvidence{
			ThreadID:           item.ThreadID,
			ThreadFingerprint:  normalizeThreadFingerprint(item.ThreadFingerprint, item.ThreadID, item.ID),
			EvidenceHeadSHA:    evidence.HeadSHA,
			CommitSHA:          firstNonEmpty(record.CommitSHA, defaultCommitSHA),
			CommitSHAs:         roundEvidenceCommitSHAs(evidence),
			ValidationHeadSHA:  strings.TrimSpace(checkpoint.Validation.HeadSHA),
			ProducedNewCommits: evidence.ProducedNewCommits,
			FixItemsHash:       evidence.FixItemsHash,
			Source:             evidence.Source,
			RunID:              runID,
			PushedAt:           evidence.PushedAt,
			Explanation:        firstNonEmpty(explanationByID[item.ID], record.Explanation),
			ReplyState:         "pending",
			ResolveState:       "pending",
		}
		store = upsertThreadFixEvidence(store, entry)
	}
	if len(store.Threads) == 0 {
		return nil
	}
	return store
}

func mergeFixEvidenceStoreV2(current, next *fixEvidenceStoreV2) *fixEvidenceStoreV2 {
	if current == nil {
		return cloneFixEvidenceStoreV2(next)
	}
	merged := cloneFixEvidenceStoreV2(current)
	if next == nil {
		return merged
	}
	for _, entries := range next.Threads {
		for _, entry := range entries {
			merged = upsertThreadFixEvidence(merged, entry)
		}
	}
	return merged
}

func cloneFixEvidenceStoreV2(store *fixEvidenceStoreV2) *fixEvidenceStoreV2 {
	if store == nil {
		return nil
	}
	cloned := &fixEvidenceStoreV2{Version: store.Version, Threads: make(map[string][]threadFixEvidence, len(store.Threads))}
	for key, entries := range store.Threads {
		items := make([]threadFixEvidence, len(entries))
		copy(items, entries)
		for i := range items {
			items[i].CommitSHAs = cloneStrings(items[i].CommitSHAs)
		}
		cloned.Threads[key] = items
	}
	return cloned
}

func upsertThreadFixEvidence(store *fixEvidenceStoreV2, next threadFixEvidence) *fixEvidenceStoreV2 {
	if strings.TrimSpace(next.ThreadID) == "" || strings.TrimSpace(next.ThreadFingerprint) == "" {
		return store
	}
	if store == nil {
		store = &fixEvidenceStoreV2{Version: 2, Threads: map[string][]threadFixEvidence{}}
	}
	if store.Threads == nil {
		store.Threads = map[string][]threadFixEvidence{}
	}
	entries := store.Threads[next.ThreadID]
	for i := range entries {
		if entries[i].ThreadFingerprint != next.ThreadFingerprint {
			continue
		}
		if strings.TrimSpace(next.ReplyState) == "pending" && strings.TrimSpace(entries[i].ReplyState) != "" && strings.TrimSpace(entries[i].ReplyState) != "pending" {
			next.ReplyState = entries[i].ReplyState
		}
		if strings.TrimSpace(next.ResolveState) == "pending" && strings.TrimSpace(entries[i].ResolveState) != "" && strings.TrimSpace(entries[i].ResolveState) != "pending" {
			next.ResolveState = entries[i].ResolveState
		}
		if strings.TrimSpace(next.Explanation) == "" {
			next.Explanation = entries[i].Explanation
		}
		if len(next.CommitSHAs) == 0 {
			next.CommitSHAs = cloneStrings(entries[i].CommitSHAs)
		}
		entries[i] = next
		store.Threads[next.ThreadID] = entries
		return store
	}
	store.Threads[next.ThreadID] = append(entries, next)
	return store
}

func findThreadFixEvidence(store *fixEvidenceStoreV2, item FixItem) (threadFixEvidence, bool) {
	if store == nil || len(store.Threads) == 0 {
		return threadFixEvidence{}, false
	}
	threadID := strings.TrimSpace(item.ThreadID)
	if threadID == "" {
		return threadFixEvidence{}, false
	}
	entries := store.Threads[threadID]
	for i := len(entries) - 1; i >= 0; i-- {
		if threadFixEvidenceMatchesItem(entries[i], item) {
			return entries[i], true
		}
	}
	return threadFixEvidence{}, false
}

func threadFixEvidenceMatchesItem(evidence threadFixEvidence, item FixItem) bool {
	if strings.TrimSpace(evidence.ThreadID) == "" || strings.TrimSpace(item.ThreadID) == "" || strings.TrimSpace(evidence.ThreadID) != strings.TrimSpace(item.ThreadID) {
		return false
	}
	recordFingerprint := normalizeThreadFingerprint(evidence.ThreadFingerprint, evidence.ThreadID, item.ID)
	itemFingerprint := normalizeThreadFingerprint(item.ThreadFingerprint, item.ThreadID, item.ID)
	if recordFingerprint == "" || itemFingerprint == "" {
		return false
	}
	return recordFingerprint == itemFingerprint
}

func loadFixEvidenceStoreV2(loopMetadataJSON *string) *fixEvidenceStoreV2 {
	metadata := parseJSONObject(loopMetadataJSON)
	raw, ok := metadata["fixEvidenceStoreV2"]
	if !ok || raw == nil {
		return buildFixEvidenceStoreV2FromPersistedEvidence(loopMetadataJSON)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var store fixEvidenceStoreV2
	if err := json.Unmarshal(encoded, &store); err != nil {
		return nil
	}
	if store.Version == 0 {
		store.Version = 2
	}
	if len(store.Threads) == 0 {
		return buildFixEvidenceStoreV2FromPersistedEvidence(loopMetadataJSON)
	}
	return cloneFixEvidenceStoreV2(&store)
}

func buildFixEvidenceStoreV2FromPersistedEvidence(loopMetadataJSON *string) *fixEvidenceStoreV2 {
	evidence := persistedFixEvidence(loopMetadataJSON)
	if evidence == nil || !evidence.Valid || strings.TrimSpace(evidence.HeadSHA) == "" || len(evidence.CommentRecords) == 0 {
		return nil
	}
	store := &fixEvidenceStoreV2{Version: 2, Threads: map[string][]threadFixEvidence{}}
	for _, record := range evidence.CommentRecords {
		threadID := strings.TrimSpace(record.ThreadID)
		fingerprint := normalizeThreadFingerprint(record.ThreadFingerprint, record.ThreadID, record.FixItemID)
		if threadID == "" || fingerprint == "" {
			continue
		}
		store = upsertThreadFixEvidence(store, threadFixEvidence{
			ThreadID:           threadID,
			ThreadFingerprint:  fingerprint,
			EvidenceHeadSHA:    evidence.HeadSHA,
			CommitSHA:          record.CommitSHA,
			CommitSHAs:         roundEvidenceCommitSHAs(evidence),
			ValidationHeadSHA:  evidence.HeadSHA,
			ProducedNewCommits: evidence.ProducedNewCommits,
			FixItemsHash:       evidence.FixItemsHash,
			Source:             evidence.Source,
			PushedAt:           evidence.PushedAt,
			Explanation:        record.Explanation,
			ReplyState:         "pending",
			ResolveState:       "pending",
		})
	}
	if len(store.Threads) == 0 {
		return nil
	}
	return store
}

func (r *Runner) persistFixEvidenceStoreV2(ctx context.Context, loop storage.LoopRecord, store *fixEvidenceStoreV2) error {
	if store == nil {
		return nil
	}
	merged, err := r.mergedFixEvidenceStoreV2(ctx, loop, store)
	if err != nil {
		return err
	}
	_, err = r.mergeLoopMetadata(ctx, loop, map[string]any{"fixEvidenceStoreV2": merged})
	return err
}

func (r *Runner) mergedFixEvidenceStoreV2(ctx context.Context, loop storage.LoopRecord, next *fixEvidenceStoreV2) (*fixEvidenceStoreV2, error) {
	if next == nil {
		return nil, nil
	}
	currentMetadata := loop.MetadataJSON
	if r.repos != nil && r.repos.Loops != nil && strings.TrimSpace(loop.ID) != "" {
		current, err := r.repos.Loops.GetByID(ctx, loop.ID)
		if err != nil {
			return nil, err
		}
		if current != nil {
			currentMetadata = current.MetadataJSON
		}
	}
	return mergeFixEvidenceStoreV2(loadFixEvidenceStoreV2(currentMetadata), next), nil
}

func evidenceSafeForCurrentFixItems(evidence *fixEvidence, currentFixItemsHash string) bool {
	if evidence == nil {
		return false
	}
	if !evidenceUsesLegacyThreadMatching(evidence) {
		return true
	}
	return strings.TrimSpace(evidence.FixItemsHash) != ""
}

func evidenceUsesLegacyThreadMatching(evidence *fixEvidence) bool {
	for _, record := range evidence.CommentRecords {
		fingerprint := strings.TrimSpace(record.ThreadFingerprint)
		if fingerprint == "" || strings.HasPrefix(fingerprint, "legacy:") {
			return true
		}
	}
	return false
}

func canBackfillEvidenceRecords(checkpoint fixerCheckpoint, storedFixItemsHash, liveFixItemsHash string) bool {
	if strings.TrimSpace(storedFixItemsHash) == "" || strings.TrimSpace(liveFixItemsHash) == "" {
		return false
	}
	if strings.TrimSpace(checkpoint.FixItemsHash) == "" || checkpoint.FixItemsHash != storedFixItemsHash || storedFixItemsHash != liveFixItemsHash {
		return false
	}
	return len(checkpoint.FixItems) > 0
}

func evidenceHeadSHA(evidence *fixEvidence) string {
	if evidence == nil {
		return ""
	}
	return evidence.HeadSHA
}

func resolveCommentsVerifiedNoPushHeadSHA(push *checkpointPush, loopMetadataJSON *string, fixItemsHash string) string {
	if push == nil || push.Pushed || fixItemsHash == "" {
		return ""
	}
	metadata := parseJSONObject(loopMetadataJSON)
	lastFixHeadSHA, _ := stringFromAny(metadata["lastFixHeadSha"])
	lastFixItemsHash, _ := stringFromAny(metadata["lastFixItemsHash"])
	if lastFixHeadSHA == "" || lastFixItemsHash == "" || lastFixItemsHash != fixItemsHash {
		return ""
	}
	return lastFixHeadSHA
}

func persistedFixEvidence(loopMetadataJSON *string) *fixEvidence {
	metadata := parseJSONObject(loopMetadataJSON)
	raw, ok := metadata["lastFixEvidence"]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var evidence fixEvidence
	if err := json.Unmarshal(encoded, &evidence); err != nil {
		return nil
	}
	if evidence.HeadSHA == "" {
		return nil
	}
	if evidence.FixItemsHash == "" {
		evidence.FixItemsHash, _ = stringFromAny(metadata["lastFixItemsHash"])
	}
	evidence.CommentRecords = cloneFixCommentEvidence(evidence.CommentRecords)
	evidence.CommitSHAs = cloneStrings(evidence.CommitSHAs)
	return &evidence
}

func buildFixCommentEvidenceRecords(checkpoint fixerCheckpoint, commitSHA string) []fixCommentEvidence {
	if len(checkpoint.FixItems) == 0 {
		return nil
	}
	explanationByID := lookupReplyExplanations(checkpoint)
	records := make([]fixCommentEvidence, 0, len(checkpoint.FixItems))
	for _, item := range checkpoint.FixItems {
		if item.Type != "comment" {
			continue
		}
		records = append(records, fixCommentEvidence{FixItemID: item.ID, ThreadID: item.ThreadID, ThreadFingerprint: normalizeThreadFingerprint(item.ThreadFingerprint, item.ThreadID, item.ID), CommitSHA: commitSHA, Explanation: explanationByID[item.ID]})
	}
	return records
}

func evidenceCommentRecords(evidence *fixEvidence) []fixCommentEvidence {
	if evidence == nil {
		return nil
	}
	return cloneFixCommentEvidence(evidence.CommentRecords)
}

func commitEvidenceSHAs(checkpoint fixerCheckpoint) []string {
	if checkpoint.ReconcileCommits == nil {
		return nil
	}
	if len(checkpoint.ReconcileCommits.NewCommitSHAs) > 0 {
		return cloneStrings(checkpoint.ReconcileCommits.NewCommitSHAs)
	}
	if checkpoint.ReconcileCommits.FinalHeadSHA != "" {
		return []string{checkpoint.ReconcileCommits.FinalHeadSHA}
	}
	return nil
}

func (r *Runner) verifyFixEvidence(ctx context.Context, input stepInput, checkpoint fixerCheckpoint, evidence *fixEvidence, liveDetail PullRequestDetail) (bool, error) {
	if evidence == nil || !evidence.Valid || strings.TrimSpace(evidence.HeadSHA) == "" {
		return false, nil
	}
	liveHeadSHA := strings.TrimSpace(liveDetail.HeadSHA)
	if liveHeadSHA == "" {
		return false, nil
	}
	if liveHeadSHA == strings.TrimSpace(evidence.HeadSHA) {
		return true, nil
	}
	if r.git == nil {
		return false, nil
	}
	if checkpoint.Worktree != nil && checkpoint.Worktree.Path != "" && checkpoint.Worktree.Branch != "" {
		worktreeRoot, err := fixerWorktreeRoot(input.Project)
		if err != nil {
			return false, err
		}
		if _, err := r.git.PrepareWorktree(ctx, PrepareWorktreeInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: checkpoint.Worktree.Path, Branch: checkpoint.Worktree.Branch, ExpectedHeadSHA: liveHeadSHA}); err != nil {
			return false, err
		}
	} else {
		if branch := strings.TrimSpace(liveDetail.HeadRefName); branch != "" {
			_ = r.git.FetchBranch(ctx, input.Project.RepoPath, "origin", branch)
		}
		_ = r.git.FetchBranch(ctx, input.Project.RepoPath, "origin", liveHeadSHA)
	}
	ancestor, err := r.git.IsAncestor(ctx, input.Project.RepoPath, evidence.HeadSHA, liveHeadSHA)
	if err != nil {
		if shouldTreatMissingGitRevisionAsStale(err) {
			return false, nil
		}
		return false, &loopError{message: fmt.Sprintf("failed to verify fix evidence ancestry: %v", err), kind: FailureRetryableTransient}
	}
	return ancestor, nil
}

func (r *Runner) verifyThreadEvidence(ctx context.Context, input stepInput, checkpoint fixerCheckpoint, liveDetail PullRequestDetail, evidence threadFixEvidence) (bool, error) {
	if !evidence.ProducedNewCommits || strings.TrimSpace(evidence.ThreadID) == "" || strings.TrimSpace(evidence.ThreadFingerprint) == "" || strings.TrimSpace(evidence.EvidenceHeadSHA) == "" {
		return false, nil
	}
	headVerified, err := r.verifyThreadEvidenceHead(ctx, input, checkpoint, liveDetail, evidence.EvidenceHeadSHA)
	if err != nil || !headVerified {
		return headVerified, err
	}
	return r.validationMatchesThreadEvidence(ctx, input, evidence)
}

func (r *Runner) verifyThreadEvidenceHead(ctx context.Context, input stepInput, checkpoint fixerCheckpoint, liveDetail PullRequestDetail, evidenceHeadSHA string) (bool, error) {
	if strings.TrimSpace(evidenceHeadSHA) == "" {
		return false, nil
	}
	return r.verifyFixEvidence(ctx, input, checkpoint, &fixEvidence{Valid: true, HeadSHA: evidenceHeadSHA}, liveDetail)
}

func (r *Runner) validationMatchesThreadEvidence(ctx context.Context, input stepInput, evidence threadFixEvidence) (bool, error) {
	validationHeadSHA := strings.TrimSpace(evidence.ValidationHeadSHA)
	evidenceHeadSHA := strings.TrimSpace(evidence.EvidenceHeadSHA)
	if validationHeadSHA == "" || evidenceHeadSHA == "" {
		return false, nil
	}
	if validationHeadSHA == evidenceHeadSHA {
		return true, nil
	}
	if r.git == nil {
		return false, nil
	}
	ancestor, err := r.git.IsAncestor(ctx, input.Project.RepoPath, evidenceHeadSHA, validationHeadSHA)
	if err != nil {
		if shouldTreatMissingGitRevisionAsStale(err) {
			return false, nil
		}
		return false, &loopError{message: fmt.Sprintf("failed to verify validation ancestry: %v", err), kind: FailureRetryableTransient}
	}
	return ancestor, nil
}

func (r *Runner) validationMatchesEvidence(ctx context.Context, input stepInput, checkpoint fixerCheckpoint, evidence *fixEvidence) (bool, error) {
	if checkpoint.Validation == nil || !checkpoint.Validation.Passed || evidence == nil || strings.TrimSpace(evidence.HeadSHA) == "" {
		return false, nil
	}
	validationHeadSHA := strings.TrimSpace(checkpoint.Validation.HeadSHA)
	if validationHeadSHA == "" {
		return false, nil
	}
	if validationHeadSHA == strings.TrimSpace(evidence.HeadSHA) {
		return true, nil
	}
	if r.git == nil {
		return false, nil
	}
	ancestor, err := r.git.IsAncestor(ctx, input.Project.RepoPath, evidence.HeadSHA, validationHeadSHA)
	if err != nil {
		if shouldTreatMissingGitRevisionAsStale(err) {
			return false, nil
		}
		return false, &loopError{message: fmt.Sprintf("failed to verify validation ancestry: %v", err), kind: FailureRetryableTransient}
	}
	return ancestor, nil
}

func shouldTreatMissingGitRevisionAsStale(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"unknown revision",
		"bad revision",
		"not a valid object name",
		"not a valid commit name",
		"unknown commit or path",
		"ambiguous argument",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func decideRediscoveryAfterNoopResolve(loop storage.LoopRecord, headSHA, fixItemsHash, fixItemsStateHash string, fixItems []FixItem, unresolvedThreadIDs []string, now time.Time) rediscoveryDecision {
	if headSHA == "" || fixItemsStateHash == "" || len(unresolvedThreadIDs) == 0 {
		return rediscoveryDecision{Action: rediscoveryActionEnqueue}
	}
	metadata := parseJSONObject(loop.MetadataJSON)
	hasRecoverableEvidence := hasRecoverableThreadEvidence(loop.MetadataJSON, fixItems)
	if followup, ok := parseFixerFollowupState(metadata); ok {
		if followup.HeadSHA != headSHA || followup.FixItemsStateHash != fixItemsStateHash || !sameStringSlices(followup.UnresolvedThreadIDs, unresolvedThreadIDs) {
			return rediscoveryDecision{Action: rediscoveryActionEnqueue}
		}
		if hasRecoverableEvidence {
			return rediscoveryDecision{Action: rediscoveryActionEnqueue}
		}
		if followup.Terminal {
			return rediscoveryDecision{Action: rediscoveryActionSuppress, Reason: followup.Reason}
		}
		if nextEligibleAt := parseRFC3339OrZero(followup.NextEligibleAt); !nextEligibleAt.IsZero() && now.Before(nextEligibleAt) {
			return rediscoveryDecision{Action: rediscoveryActionDefer, Reason: followup.Reason, NextEligibleAt: followup.NextEligibleAt}
		}
		return rediscoveryDecision{Action: rediscoveryActionEnqueue}
	}
	legacyHeadSHA, _ := stringFromAny(metadata["lastNoopResolveHeadSha"])
	legacyStateHash, _ := stringFromAny(metadata["lastNoopResolveStateHash"])
	if legacyStateHash == "" {
		legacyStateHash, _ = stringFromAny(metadata["lastNoopResolveFixItemsHash"])
	}
	if legacyHeadSHA == "" || legacyStateHash == "" || legacyHeadSHA != headSHA || (legacyStateHash != fixItemsStateHash && legacyStateHash != fixItemsHash) {
		return rediscoveryDecision{Action: rediscoveryActionEnqueue}
	}
	if hasRecoverableEvidence {
		return rediscoveryDecision{Action: rediscoveryActionEnqueue}
	}
	lastAttemptAt, _ := stringFromAny(metadata["lastNoopResolveAt"])
	if strings.TrimSpace(lastAttemptAt) == "" {
		lastAttemptAt = loop.UpdatedAt
	}
	lastAttempt := parseRFC3339OrZero(lastAttemptAt)
	if lastAttempt.IsZero() {
		return rediscoveryDecision{Action: rediscoveryActionEnqueue}
	}
	nextEligibleAt := lastAttempt.Add(fixerFollowupBackoffSchedule[0])
	if now.Before(nextEligibleAt) {
		return rediscoveryDecision{Action: rediscoveryActionDefer, Reason: string(fixerFollowupReasonMissingEvidence), NextEligibleAt: eventlog.FormatJavaScriptISOString(nextEligibleAt.UTC())}
	}
	return rediscoveryDecision{Action: rediscoveryActionEnqueue}
}

type legacyFixerNoopFollowup struct {
	HeadSHA           string
	FixItemsStateHash string
	LastAttemptAt     string
	AvailableAt       time.Time
}

func parseLegacyFixerNoopFollowup(loop storage.LoopRecord, metadata map[string]any, now time.Time) (legacyFixerNoopFollowup, bool) {
	legacyHeadSHA, _ := stringFromAny(metadata["lastNoopResolveHeadSha"])
	legacyStateHash, _ := stringFromAny(metadata["lastNoopResolveStateHash"])
	if legacyStateHash == "" {
		legacyStateHash, _ = stringFromAny(metadata["lastNoopResolveFixItemsHash"])
	}
	if legacyHeadSHA == "" || legacyStateHash == "" {
		return legacyFixerNoopFollowup{}, false
	}
	lastAttemptAt, _ := stringFromAny(metadata["lastNoopResolveAt"])
	if strings.TrimSpace(lastAttemptAt) == "" {
		lastAttemptAt = loop.UpdatedAt
	}
	lastAttempt := parseRFC3339OrZero(lastAttemptAt)
	if lastAttempt.IsZero() {
		lastAttempt = now
		lastAttemptAt = eventlog.FormatJavaScriptISOString(now.UTC())
	}
	availableAt := lastAttempt.Add(fixerFollowupBackoffSchedule[0])
	if !now.Before(availableAt) {
		availableAt = now
	}
	return legacyFixerNoopFollowup{HeadSHA: legacyHeadSHA, FixItemsStateHash: legacyStateHash, LastAttemptAt: lastAttemptAt, AvailableAt: availableAt}, true
}

func hasRecoverableThreadEvidence(loopMetadataJSON *string, fixItems []FixItem) bool {
	if len(fixItems) == 0 {
		return false
	}
	store := loadFixEvidenceStoreV2(loopMetadataJSON)
	if store == nil || len(store.Threads) == 0 {
		return false
	}
	for _, item := range fixItems {
		if item.Type != "comment" {
			continue
		}
		entries := store.Threads[strings.TrimSpace(item.ThreadID)]
		for _, entry := range entries {
			if !threadFixEvidenceMatchesItem(entry, item) {
				continue
			}
			if !entry.ProducedNewCommits || strings.TrimSpace(entry.EvidenceHeadSHA) == "" || strings.TrimSpace(entry.ValidationHeadSHA) == "" {
				continue
			}
			if strings.TrimSpace(entry.Explanation) == "" {
				continue
			}
			if entry.ResolveState == "resolved" || entry.ResolveState == "already_resolved" {
				continue
			}
			return true
		}
	}
	return false
}

func parseFixerFollowupState(metadata map[string]any) (fixerFollowupState, bool) {
	raw, ok := metadata["fixerFollowup"]
	if !ok {
		return fixerFollowupState{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return fixerFollowupState{}, false
	}
	var state fixerFollowupState
	if err := json.Unmarshal(encoded, &state); err != nil {
		return fixerFollowupState{}, false
	}
	state.UnresolvedThreadIDs = canonicalizeStringSlice(state.UnresolvedThreadIDs)
	return state, state.HeadSHA != "" && state.FixItemsStateHash != ""
}

func unresolvedThreadIDs(fixItems []FixItem) []string {
	threadIDs := make([]string, 0)
	for _, item := range fixItems {
		if item.Type != "comment" {
			continue
		}
		threadIDs = append(threadIDs, item.ThreadID)
	}
	return canonicalizeStringSlice(threadIDs)
}

func canonicalizeStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	if len(result) == 0 {
		return nil
	}
	return result
}

func sameStringSlices(left, right []string) bool {
	left = canonicalizeStringSlice(left)
	right = canonicalizeStringSlice(right)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func parseRFC3339OrZero(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func isoTimeBefore(candidate, current string) bool {
	parsedCandidate := parseRFC3339OrZero(candidate)
	parsedCurrent := parseRFC3339OrZero(current)
	if parsedCandidate.IsZero() || parsedCurrent.IsZero() {
		return false
	}
	return parsedCandidate.Before(parsedCurrent)
}

func buildFixerDiscoveryFingerprint(repo string, prNumber int64, headSHA, fixItemsHash string) string {
	if headSHA == "" {
		headSHA = "unknown"
	}
	return loops.ComputeDiscoveryFingerprint("fixer", repo, fmt.Sprintf("%d", prNumber), headSHA, fixItemsHash)
}

func stampFixerFailedDiscoveryFingerprint(updated *storage.LoopRecord, queueItem storage.QueueItemRecord) {
	metadata := parseJSONObject(queueItem.PayloadJSON)
	fingerprint, _ := stringFromAny(metadata["discoveryFingerprint"])
	if strings.TrimSpace(fingerprint) == "" {
		return
	}
	merged, err := loops.MergeLastFailedDiscoveryFingerprint(updated.MetadataJSON, fingerprint)
	if err != nil {
		return
	}
	updated.MetadataJSON = stringPtr(merged)
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
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

func cloneFixCommentEvidence(values []fixCommentEvidence) []fixCommentEvidence {
	if len(values) == 0 {
		return nil
	}
	return append([]fixCommentEvidence(nil), values...)
}

func cloneObjectSlice(values []map[string]any) []map[string]any {
	if values == nil {
		return nil
	}
	cloned := make([]map[string]any, 0, len(values))
	for _, value := range values {
		item := make(map[string]any, len(value))
		for key, element := range value {
			item[key] = element
		}
		cloned = append(cloned, item)
	}
	return cloned
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
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

func appendUniqueStrings(dst []string, values ...string) []string {
	seen := map[string]bool{}
	for _, value := range dst {
		seen[value] = true
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		dst = append(dst, value)
	}
	return dst
}

func stringFromAny(value any) (string, bool) {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

func lastNonEmptyString(values []string, fallback string) string {
	for i := len(values) - 1; i >= 0; i-- {
		if strings.TrimSpace(values[i]) != "" {
			return values[i]
		}
	}
	return fallback
}

func int64FromAny(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err == nil {
			return parsed
		}
	}
	return 0
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
