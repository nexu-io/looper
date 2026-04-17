# SQLite schema, migrations, and repository inventory

Source of truth inspected from:

- `apps/looperd/src/storage/store.ts`
- `apps/looperd/src/storage/types.ts`
- `apps/looperd/src/storage/sqlite/sqlite-store.ts`
- `apps/looperd/src/storage/sqlite/db.ts`
- `apps/looperd/src/storage/sqlite/migrate.ts`
- `apps/looperd/src/storage/sqlite/migrations.gen.ts`
- `apps/looperd/src/storage/sqlite/migrations/*.sql`
- `apps/looperd/src/runtime/index.ts`
- `apps/looperd/src/server/index.ts`

## Storage module map

- `apps/looperd/src/storage/store.ts:17-125` defines the storage contract consumed by the daemon: projects, loops, runs, pull-request snapshots, events, locks, queue, agent executions, notifications, worktrees, and schema helpers.
- `apps/looperd/src/storage/types.ts:54-243` defines the record shapes that each repository persists, including queue status/failure enums and storage health/migration status payloads.
- `apps/looperd/src/storage/sqlite/sqlite-store.ts:26-900` is the concrete SQLite implementation of that contract.
- `apps/looperd/src/storage/sqlite/db.ts:22-99` owns database creation, connection lifetime, pragmas, migration wiring, healthcheck, and backup delegation.
- `apps/looperd/src/storage/sqlite/migrate.ts:30-221` owns migration discovery, lexical ordering, pending/applied status, `schema_migrations`, transactional execution, and optional backup via `VACUUM INTO`.
- `apps/looperd/src/storage/sqlite/migrations.gen.ts` is the generated embedded migration bundle used by default at runtime.
- `apps/looperd/src/runtime/index.ts:133-146` shows runtime startup creating `SqliteStore` and running `initialize({ autoMigrate, requireBackup })` before scheduler/server assembly.
- `apps/looperd/src/server/index.ts:301-333` shows the daemon status surface exposing `schemaVersion` and pending migration IDs from `store.schema`.

## Current runtime schema inventory

The current schema is the result of applying migrations `0001` through `0007` in lexical order.

| Table | Current role | Key columns / constraints | Source |
| --- | --- | --- | --- |
| `schema_migrations` | Tracks applied migration IDs. | `id` primary key, `applied_at`. | `0001_init.sql:1-4`, `migrate.ts:154-185` |
| `projects` | Registered repos/projects for loop orchestration. | `id` primary key, `repo_path`, optional `base_branch`, `archived` check. | `0001_init.sql:6-18` |
| `loops` | Persisted reviewer/fixer/planner/worker loop definitions. | `id` primary key, unique `seq`, `project_id` FK, `type`, `target_type`, `target_id`, `repo`, `pr_number`, `status`, scheduling timestamps. | `0006_loop_seq_handles.sql:3-66` |
| `runs` | Historical and active executions per loop. | `id` primary key, `loop_id` FK, status/step/checkpoint/heartbeat timestamps. | `0001_init.sql:45-63` |
| `locks` | Advisory runtime locks with expiry. | `key` primary key, `owner`, optional `reason`, `expires_at`. | `0001_init.sql:65-74` |
| `event_logs` | Append-only event history for loops/runs/entities. | `id` primary key, optional project/loop/run FKs, entity and actor metadata, `payload_json`. | `0001_init.sql:76-97` |
| `pull_request_snapshots` | Cached GitHub PR state captured by the daemon. | `id` primary key, `project_id` FK, `repo`, `pr_number`, SHAs, review/check metadata, `payload_json`, `captured_at`. | `0001_init.sql:99-120` |
| `agent_executions` | Persisted coding-agent process lifecycle and output metadata. | `id` primary key, optional project/loop/run FKs, `vendor`, `status`, `pid`, `command_json`, heartbeat/output fields. | `0002_integrations.sql:1-30`, `0004_worker_project_target.sql:199-245`, `0007_agent_execution_run_index.sql:1-2` |
| `notifications` | In-app / osascript notification history and dedupe tracking. | `id` primary key, optional project/loop/run/entity refs, `channel`, `level`, `status`, `dedupe_key`, `payload_json`, `sent_at`. | `0002_integrations.sql:31-57` |
| `worktrees` | Git worktree lifecycle records. | `id` primary key, `project_id` FK, repo/worktree paths, `branch`, `base_branch`, `status`, `head_sha`, `cleaned_at`. | `0002_integrations.sql:58-75`, `0004_worker_project_target.sql:247-279` |
| `queue_items` | Durable scheduler queue and retry/recovery state. | `id` primary key, optional project/loop FKs, `type`, target selectors, `dedupe_key`, priority, claim/retry/failure fields. | `0003_scheduler_queue.sql:1-43`, `0004_worker_project_target.sql:78-197` |
| `counters` | Monotonic named counters, currently loop sequence allocation. | `name` primary key, integer `value`; seeded with `loop_seq`. | `0006_loop_seq_handles.sql:68-75` |

