package dispatch

import (
	"testing"
	"time"
)

func TestAutonomousGraceNotElapsedDoesNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan}, TriagedAt: now.Add(-29 * time.Minute)}, autonomousConfig(), now)
	if !action.NoOp || len(action.TriggerLabels) != 0 {
		t.Fatalf("action = %#v, want no-op", action)
	}
}

func TestAutonomousGraceElapsedAppliesTrigger(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan}, TriagedAt: now.Add(-31 * time.Minute)}, autonomousConfig(), now)
	if len(action.TriggerLabels) != 1 || action.TriggerLabels[0] != "looper:plan" || action.AssignTo != "octocat" {
		t.Fatalf("action = %#v, want autonomous planner dispatch", action)
	}
}

func TestAutonomousGraceElapsedAppliesAllPlannerTriggersWhenConfigured(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	cfg := autonomousConfig()
	cfg.PlannerTriggerLabels = []string{"looper:plan", "team:planner"}
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan}, TriagedAt: now.Add(-31 * time.Minute)}, cfg, now)
	if len(action.TriggerLabels) != 2 || action.TriggerLabels[0] != "looper:plan" || action.TriggerLabels[1] != "team:planner" {
		t.Fatalf("action = %#v, want all planner triggers", action)
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
		Mode:                 ModeAutonomous,
		TriagedLabel:         "triaged",
		HoldLabel:            "looper:hold",
		AutonomousDelay:      30 * time.Minute,
		AssignTo:             "octocat",
		PlannerTriggerLabels: []string{"looper:plan"},
		WorkerTriggerLabels:  []string{"looper:worker-ready"},
	}
}
