package sweeper

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	QueueType              = "sweeper"
	QueueTypeWarn          = "sweeper:warn"
	QueueTypeClose         = "sweeper:close"
	QueueTypeReconcile     = "sweeper:reconcile"
	defaultClaimedBy       = "sweeper"
	defaultSkippedSummary  = "sweeper: no action"
	javaScriptISOStringUTC = "2006-01-02T15:04:05.000Z"
	defaultRetryDelay      = 5 * time.Second
	defaultRetryMax        = int64(3)
	defaultQueuePriority   = storage.QueuePriorityWorker

	categoryNone          = "none"
	categoryStale         = "stale"
	categoryAlreadyFixed  = "already_fixed"
	categoryUnrelated     = "unrelated"
	categorySuperseded    = "superseded"
	categoryAbandonedPR   = "abandoned_pr"
	categoryRouteSecurity = "route_security"

	outcomeNoAction                = "no_action"
	outcomePending                 = "pending"
	outcomeClosed                  = "closed"
	outcomeCancelled               = "cancelled"
	outcomeCancelledByLabelRemoval = "cancelled_by_label_removal"
	outcomeAlreadyClosedByHuman    = "already_closed_by_human"
	outcomeQuarantined             = "quarantined"
	outcomeDryRun                  = "dry_run"
)

type DiscoveryInput struct {
	ProjectID string
	Repo      string
	Limit     int
	Snapshot  *githubinfra.DiscoverySnapshot
}

type DiscoveryResult struct {
	QueueItems []storage.QueueItemRecord
	Skipped    int
}

type ProcessResult struct {
	QueueItemID string
	Status      string
	Summary     string
}

type GitHubGateway interface {
	ListOpenIssues(context.Context, githubinfra.ListOpenIssuesInput) ([]githubinfra.IssueSummary, error)
	ListOpenPullRequests(context.Context, githubinfra.ListOpenPullRequestsInput) ([]githubinfra.PullRequestSummary, error)
	ViewIssue(context.Context, githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error)
	ViewPullRequest(context.Context, githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error)
	ListReviewThreads(context.Context, githubinfra.ListReviewThreadsInput) ([]githubinfra.ReviewThread, error)
	ListIssueComments(context.Context, githubinfra.ViewIssueInput) ([]githubinfra.CommentInfo, error)
	ListIssueTimeline(context.Context, githubinfra.IssueTimelineInput) ([]map[string]any, error)
	ListIssueReactions(context.Context, githubinfra.IssueReactionInput) ([]githubinfra.IssueReaction, error)
	ListLinkedPullRequests(context.Context, githubinfra.LinkedPullRequestsInput) ([]githubinfra.LinkedPullRequest, error)
	ListPullRequestReviewState(context.Context, githubinfra.PullRequestReviewStateInput) (githubinfra.PullRequestReviewState, error)
	CreateIssueComment(context.Context, githubinfra.IssueCommentInput) (githubinfra.IssueCommentResult, error)
	UpdateIssueComment(context.Context, githubinfra.UpdateIssueCommentInput) error
	CloseIssue(context.Context, githubinfra.CloseIssueInput) error
	ClosePullRequest(context.Context, githubinfra.ClosePullRequestInput) error
	AddIssueLabels(context.Context, githubinfra.IssueLabelsInput) error
	RemoveIssueLabels(context.Context, githubinfra.IssueLabelsInput) error
}

type Options struct {
	Repos               *storage.Repositories
	GitHub              GitHubGateway
	Agent               AgentExecutor
	Logger              bootstrap.Logger
	Now                 func() time.Time
	Config              *config.Config
	AgentRuntime        string
	AgentModel          *string
	OnQueueItemEnqueued func()
}

type Runner struct {
	repos               *storage.Repositories
	github              GitHubGateway
	agent               AgentExecutor
	logger              bootstrap.Logger
	now                 func() time.Time
	config              *config.Config
	agentRuntime        string
	agentModel          *string
	claimer             string
	maxTry              int64
	retryDelay          time.Duration
	onQueueItemEnqueued func()
}

type payloadEnvelope struct {
	Sweeper sweeperPayload `json:"sweeper"`
}

type persistedSweeperPayload struct {
	CaseID     string `json:"case_id,omitempty"`
	ProposalID string `json:"proposal_id,omitempty"`
}

type sweeperPayload struct {
	CaseID            string `json:"case_id,omitempty"`
	ProposalID        string `json:"proposal_id,omitempty"`
	Phase             string `json:"phase,omitempty"`
	Outcome           string `json:"outcome,omitempty"`
	Category          string `json:"category,omitempty"`
	Confidence        int    `json:"confidence,omitempty"`
	Rationale         string `json:"rationale,omitempty"`
	Repo              string `json:"repo,omitempty"`
	TargetType        string `json:"target_type,omitempty"`
	TargetNumber      int64  `json:"target_number,omitempty"`
	WarningCommentID  int64  `json:"warning_comment_id,omitempty"`
	WarningMarkerUUID string `json:"warning_marker_uuid,omitempty"`
	WarningPostedAt   string `json:"warning_posted_at,omitempty"`
	CloseBy           string `json:"close_by,omitempty"`
	CommentBody       string `json:"comment_body,omitempty"`
	Summary           string `json:"summary,omitempty"`
	PendingLabel      string `json:"pending_label,omitempty"`
	ClosedLabel       string `json:"closed_label,omitempty"`
	KeepLabel         string `json:"keep_label,omitempty"`
	QuarantineLabel   string `json:"quarantine_label,omitempty"`
}

type sweeperStateRecord struct {
	item    storage.QueueItemRecord
	payload sweeperPayload
}

type liveTarget struct {
	Number              int64
	State               string
	Title               string
	Body                string
	CreatedAt           string
	UpdatedAt           string
	ClosedAt            string
	Author              string
	AuthorAssociation   string
	Labels              []string
	CommentCount        int
	IssueComments       []githubinfra.CommentInfo
	HeadSHA             string
	IsPR                bool
	Draft               bool
	RecentHumanComments []FactComment
	WarningComment      *FactWarningComment
	Timeline            FactTimeline
	LinkedPRs           []FactLinkedPR
	ReviewThreads       []githubinfra.ReviewThread
	PRReviewState       *FactPRReviewState
}

type caseDiscoveryState struct {
	Case           *storage.SweeperCaseRecord
	Legacy         sweeperStateRecord
	HasLegacy      bool
	LastProposalID string
	CloseDueAt     string
	Phase          string
	Outcome        string
}

type prefilterCandidate struct {
	TargetID       string
	TargetType     string
	TargetNumber   int64
	Labels         []string
	Author         string
	Association    string
	State          caseDiscoveryState
	DefaultPayload sweeperPayload
}

func New(options Options) *Runner {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Runner{repos: options.Repos, github: options.GitHub, agent: options.Agent, logger: options.Logger, now: now, config: options.Config, agentRuntime: options.AgentRuntime, agentModel: options.AgentModel, claimer: defaultClaimedBy, maxTry: defaultRetryMax, retryDelay: defaultRetryDelay, onQueueItemEnqueued: options.OnQueueItemEnqueued}
}

func (r *Runner) DiscoverIssues(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	return r.discoverIssuesAndClosures(ctx, input)
}

func (r *Runner) DiscoverPullRequests(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	return r.discoverPullRequestsAndClosures(ctx, input)
}

func (r *Runner) DiscoverReconcile(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	return r.discoverReconcile(ctx, input, false)
}

func (r *Runner) DiscoverMaintenanceReconcile(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	return r.discoverReconcile(ctx, input, true)
}

func (r *Runner) discoverReconcile(ctx context.Context, input DiscoveryInput, maintenance bool) (DiscoveryResult, error) {
	if r.repos == nil || r.repos.Projects == nil || r.repos.Queue == nil {
		return DiscoveryResult{}, fmt.Errorf("sweeper repositories are not configured")
	}
	project, roleCfg, err := r.projectConfig(ctx, input.ProjectID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	if project.Archived || (!maintenance && !roleCfg.AutoDiscovery) {
		return DiscoveryResult{Skipped: 1}, nil
	}
	limit := r.discoveryLimit(input.Limit, roleCfg.Triggers.MaxPerTick)
	items := make([]storage.QueueItemRecord, 0, limit)
	if r.repos.SweeperCases != nil {
		cases, err := r.repos.SweeperCases.ListByProjectRepoPhase(ctx, input.ProjectID, input.Repo, "warn")
		if err != nil {
			return DiscoveryResult{}, err
		}
		for _, caseRecord := range cases {
			if len(items) >= limit {
				break
			}
			if caseRecord.Status != "pending" {
				continue
			}
			payload := sweeperPayload{CaseID: caseRecord.ID}
			targetID := buildTargetID(caseRecord.Repo, caseRecord.TargetNumber)
			queueItem, ok, err := r.buildQueueItem(ctx, queueSeed{ProjectID: input.ProjectID, Repo: input.Repo, QueueType: QueueTypeReconcile, TargetType: caseRecord.TargetType, TargetID: targetID, Number: caseRecord.TargetNumber, Payload: payload})
			if err != nil {
				return DiscoveryResult{}, err
			}
			if ok {
				items = append(items, queueItem)
			}
		}
		if len(items) > 0 || maintenance {
			return DiscoveryResult{QueueItems: items}, nil
		}
	}
	states, err := r.latestSweeperRecords(ctx)
	if err != nil {
		return DiscoveryResult{}, err
	}
	for targetID, state := range states {
		if len(items) >= limit {
			break
		}
		if state.payload.Repo != input.Repo || state.payload.Outcome != outcomePending || state.payload.Phase != "warn" {
			continue
		}
		caseState, err := r.getCaseDiscoveryState(ctx, input.ProjectID, input.Repo, state.payload.TargetType, state.payload.TargetNumber, states)
		if err != nil {
			return DiscoveryResult{}, err
		}
		reconcilePayload := sweeperPayload{CaseID: firstNonEmpty(state.payload.CaseID, derefStringFromCase(caseState.Case))}
		queueItem, ok, err := r.buildQueueItem(ctx, queueSeed{ProjectID: input.ProjectID, Repo: input.Repo, QueueType: QueueTypeReconcile, TargetType: state.payload.TargetType, TargetID: targetID, Number: state.payload.TargetNumber, Payload: reconcilePayload})
		if err != nil {
			return DiscoveryResult{}, err
		}
		if !ok {
			continue
		}
		items = append(items, queueItem)
	}
	return DiscoveryResult{QueueItems: items}, nil
}

func (r *Runner) ProcessNext(ctx context.Context, claimedBy string) (*ProcessResult, error) {
	if r.repos == nil || r.repos.Queue == nil {
		return nil, fmt.Errorf("sweeper queue repository is not configured")
	}
	claimedBy = strings.TrimSpace(claimedBy)
	if claimedBy == "" {
		claimedBy = r.claimer
	}
	for _, queueType := range []string{QueueTypeWarn, QueueTypeClose, QueueTypeReconcile, QueueType} {
		item, err := r.repos.Queue.ClaimNextOfType(ctx, r.nowISO(), claimedBy, queueType)
		if err != nil {
			return nil, err
		}
		if item == nil {
			continue
		}
		return r.ProcessClaimedQueueItem(ctx, *item)
	}
	return nil, nil
}

func (r *Runner) ProcessClaimedQueueItem(ctx context.Context, queueItem storage.QueueItemRecord) (*ProcessResult, error) {
	if !isSupportedQueueType(queueItem.Type) {
		return nil, fmt.Errorf("unsupported sweeper queue item type %q", queueItem.Type)
	}
	if strings.TrimSpace(queueItem.ID) == "" {
		return nil, fmt.Errorf("sweeper queue item id is required")
	}
	if r.repos == nil || r.repos.Queue == nil {
		return nil, fmt.Errorf("sweeper queue repository is not configured")
	}
	stored, err := r.repos.Queue.GetByID(ctx, queueItem.ID)
	if err != nil {
		return nil, err
	}
	if stored != nil {
		queueItem = *stored
	}
	payload := r.readPayload(queueItem)
	if payload.Repo == "" && queueItem.Repo != nil {
		payload.Repo = *queueItem.Repo
	}
	if payload.TargetType == "" {
		payload.TargetType = queueItem.TargetType
	}
	if payload.TargetNumber == 0 {
		if targetNumber, parseErr := parseTargetNumber(queueItem); parseErr == nil {
			payload.TargetNumber = targetNumber
		}
	}
	var summary string
	var status string
	switch queueItem.Type {
	case QueueTypeWarn:
		payload, status, summary, err = r.processWarn(ctx, queueItem, payload)
	case QueueTypeClose:
		payload, status, summary, err = r.processClose(ctx, queueItem, payload)
	case QueueTypeReconcile:
		payload, status, summary, err = r.processReconcile(ctx, queueItem, payload)
	default:
		status = "skipped"
		summary = defaultSkippedSummary
	}
	queueItem.PayloadJSON = stringPtr(mustMarshalPayload(payload))
	queueItem.UpdatedAt = r.nowISO()
	if upsertErr := r.repos.Queue.Upsert(ctx, queueItem); upsertErr != nil {
		return nil, upsertErr
	}
	if err != nil {
		return r.recoverClaimedQueueItem(ctx, queueItem, err)
	}
	if err := r.repos.Queue.Complete(ctx, queueItem.ID, r.nowISO()); err != nil {
		return nil, err
	}
	return &ProcessResult{QueueItemID: queueItem.ID, Status: status, Summary: summary}, nil
}

func (r *Runner) recoverClaimedQueueItem(ctx context.Context, queueItem storage.QueueItemRecord, err error) (*ProcessResult, error) {
	failureKind, failureMessage := classifyQueueFailure(err)
	nowISO := r.nowISO()
	nextAttempts := queueItem.Attempts + 1
	if failureKind == "retryable_transient" && nextAttempts < queueItem.MaxAttempts {
		retryAt := eventlog.FormatJavaScriptISOString(r.now().Add(backoffDelay(r.retryDelay, nextAttempts)))
		if markErr := r.repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: queueItem.ID, AvailableAt: retryAt, Attempts: nextAttempts, ErrorMessage: stringPtr(failureMessage), ErrorKind: failureKind, UpdatedAt: nowISO}); markErr != nil {
			return nil, markErr
		}
	} else {
		if failErr := r.repos.Queue.Fail(ctx, storage.QueueFailInput{ID: queueItem.ID, Attempts: nextAttempts, FinishedAt: nowISO, ErrorMessage: stringPtr(failureMessage), ErrorKind: failureKind, UpdatedAt: nowISO}); failErr != nil {
			return nil, failErr
		}
	}
	return &ProcessResult{QueueItemID: queueItem.ID, Status: "failed", Summary: failureMessage}, nil
}

