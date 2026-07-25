package fixer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeMinimalGitRepoMetadata creates non-remote repository integrity metadata
// (HEAD + objects/ + refs/) that Git 2.43 accepts as a repository root.
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

func TestIsMissingOrUnusableFixerWorktree(t *testing.T) {
	t.Parallel()

	existing := t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")

	// Valid linked-worktree-style checkout: .git file points at a private gitdir
	// that still has required metadata (HEAD + resolvable common repo integrity).
	// Existence of the private dir alone is not enough.
	usable := t.TempDir()
	gitdir := filepath.Join(t.TempDir(), "gitdir")
	common := filepath.Join(t.TempDir(), "common")
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatalf("MkdirAll gitdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitdir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile usable gitdir HEAD: %v", err)
	}
	writeMinimalGitRepoMetadata(t, common)
	if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte(common+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile usable commondir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(usable, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile usable .git: %v", err)
	}

	// Linked gitfile to an existing but empty/corrupt private gitdir (no HEAD).
	// Real git reports "not a git repository"; probe must treat as unusable.
	corruptLinked := t.TempDir()
	emptyGitdir := filepath.Join(t.TempDir(), "empty-gitdir")
	if err := os.MkdirAll(emptyGitdir, 0o755); err != nil {
		t.Fatalf("MkdirAll emptyGitdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptLinked, ".git"), []byte("gitdir: "+emptyGitdir+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile corruptLinked .git: %v", err)
	}

	// Linked private gitdir has HEAD but lost commondir (or it no longer
	// resolves). Real git still reports "not a git repository"; probe must
	// treat as unusable so prepare recreates instead of preserving forever.
	missingCommondir := t.TempDir()
	headOnlyGitdir := filepath.Join(t.TempDir(), "head-only-gitdir")
	if err := os.MkdirAll(headOnlyGitdir, 0o755); err != nil {
		t.Fatalf("MkdirAll headOnlyGitdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(headOnlyGitdir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile headOnly HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(missingCommondir, ".git"), []byte("gitdir: "+headOnlyGitdir+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile missingCommondir .git: %v", err)
	}

	// Linked private gitdir has HEAD + commondir, but common repo is gone.
	danglingCommondir := t.TempDir()
	danglingGitdir := filepath.Join(t.TempDir(), "dangling-gitdir")
	if err := os.MkdirAll(danglingGitdir, 0o755); err != nil {
		t.Fatalf("MkdirAll danglingGitdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(danglingGitdir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dangling HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(danglingGitdir, "commondir"), []byte(filepath.Join(t.TempDir(), "gone-common")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dangling commondir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(danglingCommondir, ".git"), []byte("gitdir: "+danglingGitdir+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile danglingCommondir .git: %v", err)
	}

	// Linked private gitdir has HEAD + commondir, but common only has HEAD
	// (missing objects/refs). Real git reports "not a git repository".
	corruptCommon := t.TempDir()
	corruptCommonGitdir := filepath.Join(t.TempDir(), "corrupt-common-gitdir")
	headOnlyCommon := filepath.Join(t.TempDir(), "head-only-common")
	if err := os.MkdirAll(corruptCommonGitdir, 0o755); err != nil {
		t.Fatalf("MkdirAll corruptCommonGitdir: %v", err)
	}
	if err := os.MkdirAll(headOnlyCommon, 0o755); err != nil {
		t.Fatalf("MkdirAll headOnlyCommon: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptCommonGitdir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile corruptCommon private HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(headOnlyCommon, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile headOnlyCommon HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptCommonGitdir, "commondir"), []byte(headOnlyCommon+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile corruptCommon commondir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptCommon, ".git"), []byte("gitdir: "+corruptCommonGitdir+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile corruptCommon .git: %v", err)
	}

	// Malformed .git file (not a gitdir: pointer). Real git reports
	// "fatal: invalid gitfile format"; probe must treat as unusable.
	malformedGitfile := t.TempDir()
	if err := os.WriteFile(filepath.Join(malformedGitfile, ".git"), []byte("not-a-valid-gitfile\n"), 0o644); err != nil {
		t.Fatalf("WriteFile malformed .git: %v", err)
	}

	// Ordinary checkout with full local repository metadata.
	usableDir := t.TempDir()
	writeMinimalGitRepoMetadata(t, filepath.Join(usableDir, ".git"))

	// Ordinary checkout with only HEAD (missing objects/refs) is unusable.
	headOnlyDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(headOnlyDir, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll headOnlyDir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(headOnlyDir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile headOnlyDir HEAD: %v", err)
	}

	remoteIntegrityText := errors.New("fatal: not a git repository (or any of the parent directories): .git")
	invalidGitfileText := errors.New("fatal: invalid gitfile format: .git")

	cases := []struct {
		name    string
		path    string
		prepErr error
		want    bool
	}{
		{name: "empty_path", path: "", want: true},
		{name: "missing_path", path: missing, want: true},
		{name: "existing_no_err", path: existing, want: false},
		// Directory without local git metadata + integrity-looking prepare text → recreate.
		{name: "not_a_working_tree_no_git", path: existing, prepErr: fmt.Errorf("fatal: %s is not a working tree", existing), want: true},
		{name: "not_a_git_repository_no_git", path: existing, prepErr: remoteIntegrityText, want: true},
		// Same remote/helper wording must NOT force cleanup when local checkout is valid.
		{name: "not_a_git_repository_usable_gitfile", path: usable, prepErr: remoteIntegrityText, want: false},
		{name: "not_a_git_repository_usable_gitdir", path: usableDir, prepErr: remoteIntegrityText, want: false},
		{name: "not_a_working_tree_usable", path: usable, prepErr: fmt.Errorf("fatal: %s is not a working tree", usable), want: false},
		// Existing but empty/corrupt linked gitdir must recreate (not preserve forever).
		{name: "not_a_git_repository_corrupt_linked_gitdir", path: corruptLinked, prepErr: remoteIntegrityText, want: true},
		// HEAD present but missing/dangling/corrupt common must recreate (not preserve forever).
		{name: "not_a_git_repository_missing_commondir", path: missingCommondir, prepErr: remoteIntegrityText, want: true},
		{name: "not_a_git_repository_dangling_commondir", path: danglingCommondir, prepErr: remoteIntegrityText, want: true},
		{name: "not_a_git_repository_corrupt_common_objects", path: corruptCommon, prepErr: remoteIntegrityText, want: true},
		// Ordinary checkout with only HEAD (no objects/refs) must recreate.
		{name: "not_a_git_repository_head_only_gitdir", path: headOnlyDir, prepErr: remoteIntegrityText, want: true},
		// Malformed gitfile + Git's distinct error must recreate (not retry forever).
		{name: "invalid_gitfile_format_malformed", path: malformedGitfile, prepErr: invalidGitfileText, want: true},
		// Usable checkout must not be force-cleaned even if error text mentions gitfile.
		{name: "invalid_gitfile_format_usable", path: usable, prepErr: invalidGitfileText, want: false},
		// Regression: external ssh/fetch text must not classify a live checkout as unusable.
		{name: "ssh_no_such_file", path: existing, prepErr: errors.New("error: cannot run ssh: No such file or directory\nfatal: unable to fork"), want: false},
		{name: "fetch_transport", path: existing, prepErr: errors.New("git fetch origin feature/fix-42: fatal: unable to access remote"), want: false},
		{name: "remote_head_changed", path: existing, prepErr: errors.New("remote head for feature/fix-42 changed: expected a, got b"), want: false},
		// Generic existence phrases without a missing checkout must not force cleanup.
		{name: "does_not_exist_remote_ref", path: existing, prepErr: errors.New("fatal: couldn't find remote ref does not exist"), want: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isMissingOrUnusableFixerWorktree(tc.path, tc.prepErr); got != tc.want {
				t.Fatalf("isMissingOrUnusableFixerWorktree(%q, %v) = %v, want %v", tc.path, tc.prepErr, got, tc.want)
			}
		})
	}
}

