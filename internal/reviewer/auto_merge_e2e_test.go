package reviewer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	coordpkg "github.com/nexu-io/looper/internal/coordinator"
	coordtriage "github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/e2e/harness"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/reviewer/criteria"
	"github.com/nexu-io/looper/internal/storage"
)

func TestReviewerAutoMergeWaitsForMergeBeforeUnblockingDependentDispatchWithFakeGH(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, harness.GHSchema{JSONFieldAllowlist: map[string][]string{
		"issue list": {"number", "title", "body", "url", "state", "updatedAt", "author", "assignees", "labels"},
		"pr list":    {"number", "title", "url", "state", "updatedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "reviewRequests", "reviews", "mergeStateStatus"},
		"pr view":    {"number", "title", "body", "url", "state", "createdAt", "updatedAt", "closedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "authorAssociation", "reviewRequests", "comments", "reviews", "statusCheckRollup", "mergeStateStatus"},
	}})
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	issueListAllOpen := json.RawMessage(`[{"number":1,"title":"Parent","body":"## Acceptance criteria\n- ship app change\n- add more\n","url":"https://example.test/issues/1","state":"open","updatedAt":"2026-04-11T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"}]},{"number":2,"title":"Dependent","body":"dispatch me later","url":"https://example.test/issues/2","state":"open","updatedAt":"2026-04-11T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}]`)
	issueListDependentOnly := json.RawMessage(`[{"number":2,"title":"Dependent","body":"dispatch me later","url":"https://example.test/issues/2","state":"open","updatedAt":"2026-04-11T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}]`)
	fakeGH.WriteState(t, harness.GHState{
		Commands: map[string]any{
			"issue list":   map[string]any{"stdout": issueListAllOpen},
			"label create": map[string]any{"stdout": json.RawMessage(`{}`)},
			"pr diff":      map[string]any{"stdout": json.RawMessage(`"diff --git a/app.go b/app.go\n@@ -1,1 +1,2 @@\n-old\n+new\n+more\n"`)},
		},
		Routes: map[string]any{
			"repos/acme/looper":                                  json.RawMessage(`{"allow_squash_merge":true,"allow_merge_commit":true,"allow_rebase_merge":true,"allow_auto_merge":true}`),
			"repos/acme/looper/branches/main/protection":         json.RawMessage(`{"required_status_checks":{"contexts":["ci"]}}`),
			"repos/acme/looper/issues/1":                         json.RawMessage(`{"number":1,"title":"Parent","body":"## Acceptance criteria\n- ship app change\n- add more\n","html_url":"https://example.test/issues/1","state":"open","created_at":"2026-04-11T10:00:00Z","updated_at":"2026-04-11T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"}]}`),
			"repos/acme/looper/issues/2":                         json.RawMessage(`{"number":2,"title":"Dependent","body":"dispatch me later","html_url":"https://example.test/issues/2","state":"open","created_at":"2026-04-11T10:00:00Z","updated_at":"2026-04-11T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}`),
			"repos/acme/looper/issues/1/comments":                json.RawMessage(`[[]]`),
			"repos/acme/looper/issues/2/comments":                json.RawMessage(`[[]]`),
			"repos/acme/looper/issues/1/timeline":                json.RawMessage(`[[]]`),
			"repos/acme/looper/issues/2/timeline":                json.RawMessage(`[[{"event":"labeled","created_at":"2026-04-11T11:00:00Z","label":{"name":"triaged"}}]]`),
			"repos/acme/looper/issues/1/dependencies/blocked_by": json.RawMessage(`[[]]`),
			"repos/acme/looper/issues/2/dependencies/blocked_by": json.RawMessage(`[[{"number":1,"repository":{"full_name":"acme/looper"}}]]`),
		},
		CurrentUserLogin: "reviewer",
		PullRequests: map[string]harness.GHPullRequest{
			"acme/looper#42": {Number: 42, Repo: "acme/looper", Title: "Worker PR", Body: "Implements feature.\n\nCloses #1", State: "OPEN", Labels: []string{"looper:worker-ready"}, HeadRefName: "feature/worker-pr", BaseRefName: "main", HeadSHA: "abc123", BaseSHA: "base123", Author: "octocat", ReviewRequests: []string{"reviewer"}},
		},
	})

	fixture := newRunnerFixture(t)
	ctx := context.Background()
	repoPath := t.TempDir()
	baseBranch := "main"
	if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: repoPath, BaseBranch: &baseBranch, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	cfg := reviewerAutoMergeTestConfig(t)
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.PollInterval = "0s"
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	cfg.Roles.Coordinator.Dispatch.Mode = "autonomous"
	cfg.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	cfg.Scheduler.MaxConcurrentRuns = 2
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: repoPath, Now: fixture.now})
	reviewerRunner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: reviewerIntegrationGatewayAdapter{Gateway: gateway}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "No actionable findings", Stdout: `__LOOPER_RESULT__={"summary":"No actionable findings"}`, ParseStatus: "parsed"}}}, Logger: fixture.logger, Now: fixture.now, ReviewEvents: config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventApprove}, LoopConfig: testReviewerLoopConfig(), CustomInstructions: cfg, CriteriaVerifier: stubCriteriaVerifier{responses: map[criteria.AcceptanceCriterion]criteria.CriterionAssessment{"ship app change": {Verdict: criteria.VerdictPass, Justification: "present in diff", Evidence: []criteria.Evidence{{FilePath: "app.go", StartLine: 1, EndLine: 2}}}, "add more": {Verdict: criteria.VerdictPass, Justification: "present in diff", Evidence: []criteria.Evidence{{FilePath: "app.go", StartLine: 2, EndLine: 2}}}}}})
	coordinatorRunner := coordpkg.New(coordpkg.Options{Repos: fixture.repos, GitHub: gateway, Config: cfg, Now: fixture.now, TriageLLM: coordStubLLM{}, Inspector: coordStubInspector{}})

	if _, err := coordinatorRunner.DiscoverIssues(ctx, coordpkg.DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("Coordinator first tick error = %v", err)
	}
	logBeforeReview, err := os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	if strings.Contains(string(logBeforeReview), `labels[]=looper:plan`) {
		t.Fatalf("dependent issue dispatched before blocker merged:\n%s", string(logBeforeReview))
	}

	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
	loop := storage.LoopRecord{ID: "loop_fakegh_auto_merge_e2e", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue, err := reviewerRunner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "reviewer-worker-1", "reviewer")
	if err != nil || claimed == nil || claimed.ID != queue.ID {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want queue item %s", claimed, err, queue.ID)
	}
	result, err := reviewerRunner.ProcessClaimedItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("reviewer result = %#v", result)
	}

	state := readFakeGHState(t, fakeGH.StatePath)
	state.Commands["issue list"] = map[string]any{"stdout": issueListDependentOnly}
	fakeGH.WriteState(t, state)

	if _, err := coordinatorRunner.DiscoverIssues(ctx, coordpkg.DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		logBytes, _ := os.ReadFile(fakeGH.InvocationLog)
		t.Fatalf("Coordinator second tick error = %v\ninvocations:\n%s", err, string(logBytes))
	}
	logBytes, err := os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	assertOrderedText(t, string(logBytes),
		`"argv":["api","repos/acme/looper/pulls/42/reviews","--method","POST","--input","-","--include"]`,
		`"argv":["pr","merge","42","--repo","acme/looper","--auto","--squash","--match-head-commit","abc123"]`,
	)
	state = readFakeGHState(t, fakeGH.StatePath)
	pendingPR := state.PullRequests["acme/looper#42"]
	if pendingPR.State != "OPEN" {
		t.Fatalf("pull request state = %q, want OPEN while auto-merge is pending", pendingPR.State)
	}
	if pendingPR.AutoMerge == nil {
		t.Fatal("pull request auto-merge metadata = nil, want pending auto-merge state")
	}
	issueOnePayload, ok := state.Routes["repos/acme/looper/issues/1"]
	if !ok {
		t.Fatal("missing issue #1 route after merge")
	}
	issueOneJSON, err := json.Marshal(issueOnePayload)
	if err != nil {
		t.Fatalf("Marshal(issue #1 route) error = %v", err)
	}
	if !strings.Contains(string(issueOneJSON), `"state":"open"`) {
		t.Fatalf("issue #1 route = %s, want open state before merge completes", string(issueOneJSON))
	}
	if strings.Contains(string(logBytes), `"argv":["api","repos/acme/looper/issues/2/labels","--method","POST","-f","labels[]=looper:plan"]`) {
		t.Fatalf("dependent issue dispatched before auto-merge completed:\n%s", string(logBytes))
	}
	if !strings.Contains(string(logBytes), criteriaVerificationHeading) {
		t.Fatalf("invocation log missing criteria verification heading:\n%s", string(logBytes))
	}

	state.PullRequests["acme/looper#42"] = harness.GHPullRequest{
		Number:         pendingPR.Number,
		Repo:           pendingPR.Repo,
		Title:          pendingPR.Title,
		Body:           pendingPR.Body,
		URL:            pendingPR.URL,
		State:          "MERGED",
		CreatedAt:      pendingPR.CreatedAt,
		UpdatedAt:      pendingPR.UpdatedAt,
		ClosedAt:       "2026-05-12T00:00:00Z",
		IsDraft:        pendingPR.IsDraft,
		ReviewDecision: pendingPR.ReviewDecision,
		Labels:         pendingPR.Labels,
		HeadRefName:    pendingPR.HeadRefName,
		BaseRefName:    pendingPR.BaseRefName,
		HeadSHA:        pendingPR.HeadSHA,
		BaseSHA:        pendingPR.BaseSHA,
		Author:         pendingPR.Author,
		ReviewRequests: pendingPR.ReviewRequests,
		MergedAt:       "2026-05-12T00:00:00Z",
	}
	state.Routes["repos/acme/looper/issues/1"] = json.RawMessage(`{"number":1,"title":"Parent","body":"## Acceptance criteria\n- ship app change\n- add more\n","html_url":"https://example.test/issues/1","state":"closed","state_reason":"completed","created_at":"2026-04-11T10:00:00Z","updated_at":"2026-05-12T00:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"}]}`)
	fakeGH.WriteState(t, state)

	if _, err := coordinatorRunner.DiscoverIssues(ctx, coordpkg.DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		logBytes, _ := os.ReadFile(fakeGH.InvocationLog)
		t.Fatalf("Coordinator third tick error = %v\ninvocations:\n%s", err, string(logBytes))
	}
	logBytes, err = os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	assertOrderedText(t, string(logBytes),
		`"argv":["pr","merge","42","--repo","acme/looper","--auto","--squash","--match-head-commit","abc123"]`,
		`"argv":["api","repos/acme/looper/issues/2/assignees","--method","POST","-f","assignees[]=octocat"]`,
		`"argv":["api","repos/acme/looper/issues/2/labels","--method","POST","-f","labels[]=looper:plan"]`,
	)
}

type coordStubLLM struct{}

func (coordStubLLM) Complete(context.Context, coordtriage.Request) (string, error) {
	return `{"disposition":"valid","comment":"Looks actionable.","labels":{"kind":["kind/bug"],"area":["area/coordinator"],"complexity":["complexity/m"],"dispatch":["dispatch/plan"]}}`, nil
}

type coordStubInspector struct{}

func (coordStubInspector) Inspect(context.Context, string, coordtriage.Issue) (coordtriage.RepoContext, error) {
	return coordtriage.RepoContext{}, nil
}

func readFakeGHState(t *testing.T, statePath string) harness.GHState {
	t.Helper()
	payload, err := os.ReadFile(filepath.Clean(statePath))
	if err != nil {
		t.Fatalf("ReadFile(fake-gh state) error = %v", err)
	}
	var state harness.GHState
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatalf("Unmarshal(fake-gh state) error = %v", err)
	}
	if state.Commands == nil {
		state.Commands = map[string]any{}
	}
	if state.Routes == nil {
		state.Routes = map[string]any{}
	}
	return state
}
