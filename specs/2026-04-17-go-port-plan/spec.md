# Looper Golang Port Plan

## 1. Background

Looper is currently a Bun workspace with three apps:

- `apps/looperd` — the daemon, runtime, scheduler, worker orchestration, HTTP API server, and SQLite store
- `apps/cli` — the `looper` command-line client that talks to the daemon over HTTP
- `apps/web` — a placeholder only

The real product surface today is the daemon + CLI pair. The daemon owns most of the system complexity: config loading and validation, runtime bootstrapping, SQLite persistence, worktree management, GitHub/Git integration, agent execution, scheduling, logs, and notifications.

This document defines a full-project plan to port Looper from TypeScript/Bun to Go.

An additional practical reason for this port is that Bun compile is not currently a reliable deployment path for `looperd` on `darwin-arm64` with Bun `1.3.12`.

Observed behavior from local validation:

- Normal compile does not proceed:
  - command: `bun run --cwd apps/looperd compile:darwin-arm64`
  - result: blocked by the guard in `apps/looperd/scripts/compile.ts`
  - platform: `darwin-arm64`
  - Bun: `1.3.12`
- Forced compile can emit a binary, but the binary is unusable:
  - command: `LOOPER_FORCE_COMPILE=1 bun run --cwd apps/looperd compile:darwin-arm64`
  - emitted artifact: `apps/looperd/dist/compiled/looperd-darwin-arm64`
  - validation command: `looperd-darwin-arm64 --version`
  - observed result: hangs, is killed after about 8 seconds, and produces no stdout/stderr

So the practical conclusion matches the release validation result:

> On `darwin-arm64`, Bun `1.3.12` can be forced to emit a `looperd` binary, but the resulting binary is not usable.

---

## 2. Goals

Primary goals:

1. Rebuild the current daemon and CLI in Go without losing core behavior.
2. Preserve the existing product model: local daemon + local CLI + HTTP management API.
3. Keep the current user-visible workflows working during and after migration.
4. Reduce Bun-specific runtime coupling in favor of a single compiled Go codebase.
5. End with a simpler release model centered on native Go binaries.

Secondary goals:

1. Improve package boundaries while porting.
2. Make storage, process execution, and API contracts more explicit.
3. Keep a migration path that allows incremental validation instead of one big rewrite cutover.

Non-goals:

1. Building the web app during this effort.
2. Redesigning the product UX from scratch.
3. Introducing distributed or cloud-native architecture.
4. Adding major new features unrelated to parity.

---

## 3. Porting strategy

## 3.1 Recommended strategy

Use a **strangler migration** with explicit parity checkpoints, not a blind rewrite.

That means:

1. Freeze the current product behavior as the source of truth.
2. Define stable HTTP, config, and storage contracts.
3. Implement a new Go daemon and Go CLI alongside the current TypeScript apps.
4. Move subsystem-by-subsystem, validating parity after each phase.
5. Retire Bun/TypeScript only after the Go path is production-complete.

## 3.2 Why this is preferred over a hard cutover rewrite

The project contains several high-risk areas:

- SQLite persistence and migrations
- process management and long-running agent execution
- Git and GitHub integrations
- daemon/CLI compatibility
- installer and release behavior

A big-bang rewrite would combine architecture migration, behavior migration, and release migration into one failure domain. The better path is to separate those concerns and prove parity in layers.

---

## 4. Current system map to port

## 4.1 Apps

- `apps/looperd`
  - CLI entrypoint for daemon binary
  - bootstrap and config validation
  - runtime assembly
  - HTTP API server under `/api/v1/*`
  - SQLite-backed persistence
  - scheduler / loops / runs / worker flows
  - infra adapters for git, gh, shell, notifications, and agent vendors
- `apps/cli`
  - local CLI entrypoint
  - daemon install / upgrade / start / restart / status
  - API client and human-readable formatting
- `apps/web`
  - no meaningful migration work yet

## 4.2 Major daemon subsystems

From the current repo structure, the Go port must cover at least these domains:

1. **Bootstrap**
   - process args
   - environment loading
   - runtime path checks
   - logger setup
   - signal handling
2. **Configuration**
   - defaults
   - config file loading
   - env overrides
   - CLI flag overrides
   - validation and tool auto-detection
