package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/disclosure"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/network/protocol"
	"github.com/nexu-io/looper/internal/storage"
)

func TestDiscoverIssuesRespectsMaxPerTick(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Triage.MaxPerTick = 5
	for i := 1; i <= 50; i++ {
		fixture.github.issues = append(fixture.github.issues, githubinfra.IssueSummary{Number: int64(i), Labels: nil})
		fixture.github.details[int64(i)] = githubinfra.IssueDetail{Number: int64(i), Title: "Issue", Author: "octo", CreatedAt: fixture.now.Format(time.RFC3339)}
	}
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if got := countOperations(fixture.github.ops, "add:"); got != 10 {
		t.Fatalf("label add operations = %d, want 10 (two per issue for five issues)", got)
	}
	if got := countOperations(fixture.github.ops, "create-comment"); got != 5 {
		t.Fatalf("comment creates = %d, want 5", got)
	}
}

func TestRunnerAppliesLabelsThenCommentThenTriaged(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Format(time.RFC3339)}
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	want := []string{"add:kind/bug,area/coordinator,complexity/m,dispatch/plan", "create-comment", "add:triaged"}
	assertOrderedOps(t, fixture.github.ops, want)
	if body := fixture.github.createdBodies[0]; !containsAll(body, triageCommentMarker, "<!-- looper:stamp v=1 -->", "runner=coordinator") {
		t.Fatalf("comment body = %q, want coordinator marker and disclosure stamp", body)
	}
}

func TestRunnerEditsExistingMarkerComment(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Format(time.RFC3339)}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Format(time.RFC3339), Comments: []githubinfra.CommentInfo{{ID: 91, Author: "looper", Body: triageCommentMarker + "\n\nOld", CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 91, Author: "looper", Body: stampedCoordinatorBody(fixture.cfg, triageCommentMarker+"\n\nOld"), CreatedAt: fixture.now.Format(time.RFC3339)}}}
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(fixture.github.updatedBodies) != 1 || len(fixture.github.createdBodies) != 0 {
		t.Fatalf("updated=%d created=%d, want edit-in-place only", len(fixture.github.updatedBodies), len(fixture.github.createdBodies))
	}
}

func TestRunnerStaysSilentWhenHumanCommentsBeforePost(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Format(time.RFC3339)}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 77, Author: "human", Body: "I triaged this", CreatedAt: fixture.now.Add(time.Second).Format(time.RFC3339)}}}
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(fixture.github.createdBodies) != 0 || len(fixture.github.updatedBodies) != 0 {
		t.Fatal("runner posted or edited a comment after concurrent human triage")
	}
	assertOrderedOps(t, fixture.github.ops, []string{"add:kind/bug,area/coordinator,complexity/m,dispatch/plan", "add:triaged"})
	if countOperations(fixture.github.ops, "add:triaged") != 1 {
		t.Fatal("triaged label not applied")
	}
}

func TestRunnerStaysSilentWhenHumanCommentsInSameSecond(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.now = fixture.now.Add(500 * time.Millisecond)
	fixture.runner.now = func() time.Time { return fixture.now }
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Format(time.RFC3339Nano)}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 78, Author: "human", Body: "same-second update", CreatedAt: fixture.now.Truncate(time.Second).Format(time.RFC3339)}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	if len(fixture.github.createdBodies) != 0 || len(fixture.github.updatedBodies) != 0 {
		t.Fatal("runner posted or edited a comment after same-second human update")
	}
	assertOrderedOps(t, fixture.github.ops, []string{"add:kind/bug,area/coordinator,complexity/m,dispatch/plan", "add:triaged"})
	if countOperations(fixture.github.ops, "add:triaged") != 1 {
		t.Fatal("triaged label not applied")
	}
}

func TestRunnerReTriagesStaleClarifiedIssueInSamePass(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"needs-info", "triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number:    1,
		Title:     "Bug",
		Author:    "octo",
		CreatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339),
		Labels:    []string{"needs-info", "triaged"},
		Comments:  []githubinfra.CommentInfo{{ID: 77, Author: "octo", Body: "Added details", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339)}},
	}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 77, Author: "octo", Body: "Added details", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339)}}}
	fixture.github.timeline[1] = []map[string]any{{
		"event":      "labeled",
		"created_at": fixture.now.Add(-2 * time.Hour).Format(time.RFC3339),
		"label":      map[string]any{"name": "needs-info"},
	}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	assertOrderedOps(t, fixture.github.ops, []string{"remove:triaged", "add:kind/bug,area/coordinator,complexity/m,dispatch/plan", "create-comment", "add:triaged", "remove:needs-info"})
	if countOperations(fixture.github.ops, "remove:triaged") != 1 {
		t.Fatalf("remove:triaged count = %d, want 1", countOperations(fixture.github.ops, "remove:triaged"))
	}
	if countOperations(fixture.github.ops, "remove:needs-info") != 1 {
		t.Fatalf("remove:needs-info count = %d, want 1", countOperations(fixture.github.ops, "remove:needs-info"))
	}
	if countOperations(fixture.github.ops, "create-comment") != 1 {
		t.Fatal("create-comment count = 0, want 1")
	}
	if countOperations(fixture.github.ops, "add:triaged") != 1 {
		t.Fatal("triaged label was not re-added after successful re-triage")
	}
	if len(fixture.github.createdBodies) != 1 || !strings.Contains(fixture.github.createdBodies[0], "Looks actionable.") {
		t.Fatalf("createdBodies = %v, want retriage comment", fixture.github.createdBodies)
	}
}

func TestRunnerLeavesIssueUntriagedWhenReTriageCommentSkipped(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"needs-info", "triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number:    1,
		Title:     "Bug",
		Author:    "octo",
		CreatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339),
		Labels:    []string{"needs-info", "triaged"},
		Comments:  []githubinfra.CommentInfo{{ID: 77, Author: "octo", Body: "Added details", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339)}},
	}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{
		{ID: 77, Author: "octo", Body: "Added details", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339)},
		{ID: 78, Author: "human", Body: "hold on", CreatedAt: fixture.now.Add(time.Second).Format(time.RFC3339)},
	}}
	fixture.github.timeline[1] = []map[string]any{{
		"event":      "labeled",
		"created_at": fixture.now.Add(-2 * time.Hour).Format(time.RFC3339),
		"label":      map[string]any{"name": "needs-info"},
	}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	assertOrderedOps(t, fixture.github.ops, []string{"remove:triaged", "add:kind/bug,area/coordinator,complexity/m,dispatch/plan"})
	if countOperations(fixture.github.ops, "create-comment") != 0 {
		t.Fatal("comment should be skipped after concurrent human reply")
	}
	if countOperations(fixture.github.ops, "add:triaged") != 0 {
		t.Fatal("triaged label should stay cleared when re-triage comment is skipped")
	}
}

func TestRunnerIgnoresAlreadyLoadedSameSecondComment(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.now = fixture.now.Add(500 * time.Millisecond)
	fixture.runner.now = func() time.Time { return fixture.now }
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"needs-info", "triaged"}}}
	sameSecondComment := githubinfra.CommentInfo{ID: 77, Author: "octo", Body: "Added details", CreatedAt: fixture.now.Truncate(time.Second).Format(time.RFC3339)}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number:    1,
		Title:     "Bug",
		Author:    "octo",
		CreatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339),
		Labels:    []string{"needs-info", "triaged"},
		Comments:  []githubinfra.CommentInfo{sameSecondComment},
	}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{sameSecondComment}, {sameSecondComment}}
	fixture.github.timeline[1] = []map[string]any{{
		"event":      "labeled",
		"created_at": fixture.now.Add(-2 * time.Hour).Format(time.RFC3339),
		"label":      map[string]any{"name": "needs-info"},
	}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	assertOrderedOps(t, fixture.github.ops, []string{"remove:triaged", "add:kind/bug,area/coordinator,complexity/m,dispatch/plan", "create-comment", "add:triaged", "remove:needs-info"})
	if countOperations(fixture.github.ops, "create-comment") != 1 {
		t.Fatal("same-second loaded comment should not block retriage comment")
	}
	if countOperations(fixture.github.ops, "add:triaged") != 1 {
		t.Fatal("same-second loaded comment should still allow triaged label re-add")
	}
}

func TestRunnerKeepsNeedsInfoWhenReTriageTriagedWriteFails(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.github.failAddLabels = map[string]error{"triaged": errors.New("boom")}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"needs-info", "triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number:    1,
		Title:     "Bug",
		Author:    "octo",
		CreatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339),
		Labels:    []string{"needs-info", "triaged"},
		Comments:  []githubinfra.CommentInfo{{ID: 77, Author: "octo", Body: "Added details", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339)}},
	}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 77, Author: "octo", Body: "Added details", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339)}}}
	fixture.github.timeline[1] = []map[string]any{{
		"event":      "labeled",
		"created_at": fixture.now.Add(-2 * time.Hour).Format(time.RFC3339),
		"label":      map[string]any{"name": "needs-info"},
	}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err == nil {
		t.Fatal("DiscoverIssues() error = nil, want triaged write failure")
	}

	assertOrderedOps(t, fixture.github.ops, []string{"remove:triaged", "add:kind/bug,area/coordinator,complexity/m,dispatch/plan", "create-comment", "add:triaged"})
	if countOperations(fixture.github.ops, "remove:needs-info") != 0 {
		t.Fatal("needs-info should remain when re-triage triaged write fails")
	}
}

func TestRunnerKeepsNeedsInfoWhenReTriageStaysUnclear(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.triageLLM = stubUnclearCoordinatorLLM{}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"needs-info", "triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number:    1,
		Title:     "Bug",
		Author:    "octo",
		CreatedAt: fixture.now.Add(-8 * 24 * time.Hour).Format(time.RFC3339),
		Labels:    []string{"needs-info", "triaged"},
		Comments:  []githubinfra.CommentInfo{{ID: 77, Author: "octo", Body: "Added details", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339)}},
	}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 77, Author: "octo", Body: "Added details", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339)}}}
	fixture.github.timeline[1] = []map[string]any{{
		"event":      "labeled",
		"created_at": fixture.now.Add(-2 * time.Hour).Format(time.RFC3339),
		"label":      map[string]any{"name": "needs-info"},
	}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	assertOrderedOps(t, fixture.github.ops, []string{"remove:triaged", "add:needs-info", "create-comment", "add:triaged"})
	if countOperations(fixture.github.ops, "remove:needs-info") != 0 {
		t.Fatal("needs-info should remain when re-triage stays unclear")
	}
}

func TestRunnerProjectConfigRequiresConfig(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config = nil

	_, _, _, err := fixture.runner.projectConfig(context.Background(), fixture.projectID)
	if err == nil || !strings.Contains(err.Error(), "coordinator config is not configured") {
		t.Fatalf("projectConfig() error = %v, want missing config error", err)
	}
}

