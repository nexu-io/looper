# Runner retry recovery: boundary-aware transient classification, explicit manual retry, and safe worktree handling

Issue: TBD
Base branch: `main`

## Scope

This spec covers retry classification and recovery UX for **all four loop runners**:

- `internal/reviewer/runner.go`
- `internal/worker/runner.go`
- `internal/fixer/runner.go`
- `internal/planner/runner.go`

It is **not** reviewer-only. Reviewer is the most exposed case (most remote
git/GitHub touches, lowest tolerance for unstable transport), but the underlying
classifier weakness is shared by every runner.

## Problem

All four runners share the same final fallback in their `classifyFailure`
helper: any error that is not already a typed `loopError`, not a
`context.Canceled` / `DeadlineExceeded`, not `githubinfra.IsTransientError`, and
not a runner-specific narrow rule, lands as `FailureNonRetryable`.

Concretely today:

- `reviewer.classifyFailureForProject` (`internal/reviewer/runner.go`) — has
  enhanced transient string matching gated by
  `enhancedTransientClassification`, but defaults to `non_retryable` when the
  feature flag is off (it is off by default).
- `worker.classifyFailure` (`internal/worker/runner.go`) — only
  `context.Canceled/DeadlineExceeded` and `githubinfra.IsTransientError` are
  retryable; everything else is `non_retryable`.
- `fixer.classifyFailure` (`internal/fixer/runner.go`) — only
  `remote head changed` (string match) and `githubinfra.IsTransientError` are
  treated as recoverable; **does not** retry `context.Canceled` /
  `DeadlineExceeded`; everything else is `non_retryable`.
- `planner.classifyFailure` (`internal/planner/runner.go`) — same shape as
  worker, plus a `transientFailure` interface check.

This causes remote-dependency failures such as `git ls-remote`, `git fetch`,
SSH disconnects, broken pipes, early EOFs, and similar transport failures to
sometimes land as terminal `failed` after a single attempt — across any runner
that touches the network, not just reviewer.

The result is an unsatisfying split for every runner:

- truly transient external failures are not retried often enough;
- truly deterministic local failures are hard-stopped (correct), but resuming
  after a human fix is too cumbersome;
- the system has no explicit concept of failure provenance, so message matching
  is doing too much work, and each runner reinvents a slightly different
  fragment of it.

`dirty worktree` is already treated as `manual_intervention` in
`reviewer`, `worker`, and `fixer` (planner has no worktree). What is missing
in all four is a first-class operator retry UX that makes the post-fix path
obvious.

## Goals

- Make retry behavior resilient for **unknown external boundary failures** by
  default, uniformly across all runners.
- Preserve current terminal loop and queue semantics: automatic retry must
  remain bounded and must still end in terminal `failed` after budget
  exhaustion.
- Preserve existing hard-stop behavior for **manual intervention** and
  **deterministic local failures**.
- Introduce an explicit failure-classification model based on
  **provenance / boundary**, shared across runners, not only on free-form
  message matching.
- Add a first-class operator retry flow so user-fixed deterministic failures
  can be resumed conveniently for any runner type.
- Make `manual_intervention` failures highly visible in operator tooling,
  especially `looper ps`, so they are not silently buried inside generic
  `paused` or `failed` output.
- Keep `dirty worktree` conservative by default for reviewer/worker/fixer,
  while still providing an explicit, convenient recovery path.
- Preserve dedupe, active-run, queue, and scheduler invariants.
- Fix the small fixer-specific gap where `context.Canceled` /
  `DeadlineExceeded` is not classified as transient.

## Non-goals

- Do not make any runner's loops non-terminal forever.
- Do not auto-delete dirty worktree state by default.
- Do not weaken queue-attempt budgets or retry ceilings.
- Do not silently convert config, schema, checkpoint, or storage bugs into
  endless retries.
- Do not redesign reviewer publish, worker validation, fixer reconcile, or
  planner spec semantics beyond what is required to support safe
  retry-after-resume.
- Do not unify the four runner state machines themselves; only the
  classification and recovery UX are unified.

## Proposed approach

### 1. Keep terminal semantics and retry budgets unchanged

This change does **not** replace terminal states with endless non-terminal
retries.

The following semantics remain authoritative for every runner:

