package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/diffanchor"
	"github.com/nexu-io/looper/internal/infra/shell"
)

func TestGatewayListsSnapshotsAndReviewsThroughGH(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case args == "api repos/acme/looper/pulls/42/reviews --method POST --input - --include":
			runner.stdin = options.Stdin
			return shell.Result{Stdout: "{}"}, nil
		case strings.HasPrefix(args, "pr list"):
			return shell.Result{Stdout: `[{"number":42,"title":"Review me","url":"https://example.test/pull/42","state":"OPEN","updatedAt":"2026-05-01T12:00:00Z","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","baseRefOid":"def456","mergeStateStatus":"DIRTY","author":{"login":"octocat"},"reviewRequests":[{"__typename":"User","login":"OctoCat"},{"__typename":"Team","slug":"platform"}]}]`}, nil
		case strings.HasPrefix(args, "issue list"):
			return shell.Result{Stdout: `[{"number":8,"title":"Fix gateway","body":"Issue body","url":"https://example.test/issues/8","state":"OPEN","updatedAt":"2026-05-02T12:00:00Z","author":{"login":"octocat"},"assignees":[{"login":"reviewer"}],"labels":[{"name":"phase-1"},{"name":"gateway"}]}]`}, nil
		case args == "api repos/acme/looper/issues/8":
			return shell.Result{Stdout: `{"number":8,"title":"Fix gateway","body":"Issue body","html_url":"https://example.test/issues/8","state":"open","state_reason":"completed","updated_at":"2026-05-03T12:00:00Z","user":{"login":"octocat"},"author_association":"COLLABORATOR","assignees":[{"login":"reviewer"}],"labels":[{"name":"phase-1"},{"name":"gateway"}]}`}, nil
		case args == "api --paginate --slurp repos/acme/looper/issues/8/dependencies/blocked_by -H Accept: application/vnd.github+json":
			return shell.Result{Stdout: `[[{"id":12,"number":12,"title":"blocked by","url":"https://api.example.test/issues/12","html_url":"https://example.test/issues/12","repository_url":"https://api.example.test/repos/acme/looper","state":"open","state_reason":"","repository":{"name":"looper","full_name":"acme/looper","url":"https://api.example.test/repos/acme/looper","html_url":"https://example.test/acme/looper"}}]]`}, nil
		case args == "api --paginate --slurp repos/acme/looper/issues/8/dependencies/blocking -H Accept: application/vnd.github+json":
			return shell.Result{Stdout: `[[{"id":13,"number":13,"title":"blocking","url":"https://api.example.test/issues/13","html_url":"https://example.test/issues/13","repository_url":"https://api.example.test/repos/acme/looper","state":"closed","state_reason":"completed","repository":{"name":"looper","full_name":"acme/looper","url":"https://api.example.test/repos/acme/looper","html_url":"https://example.test/acme/looper"}}]]`}, nil
		case args == "api --paginate --slurp repos/acme/looper/issues/8/sub_issues -H Accept: application/vnd.github+json":
			return shell.Result{Stdout: `[[{"id":14,"number":14,"title":"sub issue","url":"https://api.example.test/issues/14","html_url":"https://example.test/issues/14","repository_url":"https://api.example.test/repos/acme/looper","state":"open","state_reason":"","repository":{"name":"looper","full_name":"acme/looper","url":"https://api.example.test/repos/acme/looper","html_url":"https://example.test/acme/looper"}}]]`}, nil
		case args == "api --paginate --slurp repos/acme/looper/issues/8/comments":
			return shell.Result{Stdout: `[[{"id":91,"body":"First human follow-up","html_url":"https://example.test/issues/8#issuecomment-91","created_at":"2026-05-03T13:00:00Z","updated_at":"2026-05-03T13:00:00Z","user":{"login":"reviewer"},"author_association":"MEMBER"}]]`}, nil
		case args == "api repos/acme/looper/issues/8/comments --method POST -f body=Looper started":
			return shell.Result{Stdout: `{"id":91,"html_url":"https://example.test/issues/8#issuecomment-91"}`}, nil
		case args == "api repos/acme/looper/issues/comments/91 --method PATCH -f body=Looper finished":
			return shell.Result{Stdout: "{}"}, nil
		case args == "api repos/acme/looper/issues/8/assignees --method POST -f assignees[]=reviewer":
			return shell.Result{Stdout: "{}"}, nil
		case strings.HasPrefix(args, "pr view"):
			if !strings.Contains(args, "createdAt") || !strings.Contains(args, "closedAt") {
				t.Fatalf("pr view args = %q, want createdAt and closedAt fields requested", args)
			}
			return shell.Result{Stdout: `{"number":42,"title":"Review me","body":"Body","url":"https://example.test/pull/42","state":"OPEN","createdAt":"2026-05-03T12:00:00Z","updatedAt":"2026-05-04T12:00:00Z","closedAt":"2026-05-05T12:00:00Z","isDraft":false,"reviewDecision":"CHANGES_REQUESTED","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","baseRefOid":"def456","mergeStateStatus":"DIRTY","author":{"login":"octocat"},"reviewRequests":[{"requestedReviewer":{"__typename":"User","login":"reviewer"}},{"requestedReviewer":{"__typename":"Team","slug":"platform"}}],"comments":[{"id":"issue-comment-1","body":"conversation notice"}],"reviews":[{"state":"COMMENTED"}],"statusCheckRollup":[{"conclusion":"SUCCESS"}]}`}, nil
		case strings.HasPrefix(args, "pr diff"):
			return shell.Result{Stdout: "diff --git a/a.ts b/a.ts\n"}, nil
		case strings.HasPrefix(args, "api user"):
			return shell.Result{Stdout: "reviewer\n"}, nil
		case strings.Contains(args, "resolveReviewThread"):
			return shell.Result{Stdout: `{"data":{"resolveReviewThread":{"thread":{"id":"thread-1","isResolved":true}}}}`}, nil
		case strings.Contains(args, "reviewThreads"):
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"thread-1","isResolved":false,"comments":{"nodes":[{"id":"comment-1","body":"Fix this"}]}}]}}}}}`}, nil
		case strings.Contains(args, "threadId=thread-1"):
			return shell.Result{Stdout: `{"data":{"node":{"id":"thread-1","isResolved":false}}}`}, nil
		case strings.HasPrefix(args, "label create"):
			return shell.Result{Stdout: "{}"}, nil
		case args == "api repos/acme/looper/issues/42/labels --method POST -f labels[]=phase-1 -f labels[]=ready":
			return shell.Result{Stdout: "{}"}, nil
		case args == "api repos/acme/looper/issues/42/labels/needs-work --method DELETE":
			return shell.Result{Stdout: "{}"}, nil
		case args == "api repos/acme/looper/pulls/42/requested_reviewers --method POST -f reviewers[]=reviewer":
			return shell.Result{Stdout: "{}"}, nil
		case args == "pr review 42 --repo acme/looper --comment --body app.go: Looks good":
			return shell.Result{Stdout: "{}"}, nil
		case args == "pr comment 42 --repo acme/looper --body High-level follow-up":
			return shell.Result{Stdout: "{}"}, nil
		case args == "api repos/acme/looper/issues/42/reactions --method POST -H Accept: application/vnd.github+json -f content=eyes":
			return shell.Result{Stdout: "{}"}, nil
		case args == "api --paginate --slurp repos/acme/looper/issues/42/reactions -H Accept: application/vnd.github+json":
			return shell.Result{Stdout: `[{"id":7,"content":"eyes","user":{"login":"reviewer"}}]`}, nil
		case args == "api repos/acme/looper/issues/42/reactions/7 --method DELETE -H Accept: application/vnd.github+json":
			return shell.Result{Stdout: "{}"}, nil
		case args == "pr create --repo acme/looper --head feature --base main --title Add support --body Body":
			return shell.Result{Stdout: "https://example.test/pull/88\n"}, nil
		case args == "pr edit 42 --repo acme/looper --title Implement support":
			return shell.Result{Stdout: ""}, nil
		case args == "pr edit 42 --repo acme/looper --body Updated body":
			return shell.Result{Stdout: ""}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	prs, err := gateway.ListOpenPullRequests(context.Background(), ListOpenPullRequestsInput{Repo: "acme/looper", Label: "phase-1"})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	issues, err := gateway.ListOpenIssues(context.Background(), ListOpenIssuesInput{Repo: "acme/looper", Assignee: "reviewer", Label: "phase-1"})
	if err != nil {
		t.Fatalf("ListOpenIssues() error = %v", err)
	}
	issueDetail, err := gateway.ViewIssue(context.Background(), ViewIssueInput{Repo: "acme/looper", IssueNumber: 8})
	if err != nil {
		t.Fatalf("ViewIssue() error = %v", err)
	}
	blockedBy, err := gateway.ListBlockedByIssues(context.Background(), ViewIssueInput{Repo: "acme/looper", IssueNumber: 8})
	if err != nil {
		t.Fatalf("ListBlockedByIssues() error = %v", err)
	}
	blocking, err := gateway.ListBlockingIssues(context.Background(), ViewIssueInput{Repo: "acme/looper", IssueNumber: 8})
	if err != nil {
		t.Fatalf("ListBlockingIssues() error = %v", err)
	}
	subIssues, err := gateway.ListSubIssues(context.Background(), ViewIssueInput{Repo: "acme/looper", IssueNumber: 8})
	if err != nil {
		t.Fatalf("ListSubIssues() error = %v", err)
	}
	comment, err := gateway.CreateIssueComment(context.Background(), IssueCommentInput{Repo: "acme/looper", IssueNumber: 8, Body: "Looper started"})
	if err != nil {
		t.Fatalf("CreateIssueComment() error = %v", err)
	}
	if err := gateway.UpdateIssueComment(context.Background(), UpdateIssueCommentInput{Repo: "acme/looper", CommentID: 91, Body: "Looper finished"}); err != nil {
		t.Fatalf("UpdateIssueComment() error = %v", err)
	}
	if err := gateway.AddIssueAssignees(context.Background(), IssueAssigneesInput{Repo: "acme/looper", IssueNumber: 8, Assignees: []string{"reviewer"}}); err != nil {
		t.Fatalf("AddIssueAssignees() error = %v", err)
	}
	snapshot, err := gateway.CapturePullRequestSnapshot(context.Background(), CapturePullRequestSnapshotInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("CapturePullRequestSnapshot() error = %v", err)
	}
	if err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "app.go: Looks good"}); err != nil {
		t.Fatalf("SubmitReview(comment only) error = %v", err)
	}
	if err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "Needs work", CommitID: "abc123", Comments: []ReviewComment{{Body: "Please handle the null case.", Path: "src/a.ts", Line: 12, Side: "RIGHT"}}}); err != nil {
		t.Fatalf("SubmitReview(inline) error = %v", err)
	}
	if err := gateway.AddPullRequestComment(context.Background(), PullRequestCommentInput{Repo: "acme/looper", PRNumber: 42, Body: "High-level follow-up"}); err != nil {
		t.Fatalf("AddPullRequestComment() error = %v", err)
	}
	if err := gateway.AddPullRequestReaction(context.Background(), PullRequestReactionInput{Repo: "acme/looper", PRNumber: 42, Content: "eyes"}); err != nil {
		t.Fatalf("AddPullRequestReaction() error = %v", err)
	}
	if err := gateway.RemovePullRequestReaction(context.Background(), PullRequestReactionInput{Repo: "acme/looper", PRNumber: 42, Content: "eyes"}); err != nil {
		t.Fatalf("RemovePullRequestReaction() error = %v", err)
	}
	if err := gateway.ResolveReviewThread(context.Background(), ResolveReviewThreadInput{Repo: "acme/looper", ThreadID: "thread-1"}); err != nil {
		t.Fatalf("ResolveReviewThread() error = %v", err)
	}
	if err := gateway.AddPullRequestLabels(context.Background(), PullRequestLabelsInput{Repo: "acme/looper", PRNumber: 42, Labels: []string{"phase-1", "ready"}}); err != nil {
		t.Fatalf("AddPullRequestLabels() error = %v", err)
	}
	if err := gateway.RemovePullRequestLabels(context.Background(), PullRequestLabelsInput{Repo: "acme/looper", PRNumber: 42, Labels: []string{"needs-work"}}); err != nil {
		t.Fatalf("RemovePullRequestLabels() error = %v", err)
	}
	if err := gateway.AddPullRequestReviewers(context.Background(), PullRequestReviewersInput{Repo: "acme/looper", PRNumber: 42, Reviewers: []string{"reviewer"}}); err != nil {
		t.Fatalf("AddPullRequestReviewers() error = %v", err)
	}
	created, err := gateway.CreatePullRequest(context.Background(), CreatePullRequestInput{Repo: "acme/looper", HeadBranch: "feature", BaseBranch: "main", Title: "Add support", Body: "Body"})
	if err != nil {
		t.Fatalf("CreatePullRequest() error = %v", err)
	}
	if err := gateway.UpdatePullRequestTitle(context.Background(), UpdatePullRequestTitleInput{Repo: "acme/looper", PRNumber: 42, Title: "Implement support"}); err != nil {
		t.Fatalf("UpdatePullRequestTitle() error = %v", err)
	}
	if err := gateway.UpdatePullRequestBody(context.Background(), UpdatePullRequestBodyInput{Repo: "acme/looper", PRNumber: 42, Body: "Updated body"}); err != nil {
		t.Fatalf("UpdatePullRequestBody() error = %v", err)
	}
	detail, err := gateway.ViewPullRequest(context.Background(), ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("ViewPullRequest() error = %v", err)
	}
	login, err := gateway.GetCurrentUserLogin(context.Background(), "")
	if err != nil {
		t.Fatalf("GetCurrentUserLogin() error = %v", err)
	}

	if got := prs[0].Number; got != 42 {
		t.Fatalf("prs[0].Number = %d, want 42", got)
	}
	if got := prs[0].ReviewRequests; len(got) != 1 || got[0] != "OctoCat" {
		t.Fatalf("prs[0].ReviewRequests = %#v, want [OctoCat]", got)
	}
	if prs[0].AuthorAssociation != "" {
		t.Fatalf("prs[0].AuthorAssociation = %q, want empty unsupported field", prs[0].AuthorAssociation)
	}
	if prs[0].UpdatedAt != "2026-05-01T12:00:00Z" {
		t.Fatalf("prs[0].UpdatedAt = %q, want parsed updated timestamp", prs[0].UpdatedAt)
	}
	if prs[0].BaseSHA != "def456" || !prs[0].HasConflicts {
		t.Fatalf("prs[0] = %#v, want base sha and conflict state", prs[0])
	}
	if got := issues[0].Assignees; len(got) != 1 || got[0] != "reviewer" {
		t.Fatalf("issues[0].Assignees = %#v, want [reviewer]", got)
	}
	if got := issues[0].Labels; len(got) != 2 || got[0] != "phase-1" || got[1] != "gateway" {
		t.Fatalf("issues[0].Labels = %#v, want [phase-1 gateway]", got)
	}
	if issues[0].AuthorAssociation != "" {
		t.Fatalf("issues[0].AuthorAssociation = %q, want empty unsupported field", issues[0].AuthorAssociation)
	}
	if issues[0].UpdatedAt != "2026-05-02T12:00:00Z" {
		t.Fatalf("issues[0].UpdatedAt = %q, want parsed updated timestamp", issues[0].UpdatedAt)
	}
	if issueDetail.Number != 8 {
		t.Fatalf("issueDetail.Number = %d, want 8", issueDetail.Number)
	}
	if issueDetail.State != "open" || issueDetail.IsPullRequest {
		t.Fatalf("issueDetail = %#v, want open issue not pull request", issueDetail)
	}
	if issueDetail.StateReason != "completed" {
		t.Fatalf("issueDetail.StateReason = %q, want completed", issueDetail.StateReason)
	}
	if issueDetail.AuthorAssociation != "COLLABORATOR" {
		t.Fatalf("issueDetail.AuthorAssociation = %q, want COLLABORATOR", issueDetail.AuthorAssociation)
	}
	if issueDetail.UpdatedAt != "2026-05-03T12:00:00Z" {
		t.Fatalf("issueDetail.UpdatedAt = %q, want parsed updated timestamp", issueDetail.UpdatedAt)
	}
	if issueDetail.CommentCount != 1 || len(issueDetail.Comments) != 1 || issueDetail.Comments[0].Body != "First human follow-up" {
		t.Fatalf("issueDetail comments = %#v / %d, want one parsed issue comment", issueDetail.Comments, issueDetail.CommentCount)
	}
	if comment.ID != 91 || comment.URL != "https://example.test/issues/8#issuecomment-91" {
		t.Fatalf("comment = %#v, want parsed issue comment metadata", comment)
	}
	if len(blockedBy) != 1 || blockedBy[0].Number != 12 || blockedBy[0].Repository.FullName != "acme/looper" {
		t.Fatalf("blockedBy = %#v, want parsed blocked-by issue", blockedBy)
	}
	if len(blocking) != 1 || blocking[0].Number != 13 || blocking[0].StateReason != "completed" {
		t.Fatalf("blocking = %#v, want parsed blocking issue", blocking)
	}
	if len(subIssues) != 1 || subIssues[0].Number != 14 {
		t.Fatalf("subIssues = %#v, want parsed sub-issue", subIssues)
	}
	if snapshot.HeadSHA != "abc123" {
		t.Fatalf("snapshot.HeadSHA = %q, want abc123", snapshot.HeadSHA)
	}
	if snapshot.ReviewState == nil || *snapshot.ReviewState != "CHANGES_REQUESTED" {
		t.Fatalf("snapshot.ReviewState = %v, want CHANGES_REQUESTED", snapshot.ReviewState)
	}
	if got := detail.ReviewRequests; len(got) != 1 || got[0] != "reviewer" {
		t.Fatalf("detail.ReviewRequests = %#v, want [reviewer]", got)
	}
	if detail.AuthorAssociation != "" {
		t.Fatalf("detail.AuthorAssociation = %q, want empty unsupported field", detail.AuthorAssociation)
	}
	if detail.UpdatedAt != "2026-05-04T12:00:00Z" {
		t.Fatalf("detail.UpdatedAt = %q, want parsed updated timestamp", detail.UpdatedAt)
	}
	if detail.CreatedAt != "2026-05-03T12:00:00Z" || detail.ClosedAt != "2026-05-05T12:00:00Z" {
		t.Fatalf("detail timestamps = (%q, %q), want parsed created/closed timestamps", detail.CreatedAt, detail.ClosedAt)
	}
	if !detail.HasConflicts {
		t.Fatal("detail.HasConflicts = false, want true")
	}
	if len(detail.Comments) != 1 || detail.Comments[0]["id"] != "comment-1" || detail.Comments[0]["threadId"] != "thread-1" || detail.Comments[0]["state"] != "UNRESOLVED" || detail.Comments[0]["body"] != "Fix this" {
		t.Fatalf("detail.Comments = %#v, want normalized review thread", detail.Comments)
	}
	if len(detail.IssueComments) != 1 || detail.IssueComments[0].Body != "conversation notice" {
		t.Fatalf("detail.IssueComments = %#v, want PR conversation comments", detail.IssueComments)
	}
	if login != "reviewer" {
		t.Fatalf("login = %q, want reviewer", login)
	}
	if created.URL != "https://example.test/pull/88" || created.Number != 88 {
		t.Fatalf("created = %#v, want parsed PR URL/number", created)
	}

	log := strings.Join(runner.calls, "\n")
	for _, needle := range []string{
		"pr review 42 --repo acme/looper --comment --body app.go: Looks good",
		"api repos/acme/looper/pulls/42/reviews --method POST --input - --include",
		"pr comment 42 --repo acme/looper --body High-level follow-up",
		"api repos/acme/looper/issues/42/reactions --method POST -H Accept: application/vnd.github+json -f content=eyes",
		"api --paginate --slurp repos/acme/looper/issues/42/reactions -H Accept: application/vnd.github+json",
		"api repos/acme/looper/issues/42/reactions/7 --method DELETE -H Accept: application/vnd.github+json",
		"pr list --repo acme/looper --state open --limit 30 --label phase-1",
		"issue list --repo acme/looper --state open --limit 30 --assignee reviewer --label phase-1",
		"api repos/acme/looper/issues/8",
		"api --paginate --slurp repos/acme/looper/issues/8/dependencies/blocked_by -H Accept: application/vnd.github+json",
		"api --paginate --slurp repos/acme/looper/issues/8/dependencies/blocking -H Accept: application/vnd.github+json",
		"api --paginate --slurp repos/acme/looper/issues/8/sub_issues -H Accept: application/vnd.github+json",
		"api --paginate --slurp repos/acme/looper/issues/8/comments",
		"api repos/acme/looper/issues/8/comments --method POST -f body=Looper started",
		"api repos/acme/looper/issues/comments/91 --method PATCH -f body=Looper finished",
		"api repos/acme/looper/issues/8/assignees --method POST -f assignees[]=reviewer",
		"label create phase-1 --repo acme/looper --color 5319e7 --description Managed by looper --force",
		"label create ready --repo acme/looper --color 5319e7 --description Managed by looper --force",
		"api repos/acme/looper/issues/42/labels --method POST -f labels[]=phase-1 -f labels[]=ready",
		"api repos/acme/looper/issues/42/labels/needs-work --method DELETE",
		"api repos/acme/looper/pulls/42/requested_reviewers --method POST -f reviewers[]=reviewer",
		"threadId=thread-1",
	} {
		if !strings.Contains(log, needle) {
			t.Fatalf("gh log missing %q\n%s", needle, log)
		}
	}
	for _, needle := range []string{"\"event\":\"COMMENT\"", "\"body\":\"Needs work\"", "\"commit_id\":\"abc123\"", "\"path\":\"src/a.ts\"", "\"line\":12", "\"side\":\"RIGHT\""} {
		if !strings.Contains(runner.stdin, needle) {
			t.Fatalf("review stdin missing %q\n%s", needle, runner.stdin)
		}
	}
}

func TestGatewayListOpenPullRequestsFallsBackWhenReviewRequestReviewerIsInaccessible(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if strings.HasPrefix(args, "pr list") && strings.Contains(args, "reviewRequests") {
			result := shell.Result{ExitCode: 1, Stderr: "GraphQL: Resource not accessible by personal access token (repository.pullRequests.nodes.3.reviewRequests.nodes.0.requestedReviewer)"}
			return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
		}
		if strings.HasPrefix(args, "pr list") {
			if strings.Contains(args, "reviewRequests") {
				t.Fatalf("fallback gh args = %q, want reviewRequests omitted", args)
			}
			return shell.Result{Stdout: `[{"number":42,"title":"Review me","url":"https://example.test/pull/42","state":"OPEN","updatedAt":"2026-05-01T12:00:00Z","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","labels":[{"name":"ready"}],"headRefName":"feature","baseRefName":"main","headRefOid":"abc123","baseRefOid":"def456","mergeStateStatus":"CLEAN","author":{"login":"octocat"},"reviews":[{"state":"COMMENTED"}]}]`}, nil
		}
		t.Fatalf("unexpected gh args: %q", args)
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	prs, err := gateway.ListOpenPullRequests(context.Background(), ListOpenPullRequestsInput{Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 42 || prs[0].Title != "Review me" {
		t.Fatalf("ListOpenPullRequests() = %#v, want fallback PR metadata", prs)
	}
	if prs[0].ReviewRequests != nil || prs[0].ReviewRequestUsers != nil {
		t.Fatalf("fallback review requests = %#v/%#v, want unknown metadata", prs[0].ReviewRequests, prs[0].ReviewRequestUsers)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("gh calls = %#v, want primary and fallback list calls", runner.calls)
	}
}

func TestGatewayViewPullRequestFallsBackWhenReviewRequestReviewerIsInaccessible(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if strings.HasPrefix(args, "pr view") && strings.Contains(args, "reviewRequests") {
			result := shell.Result{ExitCode: 1, Stderr: "GraphQL: Resource not accessible by personal access token (repository.pullRequest.reviewRequests.nodes.0.requestedReviewer)"}
			return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
		}
		if strings.HasPrefix(args, "pr view") {
			if strings.Contains(args, "reviewRequests") {
				t.Fatalf("fallback gh args = %q, want reviewRequests omitted", args)
			}
			return shell.Result{Stdout: `{"number":42,"title":"Review me","body":"Body","url":"https://example.test/pull/42","state":"OPEN","createdAt":"2026-05-03T12:00:00Z","updatedAt":"2026-05-04T12:00:00Z","closedAt":"","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","labels":[{"name":"ready"}],"headRefName":"feature","baseRefName":"main","headRefOid":"abc123","baseRefOid":"def456","mergeStateStatus":"CLEAN","author":{"login":"octocat"},"comments":[{"id":"issue-comment-1","body":"conversation notice"}],"reviews":[{"state":"COMMENTED"}],"statusCheckRollup":[{"conclusion":"SUCCESS"}]}`}, nil
		}
		if strings.Contains(args, "reviewThreads") {
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`}, nil
		}
		t.Fatalf("unexpected gh args: %q", args)
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	detail, err := gateway.ViewPullRequest(context.Background(), ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("ViewPullRequest() error = %v", err)
	}
	if detail.Number != 42 || detail.Title != "Review me" || detail.Author != "octocat" {
		t.Fatalf("ViewPullRequest() = %#v, want fallback PR metadata", detail)
	}
	if detail.ReviewRequests != nil || detail.ReviewRequestUsers != nil {
		t.Fatalf("fallback review requests = %#v/%#v, want unknown metadata", detail.ReviewRequests, detail.ReviewRequestUsers)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("gh calls = %#v, want primary view, fallback view, and review thread fetch", runner.calls)
	}
}