func TestLocalRepositoryInspectorStopsAfterContextCaps(t *testing.T) {
	t.Parallel()
	repoPath := t.TempDir()
	for i := 0; i < 20; i++ {
		name := filepath.Join(repoPath, "coordinator-token-file-"+strconv.Itoa(i)+".go")
		contents := []byte("package demo\n\nfunc coordinatorToken" + strconv.Itoa(i) + "() {}\n")
		if err := os.WriteFile(name, contents, 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}

	ctx, err := (localRepositoryInspector{}).Inspect(context.Background(), repoPath, triage.Issue{Title: "Coordinator token issue"})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if got := len(ctx.Paths); got != 12 {
		t.Fatalf("len(Paths) = %d, want 12", got)
	}
	if got := len(ctx.Symbols); got != 12 {
		t.Fatalf("len(Symbols) = %d, want 12", got)
	}
}

func TestRunnerHumanDispatchOrdersAssignLabelReact(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged", "dispatch/plan"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339), Labels: []string{"triaged", "dispatch/plan"}, Comments: []githubinfra.CommentInfo{{ID: 11, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 11, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.timeline[1] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	assertOrderedOps(t, fixture.github.ops, []string{"assign:octocat", "add:looper:plan", "react:+1:11"})
}

func TestRunnerDispatchFailureDedupesMarkedComment(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Add(-10 * 24 * time.Hour).Format(time.RFC3339), Comments: []githubinfra.CommentInfo{{ID: 12, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 12, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}, {ID: 99, Author: "looper", Body: stampedCoordinatorBody(fixture.cfg, dispatchFailureCommentMarker+"\n\nOld failure"), CreatedAt: fixture.now.Format(time.RFC3339)}}}
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(fixture.github.createdBodies) != 0 || len(fixture.github.updatedBodies) != 1 {
		t.Fatalf("created=%d updated=%d, want updated failure comment only", len(fixture.github.createdBodies), len(fixture.github.updatedBodies))
	}
	assertOrderedOps(t, fixture.github.ops, []string{"update-comment", "react:confused:12"})
}

func TestRunnerAutonomousDispatchAppliesConfiguredPlannerTrigger(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.runner.config.Roles.Planner.Triggers.Labels = []string{"my-custom-plan"}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged", "dispatch/plan"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339), Labels: []string{"triaged", "dispatch/plan"}}
	fixture.github.timeline[1] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	assertOrderedOps(t, fixture.github.ops, []string{"assign:octocat", "add:my-custom-plan"})
}

func TestRunnerAutonomousDispatchAppliesAllConfiguredPlannerTriggersWhenLabelModeAll(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.runner.config.Roles.Planner.Triggers.Labels = []string{"my-custom-plan", "team:planner"}
	fixture.runner.config.Roles.Planner.Triggers.LabelMode = config.LabelModeAll
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged", "dispatch/plan"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339), Labels: []string{"triaged", "dispatch/plan"}}
	fixture.github.timeline[1] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	assertOrderedOps(t, fixture.github.ops, []string{"assign:octocat", "add:my-custom-plan,team:planner"})
}

func TestRunnerLocalOnlyImplementAdmissionAddsWorkerReadyWithoutTargetLabel(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged", "dispatch/implement"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Build it", Author: "octo", URL: "https://github.com/acme/looper/issues/1", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339), Labels: []string{"triaged", "dispatch/implement"}}
	fixture.github.timeline[1] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	assertOrderedOps(t, fixture.github.ops, []string{"assign:octocat", "add:looper:worker-ready"})
	for _, op := range fixture.github.ops {
		if strings.Contains(op, "looper:target:") {
			t.Fatalf("ops = %v, want no target label in local-only mode", fixture.github.ops)
		}
	}
}

func TestRunnerPlanDispatchIgnoresStaleWorkerReadyLabel(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "human-gated"
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.runner.config.Roles.Planner.Triggers.Labels = []string{"my-custom-plan"}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged", "dispatch/plan", "looper:worker-ready"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number:    1,
		Title:     "Bug",
		Author:    "octo",
		CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339),
		Labels:    []string{"triaged", "dispatch/plan", "looper:worker-ready"},
		Comments:  []githubinfra.CommentInfo{{ID: 11, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}},
	}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 11, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.timeline[1] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	assertOrderedOps(t, fixture.github.ops, []string{"assign:octocat", "add:my-custom-plan", "react:+1:11"})
	if got := countAddedIssueOperations(fixture.github.addedLabels, 1, "my-custom-plan"); got != 1 {
		t.Fatalf("planner trigger add count = %d, want 1", got)
	}
	if got := countAddedIssueOperations(fixture.github.addedLabels, 1, "looper:worker-ready"); got != 0 {
		t.Fatalf("worker-ready add count = %d, want 0", got)
	}
}

func TestRunnerRoutedImplementAdmissionAssignsReadyThenExactTargetLast(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.cfg.Projects[0].Network = config.ProjectNetworkConfig{Mode: config.ProjectNetworkModeRouted}
	fixture.network.status = protocol.NodeStatusResponse{
		Membership: protocol.Membership{NodeID: "coord-1", NodeName: "coord-1", GitHub: protocol.GitHubIdentity{NumericID: 1, Login: "coord"}},
		Memberships: []protocol.Membership{
			{NodeID: "coord-1", NodeName: "coord-1", GitHub: protocol.GitHubIdentity{NumericID: 1, Login: "coord"}, Capabilities: protocol.NodeCapabilities{Roles: []string{"coordinator"}}},
			{NodeID: "worker-1", NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 101, Login: "worker-bot"}, TargetLabels: []string{protocol.TargetLabelForNode("worker-1")}, Capabilities: protocol.NodeCapabilities{Roles: []string{"worker"}, DynamicLoad: 1}, LastHeartbeatAt: timePtr(fixture.now)},
			{NodeID: "worker-2", NodeName: "worker-2", GitHub: protocol.GitHubIdentity{NumericID: 102, Login: "worker-bot-2"}, TargetLabels: []string{protocol.TargetLabelForNode("worker-2")}, Capabilities: protocol.NodeCapabilities{Roles: []string{"worker"}, DynamicLoad: 2}, LastHeartbeatAt: timePtr(fixture.now)},
		},
		Lease: protocol.CoordinatorLease{HolderNodeID: "coord-1", FencingToken: 44, ExpiresAt: timePtr(fixture.now.Add(time.Minute))},
	}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged", "dispatch/implement"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Build it", Author: "octo", URL: "https://github.com/acme/looper/issues/1", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339), Labels: []string{"triaged", "dispatch/implement"}}
	fixture.github.timeline[1] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	assertOrderedOps(t, fixture.github.ops, []string{"assign:worker-bot", "add:looper:worker-ready", "add:looper:target:worker-1"})
	if len(fixture.network.revalidateCalls) != 2 {
		t.Fatalf("revalidateCalls = %d, want 2", len(fixture.network.revalidateCalls))
	}
	for _, call := range fixture.network.revalidateCalls {
		if call.Method != "GET" || call.URL != "https://github.com/" || call.FencingToken != 44 {
			t.Fatalf("revalidate call = %#v, want GET repo host root with token 44", call)
		}
	}
}

func TestRunnerRoutedImplementAdmissionRepairsHumanWorkerReadyIntent(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.cfg.Projects[0].Network = config.ProjectNetworkConfig{Mode: config.ProjectNetworkModeRouted}
	fixture.network.status = protocol.NodeStatusResponse{
		Membership: protocol.Membership{NodeID: "coord-1", NodeName: "coord-1", GitHub: protocol.GitHubIdentity{NumericID: 1, Login: "coord"}},
		Memberships: []protocol.Membership{
			{NodeID: "coord-1", NodeName: "coord-1", GitHub: protocol.GitHubIdentity{NumericID: 1, Login: "coord"}, Capabilities: protocol.NodeCapabilities{Roles: []string{"coordinator"}}},
			{NodeID: "worker-1", NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 101, Login: "worker-bot"}, TargetLabels: []string{protocol.TargetLabelForNode("worker-1")}, Capabilities: protocol.NodeCapabilities{Roles: []string{"worker"}}, LastHeartbeatAt: timePtr(fixture.now)},
		},
		Lease: protocol.CoordinatorLease{HolderNodeID: "coord-1", FencingToken: 7, ExpiresAt: timePtr(fixture.now.Add(time.Minute))},
	}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 2, Labels: []string{"looper:worker-ready"}}}
	fixture.github.details[2] = githubinfra.IssueDetail{Number: 2, Title: "Human admitted", Author: "octo", URL: "https://github.com/acme/looper/issues/2", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339), Labels: []string{"looper:worker-ready"}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	assertOrderedOps(t, fixture.github.ops, []string{"assign:worker-bot", "add:looper:target:worker-1"})
}

func TestRunnerRoutedImplementAdmissionRetargetsIssueToSingleWorker(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.cfg.Projects[0].Network = config.ProjectNetworkConfig{Mode: config.ProjectNetworkModeRouted}
	fixture.network.status = protocol.NodeStatusResponse{
		Membership: protocol.Membership{NodeID: "coord-1", NodeName: "coord-1", GitHub: protocol.GitHubIdentity{NumericID: 1, Login: "coord"}},
		Memberships: []protocol.Membership{
			{NodeID: "coord-1", NodeName: "coord-1", GitHub: protocol.GitHubIdentity{NumericID: 1, Login: "coord"}, Capabilities: protocol.NodeCapabilities{Roles: []string{"coordinator"}}},
			{NodeID: "worker-1", NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 101, Login: "worker-bot"}, TargetLabels: []string{protocol.TargetLabelForNode("worker-1")}, Capabilities: protocol.NodeCapabilities{Roles: []string{"worker"}, DynamicLoad: 1}, LastHeartbeatAt: timePtr(fixture.now)},
			{NodeID: "worker-2", NodeName: "worker-2", GitHub: protocol.GitHubIdentity{NumericID: 102, Login: "worker-bot-2"}, TargetLabels: []string{protocol.TargetLabelForNode("worker-2")}, Capabilities: protocol.NodeCapabilities{Roles: []string{"worker"}, DynamicLoad: 2}, LastHeartbeatAt: timePtr(fixture.now)},
		},
		Lease: protocol.CoordinatorLease{HolderNodeID: "coord-1", FencingToken: 8, ExpiresAt: timePtr(fixture.now.Add(time.Minute))},
	}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 5, Labels: []string{"looper:worker-ready", protocol.TargetLabelForNode("worker-2")}}}
	fixture.github.details[5] = githubinfra.IssueDetail{Number: 5, Title: "Retarget me", Author: "octo", URL: "https://github.com/acme/looper/issues/5", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339), Labels: []string{"looper:worker-ready", protocol.TargetLabelForNode("worker-2")}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	assertOrderedOps(t, fixture.github.ops, []string{"assign:worker-bot", "remove:looper:target:worker-2", "add:looper:target:worker-1"})
	if got := countRemovedIssueOperations(fixture.github.removedLabels, 5, protocol.TargetLabelForNode("worker-2")); got != 1 {
		t.Fatalf("removed target label count = %d, want 1", got)
	}
}

