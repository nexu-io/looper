package github

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGitForHeadDriftTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func seedHeadDriftRepos(t *testing.T) (remoteDir, workDir, baseSHA, oldHeadSHA string) {
	t.Helper()
	root := t.TempDir()
	remoteDir = filepath.Join(root, "origin.git")
	workDir = filepath.Join(root, "work")

	runGitForHeadDriftTest(t, root, "init", "--bare", remoteDir)
	runGitForHeadDriftTest(t, root, "clone", remoteDir, workDir)
	runGitForHeadDriftTest(t, workDir, "config", "user.name", "Test")
	runGitForHeadDriftTest(t, workDir, "config", "user.email", "test@example.com")
	runGitForHeadDriftTest(t, workDir, "checkout", "-b", "main")

	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package app\n\nfunc Hello() string { return \"v1\" }\n"), 0o644); err != nil {
		t.Fatalf("write app.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "docs.md"), []byte("# Docs\n"), 0o644); err != nil {
		t.Fatalf("write docs.md: %v", err)
	}
	runGitForHeadDriftTest(t, workDir, "add", ".")
	runGitForHeadDriftTest(t, workDir, "commit", "-m", "initial")
	baseSHA = runGitForHeadDriftTest(t, workDir, "rev-parse", "HEAD")
	runGitForHeadDriftTest(t, workDir, "push", "-u", "origin", "main")

	runGitForHeadDriftTest(t, workDir, "checkout", "-b", "feature/pr")
	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package app\n\nfunc Hello() string { return \"v2\" }\n"), 0o644); err != nil {
		t.Fatalf("write app.go v2: %v", err)
	}
	runGitForHeadDriftTest(t, workDir, "add", "app.go")
	runGitForHeadDriftTest(t, workDir, "commit", "-m", "feature commit 1")
	oldHeadSHA = runGitForHeadDriftTest(t, workDir, "rev-parse", "HEAD")
	runGitForHeadDriftTest(t, workDir, "push", "-u", "origin", "feature/pr")

	return remoteDir, workDir, baseSHA, oldHeadSHA
}

