package agent

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/processcontainment"
	"github.com/nexu-io/looper/internal/storage"
)

// trackingLease records BindHandle ownership and Release calls for Start failure paths.
type trackingLease struct {
	ctx      context.Context
	cancel   context.CancelFunc
	handle   *processcontainment.Handle
	released bool
}

func (l *trackingLease) Context() context.Context { return l.ctx }
func (l *trackingLease) BindHandle(handle *processcontainment.Handle, _ SoftKillFunc) error {
	l.handle = handle
	return nil
}
func (l *trackingLease) Release() {
	l.released = true
	if l.cancel != nil {
		l.cancel()
	}
}

type trackingOwner struct {
	lease *trackingLease
}

func (o *trackingOwner) AdmitSpawn(ctx context.Context, _ SpawnMeta) (SpawnLease, error) {
	leaseCtx, cancel := context.WithCancel(ctx)
	o.lease = &trackingLease{ctx: leaseCtx, cancel: cancel}
	return o.lease, nil
}

// When initial ownership persist fails and Kill confirms death, Start may Release
// the lease. When Kill cannot confirm death, Start must keep ownership.
func TestStartOwnershipAfterInitialPersistFailure(t *testing.T) {
	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	if err := coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() error = %v", err)
	}

	owner := &trackingOwner{}
	custom := config.AgentVendor("custom")
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: custom, Params: map[string]any{
			"command": "/bin/sh", "args": []any{"-c", "trap '' TERM; while true; do sleep 1; done"},
		}},
		Repos:             repos,
		ParamsOwnerVendor: &custom,
		Owner:             owner,
	})

	handle, err := executor.Start(context.Background(), RunInput{
		ExecutionID: "agent_initial_persist_keep_owner", WorkingDirectory: t.TempDir(), Prompt: "ignored",
		Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("Start() error = nil, want initial persistence failure")
	}
	if !errors.Is(err, ErrExecutionPersistence) {
		t.Fatalf("Start() error = %v, want ErrExecutionPersistence", err)
	}
	if handle != nil {
		t.Fatalf("Start() handle = %#v, want nil", handle)
	}
	if owner.lease == nil || owner.lease.handle == nil {
		t.Fatal("expected tracking lease with bound handle")
	}
	// Normal path: reap succeeds → ConfirmedDead and lease released.
	if !owner.lease.handle.ConfirmedDead() {
		t.Fatal("handle not ConfirmedDead after initial-persist failure reap")
	}
	if !owner.lease.released {
		t.Fatal("lease.Release not called after ConfirmedDead reap on Start failure")
	}
}

// releaseLease must not drop Supervisor ownership while containment is live.
func TestReleaseLeaseSkipsWhenHandleNotConfirmedDead(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	signalFail := errors.New("synthetic signal delivery failure")
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  10 * time.Millisecond,
		DrainTimeout: 100 * time.Millisecond,
		Signal: func(pid int, sig syscall.Signal) error {
			return signalFail
		},
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	lease := &trackingLease{}
	x := &execution{
		handle: handle,
		lease:  lease,
	}
	killCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	killErr := handle.Kill(killCtx)
	cancel()
	if killErr == nil {
		t.Fatal("Kill() error = nil, want not-confirmed failure with injected signal")
	}
	if handle.ConfirmedDead() {
		t.Fatal("ConfirmedDead() = true after failed Kill")
	}

	x.releaseLease()
	if lease.released {
		t.Fatal("releaseLease released ownership while handle not ConfirmedDead")
	}

	// Normal path still releases when there is no live handle.
	x.handle = nil
	x.releaseLease()
	if !lease.released {
		t.Fatal("releaseLease did not release when handle is nil")
	}
}

// reapOnOwnershipPersistFailure must join Kill errors into the return value.
func TestReapOnOwnershipPersistFailureJoinsKillError(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}
	signalFail := errors.New("synthetic signal delivery failure")
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{
		GracePeriod:  10 * time.Millisecond,
		DrainTimeout: 50 * time.Millisecond,
		Signal: func(pid int, sig syscall.Signal) error {
			return signalFail
		},
	})
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Bind: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendorCodex}})
	x := &execution{executor: executor, handle: handle}
	persistErr := errors.New("sqlite closed")
	got := x.reapOnOwnershipPersistFailure(cmd, 10*time.Millisecond, persistErr, "persist initial agent execution ownership")
	if !errors.Is(got, ErrExecutionPersistence) {
		t.Fatalf("error = %v, want ErrExecutionPersistence", got)
	}
	if !errors.Is(got, persistErr) {
		t.Fatalf("error = %v, want joined persist cause", got)
	}
	if !errors.Is(got, signalFail) && !errors.Is(got, processcontainment.ErrNotConfirmedDead) && !strings.Contains(got.Error(), "synthetic signal") {
		t.Fatalf("error = %v, want joined kill/not-confirmed drain failure", got)
	}
	if handle.ConfirmedDead() {
		t.Fatal("ConfirmedDead() = true after failed reap")
	}
}

// Competing terminal status must surface ErrAgentExecutionConflict, not success.
func TestPersistFinalSurfacesTerminalConflict(t *testing.T) {
	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendorCodex}, Repos: repos})
	x := &execution{
		executor:       executor,
		executionID:    "agent_terminal_conflict",
		input:          RunInput{WorkingDirectory: t.TempDir(), Prompt: "x"},
		startedAtISO:   "2026-07-18T00:00:00.000Z",
		maxOutputBytes: defaultMaxOutputBytes,
		process:        exec.Command("/bin/true"),
	}
	if err := x.persistStatus(context.Background(), "running", nil, nil, nil); err != nil {
		t.Fatalf("initial persistStatus error = %v", err)
	}
	if err := x.persistFinal("killed", Result{Status: "killed", HeartbeatCount: 1}, "stop", "2026-07-18T00:01:00.000Z"); err != nil {
		t.Fatalf("persistFinal(killed) error = %v", err)
	}
	x2 := &execution{
		executor:       executor,
		executionID:    "agent_terminal_conflict",
		input:          x.input,
		startedAtISO:   x.startedAtISO,
		maxOutputBytes: defaultMaxOutputBytes,
		process:        exec.Command("/bin/true"),
	}
	err := x2.persistFinal("completed", Result{Status: "completed", HeartbeatCount: 2}, "", "2026-07-18T00:02:00.000Z")
	if err == nil {
		t.Fatal("persistFinal(completed over killed) error = nil, want conflict")
	}
	if !errors.Is(err, storage.ErrAgentExecutionConflict) {
		t.Fatalf("persistFinal error = %v, want ErrAgentExecutionConflict", err)
	}
	if !x2.terminalPersisted {
		t.Fatal("terminalPersisted = false after conflict, want true so live writers stop")
	}
	if hard := x2.classifyPersistError(err); hard != nil {
		t.Fatalf("classifyPersistError(conflict) = %v, want nil (no sticky degrade)", hard)
	}
	got, getErr := repos.AgentExecutions.GetByID(context.Background(), x.executionID)
	if getErr != nil {
		t.Fatalf("GetByID error = %v", getErr)
	}
	if got == nil || got.Status != "killed" {
		t.Fatalf("durable status = %#v, want killed", got)
	}
}
