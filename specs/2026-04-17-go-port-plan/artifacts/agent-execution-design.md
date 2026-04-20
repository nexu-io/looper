# Agent execution subsystem design

Source of truth inspected from:

- `apps/looperd/src/infra/agent.ts`
- `apps/looperd/src/infra/agent-prompt.ts`
- `apps/looperd/src/infra/agent.test.ts`
- `apps/looperd/src/runtime/index.ts`
- `apps/looperd/src/server/index.ts`
- `internal/infra/shell/runner.go`
- `internal/planner/runner.go`
- `internal/reviewer/runner.go`
- `internal/fixer/runner.go`
- `internal/worker/runner.go`
- `specs/2026-04-17-go-port-plan/artifacts/spec-pr-and-agent-completion.md`
- `specs/2026-04-17-go-port-plan/artifacts/daemon-lifecycle-parity.md`
- `specs/2026-04-17-go-port-plan/artifacts/sqlite-inventory.md`

This document is the dedicated design spike for the Go agent-execution subsystem. It defines the package shape, lifecycle contract, parity requirements, intentional deviations, and sequencing for the next implementation tasks.

## 1. Scope

The Go agent-execution subsystem must replace the current TypeScript/Bun implementation that:

- resolves vendor-specific agent commands and args
- injects prompt/completion-marker environment variables
- starts long-running subprocesses
- captures stdout and stderr concurrently
- tracks heartbeats and inactivity
- persists `agent_executions` state during and after execution
- parses the final completion marker from combined output
- supports best-effort stop and startup orphan cleanup

This subsystem is shared by planner, reviewer, fixer, and worker runs, and its persisted/runtime behavior is already part of the compatibility boundary.

## 2. Compatibility boundary to preserve

### 2.1 Completion-marker contract

- Canonical marker token stays `__LOOPER_RESULT__`.
- The daemon injects `LOOPER_COMPLETION_MARKER=__LOOPER_RESULT__=` into the subprocess environment.
- Prompts must keep the exact final-line contract:

  ```text
  __LOOPER_RESULT__={"summary":"<one-sentence summary>"}
  ```

- Marker parsing searches the last matching line across `stdout + "\n" + stderr`.
- Parse statuses stay:
  - `parsed`
  - `missing`
  - `invalid_json`
- `completionSignal` is set to `__LOOPER_RESULT__=` only when a marker line exists.
- Fallback summary stays "last non-empty log line" when the marker is missing or invalid.

### 2.2 Execution lifecycle contract

- Execution statuses must preserve the current observable outcomes:
  - `running`
  - `completed`
  - `failed`
  - `timeout`
  - `killed`
- Persisted active executions must continue to support recovery and stop flows.
- Runtime stop/recovery behavior from `daemon-lifecycle-parity.md` remains in force:
  - startup recovery attempts orphan cleanup for active agent executions
  - successful cleanup writes `agent.killed`
  - loop stop remains best-effort and stateful, not a synchronous hard-kill guarantee

### 2.3 Capture, heartbeat, and timeout contract

- Stdout and stderr must be captured concurrently.
- Each stream has its own bounded buffer.
- Default captured size remains `256 * 1024` bytes per stream unless overridden.
- Every received chunk increments `heartbeatCount` and updates `lastHeartbeatAt`.
- Wall-clock timeout sends `SIGTERM`, then escalates to `SIGKILL` after the graceful-shutdown window.
- Inactivity timeout uses the same `SIGTERM` path when no output heartbeat arrives before the threshold.
- Output byte accounting remains byte-based, not rune-based.

### 2.4 Persistence and event contract

- Starting an execution persists a `running` `agent_executions` row and appends `agent.invoked`.
- Mid-stream updates continue persisting heartbeat and output state.
- Final persistence continues to write:
  - `status`
  - `summary`
  - `parseStatus`
  - `completionSignal`
  - `heartbeatCount`
  - `lastHeartbeatAt`
  - `outputJson`
  - `errorMessage`
  - `endedAt`
- Terminal events remain:
  - `agent.completed`
  - `agent.timed_out`
  - `agent.killed`

## 3. Recommended Go package shape

Add a real `internal/agent` package and keep it responsible for agent-specific behavior instead of stretching `internal/infra/shell` into a second role.

Recommended file layout:

- `internal/agent/doc.go`
- `internal/agent/types.go`
- `internal/agent/prompt.go`
- `internal/agent/vendor.go`
- `internal/agent/parse.go`
- `internal/agent/buffer.go`
- `internal/agent/executor.go`
- `internal/agent/persistence.go`

Recommended public surface:

```go
type Status string

const (
    StatusRunning   Status = "running"
    StatusCompleted Status = "completed"
    StatusFailed    Status = "failed"
    StatusTimeout   Status = "timeout"
    StatusKilled    Status = "killed"
)

type ParseStatus string

const (
    ParseStatusParsed      ParseStatus = "parsed"
    ParseStatusMissing     ParseStatus = "missing"
    ParseStatusInvalidJSON ParseStatus = "invalid_json"
)

type RunInput struct {
    ExecutionID      string
    ProjectID        string
    LoopID           string
    RunID            string
    Prompt           string
    WorkingDirectory string
    Timeout          time.Duration
    HeartbeatTimeout time.Duration
    GracefulShutdown time.Duration
    MaxOutputBytes   int
    IdempotencyKey   string
    Metadata         map[string]any
    Env              map[string]string
}

type Result struct {
    Status           Status
    Summary          string
    Artifacts        []string
    ChangedFiles     []string
    Commits          []string
    Stdout           string
    Stderr           string
    ParseStatus      ParseStatus
    CompletionSignal string
    HeartbeatCount   int
    WallTime         time.Duration
    OutputBytes      int
    PID              int
}

type Execution interface {
    PID() int
    StartedAt() time.Time
    Status() Status
    Wait(context.Context) (Result, error)
    Kill(context.Context, string) error
}

type Executor interface {
    Start(context.Context, RunInput) (Execution, error)
}
```

## 4. Responsibilities by file

### `types.go`

- shared enums and data shapes
- explicit separation between execution status and parse status
- enough result detail for all loop types

### `prompt.go`

- define `AgentCompletionMarker = "__LOOPER_RESULT__"`
- provide one shared `AppendCompletionInstruction(prompt string) string`
- remove prompt-instruction drift across planner/reviewer/fixer/worker

### `vendor.go`

- port vendor-specific spawn resolution from `apps/looperd/src/infra/agent.ts`
- preserve command override and args override behavior
- preserve model-flag insertion rules
- preserve existing vendor names:
  - `claude-code`
  - `codex`
  - `opencode`
  - `cursor-cli`

### `parse.go`

- parse the last marker line across combined stdout/stderr
- return `summary`, `artifacts`, `changedFiles`, `commits`, `parseStatus`, and `completionSignal`
- preserve fallback-summary behavior

### `buffer.go`

- hold the bounded per-stream output buffer
- preserve byte-based truncation semantics
- keep this local to `internal/agent` unless shell/agent reuse becomes necessary later

### `executor.go`

- launch the subprocess
- wire concurrent stdout/stderr readers
- maintain in-memory heartbeat state
- own timeout and kill logic
- expose `Start(...)` returning a live `Execution`

### `persistence.go`

- upsert `agent_executions`
- append lifecycle events
- isolate storage/event-log details from process logic

## 5. Why not reuse `internal/infra/shell` directly

`internal/infra/shell/runner.go` already has useful pieces:

- concurrent stdout/stderr copy
- bounded buffers
- `SIGTERM` then `SIGKILL` escalation

But it is still the wrong abstraction for agent execution because it:

- is synchronous instead of returning a live execution handle
- has no heartbeat tracking
- has no inactivity timeout
- has no mid-stream persistence
- has no completion-marker parsing
- has no externally callable `Kill(reason)` behavior

The agent subsystem should therefore be a separate package. The bounded-buffer logic can be copied first and deduplicated later if it becomes worthwhile.

## 6. Runtime model

Recommended lifecycle:

```text
start
  -> persist running execution
  -> append agent.invoked
  -> stream stdout/stderr concurrently
  -> update heartbeat state in memory on each chunk
  -> debounce persistence while running
  -> wait for exit, timeout, inactivity timeout, or external kill
  -> parse completion marker from final captured output
  -> persist final record
  -> append terminal event
  -> return result
```

Recommended state transitions:

```text
running -> completed
running -> failed
running -> timeout
running -> killed
```

The storage layer may still persist transitional states such as `cancelling` for stop/recovery integration, but the executor's direct result surface should remain aligned with the current TypeScript statuses above.

## 7. Spawn and environment rules

The Go executor should preserve TypeScript spawn behavior exactly:

- merge env in this order:
  1. configured agent env
  2. per-run env
  3. daemon-injected env values
- injected env values:
  - `LOOPER_PROMPT=<full prompt>`
  - `LOOPER_COMPLETION_MARKER=__LOOPER_RESULT__=`
- preserve current command/args resolution behavior for each vendor
- preserve model override handling
- preserve existing command override escape hatches via config params

This logic belongs in a tested resolver instead of being re-implemented inside each runner.

## 8. Timeout, inactivity, and kill semantics

### 8.1 Wall-clock timeout

- When `Timeout` elapses, mark the execution as timed out.
- Send `SIGTERM` first.
- If the process still lives after `GracefulShutdown`, send `SIGKILL`.
- Default graceful window stays `5s` unless explicitly overridden.

### 8.2 Inactivity timeout

