package planner

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	htmlpkg "html"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/infra/specpr"
	"github.com/nexu-io/looper/internal/lifecycle"
	"github.com/nexu-io/looper/internal/loops"
	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
	"github.com/nexu-io/looper/internal/loops/failureclass"
	"github.com/nexu-io/looper/internal/planner/decisions"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

const (
	stepDiscoverIssues           PlannerStep = "discover-issues"
	stepPrepareWorktree          PlannerStep = "prepare-worktree"
	stepAuthorDecisionBrief      PlannerStep = "author-decision-brief"
	stepGrillProductDecisions    PlannerStep = "grill-product-decisions"
	stepRouteProductDecisions    PlannerStep = "route-product-decisions"
	stepGrillDownstreamDecisions PlannerStep = "grill-downstream-decisions"
	stepRouteDownstreamDecisions PlannerStep = "route-downstream-decisions"
	stepGrillFinalDecisions      PlannerStep = "grill-final-decisions"
	stepWriteSpec                PlannerStep = "write-spec"
	stepPublish                  PlannerStep = "publish"
	// Plane node H: grill (fresh adversarial pass) → review (independent verdict). Both
	// are no-ops for GitHub/forgejo projects (their spec is the PR, reviewed there).
	stepGrill  PlannerStep = "grill"
	stepReview PlannerStep = "review"
	stepNotify PlannerStep = "notify"

	discoveryLabel             = "looper:plan"
	plannerPRDedupeLookupLimit = 1000

	defaultAgentTimeout = 30 * time.Minute
	defaultClaimTTL     = 10 * time.Minute
	defaultRetryDelay   = 5 * time.Second
	maxRetryDelay       = 300 * time.Second
	defaultRetryMax     = 3
	defaultIssueLimit   = 30

	awaitingProductDecisionSkipReason = "awaiting product decision on Plane"
)

var plannerStepSequence = []PlannerStep{stepDiscoverIssues, stepPrepareWorktree, stepWriteSpec, stepPublish, stepGrill, stepReview, stepNotify}

var plannerV2StepSequence = []PlannerStep{
	stepDiscoverIssues,
	stepPrepareWorktree,
	stepAuthorDecisionBrief,
	stepGrillProductDecisions,
	stepRouteProductDecisions,
	stepGrillDownstreamDecisions,
	stepRouteDownstreamDecisions,
	stepGrillFinalDecisions,
	stepWriteSpec,
	stepPublish,
	stepGrill,
	stepReview,
	stepNotify,
}

type PlannerStep string

type QueueFailureKind = failureclass.Kind

const (
	FailureRetryableTransient   = failureclass.RetryableTransient
	FailureRetryableAfterResume = failureclass.RetryableAfterResume
	FailureRecoverableInfra     = failureclass.RecoverableInfra
	FailureNonRetryable         = failureclass.NonRetryable
	FailureManualIntervention   = failureclass.ManualIntervention
	// FailureHITLInterrupt marks a step whose agent was deliberately killed to answer a
	// human's mid-run @bot (强操控). It is NOT a real failure: the run completes as
	// "interrupted" and is re-dispatched immediately so follow-up-resume answers them
	// from the MAIN session, without counting against the retry cap.
	FailureHITLInterrupt = failureclass.HITLInterrupt
)

// plannerInterruptError signals that the step's agent was killed by a human mid-run
// @bot (result.Interrupted). handled specially in the run loop (interrupted, not failed).
func plannerInterruptError() *loopError {
	return &loopError{message: "agent interrupted by a human mid-run message", kind: FailureHITLInterrupt}
}

type IssueSummary struct {
	Number           int64
	Title            string
	Body             string
	URL              string
	Assignees        []string
	Labels           []string
	StrictDispatchID string
}

type IssueDetail struct {
	Number    int64
	Title     string
	Body      string
	URL       string
	Assignees []string
	Labels    []string
}

type PullRequestSummary struct {
	Number      int64
	URL         string
	State       string
	HeadRefName string
	BaseRefName string
}

type ListOpenPullRequestsInput struct {
	Repo  string
	CWD   string
	Limit int
}

type ListOpenIssuesInput struct {
	Repo     string
	CWD      string
	Limit    int
	Assignee string
	Label    string
	Labels   []string
}

type ViewIssueInput struct {
	Repo        string
	IssueNumber int64
	CWD         string
}

type CreatePullRequestInput struct {
	Repo            string
	HeadBranch      string
	BaseBranch      string
	Title           string
	Body            string
	CWD             string
	DisclosureAgent string
	DisclosureModel string
}

type CreatePullRequestResult struct {
	Number int64
	URL    string
}

type ViewPullRequestInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type PullRequestDetail struct {
	Number      int64
	Title       string
	Body        string
	URL         string
	State       string
	Labels      []string
	HeadRefName string
	BaseRefName string
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

type UpdatePullRequestBodyInput struct {
	Repo            string
	PRNumber        int64
	Body            string
	CWD             string
	DisclosureAgent string
	DisclosureModel string
}

type IssueAssigneesInput struct {
	Repo        string
	IssueNumber int64
	Assignees   []string
	CWD         string
}

type GitHubGateway interface {
	ListOpenIssues(context.Context, ListOpenIssuesInput) ([]IssueSummary, error)
	ViewIssue(context.Context, ViewIssueInput) (IssueDetail, error)
	GetCurrentUserLogin(context.Context, string) (string, error)
	AddIssueAssignees(context.Context, IssueAssigneesInput) error
	ListOpenPullRequests(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error)
	ViewPullRequest(context.Context, ViewPullRequestInput) (PullRequestDetail, error)
	CreatePullRequest(context.Context, CreatePullRequestInput) (CreatePullRequestResult, error)
	UpdatePullRequestBody(context.Context, UpdatePullRequestBodyInput) error
	AddPullRequestLabels(context.Context, PullRequestLabelsInput) error
	AddPullRequestReviewers(context.Context, PullRequestReviewersInput) error
	ClosePullRequest(context.Context, ClosePullRequestInput) error
}

type strictDispatchGateway interface {
	TransitionStrictDispatch(context.Context, StrictDispatchTransitionInput) error
	CreateStrictRoleRequest(context.Context, StrictRoleRequestInput) (StrictRoleRequestResult, error)
}

type StrictDispatchTransitionInput struct {
	Repo       string
	CWD        string
	DispatchID string
	State      string
	WaitKind   *string
}

type StrictRoleRequestInput struct {
	Repo             string
	CWD              string
	DispatchID       string
	LoopID           string
	DecisionRevision int
	Role             decisions.Role
	BriefSummary     string
	Questions        []decisions.Question
}

type StrictRoleRequestResult struct {
	RoleRequestID    string
	CommentID        string
	CreatedAt        string
	EligibleMemberID string
}

// ClosePullRequestInput closes a pull request (used to retire a stray PR an agent
// opened on a Plane project, where the spec lives on a Plane page not a PR).
type ClosePullRequestInput struct {
	Repo     string
	PRNumber int64
	CWD      string
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
	RepoPath          string
	WorktreeRoot      string
	WorktreePath      string
	Branch            string
	Remote            string
	ProtectedBranches []string
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
	CommittedChangedFiles []string
	HasUncommittedChanges bool
	ChangedFiles          []string
}

type CommitInput struct {
	RepoPath        string
	WorktreeRoot    string
	WorktreePath    string
	Message         string
	DisclosureAgent string
	DisclosureModel string
}

type CommitResult struct{ CommitSHA string }

type GitGateway interface {
	CreateWorktree(context.Context, CreateWorktreeInput) (CreateWorktreeResult, error)
	InspectHead(context.Context, InspectHeadInput) (InspectHeadResult, error)
	Commit(context.Context, CommitInput) (CommitResult, error)
	Push(context.Context, PushInput) error
}

type AgentRunInput struct {
	ExecutionID string
	ProjectID   string
	LoopID      string
	RunID       string
	Prompt      string
	// NativeResumePrompt + NativeSessionID drive native agent-session resume: when a
	// finished planner loop is reactivated for a follow-up turn (线程永远可追问), the
	// write-spec step resumes the SAME planner session so it retains the full
	// spec-authoring history and revises the spec incrementally rather than replanning.
	NativeResumePrompt string
	NativeSessionID    string
	WorkingDirectory   string
	Timeout            time.Duration
	HeartbeatTimeout   time.Duration
	Metadata           map[string]any
	IdempotencyKey     string
	// UseSnapshot + SnapshotVendor/Model override the executor config for this
	// start when the run has a durable agent snapshot (execution authority).
	UseSnapshot    bool
	SnapshotVendor string
	SnapshotModel  *string
}

type AgentResult struct {
	Status                       string
	Summary                      string
	ProductAsk                   string
	Stdout                       string
	Stderr                       string
	Commits                      []string
	Lifecycle                    *lifecycle.State
	TimeoutType                  string
	ConfiguredIdleTimeoutSeconds int64
	ConfiguredMaxRuntimeSeconds  int64
	ElapsedRuntimeSeconds        int64
	LastProgressAt               string
	// Interrupted is true when the agent was killed by a human's mid-run @bot (强操控),
	// so the step returns an interrupt (not a failure) and the run re-dispatches to
	// answer them from the same session.
	Interrupted bool
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
	AgentIdleTimeout        time.Duration
	ClaimTTL                time.Duration
	AllowAutoPush           *bool
	Disclosure              *config.DisclosureConfig
	AgentRuntime            string
	AgentProfileID          string
	CustomInstructions      *config.Config
	AgentModel              *string
	RetryBaseDelay          time.Duration
	RetryMaxAttempts        int64
	OnAgentExecutionStarted AgentExecutionStartedFunc
	OnQueueItemEnqueued     func()
	// PostThreadNote posts a plain-text reply into a loop's Feishu thread (node H
	// touchpoints: spec-draft FYI, grill transcript), optionally @-mentioning open_ids.
	PostThreadNote         func(ctx context.Context, loopID, text string, mentionOpenIDs []string) error
	PostThreadNoteWithUUID func(ctx context.Context, loopID, text string, mentionOpenIDs []string, uuid string) error
	// PostThreadCard posts a header-less interactive card into a loop's Feishu thread —
	// used for the node-H product-decision ask so the product-language body renders with
	// structure (bold sub-headers, line breaks) instead of a flat text wall. Falls back
	// to PostThreadNote when unset (e.g. tests).
	PostThreadCard func(ctx context.Context, loopID, body string, mentionOpenIDs []string) error
	// PostThreadImage uploads and posts one PNG with a stable Feishu message UUID.
	PostThreadImage func(ctx context.Context, loopID, pngPath, dedupeUUID string) (string, error)
	// PostThreadApprovalCard posts the separate owner-facing tech-spec approval card.
	// Keeping this distinct from product decisions prevents the wrong role, copy, and
	// card dedupe key from being reused at node H.
	PostThreadApprovalCard func(ctx context.Context, loopID, body, actionURL string, mentionOpenIDs []string) error
	// PostThreadProductSpecCard asks product to create or update the authoritative
	// product spec on Plane before planning continues.
	PostThreadProductSpecCard func(ctx context.Context, loopID, body, actionURL string, mentionOpenIDs []string) error
	DiscoveryPolicy           DiscoveryPolicy
	// PlaneDoc resolves a Plane spec-document gateway + Plane project UUID for a
	// project whose task source is Plane (§8 flowchart). nil / (…,false) → the
	// project keeps the repo-file spec path (github/forgejo). Set for plane projects.
	PlaneDoc PlaneDocResolver
}

// PlaneDocResolver returns a project's Plane spec-document gateway + Plane project
// UUID, or ok=false for non-Plane projects.
type PlaneDocResolver func(projectID string) (*planedoc.Gateway, string, bool)

type DiscoveryPolicy struct {
	AutoDiscovery              bool
	Labels                     []string
	LabelMode                  config.LabelMode
	RequireAssigneeCurrentUser bool
}

type Runner struct {
	db                        *sql.DB
	repos                     *storage.Repositories
	github                    GitHubGateway
	git                       GitGateway
	agentExecutor             AgentExecutor
	logger                    bootstrap.Logger
	now                       func() time.Time
	agentTimeout              time.Duration
	agentIdleTimeout          time.Duration
	claimTTL                  time.Duration
	allowAutoPush             bool
	disclosure                config.DisclosureConfig
	agentRuntime              string
	agentProfileID            string
	customInstructions        config.Config
	projectRoleConfig         *config.Config
	agentModel                *string
	retryBaseDelay            time.Duration
	retryMaxAttempts          int64
	onAgentExecutionStarted   AgentExecutionStartedFunc
	onQueueItemEnqueued       func()
	postThreadNote            func(ctx context.Context, loopID, text string, mentionOpenIDs []string) error
	postThreadNoteWithUUID    func(ctx context.Context, loopID, text string, mentionOpenIDs []string, uuid string) error
	postThreadCard            func(ctx context.Context, loopID, body string, mentionOpenIDs []string) error
	postThreadImage           func(ctx context.Context, loopID, pngPath, dedupeUUID string) (string, error)
	postThreadApprovalCard    func(ctx context.Context, loopID, body, actionURL string, mentionOpenIDs []string) error
	postThreadProductSpecCard func(ctx context.Context, loopID, body, actionURL string, mentionOpenIDs []string) error
	decisionArtifactRoot      string
	discoveryPolicy           DiscoveryPolicy
	planeDoc                  PlaneDocResolver
}

type DiscoveryInput struct {
	ProjectID string
	Repo      string
	Limit     int
	Snapshot  *githubinfra.DiscoverySnapshot
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
	PipelineVersion int                     `json:"plannerPipelineVersion,omitempty"`
	Phase           string                  `json:"phase,omitempty"`
	Wait            *checkpointPlannerWait  `json:"wait,omitempty"`
	Decisions       *decisions.State        `json:"decisions,omitempty"`
	ResumePolicy    string                  `json:"resumePolicy,omitempty"`
	Issue           *checkpointIssue        `json:"issue,omitempty"`
	ClaimedLockKey  string                  `json:"claimedLockKey,omitempty"`
	Worktree        *checkpointWorktree     `json:"worktree,omitempty"`
	WriteSpec       *checkpointWriteSpec    `json:"writeSpec,omitempty"`
	Lifecycle       *lifecycle.State        `json:"gitPrLifecycle,omitempty"`
	Publish         *checkpointPublishState `json:"publish,omitempty"`
	Notify          *checkpointNotify       `json:"notify,omitempty"`
	SkipReason      string                  `json:"skipReason,omitempty"`
}

type checkpointPlannerWait struct {
	Reason     string      `json:"reason"`
	ResumeStep PlannerStep `json:"resumeStep"`
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
	ProductSpecURL     string   `json:"productSpecUrl,omitempty"`
	ProductSpec        string   `json:"productSpec,omitempty"`
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
	Status                       string           `json:"status,omitempty"`
	Summary                      string           `json:"summary,omitempty"`
	ProductAsk                   string           `json:"productAsk,omitempty"`
	Stdout                       string           `json:"stdout,omitempty"`
	Commits                      []string         `json:"commits,omitempty"`
	Lifecycle                    *lifecycle.State `json:"gitPrLifecycle,omitempty"`
	GitReconciled                bool             `json:"gitReconciled,omitempty"`
	TimeoutType                  string           `json:"timeoutType,omitempty"`
	ConfiguredIdleTimeoutSeconds int64            `json:"configuredIdleTimeoutSeconds,omitempty"`
	ConfiguredMaxRuntimeSeconds  int64            `json:"configuredMaxRuntimeSeconds,omitempty"`
	ElapsedRuntimeSeconds        int64            `json:"elapsedRuntimeSeconds,omitempty"`
	LastProgressAt               string           `json:"lastProgressAt,omitempty"`
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
	// PlaneSpecReview marks the Plane-provider publish path: the tech spec was
	// written to a Plane page (node G) and is awaiting page-comment review (node H) —
	// there is no spec PR. Drives the completion summary + card wording.
	PlaneSpecReview bool `json:"planeSpecReview,omitempty"`
	// Grilled / Reviewed track node H's two mandatory agent gates (idempotent resume).
	Grilled  bool `json:"grilled,omitempty"`
	Reviewed bool `json:"reviewed,omitempty"`
	// The technical GRILL result is persisted before Looper commits its spec edit.
	// This closes the commit→checkpoint crash window for RETURN_TO_REQUIREMENTS.
	GrillAgentCompleted bool   `json:"grillAgentCompleted,omitempty"`
	GrillBaselineHead   string `json:"grillBaselineHead,omitempty"`
	GrillProductAsk     string `json:"grillProductAsk,omitempty"`
	GrillSummary        string `json:"grillSummary,omitempty"`
	GrillSpecHash       string `json:"grillSpecHash,omitempty"`
	GrillGitReconciled  bool   `json:"grillGitReconciled,omitempty"`
	GrillReconciledHead string `json:"grillReconciledHead,omitempty"`
	// ReviewPlaneContentHash binds the independent REVIEW to the exact rendered
	// Plane page revision produced from the converged local spec. The approval
	// gate may open only while the remote page still has this hash.
	ReviewPlaneContentHash string `json:"reviewPlaneContentHash,omitempty"`
}

type checkpointNotify struct {
	SentAt  string `json:"sentAt,omitempty"`
	Message string `json:"message,omitempty"`
}

func checkpointWriteSpecFromAgentResult(result AgentResult) *checkpointWriteSpec {
	return &checkpointWriteSpec{Status: result.Status, Summary: result.Summary, ProductAsk: strings.TrimSpace(result.ProductAsk), Stdout: result.Stdout, Commits: append([]string(nil), result.Commits...), Lifecycle: result.Lifecycle, TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds, ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt}
}

// reopenV2Requirements invalidates every authority receipt and every artifact derived
// from the previous requirement revision. The brief itself is retained as adversarial
// input for the fresh requirement grill, together with the explicit reopen reason;
// nothing that previously proved a human answered or approved may survive as current.
func reopenV2Requirements(checkpoint plannerCheckpoint, reason string) plannerCheckpoint {
	if checkpoint.Decisions != nil {
		checkpoint.Decisions.Stage = "requirements_reopened"
		checkpoint.Decisions.ReopenReason = strings.TrimSpace(reason)
		checkpoint.Decisions.ProductSpec = ""
		checkpoint.Decisions.Requests = nil
		checkpoint.Decisions.RequestedQuestions = nil
		checkpoint.Decisions.Answers = nil
		checkpoint.Decisions.DecisionLog = ""
		checkpoint.Decisions.ImageMessages = nil
	}
	checkpoint.Phase = "requirements_reopened"
	checkpoint.Wait = nil
	checkpoint.WriteSpec = nil
	checkpoint.Publish = nil
	checkpoint.Notify = nil
	checkpoint.SkipReason = ""
	return checkpoint
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

type holdSkipError struct{ summary string }

func (e *holdSkipError) Error() string { return e.summary }

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
	if retryMax == 0 {
		retryMax = defaultRetryMax
	}
	allowAutoPush := true
	if options.AllowAutoPush != nil {
		allowAutoPush = *options.AllowAutoPush
	}
	disclosureCfg := config.DefaultDisclosureConfig()
	if options.Disclosure != nil {
		disclosureCfg = *options.Disclosure
	}
	policy := options.DiscoveryPolicy
	if policy.LabelMode == "" {
		policy = DiscoveryPolicy{AutoDiscovery: true, Labels: []string{discoveryLabel}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: true}
	}
	artifactRoot := ""
	if options.CustomInstructions != nil && strings.TrimSpace(options.CustomInstructions.Storage.DBPath) != "" {
		artifactRoot = filepath.Join(filepath.Dir(options.CustomInstructions.Storage.DBPath), "decision-artifacts")
	}
	return &Runner{db: options.DB, repos: options.Repos, github: options.GitHub, git: options.Git, agentExecutor: options.AgentExecutor, logger: options.Logger, now: now, agentTimeout: agentTimeout, agentIdleTimeout: agentIdleTimeout, claimTTL: claimTTL, allowAutoPush: allowAutoPush, disclosure: disclosureCfg, agentRuntime: strings.TrimSpace(options.AgentRuntime), agentProfileID: strings.TrimSpace(options.AgentProfileID), customInstructions: customInstructionConfig(options.CustomInstructions), projectRoleConfig: options.CustomInstructions, agentModel: cloneStringPtr(options.AgentModel), retryBaseDelay: retryBaseDelay, retryMaxAttempts: retryMax, onAgentExecutionStarted: options.OnAgentExecutionStarted, onQueueItemEnqueued: options.OnQueueItemEnqueued, postThreadNote: options.PostThreadNote, postThreadNoteWithUUID: options.PostThreadNoteWithUUID, postThreadCard: options.PostThreadCard, postThreadImage: options.PostThreadImage, postThreadApprovalCard: options.PostThreadApprovalCard, postThreadProductSpecCard: options.PostThreadProductSpecCard, decisionArtifactRoot: artifactRoot, discoveryPolicy: policy, planeDoc: options.PlaneDoc}
}

func (r *Runner) DiscoverIssues(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	ctx = githubinfra.ContextWithDiscoverySnapshot(ctx, input.Snapshot)
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
	policy := r.discoveryPolicyForProject(project.ID)
	if !policy.AutoDiscovery {
		return DiscoveryResult{Skipped: 1}, nil
	}
	login := ""
	if policy.RequireAssigneeCurrentUser {
		var err error
		login, err = r.github.GetCurrentUserLogin(ctx, project.RepoPath)
		if err != nil {
			return DiscoveryResult{}, err
		}
		login = normalizeLogin(login)
	}
	if policy.RequireAssigneeCurrentUser && login == "" {
		return DiscoveryResult{Skipped: 1}, nil
	}
	assigneeFilter := ""
	if policy.RequireAssigneeCurrentUser {
		assigneeFilter = login
	}
	issues, err := r.listOpenIssuesForDiscovery(ctx, ListOpenIssuesInput{Repo: input.Repo, CWD: project.RepoPath, Limit: input.Limit, Assignee: assigneeFilter}, policy)
	if err != nil {
		return DiscoveryResult{}, err
	}
	result := DiscoveryResult{}
	for _, issue := range issues {
		if domain.IsAutoLaneHeld(domain.LoopTypePlanner, issue.Labels) {
			result.Skipped++
			continue
		}
		if !shouldClaimIssue(issue, login, policy) {
			result.Skipped++
			continue
		}
		fingerprint := buildPlannerDiscoveryFingerprint(input.Repo, r.now(), issue)
		loopResult, err := r.ensureLoopForIssue(ctx, *project, input.Repo, issue, fingerprint)
		if err != nil {
			return DiscoveryResult{}, err
		}
		if loopResult.created {
			result.CreatedLoopIDs = append(result.CreatedLoopIDs, loopResult.record.ID)
		}
		if loopResult.record.Status == "paused" || loopResult.record.Status == "completed" || loopResult.record.Status == "awaiting_human" {
			result.Skipped++
			continue
		}
		// Anti-thrash: skip enqueue when ensureLoopForIssue left a previously
		// failed loop in place because its discovery inputs match the last
		// terminal failure.
		if loopResult.record.Status == "failed" {
			result.Skipped++
			continue
		}
		queueItem, err := r.enqueue(ctx, enqueueInput{ProjectID: project.ID, LoopID: loopResult.record.ID, Repo: input.Repo, IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number, "title": issue.Title, "body": issue.Body, "url": issue.URL, "assignees": issue.Assignees, "labels": issue.Labels, "currentUserLogin": login, "strictDispatchId": issue.StrictDispatchID, plannerQueuePayloadFPKey: fingerprint}})
		if err != nil {
			return DiscoveryResult{}, err
		}
		result.QueueItems = append(result.QueueItems, queueItem)
	}
	return result, nil
}

