import { createHash, randomUUID } from "node:crypto";
import { join } from "node:path";

import type { Logger } from "../bootstrap/logger";
import { getDefaultProjectWorktreeRoot } from "../config/index";
import type { AgentResult, AgentRunInput } from "../infra/agent";
import { appendCompletionInstruction } from "../infra/agent-prompt";
import { CommandExecutionError, runCommand } from "../infra/command";
import { RemoteHeadChangedError } from "../infra/git";
import type {
  GitHubPullRequestDetail,
  GitHubPullRequestSummary,
} from "../infra/github";
import {
  SPEC_READY_LABEL,
  SPEC_REVIEWING_LABEL,
  hasLabel,
  isSpecReviewClean,
} from "../infra/spec-pr";
import type { SchedulerQueue } from "../scheduler/index";
import type { Store } from "../storage/store";
import type {
  LoopRecord,
  ProjectRecord,
  QueueFailureKind,
  QueueItemRecord,
  RunRecord,
} from "../storage/types";

const FIXER_STEP_SEQUENCE = [
  "discover-pr",
  "claim-pr",
  "collect-fixes",
  "prepare-worktree",
  "repair",
  "reconcile-commits",
  "validate",
  "push",
  "resolve-comments",
  "recheck",
] as const;

export type FixerStep = (typeof FIXER_STEP_SEQUENCE)[number];

export type FixItem =
  | { type: "comment"; id: string; threadId: string; summary: string }
  | { type: "check"; name: string; summary: string }
  | { type: "conflict"; files: string[] };

export interface FixerGitHubGateway {
  listOpenPullRequests(input: {
    repo: string;
    cwd?: string;
    limit?: number;
  }): Promise<GitHubPullRequestSummary[]>;
  viewPullRequest(input: {
    repo: string;
    prNumber: number;
    cwd?: string;
  }): Promise<GitHubPullRequestDetail>;
  resolveReviewThread(input: {
    repo: string;
    threadId: string;
    cwd?: string;
  }): Promise<void>;
  addPullRequestLabels(input: {
    repo: string;
    prNumber: number;
    labels: string[];
    cwd?: string;
  }): Promise<void>;
  removePullRequestLabels(input: {
    repo: string;
    prNumber: number;
    labels: string[];
    cwd?: string;
  }): Promise<void>;
}

export interface FixerGitGateway {
  createWorktree(input: {
    projectId: string;
    repoPath: string;
    worktreeRoot: string;
    branch: string;
    baseBranch: string;
    prNumber: number;
    protectedBranches?: string[];
    checkoutMode?: "branch" | "detached";
  }): Promise<{
    worktreePath: string;
    branch: string;
    headSha?: string | null;
  }>;
  prepareWorktree(input: {
    worktreePath: string;
    branch: string;
    expectedHeadSha?: string;
    remote?: string;
  }): Promise<{
    headSha?: string;
    clean: boolean;
  }>;
  inspectHead(input: { worktreePath: string; baseRef?: string }): Promise<{
    headSha?: string;
    newCommitShas: string[];
    hasUncommittedChanges: boolean;
    changedFiles: string[];
  }>;
  commit(input: {
    worktreePath: string;
    message: string;
  }): Promise<{ commitSha: string }>;
  push(input: {
    worktreePath: string;
    branch: string;
    remote?: string;
    expectedRemoteHeadSha?: string;
    protectedBranches?: string[];
  }): Promise<void>;
  cleanupWorktree(input: {
    projectId: string;
    repoPath: string;
    worktreePath: string;
    branch: string;
    protectedBranches?: string[];
  }): Promise<void>;
}

export interface FixerAgentExecution {
  wait(): Promise<AgentResult>;
}

export interface FixerAgentExecutor {
  start(input: AgentRunInput): Promise<FixerAgentExecution>;
}

export interface FixerValidationResult {
  passed: boolean;
  summary?: string;
  output?: string;
}

export interface FixerLoopRunnerOptions {
  store: Store;
  scheduler: SchedulerQueue;
  github: FixerGitHubGateway;
  git: FixerGitGateway;
  agentExecutor: FixerAgentExecutor;
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
  }) => Promise<FixerValidationResult>;
  allowAutoCommit?: boolean;
  allowAutoPush?: boolean;
  allowRiskyFixes?: boolean;
  sleep?: (ms: number) => Promise<void>;
}

export interface FixerDiscoveryResult {
  queueItems: QueueItemRecord[];
  createdLoopIds: string[];
  skipped: number;
}

export interface FixerProcessResult {
  loopId: string;
  runId: string;
  queueItemId: string;
  status: "success" | "skipped" | "failed";
  summary: string;
  failureKind?: QueueFailureKind;
}

interface FixerCheckpoint {
  resumePolicy?:
    | "replay_step"
    | "advance_from_checkpoint"
    | "manual_intervention";
  detail?: {
    state?: string;
    isDraft?: boolean;
    labels?: string[];
    headSha?: string;
    headRefName?: string;
    baseRefName?: string;
    baseSha?: string;
    reviewDecision?: string;
    comments?: unknown[];
    checks?: unknown[];
    hasConflicts?: boolean;
  };
  claimedLockKey?: string;
  fixItems?: FixItem[];
  fixItemsHash?: string;
  worktree?: {
    path?: string;
    branch?: string;
    headSha?: string;
    baseHeadSha?: string;
    preparedAt?: string;
    cleanupAttemptedAt?: string;
    cleanedAt?: string;
  };
  repair?: {
    agentExecutionId?: string;
    summary?: string;
    headSha?: string;
    parseStatus?: "parsed" | "missing" | "invalid_json";
    completedAt?: string;
  };
  reconcileCommits?: {
    baseHeadSha?: string;
    finalHeadSha?: string;
    newCommitShas: string[];
    committedByAgent: boolean;
    committedByLooperd: boolean;
    workingTreeClean: boolean;
    changedFiles?: string[];
    completedAt?: string;
  };
  validation?: FixerValidationResult;
  push?: {
    pushed: boolean;
    branch: string;
    remote?: string;
    pushedAt?: string;
    skippedReason?: string;
  };
  resolvedComments?: {
    items: Array<{
      fixItemId: string;
      threadId?: string;
      status: "resolved" | "already_resolved" | "failed";
      message?: string;
      updatedAt: string;
    }>;
  };
  recheck?: {
    remainingFixItems: FixItem[];
  };
  skipReason?: string;
}

interface ResumedRunContext {
  run: RunRecord;
  startStep: FixerStep;
  checkpoint: FixerCheckpoint;
  resumed: boolean;
}

class FixerLoopError extends Error {
  constructor(
    message: string,
    public readonly kind: QueueFailureKind,
  ) {
    super(message);
    this.name = "FixerLoopError";
  }
}

export class FixerLoopRunner {
  private readonly now: () => Date;
  private readonly agentTimeoutMs: number;
  private readonly claimTtlMs: number;
  private readonly validationCommands: string[];
  private readonly allowAutoCommit: boolean;
  private readonly allowAutoPush: boolean;
  private readonly allowRiskyFixes: boolean;
  private readonly sleep: (ms: number) => Promise<void>;

