# SQLite schema DDL snapshot and migration-sequence notes

This artifact complements `sqlite-inventory.md` with the exact post-migration DDL and the migration-order notes needed to reproduce it.

## Snapshot source

- Migration files: `apps/looperd/src/storage/sqlite/migrations/*.sql`
- Runner: `apps/looperd/src/storage/sqlite/migrate.ts`
- Snapshot file: `internal/storage/testdata/schema/sqlite-schema.snapshot.sql`

The snapshot was produced by applying migrations `0001` through `0007` in lexical order to an empty SQLite database and then reading `sqlite_master` for all non-internal tables and indexes.

## Final schema objects

Tables present after the full migration sequence:

- `agent_executions`
- `counters`
- `event_logs`
- `locks`
- `loops`
- `notifications`
- `projects`
- `pull_request_snapshots`
- `queue_items`
- `runs`
- `schema_migrations`
- `worktrees`

Indexes present after the full migration sequence:

- `idx_agent_executions_loop_started_at`
- `idx_agent_executions_run`
- `idx_agent_executions_status_started_at`
- `idx_event_logs_entity_created_at`
- `idx_event_logs_type_created_at`
- `idx_locks_expires_at`
- `idx_loops_next_run_at`
- `idx_loops_repo_pr`
- `idx_loops_seq`
- `idx_loops_status`
- `idx_loops_target`
- `idx_notifications_dedupe`
- `idx_notifications_entity_created_at`
- `idx_projects_archived`
- `idx_pull_request_snapshots_repo_pr`
- `idx_queue_items_dedupe_status`
- `idx_queue_items_loop_status`
- `idx_queue_items_status_available_priority`
- `idx_queue_items_type_repo_pr_status`
- `idx_runs_loop_id_started_at`
- `idx_runs_status`
- `idx_worktrees_project_branch`
- `idx_worktrees_project_status`

There are no views or triggers in the current runtime schema.

## Migration sequence

| Order | Migration | Schema effect | Notes |
| --- | --- | --- | --- |
| 1 | `0001_init.sql` | Creates `schema_migrations`, `projects`, `loops`, `runs`, `locks`, `event_logs`, and `pull_request_snapshots` plus their base indexes. | Establishes the initial contract and `schema_migrations` bookkeeping table. |
| 2 | `0002_integrations.sql` | Adds `agent_executions`, `notifications`, and `worktrees`. | Also adds the initial `idx_agent_executions_run` index. |
| 3 | `0003_scheduler_queue.sql` | Adds durable `queue_items` and its scheduler/retry indexes. | Introduces the persisted queue contract used by recovery and worker scheduling. |
| 4 | `0004_worker_project_target.sql` | Rebuilds `loops`, `queue_items`, `agent_executions`, and `worktrees`; drops obsolete `tasks` and `task_items`. | Requires foreign keys to be disabled during table rebuilds. Normalizes worker targeting to `project`, makes `command_json`, `cwd`, and `base_branch` nullable, and recreates affected indexes. |
| 5 | `0005_planner_issue_target.sql` | Rebuilds `loops` to allow `target_type = 'issue'`. | Also runs with foreign keys disabled during the rebuild. |
| 6 | `0006_loop_seq_handles.sql` | Rebuilds `loops` to add `seq`, adds `counters`, and creates `idx_loops_seq`. | Populates `seq` with `ROW_NUMBER() OVER (ORDER BY created_at, id)` and seeds `counters.name = 'loop_seq'` to the current max sequence. |
| 7 | `0007_agent_execution_run_index.sql` | Adds `idx_agent_executions_run` with `IF NOT EXISTS`. | This preserves compatibility across databases where the index may already exist from earlier migrations. |

## Runner behavior notes

- The migration runner only accepts files matching `^\d{4}_[a-zA-Z0-9_\-]+\.sql$`.
- Migrations are applied in lexical filename order, and each successful migration is inserted into `schema_migrations` inside the same transaction as the SQL payload.
- If a migration contains `PRAGMA foreign_keys = ON/OFF`, the runner temporarily switches the connection to that setting before running the transaction and restores the previous setting afterward.
- The foreign-key pragma handling is required for rebuild migrations `0004`, `0005`, and `0006`.
- When backup is required, the runner creates an on-disk backup with `VACUUM INTO` before applying pending migrations.

## Compatibility implications for the Go port

- Reusing the current schema means matching the final DDL in `internal/storage/testdata/schema/sqlite-schema.snapshot.sql`, not just recreating tables with similar names.
- Migration compatibility includes preserving lexical ordering, `schema_migrations` IDs, and the same foreign-key toggling behavior for rebuild migrations.
- The latest schema version is `0007_agent_execution_run_index`; the latest schema shape still includes all runtime tables inventoried in `sqlite-inventory.md`.
