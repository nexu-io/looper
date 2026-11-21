package executionhost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/processcontainment"
	"github.com/nexu-io/looper/internal/storage"
)

// Recovery is the existing daemon reconciliation logic, shared by independent
// hosts. Process probes are observations, never authority to kill or requeue.
type Recovery struct {
	Config             func() config.Config
	Logger             bootstrap.Logger
	ReadProcessCommand func(context.Context, int) (string, error)
	ActiveExecutions   *ActiveExecutionRegistry
}

// RecoverQueueClaims closes the claim-before-run crash window using durable
// task/queue state. It runs after orphan quarantine and stale-run repair, before
// either host starts claiming work. Active process evidence always wins.
func (r *Recovery) RecoverQueueClaims(ctx context.Context, repositories *storage.Repositories, nowISO string) error {
	if repositories == nil || repositories.Queue == nil || repositories.AgentExecutions == nil || repositories.Loops == nil || repositories.Runs == nil {
		return nil
	}
	items, err := repositories.Queue.ListByStatuses(ctx, []string{"running"})
	if err != nil {
		return err
	}
	executions, err := repositories.AgentExecutions.ListActive(ctx)
	if err != nil {
		return err
	}
	blocked := make(map[string]bool)
	for _, execution := range executions {
		if execution.LoopID != nil {
			blocked[*execution.LoopID] = true
		}
	}
	for _, item := range items {
		if item.LoopID == nil || strings.TrimSpace(*item.LoopID) == "" {
			return fmt.Errorf("running queue item %s has no task identity", item.ID)
		}
		if blocked[*item.LoopID] || (r.ActiveExecutions != nil && r.ActiveExecutions.OwnsQueueClaim(item.ID)) {
			continue
		}
		loop, err := repositories.Loops.GetByID(ctx, *item.LoopID)
		if err != nil {
			return err
		}
		if loop == nil {
			return fmt.Errorf("running queue item %s references missing task %s", item.ID, *item.LoopID)
		}
		if loop.Status == "queued" && !LatestQueueIsManualIntervention(&item) {
			// The queue was claimed but the runner never changed the task to
			// running. Preserve this queue's payload, attempts and identity.
			if _, err := repositories.Queue.RequeueRunningByLoop(ctx, loop.ID, nowISO); err != nil {
				return err
			}
			continue
		}
		latest, err := repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return err
		}
		if _, err := r.RepairStaleRunQueueState(ctx, repositories, *loop, latest, false, nowISO); err != nil {
			return err
		}
	}
	return nil
}

type StaleRunReconcileSummary struct {
	Mode                string `json:"mode"`
	StartedAt           string `json:"startedAt,omitempty"`
	CompletedAt         string `json:"completedAt,omitempty"`
	CandidateRuns       int64  `json:"candidateRuns"`
	InterruptedRuns     int64  `json:"interruptedRuns"`
	LoopsRequeued       int64  `json:"loopsRequeued"`
	QueueItemsRequeued  int64  `json:"queueItemsRequeued"`
	QueueItemsCancelled int64  `json:"queueItemsCancelled"`
	CleanedExecutions   int64  `json:"cleanedExecutions"`
	// QuarantinedExecutions counts executions parked via QuarantineRecoveryEvidence
	// (still-running evidence + manual_intervention). Never report these as cleaned.
	QuarantinedExecutions int64    `json:"quarantinedExecutions"`
	SkippedUncertainRuns  int64    `json:"skippedUncertainRuns"`
	EventsWritten         int64    `json:"eventsWritten"`
	RunIDs                []string `json:"runIds,omitempty"`
	LoopIDs               []string `json:"loopIds,omitempty"`
	ExecutionIDs          []string `json:"executionIds,omitempty"`
}

type StaleRunReconcileMode string

const (
	StaleRunReconcileModeStartup StaleRunReconcileMode = "startup"
	StaleRunReconcileModeLive    StaleRunReconcileMode = "live"
	StaleRunReconcileModeManual  StaleRunReconcileMode = "manual"
)

func DerefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (r *Recovery) ReconcileStaleRunningRunsWithMode(ctx context.Context, repositories *storage.Repositories, now time.Time, mode StaleRunReconcileMode) (StaleRunReconcileSummary, error) {
	summary := StaleRunReconcileSummary{Mode: string(mode), StartedAt: FormatJavaScriptISOString(now)}
	if repositories == nil || repositories.Runs == nil || repositories.Loops == nil {
		return summary, nil
	}
	nowISO := summary.StartedAt
	runningRuns, err := repositories.Runs.ListByStatus(ctx, string(domain.RunStatusRunning))
	if err != nil {
		return StaleRunReconcileSummary{}, err
	}
	activeExecutionsByRunID := make(map[string][]storage.AgentExecutionRecord)
	if repositories.AgentExecutions != nil {
		activeExecutions, err := repositories.AgentExecutions.ListActive(ctx)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		for _, execution := range activeExecutions {
			if execution.RunID == nil || strings.TrimSpace(*execution.RunID) == "" {
				continue
			}
			activeExecutionsByRunID[*execution.RunID] = append(activeExecutionsByRunID[*execution.RunID], execution)
		}
	}
	// Loops whose work was parked via QuarantineRecoveryEvidence must not be
	// requeued by the post-interrupt repair pass or the later interrupted-loop
	// sweep while agent_executions remain running evidence (#575).
	quarantinedLoopIDs := make(map[string]struct{})
	for _, run := range runningRuns {
		if err := ctx.Err(); err != nil {
			return StaleRunReconcileSummary{}, err
		}
		loop, err := repositories.Loops.GetByID(ctx, run.LoopID)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		if loop == nil {
			continue
		}
		latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, run.LoopID)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		decision, err := r.EvaluateStaleRunCandidate(ctx, repositories, run, *loop, latestRun, activeExecutionsByRunID[run.ID], now, mode)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		if !decision.Candidate {
			continue
		}
		summary.CandidateRuns += 1
		if decision.Uncertain {
			summary.SkippedUncertainRuns += 1
			summary.EventsWritten += decision.EventsWritten
			continue
		}
		if !decision.Interrupt {
			continue
		}
		if err := InterruptRecoveryRun(ctx, repositories, run, *loop, nowISO, decision.Message); err != nil {
			return StaleRunReconcileSummary{}, err
		}
		summary.InterruptedRuns += 1
		summary.EventsWritten += 1
		summary.RunIDs = append(summary.RunIDs, run.ID)
		summary.LoopIDs = append(summary.LoopIDs, run.LoopID)

		quarantinedAny := false
		for _, execution := range decision.CleanupExecutions {
			// #575: do not mark terminal as "cleaned" from PID evidence alone.
			// Quarantine affected work; leave agent_executions as evidence.
			message := decision.ExecutionMessage
			if strings.TrimSpace(message) == "" {
				message = "stale-run reconciliation: execution evidence without containment confirmation"
			}
			quarantined, wrote, err := r.QuarantineRecoveryEvidence(ctx, repositories, execution, nowISO, message)
			if err != nil {
				return StaleRunReconcileSummary{}, err
			}
			if quarantined {
				quarantinedAny = true
				// Quarantine is not cleanup: agent_executions stay active evidence.
				summary.QuarantinedExecutions += 1
				summary.ExecutionIDs = append(summary.ExecutionIDs, execution.ID)
			}
			if wrote {
				summary.EventsWritten += 1
			}
		}
		// Dead/nil-PID cleanup paths leave agent_executions as running evidence and
		// may have just parked the loop/queue. Do not continue into queue repair
		// with the pre-quarantine loop/latestRun snapshot — that can flip
		// paused/manual_intervention back to interrupted/queued and create an
		// active queue item, defeating the safety floor.
		if quarantinedAny || len(decision.CleanupExecutions) > 0 {
			quarantinedLoopIDs[run.LoopID] = struct{}{}
			continue
		}

		latestRunBlocksRequeue := false
		if latestRun != nil && latestRun.ID != run.ID && latestRun.Status == string(domain.RunStatusRunning) {
			verification, err := r.VerifyRunExecutionLiveness(ctx, repositories, activeExecutionsByRunID[latestRun.ID], now, string(mode)+"_latest_run")
			if err != nil {
				return StaleRunReconcileSummary{}, err
			}
			summary.EventsWritten += verification.EventsWritten
			latestRunBlocksRequeue = verification.Live || verification.Uncertain
		}
		queueRepair, err := r.RepairStaleRunQueueState(ctx, repositories, *loop, latestRun, latestRunBlocksRequeue, nowISO)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		summary.LoopsRequeued += queueRepair.LoopsRequeued
		summary.QueueItemsRequeued += queueRepair.QueueItemsRequeued
		summary.QueueItemsCancelled += queueRepair.QueueItemsCancelled
		summary.EventsWritten += queueRepair.EventsWritten
	}
	if repositories.Queue != nil {
		loops, err := repositories.Loops.List(ctx)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		for _, loop := range loops {
			if _, quarantined := quarantinedLoopIDs[loop.ID]; quarantined {
				continue
			}
			queueRepair, err := r.RepairInterruptedLoopQueueIfNeeded(ctx, repositories, loop, nowISO)
			if err != nil {
				return StaleRunReconcileSummary{}, err
			}
			summary.LoopsRequeued += queueRepair.LoopsRequeued
			summary.QueueItemsRequeued += queueRepair.QueueItemsRequeued
			summary.QueueItemsCancelled += queueRepair.QueueItemsCancelled
			summary.EventsWritten += queueRepair.EventsWritten
		}
	}
	summary.CompletedAt = nowISO
	return summary, nil
}

