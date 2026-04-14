import { randomUUID } from "node:crypto";
import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import type { Logger } from "../bootstrap/logger";
import type { LooperConfig } from "../config/index";
import { FixerLoopRunner } from "../fixer/index";
import type { AgentResult } from "../infra/agent";
import {
  ConfiguredAgentExecutor,
  GhCliGitHubGateway,
  GitWorktreeGateway,
  NotificationGateway,
} from "../infra/index";
import { PlannerLoopRunner } from "../planner/index";
import { ProjectManager } from "../projects/index";
import { ReviewerLoopRunner } from "../reviewer/index";
import { SchedulerQueue } from "../scheduler/index";
import { type LooperdApiServer, createLooperdApiServer } from "../server/index";
import { SqliteStore } from "../storage/sqlite/sqlite-store";
import type {
  AgentExecutionRecord,
  EventLogRecord,
  LoopRecord,
  QueueItemRecord,
  RunRecord,
} from "../storage/types";
import { WorkerLoopRunner } from "../worker/index";

interface RuntimeAgentExecution {
  wait(): Promise<AgentResult>;
}

interface RuntimeAgentExecutor {
  start(
    input: Parameters<ConfiguredAgentExecutor["start"]>[0],
  ): Promise<RuntimeAgentExecution>;
}

export interface RecoverySummary {
  startedAt?: string;
  completedAt?: string;
  orphanAgentCleanup: {
    attempted: boolean;
    cleanedCount: number;
    warning?: string;
  };
  expiredLocksReleased: number;
  interruptedRunsMarked: number;
  loopsRequeued: number;
  eventsWritten: number;
}

export interface LooperdRuntime {
  start(): Promise<void>;
  stop(reason?: string): Promise<void>;
  triggerSchedulerTick(): void;
  waitForShutdown(): Promise<void>;
  readonly startedAt?: Date;
}

export interface CreateLooperdRuntimeOptions {
  config: LooperConfig;
  logger: Logger;
  github?: Pick<
    GhCliGitHubGateway,
    | "listOpenIssues"
    | "viewIssue"
    | "listOpenPullRequests"
    | "getCurrentUserLogin"
    | "viewPullRequest"
    | "resolveReviewThread"
    | "capturePullRequestSnapshot"
    | "submitReview"
    | "createPullRequest"
    | "addPullRequestLabels"
    | "removePullRequestLabels"
    | "addPullRequestReviewers"
  >;
  git?: Pick<
    GitWorktreeGateway,
    | "detectGitHubRepo"
    | "push"
    | "createWorktree"
    | "prepareWorktree"
    | "inspectHead"
    | "commit"
    | "cleanupWorktree"
  >;
  agentExecutor?: RuntimeAgentExecutor;
  plannerRunner?: PlannerLoopRunner;
  reviewerRunner?: ReviewerLoopRunner;
  fixerRunner?: FixerLoopRunner;
  workerRunner?: WorkerLoopRunner;
  enableReviewer?: boolean;
  enableFixer?: boolean;
  enablePlanner?: boolean;
}

const STOP_IN_FLIGHT_SCHEDULER_WORK_TIMEOUT_MS = 1_000;

const MIGRATIONS_DIR = resolveMigrationsDir();

function resolveMigrationsDir(): string {
  const currentDir = dirname(fileURLToPath(import.meta.url));
  const distPath = join(currentDir, "../storage/sqlite/migrations");
  if (existsSync(distPath)) {
    return distPath;
  }

  return join(currentDir, "../../src/storage/sqlite/migrations");
}

class BasicLooperdRuntime implements LooperdRuntime {
  public startedAt?: Date;

  private readonly shutdownPromise: Promise<void>;
  private resolveShutdown!: () => void;
  private stopped = false;
  private store?: SqliteStore;
  private server?: LooperdApiServer;
  private scheduler?: SchedulerQueue;
  private git?: CreateLooperdRuntimeOptions["git"];
  private plannerRunner?: PlannerLoopRunner;
  private reviewerRunner?: ReviewerLoopRunner;
  private fixerRunner?: FixerLoopRunner;
  private workerRunner?: WorkerLoopRunner;
  private schedulerTimer?: ReturnType<typeof setInterval>;
  private schedulerTickRunning = false;
  private readonly inFlightSchedulerWork = new Map<string, Promise<void>>();
  private recoverySummary: RecoverySummary = createEmptyRecoverySummary();

