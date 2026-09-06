package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/fixer"
	"github.com/nexu-io/looper/internal/reviewer"
)

func TestForgejoAdaptersExposeConflictsChecksAndNativeReviewHandoff(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	cwd, baseSHA, headSHA, cleanSHA := forgejoConflictRepo(t)
	inlineReads := 0
	requested := false
	dismissed := false
	marker := "<!-- looper:review id=reviewer:loop:" + headSHA + " head=" + headSHA + " outcome=blocking -->"
	pr := map[string]any{
		"number": 42, "title": "Fix me", "state": "open", "mergeable": false,
		"html_url": "https://forge.test/acme/looper/pulls/42",
		"head":     map[string]any{"sha": headSHA, "ref": "feature"}, "base": map[string]any{"sha": baseSHA, "ref": "main"},
		"user": map[string]any{"login": "author"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/swagger.v1.json":
			_, _ = w.Write([]byte(`{"paths":{"/repos/{owner}/{repo}/pulls/{index}/reviews":{"get":{},"post":{}},"/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments":{"get":{}},"/repos/{owner}/{repo}/pulls/{index}/requested_reviewers":{"post":{}}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42":
			_ = json.NewEncoder(w).Encode(pr)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			_ = json.NewEncoder(w).Encode([]any{pr})
		case r.URL.Path == "/api/v1/repos/acme/looper/pulls/42.diff":
			_, _ = w.Write([]byte("diff --git a/app.go b/app.go\n"))
		case r.URL.Path == "/api/v1/repos/acme/looper/issues/42/comments":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 8, "state": "REQUEST_CHANGES", "body": marker, "commit_id": headSHA, "comments_count": 1, "user": map[string]any{"login": "reviewer"}},
				{"id": 9, "state": "COMMENT", "body": "Follow-up", "comments_count": 0, "user": map[string]any{"login": "reviewer"}},
				{"id": 10, "state": "APPROVED", "dismissed": true, "comments_count": 0, "user": map[string]any{"login": "another-reviewer"}},
			})
		case r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews/8/comments":
			inlineReads++
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 101, "body": "Fix discount", "path": "app.go", "diff_hunk": "@@ -1 +1 @@", "updated_at": "2026-09-06T00:00:00Z", "user": map[string]any{"login": "reviewer"}, "resolver": nil}})
		case strings.HasPrefix(r.URL.Path, "/api/v1/repos/acme/looper/statuses/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "context": "unit", "status": "failure", "description": "price expected 80, got 20", "target_url": "https://ci.test/unit"}})
		case r.URL.Path == "/api/v1/repos/acme/looper/actions/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/requested_reviewers":
			var body struct {
				Reviewers []string `json:"reviewers"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Reviewers) != 1 || body.Reviewers[0] != "reviewer" {
				t.Errorf("review request body = %#v, %v", body, err)
			}
			requested = true
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42/reviews/8/dismissals":
			var body struct {
				Message string `json:"message"`
				Priors  bool   `json:"priors"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message != "Finding withdrawn" || body.Priors {
				t.Errorf("dismissal body = %#v, %v", body, err)
			}
			dismissed = true
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 8, "dismissed": true})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project", Provider: "forgejo", Repo: "acme/looper", RepoPath: cwd}},
	}
	ctx := context.Background()
	reviewerAdapter := reviewerGitHubAdapter{config: &cfg}
	review, err := reviewerAdapter.ViewPullRequest(ctx, reviewer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: cwd})
	if err != nil || !review.HasConflicts || !strings.Contains(review.ChecksSummary, "FAILURE") || review.URL != pr["html_url"] || review.ReviewDecision != "CHANGES_REQUESTED" || len(review.Comments) != 1 {
		t.Fatalf("review detail = %#v, %v", review, err)
	}
	prs, err := reviewerAdapter.ListOpenPullRequests(ctx, reviewer.ListOpenPullRequestsInput{Repo: "acme/looper", CWD: cwd})
	if err != nil || len(prs) != 1 || !prs[0].HasConflicts || inlineReads != 1 {
		t.Fatalf("discovery = %#v, %v; inline reads = %d, want no extra inline fetch", prs, err, inlineReads)
	}
	fixerAdapter := fixerGitHubAdapter{config: &cfg}
	fix, err := fixerAdapter.ViewPullRequest(ctx, fixer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: cwd})
	if err != nil || !fix.HasConflicts || fix.URL != pr["html_url"] || len(fix.Checks) != 1 || fix.Checks[0]["state"] != "FAILURE" || fix.Checks[0]["url"] != "https://ci.test/unit" || fix.Checks[0]["description"] != "price expected 80, got 20" {
		t.Fatalf("fix detail = %#v, %v", fix, err)
	}
	comments, err := fixerAdapter.ListNativeReviewComments(ctx, fixer.ListNativeReviewCommentsInput{Repo: "acme/looper", PRNumber: 42, CWD: cwd})
	if err != nil || len(comments) != 1 || comments[0].ReviewBody != marker || comments[0].ReviewState != "CHANGES_REQUESTED" || comments[0].ReviewAuthor != "reviewer" || comments[0].ReviewCommitID != headSHA || comments[0].IsResolved {
		t.Fatalf("native comments = %#v, %v", comments, err)
	}
	reviews, err := fixerAdapter.ListPullRequestReviews(ctx, fixer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: cwd})
	if err != nil || len(reviews) != 3 || reviews[2].State != "DISMISSED" || inlineReads != 2 {
		t.Fatalf("reviews = %#v, %v; inline reads = %d", reviews, err, inlineReads)
	}
	if err := fixerAdapter.AddPullRequestReviewers(ctx, fixer.PullRequestReviewersInput{Repo: "acme/looper", PRNumber: 42, Reviewers: []string{"reviewer"}, CWD: cwd}); err != nil || !requested {
		t.Fatalf("re-request = %v, requested = %v", err, requested)
	}
	if err := fixerAdapter.DismissReview(ctx, fixer.DismissReviewInput{Repo: "acme/looper", PRNumber: 42, ReviewID: 8, Message: "Finding withdrawn", CWD: cwd}); err != nil || !dismissed {
		t.Fatalf("dismiss = %v, dismissed = %v", err, dismissed)
	}
	// The same remote false can mean CHECKING, ERROR or WIP. When the exact
	// commits merge cleanly neither runner should advertise a conflict.
	pr["head"] = map[string]any{"sha": cleanSHA, "ref": "clean"}
	cleanReview, err := reviewerAdapter.ViewPullRequest(ctx, reviewer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: cwd})
	if err != nil || cleanReview.HasConflicts {
		t.Fatalf("false mergeability on clean reviewer PR = %#v, %v", cleanReview, err)
	}
	cleanFix, err := fixerAdapter.ViewPullRequest(ctx, fixer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: cwd})
	if err != nil || cleanFix.HasConflicts {
		t.Fatalf("false mergeability on clean fixer PR = %#v, %v", cleanFix, err)
	}

}