func (r *Runner) discoveryPolicyForProject(projectID string) DiscoveryPolicy {
	if r.projectRoleConfig == nil {
		return r.discoveryPolicy
	}
	roles := config.ProjectRoleConfigs(*r.projectRoleConfig, projectID)
	return DiscoveryPolicy{AutoDiscovery: roles.Planner.AutoDiscovery, Labels: append([]string(nil), roles.Planner.Triggers.Labels...), LabelMode: roles.Planner.Triggers.LabelMode, RequireAssigneeCurrentUser: roles.Planner.Triggers.RequireAssigneeCurrentUser}
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
	r.transitionTerminalStrictDispatchFailure(ctx, queueItem, failedQueue)
	return &ProcessResult{LoopID: derefString(queueItem.LoopID), QueueItemID: queueItem.ID, Status: "failed", Summary: failure.message, FailureKind: failure.kind}, nil
}

func (r *Runner) transitionTerminalStrictDispatchFailure(ctx context.Context, queueItem storage.QueueItemRecord, failedQueue *storage.QueueItemRecord) {
	if failedQueue == nil || (failedQueue.Status != "failed" && failedQueue.Status != "manual_intervention") {
		return
	}
	dispatchID := strings.TrimSpace(stringFromAnyDefault(parseJSONObject(queueItem.PayloadJSON)["strictDispatchId"]))
	if dispatchID == "" {
		return
	}
	gateway, ok := r.github.(strictDispatchGateway)
	if !ok {
		return
	}
	cwd := ""
	if queueItem.ProjectID != nil {
		if project, err := r.repos.Projects.GetByID(ctx, *queueItem.ProjectID); err == nil && project != nil {
			cwd = project.RepoPath
		}
	}
	transitionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := gateway.TransitionStrictDispatch(transitionCtx, StrictDispatchTransitionInput{Repo: derefString(queueItem.Repo), CWD: cwd, DispatchID: dispatchID, State: "failed"}); err != nil && r.logger != nil {
		r.logger.Warn("planner: mark terminal strict dispatch failed", map[string]any{"dispatchId": dispatchID, "queueItemId": queueItem.ID, "error": err.Error()})
	}
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
			updated.Status = "paused"
			r.stampFailedDiscoveryFingerprint(updated, queueItem)
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
	strictDispatchID := strings.TrimSpace(stringFromAnyDefault(parseJSONObject(queueItem.PayloadJSON)["strictDispatchId"]))
	if strictDispatchID != "" {
		metadataJSON, mergeErr := mergeLoopMetadataJSON(loop.MetadataJSON, map[string]any{"strictDispatchId": strictDispatchID})
		if mergeErr != nil {
			return ProcessResult{}, mergeErr
		}
		updated, updateErr := r.updateLoop(ctx, *loop, func(value *storage.LoopRecord) {
			value.MetadataJSON = &metadataJSON
		})
		if updateErr != nil {
			return ProcessResult{}, updateErr
		}
		loop = &updated
	}
	if strictDispatchID != "" {
		gateway, ok := r.github.(strictDispatchGateway)
		if !ok {
			return ProcessResult{}, fmt.Errorf("strict dispatch gateway is not configured")
		}
		if err := gateway.TransitionStrictDispatch(ctx, StrictDispatchTransitionInput{
			Repo: derefString(loop.Repo), CWD: project.RepoPath,
			DispatchID: strictDispatchID, State: "running",
		}); err != nil {
			return ProcessResult{}, fmt.Errorf("start strict dispatch %s: %w", strictDispatchID, err)
		}
	}
	resumedRun, err := r.createRunContext(ctx, *loop)
	if err != nil {
		return ProcessResult{}, err
	}
	run := resumedRun.Run
	checkpoint := resumedRun.Checkpoint
	if !plannerQueueItemIsManual(queueItem) {
		if held, summary, err := r.plannerHoldSummary(ctx, *project, queueItem, *loop); err != nil {
			return ProcessResult{}, err
		} else if held {
			return r.finishHeldPlannerQueueItem(ctx, *loop, &run, queueItem, checkpoint, summary)
		}
	}
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

	for _, step := range stepsFromVersion(resumedRun.StartStep, checkpoint.PipelineVersion) {
		run, err = r.persistStepStarted(ctx, run, step, checkpoint)
		if err != nil {
			return ProcessResult{}, err
		}
		r.appendEvent(ctx, eventInput{eventType: "loop.step.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"step": string(step)}})
		checkpoint, err = r.executeStep(ctx, step, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: queueItem, Checkpoint: checkpoint})
		if err != nil {
			var holdErr *holdSkipError
			if errors.As(err, &holdErr) {
				return r.finishHeldPlannerQueueItem(ctx, *loop, &run, queueItem, checkpoint, holdErr.summary)
			}
			failure := r.classifyFailureWithBoundary(err, plannerFailureBoundaryForStep(step))
			latest := r.getLatestCheckpoint(ctx, run, checkpoint)
			if failure.kind == FailureHITLInterrupt {
				// 强操控: a human's mid-run @bot killed the agent. Complete the run as
				// "interrupted" (NOT failed) and re-dispatch it IMMEDIATELY (no backoff,
				// attempts unchanged so it never hits the retry cap) so follow-up-resume
				// answers them from the MAIN write-spec session.
				latest.ResumePolicy = "advance_from_checkpoint"
				if _, err := r.completeRun(ctx, run, "interrupted", failure.message, "", latest); err != nil {
					return ProcessResult{}, err
				}
				r.appendEvent(ctx, eventInput{eventType: "run.interrupted", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"message": failure.message, "currentStep": derefString(run.CurrentStep)}})
				nowISO := r.nowISO()
				if err := r.repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: queueItem.ID, AvailableAt: nowISO, Attempts: queueItem.Attempts, ErrorMessage: optionalString(failure.message), ErrorKind: string(failure.kind), UpdatedAt: nowISO}); err != nil {
					return ProcessResult{}, err
				}
				if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
					updated.LastRunAt = stringPtr(nowISO)
					updated.Status = "queued"
					updated.NextRunAt = stringPtr(nowISO)
				}); err != nil {
					return ProcessResult{}, err
				}
				return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: "interrupted", Summary: failure.message, FailureKind: failure.kind}, nil
			}
			latest.ResumePolicy = loops.NormalizeResumePolicy(string(failure.kind), latest.ResumePolicy)
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
					updated.Status = "paused"
					r.stampFailedDiscoveryFingerprint(updated, queueItem)
					updated.NextRunAt = nil
				}
			}); err != nil {
				return ProcessResult{}, err
			}
			r.transitionTerminalStrictDispatchFailure(ctx, queueItem, failedQueue)
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
		if checkpoint.Wait != nil {
			break
		}
	}

	summary := checkpoint.SkipReason
	if checkpoint.Wait != nil {
		summary = checkpoint.Wait.Reason
	}
	if summary == "" {
		issue := checkpoint.Issue
		if issue != nil {
			if checkpoint.Publish != nil && checkpoint.Publish.PlaneSpecReview {
				summary = fmt.Sprintf("Wrote tech spec to Plane for %s#%d — awaiting review (node H)", issue.Repo, issue.IssueNumber)
			} else {
				summary = fmt.Sprintf("Opened spec PR for %s#%d", issue.Repo, issue.IssueNumber)
			}
		} else {
			summary = "Completed planner run"
		}
	}
	if _, err := r.completeRun(ctx, run, "success", summary, "", checkpoint); err != nil {
		return ProcessResult{}, err
	}
	r.appendEvent(ctx, eventInput{eventType: "run.completed", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"summary": summary}})
	status := "success"
	if checkpoint.SkipReason != "" {
		status = "skipped"
	} else if checkpoint.Wait != nil {
		status = "waiting"
	}
	prNumber := int64(0)
	if checkpoint.Publish != nil && checkpoint.Publish.PullRequest != nil {
		prNumber = checkpoint.Publish.PullRequest.Number
	}
	if strictDispatchID != "" {
		gateway := r.github.(strictDispatchGateway)
		nextState := "completed"
		var waitKind *string
		if checkpoint.Wait != nil || checkpoint.SkipReason == awaitingProductDecisionSkipReason {
			nextState = "awaiting_human"
			value := "technical_spec_approval"
			if checkpoint.SkipReason == awaitingProductDecisionSkipReason {
				value = "role_decision"
			}
			waitKind = &value
		}
		if err := gateway.TransitionStrictDispatch(ctx, StrictDispatchTransitionInput{
			Repo: derefString(loop.Repo), CWD: project.RepoPath,
			DispatchID: strictDispatchID, State: nextState, WaitKind: waitKind,
		}); err != nil {
			return ProcessResult{}, fmt.Errorf("finish strict dispatch %s: %w", strictDispatchID, err)
		}
	}
	if err := r.repos.Queue.Complete(ctx, queueItem.ID, r.nowISO()); err != nil {
		if !errors.Is(err, storage.ErrQueueItemNotActive) {
			return ProcessResult{}, err
		}
	}
	if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
		if checkpoint.Wait != nil {
			updated.Status = "paused"
			phase := checkpoint.Phase
			if metadata, metaErr := mergeLoopMetadataJSON(updated.MetadataJSON, map[string]any{"decisionPhase": phase, "nodeHPhase": phase, "plannerPipelineVersion": checkpoint.PipelineVersion}); metaErr == nil {
				updated.MetadataJSON = stringPtr(metadata)
			}
		} else if checkpoint.SkipReason == awaitingProductDecisionSkipReason {
			updated.Status = "awaiting_human"
		} else {
			updated.Status = "completed"
		}
		updated.LastRunAt = stringPtr(r.nowISO())
		updated.NextRunAt = nil
	}); err != nil {
		return ProcessResult{}, err
	}
	return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: status, Summary: summary, PullRequestNumber: prNumber}, nil
}

func (r *Runner) executeStep(ctx context.Context, step PlannerStep, input stepInput) (plannerCheckpoint, error) {
	switch step {
	case stepDiscoverIssues:
		return r.runDiscoverIssueStep(ctx, input)
	case stepPrepareWorktree:
		return r.runPrepareWorktreeStep(ctx, input)
	case stepAuthorDecisionBrief:
		return r.runAuthorDecisionBriefStep(ctx, input)
	case stepGrillProductDecisions:
		return r.runRequirementGrillStep(ctx, input, "product")
	case stepRouteProductDecisions:
		return r.runRouteProductDecisionsStep(ctx, input)
	case stepGrillDownstreamDecisions:
		return r.runRequirementGrillStep(ctx, input, "downstream")
	case stepRouteDownstreamDecisions:
		return r.runRouteDownstreamDecisionsStep(ctx, input)
	case stepGrillFinalDecisions:
		return r.runRequirementGrillStep(ctx, input, "final")
	case stepWriteSpec:
		return r.runWriteSpecStep(ctx, input)
	case stepPublish:
		return r.runPublishStep(ctx, input)
	case stepGrill:
		return r.runGrillStep(ctx, input)
	case stepReview:
		return r.runReviewStep(ctx, input)
	case stepNotify:
		return r.runNotifyStep(input)
	default:
		return input.Checkpoint, fmt.Errorf("unsupported planner step: %s", step)
	}
}

func (r *Runner) runDiscoverIssueStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	payload := parseJSONObject(input.QueueItem.PayloadJSON)
	checkpoint := input.Checkpoint
	configuredPipelineVersion := int(int64FromAny(parseJSONObject(input.Loop.MetadataJSON)["plannerPipelineVersion"]))
	if configuredPipelineVersion > checkpoint.PipelineVersion {
		checkpoint.PipelineVersion = configuredPipelineVersion
	}
	if checkpoint.PipelineVersion == 0 {
		checkpoint.PipelineVersion = 1
	}
	input.Checkpoint = checkpoint
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
	currentLogin := firstNonEmpty(normalizeLogin(stringFromAnyDefault(payload["currentUserLogin"])), input.CheckpointIssueLogin())
	lockKey := firstNonEmpty(derefString(input.QueueItem.LockKey), storage.IssueLockKey(input.Project.ID, repo, issueNumber))
	nowISO := r.nowISO()
	reason := "planner-run"
	acquired, err := r.repos.Locks.Acquire(ctx, storage.LockRecord{Key: lockKey, Owner: input.QueueItem.ID, Reason: &reason, ExpiresAt: eventlog.FormatJavaScriptISOString(r.now().Add(r.claimTTL)), CreatedAt: nowISO, UpdatedAt: nowISO})
	if err != nil {
		return input.Checkpoint, err
	}
	if !acquired {
		return input.Checkpoint, &loopError{message: fmt.Sprintf("Issue lock is already held for %s", lockKey), kind: FailureRetryableTransient}
	}
	releaseOnError := true
	defer func() {
		if releaseOnError {
			_ = r.repos.Locks.Release(context.Background(), lockKey)
		}
	}()
	manual := isManualPlannerQueue(payload)
	strictDispatch := strings.TrimSpace(stringFromAnyDefault(payload["strictDispatchId"])) != ""
	policy := r.discoveryPolicyForProject(input.Project.ID)
	if currentLogin == "" && (manual || policy.RequireAssigneeCurrentUser || hasRequestedReviewerSources(input.Project, input.Loop, detail.Assignees)) {
		login, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
		if err != nil {
			return input.Checkpoint, &loopError{message: fmt.Sprintf("Unable to resolve GitHub login for planner issue %s#%d: %v", repo, issueNumber, err), kind: FailureRetryableAfterResume}
		}
		currentLogin = normalizeLogin(login)
		if currentLogin == "" {
			return input.Checkpoint, &loopError{message: fmt.Sprintf("Unable to resolve GitHub login for planner issue %s#%d", repo, issueNumber), kind: FailureRetryableAfterResume}
		}
	}
	if !manual && !strictDispatch && !labelsMatch(detail.Labels, policy.Labels, policy.LabelMode) {
		checkpoint := input.Checkpoint
		checkpoint.Issue = &checkpointIssue{Repo: repo, IssueNumber: issueNumber, Title: detail.Title, Body: detail.Body, URL: detail.URL, Assignees: cloneStrings(detail.Assignees), Labels: cloneStrings(detail.Labels), CurrentUserLogin: currentLogin, SpecPath: buildSpecPath(r.now(), issueNumber, detail.Title), RequestedReviewers: resolveRequestedReviewers(input.Project, input.Loop, detail.Assignees, currentLogin)}
		checkpoint.ClaimedLockKey = lockKey
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		checkpoint.SkipReason = fmt.Sprintf("Issue %s#%d no longer matches planner labels", repo, issueNumber)
		releaseOnError = false
		return checkpoint, nil
	}
	if !manual && !strictDispatch && policy.RequireAssigneeCurrentUser && currentLogin != "" && !includesLogin(detail.Assignees, currentLogin) {
		checkpoint := input.Checkpoint
		checkpoint.Issue = &checkpointIssue{Repo: repo, IssueNumber: issueNumber, Title: detail.Title, Body: detail.Body, URL: detail.URL, Assignees: cloneStrings(detail.Assignees), Labels: cloneStrings(detail.Labels), CurrentUserLogin: currentLogin, SpecPath: buildSpecPath(r.now(), issueNumber, detail.Title), RequestedReviewers: resolveRequestedReviewers(input.Project, input.Loop, detail.Assignees, currentLogin)}
		checkpoint.ClaimedLockKey = lockKey
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		checkpoint.SkipReason = fmt.Sprintf("Issue %s#%d is no longer assigned to %s", repo, issueNumber, currentLogin)
		releaseOnError = false
		return checkpoint, nil
	}
	if manual && currentLogin != "" && !includesLogin(detail.Assignees, currentLogin) {
		if err := r.github.AddIssueAssignees(ctx, IssueAssigneesInput{Repo: repo, IssueNumber: issueNumber, Assignees: []string{currentLogin}, CWD: input.Project.RepoPath}); err != nil {
			return input.Checkpoint, &loopError{message: fmt.Sprintf("Unable to assign issue %s#%d to %s: %v", repo, issueNumber, currentLogin, err), kind: FailureRetryableAfterResume}
		}
		detail.Assignees = appendUniqueStrings(detail.Assignees, currentLogin)
	}
	checkpoint = input.Checkpoint
	checkpoint.Issue = &checkpointIssue{Repo: repo, IssueNumber: issueNumber, Title: detail.Title, Body: detail.Body, URL: detail.URL, Assignees: cloneStrings(detail.Assignees), Labels: cloneStrings(detail.Labels), CurrentUserLogin: currentLogin, SpecPath: buildSpecPath(r.now(), issueNumber, detail.Title), RequestedReviewers: resolveRequestedReviewers(input.Project, input.Loop, detail.Assignees, currentLogin)}
	checkpoint.ClaimedLockKey = lockKey
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	checkpoint.SkipReason = ""
	releaseOnError = false
	return checkpoint, nil
}

func (input stepInput) CheckpointIssueLogin() string {
	if input.Checkpoint.Issue == nil {
		return ""
	}
	return input.Checkpoint.Issue.CurrentUserLogin
}