type StaleRunCandidateDecision struct {
	Candidate         bool
	Interrupt         bool
	Uncertain         bool
	Message           string
	ExecutionMessage  string
	CleanupExecutions []storage.AgentExecutionRecord
	EventsWritten     int64
}

type StaleRunQueueRepairSummary struct {
	LoopsRequeued       int64
	QueueItemsRequeued  int64
	QueueItemsCancelled int64
	EventsWritten       int64
}

func (r *Recovery) EvaluateStaleRunCandidate(ctx context.Context, repositories *storage.Repositories, run storage.RunRecord, loop storage.LoopRecord, latestRun *storage.RunRecord, activeExecutions []storage.AgentExecutionRecord, now time.Time, mode StaleRunReconcileMode) (StaleRunCandidateDecision, error) {
	decision := StaleRunCandidateDecision{}
	if run.Status != string(domain.RunStatusRunning) {
		return decision, nil
	}
	if mode == StaleRunReconcileModeStartup {
		decision.Candidate = true
		if latestRun != nil && latestRun.ID == run.ID && len(activeExecutions) > 0 {
			// Active execution evidence is never Authority to kill, mark terminal,
			// or requeue as cleaned (#575/#581). Classify for events; orphan
			// cleanup quarantines observed_live/uncertain. Leader exit / PID
			// absence alone cannot confirm dead or interrupt for requeue.
			verification, err := r.VerifyRunExecutionLiveness(ctx, repositories, activeExecutions, now, "startup_stale_run")
			if err != nil {
				return StaleRunCandidateDecision{}, err
			}
			decision.EventsWritten += verification.EventsWritten
			decision.Uncertain = true
			return decision, nil
		}
		// No active agent_execution rows: no process evidence to mis-handle.
		// Interrupt orphan running runs only; queue repair uses durable
		// finalize/lease semantics (not PID inference).
		decision.Interrupt = latestRun == nil || latestRun.ID != run.ID || len(activeExecutions) == 0
		decision.Message = "Interrupted stale/orphaned running run during looperd recovery"
		return decision, nil
	}

	if latestRun == nil {
		return decision, nil
	}
	if RunHeartbeatIsRecent(run, now, 30*time.Minute) {
		return decision, nil
	}
	if len(activeExecutions) == 0 && latestRun.ID == run.ID && !IsAgentBackedRunStep(loop, run) {
		return decision, nil
	}
	decision.Candidate = true
	if len(activeExecutions) > 0 {
		verification, err := r.VerifyRunExecutionLiveness(ctx, repositories, activeExecutions, now, string(mode)+"_stale_run")
		if err != nil {
			return StaleRunCandidateDecision{}, err
		}
		decision.EventsWritten += verification.EventsWritten
		if verification.Uncertain {
			decision.Uncertain = true
			return decision, nil
		}
		if verification.Live {
			return StaleRunCandidateDecision{}, nil
		}
		decision.CleanupExecutions = append(decision.CleanupExecutions, verification.DeadExecutions...)
		decision.ExecutionMessage = "Killed during stale-run reconciliation"
	}
	decision.Interrupt = true
	if latestRun.ID != run.ID {
		decision.Message = "Interrupted superseded stale running run during stale-run reconciliation"
	} else {
		decision.Message = "Interrupted stale running run during stale-run reconciliation"
	}
	return decision, nil
}

type ExecutionLivenessResult struct {
	Live bool
	// Uncertain is true when any execution cannot be treated as confirmed-dead
	// Authority (including PID absent / not running after restart rules).
	Uncertain bool
	// DeadExecutions holds rows whose PID probe found no matching live process.
	// These are NOT confirmed-dead Authority (#581): callers may quarantine
	// evidence but must not mark terminal, signal, or requeue from this alone.
	// Name retained for call-site compatibility; treat as "no live match".
	DeadExecutions []storage.AgentExecutionRecord
	EventsWritten  int64
}

