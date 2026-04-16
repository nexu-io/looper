import { describe, expect, test } from "bun:test";

import { resolveLooperdCliArgv, runLooperdCli } from "./index";
import { LOOPERD_VERSION } from "./metadata";

describe("runLooperdCli", () => {
  test("short-circuits --version before bootstrap", async () => {
    const stdout: string[] = [];
    let bootstrapCalled = false;

    const exitCode = await runLooperdCli(["--version"], {
      stdout: (line) => stdout.push(line),
      bootstrapImpl: async () => {
        bootstrapCalled = true;
        throw new Error("bootstrap should not be called");
      },
    });

    expect(exitCode).toBe(0);
    expect(stdout).toEqual([LOOPERD_VERSION]);
    expect(bootstrapCalled).toBe(false);
  });

  test("uses shared Bun argv resolution for source entrypoint", () => {
    expect(
      resolveLooperdCliArgv(
        ["/opt/homebrew/bin/bun", "ignored", "--version"],
        [
          "bun",
          "bunfs:///Users/mrc/Projects/looper/apps/looperd/src/index.ts",
          "--version",
        ],
      ),
    ).toEqual(["--version"]);
  });

  test("uses shared Bun argv resolution for compiled entrypoint", () => {
    expect(
      resolveLooperdCliArgv(
        ["/opt/homebrew/bin/bun", "ignored", "--version"],
        [
          "bun",
          "bunfs:///Users/mrc/Projects/looper/apps/looperd/src/compiled.ts",
          "--version",
        ],
      ),
    ).toEqual(["--version"]);
  });

  test("prefers Bun.argv for compiled executables", () => {
    expect(
      resolveLooperdCliArgv(
        [
          "/Users/mrc/Projects/looper/apps/looperd/dist/compiled/looperd-darwin-arm64",
          "--version",
        ],
        [
          "bun",
          "bunfs:///Users/mrc/Projects/looper/apps/looperd/src/index.ts",
          "--version",
        ],
      ),
    ).toEqual(["--version"]);
  });

  test("falls back to process.argv when Bun.argv is unavailable", () => {
    expect(
      resolveLooperdCliArgv(
        [
          "/opt/homebrew/bin/bun",
          "/Users/mrc/Projects/looper/apps/looperd/src/index.ts",
          "--version",
        ],
        undefined,
      ),
    ).toEqual(["--version"]);
  });
});
