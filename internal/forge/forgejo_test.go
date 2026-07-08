package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   string
}

func TestForgejoClientContract(t *testing.T) {
	t.Parallel()

	var requests []recordedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"), Body: string(body)})
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/forge/api/v1/user":
			writeJSON(t, w, http.StatusOK, map[string]any{"id": 42, "login": "ralph"})
		case r.Method == http.MethodGet && r.URL.Path == "/forge/api/v1/repos/acme/looper/issues":
			if got := r.URL.Query().Get("labels"); got != "planner,ready" {
				t.Fatalf("issue list labels query = %q, want planner,ready", got)
			}
			if got := r.URL.Query().Get("assignee"); got != "ralph" {
				t.Fatalf("issue list assignee query = %q, want ralph", got)
			}
			page := r.URL.Query().Get("page")
			switch page {
			case "1":
				w.Header().Set("X-Total-Pages", "2")
				writeJSON(t, w, http.StatusOK, []map[string]any{{"number": 7, "title": "Issue 7", "body": "body 7", "state": "open", "html_url": "https://example.test/issues/7", "updated_at": "2026-06-18T00:00:00Z", "user": map[string]any{"id": 1, "login": "octo"}, "labels": []map[string]any{{"id": 11, "name": "planner"}}, "assignees": []map[string]any{{"id": 42, "login": "ralph"}}}})
			case "2":
				writeJSON(t, w, http.StatusOK, []map[string]any{{"number": 8, "title": "Issue 8", "body": "body 8", "state": "open", "html_url": "https://example.test/issues/8", "updated_at": "2026-06-18T01:00:00Z", "user": map[string]any{"id": 2, "login": "marge"}}})
			default:
				t.Fatalf("unexpected issue list page %q", page)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/forge/api/v1/repos/acme/looper/issues/7":
			writeJSON(t, w, http.StatusOK, map[string]any{"number": 7, "title": "Issue 7", "body": "body 7", "state": "open", "html_url": "https://example.test/issues/7", "updated_at": "2026-06-18T00:00:00Z", "user": map[string]any{"id": 1, "login": "octo"}, "labels": []map[string]any{{"id": 11, "name": "planner"}}, "assignees": []map[string]any{{"id": 42, "login": "ralph"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/forge/api/v1/repos/acme/looper/issues/7/labels":
			writeJSON(t, w, http.StatusOK, []map[string]any{{"id": 11, "name": "planner"}, {"id": 12, "name": "ready"}})
		case r.Method == http.MethodDelete && r.URL.Path == "/forge/api/v1/repos/acme/looper/issues/7/labels/planner":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/forge/api/v1/repos/acme/looper/issues/7/labels/team/review":
			if got := r.URL.EscapedPath(); got != "/forge/api/v1/repos/acme/looper/issues/7/labels/team%2Freview" {
				t.Fatalf("slash label escaped path = %q, want encoded slash preserved", got)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/forge/api/v1/repos/acme/looper/issues/7/assignees":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/forge/api/v1/repos/acme/looper/issues/7/assignees":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/forge/api/v1/repos/acme/looper/issues/7/comments":
			writeJSON(t, w, http.StatusOK, map[string]any{"id": 301, "body": "comment body", "html_url": "https://example.test/comments/301", "updated_at": "2026-06-18T02:00:00Z", "user": map[string]any{"id": 42, "login": "ralph"}})
		case r.Method == http.MethodGet && r.URL.Path == "/forge/api/v1/repos/acme/looper/issues/7/comments":
			writeJSON(t, w, http.StatusOK, []map[string]any{
				{"id": 301, "body": "comment body", "html_url": "https://example.test/comments/301", "updated_at": "2026-06-18T02:00:00Z", "user": map[string]any{"id": 42, "login": "ralph"}},
				{"id": 302, "body": "follow-up", "html_url": "https://example.test/comments/302", "updated_at": "2026-06-18T02:30:00Z", "user": map[string]any{"id": 7, "login": "marge"}},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/forge/api/v1/repos/acme/looper/issues/comments/301":
			writeJSON(t, w, http.StatusOK, map[string]any{"id": 301, "body": "updated body", "html_url": "https://example.test/comments/301", "updated_at": "2026-06-18T03:00:00Z", "user": map[string]any{"id": 42, "login": "ralph"}})
		case r.Method == http.MethodGet && r.URL.Path == "/forge/api/v1/repos/acme/looper/pulls":
			if got := r.URL.Query().Get("labels"); got != "" {
				t.Fatalf("pull list labels query = %q, want empty client-side filtering", got)
			}
			if r.URL.Query().Get("page") == "2" {
				writeJSON(t, w, http.StatusOK, []map[string]any{})
				return
			}
			w.Header().Set("Link", `</forge/api/v1/repos/acme/looper/pulls?page=2>; rel="next"`)
			writeJSON(t, w, http.StatusOK, []map[string]any{
				{"number": 9, "title": "PR 9", "body": "body 9", "state": "open", "draft": true, "html_url": "https://example.test/pulls/9", "updated_at": "2026-06-18T04:00:00Z", "user": map[string]any{"id": 3, "login": "lisa"}, "head": map[string]any{"ref": "feature", "sha": "headsha"}, "base": map[string]any{"ref": "main", "sha": "basesha"}, "labels": []map[string]any{{"id": 13, "name": "review"}}, "assignees": []map[string]any{{"id": 42, "login": "ralph"}}},
				{"number": 10, "title": "PR 10", "body": "body 10", "state": "open", "html_url": "https://example.test/pulls/10", "updated_at": "2026-06-18T04:30:00Z", "user": map[string]any{"id": 4, "login": "maggie"}, "head": map[string]any{"ref": "other", "sha": "othersha"}, "base": map[string]any{"ref": "main", "sha": "basesha"}, "labels": []map[string]any{{"id": 14, "name": "other"}}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/forge/api/v1/repos/acme/looper/pulls/9":
			writeJSON(t, w, http.StatusOK, map[string]any{"number": 9, "title": "PR 9", "body": "body 9", "state": "open", "draft": true, "html_url": "https://example.test/pulls/9", "updated_at": "2026-06-18T04:00:00Z", "user": map[string]any{"id": 3, "login": "lisa"}, "head": map[string]any{"ref": "feature", "sha": "headsha"}, "base": map[string]any{"ref": "main", "sha": "basesha"}})
		case r.Method == http.MethodGet && r.URL.Path == "/forge/api/v1/repos/acme/looper/pulls/9.diff":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("diff --git a/foo b/foo"))
		case r.Method == http.MethodGet && r.URL.Path == "/forge/api/v1/repos/acme/looper/compare/main...feature":
			writeJSON(t, w, http.StatusOK, map[string]any{"status": "ahead", "ahead_by": 2, "behind_by": 0, "total_commits": 2})
		case r.Method == http.MethodPost && r.URL.Path == "/forge/api/v1/repos/acme/looper/pulls":
			writeJSON(t, w, http.StatusCreated, map[string]any{"number": 10, "title": "New PR", "body": "new body", "state": "open", "html_url": "https://example.test/pulls/10", "updated_at": "2026-06-18T05:00:00Z", "user": map[string]any{"id": 42, "login": "ralph"}, "head": map[string]any{"ref": "feat", "sha": "newsha"}, "base": map[string]any{"ref": "main", "sha": "basesha"}})
		case r.Method == http.MethodPatch && r.URL.Path == "/forge/api/v1/repos/acme/looper/pulls/10":
			writeJSON(t, w, http.StatusOK, map[string]any{"number": 10, "title": "Updated PR", "body": "updated body", "state": "open", "html_url": "https://example.test/pulls/10", "updated_at": "2026-06-18T06:00:00Z", "user": map[string]any{"id": 42, "login": "ralph"}, "head": map[string]any{"ref": "feat", "sha": "newsha"}, "base": map[string]any{"ref": "main", "sha": "basesha"}})
		case r.Method == http.MethodGet && r.URL.Path == "/forge/api/v1/repos/acme/looper/issues/99":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("token super-secret rejected"))
		default:
			t.Fatalf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer server.Close()

	client, err := NewForgejoClient(RepositoryRef{ProviderID: "fj", Kind: ProviderKindForgejo, BaseURL: server.URL + "/forge/", Repo: "acme/looper"}, "super-secret")
	if err != nil {
		t.Fatalf("NewForgejoClient() error = %v", err)
	}

	ctx := context.Background()
	identity, err := client.CurrentUser(ctx)
	if err != nil || identity.Login != "ralph" || identity.ID != 42 {
		t.Fatalf("CurrentUser() = %#v, %v", identity, err)
	}
	issues, err := client.ListOpenIssues(ctx, ListIssuesInput{Labels: []string{"planner", "ready"}, Assignee: "ralph"})
	if err != nil || len(issues) != 2 || issues[0].Labels[0].Name != "planner" || issues[0].Assignees[0].Login != "ralph" {
		t.Fatalf("ListOpenIssues() = %#v, %v", issues, err)
	}
	issue, err := client.ViewIssue(ctx, 7)
	if err != nil || issue.Number != 7 || issue.User.Login != "octo" {
		t.Fatalf("ViewIssue() = %#v, %v", issue, err)
	}
	labels, err := client.AddIssueLabels(ctx, 7, []string{"planner", "ready"})
	if err != nil || len(labels) != 2 {
		t.Fatalf("AddIssueLabels() = %#v, %v", labels, err)
	}
	if err := client.RemoveIssueLabel(ctx, 7, "planner"); err != nil {
		t.Fatalf("RemoveIssueLabel() error = %v", err)
	}
	if err := client.RemoveIssueLabel(ctx, 7, "team/review"); err != nil {
		t.Fatalf("RemoveIssueLabel() slash label error = %v", err)
	}
	if err := client.AddIssueAssignees(ctx, 7, []string{"ralph"}); err != nil {
		t.Fatalf("AddIssueAssignees() error = %v", err)
	}
	if err := client.RemoveIssueAssignees(ctx, 7, []string{"ralph"}); err != nil {
		t.Fatalf("RemoveIssueAssignees() error = %v", err)
	}
	comment, err := client.CreateIssueComment(ctx, CreateCommentInput{IssueNumber: 7, Body: "comment body"})
	if err != nil || comment.ID != 301 {
		t.Fatalf("CreateIssueComment() = %#v, %v", comment, err)
	}
	comments, err := client.ListIssueComments(ctx, 7)
	if err != nil || len(comments) != 2 || comments[1].User.Login != "marge" {
		t.Fatalf("ListIssueComments() = %#v, %v", comments, err)
	}
	comment, err = client.UpdateIssueComment(ctx, UpdateCommentInput{CommentID: 301, Body: "updated body"})
	if err != nil || comment.Body != "updated body" {
		t.Fatalf("UpdateIssueComment() = %#v, %v", comment, err)
	}
	pulls, err := client.ListOpenPullRequests(ctx, ListPullRequestsInput{Labels: []string{"review"}})
	if err != nil || len(pulls) != 1 || pulls[0].Head.Name != "feature" {
		t.Fatalf("ListOpenPullRequests() = %#v, %v", pulls, err)
	}
	if !pulls[0].IsDraft {
		t.Fatalf("ListOpenPullRequests() = %#v, want draft preserved", pulls)
	}
	pull, err := client.ViewPullRequest(ctx, 9)
	if err != nil || pull.Base.Name != "main" {
		t.Fatalf("ViewPullRequest() = %#v, %v", pull, err)
	}
	if !pull.IsDraft {
		t.Fatalf("ViewPullRequest() = %#v, want draft preserved", pull)
	}
	diff, err := client.PullRequestDiff(ctx, 9)
	if err != nil || !strings.Contains(diff, "diff --git") {
		t.Fatalf("PullRequestDiff() = %q, %v", diff, err)
	}
	comparison, err := client.CompareBranches(ctx, CompareBranchesInput{Base: "main", Head: "feature"})
	if err != nil || comparison.Status != "ahead" || comparison.AheadBy != 2 || comparison.TotalCommits != 2 {
		t.Fatalf("CompareBranches() = %#v, %v", comparison, err)
	}
	pull, err = client.CreatePullRequest(ctx, CreatePullRequestInput{Title: "New PR", Body: "new body", Head: "feat", Base: "main"})
	if err != nil || pull.Number != 10 {
		t.Fatalf("CreatePullRequest() = %#v, %v", pull, err)
	}
	title := "Updated PR"
	body := "updated body"
	pull, err = client.UpdatePullRequest(ctx, UpdatePullRequestInput{Number: 10, Title: &title, Body: &body})
	if err != nil || pull.Title != "Updated PR" {
		t.Fatalf("UpdatePullRequest() = %#v, %v", pull, err)
	}
	_, err = client.ViewIssue(ctx, 99)
	if err == nil || strings.Contains(err.Error(), "super-secret") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("sanitized error = %v, want redacted token", err)
	}

	if got := client.Repository().BaseURL; got != server.URL+"/forge" {
		t.Fatalf("Repository().BaseURL = %q, want %q", got, server.URL+"/forge")
	}
	if got := client.Capabilities().ReviewPublish; got != ReviewPublishCommentOnly {
		t.Fatalf("Capabilities().ReviewPublish = %q, want %q", got, ReviewPublishCommentOnly)
	}
	if len(requests) == 0 || requests[0].Auth != "token super-secret" {
		t.Fatalf("Authorization header = %#v, want token auth", requests)
	}
	if !containsRequest(requests, http.MethodDelete, "/forge/api/v1/repos/acme/looper/issues/7/assignees", `{"assignees":["ralph"]}`) {
		t.Fatalf("requests = %#v, want assignee delete payload", requests)
	}
	if !containsRequest(requests, http.MethodPatch, "/forge/api/v1/repos/acme/looper/pulls/10", `{"body":"updated body","title":"Updated PR"}`) && !containsRequest(requests, http.MethodPatch, "/forge/api/v1/repos/acme/looper/pulls/10", `{"title":"Updated PR","body":"updated body"}`) {
		t.Fatalf("requests = %#v, want patch PR payload", requests)
	}
}

func TestNewForgejoClientFromConfigReadsTokenEnv(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "token-value")
	client, err := NewForgejoClientFromConfig(config.ProviderConfig{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: stringPtr("FORGEJO_TOKEN")}, "acme/looper")
	if err != nil {
		t.Fatalf("NewForgejoClientFromConfig() error = %v", err)
	}
	if client.Repository().ProviderID != "fj" || client.Repository().Repo != "acme/looper" {
		t.Fatalf("Repository() = %#v", client.Repository())
	}
}

func TestListOpenPullRequestsAppliesLimitAfterLabelFiltering(t *testing.T) {
	t.Parallel()

	var requests []recordedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"), Body: string(body)})
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/acme/looper/pulls" {
			t.Fatalf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if got := r.URL.Query().Get("labels"); got != "" {
			t.Fatalf("pull list labels query = %q, want empty client-side filtering", got)
		}
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("Link", `</api/v1/repos/acme/looper/pulls?page=2>; rel="next"`)
			writeJSON(t, w, http.StatusOK, []map[string]any{{
				"number": 1, "title": "Skip me", "body": "body 1", "state": "open",
				"head":   map[string]any{"ref": "skip", "sha": "sha-1"},
				"base":   map[string]any{"ref": "main", "sha": "base"},
				"user":   map[string]any{"id": 1, "login": "octo"},
				"labels": []map[string]any{{"id": 11, "name": "other"}},
			}})
		case "2":
			writeJSON(t, w, http.StatusOK, []map[string]any{{
				"number": 2, "title": "Match me", "body": "body 2", "state": "open",
				"head":   map[string]any{"ref": "match", "sha": "sha-2"},
				"base":   map[string]any{"ref": "main", "sha": "base"},
				"user":   map[string]any{"id": 2, "login": "marge"},
				"labels": []map[string]any{{"id": 12, "name": "review"}},
			}})
		default:
			t.Fatalf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer server.Close()

	client, err := NewForgejoClient(RepositoryRef{ProviderID: "fj", Kind: ProviderKindForgejo, BaseURL: server.URL, Repo: "acme/looper"}, "secret")
	if err != nil {
		t.Fatalf("NewForgejoClient() error = %v", err)
	}

	pulls, err := client.ListOpenPullRequests(context.Background(), ListPullRequestsInput{Labels: []string{"review"}, Limit: 1})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(pulls) != 1 || pulls[0].Number != 2 {
		t.Fatalf("pulls = %#v, want second-page labeled PR", pulls)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %#v, want two pages fetched before limit satisfied", requests)
	}
}

func TestCompareBranchesNormalizesForgejoTotalCommitsOnlyResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/v1/repos/acme/looper/compare/main...feature%2Freview" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"total_commits": 1, "commits": []map[string]any{{"sha": "abc"}}})
	}))
	defer server.Close()

	client, err := NewForgejoClient(RepositoryRef{ProviderID: "fj", Kind: ProviderKindForgejo, BaseURL: server.URL, Repo: "acme/looper"}, "secret")
	if err != nil {
		t.Fatalf("NewForgejoClient() error = %v", err)
	}

	comparison, err := client.CompareBranches(context.Background(), CompareBranchesInput{Base: "main", Head: "feature/review"})
	if err != nil {
		t.Fatalf("CompareBranches() error = %v", err)
	}
	if comparison.Status != "ahead" || comparison.AheadBy != 1 || comparison.TotalCommits != 1 {
		t.Fatalf("comparison = %#v, want normalized ahead result", comparison)
	}
}

func TestNewForgejoClientRejectsInvalidInputs(t *testing.T) {
	_, err := NewForgejoClient(RepositoryRef{ProviderID: "", BaseURL: "https://forgejo.example.test", Repo: "acme/looper"}, "token")
	if err == nil || !strings.Contains(err.Error(), "provider id") {
		t.Fatalf("missing provider id error = %v", err)
	}
	_, err = NewForgejoClient(RepositoryRef{ProviderID: "fj", BaseURL: "ftp://forgejo.example.test", Repo: "acme/looper"}, "token")
	if err == nil || !strings.Contains(err.Error(), "absolute http(s) URL") {
		t.Fatalf("invalid baseURL error = %v", err)
	}
	_, err = NewForgejoClientFromConfig(config.ProviderConfig{ID: "fj", Kind: config.ProviderKindGitHub, BaseURL: "https://github.com", TokenEnv: stringPtr("TOKEN")}, "acme/looper")
	if err == nil || !strings.Contains(err.Error(), "want forgejo") {
		t.Fatalf("wrong provider kind error = %v", err)
	}
	_, err = NewForgejoClientFromConfig(config.ProviderConfig{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: stringPtr("MISSING_TOKEN")}, "acme/looper")
	if err == nil || !strings.Contains(err.Error(), "environment variable MISSING_TOKEN is required") {
		t.Fatalf("missing token env error = %v", err)
	}
}

func TestForgejoClientTimeoutOption(t *testing.T) {
	t.Parallel()
	client, err := NewForgejoClient(RepositoryRef{ProviderID: "fj", BaseURL: "https://forgejo.example.test", Repo: "acme/looper"}, "token", WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("NewForgejoClient() error = %v", err)
	}
	if client.httpClient.Timeout != 5*time.Second {
		t.Fatalf("httpClient.Timeout = %v, want %v", client.httpClient.Timeout, 5*time.Second)
	}
	customHTTPClient := &http.Client{}
	client, err = NewForgejoClient(RepositoryRef{ProviderID: "fj", BaseURL: "https://forgejo.example.test", Repo: "acme/looper"}, "token", WithHTTPClient(customHTTPClient), WithTimeout(7*time.Second))
	if err != nil {
		t.Fatalf("NewForgejoClient() with custom client error = %v", err)
	}
	if client.httpClient != customHTTPClient || client.httpClient.Timeout != 7*time.Second {
		t.Fatalf("custom client = %#v, want timeout applied", client.httpClient)
	}
}

func TestForgejoPullRequestDiffRejectsOversizedResponses(t *testing.T) {
	t.Parallel()

	diffPayload := strings.Repeat("d", maxForgejoResponseBodyBytes+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/acme/looper/pulls/9.diff" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(diffPayload))
	}))
	defer server.Close()

	client, err := NewForgejoClient(RepositoryRef{ProviderID: "fj", Kind: ProviderKindForgejo, BaseURL: server.URL, Repo: "acme/looper"}, "super-secret")
	if err != nil {
		t.Fatalf("NewForgejoClient() error = %v", err)
	}

	_, err = client.PullRequestDiff(context.Background(), 9)
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("PullRequestDiff() error = %v, want oversized response failure", err)
	}
}

