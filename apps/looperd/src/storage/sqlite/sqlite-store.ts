import type { Store } from "../store";
import type {
  AgentExecutionRecord,
  EventLogRecord,
  LockRecord,
  LoopRecord,
  NotificationRecord,
  ProjectRecord,
  PullRequestSnapshotRecord,
  QueueFailureKind,
  QueueItemRecord,
  RunRecord,
  WorktreeRecord,
} from "../types";
import { SqliteDbCoordinator } from "./db";

export interface SqliteStoreOptions {
  dbPath: string;
  backupDir?: string;
  migrationsDir?: string;
  now?: () => Date;
}

export class SqliteStore implements Store {
  private readonly coordinator: SqliteDbCoordinator;
  private readonly now: () => Date;

  constructor(options: SqliteStoreOptions) {
    this.now = options.now ?? (() => new Date());
    this.coordinator = new SqliteDbCoordinator(options);
  }

  public initialize(options?: {
    autoMigrate?: boolean;
    requireBackup?: boolean;
  }): void {
    this.coordinator.initialize(options);
  }

  public close(): void {
    this.coordinator.close();
  }

  public withTransaction<T>(fn: (store: Store) => T): T {
    return this.coordinator.withTransaction(() => fn(this));
  }

  public readonly projects = {
    upsert: (record: ProjectRecord): void => {
      this.coordinator.db
        .query(`
          INSERT INTO projects (id, name, repo_path, base_branch, archived, metadata_json, created_at, updated_at)
          VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)
          ON CONFLICT(id) DO UPDATE SET
            name=excluded.name,
            repo_path=excluded.repo_path,
            base_branch=excluded.base_branch,
            archived=excluded.archived,
            metadata_json=excluded.metadata_json,
            updated_at=excluded.updated_at
        `)
        .run(
          record.id,
          record.name,
          record.repoPath,
          record.baseBranch ?? null,
          record.archived ? 1 : 0,
          record.metadataJson ?? null,
          record.createdAt,
          record.updatedAt,
        );
    },
    getById: (id: string): ProjectRecord | null => {
      const row = this.coordinator.db
        .query("SELECT * FROM projects WHERE id = ?1")
        .get(id) as Record<string, unknown> | null;
      return row ? mapProject(row) : null;
    },
    list: (): ProjectRecord[] => {
      const rows = this.coordinator.db
        .query("SELECT * FROM projects ORDER BY updated_at DESC")
        .all() as Record<string, unknown>[];
      return rows.map(mapProject);
    },
  };

  public readonly loops = {
    upsert: (record: LoopRecord): void => {
      this.coordinator.db
        .query(`
          INSERT INTO loops (id, seq, project_id, type, target_type, target_id, repo, pr_number, status, config_json, metadata_json, last_run_at, next_run_at, created_at, updated_at)
          VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15)
          ON CONFLICT(id) DO UPDATE SET
            seq=excluded.seq,
            project_id=excluded.project_id,
            type=excluded.type,
            target_type=excluded.target_type,
            target_id=excluded.target_id,
            repo=excluded.repo,
            pr_number=excluded.pr_number,
            status=excluded.status,
            config_json=excluded.config_json,
            metadata_json=excluded.metadata_json,
            last_run_at=excluded.last_run_at,
            next_run_at=excluded.next_run_at,
            updated_at=excluded.updated_at
        `)
        .run(
          record.id,
          record.seq,
          record.projectId,
          record.type,
          record.targetType,
          record.targetId ?? null,
          record.repo ?? null,
          record.prNumber ?? null,
          record.status,
          record.configJson ?? null,
          record.metadataJson ?? null,
          record.lastRunAt ?? null,
          record.nextRunAt ?? null,
          record.createdAt,
          record.updatedAt,
        );

      this.coordinator.db
        .query(
          `INSERT INTO counters (name, value)
           VALUES ('loop_seq', ?1)
           ON CONFLICT(name) DO UPDATE SET value =
             CASE WHEN excluded.value > counters.value THEN excluded.value ELSE counters.value END`,
        )
        .run(record.seq);
    },
    getById: (id: string): LoopRecord | null => {
      const row = this.coordinator.db
        .query("SELECT * FROM loops WHERE id = ?1")
        .get(id) as Record<string, unknown> | null;
      return row ? mapLoop(row) : null;
    },
    getBySeq: (seq: number): LoopRecord | null => {
      const row = this.coordinator.db
        .query("SELECT * FROM loops WHERE seq = ?1")
        .get(seq) as Record<string, unknown> | null;
      return row ? mapLoop(row) : null;
    },
    allocateSeq: (): number => {
      const existing = this.coordinator.db
        .query("SELECT value FROM counters WHERE name = 'loop_seq'")
        .get() as Record<string, unknown> | null;

      if (!existing) {
        const maxSeqRow = this.coordinator.db
          .query("SELECT COALESCE(MAX(seq), 0) AS value FROM loops")
          .get() as Record<string, unknown> | null;
        const currentValue = Number(maxSeqRow?.value ?? 0);
        this.coordinator.db
          .query("INSERT INTO counters (name, value) VALUES ('loop_seq', ?1)")
          .run(currentValue);
      }

      const row = this.coordinator.db
        .query(
          "UPDATE counters SET value = value + 1 WHERE name = 'loop_seq' RETURNING value",
        )
        .get() as Record<string, unknown> | null;

      if (!row) {
        throw new Error("Failed to allocate loop sequence");
      }

      return Number(row.value);
    },
    list: (): LoopRecord[] => {
      const rows = this.coordinator.db
        .query("SELECT * FROM loops ORDER BY updated_at DESC, seq DESC")
        .all() as Record<string, unknown>[];
      return rows.map(mapLoop);
    },
  };