func (r *Recovery) VerifyRunExecutionLiveness(ctx context.Context, repositories *storage.Repositories, executions []storage.AgentExecutionRecord, now time.Time, scope string) (ExecutionLivenessResult, error) {
	result := ExecutionLivenessResult{}
	nowISO := FormatJavaScriptISOString(now)
	for _, execution := range executions {
		// Shared classification: PID/PGID is evidence only (#581). Confirmed-dead
		// never comes from PID absence. Live reconcile still uses DeadExecutions
		// to quarantine "no live match" rows without requeue or SIGKILL.
		classification, err := r.ClassifyStartupExecution(ctx, execution, nil)
		if err != nil {
			return ExecutionLivenessResult{}, err
		}
		wroteClass, classErr := r.AppendContainmentClassificationEvent(ctx, repositories, execution, classification, scope, nowISO)
		if classErr != nil {
			return ExecutionLivenessResult{}, classErr
		}
		if wroteClass {
			result.EventsWritten += 1
		}
		switch classification.Class {
		case ContainmentConfirmedDead:
			// Durable terminal or current-daemon drain — not expected for active
			// ListActive rows without a handle.
			continue
		case ContainmentObservedLive:
			result.Live = true
			continue
		default:
			// Uncertain classes:
			// - identity probe error / mismatch → skip automatic interrupt (ambiguous)
			// - pid absent / not running → not confirmed-dead; still list for
			//   quarantine-only cleanup (no signal, no agent_execution terminal)
			if classification.Reason == "process_probe_error" || classification.Reason == "process_identity_mismatch" {
				if r.Logger != nil && classification.Reason == "process_probe_error" {
					r.Logger.Warn("failed to verify active agent execution identity", map[string]any{
						"executionId": execution.ID, "pid": classification.PID, "error": classification.Reason, "scope": scope,
					})
				}
				written, appendErr := r.AppendUncertainProcessIdentityEvent(ctx, repositories, execution, classification.PID, scope, nowISO)
				if appendErr != nil {
					return ExecutionLivenessResult{}, appendErr
				}
				if written {
					result.EventsWritten += 1
				}
				result.Uncertain = true
				continue
			}
			// pid_absent / pid_not_running_not_confirmed_dead / other uncertain:
			// "no live match" evidence for quarantine paths. Do not set Uncertain
			// alone here — that would skip quarantine in live reconcile. Callers
			// must still treat DeadExecutions as non-Authority for terminal/requeue.
			result.DeadExecutions = append(result.DeadExecutions, execution)
		}
	}
	return result, nil
}

func (r *Recovery) RepairStaleRunQueueState(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, latestRun *storage.RunRecord, latestRunHasLiveAgent bool, nowISO string) (StaleRunQueueRepairSummary, error) {
	summary := StaleRunQueueRepairSummary{}
	if repositories == nil || repositories.Queue == nil {
		return summary, nil
	}
	latestQueue, err := repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return StaleRunQueueRepairSummary{}, err
	}
	if LatestQueueIsManualIntervention(latestQueue) {
		if loop.Status == "running" || loop.Status == "queued" {
			if latestQueue.Status == "manual_intervention" {
				normalizedLoop := loop
				normalizedLoop.Status = NormalizeStaleQueuedLoopStatus(loop, DerefRunOrZero(latestRun))
				normalizedLoop.NextRunAt = nil
				if latestRun != nil {
					normalizedLoop.LastRunAt = CoalesceString(latestRun.EndedAt, StringPtr(latestRun.StartedAt), loop.LastRunAt)
				}
				normalizedLoop.UpdatedAt = nowISO
				if err := repositories.Loops.Upsert(ctx, normalizedLoop); err != nil {
					return StaleRunQueueRepairSummary{}, err
				}
			} else if latestQueue.Status == "running" {
				requeuedLoop := loop
				requeuedLoop.Status = "queued"
				requeuedLoop.NextRunAt = StringPtr(nowISO)
				if latestRun != nil {
					requeuedLoop.LastRunAt = CoalesceString(latestRun.EndedAt, StringPtr(latestRun.StartedAt), loop.LastRunAt)
				}
				requeuedLoop.UpdatedAt = nowISO
				if _, err := repositories.Queue.RequeueRunningByLoop(ctx, loop.ID, nowISO); err != nil {
					return StaleRunQueueRepairSummary{}, err
				}
				if err := repositories.Loops.Upsert(ctx, requeuedLoop); err != nil {
					return StaleRunQueueRepairSummary{}, err
				}
				summary.LoopsRequeued = 1
				summary.QueueItemsRequeued = 1
				return summary, nil
			}
		}
	}
	if ShouldRequeueLoop(loop, latestRun, latestRunHasLiveAgent) {
		requeuedLoop := loop
		requeuedLoop.Status = "queued"
		requeuedLoop.NextRunAt = StringPtr(nowISO)
		if latestRun != nil {
			requeuedLoop.LastRunAt = CoalesceString(latestRun.EndedAt, StringPtr(latestRun.StartedAt), loop.LastRunAt)
		}
		requeuedLoop.UpdatedAt = nowISO
		if err := repositories.Loops.Upsert(ctx, requeuedLoop); err != nil {
			return StaleRunQueueRepairSummary{}, err
		}
		activeQueue, err := repositories.Queue.FindActiveByLoopID(ctx, loop.ID)
		if err != nil {
			return StaleRunQueueRepairSummary{}, err
		}
		keepQueueID := ""
		if activeQueue != nil {
			keepQueueID = activeQueue.ID
		}
		requeuedCount, err := repositories.Queue.RequeueRunningByLoop(ctx, loop.ID, nowISO)
		if err != nil {
			return StaleRunQueueRepairSummary{}, err
		}
		createdQueue := int64(0)
		if requeuedCount == 0 {
			if err := EnsureRecoveryQueueItem(ctx, repositories, requeuedLoop, nowISO, int64(r.Config().Scheduler.RetryMaxAttempts)); err != nil {
				return StaleRunQueueRepairSummary{}, err
			}
			activeQueue, err = repositories.Queue.FindActiveByLoopID(ctx, loop.ID)
			if err != nil {
				return StaleRunQueueRepairSummary{}, err
			}
			if activeQueue != nil {
				keepQueueID = activeQueue.ID
				createdQueue = 1
			}
		}
		if keepQueueID != "" {
			duplicateReason := "Cancelled duplicate active queue items during stale-run reconciliation"
			cancelledDuplicates, err := repositories.Queue.CancelActiveByLoopExcept(ctx, loop.ID, keepQueueID, nowISO, &duplicateReason)
			if err != nil {
				return StaleRunQueueRepairSummary{}, err
			}
			summary.QueueItemsCancelled += cancelledDuplicates
		}
		summary.LoopsRequeued = 1
		summary.QueueItemsRequeued = requeuedCount + createdQueue
		if err := AppendSystemEvent(ctx, repositories, storage.EventLogRecord{
			ID:         NewRuntimeEventID(),
			EventType:  "looperd.recovery.loop_requeued",
			LoopID:     StringPtr(loop.ID),
			EntityType: StringPtr("loop"),
			EntityID:   StringPtr(loop.ID),
			PayloadJSON: MustMarshalJSON(map[string]any{
				"previousStatus":      loop.Status,
				"nextRunAt":           nowISO,
				"recoveredQueueItems": requeuedCount + createdQueue,
			}),
			CreatedAt: nowISO,
		}); err != nil {
			return StaleRunQueueRepairSummary{}, err
		}
		summary.EventsWritten = 1
		return summary, nil
	}
	reason := "Cancelled stale queue items during stale-run reconciliation"
	cancelledCount, err := repositories.Queue.CancelByLoop(ctx, loop.ID, nowISO, &reason)
	if err != nil {
		return StaleRunQueueRepairSummary{}, err
	}
	summary.QueueItemsCancelled = cancelledCount
	return summary, nil
}

