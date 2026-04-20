# Daemon lifecycle notes

Source of truth inspected from:

- `apps/looperd/src/bootstrap/index.ts`
- `apps/looperd/src/runtime/index.ts`
- `apps/looperd/src/server/index.ts`
- `apps/cli/src/index.ts`
- `docs/configuration.md`
- `specs/2026-04-17-go-port-plan/artifacts/daemon-lifecycle-parity.md`

This document is the operator-focused companion to `daemon-lifecycle-parity.md`. The parity file defines the compatibility contract; this file describes how the current daemon actually starts, stops, recovers, and shuts down from an operator point of view.

## 1. How daemon start works today

There are two practical start paths:

1. direct daemon execution (`looperd`, or `bun run --cwd apps/looperd dev -- ...` during development)
2. `looper daemon start`, which launches `looperd` as a detached background process and writes `~/.looper/looperd.pid`

### `looper daemon start`

- binary lookup order is `~/.looper/bin/looperd`, then `$PATH`
- if the pid file exists and points to a live `looperd`, start is refused with an "already appears to be running" message
- if the pid file is stale or points at some other process, the pid file is removed and startup continues
- the CLI launches the daemon detached with stdio ignored, sleeps `100` ms, checks that the process is still alive and still looks like `looperd`, then writes the pid file
- this process manager is intentionally minimal; there is no full supervision or restart policy today

### Daemon bootstrap sequence

Once the `looperd` process starts, bootstrap is strictly ordered:

1. load config from defaults, config file, env, and CLI overrides
2. ensure `daemon.logDir` exists and is writable
3. ensure the parent directory of `storage.dbPath` exists and is writable
4. require `daemon.workingDirectory` to already be writable
5. create the logger and log bootstrap metadata
6. create the runtime and call `runtime.start()`

### Runtime startup sequence

`runtime.start()` is fail-fast and does not leave a partially running service behind. The current order is:

1. open SQLite store
2. initialize schema and run startup migrations if enabled
3. sync configured projects into storage
4. run the recovery pipeline
5. assemble scheduler, Git/GitHub adapters, agent executor, and loop runners
6. start the HTTP API server
7. persist the bound port back into runtime config when the port was dynamic
8. set `startedAt`
9. run one immediate scheduler tick
10. start periodic scheduler polling
11. append `looperd.started` and emit the user-facing "Started" notification

### When the daemon is considered live

The daemon is only considered live after the API server has started, recovery has finished, and the first scheduler tick has completed.

Operators can observe readiness through:

- successful completion of `looper daemon start`
- `looper daemon status` / `/api/v1/status`
- a `looperd.started` event
- a "Recovery completed" notification when startup recovery actually changed state

## 2. How daemon stop works today

There is no dedicated `looper daemon stop` command in the current CLI inventory.

Current daemon-stop paths are:

1. send `SIGINT` or `SIGTERM` to the `looperd` process directly
2. run `looper daemon restart`, which sends `SIGTERM` to the pid from `~/.looper/looperd.pid`, waits up to `2000` ms for exit, removes the pid file, then starts a fresh daemon

### Signal handling

- `bootstrapLooperd({ waitForShutdown: true })` registers one-shot handlers for `SIGINT` and `SIGTERM`
- either signal logs the signal name and calls `runtime.stop(signal)`
- in this mode the bootstrap path blocks on `runtime.waitForShutdown()`

### Runtime stop sequence

`runtime.stop(reason)` is idempotent. On the first call it:

1. marks the runtime stopped
2. logs that shutdown has started
3. appends `looperd.stopped` with the shutdown reason
4. clears the scheduler polling timer so no new interval ticks are scheduled
5. stops the Bun HTTP server with `server.stop(true)`
6. waits for in-flight scheduler work, but only up to `daemon.shutdownTimeoutMs` (`1000` ms by default)
7. closes SQLite and clears runtime references even if the wait timed out
8. resolves `waitForShutdown()`

If the in-flight wait times out, shutdown still completes; the daemon only logs a warning.

## 3. What graceful shutdown means today

"Graceful shutdown" is limited and specific in the current implementation:

- the API server is stopped before waiting on in-flight scheduler work
- no new polling ticks are scheduled after shutdown begins
- already claimed scheduler work is given up to `daemon.shutdownTimeoutMs` (`1000` ms by default) to finish
- the daemon does **not** walk all active runs and force them into terminal states during shutdown
- any still-running loop/run state is intentionally left for the next startup recovery pass unless that work was separately stopped earlier

This means the present system favors bounded process exit over fully draining all long-running work.

## 4. How startup recovery works today

Recovery is a startup-only step that runs before the API is treated as live.

### Recovery actions

On every daemon start, recovery currently:

1. scans active persisted agent executions and tries to clean up orphan processes
   - if a PID can be `SIGTERM`'d, or the process is already gone (`ESRCH`), the execution is marked `killed`
   - successful cleanup appends `agent.killed`
   - other signal errors leave the execution record `running`
2. releases expired locks and appends `looperd.recovery.lock_released`
3. marks any latest `running` run as `interrupted`, sets `endedAt`, and appends `looperd.recovery.run_interrupted`
4. requeues resumable loops by setting the loop back to `queued`, moving `nextRunAt` to the recovery timestamp, and rewriting `running` queue rows back to `queued`
5. normalizes stale queued loops that no longer have queued/running queue rows
6. appends `looperd.recovery.completed` with summary counts

### Recovery summary surface

The recovery summary is stored in runtime memory and exposed through `/api/v1/status`. It includes:

- `startedAt`
- `completedAt`
- `orphanAgentCleanup.attempted`
- `orphanAgentCleanup.cleanedCount`
- `expiredLocksReleased`
- `interruptedRunsMarked`
- `loopsRequeued`
- `eventsWritten`

Operationally, this means the first status response after a restart already contains the effects of recovery.

## 5. Loop stop vs daemon stop

`looper stop <id>` is not a daemon shutdown command. It is a best-effort request to stop one active loop via `POST /api/v1/runs/active/:id/stop`.

Current behavior:

- the loop is immediately moved to `paused`
- queued/running queue items for that loop are cancelled
- if there is an active agent PID, the runtime sends `SIGTERM`
- successful `SIGTERM` usually leaves the run `running` and the agent execution `cancelling`
- `ESRCH` is treated as already gone, which terminalizes the execution as `killed` and the run as `cancelled`
- other signal errors leave the active run/execution in place and return `stopped: false`

So a successful loop stop request does not guarantee that the run has fully ended yet; recovery on the next daemon start is part of the real stop story for interrupted work.

## 6. Operator implications to preserve in the Go port

- startup must remain fail-fast before the API is live
- recovery must still run before the first successful status response
- startup must still do one immediate scheduler tick instead of waiting for the first poll interval
- daemon shutdown should stay idempotent
- bounded shutdown waiting is part of today's behavior; the explicit timeout budget can change later, but it must be deliberate
- the current CLI only offers `daemon start` and `daemon restart`; if Go adds `daemon stop`, that would be a product-surface change, not an invisible implementation detail
- loop stop must remain distinct from daemon stop
