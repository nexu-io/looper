# Acceptance Notes

## Simulated flow coverage

The v1 same-PR flow is covered by the following automated tests:

- `bun test apps/looperd/src/server/index.test.ts`
  - verifies manual planner API creation and planner visibility in status output
- `bun test apps/cli/src/index.test.ts`
  - verifies manual `looper plan --project ... --issue ...` triggering
- `bun test apps/looperd/src/reviewer/index.test.ts`
  - verifies `looper:spec-reviewing` label-based discovery
  - verifies clean spec review promotion from `looper:spec-reviewing` to `looper:spec-ready`
- `bun test apps/looperd/src/fixer/index.test.ts`
  - verifies fix/recheck/recovery behavior still works after label-aware changes
- `bun test apps/looperd/src/worker/index.test.ts`
  - verifies `looper:spec-ready` discovery
  - verifies worker `pull_request` mode reuses the existing PR branch, removes `looper:spec-ready`, reads `Spec: ...` from the PR body, validates, pushes, and re-requests reviewers without opening a second PR
- `bun test apps/looperd/src/runtime/index.test.ts`
  - verifies runtime routing for planner/reviewer/fixer/worker paths

## Verification commands

```sh
bun run typecheck
bun test apps/looperd/src/reviewer/index.test.ts apps/looperd/src/worker/index.test.ts apps/looperd/src/server/index.test.ts apps/looperd/src/runtime/index.test.ts apps/looperd/src/fixer/index.test.ts apps/looperd/src/projects/index.test.ts apps/cli/src/index.test.ts
```

## Acceptance summary

- Issue intake is manually triggerable through API/CLI and planner loops are visible in status output.
- Spec review is driven by `looper:spec-reviewing` and clean reviews promote to `looper:spec-ready`.
- Worker discovery is driven by `looper:spec-ready` and continues implementation on the same PR branch.
- Implementation review falls back to the normal reviewer/fixer loop after the worker re-requests reviewers.