func (r *Runner) discoverIssuesAndClosures(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	ctx = githubinfra.ContextWithDiscoverySnapshot(ctx, input.Snapshot)
	project, roleCfg, err := r.projectConfig(ctx, input.ProjectID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	if project.Archived || !roleCfg.AutoDiscovery {
		return DiscoveryResult{Skipped: 1}, nil
	}
	if !roleCfg.Triggers.IncludeIssues {
		return DiscoveryResult{Skipped: 1}, nil
	}
	if r.github == nil {
		return DiscoveryResult{Skipped: 1}, nil
	}
	issues, err := r.github.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{Repo: input.Repo, Limit: r.discoveryLimit(input.Limit, roleCfg.Triggers.MaxPerTick*6), CWD: project.RepoPath})
	if err != nil {
		return DiscoveryResult{}, err
	}
	states, err := r.latestSweeperRecords(ctx)
	if err != nil {
		return DiscoveryResult{}, err
	}
	warnLimit, closeLimit, err := r.discoveryBudgets(ctx, input, roleCfg)
	if err != nil {
		return DiscoveryResult{}, err
	}
	result := DiscoveryResult{QueueItems: make([]storage.QueueItemRecord, 0, warnLimit+closeLimit)}
	warnCount := 0
	closeCount := 0
	for _, issue := range issues {
		if issue.IsPullRequest {
			continue
		}
		targetID := buildTargetID(input.Repo, issue.Number)
		legacyState := states[targetID]
		if r.shouldSkipSummary(issue.Labels, issue.Author, legacyState, roleCfg) {
			result.Skipped++
			continue
		}
		caseState, err := r.getCaseDiscoveryState(ctx, input.ProjectID, input.Repo, "issue", issue.Number, states)
		if err != nil {
			return DiscoveryResult{}, err
		}
		var associationOK bool
		issue.AuthorAssociation, associationOK = r.summaryAuthorAssociation(ctx, input.Repo, project.RepoPath, issue.Number, issue.AuthorAssociation, roleCfg)
		if !associationOK {
			result.Skipped++
			continue
		}
		candidate := prefilterCandidate{TargetID: targetID, TargetType: "issue", TargetNumber: issue.Number, Labels: issue.Labels, Author: issue.Author, Association: issue.AuthorAssociation, State: caseState, DefaultPayload: sweeperPayload{CaseID: derefStringFromCase(caseState.Case)}}
		if r.prefilterSkipCandidate(candidate, legacyState, roleCfg) {
			result.Skipped++
			continue
		}
		if hasLabel(issue.Labels, roleCfg.Lifecycle.PendingLabel) {
			closeDueAt := caseState.CloseDueAt
			if closeDueAt == "" && caseState.HasLegacy {
				closeDueAt = caseState.Legacy.payload.CloseBy
			}
			if caseState.Phase != "warn" || !dueForClose(closeDueAt, r.now()) || closeCount >= closeLimit || (caseState.Case == nil && (!caseState.HasLegacy || caseState.Legacy.payload.Outcome != outcomePending)) {
				continue
			}
			closePayload := sweeperPayload{CaseID: derefStringFromCase(caseState.Case)}
			if caseState.HasLegacy {
				closePayload.CaseID = firstNonEmpty(closePayload.CaseID, caseState.Legacy.payload.CaseID)
			}
			queueItem, ok, err := r.buildQueueItem(ctx, queueSeed{ProjectID: input.ProjectID, Repo: input.Repo, QueueType: QueueTypeClose, TargetType: "issue", TargetID: targetID, Number: issue.Number, Payload: closePayload})
			if err != nil {
				return DiscoveryResult{}, err
			}
			if ok {
				result.QueueItems = append(result.QueueItems, queueItem)
				closeCount++
			}
			continue
		}
		if warnCount >= warnLimit {
			continue
		}
		warnPayload := candidate.DefaultPayload
		queueItem, ok, err := r.buildQueueItem(ctx, queueSeed{ProjectID: input.ProjectID, Repo: input.Repo, QueueType: QueueTypeWarn, TargetType: "issue", TargetID: targetID, Number: issue.Number, Payload: warnPayload})
		if err != nil {
			return DiscoveryResult{}, err
		}
		if ok {
			result.QueueItems = append(result.QueueItems, queueItem)
			warnCount++
		}
	}
	if r.logger != nil {
		r.logger.Debug("sweeper issue discovery summary", map[string]any{"repo": input.Repo, "filteredOut": result.Skipped, "queued": len(result.QueueItems), "agentReviewed": 0})
	}
	return result, nil
}

func (r *Runner) discoverPullRequestsAndClosures(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	ctx = githubinfra.ContextWithDiscoverySnapshot(ctx, input.Snapshot)
	project, roleCfg, err := r.projectConfig(ctx, input.ProjectID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	if project.Archived || !roleCfg.AutoDiscovery {
		return DiscoveryResult{Skipped: 1}, nil
	}
	if !roleCfg.Triggers.IncludePullRequests {
		return DiscoveryResult{Skipped: 1}, nil
	}
	if r.github == nil {
		return DiscoveryResult{Skipped: 1}, nil
	}
	prs, err := r.github.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, Limit: r.discoveryLimit(input.Limit, roleCfg.Triggers.MaxPerTick*6), CWD: project.RepoPath})
	if err != nil {
		return DiscoveryResult{}, err
	}
	states, err := r.latestSweeperRecords(ctx)
	if err != nil {
		return DiscoveryResult{}, err
	}
	warnLimit, closeLimit, err := r.discoveryBudgets(ctx, input, roleCfg)
	if err != nil {
		return DiscoveryResult{}, err
	}
	result := DiscoveryResult{QueueItems: make([]storage.QueueItemRecord, 0, warnLimit+closeLimit)}
	warnCount := 0
	closeCount := 0
	for _, pr := range prs {
		if pr.IsDraft && !roleCfg.Triggers.IncludeDrafts {
			result.Skipped++
			continue
		}
		targetID := buildTargetID(input.Repo, pr.Number)
		legacyState := states[targetID]
		if r.shouldSkipSummary(pr.Labels, pr.Author, legacyState, roleCfg) {
			result.Skipped++
			continue
		}
		caseState, err := r.getCaseDiscoveryState(ctx, input.ProjectID, input.Repo, "pull_request", pr.Number, states)
		if err != nil {
			return DiscoveryResult{}, err
		}
		var associationOK bool
		pr.AuthorAssociation, associationOK = r.summaryAuthorAssociation(ctx, input.Repo, project.RepoPath, pr.Number, pr.AuthorAssociation, roleCfg)
		if !associationOK {
			result.Skipped++
			continue
		}
		candidate := prefilterCandidate{TargetID: targetID, TargetType: "pull_request", TargetNumber: pr.Number, Labels: pr.Labels, Author: pr.Author, Association: pr.AuthorAssociation, State: caseState, DefaultPayload: sweeperPayload{CaseID: derefStringFromCase(caseState.Case)}}
		if r.prefilterSkipCandidate(candidate, legacyState, roleCfg) {
			result.Skipped++
			continue
		}
		if hasLabel(pr.Labels, roleCfg.Lifecycle.PendingLabel) {
			closeDueAt := caseState.CloseDueAt
			if closeDueAt == "" && caseState.HasLegacy {
				closeDueAt = caseState.Legacy.payload.CloseBy
			}
			if caseState.Phase != "warn" || !dueForClose(closeDueAt, r.now()) || closeCount >= closeLimit || (caseState.Case == nil && (!caseState.HasLegacy || caseState.Legacy.payload.Outcome != outcomePending)) {
				continue
			}
			closePayload := sweeperPayload{CaseID: derefStringFromCase(caseState.Case)}
			if caseState.HasLegacy {
				closePayload.CaseID = firstNonEmpty(closePayload.CaseID, caseState.Legacy.payload.CaseID)
			}
			queueItem, ok, err := r.buildQueueItem(ctx, queueSeed{ProjectID: input.ProjectID, Repo: input.Repo, QueueType: QueueTypeClose, TargetType: "pull_request", TargetID: targetID, Number: pr.Number, Payload: closePayload})
			if err != nil {
				return DiscoveryResult{}, err
			}
			if ok {
				result.QueueItems = append(result.QueueItems, queueItem)
				closeCount++
			}
			continue
		}
		if warnCount >= warnLimit {
			continue
		}
		warnPayload := candidate.DefaultPayload
		queueItem, ok, err := r.buildQueueItem(ctx, queueSeed{ProjectID: input.ProjectID, Repo: input.Repo, QueueType: QueueTypeWarn, TargetType: "pull_request", TargetID: targetID, Number: pr.Number, Payload: warnPayload})
		if err != nil {
			return DiscoveryResult{}, err
		}
		if ok {
			result.QueueItems = append(result.QueueItems, queueItem)
			warnCount++
		}
	}
	if r.logger != nil {
		r.logger.Debug("sweeper pull request discovery summary", map[string]any{"repo": input.Repo, "filteredOut": result.Skipped, "queued": len(result.QueueItems), "agentReviewed": 0})
	}
	return result, nil
}

