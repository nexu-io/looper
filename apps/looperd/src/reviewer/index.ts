import { randomUUID } from "node:crypto";

import type { Logger } from "../bootstrap/logger";
import type { AgentResult, AgentRunInput } from "../infra/agent";
import { appendCompletionInstruction } from "../infra/agent-prompt";
import { CommandExecutionError } from "../infra/command";
import type {
  GitHubPullRequestDetail,
  GitHubPullRequestSummary,
  SubmitReviewInput,
} from "../infra/github";
import {
  SPEC_READY_LABEL,
  SPEC_REVIEWING_LABEL,
  hasLabel,
  isSpecReviewClean,
  resolvePullRequestPhase,
} from "../infra/spec-pr";
import type { SchedulerQueue } from "../scheduler/index";
import type { Store } from "../storage/store";
import type {
  LoopRecord,
  ProjectRecord,
  PullRequestSnapshotRecord,
  QueueFailureKind,
  QueueItemRecord,
  RunRecord,
} from "../storage/types";

const REVIEWER_STEP_SEQUENCE = [
  "discover",
  "filter",
  "claim",
  "snapshot",
  "review",
  "publish",
] as const;

export type ReviewerStep = (typeof REVIEWER_STEP_SEQUENCE)[number];
export type ReviewEvent = SubmitReviewInput["event"];

export interface ReviewerGitHubGateway {
  listOpenPullRequests(input: {
    repo: string;
    cwd?: string;
    limit?: number;
    label?: string;
  }): Promise<GitHubPullRequestSummary[]>;
  getCurrentUserLogin(input?: { cwd?: string }): Promise<string | undefined>;
  viewPullRequest(input: {
    repo: string;
    prNumber: number;
    cwd?: string;
  }): Promise<GitHubPullRequestDetail>;
  capturePullRequestSnapshot(input: {
    projectId: string;
    repo: string;
    prNumber: number;
    cwd?: string;
    capturedAt?: string;
  }): Promise<PullRequestSnapshotRecord>;
  submitReview(input: SubmitReviewInput): Promise<void>;
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

export interface ReviewerAgentExecution {
  wait(): Promise<AgentResult>;
}

export interface ReviewerAgentExecutor {
  start(input: AgentRunInput): Promise<ReviewerAgentExecution>;
}

export interface ReviewerLoopRunnerOptions {
  store: Store;
  scheduler: SchedulerQueue;
  github: ReviewerGitHubGateway;
  agentExecutor: ReviewerAgentExecutor;
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
  allowAutoApprove?: boolean;
}

export interface ReviewerDiscoveryResult {
  queueItems: QueueItemRecord[];
  createdLoopIds: string[];
  skipped: number;
}

export interface ReviewerProcessResult {
  loopId: string;
  runId: string;
  queueItemId: string;
  status: "success" | "skipped" | "failed";
  summary: string;
  failureKind?: QueueFailureKind;
}

interface ReviewerCheckpoint {
  resumePolicy?:
    | "replay_step"
    | "advance_from_checkpoint"
    | "manual_intervention";
  detail?: {
    title?: string;
    state?: string;
    isDraft?: boolean;
    reviewDecision?: string;
    labels?: string[];
    headSha?: string;
    baseSha?: string;
    author?: string;
  };
  claimedLockKey?: string;
  snapshot?: {
    id: string;
    headSha: string;
    capturedAt: string;
    title?: string | null;
    body?: string | null;
    author?: string | null;
    checksSummary?: string | null;
    unresolvedThreadCount?: number | null;
    payloadJson?: string | null;
  };
  pendingReview?: {
    headSha: string;
    event: ReviewEvent;
    body: string;
    summary?: string;
  };
  skipReason?: string;
}

interface ResumedRunContext {
  run: RunRecord;
  startStep: ReviewerStep;
  checkpoint: ReviewerCheckpoint;
  resumed: boolean;
}

class ReviewerLoopError extends Error {
  constructor(
    message: string,
    public readonly kind: QueueFailureKind,
  ) {
    super(message);
    this.name = "ReviewerLoopError";
  }
}

export class ReviewerLoopRunner {
  private readonly now: () => Date;
  private readonly agentTimeoutMs: number;
  private readonly claimTtlMs: number;
  private readonly allowAutoApprove: boolean;

