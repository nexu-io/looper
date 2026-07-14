package github

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

// seedRenamedFileRepo creates a base→head rename with a deleted line, an added
// line, and distant unchanged content so path-only post-rename diffs diverge
// from complete rename authority (pure add vs rename hunks).
func seedRenamedFileRepo(t *testing.T, repo string) (baseSHA, headSHA, newPath string, leftDeletedLine, rightAddedLine, distantUnchangedLine int64) {
	t.Helper()
	runGitRepo(t, repo, "init")
	runGitRepo(t, repo, "config", "user.email", "test@example.com")
	runGitRepo(t, repo, "config", "user.name", "Test")

	oldPath := "pkg/old_name.go"
	newPath = "pkg/new_name.go"
	if err := os.MkdirAll(filepath.Join(repo, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir pkg: %v", err)
	}
	// Distant unchanged lines stay outside the default 3-line context hunk when
	// only the middle of the file changes. A pure new-file path-only diff would
	// incorrectly mark every RIGHT line as added and omit LEFT ranges.
	var baseBody strings.Builder
	baseBody.WriteString("package pkg\n\n")
	for i := 1; i <= 20; i++ {
		baseBody.WriteString(fmt.Sprintf("func Unchanged%02d() {}\n", i))
	}
	baseBody.WriteString("func DeleteMe() {}\n")
	for i := 21; i <= 40; i++ {
		baseBody.WriteString(fmt.Sprintf("func Unchanged%02d() {}\n", i))
	}
	if err := os.WriteFile(filepath.Join(repo, oldPath), []byte(baseBody.String()), 0o644); err != nil {
		t.Fatalf("write base renamed file: %v", err)
	}
	runGitRepo(t, repo, "add", ".")
	runGitRepo(t, repo, "commit", "-m", "base")
	baseSHA = strings.TrimSpace(runGitRepoOutput(t, repo, "rev-parse", "HEAD"))

	runGitRepo(t, repo, "mv", oldPath, newPath)
	var headBody strings.Builder
	headBody.WriteString("package pkg\n\n")
	for i := 1; i <= 20; i++ {
		headBody.WriteString(fmt.Sprintf("func Unchanged%02d() {}\n", i))
	}
	headBody.WriteString("func Added() {}\n")
	for i := 21; i <= 40; i++ {
		headBody.WriteString(fmt.Sprintf("func Unchanged%02d() {}\n", i))
	}
	if err := os.WriteFile(filepath.Join(repo, newPath), []byte(headBody.String()), 0o644); err != nil {
		t.Fatalf("write head renamed file: %v", err)
	}
	runGitRepo(t, repo, "add", ".")
	runGitRepo(t, repo, "commit", "-m", "rename and edit")
	headSHA = strings.TrimSpace(runGitRepoOutput(t, repo, "rev-parse", "HEAD"))

	// package + blank + 20 unchanged + DeleteMe/Added at line 23.
	leftDeletedLine = 23
	rightAddedLine = 23
	// First unchanged function is line 3; outside rename hunk context.
	distantUnchangedLine = 3
	return baseSHA, headSHA, newPath, leftDeletedLine, rightAddedLine, distantUnchangedLine
}

// seedPathspecMagicFilenameRepo creates a commit pair for a legal filename that
// Git would parse as pathspec magic without --literal-pathspecs (e.g. ":(foo).txt").
func seedPathspecMagicFilenameRepo(t *testing.T, repo string) (baseSHA, headSHA, magicPath string, targetLine int64) {
	t.Helper()
	runGitRepo(t, repo, "init")
	runGitRepo(t, repo, "config", "user.email", "test@example.com")
	runGitRepo(t, repo, "config", "user.name", "Test")

	magicPath = ":(foo).txt"
	if err := os.WriteFile(filepath.Join(repo, magicPath), []byte("line1\n"), 0o644); err != nil {
		t.Fatalf("write base magic path: %v", err)
	}
	// Plain `git add .` can trip pathspec magic for ":(...)" names; force literal.
	runGitRepo(t, repo, "--literal-pathspecs", "add", "--", magicPath)
	runGitRepo(t, repo, "commit", "-m", "base")
	baseSHA = strings.TrimSpace(runGitRepoOutput(t, repo, "rev-parse", "HEAD"))

	if err := os.WriteFile(filepath.Join(repo, magicPath), []byte("line1\nline2-added\n"), 0o644); err != nil {
		t.Fatalf("write head magic path: %v", err)
	}
	runGitRepo(t, repo, "--literal-pathspecs", "add", "--", magicPath)
	runGitRepo(t, repo, "commit", "-m", "head")
	headSHA = strings.TrimSpace(runGitRepoOutput(t, repo, "rev-parse", "HEAD"))
	targetLine = 2
	return baseSHA, headSHA, magicPath, targetLine
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
