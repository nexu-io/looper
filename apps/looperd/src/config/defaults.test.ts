import { describe, expect, test } from "bun:test";
import { homedir } from "node:os";
import { join } from "node:path";

import {
  createDefaultLooperConfig,
  getDefaultProjectWorktreeRoot,
  getDefaultWorktreeRoot,
} from "./index";

describe("config defaults", () => {
  test("uses ~/.looper for runtime artifacts and worktree roots", () => {
    const config = createDefaultLooperConfig("/tmp/workspace");

    expect(config.storage.dbPath).toBe(
      join(homedir(), ".looper", "looper.sqlite"),
    );
    expect(config.storage.backupDir).toBe(
      join(homedir(), ".looper", "backups"),
    );
    expect(config.daemon.logDir).toBe(join(homedir(), ".looper", "logs"));
    expect(getDefaultWorktreeRoot()).toBe(
      join(homedir(), ".looper", "worktrees"),
    );
    expect(getDefaultProjectWorktreeRoot("project_1")).toBe(
      join(homedir(), ".looper", "worktrees", "project_1"),
    );
  });

  test("sanitizes legacy project ids when deriving project worktree roots", () => {
    expect(getDefaultProjectWorktreeRoot("../tmp")).toBe(
      join(homedir(), ".looper", "worktrees", "legacy-id-Li4vdG1w"),
    );
    expect(getDefaultProjectWorktreeRoot("..")).toBe(
      join(homedir(), ".looper", "worktrees", "legacy-id-Li4"),
    );
    expect(getDefaultProjectWorktreeRoot("/var/tmp/x")).toBe(
      join(homedir(), ".looper", "worktrees", "legacy-id-L3Zhci90bXAveA"),
    );
    expect(getDefaultProjectWorktreeRoot("legacy-id-Li4vdG1w")).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        "legacy-id-bGVnYWN5LWlkLUxpNHZkRzF3",
      ),
    );
  });
});
