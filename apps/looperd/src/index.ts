#!/usr/bin/env bun

import { bootstrapLooperd } from "./bootstrap/index";
import { ConfigValidationError } from "./config/index";
import { LOOPERD_VERSION } from "./metadata";

interface LooperdCliDeps {
  bootstrapImpl?: typeof bootstrapLooperd;
  stdout?: (line: string) => void;
  stderr?: (line: string) => void;
  env?: Record<string, string | undefined>;
}

export async function runLooperdCli(
  argv: string[],
  deps: LooperdCliDeps = {},
): Promise<number> {
  if (argv.includes("--version")) {
    (deps.stdout ?? ((line) => console.log(line)))(LOOPERD_VERSION);
    return 0;
  }

  try {
    await (deps.bootstrapImpl ?? bootstrapLooperd)({
      argv,
      env: deps.env ?? (process.env as Record<string, string | undefined>),
      waitForShutdown: true,
    });
    return 0;
  } catch (error) {
    if (error instanceof ConfigValidationError) {
      const stderr = deps.stderr ?? ((line) => console.error(line));
      stderr("looperd failed to start due to invalid configuration:");

      for (const issue of error.issues) {
        stderr(`- ${issue.path}: ${issue.message}`);
      }

      return 1;
    }

    throw error;
  }
}

export function resolveLooperdCliArgv(
  processArgv: string[],
  bunArgv: string[] | undefined = typeof Bun !== "undefined"
    ? Bun.argv
    : undefined,
): string[] {
  if (bunArgv && bunArgv.length >= 3) {
    return bunArgv.slice(2);
  }

  return processArgv.slice(2);
}

if (import.meta.main) {
  process.exit(await runLooperdCli(resolveLooperdCliArgv(process.argv)));
}
