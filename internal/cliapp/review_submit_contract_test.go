package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Contract coverage for issue #557: review submit must keep valid inline comments
// when the full PR diff exceeds the generic shell capture limit by building
// path-targeted base/head authority from the prepared local checkout.
func TestReviewSubmitOrchestrationPreservesInlineCommentsWhenFullDiffExceedsCaptureLimit(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, targetLine := seedReviewSubmitLargeRepo(t, repo)
	payloadPath, configPath, submitLog, ghPath := writeReviewSubmitHarness(t, repo, baseSHA, headSHA, "truncated")

	payload := map[string]any{
		"body": "Actionable review\n<!-- looper:review id=review-large head=" + headSHA + " outcome=actionable -->",
		"comments": []map[string]any{
			{
				"body": "late change needs attention",
				"path": "target/late.go",
				"line": targetLine,
				"side": "RIGHT",
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := os.WriteFile(payloadPath, raw, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runtime := newCommandRuntime(New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		Getwd:  func() (string, error) { return repo, nil },
	}), []string{"--config", configPath})

	cmd := newReviewSubmitTestCommand(stdout, stderr)
	cmd.SetContext(context.Background())
	payloadFile, err := os.Open(payloadPath)
	if err != nil {
		t.Fatalf("open payload: %v", err)
	}
	defer payloadFile.Close()
	cmd.SetIn(payloadFile)
	if err := cmd.Flags().Set("event", "COMMENT"); err != nil {
		t.Fatalf("set event: %v", err)
	}
	if err := cmd.Flags().Set("commit-id", headSHA); err != nil {
		t.Fatalf("set commit-id: %v", err)
	}

	if err := runtime.reviewSubmit(cmd, []string{"acme/looper#42"}); err != nil {
		t.Fatalf("reviewSubmit() error = %v\nstderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"submitted"`) || !strings.Contains(stdout.String(), "true") {
		t.Fatalf("stdout = %q, want submitted true", stdout.String())
	}

	submitted := readLastReviewSubmitPayload(t, submitLog)
	comments, _ := submitted["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("outgoing GitHub review comments = %#v, want original inline comment", submitted)
	}
	comment, _ := comments[0].(map[string]any)
	if comment["path"] != "target/late.go" || int64(comment["line"].(float64)) != targetLine || comment["side"] != "RIGHT" {
		t.Fatalf("comment = %#v, want resolvable target/late.go RIGHT %d", comment, targetLine)
	}
	// A non-empty comments[] entry is the GitHub contract for a resolvable review thread.
	if submitted["commit_id"] != headSHA {
		t.Fatalf("commit_id = %#v, want %s", submitted["commit_id"], headSHA)
	}
	_ = ghPath
}

func TestReviewSubmitOrchestrationPreservesLeftDeletedInlineComment(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, deletedLine := seedReviewSubmitDeletedRepo(t, repo)
	payloadPath, configPath, submitLog, _ := writeReviewSubmitHarness(t, repo, baseSHA, headSHA, "truncated")

	payload := map[string]any{
		"body": "Actionable review\n<!-- looper:review id=review-left head=" + headSHA + " outcome=actionable -->",
		"comments": []map[string]any{
			{"body": "deleted line issue", "path": "removed.go", "line": deletedLine, "side": "LEFT"},
		},
	}
	raw, _ := json.Marshal(payload)
	if err := os.WriteFile(payloadPath, raw, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runtime := newCommandRuntime(New(Deps{Stdout: stdout, Stderr: stderr, Getwd: func() (string, error) { return repo, nil }}), []string{"--config", configPath})
	cmd := newReviewSubmitTestCommand(stdout, stderr)
	cmd.SetContext(context.Background())
	f, err := os.Open(payloadPath)
	if err != nil {
		t.Fatalf("open payload: %v", err)
	}
	defer f.Close()
	cmd.SetIn(f)
	_ = cmd.Flags().Set("event", "COMMENT")
	_ = cmd.Flags().Set("commit-id", headSHA)

	if err := runtime.reviewSubmit(cmd, []string{"acme/looper#42"}); err != nil {
		t.Fatalf("reviewSubmit() error = %v\nstderr=%s", err, stderr.String())
	}
	submitted := readLastReviewSubmitPayload(t, submitLog)
	comments, _ := submitted["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("comments = %#v, want LEFT deleted-line comment", submitted)
	}
	comment, _ := comments[0].(map[string]any)
	if comment["path"] != "removed.go" || int64(comment["line"].(float64)) != deletedLine || comment["side"] != "LEFT" {
		t.Fatalf("comment = %#v, want removed.go LEFT %d", comment, deletedLine)
	}
}