  public readonly runs = {
    upsert: (record: RunRecord): void => {
      this.coordinator.db
        .query(`
          INSERT INTO runs (id, loop_id, status, current_step, last_completed_step, checkpoint_json, summary, error_message, started_at, last_heartbeat_at, ended_at, created_at, updated_at)
          VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13)
          ON CONFLICT(id) DO UPDATE SET
            status=excluded.status,
            current_step=excluded.current_step,
            last_completed_step=excluded.last_completed_step,
            checkpoint_json=excluded.checkpoint_json,
            summary=excluded.summary,
            error_message=excluded.error_message,
            started_at=excluded.started_at,
            last_heartbeat_at=excluded.last_heartbeat_at,
            ended_at=excluded.ended_at,
            updated_at=excluded.updated_at
        `)
        .run(
          record.id,
          record.loopId,
          record.status,
          record.currentStep ?? null,
          record.lastCompletedStep ?? null,
          record.checkpointJson ?? null,
          record.summary ?? null,
          record.errorMessage ?? null,
          record.startedAt,
          record.lastHeartbeatAt ?? null,
          record.endedAt ?? null,
          record.createdAt,
          record.updatedAt,
        );
    },
    getById: (id: string): RunRecord | null => {
      const row = this.coordinator.db
        .query("SELECT * FROM runs WHERE id = ?1")
        .get(id) as Record<string, unknown> | null;
      return row ? mapRun(row) : null;
    },
    getLatestByLoopId: (loopId: string): RunRecord | null => {
      const row = this.coordinator.db
        .query(
          "SELECT * FROM runs WHERE loop_id = ?1 ORDER BY started_at DESC LIMIT 1",
        )
        .get(loopId) as Record<string, unknown> | null;
      return row ? mapRun(row) : null;
    },
    list: (): RunRecord[] => {
      const rows = this.coordinator.db
        .query("SELECT * FROM runs ORDER BY started_at DESC")
        .all() as Record<string, unknown>[];
      return rows.map(mapRun);
    },
    listByStatus: (status: RunRecord["status"]): RunRecord[] => {
      const rows = this.coordinator.db
        .query(
          "SELECT * FROM runs WHERE status = ?1 ORDER BY started_at DESC, id DESC",
        )
        .all(status) as Record<string, unknown>[];
      return rows.map(mapRun);
    },
    listByLoop: (loopId: string): RunRecord[] => {
      const rows = this.coordinator.db
        .query("SELECT * FROM runs WHERE loop_id = ?1 ORDER BY started_at DESC")
        .all(loopId) as Record<string, unknown>[];
      return rows.map(mapRun);
    },
  };

  public readonly pullRequestSnapshots = {
    upsert: (record: PullRequestSnapshotRecord): void => {
      this.coordinator.db
        .query(`
          INSERT INTO pull_request_snapshots (id, project_id, repo, pr_number, head_sha, base_sha, title, body, author, diff_ref, checks_summary, unresolved_thread_count, review_state, payload_json, captured_at, created_at)
          VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16)
          ON CONFLICT(id) DO UPDATE SET
            project_id=excluded.project_id,
            repo=excluded.repo,
            pr_number=excluded.pr_number,
            head_sha=excluded.head_sha,
            base_sha=excluded.base_sha,
            title=excluded.title,
            body=excluded.body,
            author=excluded.author,
            diff_ref=excluded.diff_ref,
            checks_summary=excluded.checks_summary,
            unresolved_thread_count=excluded.unresolved_thread_count,
            review_state=excluded.review_state,
            payload_json=excluded.payload_json,
            captured_at=excluded.captured_at
        `)
        .run(
          record.id,
          record.projectId,
          record.repo,
          record.prNumber,
          record.headSha,
          record.baseSha ?? null,
          record.title ?? null,
          record.body ?? null,
          record.author ?? null,
          record.diffRef ?? null,
          record.checksSummary ?? null,
          record.unresolvedThreadCount ?? null,
          record.reviewState ?? null,
          record.payloadJson ?? null,
          record.capturedAt,
          record.createdAt,
        );
    },
    list: (): PullRequestSnapshotRecord[] => {
      return this.coordinator.db
        .query(
          "SELECT * FROM pull_request_snapshots ORDER BY captured_at DESC, created_at DESC",
        )
        .all()
        .map((row: unknown) =>
          mapPullRequestSnapshot(row as Record<string, unknown>),
        );
    },
    getLatest: (
      repo: string,
      prNumber: number,
    ): PullRequestSnapshotRecord | null => {
      const row = this.coordinator.db
        .query(
          "SELECT * FROM pull_request_snapshots WHERE repo = ?1 AND pr_number = ?2 ORDER BY captured_at DESC LIMIT 1",
        )
        .get(repo, prNumber) as Record<string, unknown> | null;
      return row ? mapPullRequestSnapshot(row) : null;
    },
  };