  constructor(private readonly options: CreateLooperdRuntimeOptions) {
    this.shutdownPromise = new Promise<void>((resolve) => {
      this.resolveShutdown = resolve;
    });
  }

  public async start(): Promise<void> {
    if (this.startedAt) {
      return;
    }

    const store = new SqliteStore({
      dbPath: this.options.config.storage.dbPath,
      backupDir: this.options.config.storage.backupDir,
      migrationsDir: MIGRATIONS_DIR,
    });

    try {
      store.initialize({
        autoMigrate: this.options.config.package.autoMigrateOnStartup,
        requireBackup: this.options.config.package.requireBackupBeforeMigrate,
      });

      this.store = store;
      this.syncConfiguredProjects();
      this.recoverySummary = this.runRecoveryPipeline();

      this.scheduler = new SchedulerQueue({
        store,
        retryMaxAttempts: this.options.config.scheduler.retryMaxAttempts,
        retryBaseDelayMs: this.options.config.scheduler.retryBaseDelayMs,
      });

      const github =
        this.options.github ??
        (this.options.config.tools.ghPath
          ? new GhCliGitHubGateway({
              ghPath: this.options.config.tools.ghPath,
            })
          : undefined);
      const git =
        this.options.git ??
        (this.options.config.tools.gitPath
          ? new GitWorktreeGateway({
              gitPath: this.options.config.tools.gitPath,
              store,
            })
          : undefined);
      const agentExecutor = isAgentConfigured(this.options.config)
        ? (this.options.agentExecutor ??
          new ConfiguredAgentExecutor({
            config: this.options.config.agent,
            store,
          }))
        : undefined;
      this.git = git;

      if (github && git && agentExecutor) {
        this.plannerRunner =
          this.options.plannerRunner ??
          new PlannerLoopRunner({
            store,
            scheduler: this.scheduler,
            git,
            github,
            agentExecutor,
            logger: this.options.logger,
            onAgentExecutionStarted: (input) =>
              this.notifySystemEvent({
                projectId: input.projectId,
                loopId: input.loopId,
                runId: input.runId,
                level: "info",
                title: "Looper Planner",
                subtitle: input.subtitle,
                body: input.body,
                entityType: "agent_execution",
                entityId: input.executionId,
                dedupeKey: input.dedupeKey,
              }),
            allowAutoPush: this.options.config.defaults.allowAutoPush,
          });
      }

      if (github && agentExecutor && this.options.enableReviewer !== false) {
        this.reviewerRunner =
          this.options.reviewerRunner ??
          new ReviewerLoopRunner({
            store,
            scheduler: this.scheduler,
            github,
            agentExecutor,
            logger: this.options.logger,
            onAgentExecutionStarted: (input) =>
              this.notifySystemEvent({
                projectId: input.projectId,
                loopId: input.loopId,
                runId: input.runId,
                level: "info",
                title: "Looper Reviewer",
                subtitle: input.subtitle,
                body: input.body,
                entityType: "agent_execution",
                entityId: input.executionId,
                dedupeKey: input.dedupeKey,
              }),
            allowAutoApprove: this.options.config.defaults.allowAutoApprove,
          });
      }

      if (
        github &&
        git &&
        agentExecutor &&
        this.options.enableFixer !== false
      ) {
        this.fixerRunner =
          this.options.fixerRunner ??
          new FixerLoopRunner({
            store,
            scheduler: this.scheduler,
            github,
            git,
            agentExecutor,
            logger: this.options.logger,
            onAgentExecutionStarted: (input) =>
              this.notifySystemEvent({
                projectId: input.projectId,
                loopId: input.loopId,
                runId: input.runId,
                level: "info",
                title: "Looper Fixer",
                subtitle: input.subtitle,
                body: input.body,
                entityType: "agent_execution",
                entityId: input.executionId,
                dedupeKey: input.dedupeKey,
              }),
            allowAutoCommit: this.options.config.defaults.allowAutoCommit,
            allowAutoPush: this.options.config.defaults.allowAutoPush,
            allowRiskyFixes: this.options.config.defaults.allowRiskyFixes,
          });
      }

      if (github && git && agentExecutor) {
        this.workerRunner =
          this.options.workerRunner ??
          new WorkerLoopRunner({
            store,
            scheduler: this.scheduler,
            github,
            git,
            agentExecutor,
            logger: this.options.logger,
            onAgentExecutionStarted: (input) =>
              this.notifySystemEvent({
                projectId: input.projectId,
                loopId: input.loopId,
                runId: input.runId,
                level: "info",
                title: "Looper Worker",
                subtitle: input.subtitle,
                body: input.body,
                entityType: "agent_execution",
                entityId: input.executionId,
                dedupeKey: input.dedupeKey,
              }),
            allowAutoCommit: this.options.config.defaults.allowAutoCommit,
            allowAutoPush: this.options.config.defaults.allowAutoPush,
            openPrStrategy: this.options.config.defaults.openPrStrategy,
          });
      }

      const projects =
        this.options.config.tools.gitPath && this.options.config.tools.ghPath
          ? new ProjectManager({
              store,
              logger: this.options.logger,
              git: new GitWorktreeGateway({
                gitPath: this.options.config.tools.gitPath,
                store,
              }),
              github: new GhCliGitHubGateway({
                ghPath: this.options.config.tools.ghPath,
              }),
            })
          : undefined;

      this.server = createLooperdApiServer({
        config: this.options.config,
        logger: this.options.logger,
        store,
        projects,
        runtimeControl: {
          stopLoop: (input) => this.stopLoop(input),
          triggerSchedulerTick: () => this.triggerSchedulerTick(),
        },
        getStartedAt: () => this.startedAt,
        getRecoverySummary: () => ({ ...this.recoverySummary }),
      });
      await this.server.start();
      this.options.config.server.port =
        this.server.getPort() ?? this.options.config.server.port;

      this.startedAt = new Date();
      await this.runSchedulerTick();
      this.startSchedulerLoop();

      this.appendEvent({
        id: randomUUID(),
        eventType: "looperd.started",
        entityType: "notification",
        entityId: "looperd",
        payloadJson: JSON.stringify({
          daemonMode: this.options.config.daemon.mode,
          host: this.options.config.server.host,
          port: this.server.getPort() ?? this.options.config.server.port,
          recovery: this.recoverySummary,
        }),
        createdAt: this.startedAt.toISOString(),
      });

      this.options.logger.info("looperd runtime started", {
        daemonMode: this.options.config.daemon.mode,
        host: this.options.config.server.host,
        port: this.server.getPort() ?? this.options.config.server.port,
        recoverySummary: this.recoverySummary,
      });

      await this.notifySystemEvent({
        level: "success",
        title: "Looper",
        subtitle: "Service",
        body: "Started",
        entityType: "notification",
        entityId: "looperd.started",
        dedupeKey: "looperd.started:system:looperd",
      });

      if (
        this.recoverySummary.interruptedRunsMarked > 0 ||
        this.recoverySummary.orphanAgentCleanup.cleanedCount > 0
      ) {
        await this.notifySystemEvent({
          level: "info",
          title: "Looper",
          subtitle: "Service",
          body: "Recovery completed",
          entityType: "notification",
          entityId: "looperd.recovered",
          dedupeKey: "looperd.recovered:system:looperd",
        });
      }
    } catch (error) {
      this.server = undefined;
      this.store?.close();
      this.store = undefined;
      throw error;
    }
  }

