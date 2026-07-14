package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/diffanchor"
	"github.com/nexu-io/looper/internal/infra/shell"
)

func TestBuildReviewAnchorIndexUsesLocalPathDiffWhenRemoteIsTruncated(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, targetLine := seedLargePRRepo(t, repo)
	gateway := New(Options{GHPath: "gh", GitPath: "git", CWD: repo})

	remoteCalls := 0
	index, source, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     repo,
		BaseSHA: baseSHA,
		HeadSHA: headSHA,
		Paths:   []string{"target/late.go"},
		RemoteDiff: func(context.Context) (string, error) {
			remoteCalls++
			return "", ErrLocalCaptureTruncated
		},
	})
	if err != nil {
		t.Fatalf("BuildReviewAnchorIndex() error = %v", err)
	}
	if source != ReviewAnchorAuthorityLocalPathDiff {
		t.Fatalf("source = %q, want %q", source, ReviewAnchorAuthorityLocalPathDiff)
	}
	if remoteCalls != 0 {
		t.Fatalf("remote fallback calls = %d, want 0 when local path authority succeeds", remoteCalls)
	}
	if index == nil || !index.Validate(diffanchor.Anchor{Path: "target/late.go", Line: targetLine, Side: diffanchor.SideRight}).Valid {
		t.Fatalf("index did not validate target RIGHT line %d: %#v", targetLine, index)
	}
}

func TestBuildReviewAnchorIndexValidatesLeftDeletedLine(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, deletedLine := seedDeletedLineRepo(t, repo)
	gateway := New(Options{GHPath: "gh", GitPath: "git", CWD: repo})

	index, source, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     repo,
		BaseSHA: baseSHA,
		HeadSHA: headSHA,
		Paths:   []string{"removed.go"},
	})
	if err != nil {
		t.Fatalf("BuildReviewAnchorIndex() error = %v", err)
	}
	if source != ReviewAnchorAuthorityLocalPathDiff {
		t.Fatalf("source = %q, want local_path_diff", source)
	}
	if index == nil || !index.Validate(diffanchor.Anchor{Path: "removed.go", Line: deletedLine, Side: diffanchor.SideLeft}).Valid {
		t.Fatalf("LEFT deleted line %d should be valid: %#v", deletedLine, index)
	}
}

func TestBuildReviewAnchorIndexRejectsLocalBaseHeadMismatch(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, _ := seedLargePRRepo(t, repo)
	gateway := New(Options{GHPath: "gh", GitPath: "git", CWD: repo})

	_, _, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     repo,
		BaseSHA: baseSHA,
		HeadSHA: strings.Repeat("0", 40),
		Paths:   []string{"target/late.go"},
	})
	if err == nil || !errors.Is(err, ErrAnchorValidationUnavailable) && !errors.Is(err, ErrReviewBaseHeadMismatch) {
		t.Fatalf("error = %v, want base/head mismatch wrapped as unavailable", err)
	}
	_ = headSHA
}

func TestBuildReviewAnchorIndexFallsBackToCompleteRemoteDiff(t *testing.T) {
	t.Parallel()

	gateway := New(Options{
		GHPath:  "gh",
		GitPath: "git",
		GitRun: func(context.Context, shell.Options) (shell.Result, error) {
			return shell.Result{ExitCode: 1, Stderr: "not a git repository"}, &shell.CommandExecutionError{Message: "not a git repository"}
		},
	})
	remoteDiff := "diff --git a/app.go b/app.go\n@@ -1 +1 @@\n-old\n+new\n"
	index, source, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     t.TempDir(),
		BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Paths:   []string{"app.go"},
		RemoteDiff: func(context.Context) (string, error) {
			return remoteDiff, nil
		},
	})
	if err != nil {
		t.Fatalf("BuildReviewAnchorIndex() error = %v", err)
	}
	if source != ReviewAnchorAuthorityRemotePRDiff {
		t.Fatalf("source = %q, want remote_pr_diff", source)
	}
	if index == nil || !index.Validate(diffanchor.Anchor{Path: "app.go", Line: 1, Side: diffanchor.SideRight}).Valid {
		t.Fatalf("remote fallback index invalid: %#v", index)
	}
}

