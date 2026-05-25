package reviewer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/infra/specpr"
	"github.com/nexu-io/looper/internal/storage"
)

var (
	ErrRepairActiveWork   = errors.New("reviewer repair active work")
	ErrRepairLoopNotFound = errors.New("reviewer repair loop not found")
)

type RepairGitHub interface {
	ViewPullRequest(context.Context, ViewPullRequestInput) (PullRequestDetail, error)
	GetCurrentUserLogin(context.Context, string) (string, error)
}

type RepairOptions struct {
	DB           *sql.DB
	Repos        *storage.Repositories
	GitHub       RepairGitHub
	Now          func() time.Time
	ReviewEvents config.ReviewerReviewEventsConfig
}

type Repairer struct {
	db           *sql.DB
	repos        *storage.Repositories
	github       RepairGitHub
	now          func() time.Time
	reviewEvents config.ReviewerReviewEventsConfig
}

type RepairInput struct {
	ProjectID string `json:"projectId,omitempty"`
	Repo      string `json:"repo"`
	PRNumber  int64  `json:"prNumber"`
	Apply     bool   `json:"apply"`
}

type RepairResult struct {
	Repo           string                `json:"repo"`
	PRNumber       int64                 `json:"prNumber"`
	ProjectID      string                `json:"projectId,omitempty"`
	LoopID         string                `json:"loopId,omitempty"`
	LoopSeq        int64                 `json:"loopSeq,omitempty"`
	Apply          bool                  `json:"apply"`
	Applied        bool                  `json:"applied"`
	AppliedChanges int                   `json:"appliedChanges"`
	GitHub         RepairGitHubSnapshot  `json:"github"`
	Local          RepairLocalSnapshot   `json:"local"`
	Diagnoses      []RepairDiagnosis     `json:"diagnoses"`
	Actions        []RepairPlannedAction `json:"actions"`
}

type RepairGitHubSnapshot struct {
	CurrentLogin         string   `json:"currentLogin,omitempty"`
	State                string   `json:"state,omitempty"`
	IsDraft              bool     `json:"isDraft"`
	HasConflicts         bool     `json:"hasConflicts"`
	ReviewDecision       string   `json:"reviewDecision,omitempty"`
	HeadSHA              string   `json:"headSha,omitempty"`
	ReviewRequests       []string `json:"reviewRequests,omitempty"`
	CurrentUserRequested bool     `json:"currentUserRequested"`
	CurrentUserReviewed  bool     `json:"currentUserReviewed"`
}

type RepairLocalSnapshot struct {
	Status               string `json:"status,omitempty"`
	CleanPolicy          string `json:"cleanPolicy,omitempty"`
	BlockingPolicy       string `json:"blockingPolicy,omitempty"`
	LastPublishedHeadSHA string `json:"lastPublishedHeadSha,omitempty"`
	LastReviewEvent      string `json:"lastReviewEvent,omitempty"`
	LastFilterSkipKind   string `json:"lastFilterSkipKind,omitempty"`
	LastFilterSkipReason string `json:"lastFilterSkipReason,omitempty"`
	HasActiveRun         bool   `json:"hasActiveRun"`
	HasActiveQueue       bool   `json:"hasActiveQueue"`
	LatestQueueStatus    string `json:"latestQueueStatus,omitempty"`
}

type RepairDiagnosis struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RepairPlannedAction struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func NewRepairer(options RepairOptions) *Repairer {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Repairer{db: options.DB, repos: options.Repos, github: options.GitHub, now: now, reviewEvents: options.ReviewEvents}
}

