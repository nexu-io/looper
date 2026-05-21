package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

func TestGatewayRejectsRepoPathAsMutationWorktree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{name: "prepare", run: func() error {
			_, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{RepoPath: fixture.repoPath, WorktreePath: fixture.repoPath, Branch: "main"})
			return err
		}},
		{name: "commit", run: func() error {
			_, err := gateway.Commit(ctx, CommitInput{RepoPath: fixture.repoPath, WorktreePath: fixture.repoPath, Message: "test"})
			return err
		}},
		{name: "push", run: func() error {
			return gateway.Push(ctx, PushInput{RepoPath: fixture.repoPath, WorktreePath: fixture.repoPath, Branch: "feature/test"})
		}},
		{name: "cleanup", run: func() error {
			return gateway.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreePath: fixture.repoPath, Branch: "feature/test"})
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.run()
			if err == nil || !strings.Contains(err.Error(), "must not equal project repo path") {
				t.Fatalf("error = %v, want repo-path safety failure", err)
			}
		})
	}
}

func TestGatewayRejectsMutationWorktreeOutsideRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()
	outsidePath := filepath.Join(fixture.rootDir, "outside-worktree")
	mustMkdirAll(t, outsidePath)

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{name: "prepare", run: func() error {
			_, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: outsidePath, Branch: "feature/test"})
			return err
		}},
		{name: "commit", run: func() error {
			_, err := gateway.Commit(ctx, CommitInput{RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: outsidePath, Message: "test"})
			return err
		}},
		{name: "push", run: func() error {
			return gateway.Push(ctx, PushInput{RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: outsidePath, Branch: "feature/test"})
		}},
		{name: "cleanup", run: func() error {
			return gateway.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: outsidePath, Branch: "feature/test"})
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.run()
			if err == nil || !strings.Contains(err.Error(), "must be under worktree root") {
				t.Fatalf("error = %v, want worktree-root safety failure", err)
			}
		})
	}
}

func TestGatewayCreatesRestoresAndCleansWorktreesWithBranchProtection(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)

	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "feature/fixer",
		BaseBranch:   "main",
		PRNumber:     42,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	restored, err := gateway.RestoreWorktree(ctx, RestoreWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, Branch: "feature/fixer"})
	if err != nil {
		t.Fatalf("RestoreWorktree() error = %v", err)
	}
	prepared, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: worktree.WorktreePath, Branch: "feature/fixer"})
	if err != nil {
		t.Fatalf("PrepareWorktree() error = %v", err)
	}

	writeFile(t, filepath.Join(worktree.WorktreePath, "README.md"), "hello updated\n")
	inspectBeforeCommit, err := gateway.InspectHead(ctx, InspectHeadInput{WorktreePath: worktree.WorktreePath, BaseRef: prepared.HeadSHA})
	if err != nil {
		t.Fatalf("InspectHead(before) error = %v", err)
	}
	globalEmailBefore := stringsTrimSpace(runGitMaybe(t, fixture.repoPath, "config", "--global", "--get", "user.email"))
	commitResult, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, Message: "fixer: address PR #42 follow-up items"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	inspectAfterCommit, err := gateway.InspectHead(ctx, InspectHeadInput{WorktreePath: worktree.WorktreePath, BaseRef: prepared.HeadSHA})
	if err != nil {
		t.Fatalf("InspectHead(after) error = %v", err)
	}

	if got := readFile(t, filepath.Join(worktree.WorktreePath, "README.md")); got != "hello updated\n" {
		t.Fatalf("README.md = %q, want updated contents", got)
	}
	if restored == nil || restored.Branch != "feature/fixer" {
		t.Fatalf("RestoreWorktree() = %#v, want branch feature/fixer", restored)
	}
	if !prepared.Clean {
		t.Fatalf("PrepareWorktree().Clean = false, want true")
	}
	if !inspectBeforeCommit.HasUncommittedChanges {
		t.Fatalf("InspectHead(before).HasUncommittedChanges = false, want true")
	}
	if commitResult.CommitSHA == "" {
		t.Fatal("Commit().CommitSHA = empty, want value")
	}
	if inspectAfterCommit.HasUncommittedChanges {
		t.Fatalf("InspectHead(after).HasUncommittedChanges = true, want false")
	}
	if len(inspectAfterCommit.NewCommitSHAs) != 1 {
		t.Fatalf("InspectHead(after).NewCommitSHAs = %#v, want 1 entry", inspectAfterCommit.NewCommitSHAs)
	}
	commitAuthor := stringsTrimSpace(runGit(t, worktree.WorktreePath, "log", "-1", "--format=%an <%ae>"))
	if commitAuthor != "Looper Test <test@example.com>" {
		t.Fatalf("commit author = %q, want Looper Test <test@example.com>", commitAuthor)
	}
	globalEmailAfter := stringsTrimSpace(runGitMaybe(t, fixture.repoPath, "config", "--global", "--get", "user.email"))
	if globalEmailAfter != globalEmailBefore {
		t.Fatalf("global git email changed: before=%q after=%q", globalEmailBefore, globalEmailAfter)
	}

	if err := gateway.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreePath: worktree.WorktreePath, Branch: "feature/fixer"}); err != nil {
		t.Fatalf("CleanupWorktree() error = %v", err)
	}
	stored, err := fixture.repos.Worktrees.GetByBranch(ctx, fixture.projectID, "feature/fixer")
	if err != nil {
		t.Fatalf("GetByBranch() error = %v", err)
	}
	if stored == nil || stored.Status != "cleaned" {
		t.Fatalf("stored worktree after cleanup = %#v, want cleaned", stored)
	}

	err = gateway.AssertWritableBranch("main", []string{"main"})
	var protectedErr *ProtectedBranchError
	if err == nil || !errors.As(err, &protectedErr) {
		t.Fatalf("AssertWritableBranch() error = %v, want *ProtectedBranchError", err)
	}
}

