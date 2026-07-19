package runtime

import (
	"context"
	"strings"

	"github.com/nexu-io/looper/internal/processcontainment"
	"github.com/nexu-io/looper/internal/storage"
)

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

// durableTerminalExecution reports whether SQLite already holds a terminal
// finalization for the row. Active statuses are never confirmed-dead by status.
func durableTerminalExecution(status string) bool {
	switch strings.TrimSpace(status) {
	case "completed", "failed", "timeout", "killed", "success":
		return true
	default:
		return false
	}
}

// classifyFromDurableStatusAndHandle applies confirmed-dead Authority rules that
// do not depend on PID probes. currentDaemonHandle may be nil (always after a
// crash — pre-crash handles do not exist).
func classifyFromDurableStatusAndHandle(execution storage.AgentExecutionRecord, currentDaemonHandle *processcontainment.Handle) (ContainmentClassification, bool) {
	pid := 0
	if execution.PID != nil && *execution.PID > 0 {
		pid = int(*execution.PID)
	}
	if durableTerminalExecution(execution.Status) {
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

// classifyStartupProbeEvidence maps PID probe outcomes to observed_live or
// uncertain. Never returns confirmed_dead — PID absence / leader exit alone
// cannot authorize that class after restart.
func classifyStartupProbeEvidence(pid int, matches, running bool, probeErr error) ContainmentClassification {
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

// classifyStartupExecution classifies one durable agent_execution observation
// for startup recovery. PID probes are evidence only.
//
// currentDaemonHandle is optional: after crash it is always nil. Mid-life tests
// may inject a handle that has completed confirmed drain.
func (r *Runtime) classifyStartupExecution(ctx context.Context, execution storage.AgentExecutionRecord, currentDaemonHandle *processcontainment.Handle) (ContainmentClassification, error) {
	if class, ok := classifyFromDurableStatusAndHandle(execution, currentDaemonHandle); ok {
		return class, nil
	}
	pid := 0
	if execution.PID != nil && *execution.PID > 0 {
		pid = int(*execution.PID)
	}
	if pid <= 0 {
		return classifyStartupProbeEvidence(0, false, false, nil), nil
	}
	matches, running, err := r.executionMatchesProcess(ctx, execution, pid)
	return classifyStartupProbeEvidence(pid, matches, running, err), nil
}

// classificationAllowsTerminalOrRequeue is true only for confirmed-dead.
// Observed live and uncertain must not mark terminal, requeue, signal, or overlap.
func classificationAllowsTerminalOrRequeue(class ContainmentClass) bool {
	return class == ContainmentConfirmedDead
}

// classificationRequiresQuarantine is true when recovery must park work via
// existing manual_intervention / paused states without PID action.
func classificationRequiresQuarantine(class ContainmentClass) bool {
	return class == ContainmentObservedLive || class == ContainmentUncertain
}
