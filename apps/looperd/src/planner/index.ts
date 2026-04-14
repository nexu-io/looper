import { randomUUID } from "node:crypto";
import { readFile } from "node:fs/promises";
import { join } from "node:path";

import type { Logger } from "../bootstrap/logger";
import { getDefaultProjectWorktreeRoot } from "../config/index";
import type { AgentResult, AgentRunInput } from "../infra/agent";
import { appendCompletionInstruction } from "../infra/agent-prompt";
import { CommandExecutionError } from "../infra/command";
import type { GitHubIssueDetail, GitHubIssueSummary } from "../infra/github";
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

const PLANNER_STEP_SEQUENCE = [
  "discover-issues",
  "prepare-worktree",
  "write-spec",
  "publish",
  "notify",
] as const;

const DISCOVERY_LABEL = "looper:plan";
const SPEC_REVIEWING_LABEL = "looper:spec-reviewing";

export type PlannerStep = (typeof PLANNER_STEP_SEQUENCE)[number];

export interface PlannerGitHubGateway {
  listOpenIssues(input: {
    repo: string;
    cwd?: string;
    limit?: number;
    assignee?: string;
    label?: string;
  }): Promise<GitHubIssueSummary[]>;
  viewIssue(input: {
    repo: string;
    issueNumber: number;
    cwd?: string;
  }): Promise<GitHubIssueDetail>;
  getCurrentUserLogin(input?: { cwd?: string }): Promise<string | undefined>;
  createPullRequest(input: {
    repo: string;
    headBranch: string;
    baseBranch: string;
    title: string;
    body?: string;
    cwd?: string;
  }): Promise<{ number?: number; url: string }>;
  addPullRequestLabels(input: {
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

export interface PlannerGitGateway {
  createWorktree(input: {
    projectId: string;
    repoPath: string;
    worktreeRoot: string;
    branch: string;
    baseBranch: string;
    protectedBranches?: string[];
  }): Promise<WorktreeRecord>;
  push(input: {
    worktreePath: string;
    branch: string;
    remote?: string;
    protectedBranches?: string[];
  }): Promise<void>;
}

export interface PlannerAgentExecution {
  wait(): Promise<AgentResult>;
}

export interface PlannerAgentExecutor {
  start(input: AgentRunInput): Promise<PlannerAgentExecution>;
}

export interface PlannerLoopRunnerOptions {
  store: Store;
  scheduler: SchedulerQueue;
  git: PlannerGitGateway;
  github: PlannerGitHubGateway;
  agentExecutor: PlannerAgentExecutor;
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
  allowAutoPush?: boolean;
}

export interface DiscoverIssuesResult {
  queueItems: QueueItemRecord[];
  createdLoopIds: string[];
  skipped: number;
}

export interface PlannerClaimedItemResult {
  loopId: string;
  runId: string;
  queueItemId: string;
  status: "success" | "failed";
  summary: string;
  failureKind?: QueueFailureKind;
  pullRequestNumber?: number;
}

interface PlannerIssueState {
  repo: string;
  issueNumber: number;
  title: string;
  body?: string | null;
  url?: string | null;
  assignees: string[];
  labels: string[];
  currentUserLogin?: string | null;
  specPath: string;
  requestedReviewers: string[];
}

interface PlannerCheckpoint {
  resumePolicy?:
    | "replay_step"
    | "advance_from_checkpoint"
    | "manual_intervention";
  issue?: PlannerIssueState;
  claimedLockKey?: string;
  worktree?: {
    id: string;
    path: string;
    branch: string;
    baseBranch: string;
    specPath: string;
  };
  writeSpec?: {
    status: AgentResult["status"];
    summary?: string;
    changedFiles: string[];
    commits: string[];
    stdout: string;
  };
  publish?: {
    pushed?: boolean;
    pullRequest?: {
      number?: number;
      url: string;
      body: string;
    };
    labelsAdded?: string[];
    reviewersAdded?: string[];
  };
  notify?: {
    sentAt: string;
    message: string;
  };
  skipReason?: string;
}

interface ResumedRunContext {
  run: RunRecord;
  startStep: PlannerStep;
  checkpoint: PlannerCheckpoint;
  resumed: boolean;
}

class PlannerLoopError extends Error {
  constructor(
    message: string,
    public readonly kind: QueueFailureKind,
  ) {
    super(message);
    this.name = "PlannerLoopError";
  }
}

export class PlannerLoopRunner {
  private readonly now: () => Date;
  private readonly agentTimeoutMs: number;
  private readonly claimTtlMs: number;
  private readonly allowAutoPush: boolean;

  constructor(private readonly options: PlannerLoopRunnerOptions) {
    this.now = options.now ?? (() => new Date());
    this.agentTimeoutMs = options.agentTimeoutMs ?? 30 * 60_000;
    this.claimTtlMs = options.claimTtlMs ?? 10 * 60_000;
    this.allowAutoPush = options.allowAutoPush ?? true;
  }

  public async discoverIssues(): Promise<DiscoverIssuesResult> {
    const projectsByRepo = new Map<string, ProjectRecord[]>();
    for (const project of this.options.store.projects.list()) {
      if (project.archived) {
        continue;
      }

      const repo = readProjectRepo(project);
      if (!repo) {
        continue;
      }
      const existing = projectsByRepo.get(repo) ?? [];
      existing.push(project);
      projectsByRepo.set(repo, existing);
    }

    const queueItems: QueueItemRecord[] = [];
    const createdLoopIds: string[] = [];
    let skipped = 0;

    for (const [repo, projects] of projectsByRepo.entries()) {
      if (projects.length !== 1) {
        this.options.logger.warn("planner discovery skipped ambiguous repo", {
          repo,
          projectIds: projects.map((project) => project.id),
        });
        skipped += 1;
        continue;
      }

      const project = projects[0];
      if (!project) {
        continue;
      }

      const currentLogin = await this.resolveCurrentGhLogin(project.repoPath);
      if (!currentLogin) {
        skipped += 1;
        continue;
      }

      const issues = await this.options.github.listOpenIssues({
        repo,
        cwd: project.repoPath,
        assignee: currentLogin,
        label: DISCOVERY_LABEL,
      });

      for (const issue of issues) {
        if (!shouldClaimIssue(issue, currentLogin)) {
          skipped += 1;
          continue;
        }

        const loop = this.ensureLoopForIssue({
          project,
          repo,
          issueNumber: issue.number,
          issue,
          currentUserLogin: currentLogin,
        });
        if (loop.created) {
          createdLoopIds.push(loop.record.id);
        }

        if (
          loop.record.status === "paused" ||
          loop.record.status === "completed"
        ) {
          skipped += 1;
          continue;
        }

        queueItems.push(
          this.options.scheduler.enqueue({
            projectId: project.id,
            loopId: loop.record.id,
            type: "planner",
            targetType: "issue",
            targetId: buildIssueTargetId(repo, issue.number),
            repo,
            dedupeKey: buildPlannerDedupeKey(repo, issue.number),
            lockKey: buildIssueLockKey(repo, issue.number),
            payloadJson: JSON.stringify({
              issueNumber: issue.number,
              title: issue.title,
              body: issue.body,
              url: issue.url,
              assignees: issue.assignees,
              labels: issue.labels,
              currentUserLogin: currentLogin,
            }),
          }),
        );
      }
    }

    return { queueItems, createdLoopIds, skipped };
  }

  public async processClaimedItem(
    queueItem: QueueItemRecord,
  ): Promise<PlannerClaimedItemResult> {
    if (queueItem.type !== "planner" || !queueItem.loopId) {
      throw new Error("planner queue item is missing loop context");
    }

    const loop = this.getLoop(queueItem.loopId);
    const project = this.getProject(loop.projectId);
    const resumedRun = this.createRunContext(loop);
    let run = resumedRun.run;
    let checkpoint = resumedRun.checkpoint;
    let claimedLockKey =
      resumedRun.startStep !== "discover-issues"
        ? checkpoint.claimedLockKey
        : undefined;
    let acquiredClaimedLock = false;

    if (claimedLockKey) {
      const acquired = this.options.scheduler.acquireBusinessLock({
        key: claimedLockKey,
        owner: queueItem.id,
        reason: "planner-run-resume",
        expiresAt: new Date(
          this.now().getTime() + this.claimTtlMs,
        ).toISOString(),
      });
      if (!acquired) {
        throw new PlannerLoopError(
          `Planner lock is already held for ${claimedLockKey}`,
          "retryable_transient",
        );
      }

      acquiredClaimedLock = true;
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

    try {
      for (const step of PLANNER_STEP_SEQUENCE.slice(
        PLANNER_STEP_SEQUENCE.indexOf(resumedRun.startStep),
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

        if (step === "discover-issues") {
          claimedLockKey = checkpoint.claimedLockKey;
          acquiredClaimedLock = Boolean(claimedLockKey);
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
        status: "success",
        summary,
        pullRequestNumber: checkpoint.publish?.pullRequest?.number,
      };
    } catch (error) {
      const failure = this.classifyFailure(error);
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
      if (acquiredClaimedLock && claimedLockKey) {
        this.options.scheduler.releaseBusinessLock(claimedLockKey);
      }
    }
  }

  private async executeStep(input: {
    step: PlannerStep;
    checkpoint: PlannerCheckpoint;
    project: ProjectRecord;
    loop: LoopRecord;
    run: RunRecord;
    queueItem: QueueItemRecord;
  }): Promise<PlannerCheckpoint> {
    switch (input.step) {
      case "discover-issues":
        return this.runDiscoverIssueStep(input);
      case "prepare-worktree":
        return this.runPrepareWorktreeStep(input);
      case "write-spec":
        return this.runWriteSpecStep(input);
      case "publish":
        return this.runPublishStep(input);
      case "notify":
        return this.runNotifyStep(input);
    }
  }

  private async runDiscoverIssueStep(input: {
    checkpoint: PlannerCheckpoint;
    queueItem: QueueItemRecord;
    loop: LoopRecord;
    project: ProjectRecord;
  }): Promise<PlannerCheckpoint> {
    const payload = parseJsonObject(input.queueItem.payloadJson);
    const repo =
      input.queueItem.repo ?? input.loop.repo ?? readProjectRepo(input.project);
    const issueNumber =
      readNumber(payload.issueNumber) ??
      parseIssueNumberFromTargetId(input.loop.targetId);
    if (!repo || !issueNumber) {
      throw new PlannerLoopError(
        "Planner queue item requires repo and issue number",
        "non_retryable",
      );
    }

    const detail = await this.options.github.viewIssue({
      repo,
      issueNumber,
      cwd: input.project.repoPath,
    });
    const currentUserLogin =
      readString(payload.currentUserLogin) ??
      input.checkpoint.issue?.currentUserLogin ??
      (await this.resolveCurrentGhLogin(input.project.repoPath));
    const lockKey =
      input.queueItem.lockKey ?? buildIssueLockKey(repo, issueNumber);
    const acquired = this.options.scheduler.acquireBusinessLock({
      key: lockKey,
      owner: input.queueItem.id,
      reason: "planner-run",
      expiresAt: new Date(this.now().getTime() + this.claimTtlMs).toISOString(),
    });
    if (!acquired) {
      throw new PlannerLoopError(
        `Planner lock is already held for ${lockKey}`,
        "retryable_transient",
      );
    }

    if (
      currentUserLogin &&
      !includesLogin(detail.assignees, currentUserLogin)
    ) {
      return {
        ...input.checkpoint,
        issue: {
          repo,
          issueNumber,
          title: detail.title,
          body: detail.body,
          url: detail.url,
          assignees: detail.assignees,
          labels: detail.labels,
          currentUserLogin,
          specPath: buildSpecPath(this.now(), issueNumber, detail.title),
          requestedReviewers: [],
        },
        claimedLockKey: lockKey,
        skipReason: `Issue ${repo}#${issueNumber} is no longer assigned to ${currentUserLogin}`,
        resumePolicy: "advance_from_checkpoint",
      };
    }

    if (
      !detail.labels.some((label) => normalizeLabel(label) === DISCOVERY_LABEL)
    ) {
      return {
        ...input.checkpoint,
        issue: {
          repo,
          issueNumber,
          title: detail.title,
          body: detail.body,
          url: detail.url,
          assignees: detail.assignees,
          labels: detail.labels,
          currentUserLogin,
          specPath: buildSpecPath(this.now(), issueNumber, detail.title),
          requestedReviewers: [],
        },
        claimedLockKey: lockKey,
        skipReason: `Issue ${repo}#${issueNumber} no longer has ${DISCOVERY_LABEL}`,
        resumePolicy: "advance_from_checkpoint",
      };
    }

    return {
      ...input.checkpoint,
      issue: {
        repo,
        issueNumber,
        title: detail.title,
        body: detail.body,
        url: detail.url,
        assignees: detail.assignees,
        labels: detail.labels,
        currentUserLogin,
        specPath: buildSpecPath(this.now(), issueNumber, detail.title),
        requestedReviewers: this.resolveRequestedReviewers({
          project: input.project,
          loop: input.loop,
          issue: detail,
          currentUserLogin,
        }),
      },
      claimedLockKey: lockKey,
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runPrepareWorktreeStep(input: {
    checkpoint: PlannerCheckpoint;
    project: ProjectRecord;
  }): Promise<PlannerCheckpoint> {
    if (input.checkpoint.worktree || input.checkpoint.skipReason) {
      return input.checkpoint;
    }

    const issue = requireIssue(input.checkpoint);
    const projectMetadata = parseJsonObject(input.project.metadataJson);
    const configuredRoot = readString(projectMetadata.worktreeRoot);
    const worktreeRoot =
      configuredRoot ??
      getDefaultProjectWorktreeRoot(input.project.id, input.project.repoPath);
    const branch = buildPlannerBranch(
      issue.issueNumber,
      issue.title || "issue",
    );
    const worktree = await this.options.git.createWorktree({
      projectId: input.project.id,
      repoPath: input.project.repoPath,
      worktreeRoot,
      branch,
      baseBranch: input.project.baseBranch ?? "main",
      protectedBranches: [input.project.baseBranch ?? "main"],
    });

    return {
      ...input.checkpoint,
      worktree: {
        id: worktree.id,
        path: worktree.worktreePath,
        branch: worktree.branch,
        baseBranch: worktree.baseBranch,
        specPath: issue.specPath,
      },
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runWriteSpecStep(input: {
    checkpoint: PlannerCheckpoint;
    project: ProjectRecord;
    loop: LoopRecord;
    run: RunRecord;
  }): Promise<PlannerCheckpoint> {
    if (
      input.checkpoint.writeSpec?.status === "completed" ||
      input.checkpoint.skipReason
    ) {
      return input.checkpoint;
    }

    const issue = requireIssue(input.checkpoint);
    const worktree = requireWorktree(input.checkpoint);
    const prompt = await buildPlannerPrompt({
      project: input.project,
      issue,
      worktree,
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
        loopType: "planner",
        repo: issue.repo,
        issueNumber: issue.issueNumber,
        specPath: issue.specPath,
      },
      idempotencyKey: `planner:${input.loop.id}`,
    });
    try {
      await this.options.onAgentExecutionStarted?.({
        executionId,
        projectId: input.project.id,
        loopId: input.loop.id,
        runId: input.run.id,
        subtitle: `${issue.repo}#${issue.issueNumber}`,
        body: `Planner started for ${issue.title}`,
        dedupeKey: `runtime.agent.started:planner:${input.run.id}`,
      });
    } catch (error) {
      this.options.logger.warn("planner agent start notification failed", {
        loopId: input.loop.id,
        runId: input.run.id,
        error: error instanceof Error ? error.message : String(error),
      });
    }
    const result = await execution.wait();
    if (result.status !== "completed") {
      throw new PlannerLoopError(
        result.summary ?? `Planner agent ${result.status}`,
        "retryable_transient",
      );
    }

    return {
      ...input.checkpoint,
      writeSpec: {
        status: result.status,
        summary: result.summary,
        changedFiles: result.changedFiles,
        commits: result.commits,
        stdout: result.rawLogs.stdout,
      },
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private async runPublishStep(input: {
    checkpoint: PlannerCheckpoint;
    project: ProjectRecord;
    loop: LoopRecord;
    run: RunRecord;
  }): Promise<PlannerCheckpoint> {
    if (input.checkpoint.skipReason) {
      return input.checkpoint;
    }
    if (!this.allowAutoPush) {
      return {
        ...input.checkpoint,
        skipReason: `Auto push disabled; manual publish required for planner ${input.loop.id}`,
        resumePolicy: "manual_intervention",
      };
    }

    const issue = requireIssue(input.checkpoint);
    const worktree = requireWorktree(input.checkpoint);
    const publish = {
      ...input.checkpoint.publish,
      labelsAdded: input.checkpoint.publish?.labelsAdded ?? [],
      reviewersAdded: input.checkpoint.publish?.reviewersAdded ?? [],
    };

    try {
      if (!publish.pushed) {
        await this.options.git.push({
          worktreePath: worktree.path,
          branch: worktree.branch,
          protectedBranches: [worktree.baseBranch],
        });
        publish.pushed = true;
      }

      if (!publish.pullRequest) {
        const body = buildPlannerPullRequestBody({
          issue,
          branch: worktree.branch,
          executionSummary: input.checkpoint.writeSpec?.summary,
        });
        const pullRequest = await this.options.github.createPullRequest({
          repo: issue.repo,
          headBranch: worktree.branch,
          baseBranch: worktree.baseBranch,
          title: `Spec: ${issue.title}`,
          body,
          cwd: input.project.repoPath,
        });
        publish.pullRequest = {
          number: pullRequest.number,
          url: pullRequest.url,
          body,
        };

        this.updateLoop(input.loop, {
          repo: issue.repo,
          prNumber: pullRequest.number ?? null,
        });
        this.updateLoopMetadata(input.loop.id, {
          issueNumber: issue.issueNumber,
          issueUrl: issue.url,
          issueTitle: issue.title,
          specPath: issue.specPath,
          branch: worktree.branch,
          prUrl: pullRequest.url,
          prNumber: pullRequest.number ?? null,
          requestedReviewers: issue.requestedReviewers,
        });

        this.persistStepStarted(input.run, "publish", {
          ...input.checkpoint,
          publish,
          resumePolicy: "advance_from_checkpoint",
        });
      }

      const prNumber = publish.pullRequest.number;
      if (!prNumber) {
        throw new PlannerLoopError(
          "Planner publish requires a pull request number",
          "retryable_after_resume",
        );
      }

      if (!publish.labelsAdded.includes(SPEC_REVIEWING_LABEL)) {
        await this.options.github.addPullRequestLabels({
          repo: issue.repo,
          prNumber,
          labels: [SPEC_REVIEWING_LABEL],
          cwd: input.project.repoPath,
        });
        publish.labelsAdded = [...publish.labelsAdded, SPEC_REVIEWING_LABEL];
      }

      const pendingReviewers = issue.requestedReviewers.filter(
        (reviewer) => !publish.reviewersAdded.includes(reviewer),
      );
      if (pendingReviewers.length > 0) {
        await this.options.github.addPullRequestReviewers({
          repo: issue.repo,
          prNumber,
          reviewers: pendingReviewers,
          cwd: input.project.repoPath,
        });
        publish.reviewersAdded = [
          ...publish.reviewersAdded,
          ...pendingReviewers,
        ];
      }

      return {
        ...input.checkpoint,
        publish,
        resumePolicy: "advance_from_checkpoint",
      };
    } catch (error) {
      if (error instanceof PlannerLoopError) {
        throw error;
      }
      throw new PlannerLoopError(
        error instanceof Error ? error.message : "Planner publish failed",
        "retryable_after_resume",
      );
    }
  }

  private async runNotifyStep(input: {
    checkpoint: PlannerCheckpoint;
    loop: LoopRecord;
  }): Promise<PlannerCheckpoint> {
    if (input.checkpoint.notify || input.checkpoint.skipReason) {
      return input.checkpoint;
    }

    const issue = requireIssue(input.checkpoint);
    const pullRequest = input.checkpoint.publish?.pullRequest;
    const message = pullRequest?.url
      ? `Spec PR ready for review: ${pullRequest.url}`
      : `Planner completed for ${issue.repo}#${issue.issueNumber}`;
    this.appendEvent({
      eventType: "loop.step.completed",
      loopId: input.loop.id,
      entityType: "loop",
      entityId: input.loop.id,
      payload: {
        step: "notify",
        message,
      },
    });

    return {
      ...input.checkpoint,
      notify: {
        sentAt: this.nowIso(),
        message,
      },
      resumePolicy: "advance_from_checkpoint",
    };
  }

  private resolveRequestedReviewers(input: {
    project: ProjectRecord;
    loop: LoopRecord;
    issue: GitHubIssueDetail;
    currentUserLogin?: string | null;
  }): string[] {
    const projectMetadata = parseJsonObject(input.project.metadataJson);
    const loopConfig = parseJsonObject(input.loop.configJson);
    const projectReviewers = readStringArray(projectMetadata.reviewers);
    const loopReviewers = readStringArray(loopConfig.reviewers);
    const requested = [
      ...loopReviewers,
      ...projectReviewers,
      ...input.issue.assignees,
    ]
      .map((reviewer) => normalizeLogin(reviewer))
      .filter((reviewer): reviewer is string => Boolean(reviewer))
      .filter((reviewer) => reviewer !== normalizeLogin(input.currentUserLogin))
      .filter((reviewer, index, values) => values.indexOf(reviewer) === index);
    return requested;
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

  private createRunContext(loop: LoopRecord): ResumedRunContext {
    const latestRun = this.options.store.runs.listByLoop(loop.id)[0] ?? null;
    const nowIso = this.nowIso();
    const checkpoint = parseCheckpoint(latestRun?.checkpointJson);
    const lastCompletedStep = asPlannerStep(latestRun?.lastCompletedStep);
    const shouldResume =
      checkpoint.resumePolicy !== "manual_intervention" &&
      Boolean(latestRun) &&
      (latestRun?.status === "failed" || latestRun?.status === "interrupted") &&
      Boolean(lastCompletedStep);
    const startStep =
      shouldResume && lastCompletedStep
        ? (nextPlannerStep(lastCompletedStep) ?? "discover-issues")
        : "discover-issues";
    const resumed = shouldResume && startStep !== "discover-issues";

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
    step: PlannerStep,
    checkpoint: PlannerCheckpoint,
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
    step: PlannerStep,
    checkpoint: PlannerCheckpoint,
  ): RunRecord {
    const updated: RunRecord = {
      ...run,
      currentStep: nextPlannerStep(step),
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
      checkpoint: PlannerCheckpoint;
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
    fallback: PlannerCheckpoint,
  ): PlannerCheckpoint {
    const persistedRun = this.options.store.runs.getById(run.id);

    return (
      parseCheckpoint(persistedRun?.checkpointJson ?? run.checkpointJson) ??
      fallback
    );
  }

  private ensureLoopForIssue(input: {
    project: ProjectRecord;
    repo: string;
    issueNumber: number;
    issue: GitHubIssueSummary;
    currentUserLogin: string;
  }): { record: LoopRecord; created: boolean } {
    const existing = this.options.store.loops
      .list()
      .find(
        (loop) =>
          loop.type === "planner" &&
          loop.projectId === input.project.id &&
          loop.targetType === "issue" &&
          loop.targetId === buildIssueTargetId(input.repo, input.issueNumber),
      );

    const metadata = {
      issueTitle: input.issue.title,
      issueUrl: input.issue.url,
      issueNumber: input.issue.number,
      currentUserLogin: input.currentUserLogin,
      specPath: buildSpecPath(this.now(), input.issueNumber, input.issue.title),
    };
    const nowIso = this.nowIso();
    if (existing) {
      const isPausedOrCompleted =
        existing.status === "paused" || existing.status === "completed";
      const updated = {
        ...existing,
        repo: input.repo,
        status: isPausedOrCompleted
          ? existing.status
          : existing.status === "running"
            ? existing.status
            : "queued",
        metadataJson: JSON.stringify({
          ...parseJsonObject(existing.metadataJson),
          ...metadata,
        }),
        nextRunAt: isPausedOrCompleted ? existing.nextRunAt : nowIso,
        updatedAt: nowIso,
      };
      this.options.store.loops.upsert(updated);
      return { record: updated, created: false };
    }

    const loop: LoopRecord = {
      id: randomUUID(),
      seq: this.options.store.loops.allocateSeq(),
      projectId: input.project.id,
      type: "planner",
      targetType: "issue",
      targetId: buildIssueTargetId(input.repo, input.issueNumber),
      repo: input.repo,
      prNumber: null,
      status: "queued",
      configJson: null,
      metadataJson: JSON.stringify(metadata),
      lastRunAt: null,
      nextRunAt: nowIso,
      createdAt: nowIso,
      updatedAt: nowIso,
    };
    this.options.store.loops.upsert(loop);
    this.appendEvent({
      eventType: "loop.created",
      projectId: input.project.id,
      loopId: loop.id,
      entityType: "loop",
      entityId: loop.id,
      payload: {
        type: "planner",
        repo: input.repo,
        issueNumber: input.issueNumber,
      },
    });
    return { record: loop, created: true };
  }

  private buildSuccessSummary(
    loop: LoopRecord,
    checkpoint: PlannerCheckpoint,
  ): string {
    if (checkpoint.skipReason) {
      return checkpoint.skipReason;
    }
    if (checkpoint.publish?.pullRequest?.url) {
      return `Opened spec PR for planner ${loop.id}: ${checkpoint.publish.pullRequest.url}`;
    }

    return `Completed planner ${loop.id}`;
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

  private classifyFailure(error: unknown): PlannerLoopError {
    if (error instanceof PlannerLoopError) {
      return error;
    }
    if (error instanceof CommandExecutionError) {
      return new PlannerLoopError(error.message, "retryable_transient");
    }

    return new PlannerLoopError(
      error instanceof Error ? error.message : "Planner loop failed",
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
      actorId: "planner-loop",
      actorDisplayName: "planner-loop",
      payloadJson: JSON.stringify(input.payload),
      createdAt: this.nowIso(),
    });
  }

  private nowIso(): string {
    return this.now().toISOString();
  }
}

function nextPlannerStep(step: PlannerStep): PlannerStep | null {
  const index = PLANNER_STEP_SEQUENCE.indexOf(step);
  return PLANNER_STEP_SEQUENCE[index + 1] ?? null;
}

function asPlannerStep(value: string | null | undefined): PlannerStep | null {
  return PLANNER_STEP_SEQUENCE.includes(value as PlannerStep)
    ? (value as PlannerStep)
    : null;
}

function parseCheckpoint(value?: string | null): PlannerCheckpoint {
  if (!value) {
    return {};
  }

  return parseJsonObject(value) as PlannerCheckpoint;
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

function readString(value: unknown): string | null {
  return typeof value === "string" && value.trim().length > 0
    ? value.trim()
    : null;
}

function readNumber(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function readStringArray(value: unknown): string[] {
  return Array.isArray(value)
    ? value
        .map((item) => readString(item))
        .filter((item): item is string => Boolean(item))
    : [];
}

function normalizeLogin(value: string | null | undefined): string | undefined {
  return value?.trim().toLowerCase() || undefined;
}

function normalizeLabel(value: string): string {
  return value.trim().toLowerCase();
}

function includesLogin(values: string[], target: string): boolean {
  const normalizedTarget = normalizeLogin(target);
  return values.some((value) => normalizeLogin(value) === normalizedTarget);
}

function shouldClaimIssue(
  issue: GitHubIssueSummary,
  currentLogin: string,
): boolean {
  return (
    includesLogin(issue.assignees, currentLogin) &&
    issue.labels.some((label) => normalizeLabel(label) === DISCOVERY_LABEL)
  );
}

function readProjectRepo(project: ProjectRecord): string | null {
  return readString(parseJsonObject(project.metadataJson).repo);
}

function buildIssueTargetId(repo: string, issueNumber: number): string {
  return `issue:${repo}:${issueNumber}`;
}

function buildPlannerDedupeKey(repo: string, issueNumber: number): string {
  return `planner:${repo}:${issueNumber}`;
}

function buildIssueLockKey(repo: string, issueNumber: number): string {
  return `issue:${repo}:${issueNumber}`;
}

function parseIssueNumberFromTargetId(targetId?: string | null): number | null {
  if (!targetId) {
    return null;
  }
  const match = /^issue:[^:]+\/[^:]+:(\d+)$/.exec(targetId);
  if (!match) {
    return null;
  }

  return Number(match[1]);
}

function requireIssue(
  checkpoint: PlannerCheckpoint,
): NonNullable<PlannerCheckpoint["issue"]> {
  if (!checkpoint.issue) {
    throw new PlannerLoopError(
      "Missing issue checkpoint for planner step",
      "retryable_transient",
    );
  }

  return checkpoint.issue;
}

function requireWorktree(
  checkpoint: PlannerCheckpoint,
): NonNullable<PlannerCheckpoint["worktree"]> {
  if (!checkpoint.worktree) {
    throw new PlannerLoopError(
      "Missing worktree checkpoint for planner step",
      "retryable_transient",
    );
  }

  return checkpoint.worktree;
}

function slugify(value: string): string {
  const slug = value
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return slug || "issue";
}

function buildPlannerSlug(title: string, maxWords = 4): string {
  const words = slugify(title)
    .split("-")
    .filter((word) => word.length > 0)
    .slice(0, maxWords);
  return words.join("-") || "issue";
}

function formatDatePrefix(value: Date): string {
  return value.toISOString().slice(0, 10);
}

function buildPlannerBranch(issueNumber: number, title: string): string {
  return `looper/planner/${issueNumber}-${buildPlannerSlug(title)}`;
}

function buildSpecPath(now: Date, issueNumber: number, title: string): string {
  return `specs/${formatDatePrefix(now)}-${issueNumber}-${buildPlannerSlug(title)}.md`;
}

async function buildPlannerPrompt(input: {
  project: ProjectRecord;
  issue: PlannerIssueState;
  worktree: NonNullable<PlannerCheckpoint["worktree"]>;
}): Promise<string> {
  const agentsBlock = await readAgentsBlock(input.project.repoPath);
  return appendCompletionInstruction(
    [
      `Write a planning spec for GitHub issue ${input.issue.repo}#${input.issue.issueNumber}.`,
      `Repository: ${input.issue.repo}`,
      `Base branch: ${input.worktree.baseBranch}`,
      `Spec path: ${input.issue.specPath}`,
      `Issue title: ${input.issue.title}`,
      input.issue.body ? `Issue body:\n${input.issue.body}` : null,
      input.issue.url ? `Issue URL: ${input.issue.url}` : null,
      agentsBlock,
      [
        "Requirements:",
        `- Create or update the spec at ${input.issue.specPath}`,
        "- Use Markdown with clear problem, goals, approach, risks, and validation sections",
        "- Keep the implementation scope aligned to the issue",
        "- Commit the spec changes on the current branch so the PR can be opened",
      ].join("\n"),
    ]
      .filter((value): value is string => Boolean(value))
      .join("\n\n"),
  );
}

async function readAgentsBlock(
  projectRepoPath: string,
): Promise<string | null> {
  try {
    const content = await readFile(join(projectRepoPath, "AGENTS.md"), "utf8");
    return `AGENTS.md:\n${content}`;
  } catch {
    return null;
  }
}

function buildPlannerPullRequestBody(input: {
  issue: PlannerIssueState;
  branch: string;
  executionSummary?: string;
}): string {
  return [
    "## Summary",
    `- Adds the planning spec for ${input.issue.repo}#${input.issue.issueNumber}`,
    `- Spec path: ${input.issue.specPath}`,
    `- Source issue: ${input.issue.url ?? `${input.issue.repo}#${input.issue.issueNumber}`}`,
    `- Planner branch: ${input.branch}`,
    input.executionSummary
      ? `\n## Agent Summary\n${input.executionSummary}`
      : null,
    `\nSpec: ${input.issue.specPath}`,
    `Issue: ${input.issue.repo}#${input.issue.issueNumber}`,
  ]
    .filter((value): value is string => Boolean(value))
    .join("\n");
}