// productSpecGate implements flowchart node D/E for a Plane-provider feature: check
// whether the work item has a readable, non-empty product spec whose provenance
// resolves to the configured product owner. A link created only by Looper or its
// operator is not a product spec. If verification fails, ask product on the work
// item and hold before any technical planning. A no-op for github/forgejo projects
// and non-features.
func (r *Runner) productSpecGate(ctx context.Context, input stepInput, checkpoint plannerCheckpoint) *loopError {
	if r.planeDoc == nil || checkpoint.Issue == nil {
		return nil
	}
	gateway, planeProjectID, ok := r.planeDoc(input.Project.ID)
	if !ok || gateway == nil {
		return nil
	}
	issue := checkpoint.Issue
	if !stringInSlice("kind/feature", issue.Labels) {
		return nil // bugs / refactors / perf don't need a product spec
	}
	// Checkpoints survive retries. Never carry a formerly accepted product spec
	// across a fresh provenance check; only repopulate these fields after the current
	// linked document passes verification below.
	issue.ProductSpec = ""
	issue.ProductSpecURL = ""
	workItemID := planedoc.WorkItemIDFromURL(issue.URL)
	if workItemID == "" {
		return nil
	}
	productSpecURL, present, err := gateway.FindSpecLink(ctx, planeProjectID, workItemID, planedoc.ProductSpecLinkTitle)
	if err != nil {
		return &loopError{message: fmt.Sprintf("check product spec on work item: %v", err), kind: FailureRetryableTransient}
	}
	productOwnerPlaneID := ""
	if r.projectRoleConfig != nil {
		productOwnerPlaneID = strings.TrimSpace(config.ProjectProductOwner(*r.projectRoleConfig, input.Project.ID).PlaneID)
	}
	productSpec := strings.TrimSpace(productSpecURL)
	verified := present && loops.ProductSpecConfirmedBy(input.Loop.MetadataJSON, productSpecURL, productOwnerPlaneID)
	if present {
		if pageID := planedoc.PageIDFromURL(productSpecURL); pageID != "" {
			page, err := gateway.PageDocument(ctx, planeProjectID, pageID)
			if err != nil {
				return &loopError{message: fmt.Sprintf("read product spec document on work item: %v", err), kind: FailureRetryableTransient}
			}
			productSpec = page.ContentHTML
			verified = verified || page.AuthoredBy(productOwnerPlaneID)
		}
	}
	if present && strings.TrimSpace(productSpec) != "" && verified {
		issue.ProductSpec = strings.TrimSpace(productSpec)
		issue.ProductSpecURL = strings.TrimSpace(productSpecURL)
		// A resumed planner passes here once product supplied the spec (node E2) —
		// clear the hold marker so the card leaves "⏸ 等待产品方案".
		r.setAwaitingProductSpecMarker(ctx, input.Loop, false, "", issue)
		return nil // has a product spec → proceed to write the tech spec
	}
	reason := "还没有关联产品 spec"
	if present && productOwnerPlaneID == "" {
		reason = "已有关联页面，但项目尚未配置产品负责人的 Plane member ID，无法验证作者"
	} else if present && strings.TrimSpace(productSpec) == "" {
		reason = "已有关联页面，但正文为空"
	} else if present {
		reason = "已有关联页面，但它不是由配置的产品负责人创建、接管或明确确认的"
	}
	comment, err := gateway.RequestProductSpec(ctx, planeProjectID, workItemID, "产品负责人", issue.Title)
	if err != nil {
		return &loopError{message: fmt.Sprintf("request product spec on Plane: %v", err), kind: FailureRetryableTransient}
	}
	// Mark the hold reason so the anchor card reads "⏸ 等待产品方案" (node E) rather
	// than the generic "⏸ 等你定夺". Persist this before posting the Feishu card:
	// card delivery refreshes the anchor header from the current loop metadata.
	r.setAwaitingProductSpecMarker(ctx, input.Loop, true, comment.ID, issue)
	// Plane owns the response. Feishu only targets the owner and deep-links to the
	// exact source comment; replies in the thread are intentionally ignored.
	r.requestProductSpecInThread(ctx, input, issue.Title, reason, planedoc.WorkItemCommentURL(issue.URL, comment.ID))
	return &loopError{message: "awaiting an actionable product spec — asked product to supply one on the work item", kind: FailureManualIntervention}
}

// requestProductSpecInThread @-mentions the project's product owner in the loop's
// Feishu thread to ask for a missing product spec (node E) — the thread mirror of the
// Plane comment RequestProductSpec posts, so the ask reaches them where they watch.
// The open_id is resolved per-project from config (never hardcoded); a missing
// transport or an unset owner just skips the ping. Best-effort.
func (r *Runner) requestProductSpecInThread(ctx context.Context, input stepInput, workItemTitle, reason, actionURL string) {
	if r.postThreadProductSpecCard == nil || r.projectRoleConfig == nil || strings.TrimSpace(actionURL) == "" {
		return
	}
	var mentions []string
	if openID := strings.TrimSpace(config.ProjectProductOwner(*r.projectRoleConfig, input.Project.ID).FeishuOpenID); openID != "" {
		mentions = []string{openID}
	}
	note := fmt.Sprintf("⏸ 需求「%s」需要产品负责人先出 product spec：%s。请前往 Plane 的具体评论补充方案页链接或正文；Looper 不会代写产品范围，飞书回复不会被读取。", strings.TrimSpace(workItemTitle), strings.TrimSpace(reason))
	if err := r.postThreadProductSpecCard(ctx, input.Loop.ID, note, actionURL, mentions); err != nil && r.logger != nil {
		r.logger.Warn("planner: post product-spec request note failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
	}
}

// setAwaitingProductSpecMarker records (or clears) the "awaiting product spec" hold
// flag in the loop metadata, so the anchor card can distinguish node E's product-spec
// wait from a generic HITL ask. Best-effort + guarded: a Runner without a loops repo
// (e.g. a unit/live test wiring only planeDoc) silently skips it.
func (r *Runner) setAwaitingProductSpecMarker(ctx context.Context, loop storage.LoopRecord, waiting bool, askCommentID string, issue *checkpointIssue) {
	if r.repos == nil || r.repos.Loops == nil {
		return
	}
	updates := map[string]any{"awaitingProductSpec": waiting}
	if issue != nil {
		updates["issueUrl"] = issue.URL
		updates["issueTitle"] = issue.Title
		updates["issueNumber"] = issue.IssueNumber
	}
	var metadataErr error
	if _, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		metadataJSON, err := mergeLoopMetadataJSON(updated.MetadataJSON, updates)
		if err == nil && waiting {
			metadataJSON, err = loopcondition.Set(&metadataJSON, loopcondition.Record{Kind: loopcondition.ProductSpec, Since: r.nowISO(), Fingerprint: strings.TrimSpace(askCommentID)})
		} else if err == nil {
			metadataJSON, err = loopcondition.Clear(&metadataJSON)
		}
		if err != nil {
			metadataErr = err
			return
		}
		updated.MetadataJSON = stringPtr(metadataJSON)
	}); err != nil && r.logger != nil {
		r.logger.Warn("planner: mark awaiting product spec failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
	} else if metadataErr != nil && r.logger != nil {
		r.logger.Warn("planner: encode awaiting product spec marker failed", map[string]any{"loopId": loop.ID, "error": metadataErr.Error()})
	}
}

// requestProductSpecClarificationOnPlane returns a genuinely blocking product gap
// to the work item, where product can update the authoritative spec. Feishu remains
// a one-way notification carrying the exact comment URL. Repeated execution reuses
// the current awaiting ask instead of creating duplicate comments.
func (r *Runner) requestProductSpecClarificationOnPlane(ctx context.Context, input stepInput, checkpoint plannerCheckpoint, productAsk string) (string, error) {
	if r.planeDoc == nil {
		return "", fmt.Errorf("planner: Plane decision requires a Plane document gateway")
	}
	gateway, planeProjectID, ok := r.planeDoc(input.Project.ID)
	if !ok || gateway == nil {
		return "", fmt.Errorf("planner: Plane decision gateway is not configured for project %s", input.Project.ID)
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return "", err
	}
	workItemID := planedoc.WorkItemIDFromURL(issue.URL)
	if workItemID == "" {
		return "", fmt.Errorf("planner: cannot resolve Plane work item from %q", issue.URL)
	}
	fresh, err := r.repos.Loops.GetByID(ctx, input.Loop.ID)
	if err != nil {
		return "", err
	}
	if fresh == nil {
		return "", fmt.Errorf("planner: loop disappeared: %s", input.Loop.ID)
	}
	ask, hasAsk := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !(hasAsk && ask.Transport == "plane" && ask.Status == "awaiting" && strings.TrimSpace(ask.Question) == strings.TrimSpace(productAsk) && strings.TrimSpace(ask.ActionURL) != "") {
		body := "<p><strong>📝 产品 spec 需要补充</strong></p><p>" + strings.ReplaceAll(htmlpkg.EscapeString(strings.TrimSpace(productAsk)), "\n", "<br>") + "</p><p>请先更新本 work item 已关联的 product spec，再回复本评论说明已更新。Looper 会重新读取 product spec 后再做技术方案。</p>"
		comment, createErr := gateway.CreateWorkItemComment(ctx, planeProjectID, workItemID, planedoc.SignComment(body, "planner", derefString(r.agentModel)))
		if createErr != nil {
			return "", createErr
		}
		actionURL := planedoc.WorkItemCommentURL(issue.URL, comment.ID)
		if actionURL == "" {
			return "", fmt.Errorf("planner: Plane product-spec comment did not return an id")
		}
		ask = loops.HITLAsk{Question: strings.TrimSpace(productAsk), SessionID: r.latestNativeSessionID(ctx, input.Loop.ID), Status: "awaiting", AskedAt: r.nowISO(), Transport: "plane", ActionURL: actionURL}
		metadata, writeErr := loops.WriteHITLAsk(fresh.MetadataJSON, ask)
		if writeErr != nil {
			return "", writeErr
		}
		metadata, writeErr = mergeLoopMetadataJSON(&metadata, map[string]any{"awaitingProductAnswer": true})
		if writeErr != nil {
			return "", writeErr
		}
		metadata, writeErr = loopcondition.Set(&metadata, loopcondition.Record{Kind: loopcondition.HumanAnswered, Since: ask.AskedAt})
		if writeErr != nil {
			return "", writeErr
		}
		if _, writeErr = r.updateLoop(ctx, *fresh, func(updated *storage.LoopRecord) { updated.MetadataJSON = stringPtr(metadata) }); writeErr != nil {
			return "", writeErr
		}
	}

	r.notifyProductSpecClarification(ctx, input, productAsk, ask.ActionURL)
	return ask.ActionURL, nil
}

func (r *Runner) notifyProductSpecClarification(ctx context.Context, input stepInput, productAsk, actionURL string) {
	if r.postThreadProductSpecCard == nil || strings.TrimSpace(actionURL) == "" {
		return
	}
	var mentions []string
	if r.projectRoleConfig != nil {
		if openID := strings.TrimSpace(config.ProjectProductOwner(*r.projectRoleConfig, input.Project.ID).FeishuOpenID); openID != "" {
			mentions = []string{openID}
		}
	}
	if err := r.postThreadProductSpecCard(ctx, input.Loop.ID, strings.TrimSpace(productAsk), actionURL, mentions); err != nil && r.logger != nil {
		r.logger.Warn("planner: post product-spec clarification card failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
	}
}

func (r *Runner) notifySpecApproval(ctx context.Context, input stepInput, body, actionURL string) {
	if r.postThreadApprovalCard == nil || strings.TrimSpace(actionURL) == "" {
		return
	}
	var mentions []string
	if r.projectRoleConfig != nil {
		if openID := strings.TrimSpace(config.ProjectOwner(*r.projectRoleConfig, input.Project.ID)); openID != "" {
			mentions = []string{openID}
		}
	}
	if err := r.postThreadApprovalCard(ctx, input.Loop.ID, strings.TrimSpace(body), actionURL, mentions); err != nil && r.logger != nil {
		r.logger.Warn("planner: post spec-approval card failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
	}
}

// setAwaitingProductAnswerMarker records (or clears) the "awaiting product answer"
// hold flag in the loop metadata (node H product-decision gate), distinct from the
// node-E "awaiting product spec" wait and the owner "awaiting spec approval" gate.
// Best-effort + guarded like setAwaitingProductSpecMarker.
func (r *Runner) setAwaitingProductAnswerMarker(ctx context.Context, loop storage.LoopRecord, waiting bool) {
	if r.repos == nil || r.repos.Loops == nil {
		return
	}
	metadataJSON, err := mergeLoopMetadataJSON(loop.MetadataJSON, map[string]any{"awaitingProductAnswer": waiting})
	if err != nil {
		return
	}
	if _, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) { updated.MetadataJSON = stringPtr(metadataJSON) }); err != nil && r.logger != nil {
		r.logger.Warn("planner: mark awaiting product answer failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
	}
}

func (r *Runner) runPrepareWorktreeStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	// Flowchart node D/E: on a Plane project, a feature can't be planned until it has
	// a product spec. If missing, ask product on the work item and hold (no worktree,
	// no tech spec) until it's supplied.
	if checkpoint.PipelineVersion < 2 {
		if gateErr := r.productSpecGate(ctx, input, checkpoint); gateErr != nil {
			return checkpoint, gateErr
		}
	}
	projectMetadata := parseJSONObject(input.Project.MetadataJSON)
	worktreeRoot := stringFromAnyDefault(projectMetadata["worktreeRoot"])
	var err error
	if worktreeRoot == "" {
		worktreeRoot, err = config.DefaultProjectWorktreeRoot(input.Project.ID, input.Project.RepoPath)
		if err != nil {
			return checkpoint, err
		}
	}
	if checkpoint.Worktree != nil {
		if plannerCheckpointWorktreeUsable(checkpoint.Worktree.Path, input.Project.RepoPath, worktreeRoot) {
			return checkpoint, nil
		}
		checkpoint.Worktree = nil
		checkpoint.ResumePolicy = "advance_from_checkpoint"
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	baseBranch := firstNonEmpty(derefString(input.Project.BaseBranch), "main")
	branch := buildPlannerBranch(issue.IssueNumber, issue.Title)
	created, err := r.git.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: input.Project.ID, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, Branch: branch, BaseBranch: baseBranch, ProtectedBranches: []string{baseBranch}})
	if err != nil {
		return checkpoint, err
	}
	checkpoint.Worktree = &checkpointWorktree{ID: created.ID, Path: created.WorktreePath, Branch: created.Branch, BaseBranch: firstNonEmpty(created.BaseBranch, baseBranch), SpecPath: issue.SpecPath}
	checkpoint.Lifecycle = lifecycle.NewState(lifecycle.AgentManagedWithFallbackPolicy("planner", true), created.Branch, firstNonEmpty(created.BaseBranch, baseBranch))
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

// plannerProductAskInstruction tells the planner agent to escalate — in product
// language — the open questions in a spec that only the product owner can decide,
// via a `productAsk` field in its completion marker. Technical open questions are
// the agent's own to decide and must NOT be escalated. Kept Plane-node-H-scoped
// (added only for Plane projects) since that is where a human product owner reviews.
const plannerProductAskInstruction = `PRODUCT DECISION ESCALATION (only when the spec has a real product question):
As you write the spec, sort every open question into PRODUCT vs TECHNICAL.
- TECHNICAL (which library, how to structure the code, a data shape, error handling, an internal name, anything a competent engineer can just pick): DECIDE it yourself, note it in the spec, and do NOT escalate.
- PRODUCT: a decision only the product owner can make — it changes what the user sees or can do, the product's scope/behavior, or hinges on business intent, a user-facing tradeoff, or an unstated requirement.

The authoritative product spec has already been supplied above. Follow every explicit decision in it. Do NOT reopen or replace its phase order, scope, priority, acceptance criteria, or other user-visible decisions. Do NOT invent optional pricing, packaging, or prioritization questions merely because the product spec does not discuss them.

ONLY when the product spec itself marks a required item unresolved/TBD, or omits information without which implementation is genuinely impossible, emit a "productAsk" field in your final __LOOPER_RESULT__ line. The message asks the product owner to UPDATE THE PRODUCT SPEC before planning continues. Write it in the product owner's own language (match the issue/thread).

FORMATTING — this message is shown as a Feishu card, so it MUST be structured, never one long paragraph. Use lark-markdown: **bold** for every sub-header and for the recommended option; a BLANK LINE between sections; and put each option on ITS OWN line. Do NOT use "#" headers (use **bold** instead). Lay it out EXACTLY like this (keep the bold sub-headers verbatim):

**背景**
<one or two sentences: which user/customer asked for what, and why it matters>

**现状**
<what is already decided or known, so they don't re-litigate it — one short paragraph>

**需要你拍板**
<for EACH product decision, a block shaped like:>
**问题一:<the question in plain words>**
- 选项A:<plain-language option>
- 选项B:<plain-language option>
- 建议:**<your pick>** —— <one short line of why>
<blank line, then 问题二 the same way if there is a second decision>

Hard rules for productAsk:
  - Write like a product manager briefing a business stakeholder, NOT like an engineer. Someone who cannot read code must be able to decide from it alone.
  - NO engineering jargon of ANY kind: no API/endpoint/route, component/module/package/library names, field/column/schema names, CSS/z-index, file paths, or function names. Translate every technical thing into what the user actually experiences. (e.g. "read from the brand endpoint" → "where a brand's info is pulled from"; "the design-system package" → "the brand kit they imported".)
  - Put everything needed to update the product spec INSIDE the message. Do NOT include a tech-spec link.
  - If there is NO product decision (only technical questions, which you resolved yourself), set productAsk to "" — never invent one.

The productAsk value is a JSON string, so encode the line breaks as \n (a blank line between sections is \n\n). Your final marker line carries the extra field:
__LOOPER_RESULT__={"summary":"<one-sentence summary>","productAsk":"<the product-language message with \n line breaks, or empty>"}`

