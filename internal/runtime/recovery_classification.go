package runtime

import (
	"context"

	"github.com/nexu-io/looper/internal/executionhost"

	"github.com/nexu-io/looper/internal/processcontainment"
	"github.com/nexu-io/looper/internal/storage"
)

type ContainmentClass = executionhost.ContainmentClass

const (
	ContainmentConfirmedDead = executionhost.ContainmentConfirmedDead
	ContainmentObservedLive  = executionhost.ContainmentObservedLive
	ContainmentUncertain     = executionhost.ContainmentUncertain
)

type ContainmentClassification = executionhost.ContainmentClassification

func durableTerminalExecution(status string) bool {
	return executionhost.DurableTerminalExecution(status)
}

func classifyFromDurableStatusAndHandle(execution storage.AgentExecutionRecord, currentDaemonHandle *processcontainment.Handle) (ContainmentClassification, bool) {
	return executionhost.ClassifyFromDurableStatusAndHandle(execution, currentDaemonHandle)
}

func classifyStartupProbeEvidence(pid int, matches, running bool, probeErr error) ContainmentClassification {
	return executionhost.ClassifyStartupProbeEvidence(pid, matches, running, probeErr)
}

func (r *Runtime) classifyStartupExecution(ctx context.Context, execution storage.AgentExecutionRecord, currentDaemonHandle *processcontainment.Handle) (ContainmentClassification, error) {
	return r.recoveryHost().ClassifyStartupExecution(ctx, execution, currentDaemonHandle)
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
