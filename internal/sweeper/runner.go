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
	CreateIssueComment(context.Context, githubinfra.IssueCommentInput) (githubinfra.IssueCommentResult, error)
	UpdateIssueComment(context.Context, githubinfra.UpdateIssueCommentInput) error
	CloseIssue(context.Context, githubinfra.CloseIssueInput) error
	ClosePullRequest(context.Context, githubinfra.ClosePullRequestInput) error
	AddIssueLabels(context.Context, githubinfra.IssueLabelsInput) error
	RemoveIssueLabels(context.Context, githubinfra.IssueLabelsInput) error
}

type Options struct {
	Repos  *storage.Repositories
	GitHub GitHubGateway
	Logger bootstrap.Logger
	Now    func() time.Time
	Config *config.Config
}

type Runner struct {
	repos   *storage.Repositories
	github  GitHubGateway
	logger  bootstrap.Logger
	now     func() time.Time
	config  *config.Config
	claimer string
	maxTry  int64
}

type payloadEnvelope struct {
	Sweeper sweeperPayload `json:"sweeper"`
}

type sweeperPayload struct {
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
	Number    int64
	State     string
	Title     string
	Body      string
	UpdatedAt string
	Author    string
	Labels    []string
	IsPR      bool
	Draft     bool
}

func New(options Options) *Runner {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Runner{repos: options.Repos, github: options.GitHub, logger: options.Logger, now: now, config: options.Config, claimer: defaultClaimedBy, maxTry: defaultRetryMax}
}

func (r *Runner) DiscoverIssues(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	return r.discoverIssuesAndClosures(ctx, input)
}

func (r *Runner) DiscoverPullRequests(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	return r.discoverPullRequestsAndClosures(ctx, input)
}