func TestGatewayKeepsPrimaryCheckoutCleanForDetachedFixerWorktree(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "feature/fixer",
		BaseBranch:   "main",
		PRNumber:     42,
		CheckoutMode: CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	if got := stringsTrimSpace(runGit(t, worktree.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("detached branch name = %q, want empty", got)
	}

	writeFile(t, filepath.Join(worktree.WorktreePath, "README.md"), "hello updated\n")
	baseHeadSHA := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", "refs/remotes/origin/feature/fixer"))
	repoHeadBefore := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", "HEAD"))
	if _, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, Message: "fixer: address PR #42 follow-up items"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := gateway.Push(ctx, PushInput{WorktreePath: worktree.WorktreePath, Branch: "feature/fixer", ExpectedRemoteHeadSHA: baseHeadSHA}); err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	if got := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", "HEAD")); got != repoHeadBefore {
		t.Fatalf("repo HEAD = %q, want %q", got, repoHeadBefore)
	}
	if got := stringsTrimSpace(runGit(t, fixture.repoPath, "status", "--porcelain")); got != "" {
		t.Fatalf("repo status = %q, want empty", got)
	}
	if got := stringsTrimSpace(runGit(t, fixture.repoPath, "diff", "--cached", "--name-only")); got != "" {
		t.Fatalf("repo cached diff = %q, want empty", got)
	}
}

func TestGatewayDetachedWorktreeFallsBackToRemoteOnlyBaseBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	fixture.createUnfetchedRemoteBranch(t, "release/base")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "reviewer/pr-42",
		BaseBranch:   "release/base",
		PRNumber:     42,
		CheckoutMode: CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	if got := stringsTrimSpace(runGit(t, worktree.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("detached branch name = %q, want empty", got)
	}

	remoteBaseSHA := stringsTrimSpace(runGit(t, fixture.remotePath, "rev-parse", "refs/heads/release/base"))
	worktreeHeadSHA := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))
	if worktreeHeadSHA != remoteBaseSHA {
		t.Fatalf("detached HEAD = %q, want %q", worktreeHeadSHA, remoteBaseSHA)
	}
}

func TestGatewayAttachedWorktreeFallsBackToRemoteOnlyBaseBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	fixture.createUnfetchedRemoteBranch(t, "release/base")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "worker/release-base-sync",
		BaseBranch:   "release/base",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	if got := stringsTrimSpace(runGit(t, worktree.WorktreePath, "branch", "--show-current")); got != "worker/release-base-sync" {
		t.Fatalf("attached branch name = %q, want worker/release-base-sync", got)
	}

	remoteBaseSHA := stringsTrimSpace(runGit(t, fixture.remotePath, "rev-parse", "refs/heads/release/base"))
	worktreeHeadSHA := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))
	if worktreeHeadSHA != remoteBaseSHA {
		t.Fatalf("attached HEAD = %q, want %q", worktreeHeadSHA, remoteBaseSHA)
	}
}

