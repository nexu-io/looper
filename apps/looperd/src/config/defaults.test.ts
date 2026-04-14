import { describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
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
      join(homedir(), ".looper", "worktrees", "legacy-id-2e2e2f746d70"),
    );
    expect(getDefaultProjectWorktreeRoot("..")).toBe(
      join(homedir(), ".looper", "worktrees", "legacy-id-2e2e"),
    );
    expect(getDefaultProjectWorktreeRoot("/var/tmp/x")).toBe(
      join(homedir(), ".looper", "worktrees", "legacy-id-2f7661722f746d702f78"),
    );
    expect(getDefaultProjectWorktreeRoot("legacy-id-Li4vdG1w")).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        "legacy-id-6c65676163792d69642d4c69347664473177",
      ),
    );
  });

  test("canonicalizes mixed-case project ids when deriving project worktree roots", () => {
    expect(getDefaultProjectWorktreeRoot("foo")).toBe(
      join(homedir(), ".looper", "worktrees", "foo"),
    );
    expect(getDefaultProjectWorktreeRoot("Foo")).toBe(
      join(homedir(), ".looper", "worktrees", "legacy-id-466f6f"),
    );
    expect(getDefaultProjectWorktreeRoot("FOO")).toBe(
      join(homedir(), ".looper", "worktrees", "legacy-id-464f4f"),
    );
  });

  test("hashes long canonical project ids when deriving project worktree roots", () => {
    const projectId = "a".repeat(256);
    const hashedProjectId = createHash("sha256")
      .update(projectId)
      .digest("hex");

    expect(getDefaultProjectWorktreeRoot(projectId)).toBe(
      join(homedir(), ".looper", "worktrees", `legacy-id-${hashedProjectId}`),
    );
  });

  test("hashes long legacy project ids when deriving project worktree roots", () => {
    const projectId = "A".repeat(123);
    const hashedProjectId = createHash("sha256")
      .update(projectId)
      .digest("hex");

    expect(getDefaultProjectWorktreeRoot(projectId)).toBe(
      join(homedir(), ".looper", "worktrees", `legacy-id-${hashedProjectId}`),
    );
  });
});
