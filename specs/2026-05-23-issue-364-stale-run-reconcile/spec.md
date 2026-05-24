# Issue #364 — Recover Stale Running Tasks After Sleep/Wake

## 1. Background

Looper already has startup recovery that can interrupt stale or orphaned `running` runs when `looperd` starts. That helps after a daemon restart, but it does not fully solve the user-visible failure from issue #364:

- the machine sleeps,
- the child agent process disappears,
- the daemon later appears healthy again,
- one or more runs remain `status=running`,
- scheduler capacity stays consumed,
- queued work remains blocked indefinitely.

The problem is not merely presentation. The underlying run and queue bookkeeping remain in a blocking state while there is no real live agent execution behind them.

Today Looper also lacks a clear product surface for this situation. Operators can use `looper daemon restart` or try `looper stop`, but there is no explicit “reconcile stale running runs” workflow documented or exposed as a dedicated API/CLI.

---

## 2. Goals

### 2.1 Primary goals

1. Automatically release scheduler capacity held by stale `running` runs after sleep/wake without requiring a daemon restart.
2. Add a supported manual recovery command/API for operators.
3. Reuse the existing startup stale-run recovery logic instead of introducing a second lifecycle system.
4. Prevent stale runs from appearing as normal active work in default active-run views.

### 2.2 Non-goals

1. Do not introduce a new persisted run state such as `stale` or `recovered`.
2. Do not add a new background watchdog or a large new state machine.
3. Do not use GitHub state as the authority for local process liveness.
4. Do not solve this by only hiding stale rows in the UI while leaving scheduler state blocked.

---

## 3. Problem summary

The issue is caused by a mismatch between:

- **DB state**: run row says `running`, queue row may say `running`, loop may say `running`
- **real runtime state**: the underlying agent process is gone

The scheduler trusts the persisted `running` bookkeeping enough to count those items against capacity. That is correct when the bookkeeping is fresh, but wrong after machine sleep/wake if the child process has disappeared and no online reconciliation happens.

Existing startup recovery already proves that Looper has the right basic primitive:

- identify stale/orphaned running runs
- interrupt them safely
- repair queue state so work can continue

The missing piece is to reuse that primitive while the daemon is still alive.

---

## 4. Design principles

### 4.1 Reuse one reconciliation primitive everywhere

Looper should have one stale-run reconciliation primitive reused by:

1. startup recovery,
2. live scheduler recovery,
3. manual operator recovery.

This avoids split logic and prevents startup and live recovery from drifting apart.

### 4.2 Name the authority clearly

The authority for whether a run is still alive is **verified local execution identity**, not the persisted `running` status by itself.

A run should be treated as live only when Looper can verify a matching active agent execution with a real process behind it.

In live daemon mode, that authority must stay narrow. The absence of a verified agent process is **not** by itself authority to interrupt every `running` run, because some steps may still be executing inside the daemon without an agent process.

### 4.3 Repair state, do not mask symptoms

The fix must repair the run/queue state that blocks scheduler capacity. Merely filtering stale items out of `/api/v1/runs/active` is insufficient.

### 4.4 Be conservative before interrupting live work

Live reconciliation must use a grace period and identity checks so a fresh run is not interrupted just because its PID or first heartbeat has not propagated yet.

---

## 5. Recommended approach

## 5.1 Extract a shared stale-run reconciler

Extract the stale/orphaned running-run logic from startup recovery into a shared helper, for example:

```go
ReconcileStaleRunningRuns(ctx, repositories, now, mode) (summary, error)
```

Where `mode` is one of:

- `startup`
- `live`
- `manual`

The helper should:

1. list candidate `running` runs,
2. inspect the latest run/loop/queue/execution state,
3. verify whether a live agent execution still exists,
4. interrupt truly stale runs,
5. repair queue state,
6. emit a structured summary and recovery events.

### 5.1.1 Why this is the right level of abstraction

This adds one reusable behavior, not a new concept. The concrete failure it prevents is duplicated or inconsistent stale-run handling between startup recovery, live scheduler operation, and manual recovery. The cost is limited to one shared helper and mode-specific guards. A simpler alternative — continuing to keep startup-only logic and adding ad hoc live fixes elsewhere — would create divergence and more failure modes over time.

---

## 5.2 Add automatic live reconciliation at the scheduler boundary

When the scheduler is about to skip claiming work because running capacity is full, it should first attempt stale-run reconciliation.

Recommended flow:

1. scheduler computes running capacity,
2. if capacity is available, continue normally,
3. if capacity is exhausted, call live stale-run reconcile,
4. recompute capacity,
5. if stale work was released, continue claiming queued work.

This keeps the extra process inspection work focused on the exact scenario where stale runs are causing damage.