  public async stop(reason = "shutdown"): Promise<void> {
    if (this.stopped) {
      return;
    }

    this.stopped = true;
    this.options.logger.info("looperd runtime stopping", { reason });

    const stoppedAt = new Date().toISOString();

    try {
      this.appendEvent({
        id: randomUUID(),
        eventType: "looperd.stopped",
        entityType: "notification",
        entityId: "looperd",
        payloadJson: JSON.stringify({ reason }),
        createdAt: stoppedAt,
      });
      if (this.schedulerTimer) {
        clearInterval(this.schedulerTimer);
        this.schedulerTimer = undefined;
      }
      await this.server?.stop();
      await this.waitForInFlightSchedulerWorkOnStop();
    } finally {
      this.store?.close();
      this.server = undefined;
      this.scheduler = undefined;
      this.git = undefined;
      this.plannerRunner = undefined;
      this.reviewerRunner = undefined;
      this.fixerRunner = undefined;
      this.workerRunner = undefined;
      this.store = undefined;
      this.resolveShutdown();
    }
  }

  private async waitForInFlightSchedulerWorkOnStop(): Promise<void> {
    if (this.inFlightSchedulerWork.size === 0) {
      return;
    }

    const inFlightWork = [...this.inFlightSchedulerWork.entries()];
    let timeoutHandle: ReturnType<typeof setTimeout> | undefined;

    const completed = await Promise.race([
      Promise.allSettled(
        inFlightWork.map(([, workPromise]) => workPromise),
      ).then(() => true),
      new Promise<boolean>((resolve) => {
        timeoutHandle = setTimeout(
          () => resolve(false),
          STOP_IN_FLIGHT_SCHEDULER_WORK_TIMEOUT_MS,
        );
      }),
    ]);

    if (timeoutHandle) {
      clearTimeout(timeoutHandle);
    }

    if (!completed) {
      this.options.logger.warn(
        "looperd stop timed out waiting for in-flight scheduler work",
        {
          timeoutMs: STOP_IN_FLIGHT_SCHEDULER_WORK_TIMEOUT_MS,
          queueItemIds: inFlightWork.map(([queueItemId]) => queueItemId),
        },
      );
    }
  }

