package coordinator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/e2e/harness"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

func coordinatorFakeGHSchema() harness.GHSchema {
	return harness.GHSchema{JSONFieldAllowlist: map[string][]string{
		"issue list": {"number", "title", "body", "url", "state", "updatedAt", "author", "assignees", "labels"},
		"pr list":    {"number", "title", "url", "state", "updatedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "reviewRequests", "reviews", "mergeStateStatus"},
		"pr view":    {"number", "title", "body", "url", "state", "createdAt", "updatedAt", "closedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "reviewRequests", "comments", "reviews", "statusCheckRollup", "mergeStateStatus"},
	}}
}

func TestCoordinatorHappyPathWithFakeGH(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, coordinatorFakeGHSchema())
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	fakeGH.WriteState(t, harness.GHState{
		Commands: map[string]any{"label create": map[string]any{"stdout": json.RawMessage(`{}`)}},
		Routes: map[string]any{
			"repos/acme/looper/issues/1":          json.RawMessage(`{"number":1,"title":"Coordinator bug","body":"triage me","html_url":"https://example.test/issues/1","state":"open","created_at":"2026-05-14T12:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[]}`),
			"repos/acme/looper/issues/1/comments": json.RawMessage(`[[]]`),
			"repos/acme/looper/issues/1/timeline": json.RawMessage(`[[]]`),
		},
	})

	coord, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "coordinator.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coord.Close() })
	if _, err := coord.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coord.DB())
	repoPath := t.TempDir()
	now := time.Date(2026, time.May, 14, 12, 0, 0, 0, time.UTC)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "demo", Name: "Demo", RepoPath: repoPath, CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Coordinator.Enabled = true
	cfg.Disclosure.Enabled = true
	cfg.Disclosure.Channels.IssueComment = true
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: repoPath, Now: func() time.Time { return now }})
	runner := New(Options{Repos: repos, GitHub: gateway, Config: &cfg, Now: func() time.Time { return now }, TriageLLM: stubCoordinatorLLM{}, Inspector: stubCoordinatorInspector{}})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"}); err != nil {
		logBytes, _ := os.ReadFile(fakeGH.InvocationLog)
		t.Fatalf("DiscoverIssues() error = %v\ninvocations:\n%s", err, string(logBytes))
	}
	logBytes, err := os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	logText := string(logBytes)
	assertOrderedText(t, logText,
		`"argv":["issue","list"`,
		`"argv":["label","create","kind/bug"`,
		`"argv":["api","repos/acme/looper/issues/1/labels","--method","POST","-f","labels[]=kind/bug","-f","labels[]=area/coordinator","-f","labels[]=complexity/m","-f","labels[]=dispatch/plan"`,
		`"argv":["api","repos/acme/looper/issues/1/comments","--method","POST"`,
		`"argv":["api","repos/acme/looper/issues/1/labels","--method","POST","-f","labels[]=triaged"`,
	)
	if !(strings.Contains(logText, `<!-- looper:stamp v=1 -->`) || strings.Contains(logText, `\u003c!-- looper:stamp v=1 --\u003e`)) || !strings.Contains(logText, `runner=coordinator`) {
		t.Fatal("invocation log missing stamped coordinator comment body")
	}
}