func pushFeatureCommit(t *testing.T, remoteDir, filename, content, message string, amend bool) string {
	t.Helper()
	pusher := filepath.Join(t.TempDir(), "pusher")
	runGitForHeadDriftTest(t, filepath.Dir(pusher), "clone", remoteDir, pusher)
	runGitForHeadDriftTest(t, pusher, "config", "user.name", "Test")
	runGitForHeadDriftTest(t, pusher, "config", "user.email", "test@example.com")
	runGitForHeadDriftTest(t, pusher, "checkout", "feature/pr")
	if err := os.WriteFile(filepath.Join(pusher, filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
	runGitForHeadDriftTest(t, pusher, "add", filename)
	if amend {
		runGitForHeadDriftTest(t, pusher, "commit", "--amend", "-m", message)
		sha := runGitForHeadDriftTest(t, pusher, "rev-parse", "HEAD")
		runGitForHeadDriftTest(t, pusher, "push", "--force", "origin", "feature/pr")
		return sha
	}
	runGitForHeadDriftTest(t, pusher, "commit", "-m", message)
	sha := runGitForHeadDriftTest(t, pusher, "rev-parse", "HEAD")
	runGitForHeadDriftTest(t, pusher, "push", "origin", "feature/pr")
	return sha
}

func TestHeadAdvanceDisjointFromPathsAllowsUntouchedFiles(t *testing.T) {
	t.Parallel()
	remoteDir, workDir, _, oldHead := seedHeadDriftRepos(t)
	// Push an additive commit touching only docs.md (not fetched in workDir yet).
	newHead := pushFeatureCommit(t, remoteDir, "docs.md", "# Docs updated\n", "update docs", false)

	disjoint, err := HeadAdvanceDisjointFromPaths(context.Background(), "git", workDir, oldHead, newHead, "feature/pr", []string{"app.go"})
	if err != nil {
		t.Fatalf("HeadAdvanceDisjointFromPaths() error = %v", err)
	}
	if !disjoint {
		t.Fatal("HeadAdvanceDisjointFromPaths() = false, want true when new commit only touches docs.md")
	}
}

func TestHeadAdvanceDisjointFromPathsRejectsTouchedOrRenamedFile(t *testing.T) {
	t.Parallel()
	remoteDir, workDir, _, oldHead := seedHeadDriftRepos(t)

	// Overlapping edit on app.go must be rejected.
	overlappingHead := pushFeatureCommit(t, remoteDir, "app.go", "package app\n\nfunc Hello() string { return \"v3\" }\n", "touch app.go", false)
	disjoint, err := HeadAdvanceDisjointFromPaths(context.Background(), "git", workDir, oldHead, overlappingHead, "feature/pr", []string{"app.go"})
	if err != nil {
		t.Fatalf("HeadAdvanceDisjointFromPaths(overlap) error = %v", err)
	}
	if disjoint {
		t.Fatal("HeadAdvanceDisjointFromPaths(overlap) = true, want false when commented file changed")
	}

	// Rename of app.go -> app_renamed.go must also be rejected when querying app.go.
	pusher := filepath.Join(t.TempDir(), "renamer")
	runGitForHeadDriftTest(t, filepath.Dir(pusher), "clone", remoteDir, pusher)
	runGitForHeadDriftTest(t, pusher, "config", "user.name", "Test")
	runGitForHeadDriftTest(t, pusher, "config", "user.email", "test@example.com")
	runGitForHeadDriftTest(t, pusher, "checkout", "feature/pr")
	runGitForHeadDriftTest(t, pusher, "mv", "app.go", "app_renamed.go")
	runGitForHeadDriftTest(t, pusher, "commit", "-m", "rename app.go")
	renamedHead := runGitForHeadDriftTest(t, pusher, "rev-parse", "HEAD")
	runGitForHeadDriftTest(t, pusher, "push", "origin", "feature/pr")

	disjoint, err = HeadAdvanceDisjointFromPaths(context.Background(), "git", workDir, overlappingHead, renamedHead, "feature/pr", []string{"app.go"})
	if err != nil {
		t.Fatalf("HeadAdvanceDisjointFromPaths(rename) error = %v", err)
	}
	if disjoint {
		t.Fatal("HeadAdvanceDisjointFromPaths(rename) = true, want false when commented file was renamed")
	}
}

func TestHeadAdvanceDisjointFromPathsRejectsForcePush(t *testing.T) {
	t.Parallel()
	remoteDir, workDir, _, oldHead := seedHeadDriftRepos(t)
	forcePushedHead := pushFeatureCommit(t, remoteDir, "docs.md", "# Amended\n", "amend commit", true)

	disjoint, err := HeadAdvanceDisjointFromPaths(context.Background(), "git", workDir, oldHead, forcePushedHead, "feature/pr", []string{"app.go"})
	if err != nil {
		t.Fatalf("HeadAdvanceDisjointFromPaths(force-push) error = %v", err)
	}
	if disjoint {
		t.Fatal("HeadAdvanceDisjointFromPaths(force-push) = true, want false when oldHead is not an ancestor")
	}
}

func TestHeadAdvanceDisjointFromPullRequestDiff(t *testing.T) {
	t.Parallel()
	remoteDir, workDir, baseSHA, oldHead := seedHeadDriftRepos(t)

	// Disjoint additive commit on docs.md (PR diff baseSHA...oldHead only touches app.go).
	disjointHead := pushFeatureCommit(t, remoteDir, "docs.md", "# Updated\n", "docs only", false)
	ok, err := HeadAdvanceDisjointFromPullRequestDiff(context.Background(), "git", workDir, baseSHA, oldHead, disjointHead, "feature/pr")
	if err != nil || !ok {
		t.Fatalf("HeadAdvanceDisjointFromPullRequestDiff(disjoint) = (%v, %v), want (true, nil)", ok, err)
	}

	// Subsequent commit touching app.go must be detected as overlapping.
	overlappingHead := pushFeatureCommit(t, remoteDir, "app.go", "package app\n\nfunc Hello() string { return \"v3\" }\n", "edit app.go", false)
	ok, err = HeadAdvanceDisjointFromPullRequestDiff(context.Background(), "git", workDir, baseSHA, oldHead, overlappingHead, "feature/pr")
	if err != nil || ok {
		t.Fatalf("HeadAdvanceDisjointFromPullRequestDiff(overlap) = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestHeadAdvanceFetchDoesNotMutateFetchHeadOrTrackingRefs(t *testing.T) {
	t.Parallel()
	remoteDir, workDir, baseSHA, oldHead := seedHeadDriftRepos(t)
	trackingBefore := runGitForHeadDriftTest(t, workDir, "rev-parse", "refs/remotes/origin/feature/pr")
	fetchHeadPath := filepath.Join(workDir, ".git", "FETCH_HEAD")
	_ = os.Remove(fetchHeadPath)

	disjointHead := pushFeatureCommit(t, remoteDir, "docs.md", "# Docs no ref mutation\n", "docs commit", false)
	ok, err := HeadAdvanceDisjointFromPullRequestDiff(context.Background(), "git", workDir, baseSHA, oldHead, disjointHead, "feature/pr")
	if err != nil || !ok {
		t.Fatalf("HeadAdvanceDisjointFromPullRequestDiff() = (%v, %v), want (true, nil)", ok, err)
	}
	trackingAfter := runGitForHeadDriftTest(t, workDir, "rev-parse", "refs/remotes/origin/feature/pr")
	if trackingAfter != trackingBefore {
		t.Fatalf("refs/remotes/origin/feature/pr changed from %s to %s", trackingBefore, trackingAfter)
	}
	if _, statErr := os.Stat(fetchHeadPath); !os.IsNotExist(statErr) {
		t.Fatalf("FETCH_HEAD should not be written by --no-write-fetch-head fetch (stat err = %v)", statErr)
	}
}

func TestHasLocalMergeConflictsDistinguishesCleanMergeFromRealConflict(t *testing.T) {
	t.Parallel()
	remoteDir, workDir, _, oldHead := seedHeadDriftRepos(t)
	gateway := New(Options{GHPath: "gh", GitPath: "git", CWD: workDir})

	// Push an unrelated commit to main -> clean merge.
	pusher := filepath.Join(t.TempDir(), "pusher")
	runGitForHeadDriftTest(t, filepath.Dir(pusher), "clone", remoteDir, pusher)
	runGitForHeadDriftTest(t, pusher, "config", "user.name", "Test")
	runGitForHeadDriftTest(t, pusher, "config", "user.email", "test@example.com")
	runGitForHeadDriftTest(t, pusher, "checkout", "main")
	if err := os.WriteFile(filepath.Join(pusher, "other.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatalf("write other.go: %v", err)
	}
	runGitForHeadDriftTest(t, pusher, "add", "other.go")
	runGitForHeadDriftTest(t, pusher, "commit", "-m", "clean main commit")
	cleanBase := runGitForHeadDriftTest(t, pusher, "rev-parse", "HEAD")
	runGitForHeadDriftTest(t, pusher, "push", "origin", "main")

	conflicted, err := gateway.HasLocalMergeConflicts(context.Background(), workDir, cleanBase, "main", oldHead, "feature/pr")
	if err != nil || conflicted {
		t.Fatalf("HasLocalMergeConflicts(clean) = (%v, %v), want (false, nil)", conflicted, err)
	}

	// Push a conflicting commit on app.go to main -> real conflict.
	if err := os.WriteFile(filepath.Join(pusher, "app.go"), []byte("package app\n\nfunc Hello() string { return \"conflict\" }\n"), 0o644); err != nil {
		t.Fatalf("write conflicting app.go: %v", err)
	}
	runGitForHeadDriftTest(t, pusher, "add", "app.go")
	runGitForHeadDriftTest(t, pusher, "commit", "-m", "conflicting main commit")
	conflictingBase := runGitForHeadDriftTest(t, pusher, "rev-parse", "HEAD")
	runGitForHeadDriftTest(t, pusher, "push", "origin", "main")

	conflicted, err = gateway.HasLocalMergeConflicts(context.Background(), workDir, conflictingBase, "main", oldHead, "feature/pr")
	if err != nil || !conflicted {
		t.Fatalf("HasLocalMergeConflicts(conflict) = (%v, %v), want (true, nil)", conflicted, err)
	}
}