func TestForgejoListPullRequestReviewCommentsContract(t *testing.T) {
	t.Parallel()

	var requests []recordedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"), Body: string(body)})
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		switch r.URL.Path {
		case "/api/v1/repos/acme/looper/pulls/42/reviews":
			switch r.URL.Query().Get("page") {
			case "1":
				w.Header().Set("X-Total-Pages", "2")
				writeJSON(t, w, http.StatusOK, []map[string]any{{"id": 201}})
			case "2":
				writeJSON(t, w, http.StatusOK, []map[string]any{{"id": 202}, {"id": 203}})
			default:
				t.Fatalf("unexpected reviews page %q", r.URL.Query().Get("page"))
			}
		case "/api/v1/repos/acme/looper/pulls/42/reviews/201/comments":
			writeJSON(t, w, http.StatusOK, []map[string]any{{
				"id": 101, "body": "resolver absent", "path": "app.go", "commit_id": "head1", "original_commit_id": "base1",
				"position": 4, "original_position": 4, "diff_hunk": "@@ -1 +1 @@", "html_url": "https://example.test/comments/101",
				"pull_request_review_id": 201, "updated_at": "2026-07-07T01:00:00Z", "user": map[string]any{"id": 1, "login": "octo"},
			}})
		case "/api/v1/repos/acme/looper/pulls/42/reviews/202/comments":
			writeJSON(t, w, http.StatusOK, []map[string]any{{
				"id": 102, "body": "resolver null", "path": "app.go", "commit_id": "head2", "original_commit_id": "base2",
				"position": 8, "original_position": 7, "diff_hunk": "@@ -2 +2 @@", "html_url": "https://example.test/comments/102",
				"pull_request_review_id": 202, "updated_at": "2026-07-07T02:00:00Z", "user": map[string]any{"id": 2, "login": "marge"}, "resolver": nil,
			}})
		case "/api/v1/repos/acme/looper/pulls/42/reviews/203/comments":
			writeJSON(t, w, http.StatusOK, []map[string]any{{
				"id": 103, "body": "resolver object", "path": "app.go", "commit_id": "head3", "original_commit_id": "base3",
				"position": 9, "original_position": 9, "diff_hunk": "@@ -3 +3 @@", "html_url": "https://example.test/comments/103",
				"pull_request_review_id": 203, "updated_at": "2026-07-07T03:00:00Z", "user": map[string]any{"id": 3, "login": "lisa"}, "resolver": map[string]any{"id": 9, "login": "ralph"},
			}})
		default:
			t.Fatalf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer server.Close()

	client := newForgejoTestClient(t, server.URL)
	comments, err := client.ListPullRequestReviewComments(context.Background(), 42)
	if err != nil {
		t.Fatalf("ListPullRequestReviewComments() error = %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("len(comments) = %d, want 3", len(comments))
	}
	if comments[0].Resolver.Present {
		t.Fatalf("comments[0].Resolver = %#v, want absent resolver", comments[0].Resolver)
	}
	if !comments[1].Resolver.Present || comments[1].Resolver.Value != nil {
		t.Fatalf("comments[1].Resolver = %#v, want explicit null resolver", comments[1].Resolver)
	}
	if !comments[2].Resolver.Present || comments[2].Resolver.Value == nil || comments[2].Resolver.Value.Login != "ralph" {
		t.Fatalf("comments[2].Resolver = %#v, want resolver object", comments[2].Resolver)
	}
	if comments[2].PullRequestReviewID != 203 || comments[2].OriginalPosition != 9 {
		t.Fatalf("comments[2] = %#v, want decoded review comment fields", comments[2])
	}
	if len(requests) != 5 {
		t.Fatalf("requests = %#v, want review pagination plus per-review comment fetches", requests)
	}
}

func TestForgejoListPullRequestReviewCommentsEmptyList(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/acme/looper/pulls/42/reviews" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, http.StatusOK, []map[string]any{})
	}))
	defer server.Close()

	client := newForgejoTestClient(t, server.URL)
	comments, err := client.ListPullRequestReviewComments(context.Background(), 42)
	if err != nil {
		t.Fatalf("ListPullRequestReviewComments() error = %v", err)
	}
	if len(comments) != 0 {
		t.Fatalf("comments = %#v, want empty list", comments)
	}
}

