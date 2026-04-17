# SQLite schema reuse decision

## Decision

The Go port will reuse the current SQLite schema and migration lineage from the TypeScript daemon.

Status: accepted for Phase 3 storage work.

## Source of truth

The compatibility boundary for this decision is the existing TypeScript storage contract and the artifacts already captured for the port plan:

- `apps/looperd/src/storage/sqlite/migrations/*.sql`
- `apps/looperd/src/storage/sqlite/migrations.gen.ts`
- `apps/looperd/src/storage/sqlite/migrate.ts`
- `apps/looperd/src/storage/sqlite/db.ts`
- `specs/2026-04-17-go-port-plan/artifacts/sqlite-inventory.md`
- `specs/2026-04-17-go-port-plan/artifacts/sqlite-migration-sequence.md`
- `specs/2026-04-17-go-port-plan/artifacts/sqlite-schema.snapshot.sql`

Today that means preserving the schema produced by migrations `0001` through `0007`, with `0007_agent_execution_run_index` as the current latest migration ID.

## Blocker check

No blocker was found that justifies a schema redesign before the Go port:

- The current schema already covers the daemon's runtime state: projects, loops, runs, locks, queue items, event logs, PR snapshots, agent executions, notifications, worktrees, and counters.
- The migration sequence and resulting DDL have already been captured in machine-reviewable artifacts, which makes parity validation practical without inventing a new import path.
- The Go port does not yet have a competing storage model that would require a different schema.
- Reusing the schema keeps TypeScript-created databases in scope for direct validation instead of introducing a second migration project.

## Implications

- The Go migration runner must preserve the existing migration IDs, lexical ordering, and `schema_migrations` bookkeeping behavior.
- The Go storage layer must match the current post-migration DDL in `sqlite-schema.snapshot.sql`, not a merely similar schema.
- Startup migration, backup, queue recovery, and event-log behavior remain part of the persisted compatibility boundary.
- If a future blocker is found, it must be documented explicitly before changing schema shape or migration history.

## Follow-on work

This decision feeds the next storage tasks:

1. Decide and document the initial single-connection SQLite model.
2. Port embedded migrations.
3. Preserve migration naming/order and `schema_migrations` behavior.
4. Validate the Go migration runner against databases created by the TypeScript runner.
