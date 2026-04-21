# looper

Looper ships as Go binaries for the supported CLI + daemon workflows.

This repository currently contains:

- `cmd/looperd` — the supported `looperd` daemon binary
- `cmd/looper` — the supported `looper` CLI binary
- `internal/` and `pkg/` — the active Go implementation

The current product is the Go daemon + CLI.

## Requirements

For the default supported install path:

- macOS (`darwin-arm64` or `darwin-x64`)
- `git`
- `gh`

For source development:

- Go `1.22`
- `git`
- `gh`
- `osascript` if macOS notifications stay enabled

`looperd` auto-detects tool paths from `PATH`, but startup validation fails if required tools cannot be resolved.

## Installation

Looper now uses Go binaries as the default supported implementation:

- install the `looper` CLI from GitHub Releases
- install `looperd` separately as a managed macOS binary with `looper daemon install`

GitHub Releases publish standalone Go binaries for both `looper` and `looperd` on `darwin-arm64` and `darwin-x64`.

Linux daemon artifacts are not supported in this phase.

### Install the CLI

Recommended path:

1. Download the matching `looper` release artifact for your macOS architecture from GitHub Releases.
2. Rename it to `looper` if needed.
3. Place it on your `PATH`, for example `/usr/local/bin/looper` or `~/.local/bin/looper`.

### Install the daemon

Recommended path:

```bash
looper daemon install
```

This command:

- detects the current macOS architecture
- downloads the matching GitHub Release artifact
- installs it to `~/.looper/bin/looperd`

Current release binaries are unsigned. If macOS Gatekeeper blocks the first launch, you may need to allow the binary manually in System Settings.

Manual fallback:

- download the matching `looperd` release artifact yourself
- place it at `~/.looper/bin/looperd` or somewhere on your `PATH`

Daemon lookup order is fixed to `~/.looper/bin/looperd`, then `$PATH`.

### Start the daemon

```bash
looper daemon start
```

Phase 1 process management is intentionally minimal. `looper daemon start` writes a pid file and launches the daemon, but it does not provide full background supervision.

### Verify the install

In another shell, verify the install and daemon connection:

```bash
looper status
looper daemon status
```

### Upgrade

Unified upgrade entrypoint:

```bash
looper upgrade --check
looper upgrade --daemon
```

Current phase behavior:

- `looper upgrade --check` shows current/latest CLI and daemon versions
- `looper upgrade --daemon` installs or upgrades the managed daemon binary
- full `looper upgrade` for CLI + daemon together is not implemented yet
- after a daemon upgrade, restart manually with `looper daemon restart`
- replace the `looper` CLI binary manually when a newer GitHub Release is available

### From source

If you want to develop the supported Go binaries from source, clone the repo:

```bash
git clone https://github.com/powerformer/looper.git
cd looper
```

Then build or run the Go binaries:

```bash
go build ./cmd/looper
go build ./cmd/looperd
go run ./cmd/looperd
```

In another shell, run the CLI from source:

```bash
go run ./cmd/looper -- status
```

### Compatibility and version policy

- CLI and daemon are published from the same git tag and should normally share the same version.
- Short-lived version skew is allowed when the HTTP API remains compatible; the current expectation is that newer CLI builds should keep working with same-major daemons.
- Management endpoints stay under `/api/v1/*` in the current phase, and minor releases should not introduce breaking protocol changes.
- If the daemon is running, the CLI reads its current version from `/api/v1/status`; otherwise it falls back to `looperd --version`.
- `looper upgrade --check` reads the latest CLI and daemon versions from GitHub Releases metadata. If the daemon is not running, the CLI falls back to the installed daemon binary version; if no binary is found, daemon current version is reported as not installed.
- The CLI does not currently inject upgrade prompts into every command when the daemon is old; use `looper upgrade --check` to inspect drift and `looper upgrade --daemon` to update the managed binary.
- Full major-version upgrade confirmation is not implemented in this phase because full `looper upgrade` is not implemented yet. If a future release needs breaking management API changes, it should move to a new API version such as `/api/v2/*` instead of silently breaking `/api/v1`.


## Development commands

From the repo root:

- `go run ./cmd/looperd`
- `go run ./cmd/looper -- <args>`
- `go build ./...`
- `go vet ./...`
- `go test ./...`

## Project structure

### `cmd/looperd`

Supported daemon entrypoint. Responsibilities include:

- loading and validating config
- starting the SQLite-backed runtime
- serving the HTTP API
- recovery on startup
- writing logs and notifications

### `cmd/looper`

Supported CLI entrypoint. The CLI connects to `looperd` over HTTP and supports commands under:

- `project list|add`
- `ps`
- `jump`
- `logs`
- `stop`
- `status`
- `plan`
- `config show`
- `daemon install|start|restart|status|logs`
- `loop list|start|pause`
- `review <pr> [--loop]`
- `upgrade`
- `work`
- `pr list|show|status`
- `run list`

Manual review examples:

- `looper review 123` — create a one-shot reviewer task for PR `123` in the current project
- `looper review powerformer/looper#123 --loop` — keep re-reviewing that PR as new commits are pushed

## Configuration

For a full configuration guide with examples, field reference, env overrides, and CLI flags, see [docs/configuration.md](docs/configuration.md).

Default config path:

- `~/.looper/config.json`

Config precedence:

1. built-in defaults
2. config file
3. environment variables
4. CLI flags

Important defaults verified in code:

- host: `127.0.0.1`
- port: `4310`
- storage DB: `~/.looper/looper.sqlite`
- backup dir: `~/.looper/backups`
- log dir: `~/.looper/logs`
- daemon mode: `foreground`
- agent vendor: `opencode`
- base branch: `main`

Selected looperd environment overrides:

- `LOOPER_CONFIG`
- `LOOPER_HOST`
- `LOOPER_PORT`
- `LOOPER_DB_PATH`
- `LOOPER_LOG_DIR`
- `LOOPER_DAEMON_MODE`
- `LOOPER_WORKING_DIRECTORY`
- `LOOPER_GIT_PATH`
- `LOOPER_GH_PATH`
- `LOOPER_OSASCRIPT_PATH`
- `LOOPER_OSASCRIPT_ENABLED`
- `LOOPER_IN_APP_NOTIFICATIONS`
- `LOOPER_ALLOW_AUTO_COMMIT`
- `LOOPER_ALLOW_AUTO_PUSH`
- `LOOPER_ALLOW_AUTO_APPROVE`

CLI-only environment override:

- `LOOPER_TOKEN` — auth token sent by the CLI when talking to a token-protected daemon

Selected CLI config flags:

- `--config`
- `--host`
- `--port`
- `--db-path`
- `--log-dir`
- `--daemon-mode`
- `--git-path`
- `--gh-path`
- `--osascript-path`
- `--allow-auto-commit`
- `--allow-auto-push`
- `--allow-auto-approve`

## Runtime notes

- `looperd` fails fast on invalid config.
- Runtime paths must be writable.
- If `notifications.osascript.enabled` is `true`, `osascript` must resolve.
- Managed daemon installs live at `~/.looper/bin/looperd`.
- Default daemon-managed worktrees now live under `~/.looper/worktrees/<project-id>/`; if you still have legacy repo-local `.looper-worktrees/` entries, prune any stale `.git/worktrees/*/gitdir` references before deleting those old directories.
- SQLite migrations are embedded in the daemon binary/build output for normal runtime execution; directory-based migration loading is only used in explicit test/injection paths.

## Development notes

- Build output lives in `dist/`; do not edit generated files.