  constructor(private readonly options: ReviewerLoopRunnerOptions) {
    this.now = options.now ?? (() => new Date());
    this.agentTimeoutMs = options.agentTimeoutMs ?? 15 * 60_000;
    this.claimTtlMs = options.claimTtlMs ?? 5 * 60_000;
    this.allowAutoApprove = options.allowAutoApprove ?? false;
  }

  public async discoverPullRequests(input: {
    projectId: string;
    repo: string;
    limit?: number;
  }): Promise<ReviewerDiscoveryResult> {
    const project = this.getProject(input.projectId);
    const openPullRequests = await this.options.github.listOpenPullRequests({
      repo: input.repo,
      cwd: project.repoPath,
      limit: input.limit,
    });
    const specReviewPullRequests =
      await this.options.github.listOpenPullRequests({
        repo: input.repo,
        cwd: project.repoPath,
        limit: input.limit,
        label: SPEC_REVIEWING_LABEL,
      });
    const currentLogin = await this.resolveCurrentGhLogin(project.repoPath);

    const queueItems: QueueItemRecord[] = [];
    const createdLoopIds: string[] = [];
    let skipped = 0;
    const seen = new Set<string>();

    const enqueuePullRequest = (pullRequest: GitHubPullRequestSummary) => {
      const dedupe = `${input.repo}#${pullRequest.number}`;
      if (seen.has(dedupe)) {
        return;
      }
      seen.add(dedupe);

      const loop = this.ensureLoopForPullRequest({
        projectId: project.id,
        repo: input.repo,
        prNumber: pullRequest.number,
      });

      if (loop.created) {
        createdLoopIds.push(loop.record.id);
      }

      const loopMetadata = parseJsonObject(loop.record.metadataJson);
      const lastPublishedHeadSha = readString(
        loopMetadata.lastPublishedHeadSha,
      );
      if (
        !loop.created &&
        lastPublishedHeadSha &&
        pullRequest.headSha &&
        lastPublishedHeadSha === pullRequest.headSha
      ) {
        skipped += 1;
        this.options.logger.info(
          "reviewer discovery skipped already-reviewed head",
          {
            projectId: project.id,
            repo: input.repo,
            prNumber: pullRequest.number,
            headSha: pullRequest.headSha,
          },
        );
        return;
      }

      queueItems.push(
        this.options.scheduler.enqueue({
          projectId: project.id,
          loopId: loop.record.id,
          type: "reviewer",
          targetType: "pull_request",
          targetId: buildPullRequestTargetId(input.repo, pullRequest.number),
          repo: input.repo,
          prNumber: pullRequest.number,
          dedupeKey: buildReviewerDedupeKey(input.repo, pullRequest.number),
        }),
      );
    };

    for (const pullRequest of openPullRequests) {
      if (
        pullRequest.isDraft ||
        normalizePrState(pullRequest.state) !== "open" ||
        !currentLogin ||
        !isCurrentUserRequested(pullRequest.reviewRequests, currentLogin)
      ) {
        skipped += 1;
        continue;
      }

      enqueuePullRequest(pullRequest);
    }

    for (const pullRequest of specReviewPullRequests) {
      if (
        pullRequest.isDraft ||
        normalizePrState(pullRequest.state) !== "open"
      ) {
        skipped += 1;
        continue;
      }

      enqueuePullRequest(pullRequest);
    }

    return { queueItems, createdLoopIds, skipped };
  }

  private async resolveCurrentGhLogin(
    cwd: string,
  ): Promise<string | undefined> {
    try {
      return normalizeLogin(
        await this.options.github.getCurrentUserLogin({ cwd }),
      );
    } catch {
      return undefined;
    }
  }

  public async processNext(
    claimedBy: string,
  ): Promise<ReviewerProcessResult | null> {
    const item = this.options.scheduler.claimNextOfType(claimedBy, "reviewer");
    if (!item) {
      return null;
    }

    return this.processClaimedItem(item);
  }

