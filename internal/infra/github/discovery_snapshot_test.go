package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
)

func TestDiscoverySnapshotCachesPerProjectDataAndTickLoginByCWD(t *testing.T) {
	t.Parallel()

	counts := map[string]int{}
	gateway := New(Options{GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
		cmd := strings.Join(options.Args, " ")
		switch {
		case strings.Contains(cmd, "pr list"):
			counts["pr_list"]++
			return shell.Result{Stdout: `[
				{"number":1,"title":"PR 1","state":"OPEN","labels":[{"name":"bug"}],"baseRefName":"main","headRefOid":"sha-1","author":{"login":"octo"}},
				{"number":2,"title":"PR 2","state":"OPEN","labels":[{"name":"feature"}],"baseRefName":"release","headRefOid":"sha-2","author":{"login":"other"}},
				{"number":3,"title":"PR 3","state":"OPEN","labels":[{"name":"feature"}],"baseRefName":"Release","headRefOid":"sha-3","author":{"login":"other"}}
			]`}, nil
		case strings.Contains(cmd, "issue list"):
			counts["issue_list"]++
			return shell.Result{Stdout: `[
				{"number":11,"title":"Issue 11","body":"body","url":"https://example.com/issues/11","state":"OPEN","assignees":[{"login":"octo"}],"labels":[{"name":"ready"}]},
				{"number":12,"title":"Issue 12","body":"body","url":"https://example.com/issues/12","state":"OPEN","assignees":[{"login":"other"}],"labels":[{"name":"other"}]}
			]`}, nil
		case strings.Contains(cmd, "pr view 1") && strings.Contains(cmd, "statusCheckRollup"):
			counts["pr_view"]++
			return shell.Result{Stdout: `{"number":1,"title":"PR 1","body":"body","url":"https://example.com/pulls/1","state":"OPEN","labels":[{"name":"bug"}],"headRefName":"feature","baseRefName":"main","headRefOid":"sha-1","baseRefOid":"base-1","author":{"login":"octo"},"comments":[],"reviewRequests":[],"reviews":[],"statusCheckRollup":[]}`}, nil
		case strings.Contains(cmd, "api graphql") && strings.Contains(cmd, "reviewThreads"):
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`}, nil
		case strings.Contains(cmd, "api user --jq .login"):
			counts["user_login"]++
			switch options.CWD {
			case "/repo-one":
				return shell.Result{Stdout: "octo\n"}, nil
			case "/repo-two":
				return shell.Result{Stdout: "other\n"}, nil
			default:
				return shell.Result{}, errors.New("unexpected cwd for user login: " + options.CWD)
			}
		default:
			return shell.Result{}, errors.New("unexpected command: " + cmd)
		}
	}})

	tick := NewDiscoveryTickState()
	options := DiscoverySnapshotOptions{PullRequestLimit: 100, IssueLimit: 100}
	projectOneCtx := ContextWithDiscoverySnapshot(context.Background(), NewDiscoverySnapshot(gateway, tick, options))
	projectTwoCtx := ContextWithDiscoverySnapshot(context.Background(), NewDiscoverySnapshot(gateway, tick, options))

	prs, err := gateway.ListOpenPullRequests(projectOneCtx, ListOpenPullRequestsInput{Repo: "acme/looper", CWD: "/repo-one", Limit: 10, Label: "bug"})
	if err != nil {
		t.Fatalf("ListOpenPullRequests(label) error = %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 1 {
		t.Fatalf("ListOpenPullRequests(label) = %#v, want only PR 1", prs)
	}
	prs, err = gateway.ListOpenPullRequests(projectOneCtx, ListOpenPullRequestsInput{Repo: "acme/looper", CWD: "/repo-one", Limit: 10, Author: "octo"})
	if err != nil {
		t.Fatalf("ListOpenPullRequests(author) error = %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 1 {
		t.Fatalf("ListOpenPullRequests(author) = %#v, want only PR 1", prs)
	}
	prs, err = gateway.ListOpenPullRequests(projectOneCtx, ListOpenPullRequestsInput{Repo: "acme/looper", CWD: "/repo-one", Limit: 10, BaseRefName: "main"})
	if err != nil {
		t.Fatalf("ListOpenPullRequests(base) error = %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 1 {
		t.Fatalf("ListOpenPullRequests(base) = %#v, want only PR 1", prs)
	}
	prs, err = gateway.ListOpenPullRequests(projectOneCtx, ListOpenPullRequestsInput{Repo: "acme/looper", CWD: "/repo-one", Limit: 10, BaseRefName: "release"})
	if err != nil {
		t.Fatalf("ListOpenPullRequests(base case) error = %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 2 {
		t.Fatalf("ListOpenPullRequests(base case) = %#v, want only PR 2", prs)
	}
	issues, err := gateway.ListOpenIssues(projectOneCtx, ListOpenIssuesInput{Repo: "acme/looper", CWD: "/repo-one", Limit: 10, Assignee: "octo", Label: "ready"})
	if err != nil {
		t.Fatalf("ListOpenIssues() error = %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 11 {
		t.Fatalf("ListOpenIssues() = %#v, want only issue 11", issues)
	}
	if _, err := gateway.ViewPullRequest(projectOneCtx, ViewPullRequestInput{Repo: "acme/looper", PRNumber: 1, CWD: "/repo-one"}); err != nil {
		t.Fatalf("ViewPullRequest(first) error = %v", err)
	}
	if _, err := gateway.ViewPullRequest(projectOneCtx, ViewPullRequestInput{Repo: "acme/looper", PRNumber: 1, CWD: "/repo-one"}); err != nil {
		t.Fatalf("ViewPullRequest(second) error = %v", err)
	}
	login, err := gateway.GetCurrentUserLogin(projectOneCtx, "/repo-one")
	if err != nil {
		t.Fatalf("GetCurrentUserLogin(project one) error = %v", err)
	}
	if login != "octo" {
		t.Fatalf("GetCurrentUserLogin(project one) = %q, want octo", login)
	}
	login, err = gateway.GetCurrentUserLogin(projectTwoCtx, "/repo-two")
	if err != nil {
		t.Fatalf("GetCurrentUserLogin(project two) error = %v", err)
	}
	if login != "other" {
		t.Fatalf("GetCurrentUserLogin(project two) = %q, want other", login)
	}

	if got := counts["pr_list"]; got != 1 {
		t.Fatalf("pr list calls = %d, want 1", got)
	}
	if got := counts["issue_list"]; got != 1 {
		t.Fatalf("issue list calls = %d, want 1", got)
	}
	if got := counts["pr_view"]; got != 1 {
		t.Fatalf("pr view calls = %d, want 1", got)
	}
	if got := counts["user_login"]; got != 2 {
		t.Fatalf("user login calls = %d, want 2", got)
	}
}

func TestDiscoverySnapshotDoesNotCacheFailedPullRequestDetailFetch(t *testing.T) {
	t.Parallel()

	viewCalls := 0
	gateway := New(Options{GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
		cmd := strings.Join(options.Args, " ")
		switch {
		case strings.Contains(cmd, "pr view 7") && strings.Contains(cmd, "statusCheckRollup"):
			viewCalls++
			if viewCalls == 1 {
				return shell.Result{}, errors.New("temporary gh failure")
			}
			return shell.Result{Stdout: `{"number":7,"title":"PR 7","body":"body","url":"https://example.com/pulls/7","state":"OPEN","labels":[],"headRefName":"feature","baseRefName":"main","headRefOid":"sha-7","baseRefOid":"base-7","author":{"login":"octo"},"comments":[],"reviewRequests":[],"reviews":[],"statusCheckRollup":[]}`}, nil
		case strings.Contains(cmd, "api graphql") && strings.Contains(cmd, "reviewThreads"):
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`}, nil
		default:
			return shell.Result{}, errors.New("unexpected command: " + cmd)
		}
	}})

	ctx := ContextWithDiscoverySnapshot(context.Background(), NewDiscoverySnapshot(gateway, NewDiscoveryTickState(), DiscoverySnapshotOptions{PullRequestLimit: 100, IssueLimit: 100}))
	if _, err := gateway.ViewPullRequest(ctx, ViewPullRequestInput{Repo: "acme/looper", PRNumber: 7, CWD: "/repo"}); err == nil {
		t.Fatal("ViewPullRequest(first) error = nil, want error")
	}
	if _, err := gateway.ViewPullRequest(ctx, ViewPullRequestInput{Repo: "acme/looper", PRNumber: 7, CWD: "/repo"}); err != nil {
		t.Fatalf("ViewPullRequest(second) error = %v", err)
	}
	if viewCalls != 2 {
		t.Fatalf("pr view calls = %d, want 2 after retry", viewCalls)
	}
}