func TestRunnerRoutedImplementAdmissionRemovesMixedCaseStaleTargetLabel(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.cfg.Projects[0].Network = config.ProjectNetworkConfig{Mode: config.ProjectNetworkModeRouted}
	fixture.network.status = protocol.NodeStatusResponse{
		Membership: protocol.Membership{NodeID: "coord-1", NodeName: "coord-1", GitHub: protocol.GitHubIdentity{NumericID: 1, Login: "coord"}},
		Memberships: []protocol.Membership{
			{NodeID: "coord-1", NodeName: "coord-1", GitHub: protocol.GitHubIdentity{NumericID: 1, Login: "coord"}, Capabilities: protocol.NodeCapabilities{Roles: []string{"coordinator"}}},
			{NodeID: "worker-1", NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 101, Login: "worker-bot"}, TargetLabels: []string{protocol.TargetLabelForNode("worker-1")}, Capabilities: protocol.NodeCapabilities{Roles: []string{"worker"}, DynamicLoad: 1}, LastHeartbeatAt: timePtr(fixture.now)},
			{NodeID: "worker-2", NodeName: "worker-2", GitHub: protocol.GitHubIdentity{NumericID: 102, Login: "worker-bot-2"}, TargetLabels: []string{protocol.TargetLabelForNode("worker-2")}, Capabilities: protocol.NodeCapabilities{Roles: []string{"worker"}, DynamicLoad: 2}, LastHeartbeatAt: timePtr(fixture.now)},
		},
		Lease: protocol.CoordinatorLease{HolderNodeID: "coord-1", FencingToken: 8, ExpiresAt: timePtr(fixture.now.Add(time.Minute))},
	}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 5, Labels: []string{"looper:worker-ready", "Looper:target:worker-2"}}}
	fixture.github.details[5] = githubinfra.IssueDetail{Number: 5, Title: "Retarget me", Author: "octo", URL: "https://github.com/acme/looper/issues/5", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339), Labels: []string{"looper:worker-ready", "Looper:target:worker-2"}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	assertOrderedOps(t, fixture.github.ops, []string{"assign:worker-bot", "remove:Looper:target:worker-2", "add:looper:target:worker-1"})
	if got := countRemovedIssueOperations(fixture.github.removedLabels, 5, "Looper:target:worker-2"); got != 1 {
		t.Fatalf("removed mixed-case target label count = %d, want 1", got)
	}
}

func TestRunnerRoutedImplementAdmissionSkipsDuplicateIdentityWorkers(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.cfg.Projects[0].Network = config.ProjectNetworkConfig{Mode: config.ProjectNetworkModeRouted}
	fixture.network.status = protocol.NodeStatusResponse{
		Membership: protocol.Membership{NodeID: "coord-1", NodeName: "coord-1", GitHub: protocol.GitHubIdentity{NumericID: 1, Login: "coord"}},
		Memberships: []protocol.Membership{
			{NodeID: "coord-1", NodeName: "coord-1", GitHub: protocol.GitHubIdentity{NumericID: 1, Login: "coord"}, Capabilities: protocol.NodeCapabilities{Roles: []string{"coordinator"}}},
			{NodeID: "worker-1", NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 101, Login: "worker-bot"}, TargetLabels: []string{protocol.TargetLabelForNode("worker-1")}, DuplicateWarning: true, Capabilities: protocol.NodeCapabilities{Roles: []string{"worker"}}, LastHeartbeatAt: timePtr(fixture.now)},
			{NodeID: "worker-2", NodeName: "worker-2", GitHub: protocol.GitHubIdentity{NumericID: 101, Login: "worker-bot"}, TargetLabels: []string{protocol.TargetLabelForNode("worker-2")}, DuplicateWarning: true, Capabilities: protocol.NodeCapabilities{Roles: []string{"worker"}}, LastHeartbeatAt: timePtr(fixture.now)},
		},
		Lease: protocol.CoordinatorLease{HolderNodeID: "coord-1", FencingToken: 9, ExpiresAt: timePtr(fixture.now.Add(time.Minute))},
	}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 3, Labels: []string{"triaged", "dispatch/implement"}}}
	fixture.github.details[3] = githubinfra.IssueDetail{Number: 3, Title: "Build it", Author: "octo", URL: "https://github.com/acme/looper/issues/3", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339), Labels: []string{"triaged", "dispatch/implement"}}
	fixture.github.timeline[3] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	if len(fixture.github.assigned) != 0 || len(fixture.github.addedLabels) != 0 {
		t.Fatalf("assigned=%v addedLabels=%v, want no GitHub admission mutations", fixture.github.assigned, fixture.github.addedLabels)
	}
	if len(fixture.github.createdBodies) != 1 || !strings.Contains(fixture.github.createdBodies[0], noEligibleNodeStatus) {
		t.Fatalf("createdBodies = %v, want no-eligible-node status comment", fixture.github.createdBodies)
	}
}

func TestRunnerRoutedImplementAdmissionStopsBeforeTargetLabelWhenLeaseLostMidSequence(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.cfg.Projects[0].Network = config.ProjectNetworkConfig{Mode: config.ProjectNetworkModeRouted}
	fixture.network.status = protocol.NodeStatusResponse{
		Membership: protocol.Membership{NodeID: "coord-1", NodeName: "coord-1", GitHub: protocol.GitHubIdentity{NumericID: 1, Login: "coord"}},
		Memberships: []protocol.Membership{
			{NodeID: "coord-1", NodeName: "coord-1", GitHub: protocol.GitHubIdentity{NumericID: 1, Login: "coord"}, Capabilities: protocol.NodeCapabilities{Roles: []string{"coordinator"}}},
			{NodeID: "worker-1", NodeName: "worker-1", GitHub: protocol.GitHubIdentity{NumericID: 101, Login: "worker-bot"}, TargetLabels: []string{protocol.TargetLabelForNode("worker-1")}, Capabilities: protocol.NodeCapabilities{Roles: []string{"worker"}}, LastHeartbeatAt: timePtr(fixture.now)},
		},
		Lease: protocol.CoordinatorLease{HolderNodeID: "coord-1", FencingToken: 11, ExpiresAt: timePtr(fixture.now.Add(time.Minute))},
	}
	fixture.network.revalidateErrs = []error{nil, errors.New("stale coordinator lease token; current token is 12")}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 4, Labels: []string{"triaged", "dispatch/implement"}}}
	fixture.github.details[4] = githubinfra.IssueDetail{Number: 4, Title: "Build it", Author: "octo", URL: "https://github.com/acme/looper/issues/4", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339), Labels: []string{"triaged", "dispatch/implement"}}
	fixture.github.timeline[4] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	assertOrderedOps(t, fixture.github.ops, []string{"assign:worker-bot", "add:looper:worker-ready"})
	for _, op := range fixture.github.ops {
		if strings.Contains(op, "looper:target:worker-1") {
			t.Fatalf("ops = %v, want no target label after lease loss", fixture.github.ops)
		}
	}
}

func TestRunnerDiscoverIssuesPropagatesRepositoryPermissionFailures(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.github.permissionErr = errors.New("permission lookup failed")
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged", "dispatch/plan"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339), Labels: []string{"triaged", "dispatch/plan"}, Comments: []githubinfra.CommentInfo{{ID: 11, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 11, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.timeline[1] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}

	_, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err == nil || !strings.Contains(err.Error(), "permission lookup failed") {
		t.Fatalf("DiscoverIssues() error = %v, want propagated repository permission failure", err)
	}
	if len(fixture.github.ops) != 0 {
		t.Fatalf("ops = %v, want no dispatch side effects after permission failure", fixture.github.ops)
	}
}

