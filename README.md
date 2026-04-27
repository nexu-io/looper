# looper

Looper is a Go-based CLI + daemon for planner / reviewer / fixer / worker workflows.

This repository contains:

- `cmd/looperd` — the supported daemon binary
- `cmd/looper` — the supported CLI binary
- `internal/` and `pkg/` — the active Go implementation

## Installation

Quick start:

```bash
curl -fsSL https://raw.githubusercontent.com/powerformer/looper/main/scripts/install.sh | sh
looper bootstrap --yes --project-path /path/to/repo --agent-vendor opencode
```

Detailed install, upgrade, uninstall, requirements, and source-build instructions: [docs/installation.md](docs/installation.md).

## Usage

For the user-facing workflow guide, see [docs/users-guide.md](docs/users-guide.md).

Typical flow:

```bash
looper plan --project myproj --issue 123
looper review owner/repo#42 --loop
looper work --issue 123
```

Notes:

- when you run commands inside a registered repo, `looper` can often infer the current project, so `--project` is often optional for `review` and `work`
- use `--project` when the current directory is outside the repo, or when multiple projects could match
- for the full planner / reviewer / fixer / worker flow, GitHub label usage, and assign-based automation, see the guide above

## Common commands

- `looper bootstrap`
- `looper status`
- `looper version`
- `looper project list|add`
- `looper plan --project <id> --issue <number>`
- `looper review <pr> [--loop]`
- `looper work --issue <number>`
- `looper pr list|show|status`
- `looper daemon install|start|stop|restart|status`
- `looper ps`, `looper logs <id>`, `looper jump <id>`
- `looper stop <id>`

## Configuration

For configuration details, defaults, environment overrides, CLI flags, auth notes, and troubleshooting, see [docs/configuration.md](docs/configuration.md).

Useful pointers:

- default config path: `~/.looper/config.json`
- config precedence: defaults → config file → environment → CLI flags
- if daemon auth is set to `local-token`, the CLI uses `LOOPER_TOKEN`

## Development commands

From the repo root:

- `go run ./cmd/looperd`
- `go run ./cmd/looper <args>`
- `go build ./...`
- `go vet ./...`
- `go test ./...`

## Runtime notes

- `looperd` fails fast on invalid config
- runtime paths must be writable
- if `notifications.osascript.enabled` is `true`, `osascript` must resolve
- managed daemon installs live at `~/.looper/bin/looperd`
- default daemon-managed worktrees live under `~/.looper/worktrees/<project-id>/`

## Development notes

- build output lives in `dist/`; do not edit generated files