type queueSeed struct {
	ProjectID  string
	Repo       string
	QueueType  string
	TargetType string
	TargetID   string
	Number     int64
	Payload    sweeperPayload
}

func (r *Runner) buildQueueItem(ctx context.Context, seed queueSeed) (storage.QueueItemRecord, bool, error) {
	dedupeKey := fmt.Sprintf("sweeper:%s:%s", strings.TrimPrefix(seed.QueueType, "sweeper:"), seed.TargetID)
	existing, err := r.repos.Queue.FindActiveByDedupe(ctx, dedupeKey)
	if err != nil {
		return storage.QueueItemRecord{}, false, err
	}
	if existing != nil {
		return storage.QueueItemRecord{}, false, nil
	}
	nowISO := r.nowISO()
	repo := seed.Repo
	var prNumber *int64
	if seed.TargetType == "pull_request" {
		prNumber = &seed.Number
	}
	payloadJSON := mustMarshalPayload(seed.Payload)
	item := storage.QueueItemRecord{
		ID:          eventlog.NewEventID("queue"),
		ProjectID:   stringPtr(seed.ProjectID),
		Type:        seed.QueueType,
		TargetType:  seed.TargetType,
		TargetID:    seed.TargetID,
		Repo:        &repo,
		PRNumber:    prNumber,
		DedupeKey:   dedupeKey,
		Priority:    defaultQueuePriority,
		Status:      "queued",
		AvailableAt: nowISO,
		Attempts:    0,
		MaxAttempts: r.maxTry,
		LockKey:     stringPtr("sweeper:" + seed.TargetID),
		PayloadJSON: &payloadJSON,
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}
	_, created, err := r.repos.Queue.CreateOrGetActiveByDedupe(ctx, item)
	if err != nil {
		return storage.QueueItemRecord{}, false, err
	}
	if created {
		r.wakeSchedulerAfterEnqueue()
		return item, true, nil
	}
	return storage.QueueItemRecord{}, false, nil
}

func (r *Runner) wakeSchedulerAfterEnqueue() {
	if r.onQueueItemEnqueued != nil {
		r.onQueueItemEnqueued()
	}
}

func (r *Runner) processWarn(ctx context.Context, queueItem storage.QueueItemRecord, payload sweeperPayload) (sweeperPayload, string, string, error) {
	project, roleCfg, err := r.projectConfig(ctx, derefString(queueItem.ProjectID))
	if err != nil {
		return payload, "failed", "", err
	}
	target, err := r.loadTarget(ctx, queueItem)
	if err != nil {
		return payload, "failed", "", err
	}
	payload.Repo = derefString(queueItem.Repo)
	caseRecord, err := r.ensureCase(ctx, derefString(queueItem.ProjectID), target, payload, roleCfg)
	if err != nil {
		return payload, "failed", "", err
	}
	if caseRecord != nil {
		payload.CaseID = caseRecord.ID
		if payload.WarningCommentID == 0 {
			payload.WarningCommentID = derefInt64(caseRecord.WarningCommentID)
		}
		if payload.WarningMarkerUUID == "" {
			payload.WarningMarkerUUID = derefString(caseRecord.WarningMarkerUUID)
		}
		if payload.WarningPostedAt == "" {
			payload.WarningPostedAt = derefString(caseRecord.WarnedAt)
		}
		if payload.CloseBy == "" {
			payload.CloseBy = derefString(caseRecord.CloseDueAt)
		}
	}
	existingProposal, err := r.loadProposalForApply(ctx, payload, caseRecord)
	if err != nil {
		return payload, "failed", "", err
	}
	category, confidence, rationale := classifyTarget(target, roleCfg, r.now())
	payload.Phase = "warn"
	payload.Outcome = outcomeNoAction
	payload.Category = category
	payload.Confidence = confidence
	payload.Rationale = rationale
	payload.TargetType = queueItem.TargetType
	payload.TargetNumber = target.Number
	payload.PendingLabel = roleCfg.Lifecycle.PendingLabel
	payload.ClosedLabel = roleCfg.Lifecycle.ClosedLabel
	payload.KeepLabel = roleCfg.Lifecycle.KeepLabel
	payload.QuarantineLabel = roleCfg.Security.QuarantineLabel
	decision := categoryDecisionForPhase("warn", category)
	proposal, fingerprintJSON, err := r.persistProposal(ctx, derefString(queueItem.ProjectID), target, payload, caseRecord, roleCfg, decision, category, confidence, rationale)
	if err != nil {
		return payload, "failed", "", err
	}
	if diagnosticModeEnabled(roleCfg) && existingProposal != nil {
		existingProposal = nil
	}
	if r.agentApplyEnabled(roleCfg) && agentEligibleCategory(category) {
		if validAgentApplyProposal(existingProposal, "warn", roleCfg.Proposer.SchemaVersion) {
			proposal = existingProposal
			fingerprintJSON = existingProposal.FingerprintJSON
			payload = hydratePayloadFromProposal(payload, proposal)
			decision = proposal.Decision
			category = proposal.Category
			confidence = int(proposal.ConfidenceScore)
			rationale = derefString(proposal.Rationale)
			payload.Category = category
			payload.Confidence = confidence
			payload.Rationale = rationale
		} else {
			proposal, fingerprintJSON, err = r.proposeAgentDecision(ctx, project, queueItem, target, caseRecord, payload, roleCfg, "warn", category, rationale)
			if err != nil {
				return payload, "failed", "", err
			}
			payload = hydratePayloadFromProposal(payload, proposal)
			decision = proposal.Decision
			category = proposal.Category
			confidence = int(proposal.ConfidenceScore)
			rationale = derefString(proposal.Rationale)
			payload.Category = category
			payload.Confidence = confidence
			payload.Rationale = rationale
		}
	}
	if proposal != nil {
		payload.ProposalID = proposal.ID
	}
	if category == categoryNone {
		payload.Summary = defaultSkippedSummary
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "skipped_no_action", payload.Summary, nil, false); err != nil {
			return payload, "failed", "", err
		}
		if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
			return payload, "failed", "", err
		}
		return payload, "skipped", defaultSkippedSummary, nil
	}
	if category == categoryRouteSecurity {
		if roleCfg.DryRun || roleCfg.Limits.GlobalKillSwitch || r.github == nil || forcedDryRunCategory(roleCfg, category) {
			payload.Outcome = outcomeDryRun
			payload.Summary = "sweeper dry-run quarantine"
			if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "skipped_dry_run", payload.Summary, nil, false); err != nil {
				return payload, "failed", "", err
			}
			if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
				return payload, "failed", "", err
			}
			return payload, "skipped", payload.Summary, nil
		}
		if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: payload.Repo, IssueNumber: target.Number, Labels: []string{roleCfg.Security.QuarantineLabel}}); err != nil {
			return payload, "failed", "", err
		}
		payload.Outcome = outcomeQuarantined
		payload.Summary = "sweeper quarantined target"
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "completed_quarantined", payload.Summary, nil, true); err != nil {
			return payload, "failed", "", err
		}
		if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
			return payload, "failed", "", err
		}
		return payload, "completed", payload.Summary, nil
	}
	graceDays := gracePeriodForCategory(category, roleCfg)
	if proposal != nil && proposal.MarkerUUID != nil {
		payload.WarningMarkerUUID = *proposal.MarkerUUID
	} else if strings.TrimSpace(payload.WarningMarkerUUID) == "" {
		payload.WarningMarkerUUID = NewMarkerUUID()
	}
	haveWarningComment := false
	if existingComment := markerComment(target.IssueComments, payload.WarningMarkerUUID); existingComment != nil {
		payload.WarningCommentID = existingComment.ID
		if strings.TrimSpace(payload.WarningPostedAt) == "" {
			payload.WarningPostedAt = strings.TrimSpace(existingComment.CreatedAt)
		}
		if strings.TrimSpace(payload.CloseBy) == "" {
			payload.CloseBy = warningCommentCloseBy(existingComment.Body)
		}
		haveWarningComment = true
	} else {
		payload.WarningCommentID = 0
		payload.WarningPostedAt = ""
		payload.CloseBy = ""
	}
	if strings.TrimSpace(payload.WarningPostedAt) == "" {
		payload.WarningPostedAt = r.nowISO()
	}
	if strings.TrimSpace(payload.CloseBy) == "" {
		payload.CloseBy = r.now().UTC().Add(time.Duration(graceDays) * 24 * time.Hour).Format(javaScriptISOStringUTC)
	}
	payload.CommentBody = buildWarningComment(target, payload, graceDays)
	if haveWarningComment {
		if hasLabel(target.Labels, roleCfg.Lifecycle.PendingLabel) {
			payload.Outcome = outcomePending
			payload.Summary = fmt.Sprintf("sweeper warned %s #%d", targetKind(target.IsPR), target.Number)
			if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "completed_warned", payload.Summary, nil, true); err != nil {
				return payload, "failed", "", err
			}
			if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
				return payload, "failed", "", err
			}
			return payload, "completed", payload.Summary, nil
		}
	}
	if roleCfg.DryRun || roleCfg.Limits.GlobalKillSwitch || r.github == nil || forcedDryRunCategory(roleCfg, category) {
		payload.Outcome = outcomeDryRun
		payload.Summary = "sweeper dry-run warning"
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "skipped_dry_run", payload.Summary, nil, false); err != nil {
			return payload, "failed", "", err
		}
		if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
			return payload, "failed", "", err
		}
		return payload, "skipped", payload.Summary, nil
	}
	if !haveWarningComment {
		comment, err := r.github.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: payload.Repo, IssueNumber: target.Number, Body: payload.CommentBody})
		if err != nil {
			applyErr := err.Error()
			_ = r.updateProposalApplyReceipt(ctx, payload.ProposalID, "failed_retryable", "warning comment failed", &applyErr, false)
			return payload, "failed", "", err
		}
		payload.WarningCommentID = comment.ID
		if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
			return payload, "failed", "", err
		}
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "partial:commented", "warning comment posted", nil, false); err != nil {
			return payload, "failed", "", err
		}
	}
	if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: payload.Repo, IssueNumber: target.Number, Labels: []string{roleCfg.Lifecycle.PendingLabel}}); err != nil {
		applyErr := err.Error()
		_ = r.updateProposalApplyReceipt(ctx, payload.ProposalID, "failed_retryable", "warning label failed", &applyErr, false)
		_ = r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg)
		return payload, "failed", "", err
	}
	if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "partial:labeled", "warning label added", nil, false); err != nil {
		return payload, "failed", "", err
	}
	payload.Outcome = outcomePending
	payload.Summary = fmt.Sprintf("sweeper warned %s #%d", targetKind(target.IsPR), target.Number)
	if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "completed_warned", payload.Summary, nil, true); err != nil {
		return payload, "failed", "", err
	}
	if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
		return payload, "failed", "", err
	}
	return payload, "completed", payload.Summary, nil
}