  public readonly events = {
    append: (record: EventLogRecord): void => {
      this.coordinator.db
        .query(
          "INSERT INTO event_logs (id, event_type, project_id, loop_id, run_id, entity_type, entity_id, correlation_id, causation_id, actor_type, actor_id, actor_display_name, payload_json, created_at) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14)",
        )
        .run(
          record.id,
          record.eventType,
          record.projectId ?? null,
          record.loopId ?? null,
          record.runId ?? null,
          record.entityType ?? null,
          record.entityId ?? null,
          record.correlationId ?? null,
          record.causationId ?? null,
          record.actorType ?? null,
          record.actorId ?? null,
          record.actorDisplayName ?? null,
          record.payloadJson,
          record.createdAt,
        );
    },
    list: (limit = 100): EventLogRecord[] => {
      return this.coordinator.db
        .query("SELECT * FROM event_logs ORDER BY created_at DESC LIMIT ?1")
        .all(limit)
        .map((row: unknown) => mapEvent(row as Record<string, unknown>));
    },
    listByEntity: (entityType: string, entityId: string): EventLogRecord[] => {
      return this.coordinator.db
        .query(
          "SELECT * FROM event_logs WHERE entity_type = ?1 AND entity_id = ?2 ORDER BY created_at ASC",
        )
        .all(entityType, entityId)
        .map((row: unknown) => mapEvent(row as Record<string, unknown>));
    },
  };

  public readonly locks = {
    acquire: (record: LockRecord): boolean => {
      const now = this.now().toISOString();
      const result = this.coordinator.db
        .query(`
          INSERT INTO locks (key, owner, reason, expires_at, created_at, updated_at)
          VALUES (?1, ?2, ?3, ?4, ?5, ?6)
          ON CONFLICT(key) DO UPDATE SET
            owner=excluded.owner,
            reason=excluded.reason,
            expires_at=excluded.expires_at,
            updated_at=excluded.updated_at
          WHERE locks.expires_at <= ?7
        `)
        .run(
          record.key,
          record.owner,
          record.reason ?? null,
          record.expiresAt,
          record.createdAt,
          record.updatedAt,
          now,
        );
      return result.changes > 0;
    },
    release: (key: string): void => {
      this.coordinator.db.query("DELETE FROM locks WHERE key = ?1").run(key);
    },
    get: (key: string): LockRecord | null => {
      const row = this.coordinator.db
        .query("SELECT * FROM locks WHERE key = ?1")
        .get(key) as Record<string, unknown> | null;
      return row ? mapLock(row) : null;
    },
    listExpired: (nowIso: string): LockRecord[] => {
      return this.coordinator.db
        .query(
          "SELECT * FROM locks WHERE expires_at <= ?1 ORDER BY expires_at ASC",
        )
        .all(nowIso)
        .map((row: unknown) => mapLock(row as Record<string, unknown>));
    },
  };

