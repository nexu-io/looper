# Coordinator is stateless

## Context

Looper's existing reactive Roles (Planner, Worker, Reviewer, Fixer) persist run state, queue items, agent executions, and checkpoints in a local SQLite database. The Coordinator Role is fundamentally different in shape — it sweeps a continuous stream of fresh Issues and shepherds them toward Dispatch — which initially suggested it would also need persistent state: a dispatch ledger, a shepherd state machine, a per-Issue triage-failure counter.

## Decision

Coordinator owns no private database tables. All Coordinator memory lives in GitHub: labels (`triaged`, `kind/*`, `area/*`, `complexity/*`, `dispatch/*`, `wont-fix`, `needs-info`), HTML comment markers (`<!-- looper:coordinator:triage -->`) for self-dedup, and the Issue's event timeline for derived facts like "when was `triaged` applied." This makes GitHub the single source of truth for Coordinator's view of every Issue.

## Considered Options

- **Dispatch ledger** — local table recording "I added trigger label X to Issue N at time T" so Coordinator could detect stalled dispatches. Rejected because the same fact is queryable from the GitHub event timeline; a private ledger introduces a second source of truth that can drift from reality.
- **Shepherd state machine** — per-Issue stage tracking (e.g. `awaiting-spec`, `awaiting-implementation`). Rejected because the labels on the Issue already encode stage, and a private state machine creates a reconciliation surface where Coordinator can believe an Issue is in stage X while GitHub says Y.
- **Failure counter** — local count of consecutive triage failures per Issue, with quarantine after N strikes. Rejected because counting requires either a private table or fragile timeline reconstruction; the simpler choice is infinite retry bounded by the per-tick cap and the `looper:hold` veto label.

## Consequences

- Every Coordinator action must be designed to be re-runnable safely from any partial state. Action ordering — labels first, comment, then `triaged` label as durability commit — is what makes this work.
- Dedup of Coordinator's own comments depends on GitHub's comment-list API and the `<!-- looper:coordinator:* -->` marker convention. If GitHub changes comment query semantics, this dedup mechanism is exposed.
- A poison-pill Issue body that consistently breaks the LLM prompt will burn cost on every tick until an operator applies `looper:hold`. The per-tick cap (`triage.maxPerTick`) bounds the damage; logs surface the pattern.
- Concurrent Coordinator instances against the same repo are explicitly out of scope. Single instance is assumed, as for every other Role.