func TestCoordinatorHumanDispatchWithFakeGH(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, coordinatorFakeGHSchema())
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	fakeGH.WriteState(t, harness.GHState{Commands: map[string]any{"issue list": map[string]any{"stdout": json.RawMessage(`[{"number":2,"title":"Coordinator bug","body":"dispatch me","url":"https://example.test/issues/2","state":"open","updatedAt":"2026-05-14T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}]`)}, "label create": map[string]any{"stdout": json.RawMessage(`{}`)}}, Routes: map[string]any{
		"repos/acme/looper/issues/2":                      json.RawMessage(`{"number":2,"title":"Coordinator bug","body":"dispatch me","html_url":"https://example.test/issues/2","state":"open","created_at":"2026-05-14T11:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}`),
		"repos/acme/looper/issues/2/comments":             json.RawMessage(`[[{"id":17,"body":"/plan","created_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"author_association":"MEMBER"}]]`),
		"repos/acme/looper/issues/2/timeline":             json.RawMessage(`[[{"event":"labeled","created_at":"2026-05-14T11:30:00Z","label":{"name":"triaged"}}]]`),
		"repos/acme/looper/collaborators/octo/permission": json.RawMessage(`{"permission":"write"}`),
	}})

	coord, repos, cfg, repoPath, now := coordinatorFakeGHFixture(t)
	_ = coord
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: repoPath, Now: func() time.Time { return now }})
	runner := New(Options{Repos: repos, GitHub: gateway, Config: &cfg, Now: func() time.Time { return now }, TriageLLM: stubCoordinatorLLM{}, Inspector: stubCoordinatorInspector{}})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"}); err != nil {
		logBytes, _ := os.ReadFile(fakeGH.InvocationLog)
		t.Fatalf("DiscoverIssues() error = %v\ninvocations:\n%s", err, string(logBytes))
	}
	logBytes, err := os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	assertOrderedText(t, string(logBytes),
		`"argv":["api","repos/acme/looper/issues/2/assignees","--method","POST","-f","assignees[]=octocat"]`,
		`"argv":["label","create","looper:plan"`,
		`"argv":["api","repos/acme/looper/issues/2/labels","--method","POST","-f","labels[]=looper:plan"]`,
		`"argv":["api","repos/acme/looper/issues/comments/17/reactions","--method","POST","-H","Accept: application/vnd.github+json","-f","content=+1"]`,
	)
	if strings.Contains(string(logBytes), dispatchFailureCommentMarker) {
		t.Fatal("dispatch happy path unexpectedly posted failure comment")
	}
}

func TestCoordinatorHumanDispatchBlockedByDependencyWithFakeGH(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, coordinatorFakeGHSchema())
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	fakeGH.WriteState(t, harness.GHState{Commands: map[string]any{"issue list": map[string]any{"stdout": json.RawMessage(`[{"number":2,"title":"Coordinator bug","body":"dispatch me","url":"https://example.test/issues/2","state":"open","updatedAt":"2026-05-14T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}]`)}, "label create": map[string]any{"stdout": json.RawMessage(`{}`)}}, Routes: map[string]any{
		"repos/acme/looper/issues/2":                         json.RawMessage(`{"number":2,"title":"Coordinator bug","body":"dispatch me","html_url":"https://example.test/issues/2","state":"open","created_at":"2026-05-14T11:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}`),
		"repos/acme/looper/issues/2/comments":                json.RawMessage(`[[{"id":17,"body":"/plan","created_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"author_association":"MEMBER"}]]`),
		"repos/acme/looper/issues/2/timeline":                json.RawMessage(`[[{"event":"labeled","created_at":"2026-05-14T11:30:00Z","label":{"name":"triaged"}}]]`),
		"repos/acme/looper/issues/2/dependencies/blocked_by": json.RawMessage(`[{"number":1,"state":"open","state_reason":"","repository":{"full_name":"acme/looper"}}]`),
		"repos/acme/looper/collaborators/octo/permission":    json.RawMessage(`{"permission":"write"}`),
	}})

	_, repos, cfg, repoPath, now := coordinatorFakeGHFixture(t)
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: repoPath, Now: func() time.Time { return now }})
	runner := New(Options{Repos: repos, GitHub: gateway, Config: &cfg, Now: func() time.Time { return now }, TriageLLM: stubCoordinatorLLM{}, Inspector: stubCoordinatorInspector{}})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"}); err != nil {
		logBytes, _ := os.ReadFile(fakeGH.InvocationLog)
		t.Fatalf("DiscoverIssues() error = %v\ninvocations:\n%s", err, string(logBytes))
	}
	logBytes, err := os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	assertOrderedText(t, string(logBytes),
		`"argv":["api","--paginate","--slurp","repos/acme/looper/issues/2/dependencies/blocked_by"`,
		`"argv":["api","repos/acme/looper/issues/2/comments","--method","POST"`,
		`"argv":["api","repos/acme/looper/issues/comments/17/reactions","--method","POST","-H","Accept: application/vnd.github+json","-f","content=confused"`,
	)
	if !strings.Contains(string(logBytes), dispatchFailureCommentMarker) && !strings.Contains(string(logBytes), `\u003c!-- looper:coordinator:dispatch-failure --\u003e`) {
		t.Fatal("dependency-blocked human dispatch should reuse dispatch failure marker")
	}
	if strings.Contains(string(logBytes), `labels[]=looper:plan`) {
		t.Fatal("blocked human dispatch should not apply trigger label")
	}
}

