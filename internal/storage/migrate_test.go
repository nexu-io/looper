package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReadMigrationsFromDirSortsAndFiltersValidFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for name, sqlText := range map[string]string{
		"README.md":         "ignored",
		"0002_seed.sql":     "INSERT INTO widgets (id) VALUES ('w_1');",
		"0001_init.sql":     "CREATE TABLE widgets (id TEXT PRIMARY KEY);",
		"not-a-migration":   "ignored",
		"0003 bad name.sql": "ignored",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(sqlText), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%q) error = %v", name, err)
		}
	}

	migrations, err := ReadMigrationsFromDir(dir)
	if err != nil {
		t.Fatalf("ReadMigrationsFromDir() error = %v", err)
	}

	got := make([]string, len(migrations))
	for i, migration := range migrations {
		got[i] = migration.FileName
	}

	want := []string{"0001_init.sql", "0002_seed.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadMigrationsFromDir() file order = %v, want %v", got, want)
	}

	if migrations[0].ID != "0001_init" || migrations[1].ID != "0002_seed" {
		t.Fatalf("ReadMigrationsFromDir() IDs = [%q %q], want [0001_init 0002_seed]", migrations[0].ID, migrations[1].ID)
	}
}

func TestMigrationRunnerPreservesSchemaMigrationsOrderingAndStatus(t *testing.T) {
	t.Parallel()

	db := openTestSQLiteDB(t)
	runner := NewMigrationRunner(db, MigrationRunnerOptions{
		Migrations: []EmbeddedMigration{
			{ID: "0001_init", FileName: "0001_init.sql", SQL: "CREATE TABLE widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL);"},
			{ID: "0002_seed", FileName: "0002_seed.sql", SQL: "INSERT INTO widgets (id, name) VALUES ('w_1', 'alpha');"},
		},
		Now: func() time.Time { return time.Date(2026, time.April, 11, 10, 20, 30, 0, time.UTC) },
	})

	ctx := context.Background()

	status, err := runner.Status(ctx)
	if err != nil {
		t.Fatalf("runner.Status() error = %v", err)
	}

	assertDescriptors(t, status.Available, []string{"0001_init", "0002_seed"})
	assertDescriptors(t, status.Pending, []string{"0001_init", "0002_seed"})
	if len(status.Applied) != 0 {
		t.Fatalf("runner.Status().Applied = %v, want empty", status.Applied)
	}

	result, err := runner.RunPending(ctx)
	if err != nil {
		t.Fatalf("runner.RunPending() error = %v", err)
	}

	if !reflect.DeepEqual(result.AppliedIDs, []string{"0001_init", "0002_seed"}) {
		t.Fatalf("runner.RunPending().AppliedIDs = %v, want %v", result.AppliedIDs, []string{"0001_init", "0002_seed"})
	}
	if len(result.SkippedIDs) != 0 {
		t.Fatalf("runner.RunPending().SkippedIDs = %v, want empty", result.SkippedIDs)
	}

	status, err = runner.Status(ctx)
	if err != nil {
		t.Fatalf("runner.Status() after run error = %v", err)
	}

	assertAppliedMigrations(t, status.Applied, []string{"0001_init", "0002_seed"}, "2026-04-11T10:20:30.000Z")
	if len(status.Pending) != 0 {
		t.Fatalf("runner.Status().Pending = %v, want empty", status.Pending)
	}

	pending, err := runner.ListPending(ctx)
	if err != nil {
		t.Fatalf("runner.ListPending() error = %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("runner.ListPending() = %v, want empty", pending)
	}

	var name string
	if err := db.QueryRow(`SELECT name FROM widgets WHERE id = ?`, "w_1").Scan(&name); err != nil {
		t.Fatalf("db.QueryRow().Scan() error = %v", err)
	}
	if name != "alpha" {
		t.Fatalf("widgets name = %q, want %q", name, "alpha")
	}
}

func TestMigrationRunnerDoesNotRecordFailedMigration(t *testing.T) {
	t.Parallel()

	db := openTestSQLiteDB(t)
	runner := NewMigrationRunner(db, MigrationRunnerOptions{
		Migrations: []EmbeddedMigration{
			{ID: "0001_init", FileName: "0001_init.sql", SQL: "CREATE TABLE widgets (id TEXT PRIMARY KEY);"},
			{ID: "0002_broken", FileName: "0002_broken.sql", SQL: "INSERT INTO missing_table (id) VALUES ('w_1');"},
		},
		Now: func() time.Time { return time.Date(2026, time.April, 11, 10, 20, 30, 0, time.UTC) },
	})

	_, err := runner.RunPending(context.Background())
	if err == nil {
		t.Fatal("runner.RunPending() error = nil, want non-nil")
	}
	if got := err.Error(); got == "" || !containsAll(got, []string{"Migration failed (0002_broken.sql)", "no such table: missing_table"}) {
		t.Fatalf("runner.RunPending() error = %q, want migration failure for 0002_broken.sql", got)
	}

	status, statusErr := runner.Status(context.Background())
	if statusErr != nil {
		t.Fatalf("runner.Status() error = %v", statusErr)
	}

	assertAppliedMigrations(t, status.Applied, []string{"0001_init"}, "2026-04-11T10:20:30.000Z")
	assertDescriptors(t, status.Pending, []string{"0002_broken"})
}

func TestMigrationRunnerHandlesForeignKeyPragmasLikeTypeScriptRunner(t *testing.T) {
	t.Parallel()

	db := openTestSQLiteDB(t)
	ctx := context.Background()
	initialForeignKeys := readForeignKeysPragmaForTest(t, db)

	runner := NewMigrationRunner(db, MigrationRunnerOptions{
		Migrations: []EmbeddedMigration{
			{ID: "0001_init", FileName: "0001_init.sql", SQL: joinSQL(
				"CREATE TABLE parents (id TEXT PRIMARY KEY, label TEXT NOT NULL);",
				"CREATE TABLE children (id TEXT PRIMARY KEY, parent_id TEXT NOT NULL, label TEXT NOT NULL, FOREIGN KEY (parent_id) REFERENCES parents (id) ON DELETE CASCADE);",
				"INSERT INTO parents (id, label) VALUES ('p_1', 'alpha');",
				"INSERT INTO children (id, parent_id, label) VALUES ('c_1', 'p_1', 'child');",
			)},
			{ID: "0002_rebuild_parents", FileName: "0002_rebuild_parents.sql", SQL: joinSQL(
				"PRAGMA foreign_keys = OFF;",
				"CREATE TABLE parents_v2 (id TEXT PRIMARY KEY, label TEXT NOT NULL, extra TEXT);",
				"INSERT INTO parents_v2 (id, label, extra) SELECT id, label, NULL FROM parents;",
				"DROP TABLE parents;",
				"ALTER TABLE parents_v2 RENAME TO parents;",
				"PRAGMA foreign_keys = ON;",
			)},
		},
	})

	result, err := runner.RunPending(ctx)
	if err != nil {
		t.Fatalf("runner.RunPending() error = %v", err)
	}
	if !reflect.DeepEqual(result.AppliedIDs, []string{"0001_init", "0002_rebuild_parents"}) {
		t.Fatalf("runner.RunPending().AppliedIDs = %v, want %v", result.AppliedIDs, []string{"0001_init", "0002_rebuild_parents"})
	}

	var childID, parentID, label string
	if err := db.QueryRow(`SELECT id, parent_id, label FROM children WHERE id = ?`, "c_1").Scan(&childID, &parentID, &label); err != nil {
		t.Fatalf("db.QueryRow().Scan() error = %v", err)
	}
	if childID != "c_1" || parentID != "p_1" || label != "child" {
		t.Fatalf("child row = [%q %q %q], want [c_1 p_1 child]", childID, parentID, label)
	}

	if got := readForeignKeysPragmaForTest(t, db); got != initialForeignKeys {
		t.Fatalf("PRAGMA foreign_keys = %v after run, want %v", got, initialForeignKeys)
	}
}

func TestMigrationRunnerRollsBackForeignKeyPragmaMigrationSideEffectsOnFailure(t *testing.T) {
	t.Parallel()

	db := openTestSQLiteDB(t)
	ctx := context.Background()
	initialForeignKeys := readForeignKeysPragmaForTest(t, db)

	runner := NewMigrationRunner(db, MigrationRunnerOptions{
		Migrations: []EmbeddedMigration{
			{ID: "0001_init", FileName: "0001_init.sql", SQL: "CREATE TABLE widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL);"},
			{ID: "0002_partial_fail", FileName: "0002_partial_fail.sql", SQL: joinSQL(
				"PRAGMA foreign_keys = OFF;",
				"CREATE TABLE tmp_widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL);",
				"INSERT INTO tmp_widgets (id, name) VALUES ('w_1', 'alpha');",
				"INSERT INTO definitely_missing_table (id) VALUES ('x');",
				"PRAGMA foreign_keys = ON;",
			)},
		},
		Now: func() time.Time { return time.Date(2026, time.April, 11, 10, 20, 30, 0, time.UTC) },
	})

	_, err := runner.RunPending(ctx)
	if err == nil {
		t.Fatal("runner.RunPending() error = nil, want non-nil")
	}
	if got := err.Error(); !containsAll(got, []string{"Migration failed (0002_partial_fail.sql)", "no such table: definitely_missing_table"}) {
		t.Fatalf("runner.RunPending() error = %q, want migration failure for 0002_partial_fail.sql", got)
	}

	var tableName string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, "tmp_widgets").Scan(&tableName)
	if err != sql.ErrNoRows {
		t.Fatalf("tmp_widgets lookup error = %v, want %v", err, sql.ErrNoRows)
	}

	status, statusErr := runner.Status(ctx)
	if statusErr != nil {
		t.Fatalf("runner.Status() error = %v", statusErr)
	}
	assertAppliedMigrations(t, status.Applied, []string{"0001_init"}, "2026-04-11T10:20:30.000Z")
	assertDescriptors(t, status.Pending, []string{"0002_partial_fail"})

	if got := readForeignKeysPragmaForTest(t, db); got != initialForeignKeys {
		t.Fatalf("PRAGMA foreign_keys = %v after failed run, want %v", got, initialForeignKeys)
	}
}