func (r *Recovery) RepairInterruptedLoopQueueIfNeeded(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, nowISO string) (StaleRunQueueRepairSummary, error) {
	if repositories == nil || repositories.Runs == nil || repositories.Queue == nil {
		return StaleRunQueueRepairSummary{}, nil
	}
	latestRun, err := repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil || latestRun == nil || latestRun.Status != string(domain.RunStatusInterrupted) {
		return StaleRunQueueRepairSummary{}, err
	}
	// A resumed claim is owned before its new run is created. Do not reclaim it
	// from the preceding interrupted run while the scheduler is starting it.
	activeQueue, err := repositories.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil {
		return StaleRunQueueRepairSummary{}, err
	}
	if activeQueue != nil && r.ActiveExecutions.OwnsQueueClaim(activeQueue.ID) {
		return StaleRunQueueRepairSummary{}, nil
	}
	activeCount, err := repositories.Queue.CountActiveByLoopID(ctx, loop.ID)
	if err != nil {
		return StaleRunQueueRepairSummary{}, err
	}
	runningCount, err := repositories.Queue.CountByLoopIDAndStatus(ctx, loop.ID, "running")
	if err != nil {
		return StaleRunQueueRepairSummary{}, err
	}
	if loop.Status == "queued" && runningCount == 0 && activeCount == 1 {
		return StaleRunQueueRepairSummary{}, nil
	}
	if activeCount == 0 && !ShouldRequeueLoop(loop, latestRun, false) {
		return StaleRunQueueRepairSummary{}, nil
	}
	return r.RepairStaleRunQueueState(ctx, repositories, loop, latestRun, false, nowISO)
}

func IsAgentBackedRunStep(loop storage.LoopRecord, run storage.RunRecord) bool {
	if run.CurrentStep == nil {
		return false
	}
	step := strings.TrimSpace(*run.CurrentStep)
	switch loop.Type {
	case "planner":
		return step == "write-spec"
	case "reviewer":
		return step == "thread_resolution" || step == "review"
	case "fixer":
		return step == "repair"
	case "worker":
		return step == "execute"
	default:
		return false
	}
}

func RunHeartbeatIsRecent(run storage.RunRecord, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return true
	}
	heartbeatAt := FirstNonEmpty(StringOrEmpty(run.LastHeartbeatAt), run.UpdatedAt, run.StartedAt)
	if heartbeatAt == "" {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(heartbeatAt))
	if err != nil {
		return false
	}
	return !parsed.UTC().Before(now.UTC().Add(-ttl))
}

func (r *Recovery) AppendUncertainProcessIdentityEvent(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, pid int, scope string, nowISO string) (bool, error) {
	if r.Logger != nil {
		r.Logger.Warn("recovery skipped due to uncertain process identity", map[string]any{"executionId": execution.ID, "pid": pid, "scope": scope})
	}
	payloadJSON := MustMarshalJSON(map[string]any{
		"pid":    pid,
		"reason": "command_mismatch",
		"scope":  scope,
	})
	if repositories != nil && repositories.Events != nil {
		events, err := repositories.Events.ListByEntity(ctx, "agent_execution", execution.ID)
		if err != nil {
			return false, err
		}
		for _, event := range events {
			if event.EventType == "looperd.recovery.process_identity_uncertain" && event.PayloadJSON == payloadJSON {
				return false, nil
			}
		}
	}
	if err := AppendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:          NewRuntimeEventID(),
		EventType:   "looperd.recovery.process_identity_uncertain",
		ProjectID:   execution.ProjectID,
		LoopID:      execution.LoopID,
		RunID:       execution.RunID,
		EntityType:  StringPtr("agent_execution"),
		EntityID:    StringPtr(execution.ID),
		PayloadJSON: payloadJSON,
		CreatedAt:   nowISO,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// AppendContainmentClassificationEvent records confirmed_dead / observed_live /
// uncertain for an execution observation. Dedupes identical class+reason+scope.
func (r *Recovery) AppendContainmentClassificationEvent(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, classification ContainmentClassification, scope, nowISO string) (bool, error) {
	payload := map[string]any{
		"class":  string(classification.Class),
		"reason": classification.Reason,
		"scope":  scope,
	}
	if classification.PID > 0 {
		payload["pid"] = classification.PID
	}
	payloadJSON := MustMarshalJSON(payload)
	if repositories != nil && repositories.Events != nil {
		events, err := repositories.Events.ListByEntity(ctx, "agent_execution", execution.ID)
		if err != nil {
			return false, err
		}
		for _, event := range events {
			if event.EventType == "looperd.recovery.containment_classified" && event.PayloadJSON == payloadJSON {
				return false, nil
			}
		}
	}
	if r.Logger != nil {
		r.Logger.Info("startup recovery classified containment evidence", map[string]any{
			"executionId": execution.ID,
			"class":       string(classification.Class),
			"reason":      classification.Reason,
			"scope":       scope,
			"pid":         classification.PID,
		})
	}
	if err := AppendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:          NewRuntimeEventID(),
		EventType:   "looperd.recovery.containment_classified",
		ProjectID:   execution.ProjectID,
		LoopID:      execution.LoopID,
		RunID:       execution.RunID,
		EntityType:  StringPtr("agent_execution"),
		EntityID:    StringPtr(execution.ID),
		PayloadJSON: payloadJSON,
		CreatedAt:   nowISO,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// QuarantineRecoveryEvidence parks work tied to uncertain/orphan execution
// evidence using existing manual_intervention / paused states. It does not
// signal processes or mark agent_executions terminal.
//
// Returns (didQuarantine, wroteEvent, err).
func (r *Recovery) QuarantineRecoveryEvidence(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, nowISO, reason string) (bool, bool, error) {
	if repositories == nil {
		return false, false, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "recovery quarantine: execution evidence without containment confirmation"
	}
	did := false
	wrote := false

	if execution.LoopID != nil && strings.TrimSpace(*execution.LoopID) != "" && repositories.Loops != nil {
		loop, err := repositories.Loops.GetByID(ctx, *execution.LoopID)
		if err != nil {
			return false, false, err
		}
		if loop != nil {
			switch loop.Status {
			case "paused", "completed", "failed", "terminated", "stopped", "human_takeover":
				// Already non-actionable for claim/requeue. human_takeover is a
				// deliberate park (interactive handback); do not rewrite to paused.
			default:
				updated := *loop
				updated.Status = "paused"
				updated.NextRunAt = nil
				updated.UpdatedAt = nowISO
				if err := repositories.Loops.Upsert(ctx, updated); err != nil {
					return false, false, err
				}
				did = true
			}
		}
	}

	if execution.LoopID != nil && strings.TrimSpace(*execution.LoopID) != "" && repositories.Queue != nil {
		var items []storage.QueueItemRecord
		if active, err := repositories.Queue.FindActiveByLoopID(ctx, *execution.LoopID); err != nil {
			return false, false, err
		} else if active != nil {
			items = append(items, *active)
		}
		if latest, err := repositories.Queue.GetLatestByLoopID(ctx, *execution.LoopID); err != nil {
			return false, false, err
		} else if latest != nil && (len(items) == 0 || items[0].ID != latest.ID) {
			items = append(items, *latest)
		}
		for _, item := range items {
			if item.Status != "queued" && item.Status != "running" {
				continue
			}
			msg := reason
			if err := repositories.Queue.Fail(ctx, storage.QueueFailInput{
				ID:           item.ID,
				FinishedAt:   nowISO,
				UpdatedAt:    nowISO,
				ErrorMessage: &msg,
				ErrorKind:    "manual_intervention",
			}); err != nil {
				return false, false, err
			}
			did = true
		}
	}

	payload := map[string]any{
		"reason":      reason,
		"recoveredAt": nowISO,
		"executionId": execution.ID,
		"status":      execution.Status,
	}
	if execution.PID != nil && *execution.PID > 0 {
		payload["pid"] = *execution.PID
	}
	if err := AppendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:          NewRuntimeEventID(),
		EventType:   "looperd.recovery.execution_quarantined",
		ProjectID:   execution.ProjectID,
		LoopID:      execution.LoopID,
		RunID:       execution.RunID,
		EntityType:  StringPtr("agent_execution"),
		EntityID:    StringPtr(execution.ID),
		PayloadJSON: MustMarshalJSON(payload),
		CreatedAt:   nowISO,
	}); err != nil {
		return did, false, err
	}
	wrote = true
	if r.Logger != nil {
		r.Logger.Warn("recovery quarantined execution evidence without process kill", map[string]any{
			"executionId": execution.ID,
			"loopId":      execution.LoopID,
			"runId":       execution.RunID,
			"reason":      reason,
		})
	}
	return did || wrote, wrote, nil
}