  private async stopLoop(input: { loopId: string; reason: string }): Promise<{
    stopped: boolean;
    loopId: string;
    runId?: string;
    executionId?: string;
    vendor?: string;
    pid?: number | null;
  }> {
    if (!this.store) {
      throw new Error("Runtime is not started");
    }

    const loop = this.store.loops.getById(input.loopId);
    if (!loop) {
      throw new Error(`Loop not found: ${input.loopId}`);
    }

    const nowIso = new Date().toISOString();
    this.store.loops.upsert({
      ...loop,
      status: "paused",
      nextRunAt: null,
      updatedAt: nowIso,
    });
    const cancelledQueueItems = this.store.queue.cancelByLoop(
      loop.id,
      nowIso,
      input.reason,
    );

    const activeExecution = this.store.agentExecutions
      .listActive()
      .find((execution) => execution.loopId === loop.id);
    const activeRun = this.store.runs
      .listByLoop(loop.id)
      .find((run) => run.status === "running");

    let stopRequested = false;
    let executionTerminated = false;

    if (activeExecution?.pid) {
      try {
        process.kill(activeExecution.pid, "SIGTERM");
        stopRequested = true;
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ESRCH") {
          stopRequested = true;
          executionTerminated = true;
        } else {
          this.options.logger.warn("failed to stop active agent execution", {
            loopId: loop.id,
            executionId: activeExecution.id,
            pid: activeExecution.pid,
            error: error instanceof Error ? error.message : String(error),
          });
        }
      }

      this.store.agentExecutions.upsert({
        ...activeExecution,
        status: executionTerminated
          ? "killed"
          : stopRequested
            ? "cancelling"
            : activeExecution.status,
        errorMessage: stopRequested
          ? input.reason
          : activeExecution.errorMessage,
        endedAt: executionTerminated ? nowIso : activeExecution.endedAt,
        updatedAt: nowIso,
      });
    }

    if (activeRun && executionTerminated) {
      this.store.runs.upsert({
        ...activeRun,
        status: "cancelled",
        errorMessage: input.reason,
        endedAt: nowIso,
        updatedAt: nowIso,
      });
    }

    const runStopped = Boolean(activeRun && executionTerminated);
    const stopped =
      stopRequested || runStopped || (!activeRun && cancelledQueueItems > 0);

    this.appendEvent({
      id: randomUUID(),
      eventType: "loop.stopped",
      projectId: loop.projectId,
      loopId: loop.id,
      runId: activeRun?.id ?? null,
      entityType: "loop",
      entityId: loop.id,
      actorType: "user",
      actorId: "cli",
      actorDisplayName: "cli",
      payloadJson: JSON.stringify({
        reason: input.reason,
        executionId: activeExecution?.id,
        vendor: activeExecution?.vendor,
        pid: activeExecution?.pid ?? null,
        stopped,
      }),
      createdAt: nowIso,
    });