func (r *Repairer) Repair(ctx context.Context, input RepairInput) (RepairResult, error) {
	input.Repo = strings.TrimSpace(input.Repo)
	if input.Repo == "" || input.PRNumber <= 0 {
		return RepairResult{}, fmt.Errorf("repair requires repo and positive prNumber")
	}
	if r.repos == nil || r.repos.Loops == nil || r.repos.Projects == nil || r.repos.Runs == nil || r.repos.Queue == nil || r.repos.Events == nil {
		return RepairResult{}, fmt.Errorf("repair repositories are not configured")
	}
	if r.github == nil {
		return RepairResult{}, fmt.Errorf("repair github gateway is not configured")
	}

	loop, err := r.findReviewerLoop(ctx, input)
	if err != nil {
		return RepairResult{}, err
	}
	if loop == nil {
		return RepairResult{}, fmt.Errorf("%w: %s#%d", ErrRepairLoopNotFound, input.Repo, input.PRNumber)
	}
	project, err := r.repos.Projects.GetByID(ctx, loop.ProjectID)
	if err != nil {
		return RepairResult{}, err
	}
	if project == nil {
		return RepairResult{}, fmt.Errorf("project %s for loop %s was not found", loop.ProjectID, loop.ID)
	}

	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: project.RepoPath})
	if err != nil {
		return RepairResult{}, err
	}
	currentLogin, err := r.github.GetCurrentUserLogin(ctx, project.RepoPath)
	if err != nil {
		return RepairResult{}, err
	}

	latestQueue, err := r.repos.Queue.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return RepairResult{}, err
	}
	activeRun, err := r.repos.Runs.HasRunningByLoopID(ctx, loop.ID)
	if err != nil {
		return RepairResult{}, err
	}
	activeQueue, err := r.repos.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil {
		return RepairResult{}, err
	}

	result := r.buildPlan(*loop, detail, normalizeLogin(currentLogin), latestQueue, activeRun, activeQueue != nil, input.Apply)
	result.Repo = input.Repo
	result.PRNumber = input.PRNumber
	result.ProjectID = loop.ProjectID
	result.LoopID = loop.ID
	result.LoopSeq = loop.Seq

	if !input.Apply || len(result.Actions) == 0 {
		return result, nil
	}
	if result.Local.HasActiveRun || result.Local.HasActiveQueue {
		return result, fmt.Errorf("%w: cannot repair loop %s while it has an active run or queue item", ErrRepairActiveWork, loop.ID)
	}
	if r.db == nil {
		return result, fmt.Errorf("repair database is not configured")
	}

	applied, err := storage.WithTransactionValue(ctx, r.db, nil, func(tx *sql.Tx) (int, error) {
		transactionRepos := storage.NewRepositories(tx)
		current, err := transactionRepos.Loops.GetByID(ctx, loop.ID)
		if err != nil {
			return 0, err
		}
		if current == nil {
			return 0, fmt.Errorf("loop %s was not found", loop.ID)
		}
		hasRun, err := transactionRepos.Runs.HasRunningByLoopID(ctx, loop.ID)
		if err != nil {
			return 0, err
		}
		hasQueue, err := transactionRepos.Queue.FindActiveByLoopID(ctx, loop.ID)
		if err != nil {
			return 0, err
		}
		if hasRun || hasQueue != nil {
			return 0, fmt.Errorf("%w: cannot repair loop %s while it has an active run or queue item", ErrRepairActiveWork, loop.ID)
		}
		return r.applyPlan(ctx, transactionRepos, *current, result)
	})
	if err != nil {
		return result, err
	}
	result.Applied = applied > 0
	result.AppliedChanges = applied
	return result, nil
}

func (r *Repairer) findReviewerLoop(ctx context.Context, input RepairInput) (*storage.LoopRecord, error) {
	loops, err := r.repos.Loops.List(ctx)
	if err != nil {
		return nil, err
	}
	var selected *storage.LoopRecord
	for _, loop := range loops {
		if loop.Type != string(domain.LoopTypeReviewer) || loop.Repo == nil || loop.PRNumber == nil {
			continue
		}
		if *loop.Repo != input.Repo || *loop.PRNumber != input.PRNumber {
			continue
		}
		if input.ProjectID != "" && loop.ProjectID != input.ProjectID {
			continue
		}
		candidate := loop
		if selected == nil || preferRepairLoop(candidate, *selected) {
			selected = &candidate
		}
	}
	return selected, nil
}

