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

func runGitForSubmitHeadDriftTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func seedSubmitHeadDriftRepo(t *testing.T) (workDir, baseSHA, oldHead, disjointHead, overlappingHead, forcePushedHead string) {
	t.Helper()
	root := t.TempDir()
	remoteDir := filepath.Join(root, "origin.git")
	workDir = filepath.Join(root, "work")

	runGitForSubmitHeadDriftTest(t, root, "init", "--bare", remoteDir)
	runGitForSubmitHeadDriftTest(t, root, "clone", remoteDir, workDir)
	runGitForSubmitHeadDriftTest(t, workDir, "config", "user.name", "Test")
	runGitForSubmitHeadDriftTest(t, workDir, "config", "user.email", "test@example.com")
	runGitForSubmitHeadDriftTest(t, workDir, "checkout", "-b", "main")

	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package app\n\nfunc Hello() int { return 1 }\n"), 0o644); err != nil {
		t.Fatalf("write app.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "docs.md"), []byte("# Docs\n"), 0o644); err != nil {
		t.Fatalf("write docs.md: %v", err)
	}
	runGitForSubmitHeadDriftTest(t, workDir, "add", ".")
	runGitForSubmitHeadDriftTest(t, workDir, "commit", "-m", "base")
	baseSHA = runGitForSubmitHeadDriftTest(t, workDir, "rev-parse", "HEAD")
	runGitForSubmitHeadDriftTest(t, workDir, "push", "-u", "origin", "main")

	runGitForSubmitHeadDriftTest(t, workDir, "checkout", "-b", "feature/pr")
	if err := os.WriteFile(filepath.Join(workDir, "app.go"), []byte("package app\n\nfunc Hello() int { return 2 }\n"), 0o644); err != nil {
		t.Fatalf("write app.go v2: %v", err)
	}
	runGitForSubmitHeadDriftTest(t, workDir, "add", "app.go")
	runGitForSubmitHeadDriftTest(t, workDir, "commit", "-m", "pr commit 1")
	oldHead = runGitForSubmitHeadDriftTest(t, workDir, "rev-parse", "HEAD")
	runGitForSubmitHeadDriftTest(t, workDir, "push", "-u", "origin", "feature/pr")

	// Disjoint additive commit on feature/pr touching only docs.md.
	pusher := filepath.Join(root, "pusher")
	runGitForSubmitHeadDriftTest(t, root, "clone", remoteDir, pusher)
	runGitForSubmitHeadDriftTest(t, pusher, "config", "user.name", "Test")
	runGitForSubmitHeadDriftTest(t, pusher, "config", "user.email", "test@example.com")
	runGitForSubmitHeadDriftTest(t, pusher, "checkout", "feature/pr")
	if err := os.WriteFile(filepath.Join(pusher, "docs.md"), []byte("# Docs v2\n"), 0o644); err != nil {
		t.Fatalf("write docs.md v2: %v", err)
	}
	runGitForSubmitHeadDriftTest(t, pusher, "add", "docs.md")
	runGitForSubmitHeadDriftTest(t, pusher, "commit", "-m", "docs update")
	disjointHead = runGitForSubmitHeadDriftTest(t, pusher, "rev-parse", "HEAD")
	runGitForSubmitHeadDriftTest(t, pusher, "push", "origin", "feature/pr")

	// Overlapping additive commit touching app.go on another branch.
	runGitForSubmitHeadDriftTest(t, pusher, "checkout", "-b", "feature/overlap")
	if err := os.WriteFile(filepath.Join(pusher, "app.go"), []byte("package app\n\nfunc Hello() int { return 3 }\n"), 0o644); err != nil {
		t.Fatalf("write app.go v3: %v", err)
	}
	runGitForSubmitHeadDriftTest(t, pusher, "add", "app.go")
	runGitForSubmitHeadDriftTest(t, pusher, "commit", "-m", "edit app.go")
	overlappingHead = runGitForSubmitHeadDriftTest(t, pusher, "rev-parse", "HEAD")
	runGitForSubmitHeadDriftTest(t, pusher, "push", "origin", "feature/overlap")

	// Force-pushed commit (oldHead is NOT an ancestor).
	runGitForSubmitHeadDriftTest(t, pusher, "checkout", "-b", "feature/force", baseSHA)
	if err := os.WriteFile(filepath.Join(pusher, "docs.md"), []byte("# Rewritten\n"), 0o644); err != nil {
		t.Fatalf("write docs.md rewritten: %v", err)
	}
	runGitForSubmitHeadDriftTest(t, pusher, "add", "docs.md")
	runGitForSubmitHeadDriftTest(t, pusher, "commit", "-m", "rewritten commit")
	forcePushedHead = runGitForSubmitHeadDriftTest(t, pusher, "rev-parse", "HEAD")
	runGitForSubmitHeadDriftTest(t, pusher, "push", "origin", "feature/force")

	return workDir, baseSHA, oldHead, disjointHead, overlappingHead, forcePushedHead
}

