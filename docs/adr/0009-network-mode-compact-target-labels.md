# Network mode v1 avoids Role×Node labels and uses compact target labels

## Context

Looper's reactive Roles support configurable Trigger labels via `roles.<role>.triggers.labels` plus `LabelMode` (`Any` or `All`). The first Network design considered suffixing every Role trigger with the Node Name (for example, `looper:worker-ready:red`). That would create a Role×Node label matrix and would still not solve the "one human GitHub account, multiple Nodes" case without making labels the exact target Authority.

The current v1 design uses a single cross-role target-label namespace instead: `looper:target:<node_name>`.

## Decision

In v1, Routed projects do not use Role×Node trigger labels. Exact Node targeting is represented by exactly one `looper:target:<node_name>` label. Worker still uses `looper:worker-ready` as generic work intent. Reviewer uses a GitHub review request as work intent and does not introduce `looper:reviewer-ready`.

Coordinator may create the generic work-intent signals in both local-only and Routed projects. The target label remains separate: it selects the exact Node in Routed projects, while trigger labels and review requests express work intent.

## Considered Options

- **Role×Node trigger labels.** Rejected because label count grows with Roles×Nodes and labels become both role intent and exact target, making duplicate GitHub identity support harder to reason about.
- **Cloud-only exact target state.** Rejected because GitHub would no longer show the exact target Authority. Cloud DB loss or outage would obscure claim legitimacy.
- **Hidden comments/check runs as target Authority.** Rejected because they are less visible and harder to repair than labels, and add GitHub API/permission surface.

## Consequences

- A repository with N Nodes needs N target labels, not Roles×N trigger labels.
- Duplicate GitHub identities are allowed because the exact Node target is `looper:target:<node_name>`, not the GitHub account.
- Local-only projects ignore target labels entirely.
- Future Routed semantics for Planner/Fixer must consume the same exact target-label Authority or define a new ADR.
