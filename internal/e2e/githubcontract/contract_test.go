package githubcontract

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/e2e/harness"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/sweeper"
)

type fakeGHState struct {
	Routes  map[string]json.RawMessage `json:"routes,omitempty"`
	GraphQL map[string]json.RawMessage `json:"graphql,omitempty"`
}

func TestInvariantGatewayUsesSupportedGHJSONFields(t *testing.T) {
	bins := harness.MustBinaries(t)
	schema := loadFixtureSchema(t)
	fakeGH := harness.NewFakeGH(t, bins, schema)
	writeFakeState(t, fakeGH.StatePath, fakeGHState{
		Routes: map[string]json.RawMessage{
			"repos/acme/looper/issues/7": json.RawMessage(`{"number":7,"title":"Issue title","body":"body","html_url":"https://github.com/acme/looper/issues/7","state":"open","updated_at":"2026-05-12T00:00:00Z","user":{"login":"octocat"},"author_association":"COLLABORATOR","assignees":[{"login":"octocat"}],"labels":[{"name":"bug"}]}`),
		},
		GraphQL: map[string]json.RawMessage{
			"default":             json.RawMessage(`{"data":{"node":{"id":"thread-1","isResolved":false,"path":"foo.go","line":7,"comments":{"nodes":[{"id":"comment-1","body":"please fix","author":{"login":"octocat"},"createdAt":"2026-05-12T00:00:00Z","updatedAt":"2026-05-12T00:00:00Z","path":"foo.go","line":7,"originalCommit":{"oid":"base-head"},"commit":{"oid":"head-1"},"url":"https://example.test/thread-1"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`),
			"resolveReviewThread": json.RawMessage(`{"data":{"resolveReviewThread":{"thread":{"id":"thread-1","isResolved":true}}}}`),
		},
	})
	root := t.TempDir()
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	t.Setenv("HOME", root)
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: root})

	ctx := context.Background()
	issues, err := gateway.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{Repo: "acme/looper", CWD: root, Limit: 5, Assignee: "reviewer", Label: "phase-1"})
	if err != nil {
		t.Fatalf("ListOpenIssues() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("ListOpenIssues() len = %d, want 1", len(issues))
	}
	if issues[0].AuthorAssociation != "" {
		t.Fatalf("issues[0].AuthorAssociation = %q, want empty unsupported field", issues[0].AuthorAssociation)
	}
	prs, err := gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: "acme/looper", CWD: root, Limit: 5})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("ListOpenPullRequests() len = %d, want 1", len(prs))
	}
	if prs[0].AuthorAssociation != "" {
		t.Fatalf("prs[0].AuthorAssociation = %q, want empty unsupported field", prs[0].AuthorAssociation)
	}
	issue, err := gateway.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: "acme/looper", IssueNumber: 7, CWD: root})
	if err != nil {
		t.Fatalf("ViewIssue() error = %v", err)
	}
	if issue.AuthorAssociation != "COLLABORATOR" {
		t.Fatalf("issue.AuthorAssociation = %q, want COLLABORATOR", issue.AuthorAssociation)
	}
	if err := gateway.ResolveReviewThread(ctx, githubinfra.ResolveReviewThreadInput{Repo: "acme/looper", ThreadID: "thread-1", CWD: root}); err != nil {
		t.Fatalf("ResolveReviewThread() error = %v", err)
	}
	invocations := readInvocationsForContract(t, fakeGH.InvocationLog)
	assertInvocationHasJSONFields(t, invocations, "issue", "list", []string{"number", "title", "body", "url", "state", "updatedAt", "author", "assignees", "labels"})
	assertInvocationMissingJSONField(t, invocations, "issue", "list", "authorAssociation")
	assertInvocationHasJSONFields(t, invocations, "pr", "list", []string{"number", "title", "url", "state", "updatedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "reviewRequests", "reviews", "mergeStateStatus"})
	assertInvocationMissingJSONField(t, invocations, "pr", "list", "authorAssociation")
	assertInvocationContains(t, invocations, []string{"api", "repos/acme/looper/issues/7"})
	assertInvocationContains(t, invocations, []string{"api", "graphql"})
}

func TestRegressionPR261FallsBackToIssueDetailForPRAuthorAssociation(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, loadFixtureSchema(t))
	fakeGH.WriteState(t, harness.GHState{
		Commands: map[string]any{
			"pr list": map[string]any{
				"stdout": []map[string]any{{
					"number":           42,
					"title":            "stale pr",
					"url":              "https://example.test/acme/looper/pull/42",
					"state":            "OPEN",
					"updatedAt":        "2026-05-12T00:00:00Z",
					"isDraft":          false,
					"reviewDecision":   "",
					"labels":           []map[string]any{},
					"headRefName":      "feature/pr-42",
					"baseRefName":      "main",
					"headRefOid":       "head-42",
					"baseRefOid":       "base-42",
					"author":           map[string]any{"login": "octocat"},
					"reviewRequests":   []map[string]any{},
					"reviews":          []map[string]any{},
					"mergeStateStatus": "CLEAN",
				}},
			},
		},
		Routes: map[string]any{
			"repos/acme/looper/issues/42": map[string]any{
				"number":             42,
				"title":              "stale pr",
				"body":               "body",
				"html_url":           "https://example.test/acme/looper/issues/42",
				"state":              "open",
				"updated_at":         "2026-05-12T00:00:00Z",
				"user":               map[string]any{"login": "octocat"},
				"author_association": "MEMBER",
				"labels":             []map[string]any{},
			},
		},
	})
	root := t.TempDir()
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	t.Setenv("HOME", root)
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	defer coordinator.Close()
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.May, 12, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format("2006-01-02T15:04:05.000Z")
	projectRepoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(projectRepoPath, 0o755); err != nil {
		t.Fatalf("mkdir repo path: %v", err)
	}
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Demo", RepoPath: projectRepoPath, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Sweeper.AutoDiscovery = true
	cfg.Roles.Sweeper.Triggers.IncludePullRequests = true
	cfg.Roles.Sweeper.Triggers.ExcludeAuthorAssociations = []string{"MEMBER"}
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: root})
	runner := sweeper.New(sweeper.Options{Repos: repos, GitHub: gateway, Config: &cfg, Now: func() time.Time { return now }})
	result, err := runner.DiscoverPullRequests(context.Background(), sweeper.DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.QueueItems) != 0 || result.Skipped != 1 {
		t.Fatalf("DiscoverPullRequests() = %#v, want excluded PR skipped after detail fallback", result)
	}
	invocations := readInvocationsForContract(t, fakeGH.InvocationLog)
	assertInvocationMissingJSONField(t, invocations, "pr", "list", "authorAssociation")
	assertInvocationContains(t, invocations, []string{"api", "repos/acme/looper/issues/42"})
}

