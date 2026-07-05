package domain

import "testing"

func TestAssertLoopTypeMatchesTarget(t *testing.T) {
	t.Parallel()

	if err := AssertLoopTypeMatchesTarget(LoopTypePlanner, LoopTarget{TargetType: LoopTargetTypeIssue, Repo: "acme/looper", IssueNumber: 42}); err != nil {
		t.Fatalf("AssertLoopTypeMatchesTarget(planner, issue) error = %v", err)
	}
	if err := AssertLoopTypeMatchesTarget(LoopTypeWorker, LoopTarget{TargetType: LoopTargetTypeIssue, Repo: "acme/looper", IssueNumber: 42}); err != nil {
		t.Fatalf("AssertLoopTypeMatchesTarget(worker, issue) error = %v", err)
	}
	if err := AssertLoopTypeMatchesTarget(LoopTypeReviewer, LoopTarget{TargetType: LoopTargetTypeProject, ProjectID: "project_1"}); err == nil {
		t.Fatal("AssertLoopTypeMatchesTarget(reviewer, project) error = nil, want failure")
	}
}

func TestAssertUniqueActiveLoopAllowsConcurrentProjectWorkers(t *testing.T) {
	t.Parallel()

	err := AssertUniqueActiveLoop([]LoopSummary{{
		ID:        "loop_1",
		ProjectID: "project_1",
		Type:      LoopTypeWorker,
		Status:    LoopStatusRunning,
		Target:    LoopTarget{TargetType: LoopTargetTypeProject, ProjectID: "project_1"},
	}}, LoopSummary{
		ID:        "loop_2",
		ProjectID: "project_1",
		Type:      LoopTypeWorker,
		Status:    LoopStatusQueued,
		Target:    LoopTarget{TargetType: LoopTargetTypeProject, ProjectID: "project_1"},
	})
	if err != nil {
		t.Fatalf("AssertUniqueActiveLoop() error = %v, want nil", err)
	}
}

func TestAssertUniqueActiveLoopRejectsConflict(t *testing.T) {
	t.Parallel()

	err := AssertUniqueActiveLoop([]LoopSummary{{
		ID:        "loop_1",
		ProjectID: "project_1",
		Type:      LoopTypeReviewer,
		Status:    LoopStatusRunning,
		Target:    LoopTarget{TargetType: LoopTargetTypePullRequest, Repo: "acme/looper", PRNumber: 42},
	}}, LoopSummary{
		ID:        "loop_2",
		ProjectID: "project_1",
		Type:      LoopTypeReviewer,
		Status:    LoopStatusQueued,
		Target:    LoopTarget{TargetType: LoopTargetTypePullRequest, Repo: "acme/looper", PRNumber: 42},
	})
	if err == nil {
		t.Fatal("AssertUniqueActiveLoop() error = nil, want failure")
	}
}

func TestAssertUniqueActiveLoopRejectsIssueWorkerConflict(t *testing.T) {
	t.Parallel()

	err := AssertUniqueActiveLoop([]LoopSummary{{
		ID:        "loop_1",
		ProjectID: "project_1",
		Type:      LoopTypeWorker,
		Status:    LoopStatusRunning,
		Target:    LoopTarget{TargetType: LoopTargetTypeIssue, Repo: "acme/looper", IssueNumber: 42},
	}}, LoopSummary{
		ID:        "loop_2",
		ProjectID: "project_1",
		Type:      LoopTypeWorker,
		Status:    LoopStatusQueued,
		Target:    LoopTarget{TargetType: LoopTargetTypeIssue, Repo: "acme/looper", IssueNumber: 42},
	})
	if err == nil {
		t.Fatal("AssertUniqueActiveLoop() error = nil, want failure")
	}
}

func TestAssertStatusTransitions(t *testing.T) {
	t.Parallel()

	if err := AssertLoopStatusTransition(LoopStatusIdle, LoopStatusQueued); err != nil {
		t.Fatalf("AssertLoopStatusTransition(idle, queued) error = %v", err)
	}
	if err := AssertLoopStatusTransition(LoopStatusQueued, LoopStatusCompleted); err == nil {
		t.Fatal("AssertLoopStatusTransition(queued, completed) error = nil, want failure")
	}
	if err := AssertLoopStatusTransition(LoopStatusRunning, LoopStatusAwaitingHuman); err != nil {
		t.Fatalf("AssertLoopStatusTransition(running, awaiting_human) error = %v", err)
	}
	if err := AssertLoopStatusTransition(LoopStatusAwaitingHuman, LoopStatusRunning); err != nil {
		t.Fatalf("AssertLoopStatusTransition(awaiting_human, running) error = %v", err)
	}
	if err := AssertLoopStatusTransition(LoopStatusCompleted, LoopStatusAwaitingHuman); err == nil {
		t.Fatal("AssertLoopStatusTransition(completed, awaiting_human) error = nil, want failure")
	}
	if err := AssertKnownLoopStatus(LoopStatusAwaitingHuman); err != nil {
		t.Fatalf("AssertKnownLoopStatus(awaiting_human) error = %v", err)
	}
	if !IsActiveLoopStatus(LoopStatusAwaitingHuman) {
		t.Fatal("IsActiveLoopStatus(awaiting_human) = false, want true")
	}
	// Human takeover: reachable from running / awaiting_human; hands back to queued.
	if err := AssertLoopStatusTransition(LoopStatusRunning, LoopStatusHumanTakeover); err != nil {
		t.Fatalf("AssertLoopStatusTransition(running, human_takeover) error = %v", err)
	}
	if err := AssertLoopStatusTransition(LoopStatusAwaitingHuman, LoopStatusHumanTakeover); err != nil {
		t.Fatalf("AssertLoopStatusTransition(awaiting_human, human_takeover) error = %v", err)
	}
	if err := AssertLoopStatusTransition(LoopStatusHumanTakeover, LoopStatusQueued); err != nil {
		t.Fatalf("AssertLoopStatusTransition(human_takeover, queued) error = %v", err)
	}
	if err := AssertLoopStatusTransition(LoopStatusQueued, LoopStatusHumanTakeover); err == nil {
		t.Fatal("AssertLoopStatusTransition(queued, human_takeover) error = nil, want failure")
	}
	if err := AssertKnownLoopStatus(LoopStatusHumanTakeover); err != nil {
		t.Fatalf("AssertKnownLoopStatus(human_takeover) error = %v", err)
	}
	if !IsActiveLoopStatus(LoopStatusHumanTakeover) {
		t.Fatal("IsActiveLoopStatus(human_takeover) = false, want true")
	}
	if err := AssertRunStatusTransition(RunStatusQueued, RunStatusRunning); err != nil {
		t.Fatalf("AssertRunStatusTransition(queued, running) error = %v", err)
	}
	if err := AssertRunStatusTransition(RunStatusSuccess, RunStatusFailed); err == nil {
		t.Fatal("AssertRunStatusTransition(success, failed) error = nil, want failure")
	}
}

func TestAssertStepBelongsToLoopType(t *testing.T) {
	t.Parallel()

	if err := AssertStepBelongsToLoopType(LoopTypeWorker, "execute"); err != nil {
		t.Fatalf("AssertStepBelongsToLoopType(worker, execute) error = %v", err)
	}
	if err := AssertStepBelongsToLoopType(LoopTypePlanner, "execute"); err == nil {
		t.Fatal("AssertStepBelongsToLoopType(planner, execute) error = nil, want failure")
	}
}
