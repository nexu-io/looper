package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/reviewer"
)

func TestReviewerForgejoAdapterNativeDiscoveryContextPublishAndRetry(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	marker := "<!-- looper:review id=reviewer:loop:head-42 head=head-42 outcome=blocking -->"
	reviews := []map[string]any{{"id": 8, "state": "APPROVED", "body": "prior", "commit_id": "old-head", "user": map[string]any{"login": "human"}}}
	publishCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/requested_reviewers":{"post":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{},"post":{}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "login": "reviewer"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 42, "title": "Review me", "state": "open", "head": map[string]any{"ref": "feature", "sha": "head-42"}, "base": map[string]any{"ref": "main", "sha": "base"}, "user": map[string]any{"login": "alice"}, "requested_reviewers": []map[string]any{{"id": 7, "login": "reviewer"}}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "title": "Review me", "state": "open", "head": map[string]any{"ref": "feature", "sha": "head-42"}, "base": map[string]any{"ref": "main", "sha": "base"}, "user": map[string]any{"login": "alice"}, "requested_reviewers": []map[string]any{{"id": 7, "login": "reviewer"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews":
			_ = json.NewEncoder(w).Encode(reviews)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/reviews/") && strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews":
			publishCalls++
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode review: %v", err)
			}
			review := map[string]any{"id": 9, "state": payload["event"], "body": payload["body"], "commit_id": payload["commit_id"], "user": map[string]any{"login": "reviewer"}}
			reviews = append(reviews, review)
			_ = json.NewEncoder(w).Encode(review)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42.diff":
			_, _ = w.Write([]byte("diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-old\n+new\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/issues/42/comments":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Roles:     config.RoleConfigs{Reviewer: config.ReviewerRoleConfig{Behavior: config.ReviewerConfig{PublishMode: config.ReviewerPublishModeSingleReview}}},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := reviewerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}
	ctx := context.Background()
	prs, err := adapter.ListReviewRequestedPullRequests(ctx, reviewer.ListReviewRequestedPullRequestsInput{Repo: "acme/looper", Reviewer: "reviewer", CWD: repoPath})
	if err != nil || len(prs) != 1 || len(prs[0].ReviewRequests) != 1 {
		t.Fatalf("native discovery = %#v, %v", prs, err)
	}
	detail, err := adapter.ViewPullRequest(ctx, reviewer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: repoPath})
	if err != nil || detail.ReviewDecision != "APPROVED" || len(detail.Reviews) != 1 {
		t.Fatalf("native context = %#v, %v", detail, err)
	}
	verify := reviewer.VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=reviewer:loop:head-42 head=head-42", AllowedReviewEvents: []reviewer.ReviewEvent{reviewer.ReviewEventRequestChanges}, AuthorLogin: "reviewer", CWD: repoPath}
	found, err := adapter.FindReviewMarker(ctx, verify)
	if err != nil || found.Found {
		t.Fatalf("marker before publish = %#v, %v", found, err)
	}
	if err := adapter.SubmitReview(ctx, githubinfra.SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "REQUEST_CHANGES", Body: "Blocking issue\n\n" + marker, CommitID: "head-42", CWD: repoPath}); err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	found, err = adapter.FindReviewMarker(ctx, verify)
	if err != nil || !found.Found || found.Event != reviewer.ReviewEventRequestChanges {
		t.Fatalf("marker after publish = %#v, %v", found, err)
	}
	// The runner's retry contract checks the marker first; a retry therefore
	// reuses the native review instead of publishing a duplicate.
	if !found.Found {
		_ = adapter.SubmitReview(ctx, githubinfra.SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "REQUEST_CHANGES", Body: marker, CommitID: "head-42", CWD: repoPath})
	}
	if publishCalls != 1 {
		t.Fatalf("publish calls = %d, want 1", publishCalls)
	}
}
