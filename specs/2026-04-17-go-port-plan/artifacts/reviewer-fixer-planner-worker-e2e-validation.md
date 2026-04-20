# Reviewer/fixer/planner/worker flow validation

Date: 2026-04-21

This validation closes the Ralph task for end-to-end validation of the Go reviewer, fixer, planner, and worker flows that still remain in scope for the rewrite.

## Commands run

- `go test ./internal/planner -count=1`
- `go test ./internal/reviewer -count=1`
- `go test ./internal/fixer -count=1`
- `go test ./internal/worker -count=1`
- `go test ./internal/cliapp -run TestInProcessSmokeWorkerWorkflowSucceedsWithManualPROpeningAndMutatesWorktree -count=1`

All five commands passed.

## Coverage confirmed

- `internal/planner/runner_test.go`
  - eligible issue discovery creates planner loops and queue items
  - happy-path spec worktree/agent/push/publish flow completes and records PR metadata
  - publish retry resumes from checkpoint without rerunning prior agent work
  - write-spec agent failure leaves the loop and queue in retryable states
  - planner worktree-root fallback matches default project behavior
- `internal/reviewer/runner_test.go`
  - PR discovery creates reviewer loops and queue items
  - publish retry resumes from checkpoint without rerunning review generation
  - head changes before publish restart from discovery and review the new head
  - structured review output strips the completion marker before parsing
- `internal/fixer/runner_test.go`
  - PR discovery selects only actionable review feedback
  - successful repair flow covers worktree prep, repair, validation, push, resolve-comments, and recheck
  - remote-head change at push restarts from discovery and rebuilds the worktree
  - auto-push-disabled path exits cleanly without pushing
  - validation shell-command success/failure behavior remains intact
  - fixer worktree-root fallback matches default project behavior
- `internal/worker/runner_test.go`
  - worker processing ignores non-worker queue items
  - create-PR flow completes successfully with PR metadata persisted
  - retryable open-PR failure resumes from the publish checkpoint without rerunning agent execution
  - validation failure pauses the loop for manual intervention
  - PR-targeted worker loops still require an explicit spec path
  - existing-PR reuse after push remains supported
  - validation shell-command success/failure behavior remains intact
- `internal/cliapp/app_test.go`
  - in-process runtime + HTTP API + CLI smoke path still drives a real worker run against a temporary git repo
  - confirms daemon/runtime startup, project registration, queue-to-run execution, successful completion, and expected worktree mutation under the manual-PR-opening path

## Notes

- The worker flow continues to have the broadest in-process smoke coverage because it is directly user-invoked through the CLI/API surface.
- Planner, reviewer, and fixer are scheduler-internal automation lanes, so their in-scope end-to-end validation is represented by full runner-package flows that exercise discovery, queue claiming, checkpoint/resume behavior, state persistence, and adapter interactions against a real SQLite-backed test coordinator.
- The separate install/upgrade validation task remains open and is not covered by this artifact.

## Result

The Go rewrite now has repeatable validation coverage for all in-scope planner, reviewer, fixer, and worker orchestration lanes, with worker additionally proven through the in-process CLI/runtime smoke path.