func TestDiscoverySnapshotUsesGatewayDiscoveryTTLCacheAcrossTicks(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)
	prListCalls := 0
	gateway := New(Options{
		Now:               func() time.Time { return now },
		DiscoveryCacheTTL: 30 * time.Second,
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			cmd := strings.Join(options.Args, " ")
			if strings.Contains(cmd, "pr list") {
				prListCalls++
				return shell.Result{Stdout: `[
					{"number":1,"title":"PR 1","state":"OPEN","labels":[{"name":"bug"}],"baseRefName":"main","headRefOid":"sha-1","author":{"login":"octo"},"reviews":[{"state":"APPROVED","author":{"login":"reviewer"},"commit":{"oid":"sha-1"},"comments":[{"body":"ok"}]}]}
				]`}, nil
			}
			return shell.Result{}, errors.New("unexpected command: " + cmd)
		},
	})

	firstCtx := ContextWithDiscoverySnapshot(context.Background(), NewDiscoverySnapshot(gateway, NewDiscoveryTickState(), DiscoverySnapshotOptions{PullRequestLimit: 100}))
	first, err := gateway.ListOpenPullRequests(firstCtx, ListOpenPullRequestsInput{Repo: "acme/looper", CWD: "/repo", Limit: 10})
	if err != nil {
		t.Fatalf("ListOpenPullRequests(first) error = %v", err)
	}
	first[0].Labels[0] = "mutated"
	firstReviewAuthor, ok := first[0].Reviews[0]["author"].(map[string]any)
	if !ok {
		t.Fatalf("review author = %#v, want object", first[0].Reviews[0]["author"])
	}
	firstReviewAuthor["login"] = "mutated"
	firstReviewCommit, ok := first[0].Reviews[0]["commit"].(map[string]any)
	if !ok {
		t.Fatalf("review commit = %#v, want object", first[0].Reviews[0]["commit"])
	}
	firstReviewCommit["oid"] = "mutated"
	firstReviewComments, ok := first[0].Reviews[0]["comments"].([]any)
	if !ok {
		t.Fatalf("review comments = %#v, want array", first[0].Reviews[0]["comments"])
	}
	firstReviewComment, ok := firstReviewComments[0].(map[string]any)
	if !ok {
		t.Fatalf("review comment = %#v, want object", firstReviewComments[0])
	}
	firstReviewComment["body"] = "mutated"

	now = now.Add(10 * time.Second)
	secondCtx := ContextWithDiscoverySnapshot(context.Background(), NewDiscoverySnapshot(gateway, NewDiscoveryTickState(), DiscoverySnapshotOptions{PullRequestLimit: 100}))
	second, err := gateway.ListOpenPullRequests(secondCtx, ListOpenPullRequestsInput{Repo: "acme/looper", CWD: "/repo", Limit: 10})
	if err != nil {
		t.Fatalf("ListOpenPullRequests(second) error = %v", err)
	}
	if prListCalls != 1 {
		t.Fatalf("pr list calls after ttl hit = %d, want 1", prListCalls)
	}
	if got := second[0].Labels[0]; got != "bug" {
		t.Fatalf("cached label = %q, want cache to be isolated from caller mutation", got)
	}
	secondReviewAuthor, ok := second[0].Reviews[0]["author"].(map[string]any)
	if !ok {
		t.Fatalf("cached review author = %#v, want object", second[0].Reviews[0]["author"])
	}
	if got := secondReviewAuthor["login"]; got != "reviewer" {
		t.Fatalf("cached review author login = %q, want cache to be isolated from caller mutation", got)
	}
	secondReviewCommit, ok := second[0].Reviews[0]["commit"].(map[string]any)
	if !ok {
		t.Fatalf("cached review commit = %#v, want object", second[0].Reviews[0]["commit"])
	}
	if got := secondReviewCommit["oid"]; got != "sha-1" {
		t.Fatalf("cached review commit oid = %q, want cache to be isolated from caller mutation", got)
	}
	secondReviewComments, ok := second[0].Reviews[0]["comments"].([]any)
	if !ok {
		t.Fatalf("cached review comments = %#v, want array", second[0].Reviews[0]["comments"])
	}
	secondReviewComment, ok := secondReviewComments[0].(map[string]any)
	if !ok {
		t.Fatalf("cached review comment = %#v, want object", secondReviewComments[0])
	}
	if got := secondReviewComment["body"]; got != "ok" {
		t.Fatalf("cached review comment body = %q, want cache to be isolated from caller mutation", got)
	}

	now = now.Add(31 * time.Second)
	thirdCtx := ContextWithDiscoverySnapshot(context.Background(), NewDiscoverySnapshot(gateway, NewDiscoveryTickState(), DiscoverySnapshotOptions{PullRequestLimit: 100}))
	if _, err := gateway.ListOpenPullRequests(thirdCtx, ListOpenPullRequestsInput{Repo: "acme/looper", CWD: "/repo", Limit: 10}); err != nil {
		t.Fatalf("ListOpenPullRequests(third) error = %v", err)
	}
	if prListCalls != 2 {
		t.Fatalf("pr list calls after ttl expiry = %d, want 2", prListCalls)
	}
}

