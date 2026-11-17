package processcontainment

import (
	"errors"
	"os/exec"
	"sync/atomic"
	"testing"
)

// TestStartCleansUpProcessGroupOnBindFailure covers the Start→Bind failure
// boundary: cmd.Start succeeded, Bind failed, the child must not outlive the
// caller without a Handle (orphaned from LiveTracker and request timeouts).
func TestStartCleansUpProcessGroupOnBindFailure(t *testing.T) {
	requireUnixProcessGroup(t)

	old := startBind
	startBind = func(cmd *exec.Cmd, opts Options) (*Handle, error) {
		return nil, errors.New("forced bind failure")
	}
	t.Cleanup(func() { startBind = old })

	cmd := exec.Command("/bin/sh", "-c", "sleep 60")
	handle, err := Start(cmd, Options{})
	if handle != nil {
		t.Fatalf("Start() handle = %#v, want nil on Bind failure", handle)
	}
	if err == nil {
		t.Fatal("Start() error = nil, want forced bind failure")
	}
	if cmd.Process == nil {
		t.Fatal("cmd.Process = nil after Start failure, want Process for pid check")
	}
	assertProcessDead(t, cmd.Process.Pid)
}

// TestStartTrackedCleansUpOnBindFailure ensures StartTracked ends the admit
// window without Track and that the orphaned process group is reaped when
// Bind fails after Start (same boundary as probes using StartTracked).
func TestStartTrackedCleansUpOnBindFailure(t *testing.T) {
	requireUnixProcessGroup(t)

	old := startBind
	startBind = func(cmd *exec.Cmd, opts Options) (*Handle, error) {
		return nil, errors.New("forced bind failure")
	}
	t.Cleanup(func() { startBind = old })

	tracker := &countingTracker{}
	cmd := exec.Command("/bin/sh", "-c", "sleep 60")
	handle, release, err := StartTracked(tracker, cmd, Options{})
	if handle != nil || release != nil {
		t.Fatalf("StartTracked() handle/release non-nil on Bind failure (handle=%v release_set=%v)", handle, release != nil)
	}
	if err == nil {
		t.Fatal("StartTracked() error = nil, want forced bind failure")
	}
	if tracker.begins.Load() != 1 {
		t.Fatalf("BeginTrack calls = %d, want 1", tracker.begins.Load())
	}
	if tracker.ends.Load() != 1 {
		t.Fatalf("end calls = %d, want 1 (admit window closed on failure)", tracker.ends.Load())
	}
	if tracker.tracks.Load() != 0 {
		t.Fatalf("Track calls = %d, want 0 on Bind failure", tracker.tracks.Load())
	}
	if cmd.Process == nil {
		t.Fatal("cmd.Process = nil after StartTracked failure")
	}
	assertProcessDead(t, cmd.Process.Pid)
}

type countingTracker struct {
	begins atomic.Int32
	ends   atomic.Int32
	tracks atomic.Int32
}

func (t *countingTracker) BeginTrack() (end func(), err error) {
	t.begins.Add(1)
	return func() { t.ends.Add(1) }, nil
}

func (t *countingTracker) Track(*Handle) (release func()) {
	t.tracks.Add(1)
	return func() {}
}

func (t *countingTracker) ReportDrainFailure(error) {}
