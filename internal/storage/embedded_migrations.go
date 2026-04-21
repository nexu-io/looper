package storage

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// EmbeddedMigration is a SQLite migration bundled into the Go daemon binary.
type EmbeddedMigration struct {
	ID       string
	FileName string
	SQL      string
}

var migrationFilePattern = regexp.MustCompile(`^(\d{4}_[a-zA-Z0-9_\-]+)\.sql$`)

//go:embed migrations/*.sql
var embeddedMigrationFiles embed.FS

// EmbeddedMigrations embeds the repository's SQLite migrations in a
// deterministic order for runtime use and migration validation.
var EmbeddedMigrations = mustLoadEmbeddedMigrations()

func mustLoadEmbeddedMigrations() []EmbeddedMigration {
	entries, err := fs.Glob(embeddedMigrationFiles, "migrations/*.sql")
	if err != nil {
		panic("storage: glob embedded migrations: " + err.Error())
	}

	slices.Sort(entries)
	migrations := make([]EmbeddedMigration, 0, len(entries))

	for _, entry := range entries {
		sqlBytes, err := embeddedMigrationFiles.ReadFile(entry)
		if err != nil {
			panic("storage: read embedded migration " + entry + ": " + err.Error())
		}

		migration, err := newEmbeddedMigration(path.Base(entry), string(sqlBytes))
		if err != nil {
			panic("storage: load embedded migration " + entry + ": " + err.Error())
		}

		migrations = append(migrations, migration)
	}

	return migrations
}

func ReadMigrationsFromDir(migrationsDir string) ([]EmbeddedMigration, error) {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir %q: %w", migrationsDir, err)
	}

	fileNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		fileName := entry.Name()
		if !migrationFilePattern.MatchString(fileName) {
			continue
		}

		fileNames = append(fileNames, fileName)
	}

	slices.Sort(fileNames)
	migrations := make([]EmbeddedMigration, 0, len(fileNames))
	for _, fileName := range fileNames {
		sqlBytes, err := os.ReadFile(filepath.Join(migrationsDir, fileName))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", fileName, err)
		}

		migration, err := newEmbeddedMigration(fileName, string(sqlBytes))
		if err != nil {
			return nil, fmt.Errorf("load migration %q: %w", fileName, err)
		}

		migrations = append(migrations, migration)
	}

	return migrations, nil
}

func newEmbeddedMigration(fileName string, sql string) (EmbeddedMigration, error) {
	if !migrationFilePattern.MatchString(fileName) {
		return EmbeddedMigration{}, fmt.Errorf("migration file name %q does not match %s", fileName, migrationFilePattern.String())
	}

	return EmbeddedMigration{
		ID:       strings.TrimSuffix(fileName, path.Ext(fileName)),
		FileName: fileName,
		SQL:      sql,
	}, nil
}
