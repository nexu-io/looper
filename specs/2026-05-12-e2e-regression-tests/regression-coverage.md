# Regression Coverage Tracking

## Policy

- Every P0/P1 bug fix must land with a regression test.
- Cross-component lifecycle, worktree, GitHub command, daemon boot, and resolve-comments regressions should prefer contract / invariant integration coverage.
- Real GitHub auth, scope, review-thread mutation, and rate-limit regressions should escalate to sandbox E2E.
- Missing regression coverage for a P0/P1 fix is a review blocker.

## Historical coverage map

| Incident | Regression coverage | Layer | Notes |
| --- | --- | --- | --- |
| PR #255 introduced unsupported `gh --json` field | `internal/e2e/githubcontract/contract_test.go` | contract / invariant integration | Summary-list field allowlist regression |
| PR #261 fixed author-association fallback | `internal/e2e/githubcontract/contract_test.go` | contract / invariant integration | Detail fallback regression |
| PR #194 broke worktree isolation / cwd safety | `internal/e2e/invariant_worktree_test.go` | contract / invariant integration | Fresh schedule, reuse, bad checkpoint coverage |
| resolve-comments stale head / no-push / no-new-commit regressions | `internal/e2e/resolve_comments_scenarios_test.go` | contract / invariant integration | Stale head, no-push rerun, unresolved-thread protections |
| Real GitHub worker/fixer/thread/auth regressions | `internal/e2e/github_sandbox_test.go` | real sandbox E2E | App token, review thread mutation, no-diff behavior |

## New regressions

Add one row per new P0/P1 fix with:

| PR / Issue | Test file | Layer | Comment anchor |
| --- | --- | --- | --- |
| _fill in_ | _fill in_ | _unit / contract / sandbox_ | `// Regression for PR #...` |
