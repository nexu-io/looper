package worktreesafety

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeMinimalGitRepoMetadata(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatalf("MkdirAll objects: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "refs"), 0o755); err != nil {
		t.Fatalf("MkdirAll refs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile HEAD: %v", err)
	}
}

func TestLocalCheckoutUsable(t *testing.T) {
	t.Parallel()

	hollow := t.TempDir()
	if LocalCheckoutUsable(hollow) {
		t.Fatal("LocalCheckoutUsable(hollow) = true, want false")
	}

	ordinary := t.TempDir()
	writeMinimalGitRepoMetadata(t, filepath.Join(ordinary, ".git"))
	if !LocalCheckoutUsable(ordinary) {
		t.Fatal("LocalCheckoutUsable(ordinary) = false, want true")
	}

	malformed := t.TempDir()
	if err := os.WriteFile(filepath.Join(malformed, ".git"), []byte("not-gitdir\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if LocalCheckoutUsable(malformed) {
		t.Fatal("LocalCheckoutUsable(malformed) = true, want false")
	}
}

func TestClearUnusableManagedPath(t *testing.T) {
	t.Parallel()

	t.Run("empty_removed", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		repo := filepath.Join(t.TempDir(), "repo")
		path := filepath.Join(root, "empty")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := ClearUnusableManagedPath(CheckInput{WorktreePath: path, RepoPath: repo, WorktreeRoot: root}, path); err != nil {
			t.Fatalf("ClearUnusableManagedPath() error = %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("path still exists, err=%v", err)
		}
	})

	t.Run("populated_preserved", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		repo := filepath.Join(t.TempDir(), "repo")
		path := filepath.Join(root, "populated")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		marker := filepath.Join(path, "keep.txt")
		if err := os.WriteFile(marker, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		err := ClearUnusableManagedPath(CheckInput{WorktreePath: path, RepoPath: repo, WorktreeRoot: root}, path)
		if !errors.Is(err, ErrUnusableWorktreePreserved) {
			t.Fatalf("error = %v, want ErrUnusableWorktreePreserved", err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("marker missing: %v", err)
		}
	})

	t.Run("only_malformed_git_removed", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		repo := filepath.Join(t.TempDir(), "repo")
		path := filepath.Join(root, "malformed")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, ".git"), []byte("garbage\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := ClearUnusableManagedPath(CheckInput{WorktreePath: path, RepoPath: repo, WorktreeRoot: root}, path); err != nil {
			t.Fatalf("ClearUnusableManagedPath() error = %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("path still exists, err=%v", err)
		}
	})

	t.Run("tmp_only_preserved", func(t *testing.T) {
		t.Parallel()
		// Generic .tmp is unknown content — preserve for MI (oracle policy).
		root := t.TempDir()
		repo := filepath.Join(t.TempDir(), "repo")
		path := filepath.Join(root, "tmp-only")
		if err := os.MkdirAll(filepath.Join(path, ".tmp"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		err := ClearUnusableManagedPath(CheckInput{WorktreePath: path, RepoPath: repo, WorktreeRoot: root}, path)
		if !errors.Is(err, ErrUnusableWorktreePreserved) {
			t.Fatalf("error = %v, want ErrUnusableWorktreePreserved for .tmp-only hollow", err)
		}
	})
}

func TestLooksLikeLocalIntegrityError(t *testing.T) {
	t.Parallel()
	if !LooksLikeLocalIntegrityError(errors.New("fatal: not a git repository")) {
		t.Fatal("expected true")
	}
	if LooksLikeLocalIntegrityError(errors.New("ssh: connection refused")) {
		t.Fatal("expected false")
	}
}
