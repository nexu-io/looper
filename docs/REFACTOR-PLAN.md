# Looper lifecycle refactor — plan & tracking

Goal: kill the structural instability + collapse the accidental complexity (the codebase
"doesn't feel as pure as synclo"). Grounded in the 2026-07-15 architecture review.

North star: **branch is truth, worktree is a disposable cache; no infra fault is a resting
terminal state; one lifecycle engine, four roles are plugins; notifications are a tracked
step, not a fire-and-forget side effect.**

## Collaboration model (decided 2026-07-15 with the owner + boss-aligned) — FIRST PRINCIPLE for the whole HITL/notify layer

**Plane + GitHub are the system of record. Feishu is a thin, ONE-WAY, targeted delivery
channel — a doorbell, never the meeting room.** Collaboration does NOT live in a chat group
(groups get flooded → people mute → HITL silently dies, exactly the 0-cards failure of the
07-15 batch).

- **Decisions & artifacts live in the system of record**: spec / approval / product-answer →
  Plane (pages + work-item state + comments); code / review / acceptance → GitHub (PR + review).
- **The human ACTS in the system of record, not in chat.** Approve a spec = comment on the
  Plane page; answer an ambiguity = a Plane/GitHub comment; review = on the PR. Feishu never
  captures the decision.
- **Feishu = one-way push only. Bidirectional is DEPRECATED.** Delete the inbound machinery
  (looper-feishu.nexu.space CF inbox poll, humanInbox thread-reply mirror, detectHumanAsk /
  awaiting_human-resumed-by-thread-reply). This is a big complexity + fragility win (it's the
  infra that broke on the 07-15 network change).
- **Every card is a targeted, one-owner nudge with DEEP LINKS that jump straight to the exact
  spot to act** — not "here's the PR" but the specific NEW review comment / the specific Plane
  page section / the exact unresolved thread. Clicking the link lands the human on the precise
  place they need to respond. Card = @named-owner + one thing to do + a deep link.
- **The "two-way" doesn't vanish — it RELOCATES**: the human acts in Plane/GitHub, and the
  agent WATCHES the system of record for that action via the blocked-condition reconciler
  (P1-B2) — {product-spec appeared, review updated, comment answered, approval given}. Notify
  one-way → poll the source of truth for the response.
- **Observability = a queryable dashboard (loop × Plane × PR state), not chat scrollback.**
  Cards are for ACTIONS; the dashboard is for "what's happening." (07-15 pain: visibility was
  wrongly pinned on group cards.)
- **Mid-run interrupt ("@bot stop") moves off chat** → `looper` CLI / dashboard control
  (cleaner than a chat @-mention).

Env: isolated worktree `/Users/elian/Documents/looper-refactor` @ `refactor/lifecycle-engine`.
Test in the `~/.looper-test` sandbox (never production) before any prod deploy. Production
keeps running the current binary off `feat/looper-auto-flowchart-runtime`.

## Root causes (from the review)
- RC1 no single authoritative loop state (loop.Status × run.Status × queue × durable markers, hand-synced, already drifted)
- RC2 no "recoverable infra fault" class → every fault is blind-infinite-retry (transient) or park-forever (manual_intervention); no bridge back from paused
- RC3 worktree unbounded + disk-blind; GC time-based and protects the wrong trees (paused/failed); recovery treats worktree object (not branch) as truth
- RC4 notifications fire-and-forget → HITL surface silently no-ops (0 cards)
- RC5 four role runners re-implement one lifecycle (~10-12k accidental lines); intended `infra/worktree` pkg is an empty stub
- RC6 no per-loop concurrency owner → correctness leans on restart timing + optimistic SQL + PID heuristics

## Phased execution (stability first, then the big simplification)

### P0 — worktree becomes a recreatable cache  (days)  [kills #1 disk-leak-stall, #3 stale-death]
- [x] **A1. Recreate-on-missing.** ✅ done+tested `recoverWorkerWorktree` (worker/runner.go:2034): when RestoreWorktree finds no registered worktree, fall through to `CreateWorktree(branch)` (branch is durable — committed locally/pushed) instead of `staleWorkerWorktreeError`→manual_intervention. Only truly-unresolvable branch → NeedsHuman.
- [x] **A2. Disk-aware backpressure.** ✅ done+tested (commit 77835fe). New `internal/infra/disk` (statfs, df-style used%, walks up to nearest existing ancestor, unix build-tag + fail-open stub). `daemon.diskBackpressure` config (enabled/path/high 85%/hard 93%, default on). Scheduler claim gate `diskBackpressureClamp` (scheduler.go, in `executeClaimPhase`): at/above high watermark clamps this phase's available slots to 0 — governs NEW claims only, in-flight loops untouched. Fails OPEN (disabled/unsupported/unreadable never wedges scheduler). High => warn, hard => error (emergency). `diskUsageStat` seam → deterministic tests (7 clamp cases + disk pkg). Also greened the config suite (defaults+parity fixtures for 19c4183's worktreeCleanup change, left red).
- [x] **A3. Unify the worktree-pin predicate; resting loops stop leaking.** ✅ done+tested (commit 7d8138a). The two gates had drifted (planner pinned failed+interrupted-not-human_takeover; runtime executor pinned human_takeover-not-failed+interrupted) → between them every resting worktree was pinned (RC3 leak). Collapsed into one source of truth `domain.StatusPinsWorktree`: pins ONLY running/queued/shepherding/human_takeover (actively-in-use); paused/waiting/failed/interrupted/awaiting_human/idle + terminal are reclaimable (branch is truth; worker recreates via A1, reviewer/fixer do a fresh CreateWorktree each pass — verified). 1-day retention grace still shields a recently-used worktree. Deleted both drifted copies; contract test + updated worktreecleanup tests. **Reclaim-on-rest = hourly GC + retention once the pin releases**; immediate evict-on-terminal-transition deferred to P3 (engine owns transitions) rather than bolted onto 4 runners.

