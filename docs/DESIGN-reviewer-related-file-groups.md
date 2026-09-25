# Reviewer related-file groups

## Problem

A single reviewer context on a large PR can drop cross-file contracts. The builtin review skill already asks one agent to plan related files. This design adds an **opt-in sequential** execution split when the changed-file count is high. Its functional guarantees do not establish a model-quality improvement; that evaluation belongs to the separate evaluation system.

## Authority

- Scope comes from the fixed `base_sha`/`head_sha` plus existing reviewer scope. The group list is work assignment only.
- Findings and dispositions come from agent structured output.
- Publish stays on the existing top-level wrapper / comment-only path. Subtasks cannot call `review submit`.
- Git state and the live PR head detect source drift; hold labels represent user control. These checks do not infer review completeness or override agent findings.

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

The runner checks the live hold state and expected PR head before grouping, between groups, and before starting the final reviewer. A changed remote head restarts discovery. Each agent receives its own timeout budget; a caller's deadline or cancellation still bounds the whole operation.

The local Git check detects a changed checkout and Git-visible tracked/untracked changes after each group. It is not a filesystem write audit: ignored build output and caches are outside that check. The no-write prompt remains an agent instruction, not an OS sandbox guarantee. This feature does not add filesystem baselines, hashes, cleanup, or ignored-file gates.

If any subtask fails, times out, or source drift is detected, the grouped pass stops before the final publisher starts. There is no per-group resume ledger; retry the whole pass. Group executions always start fresh. Native resume selects the latest non-group execution from existing execution metadata, so persisted group records cannot hide a pending main-review session after a restart. Vendor and recoverability checks still apply; a newer completed main review supersedes an older pending session.

After subtasks succeed, the top-level reviewer still runs once with the existing publish contract, the group plan, subtask findings to merge/dedupe, and a required cross-group contract check. Both the full prompt and native-resume prompt receive the current group results. Directory identifiers and path arrays are JSON-encoded. Repair-frontier later passes group the frontier delta, not the original full diff.

## Why not delete this layer

Deleting execution grouping and relying on the skill's related-file plan is the default path and removes all subtask overhead. It does not split work into separate model contexts for operators who need that option. Explicit groups cost extra model calls, sequential latency, repeated work after failure, and a final merge/deduplication pass; they stay off until an operator opts in. This implementation removes the temporary session-ID bridge and uses existing execution records instead of adding a recovery ledger.

## Out of scope

Daemon queues, per-group databases, concurrent worktree writes, comprehensive filesystem isolation/auditing, multiple remote reviews, merge gates, and model-quality datasets/replay/scoring.