func (r *Runner) DiscoverReconcile(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	if r.repos == nil || r.repos.Projects == nil || r.repos.Queue == nil {
		return DiscoveryResult{}, fmt.Errorf("sweeper repositories are not configured")
	}
	project, roleCfg, err := r.projectConfig(ctx, input.ProjectID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	if project.Archived || !roleCfg.AutoDiscovery {
		return DiscoveryResult{Skipped: 1}, nil
	}
	states, err := r.latestSweeperRecords(ctx)
	if err != nil {
		return DiscoveryResult{}, err
	}
	limit := r.discoveryLimit(input.Limit, roleCfg.Triggers.MaxPerTick)
	items := make([]storage.QueueItemRecord, 0, limit)
	for targetID, state := range states {
		if len(items) >= limit {
			break
		}
		if state.payload.Repo != input.Repo || state.payload.Outcome != outcomePending || state.payload.Phase != "warn" {
			continue
		}
		queueItem, ok, err := r.buildQueueItem(ctx, queueSeed{ProjectID: input.ProjectID, Repo: input.Repo, QueueType: QueueTypeReconcile, TargetType: state.payload.TargetType, TargetID: targetID, Number: state.payload.TargetNumber, Payload: state.payload})
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
	if err != nil {
		return nil, err
	}
	queueItem.PayloadJSON = stringPtr(mustMarshalPayload(payload))
	queueItem.UpdatedAt = r.nowISO()
	if err := r.repos.Queue.Upsert(ctx, queueItem); err != nil {
		return nil, err
	}
	if err := r.repos.Queue.Complete(ctx, queueItem.ID, r.nowISO()); err != nil {
		return nil, err
	}
	return &ProcessResult{QueueItemID: queueItem.ID, Status: status, Summary: summary}, nil
}

func (r *Runner) discoverIssuesAndClosures(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
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
		if r.shouldSkipSummary(issue.Labels, issue.Author, issue.AuthorAssociation, states[targetID], roleCfg) {
			result.Skipped++
			continue
		}
		if hasLabel(issue.Labels, roleCfg.Lifecycle.PendingLabel) {
			state, ok := states[targetID]
			if !ok || state.payload.Outcome != outcomePending || state.payload.Phase != "warn" || !dueForClose(state.payload.CloseBy, r.now()) || closeCount >= closeLimit {
				continue
			}
			queueItem, ok, err := r.buildQueueItem(ctx, queueSeed{ProjectID: input.ProjectID, Repo: input.Repo, QueueType: QueueTypeClose, TargetType: "issue", TargetID: targetID, Number: issue.Number, Payload: state.payload})
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
		queueItem, ok, err := r.buildQueueItem(ctx, queueSeed{ProjectID: input.ProjectID, Repo: input.Repo, QueueType: QueueTypeWarn, TargetType: "issue", TargetID: targetID, Number: issue.Number, Payload: sweeperPayload{Phase: "warn", Repo: input.Repo, TargetType: "issue", TargetNumber: issue.Number, PendingLabel: roleCfg.Lifecycle.PendingLabel, ClosedLabel: roleCfg.Lifecycle.ClosedLabel, KeepLabel: roleCfg.Lifecycle.KeepLabel, QuarantineLabel: roleCfg.Security.QuarantineLabel}})
		if err != nil {
			return DiscoveryResult{}, err
		}
		if ok {
			result.QueueItems = append(result.QueueItems, queueItem)
			warnCount++
		}
	}
	return result, nil
}

func (r *Runner) discoverPullRequestsAndClosures(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
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
		if r.shouldSkipSummary(pr.Labels, pr.Author, pr.AuthorAssociation, states[targetID], roleCfg) {
			result.Skipped++
			continue
		}
		if hasLabel(pr.Labels, roleCfg.Lifecycle.PendingLabel) {
			state, ok := states[targetID]
			if !ok || state.payload.Outcome != outcomePending || state.payload.Phase != "warn" || !dueForClose(state.payload.CloseBy, r.now()) || closeCount >= closeLimit {
				continue
			}
			queueItem, ok, err := r.buildQueueItem(ctx, queueSeed{ProjectID: input.ProjectID, Repo: input.Repo, QueueType: QueueTypeClose, TargetType: "pull_request", TargetID: targetID, Number: pr.Number, Payload: state.payload})
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
		queueItem, ok, err := r.buildQueueItem(ctx, queueSeed{ProjectID: input.ProjectID, Repo: input.Repo, QueueType: QueueTypeWarn, TargetType: "pull_request", TargetID: targetID, Number: pr.Number, Payload: sweeperPayload{Phase: "warn", Repo: input.Repo, TargetType: "pull_request", TargetNumber: pr.Number, PendingLabel: roleCfg.Lifecycle.PendingLabel, ClosedLabel: roleCfg.Lifecycle.ClosedLabel, KeepLabel: roleCfg.Lifecycle.KeepLabel, QuarantineLabel: roleCfg.Security.QuarantineLabel}})
		if err != nil {
			return DiscoveryResult{}, err
		}
		if ok {
			result.QueueItems = append(result.QueueItems, queueItem)
			warnCount++
		}
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
	if err := r.repos.Queue.Upsert(ctx, item); err != nil {
		return storage.QueueItemRecord{}, false, err
	}
	return item, true, nil
}

func (r *Runner) processWarn(ctx context.Context, queueItem storage.QueueItemRecord, payload sweeperPayload) (sweeperPayload, string, string, error) {
	roleCfg := r.roleConfig(derefString(queueItem.ProjectID))
	target, err := r.loadTarget(ctx, queueItem)
	if err != nil {
		return payload, "failed", "", err
	}
	category, confidence, rationale := classifyTarget(target, roleCfg, r.now())
	payload.Phase = "warn"
	payload.Outcome = outcomeNoAction
	payload.Category = category
	payload.Confidence = confidence
	payload.Rationale = rationale
	payload.Repo = derefString(queueItem.Repo)
	payload.TargetType = queueItem.TargetType
	payload.TargetNumber = target.Number
	payload.PendingLabel = roleCfg.Lifecycle.PendingLabel
	payload.ClosedLabel = roleCfg.Lifecycle.ClosedLabel
	payload.KeepLabel = roleCfg.Lifecycle.KeepLabel
	payload.QuarantineLabel = roleCfg.Security.QuarantineLabel
	if category == categoryNone {
		payload.Summary = defaultSkippedSummary
		return payload, "skipped", defaultSkippedSummary, nil
	}
	if category == categoryRouteSecurity {
		if roleCfg.DryRun || roleCfg.Limits.GlobalKillSwitch || r.github == nil {
			payload.Outcome = outcomeDryRun
			payload.Summary = "sweeper dry-run quarantine"
			return payload, "skipped", payload.Summary, nil
		}
		if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: payload.Repo, IssueNumber: target.Number, Labels: []string{roleCfg.Security.QuarantineLabel}}); err != nil {
			return payload, "failed", "", err
		}
		payload.Outcome = outcomeQuarantined
		payload.Summary = "sweeper quarantined target"
		return payload, "completed", payload.Summary, nil
	}
	graceDays := gracePeriodForCategory(category, roleCfg)
	payload.WarningMarkerUUID = eventlog.NewEventID("sweeper")
	payload.WarningPostedAt = r.nowISO()
	payload.CloseBy = r.now().UTC().Add(time.Duration(graceDays) * 24 * time.Hour).Format(javaScriptISOStringUTC)
	payload.CommentBody = buildWarningComment(target, payload, graceDays)
	if roleCfg.DryRun || roleCfg.Limits.GlobalKillSwitch || r.github == nil {
		payload.Outcome = outcomeDryRun
		payload.Summary = "sweeper dry-run warning"
		return payload, "skipped", payload.Summary, nil
	}
	comment, err := r.github.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: payload.Repo, IssueNumber: target.Number, Body: payload.CommentBody})
	if err != nil {
		return payload, "failed", "", err
	}
	payload.WarningCommentID = comment.ID
	if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: payload.Repo, IssueNumber: target.Number, Labels: []string{roleCfg.Lifecycle.PendingLabel}}); err != nil {
		return payload, "failed", "", err
	}
	payload.Outcome = outcomePending
	payload.Summary = fmt.Sprintf("sweeper warned %s #%d", targetKind(target.IsPR), target.Number)
	return payload, "completed", payload.Summary, nil
}

