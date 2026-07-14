package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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

	payload := map[string]any{
		"body": "Actionable review\n<!-- looper:review id=review-oversized head=" + headSHA + " outcome=actionable -->",
		"comments": []map[string]any{
			{"body": "needs fix", "path": "app.go", "line": 1, "side": "RIGHT"},
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

func newReviewSubmitTestCommand(stdout, stderr *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{Use: "submit"}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().String("event", "", "")
	cmd.Flags().String("commit-id", "", "")
	cmd.Flags().String("clean-review-event", "", "")
	cmd.Flags().String("blocking-review-event", "", "")
	cmd.Flags().Bool("reviewer-manual", false, "")
	cmd.Flags().String("reviewer-run-id", "", "")
	return cmd
}

func writeReviewSubmitHarness(t *testing.T, repo, baseSHA, headSHA, diffMode string) (payloadPath, configPath, submitLog, ghPath string) {
	t.Helper()
	root := t.TempDir()
	payloadPath = filepath.Join(root, "payload.json")
	configPath = filepath.Join(root, "config.json")
	submitLog = filepath.Join(root, "submit.log")
	ghPath = filepath.Join(root, "gh")

	script := fmt.Sprintf(`#!/bin/sh
set -eu
submit_log=%q
base_sha=%q
head_sha=%q
diff_mode=%q
log_invocations=%q

# Record invocations for debugging.
printf '%%s\n' "$*" >> "$log_invocations"

case "$1" in
  pr)
    case "$2" in
      view)
        printf '{"number":42,"title":"Large PR","body":"Body","state":"OPEN","isDraft":false,"headRefOid":"%%s","baseRefOid":"%%s","author":{"login":"octocat"},"labels":[],"headRefName":"feature","baseRefName":"main","mergeStateStatus":"CLEAN"}\n' "$head_sha" "$base_sha"
        exit 0
        ;;
      diff)
        if [ "$diff_mode" = "github_too_large" ]; then
          printf 'HTTP 406: diff exceeded maximum number of lines too_large\n' >&2
          exit 1
        fi
        # Emit more than 256 KiB then continue so shell capture truncates.
        # The real path under test must not depend on this incomplete output.
        i=0
        while [ "$i" -lt 300 ]; do
          printf 'pad-line-%%s-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n' "$i"
          i=$((i+1))
        done
        # Keep producing until well past the generic cap.
        dd if=/dev/zero bs=1024 count=300 2>/dev/null | tr '\0' 'x'
        exit 0
        ;;
      *)
        printf 'unexpected gh pr args: %%s\n' "$*" >&2
        exit 1
        ;;
    esac
    ;;
  api)
    # Review submit POST body is on stdin.
    if printf '%%s' "$*" | grep -q 'repos/.*/pulls/.*/reviews'; then
      cat > "$submit_log.tmp"
      mv "$submit_log.tmp" "$submit_log"
      printf 'HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{"id":1,"state":"COMMENTED"}\n'
      exit 0
    fi
    if [ "$2" = "user" ]; then
      printf '{"login":"reviewer"}\n'
      exit 0
    fi
    printf 'unexpected gh api args: %%s\n' "$*" >&2
    exit 1
    ;;
  *)
    printf 'unexpected gh args: %%s\n' "$*" >&2
    exit 1
    ;;
esac
`, submitLog, baseSHA, headSHA, diffMode, filepath.Join(root, "gh-invocations.log"))
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}

	configPayload := map[string]any{
		"tools": map[string]any{
			"ghPath":  ghPath,
			"gitPath": "git",
		},
		"roles": map[string]any{
			"reviewer": map[string]any{
				"behavior": map[string]any{
					"reviewEvents": map[string]any{
						"clean":    "COMMENT",
						"blocking": "COMMENT",
					},
				},
			},
		},
		"storage": map[string]any{
			"dbPath": filepath.Join(root, "looper.sqlite"),
		},
	}
	raw, err := json.Marshal(configPayload)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return payloadPath, configPath, submitLog, ghPath
}

func readLastReviewSubmitPayload(t *testing.T, submitLog string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(submitLog)
	if err != nil {
		t.Fatalf("read submit log: %v", err)
	}
	// strip optional HTTP framing if present; harness writes raw JSON body only
	body := bytes.TrimSpace(data)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode submit payload %q: %v", body, err)
	}
	return payload
}