func (r *Runner) runWriteSpecStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if checkpoint.PipelineVersion >= 2 {
		if checkpoint.Decisions == nil || checkpoint.Decisions.Stage != "grilled_final" {
			return checkpoint, &loopError{message: "write-spec blocked: final requirement GRILL has not converged", kind: FailureNonRetryable}
		}
	}
	// Re-read the product spec immediately before authoring. This covers native
	// resume after a product clarification and makes the product page—not a stale
	// checkpoint or the issue body—the planner's current source of truth.
	if gateErr := r.productSpecGate(ctx, input, checkpoint); gateErr != nil {
		return checkpoint, gateErr
	}
	writeSpecCompleted := checkpoint.WriteSpec != nil && strings.EqualFold(checkpoint.WriteSpec.Status, "completed")
	productAsk := ""
	if checkpoint.WriteSpec != nil {
		productAsk = strings.TrimSpace(checkpoint.WriteSpec.ProductAsk)
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktreeRoot, rootErr := plannerWorktreeRoot(input.Project)
	if rootErr != nil {
		return checkpoint, rootErr
	}
	if !plannerCheckpointWorktreeUsable(worktree.Path, input.Project.RepoPath, worktreeRoot) {
		checkpoint.Worktree = nil
		if checkpoint.WriteSpec != nil && !checkpoint.WriteSpec.GitReconciled {
			checkpoint.WriteSpec = nil
			writeSpecCompleted = false
		}
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
	if writeSpecCompleted && checkpoint.WriteSpec.GitReconciled && !(checkpoint.PipelineVersion >= 2 && productAsk != "") {
		return checkpoint, nil
	}
	if !writeSpecCompleted {
		executionID := eventlog.NewEventID("agent")
		agentVendor, agentModel, _, useSnapshot, err := r.identityFromRun(input.Run)
		if err != nil {
			return checkpoint, fmt.Errorf("resolve run agent identity: %w", err)
		}
		prompt, instructionBlock := buildPlannerPrompt(input.Project, r.customInstructions, issue, worktree, r.allowAutoPush, r.disclosure, agentVendor, derefString(agentModel))
		if r.isPlaneProject(input.Project.ID) {
			// On a Plane project the spec lives on a Plane page, never a PR. Some agents
			// still reflexively run `gh pr create`; forbid it explicitly (looper closes a
			// stray one anyway, but the ask avoids the noise).
			prompt += "\n\nCRITICAL — This is a Plane project: the spec will be published to a Plane page by looper, NOT as a pull request. Write ONLY the spec file in the worktree. Do NOT run `gh pr create`, do NOT `git push`, do NOT open or modify any pull request. Any PR you open will be closed as a mistake."
			// Node H: let the agent escalate genuine product decisions to the product
			// owner in product language (via the productAsk marker field).
			if checkpoint.PipelineVersion < 2 {
				prompt += "\n\n" + plannerProductAskInstruction
			} else {
				prompt += "\n\n必须用中文编写研发技术 Spec。需求阶段已经收敛；下面的 Decision Log 是用户行为和角色决策的事实源，必须逐条引用，不得重新发明产品或设计行为。若真实代码调研发现新的阻塞需求问题，不得猜测，也不得继续写 Spec；在最终结果的 productAsk 中用 `RETURN_TO_REQUIREMENTS:` 开头列出问题，让 Looper fail closed 回到需求阶段。\n\n" + decisions.DecisionLogMarkdown(*checkpoint.Decisions)
			}
		}
		metadata := map[string]any{"loopType": "planner", "repo": issue.Repo, "issueNumber": issue.IssueNumber, "specPath": issue.SpecPath}
		for key, value := range config.CustomInstructionMetadata(instructionBlock, prompt) {
			metadata[key] = value
		}
		nativeResumePrompt, nativeSessionID := pendingPlaneDecisionAnswer(input.Loop)
		if !plannerQueueItemIsManual(input.QueueItem) {
			if held, summary, err := r.plannerHoldSummaryForCheckpoint(ctx, input.Project, checkpoint); err != nil {
				return checkpoint, err
			} else if held {
				return checkpoint, &holdSkipError{summary: summary}
			}
		}
		useSnap, snapVendor, snapModel := agentRunSnapshotFields(agentVendor, agentModel, useSnapshot)
		execution, err := r.agentExecutor.Start(ctx, AgentRunInput{
			ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID,
			Prompt: prompt, NativeResumePrompt: nativeResumePrompt, NativeSessionID: nativeSessionID,
			WorkingDirectory: worktree.Path, Timeout: r.agentTimeout, HeartbeatTimeout: r.agentIdleTimeout,
			Metadata: metadata, IdempotencyKey: fmt.Sprintf("planner:%s", input.Loop.ID),
			UseSnapshot: useSnap, SnapshotVendor: snapVendor, SnapshotModel: snapModel,
		})
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
			if result.Interrupted {
				// A human's mid-run @bot killed this agent (强操控) — not a failure. Return
				// the interrupt signal so the run completes "interrupted" and re-dispatches
				// to answer them from the MAIN session (issue/worktree preserved).
				return checkpoint, plannerInterruptError()
			}
			checkpoint.WriteSpec = checkpointWriteSpecFromAgentResult(result)
			checkpoint.ResumePolicy = "retry_from_timeout_context"
			if err := r.persistCheckpoint(ctx, input.Run.ID, stepWriteSpec, checkpoint); err != nil {
				return checkpoint, wrapRetryableAfterResume(err)
			}
			message := firstNonEmpty(result.Summary, result.Stderr, "Planner agent "+result.Status)
			kind := FailureRetryableTransient
			if agent.IsAgentSetupFailureMessage(message) {
				kind = FailureRetryableTransient
			}
			return checkpoint, &loopError{message: message, kind: kind}
		}
		if !plannerQueueItemIsManual(input.QueueItem) {
			if held, summary, err := r.plannerHoldSummaryForCheckpoint(ctx, input.Project, checkpoint); err != nil {
				return checkpoint, err
			} else if held {
				return checkpoint, &holdSkipError{summary: summary}
			}
		}
		checkpoint.WriteSpec = checkpointWriteSpecFromAgentResult(result)
		productAsk = strings.TrimSpace(result.ProductAsk)
		if nativeResumePrompt != "" {
			r.markPlaneDecisionAnswerConsumed(ctx, &input.Loop)
		}
		checkpoint.ensureLifecycle("planner", worktree.Branch, worktree.BaseBranch, true)
		if result.Lifecycle != nil {
			checkpoint.Lifecycle.MergeAgent(result.Lifecycle, r.nowISO())
		} else if len(result.Commits) > 0 {
			checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, result.Commits...)
			checkpoint.Lifecycle.Actions.Commit = lifecycle.ActionSourceAgent
		}
	}
	checkpoint.ensureLifecycle("planner", worktree.Branch, worktree.BaseBranch, true)
	if err := r.persistCheckpoint(ctx, input.Run.ID, stepWriteSpec, checkpoint); err != nil {
		return checkpoint, wrapRetryableAfterResume(err)
	}
	if r.git != nil {
		inspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: worktree.BaseBranch})
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		if inspect.HasUncommittedChanges {
			disclosureAgent, disclosureModel := r.disclosureIdentity(input.Run)
			committed, err := r.git.Commit(ctx, CommitInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, Message: buildPlannerFallbackCommitMessage(issue), DisclosureAgent: disclosureAgent, DisclosureModel: disclosureModel})
			if err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
			}
			if committed.CommitSHA != "" {
				checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, committed.CommitSHA)
			}
			checkpoint.Lifecycle.Actions.Commit = lifecycle.ActionSourceFallback
		} else if len(inspect.NewCommitSHAs) > 0 {
			checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, inspect.NewCommitSHAs...)
			if checkpoint.Lifecycle.Actions.Commit == lifecycle.ActionSourceNone {
				checkpoint.Lifecycle.Actions.Commit = lifecycle.ActionSourceAgent
			}
		}
	}
	checkpoint.WriteSpec.GitReconciled = true
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	if checkpoint.PipelineVersion >= 2 && strings.HasPrefix(productAsk, "RETURN_TO_REQUIREMENTS:") {
		checkpoint = reopenV2Requirements(checkpoint, productAsk)
		// Git reconciliation above leaves the planner-owned spec revision committed
		// and the worktree clean, so the requirement GRILL can safely resume.
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepWriteSpec, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
		return checkpoint, &loopError{message: productAsk, kind: FailureManualIntervention}
	}
	if checkpoint.PipelineVersion >= 2 && productAsk != "" {
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepWriteSpec, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
		return checkpoint, &loopError{message: "write-spec returned a non-empty V2 productAsk without the required RETURN_TO_REQUIREMENTS: prefix; refusing legacy product notification and technical-spec progress", kind: FailureManualIntervention}
	}
	// Node H product-decision hold. Plane owns the answer; Feishu only notifies with
	// the exact comment link. The blocked-condition reconciler observes a later human
	// Plane comment, records it, and native-resumes this same authoring session.
	if productAsk != "" && r.isPlaneProject(input.Project.ID) {
		if _, err := r.requestProductSpecClarificationOnPlane(ctx, input, checkpoint, productAsk); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableTransient}
		}
		r.setNodeHPhase(ctx, input.Loop.ID, "awaiting_product_answer")
		checkpoint.SkipReason = awaitingProductDecisionSkipReason
		return checkpoint, nil
	}
	r.setAwaitingProductAnswerMarker(ctx, input.Loop, false)
	return checkpoint, nil
}

func plannerCheckpointWorktreeUsable(worktreePath, repoPath, worktreeRoot string) bool {
	if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: worktreePath, RepoPath: repoPath, WorktreeRoot: worktreeRoot}); err != nil {
		return false
	}
	info, err := os.Stat(worktreePath)
	return err == nil && info.IsDir()
}

func (r *Runner) runPublishStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if plannerQueueItemIsManual(input.QueueItem) {
		// Hold labels apply only to automatic planner lanes.
	} else if held, summary, err := r.plannerHoldSummaryForCheckpoint(ctx, input.Project, checkpoint); err != nil {
		return checkpoint, err
	} else if held {
		return checkpoint, &holdSkipError{summary: summary}
	}
	// Plane provider: the tech spec lives on a Plane page (nodes G/H) and is reviewed
	// there via page comments — there is NO GitHub spec PR, and nothing is pushed. Take
	// the Plane-only publish path before the push/PR machinery below.
	if r.planeDoc != nil {
		if gw, planeProjectID, ok := r.planeDoc(input.Project.ID); ok && gw != nil {
			return r.runPlanePublishStep(ctx, input, gw, planeProjectID)
		}
	}
	if !r.allowAutoPush {
		message := fmt.Sprintf("Auto push disabled; manual publish required for planner %s", input.Loop.ID)
		checkpoint.SkipReason = message
		checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
		return checkpoint, &loopError{message: message, kind: FailureManualIntervention}
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
		worktreeRoot, rootErr := plannerWorktreeRoot(input.Project)
		if rootErr != nil {
			return checkpoint, rootErr
		}
		if err := r.git.Push(ctx, PushInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, Branch: worktree.Branch, ProtectedBranches: []string{worktree.BaseBranch}}); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		checkpoint.Publish.Pushed = true
		checkpoint.ensureLifecycle("planner", worktree.Branch, worktree.BaseBranch, true)
		checkpoint.Lifecycle.Actions.Push = lifecycle.ActionSourceFallback
		checkpoint.Lifecycle.Pushed = true
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
	}
	if plannerQueueItemIsManual(input.QueueItem) {
		// Phase 2 applies only to automatic planner lanes.
	} else if held, summary, err := r.plannerHoldSummaryForCheckpoint(ctx, input.Project, checkpoint); err != nil {
		return checkpoint, err
	} else if held {
		return checkpoint, &holdSkipError{summary: summary}
	}
	if checkpoint.Publish.PullRequest == nil {
		if checkpoint.Lifecycle != nil && checkpoint.Lifecycle.PRNumber > 0 {
			adopted, err := r.validatedLifecyclePullRequest(ctx, input, *issue, *worktree, checkpoint.Lifecycle)
			if err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
			}
			if adopted != nil {
				if held, summary, err := r.plannerAdoptionHoldSummary(ctx, input.Project, checkpoint, issue.Repo, adopted.Number, input.QueueItem); err != nil {
					return checkpoint, err
				} else if held {
					return checkpoint, &holdSkipError{summary: summary}
				}
				if err := r.normalizePullRequestDisclosure(ctx, input.Run, issue.Repo, adopted.Number, input.Project.RepoPath, true); err != nil {
					return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
				}
				checkpoint.Publish.PullRequest = adopted
				checkpoint.Lifecycle.PRNumber = adopted.Number
				checkpoint.Lifecycle.PRURL = adopted.URL
				checkpoint.Lifecycle.PRAdopted = true
				checkpoint.Lifecycle.Actions.PR = lifecycle.ActionSourceAgent
				if err := r.persistPlannerPullRequestReference(ctx, input, *issue, *worktree, *adopted); err != nil {
					return checkpoint, wrapRetryableAfterResume(err)
				}
				if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
					return checkpoint, wrapRetryableAfterResume(err)
				}
			} else {
				checkpoint.Lifecycle.PRNumber = 0
				checkpoint.Lifecycle.PRURL = ""
				checkpoint.Lifecycle.PRAdopted = false
				checkpoint.Lifecycle.Actions.PR = lifecycle.ActionSourceNone
			}
		}
	}
	if checkpoint.Publish.PullRequest == nil {
		adopted, err := r.findOpenPullRequestForBranch(ctx, issue.Repo, worktree.Branch, worktree.BaseBranch, input.Project.RepoPath)
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		if adopted != nil {
			if held, summary, err := r.plannerAdoptionHoldSummary(ctx, input.Project, checkpoint, issue.Repo, adopted.Number, input.QueueItem); err != nil {
				return checkpoint, err
			} else if held {
				return checkpoint, &holdSkipError{summary: summary}
			}
			if err := r.normalizePullRequestDisclosure(ctx, input.Run, issue.Repo, adopted.Number, input.Project.RepoPath, false); err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
			}
			checkpoint.Publish.PullRequest = &checkpointPullRequest{Number: adopted.Number, URL: adopted.URL, Body: ""}
			checkpoint.ensureLifecycle("planner", worktree.Branch, worktree.BaseBranch, true)
			checkpoint.Lifecycle.PRNumber = adopted.Number
			checkpoint.Lifecycle.PRURL = adopted.URL
			checkpoint.Lifecycle.PRAdopted = true
			checkpoint.Lifecycle.Actions.PR = lifecycle.ActionSourceAgent
			if err := r.persistPlannerPullRequestReference(ctx, input, *issue, *worktree, *checkpoint.Publish.PullRequest); err != nil {
				return checkpoint, wrapRetryableAfterResume(err)
			}
			if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
				return checkpoint, wrapRetryableAfterResume(err)
			}
		}
	}
	if checkpoint.Publish.PullRequest == nil {
		body := buildPullRequestBody(*issue, *worktree, checkpoint.WriteSpec)
		disclosureAgent, disclosureModel := r.disclosureIdentity(input.Run)
		pr, err := r.github.CreatePullRequest(ctx, CreatePullRequestInput{Repo: issue.Repo, HeadBranch: worktree.Branch, BaseBranch: worktree.BaseBranch, Title: "Spec: " + issue.Title, Body: body, CWD: input.Project.RepoPath, DisclosureAgent: disclosureAgent, DisclosureModel: disclosureModel})
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		if pr.Number == 0 {
			return checkpoint, &loopError{message: "Planner publish requires a pull request number", kind: FailureRetryableAfterResume}
		}
		checkpoint.Publish.PullRequest = &checkpointPullRequest{Number: pr.Number, URL: pr.URL, Body: body}
		checkpoint.ensureLifecycle("planner", worktree.Branch, worktree.BaseBranch, true)
		checkpoint.Lifecycle.PRNumber = pr.Number
		checkpoint.Lifecycle.PRURL = pr.URL
		checkpoint.Lifecycle.Actions.PR = lifecycle.ActionSourceFallback
		if err := r.persistPlannerPullRequestReference(ctx, input, *issue, *worktree, checkpointPullRequest{Number: pr.Number, URL: pr.URL, Body: body}); err != nil {
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
	if plannerQueueItemIsManual(input.QueueItem) {
		// Hold labels apply only to automatic planner lanes.
	} else if held, summary, err := r.plannerHoldSummaryForCheckpoint(ctx, input.Project, checkpoint); err != nil {
		return checkpoint, err
	} else if held {
		return checkpoint, &holdSkipError{summary: summary}
	}
	// Flowchart node G: publish the tech spec to Plane as soon as the PR exists, before
	// the label/reviewer steps (so it lands even if those retry). Best-effort + idempotent.
	if err := r.publishTechSpecToPlane(ctx, input, *issue, *worktree); err != nil && r.logger != nil {
		r.logger.Warn("planner: publish tech spec to Plane failed", map[string]any{"projectId": input.Project.ID, "error": err.Error()})
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
		// A Plane assignee is a UUID, not a GitHub login — never request it as a
		// GitHub reviewer (it would 422). Skip UUID-shaped values.
		if looksLikeUUID(reviewer) {
			continue
		}
		if !stringInSlice(reviewer, checkpoint.Publish.ReviewersAdded) {
			pendingReviewers = append(pendingReviewers, reviewer)
		}
	}
	if len(pendingReviewers) > 0 {
		// Best-effort: requesting a reviewer can legitimately fail (e.g. a Plane
		// assignee is a UUID, not a GitHub collaborator) and must NOT wedge the spec
		// PR by retrying forever. Log and move on — the PR is opened and reviewable.
		if err := r.github.AddPullRequestReviewers(ctx, PullRequestReviewersInput{Repo: issue.Repo, PRNumber: pr.Number, Reviewers: pendingReviewers, CWD: input.Project.RepoPath}); err != nil {
			if r.logger != nil {
				r.logger.Warn("planner: request reviewers failed (continuing)", map[string]any{"repo": issue.Repo, "pr": pr.Number, "reviewers": pendingReviewers, "error": err.Error()})
			}
		}
		checkpoint.Publish.ReviewersAdded = append(checkpoint.Publish.ReviewersAdded, pendingReviewers...)
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
	}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

// runGrillStep is flowchart node H's first mandatory gate (Plane-only): a fresh
// adversarial agent interrogates the tech spec on its Plane page, reads the real
// source behind any claim, and REVISES the page to converge it — anything it genuinely
// can't decide is left as an open question for a human. No-op for GitHub/forgejo
// (their spec is the PR, reviewed there) and idempotent on resume.
func (r *Runner) runGrillStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if r.planeDoc == nil {
		return checkpoint, nil
	}
	gateway, planeProjectID, ok := r.planeDoc(input.Project.ID)
	if !ok || gateway == nil {
		return checkpoint, nil
	}
	if checkpoint.Publish != nil && checkpoint.Publish.Grilled {
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
	if r.projectRoleConfig == nil || strings.TrimSpace(config.ProjectOwnerActor(*r.projectRoleConfig, input.Project.ID).PlaneID) == "" {
		return checkpoint, &loopError{message: "node H: Looper owner 缺少 planeId 配置；无法安全开放技术 Spec 审批", kind: FailureManualIntervention}
	}
	specURL, found, err := gateway.FindSpecLink(ctx, planeProjectID, planedoc.WorkItemIDFromURL(issue.URL), planedoc.TechSpecLinkTitle)
	if err != nil {
		return checkpoint, &loopError{message: fmt.Sprintf("grill: find spec link: %v", err), kind: FailureRetryableTransient}
	}
	if !found {
		return checkpoint, nil // nothing to grill
	}
	if checkpoint.Publish == nil {
		checkpoint.Publish = &checkpointPublishState{}
	}
	r.setNodeHPhase(ctx, input.Loop.ID, "grilling")
	specPath := firstNonEmpty(worktree.SpecPath, issue.SpecPath)
	decisionContext := ""
	if checkpoint.PipelineVersion >= 2 && checkpoint.Decisions != nil {
		decisionContext = decisions.DecisionLogMarkdown(*checkpoint.Decisions)
	}
	baselineHead := strings.TrimSpace(checkpoint.Publish.GrillBaselineHead)
	result := AgentResult{Status: "completed", ProductAsk: checkpoint.Publish.GrillProductAsk, Summary: checkpoint.Publish.GrillSummary}
	if !checkpoint.Publish.GrillAgentCompleted {
		baselineHead, err = r.requirementWorktreeHead(ctx, input.Project, *worktree)
		if err != nil {
			return checkpoint, &loopError{message: "grill: establish clean worktree baseline: " + err.Error(), kind: FailureRetryableAfterResume}
		}
		prompt := buildGrillPrompt(*issue, specPath, decisionContext)
		result, err = r.runPlannerAgent(ctx, input, worktree.Path, prompt, "grill")
		if err != nil {
			return checkpoint, err
		}
		if !strings.EqualFold(result.Status, "completed") {
			if result.Interrupted {
				return checkpoint, plannerInterruptError() // 强操控: answer the human from the main session
			}
			checkpoint.ResumePolicy = "retry_from_timeout_context"
			return checkpoint, &loopError{message: "grill agent did not complete", kind: FailureRetryableAfterResume}
		}
		worktreeRoot, rootErr := plannerWorktreeRoot(input.Project)
		if rootErr != nil {
			return checkpoint, rootErr
		}
		postAgentInspect, inspectErr := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: baselineHead})
		if inspectErr != nil {
			return checkpoint, &loopError{message: "grill: inspect post-agent HEAD: " + inspectErr.Error(), kind: FailureRetryableAfterResume}
		}
		if strings.TrimSpace(postAgentInspect.HeadSHA) != baselineHead {
			return checkpoint, &loopError{message: "grill agent created commits; only an uncommitted spec-file edit is allowed", kind: FailureManualIntervention}
		}
		revised, readErr := readPlannerSpecFile(worktree.Path, specPath)
		if readErr != nil || strings.TrimSpace(revised) == "" {
			return checkpoint, &loopError{message: "grill: cannot checkpoint revised spec before commit", kind: FailureRetryableAfterResume}
		}
		sum := sha256.Sum256([]byte(revised))
		checkpoint.Publish.GrillAgentCompleted = true
		checkpoint.Publish.GrillBaselineHead = baselineHead
		checkpoint.Publish.GrillProductAsk = strings.TrimSpace(result.ProductAsk)
		checkpoint.Publish.GrillSummary = result.Summary
		checkpoint.Publish.GrillSpecHash = fmt.Sprintf("%x", sum[:])
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepGrill, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
	} else if baselineHead == "" || strings.TrimSpace(checkpoint.Publish.GrillSpecHash) == "" {
		return checkpoint, &loopError{message: "grill: durable agent checkpoint is incomplete", kind: FailureManualIntervention}
	}
	currentHead, err := r.verifyDurableGrillSpec(ctx, input.Project, *worktree, specPath, checkpoint.Publish.GrillSpecHash, checkpoint.Publish.GrillGitReconciled)
	if err != nil {
		return checkpoint, err
	}
	if checkpoint.Publish.GrillGitReconciled && strings.TrimSpace(checkpoint.Publish.GrillReconciledHead) != currentHead {
		return checkpoint, &loopError{message: "grill: HEAD changed after the durable GRILL commit", kind: FailureManualIntervention}
	}
	if !checkpoint.Publish.GrillGitReconciled {
		checkpoint, err = r.commitGrillSpecRevision(ctx, input, checkpoint, *issue, *worktree, specPath, baselineHead)
		if err != nil {
			return checkpoint, err
		}
		currentHead, err = r.verifyDurableGrillSpec(ctx, input.Project, *worktree, specPath, checkpoint.Publish.GrillSpecHash, true)
		if err != nil {
			return checkpoint, err
		}
		checkpoint.Publish.GrillGitReconciled = true
		checkpoint.Publish.GrillReconciledHead = currentHead
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepGrill, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
	}
	grillProductAsk := strings.TrimSpace(checkpoint.Publish.GrillProductAsk)
	if checkpoint.PipelineVersion >= 2 && strings.HasPrefix(grillProductAsk, "RETURN_TO_REQUIREMENTS:") {
		checkpoint = reopenV2Requirements(checkpoint, grillProductAsk)
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepGrill, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
		return checkpoint, &loopError{message: grillProductAsk, kind: FailureManualIntervention}
	}
	if checkpoint.PipelineVersion >= 2 && grillProductAsk != "" {
		return checkpoint, &loopError{message: "grill returned a non-empty V2 productAsk without the required RETURN_TO_REQUIREMENTS: prefix; refusing technical-spec progress", kind: FailureManualIntervention}
	}
	// The grill revised the spec FILE in the worktree (sandbox-safe); re-publish it to
	// the Plane page so the page reflects the converged spec.
	if revised, rErr := readPlannerSpecFile(worktree.Path, specPath); rErr == nil && strings.TrimSpace(revised) != "" {
		if err := gateway.UpdatePageContent(ctx, planeProjectID, planedoc.PageIDFromURL(specURL), revised); err != nil {
			// The human approves the Plane page, while the revision hash is computed
			// from this local file. Never open approval for two different documents.
			return checkpoint, &loopError{message: "grill: re-publish revised spec to Plane: " + err.Error(), kind: FailureRetryableTransient}
		}
		pageContent, readPageErr := gateway.PageContent(ctx, planeProjectID, planedoc.PageIDFromURL(specURL))
		if readPageErr != nil || strings.TrimSpace(pageContent) == "" {
			message := "grill: read back published Plane spec"
			if readPageErr != nil {
				message += ": " + readPageErr.Error()
			} else {
				message += ": page is empty"
			}
			return checkpoint, &loopError{message: message, kind: FailureRetryableTransient}
		}
		checkpoint.Publish.ReviewPlaneContentHash = contentSHA256(pageContent)
	} else {
		message := "grill: revised spec is empty"
		if rErr != nil {
			message = "grill: read revised spec: " + rErr.Error()
		}
		return checkpoint, &loopError{message: message, kind: FailureRetryableAfterResume}
	}
	// Record the grill transcript on the spec page (signed) so the challenge/fix trail
	// sits next to the spec for the human reviewer.
	if summary := cleanAgentSummary(checkpoint.Publish.GrillSummary); summary != "" {
		body := planedoc.SignComment("<p>🔬 <b>GRILL 拷问结论</b></p><p>"+htmlEscape(truncateRunes(summary, 1500))+"</p>", "grill", derefString(r.agentModel))
		if err := gateway.CommentOnPageURL(ctx, planeProjectID, specURL, body); err != nil && r.logger != nil {
			r.logger.Warn("grill: post transcript failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
		}
	}
	checkpoint.Publish.Grilled = true
	r.setNodeHPhase(ctx, input.Loop.ID, "reviewing")
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) commitGrillSpecRevision(ctx context.Context, input stepInput, checkpoint plannerCheckpoint, issue checkpointIssue, worktree checkpointWorktree, specPath, baselineHead string) (plannerCheckpoint, error) {
	if r.git == nil {
		return checkpoint, &loopError{message: "grill: git gateway unavailable for worktree guard", kind: FailureRetryableAfterResume}
	}
	worktreeRoot, err := plannerWorktreeRoot(input.Project)
	if err != nil {
		return checkpoint, err
	}
	inspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: baselineHead})
	if err != nil {
		return checkpoint, &loopError{message: "grill: inspect revised spec: " + err.Error(), kind: FailureRetryableAfterResume}
	}
	if strings.TrimSpace(inspect.HeadSHA) != strings.TrimSpace(baselineHead) {
		// A previous attempt may have completed Looper's fallback commit and then
		// failed to persist GrillGitReconciled. The pre-commit checkpoint binds that
		// intent to the exact revised spec bytes, so a clean worktree with the same
		// bytes is safe to recover without rerunning the authority-bearing GRILL.
		if checkpoint.Publish != nil && checkpoint.Publish.GrillAgentCompleted && !inspect.HasUncommittedChanges && onlyExpectedCommittedFile(inspect.CommittedChangedFiles, specPath) {
			revised, readErr := readPlannerSpecFile(worktree.Path, specPath)
			if readErr == nil {
				sum := sha256.Sum256([]byte(revised))
				if fmt.Sprintf("%x", sum[:]) == strings.TrimSpace(checkpoint.Publish.GrillSpecHash) && strings.TrimSpace(inspect.HeadSHA) != "" {
					checkpoint.ensureLifecycle("planner", worktree.Branch, worktree.BaseBranch, true)
					checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, strings.TrimSpace(inspect.HeadSHA))
					checkpoint.Lifecycle.Actions.Commit = lifecycle.ActionSourceFallback
					return checkpoint, nil
				}
			}
		}
		return checkpoint, &loopError{message: "grill agent created commits; only an uncommitted spec-file edit is allowed", kind: FailureManualIntervention}
	}
	if !inspect.HasUncommittedChanges {
		return checkpoint, nil
	}
	want := filepath.ToSlash(filepath.Clean(strings.TrimSpace(specPath)))
	for _, changed := range inspect.ChangedFiles {
		got := filepath.ToSlash(filepath.Clean(strings.TrimSpace(changed)))
		if got != want {
			return checkpoint, &loopError{message: fmt.Sprintf("grill agent modified %q; only %q is allowed", changed, specPath), kind: FailureManualIntervention}
		}
	}
	if len(inspect.ChangedFiles) == 0 {
		return checkpoint, &loopError{message: "grill worktree is dirty but changed files are unavailable", kind: FailureManualIntervention}
	}
	committed, err := r.git.Commit(ctx, CommitInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, Message: "planner: apply GRILL revision for " + strings.TrimSpace(issue.Title)})
	if err != nil {
		return checkpoint, &loopError{message: "grill: commit revised spec: " + err.Error(), kind: FailureRetryableAfterResume}
	}
	if strings.TrimSpace(committed.CommitSHA) == "" {
		return checkpoint, &loopError{message: "grill: commit returned no SHA", kind: FailureRetryableAfterResume}
	}
	checkpoint.ensureLifecycle("planner", worktree.Branch, worktree.BaseBranch, true)
	checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, committed.CommitSHA)
	checkpoint.Lifecycle.Actions.Commit = lifecycle.ActionSourceFallback
	return checkpoint, nil
}

