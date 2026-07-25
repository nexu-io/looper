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

// ReadFixerOwnerToken returns the on-disk fixer ownership token, or "" when absent.
func ReadFixerOwnerToken(worktreePath string) string {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return ""
	}
	gitDir, err := resolveWorktreePrivateGitDir(worktreePath, false)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(gitDir, FixerOwnerTokenFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ClearFixerOwnerToken removes any fixer ownership stamp from worktreePath.
// CreateWorktree / RestoreWorktree call this so a later fixer cannot treat path
// equality alone as proof that residual dirt still belongs to its prior prepare
// after another runner has claimed the shared project/PR detached directory.
func ClearFixerOwnerToken(worktreePath string) {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return
	}
	gitDir, err := resolveWorktreePrivateGitDir(worktreePath, false)
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(gitDir, FixerOwnerTokenFile))
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
