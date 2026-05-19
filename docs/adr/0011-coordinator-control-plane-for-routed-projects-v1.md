# Coordinator control plane is supported for Routed projects in v1

## Context

The Coordinator Role (per ADRs 0001–0004) is proactive and assumes single-instance operation: each daemon configured with `roles.coordinator.enabled = true` polls open Issues and performs Triage independently. In a Network with multiple Nodes subscribed to the same repository, multiple Coordinators would race on `triaged` label idempotency, duplicate LLM cost, and produce conflicting dispatch/review-assignment decisions.

The Network design needs exactly one Coordinator control-plane leader per Network for Routed projects. Earlier drafts introduced a separate product-level Router and deferred Coordinator. That split left no Looper-owned mechanism to add `looper:worker-ready` after Issue publication or request a reviewer after PR creation.

The cleaner model is to make Coordinator the control plane and treat exact targeting as an internal Coordinator capability. This matches the product goals: decide whether Issues should be worked, decide who should review PRs, and support both decisions in local-only and Routed modes.

## Decision

In v1, Routed projects support Coordinator control-plane activity, gated by the Network Lease. Specifically:

- Only the current Lease holder performs Coordinator admission/assignment GitHub mutations for Routed projects.
- Coordinator may admit Issues by applying assignee + `looper:worker-ready`, then `looper:target:<node_name>` last.
- Coordinator may assign PR review by requesting the selected reviewer, then `looper:target:<node_name>` last.
- The scheduler must ensure non-leader Nodes do not run Coordinator control-plane ticks for Routed projects.
- Lease revalidation is required before every GitHub side-effect boundary.

## Considered Options

- **Defer Coordinator in Routed projects.** Rejected because it leaves Looper without a mechanism to create the two user-requested signals: `looper:worker-ready` for Issues and review requests for PRs.
- **Allow concurrent Coordinators per repo, accept duplicate triage.** Rejected because Coordinator's stateless design (ADR-0001) relies on idempotency for re-runnability, not for concurrency safety. Concurrent Coordinators would race on `triaged` apply, comment dedup, and `dispatch/*` apply. Real correctness risk.
- **Keep a separate Router product Role.** Rejected because it duplicates Coordinator's control-plane responsibility and obscures the authority for "should this be worked?" and "who should review?".

## Consequences

- Coordinator code must learn Routed lease gating and side-effect-boundary revalidation.
- Local-only projects keep existing Coordinator semantics and ignore `looper:target:*`.
- Routed projects must fail closed when no leader, no eligible Node, or ambiguous target state exists.
- Documentation must describe Coordinator as the Network control plane, not a separate Router.
