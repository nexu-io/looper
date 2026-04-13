import type {
  AgentExecutionRecord,
  EventLogRecord,
  LockRecord,
  LoopRecord,
  MigrationStatus,
  NotificationRecord,
  ProjectRecord,
  PullRequestSnapshotRecord,
  QueueFailureKind,
  QueueItemRecord,
  RunRecord,
  StorageHealth,
  WorktreeRecord,
} from "./types";

export interface Store {
  withTransaction<T>(fn: (store: Store) => T): T;

  projects: {
    upsert(record: ProjectRecord): void;
    getById(id: string): ProjectRecord | null;
    list(): ProjectRecord[];
  };

  loops: {
    upsert(record: LoopRecord): void;
    getById(id: string): LoopRecord | null;
    getBySeq(seq: number): LoopRecord | null;
    allocateSeq(): number;
    list(): LoopRecord[];
  };

  runs: {
    upsert(record: RunRecord): void;
    getById(id: string): RunRecord | null;
    getLatestByLoopId(loopId: string): RunRecord | null;
    list(): RunRecord[];
    listByStatus(status: RunRecord["status"]): RunRecord[];
    listByLoop(loopId: string): RunRecord[];
  };

  pullRequestSnapshots: {
    upsert(record: PullRequestSnapshotRecord): void;
    list(): PullRequestSnapshotRecord[];
    getLatest(repo: string, prNumber: number): PullRequestSnapshotRecord | null;
  };

  events: {
    append(record: EventLogRecord): void;
    list(limit?: number): EventLogRecord[];
    listByEntity(entityType: string, entityId: string): EventLogRecord[];
  };

  locks: {
    acquire(record: LockRecord): boolean;
    release(key: string): void;
    get(key: string): LockRecord | null;
    listExpired(nowIso: string): LockRecord[];
  };

  queue: {
    upsert(record: QueueItemRecord): void;
    getById(id: string): QueueItemRecord | null;
    list(): QueueItemRecord[];
    findActiveByDedupe(dedupeKey: string): QueueItemRecord | null;
    listScheduled(nowIso: string, limit?: number): QueueItemRecord[];
    claimNext(nowIso: string, claimedBy: string): QueueItemRecord | null;
    claimNextOfType(
      nowIso: string,
      claimedBy: string,
      type: QueueItemRecord["type"],
    ): QueueItemRecord | null;
    complete(id: string, finishedAt: string): void;
    markRetry(input: {
      id: string;
      availableAt: string;
      attempts: number;
      errorMessage?: string | null;
      errorKind: QueueFailureKind;
      updatedAt: string;
    }): void;
    fail(input: {
      id: string;
      finishedAt: string;
      errorMessage?: string | null;
      errorKind: QueueFailureKind;
      updatedAt: string;
    }): void;
    requeueRunningByLoop(loopId: string, queuedAt: string): number;
    cancelByLoop(loopId: string, finishedAt: string, reason?: string): number;
  };

  agentExecutions: {
    upsert(record: AgentExecutionRecord): void;
    getById(id: string): AgentExecutionRecord | null;
    getLatestByRunId(runId: string): AgentExecutionRecord | null;
    list(): AgentExecutionRecord[];
    listByRunId(runId: string): AgentExecutionRecord[];
    listActive(): AgentExecutionRecord[];
  };

  notifications: {
    upsert(record: NotificationRecord): void;
    getById(id: string): NotificationRecord | null;
    list(limit?: number): NotificationRecord[];
    getLatestByDedupe(
      channel: string,
      dedupeKey: string,
    ): NotificationRecord | null;
  };

  worktrees: {
    upsert(record: WorktreeRecord): void;
    getById(id: string): WorktreeRecord | null;
    getByBranch(projectId: string, branch: string): WorktreeRecord | null;
    listByProject(projectId: string): WorktreeRecord[];
  };

  schema: {
    getMigrationStatus(): MigrationStatus;
    healthcheck(): StorageHealth;
    backup(): string;
  };
}
