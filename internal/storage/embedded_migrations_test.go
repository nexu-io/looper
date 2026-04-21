package storage

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestEmbeddedMigrationsMirrorSourceFiles(t *testing.T) {
	t.Parallel()

	if len(EmbeddedMigrations) == 0 {
		t.Fatal("EmbeddedMigrations is empty")
	}

	sourceDir := filepath.Join("migrations")
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%q) error = %v", sourceDir, err)
	}

	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return compareStrings(a.Name(), b.Name())
	})

	wantCount := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}

		wantCount++
		if wantCount-1 >= len(EmbeddedMigrations) {
			t.Fatalf("len(EmbeddedMigrations) = %d, want at least %d entries to mirror %q", len(EmbeddedMigrations), wantCount, entry.Name())
		}

		migration := EmbeddedMigrations[wantCount-1]
		if migration.FileName != entry.Name() {
			t.Fatalf("EmbeddedMigrations[%d].FileName = %q, want %q", wantCount-1, migration.FileName, entry.Name())
		}

		wantSQL, err := os.ReadFile(filepath.Join(sourceDir, entry.Name()))
		if err != nil {
			t.Fatalf("os.ReadFile(%q) error = %v", entry.Name(), err)
		}

		if migration.SQL != string(wantSQL) {
			t.Fatalf("EmbeddedMigrations[%d].SQL does not match %q", wantCount-1, entry.Name())
		}

		wantID := entry.Name()[:len(entry.Name())-len(filepath.Ext(entry.Name()))]
		if migration.ID != wantID {
			t.Fatalf("EmbeddedMigrations[%d].ID = %q, want %q", wantCount-1, migration.ID, wantID)
		}
	}

	if len(EmbeddedMigrations) != wantCount {
		t.Fatalf("len(EmbeddedMigrations) = %d, want %d", len(EmbeddedMigrations), wantCount)
	}
}

func compareStrings(a string, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
