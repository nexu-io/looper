package fixer

import (
	"context"
	"crypto/sha1"
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
	Author   string   `json:"author,omitempty"`
	URL      string   `json:"url,omitempty"`
	Path     string   `json:"path,omitempty"`
	Line     int64    `json:"line,omitempty"`
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
	ResolveReviewThread(context.Context, ResolveReviewThreadInput) error
	AddReviewThreadReply(context.Context, AddReviewThreadReplyInput) error
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
	FixItemsHash                 string                  `json:"fixItemsHash,omitempty"`
}

// replyExplanationEntry holds the agent's per-fix-item explanation for the
// auto-reply posted before resolving the review thread. Stored on the repair
// checkpoint so resume/retry reuses the same explanation if it still maps to
// the current fix items snapshot.
type replyExplanationEntry struct {
	FixItemID   string `json:"fixItemId"`
	ThreadID    string `json:"threadId,omitempty"`
	Explanation string `json:"explanation"`
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
	HeadSHA       string `json:"headSha,omitempty"`
	PushedAt      string `json:"pushedAt,omitempty"`
	SkippedReason string `json:"skippedReason,omitempty"`
}

type checkpointResolvedComments struct {
	Items []checkpointResolvedComment `json:"items,omitempty"`
}

type checkpointResolvedComment struct {
	FixItemID  string `json:"fixItemId,omitempty"`
	ThreadID   string `json:"threadId,omitempty"`
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

// parseReplyExplanations extracts the optional review_thread_replies array from
// the final __LOOPER_RESULT__ JSON line. Failure to parse is not an error: the
// runner falls back to the generic reply body. Entries are filtered against the
// current fixItems snapshot (keyed by fixItemId, with a defensive threadId
// cross-check), deduplicated keeping the first valid occurrence, and truncated.
// Disclosure markers, @mentions, and HTML tags are stripped so the adapter
// remains the only path that stamps and templates the reply.
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
			FixItemID   string `json:"fixItemId"`
			ThreadID    string `json:"threadId"`
			Explanation string `json:"explanation"`
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
		explanation := sanitizeReplyExplanation(raw.Explanation)
		if explanation == "" {
			continue
		}
		if _, dup := seen[fixItemID]; dup {
			continue
		}
		seen[fixItemID] = struct{}{}
		out = append(out, replyExplanationEntry{FixItemID: fixItemID, ThreadID: item.ThreadID, Explanation: explanation})
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
	openPRs, err := r.listOpenPullRequestsForDiscoveryWithPolicy(ctx, input.Repo, project.RepoPath, input.Limit, currentUser, policy)
	if err != nil {
		return DiscoveryResult{}, err
	}
	result := DiscoveryResult{}
	for _, pr := range openPRs {
		if (!policy.IncludeDrafts && pr.IsDraft) || normalizePRState(pr.State) != "open" || r.hasActivePRLock(ctx, input.Repo, pr.Number) {
			result.Skipped++
			continue
		}
		if policy.AuthorFilter != config.FixerAuthorFilterAny && !sameGitHubLogin(pr.Author, currentUser) {
			result.Skipped++
			continue
		}
		if !labelsMatch(pr.Labels, policy.Labels, policy.LabelMode) {
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
		fixItemsHash := hashFixItems(fixItems)
		loopResult, err := r.ensureLoopForPullRequest(ctx, *project, input.Repo, pr.Number, detail.HeadSHA, fixItemsHash)
		if err != nil {
			return DiscoveryResult{}, err
		}
		if loopResult.record.Status == "paused" || loopResult.record.Status == "failed" || loopResult.skipped {
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
			FixItemsHash: fixItemsHash,
		})
		if err != nil {
			return DiscoveryResult{}, err
		}
		result.QueueItems = append(result.QueueItems, queueItem)
	}
	return result, nil
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
	checkpoint.Repair.ReplyExplanations = parseReplyExplanations(result.Stdout, result.Stderr, checkpoint.FixItems)
	checkpoint.Repair.FixItemsHash = checkpoint.FixItemsHash
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
	if checkpoint.ReconcileCommits.FinalHeadSHA != "" && checkpoint.ReconcileCommits.FinalHeadSHA == worktree.BaseHeadSHA {
		r.appendEvent(ctx, eventInput{eventType: "fixer.push.skipped", projectID: input.Project.ID, loopID: input.Loop.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"branch": branch, "reason": "no_new_commits"}})
		checkpoint.Push = &checkpointPush{Pushed: false, Branch: branch, Remote: "origin", SkippedReason: "No new commits to push"}
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
	liveDetail, err := r.waitForPullRequestHeadSHA(ctx, waitForPullRequestHeadSHAInput{Repo: input.Repo, PRNumber: input.PRNumber, ExpectedHeadSHA: finalHeadSHA, CWD: input.Project.RepoPath, Attempts: 5, Delay: time.Second, FailureMessage: func(actual string) string {
		return fmt.Sprintf("PR head did not update after push: expected %s, got %s", finalHeadSHA, firstNonEmpty(actual, "unknown"))
	}})
	if err != nil {
		return checkpoint, err
	}
	checkpoint = refreshCheckpointHeadAfterPush(checkpoint, liveDetail)
	metadataJSON, err := mergeLoopMetadataJSON(input.Loop.MetadataJSON, map[string]any{"lastFixHeadSha": resolveCommentsExpectedHeadSHA(checkpoint), "lastFixItemsHash": checkpoint.FixItemsHash, "lastFixPushedAt": r.nowISO()})
	if err != nil {
		return checkpoint, err
	}
	if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) { updated.MetadataJSON = stringPtr(metadataJSON) }); err != nil {
		return checkpoint, err
	}
	pushedAt := r.nowISO()
	pushedHeadSHA := resolveCommentsExpectedHeadSHA(checkpoint)
	r.appendEvent(ctx, eventInput{eventType: "pr.branch.pushed", projectID: input.Project.ID, loopID: input.Loop.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"branch": branch, "pushedAt": pushedAt, "headSha": nilIfEmpty(pushedHeadSHA)}})
	checkpoint.Push = &checkpointPush{Pushed: true, Branch: branch, Remote: "origin", HeadSHA: pushedHeadSHA, PushedAt: pushedAt}
	checkpoint.ensureLifecycle("fixer", branch, detailBaseRefName(checkpoint.Detail), false)
	checkpoint.Lifecycle.Pushed = true
	checkpoint.Lifecycle.Actions.Push = lifecycle.ActionSourceFallback
	checkpoint.Lifecycle.PRNumber = input.PRNumber
	checkpoint.Lifecycle.PRAdopted = true
	checkpoint.Lifecycle.Actions.PR = lifecycle.ActionSourceFallback
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
	fixItems := checkpoint.FixItems
	currentFixItemsHash := checkpoint.FixItemsHash
	verifiedNoPushHeadSHA := ""
	if checkpoint.Push != nil && !checkpoint.Push.Pushed {
		detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.Project.RepoPath})
		if err != nil {
			return checkpoint, err
		}
		fixItems = collectFixItems(detail)
		currentFixItemsHash = hashFixItems(fixItems)
		checkpoint.Detail = mergeCheckpointDetailPreservingLabels(checkpoint.Detail, detail)
		verifiedNoPushHeadSHA = resolveCommentsVerifiedNoPushHeadSHA(checkpoint.Push, input.Loop.MetadataJSON, currentFixItemsHash)
	}
	expectedHeadSHA := resolveCommentsExpectedHeadSHA(checkpoint)
	if verifiedNoPushHeadSHA != "" {
		expectedHeadSHA = verifiedNoPushHeadSHA
	}
	if expectedHeadSHA != "" {
		liveDetail, err := r.waitForPullRequestHeadSHA(ctx, waitForPullRequestHeadSHAInput{Repo: input.Repo, PRNumber: input.PRNumber, ExpectedHeadSHA: expectedHeadSHA, CWD: input.Project.RepoPath, Attempts: 5, Delay: time.Second, FailureMessage: func(actual string) string {
			return fmt.Sprintf("PR head changed before resolving comments: expected %s, got %s", expectedHeadSHA, firstNonEmpty(actual, "unknown"))
		}})
		if err != nil {
			return checkpoint, err
		}
		checkpoint.Detail = mergeCheckpointDetailPreservingLabels(checkpoint.Detail, liveDetail)
		if checkpoint.Push != nil && !checkpoint.Push.Pushed {
			fixItems = collectFixItems(liveDetail)
			currentFixItemsHash = hashFixItems(fixItems)
			verifiedNoPushHeadSHA = resolveCommentsVerifiedNoPushHeadSHA(checkpoint.Push, input.Loop.MetadataJSON, currentFixItemsHash)
		}
	}
	if shouldBlockResolveWithoutFix(checkpoint, fixItems, verifiedNoPushHeadSHA != "" && verifiedNoPushHeadSHA == detailHeadSHA(checkpoint.Detail)) {
		metadataJSON, err := mergeLoopMetadataJSON(input.Loop.MetadataJSON, map[string]any{"lastNoopResolveHeadSha": detailHeadSHA(checkpoint.Detail), "lastNoopResolveFixItemsHash": currentFixItemsHash, "lastNoopResolveAt": r.nowISO()})
		if err != nil {
			return checkpoint, err
		}
		if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) { updated.MetadataJSON = stringPtr(metadataJSON) }); err != nil {
			return checkpoint, err
		}
		checkpoint.ResumePolicy = loops.ResumePolicyRestartFromDiscover
		return checkpoint, &loopError{message: "resolve-comments refused because fixer produced no new commits to push; leaving review threads unresolved", kind: FailureRetryableAfterResume}
	}
	if checkpoint.ResolvedComments == nil {
		checkpoint.ResolvedComments = &checkpointResolvedComments{Items: []checkpointResolvedComment{}}
	}
	commitSHA := ""
	if checkpoint.ReconcileCommits != nil {
		if len(checkpoint.ReconcileCommits.NewCommitSHAs) > 0 {
			commitSHA = checkpoint.ReconcileCommits.NewCommitSHAs[len(checkpoint.ReconcileCommits.NewCommitSHAs)-1]
		} else {
			commitSHA = checkpoint.ReconcileCommits.FinalHeadSHA
		}
	}
	if commitSHA == "" || (verifiedNoPushHeadSHA != "" && commitSHA == reconcileBaseHeadSHA(checkpoint.ReconcileCommits)) {
		commitSHA = firstNonEmpty(verifiedNoPushHeadSHA, commitSHA)
	}
	failedCount := 0
	explanationByID := lookupReplyExplanations(checkpoint)
	for _, item := range fixItems {
		if item.Type != "comment" {
			continue
		}
		if alreadyResolved(checkpoint.ResolvedComments.Items, item) {
			continue
		}
		replyState, replyError := r.replyToFixedComment(ctx, input, item, commitSHA, explanationByID[item.ID], checkpoint.ResolvedComments.Items)
		if err := r.github.ResolveReviewThread(ctx, ResolveReviewThreadInput{Repo: input.Repo, ThreadID: item.ThreadID, CWD: input.Project.RepoPath}); err != nil {
			message := err.Error()
			status := "failed"
			if strings.Contains(strings.ToLower(message), "already") {
				status = "already_resolved"
			} else {
				failedCount++
			}
			upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Status: status, Message: message, UpdatedAt: r.nowISO(), ReplyState: replyState, ReplyError: replyError})
			continue
		}
		upsertResolvedComment(&checkpoint.ResolvedComments.Items, checkpointResolvedComment{FixItemID: item.ID, ThreadID: item.ThreadID, Status: "resolved", UpdatedAt: r.nowISO(), ReplyState: replyState, ReplyError: replyError})
	}
	r.appendEvent(ctx, eventInput{eventType: "fixer.comments.resolved", projectID: input.Project.ID, entityType: "pull_request", entityID: buildPullRequestTargetID(input.Repo, input.PRNumber), payload: map[string]any{"items": checkpoint.ResolvedComments.Items}})
	if failedCount == 0 {
		r.publishRoundSummaryComment(ctx, input, &checkpoint, fixItems, commitSHA, explanationByID)
	}
	if failedCount > 0 {
		return checkpoint, &loopError{message: fmt.Sprintf("Failed to resolve %d review thread(s)", failedCount), kind: FailureRetryableAfterResume}
	}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

