# Issue #269 — Narrow `paused` Semantics and Restore Safe Autonomous Re-entry

## 1. Background

Looper currently uses three concepts too loosely and too interchangeably:

- queue failure kind: `manual_intervention`
- checkpoint resume policy: `manual_intervention`
- loop status: `paused`

In practice, many non-terminal or still-safe situations eventually land in `paused`, and `paused` currently means “removed from autonomous recovery”. That is too strong for transient infra failures, policy blockers, validation failures, stale checkpoint mismatches, or missing context that could safely re-enter later.

The result is a product mismatch:

> Looper is supposed to minimize human intervention, but today many blocked-but-safe loops become effectively invisible to autonomous recovery.

This spec defines a minimal implementation path that fixes that behavior without first exploding the lifecycle model into many new persisted statuses.

---

## 2. Problem summary

Today the main suppression bit is not merely the string `manual_intervention`.

The real problem is that many code paths map a blocked or ambiguous outcome to:

- `ResumePolicy = "manual_intervention"`
- `Loop.Status = "paused"`

and the runtime then treats `paused` as a hard exclusion from:

- requeue
- rediscovery
- reviewer auto-recovery

This conflates several different situations:

1. explicit human/operator hold
2. unsafe repository or worktree state
3. transient environment or infrastructure failure
4. policy/config blockers
5. stale checkpoint / workflow mismatch that should rediscover
6. validation failure during autonomous execution

Those categories should not share the same scheduling semantics.

---

## 3. Goals

### 3.1 Primary goals

1. Make `paused` mean only:
   - explicit operator hold, or
   - unsafe / ambiguous autonomous continuation
2. Stop using `paused` as the default sink for blocked-but-safe states.
3. Give every stopped loop an explicit re-entry mode.
4. Preserve anti-thrash behavior when making more states autonomous.

### 3.2 Non-goals

1. Do not introduce a large new persisted loop-status matrix in this change.
2. Do not auto-unpause all historical paused loops blindly.
3. Do not broaden autonomy for unsafe repository states.

---

## 4. Design principles

### 4.1 Keep lifecycle status narrow

Loop lifecycle status should remain narrow:

- `queued`
- `running`
- `failed`
- `completed`
- `paused`

The nuance should live primarily in failure classification, resume policy, and scheduler behavior.

### 4.2 Separate “blocked” from “hard hold”

`manual_intervention` should no longer automatically imply `paused`.

Instead, Looper must decide independently:

1. is this unsafe enough to pause?
2. if not paused, how does it re-enter?
3. what retry/backoff limits apply?

### 4.3 No broadened autonomy without re-entry semantics

Any scenario moved out of `paused` must define one of these re-entry modes:

- `advance_from_checkpoint`
- `replay_step`
- `restart_from_discover`
- queued retry after backoff (`retryable_transient` / `retryable_after_resume`)
- explicit human-only hold (`manual_intervention`)

---

## 5. Classification model

## 5.1 States that should still end in `paused`

These remain hard holds because autonomous continuation is unsafe or intentionally human-gated:

1. explicit operator pause
2. unsafe / dirty / ambiguous worktree state
3. high-risk conflict states intentionally gated on manual approval
4. any case where the current repo/worktree state cannot safely be retried or rediscovered automatically

## 5.2 States that should become retryable or rediscoverable instead

These should remain visible to autonomous recovery:

1. transient agent setup / environment / network failures
2. GitHub / CLI / tooling failures that are transient rather than fundamentally unsafe
3. policy blockers that are safe but external-action-dependent
4. validation failures, after bounded retry semantics
5. stale checkpoint mismatches that should restart from discovery
6. fixer no-op resolve-comments paths where the code state is safe but the current run cannot prove push evidence

---

## 6. Proposed implementation shape

## 6.1 Shared decision helper at the runner layer

Introduce a small shared helper package for scheduling semantics rather than duplicating ad hoc `if failure == manual_intervention { paused }` logic in each runner.