func TestInvariantGatewaySupportsRepoForms(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, loadFixtureSchema(t))
	writeFakeState(t, fakeGH.StatePath, fakeGHState{
		Routes: map[string]json.RawMessage{
			"repos/acme/looper/issues/11": json.RawMessage(`{"number":11,"title":"Issue title","body":"body","html_url":"https://example.test/acme/looper/issues/11","state":"open","updated_at":"2026-05-12T00:00:00Z","user":{"login":"octocat"}}`),
		},
	})
	root := t.TempDir()
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	t.Setenv("HOME", root)
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: root})
	for _, repo := range []string{"acme/looper", "github.com/acme/looper", "ghe.example.com/acme/looper"} {
		if _, err := gateway.ViewIssue(context.Background(), githubinfra.ViewIssueInput{Repo: repo, IssueNumber: 11, CWD: root}); err != nil {
			t.Fatalf("ViewIssue(%q) error = %v", repo, err)
		}
	}
	invocations := readInvocationsForContract(t, fakeGH.InvocationLog)
	assertInvocationContains(t, invocations, []string{"api", "repos/acme/looper/issues/11"})
	assertInvocationContains(t, invocations, []string{"api", "repos/acme/looper/issues/11", "--hostname", "github.com"})
	assertInvocationContains(t, invocations, []string{"api", "repos/acme/looper/issues/11", "--hostname", "ghe.example.com"})
}

func TestFakeGHFixtureRejectsUnsupportedJSONField(t *testing.T) {
	t.Parallel()
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, loadFixtureSchema(t))
	cmd := exec.Command(fakeGH.Path, "pr", "list", "--json", "number,authorAssociation")
	cmd.Env = append(os.Environ(), flattenEnv(fakeGH.EnvMap())...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected unsupported field failure")
	}
	if !strings.Contains(string(output), `unknown JSON field: "authorAssociation"`) {
		t.Fatalf("output = %s, want unsupported-field error", string(output))
	}
}