func (r *Runner) processClose(ctx context.Context, queueItem storage.QueueItemRecord, payload sweeperPayload) (sweeperPayload, string, string, error) {
	project, roleCfg, err := r.projectConfig(ctx, derefString(queueItem.ProjectID))
	if err != nil {
		return payload, "failed", "", err
	}
	target, err := r.loadTarget(ctx, queueItem)
	if err != nil {
		return payload, "failed", "", err
	}
	payload.Repo = derefString(queueItem.Repo)
	caseRecord, err := r.ensureCase(ctx, derefString(queueItem.ProjectID), target, payload, roleCfg)
	if err != nil {
		return payload, "failed", "", err
	}
	payload.Phase = "close"
	payload.TargetType = queueItem.TargetType
	payload.TargetNumber = target.Number
	payload.PendingLabel = roleCfg.Lifecycle.PendingLabel
	payload.ClosedLabel = roleCfg.Lifecycle.ClosedLabel
	payload.KeepLabel = roleCfg.Lifecycle.KeepLabel
	payload.QuarantineLabel = roleCfg.Security.QuarantineLabel
	if caseRecord != nil {
		payload.CaseID = caseRecord.ID
		if payload.Category == "" {
			payload.Category = derefString(caseRecord.CurrentCategory)
		}
		if payload.Confidence == 0 {
			payload.Confidence = int(derefInt64(caseRecord.CurrentConfidenceScore))
		}
		if payload.WarningCommentID == 0 {
			payload.WarningCommentID = derefInt64(caseRecord.WarningCommentID)
		}
		if payload.WarningMarkerUUID == "" {
			payload.WarningMarkerUUID = derefString(caseRecord.WarningMarkerUUID)
		}
		if payload.WarningPostedAt == "" {
			payload.WarningPostedAt = derefString(caseRecord.WarnedAt)
		}
		if payload.CloseBy == "" {
			payload.CloseBy = derefString(caseRecord.CloseDueAt)
		}
	}
	applyProposal, err := r.loadProposalForApply(ctx, payload, caseRecord)
	if err != nil {
		return payload, "failed", "", err
	}
	payload = hydratePayloadFromProposal(payload, applyProposal)
	if strings.TrimSpace(payload.CommentBody) == "" && payload.Category != "" && payload.Rationale != "" && payload.CloseBy != "" {
		payload.CommentBody = buildWarningComment(target, payload, gracePeriodForCategory(payload.Category, roleCfg))
	}
	category, confidence, rationale := classifyTarget(target, roleCfg, r.now())
	decision := categoryDecisionForPhase("close", category)
	var (
		proposal        *storage.SweeperProposalRecord
		fingerprintJSON string
	)
	if diagnosticModeEnabled(roleCfg) && r.agentApplyEnabled(roleCfg) && agentEligibleCategory(category) {
		if _, _, err = r.persistProposal(ctx, derefString(queueItem.ProjectID), target, payload, caseRecord, roleCfg, decision, category, confidence, rationale); err != nil {
			return payload, "failed", "", err
		}
		applyProposal = nil
	}
	if r.agentApplyEnabled(roleCfg) && agentEligibleCategory(category) {
		if applyProposal != nil && applyProposal.Decision == "close" {
			if !validAgentApplyProposal(applyProposal, "close", roleCfg.Proposer.SchemaVersion) {
				payload.Outcome = outcomeNoAction
				payload.Summary = "sweeper agent proposal required"
				_ = r.updateProposalApplyReceipt(ctx, applyProposal.ID, "skipped_schema_obsolete", payload.Summary, nil, false)
				applyProposal = nil
				payload.ProposalID = ""
			} else {
				proposal = applyProposal
				fingerprintJSON = applyProposal.FingerprintJSON
			}
		}
		if proposal == nil {
			proposal, fingerprintJSON, err = r.proposeAgentDecision(ctx, project, queueItem, target, caseRecord, payload, roleCfg, "close", category, rationale)
			if err != nil {
				return payload, "failed", "", err
			}
			payload = hydratePayloadFromProposal(payload, proposal)
			decision = proposal.Decision
			category = proposal.Category
			confidence = int(proposal.ConfidenceScore)
			rationale = derefString(proposal.Rationale)
			payload.Category = category
			payload.Confidence = confidence
			payload.Rationale = rationale
		}
	} else {
		proposal, fingerprintJSON, err = r.persistProposal(ctx, derefString(queueItem.ProjectID), target, payload, caseRecord, roleCfg, decision, category, confidence, rationale)
		if err != nil {
			return payload, "failed", "", err
		}
	}
	if proposal != nil {
		payload.ProposalID = proposal.ID
	}
	applyStatus := ""
	if proposal != nil && proposal.ApplyStatus != nil {
		applyStatus = *proposal.ApplyStatus
	}
	stale, priorProposal, fingerprintJSON, err := r.staleProposalStatusForApply(target, caseRecord, roleCfg, proposal)
	if err != nil {
		return payload, "failed", "", err
	}
	if stale {
		payload.Outcome = outcomeNoAction
		payload.Summary = "sweeper stale proposal"
		if priorProposal != nil {
			payload.ProposalID = priorProposal.ID
		}
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "skipped_stale_proposal", payload.Summary, nil, false); err != nil {
			return payload, "failed", "", err
		}
		if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, priorProposal, fingerprintJSON, roleCfg); err != nil {
			return payload, "failed", "", err
		}
		return payload, "skipped", payload.Summary, nil
	}
	if strings.EqualFold(target.State, "closed") {
		if err := r.removePendingLabel(ctx, roleCfg, payload.Repo, target.Number); err != nil {
			return payload, "failed", "", err
		}
		payload.Outcome = outcomeAlreadyClosedByHuman
		payload.Summary = "target already closed"
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "completed_cancelled", payload.Summary, nil, true); err != nil {
			return payload, "failed", "", err
		}
		if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
			return payload, "failed", "", err
		}
		return payload, "completed", payload.Summary, nil
	}
	if !hasLabel(target.Labels, roleCfg.Lifecycle.PendingLabel) && applyStatus != "partial:labeled" {
		payload.Outcome = outcomeCancelled
		payload.Summary = "sweeper warning cancelled"
		if payload.WarningCommentID > 0 && r.github != nil && !roleCfg.DryRun {
			_ = r.github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: payload.Repo, CommentID: payload.WarningCommentID, Body: payload.CommentBody + "\n\nCancellation noted by sweeper."})
		}
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "completed_cancelled", payload.Summary, nil, true); err != nil {
			return payload, "failed", "", err
		}
		if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
			return payload, "failed", "", err
		}
		return payload, "completed", payload.Summary, nil
	}
	if hasLabel(target.Labels, roleCfg.Lifecycle.KeepLabel) {
		payload.Outcome = outcomeCancelled
		payload.Summary = "sweeper warning cancelled"
		if r.github != nil && !roleCfg.DryRun {
			if err := r.github.RemoveIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: payload.Repo, IssueNumber: target.Number, Labels: []string{roleCfg.Lifecycle.PendingLabel}}); err != nil {
				return payload, "failed", "", err
			}
		}
		if payload.WarningCommentID > 0 && r.github != nil && !roleCfg.DryRun {
			_ = r.github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: payload.Repo, CommentID: payload.WarningCommentID, Body: payload.CommentBody + "\n\nCancellation noted by sweeper."})
		}
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "completed_cancelled", payload.Summary, nil, true); err != nil {
			return payload, "failed", "", err
		}
		if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
			return payload, "failed", "", err
		}
		return payload, "completed", payload.Summary, nil
	}
	if category == categoryRouteSecurity {
		if roleCfg.DryRun || roleCfg.Limits.GlobalKillSwitch || r.github == nil || forcedDryRunCategory(roleCfg, category) {
			payload.Outcome = outcomeDryRun
			payload.Summary = "sweeper dry-run quarantine"
			if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "skipped_dry_run", payload.Summary, nil, false); err != nil {
				return payload, "failed", "", err
			}
			if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
				return payload, "failed", "", err
			}
			return payload, "skipped", payload.Summary, nil
		}
		if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: payload.Repo, IssueNumber: target.Number, Labels: []string{roleCfg.Security.QuarantineLabel}}); err != nil {
			return payload, "failed", "", err
		}
		_ = r.github.RemoveIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: payload.Repo, IssueNumber: target.Number, Labels: []string{roleCfg.Lifecycle.PendingLabel}})
		payload.Outcome = outcomeQuarantined
		payload.Summary = "sweeper quarantined target"
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "completed_quarantined", payload.Summary, nil, true); err != nil {
			return payload, "failed", "", err
		}
		if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
			return payload, "failed", "", err
		}
		return payload, "completed", payload.Summary, nil
	}
	if category == categoryNone || (payload.Category != "" && category != payload.Category) {
		if err := r.removePendingLabel(ctx, roleCfg, payload.Repo, target.Number); err != nil {
			return payload, "failed", "", err
		}
		payload.Outcome = outcomeCancelled
		payload.Summary = "sweeper close cancelled"
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "completed_cancelled", payload.Summary, nil, true); err != nil {
			return payload, "failed", "", err
		}
		if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
			return payload, "failed", "", err
		}
		return payload, "completed", payload.Summary, nil
	}
	closeComment := buildCloseComment(target, payload, rationale)
	if roleCfg.DryRun || roleCfg.Limits.GlobalKillSwitch || r.github == nil || forcedDryRunCategory(roleCfg, category) {
		payload.Outcome = outcomeDryRun
		payload.Summary = "sweeper dry-run close"
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "skipped_dry_run", payload.Summary, nil, false); err != nil {
			return payload, "failed", "", err
		}
		if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
			return payload, "failed", "", err
		}
		return payload, "skipped", payload.Summary, nil
	}
	if applyStatus != "partial:commented" && applyStatus != "partial:labeled" {
		if _, err := r.github.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: payload.Repo, IssueNumber: target.Number, Body: closeComment}); err != nil {
			applyErr := err.Error()
			_ = r.updateProposalApplyReceipt(ctx, payload.ProposalID, "failed_retryable", "close comment failed", &applyErr, false)
			return payload, "failed", "", err
		}
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "partial:commented", "close comment posted", nil, false); err != nil {
			return payload, "failed", "", err
		}
	}
	if target.IsPR {
		if err := r.github.ClosePullRequest(ctx, githubinfra.ClosePullRequestInput{Repo: payload.Repo, PRNumber: target.Number}); err != nil {
			applyErr := err.Error()
			_ = r.updateProposalApplyReceipt(ctx, payload.ProposalID, "failed_retryable", "close target failed", &applyErr, false)
			return payload, "failed", "", err
		}
	} else {
		reason := "not_planned"
		if category == categoryAlreadyFixed {
			reason = "completed"
		}
		if err := r.github.CloseIssue(ctx, githubinfra.CloseIssueInput{Repo: payload.Repo, IssueNumber: target.Number, StateReason: reason}); err != nil {
			applyErr := err.Error()
			_ = r.updateProposalApplyReceipt(ctx, payload.ProposalID, "failed_retryable", "close target failed", &applyErr, false)
			return payload, "failed", "", err
		}
	}
	if applyStatus != "partial:labeled" {
		if err := r.github.RemoveIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: payload.Repo, IssueNumber: target.Number, Labels: []string{roleCfg.Lifecycle.PendingLabel}}); err != nil {
			applyErr := err.Error()
			_ = r.updateProposalApplyReceipt(ctx, payload.ProposalID, "failed_retryable", "remove pending label failed", &applyErr, false)
			return payload, "failed", "", err
		}
		if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: payload.Repo, IssueNumber: target.Number, Labels: []string{roleCfg.Lifecycle.ClosedLabel}}); err != nil {
			applyErr := err.Error()
			_ = r.updateProposalApplyReceipt(ctx, payload.ProposalID, "failed_retryable", "add closed label failed", &applyErr, false)
			return payload, "failed", "", err
		}
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "partial:labeled", "close labels updated", nil, false); err != nil {
			return payload, "failed", "", err
		}
	}
	payload.Outcome = outcomeClosed
	payload.Summary = fmt.Sprintf("sweeper closed %s #%d", targetKind(target.IsPR), target.Number)
	if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "completed_closed", payload.Summary, nil, true); err != nil {
		return payload, "failed", "", err
	}
	if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
		return payload, "failed", "", err
	}
	return payload, "completed", payload.Summary, nil
}

