import { randomUUID } from "node:crypto";
import { readFile } from "node:fs/promises";
import { isAbsolute, join } from "node:path";

import type { Logger } from "../bootstrap/logger";
import {
  type OpenPrStrategy,
  getDefaultProjectWorktreeRoot,
} from "../config/index";
import { createPrLockKey } from "../domain/index";
import type { AgentResult, AgentRunInput } from "../infra/agent";
import { appendCompletionInstruction } from "../infra/agent-prompt";
import { CommandExecutionError, runCommand } from "../infra/command";
import { ProtectedBranchError } from "../infra/git";
import type {
  GitHubIssueDetail,
  GitHubPullRequestDetail,
  GitHubPullRequestSummary,
} from "../infra/github";
import {
  SPEC_READY_LABEL,
  parseSpecPathFromPullRequestBody,
} from "../infra/spec-pr";
import type { SchedulerQueue } from "../scheduler/index";
import type { Store } from "../storage/store";
import type {
  LoopRecord,
  ProjectRecord,
  QueueFailureKind,
  QueueItemRecord,
  RunRecord,
  WorktreeRecord,
} from "../storage/types";

const WORKER_STEP_SEQUENCE = [
  "prepare-work",
  "prepare-worktree",
  "plan",
  "execute",
  "validate",
  "open-pr",
] as const;

const WORKER_BRANCH_SLUG_MAX_LENGTH = 48;
const WORKER_PR_DEDUPE_LOOKUP_LIMIT = 1000;

export type WorkerStep = (typeof WORKER_STEP_SEQUENCE)[number];

export interface WorkerGitGateway {
  createWorktree(input: {
    projectId: string;
    repoPath: string;
    worktreeRoot: string;
    branch: string;
    baseBranch: string;
    prNumber?: number;
    protectedBranches?: string[];
  }): Promise<WorktreeRecord>;
  prepareWorktree(input: {
    worktreePath: string;
    branch: string;
    expectedHeadSha?: string;
    remote?: string;
  }): Promise<{
    headSha?: string;
    clean: boolean;
  }>;
  push(input: {
    worktreePath: string;
    branch: string;
    remote?: string;
    protectedBranches?: string[];
  }): Promise<void>;
}

export interface WorkerGitHubGateway {
  listOpenPullRequests(input: {
    repo: string;
    cwd?: string;
    limit?: number;
    label?: string;
  }): Promise<GitHubPullRequestSummary[]>;
  viewPullRequest(input: {
    repo: string;
    prNumber: number;
    cwd?: string;
  }): Promise<GitHubPullRequestDetail>;
  viewIssue(input: {
    repo: string;
    issueNumber: number;
    cwd?: string;
  }): Promise<GitHubIssueDetail>;
  createPullRequest(input: {
    repo: string;
    headBranch: string;
    baseBranch: string;
    title: string;
    body?: string;
    cwd?: string;
  }): Promise<{ number?: number; url: string }>;
  removePullRequestLabels(input: {
    repo: string;
    prNumber: number;
    labels: string[];
    cwd?: string;
  }): Promise<void>;
  addPullRequestReviewers(input: {
    repo: string;
    prNumber: number;
    reviewers: string[];
    cwd?: string;
  }): Promise<void>;
}

export interface WorkerAgentExecution {
  wait(): Promise<AgentResult>;
}

export interface WorkerAgentExecutor {
  start(input: AgentRunInput): Promise<WorkerAgentExecution>;
}

export interface WorkerValidationResult {
  passed: boolean;
  summary?: string;
  output?: string;
}

export interface WorkerLoopRunnerOptions {
  store: Store;
  scheduler: SchedulerQueue;
  git: WorkerGitGateway;
  github: WorkerGitHubGateway;
  agentExecutor: WorkerAgentExecutor;
  logger: Logger;
  onAgentExecutionStarted?: (input: {
    executionId: string;
    projectId: string;
    loopId: string;
    runId: string;
    subtitle: string;
    body: string;
    dedupeKey: string;
  }) => Promise<void> | void;
  now?: () => Date;
  agentTimeoutMs?: number;
  claimTtlMs?: number;
  validationCommands?: string[];
  validationRunner?: (input: {
    cwd: string;
    commands: string[];
  }) => Promise<WorkerValidationResult>;
  openPrStrategy?: OpenPrStrategy;
  allowAutoCommit?: boolean;
  allowAutoPush?: boolean;
}

export interface WorkerProcessResult {
  loopId: string;
  runId: string;
  queueItemId: string;
  status: "success" | "skipped" | "failed";
  summary: string;
  failureKind?: QueueFailureKind;
  pullRequestNumber?: number;
}

interface WorkerInput {
  title: string;
  prompt?: string | null;
  specPath?: string | null;
  repo: string;
  baseBranch: string;
  executionMode: "create-pr" | "push-existing";
  issueNumber?: number;
  issueUrl?: string;
  prNumber?: number;
  branch?: string;
  headSha?: string;
  reviewers?: string[];
}

interface WorkerCheckpoint {
  resumePolicy?:
    | "replay_step"
    | "advance_from_checkpoint"
    | "manual_intervention";
  work?: WorkerInput;
  claimedLockKey?: string;
  worktree?: {
    id: string;
    path: string;
    branch: string;
    baseBranch: string;
  };
  plan?: {
    summary: string;
    items: string[];
  };
  execution?: {
    status: AgentResult["status"];
    summary?: string;
    changedFiles: string[];
    commits: string[];
    stdout: string;
  };
  validation?: WorkerValidationResult;
  pullRequest?: {
    number?: number;
    url: string;
  };
  skipReason?: string;
}

interface ResumedRunContext {
  run: RunRecord;
  startStep: WorkerStep;
  checkpoint: WorkerCheckpoint;
  resumed: boolean;
}

class WorkerLoopError extends Error {
  constructor(
    message: string,
    public readonly kind: QueueFailureKind,
  ) {
    super(message);
    this.name = "WorkerLoopError";
  }
}

