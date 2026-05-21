# Looper

Looper is a daemon (`looperd`) plus CLI (`looper`) that runs autonomous agent **Roles** against a GitHub repository's issues and pull requests.

## Language

### Roles

A **Role** is a configured agent that performs one specific job in the issue/PR lifecycle. Roles are either *reactive* (they wait for a matching trigger) or *proactive* (they sweep their own input set on a cadence).

**Planner**:
A reactive Role that produces a Spec from an Issue.
_Avoid_: designer, architect.

**Worker**:
A reactive Role that implements a Spec or an Issue, producing a Pull Request.
_Avoid_: implementer, builder, coder.

**Reviewer**:
A reactive Role that reviews a Pull Request and posts review comments.
_Avoid_: critic, checker.

**Fixer**:
A reactive Role that addresses review feedback on a Pull Request.
_Avoid_: patcher, responder.

**Sweeper**:
A proactive, rule-based Role that retires stale or low-signal Issues and Pull Requests through warn-then-close lifecycle.
_Avoid_: janitor, cleaner.

**Coordinator**:
A proactive, LLM-driven Role that performs Triage on fresh Issues and executes Dispatch. In Network mode, Coordinator is also the control plane for Issue admission, PR review assignment, and exact Node targeting, gated by the Network Lease.
_Avoid_: manager, commander, maintainer.

### Issue lifecycle

**Triage**:
The act of forming an opinion about a fresh Issue: applying classification labels, posting a triage comment, and committing a Disposition. Performed exactly once per Issue by Coordinator.
_Avoid_: classification (overloaded — see below), assessment.

**Disposition**:
Coordinator's high-level conclusion about an Issue. One of `valid`, `out-of-scope`, `unclear`. Distinct from `kind`/`area`/`complexity`, which are classification labels applied only when the Disposition is `valid`.
_Avoid_: verdict, outcome, status.

**Dispatch**:
The act of putting an Issue into a state where Planner or Worker will discover it: applying the role's trigger label and assigning the configured user. Performed by Coordinator either on human slash-command (human-gated mode) or autonomously after a grace window (autonomous mode).
_Avoid_: handoff (overloaded — see below), route, promote, enqueue.

