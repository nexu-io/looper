package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const javaScriptISOStringLayout = "2006-01-02T15:04:05.000Z"

type MigrationDescriptor struct {
	ID       string
	FileName string
}

type AppliedMigration struct {
	ID        string
	AppliedAt string
}

type MigrationStatus struct {
	Available []MigrationDescriptor
	Applied   []AppliedMigration
	Pending   []MigrationDescriptor
}

type MigrationRunResult struct {
	AppliedIDs []string
	SkippedIDs []string
	BackupPath string
}

type RunPendingOptions struct {
	RequireBackup bool
}

type MigrationRunnerOptions struct {
	Migrations []EmbeddedMigration
	BackupDir  string
	Now        func() time.Time
}

type MigrationRunner struct {
	db         *sql.DB
	migrations []EmbeddedMigration
	backupDir  string
	now        func() time.Time
}

func NewMigrationRunner(db *sql.DB, options MigrationRunnerOptions) *MigrationRunner {
	migrations := options.Migrations
	if migrations == nil {
		migrations = EmbeddedMigrations
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}

	return &MigrationRunner{
		db:         db,
		migrations: migrations,
		backupDir:  options.BackupDir,
		now:        now,
	}
}

func (r *MigrationRunner) ListPending(ctx context.Context) ([]string, error) {
	status, err := r.Status(ctx)
	if err != nil {
		return nil, err
	}

	pending := make([]string, len(status.Pending))
	for i, migration := range status.Pending {
		pending[i] = migration.ID
	}

	return pending, nil
}

func (r *MigrationRunner) Status(ctx context.Context) (MigrationStatus, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return MigrationStatus{}, fmt.Errorf("open sqlite connection: %w", err)
	}
	defer conn.Close()

	if err := ensureSchemaMigrationsTable(ctx, conn); err != nil {
		return MigrationStatus{}, err
	}

	available := describeMigrations(r.migrations)
	applied, err := readAppliedMigrations(ctx, conn)
	if err != nil {
		return MigrationStatus{}, err
	}

	appliedIDs := make(map[string]struct{}, len(applied))
	for _, migration := range applied {
		appliedIDs[migration.ID] = struct{}{}
	}

	pending := make([]MigrationDescriptor, 0, len(available))
	for _, migration := range available {
		if _, ok := appliedIDs[migration.ID]; ok {
			continue
		}
		pending = append(pending, migration)
	}

	return MigrationStatus{
		Available: available,
		Applied:   applied,
		Pending:   pending,
	}, nil
}

func (r *MigrationRunner) RunPending(ctx context.Context, options ...RunPendingOptions) (MigrationRunResult, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return MigrationRunResult{}, fmt.Errorf("open sqlite connection: %w", err)
	}
	defer conn.Close()

	if err := ensureSchemaMigrationsTable(ctx, conn); err != nil {
		return MigrationRunResult{}, err
	}

	applied, err := readAppliedMigrations(ctx, conn)
	if err != nil {
		return MigrationRunResult{}, err
	}

	appliedIDs := make(map[string]struct{}, len(applied))
	skipped := make([]string, len(applied))
	for i, migration := range applied {
		appliedIDs[migration.ID] = struct{}{}
		skipped[i] = migration.ID
	}

	pending := make([]EmbeddedMigration, 0, len(r.migrations))
	for _, migration := range r.migrations {
		if _, ok := appliedIDs[migration.ID]; ok {
			continue
		}
		pending = append(pending, migration)
	}
	if len(pending) == 0 {
		return MigrationRunResult{AppliedIDs: []string{}, SkippedIDs: skipped}, nil
	}

	requireBackup := false
	if len(options) > 0 {
		requireBackup = options[0].RequireBackup
	}

	result := MigrationRunResult{AppliedIDs: make([]string, 0), SkippedIDs: skipped}
	if requireBackup {
		backupPath, err := r.backupOnConn(ctx, conn)
		if err != nil {
			return MigrationRunResult{}, err
		}
		result.BackupPath = backupPath
	}

	for _, migration := range pending {
		if err := runMigration(ctx, conn, migration, r.now); err != nil {
			return MigrationRunResult{}, err
		}

		result.AppliedIDs = append(result.AppliedIDs, migration.ID)
	}

	return result, nil
}

func (r *MigrationRunner) Backup(ctx context.Context) (string, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return "", fmt.Errorf("open sqlite connection: %w", err)
	}
	defer conn.Close()

	return r.backupOnConn(ctx, conn)
}