func seedReviewSubmitLargeRepo(t *testing.T, repo string) (baseSHA, headSHA string, targetLine int64) {
	t.Helper()
	runReviewSubmitGit(t, repo, "init")
	runReviewSubmitGit(t, repo, "config", "user.email", "test@example.com")
	runReviewSubmitGit(t, repo, "config", "user.name", "Test")
	for i := 0; i < 40; i++ {
		dir := filepath.Join(repo, fmt.Sprintf("pad/%02d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "blob.txt"), []byte(strings.Repeat(fmt.Sprintf("pad-%02d\n", i), 512)), 0o644); err != nil {
			t.Fatalf("write pad: %v", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, "target"), 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "target/late.go"), []byte("package target\n\nfunc Existing() {}\n"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	runReviewSubmitGit(t, repo, "add", ".")
	runReviewSubmitGit(t, repo, "commit", "-m", "base")
	baseSHA = strings.TrimSpace(runReviewSubmitGitOutput(t, repo, "rev-parse", "HEAD"))

	for i := 0; i < 40; i++ {
		path := filepath.Join(repo, fmt.Sprintf("pad/%02d/blob.txt", i))
		if err := os.WriteFile(path, []byte(strings.Repeat(fmt.Sprintf("pad-%02d-changed\n", i), 512)), 0o644); err != nil {
			t.Fatalf("rewrite pad: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "target/late.go"), []byte("package target\n\nfunc Existing() {}\n\nfunc subscription_checkout_offer_stale() {}\n"), 0o644); err != nil {
		t.Fatalf("write target head: %v", err)
	}
	runReviewSubmitGit(t, repo, "add", ".")
	runReviewSubmitGit(t, repo, "commit", "-m", "head")
	headSHA = strings.TrimSpace(runReviewSubmitGitOutput(t, repo, "rev-parse", "HEAD"))
	fullDiff := runReviewSubmitGitOutput(t, repo, "diff", "--no-ext-diff", "--no-color", baseSHA+"..."+headSHA)
	if len(fullDiff) <= 256*1024 {
		t.Fatalf("full diff size = %d, want > 256KiB", len(fullDiff))
	}
	if idx := strings.Index(fullDiff, "diff --git a/target/late.go"); idx < 256*1024 {
		t.Fatalf("target offset = %d, want after 256KiB", idx)
	}
	targetLine = 5
	return baseSHA, headSHA, targetLine
}

func seedReviewSubmitDeletedRepo(t *testing.T, repo string) (baseSHA, headSHA string, deletedLine int64) {
	t.Helper()
	runReviewSubmitGit(t, repo, "init")
	runReviewSubmitGit(t, repo, "config", "user.email", "test@example.com")
	runReviewSubmitGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "removed.go"), []byte("package removed\n\nfunc Keep() {}\nfunc DeleteMe() {}\nfunc Tail() {}\n"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	runReviewSubmitGit(t, repo, "add", ".")
	runReviewSubmitGit(t, repo, "commit", "-m", "base")
	baseSHA = strings.TrimSpace(runReviewSubmitGitOutput(t, repo, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repo, "removed.go"), []byte("package removed\n\nfunc Keep() {}\nfunc Tail() {}\n"), 0o644); err != nil {
		t.Fatalf("write head: %v", err)
	}
	runReviewSubmitGit(t, repo, "add", ".")
	runReviewSubmitGit(t, repo, "commit", "-m", "delete")
	headSHA = strings.TrimSpace(runReviewSubmitGitOutput(t, repo, "rev-parse", "HEAD"))
	deletedLine = 4
	return baseSHA, headSHA, deletedLine
}

func runReviewSubmitGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	_ = runReviewSubmitGitOutput(t, repo, args...)
}

func runReviewSubmitGitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
