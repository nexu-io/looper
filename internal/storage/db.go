package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const sqliteBusyTimeoutMilliseconds = 5000

type SQLiteCoordinatorOptions struct {
	Migrations []EmbeddedMigration
	BackupDir  string
	Now        func() time.Time
}

type SQLiteCoordinator struct {
	db     *sql.DB
	runner *MigrationRunner
}

type txBeginner interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

func OpenSQLiteCoordinator(ctx context.Context, dbPath string, options SQLiteCoordinatorOptions) (*SQLiteCoordinator, error) {
	db, err := OpenSQLiteDB(ctx, dbPath)
	if err != nil {
		return nil, err
	}

	coordinator := &SQLiteCoordinator{
		db: db,
		runner: NewMigrationRunner(db, MigrationRunnerOptions{
			Migrations: options.Migrations,
			BackupDir:  options.BackupDir,
			Now:        options.Now,
		}),
	}

	return coordinator, nil
}

func OpenSQLiteDB(ctx context.Context, dbPath string) (*sql.DB, error) {
	if err := ensureSQLiteParentDir(dbPath); err != nil {
		return nil, err
	}

	db, err := sql.Open(DriverName, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite database: %w", err)
	}

	if err := applySQLitePragmas(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

func (c *SQLiteCoordinator) DB() *sql.DB {
	return c.db
}

func (c *SQLiteCoordinator) MigrationRunner() *MigrationRunner {
	return c.runner
}

func (c *SQLiteCoordinator) Backup(ctx context.Context) (string, error) {
	if c == nil || c.runner == nil {
		return "", fmt.Errorf("sqlite coordinator is not initialized")
	}

	return c.runner.Backup(ctx)
}

func (c *SQLiteCoordinator) Close() error {
	if c == nil || c.db == nil {
		return nil
	}

	return c.db.Close()
}

func (c *SQLiteCoordinator) WithTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	if c == nil || c.db == nil {
		return fmt.Errorf("sqlite coordinator is not initialized")
	}

	return WithTransaction(ctx, c.db, nil, fn)
}

func WithTransaction(ctx context.Context, db txBeginner, options *sql.TxOptions, fn func(*sql.Tx) error) (err error) {
	if db == nil {
		return fmt.Errorf("transaction starter is nil")
	}

	if fn == nil {
		return fmt.Errorf("transaction callback is nil")
	}

	tx, err := db.BeginTx(ctx, options)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			_ = tx.Rollback()
			panic(recovered)
		}

		if err != nil {
			_ = tx.Rollback()
			return
		}

		if commitErr := tx.Commit(); commitErr != nil {
			err = fmt.Errorf("commit transaction: %w", commitErr)
		}
	}()

	err = fn(tx)
	return err
}

func WithTransactionValue[T any](ctx context.Context, db txBeginner, options *sql.TxOptions, fn func(*sql.Tx) (T, error)) (result T, err error) {
	if fn == nil {
		return result, fmt.Errorf("transaction callback is nil")
	}

	err = WithTransaction(ctx, db, options, func(tx *sql.Tx) error {
		var runErr error
		result, runErr = fn(tx)
		return runErr
	})

	return result, err
}

func ensureSQLiteParentDir(dbPath string) error {
	if dbPath == "" || dbPath == ":memory:" || strings.HasPrefix(dbPath, "file:") {
		return nil
	}

	parentDir := filepath.Dir(dbPath)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return fmt.Errorf("create sqlite parent directory: %w", err)
	}

	return nil
}

func applySQLitePragmas(ctx context.Context, db *sql.DB) error {
	if err := execPragma(ctx, db, `PRAGMA journal_mode = WAL;`); err != nil {
		return fmt.Errorf("set sqlite pragma journal_mode=WAL: %w", err)
	}

	if err := execPragma(ctx, db, `PRAGMA foreign_keys = ON;`); err != nil {
		return fmt.Errorf("set sqlite pragma foreign_keys=ON: %w", err)
	}

	if err := execPragma(ctx, db, fmt.Sprintf(`PRAGMA busy_timeout = %d;`, sqliteBusyTimeoutMilliseconds)); err != nil {
		return fmt.Errorf("set sqlite pragma busy_timeout=%d: %w", sqliteBusyTimeoutMilliseconds, err)
	}

	return nil
}

func execPragma(ctx context.Context, db *sql.DB, statement string) error {
	_, err := db.ExecContext(ctx, statement)
	return err
}