The helper should answer:

1. is this a true hard hold?
2. should autonomous recovery be suppressed?
3. should the loop be paused after this failure?
4. should the loop become `queued` again?
5. should the loop become `failed` but remain rediscoverable later?
6. which resume policy / re-entry mode applies?

It does **not** need to persist a new entity type. It can be a small policy function over:

- failure kind
- queue terminal/retry outcome
- explicit resume policy
- optional reason category

Recommended shape:

- package-local shared helper under `internal/loops` or another low-dependency internal package
- explicit helpers such as:
  - `IsHardHold(...)`
  - `SuppressesAutonomousRecovery(...)`
  - `ShouldPauseLoopAfterFailure(...)`
  - `ShouldRestartFromDiscover(...)`
  - `IsManualHoldResumePolicy(...)`
  - `DefaultResumePolicy(...)`

The exact name is less important than centralizing the rule.

This policy must be reused by all places that currently infer semantics indirectly, including:

- runner failure handling
- `createRunContext` / resume gating
- rediscovery filters
- runtime recovery / auto-recovery gates
- user-facing status/message generation where pause semantics are surfaced

## 6.2 Resume-policy meanings

Standardize the semantic meaning of these resume policies:

- `manual_intervention`
  - human-only hold
  - should suppress autonomous resume/rediscovery
- `restart_from_discover`
  - safe to re-enter, but not by replaying stale checkpoint state
- `advance_from_checkpoint`
  - safe to continue from later step after queue retry
- `replay_step`
  - safe to rerun the current step
- `retry_from_timeout_context`
  - safe bounded retry from stored timeout context

`manual_intervention` must no longer be the default label for every non-successful blocked state.

## 6.3 Re-entry mode and re-entry trigger are both required

It is not enough to mark a state as merely “rediscoverable later”.

Every blocked-but-non-paused state must declare both:

1. **re-entry mode**
2. **re-entry trigger / source**

Allowed trigger categories include:

- queue retry after backoff
- PR/head SHA or fix-items hash change
- fresh discovery pass
- explicit human resume action
- timeout-context retry
- config / policy change

If a scenario cannot name a concrete re-entry trigger, it must not be treated as a safe autonomous state yet.

---

## 7. Role-by-role plan

## 7.1 Fixer

Fixer is the first implementation lane because it already exposes the product mismatch clearly.

Required semantics:

### Stay autonomous

- agent setup failures → `retryable_transient`
- no-new-commit `resolve-comments` path → `retryable_after_resume` + `restart_from_discover`
- any safe checkpoint mismatch that should rediscover rather than pause

### Stay paused

- dirty worktree / unsafe repo state
- risky conflict states requiring explicit human approval
- auto-commit-disabled cases only when the repo is left in a genuinely unsafe/manual state

Current Phase 2 implementation decision for `allowAutoCommit=false`:

- fixer has no separate safe policy-blocked `allowAutoCommit=false` path today
- `allowAutoCommit` is only consulted when reconcile detects uncommitted changes
- that case is treated as an unsafe/manual hold because Looper cannot safely infer whether to commit, amend, discard, or wait for human inspection
- any future safe `allowAutoCommit=false` semantics require an explicit autonomous re-entry model and are deferred beyond fixer Phase 2

### Policy blockers

`allowAutoPush=false` should not disappear into a hidden permanent pause.

Recommended v1 treatment:

- classify as human-action-required but safe
- keep the loop visible for rediscovery only when there is a meaningful state change trigger
- if that state-change gate is not yet implemented, explicitly treat it as a temporary hard-hold exception and document that exception clearly

Current Phase 2 implementation decision:

- `allowAutoPush=false` remains a temporary hard-hold exception in v1
- the fixer run now fails with `manual_intervention` and leaves the loop `paused` rather than silently completing as skipped
- this exception should be revisited once there is a concrete autonomous re-entry trigger for policy changes or manual push completion

