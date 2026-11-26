package cliapp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/spf13/cobra"
)

func runGitForSubmitBaseDriftTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func seedSubmitBaseDriftRepo(t *testing.T) (remoteDir, workDir, oldBase, unrelatedNewBase, shiftedNewBase, headSHA string) {
	t.Helper()
	root := t.TempDir()
	remoteDir = filepath.Join(root, "origin.git")
	workDir = filepath.Join(root, "work")

	runGitForSubmitBaseDriftTest(t, root, "init", "--bare", remoteDir)
	runGitForSubmitBaseDriftTest(t, root, "clone", remoteDir, workDir)
	runGitForSubmitBaseDriftTest(t, workDir, "config", "user.name", "Test")
	runGitForSubmitBaseDriftTest(t, workDir, "config", "user.email", "test@example.com")
	runGitForSubmitBaseDriftTest(t, workDir, "checkout", "-b", "main")

	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package app\n\nfunc Hello() int { return 1 }\n"), 0o644); err != nil {
		t.Fatalf("write app.go: %v", err)
	}
	runGitForSubmitBaseDriftTest(t, workDir, "add", ".")
	runGitForSubmitBaseDriftTest(t, workDir, "commit", "-m", "base")
	oldBase = runGitForSubmitBaseDriftTest(t, workDir, "rev-parse", "HEAD")
	runGitForSubmitBaseDriftTest(t, workDir, "push", "-u", "origin", "main")

	// Feature branch with two stacked commits so we can also test shifted merge-base.
	runGitForSubmitBaseDriftTest(t, workDir, "checkout", "-b", "feature/pr")
	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package app\n\nfunc Hello() int { return 2 }\n"), 0o644); err != nil {
		t.Fatalf("write app.go v2: %v", err)
	}
	runGitForSubmitBaseDriftTest(t, workDir, "add", "app.go")
	runGitForSubmitBaseDriftTest(t, workDir, "commit", "-m", "stacked part 1")
	c1 := runGitForSubmitBaseDriftTest(t, workDir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(workDir, "extra.go"), []byte("package app\n\nfunc Extra() {}\n"), 0o644); err != nil {
		t.Fatalf("write extra.go: %v", err)
	}
	runGitForSubmitBaseDriftTest(t, workDir, "add", "extra.go")
	runGitForSubmitBaseDriftTest(t, workDir, "commit", "-m", "stacked part 2")
	headSHA = runGitForSubmitBaseDriftTest(t, workDir, "rev-parse", "HEAD")
	runGitForSubmitBaseDriftTest(t, workDir, "push", "-u", "origin", "feature/pr")

	// Advance origin/main with an unrelated commit from a separate clone.
	pusher := filepath.Join(root, "pusher")
	runGitForSubmitBaseDriftTest(t, root, "clone", remoteDir, pusher)
	runGitForSubmitBaseDriftTest(t, pusher, "config", "user.name", "Test")
	runGitForSubmitBaseDriftTest(t, pusher, "config", "user.email", "test@example.com")
	runGitForSubmitBaseDriftTest(t, pusher, "checkout", "main")
	if err := os.WriteFile(filepath.Join(pusher, "unrelated.go"), []byte("package app\n\nfunc Unrelated() {}\n"), 0o644); err != nil {
		t.Fatalf("write unrelated.go: %v", err)
	}
	runGitForSubmitBaseDriftTest(t, pusher, "add", "unrelated.go")
	runGitForSubmitBaseDriftTest(t, pusher, "commit", "-m", "unrelated main commit")
	unrelatedNewBase = runGitForSubmitBaseDriftTest(t, pusher, "rev-parse", "HEAD")
	runGitForSubmitBaseDriftTest(t, pusher, "push", "origin", "main")

	// Also build a separate branch on origin merging c1 into main (shifted merge-base).
	runGitForSubmitBaseDriftTest(t, pusher, "fetch", "origin", "feature/pr")
	runGitForSubmitBaseDriftTest(t, pusher, "checkout", "-b", "main-shifted")
	runGitForSubmitBaseDriftTest(t, pusher, "merge", "--no-ff", "-m", "merge c1", c1)
	shiftedNewBase = runGitForSubmitBaseDriftTest(t, pusher, "rev-parse", "HEAD")
	runGitForSubmitBaseDriftTest(t, pusher, "push", "origin", "main-shifted")

	return remoteDir, workDir, oldBase, unrelatedNewBase, shiftedNewBase, headSHA
}

