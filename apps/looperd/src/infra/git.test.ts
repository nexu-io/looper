import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { SqliteStore } from "../storage/sqlite/sqlite-store";
import { GitWorktreeGateway, ProtectedBranchError } from "./git";

const cleanupPaths: string[] = [];

afterEach(async () => {
  while (cleanupPaths.length > 0) {
    const path = cleanupPaths.pop();
    if (path) {
      await rm(path, { recursive: true, force: true });
    }
  }
});

async function runGit(args: string[], cwd: string) {
  const proc = Bun.spawn({
    cmd: ["git", ...args],
    cwd,
    stdout: "pipe",
    stderr: "pipe",
  });
  const exitCode = await proc.exited;
  const stdout = await new Response(proc.stdout).text();
  const stderr = await new Response(proc.stderr).text();
  if (exitCode !== 0) {
    throw new Error(stderr || stdout || `git failed: ${args.join(" ")}`);
  }
  return stdout;
}

describe("GitWorktreeGateway", () => {
  test("creates, restores, and cleans worktrees with branch protection", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-git-"));
    cleanupPaths.push(rootDir);
    const repoPath = join(rootDir, "repo");
    const worktreeRoot = join(rootDir, "worktrees");
    const remotePath = join(rootDir, "remote.git");
    await mkdir(repoPath, { recursive: true });
    await mkdir(remotePath, { recursive: true });

    await runGit(["init", "-b", "main"], repoPath);
    await runGit(["init", "--bare"], remotePath);
    await runGit(["config", "user.email", "test@example.com"], repoPath);
    await runGit(["config", "user.name", "Looper Test"], repoPath);
    await runGit(["remote", "add", "origin", remotePath], repoPath);
    await writeFile(join(repoPath, "README.md"), "hello\n");
    await runGit(["add", "README.md"], repoPath);
    await runGit(["commit", "-m", "init"], repoPath);
    await runGit(["push", "-u", "origin", "main"], repoPath);
    await runGit(["checkout", "-b", "feature/fixer"], repoPath);
    await writeFile(join(repoPath, "fix.txt"), "remote change\n");
    await runGit(["add", "fix.txt"], repoPath);
    await runGit(["commit", "-m", "feature"], repoPath);
    await runGit(["push", "-u", "origin", "feature/fixer"], repoPath);
    await runGit(["checkout", "main"], repoPath);

    const store = new SqliteStore({
      dbPath: join(rootDir, "state", "looper.sqlite"),
    });
    store.initialize({ autoMigrate: true });
    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath,
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });

    const gateway = new GitWorktreeGateway({ gitPath: "git", store });
    const worktree = await gateway.createWorktree({
      projectId: "project_1",
      repoPath,
      worktreeRoot,
      branch: "feature/fixer",
      baseBranch: "main",
      prNumber: 42,
    });
    const restored = await gateway.restoreWorktree({
      projectId: "project_1",
      repoPath,
      branch: "feature/fixer",
    });
    const prepared = await gateway.prepareWorktree({
      worktreePath: worktree.worktreePath,
      branch: "feature/fixer",
    });

    await writeFile(
      join(worktree.worktreePath, "README.md"),
      "hello updated\n",
    );
    const inspectBeforeCommit = await gateway.inspectHead({
      worktreePath: worktree.worktreePath,
      baseRef: prepared.headSha,
    });
    const globalEmailBefore = (
      await runGit(
        ["config", "--global", "--get", "user.email"],
        repoPath,
      ).catch(() => "")
    ).trim();
    const commit = await gateway.commit({
      worktreePath: worktree.worktreePath,
      message: "fixer: address PR #42 follow-up items",
    });
    const inspectAfterCommit = await gateway.inspectHead({
      worktreePath: worktree.worktreePath,
      baseRef: prepared.headSha,
    });

    expect(
      await readFile(join(worktree.worktreePath, "README.md"), "utf8"),
    ).toContain("updated");
    expect(restored?.branch).toBe("feature/fixer");
    expect(prepared.clean).toBe(true);
    expect(inspectBeforeCommit.hasUncommittedChanges).toBe(true);
    expect(commit.commitSha).toBeTruthy();
    expect(inspectAfterCommit.hasUncommittedChanges).toBe(false);
    expect(inspectAfterCommit.newCommitShas).toHaveLength(1);
    const commitAuthor = (
      await runGit(["log", "-1", "--format=%an <%ae>"], worktree.worktreePath)
    ).trim();
    expect(commitAuthor).toBe("Looper Test <test@example.com>");
    const globalEmailAfter = (
      await runGit(
        ["config", "--global", "--get", "user.email"],
        repoPath,
      ).catch(() => "")
    ).trim();
    expect(globalEmailAfter).toBe(globalEmailBefore);

    await gateway.cleanupWorktree({
      projectId: "project_1",
      repoPath,
      worktreePath: worktree.worktreePath,
      branch: "feature/fixer",
    });
    expect(
      store.worktrees.getByBranch("project_1", "feature/fixer")?.status,
    ).toBe("cleaned");
    expect(() => gateway.assertWritableBranch("main", ["main"])).toThrow(
      ProtectedBranchError,
    );

    store.close();
  });

  test("keeps the primary checkout clean when a detached fixer worktree commits", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-git-"));
    cleanupPaths.push(rootDir);
    const repoPath = join(rootDir, "repo");
    const worktreeRoot = join(rootDir, "worktrees");
    const remotePath = join(rootDir, "remote.git");
    await mkdir(repoPath, { recursive: true });
    await mkdir(remotePath, { recursive: true });

    await runGit(["init", "-b", "main"], repoPath);
    await runGit(["init", "--bare"], remotePath);
    await runGit(["config", "user.email", "test@example.com"], repoPath);
    await runGit(["config", "user.name", "Looper Test"], repoPath);
    await runGit(["remote", "add", "origin", remotePath], repoPath);
    await writeFile(join(repoPath, "README.md"), "hello\n");
    await runGit(["add", "README.md"], repoPath);
    await runGit(["commit", "-m", "init"], repoPath);
    await runGit(["push", "-u", "origin", "main"], repoPath);
    await runGit(["checkout", "-b", "feature/fixer"], repoPath);
    await writeFile(join(repoPath, "fix.txt"), "remote change\n");
    await runGit(["add", "fix.txt"], repoPath);
    await runGit(["commit", "-m", "feature"], repoPath);
    await runGit(["push", "-u", "origin", "feature/fixer"], repoPath);

    const store = new SqliteStore({
      dbPath: join(rootDir, "state", "looper.sqlite"),
    });
    store.initialize({ autoMigrate: true });
    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath,
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });

    const gateway = new GitWorktreeGateway({ gitPath: "git", store });
    const worktree = await gateway.createWorktree({
      projectId: "project_1",
      repoPath,
      worktreeRoot,
      branch: "feature/fixer",
      baseBranch: "main",
      prNumber: 42,
      checkoutMode: "detached",
    });

    expect(
      (
        await runGit(["branch", "--show-current"], worktree.worktreePath)
      ).trim(),
    ).toBe("");

    await writeFile(
      join(worktree.worktreePath, "README.md"),
      "hello updated\n",
    );
    const baseHeadSha = (await runGit(["rev-parse", "HEAD"], repoPath)).trim();
    await gateway.commit({
      worktreePath: worktree.worktreePath,
      message: "fixer: address PR #42 follow-up items",
    });
    await gateway.push({
      worktreePath: worktree.worktreePath,
      branch: "feature/fixer",
      expectedRemoteHeadSha: baseHeadSha,
    });

    expect((await runGit(["rev-parse", "HEAD"], repoPath)).trim()).toBe(
      baseHeadSha,
    );
    expect((await runGit(["status", "--porcelain"], repoPath)).trim()).toBe("");
    expect(
      (await runGit(["diff", "--cached", "--name-only"], repoPath)).trim(),
    ).toBe("");

    store.close();
  });

  test("recreates an attached branch worktree when detached mode is requested", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-git-"));
    cleanupPaths.push(rootDir);
    const repoPath = join(rootDir, "repo");
    const worktreeRoot = join(rootDir, "worktrees");
    await mkdir(repoPath, { recursive: true });

    await runGit(["init", "-b", "main"], repoPath);
    await runGit(["config", "user.email", "test@example.com"], repoPath);
    await runGit(["config", "user.name", "Looper Test"], repoPath);
    await writeFile(join(repoPath, "README.md"), "hello\n");
    await runGit(["add", "README.md"], repoPath);
    await runGit(["commit", "-m", "init"], repoPath);
    await runGit(["checkout", "-b", "feature/fixer"], repoPath);
    await runGit(["checkout", "main"], repoPath);

    const store = new SqliteStore({
      dbPath: join(rootDir, "state", "looper.sqlite"),
    });
    store.initialize({ autoMigrate: true });
    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath,
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });

    const gateway = new GitWorktreeGateway({ gitPath: "git", store });
    const attached = await gateway.createWorktree({
      projectId: "project_1",
      repoPath,
      worktreeRoot,
      branch: "feature/fixer",
      baseBranch: "main",
      prNumber: 42,
    });

    expect(
      (
        await runGit(["branch", "--show-current"], attached.worktreePath)
      ).trim(),
    ).toBe("feature/fixer");

    const detached = await gateway.createWorktree({
      projectId: "project_1",
      repoPath,
      worktreeRoot,
      branch: "feature/fixer",
      baseBranch: "main",
      prNumber: 42,
      checkoutMode: "detached",
    });

    expect(detached.worktreePath).toBe(attached.worktreePath);
    expect(
      (
        await runGit(["branch", "--show-current"], detached.worktreePath)
      ).trim(),
    ).toBe("");

    store.close();
  });

  test("recreates a detached worktree when branch checkout mode is requested", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-git-"));
    cleanupPaths.push(rootDir);
    const repoPath = join(rootDir, "repo");
    const worktreeRoot = join(rootDir, "worktrees");
    await mkdir(repoPath, { recursive: true });

    await runGit(["init", "-b", "main"], repoPath);
    await runGit(["config", "user.email", "test@example.com"], repoPath);
    await runGit(["config", "user.name", "Looper Test"], repoPath);
    await writeFile(join(repoPath, "README.md"), "hello\n");
    await runGit(["add", "README.md"], repoPath);
    await runGit(["commit", "-m", "init"], repoPath);
    await runGit(["checkout", "-b", "feature/fixer"], repoPath);
    await runGit(["checkout", "main"], repoPath);

    const store = new SqliteStore({
      dbPath: join(rootDir, "state", "looper.sqlite"),
    });
    store.initialize({ autoMigrate: true });
    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath,
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });

    const gateway = new GitWorktreeGateway({ gitPath: "git", store });
    const detached = await gateway.createWorktree({
      projectId: "project_1",
      repoPath,
      worktreeRoot,
      branch: "feature/fixer",
      baseBranch: "main",
      prNumber: 42,
      checkoutMode: "detached",
    });

    expect(
      (
        await runGit(["branch", "--show-current"], detached.worktreePath)
      ).trim(),
    ).toBe("");

    const attached = await gateway.createWorktree({
      projectId: "project_1",
      repoPath,
      worktreeRoot,
      branch: "feature/fixer",
      baseBranch: "main",
      prNumber: 42,
    });

    expect(attached.worktreePath).toBe(detached.worktreePath);
    expect(
      (
        await runGit(["branch", "--show-current"], attached.worktreePath)
      ).trim(),
    ).toBe("feature/fixer");

    store.close();
  });

  test("recreates a stored attached worktree when it is on the wrong branch", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-git-"));
    cleanupPaths.push(rootDir);
    const repoPath = join(rootDir, "repo");
    const worktreeRoot = join(rootDir, "worktrees");
    await mkdir(repoPath, { recursive: true });

    await runGit(["init", "-b", "main"], repoPath);
    await runGit(["config", "user.email", "test@example.com"], repoPath);
    await runGit(["config", "user.name", "Looper Test"], repoPath);
    await writeFile(join(repoPath, "README.md"), "hello\n");
    await runGit(["add", "README.md"], repoPath);
    await runGit(["commit", "-m", "init"], repoPath);
    await runGit(["checkout", "-b", "feature/fixer"], repoPath);
    await runGit(["checkout", "-b", "feature/other"], repoPath);
    await runGit(["checkout", "main"], repoPath);

    const store = new SqliteStore({
      dbPath: join(rootDir, "state", "looper.sqlite"),
    });
    store.initialize({ autoMigrate: true });
    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath,
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });

    const gateway = new GitWorktreeGateway({ gitPath: "git", store });
    const worktree = await gateway.createWorktree({
      projectId: "project_1",
      repoPath,
      worktreeRoot,
      branch: "feature/fixer",
      baseBranch: "main",
      prNumber: 42,
    });
    await runGit(["checkout", "feature/other"], worktree.worktreePath);

    const restored = await gateway.createWorktree({
      projectId: "project_1",
      repoPath,
      worktreeRoot,
      branch: "feature/fixer",
      baseBranch: "main",
      prNumber: 42,
    });

    expect(restored.worktreePath).toBe(worktree.worktreePath);
    expect(
      (
        await runGit(["branch", "--show-current"], restored.worktreePath)
      ).trim(),
    ).toBe("feature/fixer");

    store.close();
  });

  test("restores detached worktree from its expected path when store row is missing", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-git-"));
    cleanupPaths.push(rootDir);
    const repoPath = join(rootDir, "repo");
    const worktreeRoot = join(rootDir, "worktrees");
    await mkdir(repoPath, { recursive: true });

    await runGit(["init", "-b", "main"], repoPath);
    await runGit(["config", "user.email", "test@example.com"], repoPath);
    await runGit(["config", "user.name", "Looper Test"], repoPath);
    await writeFile(join(repoPath, "README.md"), "hello\n");
    await runGit(["add", "README.md"], repoPath);
    await runGit(["commit", "-m", "init"], repoPath);
    await runGit(["checkout", "-b", "feature/fixer"], repoPath);
    await runGit(["checkout", "main"], repoPath);

    const store = new SqliteStore({
      dbPath: join(rootDir, "state", "looper.sqlite"),
    });
    store.initialize({ autoMigrate: true });
    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath,
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });

    const gateway = new GitWorktreeGateway({ gitPath: "git", store });
    const detached = await gateway.createWorktree({
      projectId: "project_1",
      repoPath,
      worktreeRoot,
      branch: "feature/fixer",
      baseBranch: "main",
      prNumber: 42,
      checkoutMode: "detached",
    });
    const statelessGateway = new GitWorktreeGateway({ gitPath: "git" });

    const restored = await statelessGateway.createWorktree({
      projectId: "project_1",
      repoPath,
      worktreeRoot,
      branch: "feature/fixer",
      baseBranch: "main",
      prNumber: 42,
      checkoutMode: "detached",
    });

    expect(normalizeForTest(restored.worktreePath)).toBe(
      normalizeForTest(detached.worktreePath),
    );
    expect(
      (
        await runGit(["branch", "--show-current"], restored.worktreePath)
      ).trim(),
    ).toBe("");

    store.close();
  });

  test("recreates worktree when stored row points at a deleted path", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-git-"));
    cleanupPaths.push(rootDir);
    const repoPath = join(rootDir, "repo");
    const worktreeRoot = join(rootDir, "worktrees");
    await mkdir(repoPath, { recursive: true });

    await runGit(["init", "-b", "main"], repoPath);
    await runGit(["config", "user.email", "test@example.com"], repoPath);
    await runGit(["config", "user.name", "Looper Test"], repoPath);
    await writeFile(join(repoPath, "README.md"), "hello\n");
    await runGit(["add", "README.md"], repoPath);
    await runGit(["commit", "-m", "init"], repoPath);

    const store = new SqliteStore({
      dbPath: join(rootDir, "state", "looper.sqlite"),
    });
    store.initialize({ autoMigrate: true });
    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath,
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });

    const missingWorktreePath = join(
      worktreeRoot,
      "looper-fix-project_1-pr-42",
    );
    store.worktrees.upsert({
      id: "missing-record",
      projectId: "project_1",
      repoPath,
      worktreePath: missingWorktreePath,
      branch: "feature/fixer",
      baseBranch: "main",
      status: "active",
      headSha: null,
      metadataJson: JSON.stringify({ recovered: false }),
      createdAt: now,
      updatedAt: now,
      cleanedAt: null,
    });

    const gateway = new GitWorktreeGateway({ gitPath: "git", store });
    const recreated = await gateway.createWorktree({
      projectId: "project_1",
      repoPath,
      worktreeRoot,
      branch: "feature/fixer",
      baseBranch: "main",
      prNumber: 42,
      checkoutMode: "detached",
    });

    expect(normalizeForTest(recreated.worktreePath)).toBe(
      normalizeForTest(missingWorktreePath),
    );
    expect(
      (
        await runGit(["branch", "--show-current"], recreated.worktreePath)
      ).trim(),
    ).toBe("");

    store.close();
  });

  test("ignores stored worktrees that belong to a different repo path", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-git-"));
    cleanupPaths.push(rootDir);
    const repoPath = join(rootDir, "repo");
    const otherRepoPath = join(rootDir, "other-repo");
    const worktreeRoot = join(rootDir, "worktrees");
    const strayWorktreePath = join(rootDir, "stray-worktree");
    await mkdir(repoPath, { recursive: true });
    await mkdir(otherRepoPath, { recursive: true });
    await mkdir(strayWorktreePath, { recursive: true });

    await runGit(["init", "-b", "main"], repoPath);
    await runGit(["config", "user.email", "test@example.com"], repoPath);
    await runGit(["config", "user.name", "Looper Test"], repoPath);
    await writeFile(join(repoPath, "README.md"), "hello\n");
    await runGit(["add", "README.md"], repoPath);
    await runGit(["commit", "-m", "init"], repoPath);

    const store = new SqliteStore({
      dbPath: join(rootDir, "state", "looper.sqlite"),
    });
    store.initialize({ autoMigrate: true });
    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath,
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });
    store.worktrees.upsert({
      id: "wrong-repo-record",
      projectId: "project_1",
      repoPath: otherRepoPath,
      worktreePath: strayWorktreePath,
      branch: "feature/fixer",
      baseBranch: "main",
      status: "active",
      headSha: null,
      metadataJson: JSON.stringify({ recovered: false }),
      createdAt: now,
      updatedAt: now,
      cleanedAt: null,
    });

    const gateway = new GitWorktreeGateway({ gitPath: "git", store });
    const worktree = await gateway.createWorktree({
      projectId: "project_1",
      repoPath,
      worktreeRoot,
      branch: "feature/fixer",
      baseBranch: "main",
      prNumber: 42,
      checkoutMode: "detached",
    });

    expect(worktree.worktreePath).not.toBe(strayWorktreePath);
    expect(
      normalizeForTest(
        store.worktrees.getByBranch("project_1", "feature/fixer")?.repoPath,
      ),
    ).toBe(normalizeForTest(repoPath));

    store.close();
  });

  test("does not treat the primary checkout as a restorable worktree", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-git-"));
    cleanupPaths.push(rootDir);
    const repoPath = join(rootDir, "repo");
    const worktreeRoot = join(rootDir, "worktrees");
    await mkdir(repoPath, { recursive: true });

    await runGit(["init", "-b", "main"], repoPath);
    await runGit(["config", "user.email", "test@example.com"], repoPath);
    await runGit(["config", "user.name", "Looper Test"], repoPath);
    await writeFile(join(repoPath, "README.md"), "hello\n");
    await runGit(["add", "README.md"], repoPath);
    await runGit(["commit", "-m", "init"], repoPath);
    await runGit(["checkout", "-b", "feature/fixer"], repoPath);

    const store = new SqliteStore({
      dbPath: join(rootDir, "state", "looper.sqlite"),
    });
    store.initialize({ autoMigrate: true });
    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath,
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });

    const gateway = new GitWorktreeGateway({ gitPath: "git", store });
    const restored = await gateway.restoreWorktree({
      projectId: "project_1",
      repoPath,
      branch: "feature/fixer",
      worktreeRoot,
    });

    expect(restored).toBeNull();

    store.close();
  });

  test("reuses an existing branch worktree record when recreating worktree", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-git-"));
    cleanupPaths.push(rootDir);
    const repoPath = join(rootDir, "repo");
    const worktreeRoot = join(rootDir, "worktrees");
    await mkdir(repoPath, { recursive: true });

    await runGit(["init", "-b", "main"], repoPath);
    await runGit(["config", "user.email", "test@example.com"], repoPath);
    await runGit(["config", "user.name", "Looper Test"], repoPath);
    await writeFile(join(repoPath, "README.md"), "hello\n");
    await runGit(["add", "README.md"], repoPath);
    await runGit(["commit", "-m", "init"], repoPath);

    const store = new SqliteStore({
      dbPath: join(rootDir, "state", "looper.sqlite"),
    });
    store.initialize({ autoMigrate: true });
    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath,
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });
    store.worktrees.upsert({
      id: "existing-record",
      projectId: "project_1",
      repoPath,
      worktreePath: repoPath,
      branch: "feature/fixer",
      baseBranch: "main",
      status: "active",
      headSha: null,
      metadataJson: JSON.stringify({ recovered: false }),
      createdAt: now,
      updatedAt: now,
      cleanedAt: null,
    });

    const gateway = new GitWorktreeGateway({ gitPath: "git", store });
    const worktree = await gateway.createWorktree({
      projectId: "project_1",
      repoPath,
      worktreeRoot,
      branch: "feature/fixer",
      baseBranch: "main",
      prNumber: 42,
    });

    expect(worktree.id).toBe("existing-record");
    expect(worktree.worktreePath).not.toBe(repoPath);
    expect(store.worktrees.getByBranch("project_1", "feature/fixer")?.id).toBe(
      "existing-record",
    );

    store.close();
  });
});

function normalizeForTest(path: string | null | undefined): string | null {
  return path ? path.replace(/^\/private/, "") : null;
}
