import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { Database } from "bun:sqlite";

import { createMigrationRunner } from "./migrate";
import { SQLITE_MIGRATIONS } from "./migrations.gen";

const cleanupPaths: string[] = [];

afterEach(async () => {
  while (cleanupPaths.length > 0) {
    const path = cleanupPaths.pop();
    if (path) {
      await rm(path, { recursive: true, force: true });
    }
  }
});

async function createFixture(prefix: string) {
  const rootDir = await mkdtemp(join(tmpdir(), prefix));
  const dbPath = join(rootDir, "looper.sqlite");
  const migrationsDir = join(rootDir, "migrations");
  const backupDir = join(rootDir, "backups");

  await mkdir(migrationsDir, { recursive: true });

  cleanupPaths.push(rootDir);

  return { rootDir, dbPath, migrationsDir, backupDir };
}

describe("createMigrationRunner", () => {
  test("uses embedded migrations by default when migrationsDir is not provided", () => {
    const db = new Database(":memory:", { create: true });
    const migrationIds = SQLITE_MIGRATIONS.map((migration) => migration.id);
    const runner = createMigrationRunner(db, {
      now: () => new Date("2026-04-11T10:20:30.000Z"),
    });

    expect(runner.status().available.map((migration) => migration.id)).toEqual(
      migrationIds,
    );
    expect(runner.status().applied).toEqual([]);
    expect(runner.status().pending.map((migration) => migration.id)).toEqual(
      migrationIds,
    );

    const result = runner.runPending();
    expect(result.appliedIds).toEqual(migrationIds);
    expect(result.skippedIds).toEqual([]);

    const status = runner.status();
    expect(status.applied.map((migration) => migration.id)).toEqual(
      migrationIds,
    );
    expect(status.pending).toEqual([]);

    db.close(false);
  });

  test("lists and runs pending migrations with a backup", async () => {
    const fixture = await createFixture("looper-migrate-");

    await Bun.write(
      join(fixture.migrationsDir, "0001_init.sql"),
      [
        "CREATE TABLE widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL);",
        "CREATE INDEX idx_widgets_name ON widgets (name);",
      ].join("\n"),
    );
    await Bun.write(
      join(fixture.migrationsDir, "0002_seed.sql"),
      "INSERT INTO widgets (id, name) VALUES ('w_1', 'alpha');",
    );

    const db = new Database(fixture.dbPath, { create: true });
    const runner = createMigrationRunner(db, {
      migrationsDir: fixture.migrationsDir,
      backupDir: fixture.backupDir,
      now: () => new Date("2026-04-11T10:20:30.000Z"),
    });

    expect(runner.listPending()).toEqual(["0001_init", "0002_seed"]);

    const result = runner.runPending({ requireBackup: true });

    expect(result.appliedIds).toEqual(["0001_init", "0002_seed"]);
    expect(result.skippedIds).toEqual([]);
    expect(result.backupPath).toBe(
      join(fixture.backupDir, "looper-2026-04-11T10-20-30.000Z.sqlite"),
    );

    if (!result.backupPath) {
      throw new Error("Expected backup path to be returned");
    }

    const backupBytes = await readFile(result.backupPath);
    expect(backupBytes.byteLength).toBeGreaterThan(0);
    expect(runner.listPending()).toEqual([]);
    expect(
      db.query("SELECT name FROM widgets WHERE id = ?1").get("w_1") as {
        name: string;
      },
    ).toEqual({ name: "alpha" });

    db.close(false);
  });

  test("stops on migration failure without recording the failed migration", async () => {
    const fixture = await createFixture("looper-migrate-fail-");

    await Bun.write(
      join(fixture.migrationsDir, "0001_init.sql"),
      "CREATE TABLE widgets (id TEXT PRIMARY KEY);",
    );
    await Bun.write(
      join(fixture.migrationsDir, "0002_broken.sql"),
      "INSERT INTO missing_table (id) VALUES ('w_1');",
    );

    const db = new Database(fixture.dbPath, { create: true });
    const runner = createMigrationRunner(db, {
      migrationsDir: fixture.migrationsDir,
      now: () => new Date("2026-04-11T10:20:30.000Z"),
    });

    expect(() => runner.runPending()).toThrow(
      "Migration failed (0002_broken.sql)",
    );

    expect(runner.status().applied.map((migration) => migration.id)).toEqual([
      "0001_init",
    ]);
    expect(runner.listPending()).toEqual(["0002_broken"]);

    db.close(false);
  });

  test("runs foreign key pragma migrations without losing child rows", async () => {
    const fixture = await createFixture("looper-migrate-fk-");

    await Bun.write(
      join(fixture.migrationsDir, "0001_init.sql"),
      [
        "CREATE TABLE parents (id TEXT PRIMARY KEY, label TEXT NOT NULL);",
        "CREATE TABLE children (",
        "  id TEXT PRIMARY KEY,",
        "  parent_id TEXT NOT NULL,",
        "  label TEXT NOT NULL,",
        "  FOREIGN KEY (parent_id) REFERENCES parents (id) ON DELETE CASCADE",
        ");",
        "INSERT INTO parents (id, label) VALUES ('p_1', 'alpha');",
        "INSERT INTO children (id, parent_id, label) VALUES ('c_1', 'p_1', 'child');",
      ].join("\n"),
    );
    await Bun.write(
      join(fixture.migrationsDir, "0002_rebuild_parents.sql"),
      [
        "PRAGMA foreign_keys = OFF;",
        "CREATE TABLE parents_v2 (id TEXT PRIMARY KEY, label TEXT NOT NULL, extra TEXT);",
        "INSERT INTO parents_v2 (id, label, extra) SELECT id, label, NULL FROM parents;",
        "DROP TABLE parents;",
        "ALTER TABLE parents_v2 RENAME TO parents;",
        "PRAGMA foreign_keys = ON;",
      ].join("\n"),
    );

    const db = new Database(fixture.dbPath, { create: true });
    const initialForeignKeys = db.query("PRAGMA foreign_keys;").get();
    const runner = createMigrationRunner(db, {
      migrationsDir: fixture.migrationsDir,
      now: () => new Date("2026-04-11T10:20:30.000Z"),
    });

    const result = runner.runPending();

    expect(result.appliedIds).toEqual(["0001_init", "0002_rebuild_parents"]);
    expect(
      db
        .query("SELECT id, parent_id, label FROM children WHERE id = ?1")
        .get("c_1"),
    ).toEqual({
      id: "c_1",
      parent_id: "p_1",
      label: "child",
    });
    expect(db.query("PRAGMA foreign_keys;").get()).toEqual(initialForeignKeys);

    db.close(false);
  });

  test("rolls back foreign key pragma migration side effects on failure", async () => {
    const fixture = await createFixture("looper-migrate-fk-fail-");

    await Bun.write(
      join(fixture.migrationsDir, "0001_init.sql"),
      "CREATE TABLE widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL);",
    );
    await Bun.write(
      join(fixture.migrationsDir, "0002_partial_fail.sql"),
      [
        "PRAGMA foreign_keys = OFF;",
        "CREATE TABLE tmp_widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL);",
        "INSERT INTO tmp_widgets (id, name) VALUES ('w_1', 'alpha');",
        "INSERT INTO definitely_missing_table (id) VALUES ('x');",
        "PRAGMA foreign_keys = ON;",
      ].join("\n"),
    );

    const db = new Database(fixture.dbPath, { create: true });
    const initialForeignKeys = db.query("PRAGMA foreign_keys;").get();
    const runner = createMigrationRunner(db, {
      migrationsDir: fixture.migrationsDir,
      now: () => new Date("2026-04-11T10:20:30.000Z"),
    });

    expect(() => runner.runPending()).toThrow(
      "Migration failed (0002_partial_fail.sql)",
    );

    expect(
      db
        .query(
          "SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?1",
        )
        .get("tmp_widgets"),
    ).toBeNull();
    expect(runner.status().applied.map((migration) => migration.id)).toEqual([
      "0001_init",
    ]);
    expect(runner.listPending()).toEqual(["0002_partial_fail"]);
    expect(db.query("PRAGMA foreign_keys;").get()).toEqual(initialForeignKeys);

    db.close(false);
  });
});
