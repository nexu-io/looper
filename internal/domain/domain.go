package domain

import "fmt"

type LoopType string

const (
	LoopTypePlanner  LoopType = "planner"
	LoopTypeReviewer LoopType = "reviewer"
	LoopTypeWorker   LoopType = "worker"
	LoopTypeFixer    LoopType = "fixer"
	// LoopTypeCoordinator is a card-anchor-only loop the auto-intake creates at the
	// classification stage so the whole looper:auto run (classify → spec → worker →
	// shepherd) collapses onto ONE task-keyed Feishu thread. It carries no queue item,
	// so the scheduler never runs it — it exists purely to own the anchor + hold the
	// classification reasoning before any planner/worker loop exists.
	LoopTypeCoordinator LoopType = "coordinator"
)

const (
	HoldLabelGlobal   = "looper:hold"
	HoldLabelWorker   = "looper:hold:worker"
	HoldLabelFixer    = "looper:hold:fixer"
	HoldLabelReviewer = "looper:hold:reviewer"
)

var LoopTypes = []LoopType{
	LoopTypePlanner,
	LoopTypeReviewer,
	LoopTypeWorker,
	LoopTypeFixer,
	LoopTypeCoordinator,
}

type LoopTargetType string

const (
	LoopTargetTypeProject     LoopTargetType = "project"
	LoopTargetTypePullRequest LoopTargetType = "pull_request"
	LoopTargetTypeIssue       LoopTargetType = "issue"
)

type LoopStatus string

const (
	LoopStatusIdle        LoopStatus = "idle"
	LoopStatusQueued      LoopStatus = "queued"
	LoopStatusRunning     LoopStatus = "running"
	LoopStatusPaused      LoopStatus = "paused"
	LoopStatusWaiting     LoopStatus = "waiting"
	LoopStatusStopped     LoopStatus = "stopped"
	LoopStatusTerminated  LoopStatus = "terminated"
	LoopStatusCompleted   LoopStatus = "completed"
	LoopStatusFailed      LoopStatus = "failed"
	LoopStatusInterrupted LoopStatus = "interrupted"
	// LoopStatusAwaitingHuman is a mid-run HITL suspension: the agent asked a
	// human a question and the run is parked until the human answers (via
	// POST /api/v1/loops/{seq}/respond), which transitions it back to running.
	// Only reachable when hitl.enabled is true.
	LoopStatusAwaitingHuman LoopStatus = "awaiting_human"
	// LoopStatusHumanTakeover means a human has taken the loop's agent session
	// over interactively (via `looper resume <seq>`): the daemon's in-flight run
	// was stopped and the scheduler leaves the loop alone until the human hands it
	// back (POST /api/v1/loops/{seq}/handback → queued). The native session id and
	// worktree are preserved so the daemon resumes seeing the human's turns.
	LoopStatusHumanTakeover LoopStatus = "human_takeover"
	// LoopStatusShepherding is the steady state of a worker loop that has opened
	// its implementation PR under looper:auto and is now driving that PR to merge
	// (watch CI/reviews/conflicts → fix → enable auto-merge) by resuming its own
	// agent session across passes. It is a live, non-terminal status: the control
	// flow keys on the durable `$.shepherd.active` loop-metadata marker (not on
	// this status, which failure/HITL paths may transiently move), and the loop
	// reaches a terminal `completed` (with `$.shepherd.outcome`) once the PR merges
	// or closes.
	LoopStatusShepherding LoopStatus = "shepherding"
)

