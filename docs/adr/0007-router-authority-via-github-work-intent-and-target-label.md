# Router Authority uses GitHub work intent plus exact target labels, gated by the Lease

## Context

Adding the Router introduces a new side-effecting actor for Routed projects. The Router does not perform work itself; it establishes exact Node targeting for work that already exists in GitHub state. Per `AGENTS.md`'s "Name the authority before enforcing it" rule, every side-effecting action must answer: *what is the Authority for this action, and why is it not the agent's own structured output?*

The v1 design uses GitHub-native work intent plus a compact exact target label:

- Worker work intent is an open Issue with `looper:worker-ready`.
- Reviewer work intent is an open Pull Request with an existing review request to a Network member's GitHub identity.
- Exact Node targeting is exactly one valid `looper:target:<node_name>` label.

## Decision

The Router's Authority for routing is the **current GitHub work-intent state**. For Worker, that is `looper:worker-ready`. For Reviewer, that is an existing review request to a Network member. The Router may add or repair `looper:target:<node_name>` only to make exact Node targeting explicit for work that GitHub already declares as eligible.

The **Lease is the gate**, not the work-intent Authority. The Router may act only while it holds a fresh Lease, validated at every GitHub side-effect boundary (per ADR-0008's revalidation requirement). The Lease authorises Router activity; it does not justify any specific item being routed.

The **cloud audit log records actions for observability**, not as Authority. Audit log entries are written *after* the rewrite as a side-effect of action; AGENTS.md is explicit that Authority must be the signal that justifies the action, not its receipt.

## Considered Options

- **Lease as the Authority.** Rejected because it is necessary but not sufficient. Holding the Lease authorises Router activity in general; it does not name *which* Issue or PR is justified.
- **Audit log as the Authority.** Rejected because the audit entry is posterior to the rewrite — it records what happened, not what justified it. Treating it as Authority would invert the ordering AGENTS.md requires.
- **Routing decision as a transient Authority.** Rejected because the decision exists only in Router process memory until the audit entry is written; it is not durable, public, or human-vetoable.
- **Role×Node trigger labels.** Rejected for v1 because it creates a label matrix (`looper:worker-ready:red`, `looper:reviewer-ready:red`, etc.) while a single cross-role target label carries the exact target with less label proliferation.

## Consequences

- Router never *creates* work intent. It only records exact Node target for current GitHub work intent. This keeps the design consistent with ADR-0002's "agent's structured output committed to GitHub" pattern.
- Humans veto Worker routing by removing `looper:worker-ready`; they veto Reviewer routing by removing the review request; they can also remove a stale or unwanted `looper:target:*` label and reconciliation will repair only if current work intent still exists.
- The Lease/revalidation mechanism (ADR-0008) is required to prevent stale Routers from acting under expired authority. Without revalidation, the gate is advisory; with it, the gate is enforced at the side-effect boundary.
- Partial routing states are expected because GitHub mutations are not atomic. A target label without the required GitHub coarse target is not claimable and must be repaired or removed by reconciliation.