export class WorkerLoopRunner {
  private readonly now: () => Date;
  private readonly agentTimeoutMs: number;
  private readonly claimTtlMs: number;
  private readonly validationCommands: string[];
  private readonly openPrStrategy: OpenPrStrategy;
  private readonly allowAutoCommit: boolean;
  private readonly allowAutoPush: boolean;

  constructor(private readonly options: WorkerLoopRunnerOptions) {
    this.now = options.now ?? (() => new Date());
    this.agentTimeoutMs = options.agentTimeoutMs ?? 30 * 60_000;
    this.claimTtlMs = options.claimTtlMs ?? 10 * 60_000;
    this.validationCommands = options.validationCommands ?? [];
    this.openPrStrategy = options.openPrStrategy ?? "manual";
    this.allowAutoCommit = options.allowAutoCommit ?? true;
    this.allowAutoPush = options.allowAutoPush ?? true;
  }

  public async processNext(
    claimedBy: string,
  ): Promise<WorkerProcessResult | null> {
    const item = this.options.scheduler.claimNextOfType(claimedBy, "worker");
    if (!item) {
      return null;
    }

    return this.processClaimedItem(item);
  }

  public async processClaimedItem(
    queueItem: QueueItemRecord,
  ): Promise<WorkerProcessResult> {
    if (queueItem.type !== "worker") {
      throw new Error(`Unsupported queue item type: ${queueItem.type}`);
    }
    if (!queueItem.loopId) {
      throw new Error("Worker queue item requires loopId");
    }

    let loop: LoopRecord | undefined;
    let run: RunRecord | undefined;
    let checkpoint: WorkerCheckpoint | undefined;
    let claimedLockKey: string | undefined;
    try {
      loop = this.getLoop(queueItem.loopId);
      const project = this.getProject(loop.projectId);
      const resumedRun = this.createRunContext(loop);
      run = resumedRun.run;
      checkpoint = resumedRun.checkpoint;
      claimedLockKey =
        resumedRun.startStep !== "prepare-work"
          ? checkpoint.claimedLockKey
          : undefined;

      if (claimedLockKey) {
        const acquired = this.options.scheduler.acquireBusinessLock({
          key: claimedLockKey,
          owner: queueItem.id,
          reason: "worker-run-resume",
          expiresAt: new Date(
            this.now().getTime() + this.claimTtlMs,
          ).toISOString(),
        });
        if (!acquired) {
          throw new WorkerLoopError(
            `Worker lock is already held for ${claimedLockKey}`,
            "retryable_transient",
          );
        }
      }

      this.updateLoop(loop, {
        status: "running",
        lastRunAt: run.startedAt,
        nextRunAt: null,
      });
      this.appendEvent({
        eventType: "loop.started",
        projectId: project.id,
        loopId: loop.id,
        runId: run.id,
        entityType: "loop",
        entityId: loop.id,
        payload: {
          queueItemId: queueItem.id,
          resumed: resumedRun.resumed,
          startStep: resumedRun.startStep,
        },
      });

      for (const step of WORKER_STEP_SEQUENCE.slice(
        WORKER_STEP_SEQUENCE.indexOf(resumedRun.startStep),
      )) {
        run = this.persistStepStarted(run, step, checkpoint);
        checkpoint = await this.executeStep({
          step,
          checkpoint,
          project,
          loop,
          run,
          queueItem,
        });

        if (step === "prepare-work") {
          claimedLockKey = checkpoint.claimedLockKey;
        }

        run = this.persistStepCompleted(run, step, checkpoint);
        if (checkpoint.skipReason) {
          break;
        }
      }

      const summary = this.buildSuccessSummary(loop, checkpoint);
      this.finalizeRun(run, {
        status: "success",
        summary,
        checkpoint,
      });
      this.options.scheduler.complete(queueItem.id);
      this.updateLoop(loop, {
        status: "completed",
        lastRunAt: this.nowIso(),
        nextRunAt: null,
      });

      return {
        loopId: loop.id,
        runId: run.id,
        queueItemId: queueItem.id,
        status: checkpoint.skipReason ? "skipped" : "success",
        summary,
        pullRequestNumber: checkpoint.pullRequest?.number,
      };
    } catch (error) {
      const failure = this.classifyFailure(error);
      if (!loop || !run || !checkpoint) {
        this.options.scheduler.fail(
          queueItem.id,
          failure.kind,
          failure.message,
        );
        throw error;
      }
      const failedCheckpoint = this.getLatestCheckpoint(run, checkpoint);
      this.finalizeRun(run, {
        status: "failed",
        summary: failure.message,
        checkpoint: {
          ...failedCheckpoint,
          resumePolicy:
            failure.kind === "retryable_after_resume"
              ? "advance_from_checkpoint"
              : failure.kind === "manual_intervention"
                ? "manual_intervention"
                : (failedCheckpoint.resumePolicy ?? "replay_step"),
        },
        errorMessage: failure.message,
      });

      const failedQueueItem = this.options.scheduler.fail(
        queueItem.id,
        failure.kind,
        failure.message,
      );

      this.updateLoop(loop, {
        status:
          failedQueueItem?.status === "queued"
            ? "queued"
            : failure.kind === "manual_intervention"
              ? "paused"
              : "failed",
        lastRunAt: this.nowIso(),
        nextRunAt:
          failedQueueItem?.status === "queued"
            ? failedQueueItem.availableAt
            : null,
      });

      return {
        loopId: loop.id,
        runId: run.id,
        queueItemId: queueItem.id,
        status: "failed",
        summary: failure.message,
        failureKind: failure.kind,
      };
    } finally {
      if (claimedLockKey) {
        this.options.scheduler.releaseBusinessLock(claimedLockKey);
      }
    }
  }

