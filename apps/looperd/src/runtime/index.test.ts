import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { createLogger } from "../bootstrap/logger";
import { createDefaultLooperConfig } from "../config/index";
import type { FixerLoopRunner } from "../fixer/index";
import type { AgentResult, AgentRunInput } from "../infra/agent";
import { SchedulerQueue } from "../scheduler/index";
import { SqliteStore } from "../storage/sqlite/sqlite-store";
import type {
  PullRequestSnapshotRecord,
  WorktreeRecord,
} from "../storage/types";
import { createLooperdRuntime } from "./index";

const cleanupPaths: string[] = [];

afterEach(async () => {
  while (cleanupPaths.length > 0) {
    const path = cleanupPaths.pop();
    if (path) {
      await rm(path, { recursive: true, force: true });
    }
  }
});

async function createFixture() {
  const rootDir = await mkdtemp(join(tmpdir(), "looperd-runtime-"));
  cleanupPaths.push(rootDir);

  await Promise.all([
    mkdir(join(rootDir, "logs"), { recursive: true }),
    mkdir(join(rootDir, "workspace"), { recursive: true }),
  ]);

  const config = createDefaultLooperConfig(rootDir);
  config.server.host = "127.0.0.1";
  config.server.port = 0;
  config.storage.dbPath = join(rootDir, "state", "looper.sqlite");
  config.storage.backupDir = join(rootDir, "backups");
  config.daemon.logDir = join(rootDir, "logs");
  config.daemon.workingDirectory = join(rootDir, "workspace");

  const logger = await createLogger(config.logging, config.daemon.logDir);
  return { rootDir, config, logger };
}

class FakeGitHubGateway {
  public submitCalls: Array<{ repo: string; prNumber: number; event: string }> =
    [];
  public createPullRequestCalls = 0;
  public resolvedThreadIds: string[] = [];
  public removedLabels: Array<{
    repo: string;
    prNumber: number;
    labels: string[];
  }> = [];
  public reviewerRequests: Array<{
    repo: string;
    prNumber: number;
    reviewers: string[];
  }> = [];
  private fixerViewIndex = 0;

  public async listOpenPullRequests(input?: { label?: string }) {
    if (input?.label === "looper:spec-ready") {
      return [];
    }
    return [
      {
        number: 42,
        title: "Review and fix me",
        state: "OPEN",
        isDraft: false,
        reviewDecision: "CHANGES_REQUESTED",
        labels: [],
        author: "octocat",
        reviewRequests: ["octocat"],
      },
    ];
  }

  public async listOpenIssues() {
    return [];
  }

  public async viewIssue() {
    return {
      number: 123,
      title: "Planner issue",
      body: "body",
      url: "https://example.test/issues/123",
      state: "OPEN",
      author: "octocat",
      assignees: ["octocat"],
      labels: ["looper:plan"],
    };
  }

  public async viewPullRequest() {
    const fixerResponses = [
      {
        comments: [{ id: "c1", state: "UNRESOLVED", body: "needs fix" }],
        checks: [],
      },
      {
        comments: [{ id: "c1", state: "UNRESOLVED", body: "needs fix" }],
        checks: [],
      },
      {
        comments: [],
        checks: [],
      },
    ];
    const response = fixerResponses[this.fixerViewIndex] ??
      fixerResponses[fixerResponses.length - 1] ?? {
        comments: [],
        checks: [],
      };
    this.fixerViewIndex += 1;

    return {
      number: 42,
      title: "Review and fix me",
      body: "body",
      url: "https://example.test/pr/42",
      state: "OPEN",
      isDraft: false,
      reviewDecision: "CHANGES_REQUESTED",
      labels: [],
      headRefName: "feature/runtime",
      baseRefName: "main",
      headSha: "abc123",
      baseSha: "base123",
      author: "octocat",
      reviewRequests: ["octocat"],
      comments: response.comments,
      reviews: [],
      checks: response.checks,
    };
  }

  public async getCurrentUserLogin(): Promise<string> {
    return "octocat";
  }

  public async capturePullRequestSnapshot(input: {
    projectId: string;
    repo: string;
    prNumber: number;
    capturedAt?: string;
  }): Promise<PullRequestSnapshotRecord> {
    return {
      id: `snapshot:${input.prNumber}:${input.capturedAt}`,
      projectId: input.projectId,
      repo: input.repo,
      prNumber: input.prNumber,
      headSha: "abc123",
      baseSha: "base123",
      title: "Review and fix me",
      body: "body",
      author: "octocat",
      diffRef: "gh:diff",
      checksSummary: "SUCCESS",
      unresolvedThreadCount: 1,
      reviewState: "CHANGES_REQUESTED",
      payloadJson: JSON.stringify({ diff: "diff --git a/a.ts b/a.ts" }),
      capturedAt: input.capturedAt ?? "2026-04-11T12:00:00.000Z",
      createdAt: input.capturedAt ?? "2026-04-11T12:00:00.000Z",
    };
  }

  public async submitReview(input: {
    repo: string;
    prNumber: number;
    event: "APPROVE" | "COMMENT" | "REQUEST_CHANGES";
  }): Promise<void> {
    this.submitCalls.push(input);
  }

  public async createPullRequest(): Promise<{ number?: number; url: string }> {
    this.createPullRequestCalls += 1;
    return {
      number: 101,
      url: "https://example.test/pr/101",
    };
  }

  public async addPullRequestLabels(): Promise<void> {}

  public async removePullRequestLabels(input: {
    repo: string;
    prNumber: number;
    labels: string[];
  }): Promise<void> {
    this.removedLabels.push(input);
  }

  public async addPullRequestReviewers(input: {
    repo: string;
    prNumber: number;
    reviewers: string[];
  }): Promise<void> {
    this.reviewerRequests.push(input);
  }

  public async resolveReviewThread(input: {
    repo: string;
    threadId: string;
    cwd?: string;
  }): Promise<void> {
    this.resolvedThreadIds.push(input.threadId);
  }
}

class FakeGitGateway {
  public pushCalls = 0;
  public createWorktreeCalls = 0;
  public commitCalls = 0;
  public cleanupCalls = 0;
  public pushError?: string;

