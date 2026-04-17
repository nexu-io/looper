# Spec-PR label/path conventions and agent completion-marker inventory

Source of truth inspected from:

- `apps/looperd/src/infra/spec-pr.ts`
- `apps/looperd/src/infra/github.ts`
- `apps/looperd/src/planner/index.ts`
- `apps/looperd/src/reviewer/index.ts`
- `apps/looperd/src/worker/index.ts`
- `apps/looperd/src/infra/agent-prompt.ts`
- `apps/looperd/src/infra/agent.ts`
- `apps/looperd/src/planner/index.test.ts`
- `apps/looperd/src/reviewer/index.test.ts`
- `apps/looperd/src/fixer/index.test.ts`
- `apps/looperd/src/worker/index.test.ts`
- `specs/2026-04-12-issue-spec-pr-flow/spec.md`

## Spec-PR label conventions

- Canonical labels live in `apps/looperd/src/infra/spec-pr.ts:3-5`:
  - `looper:spec-reviewing`
  - `looper:spec-ready`
  - `looper:needs-human`
- Label matching is intentionally case/whitespace insensitive via `normalizeLabel()` and `hasLabel()` in `apps/looperd/src/infra/spec-pr.ts:9-18`.
- Pull-request phase inference is label-based in `apps/looperd/src/infra/spec-pr.ts:20-26`:
  - PRs with `looper:spec-reviewing` are treated as `spec`
  - all others are treated as `implementation`
- GitHub label metadata is also part of the current behavior surface in `apps/looperd/src/infra/github.ts:882-909`:
  - `looper:plan` → color `5319e7`, description `Picked up automatically by planner`
  - `looper:spec-reviewing` → color `1d76db`, description `Spec PR is under review`
  - `looper:spec-ready` → color `0e8a16`, description `Spec PR is ready for implementation`
  - `looper:needs-human` → color `d93f0b`, description `Looper requires manual intervention`

## Spec-PR lifecycle / handoff behavior

- Planner discovery is driven by issue label `looper:plan` in `apps/looperd/src/planner/index.ts:30-31`.
- Planner publish opens the spec PR, adds `looper:spec-reviewing`, and can request reviewers via the GitHub gateway defined in `apps/looperd/src/planner/index.ts:49-68`.
- Reviewer publish promotes a clean spec PR from `looper:spec-reviewing` to `looper:spec-ready` in `apps/looperd/src/reviewer/index.ts:1184-1204`.
- “Spec review clean” is defined by `isSpecReviewClean()` in `apps/looperd/src/infra/spec-pr.ts:55-62`:
  - unresolved review threads count must be `0`
  - review decision must not be `CHANGES_REQUESTED`
- Worker discovery is label-driven on `looper:spec-ready` in `apps/looperd/src/worker/index.ts:430-445`.
- When a PR-targeted worker run starts, it removes `looper:spec-ready` before continuing implementation in `apps/looperd/src/worker/index.ts:594-619`.
- `looper:needs-human` exists as a defined/colored fallback label, but current code in this inventory only defines the constant and GitHub label metadata; no automatic mutation path was found in the implementation sources above.

## Spec-path conventions

### Current implementation contract

- Planner generates spec paths as flat files under `specs/` using `apps/looperd/src/planner/index.ts:1374-1376`:
  - `specs/<yyyy-mm-dd>-<issue-number>-<slug>.md`
- Planner branch names are derived as `looper/planner/<issue-number>-<slug>` via `apps/looperd/src/planner/index.ts:1370-1372`.
- Planner PR bodies persist the spec path in a dedicated body line in `apps/looperd/src/planner/index.ts:1418-1437`:
  - `Spec: <spec-path>`
- Worker reuses that PR-body contract by parsing `Spec: ...` with `parseSpecPathFromPullRequestBody()` in `apps/looperd/src/infra/spec-pr.ts:28-37`.
- For PR-targeted worker runs, the effective spec path is resolved from explicit work input first, then from the PR body, in `apps/looperd/src/worker/index.ts:571-590`.
- Missing spec-path metadata is a `manual_intervention` condition for PR-targeted worker runs in `apps/looperd/src/worker/index.ts:586-590`.
- Tests lock in the current flat-file spec-path contract, e.g. `apps/looperd/src/planner/index.test.ts:351-351,488-493,639-639` and `apps/looperd/src/server/index.test.ts:665-682,879-911`.

### Existing design-doc expectations to preserve or reconcile

- The older issue-spec flow design doc still describes a nested spec path shape, `specs/<issue-number>-<slug>/spec.md`, in `specs/2026-04-12-issue-spec-pr-flow/spec.md`.
- That differs from the currently implemented/tested flat-file path shape `specs/<date>-<issue>-<slug>.md`.
- For the Go port, the implementation/tests are the current compatibility boundary unless a deliberate contract change is made.

## Agent completion-marker behavior

- The canonical completion marker is `__LOOPER_RESULT__` in `apps/looperd/src/infra/agent-prompt.ts:1-10`.
- All agent prompts are wrapped with `appendCompletionInstruction(...)`, which requires exactly one final stdout line in this format:
  - `__LOOPER_RESULT__={"summary":"<one-sentence summary>"}`
- Planner, reviewer, fixer, and worker all use the shared completion-marker instruction before launching agent work:
  - planner: `apps/looperd/src/planner/index.ts:1378-1404`
  - reviewer: `apps/looperd/src/reviewer/index.ts:1982-1996`
  - worker: `apps/looperd/src/worker/index.ts:1529-1565`
  - fixer: `apps/looperd/src/fixer/index.ts:2091-2101`
- Agent execution exports the same marker to subprocesses via `LOOPER_COMPLETION_MARKER` and parses the last marker line across stdout/stderr in `apps/looperd/src/infra/agent.ts:65-85,238-255,487-529`.
- Parse outcomes are part of the persisted/runtime contract in `apps/looperd/src/infra/agent.ts:247-294,487-529`:
  - `parseStatus: "parsed"` when the JSON payload is valid
  - `parseStatus: "missing"` when no marker line exists
  - `parseStatus: "invalid_json"` when the marker exists but the payload is not valid JSON
- A successfully parsed marker currently records:
  - `completionSignal: "__LOOPER_RESULT__="`
  - optional `summary`
  - optional `artifacts[]`
  - optional `changedFiles[]`
  - optional `commits[]`
- If parsing fails or the marker is missing, the daemon falls back to the last non-empty log line as the summary in `apps/looperd/src/infra/agent.ts:494-538`.
- Reviewer output handling explicitly strips completion-marker lines before interpreting or publishing review text in `apps/looperd/src/reviewer/index.ts:1764-1769`.
- Tests covering the marker contract live in:
  - `apps/looperd/src/infra/agent-prompt.test.ts:5-18`
  - `apps/looperd/src/infra/agent.test.ts:39-109`
  - loop-runner tests that persist parsed completion data in planner/reviewer/fixer/worker suites

## Go-port compatibility notes

- Preserve the exact spec-PR label names and their label-driven handoff semantics.
- Preserve the current implementation-level spec-path contract (`specs/<date>-<issue>-<slug>.md`) plus the `Spec: ...` PR-body metadata line unless intentionally changed.
- Preserve worker behavior that requires a resolved spec path before continuing on an existing PR.
- Preserve the exact agent completion marker token, final-line stdout contract, parse statuses, and fallback-summary behavior.
- Preserve reviewer-side stripping of completion-marker lines from published review content.