func TestValidateLatestReviewerReviewSubmitPublicationBaseDriftSemantics(t *testing.T) {
	t.Parallel()
	_, workDir, oldBase, unrelatedNewBase, shiftedNewBase, headSHA := seedSubmitBaseDriftRepo(t)
	runtime := &commandRuntime{}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	t.Run("allows_unrelated_base_advance_with_unchanged_merge_base", func(t *testing.T) {
		gh := &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{
			Number: 42, State: "OPEN", BaseRefName: "main", HeadSHA: headSHA, BaseSHA: unrelatedNewBase, Labels: []string{"ready-for-review"},
		}}
		labels, err := runtime.validateLatestReviewerReviewSubmitPublication(cmd, gh, config.Config{}, "acme/looper", 42, headSHA, oldBase, false, "", workDir)
		if err != nil {
			t.Fatalf("validateLatestReviewerReviewSubmitPublication() error = %v, want nil", err)
		}
		if len(labels) != 1 || labels[0] != "ready-for-review" {
			t.Fatalf("labels = %#v, want refreshed labels", labels)
		}
	})

	t.Run("rejects_base_advance_with_merge_conflicts", func(t *testing.T) {
		gh := &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{
			Number: 42, State: "OPEN", BaseRefName: "main", HeadSHA: headSHA, BaseSHA: unrelatedNewBase, HasConflicts: true,
		}}
		_, err := runtime.validateLatestReviewerReviewSubmitPublication(cmd, gh, config.Config{}, "acme/looper", 42, headSHA, oldBase, false, "", workDir)
		if err == nil || !strings.Contains(err.Error(), "expected base commit") {
			t.Fatalf("error = %v, want base drift rejection when conflicted", err)
		}
	})

	t.Run("rejects_base_branch_retarget_even_when_merge_base_matches", func(t *testing.T) {
		gh := &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{
			Number: 42, State: "OPEN", BaseRefName: "release", HeadSHA: headSHA, BaseSHA: unrelatedNewBase,
		}}
		_, err := runtime.validateLatestReviewerReviewSubmitPublicationWithBaseRef(cmd, gh, config.Config{}, "acme/looper", 42, headSHA, oldBase, "main", false, "", workDir)
		if err == nil || !strings.Contains(err.Error(), "PR base branch changed before publish") {
			t.Fatalf("error = %v, want base branch retarget rejection", err)
		}
	})

	t.Run("rejects_base_advance_when_merge_base_shifts", func(t *testing.T) {
		gh := &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{
			Number: 42, State: "OPEN", BaseRefName: "main-shifted", HeadSHA: headSHA, BaseSHA: shiftedNewBase,
		}}
		_, err := runtime.validateLatestReviewerReviewSubmitPublication(cmd, gh, config.Config{}, "acme/looper", 42, headSHA, oldBase, false, "", workDir)
		if err == nil || !strings.Contains(err.Error(), "expected base commit") {
			t.Fatalf("error = %v, want base drift rejection when merge-base shifted", err)
		}
	})
}

func TestValidateSandboxReviewTargetBaseDriftSemantics(t *testing.T) {
	t.Parallel()
	_, workDir, oldBase, unrelatedNewBase, shiftedNewBase, headSHA := seedSubmitBaseDriftRepo(t)
	checkpoint := fmt.Sprintf(`{"snapshot":{"headSha":%q,"baseSha":%q},"detail":{"baseRefName":"main","isDraft":false}}`, headSHA, oldBase)

	live := githubinfra.PullRequestDetail{State: "OPEN", BaseRefName: "main", HeadSHA: headSHA, BaseSHA: unrelatedNewBase}
	if err := validateSandboxReviewTargetWithGit(context.Background(), "git", workDir, checkpoint, live); err != nil {
		t.Fatalf("validateSandboxReviewTargetWithGit(unrelated advance) error = %v, want nil", err)
	}

	conflicted := live
	conflicted.HasConflicts = true
	if err := validateSandboxReviewTargetWithGit(context.Background(), "git", workDir, checkpoint, conflicted); err == nil {
		t.Fatal("validateSandboxReviewTargetWithGit(conflicted) = nil, want rejection")
	}

	retargeted := live
	retargeted.BaseRefName = "release"
	if err := validateSandboxReviewTargetWithGit(context.Background(), "git", workDir, checkpoint, retargeted); err == nil {
		t.Fatal("validateSandboxReviewTargetWithGit(retargeted) = nil, want rejection")
	}

	// Ensure shiftedNewBase is fetched into workDir and rejected due to merge-base shift.
	runGitForSubmitBaseDriftTest(t, workDir, "fetch", "--no-tags", "origin", "main-shifted")
	shifted := live
	shifted.BaseSHA = shiftedNewBase
	if err := validateSandboxReviewTargetWithGit(context.Background(), "git", workDir, checkpoint, shifted); err == nil {
		t.Fatal("validateSandboxReviewTargetWithGit(shifted merge-base) = nil, want rejection")
	}
}
