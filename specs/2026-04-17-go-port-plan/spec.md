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
3. Optimize for a fast, clean cutover instead of a long dual-runtime migration.

Non-goals:

1. Building the web app during this effort.
2. Redesigning the product UX from scratch.
3. Introducing distributed or cloud-native architecture.
4. Adding major new features unrelated to parity.

---

## 3. Porting strategy

### 3.1 Recommended strategy

Use a **big-bang migration** with a short contract-freeze period and a single implementation track.

That means:

1. Freeze the current product behavior as the source of truth.
2. Define stable HTTP, config, and storage contracts up front.
3. Build the new Go daemon and Go CLI as the replacement implementation, not as a long-lived parallel stack.
4. Validate parity aggressively during development, but cut over in one coordinated switch.
5. Remove Bun/TypeScript from the main path as soon as the Go implementation is ready.

### 3.2 Why big-bang is preferred here

This project has not gone to production yet, so the main optimization target is delivery speed and codebase cleanliness, not migration safety for live users.

That changes the tradeoff:

- a long strangler plan would force the team to maintain dual implementations, mixed-runtime compatibility rules, and extra migration scaffolding
- the extra scaffolding would slow the port and leave temporary architecture in the codebase
- a focused rewrite can keep the target architecture cleaner and reach a Go-first repository sooner

The high-risk areas still need deliberate validation:

- SQLite persistence and migrations
- process management and long-running agent execution
- Git and GitHub integrations
- daemon/CLI compatibility
- installer and release behavior

But those risks should be handled with contract fixtures, integration tests, and end-to-end validation during the rewrite, not by extending the life of the TypeScript implementation.

---

## 4. Current system map to port

### 4.1 Apps

- `apps/looperd`
  - CLI entrypoint for daemon binary
  - bootstrap and config validation
  - runtime assembly
  - HTTP API server under `/api/v1/*`
  - SQLite-backed persistence
  - scheduler / loops / runs / worker flows
  - reviewer / fixer / planner / worker orchestration state machines
  - infra adapters for git, gh, shell, notifications, spec PR handling, and agent vendors
- `apps/cli`
  - local CLI entrypoint
  - daemon install / upgrade / start / restart / status
  - API client and human-readable formatting
- `apps/web`
  - no meaningful migration work yet
  - should remain out of scope for the port and can stay as a placeholder until there is real product work for it

### 4.2 Major daemon subsystems

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
   - persistent scheduler queue state
   - event log storage and retrieval
   - backups or retention behavior where applicable
5. **Runtime orchestration**
   - projects
   - loops
   - runs
   - reviewer lifecycle
   - fixer lifecycle
   - planner lifecycle
   - worker lifecycle
   - agent execution lifecycle
   - scheduler and recovery behavior
   - event emission and persisted run history
   - domain rules for loop targets, locking, and uniqueness constraints
6. **Infra adapters**
   - git
   - GitHub CLI / API interactions
   - worktree management
   - shell command execution
   - notifications (`osascript` today)
   - spec PR state/label handling
   - agent prompt/completion marker handling
7. **CLI surface**
   - all existing user commands
   - daemon management commands
   - release/install helpers
   - text and JSON output modes

### 4.2.1 Reviewer, fixer, planner, and worker state-machine inventory

The four automation loops share the same top-level domain model in `apps/looperd/src/domain/index.ts`:

- loop types: `planner`, `reviewer`, `worker`, `fixer`
- loop statuses: `idle -> queued -> running -> paused|completed|failed|interrupted`
- run statuses: `queued`, `running`, `success`, `failed`, `cancelled`, `interrupted`, `parse_failed`
- resume policies persisted in checkpoints: `replay_step`, `advance_from_checkpoint`, `manual_intervention`
- audit events emitted around execution: `loop.started`, `loop.step.started`, `loop.step.completed`, `loop.step.failed`, `run.started`, `run.completed`, `run.failed`

Scheduler/runtime coordination lives in `apps/looperd/src/scheduler/index.ts` and `apps/looperd/src/runtime/index.ts`:

- scheduler queue priority order is `planner` -> `reviewer` -> `fixer` -> `worker`
- queue dedupe is keyed per discovered target/work unit before enqueue
- retryable failures are re-queued with exponential backoff; terminal failures remain failed
- runtime ticks run discovery first, then claim queue items and dispatch by loop type
- each runner updates loop status to `running` on start, persists per-step checkpoints, and sets the loop to `queued`, `paused`, `completed`, or `failed` when the run ends

#### Planner (`apps/looperd/src/planner/index.ts`)

- discovery source: assigned open issues labeled `looper:plan`
- target type: `issue`
- discovery skips ambiguous repo-to-project mappings, missing current GH login, and loops already `paused` or `completed`
- step sequence: `discover-issues` -> `prepare-worktree` -> `write-spec` -> `publish` -> `notify`
- `discover-issues` claims an issue-scoped business lock and snapshots issue metadata into the checkpoint
- `write-spec` is the agent-heavy step; later steps reuse the persisted checkpoint rather than rebuilding context
- `publish` pushes the spec branch, opens the PR, adds `looper:spec-reviewing`, and requests reviewers
- failures marked `retryable_after_resume` resume from the next step using the checkpoint; `manual_intervention` pauses the loop

#### Reviewer (`apps/looperd/src/reviewer/index.ts`)

- discovery source: open PRs where the current user is requested for review, plus open PRs labeled `looper:spec-reviewing`, plus existing follow-up loops
- target type: `pull_request`
- discovery skips drafts, non-open PRs, and PR heads already matching `lastPublishedHeadSha`
- step sequence: `discover` -> `filter` -> `claim` -> `snapshot` -> `review` -> `publish`
- `claim` acquires the PR-scoped business lock; `snapshot` and later steps reuse persisted review context
- a successful publish records the reviewed head SHA so unchanged heads are skipped on later discovery passes
- retryable failures requeue the loop; `manual_intervention` pauses it; successful or skipped runs mark the loop `completed`

#### Fixer (`apps/looperd/src/fixer/index.ts`)

- discovery source: open PRs whose review state yields actionable fix items from `collectFixItems(detail)`
- target type: `pull_request`
- discovery skips drafts, non-open PRs, PRs already under an active PR lock, and PRs with no actionable fix items
- step sequence: `discover-pr` -> `claim-pr` -> `collect-fixes` -> `prepare-worktree` -> `repair` -> `validate` -> `push` -> `reconcile-commits` -> `resolve-comments` -> `recheck`
- `claim-pr` acquires the PR-scoped business lock; the rest of the run advances from the persisted checkpoint
- the loop is explicitly resume-oriented: push/open review follow-up failures can come back as `advance_from_checkpoint` instead of replaying the full repair flow
- `manual_intervention` pauses the loop; terminal states also clean up fixer worktrees

#### Worker (`apps/looperd/src/worker/index.ts`)

- discovery source: open PRs labeled `looper:spec-ready`
- target type today includes both `project` and `pull_request`, but the spec-PR flow inventories the PR-targeted path here
- discovery skips drafts, non-open PRs, and already-paused PR-targeted worker loops
- step sequence: `prepare-work` -> `prepare-worktree` -> `plan` -> `execute` -> `validate` -> `open-pr`
- `prepare-work` branches early by execution mode: create a new PR for project-targeted work, or continue on an existing PR for `pull_request` targets
- PR-targeted worker runs fetch PR details, require a spec path, remove the `looper:spec-ready` label, and acquire a PR-scoped business lock before execution continues
- `execute` is the agent-heavy step; `validate` runs configured commands; `open-pr` either pushes the existing PR branch or creates a new PR depending on execution mode
- validation failures and missing spec-path cases pause the loop via `manual_intervention`; retryable push/agent failures resume from checkpoint

The most useful behavior locks for parity are the tests in:

- `apps/looperd/src/domain/index.test.ts`
- `apps/looperd/src/planner/index.test.ts`
- `apps/looperd/src/reviewer/index.test.ts`
- `apps/looperd/src/fixer/index.test.ts`
- `apps/looperd/src/worker/index.test.ts`