  public readonly queue = {
    upsert: (record: QueueItemRecord): void => {
      this.coordinator.db
        .query(`
          INSERT INTO queue_items (
            id, project_id, loop_id, type, target_type, target_id, repo,
            pr_number, dedupe_key, priority, status, available_at, attempts,
            max_attempts, claimed_by, claimed_at, started_at, finished_at,
            lock_key, payload_json, last_error, last_error_kind, created_at,
            updated_at
          )
          VALUES (
            ?1, ?2, ?3, ?4, ?5, ?6, ?7,
            ?8, ?9, ?10, ?11, ?12, ?13,
            ?14, ?15, ?16, ?17, ?18, ?19,
            ?20, ?21, ?22, ?23, ?24
          )
          ON CONFLICT(id) DO UPDATE SET
            project_id=excluded.project_id,
            loop_id=excluded.loop_id,
            type=excluded.type,
            target_type=excluded.target_type,
            target_id=excluded.target_id,
            repo=excluded.repo,
            pr_number=excluded.pr_number,
            dedupe_key=excluded.dedupe_key,
            priority=excluded.priority,
            status=excluded.status,
            available_at=excluded.available_at,
            attempts=excluded.attempts,
            max_attempts=excluded.max_attempts,
            claimed_by=excluded.claimed_by,
            claimed_at=excluded.claimed_at,
            started_at=excluded.started_at,
            finished_at=excluded.finished_at,
            lock_key=excluded.lock_key,
            payload_json=excluded.payload_json,
            last_error=excluded.last_error,
            last_error_kind=excluded.last_error_kind,
            updated_at=excluded.updated_at
        `)
        .run(
          record.id,
          record.projectId ?? null,
          record.loopId ?? null,
          record.type,
          record.targetType,
          record.targetId,
          record.repo ?? null,
          record.prNumber ?? null,
          record.dedupeKey,
          record.priority,
          record.status,
          record.availableAt,
          record.attempts,
          record.maxAttempts,
          record.claimedBy ?? null,
          record.claimedAt ?? null,
          record.startedAt ?? null,
          record.finishedAt ?? null,
          record.lockKey ?? null,
          record.payloadJson ?? null,
          record.lastError ?? null,
          record.lastErrorKind ?? null,
          record.createdAt,
          record.updatedAt,
        );
    },
    getById: (id: string): QueueItemRecord | null => {
      const row = this.coordinator.db
        .query("SELECT * FROM queue_items WHERE id = ?1")
        .get(id) as Record<string, unknown> | null;
      return row ? mapQueueItem(row) : null;
    },
    list: (): QueueItemRecord[] => {
      return this.coordinator.db
        .query("SELECT * FROM queue_items ORDER BY created_at DESC")
        .all()
        .map((row: unknown) => mapQueueItem(row as Record<string, unknown>));
    },
    findActiveByDedupe: (dedupeKey: string): QueueItemRecord | null => {
      const row = this.coordinator.db
        .query(
          `SELECT * FROM queue_items
           WHERE dedupe_key = ?1 AND status IN ('queued', 'running')
           ORDER BY created_at DESC
           LIMIT 1`,
        )
        .get(dedupeKey) as Record<string, unknown> | null;
      return row ? mapQueueItem(row) : null;
    },
    listScheduled: (nowIso: string, limit = 100): QueueItemRecord[] => {
      return this.coordinator.db
        .query(`${SCHEDULED_QUEUE_QUERY} LIMIT ?2`)
        .all(nowIso, limit)
        .map((row: unknown) => mapQueueItem(row as Record<string, unknown>));
    },
    claimNext: (nowIso: string, claimedBy: string): QueueItemRecord | null => {
      return this.coordinator.withTransaction(() => {
        const row = this.coordinator.db
          .query(`${SCHEDULED_QUEUE_QUERY} LIMIT 1`)
          .get(nowIso) as Record<string, unknown> | null;

        if (!row) {
          return null;
        }

        const result = this.coordinator.db
          .query(
            `UPDATE queue_items
             SET status = 'running',
                 claimed_by = ?2,
                 claimed_at = ?3,
                 started_at = COALESCE(started_at, ?3),
                 updated_at = ?3
             WHERE id = ?1 AND status = 'queued'`,
          )
          .run(String(row.id), claimedBy, nowIso);

        if (result.changes === 0) {
          return null;
        }

        const claimed = this.coordinator.db
          .query("SELECT * FROM queue_items WHERE id = ?1")
          .get(String(row.id)) as Record<string, unknown> | null;
        return claimed ? mapQueueItem(claimed) : null;
      });
    },
    claimNextOfType: (
      nowIso: string,
      claimedBy: string,
      type: QueueItemRecord["type"],
    ): QueueItemRecord | null => {
      return this.coordinator.withTransaction(() => {
        const row = this.coordinator.db
          .query(
            `${SCHEDULED_QUEUE_BASE_QUERY} AND qi.type = ?2${SCHEDULED_QUEUE_ORDER_BY} LIMIT 1`,
          )
          .get(nowIso, type) as Record<string, unknown> | null;

        if (!row) {
          return null;
        }

        const result = this.coordinator.db
          .query(
            `UPDATE queue_items
             SET status = 'running',
                 claimed_by = ?2,
                 claimed_at = ?3,
                 started_at = COALESCE(started_at, ?3),
                 updated_at = ?3
             WHERE id = ?1 AND status = 'queued'`,
          )
          .run(String(row.id), claimedBy, nowIso);

        if (result.changes === 0) {
          return null;
        }

        const claimed = this.coordinator.db
          .query("SELECT * FROM queue_items WHERE id = ?1")
          .get(String(row.id)) as Record<string, unknown> | null;
        return claimed ? mapQueueItem(claimed) : null;
      });
    },
    complete: (id: string, finishedAt: string): void => {
      this.coordinator.db
        .query(
          `UPDATE queue_items
           SET status = 'completed', finished_at = ?2, updated_at = ?2
           WHERE id = ?1`,
        )
        .run(id, finishedAt);
    },
    markRetry: (input: {
      id: string;
      availableAt: string;
      attempts: number;
      errorMessage?: string | null;
      errorKind: QueueFailureKind;
      updatedAt: string;
    }): void => {
      this.coordinator.db
        .query(
          `UPDATE queue_items
           SET status = 'queued',
               available_at = ?2,
               attempts = ?3,
               last_error = ?4,
               last_error_kind = ?5,
               claimed_by = NULL,
               claimed_at = NULL,
               finished_at = NULL,
               updated_at = ?6
           WHERE id = ?1`,
        )
        .run(
          input.id,
          input.availableAt,
          input.attempts,
          input.errorMessage ?? null,
          input.errorKind,
          input.updatedAt,
        );
    },
    fail: (input: {
      id: string;
      finishedAt: string;
      errorMessage?: string | null;
      errorKind: QueueFailureKind;
      updatedAt: string;
    }): void => {
      const terminalStatus =
        input.errorKind === "manual_intervention"
          ? "manual_intervention"
          : "failed";

      this.coordinator.db
        .query(
          `UPDATE queue_items
           SET status = ?2,
               finished_at = ?3,
               last_error = ?4,
               last_error_kind = ?5,
               updated_at = ?6
           WHERE id = ?1`,
        )
        .run(
          input.id,
          terminalStatus,
          input.finishedAt,
          input.errorMessage ?? null,
          input.errorKind,
          input.updatedAt,
        );
    },
    requeueRunningByLoop: (loopId: string, queuedAt: string): number => {
      const result = this.coordinator.db
        .query(
          `UPDATE queue_items
           SET status = 'queued',
               available_at = ?2,
               claimed_by = NULL,
               claimed_at = NULL,
               started_at = NULL,
               finished_at = NULL,
               updated_at = ?2
           WHERE loop_id = ?1 AND status = 'running'`,
        )
        .run(loopId, queuedAt);
      return result.changes;
    },
    cancelByLoop: (
      loopId: string,
      finishedAt: string,
      reason?: string,
    ): number => {
      const result = this.coordinator.db
        .query(
          `UPDATE queue_items
           SET status = 'cancelled',
               finished_at = ?2,
               last_error = COALESCE(?3, last_error),
               updated_at = ?2
           WHERE loop_id = ?1 AND status IN ('queued', 'running')`,
        )
        .run(loopId, finishedAt, reason ?? null);
      return result.changes;
    },
  };

