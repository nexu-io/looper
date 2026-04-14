import { describe, expect, test } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { InvalidProjectIdError } from "../config/index";
import { SqliteStore } from "../storage/sqlite/sqlite-store";
import { ProjectManager } from "./index";

function createLogger() {
  return {
    debug() {},
    info() {},
    warn() {},
    error() {},
  };
}

describe("ProjectManager", () => {
  test("adds a project and discovers PRs and worktrees", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-projects-"));
    const store = new SqliteStore({
      dbPath: join(rootDir, "state", "looper.sqlite"),
      backupDir: join(rootDir, "backups"),
    });
    store.initialize({ autoMigrate: true });

    const manager = new ProjectManager({
      store,
      logger: createLogger(),
      now: () => new Date("2026-04-11T12:00:00.000Z"),
      git: {
        detectGitHubRepo: async () => "powerformer/looper",
        listWorktrees: async () => [
          {
            path: join(rootDir, "repo"),
            branch: "main",
            headSha: "abc123",
            bare: false,
          },
          {
            path: join(rootDir, "wt-pr-1"),
            branch: "pr-1",
            headSha: "def456",
            bare: false,
          },
        ],
      },
      github: {
        listOpenPullRequests: async () => [
          {
            number: 1,
            title: "PR 1",
            isDraft: false,
            state: "OPEN",
            labels: [],
            reviewRequests: [],
          },
          {
            number: 2,
            title: "Draft PR",
            isDraft: true,
            state: "OPEN",
            labels: [],
            reviewRequests: [],
          },
        ],
        capturePullRequestSnapshot: async ({ projectId, repo, prNumber }) => ({
          id: "snapshot_1",
          projectId,
          repo,
          prNumber,
          headSha: "abc123",
          baseSha: "base123",
          title: "PR 1",
          body: null,
          author: "octocat",
          diffRef: null,
          checksSummary: "green",
          unresolvedThreadCount: 0,
          reviewState: "approved",
          payloadJson: JSON.stringify({ prNumber }),
          capturedAt: "2026-04-11T12:00:00.000Z",
          createdAt: "2026-04-11T12:00:00.000Z",
        }),
      },
    });

    const result = await manager.addProject({
      id: "looper",
      name: "looper",
      repoPath: join(rootDir, "repo"),
      baseBranch: "main",
    });

    expect(result.repo).toBe("powerformer/looper");
    expect(result.discoveredWorktrees).toBe(2);
    expect(result.discoveredPullRequests).toBe(1);
    expect(result.warnings).toHaveLength(0);
    expect(store.projects.getById("looper")?.name).toBe("looper");
    expect(store.worktrees.listByProject("looper")).toHaveLength(2);
    expect(
      store.pullRequestSnapshots.getLatest("powerformer/looper", 1)?.title,
    ).toBe("PR 1");

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });

  test("rejects unsafe project ids", async () => {
    const rootDir = await mkdtemp(join(tmpdir(), "looper-projects-"));
    const store = new SqliteStore({
      dbPath: join(rootDir, "state", "looper.sqlite"),
      backupDir: join(rootDir, "backups"),
    });
    store.initialize({ autoMigrate: true });

    const manager = new ProjectManager({
      store,
      logger: createLogger(),
      git: {
        detectGitHubRepo: async () => null,
        listWorktrees: async () => [],
      },
      github: {
        listOpenPullRequests: async () => [],
        capturePullRequestSnapshot: async () => {
          throw new Error("not implemented");
        },
      },
    });

    await expect(
      manager.addProject({
        id: "../tmp",
        name: "looper",
        repoPath: join(rootDir, "repo"),
        baseBranch: "main",
      }),
    ).rejects.toBeInstanceOf(InvalidProjectIdError);

    store.close();
    await rm(rootDir, { recursive: true, force: true });
  });
});
