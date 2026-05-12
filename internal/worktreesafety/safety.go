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
	rel, err := filepath.Rel(normalizedRoot, normalizedPath)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

func normalizePath(path string) string {
	return normalizePathDepth(path, 0)
}

func normalizePathDepth(path string, depth int) string {
	if depth > 255 {
		return filepath.Clean(path)
	}
	abs := path
	if !filepath.IsAbs(abs) {
		wd, err := os.Getwd()
		if err != nil {
			return filepath.Clean(path)
		}
		abs = wd + string(filepath.Separator) + path
	}
	volume := filepath.VolumeName(abs)
	rest := strings.TrimPrefix(abs, volume)
	current := volume + string(filepath.Separator)
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == filepath.Separator })
	for index, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			current = filepath.Dir(current)
			continue
		}

		candidate := filepath.Join(current, part)
		info, err := os.Lstat(candidate)
		if err != nil {
			remaining := append([]string{candidate}, parts[index+1:]...)
			return filepath.Clean(filepath.Join(remaining...))
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = candidate
			continue
		}
		target, err := os.Readlink(candidate)
		if err != nil {
			current = candidate
			continue
		}
		if filepath.IsAbs(target) {
			current = normalizePathDepth(target, depth+1)
		} else {
			current = normalizePathDepth(current+string(filepath.Separator)+target, depth+1)
		}
	}
	return filepath.Clean(current)
}