### 5.2.1 Why not a polling watchdog

A periodic watchdog would add more moving parts and repeated `ps` inspection even when the system is healthy. Scheduler-bound reconciliation is simpler and directly tied to the blocking symptom.

---

## 5.3 Add an explicit manual recovery API and CLI

Add a dedicated product surface, for example:

```bash
looper run reconcile-stale
```

Back it with a route such as:

```http
POST /api/v1/runs/reconcile-stale
```

The command should return a structured summary, for example:

- interrupted runs
- repaired queue items
- cleaned agent executions
- skipped uncertain candidates

This gives operators a safe, supported recovery path without needing to restart the daemon.

Initial manual recovery behavior should be narrow and explicit:

1. it is mutating,
2. it uses the same safety rules as live reconciliation,
3. it scans all stale running candidates rather than only capacity-full cases,
4. it does not provide `--force` in the initial version,
5. it reports uncertain candidates rather than killing or signalling them blindly.

`looper daemon restart` remains a fallback, not the primary workflow.

---

## 6. Authority and stale-run decision rules

## 6.1 Positive liveness authority

For agent-backed work, a run is live only if Looper can verify all of the following:

1. there is an active agent execution associated with the run,
2. the execution has a PID,
3. the process still exists locally,
4. the running process command matches the persisted command identity.

This should reuse the existing process identity helpers already used by startup recovery.

## 6.2 Live stale-run rule

In `live` mode, a running run can be interrupted only when all of the following hold:

1. the run is still persisted as `running`,
2. the run is in an agent-backed or agent-waiting phase, or it has an associated active execution row whose PID is missing or dead,
3. there is no verified live execution behind it,
4. its heartbeat / activity timestamp is older than the stale threshold,
5. it is still occupying scheduler capacity directly or indirectly.

In live mode, absence of a verified agent execution is not universal authority to interrupt every running run. It is authority only for runs whose state indicates they are agent-backed candidates for reconciliation.

Initial stale threshold: reuse the existing active-run heartbeat TTL of **30 minutes**. Do not add a new config knob in this change.

## 6.3 Queue state is not authority

`queue.status = running` is not proof of liveness. It is scheduler bookkeeping and must be repaired after a stale run is interrupted.

## 6.4 Uncertain cases

If PID inspection fails, command identity cannot be validated, or the state is otherwise ambiguous, automatic live reconciliation should skip the candidate and report it as uncertain.

Manual recovery may support an explicit force mode later, but the initial version should avoid force semantics unless clearly needed.

## 6.5 Process identity limits

Command matching reduces accidental signalling risk, but it does not fully eliminate same-command PID reuse risk.

This change accepts that residual risk rather than introducing a new persisted process-start identity field.

---

## 7. Product behavior changes

## 7.1 Automatic behavior

After sleep/wake, stale `running` runs should stop blocking queued work once the scheduler next reaches the “capacity full” path and reconciles stale entries.

## 7.2 Manual behavior

Operators should be able to run a dedicated command that reconciles stale running tasks immediately and prints a clear summary.

## 7.3 Active-run presentation

Default active-run views should no longer treat “loop is running but the run is stale” as normal active work.

If Looper wants to keep stale historical visibility, that should live behind an explicit expanded or diagnostic view rather than the default active view.

---

## 8. Implementation plan

## 8.1 Runtime recovery extraction

Update `internal/runtime/runtime.go` to:

1. extract the stale-running-run portion of startup recovery into a shared helper,
2. preserve the existing startup behavior by calling that helper in `startup` mode,
3. return a reusable summary struct suitable for startup, live, and manual flows.

The helper should continue reusing the current interruption path instead of introducing a new terminal transition mechanism.

## 8.2 Scheduler integration

Update the scheduler path under `internal/runtime/` so that when running capacity is exhausted, it performs one live stale-run reconciliation pass before giving up on claiming new work.

This must run inside the existing scheduler claim serialization so concurrent claim passes do not duplicate repair or claim decisions.

The flow should be:

1. compute available slots,
2. if slots are available, continue normally,
3. if slots are exhausted, run one live reconcile pass inside the claim path,
4. recompute available slots in the same claim pass,
5. continue claiming only if capacity was truly released.

This must be idempotent and safe to run repeatedly.

## 8.3 Queue repair

Ensure that interrupting a stale run also repairs its queue consequences:

- stale `running` queue rows should no longer hold capacity,
- loops that should continue should become claimable again,
- paused or terminal loops should not be spuriously requeued.

Required semantics:

1. if a stale run is interrupted and the loop should continue, requeue the running queue item for that loop or ensure exactly one queued item exists,
2. if the loop is paused, stopped, or terminal, cancel stale running queue items and do not requeue them,
3. queue repair must be idempotent,
4. run interruption and queue repair should happen in the same consistency boundary where practical so capacity is not left half-repaired.