// StatusPinsWorktree reports whether a loop in this status holds a live claim on
// its worktree checkout, so worktree GC must NOT reclaim it. This is the single
// source of truth for both worktree-cleanup gates (the planner in
// internal/worktreecleanup and the runtime executor guard), which had drifted
// apart — one protected failed/interrupted, the other human_takeover — and
// between them pinned every resting worktree, leaking the disk (RC3).
//
// Only statuses where an agent or human is actively using the checkout, or is
// imminently about to, pin it:
//   - running: the daemon's agent is executing inside it.
//   - queued: the loop is about to be claimed and run.
//   - shepherding: the worker keeps driving its PR from the same worktree.
//   - human_takeover: a human is driving the agent session inside it; deleting
//     it would pull the working tree out from under them (the worktree is
//     explicitly preserved for this status).
//
// Every other status is RESTING (paused, waiting, failed, interrupted,
// awaiting_human, idle) or TERMINAL (completed, terminated, stopped). For those
// the branch is the source of truth — its commits are durable — so the worktree
// is a disposable cache the daemon recreates on resume (worker:
// recoverWorkerWorktree from the branch; reviewer/fixer: a fresh CreateWorktree
// each pass). GC may reclaim them; the retention grace still shields a
// recently-used worktree, so a human resuming a just-paused loop keeps any
// uncommitted increment.
func StatusPinsWorktree(status LoopStatus) bool {
	switch status {
	case LoopStatusRunning, LoopStatusQueued, LoopStatusShepherding, LoopStatusHumanTakeover:
		return true
	default:
		return false
	}
}

type RunStatus string

const (
	RunStatusQueued      RunStatus = "queued"
	RunStatusRunning     RunStatus = "running"
	RunStatusSuccess     RunStatus = "success"
	RunStatusFailed      RunStatus = "failed"
	RunStatusCancelled   RunStatus = "cancelled"
	RunStatusInterrupted RunStatus = "interrupted"
	RunStatusParseFailed RunStatus = "parse_failed"
)

var PlannerSteps = []string{"discover-issues", "prepare-worktree", "write-spec", "publish", "notify"}
var ReviewerSteps = []string{"discover", "filter", "claim", "snapshot", "review", "publish"}
var WorkerSteps = []string{"prepare-work", "prepare-worktree", "plan", "execute", "validate", "open-pr", "shepherd"}
var FixerSteps = []string{"discover-pr", "claim-pr", "collect-fixes", "prepare-worktree", "repair", "validate", "push", "reconcile-commits", "resolve-comments", "recheck"}
var AllLoopSteps = append(append(append(append([]string{}, PlannerSteps...), ReviewerSteps...), WorkerSteps...), FixerSteps...)

type LoopTarget struct {
	TargetType  LoopTargetType
	ProjectID   string
	Repo        string
	PRNumber    int64
	IssueNumber int64
}

type LoopSummary struct {
	ID        string
	ProjectID string
	Type      LoopType
	Target    LoopTarget
	Status    LoopStatus
}

var activeLoopStatuses = map[LoopStatus]struct{}{
	LoopStatusIdle: {}, LoopStatusQueued: {}, LoopStatusRunning: {}, LoopStatusPaused: {}, LoopStatusWaiting: {}, LoopStatusAwaitingHuman: {}, LoopStatusHumanTakeover: {}, LoopStatusShepherding: {},
}

var conflictingActiveLoopStatuses = map[LoopStatus]struct{}{
	LoopStatusIdle: {}, LoopStatusQueued: {}, LoopStatusRunning: {}, LoopStatusPaused: {}, LoopStatusAwaitingHuman: {}, LoopStatusHumanTakeover: {}, LoopStatusShepherding: {},
}

var terminalRunStatuses = map[RunStatus]struct{}{
	RunStatusSuccess: {}, RunStatusFailed: {}, RunStatusCancelled: {}, RunStatusInterrupted: {}, RunStatusParseFailed: {},
}

