package storage

import (
	"embed"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// EmbeddedMigration is a SQLite migration bundled into the Go daemon binary.
type EmbeddedMigration struct {
	ID       string
	FileName string
	SQL      string
}

//go:embed migrations/*.sql
var embeddedMigrationFiles embed.FS

// EmbeddedMigrations mirrors the current TypeScript SQLite migrations in a
// deterministic order so the Go rewrite can reuse the same schema evolution
// inputs while the migration runner is ported.
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

		fileName := path.Base(entry)
		migrations = append(migrations, EmbeddedMigration{
			ID:       strings.TrimSuffix(fileName, path.Ext(fileName)),
			FileName: fileName,
			SQL:      string(sqlBytes),
		})
	}

	return migrations
}
