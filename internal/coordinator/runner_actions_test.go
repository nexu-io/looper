package coordinator

import (
	"context"
	"errors"
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

type coordinatorFixture struct {
	runner    *Runner
	github    *stubCoordinatorGitHub
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
	cfg.Disclosure.Enabled = true
	cfg.Disclosure.Channels.IssueComment = true
	github := &stubCoordinatorGitHub{details: map[int64]githubinfra.IssueDetail{}, comments: map[int64][][]githubinfra.CommentInfo{}, timeline: map[int64][]map[string]any{}}
	runner := New(Options{Repos: repos, GitHub: github, Config: &cfg, Now: func() time.Time { return now }, TriageLLM: stubCoordinatorLLM{}, Inspector: stubCoordinatorInspector{}})
	return coordinatorFixture{runner: runner, github: github, cfg: &cfg, projectID: projectID, now: now, coord: coord}
}

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
	issues        []githubinfra.IssueSummary
	details       map[int64]githubinfra.IssueDetail
	comments      map[int64][][]githubinfra.CommentInfo
	timeline      map[int64][]map[string]any
	permissionErr error
	ops           []string
	createdBodies []string
	updatedBodies []string
	commentReads  map[int64]int
	failAddLabels map[string]error
}

func (s *stubCoordinatorGitHub) ListOpenIssues(context.Context, githubinfra.ListOpenIssuesInput) ([]githubinfra.IssueSummary, error) {
	return append([]githubinfra.IssueSummary(nil), s.issues...), nil
}
func (s *stubCoordinatorGitHub) ViewIssue(_ context.Context, input githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error) {
	return s.details[input.IssueNumber], nil
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
func (s *stubCoordinatorGitHub) GetCurrentUserLogin(context.Context, string) (string, error) {
	return "looper", nil
}
func (s *stubCoordinatorGitHub) GetCurrentUserLoginForRepo(context.Context, string, string) (string, error) {
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
func (s *stubCoordinatorGitHub) AddIssueAssignees(_ context.Context, input githubinfra.IssueAssigneesInput) error {
	s.ops = append(s.ops, "assign:"+joinLabels(input.Assignees))
	return nil
}
func (s *stubCoordinatorGitHub) AddIssueLabels(_ context.Context, input githubinfra.IssueLabelsInput) error {
	s.ops = append(s.ops, "add:"+joinLabels(input.Labels))
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

func joinLabels(labels []string) string {
	return strings.Join(labels, ",")
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

func stampedCoordinatorBody(cfg *config.Config, body string) string {
	return disclosure.FromConfig(*cfg).Markdown(body, "coordinator", disclosure.ChannelIssueComment)
}