func TestDiscoverySnapshotCachesReviewRequestedPullRequestsPerTickAndTTL(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)
	graphQLCalls := 0
	gateway := New(Options{
		Now:               func() time.Time { return now },
		DiscoveryCacheTTL: 30 * time.Second,
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			cmd := strings.Join(options.Args, " ")
			if !strings.Contains(cmd, "api graphql") {
				return shell.Result{}, errors.New("unexpected command: " + cmd)
			}
			if !strings.Contains(cmd, "repo:acme/looper is:pr is:open review-requested:reviewer") {
				return shell.Result{}, errors.New("missing review-requested search qualifier: " + cmd)
			}
			graphQLCalls++
			switch graphQLCalls {
			case 1:
				return shell.Result{Stdout: reviewRequestedSearchResponse(7, "sha-7")}, nil
			case 2:
				return shell.Result{Stdout: reviewRequestedSearchResponse(8, "sha-8")}, nil
			default:
				return shell.Result{}, errors.New("unexpected extra graphql call: " + cmd)
			}
		},
	})

	firstCtx := ContextWithDiscoverySnapshot(context.Background(), NewDiscoverySnapshot(gateway, NewDiscoveryTickState(), DiscoverySnapshotOptions{PullRequestLimit: 100}))
	first, err := gateway.ListReviewRequestedPullRequests(firstCtx, ListReviewRequestedPullRequestsInput{Repo: "acme/looper", CWD: "/repo", Reviewer: "Reviewer", Limit: 10})
	if err != nil {
		t.Fatalf("ListReviewRequestedPullRequests(first) error = %v", err)
	}
	first[0].Labels[0] = "mutated"
	first[0].ReviewRequests[0] = "mutated"

	again, err := gateway.ListReviewRequestedPullRequests(firstCtx, ListReviewRequestedPullRequestsInput{Repo: "acme/looper", CWD: "/repo", Reviewer: "reviewer", Limit: 10})
	if err != nil {
		t.Fatalf("ListReviewRequestedPullRequests(same tick) error = %v", err)
	}
	if graphQLCalls != 1 {
		t.Fatalf("graphql calls in same tick = %d, want 1", graphQLCalls)
	}
	if again[0].Number != 7 || again[0].Labels[0] != "ready" || again[0].ReviewRequests[0] != "reviewer" {
		t.Fatalf("same tick PR = %#v, want stable cloned snapshot for PR 7", again[0])
	}

	now = now.Add(10 * time.Second)
	secondCtx := ContextWithDiscoverySnapshot(context.Background(), NewDiscoverySnapshot(gateway, NewDiscoveryTickState(), DiscoverySnapshotOptions{PullRequestLimit: 100}))
	second, err := gateway.ListReviewRequestedPullRequests(secondCtx, ListReviewRequestedPullRequestsInput{Repo: "acme/looper", CWD: "/repo", Reviewer: "reviewer", Limit: 10})
	if err != nil {
		t.Fatalf("ListReviewRequestedPullRequests(ttl hit) error = %v", err)
	}
	if graphQLCalls != 1 {
		t.Fatalf("graphql calls after ttl hit = %d, want 1", graphQLCalls)
	}
	if second[0].Number != 7 || second[0].Labels[0] != "ready" {
		t.Fatalf("ttl cached PR = %#v, want PR 7 from gateway cache", second[0])
	}

	now = now.Add(31 * time.Second)
	thirdCtx := ContextWithDiscoverySnapshot(context.Background(), NewDiscoverySnapshot(gateway, NewDiscoveryTickState(), DiscoverySnapshotOptions{PullRequestLimit: 100}))
	third, err := gateway.ListReviewRequestedPullRequests(thirdCtx, ListReviewRequestedPullRequestsInput{Repo: "acme/looper", CWD: "/repo", Reviewer: "reviewer", Limit: 10})
	if err != nil {
		t.Fatalf("ListReviewRequestedPullRequests(ttl expired) error = %v", err)
	}
	if graphQLCalls != 2 {
		t.Fatalf("graphql calls after ttl expiry = %d, want 2", graphQLCalls)
	}
	if third[0].Number != 8 {
		t.Fatalf("ttl expired PR number = %d, want 8", third[0].Number)
	}
}

