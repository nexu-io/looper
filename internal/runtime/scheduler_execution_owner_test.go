package runtime

import (
	"os"

	"path/filepath"
	"strings"

	"testing"
)

// Contract: every in-scope agent producer uses the common executor Owner, not
// a worker-only post-spawn registry path (#576; must not reintroduce #572).
func TestSchedulerWiresCommonExecutorOwnerNotWorkerOnlyRegistry(t *testing.T) {
	t.Parallel()

	schedulerSrc, err := os.ReadFile(filepath.Join("scheduler.go"))
	if err != nil {
		t.Fatalf("read scheduler.go: %v", err)
	}
	src := string(schedulerSrc)

	// gofmt may pad struct field alignment; ignore spaces for the wire check.
	if !strings.Contains(strings.ReplaceAll(src, " ", ""), "Owner:activeExecutions") {
		t.Fatal("scheduler must wire agent.ExecutorOptions.Owner = activeExecutions at common boundary")
	}
	// Post-spawn worker-only registration is the incomplete #572 approach.
	if strings.Contains(src, "registry.Register(") || strings.Contains(src, "a.registry.Register(") {
		t.Fatal("scheduler must not post-spawn Register agents in role adapters; ownership is at executor Start")
	}
	if strings.Contains(src, "registerActiveAgentExecution") {
		t.Fatal("registerActiveAgentExecution post-spawn helper must not return (#572 approach)")
	}
	// Worker adapter must not carry a registry field for post-spawn ownership.
	if strings.Contains(src, "workerhost.Executor struct {\n\texecutor *agent.ConfiguredExecutor\n\tregistry *ActiveExecutionRegistry") {
		t.Fatal("worker adapter must not hold registry for post-spawn ownership")
	}
	for _, role := range []string{"plannerAgentExecutorAdapter", "reviewerAgentExecutorAdapter", "fixerAgentExecutorAdapter", "workerhost.Executor"} {
		if !strings.Contains(src, role) {
			t.Fatalf("missing role adapter %s — inventory coverage incomplete", role)
		}
	}
	// Coordinator triage uses a Supervisor-owned executor (shared global or
	// multi-role globalExecutor); both must wire Owner: activeExecutions above.
	if !strings.Contains(src, "NewAgentLLM(agentExecutor") && !strings.Contains(src, "NewAgentLLM(globalExecutor") {
		t.Fatal("coordinator triage must use a Supervisor-owned agent executor")
	}
}
