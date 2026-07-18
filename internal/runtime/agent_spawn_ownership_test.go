package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/processcontainment"
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

	if !strings.Contains(src, "Owner: activeExecutions") {
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
	if strings.Contains(src, "workerAgentExecutorAdapter struct {\n\texecutor *agent.ConfiguredExecutor\n\tregistry *ActiveExecutionRegistry") {
		t.Fatal("worker adapter must not hold registry for post-spawn ownership")
	}
	for _, role := range []string{"plannerAgentExecutorAdapter", "reviewerAgentExecutorAdapter", "fixerAgentExecutorAdapter", "workerAgentExecutorAdapter"} {
		if !strings.Contains(src, role) {
			t.Fatalf("missing role adapter %s — inventory coverage incomplete", role)
		}
	}
	// Coordinator triage uses the same shared agentExecutor (Owner wired once).
	if !strings.Contains(src, "NewAgentLLM(agentExecutor") {
		t.Fatal("coordinator triage must use the shared agentExecutor (Supervisor-owned)")
	}
}

func TestAdmitSpawnRefusesWhenAdmissionClosed(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.BeginShutdown("test stop")
	_, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{LoopID: "loop-1", RunID: "run-1", ExecutionID: "exec-1"})
	if !errors.Is(err, agent.ErrSpawnAdmissionClosed) {
		t.Fatalf("AdmitSpawn error = %v, want ErrSpawnAdmissionClosed", err)
	}
}

func TestAdmitSpawnRefusesWhenLoopStopping(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	release, err := reg.BeginLoopStop("loop-1", "stop")
	if err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	defer release()
	_, err = reg.AdmitSpawn(context.Background(), agent.SpawnMeta{LoopID: "loop-1", RunID: "run-1", ExecutionID: "exec-1"})
	if !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn error = %v, want ErrSpawnLoopStopping", err)
	}
}

func TestConcurrentStopAndSpawnLinearized(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	const n = 8
	var wg sync.WaitGroup
	var started atomic.Int32
	var rejected atomic.Int32
	var liveAfter atomic.Int32

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			meta := agent.SpawnMeta{
				LoopID: "loop-race", RunID: "run-race", ExecutionID: "exec-race-" + itoa(i),
			}
			lease, err := reg.AdmitSpawn(context.Background(), meta)
			if err != nil {
				rejected.Add(1)
				return
			}
			cmd := exec.Command("sleep", "30")
			processcontainment.Configure(cmd)
			if err := cmd.Start(); err != nil {
				lease.Release()
				t.Errorf("Start: %v", err)
				return
			}
			handle, err := processcontainment.Bind(cmd, processcontainment.Options{
				GracePeriod:  20 * time.Millisecond,
				DrainTimeout: 2 * time.Second,
			})
			if err != nil {
				_ = cmd.Process.Kill()
				lease.Release()
				t.Errorf("Bind: %v", err)
				return
			}
			if err := lease.BindHandle(handle, nil); err != nil {
				rejected.Add(1)
				if !handle.ConfirmedDead() {
					t.Errorf("rejected bind left process live")
				}
				return
			}
			started.Add(1)
			// Count live while holding ownership briefly.
			if reg.HasLiveHandle(meta.LoopID, meta.RunID, meta.ExecutionID) {
				liveAfter.Add(1)
			}
			lease.Release()
		}(i)
	}

	// Concurrently stop the loop mid-spawn.
	time.Sleep(5 * time.Millisecond)
	release, _ := reg.BeginLoopStop("loop-race", "halt")
	// Kill anything that made it into the registry.
	for i := 0; i < n; i++ {
		_, _ = reg.Kill("loop-race", "run-race", "exec-race-"+itoa(i), "halt")
	}
	release()
	wg.Wait()

	if started.Load()+rejected.Load() != n {
		t.Fatalf("started=%d rejected=%d want sum %d", started.Load(), rejected.Load(), n)
	}
	// No unowned live hangers: pending and live should drain.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if reg.LiveCount() == 0 && reg.PendingCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("after race LiveCount=%d PendingCount=%d", reg.LiveCount(), reg.PendingCount())
}

func writeSleepHelper(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sleep-helper")
	// Ignore args and sleep.
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	return path
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [16]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
