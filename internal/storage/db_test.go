package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestOpenSQLiteCoordinatorCreatesParentDirAndAppliesPragmas(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "nested", "runtime", "looper.sqlite")
	coordinator, err := OpenSQLiteCoordinator(context.Background(), dbPath, SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if err := coordinator.Close(); err != nil {
			t.Fatalf("coordinator.Close() error = %v", err)
		}
	})

	if got := coordinator.DB().Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("db.Stats().MaxOpenConnections = %d, want 1", got)
	}

	if got := readStringPragmaForTest(t, coordinator.DB(), `PRAGMA journal_mode;`); got != "wal" {
		t.Fatalf("PRAGMA journal_mode = %q, want %q", got, "wal")
	}

	if got := readIntPragmaForTest(t, coordinator.DB(), `PRAGMA busy_timeout;`); got != sqliteBusyTimeoutMilliseconds {
		t.Fatalf("PRAGMA busy_timeout = %d, want %d", got, sqliteBusyTimeoutMilliseconds)
	}

	if got := readForeignKeysPragmaForTest(t, coordinator.DB()); !got {
		t.Fatal("PRAGMA foreign_keys = false, want true")
	}
}

func TestOpenSQLiteCoordinatorBuildsMigrationRunner(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	coordinator, err := OpenSQLiteCoordinator(context.Background(), filepath.Join(rootDir, "looper.sqlite"), SQLiteCoordinatorOptions{
		Migrations: []EmbeddedMigration{{ID: "0001_init", FileName: "0001_init.sql", SQL: "CREATE TABLE widgets (id TEXT PRIMARY KEY);"}},
		BackupDir:  filepath.Join(rootDir, "backups"),
		Now:        func() time.Time { return time.Date(2026, time.April, 17, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if err := coordinator.Close(); err != nil {
			t.Fatalf("coordinator.Close() error = %v", err)
		}
	})

	result, err := coordinator.MigrationRunner().RunPending(context.Background())
	if err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}

	if len(result.AppliedIDs) != 1 || result.AppliedIDs[0] != "0001_init" {
		t.Fatalf("MigrationRunner().RunPending().AppliedIDs = %v, want [0001_init]", result.AppliedIDs)
	}

	backupPath, err := coordinator.Backup(context.Background())
	if err != nil {
		t.Fatalf("coordinator.Backup() error = %v", err)
	}
	if backupPath == "" {
		t.Fatal("coordinator.Backup() path = empty, want non-empty")
	}
}

func TestSQLiteCoordinatorWithTransactionCommitsChanges(t *testing.T) {
	t.Parallel()

	coordinator := openTestSQLiteCoordinator(t)
	ctx := context.Background()

	if _, err := coordinator.DB().ExecContext(ctx, `CREATE TABLE widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatalf("db.ExecContext(CREATE TABLE) error = %v", err)
	}

	if err := coordinator.WithTransaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO widgets (id, name) VALUES (?, ?)`, "w_1", "alpha")
		return err
	}); err != nil {
		t.Fatalf("coordinator.WithTransaction() error = %v", err)
	}

	var name string
	if err := coordinator.DB().QueryRowContext(ctx, `SELECT name FROM widgets WHERE id = ?`, "w_1").Scan(&name); err != nil {
		t.Fatalf("db.QueryRowContext().Scan() error = %v", err)
	}
	if name != "alpha" {
		t.Fatalf("widgets.name = %q, want %q", name, "alpha")
	}
}

func TestSQLiteCoordinatorWithTransactionRollsBackOnError(t *testing.T) {
	t.Parallel()

	coordinator := openTestSQLiteCoordinator(t)
	ctx := context.Background()

	if _, err := coordinator.DB().ExecContext(ctx, `CREATE TABLE widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatalf("db.ExecContext(CREATE TABLE) error = %v", err)
	}

	wantErr := errors.New("abort transaction")
	err := coordinator.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO widgets (id, name) VALUES (?, ?)`, "w_1", "alpha"); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("coordinator.WithTransaction() error = %v, want %v", err, wantErr)
	}

	var count int
	if err := coordinator.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM widgets`).Scan(&count); err != nil {
		t.Fatalf("db.QueryRowContext().Scan() error = %v", err)
	}
	if count != 0 {
		t.Fatalf("widgets row count = %d, want 0", count)
	}
}

func TestWithTransactionValueReturnsResult(t *testing.T) {
	t.Parallel()

	coordinator := openTestSQLiteCoordinator(t)
	ctx := context.Background()

	if _, err := coordinator.DB().ExecContext(ctx, `CREATE TABLE widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatalf("db.ExecContext(CREATE TABLE) error = %v", err)
	}

	got, err := WithTransactionValue(ctx, coordinator.DB(), nil, func(tx *sql.Tx) (string, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO widgets (id, name) VALUES (?, ?)`, "w_1", "alpha"); err != nil {
			return "", err
		}

		var name string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM widgets WHERE id = ?`, "w_1").Scan(&name); err != nil {
			return "", err
		}

		return name, nil
	})
	if err != nil {
		t.Fatalf("WithTransactionValue() error = %v", err)
	}
	if got != "alpha" {
		t.Fatalf("WithTransactionValue() = %q, want %q", got, "alpha")
	}
}

