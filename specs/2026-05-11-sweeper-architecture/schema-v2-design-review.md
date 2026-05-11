# Sweeper schema v2 design review

Phase 1b and early Phase 2 usage exposed a few places where `schemaVersion: 1` is too lossy for long-term operator analysis.

## Fields that proved underspecified

1. `evidence[]`
   - v1 keeps only category/decision/rationale text.
   - v2 should attach structured evidence references: source (`issue_comment`, `timeline`, `review`, `linked_pr`, `label`, `body`), locator, excerpt, and confidence contribution.

2. `decisionContext`
   - v1 cannot distinguish why a proposal was produced under `agent_apply`, `heuristic_fallback`, replay, or diagnostic comparison.
   - v2 should encode proposal origin, whether it is canonical vs shadow, and whether it came from replay or live queue execution.

3. `policySnapshotRef`
   - v1 embeds the full fact bundle snapshot but does not identify which policy knobs materially affected the decision.
   - v2 should expose the relevant category thresholds / dry-run gates / schema expectation explicitly for auditing.

4. `applyIntent`
   - v1 stores apply receipts after the fact, but the proposal itself cannot express the intended live side effects.
   - v2 should describe intended actions such as `post_warning_comment`, `add_pending_label`, `close_target`, `quarantine_label`, `cancel_pending`.

5. `routeSecurityDetails`
   - `route_security` is currently a single bucket.
   - v2 should leave room for subreasoning such as secrets exposure, abuse report, credential mention, or policy-sensitive content.

6. `closeReason`
   - v1 conflates category rationale with the GitHub close reason that may be emitted (`completed`, `not_planned`).
   - v2 should separate these so audits can compare proposed intent to emitted mutation details.

7. `diagnosticComparison`
   - Phase 2 diagnostic mode needs a first-class way to link the heuristic shadow proposal to the paired agent proposal generated from the same fact bundle.
   - v2 should include a shared comparison id and per-proposal role (`heuristic_shadow`, `agent_canonical`).

## Non-goals for v2

- No historical backfill of existing v1 proposals.
- No expansion of live category scope by schema change alone.
- No queue-payload growth; the ledger remains the source of truth.