func TestBuildReviewAnchorIndexDoesNotParseTruncatedRemoteDiff(t *testing.T) {
	t.Parallel()

	gateway := New(Options{
		GHPath:  "gh",
		GitPath: "git",
		GitRun: func(context.Context, shell.Options) (shell.Result, error) {
			return shell.Result{ExitCode: 1, Stderr: "missing objects"}, &shell.CommandExecutionError{Message: "missing objects"}
		},
	})
	_, _, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     t.TempDir(),
		BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Paths:   []string{"app.go"},
		RemoteDiff: func(context.Context) (string, error) {
			return strings.Repeat("x", 100), ErrLocalCaptureTruncated
		},
	})
	if err == nil || !errors.Is(err, ErrAnchorValidationUnavailable) {
		t.Fatalf("error = %v, want ErrAnchorValidationUnavailable", err)
	}
	if !strings.Contains(err.Error(), DiffTruncationReasonLocalCapture) {
		t.Fatalf("error = %v, want local_capture_truncated diagnostic", err)
	}
}

func TestBuildReviewAnchorIndexSurfacesGitHubOversizedSeparately(t *testing.T) {
	t.Parallel()

	gateway := New(Options{
		GHPath:  "gh",
		GitPath: "git",
		GitRun: func(context.Context, shell.Options) (shell.Result, error) {
			return shell.Result{ExitCode: 1, Stderr: "missing objects"}, &shell.CommandExecutionError{Message: "missing objects"}
		},
	})
	_, _, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     t.TempDir(),
		BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Paths:   []string{"app.go"},
		RemoteDiff: func(context.Context) (string, error) {
			return "", ErrDiffTooLarge
		},
	})
	if err == nil || !errors.Is(err, ErrAnchorValidationUnavailable) {
		t.Fatalf("error = %v, want ErrAnchorValidationUnavailable", err)
	}
	if !strings.Contains(err.Error(), DiffTruncationReasonGitHubTooLarge) {
		t.Fatalf("error = %v, want github_diff_too_large diagnostic", err)
	}
}

func TestSubmitReviewKeepsValidInlineCommentAgainstPathTargetedAuthority(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, targetLine := seedLargePRRepo(t, repo)
	gateway := New(Options{GHPath: "gh", GitPath: "git", CWD: repo})
	index, _, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD: repo, BaseSHA: baseSHA, HeadSHA: headSHA, Paths: []string{"target/late.go"},
	})
	if err != nil {
		t.Fatalf("BuildReviewAnchorIndex() error = %v", err)
	}

	var submittedPayload map[string]any
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if strings.HasPrefix(args, "api repos/acme/looper/pulls/42/reviews") {
			if err := json.Unmarshal([]byte(options.Stdin), &submittedPayload); err != nil {
				t.Fatalf("decode review payload: %v", err)
			}
			return shell.Result{Stdout: "HTTP/1.1 200 OK\r\n\r\n{}"}, nil
		}
		t.Fatalf("unexpected gh args: %q", args)
		return shell.Result{}, nil
	}
	submitGateway := New(Options{GHPath: "gh", CWD: repo, GHRun: runner.run})
	body := "Actionable findings\n<!-- looper:review id=review-1 head=" + headSHA + " outcome=actionable -->"
	err = submitGateway.SubmitReview(context.Background(), SubmitReviewInput{
		Repo:     "acme/looper",
		PRNumber: 42,
		Event:    "COMMENT",
		Body:     body,
		CommitID: headSHA,
		Comments: []ReviewComment{{
			Body: "fix the late change",
			Path: "target/late.go",
			Line: targetLine,
			Side: "RIGHT",
		}},
		Anchors: index,
		CWD:     repo,
	})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	comments, _ := submittedPayload["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("submitted comments = %#v, want original inline comment preserved", submittedPayload)
	}
	comment, _ := comments[0].(map[string]any)
	if comment["path"] != "target/late.go" || int64(comment["line"].(float64)) != targetLine || comment["side"] != "RIGHT" {
		t.Fatalf("submitted comment = %#v, want target/late.go RIGHT %d", comment, targetLine)
	}
	// GitHub creates a resolvable review thread for each comments[] entry.
	if submittedPayload["event"] != "COMMENT" {
		t.Fatalf("event = %#v, want COMMENT", submittedPayload["event"])
	}
}

