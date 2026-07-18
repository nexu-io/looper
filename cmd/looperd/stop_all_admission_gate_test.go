package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
)

// stopAll fallback after Pause failure must not leave a sticky loop stop gate:
// stopCandidateExecution closes admission only for the kill window, then releases.
func TestStopAllLoopsReleasesStopGateWhenPauseFails(t *testing.T) {
	ctx := context.Background()
	services, repos, now := newStopAllTestServices(t)
	// Terminal loop status cannot transition to paused → stopLoop fails before
	// durable pause, then stopAll falls back to stopCandidateExecution.
	insertStopAllTestLoop(t, ctx, repos, now, stopAllLoopFixture{
		loopID: "loop_failed_pause", seq: 1, loopType: "worker", loopStatus: "failed",
		runID: "run_failed_pause", runStatus: "running",
		executionID: "exec_failed_pause", executionStatus: "running", pid: 4201,
	})

	registry := looperdruntime.NewActiveExecutionRegistry()
	active := &fakeActiveExecution{}
	unregister := registry.Register("loop_failed_pause", "run_failed_pause", "exec_failed_pause", active)
	defer unregister()
	services.ActiveExecutions = registry

	response, err := stopAllLoops(ctx, services, "Stopped by test", func() time.Time { return now }, nil, nil)
	if err != nil {
		t.Fatalf("stopAllLoops() error = %v", err)
	}
	if response.Summary.Total != 1 {
		t.Fatalf("stopAllLoops() summary = %#v, want Total=1", response.Summary)
	}
	// Pause fails (failed → paused is invalid); fallback still kills the live
	// execution. Refresh may report alreadyStopping once status is cancelling.
	if !active.killed {
		t.Fatal("active execution Kill was not invoked on stop-all fallback")
	}
	storedLoop, err := repos.Loops.GetByID(ctx, "loop_failed_pause")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if storedLoop == nil || storedLoop.Status != "failed" {
		t.Fatalf("Loops.GetByID() = %#v, want still-failed (not paused) loop", storedLoop)
	}
	if registry.LoopStopActive("loop_failed_pause") {
		t.Fatal("LoopStopActive = true after stop-all Pause failure fallback, want gate released")
	}
	if _, err := registry.AdmitSpawn(ctx, agent.SpawnMeta{
		LoopID: "loop_failed_pause", RunID: "run_after_fallback", ExecutionID: "exec_after_fallback",
	}); err != nil {
		t.Fatalf("AdmitSpawn after stop-all Pause failure fallback error = %v, want success", err)
	}
}

// Successful stopAll must keep the sticky gate from haltLoop even though
// stopCandidateExecution releases its nested BeginLoopStop refcount.

// Successful stopAll must keep the sticky gate from haltLoop even though
// stopCandidateExecution releases its nested BeginLoopStop refcount.

// Successful stopAll must keep the sticky gate from haltLoop even though
// stopCandidateExecution releases its nested BeginLoopStop refcount.
func TestStopAllLoopsKeepsStopGateStickyAfterDurablePause(t *testing.T) {
	ctx := context.Background()
	services, repos, now := newStopAllTestServices(t)
	insertStopAllTestLoop(t, ctx, repos, now, stopAllLoopFixture{
		loopID: "loop_sticky", seq: 1, loopType: "worker", loopStatus: "running",
		runID: "run_sticky", runStatus: "running",
		executionID: "exec_sticky", executionStatus: "running", pid: 4202,
	})

	registry := looperdruntime.NewActiveExecutionRegistry()
	active := &fakeActiveExecution{}
	unregister := registry.Register("loop_sticky", "run_sticky", "exec_sticky", active)
	defer unregister()
	services.ActiveExecutions = registry

	response, err := stopAllLoops(ctx, services, "Stopped by test", func() time.Time { return now }, nil, nil)
	if err != nil {
		t.Fatalf("stopAllLoops() error = %v", err)
	}
	if response.Summary.Failed != 0 {
		t.Fatalf("stopAllLoops() summary = %#v, want no failures", response.Summary)
	}
	storedLoop, err := repos.Loops.GetByID(ctx, "loop_sticky")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if storedLoop == nil || storedLoop.Status != "paused" {
		t.Fatalf("Loops.GetByID() = %#v, want paused after durable stop", storedLoop)
	}
	if !registry.LoopStopActive("loop_sticky") {
		t.Fatal("LoopStopActive = false after durable stopAll, want sticky closed gate")
	}
	if _, err := registry.AdmitSpawn(ctx, agent.SpawnMeta{
		LoopID: "loop_sticky", RunID: "run_blocked", ExecutionID: "exec_blocked",
	}); !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after durable stopAll error = %v, want ErrSpawnLoopStopping", err)
	}
}