  public readonly agentExecutions = {
    upsert: (record: AgentExecutionRecord): void => {
      this.coordinator.db
        .query(`
          INSERT INTO agent_executions (id, project_id, loop_id, run_id, vendor, status, pid, command_json, cwd, summary, parse_status, completion_signal, heartbeat_count, last_heartbeat_at, output_json, error_message, started_at, ended_at, metadata_json, created_at, updated_at)
          VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18, ?19, ?20, ?21)
          ON CONFLICT(id) DO UPDATE SET
            project_id=excluded.project_id,
            loop_id=excluded.loop_id,
            run_id=excluded.run_id,
            vendor=excluded.vendor,
            status=excluded.status,
            pid=excluded.pid,
            command_json=excluded.command_json,
            cwd=excluded.cwd,
            summary=excluded.summary,
            parse_status=excluded.parse_status,
            completion_signal=excluded.completion_signal,
            heartbeat_count=excluded.heartbeat_count,
            last_heartbeat_at=excluded.last_heartbeat_at,
            output_json=excluded.output_json,
            error_message=excluded.error_message,
            started_at=excluded.started_at,
            ended_at=excluded.ended_at,
            metadata_json=excluded.metadata_json,
            updated_at=excluded.updated_at
        `)
        .run(
          record.id,
          record.projectId ?? null,
          record.loopId ?? null,
          record.runId ?? null,
          record.vendor,
          record.status,
          record.pid ?? null,
          record.commandJson,
          record.cwd,
          record.summary ?? null,
          record.parseStatus ?? null,
          record.completionSignal ?? null,
          record.heartbeatCount,
          record.lastHeartbeatAt ?? null,
          record.outputJson ?? null,
          record.errorMessage ?? null,
          record.startedAt,
          record.endedAt ?? null,
          record.metadataJson ?? null,
          record.createdAt,
          record.updatedAt,
        );
    },
    getById: (id: string): AgentExecutionRecord | null => {
      const row = this.coordinator.db
        .query("SELECT * FROM agent_executions WHERE id = ?1")
        .get(id) as Record<string, unknown> | null;
      return row ? mapAgentExecution(row) : null;
    },
    getLatestByRunId: (runId: string): AgentExecutionRecord | null => {
      const row = this.coordinator.db
        .query(
          "SELECT * FROM agent_executions WHERE run_id = ?1 ORDER BY started_at DESC LIMIT 1",
        )
        .get(runId) as Record<string, unknown> | null;
      return row ? mapAgentExecution(row) : null;
    },
    list: (): AgentExecutionRecord[] => {
      return this.coordinator.db
        .query("SELECT * FROM agent_executions ORDER BY started_at DESC")
        .all()
        .map((row: unknown) =>
          mapAgentExecution(row as Record<string, unknown>),
        );
    },
    listByRunId: (runId: string): AgentExecutionRecord[] => {
      return this.coordinator.db
        .query(
          "SELECT * FROM agent_executions WHERE run_id = ?1 ORDER BY started_at DESC",
        )
        .all(runId)
        .map((row: unknown) =>
          mapAgentExecution(row as Record<string, unknown>),
        );
    },
    listActive: (): AgentExecutionRecord[] => {
      return this.coordinator.db
        .query(
          "SELECT * FROM agent_executions WHERE status IN ('running', 'cancelling') ORDER BY started_at DESC",
        )
        .all()
        .map((row: unknown) =>
          mapAgentExecution(row as Record<string, unknown>),
        );
    },
  };

