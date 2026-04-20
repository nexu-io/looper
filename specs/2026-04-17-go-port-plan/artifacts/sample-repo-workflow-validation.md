# Sample-repo workflow validation

Date: 2026-04-20

This validation closes the Ralph task for end-to-end local workflow validation on a sample repo.

## Command run

- `go test ./internal/cliapp -run TestInProcessSmokeWorkerWorkflowSucceedsWithManualPROpeningAndMutatesWorktree -count=1`

The test passed.

## Coverage confirmed

- starts the Go runtime and HTTP API handler in process
- creates a temporary git-backed sample repo with an initial `main` branch commit
- exercises CLI/API paths for:
  - `looper status`
  - `looper config show`
  - `looper project add`
  - `looper project list`
  - `looper work`
  - `looper run list`
- runs a real worker workflow against the sample repo using the Go git gateway and a scriptable local agent command
- verifies the manual-PR-opening success path that is expected when `openPRStrategy=manual`
- verifies the resulting run reaches `success`
- verifies the generated worker worktree contains the expected file mutation (`smoke-output.txt`)

## Notes

- On this macOS host, direct execution of locally built Go binaries is blocked by a Go 1.22 Mach-O `LC_UUID` issue, so this task's sample-repo validation is codified as an in-process smoke test instead of a spawned `go run`/`go build` binary workflow.
- The separate install/upgrade validation task still remains for managed-binary distribution flows.

## Result

The Go implementation now has a repeatable sample-repo smoke path covering daemon/runtime startup, CLI/API wiring, project registration, and one run-producing worker workflow end to end.
