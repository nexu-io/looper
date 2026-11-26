package github

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/diffanchor"
)

func runGitForBaseDriftTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func seedBaseDriftRepos(t *testing.T) (remoteDir, workDir, oldBaseSHA, headSHA string) {
	t.Helper()
	root := t.TempDir()
	remoteDir = filepath.Join(root, "origin.git")
	workDir = filepath.Join(root, "work")

	runGitForBaseDriftTest(t, root, "init", "--bare", remoteDir)
	runGitForBaseDriftTest(t, root, "clone", remoteDir, workDir)
	runGitForBaseDriftTest(t, workDir, "config", "user.name", "Test")
	runGitForBaseDriftTest(t, workDir, "config", "user.email", "test@example.com")
	runGitForBaseDriftTest(t, workDir, "checkout", "-b", "main")

	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package app\n\nfunc Hello() string { return \"v1\" }\n"), 0o644); err != nil {
		t.Fatalf("write app.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "other.go"), []byte("package app\n\nfunc Other() int { return 1 }\n"), 0o644); err != nil {
		t.Fatalf("write other.go: %v", err)
	}
	runGitForBaseDriftTest(t, workDir, "add", ".")
	runGitForBaseDriftTest(t, workDir, "commit", "-m", "initial")
	oldBaseSHA = runGitForBaseDriftTest(t, workDir, "rev-parse", "HEAD")
	runGitForBaseDriftTest(t, workDir, "push", "-u", "origin", "main")

	runGitForBaseDriftTest(t, workDir, "checkout", "-b", "feature/pr")
	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package app\n\nfunc Hello() string { return \"v2\" }\n"), 0o644); err != nil {
		t.Fatalf("write feature app.go: %v", err)
	}
	runGitForBaseDriftTest(t, workDir, "add", "app.go")
	runGitForBaseDriftTest(t, workDir, "commit", "-m", "feature change")
	headSHA = runGitForBaseDriftTest(t, workDir, "rev-parse", "HEAD")
	runGitForBaseDriftTest(t, workDir, "push", "-u", "origin", "feature/pr")

	return remoteDir, workDir, oldBaseSHA, headSHA
}