  public readonly notifications = {
    upsert: (record: NotificationRecord): void => {
      this.coordinator.db
        .query(`
          INSERT INTO notifications (id, project_id, loop_id, run_id, entity_type, entity_id, channel, level, title, subtitle, body, status, dedupe_key, error_message, payload_json, sent_at, created_at, updated_at)
          VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18)
          ON CONFLICT(id) DO UPDATE SET
            project_id=excluded.project_id,
            loop_id=excluded.loop_id,
            run_id=excluded.run_id,
            entity_type=excluded.entity_type,
            entity_id=excluded.entity_id,
            channel=excluded.channel,
            level=excluded.level,
            title=excluded.title,
            subtitle=excluded.subtitle,
            body=excluded.body,
            status=excluded.status,
            dedupe_key=excluded.dedupe_key,
            error_message=excluded.error_message,
            payload_json=excluded.payload_json,
            sent_at=excluded.sent_at,
            updated_at=excluded.updated_at
        `)
        .run(
          record.id,
          record.projectId ?? null,
          record.loopId ?? null,
          record.runId ?? null,
          record.entityType ?? null,
          record.entityId ?? null,
          record.channel,
          record.level,
          record.title,
          record.subtitle ?? null,
          record.body,
          record.status,
          record.dedupeKey ?? null,
          record.errorMessage ?? null,
          record.payloadJson ?? null,
          record.sentAt ?? null,
          record.createdAt,
          record.updatedAt,
        );
    },
    getById: (id: string): NotificationRecord | null => {
      const row = this.coordinator.db
        .query("SELECT * FROM notifications WHERE id = ?1")
        .get(id) as Record<string, unknown> | null;
      return row ? mapNotification(row) : null;
    },
    list: (limit = 100): NotificationRecord[] => {
      return this.coordinator.db
        .query("SELECT * FROM notifications ORDER BY created_at DESC LIMIT ?1")
        .all(limit)
        .map((row: unknown) => mapNotification(row as Record<string, unknown>));
    },
    getLatestByDedupe: (
      channel: string,
      dedupeKey: string,
    ): NotificationRecord | null => {
      const row = this.coordinator.db
        .query(
          "SELECT * FROM notifications WHERE channel = ?1 AND dedupe_key = ?2 ORDER BY created_at DESC LIMIT 1",
        )
        .get(channel, dedupeKey) as Record<string, unknown> | null;
      return row ? mapNotification(row) : null;
    },
  };

  public readonly worktrees = {
    upsert: (record: WorktreeRecord): void => {
      this.coordinator.db
        .query(`
          INSERT INTO worktrees (id, project_id, repo_path, worktree_path, branch, base_branch, status, head_sha, metadata_json, created_at, updated_at, cleaned_at)
          VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12)
          ON CONFLICT(id) DO UPDATE SET
            project_id=excluded.project_id,
            repo_path=excluded.repo_path,
            worktree_path=excluded.worktree_path,
            branch=excluded.branch,
            base_branch=excluded.base_branch,
            status=excluded.status,
            head_sha=excluded.head_sha,
            metadata_json=excluded.metadata_json,
            updated_at=excluded.updated_at,
            cleaned_at=excluded.cleaned_at
        `)
        .run(
          record.id,
          record.projectId,
          record.repoPath,
          record.worktreePath,
          record.branch,
          record.baseBranch,
          record.status,
          record.headSha ?? null,
          record.metadataJson ?? null,
          record.createdAt,
          record.updatedAt,
          record.cleanedAt ?? null,
        );
    },
    getById: (id: string): WorktreeRecord | null => {
      const row = this.coordinator.db
        .query("SELECT * FROM worktrees WHERE id = ?1")
        .get(id) as Record<string, unknown> | null;
      return row ? mapWorktree(row) : null;
    },
    getByBranch: (projectId: string, branch: string): WorktreeRecord | null => {
      const row = this.coordinator.db
        .query(
          "SELECT * FROM worktrees WHERE project_id = ?1 AND branch = ?2 LIMIT 1",
        )
        .get(projectId, branch) as Record<string, unknown> | null;
      return row ? mapWorktree(row) : null;
    },
    listByProject: (projectId: string): WorktreeRecord[] => {
      return this.coordinator.db
        .query(
          "SELECT * FROM worktrees WHERE project_id = ?1 ORDER BY updated_at DESC",
        )
        .all(projectId)
        .map((row: unknown) => mapWorktree(row as Record<string, unknown>));
    },
  };