func TestGatewayAttachedWorktreeFallsBackToLocalBaseBranchWhenRemoteProbeFails(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	runGit(t, fixture.repoPath, "remote", "set-url", "origin", filepath.Join(fixture.rootDir, "missing-remote.git"))
	gateway := fixture.gateway()

	localBaseSHA := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", "main"))
	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "worker/offline-main-fallback",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	if got := stringsTrimSpace(runGit(t, worktree.WorktreePath, "branch", "--show-current")); got != "worker/offline-main-fallback" {
		t.Fatalf("attached branch name = %q, want worker/offline-main-fallback", got)
	}

	worktreeHeadSHA := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))
	if worktreeHeadSHA != localBaseSHA {
		t.Fatalf("attached HEAD = %q, want %q", worktreeHeadSHA, localBaseSHA)
	}
}

func TestGatewayPushesHeadToRequestedRemoteBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "looper/05e7c1d53bba907c",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	writeFile(t, filepath.Join(worktree.WorktreePath, "README.md"), "hello updated\n")
	if _, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, Message: "worker: update reused PR branch"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := gateway.Push(ctx, PushInput{WorktreePath: worktree.WorktreePath, Branch: "looper/worker/05e7c1d53bba907c"}); err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	remoteHeadSHA := stringsTrimSpace(runGit(t, fixture.remotePath, "rev-parse", "refs/heads/looper/worker/05e7c1d53bba907c"))
	worktreeHeadSHA := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))
	if remoteHeadSHA != worktreeHeadSHA {
		t.Fatalf("remote head = %q, want %q", remoteHeadSHA, worktreeHeadSHA)
	}
}

func TestGatewayCreatesSeparateDetachedWorktreeForAttachedBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()

	attached, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree(attached) error = %v", err)
	}
	if got := stringsTrimSpace(runGit(t, attached.WorktreePath, "branch", "--show-current")); got != "feature/fixer" {
		t.Fatalf("attached branch = %q, want feature/fixer", got)
	}

	detached, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree(detached) error = %v", err)
	}
	if detached.WorktreePath == attached.WorktreePath {
		t.Fatalf("detached worktree path = %q, want separate path from attached worktree", detached.WorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, detached.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("detached branch = %q, want empty", got)
	}
	if _, err := os.Stat(attached.WorktreePath); err != nil {
		t.Fatalf("attached worktree missing after detached create: %v", err)
	}
}

func TestGatewayCreatesSeparateAttachedWorktreeForDetachedBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()

	detached, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree(detached) error = %v", err)
	}
	if got := stringsTrimSpace(runGit(t, detached.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("detached branch = %q, want empty", got)
	}

	attached, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree(attached) error = %v", err)
	}
	if attached.WorktreePath == detached.WorktreePath {
		t.Fatalf("attached worktree path = %q, want separate path from detached worktree", attached.WorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, attached.WorktreePath, "branch", "--show-current")); got != "feature/fixer" {
		t.Fatalf("attached branch = %q, want feature/fixer", got)
	}
	if _, err := os.Stat(detached.WorktreePath); err != nil {
		t.Fatalf("detached worktree missing after attached create: %v", err)
	}
}

func TestGatewayRecreatesBranchNamedWorktreeAsDetachedAtSamePath(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()

	attached, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("CreateWorktree(attached) error = %v", err)
	}

	detached, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree(detached) error = %v", err)
	}
	if detached.WorktreePath != attached.WorktreePath {
		t.Fatalf("detached path = %q, want %q", detached.WorktreePath, attached.WorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, detached.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("detached branch = %q, want empty", got)
	}
}

func TestGatewayRecreatesStoredAttachedWorktreeWhenOnWrongBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureAndOtherRepo(t)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	runGit(t, worktree.WorktreePath, "checkout", "feature/other")

	restored, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree(recreated) error = %v", err)
	}
	if restored.WorktreePath != worktree.WorktreePath {
		t.Fatalf("restored path = %q, want %q", restored.WorktreePath, worktree.WorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, restored.WorktreePath, "branch", "--show-current")); got != "feature/fixer" {
		t.Fatalf("restored branch = %q, want feature/fixer", got)
	}
}