- terminal loop statuses stay `terminated`, `failed`, `paused`, `stopped`;
- queue items remain bounded by `max_attempts`;
- automatic retry still requires remaining attempt budget;
- once retry budget is exhausted, the queue item and loop still reach terminal
  `failed`.

Relevant code today:

- `internal/reviewer/runner.go:isTerminalReviewerLoopStatus`,
  `terminalReviewerLoopReason`, `failQueueItem`
- `internal/runtime/runtime.go:shouldAutoRecoverFailedReviewerLoop`
- equivalents in worker / fixer / planner runners and their queue-failure paths

This spec changes **how failures are classified**, not whether terminal states
exist.

### 2. Introduce a shared boundary-aware failure classifier

The core design change is to stop treating all unknown errors the same, and to
share the classifier across runners.

Replace the implicit per-runner rule:

- `unknown error => non_retryable`

with an explicit shared rule:

- `unknown external transport/service boundary failure => retryable_transient`
- `unknown internal deterministic failure => non_retryable or manual_intervention`

This requires explicit provenance, not just free-form string matching, and the
classifier should be shared infrastructure rather than duplicated per runner.

Add a classifier package, for example `internal/loops/failureclass`, with a
shared model:

```go
package failureclass

type Boundary string

const (
    BoundaryGitRemote     Boundary = "git_remote"
    BoundaryGitLocal      Boundary = "git_local"
    BoundaryGitHubAPI     Boundary = "github_api"
    BoundaryModelProvider Boundary = "model_provider"
    BoundaryAgentProcess  Boundary = "agent_process"
    BoundaryLocalWorktree Boundary = "local_worktree"
    BoundaryStorage       Boundary = "storage"
    BoundaryConfig        Boundary = "config"
    BoundaryCheckpoint    Boundary = "checkpoint"
    BoundaryPolicy        Boundary = "policy"
    BoundaryUnknown       Boundary = "unknown"
)

type RunnerKind string

const (
    RunnerReviewer RunnerKind = "reviewer"
    RunnerWorker   RunnerKind = "worker"
    RunnerFixer    RunnerKind = "fixer"
    RunnerPlanner  RunnerKind = "planner"
)

type Context struct {
    Runner          RunnerKind
    Step            string // runner-specific step name, opaque to the classifier
    Boundary        Boundary
    SideEffectState string // none, pre_publish, post_publish_ambiguous, etc.
}
```

The classifier maps `(error, Context)` to one of the existing per-runner
`QueueFailureKind` values:

- `FailureRetryableTransient`
- `FailureRetryableAfterResume`
- `FailureNonRetryable`
- `FailureManualIntervention`

