package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestReviewSubmitOrchestrationFailsClosedOnBaseHeadMismatch(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, targetLine := seedReviewSubmitLargeRepo(t, repo)
	// Harness advertises a different head than the local object graph.
	payloadPath, configPath, submitLog, _ := writeReviewSubmitHarness(t, repo, baseSHA, strings.Repeat("a", 40), "truncated")

	payload := map[string]any{
		"body": "Actionable review\n<!-- looper:review id=review-mismatch head=" + strings.Repeat("a", 40) + " outcome=actionable -->",
		"comments": []map[string]any{
			{"body": "late change", "path": "target/late.go", "line": targetLine, "side": "RIGHT"},
		},
	}
	raw, _ := json.Marshal(payload)
	_ = os.WriteFile(payloadPath, raw, 0o644)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runtime := newCommandRuntime(New(Deps{Stdout: stdout, Stderr: stderr, Getwd: func() (string, error) { return repo, nil }}), []string{"--config", configPath})
	cmd := newReviewSubmitTestCommand(stdout, stderr)
	cmd.SetContext(context.Background())
	f, _ := os.Open(payloadPath)
	defer f.Close()
	cmd.SetIn(f)
	_ = cmd.Flags().Set("event", "COMMENT")
	_ = cmd.Flags().Set("commit-id", strings.Repeat("a", 40))

	err := runtime.reviewSubmit(cmd, []string{"acme/looper#42"})
	if err == nil {
		t.Fatal("reviewSubmit() error = nil, want base/head mismatch fail-closed")
	}
	if !strings.Contains(err.Error(), "anchor") && !strings.Contains(err.Error(), "base/head") && !strings.Contains(err.Error(), "not available locally") && !strings.Contains(err.Error(), "authority") {
		t.Fatalf("error = %v, want anchor authority / mismatch failure", err)
	}
	if _, statErr := os.Stat(submitLog); !os.IsNotExist(statErr) {
		data, _ := os.ReadFile(submitLog)
		if len(bytes.TrimSpace(data)) > 0 {
			t.Fatalf("review was published despite mismatch: %s", data)
		}
	}
	_ = headSHA
	_ = stderr
}