func preferRepairLoop(candidate, current storage.LoopRecord) bool {
	candidateTerminal := candidate.Status == string(domain.LoopStatusTerminated)
	currentTerminal := current.Status == string(domain.LoopStatusTerminated)
	if candidateTerminal != currentTerminal {
		return !candidateTerminal
	}
	return candidate.Seq > current.Seq
}

func (r *Repairer) buildPlan(loop storage.LoopRecord, detail PullRequestDetail, currentLogin string, latestQueue *storage.QueueItemRecord, activeRun bool, activeQueue bool, apply bool) RepairResult {
	meta := parseJSONObject(loop.MetadataJSON)
	reviewEvents := r.effectiveRepairReviewEvents(meta)
	lastFilterSkip, _ := meta["lastFilterSkip"].(map[string]any)
	lastPublished, _ := stringFromAny(meta["lastPublishedHeadSha"])
	lastReviewEvent, _ := stringFromAny(meta["lastReviewEvent"])
	skipKind, _ := stringFromAny(lastFilterSkip["kind"])
	skipReason, _ := stringFromAny(lastFilterSkip["reason"])
	currentUserRequested := isCurrentUserRequested(detail.ReviewRequests, currentLogin)
	currentUserReviewed := hasReviewByAuthorForHead(detail.Reviews, currentLogin, detail.HeadSHA)
	latestQueueStatus := ""
	if latestQueue != nil {
		latestQueueStatus = latestQueue.Status
	}

	result := RepairResult{
		Apply: apply,
		GitHub: RepairGitHubSnapshot{
			CurrentLogin:         currentLogin,
			State:                detail.State,
			IsDraft:              detail.IsDraft,
			HasConflicts:         detail.HasConflicts,
			ReviewDecision:       detail.ReviewDecision,
			HeadSHA:              detail.HeadSHA,
			ReviewRequests:       append([]string(nil), detail.ReviewRequests...),
			CurrentUserRequested: currentUserRequested,
			CurrentUserReviewed:  currentUserReviewed,
		},
		Local: RepairLocalSnapshot{
			Status:               loop.Status,
			CleanPolicy:          string(reviewEvents.Clean),
			BlockingPolicy:       string(reviewEvents.Blocking),
			LastPublishedHeadSHA: lastPublished,
			LastReviewEvent:      lastReviewEvent,
			LastFilterSkipKind:   skipKind,
			LastFilterSkipReason: skipReason,
			HasActiveRun:         activeRun,
			HasActiveQueue:       activeQueue,
			LatestQueueStatus:    latestQueueStatus,
		},
	}

	if activeRun || activeQueue {
		result.Diagnoses = append(result.Diagnoses, RepairDiagnosis{Code: "active_local_work", Message: "Local loop has active work; repair will not modify it until it is idle."})
		return result
	}
	canReactivateLocalLoop := repairCanReactivateLocalLoop(loop.Status)
	terminalDiagnosisAdded := false
	addReactivationAction := func(action RepairPlannedAction) {
		if canReactivateLocalLoop {
			result.Actions = append(result.Actions, action)
			return
		}
		if !terminalDiagnosisAdded {
			result.Diagnoses = append(result.Diagnoses, RepairDiagnosis{Code: "terminal_local_loop", Message: "Local reviewer loop is terminal; repair will not reactivate it."})
			terminalDiagnosisAdded = true
		}
	}
	if !strings.EqualFold(detail.State, "OPEN") {
		result.Diagnoses = append(result.Diagnoses, RepairDiagnosis{Code: "github_pr_not_open", Message: "GitHub PR is closed or merged while local reviewer state is still live."})
		if loop.Status != string(domain.LoopStatusTerminated) {
			result.Actions = append(result.Actions, RepairPlannedAction{Code: "terminate_local_loop", Message: "Mark the local reviewer loop terminated and cancel active queue items."})
		}
		return result
	}
	if lastPublished != "" && detail.HeadSHA != "" && lastPublished == detail.HeadSHA && currentUserRequested && !currentUserReviewed {
		result.Diagnoses = append(result.Diagnoses, RepairDiagnosis{Code: "stale_local_published_head", Message: "Local state marks the current head as published, but GitHub still requests the current user and has no current-head review by that user."})
		addReactivationAction(RepairPlannedAction{Code: "clear_local_published_head", Message: "Clear local publish/review suppressors and resnapshot reviewer review-event policy."})
	}
	if staleFilterSkip(lastFilterSkip, detail, currentLogin) {
		result.Diagnoses = append(result.Diagnoses, RepairDiagnosis{Code: "stale_filter_skip", Message: "Local discovery skip metadata no longer matches current GitHub PR state."})
		addReactivationAction(RepairPlannedAction{Code: "clear_filter_skip", Message: "Clear local lastFilterSkip metadata so discovery can reconsider the PR."})
	}
	if loop.Status == string(domain.LoopStatusFailed) && currentUserRequested && !detail.IsDraft && !detail.HasConflicts {
		result.Diagnoses = append(result.Diagnoses, RepairDiagnosis{Code: "failed_loop_still_requested", Message: "Local reviewer loop is failed while GitHub still requests current-user review."})
		result.Actions = append(result.Actions, RepairPlannedAction{Code: "reactivate_failed_loop", Message: "Move the failed loop back to waiting and cancel the latest failed queue item."})
	}
	return result
}