func TestRunnerCycleDetectionRemovesLabelsAndPostsOneComment(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	fixture.runner.config.Roles.Coordinator.PollInterval = "0s"
	seedDispatchIssue(fixture, 1)
	seedDispatchIssue(fixture, 2)
	fixture.github.blockedBy[1] = []githubinfra.DependencyIssue{{Number: 2, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "open"}}
	fixture.github.blockedBy[2] = []githubinfra.DependencyIssue{{Number: 1, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "open"}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	if got := countRemovedIssueOperations(fixture.github.removedLabels, 1, "triaged", "dispatch/plan"); got != 1 {
		t.Fatalf("issue 1 remove count = %d, want 1", got)
	}
	if got := countRemovedIssueOperations(fixture.github.removedLabels, 2, "triaged", "dispatch/plan"); got != 1 {
		t.Fatalf("issue 2 remove count = %d, want 1", got)
	}
	if len(fixture.github.createdBodies) != 2 {
		t.Fatalf("createdBodies = %d, want 2 cycle comments", len(fixture.github.createdBodies))
	}
	for _, body := range fixture.github.createdBodies {
		if !containsAll(body, cycleCommentMarker, "acme/looper#1 → acme/looper#2 → acme/looper#1") {
			t.Fatalf("cycle comment body = %q", body)
		}
	}
}

func TestRunnerCycleDetectionIsIdempotent(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	seedDispatchIssue(fixture, 1)
	seedDispatchIssue(fixture, 2)
	fixture.github.blockedBy[1] = []githubinfra.DependencyIssue{{Number: 2, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "open"}}
	fixture.github.blockedBy[2] = []githubinfra.DependencyIssue{{Number: 1, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "open"}}
	marker1 := stampedCoordinatorBody(fixture.cfg, cycleCommentMarker+"\n\nold")
	marker2 := stampedCoordinatorBody(fixture.cfg, cycleCommentMarker+"\n\nold")
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{nil, {{ID: 91, Author: "looper", Body: marker1, CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.comments[2] = [][]githubinfra.CommentInfo{nil, {{ID: 92, Author: "looper", Body: marker2, CreatedAt: fixture.now.Format(time.RFC3339)}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() first error = %v", err)
	}
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() second error = %v", err)
	}
	if len(fixture.github.createdBodies) != 2 {
		t.Fatalf("createdBodies = %d, want 2 total across both ticks", len(fixture.github.createdBodies))
	}
}

func TestRunnerCycleDetectionUpdatesExistingMarkedComment(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	seedDispatchIssue(fixture, 1)
	seedDispatchIssue(fixture, 2)
	fixture.github.blockedBy[1] = []githubinfra.DependencyIssue{{Number: 2, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "open"}}
	fixture.github.blockedBy[2] = []githubinfra.DependencyIssue{{Number: 1, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "open"}}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 91, Author: "looper", Body: stampedCoordinatorBody(fixture.cfg, cycleCommentMarker+"\n\nold"), CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.comments[2] = [][]githubinfra.CommentInfo{{{ID: 92, Author: "looper", Body: stampedCoordinatorBody(fixture.cfg, cycleCommentMarker+"\n\nold"), CreatedAt: fixture.now.Format(time.RFC3339)}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(fixture.github.createdBodies) != 0 {
		t.Fatalf("createdBodies = %d, want 0 when cycle marker already exists", len(fixture.github.createdBodies))
	}
	if len(fixture.github.updatedBodies) != 2 {
		t.Fatalf("updatedBodies = %d, want 2", len(fixture.github.updatedBodies))
	}
	for _, body := range fixture.github.updatedBodies {
		if !containsAll(body, cycleCommentMarker, "acme/looper#1 → acme/looper#2 → acme/looper#1") {
			t.Fatalf("updated cycle body = %q", body)
		}
	}
}

func TestRunnerClosedNotPlannedBlockerReturnsDependentToRetriage(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	seedDispatchIssue(fixture, 1)
	seedDispatchIssue(fixture, 2)
	fixture.github.blockedBy[2] = []githubinfra.DependencyIssue{{Number: 1, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "closed", StateReason: "not_planned"}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if got := countRemovedIssueOperations(fixture.github.removedLabels, 2, "triaged", "dispatch/plan"); got != 1 {
		t.Fatalf("issue 2 remove count = %d, want 1", got)
	}
	for _, body := range fixture.github.createdBodies {
		if strings.Contains(body, cycleCommentMarker) {
			t.Fatalf("unexpected cycle comment for not_planned path: %q", body)
		}
	}
}

func TestRunnerClosedDuplicateBlockerReturnsDependentToRetriage(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	seedDispatchIssue(fixture, 1)
	seedDispatchIssue(fixture, 2)
	fixture.github.blockedBy[2] = []githubinfra.DependencyIssue{{Number: 1, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "closed", StateReason: "duplicate"}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if got := countRemovedIssueOperations(fixture.github.removedLabels, 2, "triaged", "dispatch/plan"); got != 1 {
		t.Fatalf("issue 2 remove count = %d, want 1", got)
	}
}

func TestRunnerTieBreaksAutonomousDispatchByParentSubIssueOrder(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.runner.config.Scheduler.MaxConcurrentRuns = 2
	seedParentIssue(fixture, 10)
	seedDispatchIssueWithLabels(fixture, 11, []string{"triaged", "dispatch/implement"})
	seedDispatchIssueWithLabels(fixture, 12, []string{"triaged", "dispatch/implement"})
	seedDispatchIssueWithLabels(fixture, 13, []string{"triaged", "dispatch/implement"})
	fixture.github.subIssues[10] = []githubinfra.DependencyIssue{{Number: 12}, {Number: 11}, {Number: 13}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(fixture.github.assigned) != 2 {
		t.Fatalf("assigned = %d, want 2", len(fixture.github.assigned))
	}
	if fixture.github.assigned[0].IssueNumber != 12 || fixture.github.assigned[1].IssueNumber != 11 {
		t.Fatalf("assigned order = %d,%d, want 12,11", fixture.github.assigned[0].IssueNumber, fixture.github.assigned[1].IssueNumber)
	}
	if hasAssignedIssue(fixture.github.assigned, 13) {
		t.Fatal("issue 13 should wait for next tick")
	}
}

func TestRunnerTieBreakFallsBackToAscendingIssueNumber(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.runner.config.Scheduler.MaxConcurrentRuns = 2
	seedDispatchIssueWithLabels(fixture, 22, []string{"triaged", "dispatch/implement"})
	seedDispatchIssueWithLabels(fixture, 21, []string{"triaged", "dispatch/implement"})
	seedDispatchIssueWithLabels(fixture, 23, []string{"triaged", "dispatch/implement"})

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if fixture.github.assigned[0].IssueNumber != 21 || fixture.github.assigned[1].IssueNumber != 22 {
		t.Fatalf("assigned order = %d,%d, want 21,22", fixture.github.assigned[0].IssueNumber, fixture.github.assigned[1].IssueNumber)
	}
}

func TestRunnerTieBreakFallsBackWhenSubIssueLookupFails(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.runner.config.Scheduler.MaxConcurrentRuns = 2
	seedParentIssue(fixture, 10)
	seedDispatchIssueWithLabels(fixture, 22, []string{"triaged", "dispatch/implement"})
	seedDispatchIssueWithLabels(fixture, 21, []string{"triaged", "dispatch/implement"})
	seedDispatchIssueWithLabels(fixture, 23, []string{"triaged", "dispatch/implement"})
	fixture.github.subIssueErr[10] = errors.New("sub issue api unavailable")

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if fixture.github.assigned[0].IssueNumber != 21 || fixture.github.assigned[1].IssueNumber != 22 {
		t.Fatalf("assigned order = %d,%d, want 21,22", fixture.github.assigned[0].IssueNumber, fixture.github.assigned[1].IssueNumber)
	}
}

func TestRunnerAutonomousDispatchConcurrencyPreemption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		maxConcurrentRuns int
		running           int
		readyIssues       []int64
		readyLabels       []string
		downstreamType    string
		reviewerLabels    []string
		fixerLabels       []string
		prLabels          []string
		prAuthor          string
		prIsDraft         bool
		reviewRequests    []string
		prComments        []map[string]any
		currentLogin      string
		projectRoles      *config.PartialRoleConfigs
		wantAssigned      []int64
	}{
		{name: "pool has slack without downstream pending", maxConcurrentRuns: 3, running: 1, readyIssues: []int64{1, 2, 3}, wantAssigned: []int64{1, 2}},
		{name: "pool would saturate without downstream pending", maxConcurrentRuns: 2, running: 1, readyIssues: []int64{1, 2}, wantAssigned: []int64{1}},
		{name: "pool would saturate with non-worker ready work", maxConcurrentRuns: 2, running: 1, readyIssues: []int64{1, 2}, readyLabels: []string{"triaged", "dispatch/plan"}, wantAssigned: []int64{1}},
		{name: "pool would saturate with pending reviewer work", maxConcurrentRuns: 2, running: 1, readyIssues: []int64{1, 2}, downstreamType: "reviewer", reviewRequests: []string{"looper"}, wantAssigned: nil},
		{name: "pool would saturate with pending reviewer work and only non-worker ready work", maxConcurrentRuns: 2, running: 1, readyIssues: []int64{1, 2}, readyLabels: []string{"triaged", "dispatch/plan"}, downstreamType: "reviewer", reviewRequests: []string{"looper"}, wantAssigned: []int64{1}},
		{name: "pool would saturate with pending reviewer work when labels differ only by case", maxConcurrentRuns: 2, running: 1, readyIssues: []int64{1, 2}, downstreamType: "reviewer", reviewerLabels: []string{"Looper:Review"}, prLabels: []string{"looper:review"}, reviewRequests: []string{"looper"}, wantAssigned: nil},
		{name: "pool would saturate with pending fixer work", maxConcurrentRuns: 2, running: 1, readyIssues: []int64{1, 2}, downstreamType: "fixer", prComments: []map[string]any{{"id": "comment-1", "threadId": "thread-1", "body": "fix this"}}, wantAssigned: nil},
		{name: "pool would saturate with project reviewer override", maxConcurrentRuns: 2, running: 1, readyIssues: []int64{1, 2}, downstreamType: "reviewer", prAuthor: "octocat", currentLogin: "looper", projectRoles: &config.PartialRoleConfigs{Reviewer: &config.PartialReviewerRoleConfig{Discovery: &config.PartialReviewerRoleDiscoveryConfig{Triggers: &config.PartialReviewerRoleTriggersConfig{RequireReviewRequest: boolPtr(false)}}}}, wantAssigned: nil},
		{name: "pool would saturate without reviewer request even without label filters", maxConcurrentRuns: 2, running: 1, readyIssues: []int64{1, 2}, downstreamType: "reviewer", reviewerLabels: []string{}, prLabels: []string{}, wantAssigned: []int64{1}},
		{name: "pool would saturate without actionable fixer work even without label filters", maxConcurrentRuns: 2, running: 1, readyIssues: []int64{1, 2}, downstreamType: "fixer", fixerLabels: []string{}, prLabels: []string{}, wantAssigned: []int64{1}},
		{name: "pool would saturate with pending reviewer work without label filters when requested", maxConcurrentRuns: 2, running: 1, readyIssues: []int64{1, 2}, downstreamType: "reviewer", reviewerLabels: []string{}, prLabels: []string{}, reviewRequests: []string{"looper"}, wantAssigned: nil},
		{name: "pool would saturate with pending fixer work without label filters when actionable", maxConcurrentRuns: 2, running: 1, readyIssues: []int64{1, 2}, downstreamType: "fixer", fixerLabels: []string{}, prLabels: []string{}, prComments: []map[string]any{{"id": "comment-1", "threadId": "thread-1", "body": "fix this"}}, wantAssigned: nil},
		{name: "pool ignores fixer work for draft from another author", maxConcurrentRuns: 2, running: 1, readyIssues: []int64{1, 2}, downstreamType: "fixer", prAuthor: "octocat", prIsDraft: true, currentLogin: "looper", prComments: []map[string]any{{"id": "comment-1", "threadId": "thread-1", "body": "fix this"}}, wantAssigned: []int64{1}},
		{name: "pool has slack with pending reviewer work", maxConcurrentRuns: 4, running: 1, readyIssues: []int64{1, 2}, downstreamType: "reviewer", wantAssigned: []int64{1, 2}},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newCoordinatorFixture(t)
			fixture.runner.config.Roles.Coordinator.Enabled = true
			fixture.runner.config.Roles.Coordinator.PollInterval = "0s"
			fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
			fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
			fixture.runner.config.Scheduler.MaxConcurrentRuns = tc.maxConcurrentRuns
			reviewerLabels := tc.reviewerLabels
			if reviewerLabels == nil {
				reviewerLabels = []string{"looper:review"}
			}
			fixture.runner.config.Roles.Reviewer.Discovery.Triggers.Labels = reviewerLabels
			fixerLabels := tc.fixerLabels
			if fixerLabels == nil {
				fixerLabels = []string{"looper:fix"}
			}
			fixture.runner.config.Roles.Fixer.Triggers.Labels = fixerLabels
			if tc.currentLogin != "" {
				fixture.github.currentLogin = tc.currentLogin
			}
			if tc.projectRoles != nil {
				fixture.runner.config.Projects = []config.ProjectRefConfig{{ID: fixture.projectID, Name: "Demo", RepoPath: "/tmp/demo", Roles: tc.projectRoles}}
			}
			readyLabels := tc.readyLabels
			if readyLabels == nil {
				readyLabels = []string{"triaged", "dispatch/implement"}
			}
			for _, issueNumber := range tc.readyIssues {
				seedDispatchIssueWithLabels(fixture, issueNumber, readyLabels)
			}
			if tc.downstreamType != "" {
				fixture.github.linkedPullRequests[1] = []githubinfra.LinkedPullRequest{{Number: 91, State: "OPEN"}}
				labels := []string{"looper:review"}
				if tc.downstreamType == "fixer" {
					labels = []string{"looper:fix"}
				}
				if tc.prLabels != nil {
					labels = tc.prLabels
				}
				author := tc.prAuthor
				if author == "" {
					author = "looper"
					if tc.downstreamType == "reviewer" {
						author = "octocat"
					}
				}
				fixture.github.pullRequests[91] = githubinfra.PullRequestDetail{Number: 91, State: "OPEN", Author: author, IsDraft: tc.prIsDraft, Labels: labels, ReviewRequests: tc.reviewRequests, Comments: tc.prComments}
			}
			seedRunningQueueItems(t, fixture, tc.running)

			if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
				t.Fatalf("DiscoverIssues() error = %v", err)
			}
			assertAssignedIssueNumbers(t, fixture.github.assigned, tc.wantAssigned)
		})
	}
}

func TestRunnerAutonomousDispatchPreemptionIsPerTick(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.PollInterval = "0s"
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.runner.config.Scheduler.MaxConcurrentRuns = 2
	fixture.runner.config.Roles.Reviewer.Discovery.Triggers.Labels = []string{"looper:review"}
	seedDispatchIssueWithLabels(fixture, 1, []string{"triaged", "dispatch/implement"})
	seedDispatchIssueWithLabels(fixture, 2, []string{"triaged", "dispatch/implement"})
	fixture.github.linkedPullRequests[1] = []githubinfra.LinkedPullRequest{{Number: 91, State: "OPEN"}}
	fixture.github.pullRequests[91] = githubinfra.PullRequestDetail{Number: 91, State: "OPEN", Labels: []string{"looper:review"}, ReviewRequests: []string{"looper"}}
	seedRunningQueueItems(t, fixture, 1)

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() first tick error = %v", err)
	}
	if len(fixture.github.assigned) != 0 {
		t.Fatalf("assigned on first tick = %v, want none", assignedIssueNumbers(fixture.github.assigned))
	}

	clearRunningQueueItems(t, fixture)
	fixture.runner.config.Scheduler.MaxConcurrentRuns = 3
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() second tick error = %v", err)
	}
	assertAssignedIssueNumbers(t, fixture.github.assigned, []int64{1, 2})
}

func TestRunnerAutonomousDispatchPreemptionCountsWorkerDispatchesFromDispatchType(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.PollInterval = "0s"
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.runner.config.Scheduler.MaxConcurrentRuns = 2
	fixture.runner.config.Roles.Worker.Triggers.Labels = []string{"looper:worker", "team:backend"}
	fixture.runner.config.Roles.Worker.Triggers.LabelMode = config.LabelModeAll
	fixture.runner.config.Roles.Reviewer.Discovery.Triggers.Labels = []string{"looper:review"}
	seedDispatchIssueWithLabels(fixture, 1, []string{"triaged", "dispatch/implement", "looper:worker"})
	seedDispatchIssueWithLabels(fixture, 2, []string{"triaged", "dispatch/implement"})
	fixture.github.linkedPullRequests[1] = []githubinfra.LinkedPullRequest{{Number: 91, State: "OPEN"}}
	fixture.github.pullRequests[91] = githubinfra.PullRequestDetail{Number: 91, State: "OPEN", Labels: []string{"looper:review"}, ReviewRequests: []string{"looper"}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	assertAssignedIssueNumbers(t, fixture.github.assigned, nil)
}

func TestRunnerAutonomousDispatchPreemptionOnlyCountsWorkersWithinTickBudget(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.PollInterval = "0s"
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.runner.config.Scheduler.MaxConcurrentRuns = 2
	fixture.runner.config.Roles.Reviewer.Discovery.Triggers.Labels = []string{"looper:review"}
	seedDispatchIssueWithLabels(fixture, 1, []string{"triaged", "dispatch/plan"})
	seedDispatchIssueWithLabels(fixture, 2, []string{"triaged", "dispatch/implement"})
	fixture.github.linkedPullRequests[1] = []githubinfra.LinkedPullRequest{{Number: 91, State: "OPEN"}}
	fixture.github.pullRequests[91] = githubinfra.PullRequestDetail{Number: 91, State: "OPEN", Labels: []string{"looper:review"}, ReviewRequests: []string{"looper"}}
	seedRunningQueueItems(t, fixture, 1)

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	assertAssignedIssueNumbers(t, fixture.github.assigned, []int64{1})
}

func TestRunnerAutonomousDispatchPreemptionSkipsWorkerWithoutZeroingBudget(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.PollInterval = "0s"
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.runner.config.Scheduler.MaxConcurrentRuns = 2
	fixture.runner.config.Roles.Reviewer.Discovery.Triggers.Labels = []string{"looper:review"}
	seedDispatchIssueWithLabels(fixture, 1, []string{"triaged", "dispatch/implement"})
	seedDispatchIssueWithLabels(fixture, 2, []string{"triaged", "dispatch/plan"})
	fixture.github.linkedPullRequests[1] = []githubinfra.LinkedPullRequest{{Number: 91, State: "OPEN"}}
	fixture.github.pullRequests[91] = githubinfra.PullRequestDetail{Number: 91, State: "OPEN", Labels: []string{"looper:review"}, ReviewRequests: []string{"looper"}}
	seedRunningQueueItems(t, fixture, 1)

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	assertAssignedIssueNumbers(t, fixture.github.assigned, []int64{2})
}

func TestRunnerAutonomousDispatchNoOpWorkerAdmissionDoesNotConsumeBudget(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.PollInterval = "0s"
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.runner.config.Scheduler.MaxConcurrentRuns = 1
	fixture.github.issues = []githubinfra.IssueSummary{
		{Number: 1, Labels: []string{"looper:worker-ready"}},
		{Number: 2, Labels: []string{"triaged", "dispatch/implement"}},
	}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Already admitted", Author: "octo", URL: "https://github.com/acme/looper/issues/1", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339), Labels: []string{"looper:worker-ready"}}
	fixture.github.details[2] = githubinfra.IssueDetail{Number: 2, Title: "Needs admission", Author: "octo", URL: "https://github.com/acme/looper/issues/2", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339), Labels: []string{"triaged", "dispatch/implement"}}
	fixture.github.timeline[2] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}

	assertAssignedIssueNumbers(t, fixture.github.assigned, []int64{2})
	if hasAssignedIssue(fixture.github.assigned, 1) {
		t.Fatalf("assigned issue 1 unexpectedly; assigned = %v", assignedIssueNumbers(fixture.github.assigned))
	}
	if got := countAddedIssueOperations(fixture.github.addedLabels, 1, "looper:worker-ready"); got != 0 {
		t.Fatalf("issue 1 worker-ready add count = %d, want 0", got)
	}
	if got := countAddedIssueOperations(fixture.github.addedLabels, 2, "looper:worker-ready"); got != 1 {
		t.Fatalf("issue 2 worker-ready add count = %d, want 1", got)
	}
}

func TestRunnerMatchesHostnameQualifiedRepoDependencies(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	seedDispatchIssue(fixture, 1)
	seedDispatchIssue(fixture, 2)
	fixture.github.blockedBy[1] = []githubinfra.DependencyIssue{{Number: 2, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "open"}}
	fixture.github.blockedBy[2] = []githubinfra.DependencyIssue{{Number: 1, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "open"}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "github.example.com/acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if got := countRemovedIssueOperations(fixture.github.removedLabels, 1, "triaged", "dispatch/plan"); got != 1 {
		t.Fatalf("issue 1 remove count = %d, want 1", got)
	}
	if got := countRemovedIssueOperations(fixture.github.removedLabels, 2, "triaged", "dispatch/plan"); got != 1 {
		t.Fatalf("issue 2 remove count = %d, want 1", got)
	}
}

func TestRunnerReopenedBlockerDoesNothingForInFlightIssue(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	seedDispatchIssueWithLabels(fixture, 1, []string{"triaged", "dispatch/plan", "looper:plan"})
	seedDispatchIssueWithLabels(fixture, 2, []string{"triaged", "dispatch/plan"})
	fixture.github.blockedBy[2] = []githubinfra.DependencyIssue{{Number: 1, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "open"}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if countRemovedIssueOperations(fixture.github.removedLabels, 1, "triaged", "dispatch/plan") != 0 {
		t.Fatal("in-flight blocker issue should not be reset")
	}
	if hasAssignedIssue(fixture.github.assigned, 2) {
		t.Fatal("dependent should remain blocked while blocker is open")
	}
}

func TestRunnerReopenedBlockerHoldsUndispatchedDependent(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	seedDispatchIssue(fixture, 1)
	seedDispatchIssue(fixture, 2)
	fixture.github.blockedBy[2] = []githubinfra.DependencyIssue{{Number: 1, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "open"}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if hasAssignedIssue(fixture.github.assigned, 2) {
		t.Fatal("dependent should remain held by dependency gate")
	}
}

type coordinatorFixture struct {
	runner    *Runner
	github    *stubCoordinatorGitHub
	network   *stubCoordinatorNetwork
	cfg       *config.Config
	projectID string
	now       time.Time
	coord     *storage.SQLiteCoordinator
}

func newCoordinatorFixture(t *testing.T) coordinatorFixture {
	t.Helper()
	now := time.Date(2026, time.May, 14, 12, 0, 0, 0, time.UTC)
	coord, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "coordinator.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coord.Close() })
	if _, err := coord.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coord.DB())
	projectID := "demo"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Demo", RepoPath: t.TempDir(), CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: projectID}}
	cfg.Disclosure.Enabled = true
	cfg.Disclosure.Channels.IssueComment = true
	github := &stubCoordinatorGitHub{details: map[int64]githubinfra.IssueDetail{}, comments: map[int64][][]githubinfra.CommentInfo{}, timeline: map[int64][]map[string]any{}, blockedBy: map[int64][]githubinfra.DependencyIssue{}, subIssues: map[int64][]githubinfra.DependencyIssue{}, linkedPullRequests: map[int64][]githubinfra.LinkedPullRequest{}, pullRequests: map[int64]githubinfra.PullRequestDetail{}, subIssueErr: map[int64]error{}, prDetails: map[int64]githubinfra.PullRequestDetail{}, prCheckRuns: map[string]githubinfra.PullRequestCheckRuns{}, branchProtection: map[string]githubinfra.BranchProtection{}}
	network := &stubCoordinatorNetwork{}
	runner := New(Options{Repos: repos, GitHub: github, Config: &cfg, Now: func() time.Time { return now }, TriageLLM: stubCoordinatorLLM{}, Inspector: stubCoordinatorInspector{}, Network: network})
	return coordinatorFixture{runner: runner, github: github, network: network, cfg: &cfg, projectID: projectID, now: now, coord: coord}
}

type stubCoordinatorNetwork struct {
	status          protocol.NodeStatusResponse
	statusErr       error
	revalidateErrs  []error
	revalidateCalls []protocol.CoordinatorLeaseRevalidateRequest
}

func (s *stubCoordinatorNetwork) Status(context.Context) (protocol.NodeStatusResponse, error) {
	if s.statusErr != nil {
		return protocol.NodeStatusResponse{}, s.statusErr
	}
	return s.status, nil
}

func (s *stubCoordinatorNetwork) RevalidateLease(_ context.Context, req protocol.CoordinatorLeaseRevalidateRequest) error {
	s.revalidateCalls = append(s.revalidateCalls, req)
	if len(s.revalidateErrs) == 0 {
		return nil
	}
	err := s.revalidateErrs[0]
	s.revalidateErrs = s.revalidateErrs[1:]
	return err
}

func timePtr(value time.Time) *time.Time { return &value }

type stubCoordinatorLLM struct{}

func (stubCoordinatorLLM) Complete(context.Context, triage.Request) (string, error) {
	return `{"disposition":"valid","comment":"Looks actionable.","labels":{"kind":["kind/bug"],"area":["area/coordinator"],"complexity":["complexity/m"],"dispatch":["dispatch/plan"]}}`, nil
}

type stubUnclearCoordinatorLLM struct{}

func (stubUnclearCoordinatorLLM) Complete(context.Context, triage.Request) (string, error) {
	return `{"disposition":"unclear","comment":"Please share more detail.","labels":{"kind":[],"area":[],"complexity":[],"dispatch":[]}}`, nil
}

type stubCoordinatorInspector struct{}

func (stubCoordinatorInspector) Inspect(context.Context, string, triage.Issue) (triage.RepoContext, error) {
	return triage.RepoContext{Paths: []string{"internal/coordinator/runner.go"}, Symbols: []string{"internal/coordinator/runner.go: func DiscoverIssues"}}, nil
}

type stubCoordinatorGitHub struct {
	issues               []githubinfra.IssueSummary
	details              map[int64]githubinfra.IssueDetail
	comments             map[int64][][]githubinfra.CommentInfo
	timeline             map[int64][]map[string]any
	blockedBy            map[int64][]githubinfra.DependencyIssue
	subIssues            map[int64][]githubinfra.DependencyIssue
	linkedPullRequests   map[int64][]githubinfra.LinkedPullRequest
	pullRequests         map[int64]githubinfra.PullRequestDetail
	subIssueErr          map[int64]error
	blockedByReads       int
	blockedByIssueReads  int
	permissionErr        error
	ops                  []string
	createdBodies        []string
	updatedBodies        []string
	commentReads         map[int64]int
	failAddLabels        map[string]error
	failBlockedByIssues  map[int64][]error
	addedLabels          []githubinfra.IssueLabelsInput
	removedLabels        []githubinfra.IssueLabelsInput
	assigned             []githubinfra.IssueAssigneesInput
	prDetails            map[int64]githubinfra.PullRequestDetail
	failPRDetails        map[int64][]error
	prCheckRuns          map[string]githubinfra.PullRequestCheckRuns
	failPRCheckRuns      map[string]error
	branchProtection     map[string]githubinfra.BranchProtection
	failBranchProtection map[string]error
	addedPRLabels        []githubinfra.PullRequestLabelsInput
	currentLogin         string
}

func (s *stubCoordinatorGitHub) ListOpenIssues(context.Context, githubinfra.ListOpenIssuesInput) ([]githubinfra.IssueSummary, error) {
	return append([]githubinfra.IssueSummary(nil), s.issues...), nil
}
func (s *stubCoordinatorGitHub) ViewIssue(_ context.Context, input githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error) {
	return s.details[input.IssueNumber], nil
}
func (s *stubCoordinatorGitHub) ViewPullRequest(_ context.Context, input githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error) {
	if s.pullRequests != nil {
		if detail, ok := s.pullRequests[input.PRNumber]; ok {
			return detail, nil
		}
	}
	if s.prDetails != nil {
		return s.prDetails[input.PRNumber], nil
	}
	return githubinfra.PullRequestDetail{}, nil
}
func (s *stubCoordinatorGitHub) GetIssueState(_ context.Context, input githubinfra.ViewIssueInput) (githubinfra.IssueState, error) {
	detail := s.details[input.IssueNumber]
	return githubinfra.IssueState{State: detail.State, StateReason: detail.StateReason}, nil
}
func (s *stubCoordinatorGitHub) ListIssueComments(_ context.Context, input githubinfra.ViewIssueInput) ([]githubinfra.CommentInfo, error) {
	if s.commentReads == nil {
		s.commentReads = map[int64]int{}
	}
	reads := s.commentReads[input.IssueNumber]
	batches := s.comments[input.IssueNumber]
	if len(batches) == 0 {
		return nil, nil
	}
	if reads >= len(batches) {
		reads = len(batches) - 1
	}
	s.commentReads[input.IssueNumber]++
	return append([]githubinfra.CommentInfo(nil), batches[reads]...), nil
}
func (s *stubCoordinatorGitHub) ListIssueBlockedBy(_ context.Context, input githubinfra.ListIssueBlockedByInput) ([]githubinfra.IssueDependency, error) {
	s.blockedByReads++
	issues := s.blockedBy[input.IssueNumber]
	out := make([]githubinfra.IssueDependency, 0, len(issues))
	for _, issue := range issues {
		out = append(out, githubinfra.IssueDependency{Number: issue.Number, Repo: issue.Repository.FullName})
	}
	return out, nil
}
func (s *stubCoordinatorGitHub) GetCurrentUserLogin(context.Context, string) (string, error) {
	if s.currentLogin != "" {
		return s.currentLogin, nil
	}
	return "looper", nil
}
func (s *stubCoordinatorGitHub) GetCurrentUserLoginForRepo(context.Context, string, string) (string, error) {
	if s.currentLogin != "" {
		return s.currentLogin, nil
	}
	return "looper", nil
}
func (s *stubCoordinatorGitHub) ListIssueTimeline(_ context.Context, input githubinfra.IssueTimelineInput) ([]map[string]any, error) {
	return s.timeline[input.IssueNumber], nil
}
func (s *stubCoordinatorGitHub) GetRepositoryPermission(_ context.Context, input githubinfra.RepositoryPermissionInput) (string, error) {
	if s.permissionErr != nil {
		return "", s.permissionErr
	}
	if input.User == "octo" {
		return "write", nil
	}
	return "read", nil
}
func (s *stubCoordinatorGitHub) ListBlockedByIssues(_ context.Context, input githubinfra.ViewIssueInput) ([]githubinfra.DependencyIssue, error) {
	s.blockedByIssueReads++
	if failures := s.failBlockedByIssues[input.IssueNumber]; len(failures) > 0 {
		err := failures[0]
		s.failBlockedByIssues[input.IssueNumber] = failures[1:]
		return nil, err
	}
	return append([]githubinfra.DependencyIssue(nil), s.blockedBy[input.IssueNumber]...), nil
}
func (s *stubCoordinatorGitHub) ListLinkedPullRequests(_ context.Context, input githubinfra.LinkedPullRequestsInput) ([]githubinfra.LinkedPullRequest, error) {
	return append([]githubinfra.LinkedPullRequest(nil), s.linkedPullRequests[input.IssueNumber]...), nil
}
func (s *stubCoordinatorGitHub) ListSubIssues(_ context.Context, input githubinfra.ViewIssueInput) ([]githubinfra.DependencyIssue, error) {
	if err := s.subIssueErr[input.IssueNumber]; err != nil {
		return nil, err
	}
	return append([]githubinfra.DependencyIssue(nil), s.subIssues[input.IssueNumber]...), nil
}
func (s *stubCoordinatorGitHub) AddIssueAssignees(_ context.Context, input githubinfra.IssueAssigneesInput) error {
	s.ops = append(s.ops, "assign:"+joinLabels(input.Assignees))
	s.assigned = append(s.assigned, input)
	return nil
}
func (s *stubCoordinatorGitHub) AddIssueLabels(_ context.Context, input githubinfra.IssueLabelsInput) error {
	s.ops = append(s.ops, "add:"+joinLabels(input.Labels))
	s.addedLabels = append(s.addedLabels, input)
	if s.failAddLabels != nil {
		if err, ok := s.failAddLabels[joinLabels(input.Labels)]; ok {
			return err
		}
	}
	return nil
}
func (s *stubCoordinatorGitHub) AddIssueReaction(_ context.Context, input githubinfra.CreateIssueReactionInput) error {
	s.ops = append(s.ops, "react:"+input.Content+":"+intToString(input.CommentID))
	return nil
}
func (s *stubCoordinatorGitHub) RemoveIssueLabels(_ context.Context, input githubinfra.IssueLabelsInput) error {
	s.ops = append(s.ops, "remove:"+joinLabels(input.Labels))
	s.removedLabels = append(s.removedLabels, input)
	return nil
}
func (s *stubCoordinatorGitHub) CreateIssueComment(_ context.Context, input githubinfra.IssueCommentInput) (githubinfra.IssueCommentResult, error) {
	s.ops = append(s.ops, "create-comment")
	s.createdBodies = append(s.createdBodies, input.Body)
	return githubinfra.IssueCommentResult{ID: 1}, nil
}
func (s *stubCoordinatorGitHub) UpdateIssueComment(_ context.Context, input githubinfra.UpdateIssueCommentInput) error {
	s.ops = append(s.ops, "update-comment")
	s.updatedBodies = append(s.updatedBodies, input.Body)
	return nil
}
func (s *stubCoordinatorGitHub) DeleteIssueComment(_ context.Context, input githubinfra.DeleteIssueCommentInput) error {
	s.ops = append(s.ops, "delete-comment")
	return nil
}
func (s *stubCoordinatorGitHub) AddPullRequestLabels(_ context.Context, input githubinfra.PullRequestLabelsInput) error {
	s.ops = append(s.ops, "add-pr:"+joinLabels(input.Labels))
	s.addedPRLabels = append(s.addedPRLabels, input)
	return nil
}
func (s *stubCoordinatorGitHub) ViewPullRequestMergeWatch(_ context.Context, input githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error) {
	if failures := s.failPRDetails[input.PRNumber]; len(failures) > 0 {
		err := failures[0]
		s.failPRDetails[input.PRNumber] = failures[1:]
		if err != nil {
			return githubinfra.PullRequestDetail{}, err
		}
	}
	if s.prDetails == nil {
		return githubinfra.PullRequestDetail{}, nil
	}
	return s.prDetails[input.PRNumber], nil
}
func (s *stubCoordinatorGitHub) ListPullRequestCheckRuns(_ context.Context, input githubinfra.PullRequestCheckRunsInput) (githubinfra.PullRequestCheckRuns, error) {
	if err := s.failPRCheckRuns[input.Ref]; err != nil {
		return githubinfra.PullRequestCheckRuns{}, err
	}
	if s.prCheckRuns == nil {
		return githubinfra.PullRequestCheckRuns{}, nil
	}
	return s.prCheckRuns[input.Ref], nil
}
func (s *stubCoordinatorGitHub) GetBranchProtection(_ context.Context, input githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error) {
	if err := s.failBranchProtection[input.Branch]; err != nil {
		return githubinfra.BranchProtection{}, err
	}
	if s.branchProtection == nil {
		return githubinfra.BranchProtection{}, nil
	}
	return s.branchProtection[input.Branch], nil
}

func TestRunnerHumanDispatchBlockedByPostsFailureComment(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged", "dispatch/plan"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339), Labels: []string{"triaged", "dispatch/plan"}, Comments: []githubinfra.CommentInfo{{ID: 11, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.details[9] = githubinfra.IssueDetail{Number: 9, State: "open"}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 11, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.timeline[1] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}
	fixture.github.blockedBy[1] = []githubinfra.DependencyIssue{{Number: 9, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "open"}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	assertOrderedOps(t, fixture.github.ops, []string{"create-comment", "react:confused:11"})
	if len(fixture.github.createdBodies) != 1 || !containsAll(fixture.github.createdBodies[0], dispatchFailureCommentMarker, "#9", "open") {
		t.Fatalf("createdBodies = %v, want blocked_by failure comment", fixture.github.createdBodies)
	}
}

func TestRunnerMergeWatchConflictRoutesToFixerAndUpdatesMarker(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Fixer.Triggers.Labels = []string{"looper:fix"}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1,
		Title:  "Bug",
		Author: "octo",
		Labels: []string{"triaged"},
		Comments: []githubinfra.CommentInfo{{
			ID:        44,
			Author:    "looper",
			Body:      mergeWatchCommentBody(fixture.cfg, 77, "abc123", 3, nil, nil, "old summary"),
			CreatedAt: fixture.now.Format(time.RFC3339),
		}},
		CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339),
	}
	fixture.github.prDetails[77] = githubinfra.PullRequestDetail{
		Number:         77,
		Body:           "Closes #1",
		State:          "open",
		HeadSHA:        "abc123",
		BaseRefName:    "main",
		Labels:         []string{"looper:plan"},
		Mergeable:      boolPtr(false),
		MergeableState: "dirty",
		AutoMerge:      &githubinfra.PullRequestAutoMerge{EnabledBy: "looper"},
	}
	fixture.github.timeline[1] = []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 77, "html_url": "https://github.com/acme/looper/pull/77"}}}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	assertOrderedOps(t, fixture.github.ops, []string{"add-pr:looper:fix", "update-comment"})
	if len(fixture.github.addedPRLabels) != 1 || fixture.github.addedPRLabels[0].PRNumber != 77 {
		t.Fatalf("addedPRLabels = %#v, want PR #77 label add", fixture.github.addedPRLabels)
	}
	if len(fixture.github.updatedBodies) != 1 || !containsAll(fixture.github.updatedBodies[0], "Coordinator merge-watch routed PR #77 to Fixer for conflict.", mergeWatchCommentMarkerPrefix, "pr=77", "head_sha=abc123") {
		t.Fatalf("updatedBodies = %v, want conflict summary marker update", fixture.github.updatedBodies)
	}
	if len(fixture.github.createdBodies) != 0 {
		t.Fatalf("createdBodies = %v, want no new comments", fixture.github.createdBodies)
	}
}