  public async discoverPullRequests(input: {
    projectId: string;
    repo: string;
    limit?: number;
  }): Promise<{
    queueItems: QueueItemRecord[];
    createdLoopIds: string[];
    skipped: number;
  }> {
    const project = this.getProject(input.projectId);
    const openPullRequests = await this.options.github.listOpenPullRequests({
      repo: input.repo,
      cwd: project.repoPath,
      limit: input.limit,
      label: SPEC_READY_LABEL,
    });

    const queueItems: QueueItemRecord[] = [];
    const createdLoopIds: string[] = [];
    let skipped = 0;

    for (const pullRequest of openPullRequests) {
      if (
        pullRequest.isDraft ||
        normalizePrState(pullRequest.state) !== "open"
      ) {
        skipped += 1;
        continue;
      }

      const loop = this.ensureLoopForPullRequest({
        projectId: project.id,
        repo: input.repo,
        prNumber: pullRequest.number,
      });
      if (loop.created) {
        createdLoopIds.push(loop.record.id);
      }

      if (loop.record.status === "paused") {
        skipped += 1;
        continue;
      }

      queueItems.push(
        this.options.scheduler.enqueue({
          projectId: project.id,
          loopId: loop.record.id,
          type: "worker",
          targetType: "pull_request",
          targetId: buildPullRequestTargetId(input.repo, pullRequest.number),
          repo: input.repo,
          prNumber: pullRequest.number,
          dedupeKey: buildWorkerPullRequestDedupeKey(
            project.id,
            input.repo,
            pullRequest.number,
          ),
          lockKey: buildWorkerPullRequestLockKey(
            input.repo,
            pullRequest.number,
          ),
        }),
      );
    }

    return { queueItems, createdLoopIds, skipped };
  }

  private async executeStep(input: {
    step: WorkerStep;
    checkpoint: WorkerCheckpoint;
    project: ProjectRecord;
    loop: LoopRecord;
    run: RunRecord;
    queueItem: QueueItemRecord;
  }): Promise<WorkerCheckpoint> {
    switch (input.step) {
      case "prepare-work":
        return this.runPrepareWorkStep(input);
      case "prepare-worktree":
        return this.runPrepareWorktreeStep(input);
      case "plan":
        return this.runPlanStep(input);
      case "execute":
        return this.runExecuteStep(input);
      case "validate":
        return this.runValidateStep(input);
      case "open-pr":
        return this.runOpenPrStep(input);
    }
  }

