package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
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

func TestMigrationRunnerCreatesBackupBeforeApplyingPendingMigrations(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	dbPath := filepath.Join(rootDir, "looper.sqlite")
	backupDir := filepath.Join(rootDir, "backups")
	db, err := OpenSQLiteDB(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSQLiteDB() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close() error = %v", err)
		}
	})

	runner := NewMigrationRunner(db, MigrationRunnerOptions{
		Migrations: []EmbeddedMigration{
			{ID: "0001_init", FileName: "0001_init.sql", SQL: "CREATE TABLE widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL);"},
			{ID: "0002_seed", FileName: "0002_seed.sql", SQL: "INSERT INTO widgets (id, name) VALUES ('w_1', 'alpha');"},
		},
		BackupDir: backupDir,
		Now:       func() time.Time { return time.Date(2026, time.April, 11, 10, 20, 30, 0, time.UTC) },
	})

	result, err := runner.RunPending(context.Background(), RunPendingOptions{RequireBackup: true})
	if err != nil {
		t.Fatalf("runner.RunPending() error = %v", err)
	}

	wantBackupPath := filepath.Join(backupDir, "looper-2026-04-11T10-20-30.000Z.sqlite")
	if result.BackupPath != wantBackupPath {
		t.Fatalf("runner.RunPending().BackupPath = %q, want %q", result.BackupPath, wantBackupPath)
	}

	backupInfo, err := os.Stat(result.BackupPath)
	if err != nil {
		t.Fatalf("os.Stat(%q) error = %v", result.BackupPath, err)
	}
	if backupInfo.Size() <= 0 {
		t.Fatalf("backup size = %d, want > 0", backupInfo.Size())
	}

	backupDB, err := OpenSQLiteDB(context.Background(), result.BackupPath)
	if err != nil {
		t.Fatalf("OpenSQLiteDB(backup) error = %v", err)
	}
	defer backupDB.Close()

	var backupAppliedCount int
	if err := backupDB.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&backupAppliedCount); err != nil {
		t.Fatalf("backupDB.QueryRow(schema_migrations).Scan() error = %v", err)
	}
	if backupAppliedCount != 0 {
		t.Fatalf("backup schema_migrations count = %d, want 0", backupAppliedCount)
	}

	var widgetTableCount int
	if err := backupDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, "widgets").Scan(&widgetTableCount); err != nil {
		t.Fatalf("backupDB.QueryRow(widget table).Scan() error = %v", err)
	}
	if widgetTableCount != 0 {
		t.Fatalf("backup widgets table count = %d, want 0", widgetTableCount)
	}

	var widgetName string
	if err := db.QueryRow(`SELECT name FROM widgets WHERE id = ?`, "w_1").Scan(&widgetName); err != nil {
		t.Fatalf("db.QueryRow(widgets).Scan() error = %v", err)
	}
	if widgetName != "alpha" {
		t.Fatalf("widgets name = %q, want %q", widgetName, "alpha")
	}
}

func TestMigrationRunnerSkipsBackupWhenNoPendingMigrationsRemain(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	backupDir := filepath.Join(rootDir, "backups")
	db := openTestSQLiteDB(t)
	runner := NewMigrationRunner(db, MigrationRunnerOptions{
		Migrations: []EmbeddedMigration{{ID: "0001_init", FileName: "0001_init.sql", SQL: "CREATE TABLE widgets (id TEXT PRIMARY KEY);"}},
		BackupDir:  backupDir,
		Now:        func() time.Time { return time.Date(2026, time.April, 11, 10, 20, 30, 0, time.UTC) },
	})

	if _, err := runner.RunPending(context.Background(), RunPendingOptions{RequireBackup: true}); err != nil {
		t.Fatalf("first runner.RunPending() error = %v", err)
	}

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%q) error = %v", backupDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("backup entry count after first run = %d, want 1", len(entries))
	}

	result, err := runner.RunPending(context.Background(), RunPendingOptions{RequireBackup: true})
	if err != nil {
		t.Fatalf("second runner.RunPending() error = %v", err)
	}
	if result.BackupPath != "" {
		t.Fatalf("second runner.RunPending().BackupPath = %q, want empty", result.BackupPath)
	}

	entries, err = os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%q) after second run error = %v", backupDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("backup entry count after second run = %d, want 1", len(entries))
	}
}