Safe policy blockers must **not** keep using `manual_intervention` as a compatibility label while still being considered autonomous. If a v1 path still uses `manual_intervention`, it must be treated as a true hard hold and excluded from the “issue solved” set for that scenario.

## 7.2 Worker

Worker needs the same split, but validation is especially important.

### Move away from permanent pause

- validation failure should no longer default to permanent pause
- safe validation failures should get bounded retry semantics and then either:
  - retry from checkpoint, or
  - restart from a safe rediscovery path, or
  - settle into a visible blocked state that does not hide the loop forever

Validation failures must be split at least into:

- transient / tooling / transport validation failures
- stale checkpoint / stale repo-context failures
- unsafe repo ambiguity failures
- deterministic policy / spec / content validation failures

These categories should not all map to the same resume policy or loop status.

Current implementation slice:

- transient worker agent setup failures are retryable rather than hard-held
- generic validation failures no longer default to `manual_intervention`
- transient/tooling-style validation failures now stay autonomous with retry semantics
- stale worker validation failures now restart from discovery rather than pausing
- unsafe repo-ambiguity validation failures still hard-hold and pause
- safe policy-blocker refinements are still pending follow-up work

### Still pause

- stale/inconsistent repo state that is unsafe to continue
- genuinely ambiguous repo/worktree states

### Reclassify

- auto-push / manual-PR / safe policy blockers away from “unsafe pause” semantics
- transient GitHub/tooling/setup failures to retryable states

Current implementation slice for worker policy blockers:

- `openPRStrategy=manual` is treated as an intentional terminal handoff, not a blocked autonomous failure
- worker `allowAutoPush=false` remains a temporary v1 hard-hold exception
- worker GitHub CLI unavailable remains a temporary v1 hard-hold exception
- the `allowAutoPush=false` / missing-CLI exceptions should only be relaxed once there is a concrete trigger model for config/tool/manual-completion changes that does not cause rediscovery thrash

For each worker blocker moved out of `paused`, the implementation must define:

- expected loop status
- expected failure kind
- expected resume policy
- expected re-entry trigger
- expected user-facing summary/message

## 7.3 Planner

Planner should follow the same rule:

- transient agent/setup/env failures stay autonomous
- human-gated unsafe or explicit manual states stay paused
- `manual_intervention` should only mean actual hard hold

Planner `createRunContext` should only suppress automatic continuation when the prior run truly represents a hard hold, not just any blocked state.

Current implementation slice:

- transient planner agent setup failures now stay retryable
- planner pause/recovery updates now use the shared hard-hold semantics instead of ad hoc `manual_intervention` checks
- planner `allowAutoPush=false` remains a temporary v1 hard-hold exception until there is a concrete non-thrashing re-entry trigger

## 7.4 Reviewer / runtime auto-recovery

Reviewer auto-recovery in runtime should continue to refuse:

- explicit `manual_intervention` resume policy
- queue items explicitly marked `manual_intervention`

But reviewer states that are safe and retryable should not be funneled into those markers by default.

Runtime `paused` semantics remain strict:

- `paused` loops should still not auto-requeue
- the fix is to make fewer states become `paused`, not to redefine paused as soft-blocked

Reviewer-side logic must also be audited for over-broad suppression outside loop status, including:

- reviewer runner resume gating
- reviewer follow-up loop selection
- runtime reviewer auto-recovery filtering

No safe blocked state should remain effectively treated as a hard hold just because one of these secondary gates still assumes `manual_intervention` means “no autonomy forever”.

Current implementation slice:

- runtime reviewer auto-recovery now uses the shared loop policy helper to determine whether a failed reviewer loop is a true hard hold
- reviewer failed-loop recovery gating now normalizes resume policy before deciding whether autonomy is suppressed
- reviewer failure finalization now uses the shared pause helper, so only true hard holds and cancelled queue items settle into `paused`
- runtime `shouldRequeueLoop(...)` remains strict: paused loops are still excluded from requeue, and the fix is entirely upstream classification rather than redefining `paused`
- reviewer follow-up discovery still intentionally excludes failed loops, because failed reviewer loops recover through the dedicated failed-loop recovery path rather than generic follow-up rediscovery