func TestGatewayRecreatesStoredAttachedWorktreeWhenDetached(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	runGit(t, worktree.WorktreePath, "checkout", "HEAD")

	restored, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree(recreated) error = %v", err)
	}
	if restored.WorktreePath != worktree.WorktreePath {
		t.Fatalf("restored path = %q, want %q", restored.WorktreePath, worktree.WorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, restored.WorktreePath, "branch", "--show-current")); got != "feature/fixer" {
		t.Fatalf("restored branch = %q, want feature/fixer", got)
	}
}

func TestGatewayRestoresDetachedWorktreeFromExpectedPathWithoutStoreRow(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)

	detached, err := fixture.gateway().CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree(detached) error = %v", err)
	}
	statelessGateway := New(Options{GitPath: "git", Now: fixture.now})

	restored, err := statelessGateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree(restored) error = %v", err)
	}
	if normalizeComparablePath(restored.WorktreePath) != normalizeComparablePath(detached.WorktreePath) {
		t.Fatalf("restored path = %q, want %q", restored.WorktreePath, detached.WorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, restored.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("restored detached branch = %q, want empty", got)
	}
}

func TestGatewayRecreatesWorktreeWhenStoredRowPointsAtDeletedPath(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	missingWorktreePath := filepath.Join(fixture.worktreeRoot, "looper-fix-project_1-pr-42-detached")
	metadata := `{"recovered":false}`
	baseBranch := "main"
	if err := fixture.repos.Worktrees.Upsert(ctx, storage.WorktreeRecord{ID: "missing-record", ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreePath: missingWorktreePath, Branch: "feature/fixer", BaseBranch: &baseBranch, Status: "active", MetadataJSON: &metadata, CreatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout), UpdatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout)}); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}

	recreated, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	if normalizeComparablePath(recreated.WorktreePath) != normalizeComparablePath(missingWorktreePath) {
		t.Fatalf("recreated path = %q, want %q", recreated.WorktreePath, missingWorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, recreated.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("recreated detached branch = %q, want empty", got)
	}
}

func TestGatewayIgnoresStoredWorktreesFromDifferentRepoPath(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	otherRepoPath := filepath.Join(fixture.rootDir, "other-repo")
	strayWorktreePath := filepath.Join(fixture.rootDir, "stray-worktree")
	mustMkdirAll(t, otherRepoPath)
	mustMkdirAll(t, strayWorktreePath)
	metadata := `{"recovered":false}`
	baseBranch := "main"
	if err := fixture.repos.Worktrees.Upsert(ctx, storage.WorktreeRecord{ID: "wrong-repo-record", ProjectID: fixture.projectID, RepoPath: otherRepoPath, WorktreePath: strayWorktreePath, Branch: "feature/fixer", BaseBranch: &baseBranch, Status: "active", MetadataJSON: &metadata, CreatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout), UpdatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout)}); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	if worktree.WorktreePath == strayWorktreePath {
		t.Fatalf("CreateWorktree().WorktreePath = stray path %q, want new worktree", strayWorktreePath)
	}
	stored, err := fixture.repos.Worktrees.GetByBranch(ctx, fixture.projectID, "feature/fixer")
	if err != nil {
		t.Fatalf("GetByBranch() error = %v", err)
	}
	if stored == nil || normalizeComparablePath(stored.RepoPath) != normalizeComparablePath(fixture.repoPath) {
		t.Fatalf("stored repo path = %#v, want %q", stored, fixture.repoPath)
	}
}

func TestNormalizeComparablePathTrimsOnlyPrivateSlashPrefix(t *testing.T) {
	t.Parallel()

	if got := normalizeComparablePath("/private/var/tmp/repo"); got != "/var/tmp/repo" {
		t.Fatalf("normalizeComparablePath(/private/var/tmp/repo) = %q, want %q", got, "/var/tmp/repo")
	}
	if got := normalizeComparablePath("/private-repo/worktree"); got != "/private-repo/worktree" {
		t.Fatalf("normalizeComparablePath(/private-repo/worktree) = %q, want %q", got, "/private-repo/worktree")
	}
}