## Runtime-table inventory for parity work

These are the tables that actively carry daemon runtime state today, including the notifications/worktree surfaces that need explicit parity in the Go port.

| Table | Runtime responsibility | Key persisted state | Repository surface |
| --- | --- | --- | --- |
| `projects` | Anchors runtime state to a registered repository. | Repo path, display name, base branch, archive bit, timestamps. | `projects.upsert/list/get` |
| `loops` | Persists loop definitions and current orchestration status. | Loop type/target, repo + PR selectors, status, scheduling timestamps, monotonic `seq`. | `loops.upsert/list/get/nextSeq` |
| `runs` | Tracks each active or historical loop execution. | Status, step, trigger, branch metadata, started/finished/heartbeat timestamps, error fields. | `runs.upsert/list/get/getLatestByLoopId` |
| `locks` | Coordinates daemon-owned runtime exclusivity. | Lock owner/reason plus expiry for stale-lock recovery. | `locks.acquire/release/listExpired` |
| `event_logs` | Append-only runtime audit trail. | Event type, entity linkage, actor metadata, opaque payload JSON, created timestamp. | `events.append/list/listByEntity` |
| `pull_request_snapshots` | Caches external GitHub state consumed by runtime decisions. | PR identifiers, SHA/review/check state, payload JSON, capture timestamp. | `pullRequestSnapshots.upsert/listLatest/getLatestByRepoAndNumber` |
| `agent_executions` | Persists coding-agent process lifecycle. | Vendor, status, process id, command/cwd JSON, heartbeat/output/error fields, timing data. | `agentExecutions.upsert/listActive/listByRunId/getLatestByRunId/getById` |
| `notifications` | Persists notification delivery history and dedupe state. | Channel, level, title/body payload JSON, status, dedupe key, sent/error timestamps. | `notifications.upsert/listByRunId/findRecentByDedupeKey` |
| `worktrees` | Persists git worktree allocation/cleanup lifecycle. | Project linkage, repo/worktree paths, branch/base branch, status, head SHA, cleanup timestamp. | `worktrees.upsert/listByProjectId/getById/getByBranch` |
| `queue_items` | Durable scheduler queue for deferred/retryable runtime work. | Work type/target selectors, dedupe key, status, attempts, claim metadata, retry/error fields. | `queue.enqueue/claim*/complete/markRetry/fail/requeueRunningByLoop/cancelByLoop` |
| `counters` | Stores runtime sequence allocator state. | Named integer counters; currently `loop_seq`. | `loops.nextSeq` |

Notes:

- `notifications` and `worktrees` are not optional adjunct tables; both are first-class repositories on the `Store` contract and appear in the post-migration DDL snapshot.
- `queue_items`, `agent_executions`, `notifications`, `worktrees`, `runs`, and `locks` together capture the daemon's recoverable in-flight runtime state; `event_logs` and `pull_request_snapshots` provide supporting audit/cache state that the runtime also persists.
- `schema_migrations` remains migration bookkeeping rather than business/runtime state, but it is still part of the persisted compatibility boundary because startup migration behavior depends on it.

## Migration inventory

Migration source files live in `apps/looperd/src/storage/sqlite/migrations/` and are embedded through `migrations.gen.ts` for normal runtime use.

| Order | Migration | Purpose |
| --- | --- | --- |
| 1 | `0001_init.sql` | Creates the base schema: `schema_migrations`, `projects`, `loops`, `runs`, `locks`, `event_logs`, and `pull_request_snapshots`. |
| 2 | `0002_integrations.sql` | Adds `agent_executions`, `notifications`, and `worktrees`. |
| 3 | `0003_scheduler_queue.sql` | Adds durable `queue_items` for scheduler persistence/retry state. |
| 4 | `0004_worker_project_target.sql` | Rebuilds `loops`, `queue_items`, `agent_executions`, and `worktrees` to normalize worker targeting and cleanup obsolete `tasks` tables. |
| 5 | `0005_planner_issue_target.sql` | Expands `loops.target_type` to include `issue`. |
| 6 | `0006_loop_seq_handles.sql` | Adds `loops.seq`, unique loop handle indexing, and the `counters` table for sequence allocation. |
| 7 | `0007_agent_execution_run_index.sql` | Adds/normalizes the `agent_executions (run_id, started_at desc)` index. |

