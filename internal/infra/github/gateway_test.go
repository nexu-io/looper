package github

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/powerformer/looper/internal/diffanchor"
	"github.com/powerformer/looper/internal/infra/shell"
)

func TestGatewayListsSnapshotsAndReviewsThroughGH(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case args == "api repos/acme/looper/pulls/42/reviews --method POST --input -":
			runner.stdin = options.Stdin
			return shell.Result{Stdout: "{}"}, nil
		case strings.HasPrefix(args, "pr list"):
			return shell.Result{Stdout: `[{"number":42,"title":"Review me","url":"https://example.test/pull/42","state":"OPEN","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","author":{"login":"octocat"},"reviewRequests":[{"__typename":"User","login":"OctoCat"},{"__typename":"Team","slug":"platform"}]}]`}, nil
		case strings.HasPrefix(args, "issue list"):
			return shell.Result{Stdout: `[{"number":8,"title":"Fix gateway","body":"Issue body","url":"https://example.test/issues/8","state":"OPEN","author":{"login":"octocat"},"assignees":[{"login":"reviewer"}],"labels":[{"name":"phase-1"},{"name":"gateway"}]}]`}, nil
		case args == "api repos/acme/looper/issues/8":
			return shell.Result{Stdout: `{"number":8,"title":"Fix gateway","body":"Issue body","html_url":"https://example.test/issues/8","state":"open","user":{"login":"octocat"},"assignees":[{"login":"reviewer"}],"labels":[{"name":"phase-1"},{"name":"gateway"}]}`}, nil
		case args == "api repos/acme/looper/issues/8/comments --method POST -f body=Looper started":
			return shell.Result{Stdout: `{"id":91,"html_url":"https://example.test/issues/8#issuecomment-91"}`}, nil
		case args == "api repos/acme/looper/issues/comments/91 --method PATCH -f body=Looper finished":
			return shell.Result{Stdout: "{}"}, nil
		case args == "api repos/acme/looper/issues/8/assignees --method POST -f assignees[]=reviewer":
			return shell.Result{Stdout: "{}"}, nil
		case strings.HasPrefix(args, "pr view"):
			return shell.Result{Stdout: `{"number":42,"title":"Review me","body":"Body","url":"https://example.test/pull/42","state":"OPEN","isDraft":false,"reviewDecision":"CHANGES_REQUESTED","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","baseRefOid":"def456","mergeStateStatus":"DIRTY","author":{"login":"octocat"},"reviewRequests":[{"requestedReviewer":{"__typename":"User","login":"reviewer"}},{"requestedReviewer":{"__typename":"Team","slug":"platform"}}],"comments":[{"state":"UNRESOLVED"}],"reviews":[{"state":"COMMENTED"}],"statusCheckRollup":[{"conclusion":"SUCCESS"}]}`}, nil
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
	if got := issues[0].Assignees; len(got) != 1 || got[0] != "reviewer" {
		t.Fatalf("issues[0].Assignees = %#v, want [reviewer]", got)
	}
	if got := issues[0].Labels; len(got) != 2 || got[0] != "phase-1" || got[1] != "gateway" {
		t.Fatalf("issues[0].Labels = %#v, want [phase-1 gateway]", got)
	}
	if issueDetail.Number != 8 {
		t.Fatalf("issueDetail.Number = %d, want 8", issueDetail.Number)
	}
	if issueDetail.State != "open" || issueDetail.IsPullRequest {
		t.Fatalf("issueDetail = %#v, want open issue not pull request", issueDetail)
	}
	if comment.ID != 91 || comment.URL != "https://example.test/issues/8#issuecomment-91" {
		t.Fatalf("comment = %#v, want parsed issue comment metadata", comment)
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
	if !detail.HasConflicts {
		t.Fatal("detail.HasConflicts = false, want true")
	}
	if len(detail.Comments) != 1 || detail.Comments[0]["id"] != "comment-1" || detail.Comments[0]["threadId"] != "thread-1" || detail.Comments[0]["state"] != "UNRESOLVED" || detail.Comments[0]["body"] != "Fix this" {
		t.Fatalf("detail.Comments = %#v, want normalized review thread", detail.Comments)
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
		"api repos/acme/looper/pulls/42/reviews --method POST --input -",
		"pr comment 42 --repo acme/looper --body High-level follow-up",
		"api repos/acme/looper/issues/42/reactions --method POST -H Accept: application/vnd.github+json -f content=eyes",
		"api --paginate --slurp repos/acme/looper/issues/42/reactions -H Accept: application/vnd.github+json",
		"api repos/acme/looper/issues/42/reactions/7 --method DELETE -H Accept: application/vnd.github+json",
		"pr list --repo acme/looper --state open --limit 30 --label phase-1",
		"issue list --repo acme/looper --state open --limit 30 --assignee reviewer --label phase-1",
		"api repos/acme/looper/issues/8",
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
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input -" {
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
		if args != "api repos/acme/looper/pulls/42/reviews --method POST --input -" {
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
			return shell.Result{Stdout: `[{"state":"CHANGES_REQUESTED","body":"<!-- looper:review id=abc head=def outcome=actionable -->"}]`}, nil
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

func TestListOpenPullRequestsPassesAllLabelsToGH(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "pr list --repo acme/looper --state open --limit 30 --label bug --label priority --json number,title,url,state,isDraft,reviewDecision,labels,headRefName,baseRefName,headRefOid,author,reviewRequests" {
			t.Fatalf("gh args = %q, want repeated label filters", args)
		}
		return shell.Result{Stdout: `[]`}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if _, err := gateway.ListOpenPullRequests(context.Background(), ListOpenPullRequestsInput{Repo: "acme/looper", Labels: []string{"bug", "priority"}}); err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
}

func TestListOpenIssuesPassesAllLabelsToGH(t *testing.T) {
	t.Parallel()
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if args != "issue list --repo acme/looper --state open --limit 30 --assignee reviewer --label bug --label priority --json number,title,body,url,state,author,assignees,labels" {
			t.Fatalf("gh args = %q, want repeated label filters", args)
		}
		return shell.Result{Stdout: `[]`}, nil
	}

	gateway := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run})
	if _, err := gateway.ListOpenIssues(context.Background(), ListOpenIssuesInput{Repo: "acme/looper", Assignee: "reviewer", Labels: []string{"bug", "priority"}}); err != nil {
		t.Fatalf("ListOpenIssues() error = %v", err)
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