func (r *Runner) processReconcile(ctx context.Context, queueItem storage.QueueItemRecord, payload sweeperPayload) (sweeperPayload, string, string, error) {
	roleCfg := r.roleConfig(derefString(queueItem.ProjectID))
	if r.github == nil {
		payload.Outcome = outcomeNoAction
		payload.Summary = defaultSkippedSummary
		return payload, "skipped", payload.Summary, nil
	}
	target, err := r.loadTarget(ctx, queueItem)
	if err != nil {
		return payload, "failed", "", err
	}
	payload.Repo = derefString(queueItem.Repo)
	caseRecord, err := r.ensureCase(ctx, derefString(queueItem.ProjectID), target, payload, roleCfg)
	if err != nil {
		return payload, "failed", "", err
	}
	if caseRecord != nil {
		payload.CaseID = caseRecord.ID
		if payload.Category == "" {
			payload.Category = derefString(caseRecord.CurrentCategory)
		}
		if payload.Confidence == 0 {
			payload.Confidence = int(derefInt64(caseRecord.CurrentConfidenceScore))
		}
		if payload.WarningCommentID == 0 {
			payload.WarningCommentID = derefInt64(caseRecord.WarningCommentID)
		}
		if payload.WarningMarkerUUID == "" {
			payload.WarningMarkerUUID = derefString(caseRecord.WarningMarkerUUID)
		}
		if payload.WarningPostedAt == "" {
			payload.WarningPostedAt = derefString(caseRecord.WarnedAt)
		}
		if payload.CloseBy == "" {
			payload.CloseBy = derefString(caseRecord.CloseDueAt)
		}
	}
	applyProposal, err := r.loadProposalForApply(ctx, payload, caseRecord)
	if err != nil {
		return payload, "failed", "", err
	}
	payload = hydratePayloadFromProposal(payload, applyProposal)
	if strings.TrimSpace(payload.CommentBody) == "" && payload.Category != "" && payload.Rationale != "" && payload.CloseBy != "" {
		payload.CommentBody = buildWarningComment(target, payload, gracePeriodForCategory(payload.Category, roleCfg))
	}
	proposal, fingerprintJSON, err := r.persistProposal(ctx, derefString(queueItem.ProjectID), target, payload, caseRecord, roleCfg, "cancel", payload.Category, payload.Confidence, payload.Rationale)
	if err != nil {
		return payload, "failed", "", err
	}
	if proposal != nil {
		payload.ProposalID = proposal.ID
	}
	if hasLabel(target.Labels, roleCfg.Lifecycle.PendingLabel) {
		payload.Summary = "sweeper reconcile: still pending"
		if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "skipped_no_action", payload.Summary, nil, false); err != nil {
			return payload, "failed", "", err
		}
		syncPayload := payload
		syncPayload.Phase = "warn"
		syncPayload.Outcome = outcomePending
		if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, syncPayload, proposal, fingerprintJSON, roleCfg); err != nil {
			return payload, "failed", "", err
		}
		return payload, "skipped", payload.Summary, nil
	}
	payload.Phase = "reconcile"
	if payload.WarningCommentID > 0 && !roleCfg.DryRun {
		_ = r.github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: derefString(queueItem.Repo), CommentID: payload.WarningCommentID, Body: payload.CommentBody + "\n\nCancellation acknowledged because the pending label was removed."})
	}
	payload.Outcome = outcomeCancelledByLabelRemoval
	payload.Summary = "sweeper reconciled removed pending label"
	if err := r.updateProposalApplyReceipt(ctx, payload.ProposalID, "completed_cancelled", payload.Summary, nil, true); err != nil {
		return payload, "failed", "", err
	}
	if err := r.syncCase(ctx, derefString(queueItem.ProjectID), target, payload, proposal, fingerprintJSON, roleCfg); err != nil {
		return payload, "failed", "", err
	}
	return payload, "completed", payload.Summary, nil
}

func (r *Runner) loadTarget(ctx context.Context, item storage.QueueItemRecord) (liveTarget, error) {
	if r.github == nil {
		return liveTarget{}, nil
	}
	repo := derefString(item.Repo)
	number, err := parseTargetNumber(item)
	if err != nil {
		return liveTarget{}, err
	}
	if item.TargetType == "pull_request" {
		detail, err := r.github.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: number})
		if err != nil {
			return liveTarget{}, err
		}
		return r.enrichTargetFacts(ctx, repo, liveTarget{Number: detail.Number, State: detail.State, Title: detail.Title, Body: detail.Body, CreatedAt: detail.CreatedAt, UpdatedAt: detail.UpdatedAt, ClosedAt: detail.ClosedAt, Author: detail.Author, AuthorAssociation: detail.AuthorAssociation, Labels: append([]string(nil), detail.Labels...), CommentCount: detail.CommentCount, IssueComments: append([]githubinfra.CommentInfo(nil), detail.IssueComments...), HeadSHA: detail.HeadSHA, IsPR: true, Draft: detail.IsDraft})
	}
	detail, err := r.github.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: number})
	if err != nil {
		return liveTarget{}, err
	}
	return r.enrichTargetFacts(ctx, repo, liveTarget{Number: detail.Number, State: detail.State, Title: detail.Title, Body: detail.Body, CreatedAt: detail.CreatedAt, UpdatedAt: detail.UpdatedAt, ClosedAt: detail.ClosedAt, Author: detail.Author, AuthorAssociation: detail.AuthorAssociation, Labels: append([]string(nil), detail.Labels...), CommentCount: detail.CommentCount, IssueComments: append([]githubinfra.CommentInfo(nil), detail.Comments...)})
}

func (r *Runner) enrichTargetFacts(ctx context.Context, repo string, target liveTarget) (liveTarget, error) {
	comments, _ := r.github.ListIssueComments(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: target.Number})
	for _, comment := range comments {
		body, _ := TruncateFactBody(comment.Body)
		target.RecentHumanComments = append(target.RecentHumanComments, FactComment{Author: comment.Author, Association: comment.AuthorAssociation, CreatedAt: comment.CreatedAt, Body: body, IsMaintainer: comment.AuthorAssociation == "OWNER" || comment.AuthorAssociation == "MEMBER"})
	}
	if len(target.RecentHumanComments) > 5 {
		target.RecentHumanComments = target.RecentHumanComments[len(target.RecentHumanComments)-5:]
	}
	timeline, _ := r.github.ListIssueTimeline(ctx, githubinfra.IssueTimelineInput{Repo: repo, IssueNumber: target.Number})
	for _, row := range timeline {
		payload := mustMarshalJSONValue(row)
		switch {
		case strings.Contains(payload, "cross-referenced") || strings.Contains(payload, "referenced"):
			target.Timeline.CrossReferences = append(target.Timeline.CrossReferences, row)
		case strings.Contains(payload, "marked-as-duplicate"):
			target.Timeline.Duplicates = append(target.Timeline.Duplicates, row)
		case strings.Contains(payload, "closed"):
			target.Timeline.Closures = append(target.Timeline.Closures, row)
		}
	}
	linkedPRs, _ := r.github.ListLinkedPullRequests(ctx, githubinfra.LinkedPullRequestsInput{Repo: repo, IssueNumber: target.Number})
	for _, pr := range linkedPRs {
		target.LinkedPRs = append(target.LinkedPRs, FactLinkedPR{Number: pr.Number, State: pr.State, Merged: pr.Merged, MergedAt: pr.MergedAt, MergeCommitSHA: pr.MergeCommitSHA})
	}
	if target.IsPR {
		reviewThreads, err := r.github.ListReviewThreads(ctx, githubinfra.ListReviewThreadsInput{Repo: repo, PRNumber: target.Number, Limit: int(^uint(0) >> 1)})
		if err != nil {
			return liveTarget{}, err
		}
		target.ReviewThreads = append([]githubinfra.ReviewThread(nil), reviewThreads...)
		reviewState, _ := r.github.ListPullRequestReviewState(ctx, githubinfra.PullRequestReviewStateInput{Repo: repo, PRNumber: target.Number})
		target.PRReviewState = &FactPRReviewState{RequestedReviewers: reviewState.RequestedReviewers, LatestReviewPerUser: reviewState.LatestReviewPerUser, LastReviewAt: reviewState.LastReviewAt}
	}
	return target, nil
}

func (r *Runner) getCaseDiscoveryState(ctx context.Context, projectID, repo, targetType string, targetNumber int64, legacy map[string]sweeperStateRecord) (caseDiscoveryState, error) {
	state := caseDiscoveryState{}
	if r.repos != nil && r.repos.SweeperCases != nil {
		caseRecord, err := r.repos.SweeperCases.GetByProjectRepoTarget(ctx, projectID, repo, targetType, targetNumber)
		if err != nil {
			return state, err
		}
		if caseRecord != nil {
			state.Case = caseRecord
			state.LastProposalID = derefString(caseRecord.LastProposalID)
			state.CloseDueAt = derefString(caseRecord.CloseDueAt)
			state.Phase = caseRecord.CurrentPhase
			state.Outcome = caseRecord.Status
			return state, nil
		}
	}
	key := buildTargetID(repo, targetNumber)
	legacyState, ok := legacy[key]
	if ok {
		state.Legacy = legacyState
		state.HasLegacy = true
		state.LastProposalID = legacyState.payload.ProposalID
		state.CloseDueAt = legacyState.payload.CloseBy
		state.Phase = legacyState.payload.Phase
		state.Outcome = legacyState.payload.Outcome
	}
	return state, nil
}

func (r *Runner) ensureCase(ctx context.Context, projectID string, target liveTarget, payload sweeperPayload, roleCfg config.SweeperRoleConfig) (*storage.SweeperCaseRecord, error) {
	_ = roleCfg
	if r.repos == nil || r.repos.SweeperCases == nil {
		return nil, nil
	}
	repo := strings.TrimSpace(firstNonEmpty(payload.Repo))
	targetType := targetTypeFromBool(target.IsPR)
	caseRecord, err := r.repos.SweeperCases.GetByProjectRepoTarget(ctx, projectID, repo, targetType, target.Number)
	if err != nil {
		return nil, err
	}
	if caseRecord != nil {
		return caseRecord, nil
	}
	id := payload.CaseID
	if strings.TrimSpace(id) == "" {
		id = eventlog.NewEventID("sweeper_case")
	}
	nowISO := r.nowISO()
	record := storage.SweeperCaseRecord{
		ID:           id,
		ProjectID:    projectID,
		Repo:         repo,
		TargetType:   targetType,
		TargetNumber: target.Number,
		Status:       "open",
		CurrentPhase: "prefilter",
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}
	if err := r.repos.SweeperCases.Upsert(ctx, record); err != nil {
		return nil, err
	}
	return &record, nil
}