func TestRunnerMergeWatchHumanDisabledRemovesMarkerOnly(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Fixer.Triggers.Labels = []string{"looper:fix"}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1,
		Title:  "Bug",
		Author: "octo",
		Labels: []string{"triaged"},
		Comments: []githubinfra.CommentInfo{{
			ID:        45,
			Author:    "looper",
			Body:      mergeWatchCommentBody(fixture.cfg, 78, "def456", 3, nil, nil, "watching"),
			CreatedAt: fixture.now.Format(time.RFC3339),
		}},
		CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339),
	}
	fixture.github.prDetails[78] = githubinfra.PullRequestDetail{
		Number:         78,
		Body:           "Closes #1",
		State:          "open",
		HeadSHA:        "def456",
		BaseRefName:    "main",
		Labels:         []string{"looper:plan"},
		Mergeable:      boolPtr(true),
		MergeableState: "clean",
	}
	fixture.github.timeline[1] = []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 78, "html_url": "https://github.com/acme/looper/pull/78"}}}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(fixture.github.ops) != 1 || fixture.github.ops[0] != "delete-comment" {
		t.Fatalf("ops = %v, want only delete-comment", fixture.github.ops)
	}
	if len(fixture.github.addedPRLabels) != 0 || len(fixture.github.updatedBodies) != 0 || len(fixture.github.createdBodies) != 0 || len(fixture.github.addedLabels) != 0 || len(fixture.github.removedLabels) != 0 {
		t.Fatalf("unexpected mutations: addedPRLabels=%v updatedBodies=%v createdBodies=%v addedLabels=%v removedLabels=%v", fixture.github.addedPRLabels, fixture.github.updatedBodies, fixture.github.createdBodies, fixture.github.addedLabels, fixture.github.removedLabels)
	}
}

