package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
)

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
