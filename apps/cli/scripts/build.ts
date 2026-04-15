import { mkdir, rm, cp } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const scriptDir = dirname(fileURLToPath(import.meta.url));
const packageRoot = join(scriptDir, "..");
const repoRoot = join(packageRoot, "..", "..");
const distDir = join(packageRoot, "dist");
const cliEntry = join(packageRoot, "src", "index.ts");
const looperdEntry = join(repoRoot, "apps", "looperd", "src", "index.ts");
const migrationSourceDir = join(
  repoRoot,
  "apps",
  "looperd",
  "src",
  "storage",
  "sqlite",
  "migrations",
);
const migrationDestDir = join(distDir, "storage", "sqlite", "migrations");

async function build() {
  await rm(distDir, { recursive: true, force: true });
  await mkdir(distDir, { recursive: true });

  const cliResult = await Bun.build({
    entrypoints: [cliEntry],
    outdir: distDir,
    target: "node",
    naming: "index.js",
  });
  if (!cliResult.success) {
    throw new AggregateError(cliResult.logs, "CLI build failed");
  }

  const looperdResult = await Bun.build({
    entrypoints: [looperdEntry],
    outdir: distDir,
    target: "bun",
    naming: "looperd.js",
  });
  if (!looperdResult.success) {
    throw new AggregateError(looperdResult.logs, "looperd build failed");
  }

  await mkdir(dirname(migrationDestDir), { recursive: true });
  await cp(migrationSourceDir, migrationDestDir, { recursive: true });
}

await build();
