package worktreesafety

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type CheckInput struct {
	WorktreePath string
	RepoPath     string
	WorktreeRoot string
}

func Validate(input CheckInput) error {
	worktreePath := strings.TrimSpace(input.WorktreePath)
	if worktreePath == "" {
		return fmt.Errorf("unsafe worktree path: path is required")
	}

	if samePath(worktreePath, input.RepoPath) {
		return fmt.Errorf("unsafe worktree path %q: path must not equal project repo path", worktreePath)
	}

	root := strings.TrimSpace(input.WorktreeRoot)
	if root != "" {
		if samePath(worktreePath, root) {
			return fmt.Errorf("unsafe worktree path %q: path must not equal worktree root", worktreePath)
		}
		if !withinRoot(worktreePath, root) {
			return fmt.Errorf("unsafe worktree path %q: path must be under worktree root %q", worktreePath, root)
		}
	}

	return nil
}

func IsSafe(input CheckInput) bool {
	return Validate(input) == nil
}

func samePath(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return normalizePath(a) == normalizePath(b)
}

func withinRoot(path, root string) bool {
	path = strings.TrimSpace(path)
	root = strings.TrimSpace(root)
	if path == "" || root == "" {
		return false
	}
	normalizedPath := normalizePath(path)
	normalizedRoot := normalizePath(root)
	if normalizedPath == normalizedRoot {
		return true
	}
	return strings.HasPrefix(normalizedPath, normalizedRoot+string(filepath.Separator))
}

func normalizePath(path string) string {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		abs = filepath.Clean(path)
	}
	if evaluated, err := os.Readlink(abs); err == nil && !filepath.IsAbs(evaluated) {
		abs = filepath.Join(filepath.Dir(abs), evaluated)
	} else if err == nil {
		abs = evaluated
	}
	if evaluated, err := filepath.EvalSymlinks(abs); err == nil {
		abs = evaluated
	}
	return filepath.Clean(abs)
}
