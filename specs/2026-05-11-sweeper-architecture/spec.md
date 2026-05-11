# Sweeper architecture optimization plan

Issue: TBD
Base branch: `main`

## Background

Looper's current `sweeper` implementation is functional but intentionally narrow:

- discovery, queueing, and processing are already integrated into the normal scheduler/runtime flow;
- classification is fully programmatic and local;
- dry-run writes useful decision payloads back into `queue_items.payload_json`;
- mutation and classification are still tightly coupled inside the same runner methods.

That architecture is enough for early validation, but it is not the right long-term shape for a policy engine that can warn, close, quarantine, reconcile, and explain decisions safely.

At the same time, `clawsweeper` has already validated several patterns worth borrowing:

- strict schema-driven LLM decisions;
- durable per-run artifacts;
- explicit prefilter → propose → apply separation;
- stronger retry, reconciliation, and safety-gate behavior.

This document defines a realistic upgrade path for Looper's sweeper that borrows those strengths without importing ClawSweeper wholesale.

## Terminology

Definitions used throughout this spec:

- **Case** — one mutable row in `sweeper_cases` keyed by `(project_id, repo, target_type, target_number)`. Source of truth for sweeper lifecycle state.
- **Proposal** — one immutable row in `sweeper_proposals`. Records the structured decision produced for a case at a point in time, plus the apply receipt once apply runs.
- **Fact bundle** — the normalized, read-only snapshot of GitHub state and Looper-side context handed to the proposer. Inputs to the fingerprint.
- **Fingerprint** — a stable hash over the policy-relevant subset of the fact bundle (see "Fingerprint algorithm"). Used by the apply lane to detect drift.
- **Apply receipt** — the apply-side fields written back onto a proposal row (`apply_status`, `apply_summary`, `apply_error`, `applied_at`, plus per-step progress markers).
- **Stale proposal** — a proposal whose persisted fingerprint no longer matches the freshly recomputed fingerprint at apply time, or whose `schema_version` is no longer accepted.
- **Marker UUID** — a UUID generated once at propose time, embedded in the warning comment HTML marker, and used by the apply lane to detect "warning already posted" without re-listing comments.
- **Proposer kind** — string identifier for who produced a proposal: `prefilter_v1`, `agent_v1`, `heuristic_v1`. The first two are canonical; the third is fallback only.

## Problem

The current sweeper has four structural weaknesses:

1. **Queue payload is doing too much.**
   Current sweeper reconstructs lifecycle state from queue history instead of using a canonical case record.

2. **Propose and apply are collapsed.**
   `processWarn` and `processClose` classify and mutate in one pass, which makes drift detection, retries, auditability, and future agent-backed reasoning harder.

3. **The decision core is not agent-driven yet.**
   Current decisions are driven by title/body keyword checks and inactivity windows. For a product whose value proposition is autonomous judgment, that is not enough. It is acceptable only as a temporary bootstrap or fallback, not as the steady-state architecture.

4. **Safety and reconciliation are too shallow.**
   The current reconcile path handles only a small subset of drift/cancellation scenarios, and mutation ceilings are effectively inferred from queue history rather than successful applies.

The intended target shape should match Looper's other roles:

- **programmatic filtering first** for cheap, deterministic hard gates and candidate narrowing;
- **agent judgment second** for context-aware decisions, rationale, and human-facing replies;
- **typed/apply lane last** for safe, idempotent GitHub mutation.

## Goals

1. Preserve Looper's scheduler/queue integration and typed GitHub gateway model.
2. Introduce a **strict propose/apply split** for sweeper decisions.
3. Add a **durable sweeper case ledger** and **immutable proposal artifacts** in SQLite.
4. Support a future **agent-backed propose lane** with schema-validated output.
5. Keep `dryRun` as a first-class mode that produces the same durable artifacts as live apply.
6. Make retries, reconciliation, and idempotency first-class behaviors.
7. Roll out conservatively, starting with categories that are easiest to validate safely.

## Non-goals

1. Replacing Looper's scheduler, queue, or generic run model.
2. Turning sweeper into a long-running collaborative loop like reviewer/fixer.
3. Immediately enabling every subjective closure category.
4. Making markdown exports the canonical audit store.
5. Adding many new knobs before the current sweeper surface is fully wired.

## Current-state summary

### Keep

- Queue-driven discovery and processing in the scheduler.
- Queue types `sweeper:warn`, `sweeper:close`, `sweeper:reconcile`.
- Per-target dedupe and lock keys.
- Conservative defaults: `autoDiscovery=false`, `dryRun=true`, category thresholds, daily ceilings.
- Typed GitHub gateway methods for comments, labels, issue close, and PR close.

### Change

- Stop treating queue history as the canonical case database.
- Stop collapsing classification and mutation into the same execution path.
- Stop treating synthetic threshold-derived confidence as equivalent to evidence-backed confidence.
- Stop expanding category scope before fact collection and safety checks are strong enough.

## Design principles

1. **Queue items are triggers, not truth.**
2. **Cases are mutable state; proposals are immutable evidence.**
3. **Programmatic prefiltering happens before the agent and remains deterministic.**
4. **The apply lane is the only mutator and must be idempotent.**
5. **Dry-run and live apply should share the same decision pipeline.**
6. **Category rollout should follow fact quality, not ambition.**

