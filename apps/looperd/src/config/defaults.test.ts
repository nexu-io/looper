import { describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, rm, symlink } from "node:fs/promises";
import { homedir } from "node:os";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  createDefaultLooperConfig,
  getDefaultProjectWorktreeRoot,
  getDefaultWorktreeRoot,
} from "./index";
import { toRepoWorktreeDirectoryName } from "./project-id";

describe("config defaults", () => {
  const repoPath = join(homedir(), "src", "looper");
  const repoDirectoryName = toRepoWorktreeDirectoryName(repoPath);

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
    expect(getDefaultProjectWorktreeRoot("project_1", repoPath)).toBe(
      join(homedir(), ".looper", "worktrees", repoDirectoryName, "project_1"),
    );
  });

  test("sanitizes legacy project ids when deriving project worktree roots", () => {
    expect(getDefaultProjectWorktreeRoot("../tmp", repoPath)).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        repoDirectoryName,
        "legacy-id-2e2e2f746d70",
      ),
    );
    expect(getDefaultProjectWorktreeRoot("..", repoPath)).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        repoDirectoryName,
        "legacy-id-2e2e",
      ),
    );
    expect(getDefaultProjectWorktreeRoot("/var/tmp/x", repoPath)).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        repoDirectoryName,
        "legacy-id-2f7661722f746d702f78",
      ),
    );
    expect(getDefaultProjectWorktreeRoot("legacy-id-Li4vdG1w", repoPath)).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        repoDirectoryName,
        "legacy-id-6c65676163792d69642d4c69347664473177",
      ),
    );
  });

  test("canonicalizes mixed-case project ids when deriving project worktree roots", () => {
    expect(getDefaultProjectWorktreeRoot("foo", repoPath)).toBe(
      join(homedir(), ".looper", "worktrees", repoDirectoryName, "foo"),
    );
    expect(getDefaultProjectWorktreeRoot("Foo", repoPath)).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        repoDirectoryName,
        "legacy-id-466f6f",
      ),
    );
    expect(getDefaultProjectWorktreeRoot("FOO", repoPath)).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        repoDirectoryName,
        "legacy-id-464f4f",
      ),
    );
  });

  test("sanitizes Windows-invalid canonical project ids when deriving project worktree roots", () => {
    expect(getDefaultProjectWorktreeRoot("con", repoPath)).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        repoDirectoryName,
        "legacy-id-636f6e",
      ),
    );
    expect(getDefaultProjectWorktreeRoot("nul.txt", repoPath)).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        repoDirectoryName,
        "legacy-id-6e756c2e747874",
      ),
    );
    expect(getDefaultProjectWorktreeRoot("project.", repoPath)).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        repoDirectoryName,
        "legacy-id-70726f6a6563742e",
      ),
    );
  });

  test("hashes long canonical project ids when deriving project worktree roots", () => {
    const projectId = "a".repeat(256);
    const hashedProjectId = createHash("sha256")
      .update(projectId)
      .digest("hex");

    expect(getDefaultProjectWorktreeRoot(projectId, repoPath)).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        repoDirectoryName,
        `legacy-id-${hashedProjectId}`,
      ),
    );
  });

  test("hashes long legacy project ids when deriving project worktree roots", () => {
    const projectId = "A".repeat(123);
    const hashedProjectId = createHash("sha256")
      .update(projectId)
      .digest("hex");

    expect(getDefaultProjectWorktreeRoot(projectId, repoPath)).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        repoDirectoryName,
        `legacy-id-${hashedProjectId}`,
      ),
    );
  });

  test("scopes default project worktree roots by repository identity", () => {
    expect(
      getDefaultProjectWorktreeRoot(
        "project_1",
        join(homedir(), "src", "repo-a"),
      ),
    ).not.toBe(
      getDefaultProjectWorktreeRoot(
        "project_1",
        join(homedir(), "src", "repo-b"),
      ),
    );
  });

  test("hashes repository identity when deriving default project worktree roots", () => {
    const deepRepoPath = join(
      homedir(),
      "src",
      "very".repeat(20),
      "deep".repeat(20),
      "repo",
    );

    expect(getDefaultProjectWorktreeRoot("project_1", deepRepoPath)).toBe(
      join(
        homedir(),
        ".looper",
        "worktrees",
        toRepoWorktreeDirectoryName(deepRepoPath),
        "project_1",
      ),
    );
  });

  test("canonicalizes equivalent repository paths before hashing worktree roots", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-defaults-"));
    const repoPath = join(rootDir, "repo");
    const repoAliasPath = join(rootDir, "repo-alias");

    await mkdir(repoPath, { recursive: true });
    await symlink(repoPath, repoAliasPath);

    expect(toRepoWorktreeDirectoryName(`${repoPath}/`)).toBe(
      toRepoWorktreeDirectoryName(repoPath),
    );
    expect(toRepoWorktreeDirectoryName(repoAliasPath)).toBe(
      toRepoWorktreeDirectoryName(repoPath),
    );

    await rm(rootDir, { recursive: true, force: true });
  });
});
