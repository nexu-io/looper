package github

import (
	"context"
	"errors"
	"strings"
	"testing"

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