func TestCoordinatorAutonomousDispatchWithFakeGH(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, coordinatorFakeGHSchema())
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	fakeGH.WriteState(t, harness.GHState{Commands: map[string]any{"issue list": map[string]any{"stdout": json.RawMessage(`[{"number":3,"title":"Coordinator bug","body":"dispatch me","url":"https://example.test/issues/3","state":"open","updatedAt":"2026-05-14T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}]`)}, "label create": map[string]any{"stdout": json.RawMessage(`{}`)}}, Routes: map[string]any{
		"repos/acme/looper/issues/3":                      json.RawMessage(`{"number":3,"title":"Coordinator bug","body":"dispatch me","html_url":"https://example.test/issues/3","state":"open","created_at":"2026-05-14T09:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}`),
		"repos/acme/looper/issues/3/comments":             json.RawMessage(`[[]]`),
		"repos/acme/looper/issues/3/timeline":             json.RawMessage(`[[{"event":"labeled","created_at":"2026-05-14T10:00:00Z","label":{"name":"triaged"}}]]`),
		"repos/acme/looper/collaborators/octo/permission": json.RawMessage(`{"permission":"write"}`),
	}})

	coord, repos, cfg, repoPath, now := coordinatorFakeGHFixture(t)
	_ = coord
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Dispatch.Mode = "autonomous"
	cfg.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: repoPath, Now: func() time.Time { return now }})
	runner := New(Options{Repos: repos, GitHub: gateway, Config: &cfg, Now: func() time.Time { return now }, TriageLLM: stubCoordinatorLLM{}, Inspector: stubCoordinatorInspector{}})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"}); err != nil {
		logBytes, _ := os.ReadFile(fakeGH.InvocationLog)
		t.Fatalf("DiscoverIssues() error = %v\ninvocations:\n%s", err, string(logBytes))
	}
	logBytes, err := os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	assertOrderedText(t, string(logBytes),
		`"argv":["api","repos/acme/looper/issues/3/assignees","--method","POST","-f","assignees[]=octocat"]`,
		`"argv":["label","create","looper:plan"`,
		`"argv":["api","repos/acme/looper/issues/3/labels","--method","POST","-f","labels[]=looper:plan"]`,
	)
	if strings.Contains(string(logBytes), "/reactions") {
		t.Fatal("autonomous dispatch unexpectedly reacted to a comment")
	}
}