    return {
      stopped,
      loopId: loop.id,
      runId: activeRun?.id,
      executionId: activeExecution?.id,
      vendor: activeExecution?.vendor,
      pid: activeExecution?.pid ?? null,
    };
  }

  public async waitForShutdown(): Promise<void> {
    await this.shutdownPromise;
  }

  public triggerSchedulerTick(): void {
    if (this.stopped) {
      return;
    }

    queueMicrotask(() => {
      void this.runSchedulerTick();
    });
  }

  private syncConfiguredProjects(): void {
    if (!this.store) {
      return;
    }

    const now = new Date().toISOString();

    for (const project of this.options.config.projects) {
      const existing = this.store.projects.getById(project.id);
      const metadata = parseProjectMetadata(existing?.metadataJson);
      const repoPathChanged =
        existing !== null && existing.repoPath !== project.repoPath;
      this.store.projects.upsert({
        id: project.id,
        name: project.name,
        repoPath: project.repoPath,
        baseBranch:
          project.baseBranch ?? this.options.config.defaults.baseBranch,
        archived: false,
        metadataJson: JSON.stringify({
          ...metadata,
          repo: repoPathChanged ? null : readMetadataString(metadata.repo),
          worktreeRoot: project.worktreeRoot ?? null,
          source: "config",
        }),
        createdAt: existing?.createdAt ?? now,
        updatedAt: now,
      });
    }
  }

  private runRecoveryPipeline(): RecoverySummary {
    if (!this.store) {
      return createEmptyRecoverySummary();
    }

    const now = new Date();
    const nowIso = now.toISOString();
    let eventsWritten = 0;

    const summary: RecoverySummary = {
      startedAt: nowIso,
      completedAt: undefined,
      orphanAgentCleanup: {
        attempted: true,
        cleanedCount: 0,
      },
      expiredLocksReleased: 0,
      interruptedRunsMarked: 0,
      loopsRequeued: 0,
      eventsWritten: 0,
    };

    const runningExecutions = this.store.agentExecutions.listActive();
    for (const execution of runningExecutions) {
      const cleaned = this.tryCleanupOrphanExecution(execution, nowIso);
      if (cleaned) {
        summary.orphanAgentCleanup.cleanedCount += 1;
        eventsWritten += 1;
      }
    }

    const expiredLocks = this.store.locks.listExpired(nowIso);
    for (const lock of expiredLocks) {
      this.store.locks.release(lock.key);
      summary.expiredLocksReleased += 1;
      this.appendEvent({
        id: randomUUID(),
        eventType: "looperd.recovery.lock_released",
        entityType: "lock",
        entityId: lock.key,
        payloadJson: JSON.stringify({
          owner: lock.owner,
          expiredAt: lock.expiresAt,
          recoveredAt: nowIso,
        }),
        createdAt: nowIso,
      });
      eventsWritten += 1;
    }

    const loops = this.store.loops.list();
    for (const loop of loops) {
      const latestRun = this.store.runs.listByLoop(loop.id)[0];
      if (!latestRun) {
        continue;
      }

      if (latestRun.status === "running") {
        this.store.runs.upsert({
          ...latestRun,
          status: "interrupted",
          errorMessage:
            latestRun.errorMessage ?? "Interrupted during looperd recovery",
          endedAt: nowIso,
          updatedAt: nowIso,
        });
        summary.interruptedRunsMarked += 1;
        this.appendEvent({
          id: randomUUID(),
          eventType: "looperd.recovery.run_interrupted",
          loopId: loop.id,
          runId: latestRun.id,
          entityType: "run",
          entityId: latestRun.id,
          payloadJson: JSON.stringify({
            previousStatus: latestRun.status,
            recoveredStatus: "interrupted",
          }),
          createdAt: nowIso,
        });
        eventsWritten += 1;
      }

      if (shouldRequeueLoop(loop, latestRun)) {
        this.store.loops.upsert({
          ...loop,
          status: "queued",
          nextRunAt: nowIso,
          lastRunAt: latestRun.endedAt ?? latestRun.startedAt,
          updatedAt: nowIso,
        });
        const recoveredQueueItems = this.store.queue.requeueRunningByLoop(
          loop.id,
          nowIso,
        );
        summary.loopsRequeued += 1;
        this.appendEvent({
          id: randomUUID(),
          eventType: "looperd.recovery.loop_requeued",
          loopId: loop.id,
          entityType: "loop",
          entityId: loop.id,
          payloadJson: JSON.stringify({
            previousStatus: loop.status,
            nextRunAt: nowIso,
            recoveredQueueItems,
          }),
          createdAt: nowIso,
        });
        eventsWritten += 1;
      }
    }

    summary.completedAt = nowIso;

    this.appendEvent({
      id: randomUUID(),
      eventType: "looperd.recovery.completed",
      entityType: "notification",
      entityId: "looperd-recovery",
      payloadJson: JSON.stringify({
        expiredLocksReleased: summary.expiredLocksReleased,
        interruptedRunsMarked: summary.interruptedRunsMarked,
        loopsRequeued: summary.loopsRequeued,
        orphanAgentCleanup: summary.orphanAgentCleanup,
      }),
      createdAt: nowIso,
    });
    eventsWritten += 1;
    summary.eventsWritten = eventsWritten;

    return summary;
  }

  private tryCleanupOrphanExecution(
    execution: AgentExecutionRecord,
    nowIso: string,
  ): boolean {
    if (!execution.pid) {
      return false;
    }

    try {
      process.kill(execution.pid, "SIGTERM");
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ESRCH") {
        this.options.logger.warn("failed to cleanup orphan agent execution", {
          executionId: execution.id,
          pid: execution.pid,
          error: error instanceof Error ? error.message : String(error),
        });
        return false;
      }
    }

    this.store?.agentExecutions.upsert({
      ...execution,
      status: "killed",
      errorMessage: execution.errorMessage ?? "Killed during looperd recovery",
      endedAt: nowIso,
      updatedAt: nowIso,
    });
    this.appendEvent({
      id: randomUUID(),
      eventType: "agent.killed",
      projectId: execution.projectId ?? undefined,
      loopId: execution.loopId ?? undefined,
      runId: execution.runId ?? undefined,
      entityType: "agent_execution",
      entityId: execution.id,
      payloadJson: JSON.stringify({
        pid: execution.pid,
        recoveredAt: nowIso,
      }),
      createdAt: nowIso,
    });
    return true;
  }

  private async notifySystemEvent(input: {
    projectId?: string;
    loopId?: string;
    runId?: string;
    level: "info" | "warning" | "action_required" | "success" | "failure";
    title: string;
    subtitle: string;
    body: string;
    entityType: string;
    entityId: string;
    dedupeKey: string;
  }): Promise<void> {
    if (!this.store) {
      return;
    }

    const gateway = new NotificationGateway({
      config: this.options.config.notifications,
      osascriptPath: this.options.config.tools.osascriptPath,
      logFilePath: join(this.options.config.daemon.logDir, "looperd.log"),
      store: this.store,
    });
    await gateway.notify(input);
  }

  private appendEvent(record: EventLogRecord): void {
    this.store?.events.append({
      projectId: null,
      loopId: null,
      runId: null,
      correlationId: null,
      causationId: null,
      actorType: "system",
      actorId: "looperd",
      actorDisplayName: "looperd",
      ...record,
    });
  }

  private startSchedulerLoop(): void {
    const pollIntervalMs =
      this.options.config.scheduler.pollIntervalSeconds * 1_000;
    this.schedulerTimer = setInterval(() => {
      void this.runSchedulerTick();
    }, pollIntervalMs);
  }

  private async runSchedulerTick(): Promise<void> {
    if (this.schedulerTickRunning || !this.store || !this.scheduler) {
      return;
    }

    this.schedulerTickRunning = true;
    try {
      await this.discoverIssues();
      await this.discoverPullRequests();
      await this.processScheduledWork();
    } catch (error) {
      this.options.logger.warn("looperd scheduler tick failed", {
        error: error instanceof Error ? error.message : String(error),
      });
    } finally {
      this.schedulerTickRunning = false;
    }
  }

  private async discoverIssues(): Promise<void> {
    if (!this.store || !this.plannerRunner) {
      return;
    }

    await this.plannerRunner.discoverIssues();
  }

  private async discoverPullRequests(): Promise<void> {
    if (!this.store) {
      return;
    }

    for (const project of this.store.projects.list()) {
      if (project.archived) {
        continue;
      }

      const repo = await this.resolveProjectRepo(project);
      if (!repo) {
        continue;
      }

      if (this.reviewerRunner) {
        await this.reviewerRunner.discoverPullRequests({
          projectId: project.id,
          repo,
        });
      }

      if (this.fixerRunner) {
        await this.fixerRunner.discoverPullRequests({
          projectId: project.id,
          repo,
        });
      }

      if (this.workerRunner) {
        await this.workerRunner.discoverPullRequests({
          projectId: project.id,
          repo,
        });
      }
    }
  }

  private async processScheduledWork(): Promise<void> {
    if (!this.scheduler) {
      return;
    }

    const availableSlots =
      this.options.config.scheduler.maxConcurrentRuns -
      this.inFlightSchedulerWork.size;

    for (let count = 0; count < availableSlots; count += 1) {
      const claimed = this.scheduler.claimNext("looperd-runtime");
      if (!claimed) {
        return;
      }

      const workPromise = this.runClaimedItem(claimed)
        .catch((error) => {
          this.options.logger.warn("looperd scheduled work failed", {
            queueItemId: claimed.id,
            type: claimed.type,
            error: error instanceof Error ? error.message : String(error),
          });
        })
        .finally(() => {
          this.inFlightSchedulerWork.delete(claimed.id);
          this.triggerScheduledWorkDrain();
        });
      this.inFlightSchedulerWork.set(claimed.id, workPromise);
    }
  }

  private triggerScheduledWorkDrain(): void {
    if (this.stopped) {
      return;
    }

    queueMicrotask(() => {
      void this.processScheduledWork();
    });
  }

  private async runClaimedItem(claimed: QueueItemRecord): Promise<void> {
    if (claimed.type === "planner" && this.plannerRunner) {
      try {
        const result = await this.plannerRunner.processClaimedItem(claimed);
        if (
          result.status === "failed" &&
          shouldNotifyRunFailure(claimed.type, result)
        ) {
          void this.notifySystemEvent({
            projectId: claimed.projectId ?? undefined,
            loopId: result.loopId,
            runId: result.runId,
            level: "failure",
            title: "Looper Planner",
            subtitle: claimed.targetId,
            body: `Run failed: ${result.summary}`,
            entityType: "run",
            entityId: result.runId,
            dedupeKey: `runtime.run.failed:planner:${result.runId}`,
          });
        }
      } catch (error) {
        void this.notifySystemEvent({
          projectId: claimed.projectId ?? undefined,
          loopId: claimed.loopId ?? undefined,
          level: "failure",
          title: "Looper Planner",
          subtitle: claimed.targetId,
          body: `Scheduling failed: ${error instanceof Error ? error.message : String(error)}`,
          entityType: "queue_item",
          entityId: claimed.id,
          dedupeKey: `runtime.queue.failed:planner:${claimed.id}`,
        });
        throw error;
      }
      return;
    }

    if (claimed.type === "reviewer" && this.reviewerRunner) {
      try {
        const result = await this.reviewerRunner.processClaimedItem(claimed);
        if (
          result.status === "failed" &&
          shouldNotifyRunFailure(claimed.type, result)
        ) {
          void this.notifySystemEvent({
            projectId: claimed.projectId ?? undefined,
            loopId: result.loopId,
            runId: result.runId,
            level: "failure",
            title: "Looper Reviewer",
            subtitle: `${claimed.repo}#${claimed.prNumber}`,
            body: `Run failed: ${result.summary}`,
            entityType: "run",
            entityId: result.runId,
            dedupeKey: `runtime.run.failed:reviewer:${result.runId}`,
          });
        }
      } catch (error) {
        void this.notifySystemEvent({
          projectId: claimed.projectId ?? undefined,
          loopId: claimed.loopId ?? undefined,
          level: "failure",
          title: "Looper Reviewer",
          subtitle: `${claimed.repo}#${claimed.prNumber}`,
          body: `Scheduling failed: ${error instanceof Error ? error.message : String(error)}`,
          entityType: "queue_item",
          entityId: claimed.id,
          dedupeKey: `runtime.queue.failed:reviewer:${claimed.id}`,
        });
        throw error;
      }
      return;
    }

    if (claimed.type === "fixer" && this.fixerRunner) {
      try {
        const result = await this.fixerRunner.processClaimedItem(claimed);
        if (
          result.status === "failed" &&
          shouldNotifyRunFailure(claimed.type, result)
        ) {
          void this.notifySystemEvent({
            projectId: claimed.projectId ?? undefined,
            loopId: result.loopId,
            runId: result.runId,
            level: "failure",
            title: "Looper Fixer",
            subtitle: `${claimed.repo}#${claimed.prNumber}`,
            body: `Run failed: ${result.summary}`,
            entityType: "run",
            entityId: result.runId,
            dedupeKey: `runtime.run.failed:fixer:${result.runId}`,
          });
        }
      } catch (error) {
        void this.notifySystemEvent({
          projectId: claimed.projectId ?? undefined,
          loopId: claimed.loopId ?? undefined,
          level: "failure",
          title: "Looper Fixer",
          subtitle: `${claimed.repo}#${claimed.prNumber}`,
          body: `Scheduling failed: ${error instanceof Error ? error.message : String(error)}`,
          entityType: "queue_item",
          entityId: claimed.id,
          dedupeKey: `runtime.queue.failed:fixer:${claimed.id}`,
        });
        throw error;
      }
      return;
    }

    if (claimed.type === "worker" && this.workerRunner) {
      try {
        const result = await this.workerRunner.processClaimedItem(claimed);
        if (
          result.status === "failed" &&
          shouldNotifyRunFailure(claimed.type, result)
        ) {
          void this.notifySystemEvent({
            projectId: claimed.projectId ?? undefined,
            loopId: result.loopId,
            runId: result.runId,
            level: "failure",
            title: "Looper Worker",
            subtitle: claimed.targetId,
            body: `Run failed: ${result.summary}`,
            entityType: "run",
            entityId: result.runId,
            dedupeKey: `runtime.run.failed:worker:${result.runId}`,
          });
        }
      } catch (error) {
        void this.notifySystemEvent({
          projectId: claimed.projectId ?? undefined,
          loopId: claimed.loopId ?? undefined,
          level: "failure",
          title: "Looper Worker",
          subtitle: claimed.targetId,
          body: `Scheduling failed: ${error instanceof Error ? error.message : String(error)}`,
          entityType: "queue_item",
          entityId: claimed.id,
          dedupeKey: `runtime.queue.failed:worker:${claimed.id}`,
        });
        throw error;
      }
      return;
    }

    this.options.logger.warn("claimed unsupported scheduler item", {
      queueItemId: claimed.id,
      type: claimed.type,
    });
    this.scheduler?.fail(
      claimed.id,
      "non_retryable",
      `No runtime runner configured for queue item type: ${claimed.type}`,
    );
  }

  private async resolveProjectRepo(
    project: ReturnType<SqliteStore["projects"]["list"]>[number],
  ): Promise<string | null> {
    const metadata = parseProjectMetadata(project.metadataJson);
    const existingRepo = readMetadataString(metadata.repo);
    if (existingRepo) {
      return existingRepo;
    }

    if (!this.git) {
      return null;
    }

    try {
      const detectedRepo = await this.git.detectGitHubRepo(project.repoPath);
      if (!detectedRepo || !this.store) {
        return detectedRepo;
      }

      this.store.projects.upsert({
        ...project,
        metadataJson: JSON.stringify({
          ...metadata,
          repo: detectedRepo,
        }),
        updatedAt: new Date().toISOString(),
      });
      return detectedRepo;
    } catch (error) {
      this.options.logger.warn("failed to resolve project repo for scheduler", {
        projectId: project.id,
        repoPath: project.repoPath,
        error: error instanceof Error ? error.message : String(error),
      });
      return null;
    }
  }
}