func (r *Runner) processClose(ctx context.Context, queueItem storage.QueueItemRecord, payload sweeperPayload) (sweeperPayload, string, string, error) {
	roleCfg := r.roleConfig(derefString(queueItem.ProjectID))
	target, err := r.loadTarget(ctx, queueItem)
	if err != nil {
		return payload, "failed", "", err
	}
	payload.Phase = "close"
	payload.Repo = derefString(queueItem.Repo)
	payload.TargetType = queueItem.TargetType
	payload.TargetNumber = target.Number
	payload.PendingLabel = roleCfg.Lifecycle.PendingLabel
	payload.ClosedLabel = roleCfg.Lifecycle.ClosedLabel
	payload.KeepLabel = roleCfg.Lifecycle.KeepLabel
	payload.QuarantineLabel = roleCfg.Security.QuarantineLabel
	if strings.EqualFold(target.State, "closed") {
		if err := r.removePendingLabel(ctx, roleCfg, payload.Repo, target.Number); err != nil {
			return payload, "failed", "", err
		}
		payload.Outcome = outcomeAlreadyClosedByHuman
		payload.Summary = "target already closed"
		return payload, "completed", payload.Summary, nil
	}
	if !hasLabel(target.Labels, roleCfg.Lifecycle.PendingLabel) {
		payload.Outcome = outcomeCancelled
		payload.Summary = "sweeper warning cancelled"
		if payload.WarningCommentID > 0 && r.github != nil && !roleCfg.DryRun {
			_ = r.github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: payload.Repo, CommentID: payload.WarningCommentID, Body: payload.CommentBody + "\n\nCancellation noted by sweeper."})
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
		return payload, "completed", payload.Summary, nil
	}
	category, _, rationale := classifyTarget(target, roleCfg, r.now())
	if category == categoryRouteSecurity {
		if roleCfg.DryRun || roleCfg.Limits.GlobalKillSwitch || r.github == nil {
			payload.Outcome = outcomeDryRun
			payload.Summary = "sweeper dry-run quarantine"
			return payload, "skipped", payload.Summary, nil
		}
		if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: payload.Repo, IssueNumber: target.Number, Labels: []string{roleCfg.Security.QuarantineLabel}}); err != nil {
			return payload, "failed", "", err
		}
		_ = r.github.RemoveIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: payload.Repo, IssueNumber: target.Number, Labels: []string{roleCfg.Lifecycle.PendingLabel}})
		payload.Outcome = outcomeQuarantined
		payload.Summary = "sweeper quarantined target"
		return payload, "completed", payload.Summary, nil
	}
	if category == categoryNone || (payload.Category != "" && category != payload.Category) {
		if err := r.removePendingLabel(ctx, roleCfg, payload.Repo, target.Number); err != nil {
			return payload, "failed", "", err
		}
		payload.Outcome = outcomeCancelled
		payload.Summary = "sweeper close cancelled"
		return payload, "completed", payload.Summary, nil
	}
	closeComment := buildCloseComment(target, payload, rationale)
	if roleCfg.DryRun || roleCfg.Limits.GlobalKillSwitch || r.github == nil {
		payload.Outcome = outcomeDryRun
		payload.Summary = "sweeper dry-run close"
		return payload, "skipped", payload.Summary, nil
	}
	if _, err := r.github.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: payload.Repo, IssueNumber: target.Number, Body: closeComment}); err != nil {
		return payload, "failed", "", err
	}
	if target.IsPR {
		if err := r.github.ClosePullRequest(ctx, githubinfra.ClosePullRequestInput{Repo: payload.Repo, PRNumber: target.Number}); err != nil {
			return payload, "failed", "", err
		}
	} else {
		reason := "not_planned"
		if category == categoryAlreadyFixed {
			reason = "completed"
		}
		if err := r.github.CloseIssue(ctx, githubinfra.CloseIssueInput{Repo: payload.Repo, IssueNumber: target.Number, StateReason: reason}); err != nil {
			return payload, "failed", "", err
		}
	}
	if err := r.github.RemoveIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: payload.Repo, IssueNumber: target.Number, Labels: []string{roleCfg.Lifecycle.PendingLabel}}); err != nil {
		return payload, "failed", "", err
	}
	if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: payload.Repo, IssueNumber: target.Number, Labels: []string{roleCfg.Lifecycle.ClosedLabel}}); err != nil {
		return payload, "failed", "", err
	}
	payload.Outcome = outcomeClosed
	payload.Summary = fmt.Sprintf("sweeper closed %s #%d", targetKind(target.IsPR), target.Number)
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
	if hasLabel(target.Labels, roleCfg.Lifecycle.PendingLabel) {
		payload.Summary = "sweeper reconcile: still pending"
		return payload, "skipped", payload.Summary, nil
	}
	payload.Phase = "reconcile"
	if payload.WarningCommentID > 0 && !roleCfg.DryRun {
		_ = r.github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: derefString(queueItem.Repo), CommentID: payload.WarningCommentID, Body: payload.CommentBody + "\n\nCancellation acknowledged because the pending label was removed."})
	}
	payload.Outcome = outcomeCancelledByLabelRemoval
	payload.Summary = "sweeper reconciled removed pending label"
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
		return liveTarget{Number: detail.Number, State: detail.State, Title: detail.Title, Body: detail.Body, UpdatedAt: detail.UpdatedAt, Author: detail.Author, Labels: append([]string(nil), detail.Labels...), IsPR: true, Draft: detail.IsDraft}, nil
	}
	detail, err := r.github.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: number})
	if err != nil {
		return liveTarget{}, err
	}
	return liveTarget{Number: detail.Number, State: detail.State, Title: detail.Title, Body: detail.Body, UpdatedAt: detail.UpdatedAt, Author: detail.Author, Labels: append([]string(nil), detail.Labels...)}, nil
}