  public async processClaimedItem(
    queueItem: QueueItemRecord,
  ): Promise<ReviewerProcessResult> {
    if (queueItem.type !== "reviewer") {
      throw new Error(`Unsupported queue item type: ${queueItem.type}`);
    }
    if (!queueItem.loopId || !queueItem.repo || !queueItem.prNumber) {
      throw new Error(
        "Reviewer queue item requires loopId, repo, and prNumber",
      );
    }

    const loop = this.getLoop(queueItem.loopId);
    const project = this.getProject(loop.projectId);
    const resumedRun = this.createRunContext(loop);
    let run = resumedRun.run;
    let checkpoint = resumedRun.checkpoint;
    let claimedLockKey: string | undefined;

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
    this.options.logger.info("reviewer loop started", {
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
    this.options.logger.info("reviewer run started", {
      projectId: project.id,
      loopId: loop.id,
      runId: run.id,
      queueItemId: queueItem.id,
      currentStep: resumedRun.startStep,
    });

    try {
      for (const step of REVIEWER_STEP_SEQUENCE.slice(
        REVIEWER_STEP_SEQUENCE.indexOf(resumedRun.startStep),
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
        this.options.logger.info("reviewer step started", {
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

        if (step === "claim") {
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
        this.options.logger.info("reviewer step completed", {
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
        : `Published review for ${queueItem.repo}#${queueItem.prNumber}`;

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
        checkpoint.skipReason
          ? "reviewer run skipped"
          : "reviewer run completed",
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

      return {
        loopId: loop.id,
        runId: run.id,
        queueItemId: queueItem.id,
        status: checkpoint.skipReason ? "skipped" : "success",
        summary,
      };
    } catch (error) {
      const failure = this.classifyFailure(error);
      const failedCheckpoint = this.getLatestCheckpoint(run, checkpoint);
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
      this.options.logger.error("reviewer run failed", {
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
          status: failure.kind === "manual_intervention" ? "paused" : "failed",
          lastRunAt: this.nowIso(),
          nextRunAt: null,
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
    step: ReviewerStep;
    checkpoint: ReviewerCheckpoint;
    project: ProjectRecord;
    loop: LoopRecord;
    run: RunRecord;
    queueItem: QueueItemRecord;
  }): Promise<ReviewerCheckpoint> {
    switch (input.step) {
      case "discover":
        return this.runDiscoverStep(input);
      case "filter":
        return this.runFilterStep(input);
      case "claim":
        return this.runClaimStep(input);
      case "snapshot":
        return this.runSnapshotStep(input);
      case "review":
        return this.runReviewStep(input);
      case "publish":
        return this.runPublishStep(input);
    }
  }

  private async runDiscoverStep(input: {
    checkpoint: ReviewerCheckpoint;
    project: ProjectRecord;
    queueItem: QueueItemRecord;
  }): Promise<ReviewerCheckpoint> {
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
        title: detail.title,
        state: detail.state,
        isDraft: detail.isDraft,
        reviewDecision: detail.reviewDecision,
        labels: detail.labels,
        headSha: detail.headSha,
        baseSha: detail.baseSha,
        author: detail.author,
      },
      resumePolicy: "replay_step",
    };
  }

  private async runFilterStep(input: {
    checkpoint: ReviewerCheckpoint;
    loop: LoopRecord;
    queueItem: QueueItemRecord;
  }): Promise<ReviewerCheckpoint> {
    const detail = input.checkpoint.detail;
    if (!detail) {
      throw new ReviewerLoopError(
        "Missing PR detail checkpoint for filter step",
        "retryable_transient",
      );
    }

    if (detail.isDraft) {
      return {
        ...input.checkpoint,
        skipReason: `Skipped draft pull request ${input.queueItem.repo}#${input.queueItem.prNumber}`,
      };
    }

    if (normalizePrState(detail.state) !== "open") {
      return {
        ...input.checkpoint,
        skipReason: `Skipped non-open pull request ${input.queueItem.repo}#${input.queueItem.prNumber}`,
      };
    }

    const loopMetadata = parseJsonObject(input.loop.metadataJson);
    const lastPublishedHeadSha = readString(loopMetadata.lastPublishedHeadSha);
    if (
      lastPublishedHeadSha &&
      detail.headSha &&
      lastPublishedHeadSha === detail.headSha
    ) {
      return {
        ...input.checkpoint,
        skipReason: `Skipped already-reviewed head ${detail.headSha} for ${input.queueItem.repo}#${input.queueItem.prNumber}`,
      };
    }

    return input.checkpoint;
  }

  private async runClaimStep(input: {
    checkpoint: ReviewerCheckpoint;
    queueItem: QueueItemRecord;
  }): Promise<ReviewerCheckpoint> {
    const lockKey =
      input.queueItem.lockKey ?? buildPullRequestLockKey(input.queueItem);
    const acquired = this.options.scheduler.acquireBusinessLock({
      key: lockKey,
      owner: input.queueItem.id,
      reason: "reviewer-claim",
      expiresAt: new Date(this.now().getTime() + this.claimTtlMs).toISOString(),
    });

    if (!acquired) {
      throw new ReviewerLoopError(
        `Pull request lock is already held for ${lockKey}`,
        "retryable_transient",
      );
    }

    return {
      ...input.checkpoint,
      claimedLockKey: lockKey,
    };
  }

  private async runSnapshotStep(input: {
    checkpoint: ReviewerCheckpoint;
    project: ProjectRecord;
    queueItem: QueueItemRecord;
  }): Promise<ReviewerCheckpoint> {
    const repo = requireString(input.queueItem.repo, "queueItem.repo");
    const prNumber = requireNumber(
      input.queueItem.prNumber,
      "queueItem.prNumber",
    );
    const snapshot = await this.options.github.capturePullRequestSnapshot({
      projectId: input.project.id,
      repo,
      prNumber,
      cwd: input.project.repoPath,
      capturedAt: this.nowIso(),
    });
    this.options.store.pullRequestSnapshots.upsert(snapshot);

    return {
      ...input.checkpoint,
      snapshot: {
        id: snapshot.id,
        headSha: snapshot.headSha,
        capturedAt: snapshot.capturedAt,
        title: snapshot.title,
        body: snapshot.body,
        author: snapshot.author,
        checksSummary: snapshot.checksSummary,
        unresolvedThreadCount: snapshot.unresolvedThreadCount,
        payloadJson: snapshot.payloadJson,
      },
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runReviewStep(input: {
    checkpoint: ReviewerCheckpoint;
    project: ProjectRecord;
    loop: LoopRecord;
    run: RunRecord;
    queueItem: QueueItemRecord;
  }): Promise<ReviewerCheckpoint> {
    if (input.checkpoint.pendingReview) {
      return input.checkpoint;
    }

    const snapshot = input.checkpoint.snapshot;
    if (!snapshot) {
      throw new ReviewerLoopError(
        "Missing PR snapshot checkpoint for review step",
        "retryable_transient",
      );
    }

    const repo = requireString(input.queueItem.repo, "queueItem.repo");
    const prNumber = requireNumber(
      input.queueItem.prNumber,
      "queueItem.prNumber",
    );
    const prompt = buildReviewPrompt({
      repo,
      prNumber,
      detail: input.checkpoint.detail,
      snapshot,
    });

    const executionId = randomUUID();
    const execution = await this.options.agentExecutor.start({
      executionId,
      projectId: input.project.id,
      loopId: input.loop.id,
      runId: input.run.id,
      prompt,
      workingDirectory: input.project.repoPath,
      timeoutMs: this.agentTimeoutMs,
      metadata: {
        loopType: "reviewer",
        repo: input.queueItem.repo,
        prNumber: input.queueItem.prNumber,
      },
      idempotencyKey: `reviewer:${input.loop.id}:${snapshot.headSha}`,
    });
    await this.options.onAgentExecutionStarted?.({
      executionId,
      projectId: input.project.id,
      loopId: input.loop.id,
      runId: input.run.id,
      subtitle: `${repo}#${prNumber}`,
      body: "Review started",
      dedupeKey: `runtime.agent.started:reviewer:${input.run.id}`,
    });
    const result = await execution.wait();

    if (result.status !== "completed") {
      throw new ReviewerLoopError(
        result.summary ?? `Reviewer agent ${result.status}`,
        "retryable_transient",
      );
    }

    const reviewBody = toReviewBody(result);
    if (!reviewBody) {
      throw new ReviewerLoopError(
        "Reviewer agent produced an empty review body",
        result.parseStatus === "invalid_json"
          ? "non_retryable"
          : "retryable_transient",
      );
    }

    return {
      ...input.checkpoint,
      pendingReview: {
        headSha: snapshot.headSha,
        event: "COMMENT",
        body: reviewBody,
        summary: result.summary,
      },
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runPublishStep(input: {
    checkpoint: ReviewerCheckpoint;
    loop: LoopRecord;
    project: ProjectRecord;
    run: RunRecord;
    queueItem: QueueItemRecord;
  }): Promise<ReviewerCheckpoint> {
    if (input.checkpoint.skipReason) {
      return input.checkpoint;
    }

    const pendingReview = input.checkpoint.pendingReview;
    if (!pendingReview) {
      throw new ReviewerLoopError(
        "Missing pending review checkpoint for publish step",
        "retryable_after_resume",
      );
    }

    const loopMetadata = parseJsonObject(input.loop.metadataJson);
    const lastPublishedHeadSha = readString(loopMetadata.lastPublishedHeadSha);
    if (lastPublishedHeadSha === pendingReview.headSha) {
      return {
        ...input.checkpoint,
        skipReason: `Skipped already-published review for head ${pendingReview.headSha}`,
      };
    }

    const reviewEvent =
      pendingReview.event === "APPROVE" && !this.allowAutoApprove
        ? "COMMENT"
        : pendingReview.event;

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
    const phase = resolvePullRequestPhase({ labels: detail.labels });
    const checkpointPhase = resolvePullRequestPhase({
      labels: input.checkpoint.detail?.labels,
    });
    if (detail.headSha && detail.headSha !== pendingReview.headSha) {
      throw new ReviewerLoopError(
        `PR head changed before publish: expected ${pendingReview.headSha}, got ${detail.headSha}`,
        "retryable_after_resume",
      );
    }

    try {
      await this.options.github.submitReview({
        repo,
        prNumber,
        event: reviewEvent,
        body: pendingReview.body,
        cwd: input.project.repoPath,
      });

      let postSubmitDetail = detail;
      if (
        reviewEvent === "APPROVE" &&
        (phase === "spec" || checkpointPhase === "spec")
      ) {
        postSubmitDetail = await this.options.github.viewPullRequest({
          repo,
          prNumber,
          cwd: input.project.repoPath,
        });
      }

      const shouldPromoteSpecLabel =
        reviewEvent === "APPROVE" &&
        (phase === "spec" || checkpointPhase === "spec") &&
        isSpecReviewClean(postSubmitDetail);
      if (shouldPromoteSpecLabel) {
        if (hasLabel(postSubmitDetail.labels, SPEC_REVIEWING_LABEL)) {
          await this.options.github.removePullRequestLabels({
            repo,
            prNumber,
            labels: [SPEC_REVIEWING_LABEL],
            cwd: input.project.repoPath,
          });
        }
        if (!hasLabel(postSubmitDetail.labels, SPEC_READY_LABEL)) {
          await this.options.github.addPullRequestLabels({
            repo,
            prNumber,
            labels: [SPEC_READY_LABEL],
            cwd: input.project.repoPath,
          });
        }
      }
    } catch (error) {
      if (error instanceof ReviewerLoopError) {
        throw error;
      }

      throw new ReviewerLoopError(
        error instanceof Error ? error.message : "Failed to publish review",
        "retryable_after_resume",
      );
    }

    this.updateLoop(input.loop, {
      metadataJson: JSON.stringify({
        ...loopMetadata,
        lastPublishedHeadSha: pendingReview.headSha,
        lastReviewEvent: reviewEvent,
        lastReviewSummary: pendingReview.summary ?? null,
        lastPublishedAt: this.nowIso(),
      }),
    });
    this.appendEvent({
      eventType: "pr.review.posted",
      projectId: input.project.id,
      loopId: input.loop.id,
      runId: input.run.id,
      entityType: "pull_request",
      entityId: buildPullRequestEntityId(
        requireString(input.queueItem.repo, "queueItem.repo"),
        requireNumber(input.queueItem.prNumber, "queueItem.prNumber"),
      ),
      payload: {
        repo: input.queueItem.repo,
        prNumber: input.queueItem.prNumber,
        event: reviewEvent,
        headSha: pendingReview.headSha,
      },
    });

    return input.checkpoint;
  }

  private createRunContext(loop: LoopRecord): ResumedRunContext {
    const latestRun = this.options.store.runs.listByLoop(loop.id)[0] ?? null;
    const nowIso = this.nowIso();
    const checkpoint = parseCheckpoint(latestRun?.checkpointJson);
    const lastCompletedStep = asReviewerStep(latestRun?.lastCompletedStep);
    const failedStep = asReviewerStep(latestRun?.currentStep);
    const restartFromDiscover = shouldRestartFromDiscover({
      latestRunStatus: latestRun?.status,
      failedStep,
      failureSummary: latestRun?.summary ?? latestRun?.errorMessage,
    });
    const resumedStep = lastCompletedStep
      ? (nextReviewerStep(lastCompletedStep) ?? "discover")
      : "discover";
    const startStep =
      latestRun &&
      (latestRun.status === "failed" || latestRun.status === "interrupted") &&
      (restartFromDiscover || lastCompletedStep)
        ? restartFromDiscover
          ? "discover"
          : resumedStep
        : "discover";
    const resumed =
      Boolean(latestRun) &&
      (latestRun?.status === "failed" || latestRun?.status === "interrupted") &&
      startStep !== "discover";

    const run: RunRecord = {
      id: randomUUID(),
      loopId: loop.id,
      status: "running",
      currentStep: startStep,
      lastCompletedStep: resumed
        ? restartFromDiscover
          ? null
          : (lastCompletedStep ?? null)
        : null,
      checkpointJson: JSON.stringify(
        resumed
          ? {
              ...(restartFromDiscover
                ? { resumePolicy: "replay_step" }
                : checkpoint),
              resumePolicy: restartFromDiscover
                ? "replay_step"
                : "advance_from_checkpoint",
            }
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
    step: ReviewerStep,
    checkpoint: ReviewerCheckpoint,
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
    step: ReviewerStep,
    checkpoint: ReviewerCheckpoint,
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
      currentStep: nextReviewerStep(step),
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
      checkpoint: ReviewerCheckpoint;
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
    fallback: ReviewerCheckpoint,
  ): ReviewerCheckpoint {
    const persistedRun = this.options.store.runs.getById(run.id);

    return (
      parseCheckpoint(persistedRun?.checkpointJson ?? run.checkpointJson) ??
      fallback
    );
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
          loop.type === "reviewer" &&
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
      projectId: input.projectId,
      type: "reviewer",
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
    this.appendEvent({
      eventType: "loop.created",
      projectId: input.projectId,
      loopId: loop.id,
      entityType: "loop",
      entityId: loop.id,
      payload: {
        type: "reviewer",
        repo: input.repo,
        prNumber: input.prNumber,
      },
    });
    return { record: loop, created: true };
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

  private classifyFailure(error: unknown): ReviewerLoopError {
    if (error instanceof ReviewerLoopError) {
      return error;
    }

    if (error instanceof CommandExecutionError) {
      return new ReviewerLoopError(error.message, "retryable_transient");
    }

    return new ReviewerLoopError(
      error instanceof Error ? error.message : "Reviewer loop failed",
      "non_retryable",
    );
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
      actorId: "reviewer-loop",
      actorDisplayName: "reviewer-loop",
      payloadJson: JSON.stringify(input.payload),
      createdAt: this.nowIso(),
    });
  }

  private nowIso(): string {
    return this.now().toISOString();
  }
}

function shouldRestartFromDiscover(input: {
  latestRunStatus?: string | null;
  failedStep: ReviewerStep | null;
  failureSummary?: string | null;
}): boolean {
  if (
    input.latestRunStatus !== "failed" &&
    input.latestRunStatus !== "interrupted"
  ) {
    return false;
  }

  return (
    input.failedStep === "publish" &&
    (input.failureSummary ?? "").includes("PR head changed before publish")
  );
}

function buildPullRequestTargetId(repo: string, prNumber: number): string {
  return `pr:${repo}:${prNumber}`;
}

function buildReviewerDedupeKey(repo: string, prNumber: number): string {
  return `reviewer:${repo}:${prNumber}`;
}

function buildPullRequestEntityId(repo: string, prNumber: number): string {
  return `${repo}#${prNumber}`;
}

function buildPullRequestLockKey(queueItem: QueueItemRecord): string {
  return `pr:${queueItem.repo}:${queueItem.prNumber}`;
}

function nextReviewerStep(step: ReviewerStep): ReviewerStep | null {
  const index = REVIEWER_STEP_SEQUENCE.indexOf(step);
  return REVIEWER_STEP_SEQUENCE[index + 1] ?? null;
}

function asReviewerStep(value: string | null | undefined): ReviewerStep | null {
  return REVIEWER_STEP_SEQUENCE.includes(value as ReviewerStep)
    ? (value as ReviewerStep)
    : null;
}

function parseCheckpoint(value?: string | null): ReviewerCheckpoint {
  if (!value) {
    return {};
  }

  return parseJsonObject(value) as ReviewerCheckpoint;
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

function readString(value: unknown): string | undefined {
  return typeof value === "string" && value.length > 0 ? value : undefined;
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

function normalizeLogin(login: string | undefined): string | undefined {
  const normalized = login?.trim().toLowerCase();
  return normalized && normalized.length > 0 ? normalized : undefined;
}

function isCurrentUserRequested(
  requestedReviewers: string[] | undefined,
  currentLogin: string,
): boolean {
  return (requestedReviewers ?? []).some(
    (login) => normalizeLogin(login) === currentLogin,
  );
}

function toReviewBody(result: AgentResult): string | null {
  const candidate =
    result.summary?.trim() || summarizeLogs(result.rawLogs.stdout).trim();
  return candidate.length > 0 ? candidate : null;
}

function summarizeLogs(stdout: string): string {
  return stdout
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line.length > 0 && !line.startsWith("__LOOPER_RESULT__="))
    .join("\n");
}

function buildReviewPrompt(input: {
  repo: string;
  prNumber: number;
  detail?: ReviewerCheckpoint["detail"];
  snapshot: NonNullable<ReviewerCheckpoint["snapshot"]>;
}): string {
  const parsedPayload = parseJsonObject(input.snapshot.payloadJson);
  const diff = readString(parsedPayload.diff);
  const phase = resolvePullRequestPhase({ labels: input.detail?.labels });
  const phaseInstruction =
    phase === "spec"
      ? "This is a spec review. Focus on scope, correctness, feasibility, risks, and validation. Do not review implementation details beyond whether the spec is actionable."
      : "This is an implementation review. Focus on code correctness, safety, tests, and maintainability.";

  return appendCompletionInstruction(
    [
      `Review pull request ${input.repo}#${input.prNumber}.`,
      `Phase: ${phase}`,
      phaseInstruction,
      input.snapshot.title ? `Title: ${input.snapshot.title}` : null,
      input.snapshot.body ? `Body:\n${input.snapshot.body}` : null,
      input.detail?.author ? `Author: ${input.detail.author}` : null,
      `Head SHA: ${input.snapshot.headSha}`,
      input.snapshot.checksSummary
        ? `Checks: ${input.snapshot.checksSummary}`
        : null,
      typeof input.snapshot.unresolvedThreadCount === "number"
        ? `Unresolved threads: ${input.snapshot.unresolvedThreadCount}`
        : null,
      diff ? `Diff:\n${diff}` : null,
      "Return concise GitHub review feedback. Do not approve; provide comment-ready feedback text.",
    ]
      .filter((value): value is string => Boolean(value))
      .join("\n\n"),
  );
}