### P0.5 — worker resume is PR-aware  [kills the "PR already exists" re-run death]
- [x] **A4. Persist + adopt the existing PR before worktree recovery.** ✅ done+tested (commit 53a2731). At `open-pr`, GitHub is checked before touching the disposable worktree; branch candidates include the planned, recorded worktree, and lifecycle branches. An existing PR is atomically written into loop+queue first, then the run advances into shepherding without recreating/pushing the worktree or invoking the agent. A failed PR-existence lookup now fails closed (retryable) instead of blindly attempting `pr create`; a newly-created PR is likewise persisted before reviewer assignment. Covers the real 710/787 failure shape (`prNumber` absent, PR 5469/5543 already open, worktree missing), plus the ambiguous "create returned already exists but remote PR exists" window. Daemon E2E (API → queue → resume → fake GitHub → durable PR → shepherding) and all repo tests/build/vet pass; sandbox artifacts are under `~/.looper-test/a4-e2e-artifacts`.

### P1 — failure taxonomy + blocked-condition reconciler  (1-2 wks)  [kills #2, hardens #1/#3]
- [x] **B1. Add `RecoverableInfra` class and resume autonomously.** ✅ done+tested (commit 1ead9d0). `failureclass` now distinguishes resource exhaustion (ENOSPC/quota/fd/memory/process pressure), disposable local paths/worktrees, and fork/exec disappearance from network transients and human-required failures; all four role adapters preserve the class and keep it retryable. SQLite migration 0021 durably accepts/queries the new kind (the daemon E2E caught the old CHECK constraint otherwise degrading it to `manual_intervention`). Isolated daemon E2E removes the configured fake-agent executable, observes queue+loop remain queued with `recoverable_infra`, restores it, then observes the same loop resume to success. Full test/build/vet/gofmt pass; artifacts are under `~/.looper-test/b1-e2e-artifacts`.
- [x] **B2. Generalize blocked waits into a named condition-reconciler registry.** ✅ done+tested (commit 59a3337). Added durable `blockedCondition` metadata and registry entries for `product_spec`, `disk_recovered`, `ci_settled`, `review_updated`, and `human_answered`; planner product gates, worker HITL waits, and resting shepherd PRs now write explicit conditions, while legacy boolean/HITL markers are migrated on observation. Clearing a condition atomically restores the loop to `queued`, recreates its queue item behind a one-second claim safety window, clears the marker, and emits `loop.condition.cleared`. This also fixes the old product-spec reconciler bug that created a queue item while leaving the loop `paused` (the scheduler then discarded it as parked). Boot reconciliation covers answers/specs that arrive while looperd is down. Isolated daemon E2E proves an answered source-of-truth hold resumes on restart; artifacts are under `~/.looper-test/b2-e2e-artifacts`. Full test/build/vet/gofmt pass.
- [x] **B3. Attempt budget escalates instead of silencing.** ✅ done+tested (commit 271ecc6). Added a configurable one-hour wall-clock budget for `recoverable_infra` retries while preserving `-1` as the queue's attempt policy. Before a retry reaches any role runner, a cheap condition gate checks disk/resource pressure, configured agent availability, missing repositories, and disposable worktree/git-local failures; a failed check requeues without spending an attempt or spawning an agent, with the next check capped by the remaining budget. Exhaustion parks the loop on the durable `infra_recovered` condition, leaves the queue in explicit manual intervention, emits `loop.blocked.infra`, and degrades both health and status scheduler signals with a queryable blocked-infra count. Isolated daemon E2E proves a missing agent starts exactly one run, never respawns during the hold, and becomes visible as unhealthy after budget exhaustion; artifacts are under `~/.looper-test/b3-e2e-artifacts`. Full test/build/vet/gofmt pass.