## Recommended target architecture

### 1. Keep the queue, add canonical sweeper tables

Add two sweeper-specific SQLite tables:

#### `sweeper_cases`

One row per target (`repo + target_type + target_number`).

Purpose:

- canonical lifecycle state;
- latest known target fingerprint;
- current phase (`warn`, `close`, `reconcile`, `terminal`);
- latest proposal ID;
- warning comment ID / marker ID;
- due timestamps (`warned_at`, `close_due_at`, `reconcile_due_at`);
- last observed human activity;
- terminal outcome and terminal timestamp;
- category enablement snapshot used for the live decision.

This becomes the source of truth for sweeper state. Queue payload remains orchestration metadata only.

#### `sweeper_proposals`

Immutable record for each propose attempt.

Purpose:

- normalized fact bundle;
- proposal schema version;
- proposer kind (`heuristic_v1`, `agent_v1`, etc.);
- raw structured decision JSON;
- validation status / parse errors;
- target fingerprint at propose time;
- projected action (`no_action`, `warn`, `close`, `cancel`, `quarantine`, `stale_proposal`);
- apply receipt fields (`applied_at`, `apply_status`, `apply_error`, `apply_summary`).

This becomes the durable audit artifact for both dry-run and live mode.

### 2. Split sweeper into three internal lanes

#### A. Fact collection / filter lane

Always deterministic and read-only.

Responsibilities:

- load target details from GitHub;
- apply hard exclusion rules before expensive reasoning;
- perform cheap deterministic filtering to shrink the candidate set;
- build a normalized fact bundle;
- compute a target fingerprint;
- determine whether the case is eligible for propose.

This lane mirrors the programmatic filtering other roles already do: it should reject obvious no-op cases cheaply, then hand only plausible candidates to the agent. The primary production decision path after filtering must be agent-backed.

#### B. Propose / decision lane

Read-only. Never mutates GitHub.

Responsibilities:

- consume the fact bundle;
- produce a structured sweeper proposal;
- validate proposal against a schema;
- persist the proposal to `sweeper_proposals`;
- update `sweeper_cases` with the latest proposal pointer and lifecycle intent.

This is where the context-aware decision happens. The agent is responsible for:

- category selection within the allowed policy envelope;
- confidence and rationale;
- human-facing warning / close reply draft;
- explicit no-action decisions when the filtered candidate should remain open.

There are two proposal engines, but only one is the intended production path:

1. **Agent proposer** — required primary path from Phase 1 onward, inspired by ClawSweeper's `codex exec --output-schema` flow.
2. **Heuristic fallback proposer** — optional emergency fallback for diagnostics, offline recovery, or constrained repos; it is not the product's core value path and should never be the default once agent mode exists.

#### C. Apply lane

The only mutating lane.

Responsibilities:

- reload live target facts immediately before mutation;
- recompute or verify the fingerprint;
- detect drift and stale proposals;
- perform marker pre-check for warning comments;
- execute idempotent mutation ordering;
- persist an apply receipt onto the proposal and case row.

Possible apply outcomes are defined canonically in section "Idempotent apply protocol" below.

### 3. Expand the reconcile lane into a first-class repair subsystem

Current reconcile behavior is too narrow. The full set of reconcile triggers and responses is enumerated canonically in section "Case row reconciliation triggers" below; reconcile MUST read from `sweeper_cases` and `sweeper_proposals`, not infer intent from queue history.

### 4. Move to schema-driven proposals

Borrow the strongest ClawSweeper pattern directly: proposals must be schema-validated.

The Looper sweeper proposal schema is closed-world (`additionalProperties: false`) and versioned (`schemaVersion`). Phase 1 ships `schemaVersion: 1` with the following fields:

- `schemaVersion` (integer, required) — schema generation; bumped on any breaking change.
- `decision` — one of `no_action | warn | close | cancel`. `quarantine` is intentionally **not** an agent-selectable decision in v1; security routing is produced only by the deterministic prefilter (`proposer_kind=prefilter_v1`).
- `category` — one of `stale | abandoned_pr | already_fixed | superseded | unrelated | route_security | none`.
- `confidenceScore` (integer 0–100, required) — single source of truth for confidence. Display layers may bucket as needed; the schema does not carry a separate `high|medium|low` field.
- `summary` (short, ≤ 200 chars).
- `rationale` (longer free-form, agent-authored).
- `evidence[]` — array of typed evidence pointers (e.g. `{kind: "linked_pr", number: 123}`, `{kind: "comment_id", id: 456}`); shape allowed to grow under the same `schemaVersion` only via additive enum members.
- `warningComment` — agent-authored draft body for the warning notice (used when `decision=warn`).
- `closeComment` — agent-authored draft body for the closure notice (used when `decision=close`).
- `cancelComment` — optional draft body used when `decision=cancel` and a courtesy comment is desired.
- `risks[]` — free-form risk strings the proposer wants the operator to see.
- `markerUUID` (required) — generated once at propose time, embedded into any warning comment, persisted on the proposal, and never regenerated by the apply lane (see "Idempotent apply protocol").
- `fingerprint` — the fact-bundle fingerprint (see "Fingerprint algorithm").