func EnsureRecoveryQueueItem(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, nowISO string, maxAttempts int64) error {
	activeQueue, err := repositories.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil {
		return err
	}
	if activeQueue != nil {
		return nil
	}

	latestQueue, err := repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return err
	}
	if latestQueue != nil {
		if latestQueue.Status == "queued" || latestQueue.Status == "running" {
			return nil
		}
		if latestQueue.DedupeKey != "" {
			activeByDedupe, err := repositories.Queue.FindActiveByDedupe(ctx, latestQueue.DedupeKey)
			if err != nil {
				return err
			}
			if activeByDedupe != nil {
				return nil
			}
		}

		replacement := *latestQueue
		replacement.ID = NewRuntimeEventID()
		replacement.Status = "queued"
		replacement.AvailableAt = nowISO
		replacement.Attempts = 0
		replacement.ClaimedBy = nil
		replacement.ClaimedAt = nil
		replacement.StartedAt = nil
		replacement.FinishedAt = nil
		replacement.LastError = nil
		replacement.LastErrorKind = nil
		replacement.CreatedAt = nowISO
		replacement.UpdatedAt = nowISO
		_, _, err := repositories.Queue.UpsertActiveByDedupeOrGetExisting(ctx, replacement)
		return err
	}

	queueRecord, ok, err := BuildRecoveryQueueItem(loop, nowISO, maxAttempts)
	if err != nil || !ok {
		return err
	}
	_, _, err = repositories.Queue.UpsertActiveByDedupeOrGetExisting(ctx, queueRecord)
	return err
}

func InterruptRecoveryRun(ctx context.Context, repositories *storage.Repositories, run storage.RunRecord, loop storage.LoopRecord, nowISO string, message string) error {
	interrupted := run
	interrupted.Status = string(domain.RunStatusInterrupted)
	if interrupted.ErrorMessage == nil {
		interrupted.ErrorMessage = StringPtr(message)
	}
	interrupted.EndedAt = StringPtr(nowISO)
	interrupted.LastHeartbeatAt = StringPtr(nowISO)
	interrupted.UpdatedAt = nowISO
	if err := repositories.Runs.Upsert(ctx, interrupted); err != nil {
		return err
	}
	return AppendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:         NewRuntimeEventID(),
		EventType:  "looperd.recovery.run_interrupted",
		ProjectID:  StringPtr(loop.ProjectID),
		LoopID:     StringPtr(loop.ID),
		RunID:      StringPtr(run.ID),
		EntityType: StringPtr("run"),
		EntityID:   StringPtr(run.ID),
		PayloadJSON: MustMarshalJSON(map[string]any{
			"previousStatus":  "running",
			"recoveredStatus": "interrupted",
		}),
		CreatedAt: nowISO,
	})
}

func BuildRecoveryQueueItem(loop storage.LoopRecord, nowISO string, maxAttempts int64) (storage.QueueItemRecord, bool, error) {
	queueType := domain.LoopType(loop.Type)
	if queueType != domain.LoopTypePlanner && queueType != domain.LoopTypeReviewer && queueType != domain.LoopTypeFixer && queueType != domain.LoopTypeWorker {
		return storage.QueueItemRecord{}, false, nil
	}

	projectID := loop.ProjectID
	loopID := loop.ID
	queueRecord := storage.QueueItemRecord{
		ID:          NewRuntimeEventID(),
		ProjectID:   &projectID,
		LoopID:      &loopID,
		Type:        loop.Type,
		TargetType:  loop.TargetType,
		TargetID:    strings.TrimSpace(DerefString(loop.TargetID)),
		Repo:        loop.Repo,
		PRNumber:    loop.PRNumber,
		Status:      "queued",
		AvailableAt: nowISO,
		Attempts:    0,
		MaxAttempts: maxAttempts,
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}

	switch queueType {
	case domain.LoopTypePlanner:
		repo := strings.TrimSpace(DerefString(loop.Repo))
		issueNumber, err := ParseIssueNumberFromTargetID(queueRecord.TargetID)
		if err != nil || repo == "" || loop.TargetType != string(domain.LoopTargetTypeIssue) {
			if err == nil {
				err = fmt.Errorf("planner loop requires repo and issue target")
			}
			return storage.QueueItemRecord{}, false, err
		}
		lockKey := storage.IssueLockKey(loop.ProjectID, repo, issueNumber)
		targetID := fmt.Sprintf("issue:%s:%d", repo, issueNumber)
		payload := map[string]any{"issueNumber": issueNumber}
		payloadJSON := MustMarshalJSON(payload)
		queueRecord.TargetType = string(domain.LoopTargetTypeIssue)
		queueRecord.TargetID = targetID
		queueRecord.Repo = &repo
		queueRecord.PRNumber = nil
		queueRecord.DedupeKey = fmt.Sprintf("planner:%s:%s:%s:%d", loop.ProjectID, loop.ID, repo, issueNumber)
		queueRecord.Priority = storage.QueuePriorityPlanner
		queueRecord.LockKey = &lockKey
		queueRecord.PayloadJSON = &payloadJSON
	case domain.LoopTypeReviewer:
		repo := strings.TrimSpace(DerefString(loop.Repo))
		if repo == "" || loop.PRNumber == nil || loop.TargetType != string(domain.LoopTargetTypePullRequest) {
			return storage.QueueItemRecord{}, false, fmt.Errorf("reviewer loop requires repo and pull request target")
		}
		prNumber := *loop.PRNumber
		lockKey := storage.PullRequestLockKey(loop.ProjectID, repo, prNumber)
		targetID := fmt.Sprintf("pr:%s:%d", repo, prNumber)
		queueRecord.TargetType = string(domain.LoopTargetTypePullRequest)
		queueRecord.TargetID = targetID
		queueRecord.Repo = &repo
		queueRecord.PRNumber = &prNumber
		queueRecord.DedupeKey = fmt.Sprintf("reviewer:%s:%s:%s:%d", loop.ProjectID, loop.ID, repo, prNumber)
		queueRecord.Priority = storage.QueuePriorityReviewer
		queueRecord.LockKey = &lockKey
	case domain.LoopTypeFixer:
		repo := strings.TrimSpace(DerefString(loop.Repo))
		if repo == "" || loop.PRNumber == nil || loop.TargetType != string(domain.LoopTargetTypePullRequest) {
			return storage.QueueItemRecord{}, false, fmt.Errorf("fixer loop requires repo and pull request target")
		}
		prNumber := *loop.PRNumber
		lockKey := storage.PullRequestLockKey(loop.ProjectID, repo, prNumber)
		targetID := fmt.Sprintf("pr:%s:%d", repo, prNumber)
		queueRecord.TargetType = string(domain.LoopTargetTypePullRequest)
		queueRecord.TargetID = targetID
		queueRecord.Repo = &repo
		queueRecord.PRNumber = &prNumber
		queueRecord.DedupeKey = fmt.Sprintf("fixer:%s", loop.ID)
		queueRecord.Priority = storage.QueuePriorityFixer
		queueRecord.LockKey = &lockKey
	case domain.LoopTypeWorker:
		payloadJSON := BuildRecoveryWorkerPayloadJSON(loop.MetadataJSON)
		if payloadJSON != nil {
			queueRecord.PayloadJSON = payloadJSON
		}
		queueRecord.Priority = storage.QueuePriorityWorker
		lockKey := fmt.Sprintf("worker:%s", loop.ID)
		queueRecord.DedupeKey = fmt.Sprintf("worker:%s", loop.ID)
		if loop.TargetType == string(domain.LoopTargetTypeIssue) {
			repo := strings.TrimSpace(DerefString(loop.Repo))
			issueNumber, err := ParseIssueNumberFromTargetID(queueRecord.TargetID)
			if err != nil || repo == "" {
				if err == nil {
					err = fmt.Errorf("worker loop requires repo and issue target")
				}
				return storage.QueueItemRecord{}, false, err
			}
			lockKey = storage.IssueLockKey(loop.ProjectID, repo, issueNumber)
			targetID := fmt.Sprintf("issue:%s:%d", repo, issueNumber)
			queueRecord.TargetType = string(domain.LoopTargetTypeIssue)
			queueRecord.TargetID = targetID
			queueRecord.Repo = &repo
			queueRecord.PRNumber = nil
			queueRecord.DedupeKey = fmt.Sprintf("worker:%s:%s:%d", loop.ProjectID, repo, issueNumber)
		} else if loop.TargetType == string(domain.LoopTargetTypePullRequest) {
			repo := strings.TrimSpace(DerefString(loop.Repo))
			if repo == "" || loop.PRNumber == nil {
				return storage.QueueItemRecord{}, false, fmt.Errorf("worker loop requires repo and prNumber")
			}
			prNumber := *loop.PRNumber
			lockKey = storage.PullRequestLockKey(loop.ProjectID, repo, prNumber)
			targetID := fmt.Sprintf("pr:%s:%d", repo, prNumber)
			queueRecord.TargetType = string(domain.LoopTargetTypePullRequest)
			queueRecord.TargetID = targetID
			queueRecord.Repo = &repo
			queueRecord.PRNumber = &prNumber
			queueRecord.DedupeKey = fmt.Sprintf("worker:%s:%s:%d", loop.ProjectID, repo, prNumber)
		}
		queueRecord.LockKey = &lockKey
	}

	return queueRecord, true, nil
}

