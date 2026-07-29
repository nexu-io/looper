package domain

import "testing"

func TestStatusPinsWorktree(t *testing.T) {
	// Actively-in-use statuses pin the worktree: GC must not reclaim it.
	pinned := []LoopStatus{
		LoopStatusRunning,
		LoopStatusQueued,
		LoopStatusShepherding,
		LoopStatusHumanTakeover,
	}
	for _, status := range pinned {
		if !StatusPinsWorktree(status) {
			t.Errorf("StatusPinsWorktree(%q) = false, want true (actively in use)", status)
		}
	}

	// Resting and terminal statuses do not pin: the branch is durable and the
	// worktree is recreated on resume, so GC may reclaim it.
	reclaimable := []LoopStatus{
		LoopStatusIdle,
		LoopStatusPaused,
		LoopStatusWaiting,
		LoopStatusFailed,
		LoopStatusInterrupted,
		LoopStatusAwaitingHuman,
		LoopStatusStopped,
		LoopStatusTerminated,
		LoopStatusCompleted,
	}
	for _, status := range reclaimable {
		if StatusPinsWorktree(status) {
			t.Errorf("StatusPinsWorktree(%q) = true, want false (resting/terminal, reclaimable)", status)
		}
	}

	// An unknown status must default to reclaimable, never accidentally pin.
	if StatusPinsWorktree(LoopStatus("some-future-status")) {
		t.Error("StatusPinsWorktree(unknown) = true, want false (default reclaimable)")
	}
}