func TestSubmitReviewDowngradesOnlyAfterCompleteAuthorityInvalidates(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, targetLine := seedLargePRRepo(t, repo)
	gateway := New(Options{GHPath: "gh", GitPath: "git", CWD: repo})
	index, _, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD: repo, BaseSHA: baseSHA, HeadSHA: headSHA, Paths: []string{"target/late.go"},
	})
	if err != nil {
		t.Fatalf("BuildReviewAnchorIndex() error = %v", err)
	}

	var submittedPayload map[string]any
	var processing map[string]any
	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if strings.HasPrefix(args, "api repos/acme/looper/pulls/42/reviews") {
			if err := json.Unmarshal([]byte(options.Stdin), &submittedPayload); err != nil {
				t.Fatalf("decode review payload: %v", err)
			}
			return shell.Result{Stdout: "HTTP/1.1 200 OK\r\n\r\n{}"}, nil
		}
		t.Fatalf("unexpected gh args: %q", args)
		return shell.Result{}, nil
	}
	var events []reviewSubmitDiagnosticEvent
	submitGateway := New(Options{
		GHPath: "gh",
		CWD:    repo,
		GHRun:  runner.run,
		ReviewSubmitDiagnostic: func(event string, fields map[string]any) {
			events = append(events, reviewSubmitDiagnosticEvent{Name: event, Fields: fields})
			if event == "github_review_submit_prepared" {
				request, _ := fields["request"].(map[string]any)
				processing, _ = request["comment_processing"].(map[string]any)
			}
		},
	})
	body := "Actionable findings\n<!-- looper:review id=review-1 head=" + headSHA + " outcome=actionable -->"
	err = submitGateway.SubmitReview(context.Background(), SubmitReviewInput{
		Repo:     "acme/looper",
		PRNumber: 42,
		Event:    "COMMENT",
		Body:     body,
		CommitID: headSHA,
		Comments: []ReviewComment{
			{Body: "valid late line", Path: "target/late.go", Line: targetLine, Side: "RIGHT"},
			{Body: "invalid line", Path: "target/late.go", Line: 999999, Side: "RIGHT"},
		},
		Anchors: index,
		CWD:     repo,
	})
	if err != nil {
		t.Fatalf("SubmitReview() error = %v", err)
	}
	comments, _ := submittedPayload["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("submitted comments = %#v, want only valid inline kept", submittedPayload)
	}
	if processing["original_count"] != 2 || processing["submitted_count"] != 1 || processing["downgraded_count"] != 1 {
		t.Fatalf("comment_processing = %#v, want one kept one downgraded", processing)
	}
	foundDowngraded := false
	switch entries := processing["comments"].(type) {
	case []map[string]any:
		for _, row := range entries {
			if row["action"] == "downgraded" {
				foundDowngraded = true
				if row["reason"] != AnchorOutsideCompleteDiffReason {
					t.Fatalf("downgrade reason = %#v, want %q", row["reason"], AnchorOutsideCompleteDiffReason)
				}
			}
		}
	case []any:
		for _, entry := range entries {
			row, _ := entry.(map[string]any)
			if row["action"] == "downgraded" {
				foundDowngraded = true
				if row["reason"] != AnchorOutsideCompleteDiffReason {
					t.Fatalf("downgrade reason = %#v, want %q", row["reason"], AnchorOutsideCompleteDiffReason)
				}
			}
		}
	default:
		t.Fatalf("processing comments type = %T value=%#v", processing["comments"], processing["comments"])
	}
	if !foundDowngraded {
		t.Fatalf("processing comments = %#v, want downgraded entry", processing["comments"])
	}
	bodyText, _ := submittedPayload["body"].(string)
	if !strings.Contains(bodyText, "invalid line") || !strings.Contains(bodyText, "Location: target/late.go RIGHT line 999999") {
		t.Fatalf("body = %q, want downgraded invalid anchor text", bodyText)
	}
	_ = events
}

func TestSubmitReviewDoesNotPublishWhenAnchorAuthorityUnavailable(t *testing.T) {
	t.Parallel()

	// This covers the orchestration contract for unavailable authority: callers
	// must fail closed before SubmitReview when BuildReviewAnchorIndex fails.
	gateway := New(Options{
		GHPath:  "gh",
		GitPath: "git",
		GitRun: func(context.Context, shell.Options) (shell.Result, error) {
			return shell.Result{ExitCode: 1, Stderr: "missing objects"}, &shell.CommandExecutionError{Message: "missing objects"}
		},
	})
	_, _, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:     t.TempDir(),
		BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Paths:   []string{"app.go"},
		RemoteDiff: func(context.Context) (string, error) {
			return "", ErrLocalCaptureTruncated
		},
	})
	if err == nil || !errors.Is(err, ErrAnchorValidationUnavailable) {
		t.Fatalf("error = %v, want unavailable", err)
	}
}

