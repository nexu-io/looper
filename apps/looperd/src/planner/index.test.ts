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
  type PlannerAgentExecution,
  type PlannerAgentExecutor,
  type PlannerGitGateway,
  type PlannerGitHubGateway,
  PlannerLoopRunner,
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
  const rootDir = await mkdtemp(join(tmpdir(), "looper-planner-"));
  cleanupPaths.push(rootDir);
  const repoPath = join(rootDir, "repo");
  const worktreeRoot = join(rootDir, "worktrees");
  await mkdir(repoPath, { recursive: true });
  await mkdir(worktreeRoot, { recursive: true });
  await writeFile(join(repoPath, "AGENTS.md"), "# Repo Rules\n\nUse specs/.\n");

  const store = new SqliteStore({
    dbPath: join(rootDir, "state", "looper.sqlite"),
  });
  store.initialize({ autoMigrate: true });

  const now = new Date("2026-04-12T12:00:00.000Z");
  const nowIso = now.toISOString();

  store.projects.upsert({
    id: "project_1",
    name: "Looper",
    repoPath,
    baseBranch: "main",
    archived: false,
    metadataJson: JSON.stringify({
      repo: "acme/looper",
      worktreeRoot,
      reviewers: ["review-bot"],
    }),
    createdAt: nowIso,
    updatedAt: nowIso,
  });

  const queue = new SchedulerQueue({
    store,
    retryMaxAttempts: 3,
    retryBaseDelayMs: 0,
    now: () => now,
  });

  return { rootDir, repoPath, worktreeRoot, store, queue, now };
}

class FakeGitHubGateway implements PlannerGitHubGateway {
  public createPullRequestCalls: Array<{
    repo: string;
    headBranch: string;
    baseBranch: string;
    title: string;
    body?: string;
    cwd?: string;
  }> = [];
  public addLabelCalls: Array<{
    repo: string;
    prNumber: number;
    labels: string[];
  }> = [];
  public addReviewerCalls: Array<{
    repo: string;
    prNumber: number;
    reviewers: string[];
  }> = [];

  constructor(
    private readonly options: {
      issues?: Array<{
        number: number;
        title: string;
        body?: string;
        url?: string;
        assignees?: string[];
        labels?: string[];
      }>;
      currentUserLogin?: string;
    } = {},
  ) {}

  public async listOpenIssues() {
    return (this.options.issues ?? []).map((issue) => ({
      number: issue.number,
      title: issue.title,
      body: issue.body,
      url: issue.url,
      state: "OPEN",
      author: "octocat",
      assignees: issue.assignees ?? [
        this.options.currentUserLogin ?? "octocat",
      ],
      labels: issue.labels ?? ["looper:plan"],
    }));
  }

  public async viewIssue(input: { repo: string; issueNumber: number }) {
    const issue = (this.options.issues ?? []).find(
      (candidate) => candidate.number === input.issueNumber,
    );
    if (!issue) {
      throw new Error(`Missing issue ${input.repo}#${input.issueNumber}`);
    }

    return {
      number: issue.number,
      title: issue.title,
      body: issue.body,
      url: issue.url,
      state: "OPEN",
      author: "octocat",
      assignees: issue.assignees ?? [
        this.options.currentUserLogin ?? "octocat",
      ],
      labels: issue.labels ?? ["looper:plan"],
    };
  }

  public async getCurrentUserLogin(): Promise<string | undefined> {
    return this.options.currentUserLogin ?? "octocat";
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
      number: 77,
      url: "https://example.test/acme/looper/pull/77",
    };
  }

  public async addPullRequestLabels(input: {
    repo: string;
    prNumber: number;
    labels: string[];
    cwd?: string;
  }): Promise<void> {
    this.addLabelCalls.push({
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
    this.addReviewerCalls.push({
      repo: input.repo,
      prNumber: input.prNumber,
      reviewers: input.reviewers,
    });
  }
}

class FakeGitGateway implements PlannerGitGateway {
  public createWorktreeCalls = 0;
  public pushCalls = 0;
  public lastCreateWorktreeInput?: {
    projectId: string;
    repoPath: string;
    worktreeRoot: string;
    branch: string;
    baseBranch: string;
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
      createdAt: "2026-04-12T12:00:00.000Z",
      updatedAt: "2026-04-12T12:00:00.000Z",
      cleanedAt: null,
    };
  }

  public async push(): Promise<void> {
    this.pushCalls += 1;
  }
}

class FakeAgentExecutor implements PlannerAgentExecutor {
  public starts: AgentRunInput[] = [];

  constructor(private readonly results: AgentResult[]) {}

