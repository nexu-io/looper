package worktreesafety

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateAllowsNonexistentChildUnderSymlinkedRoot(t *testing.T) {
	t.Parallel()

	realRoot := filepath.Join(t.TempDir(), "real-worktrees")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(realRoot) error = %v", err)
	}
	symlinkParent := t.TempDir()
	symlinkRoot := filepath.Join(symlinkParent, "worktrees")
	if err := os.Symlink(realRoot, symlinkRoot); err != nil {
		t.Skipf("Symlink() unsupported: %v", err)
	}

	worktreePath := filepath.Join(symlinkRoot, "new-worktree")
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree path existence error = %v, want not exist", err)
	}
	if err := Validate(CheckInput{WorktreePath: worktreePath, RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: realRoot}); err != nil {
		t.Fatalf("Validate() error = %v, want symlinked nonexistent child accepted", err)
	}
}

func TestValidateRejectsSiblingPrefixOutsideRoot(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	sibling := filepath.Join(parent, "root-other", "wt")
	if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
		t.Fatalf("MkdirAll(sibling parent) error = %v", err)
	}
	if err := Validate(CheckInput{WorktreePath: sibling, RepoPath: filepath.Join(parent, "repo"), WorktreeRoot: root}); err == nil {
		t.Fatal("Validate() error = nil, want sibling outside root rejected")
	}
}

func TestValidateRejectsRepoPathThroughSymlink(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(repoPath, link); err != nil {
		t.Skipf("Symlink() unsupported: %v", err)
	}
	if err := Validate(CheckInput{WorktreePath: link, RepoPath: repoPath}); err == nil {
		t.Fatal("Validate() error = nil, want repo path rejection")
	}
}

func TestValidateAllowsExistingWorktreeUnderSymlinkedRoot(t *testing.T) {
	t.Parallel()

	realRoot := filepath.Join(t.TempDir(), "real-worktrees")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(realRoot) error = %v", err)
	}
	symlinkRoot := filepath.Join(t.TempDir(), "worktrees")
	if err := os.Symlink(realRoot, symlinkRoot); err != nil {
		t.Skipf("Symlink() unsupported: %v", err)
	}
	worktreePath := filepath.Join(symlinkRoot, "existing-worktree")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("MkdirAll(worktreePath) error = %v", err)
	}
	if err := Validate(CheckInput{WorktreePath: worktreePath, RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: realRoot}); err != nil {
		t.Fatalf("Validate() error = %v, want existing symlinked child accepted", err)
	}
}

func TestValidateRejectsSymlinkDotDotEscape(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("MkdirAll(outside) error = %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(outside, "dir"), link); err != nil {
		t.Skipf("Symlink() unsupported: %v", err)
	}
	worktreePath := root + string(filepath.Separator) + "link" + string(filepath.Separator) + ".." + string(filepath.Separator) + "evil-wt"
	if err := Validate(CheckInput{WorktreePath: worktreePath, RepoPath: filepath.Join(parent, "repo"), WorktreeRoot: root}); err == nil {
		t.Fatal("Validate() error = nil, want symlink plus dot-dot escape rejected")
	}
}

func TestValidateRejectsNestedRelativeSymlinkDotDotEscape(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "dir"), 0o755); err != nil {
		t.Fatalf("MkdirAll(outside dir) error = %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "dir"), filepath.Join(root, "a")); err != nil {
		t.Skipf("Symlink(a) unsupported: %v", err)
	}
	if err := os.Symlink("a"+string(filepath.Separator)+".."+string(filepath.Separator)+"evil-wt", filepath.Join(root, "link")); err != nil {
		t.Skipf("Symlink(link) unsupported: %v", err)
	}
	if err := Validate(CheckInput{WorktreePath: filepath.Join(root, "link"), RepoPath: filepath.Join(parent, "repo"), WorktreeRoot: root}); err == nil {
		t.Fatal("Validate() error = nil, want nested relative symlink plus dot-dot escape rejected")
	}
}