func BuildRecoveryWorkerPayloadJSON(metadataJSON *string) *string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return nil
	}
	metadata := map[string]any{}
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil {
		return nil
	}
	workerMeta, ok := metadata["worker"].(map[string]any)
	if !ok || len(workerMeta) == 0 {
		return nil
	}
	encoded, err := json.Marshal(workerMeta)
	if err != nil {
		return nil
	}
	text := string(encoded)
	return &text
}

func ParseIssueNumberFromTargetID(targetID string) (int64, error) {
	parts := strings.Split(strings.TrimSpace(targetID), ":")
	if len(parts) != 3 || parts[0] != "issue" {
		return 0, fmt.Errorf("invalid issue target id %q", targetID)
	}
	var issueNumber int64
	if _, err := fmt.Sscanf(parts[2], "%d", &issueNumber); err != nil || issueNumber <= 0 {
		return 0, fmt.Errorf("invalid issue target id %q", targetID)
	}
	return issueNumber, nil
}

func FormatJavaScriptISOString(value time.Time) string {
	value = value.UTC()
	return fmt.Sprintf("%s.%03dZ", value.Format("2006-01-02T15:04:05"), value.Nanosecond()/int(time.Millisecond))
}

func AppendSystemEvent(ctx context.Context, repositories *storage.Repositories, record storage.EventLogRecord) error {
	if repositories == nil || repositories.Events == nil {
		return fmt.Errorf("events repository is not configured")
	}

	record.ActorType = StringPtr("system")
	record.ActorID = StringPtr("looperd")
	record.ActorDisplayName = StringPtr("looperd")
	return repositories.Events.Append(ctx, record)
}

func (r *Recovery) ExecutionMatchesProcess(ctx context.Context, execution storage.AgentExecutionRecord, pid int) (matches bool, running bool, err error) {
	processCommand, err := r.ReadProcessCommand(ctx, pid)
	if err != nil {
		return false, false, err
	}
	processCommand = strings.TrimSpace(processCommand)
	if processCommand == "" {
		return false, false, nil
	}
	expectedTokens, err := ExpectedExecutionCommandTokens(execution)
	if err != nil {
		return false, true, err
	}
	actualTokens := SplitProcessCommand(processCommand)
	return CommandPrefixMatches(expectedTokens, actualTokens), true, nil
}

func ExpectedExecutionCommandTokens(execution storage.AgentExecutionRecord) ([]string, error) {
	if execution.CommandJSON == nil || strings.TrimSpace(*execution.CommandJSON) == "" {
		return nil, fmt.Errorf("missing execution command metadata")
	}
	var payload struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(*execution.CommandJSON), &payload); err != nil {
		return nil, fmt.Errorf("parse execution command metadata: %w", err)
	}
	if strings.TrimSpace(payload.Command) == "" {
		return nil, fmt.Errorf("missing execution command")
	}
	tokens := make([]string, 0, len(payload.Args)+1)
	tokens = append(tokens, payload.Command)
	tokens = append(tokens, payload.Args...)
	return tokens, nil
}

func CommandPrefixMatches(expected, actual []string) bool {
	if len(expected) == 0 || len(actual) == 0 {
		return false
	}
	if filepath.Base(expected[0]) != filepath.Base(actual[0]) {
		return false
	}
	if len(expected) == 1 {
		return true
	}
	if len(actual) < len(expected)-1 {
		return false
	}
	for i := 1; i < len(expected)-1; i++ {
		if !ProcessCommandTokenMatches(expected[i], actual[i]) {
			return false
		}
	}
	actualTail := strings.Join(actual[len(expected)-1:], " ")
	expectedTail := expected[len(expected)-1]
	return ProcessCommandTokenMatches(expectedTail, actualTail)
}

func ProcessCommandTokenMatches(expected, actual string) bool {
	return expected == actual
}

func SplitProcessCommand(command string) []string {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}

	tokens := make([]string, 0)
	var current strings.Builder
	var quote rune
	escaped := false

	flush := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, current.String())
		current.Reset()
	}

	for _, r := range command {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}

		switch {
		case r == '\\' && quote != 0:
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			current.WriteRune(r)
		}
	}

	flush()
	return tokens
}

