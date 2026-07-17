package runtime

import (
	"errors"
	"fmt"
	"sync"
)

// AdmissionState is the single authoritative live-daemon admission state
// (ADR-0015 R1 / issue #575). HTTP mutation readiness and scheduler work
// (full tick: discovery/HITL/claims/stale-reconcile) are projections of this
// state — not a second ready flag.
//
// Trade-off (AGENTS.md new-concept gate):
//
// Failure prevented: mid-rollout dual ready flags (ownershipAcquired vs HTTP/
// scheduler gates) that disagree, admitting mutations or enqueueing work while
// recovery is incomplete or shutdown has begun; recovery inventing cleanliness
// from reusable PIDs without a single closed admission Authority.
//
// Costs / new edge cases: sticky degraded until restart/clear; startup window
// where reads work but all mutations and work-producing ticks no-op; shutdown
// must BeginShutdown before storage close; every new work-producing path must
// call AllowMutations/AllowClaim (easy to miss → #580 audit); more
// manual_intervention quarantine instead of aggressive auto-clean.
//
// Why simpler alternatives are insufficient: a boolean ready flag next to
// ownershipAcquired re-creates dual Authority; gating only ClaimNext* leaves
// discovery/HITL/reconcile free to mutate queue storage while admission is
// closed; trusting SQLite or PID probes as live Authority lags and is not
// atomic with admission decisions.
//
// Legal transitions (monotonic / legal only):
//
//	starting  → ready | stopping | degraded
//	ready     → stopping | degraded
//	degraded  → stopping          (sticky until restart/clear; no ready)
//	stopping  → (none)            (terminal for this process lifetime)
//
// any → degraded is sticky until restart or an explicit ClearDegraded.
type AdmissionState string

const (
	AdmissionStarting AdmissionState = "starting"
	AdmissionReady    AdmissionState = "ready"
	AdmissionStopping AdmissionState = "stopping"
	AdmissionDegraded AdmissionState = "degraded"
)

// ErrAdmissionNotReady is returned when a mutation or queue claim is refused
// because admission is not ready.
var (
	ErrAdmissionNotReady    = errors.New("daemon admission is not ready")
	ErrAdmissionStopping    = errors.New("daemon admission is stopping")
	ErrAdmissionDegraded    = errors.New("daemon admission is degraded")
	ErrAdmissionIllegalMove = errors.New("illegal admission state transition")
)

// Admission is the single Authority for live daemon admission.
// All gates must call AllowMutations / AllowClaim under the same mutex as
// state reads so there is no check-then-act dual flag that can disagree.
// Deletion attempt: remove separate ownershipAcquired readiness and trust only
// agent/process signals — insufficient for multi-PR rollout because recovery
// and ingress need a process-lifetime closed gate before Supervisor ownership.
type Admission struct {
	mu     sync.Mutex
	state  AdmissionState
	reason string
}

// NewAdmission starts in starting; recovery/CompleteStartup must move it to ready.
func NewAdmission() *Admission {
	return &Admission{state: AdmissionStarting}
}

// State returns the current admission state.
func (a *Admission) State() AdmissionState {
	if a == nil {
		return AdmissionStopping
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// Reason returns the last transition reason (empty when unset).
func (a *Admission) Reason() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reason
}

// legalAdmissionTransition documents the allowed graph in code.
func legalAdmissionTransition(from, to AdmissionState) bool {
	if from == to {
		return true
	}
	switch from {
	case AdmissionStarting:
		return to == AdmissionReady || to == AdmissionStopping || to == AdmissionDegraded
	case AdmissionReady:
		return to == AdmissionStopping || to == AdmissionDegraded
	case AdmissionDegraded:
		// Sticky until restart/clear: only stopping (or explicit clear → ready).
		return to == AdmissionStopping
	case AdmissionStopping:
		return false
	default:
		return false
	}
}

// Transition applies a legal state change. Illegal moves return
// ErrAdmissionIllegalMove without changing state.
func (a *Admission) Transition(to AdmissionState, reason string) error {
	if a == nil {
		return ErrAdmissionStopping
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !legalAdmissionTransition(a.state, to) {
		return fmt.Errorf("%w: %s → %s", ErrAdmissionIllegalMove, a.state, to)
	}
	a.state = to
	if reason != "" {
		a.reason = reason
	}
	return nil
}

// MarkReady is starting → ready after CompleteStartup recovery finishes.
func (a *Admission) MarkReady(reason string) error {
	return a.Transition(AdmissionReady, reason)
}

// BeginShutdown is ready|starting|degraded → stopping. Idempotent when already stopping.
func (a *Admission) BeginShutdown(reason string) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == AdmissionStopping {
		if reason != "" {
			a.reason = reason
		}
		return nil
	}
	if !legalAdmissionTransition(a.state, AdmissionStopping) {
		return fmt.Errorf("%w: %s → %s", ErrAdmissionIllegalMove, a.state, AdmissionStopping)
	}
	a.state = AdmissionStopping
	if reason != "" {
		a.reason = reason
	}
	return nil
}

// MarkDegraded is sticky until restart or ClearDegraded.
func (a *Admission) MarkDegraded(reason string) error {
	return a.Transition(AdmissionDegraded, reason)
}

// ClearDegraded reopens admission after an operator/runtime-clear path
// (restart is the normal path; this exists for tests and future clear hooks).
func (a *Admission) ClearDegraded(reason string) error {
	if a == nil {
		return ErrAdmissionStopping
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != AdmissionDegraded {
		return fmt.Errorf("%w: clear degraded from %s", ErrAdmissionIllegalMove, a.state)
	}
	a.state = AdmissionReady
	if reason != "" {
		a.reason = reason
	}
	return nil
}

// AllowMutations is the atomic gate for HTTP mutating ingress. Callers must
// treat a nil error as admission to mutate; there is no separate ready flag.
func (a *Admission) AllowMutations() error {
	return a.allowWork()
}

// AllowClaim is the atomic gate for work-producing scheduler activity (full
// tick and each durable ClaimNext*). Same Authority as AllowMutations — a
// projection, not a second decision.
func (a *Admission) AllowClaim() error {
	return a.allowWork()
}

func (a *Admission) allowWork() error {
	if a == nil {
		return ErrAdmissionStopping
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch a.state {
	case AdmissionReady:
		return nil
	case AdmissionStopping:
		return ErrAdmissionStopping
	case AdmissionDegraded:
		return ErrAdmissionDegraded
	default:
		return ErrAdmissionNotReady
	}
}

// AllowsReads reports whether read-only HTTP may proceed. Reads remain
// available in starting, ready, stopping, and degraded.
func (a *Admission) AllowsReads() bool {
	return true
}

// IsReady is a projection helper for status surfaces. Prefer AllowMutations /
// AllowClaim for gates so state and decision cannot diverge.
func (a *Admission) IsReady() bool {
	return a.State() == AdmissionReady
}