func TestGatewayListOpenPullRequestsDoesNotFallbackForUnrelatedAccessError(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if strings.HasPrefix(args, "pr list") {
			result := shell.Result{ExitCode: 1, Stderr: "GraphQL: Resource not accessible by personal access token (repository.pullRequests.nodes.0.author)"}
			return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
		}
		t.Fatalf("unexpected gh args: %q", args)
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if _, err := gateway.ListOpenPullRequests(context.Background(), ListOpenPullRequestsInput{Repo: "acme/looper"}); err == nil {
		t.Fatal("ListOpenPullRequests() error = nil, want unrelated access error")
	}
	if len(runner.calls) != 1 {
		t.Fatalf("gh calls = %#v, want no fallback for unrelated access error", runner.calls)
	}
}

func TestGetPullRequestHeadSHA(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "pr view 42 --repo acme/looper --json headRefOid" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		return shell.Result{Stdout: `{"headRefOid":"abc123"}`}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	headSHA, err := gateway.GetPullRequestHeadSHA(context.Background(), ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("GetPullRequestHeadSHA() error = %v", err)
	}
	if headSHA != "abc123" {
		t.Fatalf("GetPullRequestHeadSHA() = %q, want abc123", headSHA)
	}
}

func TestGetRepositorySettings(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		return shell.Result{Stdout: `{"allow_squash_merge":true,"allow_merge_commit":false,"allow_rebase_merge":true,"allow_auto_merge":true}`}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	settings, err := gateway.GetRepositorySettings(context.Background(), RepositorySettingsInput{Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("GetRepositorySettings() error = %v", err)
	}
	if !settings.AllowSquashMerge || settings.AllowMergeCommit || !settings.AllowRebaseMerge || !settings.AllowAutoMerge {
		t.Fatalf("GetRepositorySettings() = %#v, want decoded repo settings", settings)
	}
}

func TestGetBranchProtection(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/branches/main/protection" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		return shell.Result{Stdout: `{"required_status_checks":{"checks":[{"context":"ci"}]}}`}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	protection, err := gateway.GetBranchProtection(context.Background(), BranchProtectionInput{Repo: "acme/looper", Branch: "main"})
	if err != nil {
		t.Fatalf("GetBranchProtection() error = %v", err)
	}
	if !protection.Enabled || !protection.HasRequiredChecks {
		t.Fatalf("GetBranchProtection() = %#v, want enabled protection with required checks", protection)
	}
}

func TestGetBranchProtectionReturnsEmptyOnMissingProtection(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		return shell.Result{Stderr: "HTTP 404: Not Found"}, &shell.CommandExecutionError{Result: shell.Result{Stderr: "HTTP 404: Not Found"}}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	protection, err := gateway.GetBranchProtection(context.Background(), BranchProtectionInput{Repo: "acme/looper", Branch: "main"})
	if err != nil {
		t.Fatalf("GetBranchProtection() error = %v", err)
	}
	if protection.Enabled || protection.HasRequiredChecks {
		t.Fatalf("GetBranchProtection() = %#v, want no protection", protection)
	}
}

func TestGetBranchProtectionReturnsEmptyOnMissingProtectionFromStdout(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		result := shell.Result{Stdout: `{"message":"Branch not protected","documentation_url":"https://docs.github.com/rest/branches/branch-protection#get-branch-protection","status":"404"}`}
		return result, &shell.CommandExecutionError{Message: "Command exited with code 1: stdout: HTTP 404: Not Found", Result: result}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	protection, err := gateway.GetBranchProtection(context.Background(), BranchProtectionInput{Repo: "acme/looper", Branch: "main"})
	if err != nil {
		t.Fatalf("GetBranchProtection() error = %v", err)
	}
	if protection.Enabled || protection.HasRequiredChecks {
		t.Fatalf("GetBranchProtection() = %#v, want no protection", protection)
	}
}

func TestListPullRequestCheckRunsIncludesStatusContexts(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	call := 0
	runner.respond = func(options shell.Options) (shell.Result, error) {
		call++
		args := strings.Join(options.Args, " ")
		switch call {
		case 1:
			if args != "api repos/acme/looper/commits/abc123/check-runs -H Accept: application/vnd.github+json" {
				t.Fatalf("unexpected gh args: %q", args)
			}
			return shell.Result{Stdout: `{"total_count":1,"check_runs":[{"name":"unit","status":"completed","conclusion":"success"}]}`}, nil
		case 2:
			if args != "api repos/acme/looper/commits/abc123/status -H Accept: application/vnd.github+json" {
				t.Fatalf("unexpected gh args: %q", args)
			}
			return shell.Result{Stdout: `{"statuses":[{"context":"legacy-ci","state":"success"},{"context":"legacy-ci","state":"failure"},{"context":"lint","state":"pending"}]}`}, nil
		default:
			t.Fatalf("unexpected extra gh call: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	runs, err := gateway.ListPullRequestCheckRuns(context.Background(), PullRequestCheckRunsInput{Repo: "acme/looper", Ref: "abc123"})
	if err != nil {
		t.Fatalf("ListPullRequestCheckRuns() error = %v", err)
	}
	if len(runs.CheckRuns) != 1 || runs.CheckRuns[0].Name != "unit" {
		t.Fatalf("CheckRuns = %#v, want decoded check run", runs.CheckRuns)
	}
	if len(runs.Statuses) != 2 || runs.Statuses[0].Context != "legacy-ci" || runs.Statuses[1].Context != "lint" {
		t.Fatalf("Statuses = %#v, want deduped status contexts in API order", runs.Statuses)
	}
}

func TestGetPullRequestHeadAndAuthorUsesNarrowPRView(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "pr view 42 --repo acme/looper --json headRefOid,author" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		return shell.Result{Stdout: `{"headRefOid":"abc123","author":{"login":"octocat"}}`}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	metadata, err := gateway.GetPullRequestHeadAndAuthor(context.Background(), ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("GetPullRequestHeadAndAuthor() error = %v", err)
	}
	if metadata.HeadSHA != "abc123" || metadata.Author != "octocat" {
		t.Fatalf("GetPullRequestHeadAndAuthor() = %+v, want head abc123 author octocat", metadata)
	}
}

func TestExtractDependencyIssueFallsBackToRepositoryURL(t *testing.T) {
	t.Parallel()
	issue := extractDependencyIssue(map[string]any{
		"id":             int64(12),
		"number":         int64(34),
		"title":          "blocked by",
		"repository_url": "https://api.github.com/repos/other/repo",
	}, "acme/looper")
	if issue.Repository.Name != "repo" || issue.Repository.FullName != "other/repo" || issue.Repository.URL != "https://api.github.com/repos/other/repo" {
		t.Fatalf("issue.Repository = %#v, want repository identity parsed from repository_url", issue.Repository)
	}
}

func TestExtractDependencyIssueFallsBackToGHESRepositoryURL(t *testing.T) {
	t.Parallel()
	issue := extractDependencyIssue(map[string]any{
		"id":             int64(12),
		"number":         int64(34),
		"title":          "blocked by",
		"repository_url": "https://github.example.com/api/v3/repos/other/repo",
	}, "github.example.com/acme/looper")
	if issue.Repository.Name != "repo" || issue.Repository.FullName != "github.example.com/other/repo" || issue.Repository.URL != "https://github.example.com/api/v3/repos/other/repo" {
		t.Fatalf("issue.Repository = %#v, want repository identity parsed from GHES repository_url", issue.Repository)
	}
}

func TestExtractDependencyIssueFallsBackToRequestedRepo(t *testing.T) {
	t.Parallel()
	issue := extractDependencyIssue(map[string]any{
		"id":     int64(12),
		"number": int64(34),
		"title":  "blocked by",
	}, "github.example.com/acme/looper")
	if issue.Repository.Name != "looper" || issue.Repository.FullName != "github.example.com/acme/looper" {
		t.Fatalf("issue.Repository = %#v, want repository identity parsed from requested repo", issue.Repository)
	}
}

func TestIsTransientErrorTreatsShellCommandNetworkFailuresAsRetryable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "gateway-wrapped tls handshake timeout", err: &TransientError{Err: &shell.CommandExecutionError{Message: "Command exited with code 1", Result: shell.Result{Stderr: "net/http: TLS handshake timeout"}}}},
		{name: "unexpected eof", err: &shell.CommandExecutionError{Message: "Command exited with code 1", Result: shell.Result{Stderr: "Post https://api.github.com/graphql: unexpected EOF"}}},
		{name: "graphql transient", err: &shell.CommandExecutionError{Message: "Command exited with code 1", Result: shell.Result{Stdout: `{"errors":[{"message":"GraphQL: Something went wrong while executing your query."}]}`}}},
		{name: "bare http 504", err: fmt.Errorf("HTTP 504")},
		{name: "generic rate limit", err: fmt.Errorf("rate limit exceeded")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !IsTransientError(tc.err) {
				t.Fatalf("IsTransientError(%v) = false, want true", tc.err)
			}
		})
	}
}

