# Sweeper architecture optimization checklist

## Phase 0 — Documentation deltas

- [x] Land this spec under `specs/2026-05-11-sweeper-architecture/`
- [x] Update `docs/sweeper.md` and `roles.sweeper` config reference to the target model
- [x] Align CLI help / debug output with canonical vocabulary (case, proposal, fact bundle, apply receipt, stale proposal, marker UUID)
- [x] Define metric names for `sweeper.proposals.*` and `sweeper.apply.*`

## Phase 1a — Persisted ledger (no agent surface change)

- [x] Add `sweeper_cases` table, repository, and required indexes
- [x] Add `sweeper_proposals` table, repository, and required indexes
- [x] Migrate runner internals to read/write the new tables
- [x] Reduce queue payload to orchestration metadata only
- [x] Extract deterministic prefilter stage from current runner
- [x] Build Phase 1 fact bundle per §5.1 (no new gateway methods)
- [x] Widen `ViewIssue` / `ViewPullRequest` return shape to expose comment timestamps if missing
- [x] Implement fingerprint algorithm and persist on case + proposal rows
- [x] Implement marker pre-check using `markerUUID`
- [x] Implement per-step apply protocol with `partial:*` receipts
- [x] Implement stale-proposal detection on apply
- [ ] Expand reconcile to cover the full triggers table (event-driven path)
- [x] Add maintenance reconcile entry point
- [x] Move daily ceilings to applied-side accounting (soft + hard budget split)
- [x] Wire heuristic classifier as `proposer_kind=heuristic_v1` writing real proposals

## Phase 1b — Agent proposer on the same ledger

- [x] Add sweeper proposal schema file (`schemaVersion: 1`)
- [x] Add prompt builder + agent execution wrapper
- [x] Persist raw agent transcripts alongside normalized proposal JSON
- [x] Validate every agent proposal against schema and decision×category matrix
- [x] Wire `roles.sweeper.proposer.mode = agent_apply | heuristic_fallback`
- [x] Enforce shadow-only heuristic proposals under `agent_apply`
- [x] Switch `stale` and `abandoned_pr` apply to consume agent proposals
- [x] Keep `already_fixed`, `superseded`, `unrelated`, `route_security` gated to dry-run
- [x] Surface filtered-out vs agent-reviewed counts in CLI/debug output
- [x] Apply the failure & retry matrix uniformly across propose and apply

## Phase 2 — Hardening and operator surface

- [x] Operator inspection commands (list cases, show proposal+receipt, replay propose)
- [x] Metrics/dashboards for proposals, apply outcomes, stale rate, agent timeout rate
- [x] Backpressure: auto-flip repo to dry-run on agent timeout-rate threshold
- [x] Schema version 2 design review (no implementation)
- [x] Diagnostic mode: heuristic + agent in parallel for offline accuracy comparison

## Phase 3 — Richer fact bundle and deterministic evidence categories

- [x] Add gateway methods: `ListIssueComments`, `ListIssueTimeline`, `ListIssueReactions`, `ListLinkedPullRequests`, `ListPullRequestReviewState`
- [x] Extend fact bundle with §5.3 field groups
- [ ] Decide per-field whether any new field enters the fingerprint; amend §7 if so
- [x] Stronger evidence extraction for `already_fixed` and `superseded`
- [x] Bump proposal schema to `schemaVersion: 2` for linked-evidence references
- [x] Enable `already_fixed` and `superseded` for live apply after dry-run validation

## Phase 4 — Subjective categories and reporting polish

- [x] Decide whether `unrelated` should exist in apply mode
- [x] Optional markdown exports / durable human-readable reports
- [x] Reconsider `quarantine` as agent-selectable decision
