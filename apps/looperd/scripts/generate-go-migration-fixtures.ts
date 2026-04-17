import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { Database } from "bun:sqlite";

import { createMigrationRunner } from "../src/storage/sqlite/migrate";
import { SQLITE_MIGRATIONS } from "../src/storage/sqlite/migrations.gen";

const __dirname = dirname(fileURLToPath(import.meta.url));
const defaultOutputDir = resolve(
  __dirname,
  "../../../internal/storage/testdata/ts-created-migration-versions",
);
const fixedAppliedAt = new Date("2026-04-17T12:00:00.000Z");

async function main() {
  const outputDir = process.argv[2]
    ? resolve(process.cwd(), process.argv[2])
    : defaultOutputDir;

  await mkdir(outputDir, { recursive: true });

  const tempRoot = await mkdtemp(join(tmpdir(), "looper-ts-migrations-"));

  try {
    for (let version = 1; version <= SQLITE_MIGRATIONS.length; version += 1) {
      const appliedMigrations = SQLITE_MIGRATIONS.slice(0, version);
      const fixtureId = appliedMigrations[appliedMigrations.length - 1]?.id;

      if (!fixtureId) {
        throw new Error(`Missing fixture id for version ${version}`);
      }

      const dbPath = join(tempRoot, `${fixtureId}.sqlite`);
      const db = new Database(dbPath, { create: true });

      try {
        createMigrationRunner(db, {
          migrations: appliedMigrations,
          now: () => fixedAppliedAt,
        }).runPending();
      } finally {
        db.close(false);
      }

      const dbBytes = await readFile(dbPath);
      await writeFile(
        join(outputDir, `${fixtureId}.sqlite.base64`),
        dbBytes.toString("base64"),
      );
    }
  } finally {
    await rm(tempRoot, { recursive: true, force: true });
  }
}

await main();