func TestCoordinatorAutonomousDispatchWaitsForBlockedByCompletionWithFakeGH(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, coordinatorFakeGHSchema())
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}

	writeBlockedByState := func(blockerState, blockerStateReason string) {
		stateReasonField := `"state_reason":null`
		if blockerStateReason != "" {
			stateReasonField = `"state_reason":"` + blockerStateReason + `"`
		}
		fakeGH.WriteState(t, harness.GHState{Commands: map[string]any{"issue list": map[string]any{"stdout": json.RawMessage(`[{"number":1,"title":"A","body":"done first","url":"https://example.test/issues/1","state":"open","updatedAt":"2026-05-14T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]},{"number":2,"title":"B","body":"blocked","url":"https://example.test/issues/2","state":"open","updatedAt":"2026-05-14T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}]`)}, "label create": map[string]any{"stdout": json.RawMessage(`{}`)}}, Routes: map[string]any{
			"repos/acme/looper/issues/1":                         json.RawMessage(`{"number":1,"title":"A","body":"done first","html_url":"https://example.test/issues/1","state":"` + blockerState + `",` + stateReasonField + `,"created_at":"2026-05-14T09:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}`),
			"repos/acme/looper/issues/1/comments":                json.RawMessage(`[[]]`),
			"repos/acme/looper/issues/1/timeline":                json.RawMessage(`[[{"event":"labeled","created_at":"2026-05-14T10:00:00Z","label":{"name":"triaged"}}]]`),
			"repos/acme/looper/issues/1/dependencies/blocked_by": json.RawMessage(`[[]]`),
			"repos/acme/looper/issues/2":                         json.RawMessage(`{"number":2,"title":"B","body":"blocked","html_url":"https://example.test/issues/2","state":"open","created_at":"2026-05-14T09:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}`),
			"repos/acme/looper/issues/2/comments":                json.RawMessage(`[[]]`),
			"repos/acme/looper/issues/2/timeline":                json.RawMessage(`[[{"event":"labeled","created_at":"2026-05-14T10:00:00Z","label":{"name":"triaged"}}]]`),
			"repos/acme/looper/issues/2/dependencies/blocked_by": json.RawMessage(`[[{"number":1,"repository":{"full_name":"acme/looper"}}]]`),
		}})
	}

	coord, repos, cfg, repoPath, now := coordinatorFakeGHFixture(t)
	_ = coord
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Dispatch.Mode = "autonomous"
	cfg.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: repoPath, Now: func() time.Time { return now }})
	runner := New(Options{Repos: repos, GitHub: gateway, Config: &cfg, Now: func() time.Time { return now }, TriageLLM: stubCoordinatorLLM{}, Inspector: stubCoordinatorInspector{}})

	writeBlockedByState("open", "")
	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"}); err != nil {
		logBytes, _ := os.ReadFile(fakeGH.InvocationLog)
		t.Fatalf("DiscoverIssues() tick1 error = %v\ninvocations:\n%s", err, string(logBytes))
	}
	logBytes, err := os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	if strings.Contains(string(logBytes), `"argv":["api","repos/acme/looper/issues/2/labels","--method","POST","-f","labels[]=looper:plan"]`) {
		t.Fatal("blocked issue dispatched before blocker completed")
	}

	writeBlockedByState("closed", "completed")
	runner.now = func() time.Time { return now.Add(10 * time.Minute) }
	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"}); err != nil {
		logBytes, _ := os.ReadFile(fakeGH.InvocationLog)
		t.Fatalf("DiscoverIssues() tick2 error = %v\ninvocations:\n%s", err, string(logBytes))
	}
	logBytes, err = os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	if !strings.Contains(string(logBytes), `"argv":["api","repos/acme/looper/issues/2/labels","--method","POST","-f","labels[]=looper:plan"]`) {
		t.Fatal("blocked issue did not dispatch after blocker completed")
	}
}