// replyToFixedComment posts a reply on the review thread acknowledging the fix
// before the thread is resolved. Replies are best-effort: failures are recorded
// in the checkpoint but never block the resolve step. The disclosure stamper
// applied by the GitHub adapter ensures every reply carries the looper exposure
// marker, so automated replies can be filtered downstream.
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
	if err := r.github.AddReviewThreadReply(ctx, AddReviewThreadReplyInput{Repo: input.Repo, ThreadID: item.ThreadID, Body: body, CWD: input.Project.RepoPath}); err != nil {
		return "failed", err.Error()
	}
	return "sent", ""
}

// lookupReplyExplanations returns a map of fixItemId → sanitized agent
// explanation, but only when the explanations were captured against the same
// fix-items snapshot represented by the current checkpoint. If the snapshot
// changed (e.g. PR rebased, new comments arrived), the agent's explanations no
// longer describe the threads we are about to reply to and we fall back to the
// generic body.
func lookupReplyExplanations(checkpoint fixerCheckpoint) map[string]string {
	if checkpoint.Repair == nil || len(checkpoint.Repair.ReplyExplanations) == 0 {
		return nil
	}
	if checkpoint.Repair.FixItemsHash != "" && checkpoint.FixItemsHash != "" && checkpoint.Repair.FixItemsHash != checkpoint.FixItemsHash {
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
		return b.String()
	}
	if summary := summarizeFixItem(item); summary != "" {
		b.WriteString("\n\n")
		b.WriteString(summary)
	}
	return b.String()
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
	if checkpoint.Push == nil || !checkpoint.Push.Pushed {
		return
	}
	if !roundProducedNewCommits(checkpoint) {
		return
	}
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
	skipped bool
}

