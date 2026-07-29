package coordinator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/eventlog"
)

type agentLLM struct {
	executor    *agent.ConfiguredExecutor
	now         func() time.Time
	timeout     time.Duration
	idleTimeout time.Duration
}

func NewAgentLLM(executor *agent.ConfiguredExecutor, now func() time.Time, timeout time.Duration, idleTimeout time.Duration) triage.LLM {
	if now == nil {
		now = time.Now
	}
	return agentLLM{executor: executor, now: now, timeout: timeout, idleTimeout: idleTimeout}
}

func (l agentLLM) Complete(ctx context.Context, req triage.Request) (string, error) {
	if l.executor == nil {
		return "", fmt.Errorf("coordinator triage executor is not configured")
	}
	workingDir := strings.TrimSpace(req.WorkingDirectory)
	if workingDir == "" {
		return "", fmt.Errorf("coordinator triage working directory is required")
	}
	execHandle, err := l.executor.Start(ctx, agent.RunInput{
		ExecutionID:      eventlog.NewEventID("coordtriage"),
		Prompt:           req.Prompt,
		WorkingDirectory: workingDir,
		Timeout:          l.timeout,
		HeartbeatTimeout: l.idleTimeout,
	})
	if err != nil {
		return "", err
	}
	result, err := execHandle.Wait(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(result.Stdout) == "" {
		return "", fmt.Errorf("coordinator triage returned empty stdout")
	}
	// opencode (--format json) wraps the reply in JSONL text events; the caller
	// json.Unmarshals this, so hand back the assistant text, not the raw blob. Other
	// vendors print the answer plainly and fall through unchanged.
	if text, ok := agent.OpenCodeAssistantText(result.Stdout); ok {
		return text, nil
	}
	return strings.TrimSpace(result.Stdout), nil
}
