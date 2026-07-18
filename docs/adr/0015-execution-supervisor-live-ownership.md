# ADR-0015: Execution Supervisor is live ownership Authority

**Status:** Proposed / Partially Implemented

This ADR is the phased contract for the live execution ownership program.
Status remains **Proposed / Partially Implemented** until every full-program
exit criterion below holds. Do not mark this ADR Accepted when only a subset of
implementation slices land.

**Numbering note:** ADR-0014 is reserved for config-file policy Authority. Do
not reuse 0014 for execution ownership. Related draft work under issue #572 used
an earlier “0014” filename in a draft branch; that draft is superseded by this
ADR and the sliced implementation graph (#575–#581). Keep #572 draft — do not
land it as a competing broad implementation.

## Context

Looper starts agent and subprocess work from many daemon and CLI paths. Live
ownership is currently split across `exec.Cmd` handles, a partial in-memory
`ActiveExecutionRegistry`, persisted `agent_executions` rows (including PID),
queue `running` claims, and PID/process-group probes during stop, shutdown, and
startup recovery.

Those representations can disagree during:

- queue claim before any process exists
- agent spawn and native-resume fallback
- mid-life heartbeat / terminal persistence
- loop stop and daemon shutdown
- startup recovery after crash

Concrete failures include: an unregistered process surviving stop; a stale
`running` write overwriting terminal state; a claimed queue item with no live
owner; recovery treating a reusable PID/PGID as identity and signalling or
marking work terminal without confirmed containment.

This is the HITL gate for the ownership program: runtime PRs must map to the
enforcement matrix below and keep the producer inventory reconciled.

## Decision

The in-memory **Execution Supervisor** is the Authority for live execution
ownership in a running daemon. It owns:

1. **Admission state** for the live daemon (`starting | ready | stopping | degraded`)
2. **Operation leases** that span durable queue claims until durable finalize
3. **Process containment handles** (configure / signal / wait / confirmed drain)
4. **Stop delivery and release** only after confirmed non-runnable ownership
5. **Ordered execution persistence** as durable observation, not a second live Authority

Callers reserve ownership before they claim or start work and cannot publish a
half-started execution. SQLite `agent_executions` rows and process inspection are
**recovery evidence** for operators and startup reconciliation; they do not
authorize a running daemon to release a live handle, start overlapping work, or
treat PID absence as confirmed-dead.

Backends that cannot confirm drain fail loud / degrade instead of inferring
success from signal delivery. A daemon crash loses in-memory Authority; startup
must classify durable observations conservatively before mutations are enabled.

## Authority

> The Authority for a live action is the Execution Supervisor reservation (admission
> + operation lease) and its owned containment handle. SQLite rows and PID/PGID
> probes are recovery evidence only — never live stop, terminal, requeue, or
> overlap Authority while the daemon is live, and never confirmed-dead Authority
> after restart solely because a PID is missing or a leader has exited.

Why this is not the agent’s structured output: agents do not own process
lifecycle, queue claims, or stop semantics. Why this is not SQLite alone:
persistence can lag the process and is an observation. Why this is not raw
PID/PGID: numeric IDs are reusable and probe-then-signal is not atomic.

## Trade-off

**Prevents:** unowned durable claims; late live writes after terminal state;
shutdown closing SQLite under an active finalizer; stop/recovery signals to a
reused PID/PGID; dual ready flags that disagree with admission.

**Costs:** one in-memory Supervisor lifecycle; explicit lease release; serialized
per-execution persistence; platform-specific containment adapters; sticky
`degraded` until restart/clear; more quarantine / manual-intervention during
uncertain recovery instead of aggressive auto-clean.

**Why simpler alternatives are insufficient:**

- Extending only `ActiveExecutionRegistry` registers some agents after spawn and
  does not own queue admission, terminal persistence, or non-agent producers.
- More PID/PGID validation is insufficient: IDs are reusable and
  probe-then-signal is not atomic.
- Trusting SQLite as live Authority is insufficient: observations lag and can
  regress if writers are not ordered.
- Landing one broad “Supervisor everywhere” PR (#572-style) without the phased
  safety floor re-opens dual kill paths and mid-rollout danger.

**Deletion attempt:** remove runtime PID fallback and independent kill paths
entirely when a Supervisor exists. PID inspection remains only as recovery
evidence and for paths still documented outside the Supervisor domain until
their cutover issue closes. Do **not** remove agent live PID fallback until
every in-scope agent producer is Supervisor-owned (#576).

## Phased consequences and enforcement matrix

Implementation order (dependency graph):

```
573 (this ADR) → 575 → 574 → 576 ─┬→ 577 ─┐
                                 └→ 578 → 579 → 580 → 581
                                           └────▲
```

Native GitHub `blocked_by` edges match this graph. Do not add `ready-for-agent`
to a slice while any open blocker remains.

| Order | Issue | Role | ADR consequences in this slice | Enforcement (as of this ADR) |
|------:|-------|------|--------------------------------|------------------------------|
| R0 | #573 | Contract + inventory | Authority statement, matrix, producer inventory, mid-state rules, exit criteria | **Enforced** (docs-only; this document) |
| R1 | #575 | Safety floor: one admission state; stop unsafe recovery PID action | Single admission Authority; no mutation/claim before ready; recovery no-act + quarantine; drain ingress before storage close | **Enforced** |
| R2 | #574 | Process containment handle with confirmed drain | Containment API; kill success = confirmed drain; no production removal of PID fallback yet | **Enforced** |
| R3 | #576 | Own all in-scope agent spawns at common executor boundary | Lease before `cmd.Start`; bind handle before return; stop-kill via handle; remove agent live PID fallback only after full agent coverage | **Enforced** |
| R4 | #577 | Migrate remaining Supervisor-owned non-agent subprocesses | Validation/shell and other in-scope non-agents on containment; no raw PID fallback inside Supervisor domain; shutdown order tested | **Deferred** |
| R5 | #578 | Execution persistence Authority + degrade on mid-life failure | Ordered writer; terminal immutability; hard persist failure degrades; no terminal status before confirmed dead | **Enforced** |
| R6 | #579 | Operation lease owns queue claims until durable finalize | No live `running` claim without lease; release only after durable finalize; finalize failure retains ownership + degrades | **Deferred** |
| R7 | #580 | Full non-mutating coverage when not-ready or degraded | Exhaustive mutation surface audit; scheduler pause; HTTP 503; no dual ready Authority | **Deferred** |
| R8 | #581 | Conservative startup recovery without PID Authority | confirmed dead / observed live / uncertain; uncertain cannot act; PID is evidence only | **Deferred** |

Update the **Enforcement** column as each issue closes (move the slice from
Deferred → Enforced). ADR Accepted only when the matrix has no deferred
in-scope items and full-program exit criteria hold.

### Admission state (enforced by #575)

Authoritative live states: `starting | ready | stopping | degraded`.

- Transitions are monotonic / legal only (e.g. `starting→ready`, `ready→stopping`,
  sticky `degraded` until restart/clear). Documented in `internal/runtime/admission.go`.
- HTTP mutation readiness and the work-producing **scheduler tick** (discovery,
  HITL, claims, stale-reconcile) are **projections** of this state, not a second
  Authority. Exhaustive non-claim mutation surface audit remains #580.
- Admission decisions must be atomic with the action they gate (single `Admission`
  mutex; no dual ready flag that can disagree).

**Admission concept trade-off (R1):**

| | |
|--|--|
| **Failure prevented** | Dual ready flags and claim-only gates that admit enqueue/mutate while starting or after `BeginShutdown`; recovery acting on reusable PIDs without a closed process-lifetime Authority. |
| **Costs** | Sticky `degraded`; no-op ticks while not ready; every new work-producing path must consult admission; more quarantine/`manual_intervention` during uncertain recovery. |
| **Why not simpler** | A boolean ready next to `ownershipAcquired` re-creates dual Authority. Gating only `ClaimNext*` still lets discovery/HITL/reconcile persist queue work. SQLite/PID probes lag and are not atomic with admission. |
| **Deletion attempt** | Drop separate readiness and trust process/agent signals alone — insufficient for multi-PR ownership rollout before Supervisor coverage. |

### Process containment handle (enforced by #574)

Library: `internal/processcontainment`. Additive only — producers still use
existing stop/kill paths until #576/#577. The handle owns configure (Setpgid),
signal, exactly-once leader wait/reap, descendant drain, and **confirmed-dead**
reporting. Kill/Drain success means the owned process group is confirmed
non-runnable and the leader is reaped; signal delivery alone is never success.
Timeouts fail loud (`ErrNotConfirmedDead`) rather than reporting false success.

**Containment concept trade-off (R2):**

| | |
|--|--|
| **Failure prevented** | Stop paths treating SIGTERM/SIGKILL delivery (or leader exit alone) as success while TERM-resistant children or background group members remain runnable. |
| **Costs** | Platform-specific process-group drain; stop latency when descendants resist TERM; callers must handle explicit drain timeout/failure. |
| **Why not simpler** | Reusing PID/PGID probe-then-signal without a wait/reap Authority reintroduces the reusable-ID and false-success failures this program forbids. |
| **Deletion attempt** | Remove dual kill paths immediately — blocked until #576/#577 migrate producers; this slice stays additive so mid-rollout does not create partial cutover. |

### Agent spawn ownership at common executor boundary (enforced by #576)

All **Supervisor-owned agent** producers (Planner, Reviewer, Fixer, Worker,
Coordinator triage, native-resume fallback) go through `agent.ConfiguredExecutor.Start`
with a `SpawnOwner` (the daemon `ActiveExecutionRegistry`):

1. **Admit spawn lease** before `cmd.Start` (also projects daemon `Admission.AllowClaim`).
2. **Configure + Start + Bind** `processcontainment.Handle` before returning to role code.
3. **Stop-vs-spawn / stop-vs-bind**: `BeginLoopStop` / `BeginShutdown` cancels pending
   leases; `BindHandle` kills and confirmed-drains before Start can return success.
4. **Stop/kill** uses the bound handle (confirmed drain). Live in-process SQLite PID
   fallback is removed when the registry is present — not the recovery PID action
   forbidden by #575.
5. **Cancellation** of the lease context prevents native-resume fallback from
   spawning a second process.

This is intentionally **not** the #572 approach (post-spawn register in four role
adapters with incomplete Coordinator coverage). Ownership is at the common executor
boundary so there is no worker-only registry escape hatch.

**Agent ownership concept trade-off (R3):**

| | |
|--|--|
| **Failure prevented** | `looper stop` missing unregistered agents (Coordinator/planner/reviewer/fixer); stop-vs-spawn returning a live unowned process; dual kill via SQLite PID while a handle exists. |
| **Costs** | Every agent Start path consults the registry; stop latency includes confirmed drain; unit tests need Owner nil or a registry; native-resume fallback must rebind handles. |
| **Why not simpler** | Post-spawn adapter Register (#572) leaves Coordinator and race windows unowned. Keeping PID fallback after full coverage reintroduces reusable-ID Authority. |
| **Deletion attempt** | Remove registry and trust only `exec.Cmd` + SQLite — fails stop when live handle is missing and reopens dual Authority. |

### Execution persistence Authority (enforced by #578)

SQLite `agent_executions` is a **durable observation**, not a second live
Authority. Each in-process execution serializes its own writes (`persistMu`);
there is no general global writer subsystem.

**Terminal immutability policy** (storage-level, `AgentExecutionsRepository.Upsert`):

| From → To | Allowed? |
|-----------|----------|
| active (`running`, `cancelling`) → active | yes |
| active → terminal (`completed`, `failed`, `timeout`, `killed`, legacy `success`) | yes |
| terminal → active | **no** (conflict) |
| terminal → different terminal | **no** (first terminal wins; conflict) |
| terminal → same terminal (field enrichment) | yes (e.g. native-resume metadata) |

Zero-row / rejected upserts return `ErrAgentExecutionConflict`, never success.

**Hard persistence failure policy:**

| Path | Behavior |
|------|----------|
| **Initial** ownership after spawn+bind | Fail `Start` loud; kill/confirmed-drain the process; do not leave unowned live process |
| **Heartbeat / output** mid-life | Surface error; **first hard failure** closes admission (`degraded`) |
| **Terminal** | Fail loud via `Wait` error; do not report successful completion; degrade if storage is broken |
| Soft/transient | One retry on SQLITE_BUSY/locked; pure cancel/context death and conflict-after-terminal-won do **not** sticky-degrade |

Terminal status is not published until containment is confirmed dead for owned
handles (`Drain` when needed after leader `Wait`).

**Operator recovery from degraded (persistence hard failure):**

1. Inspect daemon logs for `daemon admission degraded after agent execution persistence failure` and the underlying storage error.
2. Repair storage (disk space, permissions, SQLite integrity) under the configured runtime path (default `~/.looper/`).
3. **Restart `looperd`** — the normal recovery path. Admission is sticky degraded until process restart (or an explicit `ClearDegraded` test/operator hook).
4. After restart, startup recovery classifies durable observations conservatively (#581); do not manually requeue uncertain work.

**Persistence concept trade-off (R5):**

| | |
|--|--|
| **Failure prevented** | Stale live writers regressing terminal rows; silent upsert “success” when zero rows changed; split-brain observations while admission keeps accepting work; terminal status before containment confirmed dead. |
| **Costs** | Sticky degrade stops new work until restart/clear; terminal conflict means first terminal wins even if a later writer had richer fields; per-execution serialization; hard fail paths kill on initial persist failure. |
| **Why not simpler** | Trusting raw Upsert success leaves #579 release able to free claims on silent no-op writes. Global writer queues add complexity without fixing per-execution ordering. Soft-degrade on cancel creates sticky noise without recovery signal. |
| **Deletion attempt** | Remove mid-life heartbeat persistence and keep only terminal — insufficient for operator progress and native-session capture while live. |

### Shutdown order (target; partial in #575, complete in #577/#580)

Drain **admission → ingress → producers → handles/finalizers** before SQLite
close. On timeout: retain storage / fail loud — never report graceful success
with undrained ownership.

## Process-producer inventory

Classification:

| Class | Meaning |
|-------|---------|
| **Supervisor-owned (in scope)** | Must eventually hold an operation lease + containment handle under the Supervisor while the daemon is live. Unowned paths block program exit. |
| **Independently lifecycle-owned** | Documented separate Authority; not Supervisor domain. Must not be “half-migrated” into Supervisor without reclassifying. |
| **Explicitly out of scope** | Not a daemon work producer for this program (tests, operator tooling outside looperd, etc.). |

### Supervisor-owned (in scope)

| Producer | Current spawn path (main) | Notes / target cutover |
|----------|---------------------------|------------------------|
| **Planner agent** | `internal/runtime/scheduler.go` planner adapter → `agent.Executor.Start` | **Enforced by #576** via common executor `SpawnOwner` |
| **Reviewer agent** | scheduler reviewer adapter → `agent.Executor.Start` (incl. native-resume fields) | **Enforced by #576** |
| **Fixer agent** | scheduler fixer adapter → `agent.Executor.Start` | **Enforced by #576** |
| **Worker agent** | scheduler worker adapter → `agent.Executor.Start` (incl. native-resume) | **Enforced by #576** (not post-spawn adapter Register) |
| **Coordinator agent** | `internal/coordinator/agent_llm.go` → shared `agent.Executor.Start` | **Enforced by #576**; same executor, not a separate spawn stack |
| **Native-resume fallback** | Same `agent.Executor.Start` with `NativeResumePrompt` / session; fallback to full prompt on resume failure | **Enforced by #576**; cancellation must not spawn a second process after stop |
| **Worker validation shell** | `internal/worker/runner.go` → `shell.Run` (`/bin/sh -c` validation commands) | #577 non-agent containment |
| **Fixer (and other role) shell helpers** that run daemon-owned long/blocking shell for work steps | e.g. fixer `shell.Run` helpers used during run processing | #577; short tool calls may reclassify only if inventory is updated |
| **Trusted review-submit children** | `internal/forge/trusted_review_proxy.go` spawns `looper review submit` child from daemon-bound proxy | #577; child is daemon-owned for stop/drain while proxy request is live |
| **Active agent stop / loop halt / daemon shutdown kill of owned agents** | Registry `Kill` via bound containment handle (confirmed drain); no live SQLite PID fallback when registry present | **Enforced by #576** after common-executor ownership; recovery still no raw PID action (#575) |

Queue **claims** themselves are not process producers, but while the daemon is
live a `queue_items.status=running` claim is an owned **operation** under #579
and must not exist without a Supervisor lease.

### Independently lifecycle-owned (documented separate Authority)

| Producer | Path | Separate Authority |
|----------|------|--------------------|
| **Webhook forwarder (`gh webhook forward`)** | `internal/runtime/webhook.go` (`newWebhookRuntime` / `runForwarder`; `webhook_forwarder.go` manager is not production-wired) | ADR-0005: local identity gate (PID + process start + command shape). Not Supervisor domain unless this ADR is amended. |
| **Webhook tunnel `gh` subprocesses** | `internal/runtime/webhook_tunnel.go` | Local tunnel lifecycle under webhook tunnel design (ADR-0006 family); not agent work ownership. |
| **CLI feedback agent** | `internal/cliapp/feedback.go` → `agent.ResolveSpawn` + `exec.CommandContext` in **CLI process** | CLI process owns the child for the duration of `looper feedback`. Not looperd Supervisor. |
| **CLI interactive takeover resume** | `internal/cliapp/takeover_commands.go` runs operator shell with `ResumeCommand` after daemon parks loop | Operator terminal owns the interactive agent. Daemon Authority for parking/stopping the prior run remains Supervisor-owned once #576 lands. |
| **CLI daemon spawn / stop** | `internal/cliapp/daemon_runtime.go` `SpawnDetached` / `KillProcess` of **looperd** | CLI/service manager owns the daemon process, not in-daemon work ownership. |
| **CLI config editor / dashboard browser open** | `config_commands.go`, `dashboard_command.go` | Short-lived operator tools; CLI-owned. |
| **osascript notifications** | `internal/infra/notify/gateway.go` via `shell.Run` | Notification channel lifecycle; short-lived; not queue/agent ownership. |
| **git / gh / tea tool invocations** | `internal/infra/git`, `internal/infra/github`, `internal/forge/tea` via `shell.Run` | Provider/tool gateways; request-scoped short commands under their gateways, not Supervisor agent leases. If a future path becomes long-lived owned work, reclassify before cutover. |
| **Daemon `ps` liveness/identity probes** | `internal/runtime/runtime.go` (`defaultReadProcessCommand` for agent execution match); `internal/runtime/webhook_lifecycle.go` (`defaultProcessProbe.Argv` / `psProcessStart` non-Linux paths for forwarder identity) | Short-lived recovery/identity **evidence** only (see Authority). Not Supervisor-owned work producers and not R4 containment targets. They must never authorize live stop, terminal, requeue, or overlap while the daemon is live, and must not become confirmed-dead Authority after restart solely from PID absence. #575/#581 keep probes as evidence; do not migrate them onto Supervisor leases. |

### Explicitly out of scope

| Producer | Reason |
|----------|--------|
| **E2E harness / test helpers** (`internal/e2e/harness`, `*_test.go` helper processes) | Test infrastructure; not production looperd ownership. |
| **External agent children of vendor CLIs** not started by Looper | Outside Looper’s spawn boundary; containment is process-group based for Looper-started leaders only. |
| **Human-edited forge state** | GitHub/Forgejo remain Authority for work eligibility; not process ownership. |

### Inventory review rules

- No known daemon spawn path may remain unclassified.
- Adding a new producer requires an inventory row in the same PR that introduces it.
- Reclassification (independent → Supervisor-owned or the reverse) requires an
  ADR matrix note and the owning implementation issue updated.
- #576 closes only when every **Supervisor-owned agent** path is covered.
- #577 closes only when every **Supervisor-owned non-agent** path is covered.

## Dangerous mid-state rules

These rules apply during multi-PR rollout and are part of this contract:

1. **Do not remove agent live in-process PID fallback** until every in-scope agent
   producer is Supervisor-owned and verified (#576). “Fallback” means reconstructing
   stop/kill from SQLite PID while the daemon is live — not reintroducing recovery
   PID action forbidden by #575.
2. **Recovery no-act must pair with quarantine**: uncertain evidence must not
   requeue, mark terminal, signal raw PID/PGID, or start overlapping work. Prefer
   existing manual-intervention (or equivalent) states unless a later slice
   justifies a new quarantine concept.
3. **No dual kill paths with partial cutover**: containment library (#574) is
   additive until producers migrate (#576/#577).
4. **Do not land operation-lease release (#579) before persistence Authority (#578)** —
   silent finalize failure plus release creates unowned durable claims.
5. **Do not mark this ADR Accepted** until full-program exit criteria hold, even
   if intermediate slices report local success.

## Full-program exit criteria (ADR Accepted only when all hold)

- [ ] R1–R8 issues (#575–#581) closed with their acceptance criteria met
- [ ] Enforcement matrix fully enforced (no deferred in-scope items left open)
- [ ] Producer inventory reconciled: every in-scope path Supervisor-owned; independent/out-of-scope documented
- [ ] No unowned in-scope agent or subprocess producer remains
- [ ] No live stop/shutdown/recovery path uses raw PID/PGID as Authority
- [ ] No running queue claim without an owned operation lease while daemon is live
- [ ] Uncertain recovery evidence cannot signal, mark terminal, requeue, or overlap work
- [ ] Shutdown drains admission → ingress → producers → handles/finalizers before SQLite close (or retains storage / fails loud on timeout)

## Follow-on implementation issues

| Order | Issue | Title |
|------:|-------|-------|
| R1 | [#575](https://github.com/nexu-io/looper/issues/575) | Safety floor: one admission state + stop unsafe recovery PID action |
| R2 | [#574](https://github.com/nexu-io/looper/issues/574) | Process containment handle with confirmed drain |
| R3 | [#576](https://github.com/nexu-io/looper/issues/576) | Own all agent spawns at common executor boundary (stop-kill) |
| R4 | [#577](https://github.com/nexu-io/looper/issues/577) | Migrate remaining daemon subprocesses onto containment |
| R5 | [#578](https://github.com/nexu-io/looper/issues/578) | Execution persistence Authority + degrade on mid-life failure |
| R6 | [#579](https://github.com/nexu-io/looper/issues/579) | Operation lease owns queue claims until durable finalize |
| R7 | [#580](https://github.com/nexu-io/looper/issues/580) | Full non-mutating coverage when not-ready or degraded |
| R8 | [#581](https://github.com/nexu-io/looper/issues/581) | Conservative startup recovery classification without PID Authority |

Related: [#572](https://github.com/nexu-io/looper/issues/572) (retargeted by #576; keep draft).

## Non-regression for this ADR

Docs only — no runtime behavior change in the PR that lands ADR-0015.