  public async detectGitHubRepo(_repoPath: string): Promise<string | null> {
    return "powerformer/looper";
  }

  public async push(): Promise<void> {
    if (this.pushError) {
      throw new Error(this.pushError);
    }
    this.pushCalls += 1;
  }

  public async prepareWorktree(): Promise<{
    headSha?: string;
    clean: boolean;
  }> {
    return { headSha: "abc123", clean: true };
  }

  public async inspectHead(): Promise<{
    headSha?: string;
    newCommitShas: string[];
    hasUncommittedChanges: boolean;
    changedFiles: string[];
  }> {
    return {
      headSha: "commit-1",
      newCommitShas: ["commit-1"],
      hasUncommittedChanges: false,
      changedFiles: [],
    };
  }

  public async commit(): Promise<{ commitSha: string }> {
    this.commitCalls += 1;
    return { commitSha: "commit-1" };
  }

  public async cleanupWorktree(): Promise<void> {
    this.cleanupCalls += 1;
  }

  public async createWorktree(input: {
    projectId: string;
    repoPath: string;
    worktreeRoot: string;
    branch: string;
    baseBranch: string;
    protectedBranches?: string[];
  }): Promise<WorktreeRecord> {
    this.createWorktreeCalls += 1;
    return {
      id: `worktree:${input.branch}`,
      projectId: input.projectId,
      repoPath: input.repoPath,
      worktreePath: input.worktreeRoot,
      branch: input.branch,
      baseBranch: input.baseBranch,
      status: "active",
      headSha: "abc123",
      metadataJson: null,
      createdAt: "2026-04-11T12:00:00.000Z",
      updatedAt: "2026-04-11T12:00:00.000Z",
      cleanedAt: null,
    };
  }
}

class FakeAgentExecutor {
  public starts: AgentRunInput[] = [];

  constructor(private readonly results: AgentResult[]) {}

  public async start(input: AgentRunInput) {
    this.starts.push(input);
    const result = this.results.shift();
    if (!result) {
      throw new Error("No agent result queued");
    }

    return {
      wait: async () => result,
    };
  }
}

function completedAgentResult(summary: string): AgentResult {
  return {
    status: "completed",
    summary,
    artifacts: [],
    changedFiles: [],
    commits: [],
    rawLogs: { stdout: `${summary}\n`, stderr: "" },
    parseStatus: "parsed",
    completionSignal: "done",
    heartbeatCount: 1,
    resourceUsage: {
      wallTimeMs: 10,
      outputBytes: summary.length,
    },
    pid: 1234,
  };
}

function failedAgentResult(summary: string): AgentResult {
  return {
    status: "failed",
    summary,
    artifacts: [],
    changedFiles: [],
    commits: [],
    rawLogs: { stdout: "", stderr: `${summary}\n` },
    parseStatus: "parsed",
    completionSignal: "done",
    heartbeatCount: 1,
    resourceUsage: {
      wallTimeMs: 10,
      outputBytes: summary.length,
    },
    pid: 1234,
  };
}

