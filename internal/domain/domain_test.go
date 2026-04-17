package domain

import "testing"

func TestAssertLoopTypeMatchesTarget(t *testing.T) {
	t.Parallel()

	if err := AssertLoopTypeMatchesTarget(LoopTypePlanner, LoopTarget{TargetType: LoopTargetTypeIssue, Repo: "acme/looper", IssueNumber: 42}); err != nil {
		t.Fatalf("AssertLoopTypeMatchesTarget(planner, issue) error = %v", err)
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

func TestAssertStatusTransitions(t *testing.T) {
	t.Parallel()

	if err := AssertLoopStatusTransition(LoopStatusIdle, LoopStatusQueued); err != nil {
		t.Fatalf("AssertLoopStatusTransition(idle, queued) error = %v", err)
	}
	if err := AssertLoopStatusTransition(LoopStatusQueued, LoopStatusCompleted); err == nil {
		t.Fatal("AssertLoopStatusTransition(queued, completed) error = nil, want failure")
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
