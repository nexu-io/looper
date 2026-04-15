# looper

Looper is a Bun workspace with three apps:

- `apps/looperd` — the main daemon and HTTP API server
- `apps/cli` — the `looper` command-line client
- `apps/web` — a placeholder web app

The current product is the daemon + CLI. The web app is not implemented yet.

## Requirements

For the recommended install path:

- macOS (`darwin-arm64` or `darwin-x64`)
- Node.js/npm for installing `looper`

For source development:

- [Bun](https://bun.sh/) `1.3.12`
- `git`
- `gh`
- `osascript` if macOS notifications stay enabled

`looperd` auto-detects tool paths with `Bun.which()`, but startup validation fails if required tools cannot be resolved.

## Installation

Looper now uses a split distribution model:

- `looper` CLI is installed with npm
- `looperd` daemon is installed separately as a managed macOS binary

Linux daemon artifacts are not supported in this phase.

### Install the CLI

```bash
npm install -g @powerformer/looper
```

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

### From source

If you want to develop from source, clone the repo and install workspace dependencies from the root:

```bash
git clone https://github.com/powerformer/looper.git
cd looper
bun install
```

Then start the daemon from source:

```bash
bun run dev
```

In another shell, run the CLI from source:

```bash
bun run looper -- status
```

### Compatibility and version policy

- CLI and daemon are published from the same git tag and should normally share the same version.
- Short-lived version skew is allowed when the HTTP API remains compatible; the current expectation is that newer CLI builds should keep working with same-major daemons.
- Management endpoints stay under `/api/v1/*` in the current phase, and minor releases should not introduce breaking protocol changes.
- If the daemon is running, the CLI reads its current version from `/api/v1/status`; otherwise it falls back to `looperd --version`.
- `looper upgrade --check` reads the latest CLI version from npm registry metadata and the latest daemon version from GitHub Releases metadata. If the daemon is not running, the CLI falls back to the installed binary version; if no binary is found, daemon current version is reported as not installed.
- The CLI does not currently inject upgrade prompts into every command when the daemon is old; use `looper upgrade --check` to inspect drift and `looper upgrade --daemon` to update the managed binary.
- Full major-version upgrade confirmation is not implemented in this phase because full `looper upgrade` is not implemented yet. If a future release needs breaking management API changes, it should move to a new API version such as `/api/v2/*`, and major-version upgrade confirmation can be added there.
- If a breaking management API change is ever needed, it should move to a new API version such as `/api/v2/*` instead of silently breaking `/api/v1`.


## Workspace commands

From the repo root:

- `bun run dev` — run `apps/looperd`
- `bun run looper -- <args>` — run the CLI directly from `apps/cli/src/index.ts` without rebuilding
- `bun run build` — build `apps/looperd`, `apps/cli`, and `apps/web`
- `bun run typecheck` — TypeScript project references check without emit
- `bun run lint` — run Biome
- `bun run test` — run all Bun tests

Package-scoped commands:

- `bun run --cwd apps/looperd dev|build|typecheck`
- `bun run --cwd apps/cli dev|build|typecheck|test`
- `bun run --cwd apps/web dev|build|typecheck`

Focused tests:

- `bun test apps/looperd/src/config/load.test.ts`
- `bun test tests/smoke.test.ts`

## Project structure

### `apps/looperd`

Daemon entry flow:

`src/index.ts` → `bootstrapLooperd()` → runtime → Bun HTTP API server + SQLite store

Responsibilities include:

- loading and validating config
- starting the SQLite-backed runtime
- starting the Bun HTTP API server
- recovery on startup
- writing logs and notifications

### `apps/cli`

The CLI binary is `looper`.

For local development, use one of these options:

- `bun run looper -- ps` — runs the CLI from source without rebuilding
- add a shell alias such as `alias looper='bun /absolute/path/to/looper/apps/cli/src/index.ts'` if you want to type `looper ps` directly during development

Published installs should use the built `dist` entry declared in `apps/cli/package.json` and install the daemon separately via `looper daemon install`.

The CLI connects to `looperd` over HTTP and supports commands under:

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

### `apps/web`

Currently a stub that only logs a placeholder message.

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
- `LOOPER_BUN_PATH`
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
- `--bun-path`
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

- This repo uses TypeScript project references from the root `tsconfig.json`; root typecheck runs with `--noEmit`.
- Formatting and linting use Biome with spaces.
- Build output lives in `apps/*/dist/`; do not edit generated files.