describe("createLooperdRuntime", () => {
  test("leaves orphan executions active when SIGTERM cannot be sent", async () => {
    const fixture = await createFixture();
    const store = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    store.initialize({ autoMigrate: true });

    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });
    store.agentExecutions.upsert({
      id: "agent_exec_1",
      projectId: "project_1",
      loopId: null,
      runId: null,
      vendor: "opencode",
      status: "running",
      pid: 12345,
      commandJson: JSON.stringify(["opencode", "run"]),
      cwd: fixture.rootDir,
      summary: null,
      parseStatus: null,
      completionSignal: null,
      heartbeatCount: 0,
      startedAt: now,
      lastHeartbeatAt: now,
      endedAt: null,
      outputJson: null,
      errorMessage: null,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });
    store.close();

    const originalKill = process.kill;
    process.kill = (() => {
      const error = new Error("permission denied") as NodeJS.ErrnoException;
      error.code = "EPERM";
      throw error;
    }) as typeof process.kill;

    try {
      const runtime = createLooperdRuntime({
        config: fixture.config,
        logger: fixture.logger,
      });

      await runtime.start();
      await runtime.stop("test");
    } finally {
      process.kill = originalKill;
    }

    const verifyStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    verifyStore.initialize();
    expect(verifyStore.agentExecutions.getById("agent_exec_1")?.status).toBe(
      "running",
    );
    expect(
      verifyStore.events
        .listByEntity("agent_execution", "agent_exec_1")
        .some((event) => event.eventType === "agent.killed"),
    ).toBe(false);
    verifyStore.close();
  });

  test("keeps stopped false when SIGTERM fails even if queue items are cancelled", async () => {
    const fixture = await createFixture();
    const originalKill = process.kill;
    process.kill = (() => {
      const error = new Error("permission denied") as NodeJS.ErrnoException;
      error.code = "EPERM";
      throw error;
    }) as typeof process.kill;

    try {
      const runtime = createLooperdRuntime({
        config: fixture.config,
        logger: fixture.logger,
      });

      await runtime.start();
      const store = new SqliteStore({
        dbPath: fixture.config.storage.dbPath,
        backupDir: fixture.config.storage.backupDir,
      });
      store.initialize({ autoMigrate: true });

      const now = "2026-04-11T12:00:00.000Z";
      store.projects.upsert({
        id: "project_1",
        name: "Looper",
        repoPath: fixture.rootDir,
        baseBranch: "main",
        archived: false,
        metadataJson: null,
        createdAt: now,
        updatedAt: now,
      });
      store.loops.upsert({
        id: "loop_1",
        seq: 1,
        projectId: "project_1",
        type: "reviewer",
        targetType: "pull_request",
        targetId: "pr:acme/looper:42",
        repo: "acme/looper",
        prNumber: 42,
        status: "running",
        configJson: null,
        metadataJson: null,
        lastRunAt: now,
        nextRunAt: now,
        createdAt: now,
        updatedAt: now,
      });
      store.runs.upsert({
        id: "run_1",
        loopId: "loop_1",
        status: "running",
        currentStep: "review",
        lastCompletedStep: "snapshot",
        checkpointJson: null,
        summary: null,
        errorMessage: null,
        startedAt: now,
        lastHeartbeatAt: now,
        endedAt: null,
        createdAt: now,
        updatedAt: now,
      });
      store.agentExecutions.upsert({
        id: "agent_exec_1",
        projectId: "project_1",
        loopId: "loop_1",
        runId: "run_1",
        vendor: "opencode",
        status: "running",
        pid: 12345,
        commandJson: JSON.stringify(["opencode", "run"]),
        cwd: fixture.rootDir,
        summary: null,
        parseStatus: null,
        completionSignal: null,
        heartbeatCount: 0,
        startedAt: now,
        lastHeartbeatAt: now,
        endedAt: null,
        outputJson: null,
        errorMessage: null,
        metadataJson: null,
        createdAt: now,
        updatedAt: now,
      });
      store.queue.upsert({
        id: "queue_1",
        projectId: "project_1",
        loopId: "loop_1",
        type: "reviewer",
        targetType: "pull_request",
        targetId: "pr:acme/looper:42",
        repo: "acme/looper",
        prNumber: 42,
        dedupeKey: "reviewer:acme/looper:42",
        priority: 1,
        status: "queued",
        availableAt: now,
        attempts: 0,
        maxAttempts: 3,
        claimedBy: null,
        claimedAt: null,
        startedAt: null,
        finishedAt: null,
        lockKey: "pr:acme/looper:42",
        payloadJson: null,
        lastError: null,
        lastErrorKind: null,
        createdAt: now,
        updatedAt: now,
      });
      store.close();

      const result = await (
        runtime as unknown as {
          stopLoop(input: { loopId: string; reason: string }): Promise<{
            stopped: boolean;
          }>;
        }
      ).stopLoop({
        loopId: "loop_1",
        reason: "test stop",
      });

      expect(result.stopped).toBe(false);

      const verifyStore = new SqliteStore({
        dbPath: fixture.config.storage.dbPath,
        backupDir: fixture.config.storage.backupDir,
      });
      verifyStore.initialize({ autoMigrate: true });
      expect(verifyStore.runs.getById("run_1")).toMatchObject({
        status: "running",
        endedAt: null,
        errorMessage: null,
      });
      expect(verifyStore.agentExecutions.getById("agent_exec_1")).toMatchObject(
        {
          status: "running",
          endedAt: null,
          errorMessage: null,
        },
      );
      expect(verifyStore.queue.getById("queue_1")).toMatchObject({
        status: "cancelled",
        lastError: "test stop",
      });
      verifyStore.close();

      await runtime.stop("test");
    } finally {
      process.kill = originalKill;
    }
  });

  test("treats ESRCH while stopping the active agent as already terminated", async () => {
    const fixture = await createFixture();
    const originalKill = process.kill;
    process.kill = (() => {
      const error = new Error("no such process") as NodeJS.ErrnoException;
      error.code = "ESRCH";
      throw error;
    }) as typeof process.kill;

    try {
      const runtime = createLooperdRuntime({
        config: fixture.config,
        logger: fixture.logger,
      });

      await runtime.start();
      const store = new SqliteStore({
        dbPath: fixture.config.storage.dbPath,
        backupDir: fixture.config.storage.backupDir,
      });
      store.initialize({ autoMigrate: true });

      const now = "2026-04-11T12:00:00.000Z";
      store.projects.upsert({
        id: "project_1",
        name: "Looper",
        repoPath: fixture.rootDir,
        baseBranch: "main",
        archived: false,
        metadataJson: null,
        createdAt: now,
        updatedAt: now,
      });
      store.loops.upsert({
        id: "loop_1",
        seq: 1,
        projectId: "project_1",
        type: "reviewer",
        targetType: "pull_request",
        targetId: "pr:acme/looper:42",
        repo: "acme/looper",
        prNumber: 42,
        status: "running",
        configJson: null,
        metadataJson: null,
        lastRunAt: now,
        nextRunAt: now,
        createdAt: now,
        updatedAt: now,
      });
      store.runs.upsert({
        id: "run_1",
        loopId: "loop_1",
        status: "running",
        currentStep: "review",
        lastCompletedStep: "snapshot",
        checkpointJson: null,
        summary: null,
        errorMessage: null,
        startedAt: now,
        lastHeartbeatAt: now,
        endedAt: null,
        createdAt: now,
        updatedAt: now,
      });
      store.agentExecutions.upsert({
        id: "agent_exec_1",
        projectId: "project_1",
        loopId: "loop_1",
        runId: "run_1",
        vendor: "opencode",
        status: "running",
        pid: 12345,
        commandJson: JSON.stringify(["opencode", "run"]),
        cwd: fixture.rootDir,
        summary: null,
        parseStatus: null,
        completionSignal: null,
        heartbeatCount: 0,
        startedAt: now,
        lastHeartbeatAt: now,
        endedAt: null,
        outputJson: null,
        errorMessage: null,
        metadataJson: null,
        createdAt: now,
        updatedAt: now,
      });
      store.close();

      const result = await (
        runtime as unknown as {
          stopLoop(input: { loopId: string; reason: string }): Promise<{
            stopped: boolean;
          }>;
        }
      ).stopLoop({
        loopId: "loop_1",
        reason: "test stop",
      });

      expect(result.stopped).toBe(true);

      const verifyStore = new SqliteStore({
        dbPath: fixture.config.storage.dbPath,
        backupDir: fixture.config.storage.backupDir,
      });
      verifyStore.initialize({ autoMigrate: true });
      expect(verifyStore.runs.getById("run_1")).toMatchObject({
        status: "cancelled",
        errorMessage: "test stop",
      });
      expect(verifyStore.agentExecutions.getById("agent_exec_1")).toMatchObject(
        {
          status: "killed",
          errorMessage: "test stop",
        },
      );
      verifyStore.close();

      await runtime.stop("test");
    } finally {
      process.kill = originalKill;
    }
  });

  test("keeps the run active after sending SIGTERM to the agent process", async () => {
    const fixture = await createFixture();
    const originalKill = process.kill;
    const killCalls: Array<{ pid: number; signal?: NodeJS.Signals | number }> =
      [];
    process.kill = ((pid: number, signal?: NodeJS.Signals | number) => {
      killCalls.push({ pid, signal });
      return true;
    }) as typeof process.kill;

    try {
      const runtime = createLooperdRuntime({
        config: fixture.config,
        logger: fixture.logger,
      });

      await runtime.start();
      const store = new SqliteStore({
        dbPath: fixture.config.storage.dbPath,
        backupDir: fixture.config.storage.backupDir,
      });
      store.initialize({ autoMigrate: true });

      const now = "2026-04-11T12:00:00.000Z";
      store.projects.upsert({
        id: "project_1",
        name: "Looper",
        repoPath: fixture.rootDir,
        baseBranch: "main",
        archived: false,
        metadataJson: null,
        createdAt: now,
        updatedAt: now,
      });
      store.loops.upsert({
        id: "loop_1",
        seq: 1,
        projectId: "project_1",
        type: "reviewer",
        targetType: "pull_request",
        targetId: "pr:acme/looper:42",
        repo: "acme/looper",
        prNumber: 42,
        status: "running",
        configJson: null,
        metadataJson: null,
        lastRunAt: now,
        nextRunAt: now,
        createdAt: now,
        updatedAt: now,
      });
      store.runs.upsert({
        id: "run_1",
        loopId: "loop_1",
        status: "running",
        currentStep: "review",
        lastCompletedStep: "snapshot",
        checkpointJson: null,
        summary: null,
        errorMessage: null,
        startedAt: now,
        lastHeartbeatAt: now,
        endedAt: null,
        createdAt: now,
        updatedAt: now,
      });
      store.agentExecutions.upsert({
        id: "agent_exec_1",
        projectId: "project_1",
        loopId: "loop_1",
        runId: "run_1",
        vendor: "opencode",
        status: "running",
        pid: 12345,
        commandJson: JSON.stringify(["opencode", "run"]),
        cwd: fixture.rootDir,
        summary: null,
        parseStatus: null,
        completionSignal: null,
        heartbeatCount: 0,
        startedAt: now,
        lastHeartbeatAt: now,
        endedAt: null,
        outputJson: null,
        errorMessage: null,
        metadataJson: null,
        createdAt: now,
        updatedAt: now,
      });
      store.close();

      const result = await (
        runtime as unknown as {
          stopLoop(input: { loopId: string; reason: string }): Promise<{
            stopped: boolean;
          }>;
        }
      ).stopLoop({
        loopId: "loop_1",
        reason: "test stop",
      });

      expect(result.stopped).toBe(true);
      expect(killCalls).toEqual([{ pid: 12345, signal: "SIGTERM" }]);

      const verifyStore = new SqliteStore({
        dbPath: fixture.config.storage.dbPath,
        backupDir: fixture.config.storage.backupDir,
      });
      verifyStore.initialize({ autoMigrate: true });
      expect(verifyStore.runs.getById("run_1")).toMatchObject({
        status: "running",
        endedAt: null,
        errorMessage: null,
      });
      expect(verifyStore.agentExecutions.getById("agent_exec_1")).toMatchObject(
        {
          status: "cancelling",
          endedAt: null,
          errorMessage: "test stop",
        },
      );
      verifyStore.close();

      await runtime.stop("test");
    } finally {
      process.kill = originalKill;
    }
  });

  test("does not stop or cancel active run without an interruptible execution pid", async () => {
    const fixture = await createFixture();

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
    });

    await runtime.start();
    await Bun.sleep(100);
    await Bun.sleep(50);
    await Bun.sleep(50);
    const store = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    store.initialize({ autoMigrate: true });

    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });
    store.loops.upsert({
      id: "loop_1",
      seq: 1,
      projectId: "project_1",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      status: "running",
      configJson: null,
      metadataJson: null,
      lastRunAt: now,
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });
    store.runs.upsert({
      id: "run_1",
      loopId: "loop_1",
      status: "running",
      currentStep: "review",
      lastCompletedStep: "snapshot",
      checkpointJson: null,
      summary: null,
      errorMessage: null,
      startedAt: now,
      lastHeartbeatAt: now,
      endedAt: null,
      createdAt: now,
      updatedAt: now,
    });
    store.agentExecutions.upsert({
      id: "agent_exec_1",
      projectId: "project_1",
      loopId: "loop_1",
      runId: "run_1",
      vendor: "opencode",
      status: "running",
      pid: null,
      commandJson: JSON.stringify(["opencode", "run"]),
      cwd: fixture.rootDir,
      summary: null,
      parseStatus: null,
      completionSignal: null,
      heartbeatCount: 0,
      startedAt: now,
      lastHeartbeatAt: now,
      endedAt: null,
      outputJson: null,
      errorMessage: null,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });
    store.close();

    const result = await (
      runtime as unknown as {
        stopLoop(input: { loopId: string; reason: string }): Promise<{
          stopped: boolean;
        }>;
      }
    ).stopLoop({
      loopId: "loop_1",
      reason: "test stop",
    });

    expect(result.stopped).toBe(false);

    const verifyStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    verifyStore.initialize({ autoMigrate: true });
    expect(verifyStore.runs.getById("run_1")).toMatchObject({
      status: "running",
      endedAt: null,
      errorMessage: null,
    });
    expect(verifyStore.agentExecutions.getById("agent_exec_1")).toMatchObject({
      status: "running",
      pid: null,
      errorMessage: null,
    });
    verifyStore.close();

    await runtime.stop("test");
  });

  test("runs recovery before serving API and marks interrupted work", async () => {
    const fixture = await createFixture();
    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });

    const now = new Date(Date.now() - 1_000).toISOString();
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });
    seedStore.loops.upsert({
      id: "loop_1",
      seq: 1,
      projectId: "project_1",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      status: "running",
      configJson: null,
      metadataJson: null,
      lastRunAt: now,
      nextRunAt: null,
      createdAt: now,
      updatedAt: now,
    });
    seedStore.runs.upsert({
      id: "run_1",
      loopId: "loop_1",
      status: "running",
      currentStep: "review",
      lastCompletedStep: "snapshot",
      checkpointJson: null,
      summary: null,
      errorMessage: null,
      startedAt: now,
      lastHeartbeatAt: now,
      endedAt: null,
      createdAt: now,
      updatedAt: now,
    });
    seedStore.queue.upsert({
      id: "queue_reviewer_1",
      projectId: "project_1",
      loopId: "loop_1",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      dedupeKey: "reviewer:acme/looper:42",
      priority: 2,
      status: "running",
      availableAt: now,
      attempts: 0,
      maxAttempts: 3,
      claimedBy: "executor_1",
      claimedAt: now,
      startedAt: now,
      finishedAt: null,
      lockKey: "pr:acme/looper:42",
      payloadJson: null,
      lastError: null,
      lastErrorKind: null,
      createdAt: now,
      updatedAt: now,
    });
    seedStore.locks.acquire({
      key: "pr:acme/looper:42",
      owner: "reviewer-loop",
      reason: "claim",
      expiresAt: "2020-01-01T00:00:00.000Z",
      createdAt: now,
      updatedAt: now,
    });
    seedStore.close();

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
    });

    await runtime.start();
    await Bun.sleep(50);

    const response = await fetch(
      `http://${fixture.config.server.host}:${fixture.config.server.port}/api/v1/status`,
    );
    const body = (await response.json()) as {
      ok: boolean;
      data: { service: { recovery: { interruptedRunsMarked: number } } };
    };

    expect(response.status).toBe(200);
    expect(body.ok).toBe(true);
    expect(body.data.service.recovery.interruptedRunsMarked).toBe(1);

    const verifyStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
    });
    verifyStore.initialize();
    expect(verifyStore.runs.getById("run_1")?.status).toBe("interrupted");
    expect(verifyStore.loops.getById("loop_1")?.status).toBe("queued");
    expect(verifyStore.queue.getById("queue_reviewer_1")?.claimedBy).not.toBe(
      "executor_1",
    );
    const requeueEvent = verifyStore.events
      .listByEntity("loop", "loop_1")
      .find((event) => event.eventType === "looperd.recovery.loop_requeued");
    expect(requeueEvent).toBeDefined();
    expect(requeueEvent?.payloadJson).toContain('"recoveredQueueItems":1');
    expect(verifyStore.locks.get("pr:acme/looper:42")).toBeNull();
    expect(
      verifyStore.events
        .list()
        .some((event) => event.eventType === "looperd.started"),
    ).toBe(true);
    verifyStore.close();

    await runtime.stop("test");
  });

  test("auto-discovers and processes reviewer work on startup", async () => {
    const fixture = await createFixture();
    fixture.config.agent.vendor = "opencode";
    fixture.config.defaults.allowAutoCommit = true;
    const now = new Date(Date.now() - 1_000).toISOString();
    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
      createdAt: now,
      updatedAt: now,
    });
    seedStore.close();

    const github = new FakeGitHubGateway();
    const agentExecutor = new FakeAgentExecutor([
      completedAgentResult("Looks good overall"),
    ]);

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
      github,
      agentExecutor,
      enableFixer: false,
    });

    await runtime.start();

    const verifyStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
    });
    verifyStore.initialize();

    expect(
      verifyStore.loops.list().some((loop) => loop.type === "reviewer"),
    ).toBe(true);
    verifyStore.close();

    await runtime.stop("test");
  });

  test("auto-discovers planner work whenever planner runner exists", async () => {
    const fixture = await createFixture();
    fixture.config.agent.vendor = "opencode";
    const now = new Date(Date.now() - 1_000).toISOString();
    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
      createdAt: now,
      updatedAt: now,
    });
    seedStore.loops.upsert({
      id: "loop_planner_1",
      seq: 1,
      projectId: "project_1",
      type: "planner",
      targetType: "issue",
      targetId: "issue:powerformer/looper:123",
      repo: "powerformer/looper",
      prNumber: null,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });
    const scheduler = new SchedulerQueue({
      store: seedStore,
      retryMaxAttempts: 3,
      retryBaseDelayMs: 0,
    });
    scheduler.enqueue({
      id: "queue_planner_1",
      projectId: "project_1",
      loopId: "loop_planner_1",
      type: "planner",
      targetType: "issue",
      targetId: "issue:powerformer/looper:123",
      repo: "powerformer/looper",
      dedupeKey: "planner:powerformer/looper:123",
      payloadJson: JSON.stringify({ issueNumber: 123 }),
      availableAt: now,
    });
    seedStore.close();

    let discoverCalls = 0;
    const processedQueueItemIds: string[] = [];
    const plannerRunner = {
      discoverIssues: async () => {
        discoverCalls += 1;
        return { queueItems: [], createdLoopIds: [], skipped: 0 };
      },
      processClaimedItem: async (queueItem: { id: string; type: string }) => {
        processedQueueItemIds.push(queueItem.id);
        return {
          loopId: "loop_planner_1",
          runId: "run_planner_1",
          queueItemId: queueItem.id,
          status: "success" as const,
          summary: "planned",
        };
      },
    };

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
      github: new FakeGitHubGateway(),
      git: new FakeGitGateway(),
      agentExecutor: new FakeAgentExecutor([]),
      plannerRunner: plannerRunner as never,
      enablePlanner: false,
      enableReviewer: false,
      enableFixer: false,
    });

    await runtime.start();
    await Bun.sleep(50);

    expect(discoverCalls).toBeGreaterThan(0);
    expect(processedQueueItemIds).toEqual(["queue_planner_1"]);

    await runtime.stop("test");
  });

  test("sends failure notification for failed reviewer run", async () => {
    const fixture = await createFixture();
    fixture.config.agent.vendor = "opencode";
    fixture.config.defaults.allowAutoCommit = true;
    const now = "2026-04-11T12:00:00.000Z";
    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
      createdAt: now,
      updatedAt: now,
    });
    seedStore.close();

    const github = new FakeGitHubGateway();
    const agentExecutor = new FakeAgentExecutor([
      failedAgentResult("agent crashed while reviewing"),
    ]);

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
      github,
      agentExecutor,
      enableFixer: false,
    });

    await runtime.start();
    await Bun.sleep(100);
    await Bun.sleep(50);

    const verifyStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
    });
    verifyStore.initialize();

    const failureNotifications = verifyStore.notifications
      .list()
      .filter(
        (record) =>
          record.channel === "in_app" &&
          record.level === "failure" &&
          record.entityType === "run",
      );
    expect(failureNotifications.length).toBeGreaterThan(0);
    expect(failureNotifications[0]?.body).toContain(
      "agent crashed while reviewing",
    );

    verifyStore.close();
    await runtime.stop("test");
  });

  test("auto-discovers and processes fixer work on startup", async () => {
    const fixture = await createFixture();
    fixture.config.agent.vendor = "opencode";
    const now = "2026-04-11T12:00:00.000Z";
    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
      createdAt: now,
      updatedAt: now,
    });
    seedStore.close();

    const github = new FakeGitHubGateway();
    const git = new FakeGitGateway();
    const agentExecutor = new FakeAgentExecutor([
      completedAgentResult("fixed"),
    ]);

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
      github,
      git,
      agentExecutor,
      enableReviewer: false,
    });

    await runtime.start();

    const verifyStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
    });
    verifyStore.initialize();

    expect(verifyStore.loops.list().some((loop) => loop.type === "fixer")).toBe(
      true,
    );
    verifyStore.close();

    await runtime.stop("test");
  });

  test("does not notify user for retryable fixer remote-head races", async () => {
    const fixture = await createFixture();
    fixture.config.agent.vendor = "opencode";
    const now = "2026-04-11T12:00:00.000Z";
    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
      createdAt: now,
      updatedAt: now,
    });
    seedStore.close();

    const github = new FakeGitHubGateway();
    const git = new FakeGitGateway();
    git.pushError =
      "Remote head changed for feature/runtime: expected abc123, got def456";
    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
      github,
      git,
      agentExecutor: new FakeAgentExecutor([completedAgentResult("fixed")]),
      enableReviewer: false,
    });

    await runtime.start();
    await Bun.sleep(50);

    const verifyStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
    });
    verifyStore.initialize();
    const failureNotifications = verifyStore.notifications
      .list()
      .filter(
        (record) =>
          record.channel === "in_app" &&
          record.level === "failure" &&
          record.entityType === "run",
      );

    expect(
      failureNotifications.some((record) =>
        record.body.includes("Remote head changed"),
      ),
    ).toBe(false);

    verifyStore.close();
    await runtime.stop("test");
  });

  test("does not notify user for retryable fixer PR-head sync delays", async () => {
    const fixture = await createFixture();
    fixture.config.agent.vendor = "opencode";
    const now = "2026-04-11T12:00:00.000Z";
    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
      createdAt: now,
      updatedAt: now,
    });
    seedStore.close();

    const github = new FakeGitHubGateway();
    const fixerRunner = {
      discoverPullRequests: async () => ({
        queueItems: [],
        createdLoopIds: [],
        skipped: 0,
      }),
      processClaimedItem: async () => ({
        loopId: "loop_1",
        runId: "run_1",
        queueItemId: "queue_1",
        status: "failed" as const,
        failureKind: "retryable_after_resume" as const,
        summary:
          "PR head changed before resolving comments: expected new-head, got old-head",
      }),
    };
    const store = new SqliteStore({ dbPath: fixture.config.storage.dbPath });
    store.initialize();
    store.loops.upsert({
      id: "loop_1",
      seq: 1,
      projectId: "project_1",
      type: "fixer",
      targetType: "pull_request",
      targetId: "pr:powerformer/looper:1",
      repo: "powerformer/looper",
      prNumber: 1,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });
    const scheduler = new SchedulerQueue({
      store,
      retryMaxAttempts: 3,
      retryBaseDelayMs: 5_000,
      now: () => new Date(now),
    });
    scheduler.enqueue({
      id: "queue_1",
      projectId: "project_1",
      loopId: "loop_1",
      type: "fixer",
      targetType: "pull_request",
      targetId: "pr:powerformer/looper:1",
      repo: "powerformer/looper",
      prNumber: 1,
      dedupeKey: "fixer:1",
      payloadJson: null,
      availableAt: now,
      maxAttempts: 3,
      lockKey: null,
    });
    store.close();

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
      github,
      git: new FakeGitGateway(),
      agentExecutor: new FakeAgentExecutor([completedAgentResult("fixed")]),
      fixerRunner: fixerRunner as unknown as FixerLoopRunner,
      enableReviewer: false,
    });

    await runtime.start();
    await Bun.sleep(50);

    const verifyStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
    });
    verifyStore.initialize();
    const failureNotifications = verifyStore.notifications
      .list()
      .filter(
        (record) =>
          record.channel === "in_app" &&
          record.level === "failure" &&
          record.entityType === "run",
      );
    expect(
      failureNotifications.some((record) =>
        record.body.includes("PR head changed before resolving comments"),
      ),
    ).toBe(false);

    verifyStore.close();
    await runtime.stop("test");
  });

  test("boots without coding agent and skips reviewer/fixer runners", async () => {
    const fixture = await createFixture();
    const now = "2026-04-11T12:00:00.000Z";
    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
      createdAt: now,
      updatedAt: now,
    });
    seedStore.close();

    const github = new FakeGitHubGateway();
    const git = new FakeGitGateway();

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
      github,
      git,
    });

    await runtime.start();

    const verifyStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
    });
    verifyStore.initialize();

    expect(
      verifyStore.loops.list().some((loop) => loop.type === "reviewer"),
    ).toBe(false);
    expect(verifyStore.loops.list().some((loop) => loop.type === "fixer")).toBe(
      false,
    );
    verifyStore.close();

    await runtime.stop("test");
  });

  test("processes scheduled worker queue items", async () => {
    const fixture = await createFixture();
    fixture.config.agent.vendor = "opencode";
    fixture.config.defaults.openPrStrategy = "all_done";
    const now = new Date(Date.now() - 1_000).toISOString();
    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
      createdAt: now,
      updatedAt: now,
    });
    seedStore.loops.upsert({
      id: "loop_worker_1",
      seq: 1,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: "powerformer/looper",
      prNumber: null,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });
    const scheduler = new SchedulerQueue({
      store: seedStore,
      retryMaxAttempts: 3,
      retryBaseDelayMs: 0,
    });
    scheduler.enqueue({
      id: "queue_worker_1",
      projectId: "project_1",
      loopId: "loop_worker_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: "powerformer/looper",
      dedupeKey: "worker:loop_worker_1",
      payloadJson: JSON.stringify({
        title: "Implement worker loop",
        specPath: "spec.md",
        repo: "powerformer/looper",
        baseBranch: "main",
      }),
      availableAt: now,
    });
    await writeFile(join(fixture.rootDir, "spec.md"), "# Worker spec\n");
    seedStore.close();

    const github = new FakeGitHubGateway();
    const git = new FakeGitGateway();
    const agentExecutor = new FakeAgentExecutor([completedAgentResult("done")]);

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
      github,
      git,
      agentExecutor,
      enableReviewer: false,
      enableFixer: false,
    });

    await runtime.start();
    await Bun.sleep(50);

    expect(agentExecutor.starts).toHaveLength(1);
    expect(git.createWorktreeCalls).toBe(1);
    expect(git.pushCalls).toBe(1);
    expect(github.createPullRequestCalls).toBe(1);

    const verifyStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
    });
    verifyStore.initialize();
    expect(verifyStore.loops.getById("loop_worker_1")?.prNumber).toBe(101);
    verifyStore.close();

    await runtime.stop("test");
  });

  test("starts multiple scheduled workers in parallel without serial blocking", async () => {
    const fixture = await createFixture();
    fixture.config.agent.vendor = "opencode";
    fixture.config.scheduler.maxConcurrentRuns = 2;
    fixture.config.scheduler.pollIntervalSeconds = 3600;
    const now = new Date(Date.now() - 1_000).toISOString();
    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
      createdAt: now,
      updatedAt: now,
    });
    seedStore.loops.upsert({
      id: "loop_worker_1",
      seq: 1,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: "powerformer/looper",
      prNumber: null,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });
    seedStore.loops.upsert({
      id: "loop_worker_2",
      seq: 2,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: "powerformer/looper",
      prNumber: null,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });
    const scheduler = new SchedulerQueue({
      store: seedStore,
      retryMaxAttempts: 3,
      retryBaseDelayMs: 0,
    });
    scheduler.enqueue({
      id: "queue_worker_1",
      projectId: "project_1",
      loopId: "loop_worker_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: "powerformer/looper",
      dedupeKey: "worker:loop_worker_1",
      payloadJson: JSON.stringify({
        title: "Worker one",
        prompt: "Implement worker one",
        repo: "powerformer/looper",
        baseBranch: "main",
      }),
      availableAt: now,
    });
    scheduler.enqueue({
      id: "queue_worker_2",
      projectId: "project_1",
      loopId: "loop_worker_2",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: "powerformer/looper",
      dedupeKey: "worker:loop_worker_2",
      payloadJson: JSON.stringify({
        title: "Worker two",
        prompt: "Implement worker two",
        repo: "powerformer/looper",
        baseBranch: "main",
      }),
      availableAt: now,
    });
    seedStore.close();

    let resolveFirstRun: (() => void) | undefined;
    const firstRunGate = new Promise<void>((resolve) => {
      resolveFirstRun = resolve;
    });
    const startedQueueItemIds: string[] = [];
    const workerRunner = {
      discoverPullRequests: async () => ({
        queueItems: [],
        createdLoopIds: [],
        skipped: 0,
      }),
      processClaimedItem: async (queueItem: { id: string }) => {
        startedQueueItemIds.push(queueItem.id);
        if (queueItem.id === "queue_worker_1") {
          await firstRunGate;
        }

        return {
          loopId:
            queueItem.id === "queue_worker_1"
              ? "loop_worker_1"
              : "loop_worker_2",
          runId:
            queueItem.id === "queue_worker_1" ? "run_worker_1" : "run_worker_2",
          queueItemId: queueItem.id,
          status: "success" as const,
          summary: "done",
        };
      },
    };

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
      github: new FakeGitHubGateway(),
      git: new FakeGitGateway(),
      agentExecutor: new FakeAgentExecutor([]),
      workerRunner: workerRunner as never,
      enableReviewer: false,
      enableFixer: false,
      enablePlanner: false,
    });

    await runtime.start();
    await Bun.sleep(50);

    expect(startedQueueItemIds).toContain("queue_worker_1");
    expect(startedQueueItemIds).toContain("queue_worker_2");

    resolveFirstRun?.();
    await Bun.sleep(20);
    await runtime.stop("test");
  });

  test("dispatches newly created worker immediately without waiting poll interval", async () => {
    const fixture = await createFixture();
    fixture.config.agent.vendor = "opencode";
    fixture.config.scheduler.pollIntervalSeconds = 3600;
    const now = new Date(Date.now() - 1_000).toISOString();
    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
      createdAt: now,
      updatedAt: now,
    });
    seedStore.close();

    let workerStarted = false;
    const workerRunner = {
      discoverPullRequests: async () => ({
        queueItems: [],
        createdLoopIds: [],
        skipped: 0,
      }),
      processClaimedItem: async (queueItem: { id: string }) => {
        workerStarted = true;
        return {
          loopId: queueItem.id,
          runId: `run:${queueItem.id}`,
          queueItemId: queueItem.id,
          status: "success" as const,
          summary: "done",
        };
      },
    };

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
      github: new FakeGitHubGateway(),
      git: new FakeGitGateway(),
      agentExecutor: new FakeAgentExecutor([]),
      workerRunner: workerRunner as never,
      enableReviewer: false,
      enableFixer: false,
      enablePlanner: false,
    });

    await runtime.start();

    const response = await fetch(
      `http://${fixture.config.server.host}:${fixture.config.server.port}/api/v1/workers`,
      {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          projectId: "project_1",
          title: "Immediate worker",
          prompt: "Run now",
          repo: "powerformer/looper",
          baseBranch: "main",
        }),
      },
    );

    expect(response.status).toBe(200);
    await Bun.sleep(50);
    expect(workerStarted).toBe(true);

    await runtime.stop("test");
  });

  test("stop times out while waiting for hung in-flight scheduler work", async () => {
    const fixture = await createFixture();
    fixture.config.agent.vendor = "opencode";
    fixture.config.scheduler.maxConcurrentRuns = 1;
    const now = "2026-04-11T12:00:00.000Z";
    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
      createdAt: now,
      updatedAt: now,
    });
    seedStore.loops.upsert({
      id: "loop_worker_1",
      seq: 1,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: "powerformer/looper",
      prNumber: null,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });
    const scheduler = new SchedulerQueue({
      store: seedStore,
      retryMaxAttempts: 3,
      retryBaseDelayMs: 0,
    });
    scheduler.enqueue({
      id: "queue_worker_1",
      projectId: "project_1",
      loopId: "loop_worker_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: "powerformer/looper",
      dedupeKey: "worker:loop_worker_1",
      payloadJson: JSON.stringify({
        title: "Hung worker",
        prompt: "Wait forever",
        repo: "powerformer/looper",
        baseBranch: "main",
      }),
      availableAt: now,
    });
    seedStore.close();

    let started = false;
    let completed = false;
    const never = new Promise<void>(() => {});
    const workerRunner = {
      discoverPullRequests: async () => ({
        queueItems: [],
        createdLoopIds: [],
        skipped: 0,
      }),
      processClaimedItem: async () => {
        started = true;
        await never;
        completed = true;
        return {
          loopId: "loop_worker_1",
          runId: "run_worker_1",
          queueItemId: "queue_worker_1",
          status: "success" as const,
          summary: "done",
        };
      },
    };

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
      github: new FakeGitHubGateway(),
      git: new FakeGitGateway(),
      agentExecutor: new FakeAgentExecutor([]),
      workerRunner: workerRunner as never,
      enableReviewer: false,
      enableFixer: false,
      enablePlanner: false,
    });

    await runtime.start();
    await Bun.sleep(50);

    expect(started).toBe(true);

    const stopStartedAt = Date.now();
    await runtime.stop("test");
    const stopElapsedMs = Date.now() - stopStartedAt;

    expect(completed).toBe(false);
    expect(stopElapsedMs).toBeGreaterThanOrEqual(900);
    expect(stopElapsedMs).toBeLessThan(2_000);
  });

  test("sends failure notification for failed worker run", async () => {
    const fixture = await createFixture();
    fixture.config.agent.vendor = "opencode";
    const now = new Date(Date.now() - 1_000).toISOString();
    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: fixture.rootDir,
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "powerformer/looper" }),
      createdAt: now,
      updatedAt: now,
    });
    seedStore.loops.upsert({
      id: "loop_worker_1",
      seq: 1,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: "powerformer/looper",
      prNumber: null,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });
    const scheduler = new SchedulerQueue({
      store: seedStore,
      retryMaxAttempts: 3,
      retryBaseDelayMs: 0,
    });
    scheduler.enqueue({
      id: "queue_worker_1",
      projectId: "project_1",
      loopId: "loop_worker_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: "powerformer/looper",
      dedupeKey: "worker:loop_worker_1",
      payloadJson: JSON.stringify({
        title: "Implement worker loop",
        specPath: "spec.md",
        repo: "powerformer/looper",
        baseBranch: "main",
      }),
      availableAt: now,
    });
    await writeFile(join(fixture.rootDir, "spec.md"), "# Worker spec\n");
    seedStore.close();

    const github = new FakeGitHubGateway();
    const git = new FakeGitGateway();
    const agentExecutor = new FakeAgentExecutor([
      failedAgentResult("worker agent crashed"),
    ]);

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
      github,
      git,
      agentExecutor,
      enableReviewer: false,
      enableFixer: false,
    });

    await runtime.start();
    await Bun.sleep(50);

    const verifyStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
    });
    verifyStore.initialize();
    const failureNotifications = verifyStore.notifications
      .list()
      .filter(
        (record) =>
          record.channel === "in_app" &&
          record.level === "failure" &&
          record.entityType === "run",
      );

    expect(failureNotifications.length).toBeGreaterThan(0);
    expect(
      failureNotifications.some((record) =>
        record.body.includes("worker agent crashed"),
      ),
    ).toBe(true);

    verifyStore.close();
    await runtime.stop("test");
  });

  test("clears cached repo metadata when configured repoPath changes", async () => {
    const fixture = await createFixture();
    const oldRepoPath = join(fixture.rootDir, "old-repo");
    const newRepoPath = join(fixture.rootDir, "new-repo");
    await Promise.all([
      mkdir(oldRepoPath, { recursive: true }),
      mkdir(newRepoPath, { recursive: true }),
    ]);

    fixture.config.projects = [
      {
        id: "project_1",
        name: "Looper",
        repoPath: newRepoPath,
        baseBranch: "main",
      },
    ];

    const seedStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
      backupDir: fixture.config.storage.backupDir,
    });
    seedStore.initialize({ autoMigrate: true });
    seedStore.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: oldRepoPath,
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({
        repo: "stale/looper",
        worktreeRoot: "/tmp/worktrees",
      }),
      createdAt: "2026-04-11T12:00:00.000Z",
      updatedAt: "2026-04-11T12:00:00.000Z",
    });
    seedStore.close();

    const git = new FakeGitGateway();
    const detectedPaths: string[] = [];
    git.detectGitHubRepo = async (repoPath: string) => {
      detectedPaths.push(repoPath);
      return "fresh/looper";
    };

    const runtime = createLooperdRuntime({
      config: fixture.config,
      logger: fixture.logger,
      github: new FakeGitHubGateway(),
      git,
      agentExecutor: new FakeAgentExecutor([]),
      enableReviewer: false,
      enableFixer: false,
      enablePlanner: false,
    });

    await runtime.start();

    const verifyStore = new SqliteStore({
      dbPath: fixture.config.storage.dbPath,
    });
    verifyStore.initialize();

    const project = verifyStore.projects.getById("project_1");
    expect(detectedPaths).toContain(newRepoPath);
    expect(project?.repoPath).toBe(newRepoPath);
    expect(project?.metadataJson).toBe(
      JSON.stringify({
        repo: "fresh/looper",
        worktreeRoot: null,
        source: "config",
      }),
    );

    verifyStore.close();
    await runtime.stop("test");
  });
});
