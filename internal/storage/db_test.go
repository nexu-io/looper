package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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

	coordinator, err := OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "looper.sqlite"), SQLiteCoordinatorOptions{
		Migrations: []EmbeddedMigration{{ID: "0001_init", FileName: "0001_init.sql", SQL: "CREATE TABLE widgets (id TEXT PRIMARY KEY);"}},
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
