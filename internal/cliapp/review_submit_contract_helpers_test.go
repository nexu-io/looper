package cliapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

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