func TestMigrationRunnerBackupCopiesExistingDatabaseState(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	dbPath := filepath.Join(rootDir, "looper.sqlite")
	backupDir := filepath.Join(rootDir, "backups")
	db, err := OpenSQLiteDB(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSQLiteDB() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close() error = %v", err)
		}
	})

	if _, err := db.Exec(`CREATE TABLE widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatalf("db.Exec(CREATE TABLE widgets) error = %v", err)
	}
	if _, err := db.Exec(`INSERT INTO widgets (id, name) VALUES (?, ?)`, "w_1", "alpha"); err != nil {
		t.Fatalf("db.Exec(INSERT widgets) error = %v", err)
	}

	runner := NewMigrationRunner(db, MigrationRunnerOptions{
		BackupDir: backupDir,
		Now:       func() time.Time { return time.Date(2026, time.April, 11, 10, 20, 30, 0, time.UTC) },
	})

	backupPath, err := runner.Backup(context.Background())
	if err != nil {
		t.Fatalf("runner.Backup() error = %v", err)
	}

	wantBackupPath := filepath.Join(backupDir, "looper-2026-04-11T10-20-30.000Z.sqlite")
	if backupPath != wantBackupPath {
		t.Fatalf("runner.Backup() path = %q, want %q", backupPath, wantBackupPath)
	}

	backupDB, err := OpenSQLiteDB(context.Background(), backupPath)
	if err != nil {
		t.Fatalf("OpenSQLiteDB(backup) error = %v", err)
	}
	defer backupDB.Close()

	var widgetName string
	if err := backupDB.QueryRow(`SELECT name FROM widgets WHERE id = ?`, "w_1").Scan(&widgetName); err != nil {
		t.Fatalf("backupDB.QueryRow(widgets).Scan() error = %v", err)
	}
	if widgetName != "alpha" {
		t.Fatalf("backup widgets name = %q, want %q", widgetName, "alpha")
	}
}

func TestMigrationRunnerBackupEscapesSingleQuotesInBackupPath(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	dbPath := filepath.Join(rootDir, "looper.sqlite")
	backupDir := filepath.Join(rootDir, "backups'quoted")
	db, err := OpenSQLiteDB(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSQLiteDB() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close() error = %v", err)
		}
	})

	if _, err := db.Exec(`CREATE TABLE widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatalf("db.Exec(CREATE TABLE widgets) error = %v", err)
	}
	if _, err := db.Exec(`INSERT INTO widgets (id, name) VALUES (?, ?)`, "w_1", "alpha"); err != nil {
		t.Fatalf("db.Exec(INSERT widgets) error = %v", err)
	}

	runner := NewMigrationRunner(db, MigrationRunnerOptions{
		BackupDir: backupDir,
		Now:       func() time.Time { return time.Date(2026, time.April, 11, 10, 20, 30, 0, time.UTC) },
	})

	backupPath, err := runner.Backup(context.Background())
	if err != nil {
		t.Fatalf("runner.Backup() error = %v", err)
	}

	if !strings.Contains(backupPath, "backups'quoted") {
		t.Fatalf("runner.Backup() path = %q, want path containing %q", backupPath, "backups'quoted")
	}

	backupDB, err := OpenSQLiteDB(context.Background(), backupPath)
	if err != nil {
		t.Fatalf("OpenSQLiteDB(backup) error = %v", err)
	}
	defer backupDB.Close()

	var widgetName string
	if err := backupDB.QueryRow(`SELECT name FROM widgets WHERE id = ?`, "w_1").Scan(&widgetName); err != nil {
		t.Fatalf("backupDB.QueryRow(widgets).Scan() error = %v", err)
	}
	if widgetName != "alpha" {
		t.Fatalf("backup widgets name = %q, want %q", widgetName, "alpha")
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

func TestMigrationRunnerHandlesForeignKeyPragmasCorrectly(t *testing.T) {
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

func TestMigrationRunnerAppliesPendingMigrationsOnLegacyDatabasesAcrossVersions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	latestFixtureID := EmbeddedMigrations[len(EmbeddedMigrations)-1].ID
	latestDB := openSQLiteDBAtPath(t, writeLegacyDBFixture(t, latestFixtureID))
	latestSchema := readSQLiteSchemaSnapshot(t, latestDB)

	const legacyAppliedAt = "2026-04-17T12:00:00.000Z"
	const goAppliedAt = "2026-04-17T13:00:00.000Z"

	for version := 1; version <= len(EmbeddedMigrations); version++ {
		version := version
		fixtureID := EmbeddedMigrations[version-1].ID

		t.Run(fixtureID, func(t *testing.T) {
			t.Parallel()

			db := openSQLiteDBAtPath(t, writeLegacyDBFixture(t, fixtureID))
			runner := NewMigrationRunner(db, MigrationRunnerOptions{
				Migrations: EmbeddedMigrations,
				Now: func() time.Time {
					return time.Date(2026, time.April, 17, 13, 0, 0, 0, time.UTC)
				},
			})

			status, err := runner.Status(ctx)
			if err != nil {
				t.Fatalf("runner.Status() before run error = %v", err)
			}

			wantAppliedIDs := migrationIDsForPrefix(version)
			wantPendingIDs := migrationIDsForSuffix(version)
			assertDescriptors(t, status.Available, migrationIDsForPrefix(len(EmbeddedMigrations)))
			assertAppliedMigrations(t, status.Applied, wantAppliedIDs, legacyAppliedAt)
			assertDescriptors(t, status.Pending, wantPendingIDs)

			result, err := runner.RunPending(ctx)
			if err != nil {
				t.Fatalf("runner.RunPending() error = %v", err)
			}

			if !reflect.DeepEqual(result.AppliedIDs, wantPendingIDs) {
				t.Fatalf("runner.RunPending().AppliedIDs = %v, want %v", result.AppliedIDs, wantPendingIDs)
			}
			if !reflect.DeepEqual(result.SkippedIDs, wantAppliedIDs) {
				t.Fatalf("runner.RunPending().SkippedIDs = %v, want %v", result.SkippedIDs, wantAppliedIDs)
			}

			status, err = runner.Status(ctx)
			if err != nil {
				t.Fatalf("runner.Status() after run error = %v", err)
			}

			assertAppliedMigrationsWithSplitTimestamps(t, status.Applied, migrationIDsForPrefix(len(EmbeddedMigrations)), version, legacyAppliedAt, goAppliedAt)
			if len(status.Pending) != 0 {
				t.Fatalf("runner.Status().Pending after run = %v, want empty", status.Pending)
			}

			gotSchema := readSQLiteSchemaSnapshot(t, db)
			if !reflect.DeepEqual(gotSchema, latestSchema) {
				t.Fatalf("sqlite schema after migrating %q fixture = %v, want %v", fixtureID, gotSchema, latestSchema)
			}
		})
	}
}

func TestMigration0008InterruptsStaleRunningRunsBeforeUniqueIndex(t *testing.T) {
	t.Parallel()

	if len(EmbeddedMigrations) < 8 || EmbeddedMigrations[7].ID != "0008_one_running_run_per_loop" {
		t.Fatalf("EmbeddedMigrations[7] = %#v, want 0008_one_running_run_per_loop", EmbeddedMigrations[7])
	}

	ctx := context.Background()
	db := openTestSQLiteDB(t)
	seedRunner := NewMigrationRunner(db, MigrationRunnerOptions{Migrations: EmbeddedMigrations[:7]})
	if _, err := seedRunner.RunPending(ctx); err != nil {
		t.Fatalf("seed RunPending() error = %v", err)
	}

	repos := NewRepositories(db)
	now := "2026-04-17T12:00:00.000Z"
	oldAt := "2026-04-17T10:00:00.000Z"
	newAt := "2026-04-17T11:00:00.000Z"
	newerCreatedAt := "2026-04-17T11:00:00.001Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_migration_0008", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for _, loop := range []LoopRecord{
		{ID: "loop_older_running", Seq: 1, ProjectID: "project_migration_0008", Type: "fixer", TargetType: "pull_request", Status: "completed", CreatedAt: oldAt, UpdatedAt: newAt},
		{ID: "loop_terminal_running", Seq: 2, ProjectID: "project_migration_0008", Type: "fixer", TargetType: "pull_request", Status: "completed", CreatedAt: oldAt, UpdatedAt: newAt},
		{ID: "loop_duplicate_running", Seq: 3, ProjectID: "project_migration_0008", Type: "fixer", TargetType: "pull_request", Status: "running", CreatedAt: oldAt, UpdatedAt: newAt},
	} {
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}
	for _, run := range []RunRecord{
		{ID: "run_older_running", LoopID: "loop_older_running", Status: "running", StartedAt: oldAt, CreatedAt: oldAt, UpdatedAt: oldAt},
		{ID: "run_newer_success", LoopID: "loop_older_running", Status: "success", StartedAt: newAt, EndedAt: &newAt, CreatedAt: newAt, UpdatedAt: newAt},
		{ID: "run_terminal_running", LoopID: "loop_terminal_running", Status: "running", StartedAt: oldAt, CreatedAt: oldAt, UpdatedAt: oldAt},
		{ID: "run_duplicate_old", LoopID: "loop_duplicate_running", Status: "running", StartedAt: oldAt, CreatedAt: oldAt, UpdatedAt: oldAt},
		{ID: "run_duplicate_new", LoopID: "loop_duplicate_running", Status: "running", StartedAt: newAt, CreatedAt: newAt, UpdatedAt: newAt},
		{ID: "run_duplicate_created_later", LoopID: "loop_duplicate_running", Status: "running", StartedAt: newAt, CreatedAt: newerCreatedAt, UpdatedAt: newerCreatedAt},
	} {
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	migrationRunner := NewMigrationRunner(db, MigrationRunnerOptions{Migrations: EmbeddedMigrations[:8]})
	result, err := migrationRunner.RunPending(ctx)
	if err != nil {
		t.Fatalf("RunPending() applying 0008 error = %v", err)
	}
	if !reflect.DeepEqual(result.AppliedIDs, []string{"0008_one_running_run_per_loop"}) {
		t.Fatalf("RunPending().AppliedIDs = %v, want [0008_one_running_run_per_loop]", result.AppliedIDs)
	}

	for _, runID := range []string{"run_older_running", "run_terminal_running", "run_duplicate_old"} {
		run, err := repos.Runs.GetByID(ctx, runID)
		if err != nil {
			t.Fatalf("Runs.GetByID(%s) error = %v", runID, err)
		}
		if run == nil || run.Status != "interrupted" || run.EndedAt == nil {
			t.Fatalf("Runs.GetByID(%s) = %#v, want interrupted with ended_at", runID, run)
		}
	}
	run, err := repos.Runs.GetByID(ctx, "run_duplicate_new")
	if err != nil {
		t.Fatalf("Runs.GetByID(run_duplicate_new) error = %v", err)
	}
	if run == nil || run.Status != "interrupted" || run.EndedAt == nil {
		t.Fatalf("run_duplicate_new = %#v, want interrupted with ended_at", run)
	}
	run, err = repos.Runs.GetByID(ctx, "run_duplicate_created_later")
	if err != nil {
		t.Fatalf("Runs.GetByID(run_duplicate_created_later) error = %v", err)
	}
	if run == nil || run.Status != "running" {
		t.Fatalf("run_duplicate_created_later = %#v, want remaining running run", run)
	}
	if err := repos.Runs.Upsert(ctx, RunRecord{ID: "run_duplicate_extra", LoopID: "loop_duplicate_running", Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err == nil {
		t.Fatal("Runs.Upsert(extra running) error = nil, want unique index failure")
	}
}

func TestMigration0013CancelsDuplicateActiveQueueItemsBeforeUniqueIndex(t *testing.T) {
	t.Parallel()

	if len(EmbeddedMigrations) < 13 || EmbeddedMigrations[12].ID != "0013_active_queue_dedupe" {
		t.Fatalf("EmbeddedMigrations[12] = %#v, want 0013_active_queue_dedupe", EmbeddedMigrations[12])
	}

	ctx := context.Background()
	db := openTestSQLiteDB(t)
	seedRunner := NewMigrationRunner(db, MigrationRunnerOptions{Migrations: EmbeddedMigrations[:12]})
	if _, err := seedRunner.RunPending(ctx); err != nil {
		t.Fatalf("seed RunPending() error = %v", err)
	}

	repos := NewRepositories(db)
	projectID := "project_migration_0013"
	loopID := "loop_migration_0013"
	repoName := "acme/looper"
	prNumber := int64(42)
	oldAt := "2026-04-17T10:00:00.000Z"
	newAt := "2026-04-17T11:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: oldAt, UpdatedAt: newAt}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", Status: "running", CreatedAt: oldAt, UpdatedAt: newAt}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	for _, item := range []QueueItemRecord{
		{ID: "queue_old_running", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: "reviewer:project_migration_0013:loop_migration_0013:acme/looper:42", Priority: QueuePriorityReviewer, Status: "running", AvailableAt: oldAt, Attempts: 1, MaxAttempts: 3, CreatedAt: oldAt, UpdatedAt: oldAt},
		{ID: "queue_new_running", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: "reviewer:project_migration_0013:loop_migration_0013:acme/looper:42", Priority: QueuePriorityReviewer, Status: "running", AvailableAt: newAt, Attempts: 1, MaxAttempts: 3, CreatedAt: newAt, UpdatedAt: newAt},
		{ID: "queue_historical", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: "reviewer:project_migration_0013:loop_migration_0013:acme/looper:42", Priority: QueuePriorityReviewer, Status: "completed", AvailableAt: oldAt, Attempts: 1, MaxAttempts: 3, FinishedAt: &oldAt, CreatedAt: oldAt, UpdatedAt: oldAt},
	} {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	migrationRunner := NewMigrationRunner(db, MigrationRunnerOptions{Migrations: EmbeddedMigrations[:13]})
	result, err := migrationRunner.RunPending(ctx)
	if err != nil {
		t.Fatalf("RunPending() applying 0013 error = %v", err)
	}
	if !reflect.DeepEqual(result.AppliedIDs, []string{"0013_active_queue_dedupe"}) {
		t.Fatalf("RunPending().AppliedIDs = %v, want [0013_active_queue_dedupe]", result.AppliedIDs)
	}

	oldRunning, err := repos.Queue.GetByID(ctx, "queue_old_running")
	if err != nil {
		t.Fatalf("Queue.GetByID(queue_old_running) error = %v", err)
	}
	if oldRunning == nil || oldRunning.Status != "cancelled" || oldRunning.FinishedAt == nil {
		t.Fatalf("queue_old_running = %#v, want cancelled with finished_at", oldRunning)
	}
	newRunning, err := repos.Queue.GetByID(ctx, "queue_new_running")
	if err != nil {
		t.Fatalf("Queue.GetByID(queue_new_running) error = %v", err)
	}
	if newRunning == nil || newRunning.Status != "running" {
		t.Fatalf("queue_new_running = %#v, want remaining running item", newRunning)
	}
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "queue_conflict", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: newRunning.DedupeKey, Priority: QueuePriorityReviewer, Status: "queued", AvailableAt: newAt, Attempts: 0, MaxAttempts: 3, CreatedAt: newAt, UpdatedAt: newAt}); err == nil {
		t.Fatal("Queue.Upsert(active duplicate) error = nil, want unique index failure")
	}
	finishedAt := "2026-04-17T12:00:00.000Z"
	if err := repos.Queue.Complete(ctx, "queue_new_running", finishedAt); err != nil {
		t.Fatalf("Queue.Complete(queue_new_running) error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "queue_after_terminal", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: newRunning.DedupeKey, Priority: QueuePriorityReviewer, Status: "queued", AvailableAt: finishedAt, Attempts: 0, MaxAttempts: 3, CreatedAt: finishedAt, UpdatedAt: finishedAt}); err != nil {
		t.Fatalf("Queue.Upsert(after terminal) error = %v", err)
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

func openSQLiteDBAtPath(t *testing.T, dbPath string) *sql.DB {
	t.Helper()

	db, err := OpenSQLiteDB(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenSQLiteDB(%q) error = %v", dbPath, err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close() error = %v", err)
		}
	})

	return db
}

type sqliteSchemaEntry struct {
	Type    string
	Name    string
	Table   string
	SQLText string
}

func readSQLiteSchemaSnapshot(t *testing.T, db *sql.DB) []sqliteSchemaEntry {
	t.Helper()

	rows, err := db.Query(`
		SELECT type, name, tbl_name, COALESCE(sql, '')
		FROM sqlite_master
		WHERE type IN ('table', 'index', 'trigger', 'view')
		  AND name NOT LIKE 'sqlite_%'
		ORDER BY type ASC, name ASC
	`)
	if err != nil {
		t.Fatalf("db.Query(sqlite_master) error = %v", err)
	}
	defer rows.Close()

	entries := make([]sqliteSchemaEntry, 0)
	for rows.Next() {
		var entry sqliteSchemaEntry
		if err := rows.Scan(&entry.Type, &entry.Name, &entry.Table, &entry.SQLText); err != nil {
			t.Fatalf("rows.Scan(sqlite_master) error = %v", err)
		}
		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err(sqlite_master) error = %v", err)
	}

	return entries
}

func writeLegacyDBFixture(t *testing.T, fixtureID string) string {
	t.Helper()

	encodedFixture, err := os.ReadFile(filepath.Join("testdata", "ts-created-migration-versions", fixtureID+".sqlite.base64"))
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", fixtureID, err)
	}

	decodedFixture, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encodedFixture)))
	if err != nil {
		t.Fatalf("DecodeString(%q) error = %v", fixtureID, err)
	}

	dbPath := filepath.Join(t.TempDir(), fixtureID+".sqlite")
	if err := os.WriteFile(dbPath, decodedFixture, 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", dbPath, err)
	}

	return dbPath
}