func (r *Runner) verifyDurableGrillSpec(ctx context.Context, project storage.ProjectRecord, worktree checkpointWorktree, specPath, expectedHash string, requireClean bool) (string, error) {
	if r.git == nil {
		return "", &loopError{message: "grill: git gateway unavailable for durable checkpoint verification", kind: FailureRetryableAfterResume}
	}
	revised, err := readPlannerSpecFile(worktree.Path, specPath)
	if err != nil {
		return "", &loopError{message: "grill: read durable revised spec: " + err.Error(), kind: FailureRetryableAfterResume}
	}
	sum := sha256.Sum256([]byte(revised))
	if fmt.Sprintf("%x", sum[:]) != strings.TrimSpace(expectedHash) {
		return "", &loopError{message: "grill: revised spec bytes no longer match the durable agent checkpoint", kind: FailureManualIntervention}
	}
	worktreeRoot, err := plannerWorktreeRoot(project)
	if err != nil {
		return "", err
	}
	inspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: worktree.BaseBranch})
	if err != nil {
		return "", &loopError{message: "grill: inspect durable revised spec: " + err.Error(), kind: FailureRetryableAfterResume}
	}
	// Before reconciliation, the expected spec-only edit may still be dirty. Once
	// GrillGitReconciled is true callers separately skip commit and require clean.
	if checkpointPathDirtyOutsideSpec(inspect, specPath) {
		return "", &loopError{message: "grill: durable checkpoint contains changes outside the spec file", kind: FailureManualIntervention}
	}
	if requireClean && inspect.HasUncommittedChanges {
		return "", &loopError{message: "grill: git-reconciled durable checkpoint unexpectedly has uncommitted changes", kind: FailureManualIntervention}
	}
	return strings.TrimSpace(inspect.HeadSHA), nil
}

func onlyExpectedCommittedFile(files []string, specPath string) bool {
	if len(files) != 1 {
		return false
	}
	want := filepath.ToSlash(filepath.Clean(strings.TrimSpace(specPath)))
	return filepath.ToSlash(filepath.Clean(strings.TrimSpace(files[0]))) == want
}

func checkpointPathDirtyOutsideSpec(inspect InspectHeadResult, specPath string) bool {
	if !inspect.HasUncommittedChanges {
		return false
	}
	want := filepath.ToSlash(filepath.Clean(strings.TrimSpace(specPath)))
	if len(inspect.ChangedFiles) == 0 {
		return true
	}
	for _, changed := range inspect.ChangedFiles {
		if filepath.ToSlash(filepath.Clean(strings.TrimSpace(changed))) != want {
			return true
		}
	}
	return false
}

// runReviewStep is node H's second mandatory gate (Plane-only): an INDEPENDENT reviewer
// agent (a different pass from the grill) re-reviews the converged spec and posts a
// verdict, then opens the human-approve gate — marks the card 需要人类审核 spec and lets
// the reconcile poll the page for a human's approve. Idempotent; no-op for non-Plane.
func (r *Runner) runReviewStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if r.planeDoc == nil {
		return checkpoint, nil
	}
	gateway, planeProjectID, ok := r.planeDoc(input.Project.ID)
	if !ok || gateway == nil {
		return checkpoint, nil
	}
	if checkpoint.Publish != nil && checkpoint.Publish.Reviewed {
		return checkpoint, nil
	}
	if checkpoint.Publish == nil {
		checkpoint.Publish = &checkpointPublishState{}
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	if r.projectRoleConfig == nil || strings.TrimSpace(config.ProjectOwnerActor(*r.projectRoleConfig, input.Project.ID).PlaneID) == "" {
		return checkpoint, &loopError{message: "review: Looper owner 缺少 planeId 配置；无法安全开放技术 Spec 审批", kind: FailureManualIntervention}
	}
	specURL, found, err := gateway.FindSpecLink(ctx, planeProjectID, planedoc.WorkItemIDFromURL(issue.URL), planedoc.TechSpecLinkTitle)
	if err != nil {
		return checkpoint, &loopError{message: fmt.Sprintf("review: find spec link: %v", err), kind: FailureRetryableTransient}
	}
	if !found {
		return checkpoint, nil
	}
	r.setNodeHPhase(ctx, input.Loop.ID, "reviewing")
	decisionContext := ""
	if checkpoint.PipelineVersion >= 2 && checkpoint.Decisions != nil {
		decisionContext = decisions.DecisionLogMarkdown(*checkpoint.Decisions)
	}
	// Older/incomplete checkpoints may not yet carry the rendered Plane-page
	// receipt. Re-publish the reviewed local source before starting a fresh REVIEW
	// so local and remote are known to describe the same revision. If the page was
	// edited after GRILL, this deliberately restores the converged source and the
	// new REVIEW evaluates it from scratch.
	if strings.TrimSpace(checkpoint.Publish.ReviewPlaneContentHash) == "" {
		localSpec, readErr := readPlannerSpecFile(worktree.Path, firstNonEmpty(worktree.SpecPath, issue.SpecPath))
		if readErr != nil || strings.TrimSpace(localSpec) == "" {
			return checkpoint, &loopError{message: "review: cannot establish published revision from local spec", kind: FailureRetryableAfterResume}
		}
		pageID := planedoc.PageIDFromURL(specURL)
		if err := gateway.UpdatePageContent(ctx, planeProjectID, pageID, localSpec); err != nil {
			return checkpoint, &loopError{message: "review: re-publish local spec before REVIEW: " + err.Error(), kind: FailureRetryableTransient}
		}
		pageContent, pageErr := gateway.PageContent(ctx, planeProjectID, pageID)
		if pageErr != nil || strings.TrimSpace(pageContent) == "" {
			return checkpoint, &loopError{message: "review: read published revision before REVIEW", kind: FailureRetryableTransient}
		}
		checkpoint.Publish.ReviewPlaneContentHash = contentSHA256(pageContent)
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepReview, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
	}
	prompt := buildReviewPrompt(*issue, firstNonEmpty(worktree.SpecPath, issue.SpecPath), decisionContext)
	baselineHead, err := r.requirementWorktreeHead(ctx, input.Project, *worktree)
	if err != nil {
		return checkpoint, &loopError{message: "review: establish read-only worktree baseline: " + err.Error(), kind: FailureRetryableAfterResume}
	}
	result, err := r.runPlannerAgent(ctx, input, worktree.Path, prompt, "review")
	if err != nil {
		return checkpoint, err
	}
	if !strings.EqualFold(result.Status, "completed") {
		if result.Interrupted {
			return checkpoint, plannerInterruptError() // 强操控: answer the human from the main session
		}
		checkpoint.ResumePolicy = "retry_from_timeout_context"
		return checkpoint, &loopError{message: "review agent did not complete", kind: FailureRetryableAfterResume}
	}
	if err := r.assertRequirementAgentDidNotEditBusinessRepo(ctx, input.Project, *worktree, baselineHead); err != nil {
		return checkpoint, &loopError{message: "review violated read-only worktree guard: " + err.Error(), kind: FailureManualIntervention}
	}
	verdict, verdictOK := parseSpecReviewVerdict(result.Summary)
	if !verdictOK {
		return checkpoint, &loopError{message: "review agent did not return the required VERDICT: READY or VERDICT: BLOCKED protocol", kind: FailureRetryableAfterResume}
	}
	if verdict == "blocked" {
		message := "technical spec review blocked: " + cleanAgentSummary(result.Summary)
		if checkpoint.PipelineVersion >= 2 && checkpoint.Decisions != nil {
			checkpoint = reopenV2Requirements(checkpoint, "RETURN_TO_REQUIREMENTS: "+message)
			if err := r.persistCheckpoint(ctx, input.Run.ID, stepReview, checkpoint); err != nil {
				return checkpoint, wrapRetryableAfterResume(err)
			}
		}
		return checkpoint, &loopError{message: message, kind: FailureManualIntervention}
	}
	// The reviewer evaluated the local spec. Before recording READY or opening the
	// human gate, prove the Plane page did not change while that reviewer ran.
	pageContent, pageErr := gateway.PageContent(ctx, planeProjectID, planedoc.PageIDFromURL(specURL))
	if pageErr != nil {
		return checkpoint, &loopError{message: "review: re-read published revision after REVIEW: " + pageErr.Error(), kind: FailureRetryableTransient}
	}
	if got, want := contentSHA256(pageContent), strings.TrimSpace(checkpoint.Publish.ReviewPlaneContentHash); strings.TrimSpace(pageContent) == "" || got != want {
		checkpoint.Publish.ReviewPlaneContentHash = ""
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepReview, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
		return checkpoint, &loopError{message: "review: Plane spec changed during independent REVIEW; re-publish and run a fresh REVIEW before approval", kind: FailureRetryableAfterResume}
	}
	summary := cleanAgentSummary(result.Summary)
	body := planedoc.SignComment("<p>👀 <b>REVIEW 复核结论</b></p><p>"+htmlEscape(truncateRunes(summary, 1500))+"</p>", "reviewer", derefString(r.agentModel))
	_, err = gateway.CreateCommentOnPageURL(ctx, planeProjectID, specURL, body)
	if err != nil {
		return checkpoint, &loopError{message: fmt.Sprintf("review: post verdict: %v", err), kind: FailureRetryableTransient}
	}
	approval, err := r.openSpecApprovalRevision(ctx, input, gateway, planeProjectID, specURL, worktree.Path, firstNonEmpty(worktree.SpecPath, issue.SpecPath), checkpoint.Publish.ReviewPlaneContentHash)
	if err != nil {
		return checkpoint, &loopError{message: fmt.Sprintf("review: open revision approval gate: %v", err), kind: FailureRetryableTransient}
	}
	// Persist the local half of the remote approval gate before changing labels or
	// notifying the owner. If this write fails, return retryable: the signed remote
	// request is idempotently rediscovered on the next review pass.
	if r.repos == nil || r.repos.Loops == nil {
		return checkpoint, &loopError{message: "review: loop repository unavailable for approval gate", kind: FailureRetryableAfterResume}
	}
	loop, err := r.repos.Loops.GetByID(ctx, input.Loop.ID)
	if err != nil || loop == nil {
		return checkpoint, &loopError{message: fmt.Sprintf("review: load loop for approval gate: %v", err), kind: FailureRetryableAfterResume}
	}
	metadataJSON, err := mergeLoopMetadataJSON(loop.MetadataJSON, map[string]any{
		"awaitingSpecApproval":         true,
		"specApprovedDispatched":       false,
		"issueUrl":                     issue.URL,
		"specApprovalRevision":         approval.Revision,
		"specApprovalContentHash":      approval.ContentHash,
		"specApprovalRequestCommentID": approval.CommentID,
		"specApprovalRequestedAt":      approval.RequestedAt,
		"specApprovalJudgedHash":       "",
	})
	if err != nil {
		return checkpoint, &loopError{message: "review: encode approval gate metadata: " + err.Error(), kind: FailureRetryableAfterResume}
	}
	if _, err := r.updateLoop(ctx, *loop, func(u *storage.LoopRecord) { u.MetadataJSON = stringPtr(metadataJSON) }); err != nil {
		return checkpoint, &loopError{message: "review: persist approval gate metadata: " + err.Error(), kind: FailureRetryableAfterResume}
	}
	if checkpoint.Publish == nil {
		checkpoint.Publish = &checkpointPublishState{}
	}
	checkpoint.Publish.Reviewed = true
	// node H converged → open the human-approve gate and @-mention the owner in the
	// thread ONCE: review is done, please approve. This review-end ping IS the
	// notification — no timer / periodic nudge.
	r.setNodeHPhase(ctx, input.Loop.ID, "awaiting_human_review")
	// Retire the planner trigger so the planner won't re-discover this item while it
	// awaits approval — the label lifecycle is plan → (approval) → worker-ready.
	if workItemID := planedoc.WorkItemIDFromURL(issue.URL); workItemID != "" {
		if err := gateway.RemoveWorkItemLabel(ctx, planeProjectID, workItemID, discoveryLabel); err != nil && r.logger != nil {
			r.logger.Warn("review: retire looper:plan failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
		}
	}
	approvalActionURL := planedoc.PageCommentURL(specURL, approval.CommentID)
	if approvalActionURL == "" {
		return checkpoint, &loopError{message: "review: Plane approval request did not return a comment id", kind: FailureRetryableTransient}
	}
	r.notifySpecApproval(ctx, input, "技术方案已过 GRILL 拷问 + 独立 REVIEW，请 Looper owner 审批当前修订 v"+strconv.Itoa(approval.Revision)+"；无异议可答 approve / 同意 / 👍，随后进入实现。", approvalActionURL)
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

type specApprovalRevision struct {
	Revision    int
	ContentHash string
	CommentID   string
	RequestedAt string
}

func contentSHA256(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum[:])
}

// openSpecApprovalRevision binds the human gate to the exact spec bytes that passed
// REVIEW. The signed marker makes the remote write idempotent across a crash between
// creating the Plane comment and persisting the loop metadata.
func (r *Runner) openSpecApprovalRevision(ctx context.Context, input stepInput, gateway *planedoc.Gateway, planeProjectID, specURL, worktreePath, specPath, reviewedPageHash string) (specApprovalRevision, error) {
	content, err := readPlannerSpecFile(worktreePath, specPath)
	if err != nil {
		return specApprovalRevision{}, err
	}
	if strings.TrimSpace(content) == "" {
		return specApprovalRevision{}, fmt.Errorf("reviewed spec file is empty")
	}
	// The owner reads and approves the Plane page, so bind the gate to the bytes
	// Plane actually returns—not merely to the local Markdown source used to publish
	// it. A later manual page edit then invalidates this revision at reconcile time.
	pageContent, err := gateway.PageContent(ctx, planeProjectID, planedoc.PageIDFromURL(specURL))
	if err != nil {
		return specApprovalRevision{}, fmt.Errorf("read published spec page: %w", err)
	}
	if strings.TrimSpace(pageContent) == "" {
		return specApprovalRevision{}, fmt.Errorf("published spec page is empty")
	}
	contentHash := contentSHA256(pageContent)
	if reviewedPageHash = strings.TrimSpace(reviewedPageHash); reviewedPageHash == "" || contentHash != reviewedPageHash {
		return specApprovalRevision{}, fmt.Errorf("published spec page no longer matches the independently reviewed revision")
	}
	previousRevision := 0
	if r.repos != nil && r.repos.Loops != nil {
		if loop, getErr := r.repos.Loops.GetByID(ctx, input.Loop.ID); getErr == nil && loop != nil {
			meta := parseJSONObject(loop.MetadataJSON)
			if value, ok := meta["specApprovalRevision"].(float64); ok {
				previousRevision = int(value)
			}
			if oldHash, _ := meta["specApprovalContentHash"].(string); strings.TrimSpace(oldHash) == contentHash && previousRevision > 0 {
				revision := specApprovalRevision{
					Revision: previousRevision, ContentHash: contentHash,
					CommentID:   stringFromAnyDefault(meta["specApprovalRequestCommentID"]),
					RequestedAt: stringFromAnyDefault(meta["specApprovalRequestedAt"]),
				}
				if strings.TrimSpace(revision.CommentID) != "" && strings.TrimSpace(revision.RequestedAt) != "" {
					if _, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(revision.RequestedAt)); parseErr == nil {
						return revision, nil
					}
				}
			}
		}
	}
	revision := previousRevision + 1
	marker := fmt.Sprintf("<!-- looper:spec-approval-request revision=%d hash=%s -->", revision, contentHash)
	comments, err := gateway.ListPageComments(ctx, planeProjectID, planedoc.PageIDFromURL(specURL))
	if err != nil {
		return specApprovalRevision{}, err
	}
	for _, comment := range comments {
		if strings.Contains(comment.CommentHTML, marker) {
			commentID, requestedAt, receiptErr := validatedSpecApprovalReceipt(comment)
			if receiptErr != nil {
				return specApprovalRevision{}, receiptErr
			}
			return specApprovalRevision{Revision: revision, ContentHash: contentHash, CommentID: commentID, RequestedAt: requestedAt}, nil
		}
	}
	body := planedoc.SignComment(marker+"<p>🙋 <b>技术方案修订 v"+strconv.Itoa(revision)+" 等待 Looper owner 审批</b></p><p>本次审批只对当前内容生效（SHA-256: <code>"+contentHash[:12]+"</code>）。请 owner 在本条之后评论 <b>approve / 同意 / 👍</b>；其他角色或旧修订的评论不会触发实现。</p>", "reviewer", derefString(r.agentModel))
	created, err := gateway.CreateCommentOnPageURL(ctx, planeProjectID, specURL, body)
	if err != nil {
		return specApprovalRevision{}, err
	}
	commentID, requestedAt, receiptErr := validatedSpecApprovalReceipt(created)
	if receiptErr != nil {
		return specApprovalRevision{}, receiptErr
	}
	return specApprovalRevision{Revision: revision, ContentHash: contentHash, CommentID: commentID, RequestedAt: requestedAt}, nil
}

