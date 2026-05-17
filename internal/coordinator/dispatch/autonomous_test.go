package dispatch

import (
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/coordinator/depgraph"
)

func TestAutonomousGraceNotElapsedDoesNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	action := Decide(Issue{Number: 1, Labels: []string{"triaged", DispatchPlan}, TriagedAt: now.Add(-29 * time.Minute)}, autonomousConfig(), now, nil)
	if !action.NoOp || len(action.TriggerLabels) != 0 {
		t.Fatalf("action = %#v, want no-op", action)
	}
}

func TestAutonomousGraceElapsedAppliesTrigger(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	action := Decide(Issue{Number: 1, Labels: []string{"triaged", DispatchPlan}, TriagedAt: now.Add(-31 * time.Minute)}, autonomousConfig(), now, nil)
	if len(action.TriggerLabels) != 1 || action.TriggerLabels[0] != "looper:plan" || action.AssignTo != "octocat" {
		t.Fatalf("action = %#v, want autonomous planner dispatch", action)
	}
}

func TestAutonomousGraceElapsedAppliesTriggerWithSatisfiedBlockedByGraph(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	graph := dependencyGraph("acme/looper", 1, depgraph.IssueRef{Repo: "acme/looper", Number: 9}, depgraph.IssueState{State: "closed", StateReason: "completed"})
	action := Decide(Issue{Number: 1, Labels: []string{"triaged", DispatchPlan}, TriagedAt: now.Add(-31 * time.Minute)}, autonomousConfig(), now, graph)
	if len(action.TriggerLabels) != 1 || action.TriggerLabels[0] != "looper:plan" || action.AssignTo != "octocat" {
		t.Fatalf("action = %#v, want autonomous planner dispatch with satisfied graph", action)
	}
}

func TestAutonomousGraceElapsedAppliesAllPlannerTriggersWhenConfigured(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	cfg := autonomousConfig()
	cfg.PlannerTriggerLabels = []string{"looper:plan", "team:planner"}
	action := Decide(Issue{Number: 1, Labels: []string{"triaged", DispatchPlan}, TriagedAt: now.Add(-31 * time.Minute)}, cfg, now, nil)
	if len(action.TriggerLabels) != 2 || action.TriggerLabels[0] != "looper:plan" || action.TriggerLabels[1] != "team:planner" {
		t.Fatalf("action = %#v, want all planner triggers", action)
	}
}

func TestAutonomousDispatchRemovedDoesNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	action := Decide(Issue{Number: 1, Labels: []string{"triaged"}, TriagedAt: now.Add(-31 * time.Minute)}, autonomousConfig(), now, nil)
	if !action.NoOp {
		t.Fatalf("action = %#v, want no-op", action)
	}
}

func TestAutonomousHoldLabelVetoesDispatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	action := Decide(Issue{Number: 1, Labels: []string{"triaged", DispatchPlan, "looper:hold"}, TriagedAt: now.Add(-31 * time.Minute)}, autonomousConfig(), now, nil)
	if !action.NoOp {
		t.Fatalf("action = %#v, want no-op", action)
	}
}

func TestAutonomousTriggerAlreadyPresentVetoesDispatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	action := Decide(Issue{Number: 1, Labels: []string{"triaged", DispatchPlan, "looper:plan"}, TriagedAt: now.Add(-31 * time.Minute)}, autonomousConfig(), now, nil)
	if !action.NoOp {
		t.Fatalf("action = %#v, want no-op", action)
	}
}

func TestAutonomousUnsatisfiedBlockedByVetoesDispatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	graph := dependencyGraph("acme/looper", 1, depgraph.IssueRef{Repo: "acme/looper", Number: 9}, depgraph.IssueState{State: "open"})
	action := Decide(Issue{Number: 1, Labels: []string{"triaged", DispatchPlan}, TriagedAt: now.Add(-31 * time.Minute)}, autonomousConfig(), now, graph)
	if !action.NoOp || len(action.TriggerLabels) != 0 {
		t.Fatalf("action = %#v, want blocked_by veto", action)
	}
}

func TestAutonomousUnreachableBlockedByVetoesDispatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 15, 12, 0, 0, 0, time.UTC)
	graph := dependencyGraph("acme/looper", 1, depgraph.IssueRef{Repo: "acme/looper", Number: 9}, depgraph.IssueState{})
	action := Decide(Issue{Number: 1, Labels: []string{"triaged", DispatchPlan}, TriagedAt: now.Add(-31 * time.Minute)}, autonomousConfig(), now, graph)
	if !action.NoOp || len(action.TriggerLabels) != 0 {
		t.Fatalf("action = %#v, want unreachable blocked_by veto", action)
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

func dependencyGraph(repo string, issueNumber int64, blocker depgraph.IssueRef, blockerState depgraph.IssueState) *depgraph.DependencyGraph {
	tracked := []depgraph.IssueRef{{Repo: repo, Number: issueNumber}}
	snapshot := depgraph.Snapshot{
		BlockedBy: map[depgraph.IssueRef][]depgraph.IssueRef{{Repo: repo, Number: issueNumber}: {blocker}},
		Issues:    map[depgraph.IssueRef]depgraph.IssueState{},
	}
	if blockerState != (depgraph.IssueState{}) {
		snapshot.Issues[blocker] = blockerState
	} else {
		snapshot.Unreachable = []depgraph.IssueRef{blocker}
	}
	graph := depgraph.Build(tracked, snapshot)
	return &graph
}