func (r *Runner) ensureLoopForPullRequest(ctx context.Context, project storage.ProjectRecord, repo string, prNumber int64, headSHA, fixItemsHash string) (loopUpsertResult, error) {
	nowISO := r.nowISO()
	existingLoops, err := r.repos.Loops.List(ctx)
	if err != nil {
		return loopUpsertResult{}, err
	}
	for _, existing := range existingLoops {
		if existing.Type == "fixer" && existing.ProjectID == project.ID && derefString(existing.Repo) == repo && derefInt64(existing.PRNumber) == prNumber {
			if existing.Status == "paused" {
				return loopUpsertResult{record: existing, created: false}, nil
			}
			if loops.ShouldSuppressFailedRediscovery(existing.Status, loops.LastFailedDiscoveryFingerprint(existing.MetadataJSON), buildFixerDiscoveryFingerprint(repo, prNumber, headSHA, fixItemsHash)) || shouldSkipRediscoveryAfterNoopResolve(existing.MetadataJSON, headSHA, fixItemsHash) {
				return loopUpsertResult{record: existing, created: false, skipped: true}, nil
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
	payload := mustMarshalJSON(map[string]any{"discoveryFingerprint": buildFixerDiscoveryFingerprint(input.Repo, input.PRNumber, input.HeadSHA, input.FixItemsHash)})
	queueItem := storage.QueueItemRecord{ID: eventlog.NewEventID("queue"), ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: targetID, Repo: &input.Repo, PRNumber: &input.PRNumber, DedupeKey: dedupeKey, Priority: storage.QueuePriorityFixer, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: r.retryMaxAttempts, LockKey: &lockKey, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}
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
		var line int64
		switch v := comment["line"].(type) {
		case float64:
			line = int64(v)
		case int64:
			line = v
		case int:
			line = int64(v)
		}
		result = append(result, FixItem{Type: "comment", ID: id, ThreadID: threadID, Summary: summary, Author: author, URL: url, Path: path, Line: line})
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
		"For each comment-type fix item you actually addressed, include an entry in a top-level `review_thread_replies` array on the final " + agent.CompletionMarker + " JSON line. Each entry must be an object with these fields:",
		`  - "fixItemId": the exact "id" of the fix item you addressed`,
		`  - "threadId": the exact "threadId" of the same fix item`,
		`  - "explanation": one or two sentences (max ~500 chars) describing what you changed and, when relevant, which file(s) or test(s) cover it. No greetings, no @mentions, no markdown headings, no HTML, no disclosure markers.`,
		"Looper posts the reply itself; do not call any GitHub API or invent URLs. Omit `review_thread_replies` (or any individual entry) if you cannot truthfully describe the fix for that item.",
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

func shouldSkipRediscoveryAfterNoopResolve(loopMetadataJSON *string, headSHA, fixItemsHash string) bool {
	if headSHA == "" || fixItemsHash == "" {
		return false
	}
	metadata := parseJSONObject(loopMetadataJSON)
	lastHeadSHA, _ := stringFromAny(metadata["lastNoopResolveHeadSha"])
	lastFixItemsHash, _ := stringFromAny(metadata["lastNoopResolveFixItemsHash"])
	return lastHeadSHA != "" && lastHeadSHA == headSHA && lastFixItemsHash != "" && lastFixItemsHash == fixItemsHash
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
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
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