### P2 — Feishu becomes a one-way, deep-linked delivery channel (was "notification outbox")  [kills #4 invisible-work, #5 loop_id leak; enacts the collaboration model]
Reframed by the 2026-07-15 collaboration decision: Feishu is push-only; humans act in Plane/GitHub.
- [x] **C0. DELETE the inbound/bidirectional machinery.** ✅ done+tested (commit e12d9c3). Removed the Cloudflare inbox worker and deployment secrets, daemon Feishu poll lane, `/api/v1/hitl/feishu` callback/card-action/thread-reply receiver, reverse thread routing, `humanInbox`, chat follow-up reactivation/interrupt bridges, resolved-card patch state, and obsolete bidirectional docs/config. Feishu answer transport now fails config validation; only GitHub source polling and the authenticated operator `/respond` fallback can deliver a decision. The ask-sentinel detector was renamed around its remaining source-of-truth role. Isolated daemon E2E proves the old callback is absent (404), while deterministic E2E harness config prevents host disk utilization from spuriously blocking claims; artifacts are under `~/.looper-test/c0-e2e-artifacts`. Relevant package/full internal tests, vet, build, and gofmt pass.
- [x] **C1. Every card carries DEEP LINKS that jump to the exact spot to act.** ✅ done+tested (commit b79222c). Decision notifications now have one named owner, one outbound action, no callback values, and an exact source URL. GitHub HITL cards target the newly-created issue comment; Plane product-spec, product-decision, and final REVIEW requests target the newly-created work-item/page comment (`#comment-…`). Product decisions are now durable Plane-source holds: the reconciler ignores old comments, records the first later human reply, requeues the planner, and native-resumes the same authoring session with the answer. Feishu delivery is best-effort and never owns the state. Full internal tests, vet, build, and gofmt pass.
- [x] **C2. Delivery reliability + observability.** ✅ done+tested (commit 5be2991). Action-card intents are persisted as `pending` before any Feishu call, every failed attempt emits a durable `notification.delivery_failed` event, scheduler ticks retry with exponential backoff, and the fifth failure becomes terminal `failed`; successful delivery becomes `delivered`. Decision delivery now creates a missing thread root instead of silently no-oping, including coordinator intake loops that intentionally have no run. A deterministic failure/retry test proves the intent survives, delivers, and creates the run-less anchor.
- [x] **C3. Never expose raw loop ids in cards.** ✅ done+tested (commit 5be2991). The unresolved-loop header fallback is now generic human text; a regression test rejects internal-id leakage.
- [x] **C4. Mid-run control is CLI/API-owned.** ✅ done+tested (commit 5be2991; inbound chat removed in C0). `looper loop stop <id|seq>` now exposes the authenticated active-run interrupt next to pause/start/retry; existing top-level stop and dashboard APIs remain compatible. CLI-to-control-API coverage proves the path.
- [x] **C5. Queryable loop × Plane × PR observability.** ✅ done+tested (commit 5be2991). `GET /api/v1/loops?include=relationships` returns normalized title/source, exact Plane/action URL, PR URL, and blocked condition without forcing dashboard clients to reverse-engineer metadata JSON; the default API contract remains unchanged.

### P3 — one lifecycle engine, roles as plugins  (weeks)  [kills #6, makes P0-P2 single-copy]
- [x] **D1. Generic lifecycle engine + role plugins.** ✅ done+tested (commits 6aeef7c, 9a01f03). `internal/loops/engine` owns ordered-step iteration, checkpoint resume, step boundaries, failure classification, blocked persistence, done persistence, and optional leases. Planner/reviewer/fixer/worker queue dispatches are now role plugins executed through this engine; their mature internal step/checkpoint implementations remain the role-owned payload instead of being duplicated in runtime. Fake plugins prove resume, block, boundary classification, and completion semantics.
- [x] **D2. Four authoritative lifecycle phases.** ✅ done+tested (commit 6aeef7c). Durable `lifecycleState` records only `running`, `blocked(condition)`, `done(outcome)`, or `dead(reason)`. Loop creation and control transitions write it, role reconciliation converges it before/after every run, blocked-condition resume updates it atomically, scheduler parking and the relationship dashboard consume it first. The legacy 13-value `status` column remains a compatibility projection for old API/SQLite clients, not a second UI state model.
- [x] **D3. Single-owner reconcile lease.** ✅ done+tested (commit 6aeef7c). Queue roles and the blocked-condition reconciler compete on the same durable `lifecycle:<loop>` SQLite lease; duplicate claimed rows retire without acting. Release is owner-qualified so an expired old owner cannot delete a successor's lease. Engine and daemon E2E coverage prove exclusion, release, restart, and idempotent condition resume.
- [x] **D4. Single failure/result adaptation layer.** ✅ done+tested (commit 6aeef7c). The four role-local `QueueFailureKind` definitions are aliases of the one `failureclass.Kind` (including HITL interrupt), and four agent-result execution DTO adapters collapsed into one typed converter. Provider routers remain deliberately at the composition boundary because GitHub, Forgejo, and Plane have different source semantics; they no longer define lifecycle/failure policy. Full role/runtime tests, vet, build, and gofmt pass.

## Rule: every change ships behind a build + the relevant package tests, smoke-tested in ~/.looper-test, never deployed to prod until the whole phase is validated.