  public readonly schema = {
    getMigrationStatus: () => this.coordinator.getMigrationStatus(),
    healthcheck: () => this.coordinator.healthcheck(),
    backup: () => this.coordinator.backup(),
  };
}

function mapProject(row: Record<string, unknown>): ProjectRecord {
  return {
    id: String(row.id),
    name: String(row.name),
    repoPath: String(row.repo_path),
    baseBranch: asNullableString(row.base_branch),
    archived: asBoolean(row.archived),
    metadataJson: asNullableString(row.metadata_json),
    createdAt: String(row.created_at),
    updatedAt: String(row.updated_at),
  };
}

function mapLoop(row: Record<string, unknown>): LoopRecord {
  return {
    id: String(row.id),
    seq: Number(row.seq),
    projectId: String(row.project_id),
    type: String(row.type),
    targetType: String(row.target_type),
    targetId: asNullableString(row.target_id),
    repo: asNullableString(row.repo),
    prNumber: asNullableNumber(row.pr_number),
    status: String(row.status),
    configJson: asNullableString(row.config_json),
    metadataJson: asNullableString(row.metadata_json),
    lastRunAt: asNullableString(row.last_run_at),
    nextRunAt: asNullableString(row.next_run_at),
    createdAt: String(row.created_at),
    updatedAt: String(row.updated_at),
  };
}

function mapRun(row: Record<string, unknown>): RunRecord {
  return {
    id: String(row.id),
    loopId: String(row.loop_id),
    status: String(row.status),
    currentStep: asNullableString(row.current_step),
    lastCompletedStep: asNullableString(row.last_completed_step),
    checkpointJson: asNullableString(row.checkpoint_json),
    summary: asNullableString(row.summary),
    errorMessage: asNullableString(row.error_message),
    startedAt: String(row.started_at),
    lastHeartbeatAt: asNullableString(row.last_heartbeat_at),
    endedAt: asNullableString(row.ended_at),
    createdAt: String(row.created_at),
    updatedAt: String(row.updated_at),
  };
}

function mapPullRequestSnapshot(
  row: Record<string, unknown>,
): PullRequestSnapshotRecord {
  return {
    id: String(row.id),
    projectId: String(row.project_id),
    repo: String(row.repo),
    prNumber: Number(row.pr_number),
    headSha: String(row.head_sha),
    baseSha: asNullableString(row.base_sha),
    title: asNullableString(row.title),
    body: asNullableString(row.body),
    author: asNullableString(row.author),
    diffRef: asNullableString(row.diff_ref),
    checksSummary: asNullableString(row.checks_summary),
    unresolvedThreadCount: asNullableNumber(row.unresolved_thread_count),
    reviewState: asNullableString(row.review_state),
    payloadJson: asNullableString(row.payload_json),
    capturedAt: String(row.captured_at),
    createdAt: String(row.created_at),
  };
}

function mapEvent(row: Record<string, unknown>): EventLogRecord {
  return {
    id: String(row.id),
    eventType: String(row.event_type),
    projectId: asNullableString(row.project_id),
    loopId: asNullableString(row.loop_id),
    runId: asNullableString(row.run_id),
    entityType: asNullableString(row.entity_type),
    entityId: asNullableString(row.entity_id),
    correlationId: asNullableString(row.correlation_id),
    causationId: asNullableString(row.causation_id),
    actorType: asNullableString(row.actor_type),
    actorId: asNullableString(row.actor_id),
    actorDisplayName: asNullableString(row.actor_display_name),
    payloadJson: String(row.payload_json),
    createdAt: String(row.created_at),
  };
}

function mapLock(row: Record<string, unknown>): LockRecord {
  return {
    key: String(row.key),
    owner: String(row.owner),
    reason: asNullableString(row.reason),
    expiresAt: String(row.expires_at),
    createdAt: String(row.created_at),
    updatedAt: String(row.updated_at),
  };
}

