package domain

import "testing"

// Stage A (inert surface): the shepherding status must be a known, active,
// non-terminal status that management paths can transition out of, and the
// worker step list must carry the shepherd step. Nothing sets $.shepherd.active
// yet, so these assertions guard the additive surface only.

func TestShepherdingStatusIsKnownAndActive(t *testing.T) {
	if err := AssertKnownLoopStatus(LoopStatusShepherding); err != nil {
		t.Fatalf("AssertKnownLoopStatus(shepherding) error = %v, want nil", err)
	}
	if !IsActiveLoopStatus(LoopStatusShepherding) {
		t.Fatal("IsActiveLoopStatus(shepherding) = false, want true")
	}
	if _, ok := conflictingActiveLoopStatuses[LoopStatusShepherding]; !ok {
		t.Fatal("shepherding not in conflictingActiveLoopStatuses")
	}
}

func TestShepherdingStatusTransitions(t *testing.T) {
	// worker completes its impl PR then enters shepherding
	if err := AssertLoopStatusTransition(LoopStatusRunning, LoopStatusShepherding); err != nil {
		t.Fatalf("running->shepherding error = %v, want nil", err)
	}
	// management + reconciler moves out of shepherding
	for _, to := range []LoopStatus{
		LoopStatusShepherding, LoopStatusQueued, LoopStatusRunning, LoopStatusPaused,
		LoopStatusAwaitingHuman, LoopStatusCompleted, LoopStatusStopped, LoopStatusTerminated,
	} {
		if err := AssertLoopStatusTransition(LoopStatusShepherding, to); err != nil {
			t.Fatalf("shepherding->%s error = %v, want nil", to, err)
		}
	}
	// shepherding is not a terminal that resurrects into a fresh run
	if err := AssertLoopStatusTransition(LoopStatusShepherding, LoopStatusFailed); err == nil {
		t.Fatal("shepherding->failed error = nil, want failure (worker self-writes bypass the map)")
	}
}

func TestWorkerStepsCarryShepherd(t *testing.T) {
	found := false
	for _, s := range WorkerSteps {
		if s == "shepherd" {
			found = true
		}
	}
	if !found {
		t.Fatalf("WorkerSteps missing shepherd: %v", WorkerSteps)
	}
	if WorkerSteps[len(WorkerSteps)-1] != "shepherd" {
		t.Fatalf("shepherd must be last worker step, got %v", WorkerSteps)
	}
}