Each runner keeps its own `QueueFailureKind` constants but delegates to the
shared classifier for the unknown-error decision. Runner-specific narrow rules
(for example fixer's `remote head changed → retryable_after_resume`) stay in
the runner, but call into the shared classifier for the residual case.

### 3. Define the default retry policy by boundary

#### 3a. External boundary defaults to retryable transient

Unknown failures should default to `retryable_transient` when they originate
from external boundaries, regardless of runner:

- git remote operations:
  - `ls-remote`
  - `fetch`
  - `pull`
  - `push`
  - remote head checks
- GitHub API calls
- model/provider network calls
- agent process transport failures
- shell command transport failures that are clearly remote/network/service
  related

Examples include:

- SSH disconnects
- `closed by remote host`
- `broken pipe`
- `early EOF`
- `connection reset by peer`
- `context deadline exceeded`
- service-side timeouts and transient 5xx responses

These should consume bounded retry attempts automatically.

#### 3b. Deterministic external denials stay hard-stop

External does **not** mean retryable by default when the error is a known
deterministic denial, for example:

- GitHub auth/scope failures (`401`, `403`)
- missing resource / bad request (`404`, `422`) when clearly not transient
- branch or repo policy denials
- unsupported or invalid provider/model configuration

These remain `non_retryable` or `manual_intervention` depending on authority
and recovery path.

#### 3c. Internal deterministic failures stay hard-stop

The following remain explicit hard-stop classes for any runner:

- dirty worktree (reviewer, worker, fixer)
- invalid checkpoint / missing invariant state that should never be absent in a
  healthy run
- config validation failures
- schema/storage failures
- local repo corruption or unexpected git-local invariants
- policy / permission denials
- explicit manual-intervention cases already modeled in code

### 4. Keep dirty worktree as `manual_intervention` by default (reviewer / worker / fixer)

`dirty worktree` should remain `manual_intervention` by default in reviewer,
worker, and fixer. Planner has no worktree and is unaffected.

Reason:

- the runner checkpoint authorizes the expected branch / PR head;
- it does **not** authorize deletion of local dirty state;
- dirty files may be human edits, partial agent output, generated evidence, or
  corruption;
- `git status` cannot tell whether those files are disposable.

Therefore Looper must not silently `reset --hard` or remove untracked files in
the generic retry path of any runner.

Current behavior in:

- `reviewer.runPrepareWorktreeStep`
- `worker` worktree prepare paths (dirty branch worktree → manual_intervention)
- `fixer` worktree prepare and adopt paths (dirty worktree → manual_intervention)

is correct and should be preserved.

#### Optional narrow future automation

This spec permits a future, strictly narrower optimization for disposable
worktree cleanup, but **not** in the base path for this change.

Any future auto-clean subset would need all of the following:

- managed Looper worktree provenance;
- no active run using the worktree;
- no tracked modifications;
- only allowlisted disposable untracked paths;
- explicit persisted authority that the worktree is disposable.

Without those guarantees, default auto-clean is out of scope.

### 5. Make retry behavior step-aware per runner

The correct retry mode depends on both the boundary and the runner-specific
step. The classifier is shared; the step-to-boundary mapping lives in each
runner.

#### Reviewer

- discover / snapshot / thread resolution / review:
  - unknown external boundary failures default to `retryable_transient`;
  - deterministic local/config/policy failures remain hard-stop.
- worktree (`runPrepareWorktreeStep`):
  - remote transport failures and remote-head fetch problems become retryable;
  - remote-head changed remains `retryable_after_resume` / stale rediscovery as
    today;
  - local dirty or inconsistent worktree stays `manual_intervention`.
- publish:
  - if failure occurs before any side effect, normal transient classification
    applies;
  - if failure occurs after a side effect but before durable confirmation,
    classify as `retryable_after_resume` **only if** checkpoint/marker state is
    persisted sufficiently to make replay safe;
  - otherwise keep current conservative behavior.
  - This prevents duplicate review/comment side effects.

#### Worker

- claim / route / spec lookup steps:
  - `manual_intervention` outcomes from routing decisions remain hard-stop;
  - GitHub API transport failures classify via the shared classifier.
- worktree prepare:
  - dirty branch worktree → `manual_intervention` (unchanged);
  - stale worktree path → `manual_intervention` (unchanged);
  - remote transport failures → `retryable_transient` via shared classifier.
- agent / validation:
  - validation hints already escalate dirty worktree / merge conflict to
    `manual_intervention` (see `containsAnyValidationHint`); preserve.
  - transport-level agent failures → `retryable_transient`.
- push / open PR:
  - transport failures → `retryable_transient`;
  - GitHub auth/scope/policy denials → `non_retryable` or
    `manual_intervention`.

#### Fixer

- discover / collect-fixes / repair / reconcile / push:
  - unknown external boundary failures → `retryable_transient`;
  - existing `remote head changed → retryable_after_resume` rule preserved;
  - dirty fixer worktree → `manual_intervention` (unchanged);
  - auto-commit/auto-push disabled with uncommitted changes →
    `manual_intervention` (unchanged).
- **fix the missing `context.Canceled` / `DeadlineExceeded` transient case** in
  fixer's `classifyFailure` to match worker / planner / reviewer behavior.

#### Planner

- spec lookup / generation / commit steps:
  - existing `transientFailure` interface check preserved;
  - existing `manual_intervention` cases (e.g. spec generation explicitly
    requiring intervention) preserved;
  - other unknown external boundary failures → `retryable_transient` via
    shared classifier.

### 6. Keep enhanced transient matching as an additive signal, not the authority

Reviewer's `enhancedTransientClassification` and
`recoverExistingMatchedFailures` are useful, but they are not the long-term
authority for retry behavior, and they should not be silently extended to other
runners as the primary mechanism.

This spec keeps enhanced transient matching as an additive signal for
**external-boundary** failures only:

- helpful for `git ls-remote`, SSH, EOF, broken pipe, and similar transport
  text;
- useful for recovery of already-failed loops when attempts remain;
- but not sufficient as a global classifier.

If defaults are changed, scope them carefully:

- enhanced matching should be consulted inside external-boundary
  wrappers/classifiers;
- it should not be used to globally convert arbitrary local/storage/checkpoint
  errors into transient failures just because a string contains `EOF` or
  `timeout`.

### 7. Add explicit operator retry after manual fix (all runner types)

Deterministic failures that a human can fix should be easy to retry afterward,
regardless of runner type.

Add a first-class retry operation for loops. The command operates at the loop
level and works for any loop type (`reviewer`, `worker`, `fixer`, `planner`).

#### CLI

```bash
looper loop retry <seq> [--mode auto|resume|rediscover]
```

Optional shorthand:

```bash
looper retry <seq>
```

#### API

```http
POST /api/v1/loops/{id}/retry
```

Payload:

```json
{
  "mode": "auto",
  "resetAttempts": true
}
```

#### Retry modes

##### `auto`

Recommended default:

- `manual_intervention` + valid checkpoint ⇒ resume from checkpoint;
- user-fixed config / setup / storage condition ⇒ restart from the runner's
  initial step unless a known-safe resume policy exists;
- ambiguous post-side-effect failure with durable marker/checkpoint ⇒ resume
  from checkpoint / verification step;
- corrupt checkpoint ⇒ restart from the runner's initial step.

##### `resume`

Force resume from checkpoint.

Only valid when checkpoint invariants are intact.

##### `rediscover`

Force restart from the runner's initial step (`discover` for reviewer / fixer,
`claim` for worker, `spec_lookup` for planner, etc.). Each runner declares its
own canonical "restart from beginning" step.