## 7.5 Cancellation semantics

Current code sometimes treats cancelled queue items as pause-adjacent, but this issue needs an explicit rule.

The implementation must define when cancellation means:

1. explicit operator/human stop that should imply `paused`
2. terminal stop without pause
3. retry suppression without redefining the loop as unsafe

Cancellation semantics should not remain an implicit side effect of existing queue-state checks.

Current implementation decision:

- cancelling active queue items as part of an explicit operator/human stop remains pause-adjacent and leaves the loop `paused`
- terminal non-paused stops continue to use the existing lifecycle states (`completed`, `failed`, `terminated`) rather than queue cancellation
- retry suppression without unsafe-pause semantics continues to use non-paused failed states plus retry-budget/whitelist gating, not queue cancellation

---

## 8. Anti-thrash requirements

Making pause rarer increases the risk of hot loops. The implementation must preserve or tighten these controls:

1. queue retry budgets (`MaxAttempts`)
2. exponential backoff for retryable queue failures
3. rediscovery only when the role’s existing dedupe / head SHA / fix-items hash model can prevent immediate replay storms
4. no unconditional “failed instantly rediscover forever” behavior

For this issue, existing queue retries and dedupe are acceptable as the minimal baseline, but any newly rediscoverable path must have a concrete state-change boundary.

---

## 9. Migration and backward compatibility

This issue should not auto-release all existing paused loops.

Minimal safe approach:

1. change new writes first
2. leave historical paused loops untouched unless a later targeted reconciliation pass is added
3. document that previously paused loops remain paused under old semantics

If a reconciliation pass is added later, it must distinguish:

- unsafe historical pauses that must remain paused
- safe historical pauses that can be reintroduced into discovery/recovery

Current implementation decision:

- this issue changes only new writes and new recovery decisions
- no automatic reconciliation pass is added in this issue
- any later reconciliation pass must distinguish explicit/manual or unsafe historical pauses from safe historical pauses using persisted failure kind, resume policy, and contextual loop metadata rather than blindly unpausing all paused loops

---

## 10. Current branch state

The current branch has already started the fixer-first slice:

- fixer agent setup failures were reclassified to stay retryable
- fixer no-new-commit resolve-comments path was changed to `restart_from_discover`

The rest of this spec describes the full intended issue-level completion, not only the changes already landed in the branch.

---

## 11. Acceptance criteria

The issue is complete when all of the following are true:

1. `paused` is only used for explicit operator holds or unsafe autonomous continuation.
2. blocked-but-safe states are not permanently hidden from autonomous recovery by default.
3. every non-paused blocked state has an explicit re-entry mode.
4. fixer no-op resolve-comments paths no longer end in permanent paused semantics.
5. worker validation failure no longer defaults to permanent pause.
6. transient setup / environment failures do not map to permanent pause across roles.
7. runtime still refuses to auto-recover genuinely paused loops.
8. retry/backoff behavior prevents immediate hot-loop regressions.
9. tests clearly distinguish unsafe pauses from safe blocked/retryable states.
10. no safe blocked state is treated as a hard hold in any of:
   - loop status
   - resume gating
   - runtime/reviewer recovery gating
   - user-facing status or messaging

---

## 12. Recommended implementation order

1. Land and stabilize the fixer-first slice.
2. Introduce shared helper(s) for pause / re-entry policy.
3. Apply the shared policy to worker, especially validation and policy blockers.
4. Apply the same split to planner.
5. Tighten runtime reviewer auto-recovery tests around true hard holds vs safe retryable states.
6. Add or document follow-up migration logic only after new-write semantics are stable.
