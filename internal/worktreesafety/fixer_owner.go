package worktreesafety

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FixerOwnerTokenFile is stored under the worktree's private git dir so it does
// not appear as uncommitted dirt. It proves which fixer prepare generation last
// stamped ownership of this managed checkout.
//
// This is persisted dual-write state: the same token also lives on
// checkpoint.Worktree.OwnerToken. Both copies must stay aligned — write on
// successful clean prepare, reuse (do not rewrite) on same-head dirty adopt so
// a crash before checkpoint persistence cannot desync marker vs checkpoint,
// clear on every non-fixer claim of the path (CreateWorktree / RestoreWorktree
// and prepared-checkpoint reuse), and fail the claim when clear cannot revoke
// authority.
const FixerOwnerTokenFile = "looper-fixer-owner"

// WriteFixerOwnerToken records a fixer-run-specific ownership token for worktreePath.
// The token is written into the worktree-private git directory (never the work tree).
func WriteFixerOwnerToken(worktreePath, token string) error {
	worktreePath = strings.TrimSpace(worktreePath)
	token = strings.TrimSpace(token)
	if worktreePath == "" {
		return fmt.Errorf("worktree path is required")
	}
	if token == "" {
		return fmt.Errorf("owner token is required")
	}
	gitDir, err := resolveWorktreePrivateGitDir(worktreePath, true)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(gitDir, FixerOwnerTokenFile), []byte(token+"\n"), 0o644)
}

// ReadFixerOwnerToken returns the on-disk fixer ownership token.
// Empty string with a nil error means the marker is absent (or the worktree
// path is empty). A non-nil error means the private git dir or marker could
// not be resolved/read (I/O, permission, or non-file marker); callers must
// not treat that as absence — resume rewind and terminal cleanup should
// conservatively preserve the path, and provenance checks must fail closed.
func ReadFixerOwnerToken(worktreePath string) (string, error) {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return "", nil
	}
	gitDir, err := resolveWorktreePrivateGitDir(worktreePath, false)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read fixer owner token for %s: %w", worktreePath, err)
	}
	data, err := os.ReadFile(filepath.Join(gitDir, FixerOwnerTokenFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read fixer owner token for %s: %w", worktreePath, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// ClearFixerOwnerToken removes any fixer ownership stamp from worktreePath.
// Callers that claim a shared worktree for a non-fixer role must invoke this
// and treat a non-nil error as claim failure: a stale token would otherwise
// authorize a later fixer retry to adopt dirt produced by that other runner.
//
// Missing paths / markers are success (already revoked). Resolve or remove I/O
// failures (including read-only private git dirs) are returned.
func ClearFixerOwnerToken(worktreePath string) error {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return nil
	}
	gitDir, err := resolveWorktreePrivateGitDir(worktreePath, false)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("clear fixer owner token for %s: %w", worktreePath, err)
	}
	markerPath := filepath.Join(gitDir, FixerOwnerTokenFile)
	if err := os.Remove(markerPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("clear fixer owner token for %s: %w", worktreePath, err)
	}
	return nil
}

// resolveWorktreePrivateGitDir locates the worktree-private git directory.
// Linked worktrees use a .git file with gitdir:; ordinary checkouts use .git/.
// When createMissing is true and .git is absent, a .git directory is created
// (unit-test / pre-git fixtures only — never overwrites a gitfile).
func resolveWorktreePrivateGitDir(worktreePath string, createMissing bool) (string, error) {
	gitMeta := filepath.Join(worktreePath, ".git")
	info, err := os.Lstat(gitMeta)
	if err != nil {
		if os.IsNotExist(err) && createMissing {
			if mkErr := os.MkdirAll(gitMeta, 0o755); mkErr != nil {
				return "", mkErr
			}
			return gitMeta, nil
		}
		return "", err
	}
	if info.IsDir() {
		return gitMeta, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, statErr := os.Stat(gitMeta)
		if statErr != nil {
			return "", statErr
		}
		if target.IsDir() {
			return gitMeta, nil
		}
	}
	data, err := os.ReadFile(gitMeta)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if len(line) < len(prefix) || !strings.EqualFold(line[:len(prefix)], prefix) {
		return "", fmt.Errorf("worktree %s: .git is not a directory or gitdir pointer", worktreePath)
	}
	gitdir := strings.TrimSpace(line[len(prefix):])
	if gitdir == "" {
		return "", fmt.Errorf("worktree %s: empty gitdir pointer", worktreePath)
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(worktreePath, gitdir)
	}
	return gitdir, nil
}
