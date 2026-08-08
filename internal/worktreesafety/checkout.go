package worktreesafety

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnusableWorktreePreserved signals that an unusable managed worktree path
// still holds content that must not be deleted automatically. Callers should
// park the loop for manual intervention rather than retrying forever.
var ErrUnusableWorktreePreserved = errors.New("unusable worktree path preserved")

// LocalCheckoutUsable reports whether path has local git metadata without
// contacting remotes. Linked worktrees use a .git file (gitdir pointer);
// ordinary checkouts use a .git directory. Metadata presence alone is not
// enough: ordinary repos and the linked common repository need full non-remote
// integrity (HEAD + objects/ + refs/), and linked private gitdirs need HEAD
// plus a resolvable usable common repo via commondir.
//
// Authority: local filesystem git metadata only. This answers "is this path a
// usable checkout?" — not "may we delete arbitrary contents?".
func LocalCheckoutUsable(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	gitMeta := filepath.Join(path, ".git")
	info, err := os.Lstat(gitMeta)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return localGitRepositoryMetadataUsable(gitMeta)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Stat(gitMeta)
		if err != nil {
			return false
		}
		if target.IsDir() {
			return localGitRepositoryMetadataUsable(gitMeta)
		}
	}
	data, err := os.ReadFile(gitMeta)
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		return false
	}
	const prefix = "gitdir:"
	if len(line) < len(prefix) || !strings.EqualFold(line[:len(prefix)], prefix) {
		// Malformed gitfile: real Git rejects non-gitdir: content as
		// "invalid gitfile format".
		return false
	}
	gitdir := strings.TrimSpace(line[len(prefix):])
	if gitdir == "" {
		return false
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(path, gitdir)
	}
	if _, err = os.Stat(filepath.Join(gitdir, "HEAD")); err != nil {
		return false
	}
	return linkedPrivateGitdirCommonUsable(gitdir)
}

// LooksLikeLocalIntegrityError reports whether err text resembles a local
// checkout integrity failure. Remote helpers can emit the same phrases, so
// callers must confirm with LocalCheckoutUsable before force-cleaning.
func LooksLikeLocalIntegrityError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not a working tree") ||
		strings.Contains(msg, "not a git repository") ||
		strings.Contains(msg, "invalid gitfile format")
}

// ClearUnusableManagedPath removes empty or metadata-only unusable leftovers at
// a managed worktree path so CreateWorktree can reuse it. Deletion requires:
//   - Validate(input) succeeds (path under worktree root, not repo path)
//   - directory is empty, OR contains only unusable local git metadata
//
// Any other content (including generic .tmp or agent output) is preserved and
// returns ErrUnusableWorktreePreserved.
func ClearUnusableManagedPath(input CheckInput, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	// Always bind deletion to the caller's safety context.
	check := input
	check.WorktreePath = path
	if err := Validate(check); err != nil {
		return err
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("worktree path %s is unusable and not empty; manual intervention required: %w", path, ErrUnusableWorktreePreserved)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if onlyUnusableLocalGitMetadata(path, entries) {
		if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return fmt.Errorf("worktree path %s is unusable and not empty; manual intervention required: %w", path, ErrUnusableWorktreePreserved)
}

// EnsureUnusableManagedPathCleared clears path only when it exists and is not a
// usable local checkout. Usable checkouts are left alone for git worktree add.
func EnsureUnusableManagedPathCleared(input CheckInput, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if LocalCheckoutUsable(path) {
		return nil
	}
	return ClearUnusableManagedPath(input, path)
}

// ErrUsableCheckoutRefusesClear signals that an operator clear was requested for
// a path that is still a usable local checkout. Callers should use discard
// (git reset/clean) or plain retry instead of RemoveAll.
var ErrUsableCheckoutRefusesClear = errors.New("usable checkout refuses clear")

// ClearManagedUnusablePathForOperator removes a managed worktree path that is
// not a usable local checkout. Unlike ClearUnusableManagedPath (runner auto-
// clear of empty/metadata-only leftovers), this allows full RemoveAll of
// non-empty hollow leftovers after explicit operator confirmation.
//
// Authority: LocalCheckoutUsable + Validate (managed path under worktree root).
// Not agent output. Never used by runners automatically.
//
// Behavior:
//   - missing path → nil (already clear)
//   - usable checkout → ErrUsableCheckoutRefusesClear
//   - unusable managed path → os.RemoveAll after Validate
func ClearManagedUnusablePathForOperator(input CheckInput, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	check := input
	check.WorktreePath = path
	if err := Validate(check); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if LocalCheckoutUsable(path) {
		return fmt.Errorf("worktree path %s is a usable checkout; use discard or plain retry, not clear: %w", path, ErrUsableCheckoutRefusesClear)
	}
	if !info.IsDir() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func localGitRepositoryMetadataUsable(dir string) bool {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return false
	}
	objects, err := os.Stat(filepath.Join(dir, "objects"))
	if err != nil || !objects.IsDir() {
		return false
	}
	refs, err := os.Stat(filepath.Join(dir, "refs"))
	if err != nil || !refs.IsDir() {
		return false
	}
	return true
}

func linkedPrivateGitdirCommonUsable(gitdir string) bool {
	data, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return false
	}
	common := strings.TrimSpace(string(data))
	if common == "" {
		return false
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	common = filepath.Clean(common)
	return localGitRepositoryMetadataUsable(common)
}

// onlyUnusableLocalGitMetadata is true when path holds nothing but a non-usable
// .git file/dir. Any other entry is treated as possible agent dirt.
func onlyUnusableLocalGitMetadata(path string, entries []os.DirEntry) bool {
	if len(entries) != 1 || entries[0].Name() != ".git" {
		return false
	}
	return !LocalCheckoutUsable(path)
}