func TestIsTransientErrorIgnoresNonGitHubShellFailures(t *testing.T) {
	t.Parallel()

	err := &shell.CommandExecutionError{Message: "Command exited with code 1", Result: shell.Result{Stderr: "unexpected EOF while reading local file"}}
	if IsTransientError(err) {
		t.Fatal("IsTransientError(non-GitHub shell EOF) = true, want false")
	}
}

func TestIsPullRequestNotFoundErrorDetectsGraphQLResolveFailure(t *testing.T) {
	t.Parallel()

	err := &shell.CommandExecutionError{Message: "Command exited with code 1", Result: shell.Result{Stderr: "GraphQL: Could not resolve to a PullRequest with the number of 345. (repository.pullRequest)"}}
	if !IsPullRequestNotFoundError(err) {
		t.Fatal("IsPullRequestNotFoundError(GraphQL resolve failure) = false, want true")
	}
}

func TestSubmitReviewRejectsQualityFlagsBeforePublishing(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		t.Fatalf("unexpected gh call: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "This needs work."})
	if err == nil || !strings.Contains(err.Error(), "review quality gate failed") || !strings.Contains(err.Error(), "top-level-location-missing") {
		t.Fatalf("SubmitReview() error = %v, want quality gate failure", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("SubmitReview() made gh calls after quality failure: %#v", runner.calls)
	}
}

func TestSubmitReviewAllowsCleanOutcomeWithoutLocation(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if args := strings.Join(options.Args, " "); args != "pr review 42 --repo acme/looper --comment --body LGTM\n<!-- looper:review id=reviewer:1 head=abc outcome=clean -->" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		return shell.Result{Stdout: "{}"}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	body := "LGTM\n<!-- looper:review id=reviewer:1 head=abc outcome=clean -->"
	if err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: body}); err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
}

func TestSubmitReviewUsesAPIForTopLevelReviewWithCommitID(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input - --include" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		runner.stdin = options.Stdin
		return shell.Result{Stdout: "{}"}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "app.go: Looks good", CommitID: "abc123"}); err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if !strings.Contains(runner.stdin, `"commit_id":"abc123"`) || strings.Contains(runner.stdin, `"comments"`) {
		t.Fatalf("review stdin = %s, want commit_id without comments", runner.stdin)
	}
}

func TestSubmitReviewNormalizesAnchorsBeforePublishing(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input - --include" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		runner.stdin = options.Stdin
		return shell.Result{Stdout: "{}"}, nil
	}
	anchors := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1,2 +1,2 @@\n-old\n+new\n keep\n")
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "Needs work", CommitID: "abc123", Comments: []ReviewComment{{Body: "Valid", Path: "app.go", Line: 1, Side: " right "}, {Body: "Invalid", Path: "missing.go", Line: 99, Side: "RIGHT"}}, Anchors: &anchors}); err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if !strings.Contains(runner.stdin, `"path":"app.go"`) {
		t.Fatalf("review payload did not publish valid path:\n%s", runner.stdin)
	}
	if strings.Contains(runner.stdin, `"path":"missing.go"`) || !strings.Contains(runner.stdin, "Invalid") || !strings.Contains(runner.stdin, "Inline comment could not be anchored") {
		t.Fatalf("review payload did not downgrade invalid anchor into body:\n%s", runner.stdin)
	}
}

func TestSubmitReviewStripsDisclosureFromInlineCommentsWhenReviewDisclosureDisabled(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input - --include" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		runner.stdin = options.Stdin
		return shell.Result{Stdout: "{}"}, nil
	}
	anchors := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1 +1 @@\n+new\n")
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "Needs work", CommitID: "abc123", Comments: []ReviewComment{{Body: "<!-- looper:stamp v=1 -->\n<sub>Generated by Looper 0.0.0-dev · runner=reviewer · agent=opencode</sub>\nPlease fix `value`.", Path: "app.go", Line: 1, Side: "RIGHT"}}, Anchors: &anchors})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if strings.Contains(runner.stdin, "looper:stamp") || strings.Contains(runner.stdin, "Generated by Looper") || !strings.Contains(runner.stdin, "Please fix `value`.") {
		t.Fatalf("review payload = %s, want inline disclosure stripped and markdown preserved", runner.stdin)
	}
}

func TestSubmitReviewDropsInlineCommentsEmptyAfterDisabledDisclosureStrip(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input - --include" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		runner.stdin = options.Stdin
		return shell.Result{Stdout: "{}"}, nil
	}
	anchors := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1 +1 @@\n+new\n")
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "Needs work", CommitID: "abc123", Comments: []ReviewComment{{Body: "<!-- looper:stamp v=1 -->", Path: "app.go", Line: 1, Side: "RIGHT"}, {Body: "Keep this", Path: "app.go", Line: 1, Side: "RIGHT"}}, Anchors: &anchors})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if strings.Contains(runner.stdin, "looper:stamp") || strings.Contains(runner.stdin, `"body":""`) || !strings.Contains(runner.stdin, "Keep this") {
		t.Fatalf("review payload = %s, want marker-only inline comment dropped", runner.stdin)
	}
}

func TestSubmitReviewPreservesInlineDisclosureWhenReviewDisclosureEnabled(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input - --include" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		runner.stdin = options.Stdin
		return shell.Result{Stdout: "{}"}, nil
	}
	anchors := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1 +1 @@\n+new\n")
	disclosureCfg := config.DefaultDisclosureConfig()
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "Needs work", CommitID: "abc123", Comments: []ReviewComment{{Body: "Please fix `value`.\n\n<!-- looper:stamp v=1 -->", Path: "app.go", Line: 1, Side: "RIGHT"}}, Anchors: &anchors, Disclosure: disclosureCfg})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if !strings.Contains(runner.stdin, "looper:stamp") || !strings.Contains(runner.stdin, "Please fix `value`.") {
		t.Fatalf("review payload = %s, want inline disclosure marker preserved", runner.stdin)
	}
}

