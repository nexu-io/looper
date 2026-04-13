import { existsSync, readFileSync, readdirSync } from "node:fs";
import { mkdirSync } from "node:fs";
import { basename, join } from "node:path";
import { fileURLToPath } from "node:url";

import type { Database } from "bun:sqlite";

import type { MigrationRunResult, MigrationStatus } from "../types";
import { buildBackupPath } from "./paths";

const MIGRATION_FILE_RE = /^(\d{4}_[a-zA-Z0-9_\-]+)\.sql$/;

export interface SqliteMigrationRunnerOptions {
  migrationsDir?: string;
  backupDir?: string;
  dbPath?: string;
  now?: () => Date;
}

export interface SqliteMigrationRunner {
  listPending(): string[];
  status(): MigrationStatus;
  runPending(options?: { requireBackup?: boolean }): MigrationRunResult;
  backup(): string;
}

export function createMigrationRunner(
  db: Database,
  options: SqliteMigrationRunnerOptions = {},
): SqliteMigrationRunner {
  return new InternalSqliteMigrationRunner(db, options);
}

class InternalSqliteMigrationRunner implements SqliteMigrationRunner {
  private readonly migrationsDir: string;
  private readonly now: () => Date;

  constructor(
    private readonly db: Database,
    private readonly options: SqliteMigrationRunnerOptions,
  ) {
    this.migrationsDir = options.migrationsDir ?? resolveDefaultMigrationsDir();
    this.now = options.now ?? (() => new Date());
  }

  public listPending(): string[] {
    return this.status().pending.map((migration) => migration.id);
  }

  public status(): MigrationStatus {
    this.ensureSchemaMigrationsTable();
    const available = this.readAvailableMigrations();
    const applied = this.readAppliedMigrations();
    const appliedIds = new Set(applied.map((item) => item.id));
    const pending = available.filter(
      (migration) => !appliedIds.has(migration.id),
    );
    return { available, applied, pending };
  }

  public runPending(
    options: { requireBackup?: boolean } = {},
  ): MigrationRunResult {
    this.ensureSchemaMigrationsTable();
    const status = this.status();
    if (status.pending.length === 0) {
      return {
        appliedIds: [],
        skippedIds: status.applied.map((item) => item.id),
      };
    }

    let backupPath: string | undefined;
    if (options.requireBackup) {
      backupPath = this.backup();
    }

    const skippedIds = status.applied.map((item) => item.id);
    const appliedIds: string[] = [];

    for (const migration of status.pending) {
      const sql = readFileSync(
        join(this.migrationsDir, migration.fileName),
        "utf8",
      );

      try {
        if (usesForeignKeyPragma(sql)) {
          const previousForeignKeysSetting = readForeignKeysSetting(this.db);
          const migrationForeignKeysSetting = readFirstForeignKeysPragma(sql);

          try {
            if (
              typeof migrationForeignKeysSetting === "boolean" &&
              migrationForeignKeysSetting !== previousForeignKeysSetting
            ) {
              this.db.exec(
                `PRAGMA foreign_keys = ${migrationForeignKeysSetting ? "ON" : "OFF"}`,
              );
            }

            this.db.transaction(() => {
              this.db.exec(sql);
              this.db
                .query(
                  "INSERT INTO schema_migrations (id, applied_at) VALUES (?1, ?2)",
                )
                .run(migration.id, this.now().toISOString());
            })();
          } finally {
            this.db.exec(
              `PRAGMA foreign_keys = ${previousForeignKeysSetting ? "ON" : "OFF"}`,
            );
          }
        } else {
          this.db.transaction(() => {
            this.db.exec(sql);
            this.db
              .query(
                "INSERT INTO schema_migrations (id, applied_at) VALUES (?1, ?2)",
              )
              .run(migration.id, this.now().toISOString());
          })();
        }
      } catch (error) {
        throw new Error(
          `Migration failed (${migration.fileName}): ${(error as Error).message}`,
        );
      }

      appliedIds.push(migration.id);
    }

    return { appliedIds, skippedIds, backupPath };
  }

  public backup(): string {
    if (!this.options.backupDir) {
      throw new Error("Backup directory is not configured");
    }

    mkdirSync(this.options.backupDir, { recursive: true });
    const backupPath = buildBackupPath(this.options.backupDir, this.now());
    const safePath = backupPath.replaceAll("'", "''");
    this.db.exec(`VACUUM INTO '${safePath}'`);
    return backupPath;
  }

  private ensureSchemaMigrationsTable(): void {
    this.db.exec(`
      CREATE TABLE IF NOT EXISTS schema_migrations (
        id TEXT PRIMARY KEY,
        applied_at TEXT NOT NULL
      );
    `);
  }

  private readAvailableMigrations() {
    const migrationFiles = readdirSync(this.migrationsDir)
      .filter((file) => MIGRATION_FILE_RE.test(file))
      .sort();

    return migrationFiles.map((fileName) => ({
      id: basename(fileName, ".sql"),
      fileName,
    }));
  }

  private readAppliedMigrations() {
    return this.db
      .query(
        "SELECT id, applied_at AS appliedAt FROM schema_migrations ORDER BY id ASC",
      )
      .all() as Array<{ id: string; appliedAt: string }>;
  }
}

function usesForeignKeyPragma(sql: string): boolean {
  return /PRAGMA\s+foreign_keys\s*=\s*(ON|OFF)\s*;/i.test(sql);
}

function readFirstForeignKeysPragma(sql: string): boolean | undefined {
  const match = sql.match(/PRAGMA\s+foreign_keys\s*=\s*(ON|OFF)\s*;/i);
  if (!match) {
    return undefined;
  }

  return match[1]?.toUpperCase() === "ON";
}

function readForeignKeysSetting(db: Database): boolean {
  const row = db.query("PRAGMA foreign_keys;").get() as Record<
    string,
    unknown
  > | null;
  const value = row ? Object.values(row)[0] : undefined;
  return value === 1 || value === "1";
}

function resolveDefaultMigrationsDir(): string {
  const colocatedDir = fileURLToPath(new URL("./migrations", import.meta.url));
  if (existsSync(colocatedDir)) {
    return colocatedDir;
  }

  return fileURLToPath(
    new URL("../../../src/storage/sqlite/migrations", import.meta.url),
  );
}
