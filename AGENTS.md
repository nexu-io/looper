# AGENTS.md

## Commands

- Supported implementation is Go-first. Root commands are the source of truth:
  - `go run ./cmd/looperd`
  - `go run ./cmd/looper <args>`
  - `go build ./...`
  - `go vet ./...`
  - `go test ./...`

## Repo structure

- `cmd/looperd` — supported `looperd` daemon entrypoint.
- `cmd/looper` — supported `looper` CLI entrypoint.
- `internal/` and `pkg/` — active Go implementation packages.

## Configuration & runtime

- Default daemon config path: `~/.looper/config.json`.
- Precedence: defaults → config file → env → CLI flags.
- looperd fails fast on config-validation errors and requires writable runtime paths.
- Tool paths (`git`, `gh`, `osascript`) are auto-detected unless explicitly configured.
- When `notifications.osascript.enabled` is true, `osascript` must resolve or startup fails.
- Default runtime artifacts: `~/.looper/` (`looper.sqlite`, `backups/`, `logs/`).

## Conventions

- Build output lives in `dist/`; do not edit generated files.
- CI (`.github/workflows/ci.yml`) runs on PR updates: `gofmt -l .` → `go vet ./...` → `go test ./...` → `go build ./...`.
- Commit messages and PR titles must use semantic prefixes, for example `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`, or `ci:`.

## Review guidelines

- Report every issue found. Do not prioritize, triage, or omit.
- Continue reviewing after finding issues. Early termination is a defect.
- Review systematically across correctness, performance, maintainability, and style.
- Treat missing regression coverage for a P0/P1 bug fix as a review blocker.
- For cross-component lifecycle, worktree, GitHub command, daemon boot, and resolve-comments regressions, prefer contract/invariant integration coverage over unit-only tests.
- Escalate only real GitHub auth, scope, thread mutation, or rate-limit regressions to sandbox E2E.

## Design guidelines

### New concepts require an explicit trade-off

Introducing a new concept (evidence record, content hash, lock, ledger, status field, intermediate state) must be justified in the PR description with:

- the concrete failure it prevents
- what it costs: new edge cases, new persisted state, new failure modes, new code paths to keep in sync
- why a simpler alternative (delete a layer, trust agent output, fail loud) is insufficient

If the cost section is empty or hand-waved, the concept is not ready to merge. Validation and gates are not free — each one expands the surface where reality and the model can disagree.

### Name the authority before enforcing it

Any PR that adds a gate, validation step, persisted field, or "verify before acting" check on an agent-driven action must answer in its description, in one sentence:

> What is the authority for this action, and why is it not the agent's own structured output?

If the honest answer is "the agent's output, but we don't trust it", make the agent's output more structured instead of building an inference layer on top of infra state. Infra signals are for drift detection, not for authority.

### A second fix to the same subsystem is a revert signal

If a subsystem receives a second `fix:` PR shortly after the first, default to reverting the first and redesigning, not stacking another patch. Near-identical `fix:` titles on the same area are a hard signal that the underlying abstraction is wrong.

### Prefer deletion over another layer

Before adding durability, recovery, persistence, confirmation, or a new gate, first attempt the opposite direction: can a layer be removed to make the problem disappear? Record the result in the PR description, even if the conclusion is "no". A strongly positive net diff on a path that has already been patched is suspect by default.

### Mandatory oracle review for authority-bearing changes

PRs on agent-driven paths must be reviewed by `@oracle` before merge if they introduce:

- a new schema field on a checkpoint, repair record, or persisted run state
- a new gate or precondition on a side-effecting action (push, resolve, reply, comment, mutate)
- a new "trust this signal over the agent" inference

### Test-file growth is a design smell

A single PR that adds more than 300 lines to one `*_test.go` file for a runner or subsystem must trigger an `@oracle` design review before merge. Explosive test growth usually means an internal state machine is being propped up rather than simplified.