func TestSubmitReviewNormalizesVisibleInlineDisclosureWhenInlineVisibleDisabled(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input - --include" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		runner.stdin = options.Stdin
		return shell.Result{Stdout: "{}"}, nil
	}
	anchors := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1 +1 @@\n+new\n")
	disclosureCfg := config.DefaultDisclosureConfig()
	disclosureCfg.Channels.InlineCommentVisible = false
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "Needs work", CommitID: "abc123", Comments: []ReviewComment{{Body: "Please fix `value`.\n\n<!-- looper:stamp v=1 -->\n<sub>Generated by Looper 0.0.0-dev · runner=reviewer · agent=opencode</sub>", Path: "app.go", Line: 1, Side: "RIGHT"}}, Anchors: &anchors, Disclosure: disclosureCfg})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if !strings.Contains(runner.stdin, "looper:stamp") || strings.Contains(runner.stdin, "Generated by Looper") || !strings.Contains(runner.stdin, "Please fix `value`.") {
		t.Fatalf("review payload = %s, want visible inline disclosure normalized to hidden marker", runner.stdin)
	}
}

func TestSubmitReviewNormalizesLinkedHTMLInlineDisclosureWhenInlineVisibleDisabled(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input - --include" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		runner.stdin = options.Stdin
		return shell.Result{Stdout: "{}"}, nil
	}
	anchors := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1 +1 @@\n+new\n")
	disclosureCfg := config.DefaultDisclosureConfig()
	disclosureCfg.Channels.InlineCommentVisible = false
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "Needs work", CommitID: "abc123", Comments: []ReviewComment{{Body: "Please fix `value`.\n\n<!-- looper:stamp v=1 -->\n<sub>🔁 Powered by <a href=\"https://github.com/nexu-io/looper\">Looper</a> · runner=reviewer · An autonomous AI dev team for your GitHub repos.</sub>", Path: "app.go", Line: 1, Side: "RIGHT"}}, Anchors: &anchors, Disclosure: disclosureCfg})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if !strings.Contains(runner.stdin, "looper:stamp") || strings.Contains(runner.stdin, "Powered by <a") || !strings.Contains(runner.stdin, "Please fix `value`.") {
		t.Fatalf("review payload = %s, want linked HTML inline disclosure normalized to hidden marker", runner.stdin)
	}
}

func TestSubmitReviewNormalizesLegacyVisibleInlineDisclosureWhenInlineVisibleDisabled(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input - --include" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		runner.stdin = options.Stdin
		return shell.Result{Stdout: "{}"}, nil
	}
	anchors := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1 +1 @@\n+new\n")
	disclosureCfg := config.DefaultDisclosureConfig()
	disclosureCfg.Channels.InlineCommentVisible = false
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "Needs work", CommitID: "abc123", Comments: []ReviewComment{{Body: "Please fix `value`.\n\n<!-- looper:stamp v=1 -->\n<sub>Generated by [Looper](https://github.com/nexu-io/looper) 0.0.0-dev · runner=reviewer · agent=opencode</sub>", Path: "app.go", Line: 1, Side: "RIGHT"}}, Anchors: &anchors, Disclosure: disclosureCfg})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if !strings.Contains(runner.stdin, "looper:stamp") || strings.Contains(runner.stdin, "Generated by Looper") || strings.Contains(runner.stdin, "Generated by [Looper](https://github.com/nexu-io/looper)") || !strings.Contains(runner.stdin, "Please fix `value`.") {
		t.Fatalf("review payload = %s, want legacy visible inline disclosure normalized to hidden marker", runner.stdin)
	}
}

func TestSubmitReviewNormalizesLegacyVisibleInlineDisclosureWithoutMarkerWhenInlineVisibleDisabled(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input - --include" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		runner.stdin = options.Stdin
		return shell.Result{Stdout: "{}"}, nil
	}
	anchors := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1 +1 @@\n+new\n")
	disclosureCfg := config.DefaultDisclosureConfig()
	disclosureCfg.Channels.InlineCommentVisible = false
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "Needs work", CommitID: "abc123", Comments: []ReviewComment{{Body: "Please fix `value`.\n\n<sub>Generated by [Looper](https://github.com/nexu-io/looper) 0.0.0-dev · runner=reviewer · agent=opencode</sub>", Path: "app.go", Line: 1, Side: "RIGHT"}}, Anchors: &anchors, Disclosure: disclosureCfg})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if !strings.Contains(runner.stdin, "looper:stamp") || strings.Contains(runner.stdin, "Generated by [Looper](https://github.com/nexu-io/looper)") || !strings.Contains(runner.stdin, "Please fix `value`.") {
		t.Fatalf("review payload = %s, want legacy footer-only inline disclosure normalized to hidden marker", runner.stdin)
	}
}

func TestSubmitReviewNormalizesLegacyVisibleInlineDisclosureOldRepoURLWhenInlineVisibleDisabled(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input - --include" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		runner.stdin = options.Stdin
		return shell.Result{Stdout: "{}"}, nil
	}
	anchors := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1 +1 @@\n+new\n")
	disclosureCfg := config.DefaultDisclosureConfig()
	disclosureCfg.Channels.InlineCommentVisible = false
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "Needs work", CommitID: "abc123", Comments: []ReviewComment{{Body: "Please fix `value`.\n\n<sub>Generated by [Looper](https://github.com/powerformer/looper) 0.0.0-dev · runner=reviewer · agent=opencode</sub>", Path: "app.go", Line: 1, Side: "RIGHT"}}, Anchors: &anchors, Disclosure: disclosureCfg})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if !strings.Contains(runner.stdin, "looper:stamp") || strings.Contains(runner.stdin, "Generated by [Looper](https://github.com/powerformer/looper)") || !strings.Contains(runner.stdin, "Please fix `value`.") {
		t.Fatalf("review payload = %s, want old-repo visible inline disclosure normalized to hidden marker", runner.stdin)
	}
}

func TestSubmitReviewNormalizesPoweredMarkdownInlineDisclosureWhenInlineVisibleDisabled(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input - --include" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		runner.stdin = options.Stdin
		return shell.Result{Stdout: "{}"}, nil
	}
	anchors := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1 +1 @@\n+new\n")
	disclosureCfg := config.DefaultDisclosureConfig()
	disclosureCfg.Channels.InlineCommentVisible = false
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "Needs work", CommitID: "abc123", Comments: []ReviewComment{{Body: "Please fix `value`.\n\n<sub>Powered by [Looper](https://github.com/nexu-io/looper) · runner=reviewer · An autonomous AI dev team for your GitHub repos.</sub>", Path: "app.go", Line: 1, Side: "RIGHT"}}, Anchors: &anchors, Disclosure: disclosureCfg})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if !strings.Contains(runner.stdin, "looper:stamp") || strings.Contains(runner.stdin, "Powered by [Looper](https://github.com/nexu-io/looper)") || !strings.Contains(runner.stdin, "Please fix `value`.") {
		t.Fatalf("review payload = %s, want powered markdown inline disclosure normalized to hidden marker", runner.stdin)
	}
}

func TestSubmitReviewNormalizesPoweredMarkdownLegacyInlineDisclosureWhenInlineVisibleDisabled(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input - --include" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		runner.stdin = options.Stdin
		return shell.Result{Stdout: "{}"}, nil
	}
	anchors := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1 +1 @@\n+new\n")
	disclosureCfg := config.DefaultDisclosureConfig()
	disclosureCfg.Channels.InlineCommentVisible = false
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "Needs work", CommitID: "abc123", Comments: []ReviewComment{{Body: "Please fix `value`.\n\n<sub>Powered by [Looper](https://github.com/powerformer/looper) · runner=reviewer · An autonomous AI dev team for your GitHub repos.</sub>", Path: "app.go", Line: 1, Side: "RIGHT"}}, Anchors: &anchors, Disclosure: disclosureCfg})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	if !strings.Contains(runner.stdin, "looper:stamp") || strings.Contains(runner.stdin, "Powered by [Looper](https://github.com/powerformer/looper)") || !strings.Contains(runner.stdin, "Please fix `value`.") {
		t.Fatalf("review payload = %s, want powered legacy markdown inline disclosure normalized to hidden marker", runner.stdin)
	}
}

func TestGatewayResolveReviewThreadReturnsNotFound(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if strings.Contains(args, "threadId=thread-missing") {
			return shell.Result{Stdout: `{"data":{"node":null}}`}, nil
		}
		return shell.Result{Stdout: "{}"}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.ResolveReviewThread(context.Background(), ResolveReviewThreadInput{Repo: "acme/looper", ThreadID: "thread-missing"})
	if _, ok := err.(*ReviewThreadNotFoundError); !ok {
		t.Fatalf("ResolveReviewThread() error = %v, want *ReviewThreadNotFoundError", err)
	}
}

