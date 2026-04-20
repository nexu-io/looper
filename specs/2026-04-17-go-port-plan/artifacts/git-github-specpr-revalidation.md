# Git/GitHub/spec-PR re-validation

Date: 2026-04-20

This re-validation closes the Phase 8 checklist item to confirm the Go rewrite still matches the contract-sensitive Git, GitHub, and spec-PR behaviors called out in the port plan.

## Commands run

- `go test ./internal/infra/git`
- `go test ./internal/infra/github`
- `go test ./internal/infra/specpr`
- `go test ./internal/worker`
- `go test ./internal/reviewer`
- `go test ./internal/fixer`

All six commands passed.

## Coverage confirmed

- `internal/infra/git/gateway_test.go`
  - worktree create/restore/cleanup behavior
  - detached vs attached checkout handling
  - protected-branch safeguards
  - commit author isolation and push behavior
- `internal/infra/github/gateway_test.go`
  - `gh` command wiring for PR list/view/create flows
  - inline review submission, comments, reactions, labels, reviewers, and review-thread resolution
  - permission and missing-resource error handling
- `internal/infra/specpr/specpr_test.go`
  - canonical label matching
  - spec-vs-implementation phase resolution
  - `Spec: ...` PR-body path parsing
  - unresolved-thread counting and clean-review detection
- `internal/worker/runner_test.go`
  - create-PR flow end to end through git/github adapters
  - resume-after-retry behavior around PR creation
  - existing-PR reuse after push
  - PR-targeted worker failure when no spec path is available
- `internal/reviewer` and `internal/fixer`
  - PR discovery and loop-creation paths that depend on GitHub/spec-PR state remain green in the Go implementation

## Result

The Go rewrite still exercises the expected contract-sensitive Git/GitHub/spec-PR behaviors described in `specs/2026-04-17-go-port-plan/spec.md` and `specs/2026-04-17-go-port-plan/artifacts/spec-pr-and-agent-completion.md`.
