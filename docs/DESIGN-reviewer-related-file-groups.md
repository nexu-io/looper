# Reviewer related-file groups

## Problem

A single reviewer context on a large PR can drop cross-file contracts. PR 4 already asks one agent to plan related files. This design adds an **opt-in sequential** execution split when the changed-file count is high.

## Authority

- Scope comes from the fixed `base_sha`/`head_sha` plus existing reviewer scope. The group list is work assignment only.
- Findings and dispositions come from agent structured output.
- Publish stays on the existing top-level wrapper / comment-only path. Subtasks cannot call `review submit`.

## Enablement

`roles.reviewer.behavior.relatedFileGroups.enabled` defaults **false**.
`minChangedFiles` defaults **24** (provisional).

Small PRs and the default config keep the current single-agent path. Disable grouping by setting `enabled=false`.

## Grouping

`git diff --name-status base...head` in the prepared worktree enumerates every changed path, including deletions. Each path belongs to exactly one group:

- Go files share a group by directory (package + `_test.go`)
- Remaining paths go to an `other` group

Each subtask receives its group paths plus the list of other changed files.

## Execution

Subtasks run **sequentially** on the same prepared worktree. They must not move checkout or write the worktree. Each returns `__LOOPER_RESULT__` findings only.

If any subtask fails, times out, or the head drifts, the whole grouped review fails and nothing is published. There is no per-group resume ledger; retry the whole pass.

After subtasks succeed, the top-level reviewer still runs once with the existing publish contract, the group plan, subtask findings to merge/dedupe, and a required cross-group contract check. Repair-frontier later passes group the frontier delta, not the original full diff.

## Why not delete this layer

A skill-only related-file plan (PR 4) does not bound context. Explicit groups cost extra prompts and failure propagation; they stay off until an operator opts in.

## Out of scope

Daemon queues, per-group databases, concurrent worktree writes, multiple remote reviews, merge gates.
