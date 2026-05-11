 # Sweeper architecture

 Looper's sweeper is a queue-driven policy engine for stale issues and pull requests. It stays conservative by default:

 - `roles.sweeper.autoDiscovery=false`
 - `roles.sweeper.dryRun=true`

 The target architecture separates sweeper into three internal lanes:

 1. **Prefilter / fact collection** — deterministic, read-only filtering and fact-bundle assembly.
 2. **Propose** — writes an immutable proposal artifact for the case.
 3. **Apply** — re-checks live state, detects drift, and performs idempotent GitHub mutation.

 ## Canonical vocabulary

 - **Case** — the mutable lifecycle row keyed by `(project_id, repo, target_type, target_number)` in `sweeper_cases`.
 - **Proposal** — an immutable decision artifact row in `sweeper_proposals`.
 - **Fact bundle** — the normalized read-only snapshot of GitHub + Looper context used by the proposer.
 - **Fingerprint** — a stable hash over the policy-relevant subset of the fact bundle.
 - **Apply receipt** — apply-side status fields written back to a proposal row.
 - **Stale proposal** — a proposal whose fingerprint or schema version is no longer acceptable at apply time.
 - **Marker UUID** — the UUID embedded into a warning comment marker and reused across retries.
 - **Proposer kind** — who produced the proposal, such as `prefilter_v1` or `heuristic_v1`.

 ## Data model

 Sweeper keeps queue items as triggers, not as canonical state.

 ### `sweeper_cases`

 One mutable row per target. The case row is the source of truth for:

 - current phase (`warn`, `close`, `reconcile`, `terminal`)
 - latest proposal pointer
 - latest target fingerprint
 - warning marker/comment metadata
 - warning / close / reconcile due timestamps
 - last observed human activity
 - terminal outcome metadata
 - policy snapshot used for the active decision

 ### `sweeper_proposals`

 One immutable row per propose attempt. Each proposal stores:

 - the persisted fact bundle
 - proposal schema version
 - proposer kind
 - structured decision JSON
 - fingerprint at propose time
 - projected action
 - apply receipt fields such as `apply_status`, `apply_summary`, `apply_error`, and `applied_at`

 ## Queue behavior

 Queue item payloads are orchestration metadata only. They can point at the case / proposal to process, but they are not the source of truth for sweeper lifecycle state.

 The queue types remain:

 - `sweeper:warn`
 - `sweeper:close`
 - `sweeper:reconcile`

 ## Phase 1 fact bundle

 The minimum fact bundle includes:

 - target identity and live state (`repo`, `target_type`, `number`, `state`, `updated_at`)
 - PR-only state where relevant (`is_draft`, `head_sha`)
 - author/content context (`title`, truncated `body`, `author`, `labels`, `comment_count`)
 - policy-relevant labels snapshot
 - Looper-side case context (`current_phase`, warning timestamps, marker UUID, policy snapshot)
 - derived human-comment aggregates (`last_human_comment_at`, `human_comment_count_since_open`)

 The full fact bundle is persisted on the proposal for audit. The apply lane always re-fetches live state before mutating.

 ## Fingerprint and drift detection

 The fingerprint is computed from the policy-relevant subset of the fact bundle, including:

 - target open/closed state
 - target `updated_at`
 - PR `head_sha`
 - PR `is_draft`
 - policy-relevant labels only
 - `last_human_comment_at`
 - `human_comment_count_since_open`

 Cosmetic label edits, body edits, bot comments, and reactions are deliberately excluded so they do not create unnecessary stale-proposal churn.

 ## Apply protocol

 The apply lane is the only mutating lane.

 Before mutating it must:

 1. reload live target state
 2. recompute the fingerprint
 3. reject stale proposals
 4. perform marker pre-check for warning comments

 Mutation order is fixed:

 1. comment
 2. label
 3. close

 Partial progress is recorded as an apply receipt (`partial:commented`, `partial:labeled`, and so on) so retries can resume without duplicating side effects.

 Warning comments always include:

 ```html
 <!-- looper:sweeper:warn id={markerUUID} -->
 ```

 ## Canonical outcomes

 Examples of canonical apply results include:

 - `skipped_no_action`
 - `skipped_dry_run`
 - `skipped_stale_proposal`
 - `skipped_schema_obsolete`
 - `completed_warned`
 - `completed_closed`
 - `completed_cancelled`

 ## Metrics namespace

 Phase 0 reserves these metric families:

 - `sweeper.proposals.*` — prefilter/propose counts, validation failures, proposer-kind breakdowns, stale-proposal counts
 - `sweeper.apply.*` — dry-run skips, ceiling skips, partial resumes, completed warnings/closes/cancellations, apply failures

 Concrete metric names should preserve that split so proposal telemetry and apply telemetry stay distinct.

 ## Rollout shape

 Phase 1a keeps the current heuristic engine but moves it behind the proposal ledger as `proposer_kind=heuristic_v1`.

 Phase 1b adds an agent proposer on the same ledger. The long-term production shape is:

 - deterministic prefilter first
 - agent proposal second
 - typed idempotent apply last
