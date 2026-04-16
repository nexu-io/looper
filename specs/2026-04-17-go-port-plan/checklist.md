# Golang Port Checklist

## Phase 0 - Freeze the current contracts

- [ ] Inventory all `looper` commands and subcommands
- [ ] Inventory all daemon HTTP endpoints under `/api/v1/*`
- [ ] Inventory config fields, env overrides, and CLI flag overrides
- [ ] Freeze CLI flag names and semantics as part of the compatibility boundary
- [ ] Freeze API paths, methods, status codes, headers, and auth behavior in machine-verifiable artifacts
- [ ] Freeze request/response JSON shapes with fixtures, schema, or OpenAPI
- [ ] Freeze API error codes and error-envelope behavior
- [ ] Inventory the SQLite schema, migrations, and repository responsibilities
- [ ] Capture a schema DDL snapshot and migration-sequence notes
- [ ] Inventory all runtime tables, including notifications and worktrees
- [ ] Inventory external tool dependencies (`git`, `gh`, `osascript`, shell)
- [ ] Define parity expectations for daemon startup, shutdown, recovery, and run lifecycle
- [ ] Capture daemon lifecycle notes for start, stop, recovery, and graceful shutdown

## Phase 1 - Establish Go project scaffolding

- [ ] Add `go.mod`
- [ ] Use a single root Go module unless a blocker is found
- [ ] Add `cmd/looper`
- [ ] Add `cmd/looperd`
- [ ] Add initial `internal/` package layout
- [ ] Add shared version package
- [ ] Add `pkg/api` for shared API types and error codes
- [ ] Add Go build/test/lint commands to CI without removing current TS/Bun CI
- [ ] Decide the CLI framework
- [ ] Decide the CLI dependency-injection/testing pattern
- [ ] Decide the SQLite driver and document why

## Phase 2 - Port shared foundations

- [ ] Port version metadata
- [ ] Port build metadata injection via Go build flags
- [ ] Port config defaults and normalization
- [ ] Port config file loading
- [ ] Port env and CLI override precedence
- [ ] Port config validation
- [ ] Port runtime path resolution and required directory checks
- [ ] Port tool path auto-detection
- [ ] Port logging setup
- [ ] Decide and implement log file rotation strategy
- [ ] Port shared API response and error types

## Phase 3 - Port storage

- [ ] Choose the Go SQLite driver
- [ ] Reuse the current schema unless a blocker is found
- [ ] Decide and document the initial single-connection SQLite model
- [ ] Port embedded migrations
- [ ] Preserve migration naming/order and schema_migrations behavior
- [ ] Port DB open/close and transaction helpers
- [ ] Preserve backup / migration safety behavior
- [ ] Port repositories needed for projects, loops, runs, and runtime metadata
- [ ] Add real SQLite integration tests
- [ ] Validate the Go migration runner against databases created by the TS runner across all existing schema versions
- [ ] Test backup and `VACUUM INTO` behavior if retained

## Phase 4 - Port daemon lifecycle and runtime core

- [ ] Port `looperd --version`
- [ ] Port bootstrap flow
- [ ] Port signal handling
- [ ] Port runtime assembly
- [ ] Port scheduler/recovery startup behavior
- [ ] Implement scheduler immediate-trigger behavior alongside polling
- [ ] Port core loop/run/project orchestration
- [ ] Port graceful shutdown coordination for in-flight work
- [ ] Define an explicit shutdown timeout budget

## Phase 5 - Port infra adapters

- [ ] Port shell command execution
- [ ] Port git adapter behavior
- [ ] Port GitHub integration behavior
- [ ] Port worktree management
- [ ] Port notifications behavior
- [ ] Do a dedicated agent execution design spike
- [ ] Port agent execution lifecycle and heartbeat handling
- [ ] Preserve concurrent stdout/stderr capture, bounded buffers, inactivity timeout, and kill escalation
- [ ] Port daemon-process detection behavior for mixed TS/Go environments

## Phase 6 - Port the HTTP API

- [ ] Port status endpoints
- [ ] Port config endpoints
- [ ] Port project endpoints
- [ ] Port loop endpoints
- [ ] Port run endpoints
- [ ] Port review/work endpoints
- [ ] Preserve `/api/v1/*` compatibility during migration
- [ ] Preserve error-envelope and error-code compatibility

## Phase 7 - Port the CLI

- [ ] Port command parsing and help output
- [ ] Port daemon API client
- [ ] Port JSON output mode
- [ ] Port human-readable formatting
- [ ] Port daemon install logic
- [ ] Port daemon start/restart/status/logs flows
- [ ] Port upgrade flows
- [ ] Preserve CLI testability with injected dependencies or equivalent isolation
- [ ] Preserve dual-daemon detection behavior during migration

## Phase 8 - Validate parity

- [ ] Add config parity fixtures
- [ ] Add API response parity fixtures
- [ ] Add API error-code and error-envelope fixtures
- [ ] Add CLI golden tests
- [ ] Smoke test TS CLI against the Go daemon
- [ ] Smoke test Go CLI against the TS daemon where feasible
- [ ] Test mixed-install daemon detection
- [ ] Use isolated DB paths/ports or strictly sequential runs during dual-run validation
- [ ] Run end-to-end local workflow validation on sample repos

## Preferred vertical slices during Phases 4-7

- [ ] Deliver status/config endpoint + CLI status/config command slices early
- [ ] Deliver project-management slice before deeper loop/run automation
- [ ] Delay process execution and agent orchestration until storage/contracts are stable

## Phase 9 - Move packaging and release to Go

- [ ] Add Go release build matrix for `looper`
- [ ] Add Go release build matrix for `looperd`
- [ ] Publish replacement artifacts
- [ ] Preserve drop-in artifact naming, install path, and executable naming where possible
- [ ] Update install and upgrade docs
- [ ] Validate release downloads and local install flows
- [ ] Preserve `looperd --version` output shape unless intentionally changed

## Phase 10 - Cut over

- [ ] Make Go binaries the default supported implementation
- [ ] Switch CI and release pipelines to Go-first
- [ ] Remove Bun from the required production runtime path
- [ ] Retire or archive the TypeScript implementation after parity is proven
- [ ] Keep the TS implementation as the production fallback until cutover is complete