Fields deliberately **not** in v1:

- `confidence` (bucket) — collapsed into `confidenceScore`.
- `requiresHumanReview` — overlaps with `decision=cancel` and with `risks[]`; if reintroduced it will be enforced as a hard downgrade to `no_action`, not as advisory metadata.
- `followUpChecks[]` — no consumer in Phase 1; reserved for `schemaVersion: 2`.

#### Legal decision × category combinations

The schema (or a runtime validator immediately after parse) MUST reject any combination outside the matrix below. This prevents the apply lane from re-implementing cross-field validation and gives the agent prompt a tight target:

| decision    | allowed categories                                                                  | who may produce it          |
| ----------- | ----------------------------------------------------------------------------------- | --------------------------- |
| `no_action` | `none` (only)                                                                       | agent or prefilter          |
| `warn`      | `stale`, `abandoned_pr`, `already_fixed`, `superseded`, `unrelated`                 | agent only                  |
| `close`     | `stale`, `abandoned_pr`, `already_fixed`, `superseded`, `unrelated`                 | agent only                  |
| `cancel`    | any (carries the prior category for traceability)                                   | agent or apply-time fixup   |
| `quarantine`| `route_security` (only)                                                             | prefilter only (Phase 1–3)  |

#### Schema versioning policy

- The apply lane MUST refuse proposals whose `schemaVersion` is not in the active acceptance set.
- A schema bump is rolled out in two steps: first widen the acceptance set on the apply lane, then flip proposers to emit the new version.
- In-flight proposals whose version becomes obsolete are marked `apply_status=skipped_schema_obsolete`; the case row is rolled back to allow re-propose.

### 5. Fact bundle contract and fact collection roadmap

The fact bundle is the read-only snapshot the proposer (agent or heuristic) sees. The fingerprint is computed from a subset of it. This section defines the Phase 1 minimum viable bundle, the GitHub gateway methods that back it, and the Phase 3 extensions required to unlock richer categories.

#### 5.1 Phase 1 fact bundle (minimum viable)

The Phase 1 bundle is the smallest set that satisfies the fingerprint algorithm AND gives the agent enough context to judge `stale` and `abandoned_pr`. It is split into three groups; "fingerprint" marks fields that flow into the fingerprint per §7.

**Target identity & state**

| field                        | type           | source                  | fingerprint |
| ---------------------------- | -------------- | ----------------------- | :---------: |
| `repo`                       | string         | discovery input         |             |
| `target_type`                | `issue` \| `pull_request` | discovery input |             |
| `number`                     | int64          | discovery input         |             |
| `state`                      | `open` \| `closed` | gh view             |     yes     |
| `is_draft`                   | bool (PR only) | gh view                 |     yes     |
| `head_sha`                   | string (PR only) | gh view               |     yes     |
| `created_at`                 | RFC3339 UTC    | gh view                 |             |
| `updated_at`                 | RFC3339 UTC    | gh view                 |     yes     |
| `closed_at`                  | RFC3339 UTC, nullable | gh view          |             |

**Authorship & content (read-only context for the agent prompt)**

| field                        | type           | source                  | fingerprint |
| ---------------------------- | -------------- | ----------------------- | :---------: |
| `title`                      | string         | gh view                 |             |
| `body`                       | string (truncated) | gh view             |             |
| `author`                     | string login   | gh view                 |             |
| `author_association`         | string enum    | gh view                 |             |
| `labels`                     | sorted []string | gh view                |             |
| `policy_labels_present`      | sorted []string | derived from `labels` ∩ policy-relevant set | yes |
| `comment_count`              | int            | gh view                 |             |

**Looper-side context**

| field                        | type           | source                  | fingerprint |
| ---------------------------- | -------------- | ----------------------- | :---------: |
| `case.current_phase`         | string         | `sweeper_cases`         |             |
| `case.warned_at`             | RFC3339 UTC, nullable | `sweeper_cases`  |             |
| `case.close_due_at`          | RFC3339 UTC, nullable | `sweeper_cases`  |             |
| `case.warning_marker_uuid`   | string, nullable | `sweeper_cases`       |             |
| `case.last_human_activity_at`| RFC3339 UTC, nullable | `sweeper_cases`  |             |
| `policy.snapshot`            | object         | resolved `roles.sweeper` config at propose time |   |
| `last_human_comment_at`      | RFC3339 UTC, nullable | derived (see 5.2) |    yes     |
| `human_comment_count_since_open` | int        | derived (see 5.2)       |     yes     |

`body` is truncated to a fixed cap (recommended 8 KiB) before bundling; the truncated marker is preserved in the bundle so prompts can render `... [truncated]`. The cap is implementation-tunable but MUST be deterministic so the fingerprint stays stable.

`policy.snapshot` is a denormalized copy of the resolved `roles.sweeper` config slice at propose time so a proposal stays auditable even after config changes.