  constructor(private readonly options: FixerLoopRunnerOptions) {
    this.now = options.now ?? (() => new Date());
    this.agentTimeoutMs = options.agentTimeoutMs ?? 30 * 60_000;
    this.claimTtlMs = options.claimTtlMs ?? 5 * 60_000;
    this.validationCommands = options.validationCommands ?? [];
    this.allowAutoCommit = options.allowAutoCommit ?? true;
    this.allowAutoPush = options.allowAutoPush ?? true;
    this.allowRiskyFixes = options.allowRiskyFixes ?? false;
    this.sleep =
      options.sleep ??
      ((ms) => new Promise((resolve) => setTimeout(resolve, ms)));
  }

  public async discoverPullRequests(input: {
    projectId: string;
    repo: string;
    limit?: number;
  }): Promise<FixerDiscoveryResult> {
    const project = this.getProject(input.projectId);
    const openPullRequests = await this.options.github.listOpenPullRequests({
      repo: input.repo,
      cwd: project.repoPath,
      limit: input.limit,
    });

    const queueItems: QueueItemRecord[] = [];
    const createdLoopIds: string[] = [];
    let skipped = 0;

    for (const pullRequest of openPullRequests) {
      if (
        pullRequest.isDraft ||
        normalizePrState(pullRequest.state) !== "open" ||
        this.hasActivePrLock(input.repo, pullRequest.number)
      ) {
        skipped += 1;
        continue;
      }

      const detail = await this.options.github.viewPullRequest({
        repo: input.repo,
        prNumber: pullRequest.number,
        cwd: project.repoPath,
      });
      const fixItems = collectFixItems(detail);
      if (fixItems.length === 0) {
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

      const headSha = detail.headSha ?? "unknown";
      const fixItemsHash = hashFixItems(fixItems);
      queueItems.push(
        this.options.scheduler.enqueue({
          projectId: project.id,
          loopId: loop.record.id,
          type: "fixer",
          targetType: "pull_request",
          targetId: buildPullRequestTargetId(input.repo, pullRequest.number),
          repo: input.repo,
          prNumber: pullRequest.number,
          dedupeKey: buildFixerDedupeKey(
            input.repo,
            pullRequest.number,
            headSha,
            fixItemsHash,
          ),
        }),
      );
    }

    return { queueItems, createdLoopIds, skipped };
  }

  public async processNext(
    claimedBy: string,
  ): Promise<FixerProcessResult | null> {
    const item = this.options.scheduler.claimNextOfType(claimedBy, "fixer");
    if (!item) {
      return null;
    }

    return this.processClaimedItem(item);
  }

  public async processClaimedItem(
    queueItem: QueueItemRecord,
  ): Promise<FixerProcessResult> {
    if (queueItem.type !== "fixer") {
      throw new Error(`Unsupported queue item type: ${queueItem.type}`);
    }
    if (!queueItem.loopId || !queueItem.repo || !queueItem.prNumber) {
      throw new Error("Fixer queue item requires loopId, repo, and prNumber");
    }

    let loop: LoopRecord | undefined;
    let project: ProjectRecord | undefined;
    let run: RunRecord | undefined;
    let checkpoint: FixerCheckpoint | undefined;
    let claimedLockKey: string | undefined;

    try {
      loop = this.getLoop(queueItem.loopId);
      project = this.getProject(loop.projectId);
      const resumedRun = this.createRunContext(loop);
      run = resumedRun.run;
      checkpoint = resumedRun.checkpoint;

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
      this.options.logger.info("fixer loop started", {
        projectId: project.id,
        loopId: loop.id,
        runId: run.id,
        queueItemId: queueItem.id,
        currentStep: resumedRun.startStep,
        resumed: resumedRun.resumed,
      });
      this.appendEvent({
        eventType: "run.started",
        projectId: project.id,
        loopId: loop.id,
        runId: run.id,
        entityType: "run",
        entityId: run.id,
        payload: {
          queueItemId: queueItem.id,
          currentStep: resumedRun.startStep,
        },
      });
      this.options.logger.info("fixer run started", {
        projectId: project.id,
        loopId: loop.id,
        runId: run.id,
        queueItemId: queueItem.id,
        currentStep: resumedRun.startStep,
      });

      for (const step of FIXER_STEP_SEQUENCE.slice(
        FIXER_STEP_SEQUENCE.indexOf(resumedRun.startStep),
      )) {
        run = this.persistStepStarted(run, step, checkpoint);
        this.appendEvent({
          eventType: "loop.step.started",
          projectId: project.id,
          loopId: loop.id,
          runId: run.id,
          entityType: "run",
          entityId: run.id,
          payload: { step },
        });
        this.options.logger.info("fixer step started", {
          projectId: project.id,
          loopId: loop.id,
          runId: run.id,
          queueItemId: queueItem.id,
          currentStep: step,
        });
        checkpoint = await this.executeStep({
          step,
          checkpoint,
          project,
          loop,
          run,
          queueItem,
        });

        if (step === "claim-pr") {
          claimedLockKey = checkpoint.claimedLockKey;
        }

        run = this.persistStepCompleted(run, step, checkpoint);
        this.appendEvent({
          eventType: "loop.step.completed",
          projectId: project.id,
          loopId: loop.id,
          runId: run.id,
          entityType: "run",
          entityId: run.id,
          payload: { step },
        });
        this.options.logger.info("fixer step completed", {
          projectId: project.id,
          loopId: loop.id,
          runId: run.id,
          queueItemId: queueItem.id,
          currentStep: step,
        });
        if (checkpoint.skipReason) {
          break;
        }
      }

      const summary = checkpoint.skipReason
        ? checkpoint.skipReason
        : `Applied fixer run for ${queueItem.repo}#${queueItem.prNumber}`;
      this.finalizeRun(run, {
        status: "success",
        summary,
        checkpoint,
      });
      this.appendEvent({
        eventType: "run.completed",
        projectId: project.id,
        loopId: loop.id,
        runId: run.id,
        entityType: "run",
        entityId: run.id,
        payload: { summary },
      });
      this.options.logger.info(
        checkpoint.skipReason ? "fixer run skipped" : "fixer run completed",
        {
          projectId: project.id,
          loopId: loop.id,
          runId: run.id,
          queueItemId: queueItem.id,
          currentStep: run.currentStep,
          summary,
        },
      );
      this.options.scheduler.complete(queueItem.id);
      this.updateLoop(loop, {
        status: "completed",
        lastRunAt: this.nowIso(),
        nextRunAt: null,
      });
      await this.cleanupFixerWorktreeIfTerminal({
        checkpoint,
        project,
      });

      return {
        loopId: loop.id,
        runId: run.id,
        queueItemId: queueItem.id,
        status: checkpoint.skipReason ? "skipped" : "success",
        summary,
      };
    } catch (error) {
      const failure = this.classifyFailure(error);
      if (!loop || !project || !run || !checkpoint) {
        this.options.scheduler.fail(
          queueItem.id,
          failure.kind,
          failure.message,
        );
        throw error;
      }
      this.appendEvent({
        eventType: "loop.step.failed",
        projectId: project.id,
        loopId: loop.id,
        runId: run.id,
        entityType: "run",
        entityId: run.id,
        payload: {
          message: failure.message,
          failureKind: failure.kind,
          currentStep: run.currentStep,
        },
      });
      this.finalizeRun(run, {
        status: "failed",
        summary: failure.message,
        checkpoint: {
          ...checkpoint,
          resumePolicy:
            failure.kind === "retryable_after_resume"
              ? "advance_from_checkpoint"
              : failure.kind === "manual_intervention"
                ? "manual_intervention"
                : (checkpoint.resumePolicy ?? "replay_step"),
        },
        errorMessage: failure.message,
      });
      this.appendEvent({
        eventType: "run.failed",
        projectId: project.id,
        loopId: loop.id,
        runId: run.id,
        entityType: "run",
        entityId: run.id,
        payload: {
          summary: failure.message,
          failureKind: failure.kind,
        },
      });
      this.options.logger.error("fixer run failed", {
        projectId: project.id,
        loopId: loop.id,
        runId: run.id,
        queueItemId: queueItem.id,
        currentStep: run.currentStep,
        failureKind: failure.kind,
        summary: failure.message,
      });

      const failedQueueItem = this.options.scheduler.fail(
        queueItem.id,
        failure.kind,
        failure.message,
      );

      if (failedQueueItem?.status === "queued") {
        this.updateLoop(loop, {
          status: "queued",
          lastRunAt: this.nowIso(),
          nextRunAt: failedQueueItem.availableAt,
        });
      } else {
        this.updateLoop(loop, {
          status:
            failedQueueItem?.status === "cancelled"
              ? "paused"
              : failure.kind === "manual_intervention"
                ? "paused"
                : "failed",
          lastRunAt: this.nowIso(),
          nextRunAt: null,
        });
        await this.cleanupFixerWorktreeIfTerminal({
          checkpoint,
          project,
        });
      }

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

  private async executeStep(input: {
    step: FixerStep;
    checkpoint: FixerCheckpoint;
    project: ProjectRecord;
    loop: LoopRecord;
    run: RunRecord;
    queueItem: QueueItemRecord;
  }): Promise<FixerCheckpoint> {
    switch (input.step) {
      case "discover-pr":
        return this.runDiscoverPrStep(input);
      case "claim-pr":
        return this.runClaimPrStep(input);
      case "collect-fixes":
        return this.runCollectFixesStep(input);
      case "prepare-worktree":
        return this.runPrepareWorktreeStep(input);
      case "repair":
        return this.runRepairStep(input);
      case "reconcile-commits":
        return this.runReconcileCommitsStep(input);
      case "validate":
        return this.runValidateStep(input);
      case "push":
        return this.runPushStep(input);
      case "resolve-comments":
        return this.runResolveCommentsStep(input);
      case "recheck":
        return this.runRecheckStep(input);
    }
  }

  private async runDiscoverPrStep(input: {
    checkpoint: FixerCheckpoint;
    project: ProjectRecord;
    queueItem: QueueItemRecord;
  }): Promise<FixerCheckpoint> {
    const repo = requireString(input.queueItem.repo, "queueItem.repo");
    const prNumber = requireNumber(
      input.queueItem.prNumber,
      "queueItem.prNumber",
    );
    const detail = await this.options.github.viewPullRequest({
      repo,
      prNumber,
      cwd: input.project.repoPath,
    });

    return {
      ...input.checkpoint,
      detail: {
        state: detail.state,
        isDraft: detail.isDraft,
        labels: detail.labels,
        headSha: detail.headSha,
        headRefName: detail.headRefName,
        baseRefName: detail.baseRefName,
        baseSha: detail.baseSha,
        reviewDecision: detail.reviewDecision,
        comments: detail.comments,
        checks: detail.checks,
        hasConflicts: readBoolean(
          (detail as unknown as Record<string, unknown>).hasConflicts,
        ),
      },
      resumePolicy: "replay_step",
    };
  }

  private async runClaimPrStep(input: {
    checkpoint: FixerCheckpoint;
    queueItem: QueueItemRecord;
  }): Promise<FixerCheckpoint> {
    const lockKey =
      input.queueItem.lockKey ??
      `pr:${input.queueItem.repo}:${input.queueItem.prNumber}`;
    const acquired = this.options.scheduler.acquireBusinessLock({
      key: lockKey,
      owner: input.queueItem.id,
      reason: "fixer-claim",
      expiresAt: new Date(this.now().getTime() + this.claimTtlMs).toISOString(),
    });
    if (!acquired) {
      throw new FixerLoopError(
        `Pull request lock is already held for ${lockKey}`,
        "retryable_transient",
      );
    }

    return {
      ...input.checkpoint,
      claimedLockKey: lockKey,
    };
  }

  private async runCollectFixesStep(input: {
    checkpoint: FixerCheckpoint;
    queueItem: QueueItemRecord;
  }): Promise<FixerCheckpoint> {
    const detail = input.checkpoint.detail;
    if (!detail) {
      throw new FixerLoopError(
        "Missing PR detail checkpoint for collect-fixes step",
        "retryable_transient",
      );
    }

    if (detail.isDraft || normalizePrState(detail.state) !== "open") {
      return {
        ...input.checkpoint,
        skipReason: `Skipped pull request ${input.queueItem.repo}#${input.queueItem.prNumber} because it is not eligible`,
      };
    }

    const fixItems = collectFixItemsFromCheckpoint(input.checkpoint);
    if (fixItems.length === 0) {
      return {
        ...input.checkpoint,
        fixItems,
        fixItemsHash: hashFixItems(fixItems),
        skipReason: `Skipped ${input.queueItem.repo}#${input.queueItem.prNumber} because no fix items remain`,
      };
    }

    return {
      ...input.checkpoint,
      fixItems,
      fixItemsHash: hashFixItems(fixItems),
      resumePolicy: "advance_from_checkpoint",
      skipReason: undefined,
    };
  }

  private async runPrepareWorktreeStep(input: {
    checkpoint: FixerCheckpoint;
    project: ProjectRecord;
    queueItem: QueueItemRecord;
  }): Promise<FixerCheckpoint> {
    if (input.checkpoint.skipReason) {
      return input.checkpoint;
    }
    if (input.checkpoint.worktree?.preparedAt) {
      return input.checkpoint;
    }

    const detail = input.checkpoint.detail;
    const branch = requireString(detail?.headRefName, "detail.headRefName");
    const prNumber = requireNumber(
      input.queueItem.prNumber,
      "queueItem.prNumber",
    );
    const projectMetadata = parseJsonObject(input.project.metadataJson);
    const worktreeRoot =
      readString(projectMetadata.worktreeRoot) ??
      getDefaultProjectWorktreeRoot(input.project.id, input.project.repoPath);
    if (shouldRebuildWorktree(input.checkpoint)) {
      const previousWorktree = input.checkpoint.worktree;
      if (previousWorktree?.path && previousWorktree.branch) {
        await this.options.git.cleanupWorktree({
          projectId: input.project.id,
          repoPath: input.project.repoPath,
          worktreePath: previousWorktree.path,
          branch: previousWorktree.branch,
          protectedBranches: compactStrings([
            detail?.baseRefName,
            input.project.baseBranch,
          ]),
        });
      }
    }
    const worktree = await this.options.git.createWorktree({
      projectId: input.project.id,
      repoPath: input.project.repoPath,
      worktreeRoot,
      branch,
      baseBranch: detail?.baseRefName ?? input.project.baseBranch ?? "main",
      prNumber,
      protectedBranches: compactStrings([
        detail?.baseRefName,
        input.project.baseBranch,
      ]),
      checkoutMode: "detached",
    });
    const prepared = await this.options.git.prepareWorktree({
      worktreePath: worktree.worktreePath,
      branch,
      expectedHeadSha: detail?.headSha,
    });
    if (!prepared.clean) {
      throw new FixerLoopError(
        `Fixer worktree is dirty for branch ${branch}; manual intervention required`,
        "manual_intervention",
      );
    }

    const preparedAt = this.nowIso();
    this.appendEvent({
      eventType: "fixer.worktree.prepared",
      projectId: input.project.id,
      entityType: "pull_request",
      entityId: buildPullRequestTargetId(
        requireString(input.queueItem.repo, "queueItem.repo"),
        prNumber,
      ),
      payload: {
        branch,
        path: worktree.worktreePath,
        headSha: prepared.headSha ?? null,
        preparedAt,
      },
    });

    return {
      ...input.checkpoint,
      worktree: {
        path: worktree.worktreePath,
        branch,
        headSha: prepared.headSha,
        baseHeadSha: prepared.headSha,
        preparedAt,
      },
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runRepairStep(input: {
    checkpoint: FixerCheckpoint;
    loop: LoopRecord;
    run: RunRecord;
    queueItem: QueueItemRecord;
  }): Promise<FixerCheckpoint> {
    if (input.checkpoint.skipReason) {
      return input.checkpoint;
    }
    if (input.checkpoint.repair) {
      return input.checkpoint;
    }

    const fixItems = input.checkpoint.fixItems;
    if (!fixItems || fixItems.length === 0) {
      throw new FixerLoopError(
        "Missing fix items checkpoint for repair step",
        "retryable_transient",
      );
    }

    if (
      !this.allowRiskyFixes &&
      fixItems.some((item) => item.type === "conflict")
    ) {
      return {
        ...input.checkpoint,
        skipReason: `Skipped ${input.queueItem.repo}#${input.queueItem.prNumber} because risky conflict fixes require manual intervention`,
        resumePolicy: "manual_intervention",
      };
    }

    const detail = input.checkpoint.detail;
    const worktree = requireWorktree(input.checkpoint);
    const executionId = randomUUID();
    const prompt = buildFixerPrompt({
      repo: requireString(input.queueItem.repo, "queueItem.repo"),
      prNumber: requireNumber(input.queueItem.prNumber, "queueItem.prNumber"),
      headSha: detail?.headSha,
      fixItems,
    });
    const execution = await this.options.agentExecutor.start({
      executionId,
      projectId: input.loop.projectId,
      loopId: input.loop.id,
      runId: input.run.id,
      prompt,
      workingDirectory: requireString(worktree.path, "worktree.path"),
      timeoutMs: this.agentTimeoutMs,
      metadata: {
        loopType: "fixer",
        repo: input.queueItem.repo,
        prNumber: input.queueItem.prNumber,
        step: "repair",
      },
      idempotencyKey: `fixer:${input.loop.id}:${input.checkpoint.fixItemsHash ?? "unknown"}:${detail?.headSha ?? "unknown"}`,
    });
    await this.options.onAgentExecutionStarted?.({
      executionId,
      projectId: input.loop.projectId,
      loopId: input.loop.id,
      runId: input.run.id,
      subtitle: `${requireString(input.queueItem.repo, "queueItem.repo")}#${requireNumber(input.queueItem.prNumber, "queueItem.prNumber")}`,
      body: "Fix started",
      dedupeKey: `runtime.agent.started:fixer:${input.run.id}`,
    });
    const result = await execution.wait();
    if (result.status !== "completed") {
      throw new FixerLoopError(
        result.summary ?? `Fixer agent ${result.status}`,
        "retryable_transient",
      );
    }

    return {
      ...input.checkpoint,
      repair: {
        agentExecutionId: executionId,
        summary: result.summary,
        headSha: detail?.headSha,
        parseStatus: result.parseStatus,
        completedAt: this.nowIso(),
      },
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runReconcileCommitsStep(input: {
    checkpoint: FixerCheckpoint;
    project: ProjectRecord;
    queueItem: QueueItemRecord;
  }): Promise<FixerCheckpoint> {
    if (input.checkpoint.skipReason) {
      return input.checkpoint;
    }
    if (input.checkpoint.reconcileCommits?.completedAt) {
      return input.checkpoint;
    }

    const checkpoint = await this.reconcileCommits({
      checkpoint: input.checkpoint,
      commitMessage: buildFixerCommitMessage(
        requireNumber(input.queueItem.prNumber, "queueItem.prNumber"),
      ),
    });
    this.appendEvent({
      eventType: "fixer.commits.reconciled",
      projectId: input.project.id,
      entityType: "pull_request",
      entityId: buildPullRequestTargetId(
        requireString(input.queueItem.repo, "queueItem.repo"),
        requireNumber(input.queueItem.prNumber, "queueItem.prNumber"),
      ),
      payload: checkpoint.reconcileCommits,
    });
    return checkpoint;
  }

  private async runValidateStep(input: {
    checkpoint: FixerCheckpoint;
    queueItem: QueueItemRecord;
  }): Promise<FixerCheckpoint> {
    if (input.checkpoint.skipReason) {
      return input.checkpoint;
    }

    const result = await this.runValidation({
      cwd: requireString(
        requireWorktree(input.checkpoint).path,
        "worktree.path",
      ),
      commands: this.validationCommands,
    });
    if (!result.passed) {
      throw new FixerLoopError(
        result.summary ?? "Validation failed",
        "retryable_after_resume",
      );
    }

    const inspect = await this.options.git.inspectHead({
      worktreePath: requireString(
        requireWorktree(input.checkpoint).path,
        "worktree.path",
      ),
      baseRef: input.checkpoint.reconcileCommits?.baseHeadSha,
    });
    if (inspect.hasUncommittedChanges) {
      if (input.checkpoint.validation?.summary?.includes("extra reconcile")) {
        throw new FixerLoopError(
          "Validation keeps producing new modifications after an extra reconcile pass",
          "retryable_after_resume",
        );
      }

      const reconciled = await this.reconcileCommits({
        checkpoint: input.checkpoint,
        commitMessage: buildFixerCommitMessage(
          requireNumber(input.queueItem.prNumber, "queueItem.prNumber"),
        ),
      });
      const secondResult = await this.runValidation({
        cwd: requireString(requireWorktree(reconciled).path, "worktree.path"),
        commands: this.validationCommands,
      });
      if (!secondResult.passed) {
        throw new FixerLoopError(
          secondResult.summary ?? "Validation failed after reconcile",
          "retryable_after_resume",
        );
      }
      const finalInspect = await this.options.git.inspectHead({
        worktreePath: requireString(
          requireWorktree(reconciled).path,
          "worktree.path",
        ),
        baseRef: reconciled.reconcileCommits?.baseHeadSha,
      });
      if (finalInspect.hasUncommittedChanges) {
        throw new FixerLoopError(
          "Validation keeps producing new modifications after an extra reconcile pass",
          "retryable_after_resume",
        );
      }

      return {
        ...reconciled,
        validation: {
          ...secondResult,
          summary:
            secondResult.summary ?? "Validation passed after extra reconcile",
          output: secondResult.output,
        },
        resumePolicy: "advance_from_checkpoint",
      };
    }

    return {
      ...input.checkpoint,
      validation: result,
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runPushStep(input: {
    checkpoint: FixerCheckpoint;
    project: ProjectRecord;
    loop: LoopRecord;
    queueItem: QueueItemRecord;
  }): Promise<FixerCheckpoint> {
    if (input.checkpoint.skipReason) {
      return input.checkpoint;
    }
    if (input.checkpoint.push?.pushed) {
      return input.checkpoint;
    }

    const worktree = requireWorktree(input.checkpoint);
    const branch = worktree.branch ?? input.checkpoint.detail?.headRefName;
    if (!branch) {
      throw new FixerLoopError(
        "Missing PR head branch for push step",
        "retryable_after_resume",
      );
    }

    if (!this.allowAutoPush) {
      this.appendEvent({
        eventType: "fixer.push.skipped",
        projectId: input.project.id,
        loopId: input.loop.id,
        entityType: "pull_request",
        entityId: buildPullRequestTargetId(
          requireString(input.queueItem.repo, "queueItem.repo"),
          requireNumber(input.queueItem.prNumber, "queueItem.prNumber"),
        ),
        payload: {
          branch,
          reason: "auto_push_disabled",
        },
      });
      return {
        ...input.checkpoint,
        skipReason: `Auto push disabled; manual fix push required for branch ${branch}`,
        resumePolicy: "manual_intervention",
      };
    }

    const reconcile = input.checkpoint.reconcileCommits;
    if (!reconcile) {
      throw new FixerLoopError(
        "Missing reconcile-commits checkpoint for push step",
        "retryable_after_resume",
      );
    }
    if (!reconcile.workingTreeClean) {
      throw new FixerLoopError(
        "Working tree must be clean before push",
        "retryable_after_resume",
      );
    }

    if (
      reconcile.finalHeadSha &&
      reconcile.finalHeadSha === input.checkpoint.worktree?.baseHeadSha
    ) {
      this.appendEvent({
        eventType: "fixer.push.skipped",
        projectId: input.project.id,
        loopId: input.loop.id,
        entityType: "pull_request",
        entityId: buildPullRequestTargetId(
          requireString(input.queueItem.repo, "queueItem.repo"),
          requireNumber(input.queueItem.prNumber, "queueItem.prNumber"),
        ),
        payload: {
          branch,
          reason: "no_new_commits",
        },
      });
      return {
        ...input.checkpoint,
        push: {
          pushed: false,
          branch,
          remote: "origin",
          skippedReason: "No new commits to push",
        },
        resumePolicy: "advance_from_checkpoint",
      };
    }

    try {
      await this.options.git.push({
        worktreePath: requireString(worktree.path, "worktree.path"),
        branch,
        expectedRemoteHeadSha: input.checkpoint.worktree?.baseHeadSha,
      });
    } catch (error) {
      const message =
        error instanceof Error ? error.message : "Failed to push fixer updates";
      this.appendEvent({
        eventType: message.toLowerCase().includes("remote head changed")
          ? "fixer.push.conflicted"
          : "fixer.push.retryable",
        projectId: input.project.id,
        loopId: input.loop.id,
        entityType: "pull_request",
        entityId: buildPullRequestTargetId(
          requireString(input.queueItem.repo, "queueItem.repo"),
          requireNumber(input.queueItem.prNumber, "queueItem.prNumber"),
        ),
        payload: {
          branch,
          message,
        },
      });
      throw new FixerLoopError(message, "retryable_after_resume");
    }

    const finalHeadSha = requireString(
      reconcile.finalHeadSha,
      "reconcileCommits.finalHeadSha",
    );
    await this.waitForPullRequestHeadSha({
      repo: requireString(input.queueItem.repo, "queueItem.repo"),
      prNumber: requireNumber(input.queueItem.prNumber, "queueItem.prNumber"),
      expectedHeadSha: finalHeadSha,
      cwd: input.project.repoPath,
      attempts: 5,
      delayMs: 1_000,
      failureMessage: (actualHeadSha) =>
        `PR head did not update after push: expected ${finalHeadSha}, got ${actualHeadSha ?? "unknown"}`,
    });

    const metadata = parseJsonObject(input.loop.metadataJson);
    const pushedAt = this.nowIso();
    this.updateLoop(input.loop, {
      metadataJson: JSON.stringify({
        ...metadata,
        lastFixHeadSha: input.checkpoint.detail?.headSha ?? null,
        lastFixItemsHash: input.checkpoint.fixItemsHash ?? null,
        lastFixPushedAt: pushedAt,
      }),
    });
    this.appendEvent({
      eventType: "pr.branch.pushed",
      projectId: input.project.id,
      loopId: input.loop.id,
      entityType: "pull_request",
      entityId: buildPullRequestTargetId(
        requireString(input.queueItem.repo, "queueItem.repo"),
        requireNumber(input.queueItem.prNumber, "queueItem.prNumber"),
      ),
      payload: {
        branch,
        pushedAt,
        headSha: input.checkpoint.detail?.headSha ?? null,
      },
    });

    return {
      ...input.checkpoint,
      push: {
        pushed: true,
        branch,
        remote: "origin",
        pushedAt,
      },
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runResolveCommentsStep(input: {
    checkpoint: FixerCheckpoint;
    project: ProjectRecord;
    queueItem: QueueItemRecord;
  }): Promise<FixerCheckpoint> {
    if (input.checkpoint.skipReason) {
      return input.checkpoint;
    }
    if (!input.checkpoint.validation?.passed) {
      throw new FixerLoopError(
        "resolve-comments requires successful validation",
        "retryable_after_resume",
      );
    }
    if (!input.checkpoint.push) {
      throw new FixerLoopError(
        "resolve-comments requires push step to complete",
        "retryable_after_resume",
      );
    }

    const finalHeadSha = input.checkpoint.reconcileCommits?.finalHeadSha;
    if (finalHeadSha) {
      await this.waitForPullRequestHeadSha({
        repo: requireString(input.queueItem.repo, "queueItem.repo"),
        prNumber: requireNumber(input.queueItem.prNumber, "queueItem.prNumber"),
        expectedHeadSha: finalHeadSha,
        cwd: input.project.repoPath,
        attempts: 5,
        delayMs: 1_000,
        failureMessage: (actualHeadSha) =>
          `PR head changed before resolving comments: expected ${finalHeadSha}, got ${actualHeadSha ?? "unknown"}`,
      });
    }

    const repo = requireString(input.queueItem.repo, "queueItem.repo");
    const _worktreePath = requireString(
      requireWorktree(input.checkpoint).path,
      "worktree.path",
    );
    const resolvedComments =
      input.checkpoint.resolvedComments ??
      ({ items: [] } satisfies NonNullable<
        FixerCheckpoint["resolvedComments"]
      >);
    input.checkpoint.resolvedComments = resolvedComments;

    let failedCount = 0;
    for (const item of input.checkpoint.fixItems ?? []) {
      if (item.type !== "comment") {
        continue;
      }
      const existing = resolvedComments.items.find(
        (entry) =>
          entry.fixItemId === item.id &&
          ["resolved", "already_resolved"].includes(entry.status),
      );
      if (existing) {
        continue;
      }

      try {
        await this.options.github.resolveReviewThread({
          repo,
          threadId: item.threadId,
          cwd: input.project.repoPath,
        });
        upsertResolvedComment(resolvedComments.items, {
          fixItemId: item.id,
          threadId: item.threadId,
          status: "resolved",
          updatedAt: this.nowIso(),
        });
      } catch (error) {
        const message =
          error instanceof Error ? error.message : "Failed to resolve thread";
        upsertResolvedComment(resolvedComments.items, {
          fixItemId: item.id,
          threadId: item.threadId,
          status: message.includes("already") ? "already_resolved" : "failed",
          message,
          updatedAt: this.nowIso(),
        });
        if (!message.includes("already")) {
          failedCount += 1;
        }
      }
    }

    this.appendEvent({
      eventType: "fixer.comments.resolved",
      projectId: input.project.id,
      entityType: "pull_request",
      entityId: buildPullRequestTargetId(
        repo,
        requireNumber(input.queueItem.prNumber, "queueItem.prNumber"),
      ),
      payload: { items: resolvedComments.items },
    });

    if (failedCount > 0) {
      throw new FixerLoopError(
        `Failed to resolve ${failedCount} review thread(s)`,
        "retryable_after_resume",
      );
    }

    return {
      ...input.checkpoint,
      resolvedComments,
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runRecheckStep(input: {
    checkpoint: FixerCheckpoint;
    project: ProjectRecord;
    queueItem: QueueItemRecord;
  }): Promise<FixerCheckpoint> {
    if (input.checkpoint.skipReason) {
      return input.checkpoint;
    }

    const repo = requireString(input.queueItem.repo, "queueItem.repo");
    const prNumber = requireNumber(
      input.queueItem.prNumber,
      "queueItem.prNumber",
    );

    try {
      const detail = await this.options.github.viewPullRequest({
        repo,
        prNumber,
        cwd: input.project.repoPath,
      });
      const checkpointHadSpecReviewing = hasLabel(
        input.checkpoint.detail?.labels,
        SPEC_REVIEWING_LABEL,
      );
      if (
        (hasLabel(detail.labels, SPEC_REVIEWING_LABEL) ||
          checkpointHadSpecReviewing) &&
        isSpecReviewClean(detail)
      ) {
        if (hasLabel(detail.labels, SPEC_REVIEWING_LABEL)) {
          await this.options.github.removePullRequestLabels({
            repo,
            prNumber,
            labels: [SPEC_REVIEWING_LABEL],
            cwd: input.project.repoPath,
          });
        }
        if (!hasLabel(detail.labels, SPEC_READY_LABEL)) {
          await this.options.github.addPullRequestLabels({
            repo,
            prNumber,
            labels: [SPEC_READY_LABEL],
            cwd: input.project.repoPath,
          });
        }
      }
      return {
        ...input.checkpoint,
        recheck: {
          remainingFixItems: collectFixItems(detail),
        },
      };
    } catch (error) {
      throw new FixerLoopError(
        error instanceof Error
          ? error.message
          : "Failed to recheck pull request",
        "retryable_after_resume",
      );
    }
  }

  private createRunContext(loop: LoopRecord): ResumedRunContext {
    const latestRun = this.options.store.runs.listByLoop(loop.id)[0] ?? null;
    const nowIso = this.nowIso();
    const checkpoint = parseCheckpoint(latestRun?.checkpointJson);
    const lastCompletedStep = asFixerStep(latestRun?.lastCompletedStep);
    const failedStep = asFixerStep(latestRun?.currentStep);
    const restartFromDiscovery = shouldRestartFromDiscovery({
      latestRunStatus: latestRun?.status,
      failedStep,
      failureSummary: latestRun?.summary ?? latestRun?.errorMessage,
    });
    const resumeFromPrepare = shouldResumeFromPrepare({
      latestRunStatus: latestRun?.status,
      failedStep,
      checkpoint,
    });
    const resumedCheckpoint = restartFromDiscovery
      ? { resumePolicy: "replay_step" }
      : resumeFromPrepare
        ? rewindCheckpointForPrepareRetry(checkpoint)
        : checkpoint;
    const startStep: FixerStep =
      latestRun &&
      (latestRun.status === "failed" || latestRun.status === "interrupted") &&
      (restartFromDiscovery || resumeFromPrepare || lastCompletedStep)
        ? restartFromDiscovery
          ? "discover-pr"
          : resumeFromPrepare
            ? "prepare-worktree"
            : (nextFixerStep(
                requireFixerStep(lastCompletedStep, "lastCompletedStep"),
              ) ?? "discover-pr")
        : "discover-pr";
    const resumed =
      Boolean(latestRun) &&
      (latestRun?.status === "failed" || latestRun?.status === "interrupted") &&
      startStep !== "discover-pr";

    const run: RunRecord = {
      id: randomUUID(),
      loopId: loop.id,
      status: "running",
      currentStep: startStep,
      lastCompletedStep: resumed
        ? restartFromDiscovery
          ? null
          : resumeFromPrepare
            ? (previousFixerStep(startStep) ?? null)
            : (lastCompletedStep ?? null)
        : null,
      checkpointJson: JSON.stringify(
        resumed
          ? { ...resumedCheckpoint, resumePolicy: "advance_from_checkpoint" }
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
    step: FixerStep,
    checkpoint: FixerCheckpoint,
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
    step: FixerStep,
    checkpoint: FixerCheckpoint,
  ): RunRecord {
    if (checkpoint.skipReason) {
      return this.finalizeRun(run, {
        status: "success",
        summary: checkpoint.skipReason,
        checkpoint,
      });
    }

    const updated: RunRecord = {
      ...run,
      currentStep: nextFixerStep(step),
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
      checkpoint: FixerCheckpoint;
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

  private ensureLoopForPullRequest(input: {
    projectId: string;
    repo: string;
    prNumber: number;
  }): { record: LoopRecord; created: boolean } {
    const existing = this.options.store.loops
      .list()
      .find(
        (loop) =>
          loop.type === "fixer" &&
          loop.projectId === input.projectId &&
          loop.repo === input.repo &&
          loop.prNumber === input.prNumber,
      );

    const nowIso = this.nowIso();
    if (existing) {
      const updated = {
        ...existing,
        status: existing.status === "running" ? existing.status : "queued",
        nextRunAt: nowIso,
        updatedAt: nowIso,
      };
      this.options.store.loops.upsert(updated);
      return { record: updated, created: false };
    }

    const loop: LoopRecord = {
      id: randomUUID(),
      seq: this.options.store.loops.allocateSeq(),
      projectId: input.projectId,
      type: "fixer",
      targetType: "pull_request",
      targetId: buildPullRequestTargetId(input.repo, input.prNumber),
      repo: input.repo,
      prNumber: input.prNumber,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: nowIso,
      createdAt: nowIso,
      updatedAt: nowIso,
    };
    this.options.store.loops.upsert(loop);
    return { record: loop, created: true };
  }

  private hasActivePrLock(repo: string, prNumber: number): boolean {
    const lock = this.options.store.locks.get(`pr:${repo}:${prNumber}`);
    if (!lock) {
      return false;
    }
    return new Date(lock.expiresAt).getTime() > this.now().getTime();
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

  private classifyFailure(error: unknown): FixerLoopError {
    if (error instanceof FixerLoopError) {
      return error;
    }
    if (error instanceof RemoteHeadChangedError) {
      return new FixerLoopError(error.message, "retryable_after_resume");
    }
    if (error instanceof CommandExecutionError) {
      return new FixerLoopError(error.message, "retryable_transient");
    }
    return new FixerLoopError(
      error instanceof Error ? error.message : "Fixer loop failed",
      "non_retryable",
    );
  }

  private async reconcileCommits(input: {
    checkpoint: FixerCheckpoint;
    commitMessage: string;
  }): Promise<FixerCheckpoint> {
    const worktree = requireWorktree(input.checkpoint);
    const worktreePath = requireString(worktree.path, "worktree.path");
    const baseHeadSha =
      input.checkpoint.reconcileCommits?.baseHeadSha ??
      worktree.baseHeadSha ??
      worktree.headSha;
    const initial = await this.options.git.inspectHead({
      worktreePath,
      baseRef: baseHeadSha,
    });

    let committedByLooperd = false;
    if (initial.hasUncommittedChanges) {
      if (!this.allowAutoCommit) {
        throw new FixerLoopError(
          `Auto commit disabled but fixer worktree has uncommitted changes: ${initial.changedFiles.join(", ") || "unknown files"}`,
          "manual_intervention",
        );
      }
      await this.options.git.commit({
        worktreePath,
        message: input.commitMessage,
      });
      committedByLooperd = true;
    }

    const final = await this.options.git.inspectHead({
      worktreePath,
      baseRef: baseHeadSha,
    });
    return {
      ...input.checkpoint,
      reconcileCommits: {
        baseHeadSha,
        finalHeadSha: final.headSha,
        newCommitShas: final.newCommitShas,
        committedByAgent: initial.newCommitShas.length > 0,
        committedByLooperd,
        workingTreeClean: !final.hasUncommittedChanges,
        changedFiles: final.changedFiles,
        completedAt: this.nowIso(),
      },
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async cleanupFixerWorktreeIfTerminal(input: {
    checkpoint: FixerCheckpoint;
    project: ProjectRecord;
  }): Promise<void> {
    const worktree = input.checkpoint.worktree;
    if (!worktree?.path || !worktree.branch || worktree.cleanedAt) {
      return;
    }

    worktree.cleanupAttemptedAt = this.nowIso();
    try {
      await this.options.git.cleanupWorktree({
        projectId: input.project.id,
        repoPath: input.project.repoPath,
        worktreePath: worktree.path,
        branch: worktree.branch,
        protectedBranches: compactStrings([input.project.baseBranch]),
      });
      worktree.cleanedAt = this.nowIso();
      this.appendEvent({
        eventType: "fixer.worktree.cleaned",
        projectId: input.project.id,
        entityType: "pull_request",
        entityId: input.project.id,
        payload: { path: worktree.path, branch: worktree.branch },
      });
    } catch (error) {
      this.appendEvent({
        eventType: "fixer.worktree.cleanup_failed",
        projectId: input.project.id,
        entityType: "pull_request",
        entityId: input.project.id,
        payload: {
          path: worktree.path,
          branch: worktree.branch,
          message:
            error instanceof Error ? error.message : "Unknown cleanup failure",
        },
      });
      this.options.logger.error("fixer worktree cleanup failed", {
        projectId: input.project.id,
        worktreePath: worktree.path,
        branch: worktree.branch,
        message:
          error instanceof Error ? error.message : "Unknown cleanup failure",
      });
    }
  }

  private async waitForPullRequestHeadSha(input: {
    repo: string;
    prNumber: number;
    expectedHeadSha: string;
    cwd: string;
    attempts: number;
    delayMs: number;
    failureMessage: (actualHeadSha?: string) => string;
  }): Promise<void> {
    let actualHeadSha: string | undefined;
    for (let attempt = 0; attempt < input.attempts; attempt += 1) {
      const currentPr = await this.options.github.viewPullRequest({
        repo: input.repo,
        prNumber: input.prNumber,
        cwd: input.cwd,
      });
      actualHeadSha = currentPr.headSha;
      if (actualHeadSha === input.expectedHeadSha) {
        return;
      }
      if (attempt < input.attempts - 1) {
        await this.sleep(input.delayMs);
      }
    }

    throw new FixerLoopError(
      input.failureMessage(actualHeadSha),
      "retryable_after_resume",
    );
  }

  private async runValidation(input: {
    cwd: string;
    commands: string[];
  }): Promise<FixerValidationResult> {
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

  private nowIso(): string {
    return this.now().toISOString();
  }

  private appendEvent(input: {
    eventType:
      | "loop.started"
      | "run.started"
      | "loop.step.started"
      | "loop.step.completed"
      | "loop.step.failed"
      | "run.completed"
      | "run.failed"
      | "pr.branch.pushed"
      | "fixer.worktree.prepared"
      | "fixer.commits.reconciled"
      | "fixer.comments.resolved"
      | "fixer.push.skipped"
      | "fixer.push.retryable"
      | "fixer.push.conflicted"
      | "fixer.worktree.cleaned"
      | "fixer.worktree.cleanup_failed";
    projectId: string;
    loopId?: string;
    runId?: string;
    entityType: "loop" | "run" | "pull_request";
    entityId: string;
    payload?: Record<string, unknown>;
  }): void {
    this.options.store.events.append({
      id: randomUUID(),
      eventType: input.eventType,
      projectId: input.projectId,
      loopId: input.loopId ?? null,
      runId: input.runId ?? null,
      entityType: input.entityType,
      entityId: input.entityId,
      correlationId: null,
      causationId: null,
      actorType: "system",
      actorId: "looperd",
      actorDisplayName: "looperd",
      payloadJson: JSON.stringify(input.payload ?? {}),
      createdAt: this.nowIso(),
    });
  }
}

function collectFixItemsFromCheckpoint(checkpoint: FixerCheckpoint): FixItem[] {
  const detail = checkpoint.detail;
  if (!detail) {
    return [];
  }

  return normalizeFixItems(detail);
}

function collectFixItems(detail: GitHubPullRequestDetail): FixItem[] {
  return normalizeFixItems(detail as unknown as Record<string, unknown>);
}

function normalizeFixItems(detail: {
  comments?: unknown[];
  checks?: unknown[];
  hasConflicts?: boolean;
}): FixItem[] {
  const commentItems: FixItem[] = asArray(detail.comments)
    .filter((comment) => !isCommentResolved(comment))
    .map((comment, index) => {
      const id =
        readString((comment as Record<string, unknown>).id) ??
        `comment-${index}`;
      const threadId =
        readString((comment as Record<string, unknown>).threadId) ?? id;
      return {
        type: "comment",
        id,
        threadId,
        summary:
          readString((comment as Record<string, unknown>).body) ??
          readString((comment as Record<string, unknown>).state) ??
          "Unresolved review comment",
      };
    });

  const checkItems: FixItem[] = asArray(detail.checks)
    .filter((check) => isFailingCheck(check as Record<string, unknown>))
    .map((check) => {
      const row = check as Record<string, unknown>;
      return {
        type: "check",
        name: readString(row.name) ?? "unnamed-check",
        summary:
          readString(row.conclusion) ??
          readString(row.state) ??
          "Failing check",
      };
    });

  const conflictItem: FixItem[] = detail.hasConflicts
    ? [{ type: "conflict", files: [] }]
    : [];

  return [...commentItems, ...checkItems, ...conflictItem];
}

function isCommentResolved(comment: unknown): boolean {
  if (!comment || typeof comment !== "object") {
    return false;
  }
  const state = readString((comment as Record<string, unknown>).state);
  if (state?.toUpperCase() === "RESOLVED") {
    return true;
  }
  if ((comment as Record<string, unknown>).isResolved === true) {
    return true;
  }
  return false;
}

function isFailingCheck(check: Record<string, unknown>): boolean {
  const state =
    readString(check.conclusion)?.toUpperCase() ??
    readString(check.state)?.toUpperCase() ??
    "UNKNOWN";
  return [
    "FAILURE",
    "FAILED",
    "ERROR",
    "TIMED_OUT",
    "ACTION_REQUIRED",
  ].includes(state);
}

function buildPullRequestTargetId(repo: string, prNumber: number): string {
  return `pr:${repo}:${prNumber}`;
}

function buildFixerDedupeKey(
  repo: string,
  prNumber: number,
  headSha: string,
  fixItemsHash: string,
): string {
  return `fixer:${repo}:${prNumber}:${headSha}:${fixItemsHash}`;
}

function nextFixerStep(step: FixerStep): FixerStep | null {
  const index = FIXER_STEP_SEQUENCE.indexOf(step);
  return FIXER_STEP_SEQUENCE[index + 1] ?? null;
}

function previousFixerStep(step: FixerStep): FixerStep | null {
  const index = FIXER_STEP_SEQUENCE.indexOf(step);
  return index > 0 ? (FIXER_STEP_SEQUENCE[index - 1] ?? null) : null;
}

function asFixerStep(value: string | null | undefined): FixerStep | null {
  return FIXER_STEP_SEQUENCE.includes(value as FixerStep)
    ? (value as FixerStep)
    : null;
}

function parseCheckpoint(value?: string | null): FixerCheckpoint {
  if (!value) {
    return {};
  }
  return parseJsonObject(value) as FixerCheckpoint;
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

function hashFixItems(fixItems: FixItem[]): string {
  const normalized = fixItems
    .map((item) => JSON.stringify(item))
    .sort()
    .join("|");
  return createHash("sha1").update(normalized).digest("hex");
}

function asArray(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function readString(value: unknown): string | undefined {
  return typeof value === "string" && value.length > 0 ? value : undefined;
}

function readBoolean(value: unknown): boolean | undefined {
  return typeof value === "boolean" ? value : undefined;
}

function requireString(
  value: string | null | undefined,
  fieldName: string,
): string {
  if (typeof value !== "string" || value.length === 0) {
    throw new Error(`${fieldName} is required`);
  }
  return value;
}

function requireNumber(
  value: number | null | undefined,
  fieldName: string,
): number {
  if (typeof value !== "number") {
    throw new Error(`${fieldName} is required`);
  }
  return value;
}

function normalizePrState(value: string | undefined): "open" | "other" {
  return value?.toLowerCase() === "open" ? "open" : "other";
}

function buildFixerPrompt(input: {
  repo: string;
  prNumber: number;
  headSha?: string;
  fixItems: FixItem[];
}): string {
  return appendCompletionInstruction(
    [
      `Fix pull request ${input.repo}#${input.prNumber}.`,
      input.headSha ? `Head SHA: ${input.headSha}` : null,
      `Fix items:\n${input.fixItems.map((item) => `- ${JSON.stringify(item)}`).join("\n")}`,
      "Only perform repair changes for the listed fix items.",
      "Focus on code changes needed for the listed fix items. Avoid pushing branches or changing remote review state; Looper will handle follow-up repository actions after your edits.",
    ]
      .filter((value): value is string => Boolean(value))
      .join("\n\n"),
  );
}

function requireWorktree(
  checkpoint: FixerCheckpoint,
): NonNullable<FixerCheckpoint["worktree"]> {
  if (!checkpoint.worktree?.path || !checkpoint.worktree.branch) {
    throw new FixerLoopError(
      "Missing worktree checkpoint for fixer step",
      "retryable_after_resume",
    );
  }
  return checkpoint.worktree;
}

function buildFixerCommitMessage(prNumber: number): string {
  return `fixer: address PR #${prNumber} follow-up items`;
}

function compactStrings(values: Array<string | null | undefined>): string[] {
  return values.filter((value): value is string => Boolean(value));
}

function requireFixerStep(
  value: FixerStep | null | undefined,
  fieldName: string,
): FixerStep {
  if (!value) {
    throw new Error(`${fieldName} is required`);
  }
  return value;
}

function shouldResumeFromPrepare(input: {
  latestRunStatus?: string | null;
  failedStep: FixerStep | null;
  checkpoint: FixerCheckpoint;
}): boolean {
  if (
    input.latestRunStatus !== "failed" &&
    input.latestRunStatus !== "interrupted"
  ) {
    return false;
  }
  if (!input.checkpoint.worktree?.preparedAt) {
    return false;
  }

  return ["repair", "reconcile-commits", "validate", "push"].includes(
    input.failedStep ?? "discover-pr",
  );
}

function shouldRestartFromDiscovery(input: {
  latestRunStatus?: string | null;
  failedStep: FixerStep | null;
  failureSummary?: string | null;
}): boolean {
  if (
    input.latestRunStatus !== "failed" &&
    input.latestRunStatus !== "interrupted"
  ) {
    return false;
  }

  if (input.failedStep === "prepare-worktree") {
    return true;
  }

  if (input.failedStep === "push") {
    return (input.failureSummary ?? "").includes("Remote head changed");
  }

  if (input.failedStep !== "resolve-comments") {
    return false;
  }

  if (input.latestRunStatus === "interrupted") {
    return true;
  }

  return (input.failureSummary ?? "").includes(
    "PR head changed before resolving comments",
  );
}

function shouldRebuildWorktree(checkpoint: FixerCheckpoint): boolean {
  return Boolean(checkpoint.worktree?.path && !checkpoint.worktree?.preparedAt);
}

function rewindCheckpointForPrepareRetry(
  checkpoint: FixerCheckpoint,
): FixerCheckpoint {
  return {
    ...checkpoint,
    skipReason: undefined,
    worktree: checkpoint.worktree
      ? {
          ...checkpoint.worktree,
          headSha: undefined,
          baseHeadSha: undefined,
          preparedAt: undefined,
          cleanedAt: undefined,
        }
      : undefined,
    repair: undefined,
    reconcileCommits: undefined,
    validation: undefined,
    push: undefined,
    resolvedComments: undefined,
    recheck: undefined,
  };
}

function upsertResolvedComment(
  items: NonNullable<FixerCheckpoint["resolvedComments"]>["items"],
  next: NonNullable<FixerCheckpoint["resolvedComments"]>["items"][number],
): void {
  const index = items.findIndex(
    (item) =>
      item.fixItemId === next.fixItemId || item.threadId === next.threadId,
  );
  if (index >= 0) {
    items[index] = next;
    return;
  }
  items.push(next);
}