func DefaultReadProcessCommand(ctx context.Context, pid int) (string, error) {
	cmd := exec.CommandContext(ctx, "ps", "-p", fmt.Sprintf("%d", pid), "-o", "command=")
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", nil
		}
		return "", fmt.Errorf("inspect process %d with ps: %w", pid, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func ParseRuntimeJSONObject(value *string) map[string]any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return map[string]any{}
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(*value), &decoded); err != nil || decoded == nil {
		return map[string]any{}
	}
	return decoded
}

func RuntimeReviewerLoopMetadata(meta map[string]any) map[string]any {
	loopMeta, _ := meta["loop"].(map[string]any)
	if loopMeta == nil {
		loopMeta = map[string]any{}
	}
	return loopMeta
}

func RuntimeStringFromAny(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok
}

func IsDeprecatedReviewerLoopBudgetReason(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "max_iterations_per_pr", "max_iterations_per_head", "max_wall_clock", "max_consecutive_failures", "max_agent_executions_per_pr":
		return true
	default:
		return false
	}
}

func RemoveDeprecatedReviewerLoopBudgetMetadata(loopMeta map[string]any) {
	for _, key := range DeprecatedReviewerLoopBudgetMetadataKeys {
		delete(loopMeta, key)
	}
	reason, _ := RuntimeStringFromAny(loopMeta["terminationReason"])
	if IsDeprecatedReviewerLoopBudgetReason(reason) {
		delete(loopMeta, "terminationReason")
		status, _ := RuntimeStringFromAny(loopMeta["status"])
		if status == "failed" || status == "terminated" {
			loopMeta["status"] = "active"
		}
	}
}

var DeprecatedReviewerLoopBudgetMetadataKeys = []string{
	"maxIterationsPerPR",
	"maxIterationsPerHead",
	"maxWallClockSeconds",
	"maxConsecutiveFailures",
	"maxAgentExecutionsPerPR",
}

func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func ShouldRequeueLoop(loop storage.LoopRecord, latestRun *storage.RunRecord, latestRunHasLiveAgent bool) bool {
	if TerminalReviewerRecoveryMetadataStatus(loop) != "" {
		return false
	}
	// Deliberately parked runs look interrupted too. Only an explicit resume,
	// human answer, or takeover handback may wake them.
	if loop.Status == "paused" || loop.Status == "awaiting_human" || loop.Status == string(domain.LoopStatusHumanTakeover) {
		return false
	}
	if loop.Status == "completed" || loop.Status == "failed" || loop.Status == "terminated" || loop.Status == "stopped" {
		return false
	}
	if latestRun == nil {
		return loop.Status == "running"
	}
	if latestRun.Status == string(domain.RunStatusRunning) && latestRunHasLiveAgent {
		return false
	}

	return loop.Status == "running" || latestRun.Status == "interrupted"
}

func TerminalReviewerRecoveryMetadataStatus(loop storage.LoopRecord) string {
	if loop.Type != string(domain.LoopTypeReviewer) || loop.MetadataJSON == nil || strings.TrimSpace(*loop.MetadataJSON) == "" {
		return ""
	}
	meta := ParseRuntimeJSONObject(loop.MetadataJSON)
	loopMeta := RuntimeReviewerLoopMetadata(meta)
	RemoveDeprecatedReviewerLoopBudgetMetadata(loopMeta)
	status, _ := RuntimeStringFromAny(loopMeta["status"])
	if status == "terminated" || status == "stopped" {
		return status
	}
	return ""
}

func NormalizeStaleQueuedLoopStatus(loop storage.LoopRecord, latestRun storage.RunRecord) string {
	if status := TerminalReviewerRecoveryMetadataStatus(loop); status != "" {
		return status
	}

	switch latestRun.Status {
	case "success":
		return "completed"
	case "interrupted", "running":
		return "interrupted"
	default:
		return "paused"
	}
}

func LatestQueueIsManualIntervention(queue *storage.QueueItemRecord) bool {
	return queue != nil && (queue.Status == "manual_intervention" || (queue.LastErrorKind != nil && *queue.LastErrorKind == "manual_intervention"))
}

func DerefRunOrZero(run *storage.RunRecord) storage.RunRecord {
	if run == nil {
		return storage.RunRecord{}
	}
	return *run
}

func MustMarshalJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func StringPtr(value string) *string {
	return &value
}

func CoalesceString(values ...*string) *string {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func StringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func NewRuntimeEventID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("runtime_%d", time.Now().UTC().UnixNano())
	}
	return "runtime_" + hex.EncodeToString(raw)
}

// ContainmentClass is the startup-recovery classification of durable execution
// evidence after a daemon restart (ADR-0015 R8 / #581).
//
// PID/PGID inspection is drift evidence only. It never authorizes live stop,
// terminal marking, requeue, or overlapping work, and never alone establishes
// confirmed-dead after restart.
type ContainmentClass string

const (
	// ContainmentConfirmedDead means Authority exists to treat the execution as
	// non-runnable for recovery purposes.
	//
	// After restart, only:
	//   - durable terminal finalization already committed before crash, or
	//   - a current-daemon owned processcontainment.Handle that has completed
	//     confirmed drain
	// may authorize this class.
	//
	// Must not authorize confirmed-dead: PID/PGID missing or not running,
	// probe-then-signal on raw PID/PGID, or leader exit alone without
	// descendant/containment proof.
	ContainmentConfirmedDead ContainmentClass = "confirmed_dead"

	// ContainmentObservedLive means a process probe matched the durable row.
	// This is evidence only — not adopted live ownership. Recovery must not
	// signal, terminalize, requeue, or start overlapping work from this class.
	ContainmentObservedLive ContainmentClass = "observed_live"

	// ContainmentUncertain covers every other observation (PID absent, command
	// mismatch, probe error, leader-exit-only without containment proof, etc.).
	// Uncertain work stays quarantined without raw PID/PGID action.
	ContainmentUncertain ContainmentClass = "uncertain"
)

// ContainmentClassification is one classified durable observation.
type ContainmentClassification struct {
	Class ContainmentClass
	// Reason is a stable machine-oriented explanation (event payloads / tests).
	Reason string
	// PID is the durable PID when present (evidence only).
	PID int
}

// DurableTerminalExecution reports whether SQLite already holds a terminal
// finalization for the row. Active statuses are never confirmed-dead by status.
func DurableTerminalExecution(status string) bool {
	switch strings.TrimSpace(status) {
	case "completed", "failed", "timeout", "killed", "success":
		return true
	default:
		return false
	}
}

// ClassifyFromDurableStatusAndHandle applies confirmed-dead Authority rules that
// do not depend on PID probes. currentDaemonHandle may be nil (always after a
// crash — pre-crash handles do not exist).
func ClassifyFromDurableStatusAndHandle(execution storage.AgentExecutionRecord, currentDaemonHandle *processcontainment.Handle) (ContainmentClassification, bool) {
	pid := 0
	if execution.PID != nil && *execution.PID > 0 {
		pid = int(*execution.PID)
	}
	if DurableTerminalExecution(execution.Status) {
		return ContainmentClassification{
			Class:  ContainmentConfirmedDead,
			Reason: "durable_terminal_finalization",
			PID:    pid,
		}, true
	}
	if currentDaemonHandle != nil && currentDaemonHandle.ConfirmedDead() {
		return ContainmentClassification{
			Class:  ContainmentConfirmedDead,
			Reason: "current_daemon_confirmed_drain",
			PID:    pid,
		}, true
	}
	return ContainmentClassification{}, false
}

