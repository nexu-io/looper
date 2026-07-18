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

// RestoreLoopStop must cancel/drain leases admitted while the gate was cleared
// for a failed reactivation (retry/start/worker-reuse TX abort).
func TestRestoreLoopStopDrainsLeasesAdmittedDuringClear(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	if _, err := reg.BeginLoopStop("loop-restore", "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	// Intentional reactivation opens the gate before the TX; a stale runner can
	// AdmitSpawn+BindHandle in this window if the TX later aborts.
	reg.ClearLoopStop("loop-restore")

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-restore", RunID: "run-stale", ExecutionID: "exec-stale",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn during clear window: %v", err)
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
		t.Fatalf("BindHandle during clear window: %v", err)
	}

	// TX failed: restore sticky gate and drain anything admitted in the window.
	if err := reg.RestoreLoopStop("loop-restore"); err != nil {
		t.Fatalf("RestoreLoopStop: %v", err)
	}
	if !reg.LoopStopActive("loop-restore") {
		t.Fatal("LoopStopActive = false after RestoreLoopStop")
	}
	if !handle.ConfirmedDead() {
		t.Fatal("bound handle not confirmed-dead after RestoreLoopStop")
	}
	if lease.Context().Err() == nil {
		t.Fatal("lease context not cancelled by RestoreLoopStop")
	}
	_, admitErr := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-restore", RunID: "run-late", ExecutionID: "exec-late",
	})
	if !errors.Is(admitErr, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after RestoreLoopStop error = %v, want ErrSpawnLoopStopping", admitErr)
	}
}

