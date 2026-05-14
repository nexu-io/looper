package dispatch

import (
	"testing"
	"time"
)

func TestAutonomousGraceNotElapsedDoesNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan}, TriagedAt: now.Add(-29 * time.Minute)}, autonomousConfig(), now)
	if !action.NoOp || action.TriggerLabel != "" {
		t.Fatalf("action = %#v, want no-op", action)
	}
}

func TestAutonomousGraceElapsedAppliesTrigger(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan}, TriagedAt: now.Add(-31 * time.Minute)}, autonomousConfig(), now)
	if action.TriggerLabel != "looper:plan" || action.AssignTo != "octocat" {
		t.Fatalf("action = %#v, want autonomous planner dispatch", action)
	}
}

func TestAutonomousDispatchRemovedDoesNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	action := Decide(Issue{Labels: []string{"triaged"}, TriagedAt: now.Add(-31 * time.Minute)}, autonomousConfig(), now)
	if !action.NoOp {
		t.Fatalf("action = %#v, want no-op", action)
	}
}

func TestAutonomousHoldLabelVetoesDispatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan, "looper:hold"}, TriagedAt: now.Add(-31 * time.Minute)}, autonomousConfig(), now)
	if !action.NoOp {
		t.Fatalf("action = %#v, want no-op", action)
	}
}

func TestAutonomousTriggerAlreadyPresentVetoesDispatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan, "looper:plan"}, TriagedAt: now.Add(-31 * time.Minute)}, autonomousConfig(), now)
	if !action.NoOp {
		t.Fatalf("action = %#v, want no-op", action)
	}
}

func autonomousConfig() Config {
	return Config{
		Mode:                ModeAutonomous,
		TriagedLabel:        "triaged",
		HoldLabel:           "looper:hold",
		AutonomousDelay:     30 * time.Minute,
		AssignTo:            "octocat",
		PlannerTriggerLabel: "looper:plan",
		WorkerTriggerLabel:  "looper:worker-ready",
	}
}