func TestRunnerMergeWatchTransientErrorKeepsMarkerAndSchedulesRetry(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.MergeWatch.TransientRetries = 3
	fixture.github.failPRCheckRuns = map[string]error{"abc123": errors.New("HTTP 504 gateway timeout")}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1,
		Title:  "Bug",
		Author: "octo",
		Labels: []string{"triaged"},
		Comments: []githubinfra.CommentInfo{{
			ID:        46,
			Author:    "looper",
			Body:      mergeWatchCommentBody(fixture.cfg, 77, "abc123", 2, nil, nil, "watching"),
			CreatedAt: fixture.now.Format(time.RFC3339),
		}},
		CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339),
	}
	fixture.github.prDetails[77] = githubinfra.PullRequestDetail{
		Number:         77,
		Body:           "Closes #1",
		State:          "open",
		HeadSHA:        "abc123",
		BaseRefName:    "main",
		Labels:         []string{"looper:plan"},
		Mergeable:      boolPtr(true),
		MergeableState: "blocked",
		AutoMerge:      &githubinfra.PullRequestAutoMerge{EnabledBy: "looper"},
	}
	fixture.github.timeline[1] = []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 77, "html_url": "https://github.com/acme/looper/pull/77"}}}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(fixture.github.ops) != 1 || fixture.github.ops[0] != "update-comment" {
		t.Fatalf("ops = %v, want retry marker update only", fixture.github.ops)
	}
	if len(fixture.github.updatedBodies) != 1 || len(fixture.github.createdBodies) != 0 {
		t.Fatalf("updatedBodies=%v createdBodies=%v, want marker retry update without recreation", fixture.github.updatedBodies, fixture.github.createdBodies)
	}
	if len(fixture.github.removedLabels) != 0 {
		t.Fatalf("removedLabels = %v, want no retriage cleanup on transient error", fixture.github.removedLabels)
	}
	if !containsAll(fixture.github.updatedBodies[0], "head_sha=abc123", "retries=1") || strings.Contains(fixture.github.updatedBodies[0], "pr=0") {
		t.Fatalf("updatedBodies = %v, want preserved head SHA and decremented retry budget", fixture.github.updatedBodies)
	}
}