func (r *Runner) persistProposal(ctx context.Context, projectID string, target liveTarget, payload sweeperPayload, caseRecord *storage.SweeperCaseRecord, roleCfg config.SweeperRoleConfig, decision string, category string, confidence int, rationale string) (*storage.SweeperProposalRecord, string, error) {
	if r.repos == nil || r.repos.SweeperProposals == nil || caseRecord == nil {
		return nil, "", nil
	}
	if existingID := strings.TrimSpace(payload.ProposalID); existingID != "" {
		existing, err := r.repos.SweeperProposals.GetByID(ctx, existingID)
		if err != nil {
			return nil, "", err
		}
		if existing != nil && existing.Decision == decision {
			return existing, existing.FingerprintJSON, nil
		}
		if existing != nil && existing.Decision != decision {
			payload.ProposalID = ""
		}
	}
	repo := strings.TrimSpace(firstNonEmpty(payload.Repo))
	factBundle := r.buildFactBundle(target, caseRecord, roleCfg)
	factBundleJSON, err := json.Marshal(factBundle)
	if err != nil {
		return nil, "", fmt.Errorf("marshal sweeper fact bundle: %w", err)
	}
	fingerprintJSON, err := BuildFingerprint(factBundle)
	if err != nil {
		return nil, "", fmt.Errorf("build sweeper fingerprint: %w", err)
	}
	markerUUID := payload.WarningMarkerUUID
	if strings.TrimSpace(markerUUID) == "" && decision == "warn" {
		markerUUID = NewMarkerUUID()
	}
	proposalBody, err := json.Marshal(map[string]any{
		"schemaVersion":   2,
		"decision":        decision,
		"category":        category,
		"confidenceScore": confidence,
		"rationale":       rationale,
		"markerUUID":      markerUUID,
		"fingerprint":     fingerprintJSON,
	})
	if err != nil {
		return nil, "", fmt.Errorf("marshal sweeper proposal: %w", err)
	}
	proposalID := payload.ProposalID
	if strings.TrimSpace(proposalID) == "" {
		proposalID = eventlog.NewEventID("sweeper_proposal")
	}
	summary := payload.Summary
	validationStatus := "passed"
	record := storage.SweeperProposalRecord{
		ID:               proposalID,
		CaseID:           caseRecord.ID,
		ProjectID:        projectID,
		Repo:             repo,
		TargetType:       targetTypeFromBool(target.IsPR),
		TargetNumber:     target.Number,
		SchemaVersion:    2,
		ProposerKind:     "heuristic_v1",
		FactBundleJSON:   string(factBundleJSON),
		FingerprintJSON:  fingerprintJSON,
		ProposalJSON:     string(proposalBody),
		Decision:         decision,
		Category:         category,
		ConfidenceScore:  int64(confidence),
		Summary:          optionalString(summary),
		Rationale:        optionalString(rationale),
		MarkerUUID:       optionalString(markerUUID),
		ValidationStatus: &validationStatus,
		CreatedAt:        r.nowISO(),
	}
	if err := r.repos.SweeperProposals.Insert(ctx, record); err != nil {
		return nil, "", err
	}
	if err := r.writeDurableReport(projectID, target, payload, caseRecord, &record, roleCfg); err != nil {
		return nil, "", err
	}
	return &record, fingerprintJSON, nil
}

func (r *Runner) updateProposalApplyReceipt(ctx context.Context, proposalID string, applyStatus string, applySummary string, applyError *string, terminal bool) error {
	if r.repos == nil || r.repos.SweeperProposals == nil || strings.TrimSpace(proposalID) == "" {
		return nil
	}
	var appliedAt *string
	if terminal {
		appliedAt = stringPtr(r.nowISO())
	}
	return r.repos.SweeperProposals.UpdateApplyReceipt(ctx, proposalID, applyStatus, optionalString(applySummary), applyError, appliedAt)
}

func (r *Runner) syncCase(ctx context.Context, projectID string, target liveTarget, payload sweeperPayload, proposal *storage.SweeperProposalRecord, fingerprintJSON string, roleCfg config.SweeperRoleConfig) error {
	if r.repos == nil || r.repos.SweeperCases == nil {
		return nil
	}
	caseRecord, err := r.ensureCase(ctx, projectID, target, payload, roleCfg)
	if err != nil || caseRecord == nil {
		return err
	}
	record := *caseRecord
	record.Status = caseStatusFromOutcome(payload.Outcome)
	record.CurrentPhase = casePhaseFromPayload(payload)
	record.CurrentCategory = optionalString(payload.Category)
	record.CurrentConfidenceScore = optionalInt64(int64(payload.Confidence))
	if payload.WarningCommentID > 0 {
		record.WarningCommentID = optionalInt64(payload.WarningCommentID)
	}
	record.WarningMarkerUUID = optionalString(payload.WarningMarkerUUID)
	if proposal != nil {
		record.LastProposalID = &proposal.ID
	}
	record.LastFingerprintJSON = optionalString(fingerprintJSON)
	lastHumanCommentAt, _ := DeriveHumanCommentStats(target.IssueComments, target.ReviewThreads, roleCfg.Triggers.ExcludeAuthors, "")
	record.LastHumanActivityAt = optionalString(lastHumanCommentAt)
	record.WarnedAt = optionalString(payload.WarningPostedAt)
	record.CloseDueAt = optionalString(payload.CloseBy)
	if terminalOutcome := terminalOutcomeFromPayload(payload); terminalOutcome != "" {
		record.TerminalOutcome = &terminalOutcome
		record.TerminalAt = stringPtr(r.nowISO())
	}
	record.UpdatedAt = r.nowISO()
	if err := r.repos.SweeperCases.Upsert(ctx, record); err != nil {
		return err
	}
	reportProposal := proposal
	if r.repos != nil && r.repos.SweeperProposals != nil && strings.TrimSpace(payload.ProposalID) != "" {
		latestProposal, err := r.repos.SweeperProposals.GetByID(ctx, strings.TrimSpace(payload.ProposalID))
		if err != nil {
			return err
		}
		if latestProposal != nil {
			reportProposal = latestProposal
		}
	}
	return r.writeDurableReport(projectID, target, payload, &record, reportProposal, roleCfg)
}

func (r *Runner) loadProposalForApply(ctx context.Context, payload sweeperPayload, caseRecord *storage.SweeperCaseRecord) (*storage.SweeperProposalRecord, error) {
	if r.repos == nil || r.repos.SweeperProposals == nil {
		return nil, nil
	}
	proposalID := strings.TrimSpace(payload.ProposalID)
	if proposalID != "" {
		return r.repos.SweeperProposals.GetByID(ctx, proposalID)
	}
	if caseRecord != nil {
		if caseRecord.LastProposalID != nil && strings.TrimSpace(*caseRecord.LastProposalID) != "" {
			return r.repos.SweeperProposals.GetByID(ctx, strings.TrimSpace(*caseRecord.LastProposalID))
		}
		return nil, nil
	}
	return nil, nil
}

func validAgentApplyProposal(proposal *storage.SweeperProposalRecord, expectedDecision string, schemaVersion int) bool {
	if proposal == nil || proposal.ProposerKind != proposerKindAgentV1 || proposal.Decision != expectedDecision {
		return false
	}
	if proposal.SchemaVersion != int64(schemaVersion) {
		return false
	}
	return proposal.ValidationStatus != nil && strings.TrimSpace(*proposal.ValidationStatus) == "passed"
}

func (r *Runner) staleProposalStatusForApply(target liveTarget, caseRecord *storage.SweeperCaseRecord, roleCfg config.SweeperRoleConfig, proposal *storage.SweeperProposalRecord) (bool, *storage.SweeperProposalRecord, string, error) {
	if proposal == nil {
		return false, nil, "", nil
	}
	bundle := r.buildFactBundle(target, caseRecord, roleCfg)
	fingerprintJSON, err := BuildFingerprint(bundle)
	if err != nil {
		return false, proposal, "", err
	}
	if strings.TrimSpace(proposal.FingerprintJSON) != strings.TrimSpace(fingerprintJSON) {
		return true, proposal, fingerprintJSON, nil
	}
	return false, proposal, fingerprintJSON, nil
}

func markerComment(issueComments []githubinfra.CommentInfo, markerUUID string) *githubinfra.CommentInfo {
	markerUUID = strings.TrimSpace(markerUUID)
	if markerUUID == "" {
		return nil
	}
	needle := "looper:sweeper:warn id=" + markerUUID
	for i := range issueComments {
		if strings.Contains(issueComments[i].Body, needle) {
			return &issueComments[i]
		}
	}
	return nil
}

func warningCommentCloseBy(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	const prefix = "This will be eligible for closure after "
	const suffix = " unless someone comments or removes `"
	idx := strings.Index(body, prefix)
	if idx < 0 {
		return ""
	}
	segment := body[idx+len(prefix):]
	end := strings.Index(segment, suffix)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(segment[:end])
}

func (r *Runner) buildFactBundle(target liveTarget, caseRecord *storage.SweeperCaseRecord, roleCfg config.SweeperRoleConfig) FactBundle {
	body, truncated := TruncateFactBody(target.Body)
	lastHumanCommentAt, humanCommentCount := DeriveHumanCommentStats(target.IssueComments, target.ReviewThreads, roleCfg.Triggers.ExcludeAuthors, "")
	bundle := FactBundle{
		Repo:                       caseRecord.Repo,
		TargetType:                 targetTypeFromBool(target.IsPR),
		Number:                     target.Number,
		State:                      target.State,
		IsDraft:                    target.Draft,
		HeadSHA:                    target.HeadSHA,
		CreatedAt:                  target.CreatedAt,
		UpdatedAt:                  target.UpdatedAt,
		ClosedAt:                   target.ClosedAt,
		Title:                      target.Title,
		Body:                       body,
		BodyTruncated:              truncated,
		Author:                     target.Author,
		AuthorAssociation:          target.AuthorAssociation,
		Labels:                     append([]string(nil), target.Labels...),
		PolicyLabelsPresent:        PolicyLabelsPresent(target.Labels, roleCfg),
		CommentCount:               target.CommentCount,
		PolicySnapshot:             roleCfg,
		LastHumanCommentAt:         lastHumanCommentAt,
		HumanCommentCountSinceOpen: humanCommentCount,
		RecentHumanComments:        append([]FactComment(nil), target.RecentHumanComments...),
		WarningComment:             target.WarningComment,
		Timeline:                   target.Timeline,
		LinkedPRs:                  append([]FactLinkedPR(nil), target.LinkedPRs...),
		PRReviewState:              target.PRReviewState,
	}
	if caseRecord != nil {
		bundle.Case = FactBundleCase{
			CurrentPhase:        caseRecord.CurrentPhase,
			WarnedAt:            derefString(caseRecord.WarnedAt),
			CloseDueAt:          derefString(caseRecord.CloseDueAt),
			WarningMarkerUUID:   derefString(caseRecord.WarningMarkerUUID),
			LastHumanActivityAt: derefString(caseRecord.LastHumanActivityAt),
		}
	}
	return bundle
}

func (r *Runner) agentApplyEnabled(roleCfg config.SweeperRoleConfig) bool {
	return roleCfg.Proposer.Mode == config.SweeperProposerModeAgentApply
}

func agentEligibleCategory(category string) bool {
	switch category {
	case categoryStale, categoryAbandonedPR, categoryAlreadyFixed, categorySuperseded:
		return true
	default:
		return false
	}
}

func forcedDryRunCategory(roleCfg config.SweeperRoleConfig, category string) bool {
	switch category {
	case categoryUnrelated:
		return true
	case categoryRouteSecurity:
		return true
	default:
		return false
	}
}

