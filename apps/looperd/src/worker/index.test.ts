import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { Logger } from "../bootstrap/logger";
import { getDefaultProjectWorktreeRoot } from "../config/index";
import type { AgentResult, AgentRunInput } from "../infra/agent";
import { SchedulerQueue } from "../scheduler/index";
import { SqliteStore } from "../storage/sqlite/sqlite-store";
import type { WorktreeRecord } from "../storage/types";
import {
  type WorkerAgentExecution,
  type WorkerAgentExecutor,
  type WorkerGitGateway,
  type WorkerGitHubGateway,
  WorkerLoopRunner,
  type WorkerValidationResult,
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
  const rootDir = await mkdtemp(join(tmpdir(), "looper-worker-"));
  cleanupPaths.push(rootDir);
  const repoPath = join(rootDir, "repo");
  const worktreeRoot = join(rootDir, "worktrees");
  const now = new Date("2026-04-11T12:00:00.000Z");
  await mkdir(repoPath, { recursive: true });
  await mkdir(worktreeRoot, { recursive: true });
  await writeFile(
    join(repoPath, "spec.md"),
    "# Spec\n\nImplement worker loop MVP.\n",
  );

  const store = new SqliteStore({
    dbPath: join(rootDir, "state", "looper.sqlite"),
    now: () => now,
  });
  store.initialize({ autoMigrate: true });

  const nowIso = now.toISOString();

  store.projects.upsert({
    id: "project_1",
    name: "Looper",
    repoPath,
    baseBranch: "main",
    archived: false,
    metadataJson: JSON.stringify({ worktreeRoot }),
    createdAt: nowIso,
    updatedAt: nowIso,
  });
  store.loops.upsert({
    id: "loop_worker_1",
    seq: 1,
    projectId: "project_1",
    type: "worker",
    targetType: "project",
    targetId: "project_1",
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
  const queue = new SchedulerQueue({
    store,
    retryMaxAttempts: 3,
    retryBaseDelayMs: 0,
    now: () => now,
  });
  queue.enqueue({
    projectId: "project_1",
    loopId: "loop_worker_1",
    type: "worker",
    targetType: "project",
    targetId: "project_1",
    repo: "acme/looper",
    dedupeKey: "worker:loop_worker_1",
    payloadJson: JSON.stringify({
      title: "Implement worker loop",
      specPath: "spec.md",
      repo: "acme/looper",
      baseBranch: "main",
    }),
  });

  return { rootDir, repoPath, worktreeRoot, store, queue, now };
}

class FakeGitGateway implements WorkerGitGateway {
  public createWorktreeCalls = 0;
  public pushCalls = 0;
  public lastCreateWorktreeInput?: {
    projectId: string;
    repoPath: string;
    worktreeRoot: string;
    branch: string;
    baseBranch: string;
    prNumber?: number;
    protectedBranches?: string[];
  };

  constructor(private readonly worktreeRoot: string) {}

  public async createWorktree(input: {
    projectId: string;
    repoPath: string;
    worktreeRoot: string;
    branch: string;
    baseBranch: string;
    protectedBranches?: string[];
  }): Promise<WorktreeRecord> {
    this.createWorktreeCalls += 1;
    this.lastCreateWorktreeInput = input;
    const worktreePath = join(
      this.worktreeRoot,
      input.branch.replace(/[^a-zA-Z0-9._-]+/g, "-"),
    );
    await mkdir(worktreePath, { recursive: true });
    return {
      id: "worktree_1",
      projectId: input.projectId,
      repoPath: input.repoPath,
      worktreePath,
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

  public async push(): Promise<void> {
    this.pushCalls += 1;
  }

  public async prepareWorktree(): Promise<{
    headSha?: string;
    clean: boolean;
  }> {
    return { headSha: "abc123", clean: true };
  }
}

class FakeGitHubGateway implements WorkerGitHubGateway {
  public listOpenPullRequestCalls: Array<{ label?: string; limit?: number }> =
    [];
  public viewIssueCalls: Array<{
    repo: string;
    issueNumber: number;
    cwd?: string;
  }> = [];
  public createPullRequestCalls: Array<{
    repo: string;
    headBranch: string;
    baseBranch: string;
    title: string;
    body?: string;
    cwd?: string;
  }> = [];
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

  public async listOpenPullRequests(input: {
    repo: string;
    cwd?: string;
    limit?: number;
    label?: string;
  }): Promise<
    Awaited<ReturnType<WorkerGitHubGateway["listOpenPullRequests"]>>
  > {
    this.listOpenPullRequestCalls.push({
      label: input.label,
      limit: input.limit,
    });
    return [];
  }

  public async viewPullRequest(input: {
    repo: string;
    prNumber: number;
    cwd?: string;
  }) {
    return {
      number: input.prNumber,
      title: "Existing PR",
      body: "Spec: spec.md",
      url: `https://example.test/${input.repo}/pull/${input.prNumber}`,
      state: "OPEN",
      isDraft: false,
      reviewDecision: undefined,
      labels: ["looper:spec-ready"],
      headRefName: "feature/existing-pr",
      baseRefName: "main",
      headSha: "abc123",
      baseSha: "base123",
      author: "octocat",
      reviewRequests: ["octocat"],
      comments: [],
      reviews: [],
      checks: [{ conclusion: "SUCCESS" }],
    };
  }

  public async viewIssue(input: {
    repo: string;
    issueNumber: number;
    cwd?: string;
  }) {
    this.viewIssueCalls.push(input);
    return {
      number: input.issueNumber,
      title: "Add worker issue fallback",
      body: "Use the issue body as worker prompt when no planner exists.",
      url: `https://example.test/${input.repo}/issues/${input.issueNumber}`,
      state: "OPEN",
      author: "octocat",
      assignees: [],
      labels: [],
    };
  }

  public async createPullRequest(input: {
    repo: string;
    headBranch: string;
    baseBranch: string;
    title: string;
    body?: string;
    cwd?: string;
  }): Promise<{ number?: number; url: string }> {
    this.createPullRequestCalls.push(input);
    return {
      number: 101,
      url: "https://example.test/acme/looper/pull/101",
    };
  }

  public async removePullRequestLabels(input: {
    repo: string;
    prNumber: number;
    labels: string[];
    cwd?: string;
  }): Promise<void> {
    this.removedLabels.push({
      repo: input.repo,
      prNumber: input.prNumber,
      labels: input.labels,
    });
  }

  public async addPullRequestReviewers(input: {
    repo: string;
    prNumber: number;
    reviewers: string[];
    cwd?: string;
  }): Promise<void> {
    this.reviewerRequests.push({
      repo: input.repo,
      prNumber: input.prNumber,
      reviewers: input.reviewers,
    });
  }
}

class FakeAgentExecutor implements WorkerAgentExecutor {
  public starts: AgentRunInput[] = [];

  constructor(private readonly results: AgentResult[]) {}

  public async start(input: AgentRunInput): Promise<WorkerAgentExecution> {
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

function completedAgentResult(
  summary: string,
  commits: string[] = [],
): AgentResult {
  return {
    status: "completed",
    summary,
    artifacts: [],
    changedFiles: ["apps/looperd/src/worker/index.ts"],
    commits,
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

describe("WorkerLoopRunner", () => {
  function configurePullRequestWorkerLoop(
    fixture: Awaited<ReturnType<typeof createFixture>>,
    prNumber: number,
  ) {
    fixture.store.loops.upsert({
      ...(fixture.store.loops.getById("loop_worker_1") ?? {
        id: "loop_worker_1",
        seq: 1,
        projectId: "project_1",
        type: "worker",
        targetType: "project",
        targetId: "project_1",
        repo: "acme/looper",
        prNumber: null,
        status: "queued",
        configJson: null,
        metadataJson: null,
        lastRunAt: null,
        nextRunAt: fixture.now.toISOString(),
        createdAt: fixture.now.toISOString(),
        updatedAt: fixture.now.toISOString(),
      }),
      targetType: "pull_request",
      targetId: `pr:acme/looper:${prNumber}`,
      repo: "acme/looper",
      prNumber,
      metadataJson: JSON.stringify({
        executionMode: "push-existing",
        prUrl: `https://example.test/acme/looper/pull/${prNumber}`,
      }),
      updatedAt: fixture.now.toISOString(),
    });
    fixture.store.queue.upsert({
      ...(fixture.store.queue.findActiveByDedupe("worker:loop_worker_1") ?? {
        id: "queue_pr_mode",
        projectId: "project_1",
        loopId: "loop_worker_1",
        type: "worker",
        targetType: "pull_request",
        targetId: `pr:acme/looper:${prNumber}`,
        repo: "acme/looper",
        prNumber,
        dedupeKey: "worker:loop_worker_1",
        priority: 0,
        status: "queued",
        availableAt: fixture.now.toISOString(),
        attempts: 0,
        maxAttempts: 3,
        claimedBy: null,
        claimedAt: null,
        startedAt: null,
        finishedAt: null,
        lockKey: `pr:acme/looper:${prNumber}`,
        payloadJson: null,
        lastError: null,
        lastErrorKind: null,
        createdAt: fixture.now.toISOString(),
        updatedAt: fixture.now.toISOString(),
      }),
      type: "worker",
      targetType: "pull_request",
      targetId: `pr:acme/looper:${prNumber}`,
      repo: "acme/looper",
      prNumber,
      lockKey: `pr:acme/looper:${prNumber}`,
      payloadJson: null,
      updatedAt: fixture.now.toISOString(),
    });
  }

  test("processNext does not claim queue items for other loop types", async () => {
    const fixture = await createFixture();
    const nowIso = fixture.now.toISOString();
    fixture.store.loops.upsert({
      id: "loop_reviewer_1",
      seq: 2,
      projectId: "project_1",
      type: "reviewer",
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
    fixture.queue.enqueue({
      projectId: "project_1",
      loopId: "loop_reviewer_1",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      dedupeKey: "reviewer:acme/looper:42",
    });

    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git: new FakeGitGateway(fixture.worktreeRoot),
      github: new FakeGitHubGateway(),
      agentExecutor: new FakeAgentExecutor([
        completedAgentResult("Implemented slice", ["abc123"]),
      ]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const result = await runner.processNext("worker-1");

    expect(result).not.toBeNull();
    expect(
      fixture.store.queue.findActiveByDedupe("reviewer:acme/looper:42")?.status,
    ).toBe("queued");

    fixture.store.close();
  });

  test("opens a pull request after a successful worker run", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const logs = createCapturingLogger();
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: logs.logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("success");
    expect(result.pullRequestNumber).toBe(101);
    expect(agent.starts).toHaveLength(1);
    expect(agent.starts[0]?.prompt).not.toContain(
      "use the GitHub CLI (`gh`) to create the pull request yourself",
    );
    expect(git.createWorktreeCalls).toBe(1);
    expect(git.lastCreateWorktreeInput?.worktreeRoot).toBe(
      fixture.worktreeRoot,
    );
    expect(git.pushCalls).toBe(1);
    expect(github.createPullRequestCalls).toHaveLength(1);
    expect(fixture.store.loops.getById("loop_worker_1")?.status).toBe(
      "completed",
    );
    fixture.store.close();
  });

  test("uses the config default worktree root when project metadata omits one", async () => {
    const fixture = await createFixture();
    const project = fixture.store.projects.getById("project_1");
    if (!project) {
      throw new Error("Expected project fixture");
    }
    fixture.store.projects.upsert({
      ...project,
      metadataJson: null,
      updatedAt: fixture.now.toISOString(),
    });

    const git = new FakeGitGateway(fixture.worktreeRoot);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github: new FakeGitHubGateway(),
      agentExecutor: new FakeAgentExecutor([
        completedAgentResult("Implemented slice and committed changes", [
          "abc123",
        ]),
      ]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);

    expect(result.status).toBe("success");
    expect(git.lastCreateWorktreeInput?.worktreeRoot).toBe(
      getDefaultProjectWorktreeRoot("project_1", fixture.repoPath),
    );

    fixture.store.close();
  });

  test("prompts agent to create PR only when looperd validation is disabled", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("success");
    expect(agent.starts[0]?.prompt).toContain(
      "use the GitHub CLI (`gh`) to create the pull request yourself",
    );

    fixture.store.close();
  });

  test("records agent-created pull requests without creating a duplicate", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    github.listOpenPullRequests = async (input) => {
      github.listOpenPullRequestCalls.push({
        label: input.label,
        limit: input.limit,
      });
      return [
        {
          number: 202,
          title: "Agent-created PR",
          url: "https://example.test/acme/looper/pull/202",
          state: "OPEN",
          isDraft: false,
          reviewDecision: undefined,
          labels: [],
          headRefName: "looper/worker/loop-worker-1",
          baseRefName: "main",
          author: "octocat",
          reviewRequests: [],
        },
      ];
    };
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and opened PR", ["abc123"]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("success");
    expect(result.pullRequestNumber).toBe(202);
    expect(git.pushCalls).toBe(1);
    expect(github.createPullRequestCalls).toHaveLength(0);
    expect(github.listOpenPullRequestCalls[0]?.limit).toBe(1000);
    expect(fixture.store.loops.getById("loop_worker_1")?.prNumber).toBe(202);
    expect(
      fixture.store.loops.getById("loop_worker_1")?.metadataJson,
    ).toContain("https://example.test/acme/looper/pull/202");

    fixture.store.close();
  });

  test("does not reuse an existing PR when the base branch differs", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    github.listOpenPullRequests = async (input) => {
      github.listOpenPullRequestCalls.push({
        label: input.label,
        limit: input.limit,
      });
      return [
        {
          number: 303,
          title: "Wrong-base PR",
          url: "https://example.test/acme/looper/pull/303",
          state: "OPEN",
          isDraft: false,
          reviewDecision: undefined,
          labels: [],
          headRefName: "looper/worker/loop-worker-1",
          baseRefName: "develop",
          author: "octocat",
          reviewRequests: [],
        },
      ];
    };
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and opened PR", ["abc123"]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("success");
    expect(result.pullRequestNumber).toBe(101);
    expect(github.createPullRequestCalls).toHaveLength(1);
    expect(fixture.store.loops.getById("loop_worker_1")?.prNumber).toBe(101);

    fixture.store.close();
  });

  test("hydrates worker input from issue details and opens a PR without planner", async () => {
    const fixture = await createFixture();
    const nowIso = fixture.now.toISOString();
    fixture.store.loops.upsert({
      ...(fixture.store.loops.getById("loop_worker_1") ?? {
        id: "loop_worker_1",
        seq: 1,
        projectId: "project_1",
        type: "worker",
        targetType: "project",
        targetId: "project_1",
        repo: "acme/looper",
        prNumber: null,
        status: "queued",
        configJson: null,
        metadataJson: null,
        lastRunAt: null,
        nextRunAt: nowIso,
        createdAt: nowIso,
        updatedAt: nowIso,
      }),
      targetType: "project",
      targetId: "project_1",
      repo: "acme/looper",
      prNumber: null,
      metadataJson: JSON.stringify({
        worker: {
          title: "Implement acme/looper#123",
          repo: "acme/looper",
          baseBranch: "main",
          issueNumber: 123,
        },
      }),
      updatedAt: nowIso,
    });
    fixture.store.queue.upsert({
      ...(fixture.store.queue.findActiveByDedupe("worker:loop_worker_1") ?? {
        id: "queue_issue_mode",
        projectId: "project_1",
        loopId: "loop_worker_1",
        type: "worker",
        targetType: "project",
        targetId: "project_1",
        repo: "acme/looper",
        prNumber: null,
        dedupeKey: "worker:loop_worker_1",
        priority: 0,
        status: "queued",
        availableAt: nowIso,
        attempts: 0,
        maxAttempts: 3,
        claimedBy: null,
        claimedAt: null,
        startedAt: null,
        finishedAt: null,
        lockKey: "worker:loop_worker_1",
        payloadJson: null,
        lastError: null,
        lastErrorKind: null,
        createdAt: nowIso,
        updatedAt: nowIso,
      }),
      targetType: "project",
      targetId: "project_1",
      repo: "acme/looper",
      prNumber: null,
      lockKey: "worker:loop_worker_1",
      payloadJson: JSON.stringify({
        title: "Implement acme/looper#123",
        repo: "acme/looper",
        baseBranch: "main",
        issueNumber: 123,
      }),
      updatedAt: nowIso,
    });

    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented issue fallback", ["abc123"]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("success");
    expect(github.viewIssueCalls).toEqual([
      {
        repo: "acme/looper",
        issueNumber: 123,
        cwd: fixture.repoPath,
      },
    ]);
    expect(agent.starts).toHaveLength(1);
    expect(agent.starts[0]?.prompt).toContain(
      "Implement GitHub issue acme/looper#123: Add worker issue fallback",
    );
    expect(git.pushCalls).toBe(1);
    expect(github.createPullRequestCalls).toHaveLength(1);
    expect(github.createPullRequestCalls[0]?.headBranch).toBe(
      "looper/worker/123-add-worker-issue-fallback-loop-worker-1",
    );
    expect(github.createPullRequestCalls[0]?.title).toBe(
      "Add worker issue fallback",
    );
    expect(github.createPullRequestCalls[0]?.body).toContain("Closes #123");

    fixture.store.close();
  });

  test("truncates long issue-derived branch slugs", async () => {
    const fixture = await createFixture();
    const nowIso = fixture.now.toISOString();
    const longTitle = `Implement ${"supercalifragilisticexpialidocious".repeat(6)}`;
    fixture.store.loops.upsert({
      ...(fixture.store.loops.getById("loop_worker_1") ?? {
        id: "loop_worker_1",
        seq: 1,
        projectId: "project_1",
        type: "worker",
        targetType: "project",
        targetId: "project_1",
        repo: "acme/looper",
        prNumber: null,
        status: "queued",
        configJson: null,
        metadataJson: null,
        lastRunAt: null,
        nextRunAt: nowIso,
        createdAt: nowIso,
        updatedAt: nowIso,
      }),
      metadataJson: JSON.stringify({
        worker: {
          title: "Implement acme/looper#124",
          repo: "acme/looper",
          baseBranch: "main",
          issueNumber: 124,
        },
      }),
      updatedAt: nowIso,
    });
    fixture.store.queue.upsert({
      ...(fixture.store.queue.findActiveByDedupe("worker:loop_worker_1") ?? {
        id: "queue_issue_mode_long",
        projectId: "project_1",
        loopId: "loop_worker_1",
        type: "worker",
        targetType: "project",
        targetId: "project_1",
        repo: "acme/looper",
        prNumber: null,
        dedupeKey: "worker:loop_worker_1",
        priority: 0,
        status: "queued",
        availableAt: nowIso,
        attempts: 0,
        maxAttempts: 3,
        claimedBy: null,
        claimedAt: null,
        startedAt: null,
        finishedAt: null,
        lockKey: "worker:loop_worker_1",
        payloadJson: null,
        lastError: null,
        lastErrorKind: null,
        createdAt: nowIso,
        updatedAt: nowIso,
      }),
      payloadJson: JSON.stringify({
        title: "Implement acme/looper#124",
        repo: "acme/looper",
        baseBranch: "main",
        issueNumber: 124,
      }),
      updatedAt: nowIso,
    });

    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    github.viewIssue = async (input) => ({
      number: input.issueNumber,
      title: longTitle,
      body: "Long title branch test.",
      url: `https://example.test/${input.repo}/issues/${input.issueNumber}`,
      state: "OPEN",
      author: "octocat",
      assignees: [],
      labels: [],
    });
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented issue fallback", ["abc123"]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("success");
    const headBranch = github.createPullRequestCalls[0]?.headBranch;
    expect(headBranch).toBeDefined();
    expect(headBranch?.length ?? 0).toBeLessThanOrEqual(80);
    expect(headBranch).toMatch(/^looper\/worker\/124-/);
    expect(headBranch).toContain("-loop-worker-1");

    fixture.store.close();
  });

  test("emits agent-start callback when worker executor starts", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const notifications: Array<{
      executionId: string;
      projectId: string;
      loopId: string;
      runId: string;
      subtitle: string;
      body: string;
      dedupeKey: string;
    }> = [];
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
      onAgentExecutionStarted: (input) => {
        notifications.push(input);
      },
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    await runner.processClaimedItem(claimed);

    expect(notifications).toHaveLength(1);
    expect(notifications[0]?.subtitle).toBe("Implement worker loop");
    expect(notifications[0]?.body).toBe("Worker started");
    expect(notifications[0]?.dedupeKey).toMatch(
      /^runtime\.agent\.started:worker:/,
    );

    fixture.store.close();
  });

  test("keeps loop queued when validation fails and requeues", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice"),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: false,
        summary: "tests failed",
        output: "tests failed",
      }),
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("failed");
    expect(github.createPullRequestCalls).toHaveLength(0);
    expect(fixture.store.loops.getById("loop_worker_1")?.status).toBe("paused");

    fixture.store.close();
  });

  test("fails claimed work instead of leaving it running when setup throws", async () => {
    const fixture = await createFixture();
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git: new FakeGitGateway(fixture.worktreeRoot),
      github: new FakeGitHubGateway(),
      agentExecutor: new FakeAgentExecutor([completedAgentResult("unused")]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
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

  test("discovers spec-ready PRs and pushes to the existing PR branch", async () => {
    const fixture = await createFixture();
    configurePullRequestWorkerLoop(fixture, 77);

    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    github.listOpenPullRequests = async (): Promise<
      Awaited<ReturnType<WorkerGitHubGateway["listOpenPullRequests"]>>
    > => {
      return [
        {
          number: 77,
          title: "Existing PR",
          state: "OPEN",
          isDraft: false,
          reviewDecision: undefined,
          labels: ["looper:spec-ready"],
          headRefName: "feature/existing-pr",
          baseRefName: "main",
          author: "octocat",
          reviewRequests: ["octocat"],
        },
      ];
    };
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented existing PR", ["abc123"]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const discovery = await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });
    expect(discovery.queueItems).toHaveLength(1);

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("success");
    expect(git.pushCalls).toBe(1);
    expect(github.createPullRequestCalls).toHaveLength(0);
    expect(github.removedLabels).toEqual([
      {
        repo: "acme/looper",
        prNumber: 77,
        labels: ["looper:spec-ready"],
      },
    ]);
    expect(github.reviewerRequests).toEqual([
      {
        repo: "acme/looper",
        prNumber: 77,
        reviewers: ["octocat"],
      },
    ]);

    fixture.store.close();
  });

  test("discovery does not requeue paused pull-request worker loops", async () => {
    const fixture = await createFixture();
    const nowIso = fixture.now.toISOString();
    fixture.store.loops.upsert({
      id: "loop_worker_paused_pr",
      seq: 2,
      projectId: "project_1",
      type: "worker",
      targetType: "pull_request",
      targetId: "pr:acme/looper:77",
      repo: "acme/looper",
      prNumber: 77,
      status: "paused",
      configJson: null,
      metadataJson: JSON.stringify({
        executionMode: "push-existing",
        prUrl: "https://example.test/acme/looper/pull/77",
      }),
      lastRunAt: nowIso,
      nextRunAt: null,
      createdAt: nowIso,
      updatedAt: nowIso,
    });

    const github = new FakeGitHubGateway();
    github.listOpenPullRequests = async (): Promise<
      Awaited<ReturnType<WorkerGitHubGateway["listOpenPullRequests"]>>
    > => {
      return [
        {
          number: 77,
          title: "Existing PR",
          state: "OPEN",
          isDraft: false,
          reviewDecision: undefined,
          labels: ["looper:spec-ready"],
          headRefName: "feature/existing-pr",
          baseRefName: "main",
          author: "octocat",
          reviewRequests: ["octocat"],
        },
      ];
    };

    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git: new FakeGitGateway(fixture.worktreeRoot),
      github,
      agentExecutor: new FakeAgentExecutor([
        completedAgentResult("Implemented existing PR", ["abc123"]),
      ]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const discovery = await runner.discoverPullRequests({
      projectId: "project_1",
      repo: "acme/looper",
    });

    expect(discovery.queueItems).toHaveLength(0);
    expect(fixture.store.loops.getById("loop_worker_paused_pr")?.status).toBe(
      "paused",
    );

    fixture.store.close();
  });

  test("keeps spec-ready label when pull_request mode has no resolved spec path", async () => {
    const fixture = await createFixture();
    configurePullRequestWorkerLoop(fixture, 78);

    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    github.viewPullRequest = async (input) => ({
      number: input.prNumber,
      title: "Missing spec",
      body: "No spec here",
      url: `https://example.test/${input.repo}/pull/${input.prNumber}`,
      state: "OPEN",
      isDraft: false,
      reviewDecision: undefined,
      labels: ["looper:spec-ready"],
      headRefName: "feature/missing-spec",
      baseRefName: "main",
      headSha: "abc123",
      baseSha: "base123",
      author: "octocat",
      reviewRequests: ["octocat"],
      comments: [],
      reviews: [],
      checks: [{ conclusion: "SUCCESS" }],
    });

    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: new FakeAgentExecutor([completedAgentResult("unused")]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("failed");
    expect(result.failureKind).toBe("manual_intervention");
    expect(github.removedLabels).toHaveLength(0);

    fixture.store.close();
  });

  test("keeps spec-ready label when pull_request lock acquisition fails", async () => {
    const fixture = await createFixture();
    configurePullRequestWorkerLoop(fixture, 79);
    fixture.queue.acquireBusinessLock({
      key: "pr:acme/looper:79",
      owner: "other-worker",
      reason: "test-lock",
      expiresAt: new Date(fixture.now.getTime() + 60_000).toISOString(),
    });

    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: new FakeAgentExecutor([completedAgentResult("unused")]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("failed");
    expect(result.failureKind).toBe("retryable_transient");
    expect(github.removedLabels).toHaveLength(0);

    fixture.store.close();
  });

  test("resumes from open-pr after a retryable PR creation failure", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    let failuresRemaining = 1;
    github.createPullRequest = async (input) => {
      github.createPullRequestCalls.push(input);
      if (failuresRemaining > 0) {
        failuresRemaining -= 1;
        throw new Error("temporary GitHub failure");
      }
      return {
        number: 101,
        url: "https://example.test/acme/looper/pull/101",
      };
    };
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const logs = createCapturingLogger();
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: logs.logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const firstClaim = fixture.queue.claimNext("worker-1");
    if (!firstClaim) {
      throw new Error("Expected first worker queue item claim");
    }

    const firstResult = await runner.processClaimedItem(firstClaim);
    expect(firstResult.status).toBe("failed");
    expect(firstResult.failureKind).toBe("retryable_after_resume");
    expect(agent.starts).toHaveLength(1);

    const retryClaim = fixture.queue.claimNext("worker-1");
    if (!retryClaim) {
      throw new Error("Expected retry worker queue item claim");
    }

    const retryResult = await runner.processClaimedItem(retryClaim);
    expect(retryResult.status).toBe("success");
    expect(agent.starts).toHaveLength(1);
    expect(retryResult.pullRequestNumber).toBe(101);

    fixture.store.close();
  });

  test("skips PR creation when loop already tracks a pull request", async () => {
    const fixture = await createFixture();
    fixture.store.loops.upsert({
      ...(fixture.store.loops.getById("loop_worker_1") ?? {
        id: "loop_worker_1",
        seq: 1,
        projectId: "project_1",
        type: "worker",
        targetType: "project",
        targetId: "project_1",
        repo: "acme/looper",
        prNumber: 101,
        status: "queued",
        configJson: null,
        metadataJson: null,
        lastRunAt: null,
        nextRunAt: fixture.now.toISOString(),
        createdAt: fixture.now.toISOString(),
        updatedAt: fixture.now.toISOString(),
      }),
      prNumber: 101,
      metadataJson: JSON.stringify({
        prUrl: "https://example.test/acme/looper/pull/101",
      }),
      updatedAt: fixture.now.toISOString(),
    });

    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("success");
    expect(result.pullRequestNumber).toBe(101);
    expect(git.pushCalls).toBe(0);
    expect(github.createPullRequestCalls).toHaveLength(0);

    fixture.store.close();
  });

  test("pauses for manual PR opening when auto push is disabled", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
      allowAutoPush: false,
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("skipped");
    expect(result.summary).toContain("Auto push disabled");
    expect(agent.starts[0]?.prompt).not.toContain(
      "use the GitHub CLI (`gh`) to create the pull request yourself",
    );
    expect(github.listOpenPullRequestCalls).toHaveLength(0);
    expect(git.pushCalls).toBe(0);
    expect(github.createPullRequestCalls).toHaveLength(0);
    expect(fixture.store.loops.getById("loop_worker_1")?.status).toBe(
      "completed",
    );

    fixture.store.close();
  });

  test("skips GitHub PR lookup when manual PR opening is configured", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "manual",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("skipped");
    expect(result.summary).toContain("PR opening is manual");
    expect(github.listOpenPullRequestCalls).toHaveLength(0);
    expect(github.createPullRequestCalls).toHaveLength(0);

    fixture.store.close();
  });

  test("defaults to manual PR opening when no strategy is configured", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("skipped");
    expect(result.summary).toContain("PR opening is manual");
    expect(github.listOpenPullRequestCalls).toHaveLength(0);
    expect(github.createPullRequestCalls).toHaveLength(0);

    fixture.store.close();
  });

  test("skips agent execution when auto commit is disabled", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
      allowAutoCommit: false,
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("skipped");
    expect(result.summary).toContain("Auto commit disabled");
    expect(agent.starts).toHaveLength(0);
    expect(git.pushCalls).toBe(0);
    expect(github.createPullRequestCalls).toHaveLength(0);

    fixture.store.close();
  });
});