func TestGatewayDoesNotTreatPrimaryCheckoutAsRestorableWorktree(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepoWithoutReturningToMain(t)
	gateway := fixture.gateway()

	restored, err := gateway.RestoreWorktree(ctx, RestoreWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, Branch: "feature/fixer", WorktreeRoot: fixture.worktreeRoot})
	if err != nil {
		t.Fatalf("RestoreWorktree() error = %v", err)
	}
	if restored != nil {
		t.Fatalf("RestoreWorktree() = %#v, want nil", restored)
	}
}

func TestGatewayReusesExistingBranchWorktreeRecordWhenRecreatingWorktree(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	metadata := `{"recovered":false}`
	baseBranch := "main"
	if err := fixture.repos.Worktrees.Upsert(ctx, storage.WorktreeRecord{ID: "existing-record", ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreePath: fixture.repoPath, Branch: "feature/fixer", BaseBranch: &baseBranch, Status: "active", MetadataJSON: &metadata, CreatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout), UpdatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout)}); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	if worktree.ID != "existing-record" {
		t.Fatalf("CreateWorktree().ID = %q, want existing-record", worktree.ID)
	}
	if worktree.WorktreePath == fixture.repoPath {
		t.Fatalf("CreateWorktree().WorktreePath = repo path %q, want separate worktree", fixture.repoPath)
	}
	stored, err := fixture.repos.Worktrees.GetByBranch(ctx, fixture.projectID, "feature/fixer")
	if err != nil {
		t.Fatalf("GetByBranch() error = %v", err)
	}
	if stored == nil || stored.ID != "existing-record" {
		t.Fatalf("stored worktree = %#v, want ID existing-record", stored)
	}
}

func TestGatewayRejectsProtectedBranchWorktreeCreation(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)

	_, err := fixture.gateway().CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:         fixture.projectID,
		RepoPath:          fixture.repoPath,
		WorktreeRoot:      fixture.worktreeRoot,
		Branch:            "main",
		BaseBranch:        "main",
		ProtectedBranches: []string{"main"},
	})
	var protectedErr *ProtectedBranchError
	if err == nil || !errors.As(err, &protectedErr) {
		t.Fatalf("CreateWorktree() error = %v, want *ProtectedBranchError", err)
	}
}

func TestGatewayPrepareWorktreeDetectsRemoteHeadChanges(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "feature/fixer",
		BaseBranch:   "main",
		PRNumber:     42,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	prepared, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: worktree.WorktreePath, Branch: "feature/fixer"})
	if err != nil {
		t.Fatalf("PrepareWorktree(initial) error = %v", err)
	}

	fixture.advanceRemoteBranch(t, "feature/fixer", "remote-update.txt", "changed remotely\n")

	_, err = gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: worktree.WorktreePath, Branch: "feature/fixer", ExpectedHeadSHA: prepared.HeadSHA})
	var remoteHeadErr *RemoteHeadChangedError
	if err == nil || !errors.As(err, &remoteHeadErr) {
		t.Fatalf("PrepareWorktree() error = %v, want *RemoteHeadChangedError", err)
	}
	if remoteHeadErr.ExpectedHeadSHA != prepared.HeadSHA {
		t.Fatalf("RemoteHeadChangedError.ExpectedHeadSHA = %q, want %q", remoteHeadErr.ExpectedHeadSHA, prepared.HeadSHA)
	}
}

func TestGatewayPrepareWorktreeSupportsExplicitRef(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "reviewer/pr-42",
		BaseBranch:   "main",
		PRNumber:     42,
		CheckoutMode: CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	prepared, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: worktree.WorktreePath, Branch: "reviewer/pr-42", Ref: "refs/heads/feature/fixer"})
	if err != nil {
		t.Fatalf("PrepareWorktree() error = %v", err)
	}
	if !prepared.Clean {
		t.Fatal("PrepareWorktree().Clean = false, want true")
	}
	remoteHeadSHA := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", "refs/remotes/origin/feature/fixer"))
	if prepared.HeadSHA != remoteHeadSHA {
		t.Fatalf("PrepareWorktree().HeadSHA = %q, want %q", prepared.HeadSHA, remoteHeadSHA)
	}
}