func TestCoordinatorCycleHandlingWithFakeGH(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, coordinatorFakeGHSchema())
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	fakeGH.WriteState(t, harness.GHState{Commands: map[string]any{"issue list": map[string]any{"stdout": json.RawMessage(`[{"number":1,"title":"A","body":"a","url":"https://example.test/issues/1","state":"open","updatedAt":"2026-05-14T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]},{"number":2,"title":"B","body":"b","url":"https://example.test/issues/2","state":"open","updatedAt":"2026-05-14T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}]`)}, "label create": map[string]any{"stdout": json.RawMessage(`{}`)}}, Routes: map[string]any{
		"repos/acme/looper/issues/1":                         json.RawMessage(`{"number":1,"title":"A","body":"a","html_url":"https://example.test/issues/1","state":"open","state_reason":"","created_at":"2026-05-14T10:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}`),
		"repos/acme/looper/issues/2":                         json.RawMessage(`{"number":2,"title":"B","body":"b","html_url":"https://example.test/issues/2","state":"open","state_reason":"","created_at":"2026-05-14T10:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}`),
		"repos/acme/looper/issues/1/comments":                json.RawMessage(`[[]]`),
		"repos/acme/looper/issues/2/comments":                json.RawMessage(`[[]]`),
		"repos/acme/looper/issues/1/timeline":                json.RawMessage(`[[{"event":"labeled","created_at":"2026-05-14T11:00:00Z","label":{"name":"triaged"}}]]`),
		"repos/acme/looper/issues/2/timeline":                json.RawMessage(`[[{"event":"labeled","created_at":"2026-05-14T11:00:00Z","label":{"name":"triaged"}}]]`),
		"repos/acme/looper/issues/1/dependencies/blocked_by": json.RawMessage(`[{"number":2,"state":"open","state_reason":"","repository":{"full_name":"acme/looper"}}]`),
		"repos/acme/looper/issues/2/dependencies/blocked_by": json.RawMessage(`[{"number":1,"state":"open","state_reason":"","repository":{"full_name":"acme/looper"}}]`),
	}})

	_, repos, cfg, repoPath, now := coordinatorFakeGHFixture(t)
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: repoPath, Now: func() time.Time { return now }})
	runner := New(Options{Repos: repos, GitHub: gateway, Config: &cfg, Now: func() time.Time { return now }, TriageLLM: stubCoordinatorLLM{}, Inspector: stubCoordinatorInspector{}})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"}); err != nil {
		logBytes, _ := os.ReadFile(fakeGH.InvocationLog)
		t.Fatalf("DiscoverIssues() error = %v\ninvocations:\n%s", err, string(logBytes))
	}
	logBytes, err := os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	assertOrderedText(t, string(logBytes),
		`"argv":["api","--paginate","--slurp","repos/acme/looper/issues/1/dependencies/blocked_by"`,
		`"argv":["api","repos/acme/looper/issues/1/labels/triaged","--method","DELETE"]`,
		`"argv":["api","repos/acme/looper/issues/1/labels/dispatch%2Fplan","--method","DELETE"]`,
		`"argv":["api","repos/acme/looper/issues/1/comments","--method","POST"`,
		`"argv":["api","repos/acme/looper/issues/2/labels/triaged","--method","DELETE"]`,
		`"argv":["api","repos/acme/looper/issues/2/labels/dispatch%2Fplan","--method","DELETE"]`,
	)
	if !strings.Contains(string(logBytes), cycleCommentMarker) && !strings.Contains(string(logBytes), `\u003c!-- looper:coordinator:cycle --\u003e`) {
		t.Fatal("cycle marker missing from fake-gh invocation log")
	}
}

func TestCoordinatorNotPlannedRetriageWithFakeGH(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, coordinatorFakeGHSchema())
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	fakeGH.WriteState(t, harness.GHState{Commands: map[string]any{"issue list": map[string]any{"stdout": json.RawMessage(`[{"number":2,"title":"B","body":"b","url":"https://example.test/issues/2","state":"open","updatedAt":"2026-05-14T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}]`)}, "label create": map[string]any{"stdout": json.RawMessage(`{}`)}}, Routes: map[string]any{
		"repos/acme/looper/issues/1":                         json.RawMessage(`{"number":1,"title":"A","body":"a","html_url":"https://example.test/issues/1","state":"closed","state_reason":"not_planned","created_at":"2026-05-14T10:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[]}`),
		"repos/acme/looper/issues/2":                         json.RawMessage(`{"number":2,"title":"B","body":"b","html_url":"https://example.test/issues/2","state":"open","state_reason":"","created_at":"2026-05-14T10:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/plan"}]}`),
		"repos/acme/looper/issues/2/comments":                json.RawMessage(`[[]]`),
		"repos/acme/looper/issues/2/timeline":                json.RawMessage(`[[{"event":"labeled","created_at":"2026-05-14T11:00:00Z","label":{"name":"triaged"}}]]`),
		"repos/acme/looper/issues/2/dependencies/blocked_by": json.RawMessage(`[{"number":1,"state":"closed","state_reason":"not_planned","repository":{"full_name":"acme/looper"}}]`),
	}})

	_, repos, cfg, repoPath, now := coordinatorFakeGHFixture(t)
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: repoPath, Now: func() time.Time { return now }})
	runner := New(Options{Repos: repos, GitHub: gateway, Config: &cfg, Now: func() time.Time { return now }, TriageLLM: stubCoordinatorLLM{}, Inspector: stubCoordinatorInspector{}})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"}); err != nil {
		logBytes, _ := os.ReadFile(fakeGH.InvocationLog)
		t.Fatalf("DiscoverIssues() error = %v\ninvocations:\n%s", err, string(logBytes))
	}
	logBytes, err := os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	if !strings.Contains(string(logBytes), `"argv":["api","repos/acme/looper/issues/2/labels/triaged","--method","DELETE"]`) || !strings.Contains(string(logBytes), `"argv":["api","repos/acme/looper/issues/2/labels/dispatch%2Fplan","--method","DELETE"]`) {
		t.Fatalf("expected label removal in log\n%s", string(logBytes))
	}
	if strings.Contains(string(logBytes), cycleCommentMarker) {
		t.Fatal("not_planned flow should not post a cycle comment")
	}
}