## Migration runner behavior

- `apps/looperd/src/storage/sqlite/migrate.ts:13-14` only accepts files matching `^\d{4}_[a-zA-Z0-9_\-]+\.sql$`.
- `apps/looperd/src/storage/sqlite/migrate.ts:45-50` uses embedded migrations by default and falls back to `migrationsDir` when explicitly configured.
- `apps/looperd/src/storage/sqlite/migrate.ts:57-66` computes `available`, `applied`, and `pending` migration sets.
- `apps/looperd/src/storage/sqlite/migrate.ts:68-140` runs pending migrations in order and records each applied ID in `schema_migrations` inside the same transaction as the SQL.
- `apps/looperd/src/storage/sqlite/migrate.ts:92-129` temporarily toggles `PRAGMA foreign_keys` when a migration file declares it, which is required by table-rebuild migrations `0004`, `0005`, and `0006`.
- `apps/looperd/src/storage/sqlite/migrate.ts:142-152` creates an on-disk backup with `VACUUM INTO` when backup is requested.
- `apps/looperd/src/storage/sqlite/db.ts:42-44` wires startup auto-migration to the daemon config flags.

## Repository responsibility map

The `Store` contract in `apps/looperd/src/storage/store.ts:17-125` is implemented by these SQLite repository groups in `sqlite-store.ts`:

| Repository surface | Responsibilities | Source |
| --- | --- | --- |
| `projects` | Upsert/list/get registered project metadata. | `sqlite-store.ts:50-87` |
| `loops` | Upsert/list/get loops and allocate monotonically increasing `seq` values using `counters`. | `sqlite-store.ts:89-182` |
| `runs` | Upsert/list/get runs, including latest-per-loop and status filtering. | `sqlite-store.ts:184-252` |
| `pullRequestSnapshots` | Upsert/list/get latest PR snapshots by repo + PR number. | `sqlite-store.ts:254-316` |
| `events` | Append and query event-log records globally or by entity. | `sqlite-store.ts:318-355` |
| `locks` | Acquire expiring locks, release them, and enumerate expired locks. | `sqlite-store.ts:357-399` |
| `queue` | Persist queue items, claim runnable work, record completion/retry/failure, and recover/cancel loop-owned work. | `sqlite-store.ts:401-672` |
| `agentExecutions` | Upsert/list/get agent process records, including latest per run and active executions. | `sqlite-store.ts:674-767` |
| `notifications` | Upsert/list/get notification records and dedupe lookups. | `sqlite-store.ts:769-837` |
| `worktrees` | Upsert/list/get worktree records by id/branch/project. | `sqlite-store.ts:839-894` |
| `schema` | Expose migration status, healthcheck, and backup hooks from the coordinator. | `sqlite-store.ts:896-900` |

## Scheduler queue and event-log recovery / retention semantics

### `queue_items`

- Schema contract: `queue_items` is the durable scheduler queue introduced in `0003_scheduler_queue.sql` and later widened in `0004_worker_project_target.sql`; it stores work identity (`type`, `target_type`, `target_id`, `repo`, `pr_number`, `dedupe_key`), scheduling fields (`priority`, `available_at`), and recovery fields (`status`, `attempts`, `max_attempts`, claim timestamps, error fields). The status enum is fixed to `queued`, `running`, `completed`, `failed`, `cancelled`, and `manual_intervention`. `project_id` and `loop_id` are optional foreign keys with `ON DELETE CASCADE`.
- Scheduling/claim semantics: runnable work is selected from rows whose status is `queued` and `available_at <= now`, ordered by priority/age, then atomically updated to `running` by `queue.claimNext()` / `queue.claimNextOfType()` (`sqlite-store.ts:492-566`). Dedupe only suppresses concurrent active work: `findActiveByDedupe()` checks `queued` + `running` rows, not terminal rows (`sqlite-store.ts:481-490`).
- Retry/failure semantics: `queue.markRetry()` returns an item to `queued`, bumps `attempts`, clears claim fields, and preserves the last error metadata for later inspection (`sqlite-store.ts:577-607`). `queue.fail()` leaves the row in place with terminal status `failed` or `manual_intervention` depending on `last_error_kind` (`sqlite-store.ts:608-637`). `queue.complete()` marks rows `completed` without deleting them (`sqlite-store.ts:568-576`).
- Recovery semantics: on daemon startup, `runRecoveryPipeline()` marks interrupted runs, then calls `queue.requeueRunningByLoop()` for loops that should resume; this rewrites any `running` row for that loop back to `queued`, clears claim/start/finish metadata, and makes it immediately runnable (`runtime/index.ts:676-729`, `sqlite-store.ts:639-654`). If a loop is still marked `queued` but no active queue row remains, runtime normalizes the loop status instead of reconstructing queue rows (`runtime/index.ts:732-773`). Explicit loop cleanup uses `queue.cancelByLoop()` to turn active queued/running rows into `cancelled` terminal rows with an optional reason (`sqlite-store.ts:655-672`).
- Retention semantics: no TTL, pruning job, archive table, or delete-on-completion behavior exists for `queue_items`. Rows are updated in place and retained after completion/failure/cancellation/manual intervention. The only automatic deletion path is foreign-key cascade when the owning project or loop is removed (`0003_scheduler_queue.sql:26-27`).

