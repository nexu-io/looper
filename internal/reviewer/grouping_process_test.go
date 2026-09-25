package reviewer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
)

type groupedProcessExecutor struct {
	inner     *agent.ConfiguredExecutor
	starts    []AgentRunInput
	execution agent.Execution
	readyPath string
	onReady   func()
}

func (e *groupedProcessExecutor) Start(ctx context.Context, input AgentRunInput) (AgentExecution, error) {
	e.starts = append(e.starts, input)
	execution, err := e.inner.Start(ctx, agent.RunInput{
		ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID,
		WorkingDirectory: input.WorkingDirectory, Prompt: input.Prompt, Metadata: input.Metadata,
		Timeout: input.Timeout, GracefulShutdown: 2 * time.Second, DisableNativeResume: true,
		Env: map[string]string{"GROUP_READY_PATH": e.readyPath},
	})
	if err != nil {
		return nil, err
	}
	e.execution = execution
	for {
		if _, err := os.Stat(e.readyPath); err == nil {
			if e.onReady != nil {
				e.onReady()
			}
			return groupedProcessExecution{execution}, nil
		}
		select {
		case <-ctx.Done():
			return groupedProcessExecution{execution}, nil
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type groupedProcessExecution struct{ agent.Execution }

func (e groupedProcessExecution) Wait(ctx context.Context) (AgentResult, error) {
	result, err := e.Execution.Wait(ctx)
	return AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr}, err
}

func TestGroupedReviewDrainsCanceledProcessBeforeWorktreeCheck(t *testing.T) {
	for _, mode := range []string{"deadline", "parent_cancel"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			runner, input, _, _ := groupedReviewLifecycleFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			readyPath := filepath.Join(t.TempDir(), "ready")
			vendor := config.AgentVendor("custom")
			script := `trap 'sleep 0.2; printf "late mutation\n" > late-write.txt; exit 0' TERM
printf ready > "$GROUP_READY_PATH"
while :; do sleep 0.05; done`
			executor := &groupedProcessExecutor{inner: agent.New(agent.ExecutorOptions{
				Config: agent.ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{"command": "/bin/sh", "args": []any{"-c", script}}},
				Repos:  runner.repos, ParamsOwnerVendor: &vendor,
			}), readyPath: readyPath}
			runner.agentExecutor = executor
			runner.agentTimeout = time.Second
			if mode == "parent_cancel" {
				executor.onReady = cancel
			}
			t.Cleanup(func() {
				if executor.execution != nil {
					_ = executor.execution.Kill("test cleanup")
					drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer drainCancel()
					_, _ = executor.execution.Wait(drainCtx)
				}
			})
			checkpoint, err := runner.executeStep(ctx, stepReview, input)
			if err == nil || !strings.Contains(err.Error(), "worktree is dirty") || len(executor.starts) != 1 || checkpoint.PendingReview != nil {
				t.Errorf("canceled process: err=%v, starts=%d; want late mutation detected after drain", err, len(executor.starts))
			}
			if _, err := os.Stat(filepath.Join(input.Checkpoint.Worktree.Path, "late-write.txt")); errors.Is(err, os.ErrNotExist) {
				t.Error("review returned before the process's termination handler wrote its file")
			}
			if len(executor.starts) == 1 {
				record, err := runner.repos.AgentExecutions.GetByID(context.Background(), executor.starts[0].ExecutionID)
				if err != nil || record == nil || record.EndedAt == nil {
					t.Errorf("execution not terminal before return: record=%+v, err=%v", record, err)
				}
			}
		})
	}
}
