package dispatch

import (
	"testing"
	"time"
)

func TestHumanGatedPlanByAllowedUserAppliesPlannerTrigger(t *testing.T) {
	t.Parallel()
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan}, Comments: []Comment{{ID: 41, Author: "octo", HasWriteAccess: true, Body: "/plan"}}}, testConfig(), time.Now())
	if action.TriggerLabel != "looper:plan" || action.AssignTo != "octocat" || action.ReactionCommentID != 41 || action.ReactionContent != ReactionSuccess {
		t.Fatalf("action = %#v, want planner dispatch with success reaction", action)
	}
}

func TestHumanGatedImplementAppliesWorkerTrigger(t *testing.T) {
	t.Parallel()
	action := Decide(Issue{Labels: []string{"triaged", DispatchImplement}, Comments: []Comment{{ID: 42, Author: "octo", HasWriteAccess: true, Body: "/implement"}}}, testConfig(), time.Now())
	if action.TriggerLabel != "looper:worker-ready" || action.ReactionContent != ReactionSuccess {
		t.Fatalf("action = %#v, want worker dispatch with success reaction", action)
	}
}

func TestHumanGatedPlanMidLineDoesNothing(t *testing.T) {
	t.Parallel()
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan}, Comments: []Comment{{ID: 43, Author: "octo", HasWriteAccess: true, Body: "please /plan this"}}}, testConfig(), time.Now())
	if !action.NoOp || action.ReactionCommentID != 0 || action.TriggerLabel != "" {
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
	if action.TriggerLabel != "looper:plan" || action.AssignTo != "octocat" || action.ReactionCommentID != 44 || action.ReactionContent != ReactionSuccess {
		t.Fatalf("action = %#v, want latest authorized command to dispatch", action)
	}
}

func TestHumanGatedTriggerAlreadyPresentIsIdempotent(t *testing.T) {
	t.Parallel()
	action := Decide(Issue{Labels: []string{"triaged", DispatchPlan, "looper:plan"}, Comments: []Comment{{ID: 45, Author: "octo", HasWriteAccess: true, Body: "/plan"}}}, testConfig(), time.Now())
	if !action.NoOp || action.TriggerLabel != "" || action.ReactionContent != ReactionSuccess || action.ReactionCommentID != 45 {
		t.Fatalf("action = %#v, want idempotent success ack", action)
	}
}

func TestHumanGatedMissingTriagedFails(t *testing.T) {
	t.Parallel()
	action := Decide(Issue{Labels: []string{DispatchPlan}, Comments: []Comment{{ID: 46, Author: "octo", HasWriteAccess: true, Body: "/plan"}}}, testConfig(), time.Now())
	if action.ReactionContent != ReactionFailure || action.FailureCommentBody == "" || action.TriggerLabel != "" {
		t.Fatalf("action = %#v, want failure reaction with comment", action)
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
		Mode:                ModeHumanGated,
		TriagedLabel:        "triaged",
		HoldLabel:           "looper:hold",
		AutonomousDelay:     30 * time.Minute,
		SlashCommands:       []string{"/plan", "/implement"},
		AssignTo:            "octocat",
		PlannerTriggerLabel: "looper:plan",
		WorkerTriggerLabel:  "looper:worker-ready",
	}
}
