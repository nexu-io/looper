package github

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayListsSnapshotsAndReviewsThroughGH(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	logPath := filepath.Join(rootDir, "gh.log")
	stdinPath := filepath.Join(rootDir, "stdin.log")
	scriptPath := filepath.Join(rootDir, "gh")
	writeExecutable(t, scriptPath, `#!/bin/sh
printf '%s
' "$*" >> "`+logPath+`"
case "$*" in
  "api repos/acme/looper/pulls/42/reviews --method POST --input -")
    cat > "`+stdinPath+`"
    printf '{}'
    ;;
  "pr list"*)
    printf '[{"number":42,"title":"Review me","url":"https://example.test/pull/42","state":"OPEN","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","author":{"login":"octocat"},"reviewRequests":[{"__typename":"User","login":"OctoCat"},{"__typename":"Team","slug":"platform"}]}]'
    ;;
  "issue list"*)
    printf '[{"number":8,"title":"Fix gateway","body":"Issue body","url":"https://example.test/issues/8","state":"OPEN","author":{"login":"octocat"},"assignees":[{"login":"reviewer"}],"labels":[{"name":"phase-1"},{"name":"gateway"}]}]'
    ;;
  "issue view"*)
    printf '{"number":8,"title":"Fix gateway","body":"Issue body","url":"https://example.test/issues/8","state":"OPEN","author":{"login":"octocat"},"assignees":[{"login":"reviewer"}],"labels":[{"name":"phase-1"},{"name":"gateway"}]}'
    ;;
  "pr view"*)
    printf '{"number":42,"title":"Review me","body":"Body","url":"https://example.test/pull/42","state":"OPEN","isDraft":false,"reviewDecision":"CHANGES_REQUESTED","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","baseRefOid":"def456","mergeStateStatus":"DIRTY","author":{"login":"octocat"},"reviewRequests":[{"requestedReviewer":{"__typename":"User","login":"reviewer"}},{"requestedReviewer":{"__typename":"Team","slug":"platform"}}],"comments":[{"state":"UNRESOLVED"}],"reviews":[{"state":"COMMENTED"}],"statusCheckRollup":[{"conclusion":"SUCCESS"}]}'
    ;;
  "pr diff"*)
    printf 'diff --git a/a.ts b/a.ts
'
    ;;
  "api user"*)
    printf 'reviewer
'
    ;;
  *"resolveReviewThread"*)
    printf '{"data":{"resolveReviewThread":{"thread":{"id":"thread-1","isResolved":true}}}}'
    ;;
  *"reviewThreads"*)
    printf '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"thread-1","isResolved":false,"comments":{"nodes":[{"id":"comment-1","body":"Fix this"}]}}]}}}}}'
    ;;
  *"threadId=thread-1"*)
    printf '{"data":{"node":{"id":"thread-1","isResolved":false}}}'
    ;;
  "label create"*)
    printf '{}'
    ;;
  "api repos/acme/looper/issues/42/labels --method POST -f labels[]=phase-1 -f labels[]=ready")
    printf '{}'
    ;;
  "api repos/acme/looper/issues/42/labels/needs-work --method DELETE")
    printf '{}'
    ;;
  "api repos/acme/looper/pulls/42/requested_reviewers --method POST -f reviewers[]=reviewer")
    printf '{}'
    ;;
  "pr review 42 --repo acme/looper --comment --body Looks good")
    printf '{}'
    ;;
  "pr comment 42 --repo acme/looper --body High-level follow-up")
    printf '{}'
    ;;
  "api repos/acme/looper/issues/42/reactions --method POST -H Accept: application/vnd.github+json -f content=eyes")
    printf '{}'
    ;;
  "api repos/acme/looper/issues/42/reactions -H Accept: application/vnd.github+json")
    printf '[{"id":7,"content":"eyes","user":{"login":"reviewer"}}]'
    ;;
  "api repos/acme/looper/issues/42/reactions/7 --method DELETE -H Accept: application/vnd.github+json")
    printf '{}'
    ;;
  "pr create --repo acme/looper --head feature --base main --title Add support --body Body")
    printf 'https://example.test/pull/88
'
    ;;
esac
`)

	gateway := New(Options{GHPath: scriptPath, CWD: rootDir})
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
	snapshot, err := gateway.CapturePullRequestSnapshot(context.Background(), CapturePullRequestSnapshotInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("CapturePullRequestSnapshot() error = %v", err)
	}
	if err := gateway.SubmitReview(context.Background(), SubmitReviewInput{Repo: "acme/looper", PRNumber: 42, Event: "COMMENT", Body: "Looks good"}); err != nil {
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

	log := readFile(t, logPath)
	for _, needle := range []string{
		"pr review 42 --repo acme/looper --comment --body Looks good",
		"api repos/acme/looper/pulls/42/reviews --method POST --input -",
		"pr comment 42 --repo acme/looper --body High-level follow-up",
		"api repos/acme/looper/issues/42/reactions --method POST -H Accept: application/vnd.github+json -f content=eyes",
		"api repos/acme/looper/issues/42/reactions/7 --method DELETE -H Accept: application/vnd.github+json",
		"pr list --repo acme/looper --state open --limit 30 --label phase-1",
		"issue list --repo acme/looper --state open --limit 30 --assignee reviewer --label phase-1",
		"issue view 8 --repo acme/looper",
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
	stdin := readFile(t, stdinPath)
	for _, needle := range []string{"\"event\":\"COMMENT\"", "\"body\":\"Needs work\"", "\"commit_id\":\"abc123\"", "\"path\":\"src/a.ts\"", "\"line\":12", "\"side\":\"RIGHT\""} {
		if !strings.Contains(stdin, needle) {
			t.Fatalf("review stdin missing %q\n%s", needle, stdin)
		}
	}
}

func TestGatewayResolveReviewThreadReturnsNotFound(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	scriptPath := filepath.Join(rootDir, "gh")
	writeExecutable(t, scriptPath, `#!/bin/sh
args="$*"
if printf '%s' "$args" | grep -Fq 'threadId=thread-missing'; then
  printf '{"data":{"node":null}}'
else
  printf '{}'
fi
`)
	gateway := New(Options{GHPath: scriptPath, CWD: rootDir})
	err := gateway.ResolveReviewThread(context.Background(), ResolveReviewThreadInput{Repo: "acme/looper", ThreadID: "thread-missing"})
	if _, ok := err.(*ReviewThreadNotFoundError); !ok {
		t.Fatalf("ResolveReviewThread() error = %v, want *ReviewThreadNotFoundError", err)
	}
}

func TestGatewayIgnoresPlainPullRequestCommentsAsReviewThreads(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	scriptPath := filepath.Join(rootDir, "gh")
	writeExecutable(t, scriptPath, `#!/bin/sh
args="$*"
if printf '%s' "$args" | grep -Fq 'pr view'; then
  printf '{"number":42,"title":"Review me","body":"Body","url":"https://example.test/pull/42","state":"OPEN","isDraft":false,"reviewDecision":"REVIEW_REQUIRED","headRefName":"feature","baseRefName":"main","headRefOid":"abc123","baseRefOid":"def456","author":{"login":"octocat"},"reviewRequests":[],"comments":[{"id":"IC_comment","body":"@codex review"}],"reviews":[],"statusCheckRollup":[]}'
elif printf '%s' "$args" | grep -Fq 'reviewThreads'; then
  printf '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}'
else
  printf '{}'
fi
`)
	gateway := New(Options{GHPath: scriptPath, CWD: rootDir})
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
	rootDir := t.TempDir()
	scriptPath := filepath.Join(rootDir, "gh")
	writeExecutable(t, scriptPath, `#!/bin/sh
case "$*" in
  *"resolveReviewThread"*)
    printf 'permission denied' >&2
    exit 1
    ;;
  *"threadId=thread-1"*)
    printf '{"data":{"node":{"id":"thread-1","isResolved":false}}}'
    ;;
  *)
    printf '{}'
    ;;
esac
`)
	gateway := New(Options{GHPath: scriptPath, CWD: rootDir})
	err := gateway.ResolveReviewThread(context.Background(), ResolveReviewThreadInput{Repo: "acme/looper", ThreadID: "thread-1"})
	if err == nil || !strings.Contains(err.Error(), "Command exited with code 1") {
		t.Fatalf("ResolveReviewThread() error = %v, want command exit error", err)
	}
}

func TestGatewayIgnoresMissingLabelDeleteErrors(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	scriptPath := filepath.Join(rootDir, "gh")
	writeExecutable(t, scriptPath, `#!/bin/sh
args="$*"
if printf '%s' "$args" | grep -Fq 'api repos/acme/looper/issues/42/labels/looper%3Aspec-ready --method DELETE'; then
  printf 'gh: HTTP 404: label does not exist (https://api.github.com/...)' >&2
  exit 1
fi
printf '{}'
`)
	gateway := New(Options{GHPath: scriptPath, CWD: rootDir})
	if err := gateway.RemovePullRequestLabels(context.Background(), PullRequestLabelsInput{Repo: "acme/looper", PRNumber: 42, Labels: []string{"looper:spec-ready"}}); err != nil {
		t.Fatalf("RemovePullRequestLabels() error = %v, want nil", err)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	tempFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		t.Fatalf("os.CreateTemp(%s) error = %v", filepath.Dir(path), err)
	}
	tempPath := tempFile.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()
	if _, err := tempFile.WriteString(contents); err != nil {
		_ = tempFile.Close()
		t.Fatalf("tempFile.WriteString(%s) error = %v", tempPath, err)
	}
	if err := tempFile.Close(); err != nil {
		t.Fatalf("tempFile.Close(%s) error = %v", tempPath, err)
	}
	if err := os.Chmod(tempPath, 0o755); err != nil {
		t.Fatalf("os.Chmod(%s) error = %v", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		t.Fatalf("os.Rename(%s, %s) error = %v", tempPath, path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%s) error = %v", path, err)
	}
	return string(data)
}
