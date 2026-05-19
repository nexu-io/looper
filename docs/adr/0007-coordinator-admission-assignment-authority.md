# Coordinator admission and assignment authority is committed to GitHub

## Context

Network mode extends Coordinator from Issue triage/dispatch into a control plane for both Issue admission and PR review assignment. Per `AGENTS.md`'s "Name the authority before enforcing it" rule, every side-effecting action must answer: *what is the Authority for this action, and why is it not the agent's own structured output?*

The v1 design commits Coordinator decisions to GitHub-native state:

- Issue admission is an open Issue with `looper:worker-ready` plus the selected GitHub assignee.
- PR review assignment is an open Pull Request with a GitHub review request to the selected reviewer identity.
- In Routed projects, exact Node targeting is exactly one valid `looper:target:<node_name>` label.

## Decision

Coordinator's Authority for admitting/assigning work is its current structured decision, committed to GitHub as durable state. For Issue admission, Coordinator applies `looper:worker-ready` and the selected assignee. For PR review assignment, Coordinator creates the selected GitHub review request. In Routed projects, Coordinator also applies `looper:target:<node_name>` as exact Node target.

The **Lease is the gate**, not the admission/assignment Authority. The Network-aware Coordinator may act only while it holds a fresh Lease, validated at every GitHub side-effect boundary (per ADR-0011's revalidation requirement). The Lease authorises Coordinator control-plane activity; it does not by itself justify any specific Issue or PR decision.

The **cloud audit log records actions for observability**, not as Authority. Audit log entries are written *after* the rewrite as a side-effect of action; AGENTS.md is explicit that Authority must be the signal that justifies the action, not its receipt.

## Considered Options

- **Lease as the Authority.** Rejected because it is necessary but not sufficient. Holding the Lease authorises Coordinator control-plane activity in general; it does not name *which* Issue or PR is justified.
- **Audit log as the Authority.** Rejected because the audit entry is posterior to the rewrite — it records what happened, not what justified it. Treating it as Authority would invert the ordering AGENTS.md requires.
- **A separate Router role.** Rejected because it splits "should this be worked / who should review" from exact targeting. Coordinator is already the Role that decides admission and dispatch; Network targeting is a Coordinator capability, not a separate product Role.
- **Role×Node trigger labels.** Rejected for v1 because it creates a label matrix (`looper:worker-ready:red`, `looper:reviewer-ready:red`, etc.) while a single cross-role target label carries the exact target with less label proliferation.

## Consequences

- Coordinator creates work intent when policy says so: `looper:worker-ready` for admitted Issues and review requests for assigned PRs. Humans/external automation may also create those same GitHub-native signals.
- Humans veto Worker work by removing `looper:worker-ready` or the assignee; they veto Reviewer work by removing the review request; they can also remove a stale or unwanted `looper:target:*` label.
- The Lease/revalidation mechanism (ADR-0011) is required to prevent stale Coordinator control-plane Nodes from acting under expired authority. Without revalidation, the gate is advisory; with it, the gate is enforced at the side-effect boundary.
- Partial mutation states are expected because GitHub mutations are not atomic. A target label without the required GitHub coarse target is not claimable and must be repaired or removed by reconciliation.
