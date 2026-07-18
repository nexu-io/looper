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
	"github.com/nexu-io/looper/internal/config"
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

// Contract: durable stop must leave the per-loop gate closed after halt returns so
// in-flight runners that reach AgentExecutor.Start late cannot AdmitSpawn.
func TestBeginLoopStopStickyWithoutReleaseBlocksLateAdmitSpawn(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	// Simulate haltLoop: BeginLoopStop without invoking release.
	if _, err := reg.BeginLoopStop("loop-1", "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	if !reg.LoopStopActive("loop-1") {
		t.Fatal("LoopStopActive = false after BeginLoopStop without release")
	}
	_, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{LoopID: "loop-1", RunID: "run-late", ExecutionID: "exec-late"})
	if !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after sticky stop error = %v, want ErrSpawnLoopStopping", err)
	}
	// Intentional re-activation reopens admission.
	reg.ClearLoopStop("loop-1")
	if reg.LoopStopActive("loop-1") {
		t.Fatal("LoopStopActive = true after ClearLoopStop")
	}
	if _, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{LoopID: "loop-1", RunID: "run-resume", ExecutionID: "exec-resume"}); err != nil {
		t.Fatalf("AdmitSpawn after ClearLoopStop error = %v, want success", err)
	}
}

func TestStopVsBindKillsAndConfirmedDrainsBeforeStartSuccess(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-1", RunID: "run-1", ExecutionID: "exec-1",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}

	// Start a long-lived process group leader.
	cmd := exec.Command("sleep", "60")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}

	// Race: close loop admission before BindHandle returns.
	release, stopErr := reg.BeginLoopStop("loop-1", "halt")
	if stopErr != nil {
		t.Fatalf("BeginLoopStop: %v", stopErr)
	}
	defer release()

	err = lease.BindHandle(handle, func(string) error { return nil })
	if !errors.Is(err, agent.ErrSpawnStoppedDuringBind) {
		t.Fatalf("BindHandle error = %v, want ErrSpawnStoppedDuringBind", err)
	}
	if !handle.ConfirmedDead() {
		t.Fatal("handle must be confirmed-dead after stop-vs-bind race")
	}
	if reg.LiveCount() != 0 {
		t.Fatalf("LiveCount = %d, want 0 after rejected bind", reg.LiveCount())
	}
	if reg.PendingCount() != 0 {
		t.Fatalf("PendingCount = %d, want 0 after rejected bind", reg.PendingCount())
	}
}

