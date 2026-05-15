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
A proactive, LLM-driven Role that performs Triage on fresh Issues and executes Dispatch.
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
The label a reactive Role watches for to claim an Issue or Pull Request. Configured per Role (e.g. Planner's trigger label is set in `roles.planner.triggers.labels`).
_Avoid_: queue label, pickup label.

**Veto signal**:
A human-applied state on an Issue that blocks Coordinator's autonomous Dispatch. Examples: removing the `dispatch/*` label, applying `looper:hold`, or applying the trigger label manually.

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

## Relationships

- A **Coordinator** performs **Triage** on a fresh **Issue**, producing a **Disposition** plus classification labels
- A **Coordinator** performs **Dispatch** on a Triaged Issue, producing a **Trigger label** that a **Planner** or **Worker** observes
- A **Sweeper** retires Issues and Pull Requests that have aged past their **Trigger label** or have an `out-of-scope` Disposition
- **Coordinator** and **Sweeper** are both stateless and compose via shared label semantics, not direct calls
- A **Veto signal** from a human overrides Coordinator's autonomous Dispatch but does not override **Triage** itself

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
> **Dev:** What if a human just types `/plan` instead of waiting?
> **Domain expert:** Then **Coordinator** dispatches immediately. Human-gated mode is the default; autonomous mode requires the grace window. Either way the **Authority** for dispatch is the durable label on the **Issue**, never an in-memory decision.
