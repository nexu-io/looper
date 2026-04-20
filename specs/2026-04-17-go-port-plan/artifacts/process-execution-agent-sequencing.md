# Process execution and agent orchestration sequencing checkpoint

Date: 2026-04-21

This checkpoint closes the preferred execution-order item:

- `Delay process execution and agent orchestration until storage/contracts are stable`

The goal of this checkpoint is not to introduce new runtime behavior. It is to prove that the Go port followed the planned dependency order: first stabilize the durable storage layer and machine-verifiable contracts, then layer shell execution, agent execution, and higher-level orchestration on top.

## Stable foundations established first

### Durable domain and storage contracts

- `internal/domain/domain.go` freezes the Go loop/run type system, statuses, transitions, target rules, and per-loop step sets.
- `internal/storage/db.go` and `internal/storage/migrate.go` open SQLite with the required pragmas, run embedded migrations, and preserve the backup-before-migrate flow.
- `internal/storage/repositories.go` defines the repository surface that later orchestration uses for projects, loops, runs, queue items, event log entries, agent executions, notifications, and worktrees.

### Frozen daemon API compatibility

- `specs/2026-04-17-go-port-plan/artifacts/daemon-http.compat.json` freezes the `/api/v1/*` HTTP contract.
- `specs/2026-04-17-go-port-plan/artifacts/daemon-http.requests.compat.json` freezes request body shapes.
- `specs/2026-04-17-go-port-plan/artifacts/daemon-http.responses.compat.json` freezes success envelopes and response payloads.
- `specs/2026-04-17-go-port-plan/artifacts/daemon-http.errors.compat.json` freezes error envelopes and error codes.
- `internal/api/handler_test.go` machine-verifies the Go handler against the frozen compatibility boundary.

## Execution and orchestration layered on top afterward

### Runtime assembly depends on the stable storage/contracts layer

- `internal/runtime/runtime.go` opens the SQLite coordinator, runs pending migrations, constructs repositories, and syncs configured projects before starting recovery and the scheduler loop.
- The same runtime assembly then wires the services and recovery pipeline that later queue and dispatch loop work.

### Process execution appears only after those foundations exist

- `internal/infra/shell/runner.go` provides bounded shell execution with stdout/stderr capture, timeout handling, and kill escalation.
- `internal/agent/executor.go` launches agent commands, persists execution records, streams output, tracks heartbeats, and records completion details through the repository layer.

### Higher-level automation depends on the persisted state model

- `internal/planner/runner.go`
- `internal/reviewer/runner.go`
- `internal/fixer/runner.go`
- `internal/worker/runner.go`

These automation lanes all depend on the domain rules, repository contracts, queue state, event log state, and persisted agent execution records established earlier in the port.

## Validation evidence

- `internal/storage/db_test.go`, `internal/storage/migrate_test.go`, and `internal/storage/repositories_test.go` cover the storage bootstrap, migration, and repository contracts.
- `internal/api/handler_test.go` verifies the frozen `/api/v1/*` contract.
- `internal/infra/shell/runner_test.go` verifies shell execution capture, timeout handling, bounded output, and cancellation.
- `internal/agent/executor_test.go` verifies agent execution lifecycle persistence, output parsing, completion markers, timeout handling, and cancellation.
- `internal/planner/runner_test.go`, `internal/reviewer/runner_test.go`, `internal/fixer/runner_test.go`, and `internal/worker/runner_test.go` verify that orchestration resumes and persists state through the repository-backed model.

Recommended verification commands for this checkpoint:

```sh
go test ./internal/storage ./internal/api ./internal/runtime ./internal/infra/shell ./internal/agent ./internal/planner ./internal/reviewer ./internal/fixer ./internal/worker
```

## Sequencing conclusion

This preferred-order checkpoint is satisfied.

The Go rewrite now has documented evidence that process execution and agent orchestration were implemented only after the storage layer and compatibility contracts were stabilized, matching the sequencing required by the port plan.