Recommended for user-fixed config, auth, routing, and similar setup failures.

### 8. Add explicit worktree-discard retry, but keep it opt-in

Applies to runners with worktrees (reviewer / worker / fixer).

Add an explicit destructive retry option for dirty-worktree recovery:

```bash
looper loop retry <seq> --discard-worktree-changes --confirm
```

or, if a separate operation is preferred:

```bash
looper worktree reset <seq> --confirm
looper loop retry <seq> --mode resume
```

The destructive worktree reset path must:

- refuse to run when there is an active run or active queue item;
- clearly state which worktree path will be reset;
- require explicit confirmation;
- preserve old run/queue history rather than mutating it in place;
- be a no-op for runners without a worktree (planner).

### 9. Preserve retry and queue invariants

This design must preserve the following invariants for every runner:

- never auto-delete dirty worktree contents without explicit authority or
  explicit user confirmation;
- never requeue when an active run or active queue item already exists for the
  same loop;
- preserve runner-specific dedupe-key behavior;
- preserve terminal semantics for `terminated` and `stopped`;
- keep automatic retry attempt budgets separate from explicit operator
  retries;
- preserve old failed run/queue history and append new retry lineage rather
  than rewriting history;
- resume from checkpoint only when checkpoint invariants are valid and
  side-effect idempotency is understood.

### 10. Surface `manual_intervention` explicitly in `looper ps`

`manual_intervention` is operationally different from both generic `failed`
and generic `paused`:

- it means Looper intentionally stopped because the next safe action requires
  human judgment or a user-applied fix;
- it is actionable now;
- if it is not surfaced prominently, operators are likely to miss it and
  assume the system is simply idle.

This applies to all runner types, not only reviewer.

`looper ps` should expose both `paused` and `manual_intervention` clearly,
with `manual_intervention` rendered as a more specific operator-facing status
instead of being buried under a generic top-level loop status.

#### Required behavior

- Add a visible `manual_intervention` status in `looper ps` output for any
  loop type.
- Keep `paused` visible in `looper ps` output as its own explicit status for
  loops that are intentionally halted but do not currently require the
  stronger `manual_intervention` label.
- A loop whose latest failure kind / effective resume policy indicates manual
  intervention should render as `manual_intervention` in list output even if
  its persisted loop status is currently `paused`.
- `looper ps --json` should include a structured field that distinguishes:
  - persisted loop status (`paused`, `failed`, etc.)
  - effective operator-facing status (`manual_intervention`, `retrying`,
    `running`, etc.)
  - last failure kind / resume policy
  - loop type (`reviewer`, `worker`, `fixer`, `planner`) so operators can
    filter
