# looper

Looper is a Bun workspace with three apps:

- `apps/looperd` — the main daemon and HTTP API server
- `apps/cli` — the `looper` command-line client
- `apps/web` — a placeholder web app

The current product is the daemon + CLI. The web app is not implemented yet.

## Requirements

- [Bun](https://bun.sh/) `1.3.12`
- `git`
- `gh`
- `osascript` if macOS notifications stay enabled

`looperd` auto-detects tool paths with `Bun.which()`, but startup validation fails if required tools cannot be resolved.

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

Published installs should continue to use the built `dist` entry declared in `apps/cli/package.json`.

The CLI connects to `looperd` over HTTP and supports commands under:

- `status`
- `config show`
- `daemon status|logs`
- `loop list|start|pause`
- `work`
- `pr list|show|status`
- `run list`

### `apps/web`

Currently a stub that only logs a placeholder message.

## Configuration

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

Selected environment overrides:

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
- `LOOPER_TOKEN`

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
- SQLite migrations are loaded from the built output when present, otherwise from source.

## Development notes

- This repo uses TypeScript project references from the root `tsconfig.json`; root typecheck runs with `--noEmit`.
- Formatting and linting use Biome with spaces.
- Build output lives in `apps/*/dist/`; do not edit generated files.