func seedLargePRRepo(t *testing.T, repo string) (baseSHA, headSHA string, targetLine int64) {
	t.Helper()
	runGitRepo(t, repo, "init")
	runGitRepo(t, repo, "config", "user.email", "test@example.com")
	runGitRepo(t, repo, "config", "user.name", "Test")

	// Create enough base content that a whole-repo diff exceeds 256 KiB, with
	// the actionable target path occurring after the generic capture limit.
	for i := 0; i < 40; i++ {
		dir := filepath.Join(repo, fmt.Sprintf("pad/%02d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir pad: %v", err)
		}
		// ~8 KiB per file => ~320 KiB across pads before the target file.
		content := strings.Repeat(fmt.Sprintf("pad-%02d-line\n", i), 512)
		if err := os.WriteFile(filepath.Join(dir, "blob.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("write pad file: %v", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, "target"), 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "target/late.go"), []byte("package target\n\nfunc Existing() {}\n"), 0o644); err != nil {
		t.Fatalf("write target base: %v", err)
	}
	runGitRepo(t, repo, "add", ".")
	runGitRepo(t, repo, "commit", "-m", "base")
	baseSHA = strings.TrimSpace(runGitRepoOutput(t, repo, "rev-parse", "HEAD"))

	// Mutate pads and target so the full diff is large and the target change is late.
	for i := 0; i < 40; i++ {
		path := filepath.Join(repo, fmt.Sprintf("pad/%02d/blob.txt", i))
		content := strings.Repeat(fmt.Sprintf("pad-%02d-changed\n", i), 512)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("rewrite pad file: %v", err)
		}
	}
	targetBody := "package target\n\nfunc Existing() {}\n\nfunc subscription_checkout_offer_stale() {}\n"
	if err := os.WriteFile(filepath.Join(repo, "target/late.go"), []byte(targetBody), 0o644); err != nil {
		t.Fatalf("write target head: %v", err)
	}
	runGitRepo(t, repo, "add", ".")
	runGitRepo(t, repo, "commit", "-m", "head changes")
	headSHA = strings.TrimSpace(runGitRepoOutput(t, repo, "rev-parse", "HEAD"))

	// Prove the whole-repo three-dot diff exceeds the generic shell capture limit.
	fullDiff := runGitRepoOutput(t, repo, "diff", "--no-ext-diff", "--no-color", baseSHA+"..."+headSHA)
	if len(fullDiff) <= 256*1024 {
		t.Fatalf("full synthetic diff size = %d, want > 256KiB for regression", len(fullDiff))
	}
	// Target file should appear after 256 KiB in lexical whole-diff order (pad/* then target/).
	idx := strings.Index(fullDiff, "diff --git a/target/late.go")
	if idx < 0 || idx < 256*1024 {
		t.Fatalf("target file offset = %d, want after 256KiB in whole diff", idx)
	}

	// RIGHT-side line of the added function in the head file.
	targetLine = 5
	return baseSHA, headSHA, targetLine
}

func seedDeletedLineRepo(t *testing.T, repo string) (baseSHA, headSHA string, deletedLine int64) {
	t.Helper()
	runGitRepo(t, repo, "init")
	runGitRepo(t, repo, "config", "user.email", "test@example.com")
	runGitRepo(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "removed.go"), []byte("package removed\n\nfunc Keep() {}\nfunc DeleteMe() {}\nfunc Tail() {}\n"), 0o644); err != nil {
		t.Fatalf("write base removed.go: %v", err)
	}
	runGitRepo(t, repo, "add", ".")
	runGitRepo(t, repo, "commit", "-m", "base")
	baseSHA = strings.TrimSpace(runGitRepoOutput(t, repo, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repo, "removed.go"), []byte("package removed\n\nfunc Keep() {}\nfunc Tail() {}\n"), 0o644); err != nil {
		t.Fatalf("write head removed.go: %v", err)
	}
	runGitRepo(t, repo, "add", ".")
	runGitRepo(t, repo, "commit", "-m", "delete line")
	headSHA = strings.TrimSpace(runGitRepoOutput(t, repo, "rev-parse", "HEAD"))
	deletedLine = 4
	return baseSHA, headSHA, deletedLine
}

func runGitRepo(t *testing.T, repo string, args ...string) {
	t.Helper()
	_ = runGitRepoOutput(t, repo, args...)
}

func runGitRepoOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