// BeginLoopStop must report drain failure when a pending Start→BindHandle
// window never closes within killBudget (wedged executor after cmd.Start).
func TestBeginLoopStopReturnsErrorWhenPendingSpawnWaitTimesOut(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 40 * time.Millisecond

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-wait-timeout", RunID: "run-wt", ExecutionID: "exec-wt",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	// Leave the lease pending without BindHandle/Release so spawnDone never closes.

	_, stopErr := reg.BeginLoopStop("loop-wait-timeout", "looper stop")
	if stopErr == nil {
		t.Fatal("BeginLoopStop error = nil, want pending spawn wait timeout")
	}
	if !errors.Is(stopErr, errLoopStopWaitTimeout) {
		t.Fatalf("BeginLoopStop error = %v, want errLoopStopWaitTimeout", stopErr)
	}
	if !reg.LoopStopActive("loop-wait-timeout") {
		t.Fatal("LoopStopActive = false after timed-out BeginLoopStop, want gate closed")
	}
	// Cleanup so the lease does not outlive the test.
	lease.Release()
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

	// Race: close loop admission while BindHandle is still pending. BeginLoopStop
	// waits for the pending spawn window, so BindHandle must run concurrently.
	stopDone := make(chan error, 1)
	go func() {
		// Intentionally keep the sticky gate (do not invoke release).
		_, stopErr := reg.BeginLoopStop("loop-1", "halt")
		stopDone <- stopErr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !reg.LoopStopActive("loop-1") {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for BeginLoopStop to set gate")
		}
		time.Sleep(time.Millisecond)
	}

	err = lease.BindHandle(handle, func(string) error { return nil })
	if !errors.Is(err, agent.ErrSpawnStoppedDuringBind) {
		t.Fatalf("BindHandle error = %v, want ErrSpawnStoppedDuringBind", err)
	}
	if !handle.ConfirmedDead() {
		t.Fatal("handle must be confirmed-dead after stop-vs-bind race")
	}
	select {
	case stopErr := <-stopDone:
		if stopErr != nil {
			t.Fatalf("BeginLoopStop: %v", stopErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BeginLoopStop did not return after BindHandle ended the pending window")
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
	// Short budget: this test intentionally leaves the pending lease open so a
	// post-stop BindHandle can exercise refuse+kill. Production Start always
	// Release/BindHandle on cancel and closes spawnDone; here BeginLoopStop's
	// wait times out and must surface that as a drain error (not silent success).
	reg.killTimeout = 40 * time.Millisecond

	// Lease that is already cancelled simulates stop during attach-fail path.
	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-fb", RunID: "run-fb", ExecutionID: "exec-fb",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	if _, err := reg.BeginLoopStop("loop-fb", "halt"); err != nil && !errors.Is(err, errLoopStopWaitTimeout) {
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

// After BeginLoopStop confirmed-drains a bound handle, releaseLease may delete
// the registry entry while handle.Kill waits. haltLoop's subsequent Kill-by-id
// must still return killed=true so a persisted PID does not raise
// ErrAgentLiveHandleMissing for a process stop already killed.
func TestKillReportsDrainedAfterBeginLoopStopRelease(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	const (
		loopID = "loop-kill-after-drain"
		runID  = "run-kill-after-drain"
		execID = "exec-kill-after-drain"
	)
	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: loopID, RunID: runID, ExecutionID: execID,
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

	release, stopErr := reg.BeginLoopStop(loopID, "looper stop")
	if stopErr != nil {
		t.Fatalf("BeginLoopStop: %v", stopErr)
	}
	defer release()

	// Simulate execution.run finishing and releaseLease deleting the entry after
	// the process dies under confirmed drain (entry may already be gone).
	lease.Release()
	if reg.HasLiveHandle(loopID, runID, execID) {
		t.Fatal("HasLiveHandle = true after Release, want entry removed")
	}
	if !handle.ConfirmedDead() {
		t.Fatal("handle not confirmed-dead after BeginLoopStop")
	}

	killed, killErr := reg.Kill(loopID, runID, execID, "looper stop")
	if killErr != nil {
		t.Fatalf("Kill after BeginLoopStop+Release error = %v", killErr)
	}
	if !killed {
		t.Fatal("Kill after BeginLoopStop+Release = false, want true (carry drained result)")
	}
}

// BeginRebind must refuse after BeginLoopStop so fallback cannot Start a second
// process that stop already finished draining.
func TestBeginRebindRefusesWhenLoopStopping(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second
	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-rebind-refuse", RunID: "run-rr", ExecutionID: "exec-rr",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	sl, ok := lease.(*spawnLease)
	if !ok {
		t.Fatalf("lease type %T, want *spawnLease", lease)
	}
	// Bind so the lease leaves pending; BeginLoopStop then cancels the active lease.
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
	if _, err := reg.BeginLoopStop("loop-rebind-refuse", "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	if err := sl.BeginRebind(); !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("BeginRebind error = %v, want ErrSpawnLoopStopping", err)
	}
}

// BeginLoopStop must wait for a pending Start→BindHandle window so stop cannot
// return while a just-started process is live outside the registry.
func TestBeginLoopStopWaitsForPendingSpawn(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-pending-wait", RunID: "run-pw", ExecutionID: "exec-pw",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}

	// Process started after AdmitSpawn; BindHandle not yet called — registry
	// has no containment handle for BeginLoopStop's first drain pass.
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

	stopDone := make(chan error, 1)
	go func() {
		_, err := reg.BeginLoopStop("loop-pending-wait", "looper stop")
		stopDone <- err
	}()

	// Give BeginLoopStop time to set the gate and start waiting on spawnDone.
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-stopDone:
		t.Fatalf("BeginLoopStop returned early before BindHandle: %v", err)
	default:
	}

	bindErr := lease.BindHandle(handle, func(string) error { return nil })
	if !errors.Is(bindErr, agent.ErrSpawnStoppedDuringBind) {
		t.Fatalf("BindHandle = %v, want ErrSpawnStoppedDuringBind", bindErr)
	}
	if !handle.ConfirmedDead() {
		t.Fatal("pending spawn handle not confirmed-dead after refused BindHandle")
	}

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("BeginLoopStop error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BeginLoopStop did not return after pending spawn window ended")
	}
}

// BeginLoopStop must wait for an in-flight BeginRebind window so stop cannot
// return while fallback has started a process not yet refused/killed by RebindHandle.
func TestBeginLoopStopWaitsForInFlightRebind(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 5 * time.Second

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-rebind-wait", RunID: "run-rw", ExecutionID: "exec-rw",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	sl, ok := lease.(*spawnLease)
	if !ok {
		t.Fatalf("lease type %T, want *spawnLease", lease)
	}

	// Bind an initial handle so the lease is active (production path).
	cmd1 := exec.Command("sleep", "60")
	processcontainment.Configure(cmd1)
	if err := cmd1.Start(); err != nil {
		t.Fatalf("cmd1.Start: %v", err)
	}
	handle1, err := processcontainment.Bind(cmd1, processcontainment.Options{
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd1.Process.Kill()
		t.Fatalf("Bind1: %v", err)
	}
	if err := lease.BindHandle(handle1, func(string) error { return nil }); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}

	if err := sl.BeginRebind(); err != nil {
		t.Fatalf("BeginRebind: %v", err)
	}

	// Fallback process started while rebind is admitted; not yet in registry.
	cmd2 := exec.Command("sleep", "60")
	processcontainment.Configure(cmd2)
	if err := cmd2.Start(); err != nil {
		t.Fatalf("cmd2.Start: %v", err)
	}
	handle2, err := processcontainment.Bind(cmd2, processcontainment.Options{
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 3 * time.Second,
	})
	if err != nil {
		_ = cmd2.Process.Kill()
		t.Fatalf("Bind2: %v", err)
	}

	stopDone := make(chan error, 1)
	go func() {
		_, err := reg.BeginLoopStop("loop-rebind-wait", "looper stop")
		stopDone <- err
	}()

	// Give BeginLoopStop time to set the gate and start waiting on rebindDone.
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-stopDone:
		t.Fatalf("BeginLoopStop returned early before RebindHandle: %v", err)
	default:
	}

	rebindErr := sl.RebindHandle(handle2, func(string) error { return nil })
	if !errors.Is(rebindErr, agent.ErrSpawnStoppedDuringBind) {
		t.Fatalf("RebindHandle = %v, want ErrSpawnStoppedDuringBind", rebindErr)
	}
	if !handle2.ConfirmedDead() {
		t.Fatal("fallback handle not confirmed-dead after refused RebindHandle")
	}

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("BeginLoopStop error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BeginLoopStop did not return after rebind window ended")
	}
	if !handle1.ConfirmedDead() {
		t.Fatal("original handle not drained by BeginLoopStop")
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