  public async start(input: AgentRunInput): Promise<PlannerAgentExecution> {
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
    changedFiles: ["specs/123-add-planner/spec.md"],
    commits: ["abc123"],
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
    completionSignal: undefined,
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
    level: "info" | "warn" | "error";
    message: string;
    context?: Record<string, unknown>;
  }> = [];
  const logger: Logger = {
    debug: () => {},
    info: (message, context) =>
      entries.push({ level: "info", message, context }),
    warn: (message, context) =>
      entries.push({ level: "warn", message, context }),
    error: (message, context) =>
      entries.push({ level: "error", message, context }),
  };

  return { logger, entries };
}

describe("PlannerLoopRunner", () => {
  test("discovers planner issues and enqueues work", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      issues: [
        {
          number: 123,
          title: "Add planner flow",
          body: "Implement planning loop",
          url: "https://example.test/acme/looper/issues/123",
          assignees: ["octocat"],
          labels: ["looper:plan"],
        },
      ],
      currentUserLogin: "octocat",
    });
    const runner = new PlannerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git: new FakeGitGateway(fixture.worktreeRoot),
      github,
      agentExecutor: new FakeAgentExecutor([
        completedAgentResult("wrote spec"),
      ]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
    });

    const result = await runner.discoverIssues();

    expect(result.createdLoopIds).toHaveLength(1);
    expect(result.queueItems).toHaveLength(1);
    expect(result.queueItems[0]?.targetId).toBe("issue:acme/looper:123");
    expect(result.queueItems[0]?.dedupeKey).toBe("planner:acme/looper:123");
    expect(fixture.store.loops.list()[0]?.targetType).toBe("issue");
    expect(fixture.store.loops.list()[0]?.metadataJson).toContain(
      "specs/2026-04-12-123-add-planner-flow.md",
    );

    fixture.store.close();
  });

  test("builds unique spec paths for same-title issues", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      issues: [
        {
          number: 123,
          title: "Add planner flow",
          assignees: ["octocat"],
          labels: ["looper:plan"],
        },
        {
          number: 124,
          title: "Add planner flow",
          assignees: ["octocat"],
          labels: ["looper:plan"],
        },
      ],
      currentUserLogin: "octocat",
    });
    const runner = new PlannerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git: new FakeGitGateway(fixture.worktreeRoot),
      github,
      agentExecutor: new FakeAgentExecutor([
        completedAgentResult("wrote spec"),
      ]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
    });

    const result = await runner.discoverIssues();

    expect(result.createdLoopIds).toHaveLength(2);
    const loopMetadata = fixture.store.loops
      .list()
      .map((loop) => loop.metadataJson ?? "");
    expect(loopMetadata).toEqual(
      expect.arrayContaining([
        expect.stringContaining("specs/2026-04-12-123-add-planner-flow.md"),
        expect.stringContaining("specs/2026-04-12-124-add-planner-flow.md"),
      ]),
    );

    fixture.store.close();
  });

  test("skips discovery when repo maps to multiple active projects", async () => {
    const fixture = await createFixture();
    const nowIso = fixture.now.toISOString();
    fixture.store.projects.upsert({
      id: "project_2",
      name: "Looper Copy",
      repoPath: join(fixture.rootDir, "repo-copy"),
      baseBranch: "main",
      archived: false,
      metadataJson: JSON.stringify({ repo: "acme/looper" }),
      createdAt: nowIso,
      updatedAt: nowIso,
    });

    const runner = new PlannerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git: new FakeGitGateway(fixture.worktreeRoot),
      github: new FakeGitHubGateway({
        issues: [{ number: 123, title: "Add planner flow" }],
      }),
      agentExecutor: new FakeAgentExecutor([
        completedAgentResult("wrote spec"),
      ]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
    });

    const result = await runner.discoverIssues();

    expect(result.queueItems).toHaveLength(0);
    expect(result.createdLoopIds).toHaveLength(0);
    expect(result.skipped).toBe(1);

    fixture.store.close();
  });

  test("processes a planner run and publishes a spec PR", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      issues: [
        {
          number: 123,
          title: "Add planner flow",
          body: "Implement planning loop",
          url: "https://example.test/acme/looper/issues/123",
          assignees: ["octocat"],
          labels: ["looper:plan"],
        },
      ],
      currentUserLogin: "octocat",
    });
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const agent = new FakeAgentExecutor([
      completedAgentResult("Spec committed"),
    ]);
    const logs = createCapturingLogger();
    const runner = new PlannerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: logs.logger,
      now: () => fixture.now,
    });

    await runner.discoverIssues();
    const claimed = fixture.queue.claimNext("planner-1");
    if (!claimed) {
      throw new Error("Expected planner queue item");
    }

    const result = await runner.processClaimedItem(claimed);

    expect(result.status).toBe("success");
    expect(result.pullRequestNumber).toBe(77);
    expect(git.createWorktreeCalls).toBe(1);
    expect(git.lastCreateWorktreeInput?.worktreeRoot).toBe(
      fixture.worktreeRoot,
    );
    expect(git.pushCalls).toBe(1);
    expect(agent.starts).toHaveLength(1);
    expect(agent.starts[0]?.prompt).toContain(
      "Spec path: specs/2026-04-12-123-add-planner-flow.md",
    );
    expect(agent.starts[0]?.prompt).toContain("AGENTS.md:");
    expect(github.createPullRequestCalls).toHaveLength(1);
    expect(github.createPullRequestCalls[0]?.body).toContain(
      "Spec: specs/2026-04-12-123-add-planner-flow.md",
    );
    expect(github.addLabelCalls).toEqual([
      {
        repo: "acme/looper",
        prNumber: 77,
        labels: ["looper:spec-reviewing"],
      },
    ]);
    expect(github.addReviewerCalls).toEqual([
      {
        repo: "acme/looper",
        prNumber: 77,
        reviewers: ["review-bot"],
      },
    ]);
    expect(fixture.store.loops.getById("loop_planner_missing")).toBeNull();
    const loop = fixture.store.loops.list()[0];
    expect(loop?.prNumber).toBe(77);
    expect(loop?.metadataJson).toContain(
      "specs/2026-04-12-123-add-planner-flow.md",
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
      metadataJson: JSON.stringify({
        repo: "acme/looper",
        reviewers: ["review-bot"],
      }),
      updatedAt: fixture.now.toISOString(),
    });

    const github = new FakeGitHubGateway({
      issues: [
        {
          number: 123,
          title: "Add planner flow",
          body: "Implement planning loop",
          url: "https://example.test/acme/looper/issues/123",
          assignees: ["octocat"],
          labels: ["looper:plan"],
        },
      ],
      currentUserLogin: "octocat",
    });
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const runner = new PlannerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: new FakeAgentExecutor([
        completedAgentResult("Spec committed"),
      ]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
    });

    await runner.discoverIssues();
    const claimed = fixture.queue.claimNext("planner-1");
    if (!claimed) {
      throw new Error("Expected planner queue item");
    }

    const result = await runner.processClaimedItem(claimed);

    expect(result.status).toBe("success");
    expect(git.lastCreateWorktreeInput?.worktreeRoot).toBe(
      getDefaultProjectWorktreeRoot("project_1", fixture.repoPath),
    );

    fixture.store.close();
  });

  test("resumes publish without re-running prior planner steps", async () => {
    const fixture = await createFixture();
    const nowIso = fixture.now.toISOString();
    fixture.store.loops.upsert({
      id: "loop_planner_1",
      seq: 1,
      projectId: "project_1",
      type: "planner",
      targetType: "issue",
      targetId: "issue:acme/looper:123",
      repo: "acme/looper",
      prNumber: 77,
      status: "queued",
      configJson: null,
      metadataJson: JSON.stringify({
        specPath: "specs/2026-04-12-123-add-planner-flow.md",
        prUrl: "https://example.test/acme/looper/pull/77",
      }),
      lastRunAt: null,
      nextRunAt: nowIso,
      createdAt: nowIso,
      updatedAt: nowIso,
    });
    fixture.store.runs.upsert({
      id: "run_planner_1",
      loopId: "loop_planner_1",
      status: "failed",
      currentStep: "publish",
      lastCompletedStep: "write-spec",
      checkpointJson: JSON.stringify({
        resumePolicy: "advance_from_checkpoint",
        claimedLockKey: "issue:acme/looper:123",
        issue: {
          repo: "acme/looper",
          issueNumber: 123,
          title: "Add planner flow",
          body: "Implement planning loop",
          url: "https://example.test/acme/looper/issues/123",
          assignees: ["octocat"],
          labels: ["looper:plan"],
          currentUserLogin: "octocat",
          specPath: "specs/2026-04-12-123-add-planner-flow.md",
          requestedReviewers: ["review-bot"],
        },
        worktree: {
          id: "worktree_1",
          path: fixture.worktreeRoot,
          branch: "looper/planner/123-add-planner-flow",
          baseBranch: "main",
          specPath: "specs/2026-04-12-123-add-planner-flow.md",
        },
        writeSpec: {
          status: "completed",
          summary: "Spec committed",
          changedFiles: ["specs/2026-04-12-123-add-planner-flow.md"],
          commits: ["abc123"],
          stdout: "Spec committed\n",
        },
        publish: {
          pushed: true,
          pullRequest: {
            number: 77,
            url: "https://example.test/acme/looper/pull/77",
            body: "Spec: specs/2026-04-12-123-add-planner-flow.md",
          },
          labelsAdded: [],
          reviewersAdded: [],
        },
      }),
      summary: "publish failed",
      errorMessage: "temporary failure",
      startedAt: nowIso,
      lastHeartbeatAt: nowIso,
      endedAt: nowIso,
      createdAt: nowIso,
      updatedAt: nowIso,
    });
    fixture.queue.enqueue({
      id: "queue_planner_1",
      projectId: "project_1",
      loopId: "loop_planner_1",
      type: "planner",
      targetType: "issue",
      targetId: "issue:acme/looper:123",
      repo: "acme/looper",
      dedupeKey: "planner:acme/looper:123",
      lockKey: "issue:acme/looper:123",
      payloadJson: JSON.stringify({ issueNumber: 123 }),
    });

    const github = new FakeGitHubGateway({
      issues: [{ number: 123, title: "Add planner flow" }],
      currentUserLogin: "octocat",
    });
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const agent = new FakeAgentExecutor([
      completedAgentResult("Spec committed"),
    ]);
    const runner = new PlannerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
    });
    const claimed = fixture.queue.claimNext("planner-1");
    if (!claimed) {
      throw new Error("Expected planner queue item");
    }

    const result = await runner.processClaimedItem(claimed);

    expect(result.status).toBe("success");
    expect(git.createWorktreeCalls).toBe(0);
    expect(git.pushCalls).toBe(0);
    expect(agent.starts).toHaveLength(0);
    expect(github.createPullRequestCalls).toHaveLength(0);
    expect(github.addLabelCalls).toHaveLength(1);
    expect(github.addReviewerCalls).toHaveLength(1);

    fixture.store.close();
  });

  test("persists publish checkpoint after PR creation before later publish failure", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      issues: [
        {
          number: 123,
          title: "Add planner flow",
          body: "Implement planning loop",
          url: "https://example.test/acme/looper/issues/123",
          assignees: ["octocat"],
          labels: ["looper:plan"],
        },
      ],
      currentUserLogin: "octocat",
    });
    github.addPullRequestLabels = async () => {
      throw new Error("labeling failed");
    };

    const runner = new PlannerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git: new FakeGitGateway(fixture.worktreeRoot),
      github,
      agentExecutor: new FakeAgentExecutor([
        completedAgentResult("Spec committed"),
      ]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
    });

    await runner.discoverIssues();
    const claimed = fixture.queue.claimNext("planner-1");
    if (!claimed) {
      throw new Error("Expected planner queue item");
    }

    const firstResult = await runner.processClaimedItem(claimed);
    const plannerLoop = fixture.store.loops.list()[0];
    if (!plannerLoop) {
      throw new Error("Expected planner loop");
    }

    expect(firstResult.status).toBe("failed");
    expect(github.createPullRequestCalls).toHaveLength(1);

    const failedRun = fixture.store.runs.listByLoop(plannerLoop.id)[0];
    const failedCheckpoint = failedRun?.checkpointJson
      ? JSON.parse(failedRun.checkpointJson)
      : null;
    expect(failedCheckpoint?.publish?.pullRequest?.number).toBe(77);

    github.addPullRequestLabels = async (input) => {
      FakeGitHubGateway.prototype.addPullRequestLabels.call(github, input);
    };
    fixture.queue.enqueue({
      id: "queue_planner_retry_1",
      projectId: "project_1",
      loopId: plannerLoop.id,
      type: "planner",
      targetType: "issue",
      targetId: "issue:acme/looper:123",
      repo: "acme/looper",
      dedupeKey: "planner:acme/looper:123:retry",
      lockKey: "issue:acme/looper:123",
      payloadJson: JSON.stringify({ issueNumber: 123 }),
    });
    const retryClaimed = fixture.queue.claimNext("planner-2");
    if (!retryClaimed) {
      throw new Error("Expected retry planner queue item");
    }

    const retryResult = await runner.processClaimedItem(retryClaimed);

    expect(retryResult.status).toBe("success");
    expect(github.createPullRequestCalls).toHaveLength(1);

    fixture.store.close();
  });

  test("discovery does not requeue paused or completed planner loops", async () => {
    const fixture = await createFixture();
    const nowIso = fixture.now.toISOString();
    fixture.store.loops.upsert({
      id: "loop_planner_paused",
      seq: 1,
      projectId: "project_1",
      type: "planner",
      targetType: "issue",
      targetId: "issue:acme/looper:123",
      repo: "acme/looper",
      prNumber: null,
      status: "paused",
      configJson: null,
      metadataJson: JSON.stringify({
        specPath: "specs/2026-04-12-123-add-planner-flow.md",
      }),
      lastRunAt: nowIso,
      nextRunAt: null,
      createdAt: nowIso,
      updatedAt: nowIso,
    });
    fixture.store.loops.upsert({
      id: "loop_planner_completed",
      seq: 2,
      projectId: "project_1",
      type: "planner",
      targetType: "issue",
      targetId: "issue:acme/looper:124",
      repo: "acme/looper",
      prNumber: 77,
      status: "completed",
      configJson: null,
      metadataJson: JSON.stringify({
        specPath: "specs/2026-04-12-124-add-another-planner-flow.md",
      }),
      lastRunAt: nowIso,
      nextRunAt: null,
      createdAt: nowIso,
      updatedAt: nowIso,
    });

    const runner = new PlannerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git: new FakeGitGateway(fixture.worktreeRoot),
      github: new FakeGitHubGateway({
        issues: [
          { number: 123, title: "Add planner flow" },
          { number: 124, title: "Add another planner flow" },
        ],
        currentUserLogin: "octocat",
      }),
      agentExecutor: new FakeAgentExecutor([
        completedAgentResult("Spec committed"),
      ]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
    });

    const discovery = await runner.discoverIssues();

    expect(discovery.queueItems).toHaveLength(0);
    expect(discovery.skipped).toBe(2);
    expect(fixture.store.loops.getById("loop_planner_paused")?.status).toBe(
      "paused",
    );
    expect(fixture.store.loops.getById("loop_planner_completed")?.status).toBe(
      "completed",
    );

    fixture.store.close();
  });

  test("marks the planner run failed when spec writing fails", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      issues: [
        {
          number: 123,
          title: "Add planner flow",
          body: "Implement planning loop",
          url: "https://example.test/acme/looper/issues/123",
          assignees: ["octocat"],
          labels: ["looper:plan"],
        },
      ],
      currentUserLogin: "octocat",
    });
    const runner = new PlannerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git: new FakeGitGateway(fixture.worktreeRoot),
      github,
      agentExecutor: new FakeAgentExecutor([
        failedAgentResult("planner agent crashed"),
      ]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
    });

    await runner.discoverIssues();
    const claimed = fixture.queue.claimNext("planner-1");
    if (!claimed) {
      throw new Error("Expected planner queue item");
    }

    const result = await runner.processClaimedItem(claimed);

    expect(result.status).toBe("failed");
    expect(result.failureKind).toBe("retryable_transient");
    const runs = fixture.store.runs.listByLoop(result.loopId);
    expect(runs[0]?.status).toBe("failed");
    expect(fixture.store.loops.getById(result.loopId)?.status).toBe("queued");

    fixture.store.close();
  });

  test("truncates planner spec paths and branch slugs to four words", async () => {
    const fixture = await createFixture();
    const github = new FakeGitHubGateway({
      issues: [
        {
          number: 123,
          title:
            "Let agents create their own git commits with looperd as fallback",
          body: "Implement planning loop",
          url: "https://example.test/acme/looper/issues/123",
          assignees: ["octocat"],
          labels: ["looper:plan"],
        },
      ],
      currentUserLogin: "octocat",
    });
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const agent = new FakeAgentExecutor([
      completedAgentResult("Spec committed"),
    ]);
    const runner = new PlannerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
    });

    await runner.discoverIssues();
    const claimed = fixture.queue.claimNext("planner-1");
    if (!claimed) {
      throw new Error("Expected planner queue item");
    }

    const result = await runner.processClaimedItem(claimed);

    expect(result.status).toBe("success");
    expect(agent.starts[0]?.prompt).toContain(
      "Spec path: specs/2026-04-12-123-let-agents-create-their.md",
    );

    const latestRun = fixture.store.runs.listByLoop(result.loopId)[0];
    const checkpoint = latestRun?.checkpointJson
      ? JSON.parse(latestRun.checkpointJson)
      : null;

    expect(checkpoint?.worktree?.branch).toBe(
      "looper/planner/123-let-agents-create-their",
    );
    expect(checkpoint?.issue?.specPath).toBe(
      "specs/2026-04-12-123-let-agents-create-their.md",
    );

    fixture.store.close();
  });
});