3. **HTTP server**
   - management endpoints
   - auth/token behavior if configured
   - JSON contracts used by the CLI
4. **Storage**
   - SQLite connection handling
   - migrations
   - repositories / queries
   - backups or retention behavior where applicable
5. **Runtime orchestration**
   - projects
   - loops
   - runs
   - agent execution lifecycle
   - scheduler and recovery behavior
6. **Infra adapters**
   - git
   - GitHub CLI / API interactions
   - worktree management
   - shell command execution
   - notifications (`osascript` today)
7. **CLI surface**
   - all existing user commands
   - daemon management commands
   - release/install helpers
   - text and JSON output modes

## 4.3 Bun-specific behavior to replace

The main Bun-native concerns are:

- `Bun.serve()`
- `bun:sqlite`
- `Bun.which()`
- Bun test/build/release flows
- Bun-specific CLI/runtime assumptions
- Bun compile reliability for shipping `looperd`

The Go port should replace these with standard Go libraries or well-contained dependencies.

---

## 5. Target Go architecture

## 5.1 Recommended repository layout

Recommended target shape:

```text
cmd/
  looper/
  looperd/
internal/
  app/
  bootstrap/
  config/
  api/
  runtime/
  storage/
  scheduler/
  projects/
  runs/
  loops/
  agent/
  infra/
    git/
    github/
    worktree/
    shell/
    notify/
pkg/
  api/
  version/
migrations/
test/
```

Recommendation:

> Use a single `go.mod` at the repo root unless a later constraint proves multi-module is necessary.

Rules:

1. Keep most implementation in `internal/`.
2. Expose only stable reusable contracts through `pkg/`.
3. Separate `cmd/looper` and `cmd/looperd` early.
4. Keep migrations as embedded files via Go `embed`.
5. Use `pkg/api` for shared request/response and error-code types consumed by both CLI and daemon.

## 5.2 Architecture boundaries

Recommended layering:

1. **Domain / application layer**
   - loop state transitions
   - run lifecycle
   - project registration
   - orchestration rules
2. **Ports / interfaces**
   - storage
   - git/github
   - notifications
   - agent execution
   - clock/process abstractions where tests need control
3. **Adapters**
   - SQLite repos
   - HTTP handlers
   - CLI commands
   - OS process execution

This is an opportunity to make the current implicit boundaries explicit rather than carrying file-for-file TypeScript structure into Go.

## 5.3 Recommended libraries

Prefer conservative choices:

- CLI: `cobra` or `urfave/cli/v3`
- HTTP router: standard library `net/http` plus a small router like `chi`, or pure stdlib if the surface stays small
- Config: stdlib + `encoding/json`; optionally `kong`/`viper` only if needed, but avoid over-abstracting precedence rules
- SQLite: `modernc.org/sqlite` for pure Go portability or `mattn/go-sqlite3` if CGO is acceptable
- Logging: `log/slog`
- Testing: stdlib `testing`, table-driven tests, golden tests for CLI output where useful

Recommendation:

> Prefer **stdlib-first Go**. Add third-party packages only when they materially reduce complexity.

Additional guidance:

1. Because Looper currently targets macOS first and uses SQLite features such as backup-oriented flows, `mattn/go-sqlite3` is an acceptable default choice if it produces more reliable behavior than `modernc.org/sqlite`.
2. If `log/slog` is used, log rotation still needs a separate solution.
3. The CLI framework choice must preserve testability comparable to the current injected-dependency model.

---

## 6. Key design decisions to settle before implementation

## 6.1 API compatibility

Decide whether the Go daemon will keep the exact `/api/v1/*` contracts.

Recommendation:

> Keep current HTTP contracts as-is until the Go CLI is complete.

That allows:

- old CLI ↔ new daemon smoke tests
- new CLI ↔ old daemon smoke tests where possible
- less migration risk

This compatibility boundary must include:

- route paths and path encoding behavior
- HTTP methods and status codes
- request/response JSON shapes
- error codes and error envelope structure
- auth behavior, including local token handling
- headers relied on by clients, such as request ID propagation

Phase 0 should freeze this contract in a machine-verifiable form, preferably one of:

1. OpenAPI plus examples
2. golden request/response fixtures
3. JSON schema plus endpoint fixtures

## 6.2 Config compatibility

Recommendation:

> Keep the same config file path, environment variable names, and precedence order.