func repairCanReactivateLocalLoop(status string) bool {
	switch domain.LoopStatus(status) {
	case domain.LoopStatusTerminated, domain.LoopStatusStopped:
		return false
	default:
		return true
	}
}

func (r *Repairer) effectiveRepairReviewEvents(meta map[string]any) config.ReviewerReviewEventsConfig {
	policy := r.reviewEvents
	if policy.Clean == "" {
		policy.Clean = config.ReviewerReviewEventComment
	}
	if policy.Blocking == "" {
		policy.Blocking = config.ReviewerReviewEventComment
	}
	if reviewEvents, ok := meta["reviewEvents"].(map[string]any); ok {
		if clean, ok := stringFromAny(reviewEvents["clean"]); ok && isValidCleanReviewEvent(clean) {
			policy.Clean = config.ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(clean)))
		}
		if blocking, ok := stringFromAny(reviewEvents["blocking"]); ok && isValidBlockingReviewEvent(blocking) {
			policy.Blocking = config.ReviewerReviewEvent(strings.ToUpper(strings.TrimSpace(blocking)))
		}
	}
	return policy
}

func staleFilterSkip(skip map[string]any, detail PullRequestDetail, currentLogin string) bool {
	if len(skip) == 0 {
		return false
	}
	headSHA, _ := stringFromAny(skip["headSha"])
	if headSHA != "" && detail.HeadSHA != "" && headSHA != detail.HeadSHA {
		return false
	}
	kind, _ := stringFromAny(skip["kind"])
	switch kind {
	case "conflicted":
		return !detail.HasConflicts
	case "draft":
		return !detail.IsDraft
	case "already_reviewed_by_current_user":
		return !hasReviewByAuthorForHead(detail.Reviews, currentLogin, detail.HeadSHA)
	case "approved":
		return !strings.EqualFold(strings.TrimSpace(detail.ReviewDecision), "APPROVED")
	case "self_authored":
		return !strings.EqualFold(normalizeLogin(detail.Author), normalizeLogin(currentLogin))
	case "ready_label":
		label, _ := stringFromAny(skip["requiredLabel"])
		if label == "" {
			return false
		}
		return !specpr.HasLabel(detail.Labels, label)
	default:
		return false
	}
}

