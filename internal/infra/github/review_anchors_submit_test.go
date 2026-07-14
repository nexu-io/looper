package github

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/infra/shell"
)

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