`last_human_comment_at` and `human_comment_count_since_open` are the only Phase 1 fields that require classifying comments as human vs bot. The classification rule is: a commenter is "human" iff their login is not in `triggers.excludeAuthors`, not the configured Looper bot login, and `author_association ∉ {BOT}`. Phase 1 does NOT bundle individual comment bodies — only the derived aggregate timestamps and counts.

#### 5.2 GitHub gateway: Phase 1 vs Phase 3

Phase 1 keeps the existing gateway surface and only computes the new aggregates from data already returned by `ViewIssue` / `ViewPullRequest`. If those existing methods do not surface comment timestamps, the Phase 1a deliverable is to widen their return shape (not to add new methods).

Phase 1 gateway requirements:

- `ViewIssue` / `ViewPullRequest` must return enough comment metadata to compute `last_human_comment_at` and `human_comment_count_since_open`. If they currently do not, extend their return types in Phase 1a.
- No new gateway methods are introduced in Phase 1.

Phase 3 gateway extensions (new methods, gated behind Phase 3 work):

| method                         | purpose                                                          | feeds                              |
| ------------------------------ | ---------------------------------------------------------------- | ---------------------------------- |
| `ListIssueComments`            | most recent N human comment bodies                               | agent prompt for nuanced reads     |
| `ListIssueTimeline`            | `cross-referenced`, `closed`, `marked-as-duplicate`, `referenced` events | `already_fixed`, `superseded` evidence |
| `ListIssueReactions`           | reactions on warning comment specifically                        | reconcile + cancellation signals   |
| `ListLinkedPullRequests`       | PRs linked via `Fixes #` / development panel                     | `already_fixed` evidence           |
| `ListPullRequestReviewState`   | review request / approval / changes-requested state              | `abandoned_pr` precision           |

These are additive; none are required before Phase 3.

#### 5.3 Phase 3 fact bundle extensions

Once the Phase 3 gateway methods exist, the fact bundle gains the following groups. None of these enter the fingerprint by default; if any do, §7 must be amended in the same change.

- `recent_human_comments[]` — last N comments with `{author, association, created_at, body (truncated), is_maintainer}`.
- `warning_comment` — full record of the marker comment if present: `{id, body, created_at, edited, reactions_summary}`.
- `timeline.cross_references[]` — referenced/cross-referenced events with the other side's `{repo, type, number, state, merged}`.
- `timeline.closures[]` — `closed` events with `{actor, state_reason, closed_by_pr_number?}`.
- `timeline.duplicates[]` — `marked-as-duplicate` events.
- `linked_prs[]` — PRs linked to this issue with `{number, state, merged, merged_at, merge_commit_sha}`.
- `pr_review_state` (PR only) — `{requested_reviewers, latest_review_per_user, last_review_at}`.
- `maintainer_interaction` — derived: did any account in `policy.maintainerLogins` (or with `OWNER`/`MEMBER` association) comment, react, or push, and when.
- `state_flags` — pinned, locked, draft, archived (repo-level), transferred-from.

#### 5.4 Bundle is persisted, not recomputed

The full Phase-N bundle is serialized into `sweeper_proposals.fact_bundle_json` exactly as the proposer saw it. Apply lane MUST NOT reuse a stored bundle as if it were live state — it always re-fetches and recomputes the fingerprint. The stored bundle exists for audit and replay only.

### 6. Reframe daily ceilings around successful applies

Current ceilings should govern **actual mutating side effects**, not discovery volume or queued candidates.

Track separately:

- successful warnings per repo per UTC day;
- successful closes per repo per UTC day;
- successful quarantines per repo per UTC day;
- cancelled/stale proposals as non-mutating telemetry.

Two layers of budget enforcement are required to make this race-free under concurrent workers:

- **Discovery-time soft budget** — `remaining = ceiling − applied_today − in_flight`, where `in_flight` counts cases whose latest proposal has a non-terminal apply status. Used to decide how many candidates to enqueue this tick.
- **Apply-time hard budget** — the apply lane re-checks `applied_today` immediately before mutating. If `applied_today >= ceiling`, the apply attempt aborts with `apply_status=skipped_ceiling_reached` and writes a receipt so subsequent discovery sees the slot was not consumed.

`applied_today` is computed from `sweeper_proposals` rows where `apply_status` is one of the `completed_*` statuses **and** `applied_at` falls within the current UTC day for the repo.

### 7. Fingerprint algorithm

The fingerprint is the contract between propose and apply for drift detection. It MUST be defined explicitly.

Inputs (in this order, hashed as canonical JSON):

1. `target.state` (`open` / `closed`)
2. `target.updated_at` (RFC3339, normalized to UTC)
3. `target.head_sha` (PR only; empty string for issues)
4. `target.is_draft` (PR only)
5. `policy_labels_present` — a sorted list of labels from the policy-relevant set only: `lifecycle.pendingLabel`, `lifecycle.closedLabel`, `lifecycle.keepLabel`, `security.quarantineLabel`, and any `triggers.excludeLabels` / `triggers.looperInternalLabels`. Cosmetic labels are deliberately excluded so unrelated label edits do not invalidate proposals.
6. `last_human_comment_at` — timestamp of the most recent non-bot, non-Looper comment (RFC3339 UTC); empty string if none.
7. `human_comment_count_since_open` — integer.

