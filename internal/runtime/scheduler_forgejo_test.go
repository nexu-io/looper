package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/fixer"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/loops/failureclass"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/worker"
)

func TestPlannerGitHubAdapterForgejoCreatePullRequestAndLabels(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	var authHeader string
	var createdBody map[string]any
	var labelBody map[string][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			if err := json.NewDecoder(r.Body).Decode(&createdBody); err != nil {
				t.Fatalf("decode create PR body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 101, "html_url": serverURL(r) + "/acme/looper/pulls/101", "head": map[string]any{"ref": "feature", "sha": "abc"}, "base": map[string]any{"ref": "main", "sha": "def"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/issues/101/labels":
			if err := json.NewDecoder(r.Body).Decode(&labelBody); err != nil {
				t.Fatalf("decode labels body: %v", err)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "name": "looper:spec-reviewing"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/101":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 101, "title": "Spec: add forgejo", "body": "Body", "state": "open",
				"html_url": serverURL(r) + "/acme/looper/pulls/101",
				"head":     map[string]any{"ref": "feature", "sha": "abc"},
				"base":     map[string]any{"ref": "main", "sha": "def"},
				"labels":   []map[string]any{{"id": 1, "name": "looper:hold"}},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := plannerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}

	created, err := adapter.CreatePullRequest(context.Background(), planner.CreatePullRequestInput{Repo: "acme/looper", HeadBranch: "feature", BaseBranch: "main", Title: "Spec: add forgejo", Body: "Body", CWD: repoPath})
	if err != nil {
		t.Fatalf("CreatePullRequest() error = %v", err)
	}
	if created.Number != 101 {
		t.Fatalf("created = %#v, want PR 101", created)
	}
	if err := adapter.AddPullRequestLabels(context.Background(), planner.PullRequestLabelsInput{Repo: "acme/looper", PRNumber: 101, Labels: []string{"looper:spec-reviewing"}, CWD: repoPath}); err != nil {
		t.Fatalf("AddPullRequestLabels() error = %v", err)
	}
	if authHeader != "token secret" {
		t.Fatalf("Authorization = %q, want Forgejo token auth", authHeader)
	}
	if createdBody["head"] != "feature" || createdBody["base"] != "main" {
		t.Fatalf("create body = %#v, want feature->main", createdBody)
	}
	if len(labelBody["labels"]) != 1 || labelBody["labels"][0] != "looper:spec-reviewing" {
		t.Fatalf("label body = %#v, want reviewing label", labelBody)
	}
	detail, err := adapter.ViewPullRequest(context.Background(), planner.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 101, CWD: repoPath})
	if err != nil {
		t.Fatalf("ViewPullRequest() error = %v", err)
	}
	if len(detail.Labels) != 1 || detail.Labels[0] != "looper:hold" {
		t.Fatalf("detail.Labels = %#v, want Forgejo PR labels", detail.Labels)
	}
	if body, _ := createdBody["body"].(string); !strings.Contains(body, "Body") {
		t.Fatalf("create PR body = %q, want stamped body content", body)
	}
}

func TestPlannerAdapterRoutesSameRepoSlugByProjectPath(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")

	serverFor := func(title string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/acme/app/issues" {
				t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 1, "title": title, "state": "open"}})
		}))
	}
	first := serverFor("first")
	defer first.Close()
	second := serverFor("second")
	defer second.Close()

	root := t.TempDir()
	firstPath := filepath.Join(root, "first")
	secondPath := filepath.Join(root, "second")
	cfg := config.Config{
		Providers: []config.ProviderConfig{
			{ID: "forgejo-one", Kind: config.ProviderKindForgejo, BaseURL: first.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")},
			{ID: "forgejo-two", Kind: config.ProviderKindForgejo, BaseURL: second.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")},
		},
		Projects: []config.ProjectRefConfig{
			{ID: "one", Provider: "forgejo-one", Repo: "acme/app", RepoPath: firstPath},
			{ID: "two", Provider: "forgejo-two", Repo: "acme/app", RepoPath: secondPath},
		},
	}
	adapter := plannerGitHubAdapter{config: &cfg}
	for _, testCase := range []struct {
		cwd  string
		want string
	}{{cwd: firstPath, want: "first"}, {cwd: secondPath, want: "second"}} {
		issues, err := adapter.ListOpenIssues(context.Background(), planner.ListOpenIssuesInput{Repo: "acme/app", CWD: testCase.cwd})
		if err != nil {
			t.Fatalf("ListOpenIssues(%s) error = %v", testCase.want, err)
		}
		if len(issues) != 1 || issues[0].Title != testCase.want {
			t.Fatalf("ListOpenIssues(%s) = %#v", testCase.want, issues)
		}
	}

	if _, _, err := forgejoClientForRepo(&cfg, "acme/app"); err == nil || !strings.Contains(err.Error(), "multiple projects") {
		t.Fatalf("forgejoClientForRepo() error = %v, want ambiguous bare-repo rejection", err)
	}
}

func TestForgeRoutingRejectsOverlappingWorktreeRoots(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outerRoot := filepath.Join(root, "worktrees")
	innerRoot := filepath.Join(outerRoot, "nested")
	cfg := config.Config{
		Providers: []config.ProviderConfig{
			{ID: "forgejo-one", Kind: config.ProviderKindForgejo},
			{ID: "forgejo-two", Kind: config.ProviderKindForgejo},
		},
		Projects: []config.ProjectRefConfig{
			{ID: "outer", Provider: "forgejo-one", Repo: "acme/outer", RepoPath: filepath.Join(root, "outer"), WorktreeRoot: &outerRoot},
			{ID: "inner", Provider: "forgejo-two", Repo: "acme/inner", RepoPath: filepath.Join(root, "inner"), WorktreeRoot: &innerRoot},
		},
	}
	_, _, ok, err := forgejoProjectProviderForCWD(&cfg, filepath.Join(innerRoot, "feature"))
	if err == nil || !strings.Contains(err.Error(), "matches multiple projects") {
		t.Fatalf("forgejoProjectProviderForCWD() = ok %v, error %v; want ambiguous root rejection", ok, err)
	}
}

func TestWorkerGitHubAdapterForgejoCreatePullRequestRequestsNativeReviewer(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	var createdBody map[string]any
	var reviewerBody map[string][]string
	var labelBody map[string][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/requested_reviewers":{"post":{}}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			if err := json.NewDecoder(r.Body).Decode(&createdBody); err != nil {
				t.Fatalf("decode create PR body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 201, "html_url": serverURL(r) + "/acme/looper/pulls/201", "head": map[string]any{"ref": "worker-branch", "sha": "abc"}, "base": map[string]any{"ref": "main", "sha": "def"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/201/requested_reviewers":
			if err := json.NewDecoder(r.Body).Decode(&reviewerBody); err != nil {
				t.Fatalf("decode reviewers body: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/issues/201/labels":
			if err := json.NewDecoder(r.Body).Decode(&labelBody); err != nil {
				t.Fatalf("decode labels body: %v", err)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "name": "team-review"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Roles: config.RoleConfigs{
			Reviewer: config.ReviewerRoleConfig{
				Discovery: config.ReviewerRoleDiscoveryConfig{
					Triggers: config.ReviewerRoleTriggersConfig{Labels: []string{"team-review"}},
				},
			},
		},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := workerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}

	created, err := adapter.CreatePullRequest(context.Background(), worker.CreatePullRequestInput{Repo: "acme/looper", HeadBranch: "worker-branch", BaseBranch: "main", Title: "Implement worker", Body: "Body", CWD: repoPath})
	if err != nil {
		t.Fatalf("CreatePullRequest() error = %v", err)
	}
	if created.Number != 201 {
		t.Fatalf("created = %#v, want PR 201", created)
	}
	if err := adapter.AddPullRequestReviewers(context.Background(), worker.PullRequestReviewersInput{Repo: "acme/looper", PRNumber: 201, Reviewers: []string{"reviewer"}, CWD: repoPath}); err != nil {
		t.Fatalf("AddPullRequestReviewers() error = %v", err)
	}
	if createdBody["head"] != "worker-branch" || createdBody["base"] != "main" {
		t.Fatalf("create body = %#v, want worker-branch->main", createdBody)
	}
	if got := reviewerBody["reviewers"]; len(got) != 1 || got[0] != "reviewer" {
		t.Fatalf("reviewer body = %#v, want native reviewer request", reviewerBody)
	}
	if got := labelBody["labels"]; len(got) != 1 || got[0] != "team-review" {
		t.Fatalf("label body = %#v, want configured reviewer discovery label fallback", labelBody)
	}
}

func TestWorkerGitHubAdapterForgejoAddReviewersFallsBackToLabelsWhenNativeUnavailable(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	var labelBody map[string][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/swagger.v1.json":
			// No requested_reviewers capability advertised.
			_, _ = w.Write([]byte(`{"paths":{}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/201/requested_reviewers":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/issues/201/labels":
			if err := json.NewDecoder(r.Body).Decode(&labelBody); err != nil {
				t.Fatalf("decode labels body: %v", err)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "name": "team-review"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Roles: config.RoleConfigs{
			Reviewer: config.ReviewerRoleConfig{
				Discovery: config.ReviewerRoleDiscoveryConfig{
					Triggers: config.ReviewerRoleTriggersConfig{Labels: []string{"team-review"}},
				},
			},
		},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := workerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}
	if err := adapter.AddPullRequestReviewers(context.Background(), worker.PullRequestReviewersInput{Repo: "acme/looper", PRNumber: 201, Reviewers: []string{"reviewer"}, CWD: repoPath}); err != nil {
		t.Fatalf("AddPullRequestReviewers() error = %v", err)
	}
	if got := labelBody["labels"]; len(got) != 1 || got[0] != "team-review" {
		t.Fatalf("label body = %#v, want configured reviewer discovery label fallback", labelBody)
	}
}

func TestWorkerGitHubAdapterForgejoAddReviewersIgnoresLabelFailureAfterNativeSuccess(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	var reviewerBody map[string][]string
	var labelAttempted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/requested_reviewers":{"post":{}}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/201/requested_reviewers":
			if err := json.NewDecoder(r.Body).Decode(&reviewerBody); err != nil {
				t.Fatalf("decode reviewers body: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/issues/201/labels":
			labelAttempted = true
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"label missing"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Roles: config.RoleConfigs{
			Reviewer: config.ReviewerRoleConfig{
				Discovery: config.ReviewerRoleDiscoveryConfig{
					// Native-request-triggered: labels are optional discovery aids.
					Triggers: config.ReviewerRoleTriggersConfig{RequireReviewRequest: true, Labels: []string{"team-review"}},
				},
			},
		},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := workerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}
	if err := adapter.AddPullRequestReviewers(context.Background(), worker.PullRequestReviewersInput{Repo: "acme/looper", PRNumber: 201, Reviewers: []string{"reviewer"}, CWD: repoPath}); err != nil {
		t.Fatalf("AddPullRequestReviewers() error = %v, want nil after native success despite label failure", err)
	}
	if got := reviewerBody["reviewers"]; len(got) != 1 || got[0] != "reviewer" {
		t.Fatalf("reviewer body = %#v, want native reviewer request", reviewerBody)
	}
	if !labelAttempted {
		t.Fatal("expected label application attempt after native success")
	}
}

func TestWorkerGitHubAdapterForgejoAddReviewersFailsLabelTriggeredWhenLabelMissingAfterNativeSuccess(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	var reviewerBody map[string][]string
	var labelAttempted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/requested_reviewers":{"post":{}}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/201/requested_reviewers":
			if err := json.NewDecoder(r.Body).Decode(&reviewerBody); err != nil {
				t.Fatalf("decode reviewers body: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/issues/201/labels":
			labelAttempted = true
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"label missing"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Roles: config.RoleConfigs{
			Reviewer: config.ReviewerRoleConfig{
				Discovery: config.ReviewerRoleDiscoveryConfig{
					// Label-triggered discovery: native request alone is not enough.
					Triggers: config.ReviewerRoleTriggersConfig{RequireReviewRequest: false, Labels: []string{"team-review"}},
				},
			},
		},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := workerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}
	err := adapter.AddPullRequestReviewers(context.Background(), worker.PullRequestReviewersInput{Repo: "acme/looper", PRNumber: 201, Reviewers: []string{"reviewer"}, CWD: repoPath})
	if err == nil {
		t.Fatal("AddPullRequestReviewers() error = nil, want label failure to keep label-triggered handoff retryable")
	}
	if !strings.Contains(err.Error(), "label missing") && !strings.Contains(err.Error(), "500") {
		t.Fatalf("AddPullRequestReviewers() error = %v, want label application failure", err)
	}
	if got := reviewerBody["reviewers"]; len(got) != 1 || got[0] != "reviewer" {
		t.Fatalf("reviewer body = %#v, want native reviewer request before label failure", reviewerBody)
	}
	if !labelAttempted {
		t.Fatal("expected label application attempt after native success")
	}
}

func TestReviewerGitHubAdapterForgejoCommentOnlyFlow(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	var listLabels string
	var commentBody map[string]any
	existingMarker := "<!-- looper:review id=reviewer:loop_123:abc123:key head=abc123 outcome=non_blocking -->"
	var removedPaths []string
	var comparePath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			listLabels = r.URL.Query().Get("labels")
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 42, "title": "Review me", "body": "PR body", "state": "open", "draft": true,
				"head":   map[string]any{"ref": "feature/review-me", "sha": "abc123"},
				"base":   map[string]any{"ref": "main", "sha": "base123"},
				"user":   map[string]any{"login": "alice", "id": 1},
				"labels": []map[string]any{{"id": 1, "name": "looper:review"}},
			}, {
				"number": 99, "title": "Skip me", "body": "PR body", "state": "open",
				"head":   map[string]any{"ref": "feature/skip-me", "sha": "def456"},
				"base":   map[string]any{"ref": "main", "sha": "base123"},
				"user":   map[string]any{"login": "bob", "id": 2},
				"labels": []map[string]any{{"id": 2, "name": "other"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "title": "Review me", "body": "PR body", "state": "open", "draft": true,
				"head":   map[string]any{"ref": "feature/review-me", "sha": "abc123"},
				"base":   map[string]any{"ref": "main", "sha": "base123"},
				"user":   map[string]any{"login": "alice", "id": 1},
				"labels": []map[string]any{{"id": 1, "name": "looper:review"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42.diff":
			_, _ = w.Write([]byte("diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-old\n+new\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/issues/42/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id":         77,
				"body":       "Existing review\n\n" + existingMarker,
				"html_url":   serverURL(r) + "/acme/looper/issues/42#issuecomment-77",
				"updated_at": "2026-06-18T00:00:00Z",
				"user":       map[string]any{"login": "reviewer-bot", "id": 7},
			}})
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/api/v1/repos/acme/looper/compare/main...feature%2Freview-me":
			comparePath = r.URL.EscapedPath()
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "ahead_by": 1, "behind_by": 0, "total_commits": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/issues/42/comments":
			if err := json.NewDecoder(r.Body).Decode(&commentBody); err != nil {
				t.Fatalf("decode comment body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 99, "html_url": serverURL(r) + "/acme/looper/issues/42#comment-99"})
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/issues/42/labels/"):
			removedPaths = append(removedPaths, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Roles:     config.RoleConfigs{Reviewer: config.ReviewerRoleConfig{Behavior: config.ReviewerConfig{PublishMode: config.ReviewerPublishModeSummaryComment}}},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := reviewerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}

	prs, err := adapter.ListOpenPullRequests(context.Background(), reviewer.ListOpenPullRequestsInput{Repo: "acme/looper", CWD: repoPath, Labels: []string{"looper:review"}})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(prs) != 1 || prs[0].HeadSHA != "abc123" || !prs[0].IsDraft {
		t.Fatalf("prs = %#v, want Forgejo PR summary", prs)
	}
	detail, err := adapter.ViewPullRequest(context.Background(), reviewer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: repoPath})
	if err != nil {
		t.Fatalf("ViewPullRequest() error = %v", err)
	}
	if !strings.Contains(detail.Diff, "diff --git") {
		t.Fatalf("detail.Diff = %q, want fetched Forgejo diff", detail.Diff)
	}
	if !detail.IsDraft {
		t.Fatalf("detail = %#v, want draft preserved", detail)
	}
	if len(detail.IssueComments) != 1 {
		t.Fatalf("detail.IssueComments = %#v, want existing Forgejo issue comment", detail.IssueComments)
	}
	if body, _ := detail.IssueComments[0]["body"].(string); !strings.Contains(body, existingMarker) {
		t.Fatalf("detail.IssueComments = %#v, want marker-bearing comment body", detail.IssueComments)
	}
	snapshot, err := adapter.CapturePullRequestSnapshot(context.Background(), reviewer.CapturePullRequestSnapshotInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, CWD: repoPath, CapturedAt: "2026-06-18T00:00:00Z"})
	if err != nil {
		t.Fatalf("CapturePullRequestSnapshot() error = %v", err)
	}
	if snapshot.HeadSHA != "abc123" || snapshot.PayloadJSON == nil || !strings.Contains(*snapshot.PayloadJSON, "diff --git") {
		t.Fatalf("snapshot = %#v, want captured Forgejo diff payload", snapshot)
	}
	comment, err := adapter.CreateIssueComment(context.Background(), reviewer.IssueCommentInput{Repo: "acme/looper", IssueNumber: 42, Body: "Needs a test", CWD: repoPath})
	if err != nil {
		t.Fatalf("CreateIssueComment() error = %v", err)
	}
	if comment.ID != 99 {
		t.Fatalf("comment = %#v, want created comment id", comment)
	}
	if err := adapter.RemovePullRequestLabels(context.Background(), reviewer.PullRequestLabelsInput{Repo: "acme/looper", PRNumber: 42, Labels: []string{"looper:review"}, CWD: repoPath}); err != nil {
		t.Fatalf("RemovePullRequestLabels() error = %v", err)
	}
	workerAdapter := workerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}
	comparison, err := workerAdapter.CompareBranches(context.Background(), worker.CompareBranchesInput{Repo: "acme/looper", BaseBranch: "main", HeadBranch: "feature/review-me", CWD: repoPath})
	if err != nil {
		t.Fatalf("CompareBranches() error = %v", err)
	}
	if comparison.AheadBy != 1 || comparison.Status != "ahead" {
		t.Fatalf("comparison = %#v, want Forgejo compare result", comparison)
	}
	if listLabels != "" {
		t.Fatalf("labels query = %q, want local label filtering", listLabels)
	}
	if body, _ := commentBody["body"].(string); !strings.Contains(body, "Needs a test") {
		t.Fatalf("comment body = %#v, want stamped comment content", commentBody)
	}
	if len(removedPaths) != 1 || !strings.Contains(removedPaths[0], "/issues/42/labels/looper:review") {
		t.Fatalf("removedPaths = %#v, want Forgejo label delete", removedPaths)
	}
	if comparePath != "/api/v1/repos/acme/looper/compare/main...feature%2Freview-me" {
		t.Fatalf("comparePath = %q, want encoded Forgejo compare path", comparePath)
	}
}

func TestReviewerGitHubAdapterForgejoThreadResolutionShortCircuits(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Roles:     config.RoleConfigs{Reviewer: config.ReviewerRoleConfig{Behavior: config.ReviewerConfig{PublishMode: config.ReviewerPublishModeSummaryComment}}},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := reviewerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}

	threads, err := adapter.ListReviewThreads(context.Background(), reviewer.ListReviewThreadsInput{Repo: "acme/looper", PRNumber: 42, CWD: repoPath, Limit: 10})
	if err != nil {
		t.Fatalf("ListReviewThreads() error = %v", err)
	}
	if len(threads) != 0 {
		t.Fatalf("threads = %#v, want empty Forgejo thread list", threads)
	}
	if err := adapter.AddReviewThreadReply(context.Background(), reviewer.AddReviewThreadReplyInput{Repo: "acme/looper", ThreadID: "thread-1", Body: "reply", CWD: repoPath}); err != nil {
		t.Fatalf("AddReviewThreadReply() error = %v", err)
	}
	if err := adapter.ResolveReviewThread(context.Background(), reviewer.ResolveReviewThreadInput{Repo: "acme/looper", ThreadID: "thread-1", CWD: repoPath}); err != nil {
		t.Fatalf("ResolveReviewThread() error = %v", err)
	}
}

func TestReviewerGitHubAdapterForgejoFindReviewMarkerUsesIssueComments(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/issues/42/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id":         77,
				"body":       "ignored\n<!-- looper:review id=reviewer:other:abc123 head=abc123 outcome=clean -->",
				"html_url":   serverURL(r) + "/acme/looper/issues/42#issuecomment-77",
				"updated_at": "2026-07-07T00:00:00Z",
				"user":       map[string]any{"login": "other-bot", "id": 8},
			}, {
				"id":         78,
				"body":       "looks good\n<!-- looper:review id=reviewer:loop-1:abc123 head=abc123 outcome=clean -->",
				"html_url":   serverURL(r) + "/acme/looper/issues/42#issuecomment-78",
				"updated_at": "2026-07-07T00:01:00Z",
				"user":       map[string]any{"login": "reviewer-bot", "id": 9},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Roles:     config.RoleConfigs{Reviewer: config.ReviewerRoleConfig{Behavior: config.ReviewerConfig{PublishMode: config.ReviewerPublishModeSummaryComment}}},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := reviewerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}

	marker, err := adapter.FindReviewMarker(context.Background(), reviewer.VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=reviewer:loop-1:abc123 head=abc123", AllowedReviewEvents: []reviewer.ReviewEvent{reviewer.ReviewEventApprove}, AuthorLogin: "reviewer-bot", AllowCleanComment: true})
	if err != nil {
		t.Fatalf("FindReviewMarker() error = %v", err)
	}
	if !marker.Found || marker.Event != reviewer.ReviewEventComment || marker.Outcome != "clean" || marker.AuthorLogin != "reviewer-bot" {
		t.Fatalf("marker = %#v, want Forgejo comment-backed marker result", marker)
	}
	if !strings.Contains(marker.Body, "looper:review id=reviewer:loop-1:abc123") {
		t.Fatalf("marker.Body = %q, want matched marker body", marker.Body)
	}
}

func TestFixerGitHubAdapterForgejoSummaryCommentNoResolveFlow(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	var createdCommentBody map[string]any
	var updatedCommentBody map[string]any
	var addedLabels map[string][]string
	var removedLabelPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "login": "fixer-bot"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 42, "title": "Fix me", "body": "PR body", "state": "open",
				"head":   map[string]any{"ref": "feature/fix-me", "sha": "abc123"},
				"base":   map[string]any{"ref": "main", "sha": "base123"},
				"user":   map[string]any{"login": "alice", "id": 1},
				"labels": []map[string]any{{"id": 1, "name": "looper:fix"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "title": "Fix me", "body": "PR body", "state": "open",
				"head":   map[string]any{"ref": "feature/fix-me", "sha": "abc123"},
				"base":   map[string]any{"ref": "main", "sha": "base123"},
				"user":   map[string]any{"login": "alice", "id": 1},
				"labels": []map[string]any{{"id": 1, "name": "looper:fix"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/issues/42/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id":         77,
				"body":       "<!-- looper:forgejo-reviewer-summary {\"kind\":\"looper.forgejo.reviewer_summary\",\"schema_version\":1,\"review_round_id\":1,\"items\":[{\"review_item_id\":\"R-001\",\"status\":\"open\",\"title\":\"Fix it\",\"body\":\"Needs repair\",\"last_seen_round_id\":1}]} -->",
				"html_url":   serverURL(r) + "/acme/looper/issues/42#issuecomment-77",
				"updated_at": "2026-06-30T00:00:00Z",
				"user":       map[string]any{"login": "reviewer-bot", "id": 8},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/issues/42/comments":
			if err := json.NewDecoder(r.Body).Decode(&createdCommentBody); err != nil {
				t.Fatalf("decode created comment body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 88, "html_url": serverURL(r) + "/acme/looper/issues/42#issuecomment-88"})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/repos/acme/looper/issues/comments/88":
			if err := json.NewDecoder(r.Body).Decode(&updatedCommentBody); err != nil {
				t.Fatalf("decode updated comment body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 88, "html_url": serverURL(r) + "/acme/looper/issues/42#issuecomment-88"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/issues/42/labels":
			if err := json.NewDecoder(r.Body).Decode(&addedLabels); err != nil {
				t.Fatalf("decode added labels: %v", err)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 2, "name": "looper:fixing"}})
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v1/repos/acme/looper/issues/42/labels/"):
			removedLabelPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/api/v1/repos/acme/looper/compare/base123...abc123":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "ahead_by": 1})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := fixerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}
	ctx := context.Background()

	login, err := adapter.GetCurrentUserLogin(ctx, repoPath)
	if err != nil || login != "fixer-bot" {
		t.Fatalf("GetCurrentUserLogin() = %q, %v; want fixer-bot", login, err)
	}
	prs, err := adapter.ListOpenPullRequests(ctx, fixer.ListOpenPullRequestsInput{Repo: "acme/looper", CWD: repoPath, Labels: []string{"looper:fix"}, BaseRefName: "main"})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(prs) != 1 || prs[0].Author != "alice" || prs[0].HeadSHA != "abc123" {
		t.Fatalf("prs = %#v, want Forgejo fixer PR summary", prs)
	}
	detail, err := adapter.ViewPullRequest(ctx, fixer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: repoPath})
	if err != nil {
		t.Fatalf("ViewPullRequest() error = %v", err)
	}
	if len(detail.IssueComments) != 1 || len(detail.Comments) != 0 {
		t.Fatalf("detail = %#v, want reviewer summary comments and no native items", detail)
	}
	created, err := adapter.CreateIssueComment(ctx, fixer.IssueCommentInput{Repo: "acme/looper", IssueNumber: 42, Body: "fixer summary", CWD: repoPath})
	if err != nil {
		t.Fatalf("CreateIssueComment() error = %v", err)
	}
	if created.ID != 88 {
		t.Fatalf("created = %#v, want comment 88", created)
	}
	if err := adapter.UpdateIssueComment(ctx, fixer.UpdateIssueCommentInput{Repo: "acme/looper", CommentID: 88, Body: "updated fixer summary", CWD: repoPath}); err != nil {
		t.Fatalf("UpdateIssueComment() error = %v", err)
	}
	if err := adapter.AddPullRequestLabels(ctx, fixer.PullRequestLabelsInput{Repo: "acme/looper", PRNumber: 42, Labels: []string{"looper:fixing"}, CWD: repoPath}); err != nil {
		t.Fatalf("AddPullRequestLabels() error = %v", err)
	}
	if err := adapter.RemovePullRequestLabels(ctx, fixer.PullRequestLabelsInput{Repo: "acme/looper", PRNumber: 42, Labels: []string{"looper:fix"}, CWD: repoPath}); err != nil {
		t.Fatalf("RemovePullRequestLabels() error = %v", err)
	}
	compare, err := adapter.CompareCommits(ctx, fixer.CompareCommitsInput{Repo: "acme/looper", Base: "base123", Head: "abc123", CWD: repoPath})
	if err != nil || compare.Status != "ahead" {
		t.Fatalf("CompareCommits() = %#v, %v; want ahead", compare, err)
	}
	if _, err := adapter.ListReviewThreads(ctx, fixer.ListReviewThreadsInput{Repo: "acme/looper", PRNumber: 42, CWD: repoPath}); err == nil || !strings.Contains(err.Error(), "does not support native review threads") {
		t.Fatalf("ListReviewThreads() error = %v, want Forgejo unsupported native review threads", err)
	}
	if err := adapter.ResolveReviewThread(ctx, fixer.ResolveReviewThreadInput{Repo: "acme/looper", ThreadID: "thread-1", CWD: repoPath}); err == nil || !strings.Contains(err.Error(), "does not support native review thread resolution") {
		t.Fatalf("ResolveReviewThread() error = %v, want Forgejo unsupported native thread resolution", err)
	}
	if body, _ := createdCommentBody["body"].(string); !strings.Contains(body, "fixer summary") {
		t.Fatalf("createdCommentBody = %#v, want stamped summary body", createdCommentBody)
	}
	if body, _ := updatedCommentBody["body"].(string); !strings.Contains(body, "updated fixer summary") {
		t.Fatalf("updatedCommentBody = %#v, want stamped summary body", updatedCommentBody)
	}
	if got := addedLabels["labels"]; len(got) != 1 || got[0] != "looper:fixing" {
		t.Fatalf("addedLabels = %#v, want looper:fixing", addedLabels)
	}
	if !strings.Contains(removedLabelPath, "/issues/42/labels/looper:fix") {
		t.Fatalf("removedLabelPath = %q, want Forgejo label removal", removedLabelPath)
	}
}

func TestFixerGitHubAdapterForgejoListNativeReviewComments(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 201}, {"id": 202}, {"id": 203}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews/201/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": 101, "body": "Open comment", "html_url": serverURL(r) + "/acme/looper/pulls/42#discussion_r101", "updated_at": "2026-07-01T00:00:00Z",
				"user": map[string]any{"login": "alice", "id": 1}, "path": "internal/runtime/scheduler.go", "diff_hunk": "@@ -1 +1 @@",
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews/202/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": 102, "body": "Explicit unresolved", "html_url": serverURL(r) + "/acme/looper/pulls/42#discussion_r102", "updated_at": "2026-07-02T00:00:00Z",
				"user": map[string]any{"login": "bob", "id": 2}, "path": "internal/fixer/runner.go", "diff_hunk": "@@ -2 +2 @@", "resolver": nil,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews/203/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": 103, "body": "Resolved", "html_url": serverURL(r) + "/acme/looper/pulls/42#discussion_r103", "updated_at": "2026-07-03T00:00:00Z",
				"user": map[string]any{"login": "carol", "id": 3}, "path": "internal/forge/forgejo.go", "diff_hunk": "@@ -3 +3 @@", "resolver": map[string]any{"login": "maintainer", "id": 9},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := fixerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}

	comments, err := adapter.ListNativeReviewComments(context.Background(), fixer.ListNativeReviewCommentsInput{Repo: "acme/looper", PRNumber: 42, CWD: repoPath})
	if err != nil {
		t.Fatalf("ListNativeReviewComments() error = %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("comments = %#v, want 3", comments)
	}
	if got := comments[0]; got.ObservedFingerprint != fixer.NativeReviewCommentFingerprint(101, "2026-07-01T00:00:00Z") || got.ResolverPresent || got.IsResolved {
		t.Fatalf("comments[0] = %#v, want absent resolver preserved as open", got)
	}
	if got := comments[1]; !got.ResolverPresent || got.IsResolved {
		t.Fatalf("comments[1] = %#v, want explicit resolver presence without resolution", got)
	}
	if got := comments[2]; !got.ResolverPresent || !got.IsResolved || got.Author != "carol" {
		t.Fatalf("comments[2] = %#v, want resolved comment with author preserved", got)
	}
}

func TestFixerGitHubAdapterForgejoFiltersAuthorBeforeLimit(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/acme/looper/pulls" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		pulls := []map[string]any{{"number": 1, "state": "open", "user": map[string]any{"login": "other"}, "head": map[string]any{"ref": "one", "sha": "head-1"}, "base": map[string]any{"ref": "main", "sha": "base"}}}
		for number := 2; number <= 36; number++ {
			pulls = append(pulls, map[string]any{"number": number, "state": "open", "user": map[string]any{"login": "Looper"}, "head": map[string]any{"ref": fmt.Sprintf("pr-%d", number), "sha": fmt.Sprintf("head-%d", number)}, "base": map[string]any{"ref": "main", "sha": "base"}})
		}
		_ = json.NewEncoder(w).Encode(pulls)
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := fixerGitHubAdapter{config: &cfg}

	prs, err := adapter.ListOpenPullRequests(context.Background(), fixer.ListOpenPullRequestsInput{Repo: "acme/looper", CWD: repoPath, Author: "looper", Limit: 1})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 2 {
		t.Fatalf("pull requests = %#v, want matching author after provider result filtering", prs)
	}

	prs, err = adapter.ListOpenPullRequests(context.Background(), fixer.ListOpenPullRequestsInput{Repo: "acme/looper", CWD: repoPath, Author: "looper"})
	if err != nil {
		t.Fatalf("ListOpenPullRequests(default limit) error = %v", err)
	}
	if len(prs) != 30 || prs[0].Number != 2 || prs[29].Number != 31 {
		t.Fatalf("pull requests = %#v, want first 30 matching authors at the default limit", prs)
	}
}

func TestFixerGitHubAdapterForgejoBoundsUnfilteredDefaultLimit(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/acme/looper/pulls" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("limit"); got != "30" {
			t.Fatalf("limit = %q, want default discovery limit 30", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 1, "state": "open", "user": map[string]any{"login": "looper"}, "head": map[string]any{"ref": "one", "sha": "head-1"}, "base": map[string]any{"ref": "main", "sha": "base"}}})
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := fixerGitHubAdapter{config: &cfg}

	prs, err := adapter.ListOpenPullRequests(context.Background(), fixer.ListOpenPullRequestsInput{Repo: "acme/looper", CWD: repoPath})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 1 {
		t.Fatalf("pull requests = %#v, want bounded provider result", prs)
	}
}

func TestForgejoSupportsFixerDiscoveryWithoutOpeningCoordinatorLane(t *testing.T) {
	if !providerSupportsFixerDiscovery(config.ProviderKindForgejo) {
		t.Fatal("providerSupportsFixerDiscovery(forgejo) = false, want true")
	}
	if providerHasGitHubPullRequests(config.ProviderKindForgejo) {
		t.Fatal("providerHasGitHubPullRequests(forgejo) = true, coordinator lane must remain disabled")
	}
}

// assertHostingBoundary checks adapter/withHostingAPIBoundary classification and
// that a later BoundaryGitHubAPI re-wrap preserves an existing local boundary.
func assertHostingBoundary(t *testing.T, name string, err error, wantBoundary failureclass.Boundary, wantKind failureclass.Kind) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s error = nil, want %s", name, wantBoundary)
	}
	var boundaryErr *failureclass.BoundaryError
	if !errors.As(err, &boundaryErr) || boundaryErr.Boundary != wantBoundary {
		t.Fatalf("%s boundary = %#v, want %s", name, err, wantBoundary)
	}
	if kind := failureclass.Classify(err, failureclass.Context{Runner: failureclass.RunnerFixer, Boundary: failureclass.BoundaryUnknown}); kind != wantKind {
		t.Fatalf("%s Classify() = %s, want %s", name, kind, wantKind)
	}
	if wantBoundary == failureclass.BoundaryConfig {
		preserved := failureclass.WithBoundary(err, failureclass.BoundaryGitHubAPI)
		if !errors.As(preserved, &boundaryErr) || boundaryErr.Boundary != failureclass.BoundaryConfig {
			t.Fatalf("%s re-wrap promoted local boundary: %#v", name, preserved)
		}
		if kind := failureclass.Classify(preserved, failureclass.Context{Runner: failureclass.RunnerFixer, Boundary: failureclass.BoundaryUnknown}); kind != failureclass.NonRetryable {
			t.Fatalf("%s re-wrapped Classify() = %s, want non_retryable", name, kind)
		}
	}
}

func writeFakeGH(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	return path
}

func TestWithHostingAPIBoundaryClassification(t *testing.T) {
	// Unit table for the adapter boundary helper: local contract/config parks,
	// host-pressure launch and remote transport stay retryable GitHubAPI.
	cases := []struct {
		name     string
		err      error
		boundary failureclass.Boundary
		kind     failureclass.Kind
	}{
		{"missing binary", fmt.Errorf("start command: %w", exec.ErrNotFound), failureclass.BoundaryConfig, failureclass.NonRetryable},
		{"EAGAIN", fmt.Errorf("start command: %w", syscall.EAGAIN), failureclass.BoundaryGitHubAPI, failureclass.RetryableTransient},
		{"ENOMEM", fmt.Errorf("start command: %w", syscall.ENOMEM), failureclass.BoundaryGitHubAPI, failureclass.RetryableTransient},
		{"ETXTBSY", fmt.Errorf("start command: %w", syscall.ETXTBSY), failureclass.BoundaryGitHubAPI, failureclass.RetryableTransient},
		{"EMFILE", fmt.Errorf("start command: %w", syscall.EMFILE), failureclass.BoundaryGitHubAPI, failureclass.RetryableTransient},
		{"ENFILE", fmt.Errorf("start command: %w", syscall.ENFILE), failureclass.BoundaryGitHubAPI, failureclass.RetryableTransient},
		{"unknown json field", &shell.CommandExecutionError{Message: "Command exited with code 1", Result: shell.Result{ExitCode: 1, Stderr: `unknown JSON field: "statusCheckRollup"`}}, failureclass.BoundaryConfig, failureclass.NonRetryable},
		{"unknown flag", &shell.CommandExecutionError{Message: "Command exited with code 1", Result: shell.Result{ExitCode: 1, Stderr: "unknown flag: --not-a-real-flag"}}, failureclass.BoundaryConfig, failureclass.NonRetryable},
		// Zero-exit decode failures from invalidJSONError must not become endless API retries.
		{"invalid gh json payload", &shell.CommandExecutionError{Message: "Invalid gh JSON payload: unexpected end of JSON input; stdoutBytes=0", Result: shell.Result{ExitCode: 0, Stdout: "", Stderr: "unexpected end of JSON input"}}, failureclass.BoundaryConfig, failureclass.NonRetryable},
		// Local shell capture truncation (256 KiB) must park as BoundaryConfig, not
		// BoundaryGitHubAPI — oversized PR status/review payloads cannot be fixed by retry.
		{"truncated stdout flags", &shell.CommandExecutionError{Message: "GitHub command output truncated: stdout after 262144 bytes", Result: shell.Result{ExitCode: 0, Stdout: strings.Repeat("x", 64), StdoutTruncated: true}}, failureclass.BoundaryConfig, failureclass.NonRetryable},
		{"truncated stderr flags", &shell.CommandExecutionError{Message: "GitHub command output truncated: stderr after 262144 bytes", Result: shell.Result{ExitCode: 1, Stderr: strings.Repeat("e", 64), StderrTruncated: true}}, failureclass.BoundaryConfig, failureclass.NonRetryable},
		{"truncated message only", &shell.CommandExecutionError{Message: "GitHub command output truncated: stdout after 262144 bytes", Result: shell.Result{ExitCode: 0}}, failureclass.BoundaryConfig, failureclass.NonRetryable},
		{"remote EOF", &shell.CommandExecutionError{Message: "Command exited with code 1", Result: shell.Result{ExitCode: 1, Stderr: `Post "https://api.github.com/graphql": unexpected EOF`}}, failureclass.BoundaryGitHubAPI, failureclass.RetryableTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertHostingBoundary(t, tc.name, withHostingAPIBoundary(tc.err), tc.boundary, tc.kind)
		})
	}
}

func TestFixerGitHubAdapterBoundaryContracts(t *testing.T) {
	// Cross-component contracts for fixerGitHubAdapter: construction, remote
	// HTTP, real gateway/shell launch, CLI schema, and zero-exit invalid JSON.
	repoPath := filepath.Join(t.TempDir(), "repo")
	input := fixer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: repoPath}

	t.Run("local construction", func(t *testing.T) {
		t.Setenv("FORGEJO_TOKEN", "")
		cfg := config.Config{
			Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "https://forge.example", TokenEnv: stringPtr("FORGEJO_TOKEN")}},
			Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
		}
		adapter := fixerGitHubAdapter{config: &cfg}
		_, err := adapter.GetCurrentUserLogin(context.Background(), repoPath)
		assertHostingBoundary(t, "GetCurrentUserLogin", err, failureclass.BoundaryConfig, failureclass.NonRetryable)
		_, err = adapter.GetPullRequestAuthor(context.Background(), input)
		assertHostingBoundary(t, "GetPullRequestAuthor", err, failureclass.BoundaryConfig, failureclass.NonRetryable)
		_, err = adapter.ViewPullRequest(context.Background(), input)
		assertHostingBoundary(t, "ViewPullRequest", err, failureclass.BoundaryConfig, failureclass.NonRetryable)
	})

	t.Run("remote 502 retryable", func(t *testing.T) {
		t.Setenv("FORGEJO_TOKEN", "secret")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("bad gateway"))
		}))
		t.Cleanup(server.Close)
		cfg := config.Config{
			Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
			Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
		}
		_, err := fixerGitHubAdapter{config: &cfg}.GetCurrentUserLogin(context.Background(), repoPath)
		assertHostingBoundary(t, "GetCurrentUserLogin", err, failureclass.BoundaryGitHubAPI, failureclass.RetryableTransient)
	})

	t.Run("forgejo PR 404 terminal", func(t *testing.T) {
		t.Setenv("FORGEJO_TOKEN", "secret")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}))
		t.Cleanup(server.Close)
		cfg := config.Config{
			Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
			Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
		}
		adapter := fixerGitHubAdapter{config: &cfg}
		_, err := adapter.GetPullRequestAuthor(context.Background(), input)
		assertHostingBoundary(t, "GetPullRequestAuthor", err, failureclass.BoundaryGitHubAPI, failureclass.NonRetryable)
		_, err = adapter.ViewPullRequest(context.Background(), input)
		assertHostingBoundary(t, "ViewPullRequest", err, failureclass.BoundaryGitHubAPI, failureclass.NonRetryable)
	})

	t.Run("missing gh launch", func(t *testing.T) {
		adapter := fixerGitHubAdapter{gateway: githubinfra.New(githubinfra.Options{GHPath: filepath.Join(t.TempDir(), "no-such-gh")})}
		cwd := t.TempDir()
		in := fixer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: cwd}
		_, err := adapter.GetCurrentUserLogin(context.Background(), cwd)
		assertHostingBoundary(t, "GetCurrentUserLogin", err, failureclass.BoundaryConfig, failureclass.NonRetryable)
		_, err = adapter.GetPullRequestAuthor(context.Background(), in)
		assertHostingBoundary(t, "GetPullRequestAuthor", err, failureclass.BoundaryConfig, failureclass.NonRetryable)
		_, err = adapter.ViewPullRequest(context.Background(), in)
		assertHostingBoundary(t, "ViewPullRequest", err, failureclass.BoundaryConfig, failureclass.NonRetryable)
	})

	t.Run("inaccessible CWD launch", func(t *testing.T) {
		ghPath := "gh"
		if resolved, err := exec.LookPath("gh"); err == nil {
			ghPath = resolved
		}
		adapter := fixerGitHubAdapter{gateway: githubinfra.New(githubinfra.Options{GHPath: ghPath})}
		missingCWD := filepath.Join(t.TempDir(), "missing-worktree")
		_, err := adapter.GetCurrentUserLogin(context.Background(), missingCWD)
		assertHostingBoundary(t, "GetCurrentUserLogin", err, failureclass.BoundaryConfig, failureclass.NonRetryable)
	})

	t.Run("CLI schema failure", func(t *testing.T) {
		// Real gateway + shell: gh launches and exits nonzero for bad --json field.
		adapter := fixerGitHubAdapter{gateway: githubinfra.New(githubinfra.Options{
			GHPath: writeFakeGH(t, "#!/bin/sh\nprintf '%s\\n' 'unknown JSON field: \"statusCheckRollup\"' >&2\nexit 1\n"),
		})}
		cwd := t.TempDir()
		in := fixer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: cwd}
		for _, name := range []string{"GetCurrentUserLogin", "GetPullRequestAuthor", "ViewPullRequest"} {
			var err error
			switch name {
			case "GetCurrentUserLogin":
				_, err = adapter.GetCurrentUserLogin(context.Background(), cwd)
			case "GetPullRequestAuthor":
				_, err = adapter.GetPullRequestAuthor(context.Background(), in)
			case "ViewPullRequest":
				_, err = adapter.ViewPullRequest(context.Background(), in)
			}
			if shell.IsStartFailure(err) {
				t.Fatalf("%s IsStartFailure = true, want completed CLI contract failure", name)
			}
			assertHostingBoundary(t, name, err, failureclass.BoundaryConfig, failureclass.NonRetryable)
		}
	})

	t.Run("invalid JSON zero-exit", func(t *testing.T) {
		// Real gateway contract: gh exits 0 with malformed stdout; invalidJSONError
		// emits "Invalid gh JSON payload" and must park as BoundaryConfig.
		// GetCurrentUserLogin uses `gh api user --jq .login` (raw text), so cover
		// the JSON-decoding entrypoints that surface invalidJSONError.
		adapter := fixerGitHubAdapter{gateway: githubinfra.New(githubinfra.Options{
			GHPath: writeFakeGH(t, "#!/bin/sh\nprintf '%s\\n' 'not-json-at-all'\nexit 0\n"),
		})}
		cwd := t.TempDir()
		in := fixer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: cwd}
		for _, name := range []string{"GetPullRequestAuthor", "ViewPullRequest"} {
			var err error
			switch name {
			case "GetPullRequestAuthor":
				_, err = adapter.GetPullRequestAuthor(context.Background(), in)
			case "ViewPullRequest":
				_, err = adapter.ViewPullRequest(context.Background(), in)
			}
			if err == nil {
				t.Fatalf("%s error = nil, want invalid JSON payload failure", name)
			}
			if shell.IsStartFailure(err) {
				t.Fatalf("%s IsStartFailure = true, want completed zero-exit decode failure", name)
			}
			if !strings.Contains(err.Error(), "Invalid gh JSON payload") {
				t.Fatalf("%s error = %v, want Invalid gh JSON payload", name, err)
			}
			assertHostingBoundary(t, name, err, failureclass.BoundaryConfig, failureclass.NonRetryable)
		}
	})

	t.Run("truncated shell capture", func(t *testing.T) {
		// Real gateway + shell path: gh writes past shell's 256 KiB capture limit.
		// runGhWithTimeout must surface truncation; withHostingAPIBoundary must park
		// it as BoundaryConfig so unlimited fixer queues request intervention.
		adapter := fixerGitHubAdapter{gateway: githubinfra.New(githubinfra.Options{
			GHPath: writeFakeGH(t, "#!/bin/sh\n# Exceed shell defaultMaxOutputBytes (256 KiB).\ndd if=/dev/zero bs=1024 count=260 2>/dev/null | tr '\\0' 'x'\nexit 0\n"),
		})}
		cwd := t.TempDir()
		in := fixer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: cwd}
		for _, name := range []string{"GetCurrentUserLogin", "GetPullRequestAuthor", "ViewPullRequest"} {
			var err error
			switch name {
			case "GetCurrentUserLogin":
				_, err = adapter.GetCurrentUserLogin(context.Background(), cwd)
			case "GetPullRequestAuthor":
				_, err = adapter.GetPullRequestAuthor(context.Background(), in)
			case "ViewPullRequest":
				_, err = adapter.ViewPullRequest(context.Background(), in)
			}
			if err == nil {
				t.Fatalf("%s error = nil, want truncated capture failure", name)
			}
			if shell.IsStartFailure(err) {
				t.Fatalf("%s IsStartFailure = true, want completed truncation contract failure", name)
			}
			if !strings.Contains(err.Error(), "GitHub command output truncated") {
				t.Fatalf("%s error = %v, want GitHub command output truncated", name, err)
			}
			var commandErr *shell.CommandExecutionError
			if !errors.As(err, &commandErr) {
				t.Fatalf("%s error type = %T, want *shell.CommandExecutionError", name, err)
			}
			if !commandErr.Result.StdoutTruncated && !commandErr.Result.StderrTruncated {
				t.Fatalf("%s truncation flags unset on Result: %#v", name, commandErr.Result)
			}
			assertHostingBoundary(t, name, err, failureclass.BoundaryConfig, failureclass.NonRetryable)
		}
	})
}

func TestFixerGitHubAdapterResolvesIntegrationTokenIdentity(t *testing.T) {
	t.Parallel()
	integrationErr := func() (shell.Result, error) {
		result := shell.Result{ExitCode: 1, Stderr: "HTTP 403: Resource not accessible by integration"}
		return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
	}
	gateway := githubinfra.New(githubinfra.Options{
		GHPath: "gh",
		CWD:    t.TempDir(),
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			switch strings.Join(options.Args, " ") {
			case "api user --jq .login", "api user --jq {login: .login, id: .id}":
				return integrationErr()
			case "api graphql -f query=query { viewer { login } }":
				return shell.Result{Stdout: `{"data":{"viewer":{"login":"looper-app[bot]"}}}`}, nil
			default:
				t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
				return shell.Result{}, nil
			}
		},
	})
	login, err := fixerGitHubAdapter{gateway: gateway}.GetCurrentUserLogin(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("GetCurrentUserLogin() error = %v", err)
	}
	if login != "looper-app[bot]" {
		t.Fatalf("GetCurrentUserLogin() = %q, want looper-app[bot]", login)
	}
}

func TestReviewerGitHubAdapterResolvesIntegrationTokenIdentity(t *testing.T) {
	t.Parallel()
	integrationErr := func() (shell.Result, error) {
		result := shell.Result{ExitCode: 1, Stderr: "HTTP 403: Resource not accessible by integration"}
		return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
	}
	gateway := githubinfra.New(githubinfra.Options{
		GHPath: "gh",
		CWD:    t.TempDir(),
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			switch strings.Join(options.Args, " ") {
			case "api user --jq .login", "api user --jq {login: .login, id: .id}":
				return integrationErr()
			case "api graphql -f query=query { viewer { login } }":
				return shell.Result{Stdout: `{"data":{"viewer":{"login":"looper-app[bot]"}}}`}, nil
			default:
				t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
				return shell.Result{}, nil
			}
		},
	})
	login, err := reviewerGitHubAdapter{gateway: gateway}.GetCurrentUserLogin(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("GetCurrentUserLogin() error = %v", err)
	}
	if login != "looper-app[bot]" {
		t.Fatalf("GetCurrentUserLogin() = %q, want looper-app[bot]", login)
	}
}

func TestFixerGitHubAdapterForgejoResolveNativeReviewComment(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	var calledPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/comments/101/resolve":
			calledPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := fixerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}

	if err := adapter.ResolveNativeReviewComment(context.Background(), fixer.ResolveNativeReviewCommentInput{Repo: "acme/looper", PRNumber: 42, ProviderCommentID: 101, CWD: repoPath}); err != nil {
		t.Fatalf("ResolveNativeReviewComment() error = %v", err)
	}
	if calledPath != "/api/v1/repos/acme/looper/pulls/comments/101/resolve" {
		t.Fatalf("calledPath = %q, want resolve endpoint", calledPath)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
