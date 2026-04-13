export type StorageMode = "sqlite";

export interface StorageHealth {
  ok: boolean;
  mode: StorageMode;
  dbPath: string;
  lastUpdatedAt?: string;
  migration: {
    latestAvailableId?: string;
    latestAppliedId?: string;
    pendingCount: number;
  };
  details?: string;
}

export interface StorageMigration {
  id: string;
  fileName: string;
}

export interface AppliedMigration {
  id: string;
  appliedAt: string;
}

export interface MigrationStatus {
  available: StorageMigration[];
  applied: AppliedMigration[];
  pending: StorageMigration[];
}

export interface MigrationRunResult {
  appliedIds: string[];
  skippedIds: string[];
  backupPath?: string;
}

export interface MigrationRunner {
  listPending(): string[];
  status(): MigrationStatus;
  runPending(): MigrationRunResult;
}

export interface StorageDriver {
  initialize(options?: {
    autoMigrate?: boolean;
    requireBackup?: boolean;
  }): void;
  backup(): string;
  healthcheck(): StorageHealth;
  close(): void;
}

export interface ProjectRecord {
  id: string;
  name: string;
  repoPath: string;
  baseBranch?: string | null;
  archived: boolean;
  metadataJson?: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface LoopRecord {
  id: string;
  seq: number;
  projectId: string;
  type: string;
  targetType: string;
  targetId?: string | null;
  repo?: string | null;
  prNumber?: number | null;
  status: string;
  configJson?: string | null;
  metadataJson?: string | null;
  lastRunAt?: string | null;
  nextRunAt?: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface RunRecord {
  id: string;
  loopId: string;
  status: string;
  currentStep?: string | null;
  lastCompletedStep?: string | null;
  checkpointJson?: string | null;
  summary?: string | null;
  errorMessage?: string | null;
  startedAt: string;
  lastHeartbeatAt?: string | null;
  endedAt?: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface PullRequestSnapshotRecord {
  id: string;
  projectId: string;
  repo: string;
  prNumber: number;
  headSha: string;
  baseSha?: string | null;
  title?: string | null;
  body?: string | null;
  author?: string | null;
  diffRef?: string | null;
  checksSummary?: string | null;
  unresolvedThreadCount?: number | null;
  reviewState?: string | null;
  payloadJson?: string | null;
  capturedAt: string;
  createdAt: string;
}

export interface EventLogRecord {
  id: string;
  eventType: string;
  projectId?: string | null;
  loopId?: string | null;
  runId?: string | null;
  entityType?: string | null;
  entityId?: string | null;
  correlationId?: string | null;
  causationId?: string | null;
  actorType?: string | null;
  actorId?: string | null;
  actorDisplayName?: string | null;
  payloadJson: string;
  createdAt: string;
}

export interface LockRecord {
  key: string;
  owner: string;
  reason?: string | null;
  expiresAt: string;
  createdAt: string;
  updatedAt: string;
}

export type QueueItemStatus =
  | "queued"
  | "running"
  | "completed"
  | "failed"
  | "cancelled"
  | "manual_intervention";

export type QueueFailureKind =
  | "retryable_transient"
  | "retryable_after_resume"
  | "non_retryable"
  | "manual_intervention";

export interface QueueItemRecord {
  id: string;
  projectId?: string | null;
  loopId?: string | null;
  type: string;
  targetType: string;
  targetId: string;
  repo?: string | null;
  prNumber?: number | null;
  dedupeKey: string;
  priority: number;
  status: QueueItemStatus;
  availableAt: string;
  attempts: number;
  maxAttempts: number;
  claimedBy?: string | null;
  claimedAt?: string | null;
  startedAt?: string | null;
  finishedAt?: string | null;
  lockKey?: string | null;
  payloadJson?: string | null;
  lastError?: string | null;
  lastErrorKind?: QueueFailureKind | null;
  createdAt: string;
  updatedAt: string;
}

export interface AgentExecutionRecord {
  id: string;
  projectId?: string | null;
  loopId?: string | null;
  runId?: string | null;
  vendor: string;
  status: string;
  pid?: number | null;
  commandJson: string;
  cwd: string;
  summary?: string | null;
  parseStatus?: string | null;
  completionSignal?: string | null;
  heartbeatCount: number;
  lastHeartbeatAt?: string | null;
  outputJson?: string | null;
  errorMessage?: string | null;
  startedAt: string;
  endedAt?: string | null;
  metadataJson?: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface NotificationRecord {
  id: string;
  projectId?: string | null;
  loopId?: string | null;
  runId?: string | null;
  entityType?: string | null;
  entityId?: string | null;
  channel: string;
  level: string;
  title: string;
  subtitle?: string | null;
  body: string;
  status: string;
  dedupeKey?: string | null;
  errorMessage?: string | null;
  payloadJson?: string | null;
  sentAt?: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface WorktreeRecord {
  id: string;
  projectId: string;
  repoPath: string;
  worktreePath: string;
  branch: string;
  baseBranch: string;
  status: string;
  headSha?: string | null;
  metadataJson?: string | null;
  createdAt: string;
  updatedAt: string;
  cleanedAt?: string | null;
}
