import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { Logger } from "../bootstrap/logger";
import { getDefaultProjectWorktreeRoot } from "../config/index";
import type { AgentResult, AgentRunInput } from "../infra/agent";
import { RemoteHeadChangedError } from "../infra/git";
import { SchedulerQueue } from "../scheduler/index";
import { SqliteStore } from "../storage/sqlite/sqlite-store";
import {
  type FixerAgentExecution,
  type FixerAgentExecutor,
  type FixerGitGateway,
  type FixerGitHubGateway,
  FixerLoopRunner,
  type FixerValidationResult,
} from "./index";

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
  const rootDir = await mkdtemp(join(tmpdir(), "looper-fixer-"));
  cleanupPaths.push(rootDir);
  const repoPath = join(rootDir, "repo");
  await mkdir(repoPath, { recursive: true });

  const store = new SqliteStore({
    dbPath: join(rootDir, "state", "looper.sqlite"),
  });
  store.initialize({ autoMigrate: true });

  const now = new Date("2026-04-11T12:00:00.000Z");
  const nowIso = now.toISOString();
  store.projects.upsert({
    id: "project_1",
    name: "Looper",
    repoPath,
    baseBranch: "main",
    archived: false,
    metadataJson: null,
    createdAt: nowIso,
    updatedAt: nowIso,
  });

  const queue = new SchedulerQueue({
    store,
    retryMaxAttempts: 3,
    retryBaseDelayMs: 5_000,
    now: () => now,
  });

  return { rootDir, repoPath, store, queue, now };
}

class FakeGitHubGateway implements FixerGitHubGateway {
  public viewCalls = 0;
  public resolvedThreadIds: string[] = [];
  public addedLabels: Array<{
    repo: string;
    prNumber: number;
    labels: string[];
  }> = [];
  public removedLabels: Array<{
    repo: string;
    prNumber: number;
    labels: string[];
  }> = [];
  public addLabelFailuresRemaining = 0;
  private readonly currentLabels: string[];

  constructor(
    private readonly options: {
      listPrs?: Array<{ number: number; isDraft?: boolean; state?: string }>;
      views: Array<
        | "error"
        | {
            comments?: unknown[];
            checks?: unknown[];
            headSha?: string;
            reviewDecision?: string;
          }
      >;
      resolveFailures?: Record<string, string>;
      labels?: string[];
    },
  ) {
    this.currentLabels = [...(this.options.labels ?? [])];
  }

  public async listOpenPullRequests(_input: {
    repo: string;
    cwd?: string;
    limit?: number;
  }) {
    return (
      this.options.listPrs ?? [{ number: 42, isDraft: false, state: "OPEN" }]
    ).map((pr) => ({
      number: pr.number,
      title: `PR ${pr.number}`,
      state: pr.state ?? "OPEN",
      isDraft: pr.isDraft ?? false,
      labels: [...this.currentLabels],
      reviewRequests: [],
    }));
  }

  public async viewPullRequest(_input: {
    repo: string;
    prNumber: number;
    cwd?: string;
  }) {
    this.viewCalls += 1;
    const next = this.options.views.shift();
    if (!next) {
      throw new Error("No view response queued");
    }
    if (next === "error") {
      throw new Error("temporary GitHub failure");
    }

    return {
      number: 42,
      title: "Fix me",
      body: "body",
      state: "OPEN",
      isDraft: false,
      reviewDecision: next.reviewDecision,
      labels: [...this.currentLabels],
      headRefName: "feature/fixer",
      baseRefName: "main",
      headSha: next.headSha ?? "abc123",
      baseSha: "base123",
      author: "octocat",
      reviewRequests: [],
      comments: next.comments ?? [],
      reviews: [],
      checks: next.checks ?? [],
    };
  }

  public async resolveReviewThread(input: {
    repo: string;
    threadId: string;
    cwd?: string;
  }) {
    const failure = this.options.resolveFailures?.[input.threadId];
    if (failure) {
      throw new Error(failure);
    }
    this.resolvedThreadIds.push(input.threadId);
  }

  public async addPullRequestLabels(input: {
    repo: string;
    prNumber: number;
    labels: string[];
    cwd?: string;
  }) {
    if (this.addLabelFailuresRemaining > 0) {
      this.addLabelFailuresRemaining -= 1;
      throw new Error("temporary add labels failure");
    }
    this.addedLabels.push({
      repo: input.repo,
      prNumber: input.prNumber,
      labels: input.labels,
    });
    for (const label of input.labels) {
      if (!this.currentLabels.includes(label)) {
        this.currentLabels.push(label);
      }
    }
  }

