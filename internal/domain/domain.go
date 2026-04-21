package domain

import "fmt"

type LoopType string

const (
	LoopTypePlanner  LoopType = "planner"
	LoopTypeReviewer LoopType = "reviewer"
	LoopTypeWorker   LoopType = "worker"
	LoopTypeFixer    LoopType = "fixer"
)

var LoopTypes = []LoopType{
	LoopTypePlanner,
	LoopTypeReviewer,
	LoopTypeWorker,
	LoopTypeFixer,
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
	LoopStatusCompleted   LoopStatus = "completed"
	LoopStatusFailed      LoopStatus = "failed"
	LoopStatusInterrupted LoopStatus = "interrupted"
)

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
var WorkerSteps = []string{"prepare-work", "prepare-worktree", "plan", "execute", "validate", "open-pr"}
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
	LoopStatusIdle: {}, LoopStatusQueued: {}, LoopStatusRunning: {}, LoopStatusPaused: {},
}

var terminalRunStatuses = map[RunStatus]struct{}{
	RunStatusSuccess: {}, RunStatusFailed: {}, RunStatusCancelled: {}, RunStatusInterrupted: {}, RunStatusParseFailed: {},
}

var loopStatusTransitions = map[LoopStatus][]LoopStatus{
	LoopStatusIdle:        {LoopStatusQueued},
	LoopStatusQueued:      {LoopStatusRunning},
	LoopStatusRunning:     {LoopStatusCompleted, LoopStatusFailed, LoopStatusPaused, LoopStatusInterrupted},
	LoopStatusPaused:      {LoopStatusQueued, LoopStatusCompleted},
	LoopStatusCompleted:   {},
	LoopStatusFailed:      {},
	LoopStatusInterrupted: {LoopStatusQueued, LoopStatusFailed},
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
	return fmt.Errorf("loop.type must be one of: %s, %s, %s, %s", LoopTypePlanner, LoopTypeReviewer, LoopTypeWorker, LoopTypeFixer)
}

func AssertKnownLoopStatus(status LoopStatus) error {
	for candidate := range loopStatusTransitions {
		if candidate == status {
			return nil
		}
	}
	return fmt.Errorf("loop.status must be one of: %s, %s, %s, %s, %s, %s, %s", LoopStatusIdle, LoopStatusQueued, LoopStatusRunning, LoopStatusPaused, LoopStatusCompleted, LoopStatusFailed, LoopStatusInterrupted)
}

func IsActiveLoopStatus(status LoopStatus) bool {
	_, ok := activeLoopStatuses[status]
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
		if target.TargetType != LoopTargetTypeProject && target.TargetType != LoopTargetTypePullRequest {
			return fmt.Errorf("worker loops must target a project or pull request")
		}
	case LoopTypePlanner:
		if target.TargetType != LoopTargetTypeIssue {
			return fmt.Errorf("planner loops must target an issue")
		}
	case LoopTypeReviewer, LoopTypeFixer:
		if target.TargetType != LoopTargetTypePullRequest {
			return fmt.Errorf("%s loops must target a pull request", loopType)
		}
	}
	return nil
}

func AssertUniqueActiveLoop(existing []LoopSummary, candidate LoopSummary) error {
	if !IsActiveLoopStatus(candidate.Status) {
		return nil
	}

	for _, loop := range existing {
		if loop.ID == candidate.ID || !IsActiveLoopStatus(loop.Status) {
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
