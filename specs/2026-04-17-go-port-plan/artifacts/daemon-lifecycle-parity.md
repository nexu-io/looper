# Daemon startup, shutdown, recovery, and run-lifecycle parity expectations

Source of truth inspected from:

- `apps/looperd/src/bootstrap/index.ts`
- `apps/looperd/src/runtime/index.ts`
- `apps/looperd/src/runtime/index.test.ts`
- `apps/looperd/src/server/index.ts`
- `specs/2026-04-17-go-port-plan/spec.md`
- `specs/2026-04-17-go-port-plan/artifacts/sqlite-inventory.md`
- `specs/2026-04-17-go-port-plan/artifacts/daemon-http-endpoints.md`

## Compatibility boundary

- The Go daemon must preserve the current lifecycle ordering: config load/validation and runtime path checks happen before runtime start; runtime start opens SQLite, runs startup migration when enabled, syncs configured projects, runs recovery, starts the HTTP server, performs an immediate scheduler tick, then starts the polling scheduler.
- Startup is fail-fast. If config loading, tool validation, runtime path checks, SQLite initialization/migration, or HTTP server startup fails, `looperd` does not enter a partially running state.
- Recovery runs before the API is considered live. `/api/v1/status` must expose the recovery summary produced during startup, and startup recovery effects must already be persisted by the time the endpoint is reachable.
- Shutdown is two-layered today:
  - process shutdown: `bootstrapLooperd()` registers `SIGINT` and `SIGTERM` to call `runtime.stop(signal)` when `waitForShutdown` is enabled
  - runtime shutdown: `runtime.stop(reason)` appends `looperd.stopped`, stops the HTTP server, stops scheduler polling, waits for in-flight scheduler work up to the fixed timeout, then closes the store and resolves `waitForShutdown()`
- Loop stop requests are best-effort and stateful, not a hard synchronous kill guarantee. The current compatibility contract is the persisted state/result of the stop attempt.
- Run and loop lifecycle state are part of parity. The Go port must preserve the existing loop statuses (`idle`, `queued`, `running`, `paused`, `completed`, `failed`, `interrupted`) and run statuses (`queued`, `running`, `success`, `failed`, `cancelled`, `interrupted`, `parse_failed`) plus the recovery/stop transitions documented below.

## Startup parity expectations

### Required ordering

1. Load config from defaults, config file, env, and CLI overrides.
2. Validate runtime paths before runtime creation:
   - ensure `daemon.logDir` exists and is writable
   - ensure the parent directory of `storage.dbPath` exists and is writable
   - require `daemon.workingDirectory` to already be writable
3. Create the logger.
4. Create the runtime.
5. Initialize SQLite and run startup migration according to config.
6. Sync configured projects into storage.
7. Run recovery.
8. Build and start the HTTP API server.
9. Persist the bound port back into config when a dynamic port was requested.
10. Record `startedAt`, run one immediate scheduler tick, then start periodic polling.
11. Append `looperd.started` with daemon mode, host, port, and recovery summary.

### Observable startup outcomes to preserve

- `looperd.started` is emitted only after the API server starts and the first startup tick has run.
- If recovery marked interrupted runs or cleaned orphan agent executions, the daemon also emits a recovery-completed notification.
- Startup auto-discovers work immediately instead of waiting for the first poll interval; the runtime tests lock this in for reviewer and fixer discovery.
- Stale loop state is normalized during startup rather than deferred to later ticks.

## Recovery parity expectations

Recovery is a startup-only pipeline in the current daemon and must run before serving API traffic.

### Recovery actions to preserve

1. Attempt orphan agent cleanup for every active persisted agent execution:
   - send `SIGTERM` when a PID exists
   - if signaling succeeds or the process is already gone (`ESRCH`), mark the execution `killed`
   - if signaling fails for another reason, leave the execution record `running`
   - append `agent.killed` only when cleanup succeeds
2. Release expired locks and append `looperd.recovery.lock_released` per lock.
3. For each loop whose latest run is still `running`:
   - mark that run `interrupted`
   - set `endedAt`
   - preserve any existing `errorMessage`, otherwise use `Interrupted during looperd recovery`
   - append `looperd.recovery.run_interrupted`
4. Requeue resumable loops:
   - loops in `paused`, `completed`, or `failed` are not requeued
   - loops still `running`, or loops whose latest run is now `interrupted`, become `queued`
   - `nextRunAt` is set to the recovery timestamp
   - queue rows in `running` for that loop are rewritten back to `queued` and made immediately runnable
   - append `looperd.recovery.loop_requeued` with `recoveredQueueItems`
5. Normalize stale queued loops that no longer have queued/running queue rows:
   - latest run `success` -> loop `completed`
   - latest run `running` or `interrupted` -> loop `interrupted`
   - all other latest-run statuses -> loop `failed`
   - `nextRunAt` becomes `null`
   - append `looperd.recovery.loop_queue_normalized`