**Trigger label**:
The label a reactive Role watches for to claim an Issue or Pull Request. Configured per Role (e.g. Planner's trigger label is set in `roles.planner.triggers.labels`). In a Routed project, Worker still uses the generic `looper:worker-ready` Trigger label as work intent; exact Node targeting is expressed separately by `looper:target:<node_name>`.
_Avoid_: queue label, pickup label, routed label, dispatched label, target label.

**Veto signal**:
A human-applied state on an Issue that blocks Coordinator's autonomous Dispatch. Examples: removing the `dispatch/*` label, applying `looper:hold`, or applying the trigger label manually.

**Blocker**:
An Issue listed in another Issue's GitHub-native `blocked_by` set. The Blocker's `state` and `state_reason`, together with `blocked_by` itself, are the named **Authority** for the dependency gate.

**Dependency gate**:
The **Dispatch** precondition that all **Blockers** be `state==closed AND state_reason==completed`. The gate is blocked when any Blocker is open or closed-not-completed, and released when every Blocker satisfies the condition.

**Ready set**:
The subset of tracked Issues whose **Dependency gate** is currently released — the Issues that may be **Dispatched** this tick, subject to the existing PRD #334 conditions.

**Acceptance criterion**:
A checkbox item under an Issue's `## Acceptance criteria` section. Reviewer's auto-merge gate verifies each criterion has a satisfying-evidence pointer in the diff before submitting APPROVE.

**Auto-merge scope**:
The Looper-only constraint identifying which PRs Looper may opt into auto-merge: `looper:` label AND tracked-Issue link, both required. Encoded in `roles.reviewer.autoMerge.scope = "looper-only"`.

**Merge-pending state**:
The GitHub-native state of a Pull Request after `gh pr merge --auto` has been called and before GitHub merges or a **Veto signal** arrives. The PR's `auto_merge` field is non-null in this state. Coordinator's merge-watch classifies merge-pending PRs into WatchActions.

**Watch marker**:
The `<!-- looper:coordinator:merge-watch retries=N -->` HTML-comment marker Coordinator places on the linked Issue (not the PR — preserves ADR-0003 Issue-rooted scope) to carry merge-watch retry-counter state across ticks. Public, durable, idempotent — preserves ADR-0001's stateless property.

### Authority and statelessness

**Authority**:
For any side-effecting action, the named, durable, structured signal that justifies the action. Per `AGENTS.md`: "What is the authority for this action, and why is it not the agent's own structured output?" Coordinator's authority for Dispatch is the durable `dispatch/*` label on the Issue, which is the agent's structured output committed to GitHub.

**Stateless Role**:
A Role whose memory lives entirely in GitHub (labels, comments with markers, event timeline). It owns no private database tables. Coordinator is stateless. Sweeper is stateless. Worker, Planner, Reviewer, and Fixer are not — they persist runs in the local SQLite database.

### Comment markers

**Stamp**:
The standard `<!-- looper:stamp v=1 -->` HTML comment plus visible footer applied by every agent-authored comment, identifying the comment as Looper-generated. Defined in `internal/disclosure/disclosure.go`.

**Self-dedup marker**:
A Role-specific HTML comment marker (e.g. `<!-- looper:coordinator:triage -->`) used by a stateless Role to recognise its own prior comments and avoid duplicate posts.

### Network

**Network**:
A coordinated set of `looperd` instances that share Coordinator admission/assignment decisions for a configured set of repositories. A Node joins exactly one Network at a time. Hosted by a `loopernet` instance (one Network per `loopernet`).

**Node**:
A single `looperd` instance enrolled in a Network. Identified by an opaque cloud-issued ID and a human-readable Name (short label-safe string; convention is to use a color, e.g. `red`, `blue`, `cyan`).
_Avoid_: peer, member, instance, agent.

**Coordinator control plane**:
The Network-aware Coordinator responsibility that decides Issue admission and PR review assignment, then applies the GitHub state that Worker/Reviewer consume. In Routed projects it also applies an exact target label (`looper:target:<node_name>`) so a specific Node can claim the work.
_Avoid_: router, dispatcher, scheduler, balancer.

**Routed project**:
A project whose `network.mode` is `routed`. Coordinator admission/assignment is performed by the current Network Lease holder. Worker/Reviewer claim only when the exact target label matches the local Node and the role-specific GitHub-native coarse target is present. The complement is a *local-only project*, whose Roles keep existing single-machine behaviour and ignore `looper:target:*` labels.

**Target label**:
A Network-only exact Node target label of the form `looper:target:<node_name>`. Exactly one valid target label must be present before a Routed Worker/Reviewer may claim. Target labels are ignored in local-only projects.
_Avoid_: trigger label, routed label, worker-ready suffix.

**Lease**:
The durable Authority for Network Coordinator control-plane leadership. A row in the `loopernet` database with a fencing token, validated at every GitHub side-effect boundary.

## Relationships

- A **Coordinator** performs **Triage** on a fresh **Issue**, producing a **Disposition** plus classification labels
- A **Coordinator** performs **Dispatch** on a Triaged Issue, producing a **Trigger label** that a **Planner** or **Worker** observes
- A **Coordinator** may perform **PR review assignment**, producing a GitHub review request that **Reviewer** observes
- A **Coordinator** consults the **Dependency gate** before performing **Dispatch** when `roles.coordinator.dependencies.enabled = true`
- **Reviewer** opts approved code PRs (carrying **Auto-merge scope**) into GitHub-native auto-merge after verifying each **Acceptance criterion** has satisfying-evidence in the diff
- **Coordinator**'s per-tick poll classifies **Merge-pending state** PRs into WatchActions, routing mechanical failures (conflict, red CI) to **Fixer** via **Trigger label** and policy failures (branch protection change) to re-Triage by removing the Issue's `triaged` and `dispatch/*` labels
- The **Watch marker** carries merge-watch retry state on the linked Issue, preserving Coordinator's stateless property
- A **Sweeper** retires Issues and Pull Requests that have aged past their **Trigger label** or have an `out-of-scope` Disposition
- **Coordinator** and **Sweeper** are both stateless and compose via shared label semantics, not direct calls
- A **Veto signal** from a human overrides Coordinator's autonomous Dispatch but does not override **Triage** itself
- In a **Routed project**, the **Coordinator control plane** applies GitHub-native coarse authority (`looper:worker-ready` plus assignee for Worker, review request for Reviewer) and writes the **Target label** last. The **Lease** gates Coordinator control-plane action; current GitHub issue/PR state remains the claim Authority.

## Flagged ambiguities

- **classification** — used by humans to mean both Disposition and the kind/area labels. Resolved: Disposition is the high-level conclusion (`valid` / `out-of-scope` / `unclear`); kind/area/complexity are classification *labels* applied during a `valid` Triage. The unqualified word "classification" is avoided in favor of "Disposition" or "label".
- **handoff** — already used in code (`authoritative handoff fields`) for the PR-seed contract between Reviewer and Fixer. Not used for Coordinator's Dispatch action, which is a different concept. Use "Dispatch" exclusively for the Coordinator action.
- **manager / commander / maintainer** — early names considered for the Coordinator Role. Rejected: "manager" implies it directs other Roles (it doesn't, it sets labels), "commander" overpromises authority, "maintainer" is a human role.

## Example dialogue

> **Dev:** When a fresh **Issue** arrives, what does **Coordinator** do?
> **Domain expert:** It performs **Triage**: it reads the Issue, decides a **Disposition**, and if `valid` applies kind/area/complexity/dispatch labels and posts a triage comment. The `triaged` label is applied last as the durability commit.
>
> **Dev:** And then a **Planner** picks it up?
> **Domain expert:** Not directly. **Coordinator** later performs **Dispatch** — applies the planner's **Trigger label** and assigns the user. **Planner** then discovers it on its normal trigger.
>
> **Dev:** Why two steps?
> **Domain expert:** Triage produces structured output. Dispatch consumes it. Splitting them gives humans a veto window between the two — they can remove `dispatch/needs-plan` if they disagree.
>
> **Dev:** Where do dependencies fit?
> **Domain expert:** Before **Dispatch**, **Coordinator** consults the **Dependency gate**. If any **Blocker** in `blocked_by` is still open or was closed as anything other than `completed`, the Issue stays out of the **Ready set** until the gate releases.
>
> **Dev:** What if a human just types `/plan` instead of waiting?
> **Domain expert:** Then **Coordinator** dispatches immediately unless the **Dependency gate** is blocked. Human-gated mode is the default; autonomous mode requires the grace window. Either way the **Authority** for dispatch is the durable label on the **Issue**, never an in-memory decision.
>
> **Dev:** What happens after **Reviewer** APPROVEs a Looper PR?
> **Domain expert:** If **Auto-merge scope** matches and the linked Issue has stated **Acceptance criterion**s, **Reviewer** verifies each criterion against the diff. On all-pass, it submits APPROVE with per-criterion evidence and calls `gh pr merge --auto`. **GitHub branch protection** is the named **Authority** for "safe to merge" — Looper does not check CI itself. **Coordinator**'s per-tick poll then watches the **Merge-pending state** PR and classifies it into WatchActions; the **Watch marker** on the linked Issue carries retry-counter state without private storage.