func TestGatewayBranchExistsTreatsOnlyExitCode1AsMissing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()

	exists, err := gateway.branchExists(ctx, fixture.repoPath, "missing")
	if err != nil {
		t.Fatalf("branchExists(missing) error = %v", err)
	}
	if exists {
		t.Fatal("branchExists(missing) = true, want false")
	}

	nonRepoPath := filepath.Join(fixture.rootDir, "not-a-repo")
	mustMkdirAll(t, nonRepoPath)
	exists, err = gateway.branchExists(ctx, nonRepoPath, "missing")
	if err == nil {
		t.Fatal("branchExists(non-repo) error = nil, want error")
	}
	if exists {
		t.Fatal("branchExists(non-repo) = true, want false")
	}
	var commandErr *shell.CommandExecutionError
	if !errors.As(err, &commandErr) {
		t.Fatalf("branchExists(non-repo) error = %T, want *shell.CommandExecutionError", err)
	}
	if commandErr.Result.ExitCode == 1 {
		t.Fatalf("branchExists(non-repo) exit code = %d, want non-1", commandErr.Result.ExitCode)
	}
}

func TestGatewayResolveDetachedStartPointPropagatesFetchFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	runGit(t, fixture.repoPath, "remote", "set-url", "origin", filepath.Join(fixture.rootDir, "missing-remote.git"))
	gateway := fixture.gateway()

	_, err := gateway.resolveDetachedStartPoint(ctx, CreateWorktreeInput{
		RepoPath:     fixture.repoPath,
		Branch:       "feature/missing",
		BaseBranch:   "main",
		CheckoutMode: CheckoutModeDetached,
	})
	if err == nil {
		t.Fatal("resolveDetachedStartPoint() error = nil, want fetch failure")
	}
	var commandErr *shell.CommandExecutionError
	if !errors.As(err, &commandErr) {
		t.Fatalf("resolveDetachedStartPoint() error = %T, want *shell.CommandExecutionError", err)
	}
	if commandErr.Result.ExitCode == 1 {
		t.Fatalf("resolveDetachedStartPoint() exit code = %d, want non-1 fetch failure", commandErr.Result.ExitCode)
	}
}

func TestGatewayRetriesFetchWhenRemoteTrackingRefWasUpdatedConcurrently(t *testing.T) {
	ctx := context.Background()
	gitPath := writeFakeGit(t, `#!/bin/sh
count_file="$FAKE_GIT_COUNT"
count=0
if [ -f "$count_file" ]; then
	count=$(cat "$count_file")
fi
count=$((count + 1))
printf '%s' "$count" > "$count_file"
if [ "$count" -eq 1 ]; then
	cat >&2 <<'EOF'
From github.com:nexu-io/open-design
 * branch                main       -> FETCH_HEAD
error: cannot lock ref 'refs/remotes/origin/main': is at e64f1d8497409e76387cce3afcd5c51406a4174d but expected 6bf865a43beb8149c8f64a0af297c09c313f9a4a
 ! 6bf865a43..e64f1d849  main       -> origin/main  (unable to update local ref)
EOF
	exit 1
fi
exit 0
`)
	countPath := filepath.Join(t.TempDir(), "git-count")
	gateway := New(Options{GitPath: gitPath})

	err := gateway.runGit(ctx, t.TempDir(), map[string]string{"FAKE_GIT_COUNT": countPath}, "fetch", "origin", "main")
	if err != nil {
		t.Fatalf("runGit(fetch) error = %v, want retry success", err)
	}
	if got := stringsTrimSpace(readFile(t, countPath)); got != "2" {
		t.Fatalf("git attempts = %s, want 2", got)
	}
}

func TestGatewayRemoteBranchExistsTreatsOnlyExitCode1AsMissing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	exists, err := gateway.remoteBranchExists(ctx, fixture.repoPath, "origin", "feature/missing")
	if err != nil {
		t.Fatalf("remoteBranchExists(missing) error = %v", err)
	}
	if exists {
		t.Fatal("remoteBranchExists(missing) = true, want false")
	}

	nonRepoPath := filepath.Join(fixture.rootDir, "not-a-repo")
	mustMkdirAll(t, nonRepoPath)
	exists, err = gateway.remoteBranchExists(ctx, nonRepoPath, "origin", "feature/missing")
	if err == nil {
		t.Fatal("remoteBranchExists(non-repo) error = nil, want error")
	}
	if exists {
		t.Fatal("remoteBranchExists(non-repo) = true, want false")
	}
	var commandErr *shell.CommandExecutionError
	if !errors.As(err, &commandErr) {
		t.Fatalf("remoteBranchExists(non-repo) error = %T, want *shell.CommandExecutionError", err)
	}
	if commandErr.Result.ExitCode == 1 {
		t.Fatalf("remoteBranchExists(non-repo) exit code = %d, want non-1", commandErr.Result.ExitCode)
	}
}

