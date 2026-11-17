package executionhost

import (
	"context"
	"errors"
	"github.com/nexu-io/looper/internal/processcontainment"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Contract (PR #630): BeginTrack + Track refuse closes the Start→Track race
// where BeginShutdown snapshots an empty nonAgentHandles set and returns before
// Track registers a just-started process. Without the admit window, a short
// shutdown budget can exit while the probe/shell cleanup still runs.
func TestActiveExecutionRegistryBeginTrackStartTrackShutdownInterleaving(t *testing.T) {
	t.Parallel()

	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 2 * time.Second

	// Hold the BeginTrack window open across Start, then interleave BeginShutdown
	// before Track — the exact gap the review called out.
	end, err := reg.BeginTrack()
	if err != nil {
		t.Fatalf("BeginTrack: %v", err)
	}

	cmd := exec.Command("sleep", "60")
	handle, err := processcontainment.Start(cmd, processcontainment.Options{
		GracePeriod:  50 * time.Millisecond,
		DrainTimeout: 2 * time.Second,
	})
	if err != nil {
		end()
		t.Fatalf("Start: %v", err)
	}

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- reg.BeginShutdown("interleave start/track")
	}()

	// Give BeginShutdown time to close admission and begin waiting on the admit slot.
	deadline := time.Now().Add(2 * time.Second)
	for {
		reg.mu.Lock()
		closed := reg.admissionClosed
		reg.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			end()
			_ = handle.Kill(context.Background())
			t.Fatal("timed out waiting for admissionClosed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Track after admission closed: must refuse-kill (not register a handle that
	// BeginShutdown already missed in its initial snapshot).
	release := reg.Track(handle)
	if release == nil {
		t.Fatal("Track returned nil release")
	}
	// end unblocks BeginShutdown's non-agent admit wait after refuse Kill.
	end()
	release()

	select {
	case err := <-shutdownDone:
		if err != nil {
			// Refuse-path kill failure would retain storage; success is ideal.
			t.Fatalf("BeginShutdown() error = %v, want nil after refuse-kill", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("BeginShutdown did not return; pending BeginTrack window likely not waited")
	}

	// Process group must be dead — otherwise we orphaned across shutdown.
	if err := syscall.Kill(handle.PID(), 0); err == nil {
		t.Fatalf("probe pid %d still live after refuse Track + BeginShutdown", handle.PID())
	} else if !errors.Is(err, syscall.ESRCH) {
		// ESRCH is confirmed gone; other errors are acceptable (e.g. EPERM on some hosts).
		t.Logf("Kill(0) after shutdown: %v (pid=%d)", err, handle.PID())
	}

	reg.mu.Lock()
	pending := len(reg.nonAgentPending)
	handles := len(reg.nonAgentHandles)
	reg.mu.Unlock()
	if pending != 0 {
		t.Fatalf("nonAgentPending = %d, want 0", pending)
	}
	if handles != 0 {
		t.Fatalf("nonAgentHandles = %d, want 0 after refuse Track", handles)
	}
}

// Contract (PR #630): BeginTrack after admission closed refuses Start entirely.
func TestActiveExecutionRegistryBeginTrackRefusesAfterShutdown(t *testing.T) {
	t.Parallel()
	reg := NewActiveExecutionRegistry()
	if err := reg.BeginShutdown("already stopping"); err != nil {
		t.Fatalf("BeginShutdown: %v", err)
	}
	end, err := reg.BeginTrack()
	if !errors.Is(err, processcontainment.ErrTrackerAdmissionClosed) {
		if end != nil {
			end()
		}
		t.Fatalf("BeginTrack() err = %v, want ErrTrackerAdmissionClosed", err)
	}
	if end != nil {
		t.Fatal("BeginTrack() end != nil on admission-closed error")
	}
}