- `looper ps` table output should make manual-intervention items easy to
  notice, for example by using the status column value directly rather than
  requiring users to inspect logs.
- `looper ps` table output should continue to show ordinary `paused` loops
  directly as `paused`.
- When available, `looper ps --json` should also include a short actionable
  reason string, such as:
  - `dirty reviewer worktree`
  - `dirty worker worktree`
  - `dirty fixer worktree`
  - `config validation failed`
  - `auto push disabled`
  - `manual retry required after user fix`

#### Status derivation

Do **not** change the persisted loop state machine solely for display.

Instead, derive an operator-facing status roughly as follows:

- if latest queue failure kind is `manual_intervention`, or effective resume
  policy is `manual_intervention` ⇒ display `manual_intervention`
- else if persisted loop status is `paused` ⇒ display `paused`
- else follow normal loop status rendering

This derivation logic lives once, near the `ps` rendering code, and is shared
across loop types.

#### Why `looper ps` must own this

Manual intervention is exactly the class of problem that users scan
`looper ps` to discover. If the signal lives only in checkpoint JSON, queue
metadata, or logs, users will miss it. This is especially important once retry
behavior becomes more resilient, because the remaining non-automatic cases are
precisely the ones that need explicit operator action.

## Concrete implementation touchpoints

### Shared classifier

- new `internal/loops/failureclass/` (or similar) package with `Boundary`,
  `RunnerKind`, `Context`, and a `Classify(error, Context) QueueFailureKind`
  function;
- shared transient-pattern table reused by reviewer enhanced matching, fixer's
  `remote head changed`, etc., but consulted only in external-boundary
  contexts.

### Git boundary typing

- `internal/infra/git/gateway.go`:
  - remote-head lookup, fetch, push, and prepare paths return richer typed
    errors for remote transport vs local deterministic git failures;
  - all four runners consume the typed errors via the shared classifier.

### GitHub boundary typing

- `internal/infra/github/errors.go`:
  - distinguish transient service failures from deterministic auth / scope /
    resource denials;
  - reuse from all runners.

### Reviewer runner

- `internal/reviewer/runner.go`:
  - `classifyFailureForProject` delegates unknown-error decision to shared
    classifier;
  - `runPrepareWorktreeStep`, `runReviewStep`, `runPublishStep`,
    `failedReviewerLoopRecoveryEligibility`, and step wrappers updated to
    pass `Context` (step + boundary + side-effect state).

### Worker runner

- `internal/worker/runner.go`:
  - `classifyFailure` delegates to shared classifier;
  - claim / worktree prepare / agent / validation / push / open-PR step
    wrappers pass `Context`;
  - existing `FailureManualIntervention` cases (routing, dirty worktree, stale
    worktree, validation hints, auto-commit gates) preserved.

### Fixer runner

- `internal/fixer/runner.go`:
  - `classifyFailure` delegates to shared classifier;
  - **add `context.Canceled` / `DeadlineExceeded` transient handling** to
    match other runners;
  - existing `remote head changed → retryable_after_resume` rule and
    manual-intervention worktree gates preserved.

### Planner runner

- `internal/planner/runner.go`:
  - `classifyFailure` delegates to shared classifier;
  - existing `transientFailure` interface and `FailureManualIntervention`
    paths preserved.

### Runtime auto-recovery

- `internal/runtime/runtime.go`:
  - keep failed-loop recovery whitelisted and bounded;
  - generalize `shouldAutoRecoverFailedReviewerLoop` (or add equivalents) so
    auto-recovery rules apply consistently per loop type;
  - allow enhanced transient recovery only when provenance indicates external
    boundary safety.

### API / CLI retry UX

- `internal/api/handler.go`:
  - add `POST /api/v1/loops/{id}/retry` endpoint that works for any loop type;
  - preserve existing terminal-loop guards except for explicit retry path.
- `internal/cliapp/app.go`:
  - add `loop retry` (and shorthand `retry`).
- `internal/cliapp/ps.go` (or equivalent):
  - derive and render operator-facing `manual_intervention` status for all
    loop types.
- `internal/cliapp/json_output.go`:
  - add structured retry output;
  - add structured `manual_intervention` / effective-status / loop-type
    fields for `ps`.
