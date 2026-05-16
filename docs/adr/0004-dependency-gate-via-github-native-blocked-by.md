# Dependency gate authority is GitHub-native blocked_by plus close state

## Context

Coordinator already performs Triage and Dispatch using durable, public GitHub state. ADR-0002 established that Dispatch must read an explicit Authority rather than infer intent from ephemeral agent output. The dependency-aware Dispatch PRD extends that rule: before Coordinator may Dispatch a Triaged Issue, it must have a named Authority for whether upstream work is complete.

Per `AGENTS.md`'s "Name the authority before enforcing it" rule, the question is: *what is the Authority for "Issue may be Dispatched," and why is it not the agent's own structured output?* For this slice, the Authority is GitHub-native issue dependencies (`blocked_by`) plus the blocker Issue's durable close state (`state` + `state_reason`). That state is public, inspectable, human-vetoable, and already lives on the Issue record Coordinator reads.

## Decision

Coordinator's dependency gate will treat GitHub-native `blocked_by` as the source of dependency structure, and blocker close state as the source of dependency satisfaction.

- `state==closed` and `state_reason==completed` is the only satisfied blocker state.
- `state==closed` with `state_reason==not_planned` or `state_reason==duplicate` is not satisfied and requires re-triage.
- `state==open` remains blocking.
- Missing blocker reads are treated as unreachable dependencies, not as satisfied work.

This keeps the Authority chain durable and human-visible: a human or agent links dependencies in GitHub, a human or agent closes blocker Issues with a durable reason, and Coordinator later reads those public records before Dispatch.

## Considered Options

- **`blocked-by:` label convention** — encode dependencies in labels. Rejected because labels do not express ordered dependency relationships well, become noisy on multi-blocker Issues, and would create a parallel Authority separate from GitHub's native dependency model.
- **Markdown regex in the Issue body** — parse dependency references from prose or checklists. Rejected because it turns Dispatch into inference over unstructured text rather than reading a durable structured Authority.
- **Private dependency cache** — mirror dependency edges and blocker state into Looper-managed storage. Rejected because it adds a second source of truth, drift risk, and recovery logic for data GitHub already persists publicly.

## Consequences

- Later Dispatch slices must read `blocked_by`, `state`, and `state_reason` directly from GitHub before applying a Trigger label.
- `not_planned` and `duplicate` do not silently unblock Dispatch; they become explicit re-triage cases so humans can revisit Disposition and dependency structure.
- Coordinator remains stateless with respect to dependency Authority. The graph used for Dispatch can be rebuilt from current GitHub state each tick.
- This follows the same pattern as ADR-0002: the agent may propose structure, but Dispatch acts only on durable GitHub Authority that humans can inspect and veto.