// ClassifyStartupProbeEvidence maps PID probe outcomes to observed_live or
// uncertain. Never returns confirmed_dead — PID absence / leader exit alone
// cannot authorize that class after restart.
func ClassifyStartupProbeEvidence(pid int, matches, running bool, probeErr error) ContainmentClassification {
	if probeErr != nil {
		return ContainmentClassification{
			Class:  ContainmentUncertain,
			Reason: "process_probe_error",
			PID:    pid,
		}
	}
	if pid <= 0 {
		return ContainmentClassification{
			Class:  ContainmentUncertain,
			Reason: "pid_absent",
			PID:    0,
		}
	}
	if running && matches {
		return ContainmentClassification{
			Class:  ContainmentObservedLive,
			Reason: "process_identity_matched",
			PID:    pid,
		}
	}
	if running && !matches {
		return ContainmentClassification{
			Class:  ContainmentUncertain,
			Reason: "process_identity_mismatch",
			PID:    pid,
		}
	}
	// PID not running / empty process command. Leader exit or PID reuse absence
	// is not confirmed-dead Authority (descendants may remain; IDs are reusable).
	return ContainmentClassification{
		Class:  ContainmentUncertain,
		Reason: "pid_not_running_not_confirmed_dead",
		PID:    pid,
	}
}

// ClassifyStartupExecution classifies one durable agent_execution observation
// for startup recovery. PID probes are evidence only.
//
// currentDaemonHandle is optional: after crash it is always nil. Mid-life tests
// may inject a handle that has completed confirmed drain.
func (r *Recovery) ClassifyStartupExecution(ctx context.Context, execution storage.AgentExecutionRecord, currentDaemonHandle *processcontainment.Handle) (ContainmentClassification, error) {
	if class, ok := ClassifyFromDurableStatusAndHandle(execution, currentDaemonHandle); ok {
		return class, nil
	}
	pid := 0
	if execution.PID != nil && *execution.PID > 0 {
		pid = int(*execution.PID)
	}
	if pid <= 0 {
		return ClassifyStartupProbeEvidence(0, false, false, nil), nil
	}
	matches, running, err := r.ExecutionMatchesProcess(ctx, execution, pid)
	return ClassifyStartupProbeEvidence(pid, matches, running, err), nil
}

type RecoveryOrphanAgentCleanup struct {
	Attempted        bool  `json:"attempted"`
	CleanedCount     int64 `json:"cleanedCount"`
	QuarantinedCount int64 `json:"quarantinedCount"`
	// Classification counts for active execution evidence (ADR-0015 R8 / #581).
	// ConfirmedDead requires durable terminal finalization or current-daemon drain;
	// PID absence never increments ConfirmedDead.
	ConfirmedDeadCount int64  `json:"confirmedDeadCount"`
	ObservedLiveCount  int64  `json:"observedLiveCount"`
	UncertainCount     int64  `json:"uncertainCount"`
	Warning            string `json:"warning,omitempty"`
}

// OrphanRecovery is the existing startup summary and exclusion sets, not new
// durable state. Both hosts preserve the same quarantine decisions.
type OrphanRecovery struct {
	Cleanup            RecoveryOrphanAgentCleanup
	EventsWritten      int64
	UncertainRunIDs    map[string]struct{}
	QuarantinedLoopIDs map[string]struct{}
}

func (r *Recovery) RecoverOrphanExecutions(ctx context.Context, repositories *storage.Repositories, nowISO string) (OrphanRecovery, error) {
	result := OrphanRecovery{}
	result.Cleanup.Attempted = true
	result.UncertainRunIDs = make(map[string]struct{})
	result.QuarantinedLoopIDs = make(map[string]struct{})
	if repositories.AgentExecutions != nil {
		activeExecutions, err := repositories.AgentExecutions.ListActive(ctx)
		if err != nil {
			return OrphanRecovery{}, err
		}
		for _, execution := range activeExecutions {
			// ADR-0015 R8 / #581: classify containment. PID/PGID is drift
			// evidence only. Never SIGTERM/SIGKILL raw PID/PGID, never mark
			// terminal from PID absence/leader exit, never requeue from
			// observed_live or uncertain. Pre-crash handles do not exist after
			// restart (currentDaemonHandle = nil).
			classification, classErr := r.ClassifyStartupExecution(ctx, execution, nil)
			if classErr != nil {
				return OrphanRecovery{}, classErr
			}
			switch classification.Class {
			case ContainmentConfirmedDead:
				result.Cleanup.ConfirmedDeadCount += 1
			case ContainmentObservedLive:
				result.Cleanup.ObservedLiveCount += 1
			default:
				result.Cleanup.UncertainCount += 1
				classification.Class = ContainmentUncertain
			}

			wroteClass, classEventErr := r.AppendContainmentClassificationEvent(ctx, repositories, execution, classification, "orphan_cleanup", nowISO)
			if classEventErr != nil {
				return OrphanRecovery{}, classEventErr
			}
			if wroteClass {
				result.EventsWritten += 1
			}

			// Legacy process-identity uncertain events for probe mismatch/error
			// (kept for operator tooling that already keys on this event type).
			if classification.Reason == "process_probe_error" || classification.Reason == "process_identity_mismatch" {
				if r.Logger != nil && classification.Reason == "process_probe_error" {
					r.Logger.Warn("failed to verify orphan agent execution identity", map[string]any{
						"executionId": execution.ID, "pid": classification.PID, "class": string(classification.Class),
					})
				}
				written, appendErr := r.AppendUncertainProcessIdentityEvent(ctx, repositories, execution, classification.PID, "orphan_cleanup", nowISO)
				if appendErr != nil {
					return OrphanRecovery{}, appendErr
				}
				if written {
					result.EventsWritten += 1
				}
			}

			// Active ListActive rows cannot be confirmed-dead from durable
			// terminal status (they would not be active). After restart there is
			// no current-daemon handle. Quarantine observed_live and uncertain.
			if classification.Class != ContainmentConfirmedDead {
				if execution.RunID != nil && strings.TrimSpace(*execution.RunID) != "" {
					result.UncertainRunIDs[*execution.RunID] = struct{}{}
				}
				reason := "startup recovery: " + string(classification.Class) + " (" + classification.Reason + "); no PID Authority"
				quarantined, wrote, err := r.QuarantineRecoveryEvidence(ctx, repositories, execution, nowISO, reason)
				if err != nil {
					return OrphanRecovery{}, err
				}
				if quarantined {
					result.Cleanup.QuarantinedCount += 1
					if execution.LoopID != nil && strings.TrimSpace(*execution.LoopID) != "" {
						result.QuarantinedLoopIDs[*execution.LoopID] = struct{}{}
					}
				}
				if wrote {
					result.EventsWritten += 1
				}
			}
		}
		if result.Cleanup.QuarantinedCount > 0 {
			result.Cleanup.Warning = "active executions were quarantined without process kill; containment is not confirmed"
		}
	}

	return result, nil
}