func TestForgejoResolvePullRequestReviewCommentContract(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/acme/looper/pulls/comments/101/resolve" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := newForgejoTestClient(t, server.URL)
	if err := client.ResolvePullRequestReviewComment(context.Background(), 42, 101); err != nil {
		t.Fatalf("ResolvePullRequestReviewComment() error = %v", err)
	}
}

func TestForgejoResolvePullRequestReviewCommentClassifiesHTTPStatusErrors(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(http.StatusText(status)))
			}))
			defer server.Close()

			client := newForgejoTestClient(t, server.URL)
			err := client.ResolvePullRequestReviewComment(context.Background(), 42, 101)
			if err == nil {
				t.Fatal("ResolvePullRequestReviewComment() error = nil, want HTTP error")
			}
			var httpErr *ForgejoHTTPError
			if !errors.As(err, &httpErr) {
				t.Fatalf("error type = %T, want *ForgejoHTTPError", err)
			}
			if httpErr.StatusCode != status {
				t.Fatalf("StatusCode = %d, want %d", httpErr.StatusCode, status)
			}
		})
	}
}

func TestForgejoResolvePullRequestReviewCommentSanitizesErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("token super-secret rejected"))
	}))
	defer server.Close()

	client := newForgejoTestClient(t, server.URL)
	err := client.ResolvePullRequestReviewComment(context.Background(), 42, 101)
	if err == nil || strings.Contains(err.Error(), "super-secret") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("sanitized error = %v, want redacted token", err)
	}
}