func validatedSpecApprovalReceipt(comment planedoc.PageComment) (string, string, error) {
	commentID := strings.TrimSpace(comment.ID)
	requestedAt := strings.TrimSpace(comment.CreatedAt)
	if commentID == "" || requestedAt == "" {
		return "", "", fmt.Errorf("Plane approval request receipt lacked id/created_at")
	}
	if _, parseErr := time.Parse(time.RFC3339Nano, requestedAt); parseErr != nil {
		return "", "", fmt.Errorf("Plane approval request receipt had invalid created_at: %w", parseErr)
	}
	return commentID, requestedAt, nil
}

// runPlannerAgent runs one bounded agent pass in the worktree and returns its result —
// the shared execution path for the grill + review node H gates.
func (r *Runner) runPlannerAgent(ctx context.Context, input stepInput, worktreePath, prompt, phase string) (AgentResult, error) {
	if r.agentExecutor == nil {
		return AgentResult{}, fmt.Errorf("planner agent executor not configured")
	}
	executionID := eventlog.NewEventID("agent")
	metadata := map[string]any{"loopType": "planner", "phase": phase}
	execution, err := r.agentExecutor.Start(ctx, AgentRunInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Prompt: prompt, WorkingDirectory: worktreePath, Timeout: r.agentTimeout, HeartbeatTimeout: r.agentIdleTimeout, Metadata: metadata, IdempotencyKey: fmt.Sprintf("planner:%s:%s", phase, input.Loop.ID)})
	if err != nil {
		return AgentResult{}, err
	}
	return execution.Wait(ctx)
}

// buildGrillPrompt is the fresh adversarial reviewer's brief (node H GRILL). It revises
// the spec FILE in the worktree — never Plane or anything outside the project (the codex
// sandbox blocks external writes); looper re-publishes the revised file to Plane after.
func buildGrillPrompt(issue checkpointIssue, specFilePath, decisionContext string) string {
	parts := []string{
		fmt.Sprintf("You are a FRESH, adversarial technical-spec reviewer for %s#%d — you did NOT write this spec.", issue.Repo, issue.IssueNumber),
		fmt.Sprintf("The tech spec is the file `%s` in this worktree. Read it.", specFilePath),
		"Adversarially interrogate it against the REAL codebase in this worktree: open the actual source at any file:line it cites; if a claim can't be verified from real code, flag it '⚠️ 未经源码验证'. Never pretend to have read code you didn't.",
		"Hunt for: missing/weak acceptance criteria, unhandled edge cases, security/data/migration/concurrency risk, hand-wavy steps, scope creep, and anything a worker couldn't implement unambiguously.",
		fmt.Sprintf("REVISE the spec file `%s` IN PLACE to resolve what you can (keep the structure; tighten, don't bloat). Edit ONLY that file — do NOT commit, do NOT write anything outside this worktree, do NOT run `plane`/`gh`, do NOT push or open a pull request. looper validates, commits, and publishes the file for you.", specFilePath),
		"Keep the entire technical spec in Simplified Chinese; preserve code identifiers, file paths, commands, API names, and other exact technical tokens in their original form.",
		"For anything you genuinely cannot decide (a real product/design/engineering ambiguity), do NOT guess and do NOT let the spec proceed with a merely-labelled open question.",
		"Finish with a concise grill transcript as your final summary: what you challenged, what you fixed in the file, and what remains open.",
	}
	if strings.TrimSpace(decisionContext) != "" {
		parts = append(parts,
			"This V2 pipeline already froze requirements using the Decision Log below. Do not re-litigate a recorded fact or answer. If source inspection uncovers a genuinely new blocking ambiguity, your final line MUST be exactly one structured marker with productAsk beginning RETURN_TO_REQUIREMENTS: and a concise self-contained list of the missing decisions. Otherwise set productAsk to an empty string.",
			decisionContext,
			agent.CompletionMarker+`={"summary":"GRILL summary","productAsk":"RETURN_TO_REQUIREMENTS: ... or empty"}`,
		)
	}
	return strings.Join(parts, "\n")
}

// buildReviewPrompt is the independent reviewer's brief (node H REVIEW) — a different
// pass from the grill, reading the converged spec file and issuing a verdict (no writes).
func buildReviewPrompt(issue checkpointIssue, specFilePath, decisionContext string) string {
	parts := []string{
		fmt.Sprintf("You are an INDEPENDENT spec reviewer for %s#%d, reviewing a tech spec that already passed an adversarial grill.", issue.Repo, issue.IssueNumber),
		fmt.Sprintf("Read the spec file `%s` in this worktree.", specFilePath),
		"Verify against the real codebase in this worktree (open cited source; flag unverified claims). Judge whether a worker could implement it unambiguously and safely.",
		"Verify that the technical spec itself is written in Simplified Chinese, while exact code identifiers, file paths, commands, and API names remain unchanged; treat a language mismatch as a blocker.",
		"This is a READ-ONLY verdict pass: do NOT edit any file, do NOT run `plane`/`gh`, do NOT push or open a pull request. If you find a blocking gap, state it precisely; otherwise confirm it is implementation-ready.",
		"Your final summary MUST begin with exactly `VERDICT: READY` when implementation is unambiguous and safe, or `VERDICT: BLOCKED` followed by the specific blockers. BLOCKED must never open human approval.",
		agent.CompletionMarker + `={"summary":"VERDICT: READY — ... or VERDICT: BLOCKED — ..."}`,
	}
	if strings.TrimSpace(decisionContext) != "" {
		parts = append(parts, "The V2 requirement Decision Log below is authoritative; verify the spec implements it and does not contain unresolved role decisions.", decisionContext)
	}
	return strings.Join(parts, "\n")
}

func parseSpecReviewVerdict(summary string) (string, bool) {
	normalized := strings.ToUpper(strings.TrimSpace(strings.ReplaceAll(summary, "**", "")))
	if strings.HasPrefix(normalized, "VERDICT: READY") {
		return "ready", true
	}
	if strings.HasPrefix(normalized, "VERDICT: BLOCKED") {
		return "blocked", true
	}
	return "", false
}

// htmlEscape escapes text for safe embedding inside a Plane comment's HTML body.
func htmlEscape(s string) string { return htmlpkg.EscapeString(s) }

// codexLogNoise matches codex runtime log lines (timestamp + level) that sometimes
// leak into an agent's summary.
var codexLogNoise = regexp.MustCompile(`(?i)\d{4}-\d{2}-\d{2}T[\d:.]+Z?\s+(?:ERROR|INFO|WARN|DEBUG|TRACE)\s+[^\n]*`)

// bareNumberSummary matches a summary that is ONLY digits + separators (e.g. a stray
// token/byte count like "105,940"). That happens when an agent emits no
// __LOOPER_RESULT__ summary and the fallback grabs its last log line — a naked number,
// never a real grill/review conclusion, so we replace it with a placeholder.
var bareNumberSummary = regexp.MustCompile(`^[\d.,%\s]+$`)

// ansiEscape matches a full ANSI/CSI escape sequence (ESC + [ ... + final byte),
// e.g. "\x1b[0m" (reset), "\x1b[2K" (erase line). These leak into a summary when the
// fallback grabs a terminal-styled log line.
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// csiResidueOnly matches a summary that is NOTHING but CSI residue whose ESC byte was
// already stripped upstream (so it renders as visible junk like "[0m" / "[2K[1G").
// Whole-string only, so legitimate prose containing a bracket is never touched.
var csiResidueOnly = regexp.MustCompile(`^(?:\[[0-9;?]*[a-zA-Z])+$`)

// cleanAgentSummary strips codex log noise (timestamped ERROR/INFO router lines, the
// stdin prompt) and terminal control codes from an agent summary so a grill/review
// transcript reads as prose, not machine logs. Falls back to a placeholder if
// filtering would leave nothing meaningful.
func cleanAgentSummary(s string) string {
	cleaned := ansiEscape.ReplaceAllString(s, " ")
	cleaned = codexLogNoise.ReplaceAllString(cleaned, " ")
	cleaned = strings.ReplaceAll(cleaned, "Reading additional input from stdin...", " ")
	cleaned = strings.TrimSpace(strings.Join(strings.Fields(cleaned), " "))
	if cleaned == "" || bareNumberSummary.MatchString(cleaned) || csiResidueOnly.MatchString(cleaned) {
		// Empty (all log noise), a bare number (a stray token/byte count), or nothing
		// but terminal control residue (e.g. "[0m") — all happen when the agent emits
		// no __LOOPER_RESULT__ summary and the fallback grabs its last log line. None is
		// a conclusion; post a placeholder, never the raw logs / a naked number / a code.
		return "(本轮 agent 未产出可展示的结论)"
	}
	return cleaned
}

// truncateRunes caps s to n runes, appending an ellipsis when it was cut.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// isPlaneProject reports whether the project delegates to Plane (its planeDoc
// resolver resolves), i.e. the tech spec lives on a Plane page rather than a spec PR.
func (r *Runner) isPlaneProject(projectID string) bool {
	if r.planeDoc == nil {
		return false
	}
	_, _, ok := r.planeDoc(projectID)
	return ok
}

// runPlanePublishStep is the Plane-provider publish path (flowchart §需求 side): the
// tech spec is written to a Plane page (node G) and reviewed there via page comments
// (node H) — there is NO GitHub spec PR and nothing is pushed. It publishes the
// agent's spec, verifies it landed (no PR fallback exists on Plane), and completes;
// opening the implementation PR is the worker's job after the spec is approved.
func (r *Runner) runPlanePublishStep(ctx context.Context, input stepInput, gateway *planedoc.Gateway, planeProjectID string) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	workItemID := planedoc.WorkItemIDFromURL(issue.URL)
	if workItemID == "" {
		return checkpoint, &loopError{message: "Plane publish: cannot resolve work item id from " + issue.URL, kind: FailureManualIntervention}
	}
	// node G: write the tech spec to a Plane page + link it (idempotent).
	if err := r.publishTechSpecToPlane(ctx, input, *issue, *worktree); err != nil {
		return checkpoint, &loopError{message: fmt.Sprintf("publish tech spec to Plane: %v", err), kind: FailureRetryableTransient}
	}
	// Verify it actually landed: the agent must have written a spec file. Without the
	// tech-spec link there is nothing to review and — unlike the GitHub path — no PR
	// to fall back to, so hold for a human rather than silently completing empty.
	specURL, found, err := gateway.FindSpecLink(ctx, planeProjectID, workItemID, planedoc.TechSpecLinkTitle)
	if err != nil {
		return checkpoint, &loopError{message: fmt.Sprintf("verify tech spec link: %v", err), kind: FailureRetryableTransient}
	}
	if !found {
		return checkpoint, &loopError{message: "planner produced no tech spec to publish to Plane (agent wrote no spec file)", kind: FailureManualIntervention}
	}
	// Retire any PR an agent opened on this branch despite instructions — on a Plane
	// project the spec is the page, not a PR, so a planner-branch PR is a stray.
	if r.github != nil {
		if stray, sErr := r.findOpenPullRequestForBranch(ctx, issue.Repo, worktree.Branch, worktree.BaseBranch, input.Project.RepoPath); sErr == nil && stray != nil && stray.Number > 0 {
			if err := r.github.ClosePullRequest(ctx, ClosePullRequestInput{Repo: issue.Repo, PRNumber: stray.Number, CWD: input.Project.RepoPath}); err != nil {
				if r.logger != nil {
					r.logger.Warn("plane publish: close stray agent PR failed (continuing)", map[string]any{"repo": issue.Repo, "pr": stray.Number, "error": err.Error()})
				}
			} else if r.logger != nil {
				r.logger.Info("plane publish: closed stray agent-opened PR", map[string]any{"repo": issue.Repo, "pr": stray.Number})
			}
		}
	}
	// Leave a neutral [looper] status note on the spec page — the tech-spec draft has
	// landed and is entering review (node H). The GRILL + owner approval is a later
	// stage; this note is only an audit breadcrumb,
	// not an approve invitation. Idempotent + best-effort — a failed note must not wedge
	// the planner (the tech-spec link is the real discovery signal).
	noteHTML := planedoc.SignComment("<p>技术方案初稿已写到本页，正在进入 GRILL 拷问与独立 REVIEW。当前尚未开放审批；评审通过后 Looper 会另发带修订哈希的 owner 审批请求。</p>", "planner", strings.TrimSpace(derefString(r.agentModel)))
	if _, err := gateway.PostSpecReviewComment(ctx, planeProjectID, specURL, noteHTML); err != nil && r.logger != nil {
		r.logger.Warn("planner: post spec status note failed (continuing)", map[string]any{"projectId": input.Project.ID, "page": specURL, "error": err.Error()})
	}
	if checkpoint.Publish == nil {
		checkpoint.Publish = &checkpointPublishState{}
	}
	checkpoint.Publish.PlaneSpecReview = true
	// node H condition #2: surface the spec DRAFT in the Feishu thread as an FYI —
	// the local Looper owner is mentioned only after grill/review open the actionable
	// approval gate.
	r.postNodeHThreadNote(ctx, input, "specDraftFYI", "📋 技术方案初稿已出炉:"+specURL+"\n即将进入 fresh agent 拷问（GRILL）+ 独立复核（REVIEW）。当前无需操作；通过后 Looper 会另行 @ owner 审批当前修订。", false)
	// node H begins here: the tech spec is on Plane. The grill step runs next; only
	// after grill + review converge does the loop open the human-approve gate. Mark the
	// card so it reads 🔬 方案拷问中 instead of resting on 编写技术方案中.
	r.setNodeHPhase(ctx, input.Loop.ID, "grilling")
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

// postNodeHThreadNote posts a node H touchpoint into the loop's Feishu thread,
// @-mentioning the project's local Looper owner for the actionable tech-spec approval.
// Draft/grill notes remain informational. Best-effort — a missing transport or owner
// just skips the ping.
func (r *Runner) postNodeHThreadNote(ctx context.Context, input stepInput, dedupKey, text string, mention bool) {
	if r.postThreadNote == nil && r.postThreadNoteWithUUID == nil {
		return
	}
	// Post each node-H thread note at most ONCE per loop. A follow-up resume re-enters
	// the pipeline (线程永远可追问); without this the draft-FYI / grill / approval notes
	// would re-post and spam the thread on every re-run. dedupKey "" opts out.
	if r.nodeHNotePosted(ctx, input.Loop.ID, dedupKey) {
		return
	}
	// Only @-mention on the ACTIONABLE post (please approve); the draft FYI and grill
	// transcript are informational and shouldn't ping a human every time.
	var mentions []string
	if mention && r.projectRoleConfig != nil {
		if openID := strings.TrimSpace(config.ProjectOwner(*r.projectRoleConfig, input.Project.ID)); openID != "" {
			mentions = []string{openID}
		}
	}
	var err error
	if r.postThreadNoteWithUUID != nil && strings.TrimSpace(dedupKey) != "" {
		sum := sha256.Sum256([]byte("node-h-note:" + input.Loop.ID + ":" + dedupKey))
		err = r.postThreadNoteWithUUID(ctx, input.Loop.ID, text, mentions, fmt.Sprintf("%x", sum[:16]))
	} else if r.postThreadNote != nil {
		err = r.postThreadNote(ctx, input.Loop.ID, text, mentions)
	}
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("planner: post node H thread note failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
		}
		return
	}
	r.markNodeHNotePosted(ctx, input.Loop.ID, dedupKey)
}

// nodeHNotesPostedSet reads the set of node-H note keys already posted for a loop from
// its metadata (`nodeHNotesPosted` string list).
func nodeHNotesPostedSet(metadataJSON *string) map[string]bool {
	out := map[string]bool{}
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return out
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return out
	}
	raw, ok := meta["nodeHNotesPosted"].([]any)
	if !ok {
		return out
	}
	for _, v := range raw {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out[s] = true
		}
	}
	return out
}

// nodeHNotePosted reports whether the node-H note keyed by dedupKey was already posted
// for this loop. dedupKey "" is never deduped; a read error reports "not posted" so the
// note still goes out rather than being silently dropped.
func (r *Runner) nodeHNotePosted(ctx context.Context, loopID, dedupKey string) bool {
	if strings.TrimSpace(dedupKey) == "" || r.repos == nil || r.repos.Loops == nil {
		return false
	}
	loop, err := r.repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return false
	}
	return nodeHNotesPostedSet(loop.MetadataJSON)[dedupKey]
}