func TestGatewayRestoreWorktreePropagatesHealthCheckFailureForStoredWorktree(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	brokenWorktreePath := filepath.Join(fixture.worktreeRoot, "broken-worktree")
	mustMkdirAll(t, brokenWorktreePath)
	metadata := `{"recovered":false}`
	baseBranch := "main"
	if err := fixture.repos.Worktrees.Upsert(ctx, storage.WorktreeRecord{ID: "broken-record", ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreePath: brokenWorktreePath, Branch: "feature/fixer", BaseBranch: &baseBranch, Status: "active", MetadataJSON: &metadata, CreatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout), UpdatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout)}); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}

	_, err := gateway.RestoreWorktree(ctx, RestoreWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, Branch: "feature/fixer", WorktreeRoot: fixture.worktreeRoot})
	if err == nil {
		t.Fatal("RestoreWorktree() error = nil, want health check failure")
	}
	var commandErr *shell.CommandExecutionError
	if !errors.As(err, &commandErr) {
		t.Fatalf("RestoreWorktree() error = %T, want *shell.CommandExecutionError", err)
	}
	if commandErr.Result.ExitCode == 1 {
		t.Fatalf("RestoreWorktree() exit code = %d, want non-1 health check failure", commandErr.Result.ExitCode)
	}
}

type fixture struct {
	rootDir      string
	repoPath     string
	remotePath   string
	worktreeRoot string
	projectID    string
	coordinator  *storage.SQLiteCoordinator
	repos        *storage.Repositories
	now          func() time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	rootDir := t.TempDir()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(rootDir, "state", "looper.sqlite"), storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := func() time.Time { return time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC) }
	projectID := "project_1"
	repoPath := filepath.Join(rootDir, "repo")
	worktreeRoot := filepath.Join(rootDir, "worktrees")
	remotePath := filepath.Join(rootDir, "remote.git")
	mustMkdirAll(t, repoPath)
	baseBranch := "main"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: repoPath, BaseBranch: &baseBranch, Archived: false, CreatedAt: now().UTC().Format(javaScriptISOStringLayout), UpdatedAt: now().UTC().Format(javaScriptISOStringLayout)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	return &fixture{rootDir: rootDir, repoPath: repoPath, remotePath: remotePath, worktreeRoot: worktreeRoot, projectID: projectID, coordinator: coordinator, repos: repos, now: now}
}

func (f *fixture) gateway() *Gateway {
	return New(Options{GitPath: "git", Repos: f.repos, Now: f.now})
}

func (f *fixture) createMainOnlyRepo(t *testing.T) {
	t.Helper()
	mustMkdirAll(t, f.remotePath)
	runGit(t, f.repoPath, "init", "-b", "main")
	runGit(t, f.remotePath, "init", "--bare")
	configureRepo(t, f.repoPath)
	runGit(t, f.repoPath, "remote", "add", "origin", f.remotePath)
	writeFile(t, filepath.Join(f.repoPath, "README.md"), "hello\n")
	runGit(t, f.repoPath, "add", "README.md")
	runGit(t, f.repoPath, "commit", "-m", "init")
	runGit(t, f.repoPath, "push", "-u", "origin", "main")
}

func (f *fixture) createRemoteRepo(t *testing.T, branch string) {
	t.Helper()
	f.createMainOnlyRepo(t)
	runGit(t, f.repoPath, "checkout", "-b", branch)
	writeFile(t, filepath.Join(f.repoPath, "fix.txt"), "remote change\n")
	runGit(t, f.repoPath, "add", "fix.txt")
	runGit(t, f.repoPath, "commit", "-m", "feature")
	runGit(t, f.repoPath, "push", "-u", "origin", branch)
	runGit(t, f.repoPath, "checkout", "main")
}