func TestForgejoResolverFieldDecodingDistinguishesAbsentNullAndObject(t *testing.T) {
	t.Parallel()

	var absent forgejoPullRequestReviewComment
	if err := json.Unmarshal([]byte(`{"id":1}`), &absent); err != nil {
		t.Fatalf("unmarshal absent resolver: %v", err)
	}
	if absent.Resolver.Present {
		t.Fatalf("absent resolver = %#v, want not present", absent.Resolver)
	}

	var explicitNull forgejoPullRequestReviewComment
	if err := json.Unmarshal([]byte(`{"id":2,"resolver":null}`), &explicitNull); err != nil {
		t.Fatalf("unmarshal null resolver: %v", err)
	}
	if !explicitNull.Resolver.Present || explicitNull.Resolver.Value != nil {
		t.Fatalf("null resolver = %#v, want present nil", explicitNull.Resolver)
	}

	var object forgejoPullRequestReviewComment
	if err := json.Unmarshal([]byte(`{"id":3,"resolver":{"id":9,"login":"ralph"}}`), &object); err != nil {
		t.Fatalf("unmarshal object resolver: %v", err)
	}
	if !object.Resolver.Present || object.Resolver.Value == nil || object.Resolver.Value.Login != "ralph" {
		t.Fatalf("object resolver = %#v, want present user", object.Resolver)
	}
}