func TestRunnerMergeWatchPRDetailTransientErrorConsumesRetryBudget(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.MergeWatch.TransientRetries = 3
	fixture.github.failPRDetails = map[int64][]error{77: {nil, errors.New("HTTP 504 gateway timeout")}}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1,
		Title:  "Bug",
		Author: "octo",
		Labels: []string{"triaged"},
		Comments: []githubinfra.CommentInfo{{
			ID:        48,
			Author:    "looper",
			Body:      mergeWatchCommentBody(fixture.cfg, 77, "abc123", 2, nil, nil, "watching"),
			CreatedAt: fixture.now.Format(time.RFC3339),
		}},
		CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339),
	}
	fixture.github.prDetails[77] = githubinfra.PullRequestDetail{
		Number:         77,
		Body:           "Closes #1",
		State:          "open",
		HeadSHA:        "abc123",
		BaseRefName:    "main",
		Labels:         []string{"looper:plan"},
		Mergeable:      boolPtr(true),
		MergeableState: "blocked",
		AutoMerge:      &githubinfra.PullRequestAutoMerge{EnabledBy: "looper"},
	}
	fixture.github.timeline[1] = []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 77, "html_url": "https://github.com/acme/looper/pull/77"}}}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(fixture.github.ops) != 1 || fixture.github.ops[0] != "update-comment" {
		t.Fatalf("ops = %v, want retry marker update only", fixture.github.ops)
	}
	if len(fixture.github.updatedBodies) != 1 || !containsAll(fixture.github.updatedBodies[0], "head_sha=abc123", "retries=1") {
		t.Fatalf("updatedBodies = %v, want preserved head SHA and decremented retry budget", fixture.github.updatedBodies)
	}
	if len(fixture.github.removedLabels) != 0 {
		t.Fatalf("removedLabels = %v, want no retriage cleanup on transient error", fixture.github.removedLabels)
	}
}

func TestRunnerMergeWatchBranchProtectionTransientErrorConsumesRetryBudget(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.MergeWatch.TransientRetries = 3
	fixture.github.failBranchProtection = map[string]error{"main": errors.New("HTTP 429 rate limit")}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1,
		Title:  "Bug",
		Author: "octo",
		Labels: []string{"triaged"},
		Comments: []githubinfra.CommentInfo{{
			ID:        49,
			Author:    "looper",
			Body:      mergeWatchCommentBody(fixture.cfg, 77, "abc123", 2, nil, nil, "watching"),
			CreatedAt: fixture.now.Format(time.RFC3339),
		}},
		CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339),
	}
	fixture.github.prDetails[77] = githubinfra.PullRequestDetail{
		Number:         77,
		Body:           "Closes #1",
		State:          "open",
		HeadSHA:        "abc123",
		BaseRefName:    "main",
		Labels:         []string{"looper:plan"},
		Mergeable:      boolPtr(true),
		MergeableState: "blocked",
		AutoMerge:      &githubinfra.PullRequestAutoMerge{EnabledBy: "looper"},
	}
	fixture.github.timeline[1] = []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 77, "html_url": "https://github.com/acme/looper/pull/77"}}}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(fixture.github.ops) != 1 || fixture.github.ops[0] != "update-comment" {
		t.Fatalf("ops = %v, want retry marker update only", fixture.github.ops)
	}
	if len(fixture.github.updatedBodies) != 1 || !containsAll(fixture.github.updatedBodies[0], "head_sha=abc123", "retries=1") {
		t.Fatalf("updatedBodies = %v, want preserved head SHA and decremented retry budget", fixture.github.updatedBodies)
	}
	if len(fixture.github.removedLabels) != 0 {
		t.Fatalf("removedLabels = %v, want no retriage cleanup on transient error", fixture.github.removedLabels)
	}
	if len(fixture.github.createdBodies) != 0 {
		t.Fatalf("createdBodies = %v, want no new comments", fixture.github.createdBodies)
	}
	if len(fixture.github.addedPRLabels) != 0 {
		t.Fatalf("addedPRLabels = %v, want no fixer routing on transient error", fixture.github.addedPRLabels)
	}
}

func TestRunnerMergeWatchStatusContextsPreventMissingRequiredCheck(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1,
		Title:  "Bug",
		Author: "octo",
		Labels: []string{"triaged"},
		Comments: []githubinfra.CommentInfo{{
			ID:        47,
			Author:    "looper",
			Body:      mergeWatchCommentBody(fixture.cfg, 79, "ghi789", 3, nil, nil, "watching"),
			CreatedAt: fixture.now.Add(-2 * time.Minute).Format(time.RFC3339),
		}},
		CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339),
	}
	fixture.github.prDetails[79] = githubinfra.PullRequestDetail{
		Number:         79,
		Body:           "Closes #1",
		State:          "open",
		HeadSHA:        "ghi789",
		BaseRefName:    "main",
		Labels:         []string{"looper:plan"},
		Mergeable:      boolPtr(true),
		MergeableState: "blocked",
		AutoMerge:      &githubinfra.PullRequestAutoMerge{EnabledBy: "looper"},
	}
	fixture.github.prCheckRuns["ghi789"] = githubinfra.PullRequestCheckRuns{Statuses: []githubinfra.PullRequestStatus{{Context: "legacy-ci", State: "success"}}}
	fixture.github.branchProtection["main"] = githubinfra.BranchProtection{Enabled: true, HasRequiredChecks: true, RequiredChecks: []string{"legacy-ci"}}
	fixture.github.timeline[1] = []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 79, "html_url": "https://github.com/acme/looper/pull/79"}}}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	for _, op := range fixture.github.ops {
		if op == "delete-comment" || strings.HasPrefix(op, "remove:") {
			t.Fatalf("ops = %v, want no branch-protection cleanup when required status succeeded", fixture.github.ops)
		}
	}
	if len(fixture.github.removedLabels) != 0 || len(fixture.github.createdBodies) != 0 {
		t.Fatalf("removedLabels=%v createdBodies=%v, want watch to stay pending without retriage", fixture.github.removedLabels, fixture.github.createdBodies)
	}
}