6. Append a final `looperd.recovery.completed` event with summary counts.

### Recovery summary contract

- The recovery summary exposed through runtime state and `/api/v1/status` currently contains:
  - `startedAt`
  - `completedAt`
  - `orphanAgentCleanup.attempted`
  - `orphanAgentCleanup.cleanedCount`
  - `expiredLocksReleased`
  - `interruptedRunsMarked`
  - `loopsRequeued`
  - `eventsWritten`
- When no runtime has started yet, the empty summary still exists and defaults to zero counts with `orphanAgentCleanup.attempted = false`.

## Shutdown parity expectations

### Runtime shutdown

- `runtime.stop(reason)` is idempotent.
- On the first call it must:
  - mark the runtime stopped
  - append `looperd.stopped` with the supplied reason
  - stop the scheduler timer so no new polling ticks are scheduled
  - stop the HTTP server
  - wait for in-flight scheduler work, but only up to `daemon.shutdownTimeoutMs` (default `1000` ms)
  - close SQLite even if waiting times out
  - resolve `waitForShutdown()`
- If waiting for in-flight scheduler work times out, shutdown still completes and only logs a warning.
- Shutdown does not, by itself, walk active loop runs and rewrite them to terminal states. Any still-running work is left for the next startup recovery pass unless separately stopped.

### Process signal handling

- With `waitForShutdown: true`, `SIGINT` and `SIGTERM` are registered exactly once and both translate into `runtime.stop(<signal>)`.
- `bootstrapLooperd()` blocks on `runtime.waitForShutdown()` only in that mode.

## Loop stop / in-flight run parity expectations

`stopLoop({ loopId, reason })` defines the current stop semantics for individual loop runs.

### State changes to preserve

- The loop is immediately marked `paused` and `nextRunAt` is cleared.
- Any queued or running queue rows for that loop are cancelled with the supplied reason.
- The runtime looks for the active persisted agent execution for the loop and the active `running` run.
- If there is an active execution with a PID:
  - send `SIGTERM`
  - if signaling succeeds, mark the execution `cancelling`, keep the run `running`, and return `stopped: true`
  - if signaling returns `ESRCH`, treat the process as already gone, mark the execution `killed`, mark the run `cancelled`, and return `stopped: true`
  - if signaling fails for another reason, leave the run/execution active and return `stopped: false`
- If there is no interruptible execution PID, the active run is not force-cancelled; the loop remains paused and queue items remain cancelled, but the active run/execution records stay active and `stopped: false` is returned unless queue-only cancellation made progress.
- Every stop attempt appends `loop.stopped` with `reason`, `executionId`, `vendor`, `pid`, and the computed `stopped` flag.

### Important current nuance to preserve

- A successful stop request does **not** mean the run has already ended. After a successful `SIGTERM`, the execution moves to `cancelling` and the run remains `running` until later completion or recovery.
- The restart/recovery path is therefore part of the stop contract for in-flight work.

## Run-lifecycle parity expectations

### Core lifecycle model

- Runtime ticks always run in this order: issue discovery, pull-request discovery, scheduled-work processing.
- Startup performs one immediate tick before polling begins.
- Queue priority remains `planner` -> `reviewer` -> `fixer` -> `worker` as documented in the main Go-port spec.
- Loop/runs remain checkpoint-driven. Recovery and retry decisions operate on persisted loop/run/queue state, not event-log replay.

### Lifecycle outcomes to preserve

- Retryable/resumable work is requeued through persisted queue state instead of creating a fresh loop identity.
- Paused loops are excluded from automatic recovery requeue.
- Completed and failed loops stay terminal across restart.
- Interrupted runs are a first-class terminal record of daemon interruption and are used to decide whether the parent loop should resume.
- Event logs are audit output for lifecycle transitions, not the source of truth for reconstructing current state.

## Minimum parity tests the Go port should satisfy

- startup succeeds with valid config and publishes a recovery summary through `/api/v1/status`
- startup fails before serving API traffic when config validation, path checks, DB initialization, or server start fails
- startup recovery marks `running` runs `interrupted`, requeues resumable loops, clears expired locks, and writes the recovery event set
- startup normalizes stale queued loops without recreating missing queue rows
- startup performs immediate discovery/tick work before the first polling interval
- runtime stop is idempotent and resolves `waitForShutdown()`
- shutdown waits for in-flight scheduler work only up to the configured timeout budget, then closes cleanly
- loop stop preserves the three current branches: successful `SIGTERM` -> `cancelling`, `ESRCH` -> `killed/cancelled`, other signal error or missing PID -> best-effort pause without terminalizing the run

## Go-port notes

- Keep this document as the behavioral source of truth for lifecycle parity.
- The next task should capture operator-focused lifecycle notes for daemon start/stop/recovery/graceful shutdown; this file defines the compatibility contract the notes should describe, not the operator playbook itself.
