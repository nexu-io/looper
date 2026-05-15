# Coordinator and Sweeper share GitHub via an explicit label contract

## Context

Coordinator (proactive, LLM-driven, handles Issue intake) and Sweeper (proactive, rule-based, handles Issue retirement) both operate on the same Issue stream and both apply labels and comments. Without an explicit contract, they can fight: Coordinator could re-Triage an Issue Sweeper has already marked for closure, or Sweeper could retire an Issue with active `dispatch/*` work pending. The council review flagged this as a real risk (problem #4: cross-role label contract underspecified).

A natural temptation was to give Coordinator close authority — "spam → close" or "out-of-scope after delay → close" — to make Coordinator feel more like a real maintainer. This was rejected because Sweeper already owns the warn-then-close lifecycle, with `pendingLabel`/`closedLabel`/`keepLabel`. Duplicating that path in Coordinator violates "A second fix to the same subsystem is a revert signal" prospectively.

## Decision

Coordinator never closes Issues. Sweeper retains exclusive close authority. The two Roles compose via shared label semantics, not direct calls or shared state:

- **Coordinator skips Triage** when an Issue bears Sweeper's `lifecycle.pendingLabel`, `lifecycle.closedLabel`, or `security.quarantineLabel`. Sweeper's lifecycle takes precedence.
- **Sweeper skips retirement** when an Issue bears `dispatch/*` or `needs-info` labels. These are configured as exempt prefixes in Sweeper. Coordinator's active-work signals take precedence over staleness.
- **`looper:hold` is a global hold.** Both Roles respect it: Coordinator skips Dispatch, Sweeper skips retirement. Single label, single semantic ("humans, leave this Issue alone").
- **Coordinator-owned label namespace** is documented (not enforced in code): `triaged`, `kind/`, `area/`, `complexity/`, `dispatch/`, plus the configured `outOfScopeLabel` (default `wontfix`) and `needs-info` labels. Coordinator clears and re-applies these on Triage rerun; no other Role writes to them.
- **The `out-of-scope` Disposition path** is Coordinator applying the configured `outOfScopeLabel` (default `wontfix`) + comment, then leaving the Issue open. Sweeper's existing inactivity rule eventually retires it.

## Considered Options

- **Give Coordinator close authority on `out-of-scope` or `spam` Disposition.** Rejected because it duplicates Sweeper's warn-then-close lifecycle and forces Coordinator to manage `pendingLabel`/`closedLabel` semantics it doesn't otherwise need.
- **Make `looper:hold` dispatch-only.** Rejected because two-scope hold semantics ("hold dispatch but allow stale closure") is more confusing than one global hold and breaks user expectations.
- **Have Coordinator detect `spam` Disposition and apply a `spam` label for Sweeper to close.** Rejected because reliable spam detection requires grounded signals (account age, history, repo norms) the LLM cannot access cheaply, and the false-positive cost is high (closing real bug reports as spam).
- **Add a runtime label-namespace registry** enforcing that only Coordinator can write to `dispatch/*` etc. Rejected as YAGNI; documentation-as-contract is sufficient for v1, with the option to add enforcement later if collisions actually occur.

## Consequences

- Coordinator and Sweeper config must reference each other's label values. The wiring happens at scheduler-config-load time; no runtime config-sharing infrastructure is needed.
- Sweeper's exempt-labels mechanism is extended to support label *prefixes* (matching `dispatch/*`), which is a small but real change to Sweeper's existing config shape.
- The `out-of-scope` path leaves the Issue open with the configured `outOfScopeLabel` (default `wontfix`) until Sweeper inactivity-retires it. This is intentional — humans get a window to dispute the Coordinator's Disposition before the Issue closes.
- Spam and duplicate Dispositions are explicitly deferred to v2. v1 Dispositions are limited to `valid` / `out-of-scope` / `unclear`.