func pushRemoteCommitOnMain(t *testing.T, remoteDir, filename, content, message string) string {
	t.Helper()
	pusher := filepath.Join(t.TempDir(), "pusher")
	runGitForBaseDriftTest(t, filepath.Dir(pusher), "clone", remoteDir, pusher)
	runGitForBaseDriftTest(t, pusher, "config", "user.name", "Test")
	runGitForBaseDriftTest(t, pusher, "config", "user.email", "test@example.com")
	runGitForBaseDriftTest(t, pusher, "checkout", "main")
	if err := os.WriteFile(filepath.Join(pusher, filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
	runGitForBaseDriftTest(t, pusher, "add", filename)
	runGitForBaseDriftTest(t, pusher, "commit", "-m", message)
	sha := runGitForBaseDriftTest(t, pusher, "rev-parse", "HEAD")
	runGitForBaseDriftTest(t, pusher, "push", "origin", "main")
	return sha
}

func TestBaseAdvancePreservesMergeBaseOnUnrelatedRemoteCommit(t *testing.T) {
	t.Parallel()
	remoteDir, workDir, oldBaseSHA, headSHA := seedBaseDriftRepos(t)

	// Advance origin/main from a separate clone so workDir does NOT have newBaseSHA locally yet.
	newBaseSHA := pushRemoteCommitOnMain(t, remoteDir, "other.go", "package app\n\nfunc Other() int { return 2 }\n", "unrelated main advance")

	preserved, err := BaseAdvancePreservesMergeBase(context.Background(), "git", workDir, oldBaseSHA, newBaseSHA, headSHA, "main")
	if err != nil {
		t.Fatalf("BaseAdvancePreservesMergeBase() error = %v", err)
	}
	if !preserved {
		t.Fatal("BaseAdvancePreservesMergeBase() = false, want true for unrelated base advance")
	}
}

func TestBaseAdvanceRejectsShiftedMergeBase(t *testing.T) {
	t.Parallel()
	remoteDir, workDir, oldBaseSHA, _ := seedBaseDriftRepos(t)

	// Stack a second commit on feature/pr so feature/pr has two commits (c1 -> c2).
	c1 := runGitForBaseDriftTest(t, workDir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(workDir, "stacked.go"), []byte("package app\n\nfunc Stacked() {}\n"), 0o644); err != nil {
		t.Fatalf("write stacked.go: %v", err)
	}
	runGitForBaseDriftTest(t, workDir, "add", "stacked.go")
	runGitForBaseDriftTest(t, workDir, "commit", "-m", "stacked commit")
	headSHA := runGitForBaseDriftTest(t, workDir, "rev-parse", "HEAD")
	runGitForBaseDriftTest(t, workDir, "push", "origin", "feature/pr")

	// Merge c1 into origin/main from another clone, shifting merge-base(main, headSHA) from oldBaseSHA to c1.
	pusher := filepath.Join(t.TempDir(), "pusher")
	runGitForBaseDriftTest(t, filepath.Dir(pusher), "clone", remoteDir, pusher)
	runGitForBaseDriftTest(t, pusher, "config", "user.name", "Test")
	runGitForBaseDriftTest(t, pusher, "config", "user.email", "test@example.com")
	runGitForBaseDriftTest(t, pusher, "checkout", "main")
	runGitForBaseDriftTest(t, pusher, "fetch", "origin", "feature/pr")
	runGitForBaseDriftTest(t, pusher, "merge", "--ff-only", c1)
	newBaseSHA := runGitForBaseDriftTest(t, pusher, "rev-parse", "HEAD")
	runGitForBaseDriftTest(t, pusher, "push", "origin", "main")

	preserved, err := BaseAdvancePreservesMergeBase(context.Background(), "git", workDir, oldBaseSHA, newBaseSHA, headSHA, "main")
	if err != nil {
		t.Fatalf("BaseAdvancePreservesMergeBase() error = %v", err)
	}
	if preserved {
		t.Fatal("BaseAdvancePreservesMergeBase() = true, want false when merge-base shifted")
	}
}

func TestBaseAdvanceFailsClosedOnMissingCommit(t *testing.T) {
	t.Parallel()
	_, workDir, oldBaseSHA, headSHA := seedBaseDriftRepos(t)

	preserved, err := BaseAdvancePreservesMergeBase(context.Background(), "git", workDir, oldBaseSHA, strings.Repeat("a", 40), headSHA, "main")
	if preserved || err == nil {
		t.Fatalf("BaseAdvancePreservesMergeBase(missing) = (%v, %v), want (false, non-nil error)", preserved, err)
	}
}

func TestBuildReviewAnchorIndexFetchesAdvancedBaseCommit(t *testing.T) {
	t.Parallel()
	remoteDir, workDir, _, headSHA := seedBaseDriftRepos(t)
	newBaseSHA := pushRemoteCommitOnMain(t, remoteDir, "other.go", "package app\n\nfunc Other() int { return 99 }\n", "advance main remotely")

	gateway := New(Options{GHPath: "gh", GitPath: "git", CWD: workDir})
	idx, authority, err := gateway.BuildReviewAnchorIndex(context.Background(), BuildReviewAnchorIndexInput{
		CWD:         workDir,
		BaseSHA:     newBaseSHA,
		BaseRefName: "main",
		HeadSHA:     headSHA,
		Paths:       []string{"app.go"},
	})
	if err != nil {
		t.Fatalf("BuildReviewAnchorIndex() error = %v", err)
	}
	if authority != ReviewAnchorAuthorityLocalPathDiff || idx == nil {
		t.Fatalf("authority = %q, idx = %#v, want local path diff index", authority, idx)
	}
	if res := idx.Validate(diffanchor.Anchor{Path: "app.go", Line: 3, Side: diffanchor.SideRight}); !res.Valid {
		t.Fatalf("Validate(app.go:3 RIGHT) = %#v, want valid", res)
	}
}

func TestBaseAdvanceFetchDoesNotMutateFetchHeadOrTrackingRefs(t *testing.T) {
	t.Parallel()
	remoteDir, workDir, oldBaseSHA, headSHA := seedBaseDriftRepos(t)
	originMainBefore := runGitForBaseDriftTest(t, workDir, "rev-parse", "refs/remotes/origin/main")
	fetchHeadPath := filepath.Join(workDir, ".git", "FETCH_HEAD")
	_ = os.Remove(fetchHeadPath)

	newBaseSHA := pushRemoteCommitOnMain(t, remoteDir, "other.go", "package app\n\nfunc Other() int { return 42 }\n", "advance main")

	preserved, err := BaseAdvancePreservesMergeBase(context.Background(), "git", workDir, oldBaseSHA, newBaseSHA, headSHA, "main")
	if err != nil || !preserved {
		t.Fatalf("BaseAdvancePreservesMergeBase() = (%v, %v), want (true, nil)", preserved, err)
	}
	originMainAfter := runGitForBaseDriftTest(t, workDir, "rev-parse", "refs/remotes/origin/main")
	if originMainAfter != originMainBefore {
		t.Fatalf("refs/remotes/origin/main changed from %s to %s; fetch must not mutate tracking refs", originMainBefore, originMainAfter)
	}
	if _, statErr := os.Stat(fetchHeadPath); !os.IsNotExist(statErr) {
		t.Fatalf("FETCH_HEAD should not be written by --no-write-fetch-head fetch (stat err = %v)", statErr)
	}
}

func TestHasLocalMergeConflictsDistinguishesCleanMergeFromRealConflict(t *testing.T) {
	t.Parallel()
	remoteDir, workDir, _, headSHA := seedBaseDriftRepos(t)
	gateway := New(Options{GHPath: "gh", GitPath: "git", CWD: workDir})

	cleanBase := pushRemoteCommitOnMain(t, remoteDir, "other.go", "package app\n\nfunc Other() int { return 5 }\n", "clean advance")
	conflicted, err := gateway.HasLocalMergeConflicts(context.Background(), workDir, cleanBase, "main", headSHA, "feature/pr")
	if err != nil || conflicted {
		t.Fatalf("HasLocalMergeConflicts(clean) = (%v, %v), want (false, nil)", conflicted, err)
	}

	conflictingBase := pushRemoteCommitOnMain(t, remoteDir, "app.go", "package app\n\nfunc Hello() string { return \"conflict\" }\n", "conflicting advance")
	conflicted, err = gateway.HasLocalMergeConflicts(context.Background(), workDir, conflictingBase, "main", headSHA, "feature/pr")
	if err != nil || !conflicted {
		t.Fatalf("HasLocalMergeConflicts(conflict) = (%v, %v), want (true, nil)", conflicted, err)
	}
}