func openTestSQLiteDB(t *testing.T) *sql.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "looper.sqlite")
	db, err := OpenSQLiteDB(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSQLiteDB() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close() error = %v", err)
		}
	})

	return db
}

func readForeignKeysPragmaForTest(t *testing.T, db *sql.DB) bool {
	t.Helper()

	var value int
	if err := db.QueryRow(`PRAGMA foreign_keys;`).Scan(&value); err != nil {
		t.Fatalf("db.QueryRow(PRAGMA foreign_keys).Scan() error = %v", err)
	}

	return value == 1
}

func assertDescriptors(t *testing.T, got []MigrationDescriptor, wantIDs []string) {
	t.Helper()

	gotIDs := make([]string, len(got))
	for i, migration := range got {
		gotIDs[i] = migration.ID
	}

	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("migration IDs = %v, want %v", gotIDs, wantIDs)
	}
}

func assertAppliedMigrations(t *testing.T, got []AppliedMigration, wantIDs []string, wantAppliedAt string) {
	t.Helper()

	gotIDs := make([]string, len(got))
	for i, migration := range got {
		gotIDs[i] = migration.ID
		if migration.AppliedAt != wantAppliedAt {
			t.Fatalf("applied[%d].AppliedAt = %q, want %q", i, migration.AppliedAt, wantAppliedAt)
		}
	}

	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("applied migration IDs = %v, want %v", gotIDs, wantIDs)
	}
}

func joinSQL(statements ...string) string {
	return strings.Join(statements, "\n")
}

func containsAll(s string, parts []string) bool {
	for _, part := range parts {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}