// markNodeHNotePosted records that the node-H note keyed by dedupKey has been posted for
// this loop, re-reading + merging so it doesn't clobber markers set by earlier steps.
func (r *Runner) markNodeHNotePosted(ctx context.Context, loopID, dedupKey string) {
	if strings.TrimSpace(dedupKey) == "" || r.repos == nil || r.repos.Loops == nil {
		return
	}
	loop, err := r.repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return
	}
	posted := nodeHNotesPostedSet(loop.MetadataJSON)
	posted[dedupKey] = true
	keys := make([]string, 0, len(posted))
	for k := range posted {
		keys = append(keys, k)
	}
	metadataJSON, err := mergeLoopMetadataJSON(loop.MetadataJSON, map[string]any{"nodeHNotesPosted": keys})
	if err != nil {
		return
	}
	if _, err := r.updateLoop(ctx, *loop, func(u *storage.LoopRecord) { u.MetadataJSON = stringPtr(metadataJSON) }); err != nil && r.logger != nil {
		r.logger.Warn("planner: mark node H note posted failed", map[string]any{"loopId": loopID, "key": dedupKey, "error": err.Error()})
	}
}

// setNodeHPhase records the planner's node H spec-pipeline phase in loop metadata
// (authoring / grilling / reviewing / awaiting_human_review) so the anchor card names
// the exact sub-phase. Best-effort + guarded — re-reads the current loop so it merges
// with markers set by earlier steps rather than clobbering them.
func (r *Runner) setNodeHPhase(ctx context.Context, loopID, phase string) {
	if r.repos == nil || r.repos.Loops == nil {
		return
	}
	loop, err := r.repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return
	}
	metadataJSON, err := mergeLoopMetadataJSON(loop.MetadataJSON, map[string]any{"nodeHPhase": phase})
	if err != nil {
		return
	}
	if _, err := r.updateLoop(ctx, *loop, func(u *storage.LoopRecord) { u.MetadataJSON = stringPtr(metadataJSON) }); err != nil && r.logger != nil {
		r.logger.Warn("planner: set node H phase failed", map[string]any{"loopId": loopID, "phase": phase, "error": err.Error()})
	}
}

// publishTechSpecToPlane writes the agent's tech spec to a Plane page and links it
// to the work item (node G). No-op for github/forgejo projects, when the work item
// can't be resolved, or when a tech-spec page is already linked (idempotent).
func (r *Runner) publishTechSpecToPlane(ctx context.Context, input stepInput, issue checkpointIssue, worktree checkpointWorktree) error {
	if r.planeDoc == nil {
		return nil
	}
	gateway, planeProjectID, ok := r.planeDoc(input.Project.ID)
	if !ok || gateway == nil {
		return nil
	}
	workItemID := planedoc.WorkItemIDFromURL(issue.URL)
	if workItemID == "" {
		return nil
	}
	if _, found, err := gateway.FindSpecLink(ctx, planeProjectID, workItemID, planedoc.TechSpecLinkTitle); err != nil {
		return err
	} else if found {
		return nil // already published
	}
	specPath := firstNonEmpty(worktree.SpecPath, issue.SpecPath)
	content, err := readPlannerSpecFile(worktree.Path, specPath)
	if err != nil || strings.TrimSpace(content) == "" {
		return err // nothing to publish (agent wrote no spec file) — leave it to the GitHub PR
	}
	_, err = gateway.WriteTechSpec(ctx, planeProjectID, workItemID, "Tech Spec: "+issue.Title, content)
	return err
}

// readPlannerSpecFile reads the spec markdown the agent wrote in the worktree.
func readPlannerSpecFile(worktreePath, specPath string) (string, error) {
	if strings.TrimSpace(specPath) == "" {
		return "", nil
	}
	resolved := specPath
	if !filepath.IsAbs(specPath) {
		resolved = filepath.Join(worktreePath, specPath)
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(content), nil
}

func (r *Runner) plannerHoldSummary(ctx context.Context, project storage.ProjectRecord, queueItem storage.QueueItemRecord, loop storage.LoopRecord) (bool, string, error) {
	if r.github == nil {
		return false, "", nil
	}
	repo := firstNonEmpty(derefString(queueItem.Repo), derefString(loop.Repo))
	issueNumber := parseIssueNumberFromTargetID(derefString(loop.TargetID))
	if issueNumber == 0 {
		issueNumber = parseIssueNumberFromTargetID(queueItem.TargetID)
	}
	if repo == "" || issueNumber == 0 {
		return false, "", nil
	}
	detail, err := r.github.ViewIssue(ctx, ViewIssueInput{Repo: repo, IssueNumber: issueNumber, CWD: project.RepoPath})
	if err != nil {
		return false, "", err
	}
	if !domain.IsAutoLaneHeld(domain.LoopTypePlanner, detail.Labels) {
		return false, "", nil
	}
	return true, fmt.Sprintf("Planner stopped because %s#%d is currently held", repo, issueNumber), nil
}

func (r *Runner) plannerHoldSummaryForCheckpoint(ctx context.Context, project storage.ProjectRecord, checkpoint plannerCheckpoint) (bool, string, error) {
	if r.github == nil {
		return false, "", nil
	}
	if checkpoint.Issue == nil {
		return false, "", nil
	}
	detail, err := r.github.ViewIssue(ctx, ViewIssueInput{Repo: checkpoint.Issue.Repo, IssueNumber: checkpoint.Issue.IssueNumber, CWD: project.RepoPath})
	if err != nil {
		return false, "", err
	}
	if domain.IsAutoLaneHeld(domain.LoopTypePlanner, detail.Labels) {
		return true, fmt.Sprintf("Planner stopped because %s#%d is currently held", checkpoint.Issue.Repo, checkpoint.Issue.IssueNumber), nil
	}
	if checkpoint.Publish == nil || checkpoint.Publish.PullRequest == nil || checkpoint.Publish.PullRequest.Number == 0 {
		return false, "", nil
	}
	prDetail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: checkpoint.Issue.Repo, PRNumber: checkpoint.Publish.PullRequest.Number, CWD: project.RepoPath})
	if err != nil {
		return false, "", err
	}
	if domain.IsAutoLaneHeld(domain.LoopTypePlanner, prDetail.Labels) {
		return true, fmt.Sprintf("Planner stopped because %s#%d is currently held", checkpoint.Issue.Repo, checkpoint.Publish.PullRequest.Number), nil
	}
	return false, "", nil
}

func (r *Runner) plannerAdoptedPullRequestHoldSummary(ctx context.Context, project storage.ProjectRecord, repo string, prNumber int64, queueItem storage.QueueItemRecord) (bool, string, error) {
	if plannerQueueItemIsManual(queueItem) || r.github == nil || repo == "" || prNumber == 0 {
		return false, "", nil
	}
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: project.RepoPath})
	if err != nil {
		return false, "", err
	}
	if domain.IsAutoLaneHeld(domain.LoopTypePlanner, detail.Labels) {
		return true, fmt.Sprintf("Planner stopped because %s#%d is currently held", repo, prNumber), nil
	}
	return false, "", nil
}

func (r *Runner) plannerAdoptionHoldSummary(ctx context.Context, project storage.ProjectRecord, checkpoint plannerCheckpoint, repo string, prNumber int64, queueItem storage.QueueItemRecord) (bool, string, error) {
	if plannerQueueItemIsManual(queueItem) {
		return false, "", nil
	}
	if held, summary, err := r.plannerHoldSummaryForCheckpoint(ctx, project, checkpoint); err != nil || held {
		return held, summary, err
	}
	return r.plannerAdoptedPullRequestHoldSummary(ctx, project, repo, prNumber, queueItem)
}

func (r *Runner) finishHeldPlannerQueueItem(ctx context.Context, loop storage.LoopRecord, run *storage.RunRecord, queueItem storage.QueueItemRecord, checkpoint plannerCheckpoint, summary string) (ProcessResult, error) {
	checkpoint.SkipReason = summary
	checkpoint.ResumePolicy = loops.ResumePolicyAdvanceFromCheckpoint
	if run != nil {
		if _, err := r.completeRun(ctx, *run, "success", summary, "", checkpoint); err != nil {
			return ProcessResult{}, err
		}
	}
	if err := r.repos.Queue.Complete(ctx, queueItem.ID, r.nowISO()); err != nil && !errors.Is(err, storage.ErrQueueItemNotActive) {
		return ProcessResult{}, err
	}
	if _, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		updated.Status = "queued"
		updated.LastRunAt = stringPtr(r.nowISO())
		updated.NextRunAt = nil
	}); err != nil {
		return ProcessResult{}, err
	}
	result := ProcessResult{LoopID: loop.ID, QueueItemID: queueItem.ID, Status: "skipped", Summary: summary}
	if run != nil {
		result.RunID = run.ID
	}
	return result, nil
}

func plannerWorktreeRoot(project storage.ProjectRecord) (string, error) {
	projectMetadata := parseJSONObject(project.MetadataJSON)
	worktreeRoot := stringFromAnyDefault(projectMetadata["worktreeRoot"])
	if worktreeRoot != "" {
		return worktreeRoot, nil
	}
	return config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
}

func (r *Runner) findOpenPullRequestForBranch(ctx context.Context, repo, branch, baseBranch, cwd string) (*PullRequestSummary, error) {
	if r.github == nil || strings.TrimSpace(branch) == "" {
		return nil, nil
	}
	pullRequests, err := r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: plannerPRDedupeLookupLimit})
	if err != nil {
		return nil, err
	}
	for _, pr := range pullRequests {
		state := strings.TrimSpace(pr.State)
		if state != "" && !strings.EqualFold(state, "open") {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(pr.HeadRefName), strings.TrimSpace(branch)) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(pr.BaseRefName), strings.TrimSpace(baseBranch)) {
			continue
		}
		if pr.Number <= 0 {
			continue
		}
		candidate := pr
		return &candidate, nil
	}
	return nil, nil
}

func (r *Runner) validatedLifecyclePullRequest(ctx context.Context, input stepInput, issue checkpointIssue, worktree checkpointWorktree, state *lifecycle.State) (*checkpointPullRequest, error) {
	if state == nil || state.PRNumber <= 0 {
		return nil, nil
	}
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: issue.Repo, PRNumber: state.PRNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return nil, nil
	}
	if detail.State != "" && !strings.EqualFold(strings.TrimSpace(detail.State), "open") {
		return nil, nil
	}
	if !strings.EqualFold(strings.TrimSpace(detail.HeadRefName), strings.TrimSpace(worktree.Branch)) || !strings.EqualFold(strings.TrimSpace(detail.BaseRefName), strings.TrimSpace(worktree.BaseBranch)) {
		return nil, nil
	}
	prNumber := detail.Number
	if prNumber == 0 {
		prNumber = state.PRNumber
	}
	return &checkpointPullRequest{Number: prNumber, URL: firstNonEmpty(detail.URL, state.PRURL), Body: ""}, nil
}

func (r *Runner) normalizePullRequestDisclosure(ctx context.Context, run storage.RunRecord, repo string, prNumber int64, cwd string, force bool) error {
	if r.github == nil || prNumber <= 0 || !r.disclosure.Enabled || !r.disclosure.Channels.PullRequest {
		return nil
	}
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		return err
	}
	if !force && !disclosure.HasMarkdownStamp(detail.Body) {
		return nil
	}
	agent, model := r.disclosureIdentity(run)
	stamper := disclosure.Stamper{Config: r.disclosure, Agent: agent, Model: model}
	body := stamper.Markdown(detail.Body, "planner", disclosure.ChannelPullRequest)
	if body == detail.Body {
		return nil
	}
	return r.github.UpdatePullRequestBody(ctx, UpdatePullRequestBodyInput{Repo: repo, PRNumber: prNumber, Body: body, CWD: cwd, DisclosureAgent: agent, DisclosureModel: model})
}

// disclosureIdentity returns agent/model for disclosure stamps from the run
// snapshot when present; falls back to runner identity on empty snapshot or
// parse errors (stamp-only paths must not fail the run).
func (r *Runner) disclosureIdentity(run storage.RunRecord) (agent, model string) {
	vendor, modelPtr, _, _, err := config.IdentityFromRunSnapshot(run.AgentSnapshotJSON, r.agentRuntime, r.agentModel, r.agentProfileID)
	if err != nil {
		// Present but invalid snapshot must not fall back to live runner identity.
		return "", ""
	}
	return vendor, derefString(modelPtr)
}

func (r *Runner) persistPlannerPullRequestReference(ctx context.Context, input stepInput, issue checkpointIssue, worktree checkpointWorktree, pr checkpointPullRequest) error {
	if pr.Number == 0 {
		return nil
	}
	if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
		updated.Repo = stringPtr(issue.Repo)
		updated.PRNumber = &pr.Number
	}); err != nil {
		return err
	}
	metadataJSON, err := mergeLoopMetadataJSON(input.Loop.MetadataJSON, map[string]any{"issueNumber": issue.IssueNumber, "issueUrl": issue.URL, "issueTitle": issue.Title, "specPath": issue.SpecPath, "branch": worktree.Branch, "prUrl": pr.URL, "prNumber": pr.Number, "requestedReviewers": issue.RequestedReviewers})
	if err != nil {
		return err
	}
	_, err = r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) { updated.MetadataJSON = stringPtr(metadataJSON) })
	return err
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

// latestNativeSessionID returns the loop's most recent captured agent session id, so
// a follow-up write-spec turn can native-resume the SAME session and keep the full
// spec-authoring conversation. Empty when none is recorded (e.g. a vendor that did
// not emit a session id) — the follow-up gate then refuses to reactivate.
func (r *Runner) latestNativeSessionID(ctx context.Context, loopID string) string {
	if r.repos == nil || r.repos.AgentExecutions == nil {
		return ""
	}
	execution, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loopID)
	if err != nil || execution == nil || execution.NativeSessionID == nil {
		return ""
	}
	return strings.TrimSpace(*execution.NativeSessionID)
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
	configuredPipelineVersion := int(int64FromAny(parseJSONObject(loop.MetadataJSON)["plannerPipelineVersion"]))
	if configuredPipelineVersion > check.PipelineVersion {
		check.PipelineVersion = configuredPipelineVersion
	}
	if check.PipelineVersion == 0 {
		check.PipelineVersion = 1
	}
	waitCandidate := latestRun != nil && latestRun.Status == "success" && check.Wait != nil && loop.Status == "queued"
	waitResume := waitCandidate && (check.PipelineVersion < 2 || v2DecisionWaitResolved(check))
	if waitCandidate && check.PipelineVersion >= 2 && !waitResume {
		stage := "<missing>"
		if check.Decisions != nil {
			stage = check.Decisions.Stage
		}
		return resumedRunContext{}, fmt.Errorf("planner V2 decision barrier %q is not resolved; refusing non-Plane requeue", stage)
	}
	requirementsReopen := latestRun != nil && latestRun.Status == "failed" && check.PipelineVersion >= 2 && check.Decisions != nil && check.Decisions.Stage == "requirements_reopened"
	shouldResume := latestRun != nil && (latestRun.Status == "failed" || latestRun.Status == "interrupted") && !loops.IsManualHoldResumePolicy(check.ResumePolicy) && lastCompleted != ""
	startStep := stepDiscoverIssues
	_, _, hasPlaneAnswer := planeDecisionAnswer(loop)
	planeAnswerResume := hasPlaneAnswer && latestRun != nil
	switch {
	case planeAnswerResume:
		// The previous successful run stopped after write-spec solely to await the
		// Plane decision. Re-enter that step with its issue/worktree checkpoint.
		shouldResume = true
		startStep = stepWriteSpec
		check.WriteSpec = nil
		// The product answer can change scope and acceptance criteria. Everything
		// derived from the prior draft must be regenerated before owner approval.
		check.Publish = nil
		check.Notify = nil
		check.SkipReason = ""
		lastCompleted = stepPrepareWorktree
	case waitResume:
		startStep = check.Wait.ResumeStep
		if startStep == "" || !plannerStepInVersion(startStep, check.PipelineVersion) {
			return resumedRunContext{}, fmt.Errorf("planner wait has invalid resumeStep %q for pipeline v%d", startStep, check.PipelineVersion)
		}
	case requirementsReopen:
		startStep = stepGrillProductDecisions
	case shouldResume:
		if next := nextPlannerStepForVersion(lastCompleted, check.PipelineVersion); next != "" {
			startStep = next
		}
	}
	resumed := planeAnswerResume || waitResume || requirementsReopen || (shouldResume && startStep != stepDiscoverIssues)
	// Even a brand-new run must carry the pipeline version frozen on the loop.
	// Dropping it here makes stepsFromVersion choose V1; discover then reloads V2
	// and write-spec correctly fails because the V2 decision gates were skipped.
	initialCheckpoint := plannerCheckpoint{PipelineVersion: check.PipelineVersion, ResumePolicy: "replay_step"}
	// stickySnapshot: any continuation of a failed/interrupted predecessor, including first-step retries.
	stickySnapshot := latestRun != nil && (latestRun.Status == "failed" || latestRun.Status == "interrupted")
	if resumed {
		initialCheckpoint = check
		if waitResume {
			initialCheckpoint.Wait = nil
			initialCheckpoint.SkipReason = ""
		}
		initialCheckpoint.ResumePolicy = "advance_from_checkpoint"
	}
	nowISO := r.nowISO()
	run := storage.RunRecord{ID: eventlog.NewEventID("run"), LoopID: loop.ID, Status: "running", CurrentStep: stringPtr(string(startStep)), StartedAt: nowISO, LastHeartbeatAt: stringPtr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO}
	snapshotJSON, err := r.agentSnapshotJSONForNewRun(latestRun, stickySnapshot)
	if err != nil {
		return resumedRunContext{}, err
	}
	if snapshotJSON == nil && strings.TrimSpace(r.agentRuntime) != "" {
		return resumedRunContext{}, fmt.Errorf("agent snapshot required for vendor %q but was not produced", r.agentRuntime)
	}
	run.AgentSnapshotJSON = snapshotJSON
	if waitResume || requirementsReopen {
		if prev := previousPlannerStepForVersion(startStep, initialCheckpoint.PipelineVersion); prev != "" {
			run.LastCompletedStep = stringPtr(string(prev))
		}
	} else if resumed && lastCompleted != "" {
		run.LastCompletedStep = stringPtr(string(lastCompleted))
	}
	encoded := mustMarshalJSON(initialCheckpoint)
	run.CheckpointJSON = &encoded
	if err := r.repos.Runs.Upsert(ctx, run); err != nil {
		return resumedRunContext{}, err
	}
	return resumedRunContext{Run: run, StartStep: startStep, Checkpoint: initialCheckpoint, Resumed: resumed}, nil
}

func v2DecisionWaitResolved(checkpoint plannerCheckpoint) bool {
	if checkpoint.PipelineVersion < 2 || checkpoint.Wait == nil || checkpoint.Decisions == nil {
		return false
	}
	switch checkpoint.Wait.ResumeStep {
	case stepGrillDownstreamDecisions:
		return checkpoint.Decisions.Stage == "product_resolved"
	case stepGrillFinalDecisions:
		return checkpoint.Decisions.Stage == "downstream_resolved"
	default:
		return false
	}
}

func planeDecisionAnswer(loop storage.LoopRecord) (answer, sessionID string, ok bool) {
	ask, exists := loops.ReadHITLAsk(loop.MetadataJSON)
	if !exists || !strings.EqualFold(ask.Transport, "plane") || !strings.EqualFold(ask.Status, "answered") || strings.TrimSpace(ask.Answer) == "" {
		return "", "", false
	}
	return strings.TrimSpace(ask.Answer), strings.TrimSpace(ask.SessionID), true
}

func pendingPlaneDecisionAnswer(loop storage.LoopRecord) (prompt, sessionID string) {
	answer, sessionID, ok := planeDecisionAnswer(loop)
	if !ok {
		return "", ""
	}
	return "The product owner answered your pending decision in the target Plane comment thread:\n\n" + answer + "\n\nTreat this comment-thread answer as authoritative lightweight product input. Revise the tech spec to reflect it. Continue from the existing work; do not restart or ask the same question again.", sessionID
}