func TestExecutorStartBindsHandleAndKillConfirmedDrains(t *testing.T) {
	// Not parallel: mutates PATH for the codex shim.
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	bin := writeSleepHelper(t)
	workdir := t.TempDir()
	executor := agent.New(agent.ExecutorOptions{
		Config: agent.ExecutorConfig{
			Vendor: config.AgentVendorCodex,
		},
		Owner: reg,
	})
	// Put a "codex" shim that sleeps on PATH.
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "codex")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec \""+bin+"\" \"$@\"\n"), 0o755); err != nil {
		t.Fatalf("write codex shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	execHandle, err := executor.Start(context.Background(), agent.RunInput{
		ExecutionID:      "exec-own-1",
		LoopID:           "loop-own-1",
		RunID:            "run-own-1",
		Prompt:           "work",
		WorkingDirectory: workdir,
		Timeout:          10 * time.Second,
		GracefulShutdown: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !reg.HasLiveHandle("loop-own-1", "run-own-1", "exec-own-1") {
		t.Fatal("registry must hold live handle after Start returns")
	}

	killed, err := reg.Kill("loop-own-1", "run-own-1", "exec-own-1", "stop test")
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !killed {
		t.Fatal("Kill returned killed=false")
	}

	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.Status != "killed" && result.Status != "timeout" && result.Status != "failed" {
		// killed is preferred; process may surface as failed if reaped by handle first
		t.Logf("result status = %q (acceptable after external kill)", result.Status)
	}
	// After release, live handle is gone.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !reg.HasLiveHandle("loop-own-1", "run-own-1", "exec-own-1") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("live handle still present after Wait")
}

func TestExecutorStartRefusesWhenOwnerClosed(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.BeginShutdown("shutdown")
	executor := agent.New(agent.ExecutorOptions{
		Config: agent.ExecutorConfig{Vendor: config.AgentVendorCodex},
		Owner:  reg,
	})
	_, err := executor.Start(context.Background(), agent.RunInput{
		ExecutionID:      "exec-closed",
		LoopID:           "loop-1",
		RunID:            "run-1",
		Prompt:           "work",
		WorkingDirectory: t.TempDir(),
		Timeout:          time.Second,
	})
	if !errors.Is(err, agent.ErrSpawnAdmissionClosed) {
		t.Fatalf("Start error = %v, want ErrSpawnAdmissionClosed", err)
	}
}

func TestNativeResumeFallbackCancelledDoesNotSpawnSecondProcess(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()

	// Lease that is already cancelled simulates stop during attach-fail path.
	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-fb", RunID: "run-fb", ExecutionID: "exec-fb",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	if _, err := reg.BeginLoopStop("loop-fb", "halt"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	// Wait until lease context is cancelled.
	select {
	case <-lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("lease context not cancelled")
	}

	// BindHandle of a live process during stop must kill, not leave unowned.
	cmd := exec.Command("sleep", "30")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  20 * time.Millisecond,
		DrainTimeout: 2 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	err = lease.BindHandle(handle, nil)
	if !errors.Is(err, agent.ErrSpawnStoppedDuringBind) {
		t.Fatalf("BindHandle = %v, want ErrSpawnStoppedDuringBind", err)
	}
	if !handle.ConfirmedDead() {
		t.Fatal("expected confirmed drain after cancelled bind")
	}
}

// Contract: after BindHandle succeeds the lease leaves pending; BeginLoopStop
// must still cancel that bound lease so executor native-resume fallback cannot
// start/rebind a second process after haltLoop drained the first handle.
// BeginLoopStop must also confirmed-drain the bound handle so haltLoop does not
// rely on a durable AgentExecutionRecord that may not exist yet
// (BindHandle→persistStatus window).
func TestBeginLoopStopCancelsBoundActiveLease(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-bound", RunID: "run-bound", ExecutionID: "exec-bound",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}

	cmd := exec.Command("sleep", "60")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	if err := lease.BindHandle(handle, func(string) error { return nil }); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}
	if reg.PendingCount() != 0 {
		t.Fatalf("PendingCount = %d after bind, want 0", reg.PendingCount())
	}
	if !reg.HasLiveHandle("loop-bound", "run-bound", "exec-bound") {
		t.Fatal("expected live handle after successful bind")
	}
	if lease.Context().Err() != nil {
		t.Fatalf("lease already cancelled before stop: %v", lease.Context().Err())
	}

	release, stopErr := reg.BeginLoopStop("loop-bound", "halt")
	if stopErr != nil {
		t.Fatalf("BeginLoopStop: %v", stopErr)
	}
	defer release()

	select {
	case <-lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("bound lease context not cancelled by BeginLoopStop")
	}
	// Fallback guard used by executor: cancelled lease blocks second spawn.
	if lease.Context().Err() == nil {
		t.Fatal("lease.Context().Err() = nil after BeginLoopStop, want cancelled")
	}

	// Confirmed drain of the post-BindHandle process (no separate Kill by ID).
	if !handle.ConfirmedDead() {
		t.Fatal("bound handle not confirmed-drained by BeginLoopStop")
	}
}

// BeginLoopStop must drain handles for the loop even when haltLoop cannot look
// up a durable execution id (registry-bound, not yet persisted).
func TestBeginLoopStopDrainsBoundHandleWithoutKillByID(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-pre-persist", RunID: "run-pre", ExecutionID: "exec-pre",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	cmd := exec.Command("sleep", "60")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	if err := lease.BindHandle(handle, func(string) error { return nil }); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}

	// Simulate haltLoop with no durable execution: only BeginLoopStop, no Kill(id).
	release, stopErr := reg.BeginLoopStop("loop-pre-persist", "looper stop")
	if stopErr != nil {
		t.Fatalf("BeginLoopStop: %v", stopErr)
	}
	defer release()

	if !handle.ConfirmedDead() {
		t.Fatal("BeginLoopStop did not confirmed-drain handle without Kill-by-id")
	}
}

// Drain failures from killOwned must surface so haltLoop cannot report stop
// success while a BindHandle→persistStatus process is unconfirmed/live.
func TestBeginLoopStopPropagatesDrainFailure(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-drain-fail", RunID: "run-df", ExecutionID: "exec-df",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	cmd := exec.Command("sleep", "60")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	softFail := errors.New("soft kill failed")
	if err := lease.BindHandle(handle, func(string) error { return softFail }); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}

	release, stopErr := reg.BeginLoopStop("loop-drain-fail", "looper stop")
	if release != nil {
		defer release()
	}
	if stopErr == nil {
		t.Fatal("BeginLoopStop error = nil, want soft-kill drain failure propagated")
	}
	if !errors.Is(stopErr, softFail) {
		t.Fatalf("BeginLoopStop error = %v, want softFail", stopErr)
	}
	// Handle drain still runs; soft failure alone must not hide gate/open release.
	if !handle.ConfirmedDead() {
		t.Fatal("handle not confirmed-dead after BeginLoopStop despite soft-kill error")
	}
	if !reg.LoopStopActive("loop-drain-fail") {
		t.Fatal("LoopStopActive = false after drain-failure BeginLoopStop, want gate closed")
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