func TestGatewayListReviewThreadsPaginatesThreadsAndComments(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case strings.Contains(args, "reviewThreads(first: $limit, after: $after)") && strings.Contains(args, "-F after=thread-cursor-1"):
			if !strings.Contains(args, "limit=1") {
				t.Fatalf("second review thread page args = %q, want remaining limit=1", args)
			}
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"thread-101","isResolved":true,"path":"b.go","line":20,"comments":{"nodes":[{"id":"comment-101","body":"last","createdAt":"2024-01-03T00:00:00Z","updatedAt":"2024-01-03T00:00:00Z","path":"b.go","line":20,"url":"https://example.test/comment-101","author":{"login":"carol"},"originalCommit":{"oid":"orig-101"},"commit":{"oid":"head-101"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`}, nil
		case strings.Contains(args, "reviewThreads(first: $limit, after: $after)") && !strings.Contains(args, "-F after="):
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[` + reviewThreadNodesJSON(1, 100) + `],"pageInfo":{"hasNextPage":true,"endCursor":"thread-cursor-1"}}}}}}`}, nil
		case strings.Contains(args, "comments(first: 100, after: $after)") && strings.Contains(args, "threadId=thread-1") && strings.Contains(args, "-F after=comment-cursor-1"):
			return shell.Result{Stdout: `{"data":{"node":{"comments":{"nodes":[{"id":"comment-2","body":"second","createdAt":"2024-01-02T00:00:00Z","updatedAt":"2024-01-02T00:00:00Z","path":"a.go","line":11,"url":"https://example.test/comment-2","author":{"login":"bob"},"originalCommit":{"oid":"orig-2"},"commit":{"oid":"head-2"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	threads, err := gateway.ListReviewThreads(context.Background(), ListReviewThreadsInput{Repo: "acme/looper", PRNumber: 42, Limit: 101})
	if err != nil {
		t.Fatalf("ListReviewThreads() error = %v", err)
	}
	if len(threads) != 101 {
		t.Fatalf("len(threads) = %d, want 101", len(threads))
	}
	if len(threads[0].Comments) != 2 {
		t.Fatalf("len(threads[0].Comments) = %d, want 2", len(threads[0].Comments))
	}
	if threads[0].Comments[1].ID != "comment-2" || threads[0].Comments[1].Author != "bob" || threads[0].Comments[1].OriginalCommitOID != "orig-2" || threads[0].Comments[1].CommitOID != "head-2" {
		t.Fatalf("threads[0].Comments[1] = %#v, want paginated comment metadata preserved", threads[0].Comments[1])
	}
	if threads[100].ID != "thread-101" || !threads[100].IsResolved {
		t.Fatalf("threads[100] = %#v, want second paginated thread", threads[100])
	}
	log := strings.Join(runner.calls, "\n")
	for _, needle := range []string{"limit=100", "after=thread-cursor-1", "threadId=thread-1", "after=comment-cursor-1"} {
		if !strings.Contains(log, needle) {
			t.Fatalf("gh log missing %q\n%s", needle, log)
		}
	}
}

func reviewThreadNodesJSON(start, count int) string {
	var b strings.Builder
	for i := start; i < start+count; i++ {
		if i > start {
			b.WriteString(",")
		}
		commentPageInfo := `"pageInfo":{"hasNextPage":false,"endCursor":""}`
		if i == 1 {
			commentPageInfo = `"pageInfo":{"hasNextPage":true,"endCursor":"comment-cursor-1"}`
		}
		b.WriteString(fmt.Sprintf(`{"id":"thread-%d","isResolved":false,"path":"a.go","line":%d,"comments":{"nodes":[{"id":"comment-%d","body":"first","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:00:00Z","path":"a.go","line":%d,"url":"https://example.test/comment-%d","author":{"login":"alice"},"originalCommit":{"oid":"orig-%d"},"commit":{"oid":"head-%d"}}],%s}}`, i, i, i, i, i, i, i, commentPageInfo))
	}
	return b.String()
}

func TestGatewayViewPullRequestPaginatesReviewThreads(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case strings.HasPrefix(args, "pr view 42 --repo acme/looper --json "):
			return shell.Result{Stdout: `{"number":42,"title":"Review me","body":"Body","url":"https://example.test/pull/42","state":"OPEN","isDraft":false,"reviewDecision":"COMMENTED","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","baseRefOid":"def456","mergeStateStatus":"CLEAN","author":{"login":"octocat"},"reviewRequests":[],"comments":[],"reviews":[],"statusCheckRollup":[]}`}, nil
		case strings.Contains(args, "reviewThreads(first: 100, after: $after)") && strings.Contains(args, "-F after=thread-cursor-1"):
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"thread-2","isResolved":true,"comments":{"nodes":[{"id":"comment-2","body":"second page"}]}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`}, nil
		case strings.Contains(args, "reviewThreads(first: 100, after: $after)") && !strings.Contains(args, "-F after="):
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"thread-1","isResolved":false,"comments":{"nodes":[{"id":"comment-1","body":"first page"}]}}],"pageInfo":{"hasNextPage":true,"endCursor":"thread-cursor-1"}}}}}}`}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	detail, err := gateway.ViewPullRequest(context.Background(), ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("ViewPullRequest() error = %v", err)
	}
	if len(detail.Comments) != 2 {
		t.Fatalf("len(detail.Comments) = %d, want 2", len(detail.Comments))
	}
	if detail.Comments[1]["threadId"] != "thread-2" || detail.Comments[1]["body"] != "second page" {
		t.Fatalf("detail.Comments[1] = %#v, want second paginated thread", detail.Comments[1])
	}
}

func TestGatewayHasReviewMarkerIgnoresIssueComments(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch args {
		case "api --paginate --slurp repos/acme/looper/pulls/42/reviews":
			return shell.Result{Stdout: `[{"body":"review without marker"}]`}, nil
		case "api --paginate --slurp repos/acme/looper/issues/42/comments":
			t.Fatalf("HasReviewMarker must not accept markers from issue comments")
		}
		t.Fatalf("unexpected gh args: %q", args)
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	found, err := gateway.HasReviewMarker(context.Background(), VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=abc"})
	if err != nil {
		t.Fatalf("HasReviewMarker() error = %v", err)
	}
	if found {
		t.Fatal("HasReviewMarker() = true, want false without marker in PR reviews")
	}
}

func TestGatewayHasReviewMarkerRequiresAllowedReviewEvent(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if strings.Join(options.Args, " ") == "api --paginate --slurp repos/acme/looper/pulls/42/reviews" {
			return shell.Result{Stdout: `[{"state":"APPROVED","body":"<!-- looper:review id=abc head=def outcome=clean -->"}]`}, nil
		}
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	found, err := gateway.HasReviewMarker(context.Background(), VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=abc", AllowedReviewEvents: []string{"COMMENT"}})
	if err != nil {
		t.Fatalf("HasReviewMarker() error = %v", err)
	}
	if found {
		t.Fatal("HasReviewMarker() = true, want false for disallowed approval marker")
	}
	found, err = gateway.HasReviewMarker(context.Background(), VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=abc", AllowedReviewEvents: []string{"COMMENT", "APPROVE"}})
	if err != nil {
		t.Fatalf("HasReviewMarker() error = %v", err)
	}
	if !found {
		t.Fatal("HasReviewMarker() = false, want true when approval is allowed")
	}
}

func TestGatewayHasReviewMarkerAllowsChangesRequestedReviewEvent(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if strings.Join(options.Args, " ") == "api --paginate --slurp repos/acme/looper/pulls/42/reviews" {
			return shell.Result{Stdout: `[{"state":"CHANGES_REQUESTED","body":"<!-- looper:review id=abc head=def outcome=blocking -->"}]`}, nil
		}
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	found, err := gateway.HasReviewMarker(context.Background(), VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=abc", AllowedReviewEvents: []string{"REQUEST_CHANGES"}})
	if err != nil {
		t.Fatalf("HasReviewMarker() error = %v", err)
	}
	if !found {
		t.Fatal("HasReviewMarker() = false, want true when request-changes review is allowed")
	}
}

func TestGatewayHasReviewMarkerRejectsEventOutcomePolicyMismatch(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if strings.Join(options.Args, " ") == "api --paginate --slurp repos/acme/looper/pulls/42/reviews" {
			return shell.Result{Stdout: `[{"state":"COMMENTED","body":"<!-- looper:review id=clean head=def outcome=clean -->"},{"state":"COMMENTED","body":"<!-- looper:review id=blocking head=def outcome=blocking -->"},{"state":"CHANGES_REQUESTED","body":"<!-- looper:review id=nonblocking head=def outcome=non_blocking -->"}]`}, nil
		}
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	for _, marker := range []string{"looper:review id=clean", "looper:review id=blocking", "looper:review id=nonblocking"} {
		found, err := gateway.HasReviewMarker(context.Background(), VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: marker, AllowedReviewEvents: []string{"COMMENT", "APPROVE", "REQUEST_CHANGES"}})
		if err != nil {
			t.Fatalf("HasReviewMarker(%q) error = %v", marker, err)
		}
		if found {
			t.Fatalf("HasReviewMarker(%q) = true, want false for event/outcome policy mismatch", marker)
		}
	}
}

func TestGatewayHasReviewMarkerAllowsCleanCommentFallback(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if strings.Join(options.Args, " ") == "api --paginate --slurp repos/acme/looper/pulls/42/reviews" {
			return shell.Result{Stdout: `[{"state":"COMMENTED","body":"<!-- looper:review id=clean head=def outcome=clean -->"}]`}, nil
		}
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	found, err := gateway.HasReviewMarker(context.Background(), VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=clean", AllowedReviewEvents: []string{"COMMENT", "APPROVE"}, AllowCleanComment: true})
	if err != nil {
		t.Fatalf("HasReviewMarker() error = %v", err)
	}
	if !found {
		t.Fatal("HasReviewMarker() = false, want true for allowed clean COMMENT fallback")
	}
}

func TestGatewayHasReviewMarkerReadsSlurpedPaginatedReviews(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if strings.Join(options.Args, " ") == "api --paginate --slurp repos/acme/looper/pulls/42/reviews" {
			return shell.Result{Stdout: `[[{"state":"COMMENTED","body":"review without marker"}],[{"state":"APPROVED","body":"<!-- looper:review id=abc head=def outcome=clean -->"}]]`}, nil
		}
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	found, err := gateway.HasReviewMarker(context.Background(), VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=abc", AllowedReviewEvents: []string{"APPROVE"}})
	if err != nil {
		t.Fatalf("HasReviewMarker() error = %v", err)
	}
	if !found {
		t.Fatal("HasReviewMarker() = false, want true for marker in slurped paginated reviews")
	}
}

func TestGatewayFindReviewMarkerExtractsOutcomeFromMatchedMarker(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if strings.Join(options.Args, " ") == "api --paginate --slurp repos/acme/looper/pulls/42/reviews" {
			return shell.Result{Stdout: `[{"state":"COMMENTED","body":"This prose mentions outcome=clean but is not the marker.\n<!-- looper:review id=abc head=def outcome=actionable -->"}]`}, nil
		}
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	marker, err := gateway.FindReviewMarker(context.Background(), VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=abc head=def", AllowedReviewEvents: []string{"COMMENT"}})
	if err != nil {
		t.Fatalf("FindReviewMarker() error = %v", err)
	}
	if !marker.Found || marker.Outcome != "actionable" || marker.Event != "COMMENT" {
		t.Fatalf("FindReviewMarker() = %#v, want actionable COMMENT marker from matched marker", marker)
	}
}

func TestGatewayFindReviewMarkerMatchesExpectedHeadWithLoopIDPrefix(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if strings.Join(options.Args, " ") == "api --paginate --slurp repos/acme/looper/pulls/42/reviews" {
			return shell.Result{Stdout: `[
				{"state":"COMMENTED","body":"<!-- looper:review id=reviewer:other-loop:abc123 head=abc123 outcome=blocking -->"},
				{"state":"COMMENTED","body":"<!-- looper:review id=reviewer:loop-1:legacy-run head=abc123 outcome=actionable -->"},
				{"state":"COMMENTED","body":"<!-- looper:review id=reviewer:loop-1:def456 head=def456 outcome=clean -->"}
			]`}, nil
		}
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	marker, err := gateway.FindReviewMarker(context.Background(), VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id_prefix=reviewer:loop-1: head=abc123", AllowedReviewEvents: []string{"COMMENT"}})
	if err != nil {
		t.Fatalf("FindReviewMarker() error = %v", err)
	}
	if !marker.Found || marker.Outcome != "actionable" || marker.Event != "COMMENT" {
		t.Fatalf("FindReviewMarker() = %#v, want marker matching expected head and loop id prefix", marker)
	}
}

func TestGatewayFindReviewMarkerRequiresWellFormedMarker(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if strings.Join(options.Args, " ") == "api --paginate --slurp repos/acme/looper/pulls/42/reviews" {
			return shell.Result{Stdout: `[{"state":"COMMENTED","body":"This prose mentions looper:review id=abc head=def and outcome=clean but has no marker comment."},{"state":"COMMENTED","body":"<!-- looper:review id=abc head=def -->"}]`}, nil
		}
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	marker, err := gateway.FindReviewMarker(context.Background(), VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=abc head=def", AllowedReviewEvents: []string{"COMMENT"}})
	if err != nil {
		t.Fatalf("FindReviewMarker() error = %v", err)
	}
	if marker.Found {
		t.Fatalf("FindReviewMarker() = %#v, want no marker for prose or missing outcome", marker)
	}
}

func TestGatewayFindReviewMarkerReturnsNewestMatchingMarker(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if strings.Join(options.Args, " ") == "api --paginate --slurp repos/acme/looper/pulls/42/reviews" {
			return shell.Result{Stdout: `[
				{"state":"COMMENTED","body":"<!-- looper:review id=abc head=def outcome=actionable -->"},
				{"state":"APPROVED","body":"<!-- looper:review id=abc head=def outcome=clean -->"},
				{"state":"COMMENTED","body":"<!-- looper:review id=abc head=def outcome=clean -->"}
			]`}, nil
		}
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	marker, err := gateway.FindReviewMarker(context.Background(), VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=abc head=def", AllowedReviewEvents: []string{"COMMENT"}})
	if err != nil {
		t.Fatalf("FindReviewMarker() error = %v", err)
	}
	if !marker.Found || marker.Outcome != "clean" || marker.Event != "COMMENT" {
		t.Fatalf("FindReviewMarker() = %#v, want newest matching COMMENT marker", marker)
	}
}

func TestGatewayFindReviewMarkerRequiresAuthorLogin(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if strings.Join(options.Args, " ") == "api --paginate --slurp repos/acme/looper/pulls/42/reviews" {
			return shell.Result{Stdout: `[
				{"state":"COMMENTED","user":{"login":"other-bot"},"body":"<!-- looper:review id=abc head=def outcome=clean -->"},
				{"state":"COMMENTED","user":{"login":"reviewer-bot"},"body":"<!-- looper:review id=abc head=def outcome=actionable -->"}
			]`}, nil
		}
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	marker, err := gateway.FindReviewMarker(context.Background(), VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=abc head=def", AllowedReviewEvents: []string{"COMMENT"}, AuthorLogin: "Reviewer-Bot"})
	if err != nil {
		t.Fatalf("FindReviewMarker() error = %v", err)
	}
	if !marker.Found || marker.Outcome != "actionable" || marker.AuthorLogin != "reviewer-bot" {
		t.Fatalf("FindReviewMarker() = %#v, want marker authored by reviewer-bot", marker)
	}
}

func TestGatewayFindReviewMarkerFetchesMatchedReviewComments(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		switch strings.Join(options.Args, " ") {
		case "api --paginate --slurp repos/acme/looper/pulls/42/reviews":
			return shell.Result{Stdout: `[
				{"id":101,"state":"COMMENTED","body":"<!-- looper:review id=abc head=def outcome=clean -->"},
				{"id":202,"state":"COMMENTED","body":"<!-- looper:review id=abc head=def outcome=actionable -->"}
			]`}, nil
		case "api --paginate --slurp repos/acme/looper/pulls/42/reviews/202/comments":
			return shell.Result{Stdout: `[[{"body":"first inline finding"}],[{"body":"second inline finding"}]]`}, nil
		}
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	marker, err := gateway.FindReviewMarker(context.Background(), VerifyReviewMarkerInput{Repo: "acme/looper", PRNumber: 42, Marker: "looper:review id=abc head=def", AllowedReviewEvents: []string{"COMMENT"}})
	if err != nil {
		t.Fatalf("FindReviewMarker() error = %v", err)
	}
	if !marker.Found || marker.ReviewID != "202" || marker.Outcome != "actionable" {
		t.Fatalf("FindReviewMarker() = %#v, want newest matching review 202", marker)
	}
	if got, want := strings.Join(marker.InlineCommentBodies, "\n"), "first inline finding\nsecond inline finding"; got != want {
		t.Fatalf("InlineCommentBodies = %q, want %q", got, want)
	}
}

func TestGatewayRemovePullRequestReactionReadsSlurpedPaginatedReactions(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		switch strings.Join(options.Args, " ") {
		case "api user --jq .login":
			return shell.Result{Stdout: "reviewer\n"}, nil
		case "api --paginate --slurp repos/acme/looper/issues/42/reactions -H Accept: application/vnd.github+json":
			return shell.Result{Stdout: `[[{"id":6,"content":"+1","user":{"login":"someoneelse"}}],[{"id":7,"content":"+1","user":{"login":"reviewer"}}]]`}, nil
		case "api repos/acme/looper/issues/42/reactions/7 --method DELETE -H Accept: application/vnd.github+json":
			return shell.Result{Stdout: "{}"}, nil
		}
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.RemovePullRequestReaction(context.Background(), PullRequestReactionInput{Repo: "acme/looper", PRNumber: 42, Content: "+1"}); err != nil {
		t.Fatalf("RemovePullRequestReaction() error = %v", err)
	}
	if !strings.Contains(strings.Join(runner.calls, "\n"), "reactions/7 --method DELETE") {
		t.Fatalf("gh calls = %#v, want deletion of paginated current-user reaction", runner.calls)
	}
}

func TestGatewayIsAuthenticatedTracksGHAuthStatus(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args == "auth status" {
			result := shell.Result{ExitCode: 1}
			return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
		}
		return shell.Result{Stdout: "{}"}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	authenticated, err := gateway.IsAuthenticated(context.Background(), "", "")
	if err != nil {
		t.Fatalf("IsAuthenticated() error = %v", err)
	}
	if authenticated {
		t.Fatal("IsAuthenticated() = true, want false for unauthenticated gh cli")
	}
}

func TestGatewayGetCurrentUserLoginReturnsEmptyForIntegrationTokens(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if strings.Join(options.Args, " ") == "api user --jq .login" {
			result := shell.Result{ExitCode: 1, Stderr: "HTTP 403: Resource not accessible by integration"}
			return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
		}
		t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	login, err := gateway.GetCurrentUserLogin(context.Background(), "")
	if err != nil {
		t.Fatalf("GetCurrentUserLogin() error = %v", err)
	}
	if login != "" {
		t.Fatalf("GetCurrentUserLogin() = %q, want empty login for integration token", login)
	}
}

func TestGatewayGetCurrentUserLoginForRepoPropagatesViewerLookupFailure(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		switch strings.Join(options.Args, " ") {
		case "api user --jq .login":
			result := shell.Result{ExitCode: 1, Stderr: "HTTP 403: Resource not accessible by integration"}
			return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
		case "api graphql -f query=query { viewer { login } }":
			result := shell.Result{ExitCode: 1, Stderr: "gh: GraphQL request failed"}
			return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
		default:
			t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	login, err := gateway.GetCurrentUserLoginForRepo(context.Background(), "acme/looper", "")
	if err == nil {
		t.Fatal("GetCurrentUserLoginForRepo() error = nil, want propagated viewer lookup failure")
	}
	if login != "" {
		t.Fatalf("GetCurrentUserLoginForRepo() login = %q, want empty login on failure", login)
	}
}

func TestGatewayGetCurrentUserLoginForRepoPropagatesViewerLookupDecodeFailure(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		switch strings.Join(options.Args, " ") {
		case "api user --jq .login":
			result := shell.Result{ExitCode: 1, Stderr: "HTTP 403: Resource not accessible by integration"}
			return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
		case "api graphql -f query=query { viewer { login } }":
			return shell.Result{Stdout: "{"}, nil
		default:
			t.Fatalf("unexpected gh args: %q", strings.Join(options.Args, " "))
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	login, err := gateway.GetCurrentUserLoginForRepo(context.Background(), "acme/looper", "")
	if err == nil {
		t.Fatal("GetCurrentUserLoginForRepo() error = nil, want propagated viewer decode failure")
	}
	if login != "" {
		t.Fatalf("GetCurrentUserLoginForRepo() login = %q, want empty login on failure", login)
	}
}

func TestGatewayIsAuthenticatedScopesStatusToHostname(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args == "auth status --hostname github.example.com" {
			return shell.Result{}, nil
		}
		result := shell.Result{ExitCode: 1}
		return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	authenticated, err := gateway.IsAuthenticated(context.Background(), "", "github.example.com")
	if err != nil {
		t.Fatalf("IsAuthenticated() error = %v", err)
	}
	if !authenticated {
		t.Fatal("IsAuthenticated() = false, want true for hostname-scoped auth")
	}
	if !strings.Contains(strings.Join(runner.calls, "\n"), "auth status --hostname github.example.com") {
		t.Fatalf("gh log = %q, want hostname-scoped auth status", strings.Join(runner.calls, "\n"))
	}
}

func TestGatewayViewIssueScopesAPIToHostname(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch args {
		case "api repos/acme/looper/issues/8 --hostname github.example.com":
			return shell.Result{Stdout: `{"number":8,"title":"Issue","body":"Body","state":"open","updated_at":"2026-05-01T00:00:00Z","user":{"login":"octo"},"author_association":"MEMBER","labels":[]}`}, nil
		case "api --paginate --slurp repos/acme/looper/issues/8/comments --hostname github.example.com":
			return shell.Result{Stdout: `[]`}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	detail, err := gateway.ViewIssue(context.Background(), ViewIssueInput{Repo: "github.example.com/acme/looper", IssueNumber: 8})
	if err != nil {
		t.Fatalf("ViewIssue() error = %v", err)
	}
	if detail.Number != 8 || detail.AuthorAssociation != "MEMBER" {
		t.Fatalf("ViewIssue() = %#v, want parsed enterprise issue detail", detail)
	}
}

func TestListIssueBlockedByUsesRepositoryURLForCrossRepoBlockers(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if got := strings.Join(options.Args, " "); got != "api --paginate --slurp repos/acme/looper/issues/12/dependencies/blocked_by" {
			t.Fatalf("unexpected gh args: %q", got)
		}
		return shell.Result{Stdout: `[[{"number":34,"repository_url":"https://github.example.com/api/v3/repos/other/repo"}]]`}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	blockedBy, err := gateway.ListIssueBlockedBy(context.Background(), ListIssueBlockedByInput{Repo: "acme/looper", IssueNumber: 12})
	if err != nil {
		t.Fatalf("ListIssueBlockedBy() error = %v", err)
	}
	if len(blockedBy) != 1 || blockedBy[0].Number != 34 || blockedBy[0].Repo != "github.example.com/other/repo" {
		t.Fatalf("ListIssueBlockedBy() = %#v, want hosted cross-repo blocker", blockedBy)
	}
}

func TestListIssueBlockedByIgnoresPublicGitHubAPIHostname(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if got := strings.Join(options.Args, " "); got != "api --paginate --slurp repos/acme/looper/issues/12/dependencies/blocked_by" {
			t.Fatalf("unexpected gh args: %q", got)
		}
		return shell.Result{Stdout: `[[{"number":34,"repository":{"full_name":"other/repo","url":"https://api.github.com/repos/other/repo"}}]]`}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	blockedBy, err := gateway.ListIssueBlockedBy(context.Background(), ListIssueBlockedByInput{Repo: "acme/looper", IssueNumber: 12})
	if err != nil {
		t.Fatalf("ListIssueBlockedBy() error = %v", err)
	}
	if len(blockedBy) != 1 || blockedBy[0].Number != 34 || blockedBy[0].Repo != "other/repo" {
		t.Fatalf("ListIssueBlockedBy() = %#v, want unqualified public GitHub repo", blockedBy)
	}
}

func TestFindAnyIssueNumberSkipsPullRequests(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if got := strings.Join(options.Args, " "); got != "api repos/acme/looper/issues?state=all&per_page=100&page=1" {
			t.Fatalf("unexpected gh args: %q", got)
		}
		return shell.Result{Stdout: `[{"number":99,"pull_request":{"url":"https://example.test/pr/99"}},{"number":7}]`}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	issueNumber, err := gateway.FindAnyIssueNumber(context.Background(), "acme/looper", "")
	if err != nil {
		t.Fatalf("FindAnyIssueNumber() error = %v", err)
	}
	if issueNumber != 7 {
		t.Fatalf("FindAnyIssueNumber() = %d, want first non-PR issue", issueNumber)
	}
}

func TestFindAnyIssueNumberChecksLaterPages(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		switch got := strings.Join(options.Args, " "); got {
		case "api repos/acme/looper/issues?state=all&per_page=100&page=1":
			return shell.Result{Stdout: `[{"number":99,"pull_request":{"url":"https://example.test/pr/99"}}]`}, nil
		case "api repos/acme/looper/issues?state=all&per_page=100&page=2":
			return shell.Result{Stdout: `[{"number":7}]`}, nil
		default:
			t.Fatalf("unexpected gh args: %q", got)
			return shell.Result{}, nil
		}
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	issueNumber, err := gateway.FindAnyIssueNumber(context.Background(), "acme/looper", "")
	if err != nil {
		t.Fatalf("FindAnyIssueNumber() error = %v", err)
	}
	if issueNumber != 7 {
		t.Fatalf("FindAnyIssueNumber() = %d, want issue discovered on later page", issueNumber)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("FindAnyIssueNumber() calls = %d, want two paged requests", len(runner.calls))
	}
}

func TestGatewayListLinkedPullRequestsHandlesHostQualifiedRepo(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if !strings.Contains(args, "api graphql") || !strings.Contains(args, "-F owner=acme") || !strings.Contains(args, "-F repo=looper") || !strings.Contains(args, "--hostname github.example.com") || strings.Contains(args, "github.example.com/acme") {
			t.Fatalf("unexpected gh args: %q", args)
		}
		return shell.Result{Stdout: `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[{"number":42,"state":"MERGED","mergedAt":"2026-05-01T00:00:00Z","mergeCommit":{"oid":"abc123"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	prs, err := gateway.ListLinkedPullRequests(context.Background(), LinkedPullRequestsInput{Repo: "github.example.com/acme/looper", IssueNumber: 8})
	if err != nil {
		t.Fatalf("ListLinkedPullRequests() error = %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 42 || prs[0].MergeCommitSHA != "abc123" {
		t.Fatalf("ListLinkedPullRequests() = %#v, want parsed linked PRs", prs)
	}
}

func TestGatewayListLinkedPullRequestsPaginates(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case strings.Contains(args, "closedByPullRequestsReferences(first: 20, after: $after)") && strings.Contains(args, "-F after=cursor-1"):
			return shell.Result{Stdout: `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[{"number":22,"state":"CLOSED","mergedAt":"","mergeCommit":{"oid":""}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`}, nil
		case strings.Contains(args, "closedByPullRequestsReferences(first: 20, after: $after)"):
			return shell.Result{Stdout: `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[{"number":21,"state":"MERGED","mergedAt":"2026-05-01T00:00:00Z","mergeCommit":{"oid":"abc123"}}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}}}`}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	prs, err := gateway.ListLinkedPullRequests(context.Background(), LinkedPullRequestsInput{Repo: "acme/looper", IssueNumber: 8})
	if err != nil {
		t.Fatalf("ListLinkedPullRequests() error = %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("ListLinkedPullRequests() len = %d, want 2", len(prs))
	}
	if prs[0].Number != 21 || !prs[0].Merged || prs[0].MergeCommitSHA != "abc123" {
		t.Fatalf("ListLinkedPullRequests()[0] = %#v, want merged first page result", prs[0])
	}
	if prs[1].Number != 22 || prs[1].Merged || prs[1].MergeCommitSHA != "" {
		t.Fatalf("ListLinkedPullRequests()[1] = %#v, want unmerged second page result", prs[1])
	}
}

func TestGatewayListIssueTimelineScopesAPIToHostname(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api --paginate --slurp repos/acme/looper/issues/8/timeline -H Accept: application/vnd.github+json --hostname github.example.com" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		return shell.Result{Stdout: `[[{"id":1,"event":"closed"}]]`}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	rows, err := gateway.ListIssueTimeline(context.Background(), IssueTimelineInput{Repo: "github.example.com/acme/looper", IssueNumber: 8})
	if err != nil {
		t.Fatalf("ListIssueTimeline() error = %v", err)
	}
	if len(rows) != 1 || asInt64(rows[0]["id"]) != 1 || asString(rows[0]["event"]) != "closed" {
		t.Fatalf("ListIssueTimeline() = %#v, want parsed enterprise timeline", rows)
	}
}

func TestGatewayListIssueReactionsScopesAPIToHostname(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch args {
		case "api --paginate --slurp repos/acme/looper/issues/8/reactions -H Accept: application/vnd.github+json --hostname github.example.com":
			return shell.Result{Stdout: `[[{"id":7,"content":"eyes","user":{"login":"reviewer"}}]]`}, nil
		case "api --paginate --slurp repos/acme/looper/issues/comments/91/reactions -H Accept: application/vnd.github+json --hostname github.example.com":
			return shell.Result{Stdout: `[[{"id":8,"content":"rocket","user":{"login":"octo"}}]]`}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	reactions, err := gateway.ListIssueReactions(context.Background(), IssueReactionInput{Repo: "github.example.com/acme/looper", IssueNumber: 8})
	if err != nil {
		t.Fatalf("ListIssueReactions(issue) error = %v", err)
	}
	if len(reactions) != 1 || reactions[0].ID != 7 || reactions[0].Content != "eyes" || reactions[0].UserLogin != "reviewer" {
		t.Fatalf("ListIssueReactions(issue) = %#v, want parsed enterprise issue reactions", reactions)
	}
	commentReactions, err := gateway.ListIssueReactions(context.Background(), IssueReactionInput{Repo: "github.example.com/acme/looper", CommentID: 91})
	if err != nil {
		t.Fatalf("ListIssueReactions(comment) error = %v", err)
	}
	if len(commentReactions) != 1 || commentReactions[0].ID != 8 || commentReactions[0].Content != "rocket" || commentReactions[0].UserLogin != "octo" {
		t.Fatalf("ListIssueReactions(comment) = %#v, want parsed enterprise comment reactions", commentReactions)
	}
}

func TestGatewayAddIssueReactionScopesAPIToHostname(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/issues/comments/91/reactions --method POST -H Accept: application/vnd.github+json -f content=+1 --hostname github.example.com" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		return shell.Result{Stdout: `{}`}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.AddIssueReaction(context.Background(), CreateIssueReactionInput{Repo: "github.example.com/acme/looper", CommentID: 91, Content: "+1"}); err != nil {
		t.Fatalf("AddIssueReaction() error = %v", err)
	}
}

func TestGatewayIgnoresPlainPullRequestCommentsAsReviewThreads(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case strings.Contains(args, "pr view"):
			return shell.Result{Stdout: `{"number":42,"title":"Review me","body":"Body","url":"https://example.test/pull/42","state":"OPEN","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","baseRefOid":"def456","author":{"login":"octocat"},"reviewRequests":[],"comments":[{"id":"IC_comment","body":"@codex review"}],"reviews":[],"statusCheckRollup":[]}`}, nil
		case strings.Contains(args, "reviewThreads"):
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`}, nil
		default:
			return shell.Result{Stdout: "{}"}, nil
		}
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	detail, err := gateway.ViewPullRequest(context.Background(), ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("ViewPullRequest() error = %v", err)
	}
	if len(detail.Comments) != 0 {
		t.Fatalf("len(detail.Comments) = %d, want 0", len(detail.Comments))
	}
}

func TestGatewaySurfacesPermissionErrorsWhenResolvingReviewThread(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case strings.Contains(args, "resolveReviewThread"):
			result := shell.Result{ExitCode: 1, Stderr: "permission denied"}
			return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
		case strings.Contains(args, "threadId=thread-1"):
			return shell.Result{Stdout: `{"data":{"node":{"id":"thread-1","isResolved":false}}}`}, nil
		default:
			return shell.Result{Stdout: "{}"}, nil
		}
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.ResolveReviewThread(context.Background(), ResolveReviewThreadInput{Repo: "acme/looper", ThreadID: "thread-1"})
	if err == nil || !strings.Contains(err.Error(), "Command exited with code 1") {
		t.Fatalf("ResolveReviewThread() error = %v, want command exit error", err)
	}
}

func TestGatewayIgnoresMissingLabelDeleteErrors(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if strings.Contains(args, "api repos/acme/looper/issues/42/labels/looper%3Aspec-ready --method DELETE") {
			result := shell.Result{ExitCode: 1, Stderr: "gh: HTTP 404: label does not exist (https://api.github.com/...)"}
			return result, &shell.CommandExecutionError{Message: "Command exited with code 1", Result: result}
		}
		return shell.Result{Stdout: "{}"}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.RemovePullRequestLabels(context.Background(), PullRequestLabelsInput{Repo: "acme/looper", PRNumber: 42, Labels: []string{"looper:spec-ready"}}); err != nil {
		t.Fatalf("RemovePullRequestLabels() error = %v, want nil", err)
	}
}

func TestGatewayCapturePullRequestSnapshotTruncatesTooLargeDiff(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case strings.HasPrefix(args, "pr view"):
			return shell.Result{Stdout: `{"number":42,"title":"Review me","body":"Body","state":"OPEN","headRefOid":"abc123"}`}, nil
		case strings.Contains(args, "reviewThreads"):
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`}, nil
		case strings.HasPrefix(args, "pr diff"):
			result := shell.Result{ExitCode: 1, Stderr: "HTTP 406: diff exceeded maximum number of lines too_large"}
			return result, &shell.CommandExecutionError{Message: result.Stderr, Result: result}
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}
	gateway := New(Options{GHPath: "gh", GHRun: runner.run})

	snapshot, err := gateway.CapturePullRequestSnapshot(context.Background(), CapturePullRequestSnapshotInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("CapturePullRequestSnapshot() error = %v", err)
	}
	if snapshot.PayloadJSON == nil || !strings.Contains(*snapshot.PayloadJSON, `"diffTruncated":true`) || !strings.Contains(*snapshot.PayloadJSON, `"diffTruncationReason":"github_too_large"`) {
		t.Fatalf("PayloadJSON = %v, want truncated marker", snapshot.PayloadJSON)
	}
}

func TestGatewayDiffGenericFailureStillFails(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		result := shell.Result{ExitCode: 1, Stderr: "network failed"}
		return result, &shell.CommandExecutionError{Message: result.Stderr, Result: result}
	}
	gateway := New(Options{GHPath: "gh", GHRun: runner.run})
	_, err := gateway.GetPullRequestDiff(context.Background(), GetPullRequestDiffInput{Repo: "acme/looper", PRNumber: 42})
	if err == nil || errors.Is(err, ErrDiffTooLarge) {
		t.Fatalf("GetPullRequestDiff() error = %v, want generic failure", err)
	}
}

func TestGatewayRunGhPassesTimeouts(t *testing.T) {
	t.Parallel()
	var timeouts []time.Duration
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		timeouts = append(timeouts, options.Timeout)
		return shell.Result{Stdout: "[]"}, nil
	}
	gateway := New(Options{GHPath: "gh", GHRun: runner.run})
	_, _ = gateway.ListOpenPullRequests(context.Background(), ListOpenPullRequestsInput{Repo: "acme/looper", Timeout: prListGhCommandTimeout})
	_, _ = gateway.GetPullRequestDiff(context.Background(), GetPullRequestDiffInput{Repo: "acme/looper", PRNumber: 42})
	if len(timeouts) != 2 || timeouts[0] != prListGhCommandTimeout || timeouts[1] != prDiffGhCommandTimeout {
		t.Fatalf("timeouts = %#v, want list/diff timeouts", timeouts)
	}
}