- When `HeartbeatTimeout` is configured, compare `now - lastHeartbeatAt` at a polling interval of `min(timeout, 1s)`.
- If the process is still alive and the threshold has elapsed, follow the same `SIGTERM` path as a timeout.

### 8.3 External kill

- `Kill(ctx, reason)` should mark the execution as killed and send `SIGTERM`.
- Preserve the current stop nuance: a best-effort stop request does not itself guarantee immediate final termination.
- Persist the kill reason into the final error path if there is no better stderr output.

### 8.4 Process-group decision

The TypeScript version signals only the spawned process. The Go port should intentionally improve this by using a dedicated process group and signaling the group on timeout/kill, because agent CLIs can spawn child processes that would otherwise leak.

This is an intentional implementation upgrade, but it does not change the user-visible contract and should be documented in code comments and tests.

## 9. Heartbeats and persistence strategy

TypeScript currently writes on every chunk and already carries a TODO about debouncing. The Go port should keep the same observable state while reducing write amplification.

Recommended policy:

- update in-memory heartbeat state on every chunk
- debounce SQLite upserts while the process is running
- flush immediately on process exit

Recommended debounce target:

- at most once per second while output is actively streaming

This preserves the current status surface (`heartbeatCount`, `lastHeartbeatAt`, partial output) while avoiding excessive single-connection SQLite churn.

## 10. Integration points with the rest of the daemon

### 10.1 Loop runners

Planner, reviewer, fixer, and worker should all consume the same shared prompt helper and shared completion-marker contract.

Short term, the runtime can bridge from `internal/agent` to the runner-local interfaces if that reduces churn. Longer term, the runner-local agent types should be consolidated to shared shapes to avoid drift.

### 10.2 Reviewer output handling

Reviewer logic must continue stripping completion-marker lines before interpreting or publishing review output.

### 10.3 Recovery

Startup recovery must keep treating active persisted agent executions as orphan-cleanup candidates.

That means the storage query used by recovery must include every persisted status that still represents in-flight work, including any transitional cancellation state used by stop handling.

### 10.4 Stop loop

`stopLoop(...)` must stay compatible with the current behavior described in `daemon-lifecycle-parity.md`:

- pause the loop immediately
- cancel queue items
- if an agent PID exists, send `SIGTERM`
- reflect the stop attempt in persisted execution/run state instead of promising an immediate hard stop

## 11. Known gaps in current Go code

The design spike should unblock these specific follow-on tasks:

1. `internal/agent` is currently only a placeholder package.
2. Planner, reviewer, fixer, and worker still carry duplicated or drift-prone local agent types.
3. Planner has a local completion-instruction helper instead of a shared package helper.
4. Worker still has an older inline completion-instruction string that does not match the shared TypeScript contract shape.
5. The shell runner solves only part of the problem and should not become the shared agent abstraction.

## 12. Recommended implementation order

1. Add `internal/agent` shared types, prompt helper, and parser.
2. Port vendor spawn resolution with unit tests copied from TypeScript behavior.
3. Implement the executor with concurrent streaming, heartbeats, timeout handling, and final parsing.
4. Add persistence/event wiring for `agent_executions` and `agent.*` events.
5. Bridge the executor into planner/reviewer/fixer/worker.
6. Wire startup recovery and loop stop to the new execution records.
7. Add parity tests for timeout, inactivity timeout, kill, parsing, reviewer stripping, recovery cleanup, and stop behavior.

This keeps the next tasks aligned with the task list:

- `Port agent execution lifecycle and heartbeat handling`
- `Port agent completion-marker behavior`
- `Preserve concurrent stdout/stderr capture, bounded buffers, inactivity timeout, and kill escalation`

## 13. Minimum test plan

### Unit tests

- prompt helper preserves exact completion instruction text
- vendor resolver parity for `claude-code`, `codex`, `opencode`, and `cursor-cli`
- parser returns `parsed`, `missing`, and `invalid_json` correctly
- parser uses the last marker line across stdout and stderr
- bounded buffer preserves byte-based truncation semantics

### Integration tests

- subprocess success path with valid completion marker
- subprocess failure path without marker and fallback summary
- wall-clock timeout with `SIGTERM` then `SIGKILL`
- inactivity timeout triggered by missing output
- external kill path
- concurrent stdout/stderr capture and final parsing

### Runtime/storage tests

- starting an execution writes `agent.invoked`
- finishing writes the correct final event and persisted fields
- startup recovery cleans orphan agent executions and appends `agent.killed`
- stop-loop behavior preserves best-effort cancellation semantics

## 14. Intentional deviations from the current TypeScript implementation

These deviations are recommended and acceptable because they improve runtime safety without changing the external compatibility boundary:

1. Debounce mid-stream heartbeat/output writes instead of writing on every chunk.
2. Use process-group signaling so timeout/kill cleanup covers subprocess trees, not only the direct parent process.

Everything else should preserve current TypeScript behavior exactly unless a later task explicitly changes the contract.