func (r *Repairer) applyPlan(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, plan RepairResult) (int, error) {
	now := r.now().UTC()
	nowISO := eventlog.FormatJavaScriptISOString(now)
	applied := 0
	meta := parseJSONObject(loop.MetadataJSON)
	loopMeta := reviewerLoopMetadata(meta)

	for _, action := range plan.Actions {
		if repairActionReactivatesLocalLoop(action.Code) && !repairCanReactivateLocalLoop(loop.Status) {
			continue
		}
		switch action.Code {
		case "terminate_local_loop":
			loop.Status = string(domain.LoopStatusTerminated)
			loop.NextRunAt = nil
			loopMeta["status"] = string(domain.LoopStatusTerminated)
			loopMeta["terminationReason"] = "reviewer_repair_pr_not_open"
			applied++
			reason := "reviewer repair: GitHub PR is not open"
			if _, err := repos.Queue.CancelByLoop(ctx, loop.ID, nowISO, &reason); err != nil {
				return applied, err
			}
		case "clear_local_published_head":
			delete(meta, "lastPublishedHeadSha")
			delete(meta, "lastPublishedAt")
			delete(meta, "lastReviewEvent")
			delete(meta, "lastReviewSummary")
			delete(meta, "lastOutputFingerprint")
			delete(loopMeta, "lastReviewedHeadSha")
			delete(loopMeta, "lastOutputFingerprint")
			reviewEventsMeta, _ := meta["reviewEvents"].(map[string]any)
			if reviewEventsMeta == nil {
				reviewEventsMeta = map[string]any{}
			}
			if r.reviewEvents.Clean != "" {
				reviewEventsMeta["clean"] = string(r.reviewEvents.Clean)
			}
			if r.reviewEvents.Blocking != "" {
				reviewEventsMeta["blocking"] = string(r.reviewEvents.Blocking)
			}
			meta["reviewEvents"] = reviewEventsMeta
			loop.Status = string(domain.LoopStatusWaiting)
			loop.NextRunAt = nil
			loopMeta["status"] = string(domain.LoopStatusWaiting)
			applied++
		case "clear_filter_skip":
			delete(meta, "lastFilterSkip")
			loop.Status = string(domain.LoopStatusWaiting)
			loop.NextRunAt = nil
			loopMeta["status"] = string(domain.LoopStatusWaiting)
			applied++
		case "reactivate_failed_loop":
			loop.Status = string(domain.LoopStatusWaiting)
			loop.NextRunAt = nil
			loopMeta["status"] = string(domain.LoopStatusWaiting)
			loopMeta["consecutiveFailures"] = 0
			delete(loopMeta, "lastFailure")
			if latest, err := repos.Queue.GetLatestByLoopID(ctx, loop.ID); err != nil {
				return applied, err
			} else if latest != nil && latest.Status == "failed" {
				latest.Status = "cancelled"
				latest.FinishedAt = stringPtr(nowISO)
				latest.UpdatedAt = nowISO
				if err := repos.Queue.Upsert(ctx, *latest); err != nil {
					return applied, err
				}
			}
			applied++
		}
	}

	if applied == 0 {
		return 0, nil
	}
	loopMeta["lastManualRepairAt"] = nowISO
	loopMeta["lastManualRepairReason"] = repairActionCodes(plan.Actions)
	meta["loop"] = loopMeta
	encoded, err := json.Marshal(meta)
	if err != nil {
		return applied, fmt.Errorf("marshal repaired loop metadata: %w", err)
	}
	loop.MetadataJSON = stringPtr(string(encoded))
	loop.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		return applied, err
	}
	entityID := fmt.Sprintf("%s#%d", plan.Repo, plan.PRNumber)
	if err := eventlog.Append(ctx, repos, eventlog.AppendInput{
		EventType:  "reviewer.loop.repair",
		ProjectID:  stringPtr(loop.ProjectID),
		LoopID:     stringPtr(loop.ID),
		EntityType: stringPtr("pull_request"),
		EntityID:   stringPtr(entityID),
		Payload: map[string]any{
			"repo":      plan.Repo,
			"prNumber":  plan.PRNumber,
			"diagnoses": plan.Diagnoses,
			"actions":   plan.Actions,
		},
		CreatedAt: now,
	}); err != nil {
		return applied, err
	}
	return applied, nil
}

func repairActionReactivatesLocalLoop(code string) bool {
	switch code {
	case "clear_local_published_head", "clear_filter_skip", "reactivate_failed_loop":
		return true
	default:
		return false
	}
}

func repairActionCodes(actions []RepairPlannedAction) []string {
	codes := make([]string, 0, len(actions))
	for _, action := range actions {
		codes = append(codes, action.Code)
	}
	return codes
}
