package dispatch

import (
	"testing"
	"time"
)

func TestHumanGatedPlanByAllowedUserAppliesPlannerTrigger(t *testing.T) {
	t.Parallel()
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan}, Comments: []Comment{{ID: 41, Author: "octo", HasWriteAccess: true, Body: "/plan"}}}, testConfig(), time.Now())
	if len(action.TriggerLabels) != 1 || action.TriggerLabels[0] != "looper:plan" || action.AssignTo != "octocat" || action.ReactionCommentID != 41 || action.ReactionContent != ReactionSuccess {
		t.Fatalf("action = %#v, want planner dispatch with success reaction", action)
	}
}

func TestHumanGatedImplementAppliesWorkerTrigger(t *testing.T) {
	t.Parallel()
	action := Decide(Issue{Labels: []string{"triaged", DispatchImplement}, Comments: []Comment{{ID: 42, Author: "octo", HasWriteAccess: true, Body: "/implement"}}}, testConfig(), time.Now())
	if len(action.TriggerLabels) != 1 || action.TriggerLabels[0] != "looper:worker-ready" || action.ReactionContent != ReactionSuccess {
		t.Fatalf("action = %#v, want worker dispatch with success reaction", action)
	}
}

func TestHumanGatedPlanMidLineDoesNothing(t *testing.T) {
	t.Parallel()
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan}, Comments: []Comment{{ID: 43, Author: "octo", HasWriteAccess: true, Body: "please /plan this"}}}, testConfig(), time.Now())
	if !action.NoOp || action.ReactionCommentID != 0 || len(action.TriggerLabels) != 0 {
		t.Fatalf("action = %#v, want no-op", action)
	}
}

func TestHumanGatedPlanFromNonAllowedUserDoesNothing(t *testing.T) {
	t.Parallel()
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan}, Comments: []Comment{{ID: 44, Author: "outsider", Body: "/plan"}}}, testConfig(), time.Now())
	if !action.NoOp || action.ReactionCommentID != 0 {
		t.Fatalf("action = %#v, want no-op", action)
	}
}

func TestHumanGatedSkipsNewerUnauthorizedCommandAttempt(t *testing.T) {
	t.Parallel()
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan}, Comments: []Comment{
		{ID: 44, Author: "octo", HasWriteAccess: true, Body: "/plan"},
		{ID: 45, Author: "outsider", Body: "/implement"},
	}}, testConfig(), time.Now())
	if len(action.TriggerLabels) != 1 || action.TriggerLabels[0] != "looper:plan" || action.AssignTo != "octocat" || action.ReactionCommentID != 44 || action.ReactionContent != ReactionSuccess {
		t.Fatalf("action = %#v, want latest authorized command to dispatch", action)
	}
}

func TestHumanGatedTriggerAlreadyPresentIsIdempotent(t *testing.T) {
	t.Parallel()
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan, "looper:plan"}, Comments: []Comment{{ID: 45, Author: "octo", HasWriteAccess: true, Body: "/plan"}}}, testConfig(), time.Now())
	if !action.NoOp || len(action.TriggerLabels) != 0 || action.ReactionContent != ReactionSuccess || action.ReactionCommentID != 45 {
		t.Fatalf("action = %#v, want idempotent success ack", action)
	}
}

func TestHumanGatedMissingTriagedFails(t *testing.T) {
	t.Parallel()
	action := Decide(Issue{Labels: []string{DispatchPlan}, Comments: []Comment{{ID: 46, Author: "octo", HasWriteAccess: true, Body: "/plan"}}}, testConfig(), time.Now())
	if action.ReactionContent != ReactionFailure || action.FailureCommentBody == "" || len(action.TriggerLabels) != 0 {
		t.Fatalf("action = %#v, want failure reaction with comment", action)
	}
}

func TestHumanGatedPlanAppliesAllPlannerTriggersWhenConfigured(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.PlannerTriggerLabels = []string{"looper:plan", "team:planner"}
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan}, Comments: []Comment{{ID: 48, Author: "octo", HasWriteAccess: true, Body: "/plan"}}}, cfg, time.Now())
	if len(action.TriggerLabels) != 2 || action.TriggerLabels[0] != "looper:plan" || action.TriggerLabels[1] != "team:planner" {
		t.Fatalf("action = %#v, want all planner triggers", action)
	}
}

func TestHumanGatedMissingDispatchFails(t *testing.T) {
	t.Parallel()
	action := Decide(Issue{Labels: []string{"triaged"}, Comments: []Comment{{ID: 47, Author: "octo", HasWriteAccess: true, Body: "/plan"}}}, testConfig(), time.Now())
	if action.ReactionContent != ReactionFailure || action.FailureCommentBody == "" {
		t.Fatalf("action = %#v, want failure reaction with comment", action)
	}
}

func testConfig() Config {
	return Config{
		Mode:                 ModeHumanGated,
		TriagedLabel:         "triaged",
		HoldLabel:            "looper:hold",
		AutonomousDelay:      30 * time.Minute,
		SlashCommands:        []string{"/plan", "/implement"},
		AssignTo:             "octocat",
		PlannerTriggerLabels: []string{"looper:plan"},
		WorkerTriggerLabels:  []string{"looper:worker-ready"},
	}
}