func TestReviewSubmitOrchestrationFailsClosedWhenRemoteOversizedAndLocalUnavailable(t *testing.T) {
	t.Parallel()

	// Empty non-git cwd: local path authority fails; remote returns GitHub oversized.
	repo := t.TempDir()
	baseSHA := strings.Repeat("b", 40)
	headSHA := strings.Repeat("c", 40)
	payloadPath, configPath, submitLog, _ := writeReviewSubmitHarness(t, repo, baseSHA, headSHA, "github_too_large")

	secretPath := "SERVICE_TOKEN=secret-value-should-not-leak"
	payload := map[string]any{
		"body": "Actionable review\n<!-- looper:review id=review-oversized head=" + headSHA + " outcome=actionable -->",
		"comments": []map[string]any{
			{"body": "needs fix", "path": secretPath, "line": 1, "side": "RIGHT"},
		},
	}
	raw, _ := json.Marshal(payload)
	_ = os.WriteFile(payloadPath, raw, 0o644)

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runtime := newCommandRuntime(New(Deps{Stdout: stdout, Stderr: stderr, Getwd: func() (string, error) { return repo, nil }}), []string{"--config", configPath})
	cmd := newReviewSubmitTestCommand(stdout, stderr)
	cmd.SetContext(context.Background())
	f, _ := os.Open(payloadPath)
	defer f.Close()
	cmd.SetIn(f)
	_ = cmd.Flags().Set("event", "COMMENT")
	_ = cmd.Flags().Set("commit-id", headSHA)

	err := runtime.reviewSubmit(cmd, []string{"acme/looper#42"})
	if err == nil {
		t.Fatal("reviewSubmit() error = nil, want fail closed when authority unavailable")
	}
	if !strings.Contains(err.Error(), "anchor") && !strings.Contains(stderr.String(), "anchor_validation_unavailable") {
		t.Fatalf("error=%v stderr=%s, want anchor_validation_unavailable", err, stderr.String())
	}
	// Returned error must not wrap raw git argv with secret-shaped pathspecs.
	if strings.Contains(err.Error(), secretPath) || strings.Contains(err.Error(), "SERVICE_TOKEN") {
		t.Fatalf("returned error leaked secret-shaped path: %v", err)
	}
	// Authority failure never reaches SubmitReview content guard; diagnostics must redact paths.
	if strings.Contains(stderr.String(), secretPath) {
		t.Fatalf("stderr diagnostic echoed secret-shaped path: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"path_present":true`) && !strings.Contains(stderr.String(), "path_present") {
		// Accept either structured redaction or absence of raw path when diagnostic is minimal.
		if strings.Contains(stderr.String(), "github_review_submit_validation_failed") && strings.Contains(stderr.String(), secretPath) {
			t.Fatalf("stderr leaked path: %s", stderr.String())
		}
	}
	if _, statErr := os.Stat(submitLog); !os.IsNotExist(statErr) {
		data, _ := os.ReadFile(submitLog)
		if len(bytes.TrimSpace(data)) > 0 {
			t.Fatalf("review published without authority: %s", data)
		}
	}
}

func TestReviewSubmitOrchestrationRetryDoesNotDuplicateAfterAuthorityRecovery(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	baseSHA, headSHA, targetLine := seedReviewSubmitLargeRepo(t, repo)
	payloadPath, configPath, submitLog, _ := writeReviewSubmitHarness(t, repo, baseSHA, headSHA, "truncated")

	payload := map[string]any{
		"body": "Actionable review\n<!-- looper:review id=review-retry head=" + headSHA + " outcome=actionable -->",
		"comments": []map[string]any{
			{"body": "late change needs attention", "path": "target/late.go", "line": targetLine, "side": "RIGHT"},
		},
	}
	raw, _ := json.Marshal(payload)
	_ = os.WriteFile(payloadPath, raw, 0o644)

	// First attempt: force local git missing so validation fails before publish.
	brokenRepo := t.TempDir()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runtime := newCommandRuntime(New(Deps{Stdout: stdout, Stderr: stderr, Getwd: func() (string, error) { return brokenRepo, nil }}), []string{"--config", configPath})
	cmd := newReviewSubmitTestCommand(stdout, stderr)
	cmd.SetContext(context.Background())
	f, _ := os.Open(payloadPath)
	_ = cmd.Flags().Set("event", "COMMENT")
	_ = cmd.Flags().Set("commit-id", headSHA)
	cmd.SetIn(f)
	firstErr := runtime.reviewSubmit(cmd, []string{"acme/looper#42"})
	_ = f.Close()
	if firstErr == nil {
		t.Fatal("first reviewSubmit() error = nil, want validation failure before publish")
	}
	if _, statErr := os.Stat(submitLog); !os.IsNotExist(statErr) {
		data, _ := os.ReadFile(submitLog)
		if len(bytes.TrimSpace(data)) > 0 {
			t.Fatalf("first attempt published review: %s", data)
		}
	}

	// Recovery attempt with correct worktree publishes exactly once.
	stdout.Reset()
	stderr.Reset()
	runtime = newCommandRuntime(New(Deps{Stdout: stdout, Stderr: stderr, Getwd: func() (string, error) { return repo, nil }}), []string{"--config", configPath})
	cmd = newReviewSubmitTestCommand(stdout, stderr)
	cmd.SetContext(context.Background())
	f, _ = os.Open(payloadPath)
	defer f.Close()
	cmd.SetIn(f)
	_ = cmd.Flags().Set("event", "COMMENT")
	_ = cmd.Flags().Set("commit-id", headSHA)
	if err := runtime.reviewSubmit(cmd, []string{"acme/looper#42"}); err != nil {
		t.Fatalf("recovery reviewSubmit() error = %v\nstderr=%s", err, stderr.String())
	}
	data, err := os.ReadFile(submitLog)
	if err != nil {
		t.Fatalf("read submit log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("submit log lines = %d (%q), want exactly one publish after recovery", len(lines), string(data))
	}
}