func TestSQLiteCoordinatorSerializesConcurrentTransactionsWithoutDataLoss(t *testing.T) {
	t.Parallel()

	coordinator := openTestSQLiteCoordinator(t)
	ctx := context.Background()

	if _, err := coordinator.DB().ExecContext(ctx, `CREATE TABLE counters (id INTEGER PRIMARY KEY, value INTEGER NOT NULL)`); err != nil {
		t.Fatalf("db.ExecContext(CREATE TABLE counters) error = %v", err)
	}
	if _, err := coordinator.DB().ExecContext(ctx, `INSERT INTO counters (id, value) VALUES (1, 0)`); err != nil {
		t.Fatalf("db.ExecContext(INSERT counters) error = %v", err)
	}
	if _, err := coordinator.DB().ExecContext(ctx, `CREATE TABLE widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatalf("db.ExecContext(CREATE TABLE widgets) error = %v", err)
	}

	const goroutines = 48
	const incrementsPerGoroutine = 25

	errCh := make(chan error, goroutines)
	var wg sync.WaitGroup
	for worker := 0; worker < goroutines; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()

			for increment := 0; increment < incrementsPerGoroutine; increment++ {
				if err := coordinator.WithTransaction(ctx, func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(ctx, `UPDATE counters SET value = value + 1 WHERE id = 1`); err != nil {
						return fmt.Errorf("worker %d increment %d update counter: %w", worker, increment, err)
					}

					_, err := tx.ExecContext(ctx, `INSERT INTO widgets (id, name) VALUES (?, ?)`, fmt.Sprintf("w_%d_%d", worker, increment), "ok")
					if err != nil {
						return fmt.Errorf("worker %d increment %d insert widget: %w", worker, increment, err)
					}

					return nil
				}); err != nil {
					errCh <- err
					return
				}
			}
		}(worker)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent transaction error = %v", err)
		}
	}

	want := goroutines * incrementsPerGoroutine

	var counterValue int
	if err := coordinator.DB().QueryRowContext(ctx, `SELECT value FROM counters WHERE id = 1`).Scan(&counterValue); err != nil {
		t.Fatalf("db.QueryRowContext(counter).Scan() error = %v", err)
	}
	if counterValue != want {
		t.Fatalf("counter value = %d, want %d", counterValue, want)
	}

	var widgetCount int
	if err := coordinator.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM widgets`).Scan(&widgetCount); err != nil {
		t.Fatalf("db.QueryRowContext(widgets count).Scan() error = %v", err)
	}
	if widgetCount != want {
		t.Fatalf("widgets row count = %d, want %d", widgetCount, want)
	}
}

func TestSQLiteCoordinatorPersistsDataAcrossCloseAndReopenWithEmbeddedMigrations(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dbPath := filepath.Join(root, "looper.sqlite")
	ctx := context.Background()

	coordinator := openMigratedSQLiteCoordinatorAtPath(t, dbPath)

	now := "2026-04-17T12:00:00.000Z"
	if _, err := coordinator.DB().ExecContext(ctx, `
		INSERT INTO projects (id, name, repo_path, archived, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, "project_persist", "Looper", "/tmp/looper", 0, now, now); err != nil {
		t.Fatalf("db.ExecContext(INSERT project) error = %v", err)
	}

	if err := coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() error = %v", err)
	}

	reopened := openMigratedSQLiteCoordinatorAtPath(t, dbPath)

	var projectName string
	if err := reopened.DB().QueryRowContext(ctx, `SELECT name FROM projects WHERE id = ?`, "project_persist").Scan(&projectName); err != nil {
		t.Fatalf("db.QueryRowContext(project).Scan() error = %v", err)
	}
	if projectName != "Looper" {
		t.Fatalf("projects.name = %q, want %q", projectName, "Looper")
	}

	status, err := reopened.MigrationRunner().Status(ctx)
	if err != nil {
		t.Fatalf("MigrationRunner().Status() error = %v", err)
	}
	if len(status.Pending) != 0 {
		t.Fatalf("pending migrations after reopen = %d, want 0", len(status.Pending))
	}
}

func openMigratedSQLiteCoordinatorAtPath(t *testing.T, dbPath string) *SQLiteCoordinator {
	t.Helper()

	coordinator, err := OpenSQLiteCoordinator(context.Background(), dbPath, SQLiteCoordinatorOptions{Migrations: EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if err := coordinator.Close(); err != nil {
			t.Fatalf("coordinator.Close() error = %v", err)
		}
	})

	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}

	return coordinator
}

func openTestSQLiteCoordinator(t *testing.T) *SQLiteCoordinator {
	t.Helper()

	coordinator, err := OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "looper.sqlite"), SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if err := coordinator.Close(); err != nil {
			t.Fatalf("coordinator.Close() error = %v", err)
		}
	})

	return coordinator
}

func readStringPragmaForTest(t *testing.T, db *sql.DB, query string) string {
	t.Helper()

	var value string
	if err := db.QueryRow(query).Scan(&value); err != nil {
		t.Fatalf("db.QueryRow(%q).Scan() error = %v", query, err)
	}

	return value
}

func readIntPragmaForTest(t *testing.T, db *sql.DB, query string) int {
	t.Helper()

	var value int
	if err := db.QueryRow(query).Scan(&value); err != nil {
		t.Fatalf("db.QueryRow(%q).Scan() error = %v", query, err)
	}

	return value
}