### 4.3 Bun-specific behavior to replace

The main Bun-native concerns are:

- `Bun.serve()`
- `bun:sqlite`
- `Bun.which()`
- `Bun.$` shell execution
- Bun subprocess APIs used for agent/process execution
- Bun test/build/release flows
- Bun-specific CLI/runtime assumptions
- Bun compile reliability for shipping `looperd`

The Go port should replace these with standard Go libraries or well-contained dependencies.

---

## 5. Target Go architecture

### 5.1 Recommended repository layout

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
  domain/
  runtime/
  storage/
  scheduler/
  projects/
  runs/
  loops/
  reviewer/
  fixer/
  planner/
  worker/
  eventlog/
  agent/
  infra/
    git/
    github/
    worktree/
    shell/
    notify/
    specpr/
pkg/
  api/
  version/
migrations/
test/
```

Recommendation:

> Use a single `go.mod` at the repo root unless a later constraint proves multi-module is necessary.

Status: confirmed for the current port scaffold. The repository now uses the root `go.mod`, and no nested `go.mod` or `go.work` files are present.

Rules:

1. Keep most implementation in `internal/`.
2. Expose only stable reusable contracts through `pkg/`.
3. Separate `cmd/looper` and `cmd/looperd` early.
4. Keep migrations as embedded files via Go `embed`.
5. Use `pkg/api` for shared request/response and error-code types consumed by both CLI and daemon.

### 5.2 Architecture boundaries

Recommended layering:

1. **Domain / application layer**
    - loop state transitions
    - run lifecycle
    - project registration
    - orchestration rules
    - reviewer/fixer/planner/worker state transitions
    - lock-key, target, and uniqueness invariants
2. **Ports / interfaces**
    - storage
    - git/github
    - notifications
    - agent execution
    - event logging
    - clock/process abstractions where tests need control
3. **Adapters**
    - SQLite repos
    - HTTP handlers
    - CLI commands
    - OS process execution
    - PR label/spec-path helpers and installer/release helpers

This is an opportunity to make the current implicit boundaries explicit rather than carrying file-for-file TypeScript structure into Go.

### 5.3 Recommended libraries

Prefer conservative choices:

- CLI: `github.com/spf13/cobra`
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

SQLite driver decision:

> Use `github.com/mattn/go-sqlite3` for the Phase 1 SQLite port.
>
> The immediate goal is behavior parity with the current Bun-backed daemon, not maximum portability. `mattn/go-sqlite3` is the lower-risk choice for that phase because it is the most established Go SQLite driver, integrates directly with `database/sql`, and maps cleanly onto the SQLite behaviors Looper already depends on: WAL mode, per-connection foreign-key pragmas, transactional migrations, `busy_timeout`, and backup-oriented flows. The port already targets macOS first, so accepting CGO in Phase 1 is an acceptable trade-off in exchange for using the native SQLite engine rather than a translated pure-Go port while we validate schema, migration, and recovery parity against databases produced by the TypeScript implementation.
>
> `modernc.org/sqlite` remains a viable later option if distribution simplicity becomes a higher priority than immediate parity, but choosing it now would introduce an extra compatibility variable during the riskiest storage phase of the rewrite.

CLI framework decision:

> Use `github.com/spf13/cobra` for the Go `looper` CLI.
>
> The frozen command tree under `specs/2026-04-17-go-port-plan/reference/cli-commands.md` maps directly to Cobra's nested `Command` model, and the frozen global/config-forwarded flag contract maps cleanly to root persistent flags plus per-command local flags. Cobra also gives enough control over command help to reproduce Looper's existing group-level `Subcommands:` sections without keeping a fully custom parser/dispatcher, while still allowing an injected-dependency `App` or `CLI` struct to build a fresh root command per test. We are explicitly not using Cobra's package-global patterns; command construction should stay instance-based so the next testing/DI task can preserve the current `runCli(argv, deps)` isolation model.

---

## 6. Key design decisions to settle before implementation

### 6.1 API compatibility

Decide whether the Go daemon will keep the exact `/api/v1/*` contracts.

Recommendation:

> Keep current HTTP contracts as-is until the Go CLI is complete.

That allows:

- the Go daemon and Go CLI to converge on one stable contract
- fixture-based validation against the TypeScript reference behavior
- less churn while the rewrite is in flight

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

### 6.2 Config compatibility

Recommendation:

> Keep the same config file path, environment variable names, and precedence order.

Specifically preserve:

1. defaults
2. config file
3. env
4. CLI flags

This avoids forcing users to rewrite local setup during the port.

The compatibility boundary should also explicitly include CLI flag names and semantics, not just daemon config inputs.

### 6.3 Storage compatibility

This is the biggest architectural fork in the road.

Options:

1. **Reuse the existing SQLite schema**
2. Create a new schema and provide migration/import tooling

Recommendation:

> Reuse the current SQLite schema first, unless the existing schema is fundamentally broken.

Decision recorded in `specs/2026-04-17-go-port-plan/reference/sqlite-schema-reuse-decision.md`:

- no blocker has been found in the current schema or migration lineage
- the Go port should treat the TypeScript migration history through `0007_agent_execution_run_index` as the storage compatibility boundary

Reason:

- faster cutover with less temporary migration code
- easier validation against real local state
- no separate data migration project in phase 1

Additional decision to settle:

> Prefer a single-connection SQLite model initially, matching current local-runtime behavior more closely than a pooled connection model.

This decision should be made explicitly before repository work begins.

The storage compatibility boundary should explicitly include:

- scheduler queue persistence and restart recovery semantics
- event-log schema and retention behavior where applicable
- migration compatibility with real databases created by the TypeScript implementation

### 6.4 External tool strategy

Current behavior depends on tools like `git`, `gh`, and `osascript`.

Recommendation:

1. Keep shelling out to `git` initially.
2. Keep shelling out to `gh` initially unless there is a clear reason to replace it with native API clients.
3. Preserve tool path auto-detection behavior in Go.
4. Preserve fail-fast startup checks when required binaries are missing.

This keeps behavior stable and avoids a second rewrite hidden inside the port.

### 6.5 Process model

Recommendation:

> Keep the local foreground daemon + detached start helper model first. Do not redesign supervision during the language port.

### 6.6 Agent execution model

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

### 6.7 Scheduler model

Recommendation:

> Keep the existing scheduler semantics: a regular poll interval plus an immediate trigger path when new work is enqueued.

In Go, that likely means a `time.Ticker` plus a trigger channel, not a goroutine-per-item architecture.

That model must also preserve persisted queue semantics so restart/recovery behavior matches the current daemon closely enough for parity.

### 6.8 Build and version metadata

Recommendation:

> Replace generated TypeScript version modules with Go build-time injection, most likely via `-ldflags`.

The Go binaries should preserve the current user-facing metadata behavior:

- `looperd --version`
- CLI version display
- daemon version in status responses
- build SHA / build timestamp where currently exposed

### 6.9 Logging and observability

Recommendation:

> Preserve current daemon log behavior intentionally, including file output, structured fields, and rotation expectations.

If exact log format compatibility is not required, the spec should still define what compatibility is required operationally.

This should also cover event-log observability expectations, not just daemon log files.

### 6.10 Rollback posture

Recommendation:

> The TypeScript implementation is the reference path only until the Go rewrite is complete; once the Go implementation is feature-complete and validated, cut over directly and archive the TypeScript path.

### 6.11 CLI framework

Decision:

> Use `github.com/spf13/cobra` for the Go CLI.

Why:

1. Cobra's command/subcommand tree matches the frozen `looper` surface (`project`, `daemon`, `loop`, `pr`, `run`, and one-off commands such as `review`, `jump`, `logs`, and `stop`) with minimal translation logic.
2. Root persistent flags provide a clean home for the frozen global flags and the `extractConfigArgs()` forwarding boundary, while still allowing command-local flags to stay local.
3. Cobra's help hooks (`SetHelpFunc`, templates, per-command usage control) are flexible enough to preserve the current help-output shape, including explicit group-level `Subcommands:` sections covered by the existing CLI tests.
4. Testability remains compatible with the current injected-dependency design if the implementation constructs a fresh root command from an `App`/`CLI` struct per invocation rather than closing over package globals.

Rejected alternative:

`urfave/cli/v3` remains viable, but it is a worse fit for this parity-first port because its context-driven handler style and coarser help templating add friction around custom grouped help output, exact flag-forwarding behavior, and dependency-injected tests.

### 6.12 CLI dependency-injection and testing pattern

Decision:

> Use an instance-based Cobra app built from a single injected `Deps` struct, and execute it through an importable `App.Run(ctx, argv) int` entrypoint.

Required constraints:

1. `cmd/looper/main.go` remains a thin adapter and is the only place that should call `os.Exit`.
2. The Go CLI must construct a fresh root command tree for every invocation rather than using Cobra package globals or `init()` registration.
3. Command handlers should receive a per-invocation context containing parsed arguments, writers, loaded config, and injected dependencies.
4. Side-effecting operations (config loading, API client creation, file I/O, process execution, shell launch, environment access, time/sleep hooks, daemon install/upgrade helpers, and version/build metadata access) must be routed through the injected dependency surface instead of direct package calls from handler code.
5. Keep the dependency surface as one explicit struct first; introduce named interfaces only where multiple real implementations or complex fakes justify them.

Testing consequences:

1. The primary test seam is `App.Run(context.Background(), argv)` with fake dependencies and buffer-backed stdout/stderr.
2. Help and usage tests should run in-process by capturing Cobra output rather than spawning subprocesses.
3. CLI golden tests should verify help output, JSON output, and selected human-readable formatting against the frozen contract artifacts.
4. End-to-end binary tests remain valuable, but they are secondary validation on top of the injected-dependency test suite.

See `specs/2026-04-17-go-port-plan/reference/cli-di-testing-pattern.md` for the concrete pattern and rejected alternatives.

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
7. reviewer/fixer/planner/worker orchestration state machines
8. event emission and persisted queue/recovery behavior

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
6. spec PR adapter behavior
7. agent execution adapter

Acceptance criteria:

- Integration tests cover the critical happy paths and failure modes.
- Tool resolution and error reporting match current user expectations.
- Agent execution parity is validated for streaming output, heartbeat tracking, inactivity timeout, and kill escalation.
- Git/GitHub/spec-PR flows are validated against the current contract-sensitive behaviors.

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

Acceptance criteria:

- Existing documented CLI flows run against the Go daemon.
- JSON mode remains stable enough for scripted use.
- The Go CLI remains testable with injected dependencies or an equivalent isolation pattern.

## Phase 8 - Big-bang validation

Deliverables:

1. fixture-based parity tests for config and API responses
2. CLI golden tests and help-output checks
3. real local workflow validation on sample repos
4. install/upgrade validation for the Go binaries
5. final SQLite migration validation against TypeScript-created databases
6. final agent execution validation for streaming, heartbeat, timeout, and kill-escalation behavior

Acceptance criteria:

- The Go implementation is feature-complete for current supported workflows.
- Remaining gaps are explicitly listed and accepted.
- The Go binaries can replace the TypeScript binaries without requiring a mixed-runtime operating mode.
- SQLite, process execution, Git/GitHub integration, daemon/CLI compatibility, and install/release behavior have all been re-validated at the end of the rewrite, not only in earlier phases.

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

1. written cutover go/no-go checklist with explicit decision gates and rollback requirements
2. default CI switched to Go paths
3. TypeScript apps moved to maintenance or removed
4. Bun runtime removed from required production path
5. docs rewritten around Go implementation

Decision artifact:

- `specs/2026-04-17-go-port-plan/reference/cutover-go-no-go-checklist.md`

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

This ordering should be treated as the preferred execution order within the phase plan. When the suggested order and the phase grouping appear to conflict, follow the subsystem order for implementation sequencing and use the phases as milestone buckets.

Why this order:

- it establishes deterministic foundations first
- it keeps the rewrite on a clean dependency order
- it postpones the most failure-prone orchestration logic until storage and contracts are stable

---

## 9. Testing strategy for the port

### 9.1 Parity tests

Add tests that compare Go behavior against frozen expectations from the current implementation for:

- config loading
- validation failures
- API response envelopes
- CLI JSON output
- migration results
- scheduler queue persistence and recovery
- event-log records for key orchestration flows

### 9.2 Integration tests

Required integration coverage:

1. daemon startup with valid config
2. daemon startup failure with invalid config
3. SQLite initialization and migration
4. status/config/project/loop/run API calls
5. detached daemon management flows
6. git/worktree shell integration
7. graceful shutdown with in-flight work
8. agent execution streaming and timeout behavior
9. reviewer/fixer/planner/worker orchestration happy paths
10. persisted queue recovery and event-log emission

### 9.3 End-to-end smoke tests

At minimum keep automated smoke paths for:

1. install daemon
2. start daemon
3. `looper status`
4. `looper daemon status`
5. project registration
6. at least one run-producing workflow
7. one end-to-end workflow each for reviewer, fixer, planner, and worker if those workflows remain supported at cutover

### 9.4 Golden fixtures

Use golden files for:

- CLI help output
- JSON envelopes
- selected human-readable table output

This is especially useful because the current CLI has many commands and formatting regressions are easy to miss.

---

## 10. Risks and mitigations

### 10.1 Hidden behavior drift

Risk:

- The TypeScript code may contain behavior that is not documented but is relied on.

Mitigation:

- create contract tests before rewriting each subsystem
- preserve API/config/storage compatibility first

### 10.2 SQLite driver differences

Risk:

- locking, pragma, timestamp, or concurrency behavior may differ between Bun SQLite and Go SQLite drivers

Mitigation:

- evaluate driver behavior early in Phase 3
- use real-database integration tests rather than mocks only
- explicitly test backup and `VACUUM INTO` related behavior if retained

### 10.3 Process execution differences

Risk:

- signal handling, detached process startup, stdout/stderr capture, and timeout behavior may change subtly across platforms

Mitigation:

- isolate process management behind a small adapter
- add OS-level integration tests for the supported platforms
- test mixed stdout/stderr streaming and process-group termination behavior explicitly

### 10.4 Over-redesign during port

Risk:

- the team may try to redesign architecture, product behavior, and release workflows all at once

Mitigation:

- treat parity as the default
- require explicit approval for intentional behavior changes

### 10.5 Release disruption

Risk:

- switching language and build pipeline simultaneously can break distribution

Mitigation:

- keep TypeScript release behavior available as a reference until Go artifacts are proven
- run release dry-runs for the Go artifacts before cutover

### 10.6 Contract drift during a long migration

Risk:

- the TypeScript implementation may continue changing while the Go port is underway, making parity a moving target

Mitigation:

- define a contract-freeze checkpoint
- require new TS behavior changes to update the frozen fixtures/specs intentionally
- avoid implicit contract expansion during the port

### 10.7 Big-bang cutover misses hidden behavior

Risk:

- the team may miss a low-visibility workflow because there is no prolonged mixed-runtime migration period

Mitigation:

- freeze contracts before implementation starts
- use fixture-based parity tests plus end-to-end sample-repo validation
- require a written cutover checklist before replacing the TypeScript binaries

Minimum cutover checklist:

1. all phase acceptance criteria are either satisfied or explicitly waived
2. Go release artifacts build successfully on target platforms
3. install/upgrade flows are validated end to end
4. config, API, CLI golden, SQLite migration, and agent execution parity suites are green
5. end-to-end sample-repo workflows are green for all supported automation paths
6. cutover docs specify the exact fate of the TypeScript apps, including `apps/web`

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
8. The reviewer, fixer, planner, and worker automation paths are either ported and validated or explicitly removed from scope with documentation.

---

## 12. Immediate next steps

1. Inventory every CLI command and daemon endpoint into a porting matrix.
2. Freeze the current API and config contracts with tests/fixtures.
3. Decide the Go SQLite driver and CLI framework.
4. Create the Go module, baseline CI, and empty `cmd/looper` / `cmd/looperd` binaries.
5. Port config + storage first, then build upward into runtime and CLI.
