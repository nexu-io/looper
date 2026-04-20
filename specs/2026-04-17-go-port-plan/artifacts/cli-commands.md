# `looper` CLI command inventory

Source of truth inspected from:

- `apps/cli/src/index.ts`
- `apps/cli/package.json`

## Entrypoints

- Binary mapping: `apps/cli/package.json`
- CLI composition and dispatch: `apps/cli/src/index.ts`
- Compatibility artifact for flag names/semantics: `specs/2026-04-17-go-port-plan/artifacts/cli-flags.compat.json`

## Compatibility boundary

- CLI flag names and meanings are part of the Go-port compatibility boundary.
- `specs/2026-04-17-go-port-plan/artifacts/cli-flags.compat.json` is the machine-verifiable artifact for:
  - global flag names
  - command-local flag names
  - forwarded-vs-local semantics
  - command-level flag meanings
- Only `--config`, `--host`, `--port`, `--db-path`, `--log-dir`, `--daemon-mode`, `--git-path`, `--gh-path`, and `--osascript-path` are forwarded by `extractConfigArgs()` into daemon/config-loading flows.
- `--json` is global but intentionally not forwarded.

## Implemented commands

### Command tree

- `looper status`
- `looper project`
  - `list`
  - `add`
- `looper config`
  - `show`
- `looper daemon`
  - `install`
  - `status`
  - `start`
  - `restart`
  - `logs`
- `looper upgrade`
- `looper loop`
  - `list`
  - `start`
  - `pause`
- `looper work`
- `looper plan`
- `looper pr`
  - `list`
  - `show`
  - `status`
- `looper review <pr>`
- `looper run`
  - `list`
- `looper ps`
- `looper jump [id]`
- `looper logs <id>`
- `looper stop <id>`

- `looper status`
  - flags: `--json`
- `looper project list`
  - flags: `--json`
- `looper project add`
  - flags: `--repo-path`, `--id`, `--name`, `--base-branch`, `--worktree-root`, `--repo`, `--json`
- `looper config show`
  - flags: `--json`
- `looper daemon install`
  - flags: `--force`, `--json`
- `looper daemon status`
  - flags: `--json`
- `looper daemon start`
  - flags: `--json`
- `looper daemon restart`
  - flags: `--json`
- `looper daemon logs`
  - flags: `--lines`, `--json`
- `looper upgrade`
  - flags: `--check`, `--daemon`
- `looper loop list`
  - flags: `--json`
- `looper loop start`
  - flags: `--id`, `--type`, `--pr`, `--json`
- `looper loop pause`
  - flags: `--id`, `--json`
- `looper work`
  - flags: `--project`, `--title`, `--prompt`, `--issue`, `--spec`, `--repo`, `--base-branch`, `--json`
- `looper plan`
  - flags: `--project`, `--issue`, `--json`
- `looper pr list`
  - flags: `--json`
- `looper pr show`
  - flags: `--json`
- `looper pr status`
  - flags: `--json`
- `looper review`
  - flags: `--project`, `--loop`, `--json`
- `looper run list`
  - flags: `--loop`, `--json`
- `looper ps`
  - flags: `--type`, `--project`, `--json`
- `looper jump`
  - flags: `--print-path`, `--shell-integration`, `--json`
- `looper logs`
  - flags: `--stderr`, `--tail`, `--full`, `--json`
- `looper stop`
  - flags: `--json`

## Reserved/removed command groups

These command families are explicitly rejected by the current dispatcher rather than implemented:

- `looper task ...`
- `looper worker ...`
- `looper workers ...`

## Defining locations

- Help/command group definitions: `apps/cli/src/index.ts:154-175`
- Entry + lifecycle: `apps/cli/src/index.ts:271-368`
- Dispatcher: `apps/cli/src/index.ts:370-466`
- Command registration: `apps/cli/src/index.ts:468-631`
