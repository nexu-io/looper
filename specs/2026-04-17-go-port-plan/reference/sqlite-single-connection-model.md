# Initial single-connection SQLite model

## Decision

The Go daemon will start with a single SQLite connection model for Phase 3 storage work.

Status: accepted for the initial Go storage port.

## Model

- Open one process-local `*sql.DB` for the daemon's configured SQLite file.
- Configure the pool as a single underlying connection with `SetMaxOpenConns(1)` and `SetMaxIdleConns(1)`.
- Route all repository access through that shared coordinator instead of opening per-repository or per-request connections.
- Preserve the current SQLite startup settings on that connection: WAL mode, `foreign_keys = ON`, and `busy_timeout = 5000`.
- Keep the migration runner, backup flow, and transaction helpers on the same coordinator so migration-time pragma changes and `VACUUM INTO` behavior stay scoped to the same connection model.

## Why this is the initial model

### 1. It matches the current daemon's effective behavior

The TypeScript daemon uses a single `bun:sqlite` `Database` handle via `SqliteDbCoordinator` in `apps/looperd/src/storage/sqlite/db.ts`. Store operations run through that one handle, and startup migration/healthcheck/backup logic all hang off the same coordinator. Starting the Go port from one shared `*sql.DB` with one underlying SQLite connection is the closest parity target.

### 2. It avoids SQLite pool footguns during the parity phase

SQLite pragma state such as `foreign_keys` is connection-scoped. The current migration runner in `apps/looperd/src/storage/sqlite/migrate.ts` temporarily toggles `PRAGMA foreign_keys` for table-rebuild migrations. A multi-connection pool would make that behavior easier to get wrong during the first port. A single connection keeps the pragma scope predictable.

### 3. The workload does not justify a more complex model yet

`looperd` is a local daemon with one process owning one SQLite file. The main requirements today are correctness, migration parity, queue durability, and recoverable runtime state. The current workload does not require a read pool, cross-process writers, or a more elaborate SQLite topology before parity is proven.

## Constraints and guardrails

- The daemon remains the sole writer to the database file; the CLI continues talking to the daemon over HTTP rather than touching SQLite directly.
- Multi-statement write paths should go through shared transaction helpers on the coordinator.
- Long-running non-database work must stay outside open SQLite transactions.
- Startup migration still runs before deeper runtime assembly so schema changes, backups, and recovery checks happen before scheduler/API traffic.
- Any future move away from a single-connection model must be justified by measured contention or a concrete correctness requirement, not by default `database/sql` pooling behavior.

## Deferred on purpose

These are not part of the initial model decision:

- separate read-only connection pools
- connection-per-request or connection-per-repository patterns
- cross-process database access patterns
- changing the SQLite driver chosen for Phase 1

## Follow-on implications

This decision sets the baseline for the next storage tasks:

1. Port embedded migrations onto the shared coordinator.
2. Preserve migration ordering and `schema_migrations` behavior under the single-connection model.
3. Port DB open/close and transaction helpers with one coordinator-owned `*sql.DB`.
4. Add SQLite integration tests that prove concurrent Go callers do not break the serialized model.