func TestCoordinatorTieBreakWithFakeGH(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, coordinatorFakeGHSchema())
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	fakeGH.WriteState(t, harness.GHState{Commands: map[string]any{"issue list": map[string]any{"stdout": json.RawMessage(`[{"number":10,"title":"Parent","body":"p","url":"https://example.test/issues/10","state":"open","updatedAt":"2026-05-14T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[]},{"number":11,"title":"A","body":"a","url":"https://example.test/issues/11","state":"open","updatedAt":"2026-05-14T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"},{"name":"dispatch/implement"}]},{"number":12,"title":"B","body":"b","url":"https://example.test/issues/12","state":"open","updatedAt":"2026-05-14T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"},{"name":"dispatch/implement"}]},{"number":13,"title":"C","body":"c","url":"https://example.test/issues/13","state":"open","updatedAt":"2026-05-14T12:00:00Z","author":{"login":"octo"},"assignees":[],"labels":[{"name":"triaged"},{"name":"dispatch/implement"}]}]`)}, "label create": map[string]any{"stdout": json.RawMessage(`{}`)}}, Routes: map[string]any{
		"repos/acme/looper/issues/10":                         json.RawMessage(`{"number":10,"title":"Parent","body":"p","html_url":"https://example.test/issues/10","state":"open","state_reason":"","created_at":"2026-05-14T09:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[]}`),
		"repos/acme/looper/issues/11":                         json.RawMessage(`{"number":11,"title":"A","body":"a","html_url":"https://example.test/issues/11","state":"open","state_reason":"","created_at":"2026-05-14T09:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/implement"}]}`),
		"repos/acme/looper/issues/12":                         json.RawMessage(`{"number":12,"title":"B","body":"b","html_url":"https://example.test/issues/12","state":"open","state_reason":"","created_at":"2026-05-14T09:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/implement"}]}`),
		"repos/acme/looper/issues/13":                         json.RawMessage(`{"number":13,"title":"C","body":"c","html_url":"https://example.test/issues/13","state":"open","state_reason":"","created_at":"2026-05-14T09:00:00Z","updated_at":"2026-05-14T12:00:00Z","user":{"login":"octo"},"labels":[{"name":"triaged"},{"name":"dispatch/implement"}]}`),
		"repos/acme/looper/issues/10/comments":                json.RawMessage(`[[]]`),
		"repos/acme/looper/issues/11/comments":                json.RawMessage(`[[]]`),
		"repos/acme/looper/issues/12/comments":                json.RawMessage(`[[]]`),
		"repos/acme/looper/issues/13/comments":                json.RawMessage(`[[]]`),
		"repos/acme/looper/issues/10/timeline":                json.RawMessage(`[[]]`),
		"repos/acme/looper/issues/11/timeline":                json.RawMessage(`[[{"event":"labeled","created_at":"2026-05-14T10:00:00Z","label":{"name":"triaged"}}]]`),
		"repos/acme/looper/issues/12/timeline":                json.RawMessage(`[[{"event":"labeled","created_at":"2026-05-14T10:00:00Z","label":{"name":"triaged"}}]]`),
		"repos/acme/looper/issues/13/timeline":                json.RawMessage(`[[{"event":"labeled","created_at":"2026-05-14T10:00:00Z","label":{"name":"triaged"}}]]`),
		"repos/acme/looper/issues/11/dependencies/blocked_by": json.RawMessage(`[]`),
		"repos/acme/looper/issues/12/dependencies/blocked_by": json.RawMessage(`[]`),
		"repos/acme/looper/issues/13/dependencies/blocked_by": json.RawMessage(`[]`),
		"repos/acme/looper/issues/10/sub_issues":              json.RawMessage(`[{"number":12},{"number":11},{"number":13}]`),
		"repos/acme/looper/issues/11/sub_issues":              json.RawMessage(`[]`),
		"repos/acme/looper/issues/12/sub_issues":              json.RawMessage(`[]`),
		"repos/acme/looper/issues/13/sub_issues":              json.RawMessage(`[]`),
	}})

	_, repos, cfg, repoPath, now := coordinatorFakeGHFixture(t)
	cfg.Roles.Coordinator.Enabled = true
	cfg.Roles.Coordinator.Dependencies.Enabled = true
	cfg.Roles.Coordinator.Dispatch.Mode = "autonomous"
	cfg.Roles.Coordinator.Dispatch.AssignTo = "octocat"
	cfg.Scheduler.MaxConcurrentRuns = 2
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: repoPath, Now: func() time.Time { return now }})
	runner := New(Options{Repos: repos, GitHub: gateway, Config: &cfg, Now: func() time.Time { return now }, TriageLLM: stubCoordinatorLLM{}, Inspector: stubCoordinatorInspector{}})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "demo", Repo: "acme/looper"}); err != nil {
		logBytes, _ := os.ReadFile(fakeGH.InvocationLog)
		t.Fatalf("DiscoverIssues() error = %v\ninvocations:\n%s", err, string(logBytes))
	}
	logBytes, err := os.ReadFile(fakeGH.InvocationLog)
	if err != nil {
		t.Fatalf("ReadFile(invocation log) error = %v", err)
	}
	assertOrderedText(t, string(logBytes),
		`"argv":["api","--paginate","--slurp","repos/acme/looper/issues/10/sub_issues"`,
		`"argv":["api","repos/acme/looper/issues/12/assignees","--method","POST","-f","assignees[]=octocat"`,
		`"argv":["api","repos/acme/looper/issues/12/labels","--method","POST","-f","labels[]=looper:worker-ready"`,
		`"argv":["api","repos/acme/looper/issues/11/assignees","--method","POST","-f","assignees[]=octocat"`,
		`"argv":["api","repos/acme/looper/issues/11/labels","--method","POST","-f","labels[]=looper:worker-ready"`,
	)
	if strings.Contains(string(logBytes), `repos/acme/looper/issues/13/assignees`) {
		t.Fatal("third child should remain queued for next tick")
	}
}

func coordinatorFakeGHFixture(t *testing.T) (*storage.SQLiteCoordinator, *storage.Repositories, config.Config, string, time.Time) {
	t.Helper()
	coord, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "coordinator.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coord.Close() })
	if _, err := coord.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coord.DB())
	repoPath := t.TempDir()
	now := time.Date(2026, time.May, 14, 12, 0, 0, 0, time.UTC)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "demo", Name: "Demo", RepoPath: repoPath, CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Disclosure.Enabled = true
	cfg.Disclosure.Channels.IssueComment = true
	return coord, repos, cfg, repoPath, now
}

func assertOrderedText(t *testing.T, text string, parts ...string) {
	t.Helper()
	index := 0
	for _, part := range parts {
		next := strings.Index(text[index:], part)
		if next < 0 {
			t.Fatalf("text missing ordered part %q\n%s", part, text)
		}
		index += next + len(part)
	}
}

var _ triage.LLM = stubCoordinatorLLM{}