func TestClearUnusableFixerWorktreePath(t *testing.T) {
	t.Parallel()

	t.Run("missing_ok", func(t *testing.T) {
		t.Parallel()
		if err := clearUnusableFixerWorktreePath(filepath.Join(t.TempDir(), "gone")); err != nil {
			t.Fatalf("clearUnusableFixerWorktreePath() error = %v", err)
		}
	})

	t.Run("empty_removed", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "empty")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := clearUnusableFixerWorktreePath(path); err != nil {
			t.Fatalf("clearUnusableFixerWorktreePath() error = %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("path still exists after clear, err=%v", err)
		}
	})

	t.Run("populated_preserved", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "populated")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		marker := filepath.Join(path, "keep.txt")
		if err := os.WriteFile(marker, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		err := clearUnusableFixerWorktreePath(path)
		if !errors.Is(err, errUnusableFixerWorktreePreserved) {
			t.Fatalf("error = %v, want errUnusableFixerWorktreePreserved", err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("populated marker missing: %v", err)
		}
	})

	t.Run("only_corrupt_linked_git_removed", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "corrupt-linked")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		emptyGitdir := filepath.Join(t.TempDir(), "empty-gitdir")
		if err := os.MkdirAll(emptyGitdir, 0o755); err != nil {
			t.Fatalf("MkdirAll emptyGitdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: "+emptyGitdir+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile .git: %v", err)
		}
		if err := clearUnusableFixerWorktreePath(path); err != nil {
			t.Fatalf("clearUnusableFixerWorktreePath() error = %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("corrupt-only path still exists after clear, err=%v", err)
		}
	})

	t.Run("only_malformed_gitfile_removed", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "malformed-gitfile")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, ".git"), []byte("garbage-not-gitdir\n"), 0o644); err != nil {
			t.Fatalf("WriteFile .git: %v", err)
		}
		if err := clearUnusableFixerWorktreePath(path); err != nil {
			t.Fatalf("clearUnusableFixerWorktreePath() error = %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("malformed-only path still exists after clear, err=%v", err)
		}
	})
}