func (r *Runner) markPlaneDecisionAnswerConsumed(ctx context.Context, loop *storage.LoopRecord) {
	if r.repos == nil || r.repos.Loops == nil || loop == nil {
		return
	}
	fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		return
	}
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.Transport != "plane" || ask.Status != "answered" {
		return
	}
	ask.Status = "consumed"
	metadata, err := loops.WriteHITLAsk(fresh.MetadataJSON, ask)
	if err != nil {
		return
	}
	metadata, err = mergeLoopMetadataJSON(&metadata, map[string]any{"awaitingProductAnswer": false})
	if err != nil {
		return
	}
	updated, err := r.updateLoop(ctx, *fresh, func(current *storage.LoopRecord) { current.MetadataJSON = stringPtr(metadata) })
	if err == nil {
		*loop = updated
	}
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
	if next := nextPlannerStepForVersion(step, checkpoint.PipelineVersion); next != "" {
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

func (r *Runner) ensureLoopForIssue(ctx context.Context, project storage.ProjectRecord, repo string, issue IssueSummary, currentFingerprint string) (loopUpsertResult, error) {
	nowISO := r.nowISO()
	targetID := buildIssueTargetID(repo, issue.Number)
	existingLoops, err := r.repos.Loops.List(ctx)
	if err != nil {
		return loopUpsertResult{}, err
	}
	for _, existing := range existingLoops {
		if existing.Type == "planner" && existing.ProjectID == project.ID && existing.TargetType == "issue" && derefString(existing.TargetID) == targetID {
			pausedOrCompleted := existing.Status == "paused" || existing.Status == "completed" || existing.Status == "awaiting_human"
			updated := existing
			updated.Repo = stringPtr(repo)
			suppressFailedRevival := loops.ShouldSuppressFailedRediscovery(existing.Status, loops.LastFailedDiscoveryFingerprint(existing.MetadataJSON), currentFingerprint)
			if !pausedOrCompleted && !suppressFailedRevival && updated.Status != "running" {
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
	pipelineVersion := r.pipelineVersionForProject(project.ID)
	meta := mustMarshalJSON(map[string]any{"issueTitle": issue.Title, "issueURL": issue.URL, "issueNumber": issue.Number, "specPath": buildSpecPath(r.now(), issue.Number, issue.Title), "plannerPipelineVersion": pipelineVersion})
	loop := storage.LoopRecord{ID: eventlog.NewEventID("loop"), Seq: seq, ProjectID: project.ID, Type: "planner", TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "queued", MetadataJSON: &meta, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := r.repos.Loops.Upsert(ctx, loop); err != nil {
		return loopUpsertResult{}, err
	}
	r.appendEvent(ctx, eventInput{eventType: "loop.created", projectID: project.ID, loopID: loop.ID, entityType: "loop", entityID: loop.ID, payload: map[string]any{"type": "planner", "repo": repo, "issueNumber": issue.Number}})
	return loopUpsertResult{record: loop, created: true}, nil
}

func (r *Runner) pipelineVersionForProject(projectID string) int {
	if r.projectRoleConfig == nil || !r.isPlaneProject(projectID) {
		return 1
	}
	if config.ProjectRoleConfigs(*r.projectRoleConfig, projectID).Planner.PreSpecDecisionGrill {
		return 2
	}
	return 1
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
	lockKey := storage.IssueLockKey(input.ProjectID, input.Repo, input.IssueNumber)
	projectID := input.ProjectID
	loopID := input.LoopID
	payload := mustMarshalJSON(input.Payload)
	queueItem := storage.QueueItemRecord{ID: eventlog.NewEventID("queue"), ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: targetID, Repo: &input.Repo, DedupeKey: dedupeKey, Priority: storage.QueuePriorityPlanner, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: r.retryMaxAttempts, LockKey: &lockKey, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}
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
	nowISO := r.nowISO()
	if !shouldRetryQueueFailure(kind, nextAttempts, queueItem.MaxAttempts) {
		if err := r.repos.Queue.Fail(ctx, storage.QueueFailInput{ID: queueItem.ID, Attempts: nextAttempts, FinishedAt: nowISO, ErrorMessage: optionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
			return nil, err
		}
		return r.repos.Queue.GetByID(ctx, queueItem.ID)
	}
	retryAt := eventlog.FormatJavaScriptISOString(r.now().Add(backoffDelay(r.retryBaseDelay, cappedRetryDelayAttempt(nextAttempts, queueItem.MaxAttempts))))
	if err := r.repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: queueItem.ID, AvailableAt: retryAt, Attempts: nextAttempts, ErrorMessage: optionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
		return nil, err
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
	if current.Status == "terminated" {
		return *current, nil
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
	return r.classifyFailureWithBoundary(err, failureclass.BoundaryUnknown)
}

func (r *Runner) classifyFailureWithBoundary(err error, boundary failureclass.Boundary) *loopError {
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
	if githubinfra.IsTransientError(err) {
		return &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	return &loopError{message: err.Error(), kind: plannerFailureKind(failureclass.Classify(err, failureclass.Context{Runner: failureclass.RunnerPlanner, Boundary: boundary}))}
}

func plannerFailureBoundaryForStep(step PlannerStep) failureclass.Boundary {
	switch step {
	case stepDiscoverIssues, stepPublish, stepNotify:
		return failureclass.BoundaryGitHubAPI
	case stepPrepareWorktree:
		return failureclass.BoundaryGitRemote
	case stepAuthorDecisionBrief, stepGrillProductDecisions, stepGrillDownstreamDecisions, stepGrillFinalDecisions, stepWriteSpec, stepGrill, stepReview:
		return failureclass.BoundaryModelProvider
	default:
		return failureclass.BoundaryUnknown
	}
}

func plannerFailureKind(kind failureclass.Kind) QueueFailureKind {
	switch kind {
	case failureclass.RetryableTransient:
		return FailureRetryableTransient
	case failureclass.RetryableAfterResume:
		return FailureRetryableAfterResume
	case failureclass.RecoverableInfra:
		return FailureRecoverableInfra
	case failureclass.ManualIntervention:
		return FailureManualIntervention
	default:
		return FailureNonRetryable
	}
}

func (r *Runner) nowISO() string { return eventlog.FormatJavaScriptISOString(r.now()) }

func stepsFrom(start PlannerStep) []PlannerStep {
	return stepsFromVersion(start, 1)
}

func plannerStepsForVersion(version int) []PlannerStep {
	if version >= 2 {
		return plannerV2StepSequence
	}
	return plannerStepSequence
}

func plannerStepInVersion(step PlannerStep, version int) bool {
	for _, candidate := range plannerStepsForVersion(version) {
		if candidate == step {
			return true
		}
	}
	return false
}

func stepsFromVersion(start PlannerStep, version int) []PlannerStep {
	sequence := plannerStepsForVersion(version)
	startIndex := 0
	for i, step := range sequence {
		if step == start {
			startIndex = i
			break
		}
	}
	return sequence[startIndex:]
}

func nextPlannerStep(step PlannerStep) PlannerStep {
	return nextPlannerStepForVersion(step, 1)
}

func nextPlannerStepForVersion(step PlannerStep, version int) PlannerStep {
	sequence := plannerStepsForVersion(version)
	for i, candidate := range sequence {
		if candidate == step && i+1 < len(sequence) {
			return sequence[i+1]
		}
	}
	return ""
}

func previousPlannerStep(step PlannerStep) PlannerStep {
	return previousPlannerStepForVersion(step, 1)
}

func previousPlannerStepForVersion(step PlannerStep, version int) PlannerStep {
	sequence := plannerStepsForVersion(version)
	for i, candidate := range sequence {
		if candidate == step && i > 0 {
			return sequence[i-1]
		}
	}
	return ""
}

func asPlannerStep(value string) PlannerStep {
	for _, candidate := range append(append([]PlannerStep{}, plannerStepSequence...), plannerV2StepSequence...) {
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

func (c *plannerCheckpoint) ensureLifecycle(runner, branch, baseBranch string, expectPR bool) {
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

func buildPlannerPrompt(project storage.ProjectRecord, instructionConfig config.Config, issue *checkpointIssue, worktree *checkpointWorktree, allowAutoPush bool, disclosureCfg config.DisclosureConfig, agentRuntime string, agentModel string) (string, config.CustomInstructionBlock) {
	providerLabel := providerIssueSystemLabel(providerKindForProject(instructionConfig, project.ID))
	parts := []string{
		fmt.Sprintf("Write a planning spec for %s issue %s#%d.", providerLabel, issue.Repo, issue.IssueNumber),
		"Repository: " + issue.Repo,
		"Base branch: " + worktree.BaseBranch,
		"Spec path: " + issue.SpecPath,
		"Issue title: " + issue.Title,
	}
	if strings.TrimSpace(issue.Body) != "" {
		parts = append(parts, "Issue body:\n"+issue.Body)
	}
	if strings.TrimSpace(issue.ProductSpec) != "" {
		productSpec := "AUTHORITATIVE PRODUCT SPEC (highest-priority source of truth):\n" + issue.ProductSpec
		if strings.TrimSpace(issue.ProductSpecURL) != "" {
			productSpec += "\nProduct spec URL: " + issue.ProductSpecURL
		}
		parts = append(parts, productSpec)
	}
	if strings.TrimSpace(issue.URL) != "" {
		parts = append(parts, "Issue URL: "+issue.URL)
	}
	if agentsBlock := readAgentsBlock(project.RepoPath); agentsBlock != "" {
		parts = append(parts, agentsBlock)
	}
	instructionBlock := config.BuildCustomInstructionBlock(instructionConfig, project.ID, "planner")
	if instructionBlock.Text != "" {
		parts = append(parts, instructionBlock.Text)
	}
	requirements := []string{
		"Requirements:",
		"- Create or update the spec at " + issue.SpecPath,
		"- Use Markdown with clear problem, goals, approach, risks, and validation sections",
		"- Write the entire technical spec in Simplified Chinese; keep code identifiers, file paths, commands, API names, and other exact technical tokens in their original form",
		"- Treat the product spec as authoritative for user-visible scope, phase order, priorities, and acceptance criteria; the issue is supporting context only",
		"- Do not replace an explicit product decision with your own recommendation or invent pricing, packaging, or prioritization questions that the product spec does not mark unresolved",
		"- Keep the implementation scope aligned to the product spec",
	}
	if allowAutoPush {
		requirements = append(requirements, "- Commit the spec changes on the current branch so the PR can be opened")
	} else {
		requirements = append(requirements, "- Do not push the branch or open/update pull requests; leave repository publishing for Looper/manual follow-up")
	}
	parts = append(parts, strings.Join(requirements, "\n"))
	if allowAutoPush {
		parts = append(parts, lifecycle.PromptInstruction("planner", worktree.Branch, worktree.BaseBranch, true, true, disclosureCfg, agentRuntime, agentModel))
	} else {
		parts = append(parts, noRemoteLifecyclePromptInstruction("planner", worktree.Branch, worktree.BaseBranch, disclosureCfg, agentRuntime, agentModel))
	}
	return agent.AppendCompletionInstruction(strings.Join(parts, "\n\n")), instructionBlock
}

func providerKindForProject(cfg config.Config, projectID string) config.ProviderKind {
	for _, project := range cfg.Projects {
		if project.ID == projectID {
			return config.ResolvedProjectProviderKind(cfg, project)
		}
	}
	return config.ProviderKindGitHub
}

func providerIssueSystemLabel(kind config.ProviderKind) string {
	if kind == config.ProviderKindForgejo {
		return "Forgejo"
	}
	return "GitHub"
}

func customInstructionConfig(value *config.Config) config.Config {
	if value == nil {
		cfg, _ := config.Normalize("")
		cfg.Instructions.Enabled = false
		return cfg
	}
	return *value
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

func buildPlannerFallbackCommitMessage(issue *checkpointIssue) string {
	title := "planner spec"
	if issue != nil && strings.TrimSpace(issue.Title) != "" {
		title = issue.Title
	}
	return "planner: " + strings.TrimSpace(title)
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

func isManualPlannerQueue(payload map[string]any) bool {
	manual, ok := payload["manual"].(bool)
	return ok && manual
}

func plannerQueueItemIsManual(queueItem storage.QueueItemRecord) bool {
	return isManualPlannerQueue(parseJSONObject(queueItem.PayloadJSON))
}

func shouldClaimIssue(issue IssueSummary, login string, policy DiscoveryPolicy) bool {
	if policy.RequireAssigneeCurrentUser && !includesLogin(issue.Assignees, login) {
		return false
	}
	return labelsMatch(issue.Labels, policy.Labels, policy.LabelMode)
}

func safeIssueQueryLabel(labels []string) string {
	for _, label := range labels {
		if strings.TrimSpace(label) != "" {
			return label
		}
	}
	return ""
}

func (r *Runner) listOpenIssuesForDiscovery(ctx context.Context, input ListOpenIssuesInput, policy DiscoveryPolicy) ([]IssueSummary, error) {
	if policy.LabelMode != config.LabelModeAny {
		input.Labels = uniqueNonEmptyLabels(policy.Labels)
		input.Label = safeIssueQueryLabel(input.Labels)
		return r.github.ListOpenIssues(ctx, input)
	}
	queryLabels := uniqueNonEmptyLabels(policy.Labels)
	if len(queryLabels) == 0 {
		return r.github.ListOpenIssues(ctx, input)
	}
	issuePages := make([][]IssueSummary, 0, len(queryLabels))
	for _, label := range queryLabels {
		queryInput := input
		queryInput.Label = label
		issues, err := r.github.ListOpenIssues(ctx, queryInput)
		if err != nil {
			return nil, err
		}
		issuePages = append(issuePages, issues)
	}
	return mergeIssuePages(issuePages, effectiveIssueLimit(input.Limit)), nil
}

func mergeIssuePages(pages [][]IssueSummary, limit int) []IssueSummary {
	seenIssues := map[int64]struct{}{}
	merged := []IssueSummary{}
	for index := 0; len(merged) < limit; index++ {
		anyPageHasIndex := false
		for _, page := range pages {
			if index >= len(page) {
				continue
			}
			anyPageHasIndex = true
			issue := page[index]
			if _, ok := seenIssues[issue.Number]; ok {
				continue
			}
			seenIssues[issue.Number] = struct{}{}
			merged = append(merged, issue)
			if len(merged) >= limit {
				break
			}
		}
		if !anyPageHasIndex {
			break
		}
	}
	return merged
}

func effectiveIssueLimit(limit int) int {
	if limit <= 0 {
		return defaultIssueLimit
	}
	return limit
}

func uniqueNonEmptyLabels(labels []string) []string {
	seen := map[string]struct{}{}
	result := []string{}
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
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

// looksLikeUUID reports whether s is shaped like a UUID (8-4-4-4-12 hex) — used to
// skip Plane assignee ids, which are UUIDs and must not be requested as GitHub
// reviewers (that would 422 and, before this, wedge the run on retry).
func looksLikeUUID(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
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

func hasRequestedReviewerSources(project storage.ProjectRecord, loop storage.LoopRecord, assignees []string) bool {
	if len(assignees) > 0 {
		return true
	}
	projectMetadata := parseJSONObject(project.MetadataJSON)
	loopConfig := parseJSONObject(loop.ConfigJSON)
	return len(toStrings(loopConfig["reviewers"])) > 0 || len(toStrings(projectMetadata["reviewers"])) > 0
}

func buildIssueTargetID(repo string, issueNumber int64) string {
	return fmt.Sprintf("issue:%s:%d", repo, issueNumber)
}

// plannerQueuePayloadFPKey is the JSON key used to forward the planner
// discovery fingerprint into the queue payload so failure handlers can stamp
// it onto the loop without recomputing inputs from scratch.
const plannerQueuePayloadFPKey = "discoveryFingerprint"

// buildPlannerDiscoveryFingerprint returns a stable fingerprint over the
// inputs that planner discovery uses to decide whether a previously-failed
// loop should be revived. specPath is included so deliberate spec-path
// changes (e.g. issue title edits affecting slug, or day rollover) trigger
// rediscovery.
func buildPlannerDiscoveryFingerprint(repo string, now time.Time, issue IssueSummary) string {
	labels := loops.CanonicalSortedStrings(issue.Labels)
	assignees := loops.CanonicalSortedStrings(issue.Assignees)
	specPath := buildSpecPath(now, issue.Number, issue.Title)
	return loops.ComputeDiscoveryFingerprint(
		"planner",
		repo,
		fmt.Sprintf("%d", issue.Number),
		strings.TrimSpace(issue.Title),
		strings.TrimSpace(issue.Body),
		strings.TrimSpace(issue.URL),
		specPath,
		strings.Join(labels, ","),
		strings.Join(assignees, ","),
	)
}

// plannerQueueDiscoveryFingerprint reads the persisted fingerprint from a
// queue item's payload, returning empty when missing or invalid.
func plannerQueueDiscoveryFingerprint(payloadJSON *string) string {
	if payloadJSON == nil {
		return ""
	}
	raw := strings.TrimSpace(*payloadJSON)
	if raw == "" {
		return ""
	}
	parsed := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return ""
	}
	value, _ := parsed[plannerQueuePayloadFPKey].(string)
	return strings.TrimSpace(value)
}

// stampFailedDiscoveryFingerprint records the queue item's discovery
// fingerprint on the loop's metadata so the next discovery tick can suppress
// autonomous rediscovery while inputs are unchanged.
func (r *Runner) stampFailedDiscoveryFingerprint(updated *storage.LoopRecord, queueItem storage.QueueItemRecord) {
	fingerprint := plannerQueueDiscoveryFingerprint(queueItem.PayloadJSON)
	if fingerprint == "" {
		return
	}
	merged, err := loops.MergeLastFailedDiscoveryFingerprint(updated.MetadataJSON, fingerprint)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("planner fingerprint stamp failed", map[string]any{"loopId": updated.ID, "error": err.Error()})
		}
		return
	}
	updated.MetadataJSON = stringPtr(merged)
}

func buildPlannerDedupeKey(projectID, loopID, repo string, issueNumber int64) string {
	return fmt.Sprintf("planner:%s:%s:%s:%d", projectID, loopID, repo, issueNumber)
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
		if delay >= maxRetryDelay || delay > maxRetryDelay/2 {
			return maxRetryDelay
		}
		delay *= 2
	}
	if delay > maxRetryDelay {
		return maxRetryDelay
	}
	return delay
}

func isRetryableFailure(kind QueueFailureKind) bool {
	return kind == FailureRetryableTransient || kind == FailureRetryableAfterResume || kind == FailureRecoverableInfra || kind == FailureNonRetryable
}

func shouldRetryQueueFailure(kind QueueFailureKind, nextAttempts, maxAttempts int64) bool {
	if !isRetryableFailure(kind) {
		return false
	}
	if maxAttempts < 0 {
		return kind != FailureNonRetryable
	}
	return maxAttempts > 0 && nextAttempts < maxAttempts
}

func cappedRetryDelayAttempt(attempts, maxAttempts int64) int64 {
	if attempts <= 0 {
		return 1
	}
	if maxAttempts > 0 && attempts > maxAttempts {
		return maxAttempts
	}
	return attempts
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

func (r *Runner) agentSnapshotJSONForNewRun(previous *storage.RunRecord, sticky bool) (*string, error) {
	var previousSnapshot *string
	if previous != nil {
		previousSnapshot = previous.AgentSnapshotJSON
	}
	snapshotJSON, legacyResume, err := config.ResolveRunAgentSnapshotJSON(previousSnapshot, sticky, r.agentRuntime, r.agentModel, r.agentProfileID)
	if err != nil {
		return nil, err
	}
	if legacyResume && r.logger != nil && previous != nil {
		r.logger.Warn("resuming run without agent_snapshot_json; using current runner agent identity", map[string]any{
			"loopId": previous.LoopID,
			"runId":  previous.ID,
			"vendor": r.agentRuntime,
			"model":  derefString(r.agentModel),
		})
	}
	return snapshotJSON, nil
}

// identityFromRun returns the vendor/model/profile that must drive this run.
// When the run has AgentSnapshotJSON, that identity is execution authority.
// model is a pointer so nil (unset) and non-nil empty (suppress) stay distinct.
func (r *Runner) identityFromRun(run storage.RunRecord) (vendor string, model *string, profile string, useSnapshot bool, err error) {
	return config.IdentityFromRunSnapshot(run.AgentSnapshotJSON, r.agentRuntime, r.agentModel, r.agentProfileID)
}

func agentRunSnapshotFields(vendor string, model *string, useSnapshot bool) (bool, string, *string) {
	if !useSnapshot {
		return false, "", nil
	}
	// Pass through including non-nil empty suppress so SnapshotModel stays
	// distinct from unset and ParamsForRoleVendor can strip params --model/-m.
	return true, vendor, model
}

func stringPtr(value string) *string { return &value }

func cloneStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