var loopStatusTransitions = map[LoopStatus][]LoopStatus{
	LoopStatusIdle:          {LoopStatusQueued, LoopStatusPaused, LoopStatusTerminated},
	LoopStatusQueued:        {LoopStatusRunning, LoopStatusPaused, LoopStatusTerminated},
	LoopStatusRunning:       {LoopStatusCompleted, LoopStatusFailed, LoopStatusPaused, LoopStatusInterrupted, LoopStatusWaiting, LoopStatusAwaitingHuman, LoopStatusHumanTakeover, LoopStatusShepherding, LoopStatusTerminated},
	LoopStatusPaused:        {LoopStatusQueued, LoopStatusCompleted, LoopStatusStopped, LoopStatusHumanTakeover, LoopStatusTerminated},
	LoopStatusWaiting:       {LoopStatusQueued, LoopStatusPaused, LoopStatusStopped, LoopStatusTerminated},
	LoopStatusAwaitingHuman: {LoopStatusRunning, LoopStatusQueued, LoopStatusPaused, LoopStatusStopped, LoopStatusHumanTakeover, LoopStatusTerminated},
	LoopStatusHumanTakeover: {LoopStatusQueued, LoopStatusRunning, LoopStatusStopped, LoopStatusTerminated},
	LoopStatusStopped:       {},
	LoopStatusTerminated:    {},
	LoopStatusCompleted:     {},
	LoopStatusFailed:        {},
	LoopStatusInterrupted:   {LoopStatusQueued, LoopStatusFailed},
	// A shepherding worker loop cycles queued→running→shepherding on each pass; a
	// human can pause/stop/close it, and it reaches completed once the PR
	// merges/closes. (Worker/reconciler self-writes go through raw Upsert and skip
	// this map; it exists so the CLI/API management paths accept these moves.)
	LoopStatusShepherding: {LoopStatusShepherding, LoopStatusQueued, LoopStatusRunning, LoopStatusPaused, LoopStatusAwaitingHuman, LoopStatusCompleted, LoopStatusStopped, LoopStatusTerminated},
}

var runStatusTransitions = map[RunStatus][]RunStatus{
	RunStatusQueued:      {RunStatusRunning},
	RunStatusRunning:     {RunStatusSuccess, RunStatusFailed, RunStatusCancelled, RunStatusInterrupted, RunStatusParseFailed},
	RunStatusSuccess:     {},
	RunStatusFailed:      {},
	RunStatusCancelled:   {},
	RunStatusInterrupted: {},
	RunStatusParseFailed: {},
}

var loopStepsByType = map[LoopType][]string{
	LoopTypePlanner:  PlannerSteps,
	LoopTypeReviewer: ReviewerSteps,
	LoopTypeWorker:   WorkerSteps,
	LoopTypeFixer:    FixerSteps,
}

func AssertKnownLoopType(loopType LoopType) error {
	for _, candidate := range LoopTypes {
		if candidate == loopType {
			return nil
		}
	}
	return fmt.Errorf("loop.type must be one of: %s, %s, %s, %s, %s", LoopTypePlanner, LoopTypeReviewer, LoopTypeWorker, LoopTypeFixer, LoopTypeCoordinator)
}

func IsAutoLaneHeld(loopType LoopType, labels []string) bool {
	if hasExactLabel(labels, HoldLabelGlobal) {
		return true
	}
	switch loopType {
	case LoopTypePlanner:
		return false
	case LoopTypeWorker:
		return hasExactLabel(labels, HoldLabelWorker)
	case LoopTypeFixer:
		return hasExactLabel(labels, HoldLabelFixer)
	case LoopTypeReviewer:
		return hasExactLabel(labels, HoldLabelReviewer)
	default:
		return false
	}
}

func IsAutomaticLoopHeld(loopType LoopType, manual bool, labels []string) bool {
	return !manual && IsAutoLaneHeld(loopType, labels)
}

func hasExactLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func AssertKnownLoopStatus(status LoopStatus) error {
	for candidate := range loopStatusTransitions {
		if candidate == status {
			return nil
		}
	}
	return fmt.Errorf("loop.status must be one of: %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s", LoopStatusIdle, LoopStatusQueued, LoopStatusRunning, LoopStatusPaused, LoopStatusWaiting, LoopStatusStopped, LoopStatusTerminated, LoopStatusCompleted, LoopStatusFailed, LoopStatusInterrupted, LoopStatusAwaitingHuman, LoopStatusHumanTakeover, LoopStatusShepherding)
}

func IsActiveLoopStatus(status LoopStatus) bool {
	_, ok := activeLoopStatuses[status]
	return ok
}