func (r *Runner) proposeAgentDecision(ctx context.Context, project *storage.ProjectRecord, queueItem storage.QueueItemRecord, target liveTarget, caseRecord *storage.SweeperCaseRecord, payload sweeperPayload, roleCfg config.SweeperRoleConfig, phase, heuristicCategory, heuristicRationale string) (*storage.SweeperProposalRecord, string, error) {
	if r.agent == nil {
		return nil, "", fmt.Errorf("sweeper agent proposer is not configured")
	}
	prompt, err := buildProposalPrompt(r.buildFactBundle(target, caseRecord, roleCfg), phase, heuristicCategory, heuristicRationale, roleCfg, r.agentRuntime, modelOrOverride(roleCfg.Proposer.Model, r.agentModel))
	if err != nil {
		return nil, "", err
	}
	executionID := eventlog.NewEventID("agent_execution")
	execution, err := r.agent.Start(ctx, AgentRunInput{
		ExecutionID:      executionID,
		ProjectID:        derefString(queueItem.ProjectID),
		LoopID:           derefString(queueItem.LoopID),
		RunID:            executionID,
		Prompt:           prompt,
		WorkingDirectory: project.RepoPath,
		Timeout:          time.Duration(roleCfg.Proposer.TimeoutSeconds) * time.Second,
		HeartbeatTimeout: time.Duration(roleCfg.Proposer.TimeoutSeconds) * time.Second,
		Metadata: map[string]any{
			"role":              "sweeper",
			"sweeperPhase":      phase,
			"heuristicCategory": heuristicCategory,
			"repo":              payload.Repo,
			"targetType":        payload.TargetType,
			"targetNumber":      payload.TargetNumber,
		},
		IdempotencyKey: fmt.Sprintf("sweeper:%s:%s:%s", phase, payload.Repo, buildTargetID(payload.Repo, payload.TargetNumber)),
	})
	if err != nil {
		return nil, "", fmt.Errorf("start sweeper proposer agent: %w", err)
	}
	result, err := execution.Wait(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("wait for sweeper proposer agent: %w", err)
	}
	if r.logger != nil {
		r.logger.Debug("sweeper proposer reviewed candidate", map[string]any{"repo": payload.Repo, "phase": phase, "targetType": payload.TargetType, "targetNumber": payload.TargetNumber, "filteredOut": 0, "agentReviewed": 1})
	}
	if result.Status != "completed" {
		statusErr := fmt.Errorf("sweeper proposer execution status %q", result.Status)
		_ = r.persistInvalidAgentProposal(ctx, derefString(queueItem.ProjectID), target, caseRecord, roleCfg, executionID, prompt, result, statusErr)
		return nil, "", statusErr
	}
	proposal, parseErr := parseNormalizedProposal(firstNonEmpty(result.Stdout, result.Summary))
	if parseErr != nil {
		_ = r.persistInvalidAgentProposal(ctx, derefString(queueItem.ProjectID), target, caseRecord, roleCfg, executionID, prompt, result, parseErr)
		return nil, "", parseErr
	}
	if validateErr := validateNormalizedProposal(proposal, phase); validateErr != nil {
		_ = r.persistInvalidAgentProposal(ctx, derefString(queueItem.ProjectID), target, caseRecord, roleCfg, executionID, prompt, result, validateErr)
		return nil, "", validateErr
	}
	return r.persistAgentProposal(ctx, derefString(queueItem.ProjectID), target, payload, caseRecord, roleCfg, phase, heuristicCategory, heuristicRationale, executionID, prompt, result, proposal)
}

func modelOrOverride(primary, fallback *string) *string {
	if primary != nil && strings.TrimSpace(*primary) != "" {
		return primary
	}
	return fallback
}

func categoryDecisionForPhase(phase, category string) string {
	if category == categoryNone {
		return "no_action"
	}
	if category == categoryRouteSecurity {
		return "quarantine"
	}
	if phase == "close" {
		return "close"
	}
	return "warn"
}

func casePhaseFromPayload(payload sweeperPayload) string {
	if payload.Outcome == outcomeClosed || payload.Outcome == outcomeCancelled || payload.Outcome == outcomeCancelledByLabelRemoval || payload.Outcome == outcomeAlreadyClosedByHuman || payload.Outcome == outcomeQuarantined {
		return "terminal"
	}
	if strings.TrimSpace(payload.Phase) != "" {
		return payload.Phase
	}
	return "prefilter"
}

func caseStatusFromOutcome(outcome string) string {
	switch outcome {
	case outcomePending:
		return "pending"
	case outcomeClosed, outcomeCancelled, outcomeCancelledByLabelRemoval, outcomeAlreadyClosedByHuman:
		return "terminal"
	case outcomeQuarantined:
		return "quarantined"
	default:
		return "open"
	}
}

func terminalOutcomeFromPayload(payload sweeperPayload) string {
	switch payload.Outcome {
	case outcomeClosed, outcomeCancelled, outcomeCancelledByLabelRemoval, outcomeAlreadyClosedByHuman, outcomeQuarantined:
		return payload.Outcome
	default:
		return ""
	}
}

func targetTypeFromBool(isPR bool) string {
	if isPR {
		return "pull_request"
	}
	return "issue"
}

func derefStringFromCase(record *storage.SweeperCaseRecord) string {
	if record == nil {
		return ""
	}
	return record.ID
}

func optionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func optionalInt64(value int64) *int64 {
	if value == 0 {
		return nil
	}
	return &value
}

func mustMarshalJSONValue(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func classifyTarget(target liveTarget, roleCfg config.SweeperRoleConfig, now time.Time) (string, int, string) {
	text := strings.ToLower(target.Title + "\n" + target.Body)
	if hasSecuritySensitiveSignal(text) {
		return categoryRouteSecurity, 100, "security-sensitive content detected"
	}
	if roleCfg.Categories.Superseded.Enabled && (hasSupersededEvidence(target) || strings.Contains(text, "duplicate of #") || strings.Contains(text, "superseded by #")) {
		return categorySuperseded, maxConfidence(roleCfg.Categories.Superseded.MinConfidence), "target appears superseded by another issue or pull request"
	}
	if roleCfg.Categories.AlreadyFixed.Enabled && (hasAlreadyFixedEvidence(target) || strings.Contains(text, "fixed by #") || strings.Contains(text, "already fixed")) {
		return categoryAlreadyFixed, maxConfidence(roleCfg.Categories.AlreadyFixed.MinConfidence), "target appears already fixed"
	}
	if !target.IsPR && roleCfg.Categories.Unrelated.Enabled && (strings.Contains(text, "support") || strings.Contains(text, "question")) {
		return categoryUnrelated, maxConfidence(roleCfg.Categories.Unrelated.MinConfidence), "target appears unrelated to repository work"
	}
	if target.IsPR && roleCfg.Categories.AbandonedPR.Enabled && inactiveLongEnough(target.UpdatedAt, roleCfg.Categories.AbandonedPR.InactivityDays, now) {
		return categoryAbandonedPR, maxConfidence(roleCfg.Categories.AbandonedPR.MinConfidence), "open pull request matched sweeper abandoned-pr heuristics"
	}
	if roleCfg.Categories.Stale.Enabled && inactiveLongEnough(target.UpdatedAt, roleCfg.Categories.Stale.InactivityDays, now) {
		return categoryStale, maxConfidence(roleCfg.Categories.Stale.MinConfidence), "open item matched stale sweeper heuristics"
	}
	return categoryNone, 0, "no enabled sweeper category matched"
}

func classifyQueueFailure(err error) (kind, message string) {
	message = strings.TrimSpace(err.Error())
	if message == "" {
		message = "sweeper processing failed"
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "unsupported sweeper queue item type"),
		strings.Contains(lower, "queue item id is required"),
		strings.Contains(lower, "queue repository is not configured"),
		strings.Contains(lower, "project repository is not configured"),
		strings.Contains(lower, "project not found"),
		strings.Contains(lower, "invalid sweeper target id"),
		strings.Contains(lower, "agent proposer is not configured"),
		strings.Contains(lower, "marshal sweeper"):
		return "non_retryable", message
	default:
		return "retryable_transient", message
	}
}

func hasSecuritySensitiveSignal(text string) bool {
	for _, signal := range []string{
		"security vulnerability",
		"cve-",
		"private disclosure",
		"responsible disclosure",
		"security incident",
		"auth bypass",
		"authentication bypass",
		"authorization bypass",
		"remote code execution",
		"sql injection",
		"command injection",
		"code injection",
		"cross-site scripting",
		"cross site scripting",
		"cross-site request forgery",
		"cross site request forgery",
		"server-side request forgery",
		"server side request forgery",
		"directory traversal",
		"path traversal",
		"privilege escalation",
		"token leak",
		"token leaked",
		"credential leak",
		"credentials leaked",
		"secret leak",
		"secrets leaked",
	} {
		if strings.Contains(text, signal) {
			return true
		}
	}
	return false
}

func hasAlreadyFixedEvidence(target liveTarget) bool {
	for _, pr := range target.LinkedPRs {
		if pr.Merged {
			return true
		}
	}
	for _, closure := range target.Timeline.Closures {
		if strings.Contains(strings.ToLower(mustMarshalJSONValue(closure)), "pull") {
			return true
		}
	}
	return false
}

func hasSupersededEvidence(target liveTarget) bool {
	if len(target.Timeline.Duplicates) > 0 {
		return true
	}
	return strings.Contains(strings.ToLower(mustMarshalJSONValue(target.Timeline.CrossReferences)), "supersed")
}

func buildWarningComment(target liveTarget, payload sweeperPayload, graceDays int) string {
	return fmt.Sprintf("Looper sweeper flagged this %s as **%s**.\n\nReason: %s\n\nThis will be eligible for closure after %s unless someone comments or removes `%s`.\n<!-- looper:sweeper:warn id=%s -->", targetKind(target.IsPR), payload.Category, payload.Rationale, strings.TrimSpace(payload.CloseBy), payload.PendingLabel, payload.WarningMarkerUUID)
}

func buildCloseComment(target liveTarget, payload sweeperPayload, rationale string) string {
	return fmt.Sprintf("Closing this %s as **%s**.\n\n%s\n\nOriginal notice comment id: %d\n<!-- looper:sweeper:close id=%s -->", targetKind(target.IsPR), payload.Category, rationale, payload.WarningCommentID, eventlog.NewEventID("sweeper"))
}

func gracePeriodForCategory(category string, roleCfg config.SweeperRoleConfig) int {
	switch category {
	case categoryAlreadyFixed:
		return positiveOr(roleCfg.Categories.AlreadyFixed.GracePeriodDays, 7)
	case categoryUnrelated:
		return positiveOr(roleCfg.Categories.Unrelated.GracePeriodDays, 7)
	case categorySuperseded:
		return positiveOr(roleCfg.Categories.Superseded.GracePeriodDays, 7)
	case categoryAbandonedPR:
		return positiveOr(roleCfg.Categories.AbandonedPR.GracePeriodDays, 7)
	default:
		return positiveOr(roleCfg.Categories.Stale.GracePeriodDays, 7)
	}
}

func (r *Runner) shouldSkipSummary(labels []string, author string, state sweeperStateRecord, roleCfg config.SweeperRoleConfig) bool {
	if hasAnyLabel(labels, roleCfg.Triggers.ExcludeLabels) || hasAnyLabelExcept(labels, roleCfg.Triggers.LooperInternalLabels, roleCfg.Lifecycle.ClosedLabel) || hasLabel(labels, roleCfg.Security.QuarantineLabel) {
		return true
	}
	if hasLabel(labels, roleCfg.Lifecycle.ClosedLabel) && r.reopenCooldownActive(state, roleCfg) {
		return true
	}
	for _, excluded := range roleCfg.Triggers.ExcludeAuthors {
		if strings.EqualFold(strings.TrimSpace(excluded), strings.TrimSpace(author)) {
			return true
		}
	}
	return false
}

func authorAssociationExcluded(authorAssociation string, roleCfg config.SweeperRoleConfig) bool {
	for _, excluded := range roleCfg.Triggers.ExcludeAuthorAssociations {
		if strings.EqualFold(strings.TrimSpace(excluded), strings.TrimSpace(authorAssociation)) {
			return true
		}
	}
	return false
}

func (r *Runner) prefilterSkipCandidate(candidate prefilterCandidate, legacyState sweeperStateRecord, roleCfg config.SweeperRoleConfig) bool {
	return r.shouldSkipSummary(candidate.Labels, candidate.Author, legacyState, roleCfg) || authorAssociationExcluded(candidate.Association, roleCfg)
}