Inputs deliberately **excluded**: title text, body text, bot comment activity, reaction counts, cosmetic label changes. Title/body edits and reactions can change frequently without changing policy intent and would otherwise create excessive stale-proposal churn.

The fingerprint is stored on both `sweeper_cases.last_fingerprint_json` (for fast drift comparison during discovery) and `sweeper_proposals.fingerprint_json` (for apply-time verification against the snapshot the proposer actually saw).

### 8. Idempotent apply protocol

The apply lane is the only mutator. It MUST be safe under retry, restart, and partial failure. The following invariants and ordering apply:

#### Invariants

- The `markerUUID` is generated once at propose time and persisted on the proposal. The apply lane MUST NOT regenerate it. Retries reuse the same UUID, which makes the warning marker pre-check work.
- A warning comment posted by sweeper always contains the HTML marker `<!-- looper:sweeper:warn id={markerUUID} -->`.
- Before mutating, the apply lane reloads the live target, recomputes the fingerprint, and aborts as `skipped_stale_proposal` if it does not match the proposal's `fingerprint`.

#### Per-step protocol

1. **Marker pre-check** — if `decision=warn`, list the most recent N comments (or use a single `gh` query that filters by the marker substring) and short-circuit to `completed_warned` if a comment with the proposal's `markerUUID` already exists.
2. **Fixed mutation order** — `comment → label → close`. Each step writes a partial apply receipt before moving on (`apply_status=partial:commented`, `partial:labeled`, etc.) so that interrupted runs can be resumed.
3. **Resume rule** — when the apply lane picks up a proposal with a `partial:*` receipt, it skips already-completed steps. Combined with the marker pre-check this makes "comment posted but label not added" recoverable without duplicate side effects.
4. **Terminal write** — only the final step transitions the proposal to a `completed_*` status and stamps `applied_at`.

#### Apply outcomes (canonical set)

- `skipped_no_action`
- `skipped_dry_run`
- `skipped_stale_proposal`
- `skipped_schema_obsolete`
- `skipped_ceiling_reached`
- `partial:commented`, `partial:labeled` (transient; not terminal)
- `completed_warned`
- `completed_closed`
- `completed_cancelled`
- `completed_quarantined`
- `failed_retryable`
- `failed_terminal`

#### Dry-run vs live divergence point

Dry-run and live MUST share the pipeline up to and including: fact collection → prefilter → agent propose → schema validation → proposal persistence → stale check → fingerprint verify. The two modes diverge only at the **final mutation call**:

- live mode invokes the GitHub gateway and writes the terminal receipt;
- dry-run writes `apply_status=skipped_dry_run` plus a structured "would-have-done" plan onto the receipt, so dry-run output is byte-comparable to live output up to the divergence point.

### 9. Case row reconciliation triggers

Cases drift from GitHub for many reasons. The behavior table is part of the spec, not implementation freedom:

| trigger                                     | detection                                        | case action                                                  |
| ------------------------------------------- | ------------------------------------------------ | ------------------------------------------------------------ |
| repo unreachable / 404                      | gateway error during fact collection             | `terminal_outcome=repo_unreachable`, stop reconcile attempts |
| target 404 / transferred                    | gateway 404 on view                              | `terminal_outcome=target_missing`; do not auto-create new case |
| PR `head_sha` changed                       | fingerprint mismatch on field 3                  | invalidate any non-applied proposal as stale                 |
| pending label removed by human              | reconcile fact collection                        | `outcomeCancelledByLabelRemoval`, edit warning comment       |
| keep label added by human                   | reconcile or apply pre-check                     | `cancel`, remove pending label                               |
| target closed by human before sweeper close | apply pre-check sees `state=closed`              | `outcomeAlreadyClosedByHuman`, remove pending label          |
| target reopened after sweeper close         | discovery sees `state=open` and prior closed case| keep `terminal_outcome` audit, allow new case after `reopenCooldownDays` |
| partial apply (e.g. commented but not closed)| `apply_status=partial:*` on retry                | resume from next step per Idempotent apply protocol          |
| proposal exists but case row missing        | DB consistency check on startup                  | rebuild case row from latest proposal                        |

Reconcile is split into two paths:

- **Event-driven reconcile** — covers the rows above that originate from GitHub-side change. Goes through the queue (`sweeper:reconcile`) and reads from `sweeper_cases` / `sweeper_proposals`, never from queue history.
- **Maintenance reconcile** — covers config/version changes (e.g. schema version bump, category enablement change). Runs as a one-shot sweep at daemon startup or on explicit operator command, not via per-target queue items.

### 10. Required indexes

The case/proposal tables MUST ship with the following indexes; otherwise the runner regresses to `Queue.List`-style full scans:

- `sweeper_cases (project_id, repo, current_phase)` — drives reconcile discovery.
- `sweeper_cases (project_id, repo, status)` — drives dedupe and reopen-cooldown lookups.
- `sweeper_cases (target_type, target_number, repo)` UNIQUE — enforces the canonical key.
- `sweeper_proposals (case_id, created_at DESC)` — fetch latest proposal per case.
- `sweeper_proposals (project_id, repo, applied_at)` — drives the daily ceiling accounting.
- `sweeper_proposals (apply_status)` partial / filtered if SQLite version allows — speeds the in-flight count.