func IsConflictingActiveLoopStatus(status LoopStatus) bool {
	_, ok := conflictingActiveLoopStatuses[status]
	return ok
}

func IsTerminalRunStatus(status RunStatus) bool {
	_, ok := terminalRunStatuses[status]
	return ok
}

func LoopTargetKey(target LoopTarget) string {
	switch target.TargetType {
	case LoopTargetTypeProject:
		return "project:" + target.ProjectID
	case LoopTargetTypeIssue:
		return fmt.Sprintf("issue:%s:%d", target.Repo, target.IssueNumber)
	default:
		return fmt.Sprintf("pr:%s:%d", target.Repo, target.PRNumber)
	}
}

func PRLockKey(repo string, prNumber int64) string {
	if repo == "" {
		return ""
	}
	return fmt.Sprintf("pr:%s:%d", repo, prNumber)
}

func AssertLoopTypeMatchesTarget(loopType LoopType, target LoopTarget) error {
	if err := AssertKnownLoopType(loopType); err != nil {
		return err
	}
	switch loopType {
	case LoopTypeWorker:
		if target.TargetType != LoopTargetTypeProject && target.TargetType != LoopTargetTypePullRequest && target.TargetType != LoopTargetTypeIssue {
			return fmt.Errorf("worker loops must target a project, issue, or pull request")
		}
	case LoopTypePlanner:
		if target.TargetType != LoopTargetTypeIssue {
			return fmt.Errorf("planner loops must target an issue")
		}
	case LoopTypeReviewer, LoopTypeFixer:
		if target.TargetType != LoopTargetTypePullRequest {
			return fmt.Errorf("%s loops must target a pull request", loopType)
		}
	case LoopTypeCoordinator:
		if target.TargetType != LoopTargetTypeIssue {
			return fmt.Errorf("coordinator loops must target an issue")
		}
	}
	return nil
}

func AssertUniqueActiveLoop(existing []LoopSummary, candidate LoopSummary) error {
	if !IsConflictingActiveLoopStatus(candidate.Status) {
		return nil
	}

	for _, loop := range existing {
		if loop.ID == candidate.ID || !IsConflictingActiveLoopStatus(loop.Status) {
			continue
		}

		allowConcurrentProjectWorkers := loop.ProjectID == candidate.ProjectID &&
			loop.Type == LoopTypeWorker &&
			candidate.Type == LoopTypeWorker &&
			loop.Target.TargetType == LoopTargetTypeProject &&
			candidate.Target.TargetType == LoopTargetTypeProject
		if allowConcurrentProjectWorkers {
			continue
		}

		if loop.ProjectID == candidate.ProjectID && loop.Type == candidate.Type && LoopTargetKey(loop.Target) == LoopTargetKey(candidate.Target) {
			return fmt.Errorf("active loop already exists for %s:%s:%s", candidate.ProjectID, candidate.Type, LoopTargetKey(candidate.Target))
		}
	}

	return nil
}

func AssertLoopStatusTransition(from, to LoopStatus) error {
	allowed, ok := loopStatusTransitions[from]
	if !ok {
		return fmt.Errorf("invalid loop status transition: %s -> %s", from, to)
	}
	for _, candidate := range allowed {
		if candidate == to {
			return nil
		}
	}
	return fmt.Errorf("invalid loop status transition: %s -> %s", from, to)
}

func AssertRunStatusTransition(from, to RunStatus) error {
	allowed, ok := runStatusTransitions[from]
	if !ok {
		return fmt.Errorf("invalid run status transition: %s -> %s", from, to)
	}
	for _, candidate := range allowed {
		if candidate == to {
			return nil
		}
	}
	return fmt.Errorf("invalid run status transition: %s -> %s", from, to)
}

func AssertStepBelongsToLoopType(loopType LoopType, step string) error {
	steps, ok := loopStepsByType[loopType]
	if !ok {
		return fmt.Errorf("unknown loop type %s", loopType)
	}
	for _, candidate := range steps {
		if candidate == step {
			return nil
		}
	}
	return fmt.Errorf("step %s does not belong to loop type %s", step, loopType)
}