func TestDiscoverySnapshotDiscoveryCacheTTLZeroDisablesGatewayCache(t *testing.T) {
	t.Parallel()

	prListCalls := 0
	gateway := New(Options{
		DiscoveryCacheTTL: 0,
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			cmd := strings.Join(options.Args, " ")
			if strings.Contains(cmd, "pr list") {
				prListCalls++
				return shell.Result{Stdout: `[
					{"number":1,"title":"PR 1","state":"OPEN","labels":[],"baseRefName":"main","headRefOid":"sha-1","author":{"login":"octo"}}
				]`}, nil
			}
			return shell.Result{}, errors.New("unexpected command: " + cmd)
		},
	})

	for i := 0; i < 2; i++ {
		ctx := ContextWithDiscoverySnapshot(context.Background(), NewDiscoverySnapshot(gateway, NewDiscoveryTickState(), DiscoverySnapshotOptions{PullRequestLimit: 100}))
		if _, err := gateway.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: "acme/looper", CWD: "/repo", Limit: 10}); err != nil {
			t.Fatalf("ListOpenPullRequests(%d) error = %v", i, err)
		}
	}
	if prListCalls != 2 {
		t.Fatalf("pr list calls with ttl disabled = %d, want 2", prListCalls)
	}
}

func reviewRequestedSearchResponse(number int, sha string) string {
	return fmt.Sprintf(`{"data":{"search":{"nodes":[{"number":%d,"title":"Review me","url":"https://example.test/pull/%d","state":"OPEN","updatedAt":"2026-06-05T10:00:00Z","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","labels":{"nodes":[{"name":"ready"}]},"headRefName":"feature","baseRefName":"main","headRefOid":%q,"baseRefOid":"base","mergeStateStatus":"CLEAN","author":{"login":"contributor"},"reviews":{"nodes":[]}}]}}}`, number, number, sha)
}