func newForgejoTestClient(tb testing.TB, baseURL string) *ForgejoClient {
	tb.Helper()
	client, err := NewForgejoClient(RepositoryRef{ProviderID: "fj", Kind: ProviderKindForgejo, BaseURL: baseURL, Repo: "acme/looper"}, "super-secret")
	if err != nil {
		tb.Fatalf("NewForgejoClient() error = %v", err)
	}
	return client
}

func TestForgejoHTTPErrorStatusCodeMethod(t *testing.T) {
	t.Parallel()
	var nilErr *ForgejoHTTPError
	if nilErr.HTTPStatusCode() != 0 {
		t.Fatalf("nil HTTPStatusCode() = %d, want 0", nilErr.HTTPStatusCode())
	}
	if (&ForgejoHTTPError{StatusCode: http.StatusNotFound}).HTTPStatusCode() != http.StatusNotFound {
		t.Fatal("HTTPStatusCode() did not return status code")
	}
}

func TestForgejoAPIURLPreservesReviewCommentResolvePathEscaping(t *testing.T) {
	t.Parallel()
	client := newForgejoTestClient(t, "https://forgejo.example.test/root")
	apiURL, err := client.apiURL(client.repoPath("pulls", "comments", "101", "resolve"))
	if err != nil {
		t.Fatalf("apiURL() error = %v", err)
	}
	if got, want := apiURL.Path, "/root/api/v1/repos/acme/looper/pulls/comments/101/resolve"; got != want {
		t.Fatalf("apiURL.Path = %q, want %q", got, want)
	}
	if _, err := url.Parse(apiURL.String()); err != nil {
		t.Fatalf("apiURL string parse error = %v", err)
	}
}

func writeJSON(tb testing.TB, w http.ResponseWriter, status int, payload any) {
	tb.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		tb.Fatalf("encode response: %v", err)
	}
}

func containsRequest(requests []recordedRequest, method string, path string, body string) bool {
	for _, request := range requests {
		if request.Method == method && request.Path == path && request.Body == body {
			return true
		}
	}
	return false
}

func stringPtr(value string) *string { return &value }

func TestSanitizeForgejoErrorBodyDefaults(t *testing.T) {
	t.Parallel()
	if got := sanitizeForgejoErrorBody(nil, "token"); got != http.StatusText(http.StatusInternalServerError) {
		t.Fatalf("sanitizeForgejoErrorBody(nil) = %q", got)
	}
	message := sanitizeForgejoErrorBody([]byte(fmt.Sprintf("failed with %s", "token")), "token")
	if strings.Contains(message, "token") || !strings.Contains(message, "[REDACTED]") {
		t.Fatalf("sanitizeForgejoErrorBody() = %q", message)
	}
}