- `internal/cliapp/loop_diagnostics_commands.go`:
  - surface next-step guidance for manual intervention and retry commands.

### Worktree safety / cleanup

- `internal/worktreesafety/safety.go`
- `internal/worktreecleanup/*`

These should remain authority-preserving. Generic background cleanup must not
silently replace explicit destructive operator retry.

## Tests

### Classification tests (shared)

Add tests for the shared classifier verifying:

- unknown git-remote transport failures classify as `retryable_transient`;
- unknown GitHub transport failures classify as `retryable_transient`;
- deterministic GitHub auth/scope/resource denials do **not** classify as
  transient;
- storage/config/checkpoint invariant failures remain `non_retryable`;
- dirty worktree contexts classify as `manual_intervention`.

### Per-runner classification tests

For each of reviewer / worker / fixer / planner:

- unknown external boundary failures from runner-specific steps classify as
  `retryable_transient`;
- existing manual-intervention cases continue to classify as
  `manual_intervention`;
- existing `retryable_after_resume` cases (e.g. fixer `remote head changed`,
  reviewer publish ambiguity) preserved;
- runner-specific narrow rules (worker validation hints, planner
  `transientFailure` interface, reviewer enhanced matching) preserved.
- fixer specifically: `context.Canceled` and `context.DeadlineExceeded`
  classify as `retryable_transient`.

### Worktree tests

For reviewer / worker / fixer:

- `git ls-remote` / fetch / push disconnect during worktree prepare ⇒
  retryable;
- dirty worktree ⇒ `manual_intervention`;
- explicit destructive retry path refuses without confirmation or with active
  run present.

### Reviewer publish tests

- ambiguous post-side-effect publish failures use `retryable_after_resume`
  only when safe checkpoint/marker state is durable;
- retry does not duplicate review/comment side effects.

### Retry UX tests

- `loop retry --mode auto` chooses resume vs rediscover appropriately for each
  loop type;
- explicit retry creates new queued work while preserving prior failure
  history;
- retry is rejected when there is already an active run/queue item;
- retry after manual fix works for paused/manual-intervention and
  failed/non-retryable loops, for any runner type.

### `looper ps` visibility tests

- loop with latest `manual_intervention` failure renders as
  `manual_intervention` in table output, regardless of loop type;
- `looper ps --json` includes persisted status, effective display status,
  loop type, and actionable reason;
- generic paused loops that are not manual-intervention continue to render as
  `paused`;
- failed loops without manual intervention continue to render as `failed`;
- paused loops remain visible in aggregated/status output and are not
  collapsed into an ambiguous catch-all bucket.

### Budget tests

- automatic retries stop after budget exhaustion and end terminal `failed`;
- explicit operator retry starts a new attempt lineage and does not mutate
  old queue attempt counts in place.

## Rollout plan

### Phase 1

- introduce shared `failureclass` package and typed git/GitHub external
  failures;
- wire reviewer, worker, fixer, planner `classifyFailure` to consult the
  shared classifier for the unknown-error case;
- fix fixer `context.Canceled` / `DeadlineExceeded` gap;
- preserve current defaults unless explicitly enabled in code paths with
  clear provenance.

### Phase 2

- add explicit `loop retry` API + CLI (loop-type agnostic);
- add operator guidance in diagnostics and failure summaries;
- add `looper ps` `manual_intervention` rendering and JSON fields.

### Phase 3

- evaluate whether enhanced transient classification should be enabled by
  default for external-boundary paths in each runner;
- evaluate whether reviewer-specific `recoverExistingMatchedFailures` becomes
  a uniform per-runner setting.

## Open questions

- Should explicit operator retry reset attempts on a replacement queue item,
  or carry a separate retry lineage field in metadata for observability?
- Should `loop retry --mode auto` prefer rediscover more aggressively for all
  user-fixed deterministic failures, even when a checkpoint technically
  exists?
- Should the destructive worktree reset path live under `loop retry` as a
  flag, or under a separate `worktree reset` command for stronger operator
  clarity?
- Should the shared classifier live under `internal/loops/failureclass`, or
  under a more neutral path such as `internal/runners/failureclass`, given
  that `internal/loops` currently hosts loop policy and not classification?
