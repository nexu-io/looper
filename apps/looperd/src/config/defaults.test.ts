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

  test("rejects unsafe project ids when deriving project worktree roots", () => {
    expect(() => getDefaultProjectWorktreeRoot("../tmp")).toThrow(
      'Invalid project id "../tmp": must not contain path separators, dot segments, or be an absolute path',
    );
    expect(() => getDefaultProjectWorktreeRoot("..")).toThrow(
      'Invalid project id "..": must not contain path separators, dot segments, or be an absolute path',
    );
    expect(() => getDefaultProjectWorktreeRoot("/var/tmp/x")).toThrow(
      'Invalid project id "/var/tmp/x": must not contain path separators, dot segments, or be an absolute path',
    );
  });
});