func migrationIDsForPrefix(count int) []string {
	ids := make([]string, count)
	for i := 0; i < count; i++ {
		ids[i] = EmbeddedMigrations[i].ID
	}

	return ids
}

func migrationIDsForSuffix(offset int) []string {
	ids := make([]string, len(EmbeddedMigrations)-offset)
	for i := offset; i < len(EmbeddedMigrations); i++ {
		ids[i-offset] = EmbeddedMigrations[i].ID
	}

	return ids
}

func assertAppliedMigrationsWithSplitTimestamps(t *testing.T, got []AppliedMigration, wantIDs []string, typeScriptCount int, typeScriptAppliedAt string, goAppliedAt string) {
	t.Helper()

	if len(got) != len(wantIDs) {
		t.Fatalf("applied migration count = %d, want %d", len(got), len(wantIDs))
	}

	for i, migration := range got {
		if migration.ID != wantIDs[i] {
			t.Fatalf("applied[%d].ID = %q, want %q", i, migration.ID, wantIDs[i])
		}

		wantAppliedAt := goAppliedAt
		if i < typeScriptCount {
			wantAppliedAt = typeScriptAppliedAt
		}

		if migration.AppliedAt != wantAppliedAt {
			t.Fatalf("applied[%d].AppliedAt = %q, want %q", i, migration.AppliedAt, wantAppliedAt)
		}
	}
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
