# GitHub state remains the work Authority; cloud audit log is observability only

## Context

The Network design introduces a `loopernet` service holding registry, lease, event, and audit-log state. A natural temptation is to make `loopernet` the source of truth for admission or assignment decisions: "node red is assigned issue #42 because the cloud's audit log says so." This would parallel the kind of central dispatch service common in distributed work-queue systems.

`AGENTS.md`'s "Name the authority before enforcing it" rule is explicit: Authority must be a durable, structured signal. It should also be the *named* signal that future readers can point to when asking "why did this happen?" Per ADR-0002 and ADR-0004, Looper has consistently chosen GitHub-native state (durable labels, native dependency relations, native close state) as that signal — not Looper-private storage.

The Network design preserves this discipline only if the cloud audit log is unambiguously *observability*, not Authority, and if exact Node target is visible in GitHub state.

## Decision

Four Authorities exist in the Network design, named distinctly:

- **GitHub work-intent state** is the source of truth for **work eligibility**. Worker intent is `looper:worker-ready`; Reviewer intent is a GitHub review request. A Role claims work because this GitHub state exists, not because the cloud audit log records an admission/assignment decision.
- **`looper:target:<node_name>` labels** are the source of truth for **exact Node target** in Routed projects. Exactly one valid target label must match the local Node before Worker or Reviewer may claim.
- **The Lease row** in the `loopernet` database is the source of truth for **Coordinator control-plane leadership**. A Node holds it or does not; if expired, it cannot mutate GitHub for Network admission/assignment/targeting. Validated at side-effect boundaries per ADR-0007.
- **The cloud audit log** is the source of truth for **observability**. It records what happened, when, and why for operator-facing debug commands (`looper netadmin trace`). It is not consulted by any Role to decide whether to act.

These Authorities never overlap. A Role asking "should I claim this work?" reads GitHub work intent and exact target labels. The Network-aware Coordinator asking "may I mutate GitHub?" checks the Lease. An operator asking "why did red get this issue?" reads the audit log.

## Considered Options

- **Cloud audit log as routing Authority.** Rejected because routing decisions become invisible to humans inspecting the GitHub Issue, breaking the "human-vetoable, public" property of every other Authority in Looper. Also: cloud DB loss would lose authority, while GitHub state would survive.
- **Cloud as a stateful dispatch queue with ack protocol.** Rejected per ADR-0008's analysis — shifts Authority off durable GitHub state, requires significant distributed-systems machinery, and creates a hot-path dependency on cloud availability.
- **Hybrid: GitHub work intent for eligibility, cloud-cached node state as exact target Authority.** Rejected as a confusion vector. Exact target must be public and repairable in GitHub state.

## Consequences

- Cloud DB loss is recoverable. Re-deploy `loopernet`, re-onboard repos (regenerating webhook secrets), re-join Nodes — work eligibility is unaffected because GitHub labels survive. Audit history is lost; ongoing work is not.
- The reconciliation poll is a correctness backstop, not a latency optimisation. If the `loopernet` webhook/event path fails, the lease-holder Node reading GitHub state directly catches up. The audit log might miss the entry, but eligibility is intact.
- Operators debugging "why did this issue go to red?" use `looper netadmin trace`, which queries the audit log. Operators verifying "is this Role claim legitimate?" inspect GitHub work intent and `looper:target:*`. Different question, different Authority.
- This ADR is the contract that prevents future contributors from routing decisions through cloud-only state. New side-effects must answer "what GitHub state justifies this?" before adding any cloud-side gate.