Specifically preserve:

1. defaults
2. config file
3. env
4. CLI flags

This avoids forcing users to rewrite local setup during the port.

The compatibility boundary should also explicitly include CLI flag names and semantics, not just daemon config inputs.

## 6.3 Storage compatibility

This is the biggest architectural fork in the road.

Options:

1. **Reuse the existing SQLite schema**
2. Create a new schema and provide migration/import tooling

Recommendation:

> Reuse the current SQLite schema first, unless the existing schema is fundamentally broken.

Reason:

- much lower cutover risk
- easier smoke validation against real local state
- no separate data migration project in phase 1

Additional decision to settle:

> Prefer a single-connection SQLite model initially, matching current local-runtime behavior more closely than a pooled connection model.

This decision should be made explicitly before repository work begins.

## 6.4 External tool strategy

Current behavior depends on tools like `git`, `gh`, and `osascript`.

Recommendation:

1. Keep shelling out to `git` initially.
2. Keep shelling out to `gh` initially unless there is a clear reason to replace it with native API clients.
3. Preserve tool path auto-detection behavior in Go.
4. Preserve fail-fast startup checks when required binaries are missing.

This keeps behavior stable and avoids a second rewrite hidden inside the port.

## 6.5 Process model

Recommendation:

> Keep the local foreground daemon + detached start helper model first. Do not redesign supervision during the language port.

## 6.6 Agent execution model

The agent execution subsystem deserves its own migration decision because it is the heaviest Bun-runtime dependency in the project.

Recommendation:

> Treat agent execution as a dedicated design spike, not just another adapter port.

The Go design must preserve:

1. concurrent stdout/stderr capture
2. heartbeat tracking
3. bounded log buffering
4. timeout and inactivity-timeout behavior
5. SIGTERM → SIGKILL escalation behavior
6. final parse/completion marker handling

## 6.7 Scheduler model

Recommendation:

> Keep the existing scheduler semantics: a regular poll interval plus an immediate trigger path when new work is enqueued.

In Go, that likely means a `time.Ticker` plus a trigger channel, not a goroutine-per-item architecture.

## 6.8 Build and version metadata

Recommendation:

> Replace generated TypeScript version modules with Go build-time injection, most likely via `-ldflags`.

The Go binaries should preserve the current user-facing metadata behavior:

- `looperd --version`
- CLI version display
- daemon version in status responses
- build SHA / build timestamp where currently exposed

## 6.9 Logging and observability

Recommendation:

> Preserve current daemon log behavior intentionally, including file output, structured fields, and rotation expectations.

If exact log format compatibility is not required, the spec should still define what compatibility is required operationally.

## 6.10 Rollback posture

Recommendation:

> The TypeScript implementation remains the production path until Phase 10 cutover; any earlier phase must be safe to abandon without user-facing migration debt.

---

## 7. Proposed implementation phases

## Phase 0 - Discovery and contract freeze

Deliverables:

1. Full command inventory for `looper`
2. Full daemon endpoint inventory
3. Config field inventory and precedence matrix
4. SQLite schema inventory
5. External tool dependency inventory
6. Behavior notes for startup, shutdown, recovery, and long-running runs
7. machine-verifiable API contract artifacts
8. CLI flag inventory and compatibility matrix
9. error-code inventory and error-envelope contract
10. schema DDL snapshot and migration runner behavior notes

Acceptance criteria:

- We can describe current system behavior without reading TypeScript source ad hoc.
- The team agrees what “parity” means for each subsystem.
- The compatibility boundary is frozen in artifacts, not just prose.

Expected Phase 0 artifacts:

1. endpoint inventory plus request/response fixtures or OpenAPI
2. config field + env + CLI flag matrix
3. SQLite schema snapshot plus migration-sequence notes
4. error-code catalog
5. daemon lifecycle notes for start, stop, recovery, and shutdown

## Phase 1 - Establish the Go workspace

Deliverables:

1. Add Go module(s)
2. Add `cmd/looper` and `cmd/looperd`
3. Add shared version package
4. Add baseline build/test/lint scripts
5. Add CI jobs for Go lint/test/build without removing current TS CI
6. Decide CLI framework and testing/dependency-injection pattern
7. Decide SQLite driver and document why

Acceptance criteria:

- Go binaries compile in CI.
- No existing TypeScript behavior changes yet.
- The Go module structure and key foundation choices are explicit.

## Phase 2 - Port foundational shared modules

Port first:

1. version metadata
2. config model + loader + validator
3. runtime paths
4. tool detection
5. logging primitives
6. common API response types
7. logging rotation/file strategy

Acceptance criteria:

- Go can load the same config inputs and produce equivalent normalized config.
- Validation errors match current semantics closely enough for tests.
- Version/build metadata behavior is reproducible in Go.

## Phase 3 - Port storage layer

Deliverables:

1. SQLite connection management
2. embedded migrations
3. repository interfaces and implementations
4. transaction helpers
5. storage-focused test fixtures
6. backup / migration safety behavior

Acceptance criteria:

- Fresh DB initialization works.
- Existing schema is supported.
- Repository tests pass against real SQLite.
- The Go migration runner succeeds against databases created by the TypeScript runner across all existing schema versions.
- Transaction behavior is defined explicitly and matches the chosen single-connection model.

## Phase 4 - Port daemon runtime core

Deliverables:

1. bootstrap flow
2. application lifecycle
3. signal handling
4. scheduler and recovery loop
5. run/loop/project orchestration core
6. graceful shutdown coordination

Acceptance criteria:

- `looperd --version` works.
- daemon startup and clean shutdown work.
- runtime recovery behavior is validated by tests.
- graceful shutdown drains or finalizes in-flight work within an explicit timeout budget and persists final state safely.

## Phase 5 - Port infra adapters

Deliverables:

1. shell execution wrapper
2. git adapter
3. GitHub adapter
4. worktree adapter
5. notification adapter
6. agent execution adapter
7. daemon-process detection compatibility for mixed TS/Go environments

Acceptance criteria:

- Integration tests cover the critical happy paths and failure modes.
- Tool resolution and error reporting match current user expectations.
- Agent execution parity is validated for streaming output, heartbeat tracking, inactivity timeout, and kill escalation.

## Phase 6 - Port HTTP API

Deliverables:

1. route registration
2. request/response models
3. auth/token checks
4. status, project, loop, run, review, and work endpoints
5. error-code compatibility

Acceptance criteria:

- CLI-relevant endpoints are parity-tested.
- JSON contracts remain compatible under `/api/v1/*`.
- Error envelopes and codes match the frozen contract.

## Phase 7 - Port the CLI

Deliverables:

1. command tree and help text
2. daemon management commands
3. status/project/loop/pr/run commands
4. config display
5. JSON/text formatting
6. install and upgrade helpers
7. dual-daemon detection behavior during migration

Acceptance criteria:

- Existing documented CLI flows run against the Go daemon.
- JSON mode remains stable enough for scripted use.
- The Go CLI remains testable with injected dependencies or an equivalent isolation pattern.

## Phase 8 - Dual-run validation

Deliverables:

1. smoke tests for TS CLI ↔ Go daemon
2. smoke tests for Go CLI ↔ TS daemon where feasible
3. fixture-based parity tests for config and API responses
4. real local workflow validation on sample repos
5. mixed-install daemon detection tests

Acceptance criteria:

- The Go implementation is feature-complete for current supported workflows.
- Remaining gaps are explicitly listed and accepted.
- Dual-run validation uses separate DB paths/ports or strictly sequential execution; both daemons are never assumed to run against the same runtime state concurrently.

## Phase 9 - Packaging and release migration

Deliverables:

1. Go build matrix for `looper` and `looperd`
2. replacement release workflow
3. updated installation docs
4. migration guidance for existing users
5. drop-in artifact naming/install-path compatibility

Acceptance criteria:

- Release artifacts can be built and installed without Bun.
- Existing install/upgrade UX has a clear Go-native path.
- `looperd` remains a drop-in replacement at the filesystem and CLI contract level: same install path, same executable name, same `--version` shape unless explicitly changed.

## Phase 10 - Cutover and retirement

Deliverables:

1. default CI switched to Go paths
2. TypeScript apps moved to maintenance or removed
3. Bun runtime removed from required production path
4. docs rewritten around Go implementation

Acceptance criteria:

- The project is operationally Go-first.
- TypeScript removal does not break supported workflows.

---

## 8. Suggested subsystem port order

Recommended order:

