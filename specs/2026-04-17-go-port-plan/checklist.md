# Golang Port Checklist

## Phase 0 - Freeze the current contracts

- [x] Inventory all `looper` commands and subcommands
- [x] Inventory all daemon HTTP endpoints under `/api/v1/*`
- [x] Inventory config fields, env overrides, and CLI flag overrides
- [x] Freeze CLI flag names and semantics as part of the compatibility boundary
- [x] Freeze API paths, methods, status codes, headers, and auth behavior in machine-verifiable artifacts
- [x] Freeze request/response JSON shapes with fixtures, schema, or OpenAPI
- [x] Freeze API error codes and error-envelope behavior
- [x] Inventory the SQLite schema, migrations, and repository responsibilities
- [x] Capture a schema DDL snapshot and migration-sequence notes
- [x] Inventory all runtime tables, including notifications and worktrees
- [x] Inventory scheduler queue and event-log tables plus their recovery/retention semantics
- [x] Inventory external tool dependencies (`git`, `gh`, `osascript`, shell)
- [x] Inventory reviewer, fixer, planner, and worker state-machine behaviors
- [x] Inventory spec-PR label/path conventions and agent completion-marker behavior
- [x] Define parity expectations for daemon startup, shutdown, recovery, and run lifecycle
- [x] Capture daemon lifecycle notes for start, stop, recovery, and graceful shutdown

## Phase 1 - Establish Go project scaffolding

- [ ] Add `go.mod`
- [x] Use a single root Go module unless a blocker is found
- [ ] Add `cmd/looper`
- [ ] Add `cmd/looperd`
- [ ] Add initial `internal/` package layout
- [ ] Add shared version package
- [ ] Add `pkg/api` for shared API types and error codes
- [ ] Add Go build/test/lint commands to CI without removing current TS/Bun CI
- [x] Decide the CLI framework
- [x] Decide the CLI dependency-injection/testing pattern
- [x] Decide the SQLite driver and document why

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

- [ ] Implement the Phase 1 SQLite driver decision
- [ ] Reuse the current schema unless a blocker is found
- [ ] Decide and document the initial single-connection SQLite model
- [ ] Port embedded migrations
- [ ] Preserve migration naming/order and schema_migrations behavior
- [ ] Port DB open/close and transaction helpers
- [ ] Preserve backup / migration safety behavior
- [ ] Port repositories needed for projects, loops, runs, and runtime metadata
- [ ] Port scheduler queue persistence and recovery state
- [ ] Port event-log storage and retrieval behavior
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
- [ ] Port reviewer orchestration state machine
- [ ] Port fixer orchestration state machine
- [ ] Port planner orchestration state machine
- [ ] Port worker orchestration state machine
- [ ] Port graceful shutdown coordination for in-flight work
- [ ] Define an explicit shutdown timeout budget

## Phase 5 - Port infra adapters

- [ ] Port shell command execution
- [ ] Port git adapter behavior
- [ ] Port GitHub integration behavior
- [ ] Port worktree management
- [ ] Port notifications behavior
- [ ] Port spec-PR label/path behavior
- [x] Do a dedicated agent execution design spike
- [ ] Port agent execution lifecycle and heartbeat handling
- [ ] Port agent completion-marker behavior
- [ ] Preserve concurrent stdout/stderr capture, bounded buffers, inactivity timeout, and kill escalation

## Phase 6 - Port the HTTP API

- [ ] Port status endpoints
- [ ] Port config endpoints
- [ ] Port project endpoints
- [ ] Port loop endpoints
- [ ] Port run endpoints
- [ ] Port review/work endpoints
- [ ] Match the frozen `/api/v1/*` contract in the Go daemon
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

## Phase 8 - Validate parity

- [ ] Add config parity fixtures
- [ ] Add API response parity fixtures
- [x] Add API error-code and error-envelope fixtures
- [ ] Add CLI golden tests
- [ ] Re-validate SQLite migrations against TypeScript-created databases at the end of the rewrite
- [ ] Re-validate agent execution streaming, heartbeat, timeout, and kill escalation at the end of the rewrite
- [x] Re-validate Git/GitHub/spec-PR integration behavior at the end of the rewrite
- [ ] Run end-to-end local workflow validation on sample repos
- [ ] Run end-to-end validation for reviewer, fixer, planner, and worker flows that remain in scope
- [x] Validate Go install/upgrade flows end to end

## Preferred execution order during Phases 4-7

- [x] Port status/config foundations before deeper runtime automation
- [ ] Port project management before deeper loop/run automation
- [x] Delay process execution and agent orchestration until storage/contracts are stable

## Phase 9 - Move packaging and release to Go

- [ ] Add Go release build matrix for `looper`
- [ ] Add Go release build matrix for `looperd`
- [ ] Publish replacement artifacts
- [ ] Preserve drop-in artifact naming, install path, and executable naming where possible
- [ ] Update install and upgrade docs
- [ ] Validate release downloads and local install flows
- [ ] Preserve `looperd --version` output shape unless intentionally changed

## Phase 10 - Cut over

- [x] Complete a written cutover go/no-go checklist before replacing TypeScript binaries (`specs/2026-04-17-go-port-plan/reference/cutover-go-no-go-checklist.md`)
- [x] Make Go binaries the default supported implementation
- [ ] Switch CI and release pipelines to Go-first
- [ ] Remove Bun from the required production runtime path
- [x] Decide and document the fate of `apps/web`
- [ ] Retire or archive the TypeScript implementation after parity is proven