  public async removePullRequestLabels(input: {
    repo: string;
    prNumber: number;
    labels: string[];
    cwd?: string;
  }) {
    this.removedLabels.push({
      repo: input.repo,
      prNumber: input.prNumber,
      labels: input.labels,
    });
    for (const label of input.labels) {
      const index = this.currentLabels.indexOf(label);
      if (index >= 0) {
        this.currentLabels.splice(index, 1);
      }
    }
  }
}

class FakeGitGateway implements FixerGitGateway {
  public pushCalls = 0;
  public createWorktreeCalls = 0;
  public prepareCalls = 0;
  public commitCalls = 0;
  public cleanupCalls = 0;
  public lastExpectedRemoteHeadSha?: string;
  public pushError?: string;
  public lastCreateWorktreeInput?: {
    projectId: string;
    repoPath: string;
    worktreeRoot: string;
    branch: string;
    baseBranch: string;
    prNumber: number;
    protectedBranches?: string[];
    checkoutMode?: "branch" | "detached";
  };

  constructor(
    private readonly options: {
      worktreePath?: string;
      prepareResult?: { headSha?: string; clean: boolean };
      inspectResults?: Array<{
        headSha?: string;
        newCommitShas: string[];
        hasUncommittedChanges: boolean;
        changedFiles: string[];
      }>;
      commitSha?: string;
      pushError?: string;
    } = {},
  ) {
    this.pushError = options.pushError;
  }

  public async createWorktree(_input: {
    projectId: string;
    repoPath: string;
    worktreeRoot: string;
    branch: string;
    baseBranch: string;
    prNumber: number;
    protectedBranches?: string[];
    checkoutMode?: "branch" | "detached";
  }) {
    this.createWorktreeCalls += 1;
    this.lastCreateWorktreeInput = _input;
    return {
      worktreePath: this.options.worktreePath ?? "/tmp/looper-fixer-worktree",
      branch: _input.branch,
      headSha: this.options.prepareResult?.headSha ?? "abc123",
    };
  }

  public async prepareWorktree(_input: {
    worktreePath: string;
    branch: string;
    expectedHeadSha?: string;
    remote?: string;
  }) {
    this.prepareCalls += 1;
    return (
      this.options.prepareResult ?? {
        headSha: _input.expectedHeadSha ?? "abc123",
        clean: true,
      }
    );
  }

  public async inspectHead(_input: { worktreePath: string; baseRef?: string }) {
    const next = this.options.inspectResults?.shift();
    return (
      next ?? {
        headSha: "commit-1",
        newCommitShas: ["commit-1"],
        hasUncommittedChanges: false,
        changedFiles: [],
      }
    );
  }

  public async commit(_input: { worktreePath: string; message: string }) {
    this.commitCalls += 1;
    return { commitSha: this.options.commitSha ?? "looperd-commit-1" };
  }

  public async push(_input: {
    worktreePath: string;
    branch: string;
    remote?: string;
    expectedRemoteHeadSha?: string;
    protectedBranches?: string[];
  }): Promise<void> {
    if (this.pushError) {
      throw new Error(this.pushError);
    }
    this.pushCalls += 1;
    this.lastExpectedRemoteHeadSha = _input.expectedRemoteHeadSha;
  }

  public async cleanupWorktree(_input: {
    projectId: string;
    repoPath: string;
    worktreePath: string;
    branch: string;
    protectedBranches?: string[];
  }): Promise<void> {
    this.cleanupCalls += 1;
  }
}

class FakeAgentExecutor implements FixerAgentExecutor {
  public starts: AgentRunInput[] = [];

  constructor(private readonly results: AgentResult[]) {}

  public async start(input: AgentRunInput): Promise<FixerAgentExecution> {
    this.starts.push(input);
    const result = this.results.shift();
    if (!result) {
      throw new Error("No agent result queued");
    }
    return { wait: async () => result };
  }
}

function completedAgentResult(summary: string): AgentResult {
  return {
    status: "completed",
    summary,
    artifacts: [],
    changedFiles: ["apps/looperd/src/fixer/index.ts"],
    commits: ["abc123"],
    rawLogs: { stdout: `${summary}\n`, stderr: "" },
    parseStatus: "parsed",
    completionSignal: "done",
    heartbeatCount: 1,
    resourceUsage: { wallTimeMs: 10, outputBytes: summary.length },
    pid: 111,
  };
}

function createCapturingLogger() {
  const entries: Array<{
    level: "info" | "error";
    message: string;
    context?: Record<string, unknown>;
  }> = [];
  const logger: Logger = {
    debug: () => {},
    warn: () => {},
    info: (message, context) =>
      entries.push({ level: "info", message, context }),
    error: (message, context) =>
      entries.push({ level: "error", message, context }),
  };

  return { logger, entries };
}