func TestRealGHReadOnlySmoke(t *testing.T) {
	if os.Getenv("LOOPER_E2E_REAL_GH") == "" {
		t.Skip("set LOOPER_E2E_REAL_GH=1 to run real-gh smoke")
	}
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		t.Skipf("gh not available: %v", err)
	}
	root := t.TempDir()
	gateway := githubinfra.New(githubinfra.Options{GHPath: ghPath, CWD: root})
	if _, err := gateway.ListOpenPullRequests(context.Background(), githubinfra.ListOpenPullRequestsInput{Repo: "nexu-io/looper", CWD: root, Limit: 1}); err != nil {
		t.Fatalf("real-gh pr list smoke failed; fixture may be stale: %v", err)
	}
}

func loadFixtureSchema(tb testing.TB) harness.GHSchema {
	tb.Helper()
	path := filepath.Join("testdata", "gh-schema", "schema.json")
	payload, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read gh schema fixture: %v", err)
	}
	var schema harness.GHSchema
	if err := json.Unmarshal(payload, &schema); err != nil {
		tb.Fatalf("decode gh schema fixture: %v", err)
	}
	return schema
}

func writeFakeState(tb testing.TB, path string, state fakeGHState) {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatalf("mkdir fake-gh state dir: %v", err)
	}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		tb.Fatalf("marshal fake-gh state: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		tb.Fatalf("write fake-gh state: %v", err)
	}
}

func flattenEnv(env map[string]string) []string {
	items := make([]string, 0, len(env))
	for key, value := range env {
		items = append(items, key+"="+value)
	}
	return items
}

func readInvocationsForContract(tb testing.TB, path string) []map[string]any {
	tb.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read invocation log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(payload)), "\n")
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			tb.Fatalf("decode invocation line %q: %v", line, err)
		}
		out = append(out, item)
	}
	return out
}

func assertInvocationContains(tb testing.TB, invocations []map[string]any, want []string) {
	tb.Helper()
	for _, invocation := range invocations {
		argv, _ := invocation["argv"].([]any)
		parts := make([]string, 0, len(argv))
		for _, part := range argv {
			parts = append(parts, part.(string))
		}
		if containsOrdered(parts, want) {
			return
		}
	}
	tb.Fatalf("did not find invocation containing %v in %#v", want, invocations)
}

func assertInvocationHasJSONFields(tb testing.TB, invocations []map[string]any, noun string, verb string, want []string) {
	tb.Helper()
	for _, invocation := range invocations {
		argv := argvStrings(invocation)
		if len(argv) < 2 || argv[0] != noun || argv[1] != verb {
			continue
		}
		for index, arg := range argv {
			if arg == "--json" && index+1 < len(argv) {
				got := strings.Split(argv[index+1], ",")
				if strings.Join(got, ",") != strings.Join(want, ",") {
					tb.Fatalf("%s %s json fields = %v, want %v", noun, verb, got, want)
				}
				return
			}
		}
	}
	tb.Fatalf("no %s %s invocation found", noun, verb)
}

func assertInvocationMissingJSONField(tb testing.TB, invocations []map[string]any, noun string, verb string, field string) {
	tb.Helper()
	for _, invocation := range invocations {
		argv := argvStrings(invocation)
		if len(argv) < 2 || argv[0] != noun || argv[1] != verb {
			continue
		}
		for index, arg := range argv {
			if arg == "--json" && index+1 < len(argv) && strings.Contains(argv[index+1], field) {
				tb.Fatalf("%s %s unexpectedly requested %s: %v", noun, verb, field, argv)
			}
		}
	}
}

func argvStrings(invocation map[string]any) []string {
	argv, _ := invocation["argv"].([]any)
	parts := make([]string, 0, len(argv))
	for _, part := range argv {
		parts = append(parts, part.(string))
	}
	return parts
}

func containsOrdered(haystack []string, needle []string) bool {
	if len(needle) == 0 {
		return true
	}
	start := 0
	for _, want := range needle {
		found := false
		for start < len(haystack) {
			if haystack[start] == want {
				found = true
				start++
				break
			}
			start++
		}
		if !found {
			return false
		}
	}
	return true
}