func classifyTarget(target liveTarget, roleCfg config.SweeperRoleConfig, now time.Time) (string, int, string) {
	text := strings.ToLower(target.Title + "\n" + target.Body)
	if strings.Contains(text, "security") {
		return categoryRouteSecurity, 100, "security-sensitive content detected"
	}
	if roleCfg.Categories.Superseded.Enabled && (strings.Contains(text, "duplicate of #") || strings.Contains(text, "superseded by #")) {
		return categorySuperseded, maxConfidence(roleCfg.Categories.Superseded.MinConfidence), "target appears superseded by another issue or pull request"
	}
	if roleCfg.Categories.AlreadyFixed.Enabled && (strings.Contains(text, "fixed by #") || strings.Contains(text, "already fixed")) {
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

func (r *Runner) shouldSkipSummary(labels []string, author string, authorAssociation string, state sweeperStateRecord, roleCfg config.SweeperRoleConfig) bool {
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
	for _, excluded := range roleCfg.Triggers.ExcludeAuthorAssociations {
		if strings.EqualFold(strings.TrimSpace(excluded), strings.TrimSpace(authorAssociation)) {
			return true
		}
	}
	return false
}

func (r *Runner) reopenCooldownActive(state sweeperStateRecord, roleCfg config.SweeperRoleConfig) bool {
	if state.payload.Outcome != outcomeClosed {
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
	remainingWarn, remainingClose, err := r.remainingDailyDiscoveryBudget(ctx, input.Repo, roleCfg)
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

func (r *Runner) remainingDailyDiscoveryBudget(ctx context.Context, repo string, roleCfg config.SweeperRoleConfig) (int, int, error) {
	if r.repos == nil || r.repos.Queue == nil {
		return 0, 0, fmt.Errorf("sweeper queue repository is not configured")
	}
	items, err := r.repos.Queue.List(ctx)
	if err != nil {
		return 0, 0, err
	}
	today := r.now().UTC().Format("2006-01-02")
	warnUsed := 0
	closeUsed := 0
	for _, item := range items {
		if derefString(item.Repo) != repo || !strings.HasPrefix(item.CreatedAt, today) {
			continue
		}
		switch item.Type {
		case QueueTypeWarn:
			warnUsed++
		case QueueTypeClose:
			closeUsed++
		}
	}
	return remainingCount(roleCfg.Limits.MaxWarningsPerRepoPerDay, warnUsed), remainingCount(roleCfg.Limits.MaxClosesPerRepoPerDay, closeUsed), nil
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
	encoded, _ := json.Marshal(payloadEnvelope{Sweeper: payload})
	return string(encoded)
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
	return project, r.roleConfig(projectID), nil
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
	return strconv.ParseInt(parts[1], 10, 64)
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