func TestValidateReviewSubmitHeadCommitAdditiveSemantics(t *testing.T) {
	t.Parallel()
	workDir, baseSHA, oldHead, disjointHead, overlappingHead, forcePushedHead := seedSubmitHeadDriftRepo(t)
	comments := []reviewSubmitComment{{Path: "app.go", Line: 3, Side: "RIGHT", Body: "fix bug"}}
	runtime := &commandRuntime{}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	t.Run("allows_actionable_inline_review_on_disjoint_additive_head", func(t *testing.T) {
		gh := &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{
			Number: 42, State: "OPEN", BaseRefName: "main", HeadRefName: "feature/pr", BaseSHA: baseSHA, HeadSHA: disjointHead, Labels: []string{"ok"},
		}}
		labels, err := runtime.validateLatestReviewerReviewSubmitPublicationWithComments(cmd, gh, config.Config{}, "acme/looper", 42, oldHead, baseSHA, "REQUEST_CHANGES", comments, false, "", workDir)
		if err != nil {
			t.Fatalf("validateLatestReviewerReviewSubmitPublicationWithComments() error = %v, want nil", err)
		}
		if len(labels) != 1 || labels[0] != "ok" {
			t.Fatalf("labels = %#v, want [ok]", labels)
		}
	})

	t.Run("rejects_approve_on_advanced_head", func(t *testing.T) {
		gh := &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{
			Number: 42, State: "OPEN", BaseRefName: "main", HeadRefName: "feature/pr", BaseSHA: baseSHA, HeadSHA: disjointHead,
		}}
		if _, err := runtime.validateLatestReviewerReviewSubmitPublicationWithComments(cmd, gh, config.Config{}, "acme/looper", 42, oldHead, baseSHA, "APPROVE", comments, false, "", workDir); err == nil {
			t.Fatal("APPROVE unexpectedly allowed when head advanced")
		}
	})

	t.Run("rejects_body_only_comment_on_advanced_head", func(t *testing.T) {
		gh := &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{
			Number: 42, State: "OPEN", BaseRefName: "main", HeadRefName: "feature/pr", BaseSHA: baseSHA, HeadSHA: disjointHead,
		}}
		if _, err := runtime.validateLatestReviewerReviewSubmitPublicationWithComments(cmd, gh, config.Config{}, "acme/looper", 42, oldHead, baseSHA, "COMMENT", nil, false, "", workDir); err == nil {
			t.Fatal("body-only COMMENT unexpectedly allowed when head advanced")
		}
	})

	t.Run("rejects_when_commented_file_changed_in_delta", func(t *testing.T) {
		gh := &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{
			Number: 42, State: "OPEN", BaseRefName: "main", HeadRefName: "feature/overlap", BaseSHA: baseSHA, HeadSHA: overlappingHead,
		}}
		if _, err := runtime.validateLatestReviewerReviewSubmitPublicationWithComments(cmd, gh, config.Config{}, "acme/looper", 42, oldHead, baseSHA, "REQUEST_CHANGES", comments, false, "", workDir); err == nil {
			t.Fatal("overlapping file change unexpectedly allowed")
		}
	})

	t.Run("rejects_force_pushed_head", func(t *testing.T) {
		gh := &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{
			Number: 42, State: "OPEN", BaseRefName: "main", HeadRefName: "feature/force", BaseSHA: baseSHA, HeadSHA: forcePushedHead,
		}}
		if _, err := runtime.validateLatestReviewerReviewSubmitPublicationWithComments(cmd, gh, config.Config{}, "acme/looper", 42, oldHead, baseSHA, "REQUEST_CHANGES", comments, false, "", workDir); err == nil {
			t.Fatal("force-pushed head unexpectedly allowed")
		}
	})

	t.Run("rejects_conflicted_closed_or_empty_state_pr", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			state        string
			hasConflicts bool
		}{
			{name: "conflicted", state: "OPEN", hasConflicts: true},
			{name: "closed", state: "CLOSED", hasConflicts: false},
			{name: "empty_state", state: "", hasConflicts: false},
		} {
			gh := &reviewSubmitFakePRViewer{detail: githubinfra.PullRequestDetail{
				Number: 42, State: tc.state, BaseRefName: "main", HeadRefName: "feature/pr", BaseSHA: baseSHA, HeadSHA: disjointHead, HasConflicts: tc.hasConflicts,
			}}
			if _, err := runtime.validateLatestReviewerReviewSubmitPublicationWithComments(cmd, gh, config.Config{}, "acme/looper", 42, oldHead, baseSHA, "REQUEST_CHANGES", comments, false, "", workDir); err == nil {
				t.Fatalf("%s PR unexpectedly allowed on advanced head", tc.name)
			}
		}
	})
}