### 11. Heuristic fallback isolation

When `roles.sweeper.proposer.mode = agent_apply`, the heuristic proposer MAY still run for diagnostic comparison, but its output is **shadowed**:

- shadow proposals are persisted with `proposer_kind=heuristic_v1`;
- shadow proposals MUST NOT update `sweeper_cases.last_proposal_id`;
- the apply lane MUST NOT consume a proposal whose `proposer_kind` is not allowed by the active `proposer.mode`.

Only `heuristic_fallback` mode (break-glass) lets heuristic proposals drive the case pointer and feed the apply lane.

### 12. Failure & retry matrix

The apply and propose lanes share a single classification table. Implementations MUST follow this matrix; they MUST NOT invent ad-hoc retry counts.

| failure                                | retryable | max attempts | next state                                              |
| -------------------------------------- | --------- | ------------ | ------------------------------------------------------- |
| agent timeout                          | yes       | 3            | re-propose (new proposal row)                           |
| agent returned non-JSON                | yes       | 2            | re-propose with stricter prompt                         |
| schema validation failed               | no        | 0            | proposal `validation_status=invalid`; case → needs_review |
| schema version not in acceptance set   | no        | 0            | `apply_status=skipped_schema_obsolete`                  |
| fingerprint mismatch on apply          | no        | 0            | `apply_status=skipped_stale_proposal`                   |
| GitHub 5xx during apply                | yes       | 5 (exp backoff) | retry apply with same proposal                       |
| GitHub 422 (state changed under us)    | no        | 0            | mark stale, enqueue reconcile                           |
| GitHub 403 rate-limited                | yes (delayed) | bounded by `available_at` | requeue with `available_at` pushed past reset |
| daily ceiling reached at apply time    | no        | 0            | `apply_status=skipped_ceiling_reached`                  |
| partial apply step error               | yes       | 3            | resume from last successful step                        |

## Category rollout strategy

### Phase-safe categories first

Enable first for live apply:

1. `stale`
2. `abandoned_pr`

Reason: these rely most heavily on deterministic inactivity signals.

### Deterministic evidence categories second

Enable later, only after better fact collection:

3. `already_fixed`
4. `superseded`

### Subjective categories last

Keep disabled until operator confidence is earned:

5. `unrelated`
6. `route_security` as agent-proposed route-only handling

## What to borrow from ClawSweeper

### Borrow now

1. **Structured schema output** for propose decisions.
2. **Durable proposal artifacts** independent of queue payload.
3. **Strict propose/apply separation**.
4. **Retry classification and bounded backoff** for GitHub and agent failures.
5. **Reconciliation ledger behavior** for partial/failed apply flows.
6. **Safety gating** before write mode.
7. **Agent-first decision making** rather than heuristic-first rollout.

### Borrow later

1. Deep operator-facing markdown/report dashboards.
2. Advanced self-heal / redispatch orchestration.
3. More expensive subjective categories.
4. Cost/performance optimizations around resumed agent sessions.

## Implementation plan

### Phase 0 — Documentation deltas

This document is itself the Phase 0 artifact. The remaining Phase 0 work is purely documentation:

1. Land this spec under `specs/2026-05-11-sweeper-architecture/`.
2. Update `docs/sweeper.md` (and any `roles.sweeper` config reference) so reader-facing architecture text matches the target model rather than the legacy queue-payload model.
3. Update CLI help / debug output naming to use the canonical vocabulary defined in "Terminology" (case, proposal, fact bundle, apply receipt, stale proposal, marker UUID).
4. Define metric names that the runner will emit in Phase 1+ (e.g. `sweeper.proposals.created`, `sweeper.apply.completed_warned`, `sweeper.apply.skipped_stale_proposal`) so dashboards can be built ahead of code.

### Phase 1 — Persisted ledger + agent-backed propose + minimal safe apply

Agent judgment is in scope from Phase 1; sweeper is not a meaningful product if heuristics remain the primary path. To keep landing risk bounded, Phase 1 is split into two sequential sub-phases that ship behind the same overall feature flag.

#### Phase 1a — Structural migration (no agent surface change)

Goal: move the canonical state off queue history without changing decision behavior. After 1a, dry-run and live decisions for the currently-enabled categories must produce the same outcomes as today, with the only behavioral delta being ceiling accounting moving to applied-side counts.

Deliverables:

1. Add `sweeper_cases` and `sweeper_proposals` tables, repositories, and required indexes (see "Required indexes"). [borrowed: durable proposal artifacts]
2. Migrate runner internals to read/write the new tables. Queue payload becomes orchestration-only.
3. Extract an explicit deterministic prefilter stage from the current runner. Existing keyword/inactivity logic moves here untouched. [borrowed: prefilter separation]
4. Add target fingerprint generation per "Fingerprint algorithm" and persist on both case and proposal rows.
5. Add marker pre-check for warning comments using `markerUUID`. [borrowed: idempotent apply]
6. Implement the per-step apply protocol with `partial:*` receipts. [borrowed: idempotent apply]
7. Implement stale-proposal detection on apply.
8. Expand reconcile to cover the full triggers table (event-driven path) and add maintenance reconcile entry point.
9. Move daily ceilings to applied-side accounting with the soft/hard budget split.
10. Keep the existing heuristic classifier as `proposer_kind=heuristic_v1` and have it write proposals into `sweeper_proposals` so apply consumes proposals (not in-memory state) from day one.

Phase 1a exit criteria:

- duplicate comment/label races are eliminated;
- restarts do not lose sweeper case state;
- every dry-run or live decision results in a durable proposal row;
- partial-apply scenarios resume cleanly under retry/restart;
- ceiling enforcement is visibly applied-side.

#### Phase 1b — Agent proposer on the same ledger

Goal: introduce schema-validated agent proposals as the canonical decision path for the categories already enabled in live apply, with heuristic kept as shadow only. [borrowed: schema output, agent-first]

Deliverables:

1. Add the sweeper proposal schema file (`schemaVersion: 1`) per "Move to schema-driven proposals". [borrowed: schema output]
2. Add prompt builder + agent execution wrapper for sweeper propose.
3. Persist raw agent transcripts/results alongside the normalized proposal JSON.
4. Validate every agent proposal against the schema and the legal decision×category matrix before it is allowed onto a case row's `last_proposal_id`.
5. Wire `roles.sweeper.proposer.mode = agent_apply | heuristic_fallback`. In `agent_apply`, heuristic output is persisted only as shadow per "Heuristic fallback isolation".
6. Switch the apply lane for `stale` and `abandoned_pr` to consume agent proposals as the source of truth; keep `already_fixed`, `superseded`, `unrelated`, and `route_security` in dry-run or gated mode until Phase 3.
7. Surface filtered-out vs agent-reviewed counts in CLI/debug output.
8. Apply the failure & retry matrix uniformly across propose and apply.

Phase 1b exit criteria:

- deterministic filtering removes obvious non-candidates before agent invocation;
- every apply attempt for an enabled category consumes a schema-validated agent proposal;
- no live mutation path can run from free-form, unvalidated, or wrong-`proposer_kind` output;
- heuristic fallback is demonstrably non-default and non-canonical;
- a few opt-in repos can run stable dry-run and live flows under `agent_apply`.

### Phase 2 — Hardening and operator surface

Phase 1b put agent_apply on the safe categories. Phase 2 invests in operator visibility and stability rather than expanding category scope.

Deliverables:

1. Operator-facing inspection commands: list cases by phase, show full proposal + receipt for a case, replay propose for a case in dry-run.
2. Metrics/dashboards for proposal volume, apply outcome distribution, stale rate, agent timeout rate.
3. Backpressure: when agent timeout rate exceeds a threshold for a repo, automatically flip that repo to dry-run (not to heuristic_fallback) and emit an alert.
4. Schema version 2 design review (no implementation yet) to capture which fields proved underspecified.
5. Diagnostic mode that runs heuristic and agent in parallel on the same fact bundle and persists both proposals (heuristic still shadowed) for offline accuracy comparison.

Exit criteria:

- operators can answer "why did sweeper do X to issue Y?" entirely from `sweeper_cases` + `sweeper_proposals` without rereading queue history;
- agent failure modes have been observed in the wild and are bounded by the failure & retry matrix;
- rollout repos sustain `agent_apply` without manual intervention for at least one operational cycle.

### Phase 3 — Richer fact bundle and deterministic evidence categories

Deliverables:

1. Add the Phase 3 GitHub gateway methods listed in §5.2 (`ListIssueComments`, `ListIssueTimeline`, `ListIssueReactions`, `ListLinkedPullRequests`, `ListPullRequestReviewState`).
2. Extend the fact bundle with the §5.3 field groups; persist them into `sweeper_proposals.fact_bundle_json` for new proposals only.
3. Decide per-field whether any new field enters the fingerprint; amend §7 if so.
4. Add stronger evidence extraction for `already_fixed` and `superseded` and reflect it in the agent prompt.
5. Expand the proposal schema (`schemaVersion: 2`) to capture linked-evidence references in `evidence[]`.
6. Enable `already_fixed` and `superseded` for live apply only after dry-run validation against real proposal volume.

### Phase 4 — Subjective categories and optional reporting polish

Deliverables:

1. Evaluate whether `unrelated` should exist at all in apply mode.
2. Add optional markdown exports / durable human-readable reports.
3. Consider security-routing enhancements only after maintainers confirm the operating model.

## Config changes

Keep the current outward-facing `roles.sweeper` surface stable where possible.

Add carefully, only when needed:

- `roles.sweeper.proposer.mode = agent_apply | heuristic_fallback`
- `roles.sweeper.filter.mode = deterministic`
- `roles.sweeper.proposer.model`
- `roles.sweeper.proposer.timeoutSeconds`
- `roles.sweeper.proposer.schemaVersion`
- optional reporting toggles for durable markdown export

