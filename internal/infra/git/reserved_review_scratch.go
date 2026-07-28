package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nexu-io/looper/internal/infra/shell"
)

// reservedReviewerScratchBaseName matches disposable reviewer submit scratch only.
// Authority: the Looper reviewer contract reserves top-level, untracked regular
// files matching this grammar in managed reviewer worktrees as disposable
// submission scratch. That contract—not git status inference—is the authority
// to delete them during reviewer prepare.
//
// Do not broaden this to staged, tracked, nested, case-folded, or preserved
// artifacts; those remain ordinary dirt. Quarantine, generation IDs, status
// filtering, and Commit interception are redesign requests, not incremental fixes.
var reservedReviewerScratchBaseName = regexp.MustCompile(`^\.looper-review-[A-Za-z0-9_-]+\.json$`)

// ScrubReservedReviewerScratch removes disposable reviewer submit scratch from a
// managed worktree root immediately before ordinary PrepareWorktree cleanliness.
//
// It enumerates the worktree root directly (not via git status). Only regular
// files matching reservedReviewerScratchBaseName that are absent from both the
// Git index and HEAD tree are deleted. Symlinks and directories are left alone.
// Files tracked by HEAD but staged for index removal (e.g. after
// `git rm --cached`) remain ordinary dirt and are preserved. Delete failures
// fail closed.
//
// gitPath must be the configured Git executable (tools.gitPath); callers must
// not hard-code "git" when a non-PATH binary is configured.
func ScrubReservedReviewerScratch(ctx context.Context, gitPath, worktreePath string) error {
	worktreePath = strings.TrimSpace(worktreePath)
	if worktreePath == "" {
		return fmt.Errorf("worktree path is required")
	}
	gitPath = strings.TrimSpace(gitPath)
	if gitPath == "" {
		gitPath = "git"
	}

	entries, err := os.ReadDir(worktreePath)
	if err != nil {
		return fmt.Errorf("read worktree root for reserved reviewer scratch: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if !reservedReviewerScratchBaseName.MatchString(name) {
			continue
		}
		// Lstat so we never follow symlinks; only plain regular files are disposable.
		fullPath := filepath.Join(worktreePath, name)
		info, lerr := os.Lstat(fullPath)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				continue
			}
			return fmt.Errorf("stat reserved reviewer scratch %q: %w", name, lerr)
		}
		if !info.Mode().IsRegular() {
			continue
		}

		preserve, terr := shouldPreserveReservedScratch(ctx, gitPath, worktreePath, name)
		if terr != nil {
			return terr
		}
		if preserve {
			continue
		}
		if err := os.Remove(fullPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("remove reserved reviewer scratch %q: %w", name, err)
		}
	}
	return nil
}

// ScrubReservedReviewerScratch removes disposable reviewer submit scratch using
// this gateway's configured Git executable.
func (g *Gateway) ScrubReservedReviewerScratch(ctx context.Context, worktreePath string) error {
	return ScrubReservedReviewerScratch(ctx, g.gitPath, worktreePath)
}

// shouldPreserveReservedScratch reports whether a reserved-name file must be
// left as ordinary dirt: present in the current index and/or the committed HEAD
// tree. Index-only checks miss `git rm --cached` (tracked by HEAD, dropped from
// the index while still on disk).
func shouldPreserveReservedScratch(ctx context.Context, gitPath, worktreePath, name string) (bool, error) {
	inIndex, err := gitListsPath(ctx, gitPath, worktreePath, name, []string{"ls-files", "--full-name", "--", name})
	if err != nil {
		return false, fmt.Errorf("probe index for reserved reviewer scratch %q: %w", name, err)
	}
	if inIndex {
		return true, nil
	}
	inHead, err := gitListsPath(ctx, gitPath, worktreePath, name, []string{"ls-tree", "--name-only", "HEAD", "--", name})
	if err != nil {
		return false, fmt.Errorf("probe HEAD for reserved reviewer scratch %q: %w", name, err)
	}
	return inHead, nil
}

func gitListsPath(ctx context.Context, gitPath, worktreePath, name string, args []string) (bool, error) {
	result, err := shell.Run(ctx, shell.Options{
		Command: gitPath,
		Args:    append([]string{"-C", worktreePath}, args...),
		CWD:     worktreePath,
	})
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

// IsReservedReviewerScratchBaseName reports whether name matches the disposable
// reviewer scratch grammar (exported for focused tests).
func IsReservedReviewerScratchBaseName(name string) bool {
	return reservedReviewerScratchBaseName.MatchString(name)
}
