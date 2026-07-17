package runtime

import (
	"errors"
	"testing"
)

func TestAdmissionLegalTransitions(t *testing.T) {
	t.Parallel()

	a := NewAdmission()
	if got := a.State(); got != AdmissionStarting {
		t.Fatalf("State() = %q, want %q", got, AdmissionStarting)
	}
	if err := a.AllowMutations(); !errors.Is(err, ErrAdmissionNotReady) {
		t.Fatalf("AllowMutations() while starting = %v, want ErrAdmissionNotReady", err)
	}
	if err := a.AllowClaim(); !errors.Is(err, ErrAdmissionNotReady) {
		t.Fatalf("AllowClaim() while starting = %v, want ErrAdmissionNotReady", err)
	}

	if err := a.MarkReady("startup complete"); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	if err := a.AllowMutations(); err != nil {
		t.Fatalf("AllowMutations() while ready = %v, want nil", err)
	}
	if err := a.AllowClaim(); err != nil {
		t.Fatalf("AllowClaim() while ready = %v, want nil", err)
	}

	// ready → starting is illegal (monotonic).
	if err := a.Transition(AdmissionStarting, "rollback"); !errors.Is(err, ErrAdmissionIllegalMove) {
		t.Fatalf("Transition(starting) from ready = %v, want illegal", err)
	}
	if got := a.State(); got != AdmissionReady {
		t.Fatalf("State() after illegal move = %q, want still ready", got)
	}

	if err := a.MarkDegraded("persist failure"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}
	if err := a.AllowMutations(); !errors.Is(err, ErrAdmissionDegraded) {
		t.Fatalf("AllowMutations() while degraded = %v, want ErrAdmissionDegraded", err)
	}
	// degraded is sticky: cannot go back to ready via Transition.
	if err := a.Transition(AdmissionReady, "nope"); !errors.Is(err, ErrAdmissionIllegalMove) {
		t.Fatalf("Transition(ready) from degraded = %v, want illegal", err)
	}
	if err := a.ClearDegraded("operator clear"); err != nil {
		t.Fatalf("ClearDegraded() error = %v", err)
	}
	if err := a.AllowClaim(); err != nil {
		t.Fatalf("AllowClaim() after clear = %v, want nil", err)
	}

	if err := a.BeginShutdown("signal"); err != nil {
		t.Fatalf("BeginShutdown() error = %v", err)
	}
	if err := a.AllowMutations(); !errors.Is(err, ErrAdmissionStopping) {
		t.Fatalf("AllowMutations() while stopping = %v, want ErrAdmissionStopping", err)
	}
	// stopping is terminal for process lifetime.
	if err := a.MarkReady("late"); !errors.Is(err, ErrAdmissionIllegalMove) {
		t.Fatalf("MarkReady() while stopping = %v, want illegal", err)
	}
	// BeginShutdown is idempotent.
	if err := a.BeginShutdown("again"); err != nil {
		t.Fatalf("BeginShutdown() second call error = %v", err)
	}
}

func TestAdmissionStartingToDegradedAndStopping(t *testing.T) {
	t.Parallel()

	a := NewAdmission()
	if err := a.MarkDegraded("startup probe failed"); err != nil {
		t.Fatalf("MarkDegraded from starting error = %v", err)
	}
	if got := a.State(); got != AdmissionDegraded {
		t.Fatalf("State() = %q, want degraded", got)
	}

	b := NewAdmission()
	if err := b.BeginShutdown("early stop"); err != nil {
		t.Fatalf("BeginShutdown from starting error = %v", err)
	}
	if got := b.State(); got != AdmissionStopping {
		t.Fatalf("State() = %q, want stopping", got)
	}
}

func TestAdmissionMutationsAndClaimShareAuthority(t *testing.T) {
	t.Parallel()

	a := NewAdmission()
	// Single state authority: claim and mutations always agree.
	for _, state := range []struct {
		apply func() error
		name  string
	}{
		{func() error { return nil }, "starting"},
		{func() error { return a.MarkReady("ok") }, "ready"},
		{func() error { return a.MarkDegraded("d") }, "degraded"},
	} {
		if err := state.apply(); err != nil {
			t.Fatalf("%s apply error = %v", state.name, err)
		}
		mutErr := a.AllowMutations()
		claimErr := a.AllowClaim()
		if (mutErr == nil) != (claimErr == nil) {
			t.Fatalf("%s: mutations err=%v claim err=%v, want agreement", state.name, mutErr, claimErr)
		}
		if mutErr != nil && !errors.Is(mutErr, claimErr) && mutErr.Error() != claimErr.Error() {
			// Both non-nil should be the same sentinel family for the state.
			if a.State() == AdmissionDegraded {
				if !errors.Is(mutErr, ErrAdmissionDegraded) || !errors.Is(claimErr, ErrAdmissionDegraded) {
					t.Fatalf("%s: want both degraded, got mut=%v claim=%v", state.name, mutErr, claimErr)
				}
			}
		}
	}
}
