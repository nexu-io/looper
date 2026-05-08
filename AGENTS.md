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
- Cross-compilation tested for linux/amd64, darwin/arm64, windows/amd64.

## Key packages

- `internal/platform/` — OS abstractions (process mgmt, signals, process info).
- `internal/infra/notify/` — platform notification providers (darwin osascript, linux notify-send, windows powershell toast).
- `internal/config/` — config loading, validation, types.
- `internal/cliapp/` — CLI runtime, daemon supervision (launchd/systemd/windows-service).

## Supervision modes

- `DaemonModeLaunchd` — macOS only (runtime check).
- `DaemonModeSystemd` — Linux only (runtime check).
- `DaemonModeWindowsService` — Windows only (runtime check).
- `DaemonModeForeground` — detached process, no supervision (all platforms).
- All supervision methods have runtime `r.platform() != "..."` guards, so they compile on all platforms.

## Platform-specific files

- `internal/cliapp/daemon_supervision.go` — shared lifecycle types and launchd code (no build tag).
- `internal/cliapp/supervisor_systemd.go` — systemd supervision (no build tag; runtime `platform() != "linux"` guard).
- `internal/cliapp/supervisor_winsvc.go` — Windows Service supervision (no build tag; runtime `platform() != "windows"` guard).
- Named `supervisor_winsvc.go` (not `_windows.go`) to avoid implicit Go build constraints.
- `internal/config/access_unix.go` / `access_windows.go` — portable write-access check.

## Review guidelines

- Report every issue found. Do not prioritize, triage, or omit.
- Continue reviewing after finding issues. Early termination is a defect.
- Review systematically across correctness, performance, maintainability, and style.