### `event_logs`

- Schema contract: `event_logs` is the append-only runtime history table from `0001_init.sql`; it stores `event_type`, optional `project_id` / `loop_id` / `run_id`, entity and actor metadata, opaque `payload_json`, and `created_at`, with indexes for entity timeline and event-type timeline queries (`0001_init.sql:76-97`).
- Write/read semantics: the repository only exposes `events.append()`, `events.list(limit)`, and `events.listByEntity(entityType, entityId)`; there is no update or delete API (`sqlite-store.ts:318-355`). Reads return newest-first globally and oldest-first for a single entity timeline.
- Recovery semantics: startup recovery writes new audit events such as `looperd.recovery.lock_released`, `looperd.recovery.run_interrupted`, `looperd.recovery.loop_requeued`, `looperd.recovery.loop_queue_normalized`, and `looperd.recovery.completed` through `appendEvent()` (`runtime/index.ts:650-791`, `866-878`). Recovery consumes queue/run state directly; it does not replay, compact, or derive state from prior `event_logs` rows.
- Retention semantics: no retention policy or cleanup path exists for `event_logs`. Parent deletions preserve history by nulling `project_id`, `loop_id`, and `run_id` (`ON DELETE SET NULL`) instead of deleting the event row (`0001_init.sql:91-93`). In practice the table is an indefinite audit log unless an external/manual cleanup step is introduced.

### Recovery / retention evidence matrix

| Surface | Recovery behavior that must be preserved | Retention behavior that must be preserved | Primary evidence |
| --- | --- | --- | --- |
| `queue_items` | Startup recovery requeues `running` rows for resumable loops, clears claim/start/finish metadata, and leaves the row immediately runnable; loops still marked `queued` without an active queue row are normalized in loop state rather than recreating queue rows. Retryable failures stay in the same row and move back to `queued` with exponential backoff; terminal failures stay as terminal rows. | Completed, failed, cancelled, and manual-intervention rows are retained in-place. No TTL, pruning job, or archive table exists; only project/loop deletion cascades remove rows. | `runtime/index.ts:676-773`, `sqlite-store.ts:568-672`, `scheduler/index.ts:118-155`, `runtime/index.test.ts:967-1086`, `sqlite-store.test.ts:759-878`, `scheduler/index.test.ts:22-156` |
| `event_logs` | Recovery appends audit rows describing recovery actions, but recovery decisions are based on current queue/run/lock state rather than replaying prior events. | The table is append-only with no delete/update API and no built-in cleanup policy. Parent deletion preserves history by nulling foreign keys instead of deleting the event row. | `runtime/index.ts:650-791`, `sqlite-store.ts:318-355`, `sqlite-store.test.ts:331-347,741-754`, `0001_init.sql:76-97` |

## Compatibility notes for the Go port

- The current daemon assumes a single SQLite database file plus startup auto-migration, not an external migration step (`runtime/index.ts:133-146`, `db.ts:42-44`).
- Migration compatibility includes both the schema shape and the `schema_migrations` bookkeeping contract (`migrate.ts:57-66`, `154-185`).
- Queue durability, event history, notifications, worktrees, and agent execution state are already part of the persisted runtime contract; they are not optional side tables.
- Exact post-migration DDL and migration-order notes now live in `sqlite-schema.snapshot.sql` and `sqlite-migration-sequence.md` for machine-reviewable parity checks.