func (f *fixture) createUnfetchedRemoteBranch(t *testing.T, branch string) {
	t.Helper()
	clonePath := filepath.Join(f.rootDir, "remote-clone-"+sanitizeBranchName(branch))
	runGit(t, f.rootDir, "clone", f.remotePath, clonePath)
	configureRepo(t, clonePath)
	runGit(t, clonePath, "checkout", "-b", branch)
	writeFile(t, filepath.Join(clonePath, sanitizeBranchName(branch)+".txt"), "remote change\n")
	runGit(t, clonePath, "add", ".")
	runGit(t, clonePath, "commit", "-m", "remote branch")
	runGit(t, clonePath, "push", "-u", "origin", branch)
	if got := stringsTrimSpace(runGitMaybe(t, f.repoPath, "show-ref", "--verify", "refs/remotes/origin/"+branch)); got != "" {
		t.Fatalf("remote tracking ref for %q already exists locally: %q", branch, got)
	}
	if got := stringsTrimSpace(runGitMaybe(t, f.repoPath, "show-ref", "--verify", "refs/heads/"+branch)); got != "" {
		t.Fatalf("local branch %q already exists: %q", branch, got)
	}
}

func (f *fixture) createLocalFeatureRepo(t *testing.T) {
	t.Helper()
	runGit(t, f.repoPath, "init", "-b", "main")
	configureRepo(t, f.repoPath)
	writeFile(t, filepath.Join(f.repoPath, "README.md"), "hello\n")
	runGit(t, f.repoPath, "add", "README.md")
	runGit(t, f.repoPath, "commit", "-m", "init")
	runGit(t, f.repoPath, "checkout", "-b", "feature/fixer")
	runGit(t, f.repoPath, "checkout", "main")
}

func (f *fixture) createLocalFeatureAndOtherRepo(t *testing.T) {
	t.Helper()
	f.createLocalFeatureRepo(t)
	runGit(t, f.repoPath, "checkout", "-b", "feature/other")
	runGit(t, f.repoPath, "checkout", "main")
}

func (f *fixture) createLocalFeatureRepoWithoutReturningToMain(t *testing.T) {
	t.Helper()
	runGit(t, f.repoPath, "init", "-b", "main")
	configureRepo(t, f.repoPath)
	writeFile(t, filepath.Join(f.repoPath, "README.md"), "hello\n")
	runGit(t, f.repoPath, "add", "README.md")
	runGit(t, f.repoPath, "commit", "-m", "init")
	runGit(t, f.repoPath, "checkout", "-b", "feature/fixer")
}

func (f *fixture) advanceRemoteBranch(t *testing.T, branch, fileName, contents string) {
	t.Helper()
	clonePath := filepath.Join(f.rootDir, "remote-clone-"+sanitizeBranchName(branch))
	runGit(t, f.rootDir, "clone", f.remotePath, clonePath)
	configureRepo(t, clonePath)
	runGit(t, clonePath, "checkout", branch)
	writeFile(t, filepath.Join(clonePath, fileName), contents)
	runGit(t, clonePath, "add", fileName)
	runGit(t, clonePath, "commit", "-m", "remote update")
	runGit(t, clonePath, "push", "origin", branch)
}

func configureRepo(t *testing.T, repoPath string) {
	t.Helper()
	runGit(t, repoPath, "config", "user.email", "test@example.com")
	runGit(t, repoPath, "config", "user.name", "Looper Test")
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return string(contents)
}

func runGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	output, err := runGitCommand(cwd, args...)
	if err != nil {
		t.Fatalf("git %v error = %v", args, err)
	}
	return output
}

func runGitMaybe(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	output, _ := runGitCommand(cwd, args...)
	return output
}

func runGitCommand(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := stringsTrimSpace(stderr.String())
		if message == "" {
			message = stringsTrimSpace(stdout.String())
		}
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("%s", message)
	}
	return stdout.String(), nil
}

func writeFakeGit(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	tmpPath := filepath.Join(dir, "git.tmp")
	if err := os.WriteFile(tmpPath, []byte(script), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		t.Fatalf("Chmod(%q) error = %v", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		t.Fatalf("Rename(%q, %q) error = %v", tmpPath, path, err)
	}
	return path
}

func stringsTrimSpace(value string) string {
	return string(bytes.TrimSpace([]byte(value)))
}