describe("FixerLoopRunner", () => {
  test("processNext does not claim queue items for other loop types", async () => {
    const fixture = await createFixture();
    const nowIso = fixture.now.toISOString();
    fixture.store.loops.upsert({
      id: "loop_worker_1",
      seq: 1,
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project:project_1",
      repo: "acme/looper",
      prNumber: null,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: nowIso,
      createdAt: nowIso,
      updatedAt: nowIso,
    });
    fixture.queue.enqueue({
      projectId: "project_1",
      loopId: "loop_worker_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: "acme/looper",
      dedupeKey: "worker:loop_worker_1",
    });

    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github: new FakeGitHubGateway({
        views: [
          {
            comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
            checks: [],
          },
        ],
      }),
      git: new FakeGitGateway(),
      agentExecutor: new FakeAgentExecutor([]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      sleep: async () => {},
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
    });

    const result = await runner.processNext("fixer-1");

    expect(result).toBeNull();
    expect(
      fixture.store.queue.findActiveByDedupe("worker:loop_worker_1")?.status,
    ).toBe("queued");

    fixture.store.close();
  });

  test("discovers and completes a full successful fixer flow", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [
            {
              id: "c1",
              threadId: "thread-1",
              state: "UNRESOLVED",
              body: "needs fix",
            },
          ],
        },
        {
          comments: [
            {
              id: "c1",
              threadId: "thread-1",
              state: "UNRESOLVED",
              body: "needs fix",
            },
          ],
        },
        {
          comments: [
            {
              id: "c1",
              threadId: "thread-1",
              state: "UNRESOLVED",
              body: "needs fix",
            },
          ],
          headSha: "commit-1",
        },
        { comments: [], checks: [], headSha: "commit-1" },
        { comments: [], checks: [], headSha: "commit-1" },
        { comments: [], checks: [], headSha: "commit-1" },
      ],
    });
    const git = new FakeGitGateway();
    const agent = new FakeAgentExecutor([completedAgentResult("fixed")]);
    const logs = createCapturingLogger();
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: agent,
      logger: logs.logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
    });

    const discovery = await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    expect(discovery.queueItems).toHaveLength(1);

    const claimed = fixture.queue.claimNext("fixer-1");
    if (!claimed) {
      throw new Error("Expected claimed fixer queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    if (result.status !== "success") {
      throw new Error(
        `${result.failureKind ?? result.status}: ${result.summary}`,
      );
    }
    expect(agent.starts).toHaveLength(1);
    expect(git.createWorktreeCalls).toBe(1);
    expect(git.lastCreateWorktreeInput?.worktreeRoot).toBe(
      getDefaultProjectWorktreeRoot("project_1", fixture.repoPath),
    );
    expect(git.lastCreateWorktreeInput?.checkoutMode).toBe("detached");
    expect(git.prepareCalls).toBe(1);
    expect(git.pushCalls).toBe(1);
    expect(git.lastExpectedRemoteHeadSha).toBe("abc123");
    expect(github.resolvedThreadIds).toEqual(["thread-1"]);
    expect(git.cleanupCalls).toBe(1);
    const run = fixture.store.runs.listByLoop(result.loopId)[0];
    const checkpoint = JSON.parse(run?.checkpointJson ?? "{}");
    expect(checkpoint.recheck.remainingFixItems).toHaveLength(0);
    expect(checkpoint.worktree.path).toBeTruthy();
    expect(checkpoint.reconcileCommits.finalHeadSha).toBe("commit-1");
    expect(checkpoint.resolvedComments.items).toHaveLength(1);
    expect(
      logs.entries.some((entry) => entry.message === "fixer loop started"),
    ).toBe(true);
    expect(
      logs.entries.some((entry) => entry.message === "fixer run started"),
    ).toBe(true);
    expect(
      logs.entries.some((entry) => entry.message === "fixer step started"),
    ).toBe(true);
    expect(
      logs.entries.some((entry) => entry.message === "fixer step completed"),
    ).toBe(true);
    expect(
      logs.entries.some((entry) => entry.message === "fixer run completed"),
    ).toBe(true);

    fixture.store.close();
  });

  test("emits agent-start callback when fixer executor starts", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [
            {
              id: "comment-1",
              body: "Please fix this",
              path: "src/index.ts",
              line: 10,
              threadId: "thread-1",
            },
          ],
          checks: [],
          headSha: "abc123",
        },
        {
          comments: [
            {
              id: "comment-1",
              body: "Please fix this",
              path: "src/index.ts",
              line: 10,
              threadId: "thread-1",
            },
          ],
          checks: [],
          headSha: "abc123",
        },
        { comments: [], checks: [], headSha: "commit-1" },
        { comments: [], checks: [], headSha: "commit-1" },
        { comments: [], checks: [], headSha: "commit-1" },
        { comments: [], checks: [], headSha: "commit-1" },
      ],
    });
    const git = new FakeGitGateway();
    const agent = new FakeAgentExecutor([completedAgentResult("fixed")]);
    const notifications: Array<{
      executionId: string;
      projectId: string;
      loopId: string;
      runId: string;
      subtitle: string;
      body: string;
      dedupeKey: string;
    }> = [];
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
      onAgentExecutionStarted: (input) => {
        notifications.push(input);
      },
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const claimed = fixture.queue.claimNext("fixer-1");
    if (!claimed) {
      throw new Error("Expected claimed fixer queue item");
    }

    await runner.processClaimedItem(claimed);

    expect(notifications).toHaveLength(1);
    expect(notifications[0]?.subtitle).toBe("acme/looper#42");
    expect(notifications[0]?.body).toBe("Fix started");
    expect(notifications[0]?.dedupeKey).toMatch(
      /^runtime\.agent\.started:fixer:/,
    );

    fixture.store.close();
  });

  test("retries from recheck without rerunning repair after recheck failure", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [
            {
              id: "c1",
              threadId: "thread-1",
              state: "UNRESOLVED",
              body: "needs fix",
            },
          ],
        },
        {
          comments: [
            {
              id: "c1",
              threadId: "thread-1",
              state: "UNRESOLVED",
              body: "needs fix",
            },
          ],
        },
        {
          comments: [
            {
              id: "c1",
              threadId: "thread-1",
              state: "UNRESOLVED",
              body: "needs fix",
            },
          ],
          headSha: "commit-1",
        },
        { comments: [], checks: [], headSha: "commit-1" },
        "error",
        { comments: [], checks: [], headSha: "commit-1" },
        { comments: [], checks: [], headSha: "commit-1" },
      ],
    });
    const git = new FakeGitGateway();
    const agent = new FakeAgentExecutor([completedAgentResult("fixed")]);
    const logs = createCapturingLogger();
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: agent,
      logger: logs.logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const firstClaim = fixture.queue.claimNext("fixer-1");
    if (!firstClaim) {
      throw new Error("Expected first fixer claim");
    }
    const firstResult = await runner.processClaimedItem(firstClaim);
    expect(firstResult.status).toBe("failed");
    expect(firstResult.failureKind).toBe("retryable_after_resume");
    expect(agent.starts).toHaveLength(1);
    expect(git.pushCalls).toBe(1);
    const failedLog = logs.entries.find(
      (entry) =>
        entry.level === "error" && entry.message === "fixer run failed",
    );
    expect(failedLog?.context).toMatchObject({
      projectId: "project_1",
      queueItemId: firstClaim.id,
      failureKind: "retryable_after_resume",
      currentStep: "recheck",
    });

    fixture.now.setTime(new Date("2026-04-11T12:00:05.000Z").getTime());
    const retryClaim = fixture.queue.claimNext("fixer-1");
    if (!retryClaim) {
      throw new Error("Expected retry fixer claim");
    }
    const retryResult = await runner.processClaimedItem(retryClaim);
    expect(retryResult.status).toBe("success");
    expect(agent.starts).toHaveLength(1);
    expect(git.pushCalls).toBe(1);
    expect(git.prepareCalls).toBe(1);

    fixture.store.close();
  });

  test("retries recheck spec label promotion when ready-label add fails after reviewing-label removal", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      labels: ["looper:spec-reviewing"],
      views: [
        {
          comments: [
            {
              id: "c1",
              threadId: "thread-1",
              state: "UNRESOLVED",
              body: "needs fix",
            },
          ],
        },
        {
          comments: [
            {
              id: "c1",
              threadId: "thread-1",
              state: "UNRESOLVED",
              body: "needs fix",
            },
          ],
        },
        {
          comments: [
            {
              id: "c1",
              threadId: "thread-1",
              state: "UNRESOLVED",
              body: "needs fix",
            },
          ],
          headSha: "commit-1",
        },
        { comments: [], checks: [], headSha: "commit-1" },
        { comments: [], checks: [], headSha: "commit-1" },
        { comments: [], checks: [], headSha: "commit-1" },
      ],
    });
    github.addLabelFailuresRemaining = 1;
    const git = new FakeGitGateway();
    const agent = new FakeAgentExecutor([completedAgentResult("fixed")]);
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const firstClaim = fixture.queue.claimNext("fixer-1");
    if (!firstClaim) {
      throw new Error("Expected first fixer claim");
    }
    const firstResult = await runner.processClaimedItem(firstClaim);
    expect(firstResult.status).toBe("failed");
    expect(firstResult.failureKind).toBe("retryable_after_resume");
    expect(github.removedLabels).toHaveLength(1);
    expect(github.addedLabels).toHaveLength(0);

    fixture.now.setTime(new Date("2026-04-11T12:00:05.000Z").getTime());
    const retryClaim = fixture.queue.claimNext("fixer-1");
    if (!retryClaim) {
      throw new Error("Expected retry fixer claim");
    }
    const retryResult = await runner.processClaimedItem(retryClaim);
    expect(retryResult.status).toBe("success");
    expect(github.removedLabels).toHaveLength(1);
    expect(github.addedLabels).toEqual([
      {
        repo: "acme/looper",
        prNumber: 42,
        labels: ["looper:spec-ready"],
      },
    ]);
    expect(agent.starts).toHaveLength(1);

    fixture.store.close();
  });

  test("skips processing when no fix items remain", async () => {
    const fixture = await createFixture();
    const nowIso = fixture.now.toISOString();

    fixture.store.loops.upsert({
      id: "loop_fixer_1",
      seq: 1,
      projectId: "project_1",
      type: "fixer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: nowIso,
      createdAt: nowIso,
      updatedAt: nowIso,
    });
    const queueItem = fixture.queue.enqueue({
      projectId: "project_1",
      loopId: "loop_fixer_1",
      type: "fixer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      dedupeKey: "fixer:acme/looper:42:abc123:none",
    });

    const github = new FakeGitHubGateway({
      views: [{ comments: [], checks: [] }],
    });
    const git = new FakeGitGateway();
    const agent = new FakeAgentExecutor([completedAgentResult("unused")]);
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
    });

    const claimed = fixture.queue.claimNext("fixer-1");
    const result = await runner.processClaimedItem(claimed ?? queueItem);
    expect(result.status).toBe("skipped");
    expect(agent.starts).toHaveLength(0);
    expect(git.pushCalls).toBe(0);

    fixture.store.close();
  });

  test("writes audit events for fixer lifecycle and push", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "commit-1",
        },
        { comments: [], checks: [], headSha: "commit-1" },
      ],
    });
    const git = new FakeGitGateway();
    const agent = new FakeAgentExecutor([completedAgentResult("fixed")]);
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      sleep: async () => {},
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const claimed = fixture.queue.claimNext("fixer-1");
    if (!claimed) {
      throw new Error("Expected claimed fixer queue item");
    }

    await runner.processClaimedItem(claimed);

    const eventTypes = fixture.store.events
      .list()
      .map((event) => event.eventType);
    expect(eventTypes).toContain("loop.started");
    expect(eventTypes).toContain("run.started");
    expect(eventTypes).toContain("loop.step.started");
    expect(eventTypes).toContain("loop.step.completed");
    expect(eventTypes).toContain("fixer.worktree.prepared");
    expect(eventTypes).toContain("fixer.commits.reconciled");
    expect(eventTypes).toContain("fixer.comments.resolved");
    expect(eventTypes).toContain("pr.branch.pushed");

    fixture.store.close();
  });

  test("pauses for manual push when auto push is disabled", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
        },
      ],
    });
    const git = new FakeGitGateway();
    const agent = new FakeAgentExecutor([completedAgentResult("fixed")]);
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      sleep: async () => {},
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
      allowAutoPush: false,
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const claimed = fixture.queue.claimNext("fixer-1");
    if (!claimed) {
      throw new Error("Expected claimed fixer queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("skipped");
    expect(result.summary).toContain("Auto push disabled");
    expect(git.pushCalls).toBe(0);
    expect(
      fixture.store.events
        .list()
        .some((event) => event.eventType === "fixer.push.skipped"),
    ).toBe(true);

    fixture.store.close();
  });

  test("fails after repair when auto commit is disabled and worktree is dirty", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
        },
      ],
    });
    const git = new FakeGitGateway({
      inspectResults: [
        {
          headSha: "abc123",
          newCommitShas: [],
          hasUncommittedChanges: true,
          changedFiles: ["apps/looperd/src/fixer/index.ts"],
        },
      ],
    });
    const agent = new FakeAgentExecutor([completedAgentResult("fixed")]);
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
      allowAutoCommit: false,
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const claimed = fixture.queue.claimNext("fixer-1");
    if (!claimed) {
      throw new Error("Expected claimed fixer queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("failed");
    expect(result.failureKind).toBe("manual_intervention");
    expect(result.summary).toContain("Auto commit disabled");
    expect(agent.starts).toHaveLength(1);
    expect(git.pushCalls).toBe(0);

    fixture.store.close();
  });

  test("fails claimed fixer work instead of leaving it running when setup throws", async () => {
    const fixture = await createFixture();
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github: new FakeGitHubGateway({
        views: [
          {
            comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
            checks: [],
          },
        ],
      }),
      git: new FakeGitGateway(),
      agentExecutor: new FakeAgentExecutor([completedAgentResult("unused")]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const claimed = fixture.queue.claimNext("fixer-1");
    if (!claimed) {
      throw new Error("Expected claimed fixer queue item");
    }

    Object.assign(runner as object, {
      getLoop() {
        throw new Error("setup exploded");
      },
    });

    await expect(runner.processClaimedItem(claimed)).rejects.toThrow(
      "setup exploded",
    );
    expect(fixture.store.queue.getById(claimed.id)?.status).toBe("failed");

    fixture.store.close();
  });

  test("allows one extra reconcile pass when validation generates changes", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "looperd-commit-2",
        },
        { comments: [], checks: [], headSha: "looperd-commit-2" },
        { comments: [], checks: [], headSha: "looperd-commit-2" },
        { comments: [], checks: [], headSha: "looperd-commit-2" },
      ],
    });
    const git = new FakeGitGateway({
      inspectResults: [
        {
          headSha: "agent-commit",
          newCommitShas: ["agent-commit"],
          hasUncommittedChanges: false,
          changedFiles: [],
        },
        {
          headSha: "agent-commit",
          newCommitShas: ["agent-commit"],
          hasUncommittedChanges: true,
          changedFiles: ["generated.txt"],
        },
        {
          headSha: "agent-commit",
          newCommitShas: ["agent-commit"],
          hasUncommittedChanges: true,
          changedFiles: ["generated.txt"],
        },
        {
          headSha: "looperd-commit-2",
          newCommitShas: ["agent-commit", "looperd-commit-2"],
          hasUncommittedChanges: false,
          changedFiles: [],
        },
        {
          headSha: "looperd-commit-2",
          newCommitShas: ["agent-commit", "looperd-commit-2"],
          hasUncommittedChanges: false,
          changedFiles: [],
        },
      ],
      commitSha: "looperd-commit-2",
    });
    let validationCalls = 0;
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: new FakeAgentExecutor([completedAgentResult("fixed")]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<FixerValidationResult> => {
        validationCalls += 1;
        return { passed: true, summary: `ok-${validationCalls}` };
      },
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const claimed = fixture.queue.claimNext("fixer-1");
    if (!claimed) throw new Error("Expected claimed fixer queue item");

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("success");
    expect(validationCalls).toBe(2);

    fixture.store.close();
  });

  test("fails safely when remote head changed before push", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
        },
      ],
    });
    const git = new FakeGitGateway({
      pushError: "Remote head changed for feature/fixer",
    });
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: new FakeAgentExecutor([completedAgentResult("fixed")]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const claimed = fixture.queue.claimNext("fixer-1");
    if (!claimed) throw new Error("Expected claimed fixer queue item");

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("failed");
    expect(result.failureKind).toBe("retryable_after_resume");
    expect(result.summary).toContain("Remote head changed");
    expect(git.cleanupCalls).toBe(0);
    expect(
      fixture.store.events
        .list()
        .some((event) => event.eventType === "fixer.push.conflicted"),
    ).toBe(true);

    fixture.store.close();
  });

  test("treats prepare-worktree remote head changes as retryable", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "old-head",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "old-head",
        },
      ],
    });
    const git = new FakeGitGateway({
      prepareResult: { headSha: "new-head", clean: true },
    });
    git.prepareWorktree = async () => {
      throw new RemoteHeadChangedError("feature/fixer", "old-head", "new-head");
    };
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: new FakeAgentExecutor([]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const claimed = fixture.queue.claimNext("fixer-1");
    if (!claimed) throw new Error("Expected claimed fixer queue item");

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("failed");
    expect(result.failureKind).toBe("retryable_after_resume");
    expect(result.summary).toContain("Remote head changed");

    fixture.store.close();
  });

  test("retries prepare-worktree head changes from discover-pr with fresh PR detail", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "old-head",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "old-head",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "commit-1",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "new-head",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "commit-1",
        },
        { comments: [], checks: [], headSha: "commit-1" },
      ],
    });
    const git = new FakeGitGateway();
    let prepareCalls = 0;
    git.prepareWorktree = async (input) => {
      prepareCalls += 1;
      if (prepareCalls === 1) {
        throw new RemoteHeadChangedError(input.branch, "old-head", "new-head");
      }
      return { headSha: input.expectedHeadSha, clean: true };
    };
    const agent = new FakeAgentExecutor([completedAgentResult("fixed")]);
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const firstClaim = fixture.queue.claimNext("fixer-1");
    if (!firstClaim) throw new Error("Expected first fixer claim");
    const first = await runner.processClaimedItem(firstClaim);
    expect(first.status).toBe("failed");
    expect(first.failureKind).toBe("retryable_after_resume");

    fixture.now.setTime(new Date("2026-04-11T12:00:05.000Z").getTime());
    const retryClaim = fixture.queue.claimNext("fixer-1");
    if (!retryClaim) throw new Error("Expected retry fixer claim");
    const retry = await runner.processClaimedItem(retryClaim);

    expect(retry.status).toBe("success");
    expect(agent.starts).toHaveLength(1);
    expect(prepareCalls).toBe(2);
    expect(github.viewCalls).toBeGreaterThanOrEqual(5);

    fixture.store.close();
  });

  test("retries push conflicts from prepare-worktree and reruns repair", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "abc123",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "abc123",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "commit-1",
        },
        {
          comments: [],
          checks: [],
          headSha: "commit-1",
        },
        {
          comments: [],
          checks: [],
          headSha: "commit-1",
        },
        {
          comments: [],
          checks: [],
          headSha: "commit-1",
        },
      ],
    });
    const git = new FakeGitGateway({
      pushError: "Remote head changed for feature/fixer",
    });
    const agent = new FakeAgentExecutor([
      completedAgentResult("fixed-on-old-head"),
      completedAgentResult("fixed-on-new-head"),
    ]);
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const firstClaim = fixture.queue.claimNext("fixer-1");
    if (!firstClaim) throw new Error("Expected first fixer claim");

    const first = await runner.processClaimedItem(firstClaim);
    expect(first.status).toBe("failed");
    expect(first.failureKind).toBe("retryable_after_resume");
    expect(agent.starts).toHaveLength(1);
    expect(git.prepareCalls).toBe(1);

    fixture.now.setTime(new Date("2026-04-11T12:00:05.000Z").getTime());
    git.pushError = undefined;
    const retryClaim = fixture.queue.claimNext("fixer-1");
    if (!retryClaim) throw new Error("Expected retry fixer claim");

    const retry = await runner.processClaimedItem(retryClaim);
    expect(retry.status).toBe("success");
    expect(agent.starts).toHaveLength(2);
    expect(git.prepareCalls).toBe(2);

    fixture.store.close();
  });

  test("restarts from discover-pr after push remote-head changes to refresh expected head", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "abc123",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "abc123",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "new-head",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "commit-1",
        },
        { comments: [], checks: [], headSha: "commit-1" },
        { comments: [], checks: [], headSha: "commit-1" },
      ],
    });
    const git = new FakeGitGateway({
      pushError:
        "Remote head changed for feature/fixer: expected abc123, got new-head",
    });
    git.prepareWorktree = async (input) => {
      git.prepareCalls += 1;
      if (git.prepareCalls === 2 && input.expectedHeadSha !== "new-head") {
        throw new RemoteHeadChangedError(
          input.branch,
          input.expectedHeadSha,
          "new-head",
        );
      }
      return { headSha: input.expectedHeadSha, clean: true };
    };
    const agent = new FakeAgentExecutor([
      completedAgentResult("fixed-on-old-head"),
      completedAgentResult("fixed-on-new-head"),
    ]);
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const firstClaim = fixture.queue.claimNext("fixer-1");
    if (!firstClaim) throw new Error("Expected first fixer claim");

    const first = await runner.processClaimedItem(firstClaim);
    expect(first.status).toBe("failed");
    expect(first.failureKind).toBe("retryable_after_resume");
    expect(agent.starts).toHaveLength(1);

    fixture.now.setTime(new Date("2026-04-11T12:00:05.000Z").getTime());
    git.pushError = undefined;
    const retryClaim = fixture.queue.claimNext("fixer-1");
    if (!retryClaim) throw new Error("Expected retry fixer claim");

    const retry = await runner.processClaimedItem(retryClaim);
    expect(retry.status).toBe("success");
    expect(agent.starts).toHaveLength(2);
    expect(git.prepareCalls).toBe(2);
    expect(github.viewCalls).toBeGreaterThanOrEqual(4);

    fixture.store.close();
  });

  test("retries resolve-comments head changes from discover-pr with fresh PR detail", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "abc123",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "abc123",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "new-head",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "new-head",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "new-head",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "new-head",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "new-head",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "abc123",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "abc123",
        },
        { comments: [], checks: [], headSha: "abc123" },
        { comments: [], checks: [], headSha: "abc123" },
        { comments: [], checks: [], headSha: "abc123" },
        { comments: [], checks: [], headSha: "abc123" },
        { comments: [], checks: [], headSha: "abc123" },
        { comments: [], checks: [], headSha: "abc123" },
        { comments: [], checks: [], headSha: "abc123" },
      ],
    });
    const git = new FakeGitGateway({
      inspectResults: [
        {
          headSha: "abc123",
          newCommitShas: [],
          hasUncommittedChanges: false,
          changedFiles: [],
        },
        {
          headSha: "abc123",
          newCommitShas: [],
          hasUncommittedChanges: false,
          changedFiles: [],
        },
        {
          headSha: "abc123",
          newCommitShas: [],
          hasUncommittedChanges: false,
          changedFiles: [],
        },
        {
          headSha: "abc123",
          newCommitShas: [],
          hasUncommittedChanges: false,
          changedFiles: [],
        },
        {
          headSha: "abc123",
          newCommitShas: [],
          hasUncommittedChanges: false,
          changedFiles: [],
        },
        {
          headSha: "abc123",
          newCommitShas: [],
          hasUncommittedChanges: false,
          changedFiles: [],
        },
        {
          headSha: "abc123",
          newCommitShas: [],
          hasUncommittedChanges: false,
          changedFiles: [],
        },
        {
          headSha: "abc123",
          newCommitShas: [],
          hasUncommittedChanges: false,
          changedFiles: [],
        },
      ],
    });
    const agent = new FakeAgentExecutor([
      completedAgentResult("fixed-attempt-1"),
      completedAgentResult("fixed-attempt-2"),
    ]);
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      sleep: async () => {},
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const firstClaim = fixture.queue.claimNext("fixer-1");
    if (!firstClaim) throw new Error("Expected first fixer claim");

    const first = await runner.processClaimedItem(firstClaim);
    expect(first.status).toBe("failed");
    expect(first.failureKind).toBe("retryable_after_resume");
    expect(first.summary).toContain(
      "PR head changed before resolving comments",
    );
    expect(github.resolvedThreadIds).toEqual([]);
    expect(agent.starts).toHaveLength(1);

    fixture.now.setTime(new Date("2026-04-11T12:00:05.000Z").getTime());
    const retryClaim = fixture.queue.claimNext("fixer-1");
    if (!retryClaim) throw new Error("Expected retry fixer claim");
    const retry = await runner.processClaimedItem(retryClaim);
    if (retry.status === "failed") {
      throw new Error(`Retry failed: ${retry.summary}`);
    }

    expect(retry.status).toBe("success");
    expect(agent.starts).toHaveLength(2);
    expect(github.resolvedThreadIds).toEqual(["thread-1"]);

    fixture.store.close();
  });

  test("waits for PR head to catch up after push before resolving comments", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "abc123",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "abc123",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "abc123",
        },
        {
          comments: [{ id: "c1", threadId: "thread-1", state: "UNRESOLVED" }],
          checks: [],
          headSha: "commit-1",
        },
        { comments: [], checks: [], headSha: "commit-1" },
        { comments: [], checks: [], headSha: "commit-1" },
      ],
    });
    const git = new FakeGitGateway();
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: new FakeAgentExecutor([completedAgentResult("fixed")]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      sleep: async () => {},
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const claimed = fixture.queue.claimNext("fixer-1");
    if (!claimed) throw new Error("Expected fixer claim");

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("success");
    expect(github.resolvedThreadIds).toEqual(["thread-1"]);

    fixture.store.close();
  });

  test("skips risky conflict fixes when policy is disabled", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      views: [
        { comments: [], checks: [], headSha: "abc123" },
        { comments: [], checks: [], headSha: "abc123" },
      ],
    });
    github.viewPullRequest = async (_input) => ({
      number: 42,
      title: "Fix me",
      body: "body",
      state: "OPEN",
      isDraft: false,
      reviewDecision: "CHANGES_REQUESTED",
      labels: [],
      headRefName: "feature/fixer",
      baseRefName: "main",
      headSha: "abc123",
      baseSha: "base123",
      author: "octocat",
      reviewRequests: [],
      comments: [],
      reviews: [],
      checks: [],
      hasConflicts: true,
    });
    const git = new FakeGitGateway();
    const agent = new FakeAgentExecutor([completedAgentResult("fixed")]);
    const runner = new FixerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      github,
      git,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<FixerValidationResult> => ({
        passed: true,
        summary: "ok",
      }),
      allowRiskyFixes: false,
    });

    await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    const claimed = fixture.queue.claimNext("fixer-1");
    if (!claimed) {
      throw new Error("Expected claimed fixer queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("skipped");
    expect(result.summary).toContain("risky conflict fixes");
    expect(agent.starts).toHaveLength(0);
    expect(git.pushCalls).toBe(0);

    fixture.store.close();
  });
});