func TestRunnerMergeWatchRetriggerSkipsSameTickDispatch(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	fixture.runner.config.Roles.Coordinator.MergeWatch.MaxIndeterminateDuration = "15m"
	firstUnknownAt := fixture.now.Add(-16 * time.Minute)
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged", "dispatch/plan"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number: 1,
		Title:  "Bug",
		Author: "octo",
		Labels: []string{"triaged", "dispatch/plan"},
		Comments: []githubinfra.CommentInfo{{
			ID:        50,
			Author:    "looper",
			Body:      mergeWatchCommentBody(fixture.cfg, 77, "abc123", 3, &firstUnknownAt, nil, "watching"),
			CreatedAt: fixture.now.Format(time.RFC3339),
		}},
		CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339),
	}
	fixture.github.prDetails[77] = githubinfra.PullRequestDetail{
		Number:         77,
		Body:           "Closes #1",
		State:          "open",
		HeadSHA:        "abc123",
		BaseRefName:    "main",
		Labels:         []string{"looper:plan"},
		Mergeable:      boolPtr(false),
		MergeableState: "unknown",
		AutoMerge:      &githubinfra.PullRequestAutoMerge{EnabledBy: "looper"},
	}
	fixture.github.timeline[1] = []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 77, "html_url": "https://github.com/acme/looper/pull/77"}}}}, {"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	assertOrderedOps(t, fixture.github.ops, []string{"remove:triaged,dispatch/plan", "delete-comment"})
	if got := countRemovedIssueOperations(fixture.github.removedLabels, 1, "triaged", "dispatch/plan"); got != 1 {
		t.Fatalf("issue 1 remove count = %d, want 1", got)
	}
	if hasAssignedIssue(fixture.github.assigned, 1) {
		t.Fatalf("assigned = %#v, want no same-tick dispatch assignment after merge-watch cleanup", fixture.github.assigned)
	}
	if len(fixture.github.addedLabels) != 0 {
		t.Fatalf("addedLabels = %#v, want no same-tick dispatch labels after merge-watch cleanup", fixture.github.addedLabels)
	}
	if len(fixture.github.createdBodies) != 0 || len(fixture.github.updatedBodies) != 0 {
		t.Fatalf("createdBodies=%v updatedBodies=%v, want only cleanup mutations", fixture.github.createdBodies, fixture.github.updatedBodies)
	}
}

func TestRunnerAutonomousDispatchBlockedByVetoesSilently(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dispatch.Mode = "autonomous"
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged", "dispatch/plan"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339), Labels: []string{"triaged", "dispatch/plan"}}
	fixture.github.details[9] = githubinfra.IssueDetail{Number: 9, State: "open"}
	fixture.github.timeline[1] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}
	fixture.github.blockedBy[1] = []githubinfra.DependencyIssue{{Number: 9, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "open"}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(fixture.github.ops) != 0 {
		t.Fatalf("ops = %v, want no autonomous dispatch side effects", fixture.github.ops)
	}
}

func TestRunnerBlockedByDependencyReadRetriesTransientErrors(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.Enabled = true
	fixture.runner.config.Roles.Coordinator.Dependencies.APIRetryAttempts = 3
	fixture.github.failBlockedByIssues = map[int64][]error{1: {errors.New("request timed out"), errors.New("request timed out")}}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged", "dispatch/plan"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339), Labels: []string{"triaged", "dispatch/plan"}, Comments: []githubinfra.CommentInfo{{ID: 11, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.details[9] = githubinfra.IssueDetail{Number: 9, State: "open"}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 11, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.timeline[1] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}
	fixture.github.blockedBy[1] = []githubinfra.DependencyIssue{{Number: 9, Repository: githubinfra.IssueRepository{FullName: "acme/looper"}, State: "open"}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if fixture.github.blockedByIssueReads != 3 {
		t.Fatalf("blocked_by issue reads = %d, want 3", fixture.github.blockedByIssueReads)
	}
	assertOrderedOps(t, fixture.github.ops, []string{"create-comment", "react:confused:11"})
}

func TestRunnerDispatchSkipsDependencyAPIsWhenDisabled(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t)
	fixture.runner.config.Roles.Coordinator.Enabled = true
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged", "dispatch/plan"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Bug", Author: "octo", CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339), Labels: []string{"triaged", "dispatch/plan"}, Comments: []githubinfra.CommentInfo{{ID: 11, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.comments[1] = [][]githubinfra.CommentInfo{{{ID: 11, Author: "octo", AuthorAssociation: "MEMBER", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339)}}}
	fixture.github.timeline[1] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if fixture.github.blockedByReads != 0 {
		t.Fatalf("blocked_by reads = %d, want 0 when dependencies are disabled", fixture.github.blockedByReads)
	}
}

func joinLabels(labels []string) string {
	return strings.Join(labels, ",")
}

func seedParentIssue(fixture coordinatorFixture, issueNumber int64) {
	fixture.github.issues = append(fixture.github.issues, githubinfra.IssueSummary{Number: issueNumber})
	fixture.github.details[issueNumber] = githubinfra.IssueDetail{Number: issueNumber, Title: "Parent", Author: "octo", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339)}
}

func seedDispatchIssue(fixture coordinatorFixture, issueNumber int64) {
	seedDispatchIssueWithLabels(fixture, issueNumber, []string{"triaged", "dispatch/plan"})
}

func seedDispatchIssueWithLabels(fixture coordinatorFixture, issueNumber int64, labels []string) {
	fixture.github.issues = append(fixture.github.issues, githubinfra.IssueSummary{Number: issueNumber, Labels: append([]string(nil), labels...)})
	fixture.github.details[issueNumber] = githubinfra.IssueDetail{Number: issueNumber, Title: "Issue", Author: "octo", CreatedAt: fixture.now.Add(-2 * time.Hour).Format(time.RFC3339), Labels: append([]string(nil), labels...), State: "open"}
	fixture.github.timeline[issueNumber] = []map[string]any{{"event": "labeled", "created_at": fixture.now.Add(-time.Hour).Format(time.RFC3339), "label": map[string]any{"name": "triaged"}}}
}

func countRemovedIssueOperations(inputs []githubinfra.IssueLabelsInput, issueNumber int64, labels ...string) int {
	count := 0
	for _, input := range inputs {
		if input.IssueNumber != issueNumber {
			continue
		}
		if joinLabels(input.Labels) == joinLabels(labels) {
			count++
		}
	}
	return count
}

func countAddedIssueOperations(inputs []githubinfra.IssueLabelsInput, issueNumber int64, labels ...string) int {
	count := 0
	for _, input := range inputs {
		if input.IssueNumber != issueNumber {
			continue
		}
		if joinLabels(input.Labels) == joinLabels(labels) {
			count++
		}
	}
	return count
}

func seedRunningQueueItems(t *testing.T, fixture coordinatorFixture, count int) {
	t.Helper()
	repos := storage.NewRepositories(fixture.coord.DB())
	for index := 0; index < count; index++ {
		repo := "acme/looper"
		prNumber := int64(index + 1)
		if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
			ID:          fmt.Sprintf("queue_running_%d", index+1),
			ProjectID:   &fixture.projectID,
			Type:        "worker",
			TargetType:  "issue",
			TargetID:    fmt.Sprintf("issue:%d", index+1),
			Repo:        &repo,
			PRNumber:    &prNumber,
			Priority:    1,
			Status:      "running",
			AvailableAt: fixture.now.Format(time.RFC3339),
			MaxAttempts: 1,
			CreatedAt:   fixture.now.Format(time.RFC3339),
			UpdatedAt:   fixture.now.Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("Queue.Upsert() error = %v", err)
		}
	}
}

func clearRunningQueueItems(t *testing.T, fixture coordinatorFixture) {
	t.Helper()
	if _, err := fixture.coord.DB().ExecContext(context.Background(), `DELETE FROM queue_items WHERE status = 'running'`); err != nil {
		t.Fatalf("delete running queue items: %v", err)
	}
}

func assertAssignedIssueNumbers(t *testing.T, assigned []githubinfra.IssueAssigneesInput, want []int64) {
	t.Helper()
	got := assignedIssueNumbers(assigned)
	if len(got) != len(want) {
		t.Fatalf("assigned issues = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("assigned issues = %v, want %v", got, want)
		}
	}
}

func assignedIssueNumbers(assigned []githubinfra.IssueAssigneesInput) []int64 {
	out := make([]int64, 0, len(assigned))
	for _, input := range assigned {
		out = append(out, input.IssueNumber)
	}
	return out
}

func hasAssignedIssue(inputs []githubinfra.IssueAssigneesInput, issueNumber int64) bool {
	for _, input := range inputs {
		if input.IssueNumber == issueNumber {
			return true
		}
	}
	return false
}

func countOperations(ops []string, prefix string) int {
	count := 0
	for _, op := range ops {
		if strings.HasPrefix(op, prefix) {
			count++
		}
	}
	return count
}

func assertOrderedOps(t *testing.T, ops []string, want []string) {
	t.Helper()
	index := 0
	for _, op := range ops {
		if index < len(want) && op == want[index] {
			index++
		}
	}
	if index != len(want) {
		t.Fatalf("ops = %v, want ordered subsequence %v", ops, want)
	}
}

func containsAll(body string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(body, part) {
			return false
		}
	}
	return true
}

func intToString(value int64) string {
	return strconv.FormatInt(value, 10)
}

func boolPtr(value bool) *bool {
	return &value
}

func mergeWatchCommentBody(cfg *config.Config, prNumber int64, headSHA string, retries int, firstUnknownAt, nextRetryAt *time.Time, summary string) string {
	body := strings.TrimSpace(summary)
	line := "<!-- looper:coordinator:merge-watch pr=" + strconv.FormatInt(prNumber, 10) + " head_sha=" + headSHA + " retries=" + strconv.Itoa(retries) + " first_unknown_at=" + mergeWatchTime(firstUnknownAt) + " next_retry_at=" + mergeWatchTime(nextRetryAt) + " -->"
	if body != "" {
		body += "\n\n" + line
	} else {
		body = line
	}
	return stampedCoordinatorBody(cfg, body)
}

func stampedCoordinatorBody(cfg *config.Config, body string) string {
	return disclosure.FromConfig(*cfg).Markdown(body, "coordinator", disclosure.ChannelIssueComment)
}
