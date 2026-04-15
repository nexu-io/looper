import { mkdir, rm } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const scriptDir = dirname(fileURLToPath(import.meta.url));
const packageRoot = join(scriptDir, "..");
const distDir = join(packageRoot, "dist");
const cliEntry = join(packageRoot, "src", "index.ts");

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
}

await build();
