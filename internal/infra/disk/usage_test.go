package disk

import (
	"path/filepath"
	"testing"
)

func TestStatReturnsSaneCapacityForExistingPath(t *testing.T) {
	usage, err := Stat(t.TempDir())
	if err == ErrUnsupported {
		t.Skip("statfs unsupported on this platform")
	}
	if err != nil {
		t.Fatalf("Stat returned error: %v", err)
	}
	if usage.TotalBytes == 0 {
		t.Fatalf("expected a non-zero total, got %+v", usage)
	}
	if usage.UsedBytes > usage.TotalBytes {
		t.Fatalf("used (%d) exceeds total (%d)", usage.UsedBytes, usage.TotalBytes)
	}
	if usage.UsedPercent < 0 || usage.UsedPercent > 100 {
		t.Fatalf("used percent out of range: %v", usage.UsedPercent)
	}
}

func TestStatWalksUpToNearestExistingAncestor(t *testing.T) {
	base := t.TempDir()
	// A path several levels deep that does not exist yet — Stat must fall back to
	// the nearest existing ancestor (same volume, same numbers) instead of erroring.
	missing := filepath.Join(base, "does", "not", "exist", "yet")

	usage, err := Stat(missing)
	if err == ErrUnsupported {
		t.Skip("statfs unsupported on this platform")
	}
	if err != nil {
		t.Fatalf("Stat on missing path returned error: %v", err)
	}
	if usage.Path != base {
		t.Fatalf("expected Stat to resolve to the existing ancestor %q, got %q", base, usage.Path)
	}
	if usage.TotalBytes == 0 {
		t.Fatalf("expected a non-zero total for the resolved ancestor, got %+v", usage)
	}
}

func TestStatEmptyPath(t *testing.T) {
	if _, err := Stat(""); err == nil {
		t.Fatal("expected an error for an empty path")
	}
}
