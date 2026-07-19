package runtime

import (
	"errors"
	"testing"
)

// Contract: hard agent_executions persistence failure closes admission via the
// single sticky degraded state (ADR-0015 R5 / #578). Soft paths must not call
// ReportHardPersistFailure.
func TestHardPersistFailureDegradesAdmission(t *testing.T) {
	rt := New(Options{})
	if err := rt.admission.MarkReady("test ready"); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	if err := rt.AllowClaim(); err != nil {
		t.Fatalf("AllowClaim() before degrade error = %v", err)
	}

	hard := errors.New("sqlite disk I/O error")
	rt.activeExecutions.ReportHardPersistFailure(hard)

	if got := rt.AdmissionState(); got != AdmissionDegraded {
		t.Fatalf("AdmissionState() = %s, want degraded", got)
	}
	if err := rt.AllowClaim(); !errors.Is(err, ErrAdmissionDegraded) {
		t.Fatalf("AllowClaim() error = %v, want ErrAdmissionDegraded", err)
	}
	if err := rt.AllowMutations(); !errors.Is(err, ErrAdmissionDegraded) {
		t.Fatalf("AllowMutations() error = %v, want ErrAdmissionDegraded", err)
	}

	// Operator recovery is process restart only: degraded cannot reopen in-process
	// because MarkDegraded cancels work-producer contexts permanently.
	if err := rt.admission.MarkReady("illegal clear after storage repair"); !errors.Is(err, ErrAdmissionIllegalMove) {
		t.Fatalf("MarkReady() from degraded = %v, want illegal (restart-only recovery)", err)
	}
	if err := rt.AllowClaim(); !errors.Is(err, ErrAdmissionDegraded) {
		t.Fatalf("AllowClaim() after illegal reopen attempt = %v, want ErrAdmissionDegraded", err)
	}
}

func TestHardPersistFailureIsStickyOnce(t *testing.T) {
	rt := New(Options{})
	if err := rt.admission.MarkReady("test ready"); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	rt.activeExecutions.ReportHardPersistFailure(errors.New("first hard failure"))
	// Second report while already degraded must not panic or reopen.
	rt.activeExecutions.ReportHardPersistFailure(errors.New("second hard failure"))
	if got := rt.AdmissionState(); got != AdmissionDegraded {
		t.Fatalf("AdmissionState() = %s, want degraded", got)
	}
	if err := rt.admission.MarkReady("illegal reopen"); err == nil {
		t.Fatal("MarkReady() from degraded succeeded, want illegal move")
	}
}