1. config + metadata
2. storage + migrations
3. bootstrap + lifecycle
4. HTTP status/config endpoints
5. CLI status/config commands
6. project management
7. loops/runs core state handling
8. process execution and agent lifecycle
9. review/work automation paths
10. install/upgrade/release pipeline

This ordering should be treated as the preferred vertical-slice execution order for Phases 4-7, even if the phase descriptions remain grouped by layer.

Why this order:

- it establishes deterministic foundations first
- it gives early end-to-end vertical slices
- it postpones the most failure-prone orchestration logic until storage and contracts are stable

---

## 9. Testing strategy for the port

## 9.1 Parity tests

Add tests that compare Go behavior against frozen expectations from the current implementation for:

- config loading
- validation failures
- API response envelopes
- CLI JSON output
- migration results

## 9.2 Integration tests

Required integration coverage:

1. daemon startup with valid config
2. daemon startup failure with invalid config
3. SQLite initialization and migration
4. status/config/project/loop/run API calls
5. detached daemon management flows
6. git/worktree shell integration
7. graceful shutdown with in-flight work
8. agent execution streaming and timeout behavior

## 9.3 End-to-end smoke tests

At minimum keep automated smoke paths for:

1. install daemon
2. start daemon
3. `looper status`
4. `looper daemon status`
5. project registration
6. at least one run-producing workflow

## 9.4 Golden fixtures

Use golden files for:

- CLI help output
- JSON envelopes
- selected human-readable table output

This is especially useful because the current CLI has many commands and formatting regressions are easy to miss.

---

## 10. Risks and mitigations

## 10.1 Hidden behavior drift

Risk:

- The TypeScript code may contain behavior that is not documented but is relied on.

Mitigation:

- create contract tests before rewriting each subsystem
- preserve API/config/storage compatibility first

## 10.2 SQLite driver differences

Risk:

- locking, pragma, timestamp, or concurrency behavior may differ between Bun SQLite and Go SQLite drivers

Mitigation:

- evaluate driver behavior early in Phase 3
- use real-database integration tests rather than mocks only
- explicitly test backup and `VACUUM INTO` related behavior if retained

## 10.3 Process execution differences

Risk:

- signal handling, detached process startup, stdout/stderr capture, and timeout behavior may change subtly across platforms

Mitigation:

- isolate process management behind a small adapter
- add OS-level integration tests for the supported platforms
- test mixed stdout/stderr streaming and process-group termination behavior explicitly

## 10.4 Over-redesign during port

Risk:

- the team may try to redesign architecture, product behavior, and release workflows all at once

Mitigation:

- treat parity as the default
- require explicit approval for intentional behavior changes

## 10.5 Release disruption

Risk:

- switching language and build pipeline simultaneously can break distribution

Mitigation:

- keep TypeScript release path alive until Go artifacts are proven
- run parallel release dry-runs before cutover

## 10.6 Contract drift during a long migration

Risk:

- the TypeScript implementation may continue changing while the Go port is underway, making parity a moving target

Mitigation:

- define a contract-freeze checkpoint
- require new TS behavior changes to update the frozen fixtures/specs intentionally
- avoid implicit contract expansion during the port

## 10.7 Dual-runtime validation confusion

Risk:

- test runs may accidentally share the same port, PID files, or SQLite DB and produce misleading migration failures

Mitigation:

- use explicit isolated runtime paths during dual-run validation
- never assume concurrent mixed-daemon operation on a shared state directory

---

## 11. Definition of done

The project can be considered fully ported when all of the following are true:

1. `looper` and `looperd` are built from Go sources.
2. Supported CLI workflows operate without Bun in production use.
3. Config path, precedence, and major user-facing behavior remain compatible unless intentionally changed.
4. The daemon exposes the required management API and supports existing workflows.
5. SQLite persistence, migrations, and runtime recovery are validated in Go.
6. Release artifacts and install/upgrade flows are documented and working.
7. The old TypeScript implementation is no longer required for supported usage.

---

## 12. Immediate next steps

1. Inventory every CLI command and daemon endpoint into a porting matrix.
2. Freeze the current API and config contracts with tests/fixtures.
3. Decide the Go SQLite driver and CLI framework.
4. Create the Go module, baseline CI, and empty `cmd/looper` / `cmd/looperd` binaries.
5. Port config + storage first, then build upward into runtime and CLI.
