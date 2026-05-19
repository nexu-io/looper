# Per-project `network.mode = routed` is the Authority for Network target-label reactivity

## Context

In a Network, multiple Nodes may subscribe to the same repository. Without exact targeting, every Node's reactive Roles sharing the same GitHub identity could race to claim the same GitHub work. The Network-aware Coordinator solves the production side by adding an exact target label (`looper:target:<node_name>`) after applying GitHub-native coarse authority. The consumption side requires a complementary rule preventing Roles from treating Network target labels as meaningful in local-only projects.

Per `AGENTS.md`'s "Name the authority before enforcing it" rule: *what is the Authority for "this Node's Worker/Reviewer Role should require `looper:target:<node_name>` before claiming?"*

The naive answer "because the daemon is joined to a Network" is too coarse — it would force every project the Node serves into Network mode, breaking mixed-mode operation. A finer-grained Authority is required.

## Decision

The Authority for a reactive Role to use Network target-label matching is the per-project `network.mode = routed` setting in `config.toml`. When a project has `network.mode = routed`:

- Worker claims only when `looper:worker-ready`, a matching exact target label, and the local Node's GitHub assignee are present.
- Reviewer claims only when a matching exact target label and the local Node's GitHub review request are present.
- Coordinator control-plane actions require the Network Lease. Planner, Fixer, and Sweeper keep existing local-only semantics unless given explicit Routed semantics in a later ADR.

When a project has `network.mode = off`, `looper:target:*` labels are ignored and legacy single-machine behaviour is preserved.

The setting extends the canonical per-project `[[projects]]` config object (`internal/config.ProjectRefConfig`) with a `network` sub-struct:

```
projects[].network.mode = "off" | "routed"
```

The default is `off` (legacy behavior). `looper network join` does not flip existing projects to `routed` automatically; operators must opt a project into `routed` after configuring Network membership and Coordinator control-plane eligibility. Validation rejects `network.mode = routed` when the daemon is not joined to a Network or cannot satisfy the Routed claim/lease prerequisites.

## Considered Options

- **Node-global flag (`network.json` presence implies all projects routed).** Rejected because it forces a big-bang migration: a Node operator could not migrate one project at a time, and could not selectively bypass network mode on a problem project during incident response. Removes the per-project escape hatch.
- **Direct RPC dispatch from Coordinator to Nodes.** Rejected because it shifts Authority off durable GitHub labels and onto in-flight RPCs, requiring a stateful dispatch queue, ack protocol, idempotency keys, and retry policy in the cloud. Inconsistent with ADR-0002's durable-label pattern.
- **Stickiness via a claim-protocol on top of generic labels.** Rejected because it requires a distributed-lock convention encoded in GitHub state, with comment-based or label-suffix-based locking — significantly more code and edge cases than per-project mode switching.
- **Hybrid (target label optional).** Rejected because it preserves the original race for duplicate GitHub identities: the first Node sharing that identity to poll wins, defeating the routing decision.

## Consequences

- Worker and Reviewer get a single new check at their discovery and claim points: "for this project, is exact target-label matching required?" The check is centralized in `internal/network/policy` to prevent drift.
- Mixed-mode operation works naturally: projects with `network.mode = off` retain legacy behavior on the same Node.
- The complement of routing — un-routing on `network.mode` flip from `routed` to `off` — must remove Network target labels before the project is considered local-only. Generic Worker labels and human GitHub metadata are not rewritten as Role×Node labels.
- `looper network leave` auto-resets `network.mode` to `off` on all projects to prevent the Node from running in a half-state where it expects a Network it is no longer part of.
- The "exactly one valid target label" invariant prevents split-brain Coordinator control-plane Nodes from causing dual claims: if two leaders each apply different target labels, no Node acts and reconciliation cleans up.