func describeMigrations(migrations []EmbeddedMigration) []MigrationDescriptor {
	descriptors := make([]MigrationDescriptor, len(migrations))
	for i, migration := range migrations {
		descriptors[i] = MigrationDescriptor{ID: migration.ID, FileName: migration.FileName}
	}

	return descriptors
}

func ensureSchemaMigrationsTable(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			id TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		);
	`)
	if err != nil {
		return fmt.Errorf("ensure schema_migrations table: %w", err)
	}

	return nil
}

func (r *MigrationRunner) backupOnConn(ctx context.Context, conn *sql.Conn) (string, error) {
	if r.backupDir == "" {
		return "", fmt.Errorf("backup directory is not configured")
	}

	if err := os.MkdirAll(r.backupDir, 0o755); err != nil {
		return "", fmt.Errorf("create backup directory: %w", err)
	}

	backupPath := buildBackupPath(r.backupDir, r.now().UTC())
	safePath := strings.ReplaceAll(backupPath, "'", "''")
	if _, err := conn.ExecContext(ctx, `VACUUM INTO '`+safePath+`'`); err != nil {
		return "", fmt.Errorf("create sqlite backup: %w", err)
	}

	return backupPath, nil
}

func buildBackupPath(backupDir string, now time.Time) string {
	stamp := strings.ReplaceAll(now.UTC().Format(javaScriptISOStringLayout), ":", "-")
	return filepath.Join(backupDir, "looper-"+stamp+".sqlite")
}

func readAppliedMigrations(ctx context.Context, conn *sql.Conn) ([]AppliedMigration, error) {
	rows, err := conn.QueryContext(ctx, `SELECT id, applied_at FROM schema_migrations ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make([]AppliedMigration, 0)
	for rows.Next() {
		var migration AppliedMigration
		if err := rows.Scan(&migration.ID, &migration.AppliedAt); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied = append(applied, migration)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}

	return applied, nil
}

func runMigration(ctx context.Context, conn *sql.Conn, migration EmbeddedMigration, now func() time.Time) error {
	if usesForeignKeyPragma(migration.SQL) {
		// SQLite ignores PRAGMA foreign_keys changes inside a transaction, so we
		// switch the connection setting before the
		// migration transaction begins, then restore it afterward.
		previousForeignKeysSetting, err := readForeignKeysSetting(ctx, conn)
		if err != nil {
			return fmt.Errorf("Migration failed (%s): read foreign_keys pragma: %w", migration.FileName, err)
		}

		migrationForeignKeysSetting := readFirstForeignKeysPragma(migration.SQL)
		if migrationForeignKeysSetting != nil && *migrationForeignKeysSetting != previousForeignKeysSetting {
			if err := setForeignKeysSetting(ctx, conn, *migrationForeignKeysSetting); err != nil {
				return fmt.Errorf("Migration failed (%s): set foreign_keys pragma: %w", migration.FileName, err)
			}
		}

		defer func() {
			_ = setForeignKeysSetting(context.Background(), conn, previousForeignKeysSetting)
		}()
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("Migration failed (%s): begin transaction: %w", migration.FileName, err)
	}

	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
		return fmt.Errorf("Migration failed (%s): %w", migration.FileName, err)
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		migration.ID,
		now().UTC().Format(javaScriptISOStringLayout),
	); err != nil {
		return fmt.Errorf("Migration failed (%s): %w", migration.FileName, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("Migration failed (%s): %w", migration.FileName, err)
	}

	return nil
}

var foreignKeysPattern = regexp.MustCompile(`(?i)PRAGMA\s+foreign_keys\s*=\s*(ON|OFF)\s*;`)

func usesForeignKeyPragma(sqlText string) bool {
	return foreignKeysPattern.MatchString(sqlText)
}

func readFirstForeignKeysPragma(sqlText string) *bool {
	match := foreignKeysPattern.FindStringSubmatch(sqlText)
	if len(match) < 2 {
		return nil
	}

	enabled := strings.EqualFold(match[1], "ON")
	return &enabled
}

func readForeignKeysSetting(ctx context.Context, conn *sql.Conn) (bool, error) {
	var value int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys;`).Scan(&value); err != nil {
		return false, err
	}

	return value == 1, nil
}

func setForeignKeysSetting(ctx context.Context, conn *sql.Conn, enabled bool) error {
	setting := "OFF"
	if enabled {
		setting = "ON"
	}

	_, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = `+setting)
	return err
}
