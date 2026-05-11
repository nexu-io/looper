# Issue #269 — Pause Semantics Checklist

## Phase 0 - Lock the semantics

- [x] Document `paused` as “explicit operator hold or unsafe autonomous continuation only”
- [x] Document that `manual_intervention` does not automatically imply discovery suppression
- [ ] Document the allowed re-entry modes:
  - [x] `advance_from_checkpoint`
  - [x] `replay_step`
  - [x] `restart_from_discover`
  - [x] queue retry via retryable failure kinds
- [x] Confirm that this issue does **not** introduce a large new persisted loop-status matrix
- [x] Confirm that historical paused loops are not auto-unpaused in this change

## Phase 1 - Shared policy helpers

- [x] Introduce shared helper(s) for “should this failure pause the loop?”
- [x] Introduce shared helper(s) for “should this state restart from discover?”
- [x] Introduce shared helper(s) for “is this a true hard hold?”
- [x] Introduce shared helper(s) for “should autonomous recovery be suppressed?”
- [x] Introduce shared helper(s) for default resume-policy normalization
- [x] Centralize the meaning of `manual_intervention` vs `restart_from_discover`
- [x] Remove duplicated ad hoc `FailureManualIntervention => paused` logic where practical
- [x] Ensure helper behavior can inspect queue outcome + resume policy together
- [x] Audit all `createRunContext` / resume-gating logic for `manual_intervention`
- [ ] Audit all rediscovery/recovery filters that exclude loops by `paused`, `failed`, or `manual_intervention`
- [x] Centralize hard-hold vs safe-blocked semantics in one shared policy path

## Phase 2 - Fixer

- [x] Keep fixer agent setup failures retryable rather than permanently paused
- [x] Keep fixer no-new-commit `resolve-comments` path rediscoverable
- [x] Preserve explicit `restart_from_discover` semantics through run failure handling
- [x] Keep dirty worktree / unsafe repo states paused
- [x] Keep risky conflict gates paused when explicit human approval is required
- [x] Clarify fixer `allowAutoCommit=false` semantics:
  - [x] no separate safe policy-blocked state exists in current fixer behavior
  - [x] genuinely unsafe manual-hold states stay paused
- [x] Decide v1 semantics for `allowAutoPush=false`
- [x] If `allowAutoPush=false` remains a hard hold in v1, document it as an explicit temporary exception
- [x] For the current fixer v1 policy-blocker exception (`allowAutoPush=false`), specify:
  - [x] expected loop status
  - [x] expected failure kind
  - [x] expected resume policy
  - [x] expected re-entry trigger
  - [x] expected user-facing summary/message
- [x] Ensure fixer discover still skips genuinely paused loops
- [x] Add/update fixer tests for:
  - [x] agent setup failure
  - [x] no-new-commit resolve-comments path
  - [x] dirty worktree unsafe hold
  - [x] risky conflict gate
  - [x] auto-commit-disabled semantics are documented as unsafe/manual only
  - [x] auto-push-disabled behavior

## Phase 3 - Worker

- [x] Audit all worker `FailureManualIntervention` mappings
- [x] Reclassify transient setup / env / tooling failures to retryable states
- [x] Reclassify worker validation failure away from default permanent pause
- [x] Add bounded autonomous retry / resume semantics for validation failures
- [ ] Split worker validation failures into:
  - [x] transient/tooling/transport
  - [x] stale checkpoint / stale repo-context
  - [x] unsafe repo ambiguity
  - [x] deterministic policy/spec/content
- [ ] Define for each worker validation subtype:
  - [x] retry from checkpoint vs replay step vs restart from discover vs hard hold
  - [x] re-entry trigger
- [x] Reclassify safe policy blockers (`auto-push`, manual PR creation, external action required) away from unsafe pause semantics
- [x] Decide v1 semantics for manual PR opening mode
- [ ] For each worker safe policy blocker, specify:
  - [x] hard hold vs safe blocked
  - [x] resume policy
  - [x] recovery trigger
  - [x] expected loop status
  - [x] expected user-facing summary/message
- [x] Keep truly unsafe repo/worktree states paused
- [ ] Add/update worker tests for:
  - [x] validation failure no longer permanently pausing by default
  - [x] retryable transient setup failure
  - [ ] safe policy blocker behavior
  - [x] unsafe repo-state hold
  - [x] stale validation rediscovery behavior

## Phase 4 - Planner

- [x] Audit planner `manual_intervention` / paused mappings
- [x] Reclassify planner transient setup / env failures to retryable states
- [x] Keep explicit human-gated unsafe states paused
- [x] Ensure planner `createRunContext` only treats true hard holds as non-autonomous
- [ ] For planner safe blocked states, specify:
  - [x] expected loop status
  - [x] expected failure kind
  - [x] expected resume policy
  - [x] expected re-entry trigger
- [ ] Add/update planner tests for:
  - [x] transient agent/setup failure
  - [ ] hard manual hold behavior
  - [x] safe re-entry behavior after non-paused failure

## Phase 5 - Runtime / reviewer recovery

- [ ] Keep `paused` loops excluded from runtime requeue
- [ ] Keep reviewer auto-recovery blocked on true hard holds (`manual_intervention` policy/kind)
- [ ] Ensure reviewer/runtime paths do not depend on over-broad upstream use of `manual_intervention`
- [ ] Audit reviewer follow-up loop selection for over-broad exclusion of failed loops
- [ ] Audit reviewer recovery gating for `manual_intervention` assumptions
- [ ] Add reviewer tests distinguishing hard hold vs safe retryable blocked states
- [ ] Add/update runtime tests for:
  - [ ] paused loops never requeue
  - [ ] reviewer hard-hold states do not auto-recover
  - [ ] safe retryable reviewer states can still auto-recover when eligible

## Phase 6 - Anti-thrash and recovery safety

- [ ] Confirm retry budgets still bound newly retryable states
- [ ] Confirm exponential backoff still applies where queue retries are used
- [ ] Confirm rediscoverable states have a concrete state-change boundary or dedupe guard
- [ ] Verify no hot-loop regression in the newly rediscoverable fixer/worker/planner paths

## Phase 7 - Migration and follow-up policy

- [ ] Confirm this issue only changes new-write semantics by default
- [ ] Define cancellation semantics:
  - [ ] operator/human stop implying `paused`
  - [ ] terminal stop without pause
  - [ ] retry suppression without unsafe-pause semantics
- [ ] Decide whether a separate reconciliation pass is needed for old paused loops
- [ ] If reconciliation is added later, define how to distinguish safe old pauses from unsafe old pauses

## Phase 8 - Verification

- [ ] Verify CLI/API/user-facing outputs do not label safe blocked states as “paused”
- [ ] Verify notifications/comments distinguish hard hold vs retryable/safe blocked failures
- [x] Run fixer tests
- [x] Run worker tests
- [x] Run planner tests
- [x] Run runtime tests
- [x] Run full `go test ./...`
- [x] Verify any unrelated pre-existing failures separately from issue-269 changes
