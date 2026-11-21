// Package disk reports filesystem capacity for a path so the scheduler can
// apply disk-aware backpressure (P0-A2): stop claiming NEW runs when the volume
// backing the worktrees is near full, instead of starting work that will fail
// mid-flight with "no such file" / ENOSPC once the disk actually fills. Each
// open-design-style worktree is multiple GB, so an unchecked backlog can drive
// the disk from healthy to full within a few claims.
package disk

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrUnsupported is returned by Stat on platforms that do not expose statfs.
// Callers should treat it as "backpressure unavailable" and fail open (do not
// block work) rather than as a hard error.
var ErrUnsupported = errors.New("disk: capacity stats unsupported on this platform")

// Usage describes a filesystem's capacity, reported the way df does.
type Usage struct {
	// Path is the ancestor directory actually stat'd (see Stat).
	Path string
	// TotalBytes is the total size of the filesystem.
	TotalBytes uint64
	// AvailBytes is space available to this (unprivileged) process.
	AvailBytes uint64
	// UsedBytes is TotalBytes minus all free blocks (including root-reserved).
	UsedBytes uint64
	// UsedPercent is the df-style capacity: used / (used + available) * 100,
	// which matches the number a human sees in `df -h`.
	UsedPercent float64
}

// Stat returns the capacity of the filesystem backing path. If path does not
// exist yet (e.g. a worktree root the daemon has not created on this boot), it
// walks up to the nearest existing ancestor. Because that ancestor sits on the
// same volume, the capacity numbers are identical — this keeps backpressure
// working even before the first worktree is laid down.
func Stat(path string) (Usage, error) {
	resolved := nearestExisting(path)
	if resolved == "" {
		return Usage{}, os.ErrNotExist
	}
	usage, err := statfsUsage(resolved)
	if err != nil {
		return Usage{}, err
	}
	usage.Path = resolved
	return usage, nil
}

// nearestExisting returns path if it exists, otherwise the closest existing
// ancestor directory, or "" if none exists (should be impossible for an
// absolute path since the root always exists).
func nearestExisting(path string) string {
	if path == "" {
		return ""
	}
	current := filepath.Clean(path)
	for {
		if _, err := os.Stat(current); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}