func (r *Runner) summaryAuthorAssociation(ctx context.Context, repo string, cwd string, number int64, current string, roleCfg config.SweeperRoleConfig) (string, bool) {
	if strings.TrimSpace(current) != "" || len(roleCfg.Triggers.ExcludeAuthorAssociations) == 0 || r.github == nil {
		return current, true
	}
	detail, err := r.github.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: strings.TrimSpace(repo), IssueNumber: number, CWD: cwd})
	if err != nil {
		return "", false
	}
	association := strings.TrimSpace(detail.AuthorAssociation)
	if association == "" {
		return current, true
	}
	return association, true
}

func (r *Runner) reopenCooldownActive(state sweeperStateRecord, roleCfg config.SweeperRoleConfig) bool {
	if state.payload.Outcome != outcomeClosed && state.item.Type != QueueTypeClose {
		return true
	}
	closedAt, ok := parseGitHubTimestamp(firstNonEmpty(state.item.UpdatedAt, state.item.CreatedAt))
	if !ok {
		return true
	}
	threshold := closedAt.Add(time.Duration(roleCfg.Triggers.ReopenCooldownDays) * 24 * time.Hour)
	return r.now().UTC().Before(threshold)
}

func (r *Runner) discoveryBudgets(ctx context.Context, input DiscoveryInput, roleCfg config.SweeperRoleConfig) (int, int, error) {
	warnLimit := r.discoveryLimit(input.Limit, roleCfg.Triggers.MaxPerTick)
	closeLimit := warnLimit * 3
	remainingWarn, remainingClose, err := r.remainingDailyDiscoveryBudget(ctx, input.ProjectID, input.Repo, roleCfg)
	if err != nil {
		return 0, 0, err
	}
	if warnLimit > remainingWarn {
		warnLimit = remainingWarn
	}
	if closeLimit > remainingClose {
		closeLimit = remainingClose
	}
	if warnLimit < 0 {
		warnLimit = 0
	}
	if closeLimit < 0 {
		closeLimit = 0
	}
	return warnLimit, closeLimit, nil
}

func (r *Runner) remainingDailyDiscoveryBudget(ctx context.Context, projectID, repo string, roleCfg config.SweeperRoleConfig) (int, int, error) {
	if r.repos == nil || r.repos.SweeperProposals == nil {
		return 0, 0, fmt.Errorf("sweeper proposal repository is not configured")
	}
	dayStart := r.now().UTC().Format("2006-01-02T00:00:00.000Z")
	warnApplied, err := r.repos.SweeperProposals.CountAppliedByRepoAndDecisionSince(ctx, projectID, repo, "warn", dayStart)
	if err != nil {
		return 0, 0, err
	}
	warnInflight, err := r.repos.SweeperProposals.CountInflightByRepoAndDecision(ctx, projectID, repo, "warn")
	if err != nil {
		return 0, 0, err
	}
	closeApplied, err := r.repos.SweeperProposals.CountAppliedByRepoAndDecisionSince(ctx, projectID, repo, "close", dayStart)
	if err != nil {
		return 0, 0, err
	}
	closeInflight, err := r.repos.SweeperProposals.CountInflightByRepoAndDecision(ctx, projectID, repo, "close")
	if err != nil {
		return 0, 0, err
	}
	return remainingCount(roleCfg.Limits.MaxWarningsPerRepoPerDay, int(warnApplied+warnInflight)), remainingCount(roleCfg.Limits.MaxClosesPerRepoPerDay, int(closeApplied+closeInflight)), nil
}

func remainingCount(limit, used int) int {
	remaining := limit - used
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (r *Runner) latestSweeperRecords(ctx context.Context) (map[string]sweeperStateRecord, error) {
	items, err := r.repos.Queue.List(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]sweeperStateRecord{}
	for _, item := range items {
		if !strings.HasPrefix(item.Type, "sweeper") {
			continue
		}
		payload := r.readPayload(item)
		if payload.TargetNumber <= 0 && item.TargetID == "" {
			continue
		}
		key := item.TargetID
		if key == "" && item.Repo != nil {
			key = buildTargetID(*item.Repo, payload.TargetNumber)
		}
		if key == "" {
			continue
		}
		if _, exists := out[key]; exists {
			continue
		}
		out[key] = sweeperStateRecord{item: item, payload: payload}
	}
	return out, nil
}

func (r *Runner) latestSweeperState(ctx context.Context) (map[string]sweeperPayload, error) {
	records, err := r.latestSweeperRecords(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]sweeperPayload, len(records))
	for key, record := range records {
		out[key] = record.payload
	}
	return out, nil
}

func (r *Runner) readPayload(item storage.QueueItemRecord) sweeperPayload {
	if item.PayloadJSON == nil || strings.TrimSpace(*item.PayloadJSON) == "" {
		return sweeperPayload{}
	}
	var envelope payloadEnvelope
	if err := json.Unmarshal([]byte(*item.PayloadJSON), &envelope); err != nil {
		return sweeperPayload{}
	}
	return envelope.Sweeper
}

func mustMarshalPayload(payload sweeperPayload) string {
	encoded, _ := json.Marshal(struct {
		Sweeper persistedSweeperPayload `json:"sweeper"`
	}{Sweeper: persistedSweeperPayload{CaseID: payload.CaseID, ProposalID: payload.ProposalID}})
	return string(encoded)
}

func hydratePayloadFromProposal(payload sweeperPayload, proposal *storage.SweeperProposalRecord) sweeperPayload {
	if proposal == nil {
		return payload
	}
	if strings.TrimSpace(payload.Category) == "" {
		payload.Category = proposal.Category
	}
	if payload.Confidence == 0 {
		payload.Confidence = int(proposal.ConfidenceScore)
	}
	if strings.TrimSpace(payload.Rationale) == "" {
		payload.Rationale = derefString(proposal.Rationale)
	}
	if strings.TrimSpace(payload.WarningMarkerUUID) == "" {
		payload.WarningMarkerUUID = derefString(proposal.MarkerUUID)
	}
	if strings.TrimSpace(payload.Summary) == "" {
		payload.Summary = derefString(proposal.Summary)
	}
	return payload
}

func (r *Runner) projectConfig(ctx context.Context, projectID string) (*storage.ProjectRecord, config.SweeperRoleConfig, error) {
	if r.repos == nil || r.repos.Projects == nil {
		return nil, config.SweeperRoleConfig{}, fmt.Errorf("sweeper project repository is not configured")
	}
	project, err := r.repos.Projects.GetByID(ctx, projectID)
	if err != nil {
		return nil, config.SweeperRoleConfig{}, err
	}
	if project == nil {
		return nil, config.SweeperRoleConfig{}, fmt.Errorf("project not found: %s", projectID)
	}
	roleCfg := r.roleConfig(projectID)
	if meta := parseProjectSweeperMetadata(project.MetadataJSON); meta.AutoDryRun {
		roleCfg.DryRun = true
	}
	return project, roleCfg, nil
}

func (r *Runner) roleConfig(projectID string) config.SweeperRoleConfig {
	if r.config == nil {
		return config.SweeperRoleConfig{AutoDiscovery: true}
	}
	return config.ProjectRoleConfigs(*r.config, projectID).Sweeper
}

func (r *Runner) discoveryLimit(requested, fallback int) int {
	if requested > 0 {
		return requested
	}
	if fallback > 0 {
		return fallback
	}
	return 10
}

func (r *Runner) autoDiscoveryEnabled(projectID string) bool {
	if r.config == nil {
		return true
	}
	return config.ProjectRoleConfigs(*r.config, projectID).Sweeper.AutoDiscovery
}

func (r *Runner) nowISO() string {
	return r.now().UTC().Format(javaScriptISOStringUTC)
}

func isSupportedQueueType(queueType string) bool {
	switch queueType {
	case QueueType, QueueTypeWarn, QueueTypeClose, QueueTypeReconcile:
		return true
	default:
		return false
	}
}

func backoffDelay(base time.Duration, attempts int64) time.Duration {
	if attempts <= 0 {
		attempts = 1
	}
	return time.Duration(attempts) * base
}

type projectSweeperMetadata struct {
	AutoDryRun       bool
	AutoDryRunReason string
	AutoDryRunSetAt  string
}

func parseProjectSweeperMetadata(metadataJSON *string) projectSweeperMetadata {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return projectSweeperMetadata{}
	}
	var decoded struct {
		Sweeper struct {
			AutoDryRun       bool   `json:"autoDryRun"`
			AutoDryRunReason string `json:"autoDryRunReason,omitempty"`
			AutoDryRunSetAt  string `json:"autoDryRunSetAt,omitempty"`
		} `json:"sweeper"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(*metadataJSON)), &decoded); err != nil {
		return projectSweeperMetadata{}
	}
	return projectSweeperMetadata{AutoDryRun: decoded.Sweeper.AutoDryRun, AutoDryRunReason: strings.TrimSpace(decoded.Sweeper.AutoDryRunReason), AutoDryRunSetAt: strings.TrimSpace(decoded.Sweeper.AutoDryRunSetAt)}
}

func diagnosticModeEnabled(roleCfg config.SweeperRoleConfig) bool {
	return roleCfg.Proposer.DiagnosticMode
}

func buildTargetID(repo string, number int64) string {
	return fmt.Sprintf("%s#%d", repo, number)
}

func parseTargetNumber(item storage.QueueItemRecord) (int64, error) {
	if item.PRNumber != nil && *item.PRNumber > 0 {
		return *item.PRNumber, nil
	}
	parts := strings.Split(item.TargetID, "#")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid sweeper target id %q", item.TargetID)
	}
	number, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid sweeper target id %q: %w", item.TargetID, err)
	}
	return number, nil
}

func (r *Runner) removePendingLabel(ctx context.Context, roleCfg config.SweeperRoleConfig, repo string, issueNumber int64) error {
	if roleCfg.DryRun || r.github == nil {
		return nil
	}
	return r.github.RemoveIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: repo, IssueNumber: issueNumber, Labels: []string{roleCfg.Lifecycle.PendingLabel}})
}

func dueForClose(closeBy string, now time.Time) bool {
	if strings.TrimSpace(closeBy) == "" {
		return false
	}
	parsed, ok := parseGitHubTimestamp(closeBy)
	if !ok {
		return false
	}
	return !parsed.After(now.UTC())
}

func inactiveLongEnough(updatedAt string, inactivityDays int, now time.Time) bool {
	if inactivityDays <= 0 {
		return false
	}
	parsed, ok := parseGitHubTimestamp(updatedAt)
	if !ok {
		return false
	}
	threshold := now.UTC().Add(-time.Duration(inactivityDays) * 24 * time.Hour)
	return !parsed.After(threshold)
}

func parseGitHubTimestamp(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		parsed, err = time.Parse(javaScriptISOStringUTC, value)
		if err != nil {
			return time.Time{}, false
		}
	}
	return parsed.UTC(), true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func hasAnyLabelExcept(labels []string, candidates []string, except string) bool {
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(except)) {
			continue
		}
		if hasLabel(labels, candidate) {
			return true
		}
	}
	return false
}

func hasLabel(labels []string, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	if strings.HasSuffix(want, "*") {
		prefix := strings.TrimSuffix(want, "*")
		if prefix == "" {
			return false
		}
		for _, label := range labels {
			if strings.HasPrefix(label, prefix) {
				return true
			}
		}
		return false
	}
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func hasAnyLabel(labels []string, want []string) bool {
	for _, label := range want {
		if hasLabel(labels, label) {
			return true
		}
	}
	return false
}

func targetKind(isPR bool) string {
	if isPR {
		return "pull request"
	}
	return "issue"
}

func maxConfidence(min int) int {
	if min < 100 {
		return min + (100-min)/2
	}
	return 100
}

func positiveOr(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func stringPtr(value string) *string {
	return &value
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