The `heuristic_fallback` mode exists only for break-glass operation and local diagnostics. Product intent is that normal sweeper operation is agent-driven.

Avoid introducing category-specific bespoke prompts or per-category file overrides in the first pass.

## Data model sketch

### `sweeper_cases`

- `id`
- `project_id`
- `repo`
- `target_type`
- `target_number`
- `status`
- `current_phase`
- `current_category`
- `current_confidence_score`
- `warning_comment_id`
- `warning_marker_uuid`
- `last_proposal_id`
- `last_fingerprint_json`
- `last_human_activity_at`
- `warned_at`
- `close_due_at`
- `terminal_outcome`
- `terminal_at`
- `created_at`
- `updated_at`

### `sweeper_proposals`

- `id`
- `case_id`
- `project_id`
- `repo`
- `target_type`
- `target_number`
- `schema_version`
- `proposer_kind`
- `fact_bundle_json`
- `fingerprint_json`
- `proposal_json`
- `decision`
- `category`
- `confidence_score`
- `summary`
- `rationale`
- `marker_uuid`
- `validation_status`
- `validation_error`
- `apply_status`
- `apply_summary`
- `apply_error`
- `applied_at`
- `created_at`

The `confidence` bucket column is intentionally absent; `confidence_score` is the only persisted confidence field.

## Testing strategy

1. Unit tests for:
   - case/proposal repositories;
   - fingerprint generation (including stability under cosmetic label edits, title/body edits, and bot-only comments);
   - marker pre-check;
   - proposal schema validation including the legal decision×category matrix;
   - retry classification per the failure & retry matrix.

2. Runner tests for:
   - deterministic prefilter → agent propose handoff;
   - agent propose → dry-run artifact flow;
   - stale-proposal aborts (fingerprint mismatch);
   - schema-obsolete aborts (active acceptance set bumped);
   - partial apply recovery from each `partial:*` resume point;
   - reconcile repairs across all rows of the case reconciliation triggers table;
   - daily ceilings under both soft (discovery-time) and hard (apply-time) budget enforcement.

3. Integration tests for:
   - end-to-end warn/close/reconcile state machine;
   - restart recovery with persisted cases and proposals;
   - agent apply mode with schema-invalid, timeout, non-JSON, and drift cases;
   - heuristic fallback mode as a non-default recovery path;
   - shadow heuristic proposals in `agent_apply` never updating `last_proposal_id` and never reaching apply.

4. Edge cases that must have explicit tests:
   - agent returns valid schema but illegal decision×category combination;
   - agent returns `confidenceScore=10` with `decision=close` (apply-side policy gate must reject regardless of schema validity);
   - two concurrent propose attempts on the same case (last-writer rules / propose lock);
   - repo archived mid-flight (case becomes zombie);
   - PR `head_sha` changes between propose and apply;
   - oversized agent transcript (size cap and truncation policy);
   - daemon killed between `partial:commented` and `partial:labeled`.

## Risks and anti-patterns to avoid

1. Reusing `queue_items.payload_json` as the long-term case store.
2. Letting apply depend on ephemeral in-memory proposal state.
3. Sending too many obviously-ineligible targets to the agent instead of filtering first.
4. Treating every proposal parse failure as retryable forever.
5. Making markdown exports the authoritative record.
6. Treating heuristic fallback as an acceptable steady-state substitute for the agent path.
7. Allowing shadow heuristic proposals to update `sweeper_cases.last_proposal_id` under `agent_apply`.
8. Regenerating the warning marker UUID on apply retry (breaks marker pre-check; causes duplicate warnings).
9. Including title/body/cosmetic-label edits in the fingerprint (causes excessive stale-proposal churn).
10. Computing daily ceilings only at discovery time (race-prone under concurrent workers).
11. Expanding config surface faster than operator tooling.
12. Letting `quarantine` become an agent-selectable decision before Phase 4 review.

## Acceptance criteria

The architecture work is complete when:

1. Sweeper has canonical persisted case state.
2. Every dry-run or live decision has a durable proposal artifact.
3. Propose and apply are code-separated and behaviorally separated.
4. Deterministic filtering and agent decision are code-separated and behaviorally separated.
5. Apply is idempotent under retry/restart/partial-failure conditions.
6. Safe categories can run directly in `agent_apply` mode with durable proposals and idempotent apply.
7. Heuristic fallback remains clearly secondary and non-default.
8. Richer categories remain gated until the fact bundle is strong enough.

## Recommendation

Do **not** replace Looper sweeper with a direct port of ClawSweeper.

Instead:

- keep Looper's queue-driven runtime model;
- add a sweeper-specific case/proposal ledger;
- import ClawSweeper's strongest ideas where they matter most:
  - deterministic prefilter before expensive judgment,
  - schema-validated proposals,
  - durable artifacts,
  - strict propose/apply separation,
  - retries, reconciliation, and write safety.

That path is the best balance of correctness, maintainability, rollout safety, and implementation cost.

Most importantly, the sweeper should behave like the other roles: programmatic filtering first, then agent-driven context-aware judgment and reply generation. Otherwise the result is only a heuristic stale bot with better plumbing, which is not the intended product.