function createEmptyRecoverySummary(): RecoverySummary {
  return {
    orphanAgentCleanup: {
      attempted: false,
      cleanedCount: 0,
    },
    expiredLocksReleased: 0,
    interruptedRunsMarked: 0,
    loopsRequeued: 0,
    eventsWritten: 0,
  };
}

function isAgentConfigured(config: LooperConfig): config is LooperConfig & {
  agent: { vendor: NonNullable<LooperConfig["agent"]["vendor"]> };
} {
  return Boolean(config.agent.vendor);
}

function shouldRequeueLoop(loop: LoopRecord, latestRun: RunRecord): boolean {
  if (loop.status === "paused") {
    return false;
  }

  if (loop.status === "completed" || loop.status === "failed") {
    return false;
  }

  return loop.status === "running" || latestRun.status === "interrupted";
}

function parseProjectMetadata(
  metadataJson?: string | null,
): Record<string, unknown> {
  if (!metadataJson) {
    return {};
  }

  try {
    const parsed = JSON.parse(metadataJson) as unknown;
    return parsed && typeof parsed === "object" && !Array.isArray(parsed)
      ? (parsed as Record<string, unknown>)
      : {};
  } catch {
    return {};
  }
}

function readMetadataString(value: unknown): string | null {
  return typeof value === "string" && value.length > 0 ? value : null;
}

function shouldNotifyRunFailure(
  type: "planner" | "reviewer" | "fixer" | "worker",
  result: { failureKind?: string; summary: string },
): boolean {
  if (type === "fixer" && result.failureKind === "retryable_after_resume") {
    return false;
  }

  return true;
}

export function createLooperdRuntime(
  options: CreateLooperdRuntimeOptions,
): LooperdRuntime {
  return new BasicLooperdRuntime(options);
}
