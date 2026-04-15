import { mkdir, rm, stat } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const scriptDir = dirname(fileURLToPath(import.meta.url));
const packageRoot = join(scriptDir, "..");
const distDir = join(packageRoot, "dist");
const compiledDir = join(distDir, "compiled");
const entrypoint = join(packageRoot, "src", "compiled.ts");

const TARGETS = {
  "darwin-arm64": {
    compileTarget: "bun-darwin-arm64",
    artifactName: "looperd-darwin-arm64",
  },
  "darwin-x64": {
    compileTarget: "bun-darwin-x64",
    artifactName: "looperd-darwin-x64",
  },
} as const;

type SupportedTarget = keyof typeof TARGETS;

function assertCompileSupported(): void {
  if (process.env.LOOPER_FORCE_COMPILE === "1") {
    return;
  }

  // Standalone `bun --compile` is part of the release architecture, but it is
  // not required for the local dev loop. Keep local known-bad environments from
  // silently producing unusable artifacts; CI/release can still opt in via
  // `LOOPER_FORCE_COMPILE=1` on vetted runners while we wait for an upstream fix.
  if (process.platform === "darwin" && process.arch === "arm64") {
    throw new Error(
      `Refusing to build a standalone looperd binary with Bun ${Bun.version} on ${process.platform}-${process.arch}: the resulting executable hangs before user code runs in this environment. Upgrade Bun or rerun with LOOPER_FORCE_COMPILE=1 to bypass this safety check.`,
    );
  }
}

function formatSize(bytes: number): string {
  return `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
}

async function compileTarget(target: SupportedTarget) {
  const targetConfig = TARGETS[target];
  const outfile = join(compiledDir, targetConfig.artifactName);
  const result = await Bun.build({
    entrypoints: [entrypoint],
    compile: {
      target: targetConfig.compileTarget,
      outfile,
    },
    minify: false,
    sourcemap: "none",
  });

  if (!result.success) {
    throw new AggregateError(
      result.logs,
      `looperd compile failed for ${target}`,
    );
  }

  const artifact = await stat(outfile);
  console.log(`${targetConfig.artifactName}: ${formatSize(artifact.size)}`);
}

async function main() {
  assertCompileSupported();

  const requested = Bun.argv[2] ?? "all";
  const targets =
    requested === "all"
      ? (Object.keys(TARGETS) as SupportedTarget[])
      : [requested as SupportedTarget];

  for (const target of targets) {
    if (!(target in TARGETS)) {
      throw new Error(
        `Unsupported compile target '${target}'. Supported targets: ${Object.keys(TARGETS).join(", ")}`,
      );
    }
  }

  await Bun.$`bun run ${join(packageRoot, "scripts", "generate-artifacts.ts")}`;
  await rm(compiledDir, { recursive: true, force: true });
  await mkdir(distDir, { recursive: true });
  await mkdir(compiledDir, { recursive: true });

  for (const target of targets) {
    await compileTarget(target);
  }
}

await main();