  private async runPrepareWorkStep(input: {
    checkpoint: WorkerCheckpoint;
    queueItem: QueueItemRecord;
    loop: LoopRecord;
    project: ProjectRecord;
  }): Promise<WorkerCheckpoint> {
    let pullRequestTarget:
      | {
          repo: string;
          prNumber: number;
        }
      | undefined;
    let work =
      input.checkpoint.work ??
      this.resolveWorkerInput(
        input.queueItem.payloadJson,
        input.loop.metadataJson,
        input.loop,
      );
    if (
      work.executionMode === "create-pr" &&
      work.issueNumber &&
      !work.prompt &&
      !work.specPath
    ) {
      const issue = await this.options.github.viewIssue({
        repo: work.repo,
        issueNumber: work.issueNumber,
        cwd: input.project.repoPath,
      });
      work = hydrateWorkerInputFromIssue(work, issue);
    }
    if (input.loop.targetType === "pull_request") {
      const repo = input.loop.repo ?? input.queueItem.repo;
      const prNumber = input.loop.prNumber ?? input.queueItem.prNumber;
      if (!repo || !prNumber) {
        throw new WorkerLoopError(
          "pull_request worker loop requires repo and prNumber",
          "non_retryable",
        );
      }
      pullRequestTarget = { repo, prNumber };

      const detail = await this.options.github.viewPullRequest({
        repo,
        prNumber,
        cwd: input.project.repoPath,
      });
      work = {
        ...work,
        title: detail.title || work.title,
        repo,
        prNumber,
        baseBranch: detail.baseRefName ?? work.baseBranch,
        branch: detail.headRefName,
        headSha: detail.headSha,
        executionMode: "push-existing",
        specPath:
          work.specPath ??
          parseSpecPathFromPullRequestBody(detail.body) ??
          null,
        reviewers: detail.reviewRequests,
      };
      if (!work.specPath) {
        throw new WorkerLoopError(
          `No explicit spec path found for ${repo}#${prNumber}`,
          "manual_intervention",
        );
      }
    }

    const lockKey =
      input.queueItem.lockKey ??
      (work.executionMode === "push-existing" && work.prNumber
        ? buildWorkerPullRequestLockKey(work.repo, work.prNumber)
        : `worker:${input.loop.id}`);
    const acquired = this.options.scheduler.acquireBusinessLock({
      key: lockKey,
      owner: input.queueItem.id,
      reason: "worker-run",
      expiresAt: new Date(this.now().getTime() + this.claimTtlMs).toISOString(),
    });
    if (!acquired) {
      throw new WorkerLoopError(
        `Worker lock is already held for ${lockKey}`,
        "retryable_transient",
      );
    }

    if (pullRequestTarget) {
      await this.options.github.removePullRequestLabels({
        repo: pullRequestTarget.repo,
        prNumber: pullRequestTarget.prNumber,
        labels: [SPEC_READY_LABEL],
        cwd: input.project.repoPath,
      });
    }

    return {
      ...input.checkpoint,
      work,
      claimedLockKey: lockKey,
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runPrepareWorktreeStep(input: {
    checkpoint: WorkerCheckpoint;
    project: ProjectRecord;
    loop: LoopRecord;
  }): Promise<WorkerCheckpoint> {
    if (input.checkpoint.worktree) {
      return input.checkpoint;
    }

    const work = requireWork(input.checkpoint);
    const projectMetadata = parseJsonObject(input.project.metadataJson);
    const configuredRoot = readString(projectMetadata.worktreeRoot);
    const worktreeRoot =
      configuredRoot ??
      getDefaultProjectWorktreeRoot(input.project.id, input.project.repoPath);
    const branch =
      work.executionMode === "push-existing"
        ? (work.branch ?? `pr-${work.prNumber}`)
        : buildWorkerBranchName(work, input.loop.id);
    const worktree = await this.options.git.createWorktree({
      projectId: input.project.id,
      repoPath: input.project.repoPath,
      worktreeRoot,
      branch,
      baseBranch: work.baseBranch,
      prNumber: work.prNumber,
      protectedBranches: [work.baseBranch],
    });

    if (work.executionMode === "push-existing") {
      const prepared = await this.options.git.prepareWorktree({
        worktreePath: worktree.worktreePath,
        branch: worktree.branch,
        expectedHeadSha: work.headSha,
      });
      if (!prepared.clean) {
        throw new WorkerLoopError(
          `Worker worktree is dirty for branch ${worktree.branch}`,
          "manual_intervention",
        );
      }
    }

    this.updateLoopMetadata(input.loop.id, {
      worktreeId: worktree.id,
      worktreePath: worktree.worktreePath,
      branch: worktree.branch,
      baseBranch: worktree.baseBranch,
    });

    return {
      ...input.checkpoint,
      worktree: {
        id: worktree.id,
        path: worktree.worktreePath,
        branch: worktree.branch,
        baseBranch: worktree.baseBranch,
      },
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runPlanStep(input: {
    checkpoint: WorkerCheckpoint;
  }): Promise<WorkerCheckpoint> {
    if (input.checkpoint.plan) {
      return input.checkpoint;
    }

    const work = requireWork(input.checkpoint);
    const items = [
      work.prompt ? `Implement: ${work.prompt}` : null,
      work.specPath ? `Follow spec: ${work.specPath}` : null,
    ].filter((value): value is string => Boolean(value));

    return {
      ...input.checkpoint,
      plan: {
        summary: work.title,
        items: items.length > 0 ? items : [work.title],
      },
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runExecuteStep(input: {
    checkpoint: WorkerCheckpoint;
    project: ProjectRecord;
    loop: LoopRecord;
    run: RunRecord;
  }): Promise<WorkerCheckpoint> {
    if (input.checkpoint.execution?.status === "completed") {
      return input.checkpoint;
    }
    if (!this.allowAutoCommit) {
      return {
        ...input.checkpoint,
        skipReason: `Auto commit disabled; manual execution required for worker ${input.loop.id}`,
        resumePolicy: "manual_intervention",
      };
    }

    const work = requireWork(input.checkpoint);
    const worktree = requireWorktree(input.checkpoint);
    const prompt = await buildWorkerPrompt({
      repoRootPath: worktree.path,
      work,
      plan: input.checkpoint.plan?.items ?? [],
      allowAgentPrCreation: this.canAgentCreatePr(work),
    });
    const executionId = randomUUID();
    const execution = await this.options.agentExecutor.start({
      executionId,
      projectId: input.project.id,
      loopId: input.loop.id,
      runId: input.run.id,
      prompt,
      workingDirectory: worktree.path,
      timeoutMs: this.agentTimeoutMs,
      metadata: {
        loopType: "worker",
        title: work.title,
        repo: work.repo,
        baseBranch: work.baseBranch,
      },
      idempotencyKey: `worker:${input.loop.id}`,
    });
    await this.options.onAgentExecutionStarted?.({
      executionId,
      projectId: input.project.id,
      loopId: input.loop.id,
      runId: input.run.id,
      subtitle: work.title,
      body: "Worker started",
      dedupeKey: `runtime.agent.started:worker:${input.run.id}`,
    });
    const result = await execution.wait();
    if (result.status !== "completed") {
      throw new WorkerLoopError(
        result.summary ?? `Worker agent ${result.status}`,
        "retryable_transient",
      );
    }

    return {
      ...input.checkpoint,
      execution: {
        status: result.status,
        summary: result.summary,
        changedFiles: result.changedFiles,
        commits: result.commits,
        stdout: result.rawLogs.stdout,
      },
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runValidateStep(input: {
    checkpoint: WorkerCheckpoint;
  }): Promise<WorkerCheckpoint> {
    const worktree = requireWorktree(input.checkpoint);
    return {
      ...input.checkpoint,
      validation: await this.runValidation({
        cwd: worktree.path,
        commands: this.validationCommands,
      }),
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runOpenPrStep(input: {
    checkpoint: WorkerCheckpoint;
    project: ProjectRecord;
    loop: LoopRecord;
  }): Promise<WorkerCheckpoint> {
    const work = requireWork(input.checkpoint);
    if (
      work.executionMode === "create-pr" &&
      (input.checkpoint.pullRequest || input.loop.prNumber)
    ) {
      const metadata = parseJsonObject(input.loop.metadataJson);
      return {
        ...input.checkpoint,
        pullRequest: input.checkpoint.pullRequest ?? {
          number: input.loop.prNumber ?? undefined,
          url: readString(metadata.prUrl) ?? "",
        },
      };
    }

    const worktree = requireWorktree(input.checkpoint);
    if (input.checkpoint.validation?.passed === false) {
      throw new WorkerLoopError(
        input.checkpoint.validation.summary ?? "Validation failed",
        "manual_intervention",
      );
    }
    if (work.executionMode === "push-existing") {
      try {
        await this.options.git.push({
          worktreePath: worktree.path,
          branch: worktree.branch,
          protectedBranches: [work.baseBranch],
        });
        if ((work.reviewers ?? []).length > 0 && work.prNumber) {
          await this.options.github.addPullRequestReviewers({
            repo: work.repo,
            prNumber: work.prNumber,
            reviewers: work.reviewers ?? [],
            cwd: input.project.repoPath,
          });
        }

        return {
          ...input.checkpoint,
          pullRequest: {
            number: work.prNumber,
            url:
              readString(parseJsonObject(input.loop.metadataJson).prUrl) ?? "",
          },
          resumePolicy: "advance_from_checkpoint",
        };
      } catch (error) {
        throw new WorkerLoopError(
          error instanceof Error
            ? error.message
            : "Failed to push existing pull request",
          "retryable_after_resume",
        );
      }
    }
    if (this.openPrStrategy === "manual") {
      return {
        ...input.checkpoint,
        skipReason: `Worker completed; PR opening is manual for ${input.loop.id}`,
        resumePolicy: "manual_intervention",
      };
    }
    if (!this.allowAutoPush) {
      return {
        ...input.checkpoint,
        skipReason: `Auto push disabled; manual PR opening required for worker ${input.loop.id}`,
        resumePolicy: "manual_intervention",
      };
    }

    try {
      const existingPullRequest = await this.findOpenPullRequestForBranch({
        repo: work.repo,
        branch: worktree.branch,
        baseBranch: work.baseBranch,
        cwd: input.project.repoPath,
      });
      if (existingPullRequest) {
        await this.options.git.push({
          worktreePath: worktree.path,
          branch: worktree.branch,
          protectedBranches: [work.baseBranch],
        });
        await this.assignReviewersIfNeeded({
          work,
          pullRequest: existingPullRequest,
          cwd: input.project.repoPath,
        });
        this.persistPullRequestReference(
          input.loop,
          work.repo,
          existingPullRequest,
        );
        return {
          ...input.checkpoint,
          pullRequest: {
            number: existingPullRequest.number,
            url: existingPullRequest.url ?? "",
          },
          resumePolicy: "advance_from_checkpoint",
        };
      }
      await this.options.git.push({
        worktreePath: worktree.path,
        branch: worktree.branch,
        protectedBranches: [work.baseBranch],
      });
      const discoveredPullRequest = await this.findOpenPullRequestForBranch({
        repo: work.repo,
        branch: worktree.branch,
        baseBranch: work.baseBranch,
        cwd: input.project.repoPath,
      });
      if (discoveredPullRequest) {
        await this.assignReviewersIfNeeded({
          work,
          pullRequest: discoveredPullRequest,
          cwd: input.project.repoPath,
        });
        this.persistPullRequestReference(
          input.loop,
          work.repo,
          discoveredPullRequest,
        );
        return {
          ...input.checkpoint,
          pullRequest: {
            number: discoveredPullRequest.number,
            url: discoveredPullRequest.url ?? "",
          },
          resumePolicy: "advance_from_checkpoint",
        };
      }
      const pullRequest = await this.options.github.createPullRequest({
        repo: work.repo,
        headBranch: worktree.branch,
        baseBranch: work.baseBranch,
        title: work.title,
        body: buildPullRequestBody({
          work,
          plan: input.checkpoint.plan?.items ?? [],
          executionSummary: input.checkpoint.execution?.summary,
        }),
        cwd: input.project.repoPath,
      });
      await this.assignReviewersIfNeeded({
        work,
        pullRequest,
        cwd: input.project.repoPath,
      });

      this.persistPullRequestReference(input.loop, work.repo, pullRequest);

      return {
        ...input.checkpoint,
        pullRequest,
        resumePolicy: "advance_from_checkpoint",
      };
    } catch (error) {
      throw new WorkerLoopError(
        error instanceof Error ? error.message : "Failed to open pull request",
        "retryable_after_resume",
      );
    }
  }

  private async assignReviewersIfNeeded(input: {
    work: WorkerInput;
    pullRequest: {
      number?: number;
    };
    cwd: string;
  }): Promise<void> {
    if (
      (input.work.reviewers ?? []).length === 0 ||
      !input.pullRequest.number
    ) {
      return;
    }

    await this.options.github.addPullRequestReviewers({
      repo: input.work.repo,
      prNumber: input.pullRequest.number,
      reviewers: input.work.reviewers ?? [],
      cwd: input.cwd,
    });
  }

  private canAgentCreatePr(work: WorkerInput): boolean {
    return (
      work.executionMode === "create-pr" &&
      this.openPrStrategy !== "manual" &&
      this.allowAutoPush &&
      this.validationCommands.length === 0 &&
      !this.options.validationRunner
    );
  }

  private async findOpenPullRequestForBranch(input: {
    repo: string;
    branch: string;
    baseBranch: string;
    cwd: string;
  }): Promise<GitHubPullRequestSummary | null> {
    const pullRequests = await this.options.github.listOpenPullRequests({
      repo: input.repo,
      cwd: input.cwd,
      limit: WORKER_PR_DEDUPE_LOOKUP_LIMIT,
    });
    return (
      pullRequests.find(
        (pullRequest) =>
          normalizePrState(pullRequest.state) === "open" &&
          pullRequest.headRefName === input.branch &&
          pullRequest.baseRefName === input.baseBranch,
      ) ?? null
    );
  }

  private resolveWorkerInput(
    payloadJson?: string | null,
    metadataJson?: string | null,
    loop?: LoopRecord,
  ): WorkerInput {
    const payload = parseJsonObject(payloadJson);
    const metadata = parseJsonObject(metadataJson);
    const worker = asObject(metadata.worker);
    const source = { ...worker, ...payload };

    const title = readString(source.title) ?? "Worker run";
    const prompt = readString(source.prompt);
    const specPath = readString(source.specPath);
    const executionMode =
      readString(source.executionMode) === "push-existing" ||
      loop?.targetType === "pull_request"
        ? "push-existing"
        : "create-pr";
    const repo =
      readString(source.repo) ??
      (executionMode === "push-existing" ? (loop?.repo ?? null) : null);
    const baseBranch =
      readString(source.baseBranch) ??
      (executionMode === "push-existing"
        ? (readString(parseJsonObject(loop?.metadataJson).baseBranch) ?? "main")
        : null);
    if (
      executionMode === "create-pr" &&
      !prompt &&
      !specPath &&
      !readNumber(source.issueNumber)
    ) {
      throw new WorkerLoopError(
        "worker.prompt or worker.specPath is required",
        "non_retryable",
      );
    }
    if (!repo) {
      throw new WorkerLoopError("worker.repo is required", "non_retryable");
    }
    if (!baseBranch) {
      throw new WorkerLoopError(
        "worker.baseBranch is required",
        "non_retryable",
      );
    }

    return {
      title,
      repo,
      baseBranch,
      prompt,
      specPath,
      executionMode,
      issueNumber: readNumber(source.issueNumber),
      issueUrl: readString(source.issueUrl) ?? undefined,
      prNumber: readNumber(source.prNumber),
      branch: readString(source.branch) ?? undefined,
      headSha: readString(source.headSha) ?? undefined,
      reviewers: readStringArray(source.reviewers),
    };
  }

  private ensureLoopForPullRequest(input: {
    projectId: string;
    repo: string;
    prNumber: number;
  }): { record: LoopRecord; created: boolean } {
    const existing = this.options.store.loops
      .list()
      .find(
        (loop) =>
          loop.type === "worker" &&
          loop.projectId === input.projectId &&
          loop.targetType === "pull_request" &&
          loop.repo === input.repo &&
          loop.prNumber === input.prNumber,
      );
    const nowIso = this.nowIso();
    if (existing) {
      const shouldPreservePausedState = existing.status === "paused";
      const updated = {
        ...existing,
        status: shouldPreservePausedState
          ? existing.status
          : existing.status === "running"
            ? existing.status
            : "queued",
        nextRunAt: shouldPreservePausedState ? existing.nextRunAt : nowIso,
        updatedAt: nowIso,
      };
      this.options.store.loops.upsert(updated);
      return { record: updated, created: false };
    }

    const loop: LoopRecord = {
      id: randomUUID(),
      seq: this.options.store.loops.allocateSeq(),
      projectId: input.projectId,
      type: "worker",
      targetType: "pull_request",
      targetId: buildPullRequestTargetId(input.repo, input.prNumber),
      repo: input.repo,
      prNumber: input.prNumber,
      status: "queued",
      configJson: null,
      metadataJson: JSON.stringify({ executionMode: "push-existing" }),
      lastRunAt: null,
      nextRunAt: nowIso,
      createdAt: nowIso,
      updatedAt: nowIso,
    };
    this.options.store.loops.upsert(loop);
    return { record: loop, created: true };
  }

  private createRunContext(loop: LoopRecord): ResumedRunContext {
    const latestRun = this.options.store.runs.listByLoop(loop.id)[0] ?? null;
    const nowIso = this.nowIso();
    const checkpoint = parseCheckpoint(latestRun?.checkpointJson);
    const lastCompletedStep = asWorkerStep(latestRun?.lastCompletedStep);
    const startStep =
      latestRun &&
      (latestRun.status === "failed" || latestRun.status === "interrupted") &&
      lastCompletedStep
        ? (nextWorkerStep(lastCompletedStep) ?? "prepare-work")
        : "prepare-work";
    const resumed =
      Boolean(latestRun) &&
      (latestRun?.status === "failed" || latestRun?.status === "interrupted") &&
      startStep !== "prepare-work";

    const run: RunRecord = {
      id: randomUUID(),
      loopId: loop.id,
      status: "running",
      currentStep: startStep,
      lastCompletedStep: resumed ? (lastCompletedStep ?? null) : null,
      checkpointJson: JSON.stringify(
        resumed
          ? { ...checkpoint, resumePolicy: "advance_from_checkpoint" }
          : { resumePolicy: "replay_step" },
      ),
      summary: null,
      errorMessage: null,
      startedAt: nowIso,
      lastHeartbeatAt: nowIso,
      endedAt: null,
      createdAt: nowIso,
      updatedAt: nowIso,
    };
    this.options.store.runs.upsert(run);

    return {
      run,
      startStep,
      checkpoint: parseCheckpoint(run.checkpointJson),
      resumed,
    };
  }

  private persistStepStarted(
    run: RunRecord,
    step: WorkerStep,
    checkpoint: WorkerCheckpoint,
  ): RunRecord {
    const updated: RunRecord = {
      ...run,
      currentStep: step,
      checkpointJson: JSON.stringify(checkpoint),
      lastHeartbeatAt: this.nowIso(),
      updatedAt: this.nowIso(),
    };
    this.options.store.runs.upsert(updated);
    return updated;
  }

  private persistStepCompleted(
    run: RunRecord,
    step: WorkerStep,
    checkpoint: WorkerCheckpoint,
  ): RunRecord {
    const updated: RunRecord = {
      ...run,
      currentStep: nextWorkerStep(step),
      lastCompletedStep: step,
      checkpointJson: JSON.stringify(checkpoint),
      lastHeartbeatAt: this.nowIso(),
      updatedAt: this.nowIso(),
    };
    this.options.store.runs.upsert(updated);
    return updated;
  }

  private finalizeRun(
    run: RunRecord,
    input: {
      status: RunRecord["status"];
      summary: string;
      checkpoint: WorkerCheckpoint;
      errorMessage?: string;
    },
  ): RunRecord {
    const endedAt = this.nowIso();
    const updated: RunRecord = {
      ...run,
      status: input.status,
      summary: input.summary,
      errorMessage: input.errorMessage ?? null,
      checkpointJson: JSON.stringify(input.checkpoint),
      lastHeartbeatAt: endedAt,
      endedAt,
      updatedAt: endedAt,
    };
    this.options.store.runs.upsert(updated);
    return updated;
  }

  private getLatestCheckpoint(
    run: RunRecord,
    fallback: WorkerCheckpoint,
  ): WorkerCheckpoint {
    const persistedRun = this.options.store.runs.getById(run.id);

    return (
      parseCheckpoint(persistedRun?.checkpointJson ?? run.checkpointJson) ??
      fallback
    );
  }

  private buildSuccessSummary(
    loop: LoopRecord,
    checkpoint: WorkerCheckpoint,
  ): string {
    if (checkpoint.skipReason) {
      return checkpoint.skipReason;
    }
    if (checkpoint.pullRequest?.url) {
      return `Opened pull request for worker ${loop.id}: ${checkpoint.pullRequest.url}`;
    }

    return `Completed worker ${loop.id}`;
  }

  private getLoop(loopId: string): LoopRecord {
    const loop = this.options.store.loops.getById(loopId);
    if (!loop) {
      throw new Error(`Loop not found: ${loopId}`);
    }

    return loop;
  }

  private getProject(projectId: string): ProjectRecord {
    const project = this.options.store.projects.getById(projectId);
    if (!project) {
      throw new Error(`Project not found: ${projectId}`);
    }

    return project;
  }

  private updateLoop(
    loop: LoopRecord,
    updates: Partial<LoopRecord>,
  ): LoopRecord {
    const current = this.options.store.loops.getById(loop.id) ?? loop;
    const updated = {
      ...current,
      ...updates,
      updatedAt: updates.updatedAt ?? this.nowIso(),
    };
    this.options.store.loops.upsert(updated);
    return updated;
  }

  private updateLoopMetadata(
    loopId: string | null | undefined,
    updates: Record<string, unknown>,
  ): void {
    if (!loopId) {
      return;
    }

    const loop = this.options.store.loops.getById(loopId);
    if (!loop) {
      return;
    }

    const metadata = parseJsonObject(loop.metadataJson);
    this.updateLoop(loop, {
      metadataJson: JSON.stringify({ ...metadata, ...updates }),
    });
  }

  private classifyFailure(error: unknown): WorkerLoopError {
    if (error instanceof WorkerLoopError) {
      return error;
    }
    if (error instanceof ProtectedBranchError) {
      return new WorkerLoopError(error.message, "manual_intervention");
    }
    if (error instanceof CommandExecutionError) {
      return new WorkerLoopError(error.message, "retryable_transient");
    }

    return new WorkerLoopError(
      error instanceof Error ? error.message : "Worker loop failed",
      "non_retryable",
    );
  }

  private async runValidation(input: {
    cwd: string;
    commands: string[];
  }): Promise<WorkerValidationResult> {
    if (this.options.validationRunner) {
      return this.options.validationRunner(input);
    }
    if (input.commands.length === 0) {
      return {
        passed: true,
        summary: "No validation commands configured",
        output: "",
      };
    }

    const outputs: string[] = [];
    for (const command of input.commands) {
      try {
        const result = await runCommand({
          command: "/bin/sh",
          args: ["-lc", command],
          cwd: input.cwd,
        });
        outputs.push(result.stdout.trim(), result.stderr.trim());
      } catch (error) {
        const output =
          error instanceof CommandExecutionError
            ? [error.result.stdout, error.result.stderr]
                .filter(Boolean)
                .join("\n")
            : error instanceof Error
              ? error.message
              : "Unknown validation failure";
        return {
          passed: false,
          summary: `Validation failed: ${command}`,
          output,
        };
      }
    }

    return {
      passed: true,
      summary: "Validation passed",
      output: outputs.filter(Boolean).join("\n"),
    };
  }

  private appendEvent(input: {
    eventType: string;
    projectId?: string;
    loopId?: string;
    runId?: string;
    entityType: string;
    entityId: string;
    payload: Record<string, unknown>;
  }): void {
    this.options.store.events.append({
      id: randomUUID(),
      eventType: input.eventType,
      projectId: input.projectId ?? null,
      loopId: input.loopId ?? null,
      runId: input.runId ?? null,
      entityType: input.entityType,
      entityId: input.entityId,
      correlationId: null,
      causationId: null,
      actorType: "system",
      actorId: "worker-loop",
      actorDisplayName: "worker-loop",
      payloadJson: JSON.stringify(input.payload),
      createdAt: this.nowIso(),
    });
  }

  private nowIso(): string {
    return this.now().toISOString();
  }

  private persistPullRequestReference(
    loop: LoopRecord,
    repo: string,
    pullRequest: {
      number?: number;
      url?: string;
    },
  ): void {
    this.updateLoop(loop, {
      repo,
      prNumber: pullRequest.number ?? null,
    });
    this.updateLoopMetadata(loop.id, {
      prUrl: pullRequest.url ?? null,
      prNumber: pullRequest.number ?? null,
    });
  }
}

function nextWorkerStep(step: WorkerStep): WorkerStep | null {
  const index = WORKER_STEP_SEQUENCE.indexOf(step);
  return WORKER_STEP_SEQUENCE[index + 1] ?? null;
}

function asWorkerStep(value: string | null | undefined): WorkerStep | null {
  return WORKER_STEP_SEQUENCE.includes(value as WorkerStep)
    ? (value as WorkerStep)
    : null;
}

function parseCheckpoint(value?: string | null): WorkerCheckpoint {
  if (!value) {
    return {};
  }

  return parseJsonObject(value) as WorkerCheckpoint;
}

function parseJsonObject(
  value: string | null | undefined,
): Record<string, unknown> {
  if (!value) {
    return {};
  }

  try {
    const parsed = JSON.parse(value) as unknown;
    return parsed && typeof parsed === "object" && !Array.isArray(parsed)
      ? (parsed as Record<string, unknown>)
      : {};
  } catch {
    return {};
  }
}

function asObject(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function readString(value: unknown): string | null {
  return typeof value === "string" && value.trim().length > 0
    ? value.trim()
    : null;
}

function readRequiredStringValue(value: unknown, fieldName: string): string {
  const result = readString(value);
  if (!result) {
    throw new WorkerLoopError(`${fieldName} is required`, "non_retryable");
  }

  return result;
}

function requireWork(
  checkpoint: WorkerCheckpoint,
): NonNullable<WorkerCheckpoint["work"]> {
  if (!checkpoint.work) {
    throw new WorkerLoopError(
      "Missing work checkpoint for worker step",
      "retryable_transient",
    );
  }

  return checkpoint.work;
}

function requireWorktree(
  checkpoint: WorkerCheckpoint,
): NonNullable<WorkerCheckpoint["worktree"]> {
  if (!checkpoint.worktree) {
    throw new WorkerLoopError(
      "Missing worktree checkpoint for worker step",
      "retryable_transient",
    );
  }

  return checkpoint.worktree;
}

function slugify(value: string): string {
  return value
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

async function buildWorkerPrompt(input: {
  repoRootPath: string;
  work: WorkerInput;
  plan: string[];
  allowAgentPrCreation: boolean;
}): Promise<string> {
  const specBlock = await readSpecBlock(
    input.repoRootPath,
    input.work.specPath,
  );
  return appendCompletionInstruction(
    [
      input.work.executionMode === "push-existing"
        ? `Continue implementing on existing pull request ${input.work.repo}#${input.work.prNumber}.`
        : `Create a pull request for: ${input.work.title}`,
      input.work.prompt ? `User prompt:\n${input.work.prompt}` : null,
      `Repository: ${input.work.repo}`,
      `Base branch: ${input.work.baseBranch}`,
      input.work.executionMode === "push-existing" && input.work.specPath
        ? `Do not modify the spec file at ${input.work.specPath}.`
        : null,
      specBlock,
      input.plan.length > 0
        ? ["Execution plan:", ...input.plan.map((item) => `- ${item}`)].join(
            "\n",
          )
        : null,
      input.allowAgentPrCreation
        ? buildAgentPullRequestInstruction(input.work)
        : null,
      input.allowAgentPrCreation
        ? "Make the necessary code changes, validate them, and ensure the branch and pull request are left in a consistent state."
        : "Make the necessary code changes, validate them, and leave the branch ready for PR creation.",
    ]
      .filter((value): value is string => Boolean(value))
      .join("\n\n"),
  );
}

function buildAgentPullRequestInstruction(work: WorkerInput): string {
  return [
    "When the implementation is ready and validation passes, use the GitHub CLI (`gh`) to create the pull request yourself.",
    "Before creating a PR, check whether one already exists for the current branch and avoid duplicates.",
    "Write a concise, accurate PR title and a structured body that explains the actual changes and why they were made.",
    work.issueNumber
      ? `Include \`Closes #${work.issueNumber}\` in the PR body.`
      : null,
    `Target base branch: ${work.baseBranch}.`,
  ]
    .filter((value): value is string => Boolean(value))
    .join("\n");
}

async function readSpecBlock(
  projectRepoPath: string,
  specPath: string | null | undefined,
): Promise<string | null> {
  if (!specPath) {
    return null;
  }

  const resolved = isAbsolute(specPath)
    ? specPath
    : join(projectRepoPath, specPath);
  try {
    const content = await readFile(resolved, "utf8");
    return `Spec (${specPath}):\n${content}`;
  } catch {
    return `Spec path: ${specPath}`;
  }
}

function buildPullRequestBody(input: {
  work: WorkerInput;
  plan: string[];
  executionSummary?: string;
}): string {
  return [
    "## Summary",
    ...input.plan.map((item) => `- ${item}`),
    input.executionSummary
      ? `\n## Agent Summary\n${input.executionSummary}`
      : null,
    input.work.issueNumber
      ? `\nIssue: ${input.work.repo}#${input.work.issueNumber}`
      : null,
    input.work.issueUrl ? `Issue URL: ${input.work.issueUrl}` : null,
    input.work.specPath ? `\nSpec: ${input.work.specPath}` : null,
    input.work.prompt ? `\nPrompt: ${input.work.prompt}` : null,
    input.work.issueNumber ? `\nCloses #${input.work.issueNumber}` : null,
  ]
    .filter((value): value is string => Boolean(value))
    .join("\n");
}

function hydrateWorkerInputFromIssue(
  work: WorkerInput,
  issue: GitHubIssueDetail,
): WorkerInput {
  const fallbackTitle = buildDefaultIssueWorkerTitle(work.repo, issue.number);
  return {
    ...work,
    title:
      work.title === "Worker run" || work.title === fallbackTitle
        ? issue.title
        : work.title,
    prompt: buildIssuePrompt(work.repo, issue),
    issueUrl: issue.url,
  };
}

function buildIssuePrompt(repo: string, issue: GitHubIssueDetail): string {
  return [
    `Implement GitHub issue ${repo}#${issue.number}: ${issue.title}`,
    issue.body ? `Issue body:\n${issue.body}` : null,
    issue.url ? `Issue URL: ${issue.url}` : null,
  ]
    .filter((value): value is string => Boolean(value))
    .join("\n\n");
}

function buildWorkerBranchName(work: WorkerInput, loopId: string): string {
  if (work.issueNumber) {
    return `looper/worker/${work.issueNumber}-${buildWorkerSlug(work.title)}-${slugify(loopId)}`;
  }

  return `looper/worker/${slugify(loopId)}`;
}

function buildWorkerSlug(title: string): string {
  const words = slugify(title).split("-").filter(Boolean).slice(0, 8).join("-");
  return (
    words.slice(0, WORKER_BRANCH_SLUG_MAX_LENGTH).replace(/-+$/g, "") ||
    "update"
  );
}

function buildDefaultIssueWorkerTitle(
  repo: string,
  issueNumber: number,
): string {
  return `Implement ${repo}#${issueNumber}`;
}

function buildPullRequestTargetId(repo: string, prNumber: number): string {
  return `pr:${repo}:${prNumber}`;
}

function buildWorkerPullRequestDedupeKey(
  projectId: string,
  repo: string,
  prNumber: number,
): string {
  return `worker:${projectId}:${repo}:${prNumber}`;
}

function buildWorkerPullRequestLockKey(repo: string, prNumber: number): string {
  return createPrLockKey(repo, prNumber);
}

function normalizePrState(value: string | undefined): "open" | "other" {
  return value?.toLowerCase() === "open" ? "open" : "other";
}

function readNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isInteger(value)
    ? value
    : undefined;
}

function readStringArray(value: unknown): string[] {
  return Array.isArray(value)
    ? value
        .map((item) => readString(item))
        .filter((item): item is string => Boolean(item))
    : [];
}
