# AGENTS.md

## Rules

- After every code change, run `biome format` on each modified file supported by Biome before finishing.

## Commands

- Supported implementation is Go-first. Root commands are the source of truth:
  - `go run ./cmd/looperd`
  - `go run ./cmd/looper -- <args>`
  - `go build ./...`
  - `go vet ./...`
  - `go test ./...`
  - `bun x @biomejs/biome check .`
- The TypeScript directories under `apps/` are archived legacy snapshots. Do not treat them as active build targets unless the task explicitly requires legacy-reference work.

## Repo structure

- `cmd/looperd` — supported `looperd` daemon entrypoint.
- `cmd/looper` — supported `looper` CLI entrypoint.
- `internal/` and `pkg/` — active Go implementation packages.
- `apps/looperd` — archived TypeScript daemon snapshot.
- `apps/cli` — archived TypeScript CLI snapshot.
- `apps/web` — archived placeholder TypeScript package.

## Configuration & runtime

- Default daemon config path: `~/.looper/config.json`.
- Precedence: defaults → config file → env → CLI flags.
- looperd fails fast on config-validation errors and requires writable runtime paths.
- Tool paths (`git`, `gh`, `osascript`) are auto-detected unless explicitly configured.
- When `notifications.osascript.enabled` is true, `osascript` must resolve or startup fails.
- Default runtime artifacts: `~/.looper/` (`looper.sqlite`, `backups/`, `logs/`).

## Conventions

- Formatting and linting use Biome (spaces). Make Biome-compatible edits only; do not hand-format to another style.
- Build output lives in `dist/` and archived `apps/*/dist/`; do not edit generated files.
- CI (`.github/workflows/ci.yml`) runs on PR updates: `gofmt -l .` → `go vet ./...` → `go test ./...` → `go build ./...`.

## Review guidelines

- Report every issue found. Do not prioritize, triage, or omit.
- Continue reviewing after finding issues. Early termination is a defect.
- Review systematically across correctness, performance, maintainability, and style.