## 8.4 API and CLI

Add:

- a manual reconcile API endpoint in `internal/api/handler.go`,
- runtime wiring in `cmd/looperd/main.go`,
- a CLI command and human/JSON output under `internal/cliapp/`.

The response should expose counts and IDs where practical, but the product surface should stay simple.

Live/manual cleanup should stay narrow: only executions tied to stale candidate runs should be marked dead or killed. This command should not become a broad orphan-agent sweeper.

When a dead execution is marked terminal, it should preserve the same native-resume metadata behavior already used by startup recovery.

## 8.5 Active-runs behavior cleanup

Update the active-runs view logic so stale fallback behavior is no longer the default interpretation of active work.

Default active view should:

1. show verified live running runs,
2. show queued loops with active queued work,
3. optionally show very fresh running loops without runs only within the same freshness window,
4. hide stale running-loop fallback by default,
5. keep stale or historical visibility behind `all=true` or another explicit diagnostic surface.

This should include replacing the current test expectation that stale running loops still appear as active.

## 8.6 Documentation

Update user-facing docs to explain:

1. what stale running tasks after sleep/wake look like,
2. the automatic recovery behavior,
3. the new manual recovery command,
4. when `looper daemon restart` is still a reasonable fallback.

---

## 9. Files likely to change

Primary areas:

- `internal/runtime/runtime.go`
- `internal/runtime/runtime_test.go`
- `internal/runtime/scheduler.go`
- `internal/api/handler.go`
- `internal/api/handler_test.go`
- `cmd/looperd/main.go`
- `internal/cliapp/app.go`
- `internal/cliapp/json_output.go`
- `internal/cliapp/human_output.go`
- relevant docs under `docs/`

Additional test files may be needed if queue/rerun behavior is owned elsewhere.

---

## 10. Required regression coverage

At minimum, add or update tests for:

1. **live stale-run interruption**
   - agent-backed or agent-waiting running run + stale heartbeat + no verified live execution → interrupted.

2. **scheduler capacity unblock**
   - stale running work consumes the last slot,
   - queued work exists,
   - reconcile releases capacity,
   - queued work becomes claimable.

3. **queue repair semantics**
   - stale running queue item no longer blocks capacity after reconcile,
   - loop is requeued only when appropriate.

4. **do not interrupt live work**
   - stale-looking heartbeat but matching live PID/command → leave running.

5. **fresh-start grace period**
   - run has not yet established full execution metadata, but timestamps are fresh → do not interrupt in live mode.

6. **non-agent live-mode safety**
   - running run in a non-agent or in-process phase + stale heartbeat + no agent execution → skipped in live mode, because absence of an agent is not authority for that phase.

7. **dead PID execution cleanup**
   - execution row exists but process is gone → cleanup is reflected in run/execution state.

8. **uncertain process identity**
   - process inspection is ambiguous → auto-reconcile skips rather than killing blindly.

9. **manual API/CLI flow**
   - API returns a stable summary,
   - CLI renders both human and JSON output.

10. **active-runs default behavior**
   - stale running loops no longer appear as normal active work by default.

11. **idempotent repeated reconcile**
   - running reconcile twice does not create duplicate queue rows, duplicate repair state, or unstable summary counts on the second pass.

---

## 11. Risks and edge cases

1. **False interruption of real work**
   - mitigated by TTL + verified process identity.

2. **PID reuse**
   - reduced, but not fully eliminated, by command identity matching before trusting or signalling a process.

3. **Fresh-start race**
   - mitigated by live-mode grace period.

4. **Repeated reconcile churn**
   - mitigated by idempotent interruption and queue-repair behavior.

5. **Presentation vs state divergence**
   - mitigated by fixing state first, then tightening active-run presentation.

6. **Manual recovery expectations**
   - documentation must explain whether the command interrupts stale work, requeues work, or only reports it.

---

## 12. Rollout order

Recommended order:

1. extract shared reconciler from startup recovery together with helper-level regression coverage,
2. add live scheduler integration together with the capacity-unblock regression test,
3. add manual API/CLI,
4. tighten active-runs presentation,
5. update docs.

This order keeps the core correctness change first and the product surface changes second.

---

## 13. Success criteria

This work is done when all of the following are true:

1. a stale post-sleep `running` run no longer blocks queued work indefinitely while the daemon remains alive,
2. operators have a documented, supported manual reconcile command,
3. startup and live stale-run recovery share one implementation path,
4. default active-run views no longer present stale work as healthy active work,
5. regression tests cover both safety and unblock behavior.