func TestValidateSandboxReviewTargetWithCommentsHeadDriftSemantics(t *testing.T) {
	t.Parallel()
	workDir, baseSHA, oldHead, disjointHead, overlappingHead, forcePushedHead := seedSubmitHeadDriftRepo(t)
	checkpoint := fmt.Sprintf(`{"snapshot":{"headSha":%q,"baseSha":%q},"detail":{"baseRefName":"main","isDraft":false}}`, oldHead, baseSHA)
	comments := []reviewSubmitComment{{Path: "app.go", Line: 3, Side: "RIGHT", Body: "fix bug"}}

	live := githubinfra.PullRequestDetail{State: "OPEN", BaseRefName: "main", HeadRefName: "feature/pr", BaseSHA: baseSHA, HeadSHA: disjointHead}
	if err := validateSandboxReviewTargetWithComments(context.Background(), "git", workDir, checkpoint, live, "REQUEST_CHANGES", comments); err != nil {
		t.Fatalf("sandbox disjoint additive head error = %v, want nil", err)
	}
	if err := validateSandboxReviewTargetWithComments(context.Background(), "git", workDir, checkpoint, live, "APPROVE", comments); err == nil {
		t.Fatal("sandbox APPROVE unexpectedly allowed on advanced head")
	}
	overlap := live
	overlap.HeadRefName = "feature/overlap"
	overlap.HeadSHA = overlappingHead
	if err := validateSandboxReviewTargetWithComments(context.Background(), "git", workDir, checkpoint, overlap, "REQUEST_CHANGES", comments); err == nil {
		t.Fatal("sandbox overlapping head unexpectedly allowed")
	}
	force := live
	force.HeadRefName = "feature/force"
	force.HeadSHA = forcePushedHead
	if err := validateSandboxReviewTargetWithComments(context.Background(), "git", workDir, checkpoint, force, "REQUEST_CHANGES", comments); err == nil {
		t.Fatal("sandbox force-pushed head unexpectedly allowed")
	}
}
