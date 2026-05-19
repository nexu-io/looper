# Coordinator is unsupported for Routed projects in v1

## Context

The Coordinator Role (per ADRs 0001–0004) is proactive and assumes single-instance operation: each daemon configured with `roles.coordinator.enabled = true` polls open Issues and performs Triage independently. In a Network with multiple Nodes subscribed to the same repository, multiple Coordinators would race on `triaged` label idempotency, duplicate LLM cost, and produce conflicting `dispatch/*` decisions.

The natural fix is a second Lease type — a Coordinator Lease, parallel to the Router Lease — held by exactly one Node per Network, gating Coordinator ticks for Routed projects. This was the round-1 design.

Round-2 oracle review surfaced that the second Lease, while structurally clean, has long-tail costs: an additional eligibility flag, additional revalidation surface inside Coordinator's LLM-driven path (which holds for tens of seconds before mutating GitHub), and additional failure modes (Coordinator-Lease handoff, dual-lease co-holding, eligibility/lease drift). It also expands the v1 implementation surface non-trivially for a use case that is opt-in and not the dominant network deployment shape.

The intersection of "operators using network mode" and "operators using Coordinator triage" is small in v1: Coordinator is `Enabled = false` by default, and most early Network deployments will not enable it.

## Decision

In v1, Routed projects do not run Coordinator. Specifically:

- Validation at config-load time rejects any project with both `network.mode = routed` and `roles.coordinator.enabled = true`, with a clear error: "Coordinator is not supported for Routed projects in v1. Either disable Coordinator on this project or set `network.mode = off`."
- The scheduler does not tick Coordinator for Routed projects, even if mis-configured at runtime.
- The cloud's lease table contains only the Router Lease; no Coordinator Lease type is defined in v1.
- Operators who require Coordinator on a project must keep that project as `network.mode = off`; the Node will run Coordinator locally, with the existing single-instance assumption.

## Considered Options

- **Build the Coordinator Lease in v1.** Rejected for the cost reasons above. Lease handoff during the long Coordinator tick (LLM call, comment posting, label sequence) requires revalidation at every side-effect boundary inside Coordinator's existing flow — significant intrusion into a working subsystem.
- **Allow concurrent Coordinators per repo, accept duplicate triage.** Rejected because Coordinator's stateless design (ADR-0001) relies on idempotency for re-runnability, not for concurrency safety. Concurrent Coordinators would race on `triaged` apply, comment dedup, and `dispatch/*` apply. Real correctness risk.
- **Suppress Coordinator on all Nodes in Routed projects, no Lease.** Rejected because it removes Coordinator function entirely for those projects, with no upgrade path. The defer-via-validation approach preserves the configuration shape so v2 can lift the restriction by adding the Lease.

## Consequences

- v2 lifts this restriction by introducing the Coordinator Lease as a parallel cloud-side Lease type. The validation rule becomes a soft warning or removed.
- Operators cannot safely share the same repository between a local-only Coordinator Node and Routed execution Nodes in v1, because the `network.mode = off` Node ignores target labels and can still claim reactive work. If they experiment with that split workflow anyway, they must isolate the repository from Planner/Worker/Reviewer/Fixer discovery on the Coordinator Node; otherwise the whole project should stay `network.mode = off`.
- The Coordinator codebase (`internal/coordinator/*`) is not modified by v1 network work. Its existing single-instance assumption (ADR-0001) is preserved verbatim.
- Documentation must be explicit about this v1 limitation in `docs/users-guide.md` and the network-mode onboarding flow.
