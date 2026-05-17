# Coordinator authority is durable labels, not in-memory inference

## Context

Coordinator decides whether a fresh Issue should go to Planner or Worker. Per `AGENTS.md`'s "Name the authority before enforcing it" rule, every side-effecting action must answer: *what is the authority for this action, and why is it not the agent's own structured output?* The honest answer for Dispatch is "the agent's structured output is the authority" — so the question becomes how to make that output durable, inspectable, and human-vetoable rather than a fleeting LLM token consumed in the same call.

The earlier design had Coordinator emit a `complexity/*` label and a separate config block (`dispatch.autonomous.{plan, implement}`) mapped complexity to dispatch destination. A council review (alpha/beta/gamma) flagged this as the inference-on-agent-output trap the rule warns against: the LLM's complexity judgment plus a config policy together formed an inference layer above the structured output.

## Decision

Coordinator's LLM emits a `dispatch/*` label directly during Triage (`dispatch/needs-plan` or `dispatch/needs-implement`). The label persists on the GitHub Issue and is the named Authority for Dispatch. In autonomous mode, Coordinator waits `dispatch.autonomous.delayMinutes` (default 30) after the `triaged` label was applied — measured from the GitHub event timeline — before consulting `dispatch/*` and applying the corresponding Trigger label. The delay is the human Veto window: removing `dispatch/*`, applying `looper:hold`, or applying the Trigger label manually all block autonomous Dispatch.

Complexity (`complexity/*`) becomes informational only — it appears on the Triage comment and as a label for human readers, but no automation reads it.

## Considered Options

- **Complexity → dispatch via config mapping** (the original design). Rejected after council review: it builds an inference layer on top of the agent's structured output, which `AGENTS.md` explicitly warns against. Made the dispatch decision a function of (LLM complexity, config policy) rather than of (LLM intent).
- **Same-tick dispatch** — Triage and Dispatch in one tick, no grace window. Rejected because it gives humans no veto opportunity between LLM judgment and Planner/Worker kickoff.
- **Next-tick dispatch with no explicit delay** — relies on `pollInterval`. Rejected because operational tuning (shortening `pollInterval` for snappy slash-command response) would silently shrink the human veto window.

## Consequences

- Coordinator's Triage prompt and structured-output schema must enforce exactly one `dispatch/*` value. Zero or multiple values are strict-parse failures and trigger retry next tick.
- Querying "when was `triaged` applied" requires one extra GitHub timeline API call per autonomous-mode candidate Issue. Cost is bounded by the per-tick cap and the small set of Issues with `triaged` + `dispatch/*` + no Trigger label.
- Humans veto autonomous Dispatch by removing the `dispatch/*` label or applying `looper:hold`. Both are durable, public, and reversible.
- This is the Authority chain `AGENTS.md` mandates oracle review for. The chain is: LLM emits structured `dispatch/*` → label durably commits to GitHub → grace window elapses → no Veto signal observed → Trigger label applied.