function mapQueueItem(row: Record<string, unknown>): QueueItemRecord {
  return {
    id: String(row.id),
    projectId: asNullableString(row.project_id),
    loopId: asNullableString(row.loop_id),
    type: String(row.type),
    targetType: String(row.target_type),
    targetId: String(row.target_id),
    repo: asNullableString(row.repo),
    prNumber: asNullableNumber(row.pr_number),
    dedupeKey: String(row.dedupe_key),
    priority: Number(row.priority),
    status: String(row.status) as QueueItemRecord["status"],
    availableAt: String(row.available_at),
    attempts: Number(row.attempts),
    maxAttempts: Number(row.max_attempts),
    claimedBy: asNullableString(row.claimed_by),
    claimedAt: asNullableString(row.claimed_at),
    startedAt: asNullableString(row.started_at),
    finishedAt: asNullableString(row.finished_at),
    lockKey: asNullableString(row.lock_key),
    payloadJson: asNullableString(row.payload_json),
    lastError: asNullableString(row.last_error),
    lastErrorKind: asNullableString(
      row.last_error_kind,
    ) as QueueFailureKind | null,
    createdAt: String(row.created_at),
    updatedAt: String(row.updated_at),
  };
}

function mapAgentExecution(row: Record<string, unknown>): AgentExecutionRecord {
  return {
    id: String(row.id),
    projectId: asNullableString(row.project_id),
    loopId: asNullableString(row.loop_id),
    runId: asNullableString(row.run_id),
    vendor: String(row.vendor),
    status: String(row.status),
    pid: asNullableNumber(row.pid),
    commandJson: String(row.command_json),
    cwd: String(row.cwd),
    summary: asNullableString(row.summary),
    parseStatus: asNullableString(row.parse_status),
    completionSignal: asNullableString(row.completion_signal),
    heartbeatCount: Number(row.heartbeat_count),
    lastHeartbeatAt: asNullableString(row.last_heartbeat_at),
    outputJson: asNullableString(row.output_json),
    errorMessage: asNullableString(row.error_message),
    startedAt: String(row.started_at),
    endedAt: asNullableString(row.ended_at),
    metadataJson: asNullableString(row.metadata_json),
    createdAt: String(row.created_at),
    updatedAt: String(row.updated_at),
  };
}

function mapNotification(row: Record<string, unknown>): NotificationRecord {
  return {
    id: String(row.id),
    projectId: asNullableString(row.project_id),
    loopId: asNullableString(row.loop_id),
    runId: asNullableString(row.run_id),
    entityType: asNullableString(row.entity_type),
    entityId: asNullableString(row.entity_id),
    channel: String(row.channel),
    level: String(row.level),
    title: String(row.title),
    subtitle: asNullableString(row.subtitle),
    body: String(row.body),
    status: String(row.status),
    dedupeKey: asNullableString(row.dedupe_key),
    errorMessage: asNullableString(row.error_message),
    payloadJson: asNullableString(row.payload_json),
    sentAt: asNullableString(row.sent_at),
    createdAt: String(row.created_at),
    updatedAt: String(row.updated_at),
  };
}

function mapWorktree(row: Record<string, unknown>): WorktreeRecord {
  return {
    id: String(row.id),
    projectId: String(row.project_id),
    repoPath: String(row.repo_path),
    worktreePath: String(row.worktree_path),
    branch: String(row.branch),
    baseBranch: String(row.base_branch),
    status: String(row.status),
    headSha: asNullableString(row.head_sha),
    metadataJson: asNullableString(row.metadata_json),
    createdAt: String(row.created_at),
    updatedAt: String(row.updated_at),
    cleanedAt: asNullableString(row.cleaned_at),
  };
}

function asNullableString(value: unknown): string | null {
  if (value === null || value === undefined) {
    return null;
  }

  return String(value);
}

function asNullableNumber(value: unknown): number | null {
  if (value === null || value === undefined) {
    return null;
  }

  return Number(value);
}

function asBoolean(value: unknown): boolean {
  return value === 1 || value === true || value === "1";
}

const SCHEDULED_QUEUE_BASE_QUERY = `
  SELECT qi.*
  FROM queue_items qi
  LEFT JOIN loops l ON l.id = qi.loop_id
  WHERE qi.status = 'queued'
    AND qi.available_at <= ?1
    AND COALESCE(l.status, 'queued') NOT IN ('paused', 'completed', 'failed', 'interrupted')
    AND (
      qi.lock_key IS NULL
      OR NOT EXISTS (
        SELECT 1
        FROM queue_items lock_blocker
        WHERE lock_blocker.lock_key = qi.lock_key
          AND lock_blocker.status = 'running'
          AND lock_blocker.id != qi.id
      )
    )
    AND (
      qi.type != 'fixer'
      OR qi.repo IS NULL
      OR qi.pr_number IS NULL
      OR NOT EXISTS (
        SELECT 1
        FROM queue_items blocker
        WHERE blocker.type = 'reviewer'
          AND blocker.repo = qi.repo
          AND blocker.pr_number = qi.pr_number
          AND blocker.status IN ('queued', 'running')
          AND blocker.id != qi.id
      )
    )
`;

const SCHEDULED_QUEUE_ORDER_BY = `
  ORDER BY qi.priority ASC, qi.available_at ASC, qi.created_at ASC
`;

const SCHEDULED_QUEUE_QUERY = `${SCHEDULED_QUEUE_BASE_QUERY}${SCHEDULED_QUEUE_ORDER_BY}`;