func TestGatewayInitializesLooperLabelsIdempotently(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch args {
		case "label list --repo acme/looper --limit 1000 --json name,color,description":
			return shell.Result{Stdout: `[{"name":"looper:plan","color":"5319e7","description":"Picked up automatically by planner"},{"name":"looper:spec-reviewing","color":"000000","description":"Old description"}]`}, nil
		case "label edit looper:spec-reviewing --repo acme/looper --color 1d76db --description Spec PR is under review":
			return shell.Result{Stdout: "{}"}, nil
		case "label create looper:spec-ready --repo acme/looper --color 0e8a16 --description Spec PR is ready for implementation":
			return shell.Result{Stdout: "{}"}, nil
		case "label create looper:needs-human --repo acme/looper --color d93f0b --description Looper requires manual intervention":
			return shell.Result{Stdout: "{}"}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	result, err := gateway.InitializeLabels(context.Background(), InitializeLabelsInput{Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("InitializeLabels() error = %v", err)
	}
	if result.Summary.Created != 2 || result.Summary.Updated != 1 || result.Summary.Skipped != 1 || result.Summary.Failed != 0 {
		t.Fatalf("InitializeLabels() summary = %#v, want created=2 updated=1 skipped=1 failed=0", result.Summary)
	}

	log := strings.Join(runner.calls, "\n")
	for _, needle := range []string{
		"label list --repo acme/looper --limit 1000 --json name,color,description",
		"label edit looper:spec-reviewing --repo acme/looper --color 1d76db --description Spec PR is under review",
		"label create looper:spec-ready --repo acme/looper --color 0e8a16 --description Spec PR is ready for implementation",
		"label create looper:needs-human --repo acme/looper --color d93f0b --description Looper requires manual intervention",
	} {
		if !strings.Contains(log, needle) {
			t.Fatalf("gh log missing %q\n%s", needle, log)
		}
	}
}

func TestGatewayInitializesLooperLabelsForHostQualifiedRepo(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch args {
		case "label list --repo github.example.com/acme/looper --limit 1000 --json name,color,description":
			return shell.Result{Stdout: `[]`}, nil
		case "label create looper:plan --repo github.example.com/acme/looper --color 5319e7 --description Picked up automatically by planner":
			return shell.Result{Stdout: "{}"}, nil
		case "label create looper:spec-reviewing --repo github.example.com/acme/looper --color 1d76db --description Spec PR is under review":
			return shell.Result{Stdout: "{}"}, nil
		case "label create looper:spec-ready --repo github.example.com/acme/looper --color 0e8a16 --description Spec PR is ready for implementation":
			return shell.Result{Stdout: "{}"}, nil
		case "label create looper:needs-human --repo github.example.com/acme/looper --color d93f0b --description Looper requires manual intervention":
			return shell.Result{Stdout: "{}"}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	result, err := gateway.InitializeLabels(context.Background(), InitializeLabelsInput{Repo: "github.example.com/acme/looper"})
	if err != nil {
		t.Fatalf("InitializeLabels() error = %v", err)
	}
	if result.Repo != "github.example.com/acme/looper" || result.Summary.Created != 4 {
		t.Fatalf("InitializeLabels() result = %#v, want host-qualified repo and created=4", result)
	}
}

func TestGatewayDryRunInitializesLooperLabelsWithoutMutating(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args == "label list --repo acme/looper --limit 1000 --json name,color,description" {
			return shell.Result{Stdout: `[]`}, nil
		}
		t.Fatalf("unexpected gh args: %q", args)
		return shell.Result{}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	result, err := gateway.InitializeLabels(context.Background(), InitializeLabelsInput{Repo: "acme/looper", DryRun: true})
	if err != nil {
		t.Fatalf("InitializeLabels(dry run) error = %v", err)
	}
	if result.Summary.Created != 4 || len(runner.calls) != 1 {
		t.Fatalf("dry run result = %#v, calls = %#v; want four planned creates and only label list", result.Summary, runner.calls)
	}
}

func TestGatewayInitializeLabelsReturnsErrorWhenMutationFails(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch args {
		case "label list --repo acme/looper --limit 1000 --json name,color,description":
			return shell.Result{Stdout: `[{"name":"looper:plan","color":"5319e7","description":"Picked up automatically by planner"}]`}, nil
		case "label create looper:spec-reviewing --repo acme/looper --color 1d76db --description Spec PR is under review":
			result := shell.Result{ExitCode: 1, Stderr: "permission denied"}
			return result, &shell.CommandExecutionError{Message: "gh exited with code 1: permission denied", Result: result}
		case "label create looper:spec-ready --repo acme/looper --color 0e8a16 --description Spec PR is ready for implementation":
			return shell.Result{Stdout: "{}"}, nil
		case "label create looper:needs-human --repo acme/looper --color d93f0b --description Looper requires manual intervention":
			return shell.Result{Stdout: "{}"}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	result, err := gateway.InitializeLabels(context.Background(), InitializeLabelsInput{Repo: "acme/looper"})
	if err == nil {
		t.Fatalf("InitializeLabels() error = nil, want failure")
	}
	if result.Summary.Failed != 1 || result.Summary.Created != 2 || result.Summary.Skipped != 1 {
		t.Fatalf("InitializeLabels() summary = %#v, want created=2 skipped=1 failed=1", result.Summary)
	}
	if got := result.Labels[1].Error; !strings.Contains(got, "permission denied") {
		t.Fatalf("failed label error = %q, want stderr details", got)
	}
}

func TestGatewayDetectsCurrentRepository(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if args := strings.Join(options.Args, " "); args != "repo view --json nameWithOwner,url" {
			t.Fatalf("gh args = %q, want repo view", args)
		}
		return shell.Result{Stdout: `{"nameWithOwner":"acme/looper","url":"https://github.com/acme/looper"}`}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	repo, err := gateway.DetectCurrentRepository(context.Background(), "")
	if err != nil {
		t.Fatalf("DetectCurrentRepository() error = %v", err)
	}
	if repo != "acme/looper" {
		t.Fatalf("DetectCurrentRepository() = %q, want acme/looper", repo)
	}
}

func TestGatewayDetectsCurrentEnterpriseRepository(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		if args := strings.Join(options.Args, " "); args != "repo view --json nameWithOwner,url" {
			t.Fatalf("gh args = %q, want repo view", args)
		}
		return shell.Result{Stdout: `{"nameWithOwner":"acme/looper","url":"https://github.example.com/acme/looper"}`}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	repo, err := gateway.DetectCurrentRepository(context.Background(), "")
	if err != nil {
		t.Fatalf("DetectCurrentRepository() error = %v", err)
	}
	if repo != "github.example.com/acme/looper" {
		t.Fatalf("DetectCurrentRepository() = %q, want github.example.com/acme/looper", repo)
	}
}

func TestGatewayCloseIssueUsesTypedReasonAndIsIdempotent(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch args {
		case "api repos/acme/looper/issues/8 --jq .state":
			return shell.Result{Stdout: "open\n"}, nil
		case "issue close 8 --repo acme/looper --reason completed":
			return shell.Result{Stdout: ""}, nil
		case "api repos/acme/looper/issues/9 --jq .state":
			return shell.Result{Stdout: "closed\n"}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.CloseIssue(context.Background(), CloseIssueInput{Repo: "acme/looper", IssueNumber: 8, StateReason: " completed "}); err != nil {
		t.Fatalf("CloseIssue(open) error = %v", err)
	}
	if err := gateway.CloseIssue(context.Background(), CloseIssueInput{Repo: "acme/looper", IssueNumber: 9, StateReason: "not_planned"}); err != nil {
		t.Fatalf("CloseIssue(closed) error = %v", err)
	}
	log := strings.Join(runner.calls, "\n")
	if !strings.Contains(log, "issue close 8 --repo acme/looper --reason completed") {
		t.Fatalf("gh log missing close issue command\n%s", log)
	}
	if strings.Contains(log, "issue close 9 --repo acme/looper") {
		t.Fatalf("gh log unexpectedly closed an already-closed issue\n%s", log)
	}
}

func TestGatewayClosePullRequestIsIdempotent(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch args {
		case "pr view 42 --repo acme/looper --json state --jq .state":
			return shell.Result{Stdout: "OPEN\n"}, nil
		case "pr close 42 --repo acme/looper":
			return shell.Result{Stdout: ""}, nil
		case "pr view 43 --repo acme/looper --json state --jq .state":
			return shell.Result{Stdout: "CLOSED\n"}, nil
		case "pr view 44 --repo acme/looper --json state --jq .state":
			return shell.Result{Stdout: "MERGED\n"}, nil
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.ClosePullRequest(context.Background(), ClosePullRequestInput{Repo: "acme/looper", PRNumber: 42}); err != nil {
		t.Fatalf("ClosePullRequest(open) error = %v", err)
	}
	if err := gateway.ClosePullRequest(context.Background(), ClosePullRequestInput{Repo: "acme/looper", PRNumber: 43}); err != nil {
		t.Fatalf("ClosePullRequest(closed) error = %v", err)
	}
	if err := gateway.ClosePullRequest(context.Background(), ClosePullRequestInput{Repo: "acme/looper", PRNumber: 44}); err != nil {
		t.Fatalf("ClosePullRequest(merged) error = %v", err)
	}
	log := strings.Join(runner.calls, "\n")
	if !strings.Contains(log, "pr close 42 --repo acme/looper") {
		t.Fatalf("gh log missing close pr command\n%s", log)
	}
	if strings.Contains(log, "pr close 43 --repo acme/looper") {
		t.Fatalf("gh log unexpectedly closed an already-closed pr\n%s", log)
	}
	if strings.Contains(log, "pr close 44 --repo acme/looper") {
		t.Fatalf("gh log unexpectedly closed an already-merged pr\n%s", log)
	}
	if strings.Contains(log, "--delete-branch") {
		t.Fatalf("gh log unexpectedly included destructive flags\n%s", log)
	}
}

func TestGatewayEnableAutoMerge(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "pr merge 42 --repo acme/looper --auto --squash --match-head-commit abc123" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		return shell.Result{}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if err := gateway.EnableAutoMerge(context.Background(), EnableAutoMergeInput{Repo: "acme/looper", PRNumber: 42, Strategy: config.ReviewerAutoMergeStrategySquash, HeadSHA: "abc123"}); err != nil {
		t.Fatalf("EnableAutoMerge() error = %v", err)
	}
}

func TestGatewayEnableAutoMergeRequiresHeadSHA(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		t.Fatalf("unexpected gh args: %v", options.Args)
		return shell.Result{}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.EnableAutoMerge(context.Background(), EnableAutoMergeInput{Repo: "acme/looper", PRNumber: 42, Strategy: config.ReviewerAutoMergeStrategySquash})
	if err == nil || err.Error() != "auto-merge head SHA is required" {
		t.Fatalf("EnableAutoMerge() error = %v, want missing head SHA error", err)
	}
}

func TestGatewayCloseIssueRejectsUnknownStateReason(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		t.Fatalf("unexpected gh call: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.CloseIssue(context.Background(), CloseIssueInput{Repo: "acme/looper", IssueNumber: 8, StateReason: "bogus"})
	if err == nil || !strings.Contains(err.Error(), "invalid issue close reason") {
		t.Fatalf("CloseIssue() error = %v, want invalid reason error", err)
	}
}

func TestListOpenPullRequestsPassesAllLabelsToGH(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "pr list --repo acme/looper --state open --limit 30 --label bug --label priority --json number,title,url,state,updatedAt,isDraft,reviewDecision,labels,headRefName,baseRefName,headRefOid,baseRefOid,author,reviewRequests,reviews,mergeStateStatus" {
			t.Fatalf("gh args = %q, want repeated label filters", args)
		}
		return shell.Result{Stdout: `[]`}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if _, err := gateway.ListOpenPullRequests(context.Background(), ListOpenPullRequestsInput{Repo: "acme/looper", Labels: []string{"bug", "priority"}}); err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
}

func TestListOpenPullRequestsPassesBaseRefToGH(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "pr list --repo acme/looper --state open --limit 30 --base main --json number,title,url,state,updatedAt,isDraft,reviewDecision,labels,headRefName,baseRefName,headRefOid,baseRefOid,author,reviewRequests,reviews,mergeStateStatus" {
			t.Fatalf("gh args = %q, want base filter", args)
		}
		return shell.Result{Stdout: `[]`}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if _, err := gateway.ListOpenPullRequests(context.Background(), ListOpenPullRequestsInput{Repo: "acme/looper", BaseRefName: "main"}); err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
}

func TestListOpenIssuesPassesAllLabelsToGH(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "issue list --repo acme/looper --state open --limit 30 --assignee reviewer --label bug --label priority --json number,title,body,url,state,updatedAt,author,assignees,labels" {
			t.Fatalf("gh args = %q, want repeated label filters", args)
		}
		return shell.Result{Stdout: `[]`}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if _, err := gateway.ListOpenIssues(context.Background(), ListOpenIssuesInput{Repo: "acme/looper", Assignee: "reviewer", Labels: []string{"bug", "priority"}}); err != nil {
		t.Fatalf("ListOpenIssues() error = %v", err)
	}
}

func TestGatewayListsReviewRequestedPullRequestsThroughSearch(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if !strings.Contains(args, "api graphql") {
			t.Fatalf("gh args = %q, want graphql search", args)
		}
		if !strings.Contains(args, "repo:acme/looper is:pr is:open review-requested:reviewer") {
			t.Fatalf("gh args = %q, want review-requested search qualifier", args)
		}
		if !strings.Contains(args, "-F first=50") {
			t.Fatalf("gh args = %q, want caller limit", args)
		}
		return shell.Result{Stdout: `{"data":{"search":{"nodes":[{"number":77,"title":"Review me","url":"https://example.test/pull/77","state":"OPEN","updatedAt":"2026-06-04T10:00:00Z","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","labels":{"nodes":[{"name":"ready"}]},"headRefName":"feature","baseRefName":"main","headRefOid":"head77","baseRefOid":"base77","mergeStateStatus":"CLEAN","author":{"login":"contributor"},"reviews":{"nodes":[{"state":"COMMENTED","author":{"login":"reviewer"},"submittedAt":"2026-06-04T10:01:00Z"}]}}]}}}`}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	prs, err := gateway.ListReviewRequestedPullRequests(context.Background(), ListReviewRequestedPullRequestsInput{Repo: "acme/looper", Reviewer: "Reviewer", Limit: 50})
	if err != nil {
		t.Fatalf("ListReviewRequestedPullRequests() error = %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("PR count = %d, want 1", len(prs))
	}
	pr := prs[0]
	if pr.Number != 77 || pr.HeadSHA != "head77" || pr.Author != "contributor" || !slices.Contains(pr.Labels, "ready") {
		t.Fatalf("PR = %#v, want parsed search result", pr)
	}
	if !slices.Contains(pr.ReviewRequests, "reviewer") || len(pr.Reviews) != 1 {
		t.Fatalf("PR review metadata = %#v / %#v, want synthetic reviewer and parsed reviews", pr.ReviewRequests, pr.Reviews)
	}
}

type fakeGHRunner struct {
	t       *testing.T
	calls   []string
	stdin   string
	respond func(options shell.Options) (shell.Result, error)
}

func (f *fakeGHRunner) run(_ context.Context, options shell.Options) (shell.Result, error) {
	f.t.Helper()
	args := strings.Join(options.Args, " ")
	f.calls = append(f.calls, args)
	if f.respond == nil {
		f.t.Fatalf("fakeGHRunner missing responder for args: %q", args)
	}
	return f.respond(options)
}

func TestSubmitReviewRejectsCleanMarkerWithComments(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		t.Fatalf("unexpected gh call: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}
	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "LGTM\n<!-- looper:review id=abc head=def outcome=clean -->", Comments: []ReviewComment{{Body: "But fix this", Path: "app.go", Line: 1, Side: "RIGHT"}}})
	if err == nil || !strings.Contains(err.Error(), "clean review marker cannot be submitted with review comments") {
		t.Fatalf("SubmitReview() error = %v, want clean-with-comments rejection", err)
	}
}

func TestSubmitReviewLogsValidationFailureDiagnostics(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		t.Fatalf("unexpected gh call: %q", strings.Join(options.Args, " "))
		return shell.Result{}, nil
	}
	var events []reviewSubmitDiagnosticEvent
	gateway := New(Options{
		GHPath: "gh",
		CWD:    t.TempDir(),
		GHRun:  runner.run,
		ReviewSubmitDiagnostic: func(event string, fields map[string]any) {
			events = append(events, reviewSubmitDiagnosticEvent{Name: event, Fields: fields})
		},
	})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "LGTM\n<!-- looper:review id=abc head=def outcome=clean -->", CommitID: "def", Comments: []ReviewComment{{Body: "But fix this", Path: "app.go", Line: 1, Side: "RIGHT"}}})
	if err == nil || !strings.Contains(err.Error(), "clean review marker cannot be submitted with review comments") {
		t.Fatalf("SubmitReview() error = %v, want clean-with-comments rejection", err)
	}
	if len(events) != 1 {
		t.Fatalf("diagnostic events = %#v, want one validation failure event", events)
	}
	if events[0].Name != "github_review_submit_validation_failed" {
		t.Fatalf("event name = %q, want github_review_submit_validation_failed", events[0].Name)
	}
	if got := events[0].Fields["error"]; got != "clean review marker cannot be submitted with review comments" {
		t.Fatalf("error = %#v, want clean marker rejection", got)
	}
	request, _ := events[0].Fields["request"].(map[string]any)
	if request["repo"] != "acme/looper" || request["pr_number"] != int64(42) || request["commit_id"] != "def" {
		t.Fatalf("request summary = %#v, want repo/pr/commit", request)
	}
	payload, _ := request["payload"].(map[string]any)
	marker, _ := payload["body_marker"].(map[string]any)
	if marker["id"] != "abc" || marker["head"] != "def" || marker["outcome"] != "clean" {
		t.Fatalf("body marker = %#v, want marker fields", marker)
	}
	comments, _ := payload["comments"].([]map[string]any)
	if len(comments) != 1 || comments[0]["path"] != "app.go" || comments[0]["line"] != int64(1) || comments[0]["side"] != "RIGHT" {
		t.Fatalf("payload comments = %#v, want inline anchor summary", comments)
	}
}

func TestSubmitReviewLogsGhFailureDiagnostics(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input - --include" {
			t.Fatalf("unexpected gh args: %q", args)
		}
		result := shell.Result{
			ExitCode: 1,
			Stdout: strings.Join([]string{
				"HTTP/2.0 422 Unprocessable Entity",
				"X-GitHub-Request-Id: req-123",
				"X-RateLimit-Remaining: 4999",
				"",
				`{"message":"Unprocessable Entity","errors":["An internal error occurred, please try again."]}`,
			}, "\n"),
			Stderr: "gh: Unprocessable Entity (HTTP 422)",
		}
		return result, &shell.CommandExecutionError{Message: "Command exited with code 1: gh: Unprocessable Entity (HTTP 422)", Result: result}
	}
	anchors := diffanchor.Parse("diff --git a/app.go b/app.go\n@@ -1 +1 @@\n+new\n")
	var events []reviewSubmitDiagnosticEvent
	gateway := New(Options{
		GHPath: "gh",
		CWD:    t.TempDir(),
		GHRun:  runner.run,
		ReviewSubmitDiagnostic: func(event string, fields map[string]any) {
			events = append(events, reviewSubmitDiagnosticEvent{Name: event, Fields: fields})
		},
	})
	err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "REQUEST_CHANGES", Body: "Please fix this.\n<!-- looper:review id=abc head=def outcome=blocking -->", CommitID: "def", Comments: []ReviewComment{{Body: "Fix this anchor", Path: "app.go", Line: 1, Side: "RIGHT"}}, Anchors: &anchors})
	if err == nil || !strings.Contains(err.Error(), "HTTP 422") {
		t.Fatalf("SubmitReview() error = %v, want HTTP 422 failure", err)
	}
	if len(events) < 2 {
		t.Fatalf("diagnostic events = %#v, want prepare and failure events", events)
	}
	failure := events[len(events)-1]
	if failure.Name != "github_review_submit_failed" {
		t.Fatalf("failure event name = %q, want github_review_submit_failed", failure.Name)
	}
	if failure.Fields["http_status"] != 422 {
		t.Fatalf("http_status = %#v, want 422", failure.Fields["http_status"])
	}
	if failure.Fields["response_body"] != `{"message":"Unprocessable Entity","errors":["An internal error occurred, please try again."]}` {
		t.Fatalf("response_body = %#v, want raw response body", failure.Fields["response_body"])
	}
	if failure.Fields["gh_stderr"] != "gh: Unprocessable Entity (HTTP 422)" {
		t.Fatalf("gh_stderr = %#v, want gh stderr", failure.Fields["gh_stderr"])
	}
	headers, _ := failure.Fields["response_headers"].(map[string]any)
	if headers["x_github_request_id"] != "req-123" || headers["x_ratelimit_remaining"] != "4999" {
		t.Fatalf("response_headers = %#v, want selected headers", headers)
	}
	request, _ := failure.Fields["request"].(map[string]any)
	if request["event"] != "REQUEST_CHANGES" {
		t.Fatalf("request = %#v, want request event", request)
	}
	normalization, _ := request["comment_processing"].(map[string]any)
	if normalization["original_count"] != 1 || normalization["submitted_count"] != 1 {
		t.Fatalf("comment_processing = %#v, want counts", normalization)
	}
}

type reviewSubmitDiagnosticEvent struct {
	Name   string
	Fields map[string]any
}
