# Config fields, env overrides, and CLI flag inventory

Source of truth inspected from:

- `apps/looperd/src/config/defaults.ts`
- `apps/looperd/src/config/load.ts`
- `apps/looperd/src/config/types.ts`
- `apps/looperd/src/config/validate.ts`
- `apps/looperd/src/config/tools.ts`
- `apps/cli/src/index.ts`

## Precedence and merge rules

### Daemon config file path precedence

1. `looperd --config <path>`
2. `LOOPER_CONFIG`
3. `options.defaultConfigPath`
4. default `~/.looper/config.json`

Source: `apps/looperd/src/config/load.ts:295-300`

### Daemon config value precedence

1. built-in defaults from `createDefaultLooperConfig()`
2. config file JSON
3. env overrides
4. CLI flag overrides

Source: `apps/looperd/src/config/load.ts:303-318`

### Merge semantics

- objects merge deeply
- arrays replace wholesale
- `undefined` override values are ignored
- empty env-override objects are compacted away before merge
- explicit tool paths win; missing tool paths are auto-detected afterward

Source: `apps/looperd/src/config/load.ts:24-68`, `apps/looperd/src/config/load.ts:320-326`

### CLI client connection precedence

- `looper` reads `--config`/`LOOPER_CONFIG`/`~/.looper/config.json` to find connection settings
- `server.host` precedence: CLI flag → `LOOPER_HOST` → config file → `127.0.0.1`
- `server.port` precedence: CLI flag → `LOOPER_PORT` → config file → `4310`
- API token precedence: `LOOPER_TOKEN` only; the CLI does not read `server.localToken` from file, it injects `localToken: options.env.LOOPER_TOKEN`
- API base URL uses `server.baseUrl` from config file when present, otherwise `http://<host>:<port>`

Source: `apps/cli/src/index.ts:282-294`, `apps/cli/src/index.ts:2473-2516`

## Daemon config schema inventory

| Field | Type / allowed values | Default / behavior | Validation / notes |
| --- | --- | --- | --- |
| `server.host` | string | `127.0.0.1` | required non-empty string |
| `server.port` | integer | `4310` | `1..65535` |
| `server.baseUrl` | optional string | none | CLI-only consumption when reading config |
| `server.authMode` | `none` \| `local-token` | `none` | `local-token` requires `server.localToken` |
| `server.localToken` | optional string | none | required when `authMode=local-token` |
| `storage.mode` | `sqlite` | `sqlite` | must remain `sqlite` |
| `storage.dbPath` | string path | `~/.looper/looper.sqlite` | parent dir must be writable |
| `storage.backupDir` | optional string path | `~/.looper/backups` | in schema/defaults; not env/CLI overridable today |
| `scheduler.pollIntervalSeconds` | integer | `30` | must be integer `>= 10` |
| `scheduler.maxConcurrentRuns` | integer | `3` | must be positive integer |
| `scheduler.retryMaxAttempts` | integer | `5` | must be positive integer |
| `scheduler.retryBaseDelayMs` | integer | `5000` | must be positive integer |
| `agent.vendor` | optional `claude-code` \| `codex` \| `opencode` \| `cursor-cli` | none | validated when set |
| `agent.model` | optional string | none | no extra validation |
| `agent.params` | optional object | `{}` | preserved as arbitrary params |
| `agent.env` | optional string map | `{}` | preserved as arbitrary env |
| `logging.level` | `debug` \| `info` \| `warn` \| `error` | `info` | validated enum |
| `logging.maxSizeMB` | integer | `10` | must be positive integer |
| `logging.maxFiles` | integer | `5` | must be positive integer |
| `notifications.inApp` | boolean | `true` | env overridable |
| `notifications.osascript.enabled` | boolean | `true` | requires `tools.osascriptPath` when enabled |
| `notifications.osascript.soundForLevels` | optional array of `action_required`/`failure` | `["action_required", "failure"]` | invalid entries rejected |
| `notifications.osascript.throttleWindowSeconds` | integer | `60` | must be positive integer |
| `tools.gitPath` | optional string path | auto-detected if absent | required overall |
| `tools.ghPath` | optional string path | auto-detected if absent | required overall |
| `tools.osascriptPath` | optional string path | auto-detected if absent | required only when osascript notifications are enabled |
| `daemon.mode` | `foreground` \| `launchd` | `foreground` | validated enum |
| `daemon.plistPath` | optional string path | none | in schema only; no env/CLI override today |
| `daemon.logDir` | string path | `~/.looper/logs` | directory must be writable |
| `daemon.workingDirectory` | string path | process cwd | directory must be writable |
| `daemon.environment` | optional string map | `{}` | in schema/defaults only |
| `package.distribution` | `npm` | `npm` | fixed enum today |
| `package.autoMigrateOnStartup` | boolean | `true` | config-file only today |
| `package.requireBackupBeforeMigrate` | boolean | `false` | config-file only today |
| `defaults.baseBranch` | string | `main` | required non-empty string |
| `defaults.allowAutoCommit` | boolean | `true` | env/CLI overridable |
| `defaults.allowAutoPush` | boolean | `true` | env/CLI overridable |
| `defaults.allowAutoApprove` | boolean | `false` | env/CLI overridable |
| `defaults.allowAutoMerge` | boolean | `false` | config-file only today |
| `defaults.allowRiskyFixes` | boolean | `false` | config-file only today |
| `defaults.openPrStrategy` | optional `all_done` \| `first_commit` \| `manual` | `manual` | validated enum when set |
| `projects[]` | array of project refs | `[]` | array replaces wholesale during merge |
| `projects[].id` | string | none | required, unique, validated project id |
| `projects[].name` | string | none | required |
| `projects[].repoPath` | string path | none | required |
| `projects[].baseBranch` | optional string | none | optional |
| `projects[].worktreeRoot` | optional string path | none | optional |

Defaults source: `apps/looperd/src/config/defaults.ts:14-73`

Schema source: `apps/looperd/src/config/types.ts:27-121`

Validation source: `apps/looperd/src/config/validate.ts:77-365`

## Environment variable overrides

### Daemon config loader env overrides

| Env var | Target field / behavior | Notes |
| --- | --- | --- |
| `LOOPER_CONFIG` | config file path selector | not persisted into config object |
| `LOOPER_HOST` | `server.host` | string override |
| `LOOPER_PORT` | `server.port` | integer parse; invalid values are ignored |
| `LOOPER_LOG_DIR` | `daemon.logDir` | string override |
| `LOOPER_DAEMON_MODE` | `daemon.mode` | string override, validated later |
| `LOOPER_WORKING_DIRECTORY` | `daemon.workingDirectory` | string override |
| `LOOPER_DB_PATH` | `storage.dbPath` | string override |
| `LOOPER_IN_APP_NOTIFICATIONS` | `notifications.inApp` | boolean parse: `1/true/yes/on` or `0/false/no/off` |
| `LOOPER_OSASCRIPT_ENABLED` | `notifications.osascript.enabled` | same boolean parsing |
| `LOOPER_ALLOW_AUTO_COMMIT` | `defaults.allowAutoCommit` | same boolean parsing |
| `LOOPER_ALLOW_AUTO_PUSH` | `defaults.allowAutoPush` | same boolean parsing |
| `LOOPER_ALLOW_AUTO_APPROVE` | `defaults.allowAutoApprove` | same boolean parsing |
| `LOOPER_GIT_PATH` | `tools.gitPath` | explicit path beats auto-detection |
| `LOOPER_GH_PATH` | `tools.ghPath` | explicit path beats auto-detection |
| `LOOPER_OSASCRIPT_PATH` | `tools.osascriptPath` | explicit path beats auto-detection |

Source: `apps/looperd/src/config/load.ts:230-263`

### CLI-only env behavior

| Env var | Behavior | Notes |
| --- | --- | --- |
| `LOOPER_CONFIG` | selects CLI config file path | same precedence role as daemon |
| `LOOPER_HOST` | overrides CLI host | used when building daemon API client |
| `LOOPER_PORT` | overrides CLI port | used when building daemon API client |
| `LOOPER_DAEMON_MODE` | populates `config.daemon.mode` in CLI config | influences daemon-management flows |
| `LOOPER_LOG_DIR` | populates `config.daemon.logDir` in CLI config | influences daemon-management flows |
| `LOOPER_TOKEN` | bearer token for API client | overrides all file-based token behavior |

Source: `apps/cli/src/index.ts:2478-2516`, `apps/cli/src/index.ts:289-294`

## CLI flag overrides

### `looperd` CLI flags accepted by the daemon config loader

| Flag | Target field / behavior | Notes |
| --- | --- | --- |
| `--config <path>` | config file path selector | highest precedence for path selection |
| `--host <host>` | `server.host` | string override |
| `--port <port>` | `server.port` | integer parse; invalid values fall through to validation |
| `--db-path <path>` | `storage.dbPath` | string override |
| `--log-dir <path>` | `daemon.logDir` | string override |
| `--daemon-mode <mode>` | `daemon.mode` | string override, validated later |
| `--git-path <path>` | `tools.gitPath` | explicit path beats auto-detection |
| `--gh-path <path>` | `tools.ghPath` | explicit path beats auto-detection |
| `--allow-auto-commit <bool>` | `defaults.allowAutoCommit` | boolean parse |
| `--allow-auto-push <bool>` | `defaults.allowAutoPush` | boolean parse |
| `--allow-auto-approve <bool>` | `defaults.allowAutoApprove` | boolean parse |
| `--osascript-path <path>` | `tools.osascriptPath` | explicit path beats auto-detection |

Unknown `looperd` flags cause `Unknown looperd argument: ...`.

Source: `apps/looperd/src/config/load.ts:99-228`

### Global `looper` flags that feed config/connection/runtime behavior

These are defined as global CLI options and are the only flags forwarded by `extractConfigArgs()` into CLI config loading and daemon-launch argument building.

Machine-verifiable freeze artifact: `specs/2026-04-17-go-port-plan/artifacts/cli-flags.compat.json`

| Flag | Used by | Notes |
| --- | --- | --- |
| `--json` | CLI output mode | not forwarded into daemon config |
| `--config <path>` | CLI config load + daemon launch args | forwarded |
| `--host <host>` | CLI config load + daemon launch args | forwarded |
| `--port <port>` | CLI config load + daemon launch args | forwarded |
| `--db-path <path>` | CLI config load + daemon launch args | forwarded even though CLI config only reads host/port/daemon fields |
| `--log-dir <path>` | CLI config load + daemon launch args | forwarded |
| `--daemon-mode <mode>` | CLI config load + daemon launch args | forwarded |
| `--git-path <path>` | daemon launch args | forwarded |
| `--gh-path <path>` | daemon launch args | forwarded |
| `--osascript-path <path>` | daemon launch args | forwarded |

Sources: `apps/cli/src/index.ts:258-269`, `apps/cli/src/index.ts:679-690`, `apps/cli/src/index.ts:2221-2249`, `apps/cli/src/index.ts:2537-2557`

## External tool dependency inventory

This inventory freezes the current non-HTTP external command dependencies that the Go port must either preserve or replace intentionally.

### `git`

- Config surface: `tools.gitPath`, `LOOPER_GIT_PATH`, `--git-path`
- Resolution behavior: explicit config/env/CLI path wins; otherwise auto-detected with `Bun.which("git")`
- Validation behavior: startup fails when `tools.gitPath` cannot be resolved
- Current responsibilities:
  - worktree and branch management via `GitWorktreeGateway`
  - project add/inspection flows that require repo/worktree mutation
  - fixer and worker runtimes that need local repo operations
- Primary sources: `apps/looperd/src/config/tools.ts`, `apps/looperd/src/config/validate.ts`, `apps/looperd/src/infra/git.ts`, `apps/looperd/src/runtime/index.ts`

### `gh`

- Config surface: `tools.ghPath`, `LOOPER_GH_PATH`, `--gh-path`
- Resolution behavior: explicit config/env/CLI path wins; otherwise auto-detected with `Bun.which("gh")`
- Validation behavior: startup fails when `tools.ghPath` cannot be resolved
- Current responsibilities:
  - GitHub PR and issue reads
  - review submission, comments, reactions, and related PR automation
  - project/runtime flows that need GitHub metadata alongside git state
- Primary sources: `apps/looperd/src/config/tools.ts`, `apps/looperd/src/config/validate.ts`, `apps/looperd/src/infra/github.ts`, `apps/looperd/src/runtime/index.ts`

### `osascript`

- Config surface: `tools.osascriptPath`, `LOOPER_OSASCRIPT_PATH`, `--osascript-path`
- Resolution behavior: explicit config/env/CLI path wins; otherwise auto-detected with `Bun.which("osascript")`
- Validation behavior: required only when `notifications.osascript.enabled` is `true`; startup fails otherwise
- Current responsibilities:
  - macOS notification delivery, including throttling and sound-level behavior controlled in config
- Primary sources: `apps/looperd/src/config/defaults.ts`, `apps/looperd/src/config/tools.ts`, `apps/looperd/src/config/validate.ts`, `apps/looperd/src/runtime/index.ts`

### Shell

- No dedicated `tools.shellPath` config exists today
- Current CLI shell behavior depends on the caller environment:
  - `looper jump --shell-integration <bash|zsh|fish>` prints shell-specific helper functions
  - interactive `looper jump <id>` shells out to `process.env.SHELL`, falling back to `/bin/zsh`
- Current daemon/process execution behavior is Bun-based rather than shell-path-configured:
  - infra commands execute binaries directly through `Bun.spawn()` with explicit `command` + `args`
  - Bun shell helpers also appear in build/dev scripts (`Bun.$`), but not as a daemon config field
- Porting implication: preserve user-visible shell integration behavior and interactive-shell fallback semantics even though shell itself is not currently part of `tools.*`
- Primary sources: `apps/cli/src/index.ts:2000-2059`, `apps/cli/src/index.ts:2209-2218`, `apps/looperd/src/infra/command.ts`, `apps/looperd/scripts/compile.ts`

## Compatibility-boundary notes for follow-up tasks

- The config surface is broader than the env/CLI override surface; many fields are config-file-only today.
- `looper` and `looperd` do not expose identical override sets: `looper` forwards tool-path and storage flags for daemon management, but its own local config reader only consumes host/port/daemon-mode/log-dir plus `server.baseUrl` from file.
- `defaults.allowAutoMerge`, `defaults.allowRiskyFixes`, `package.*`, `daemon.plistPath`, `daemon.environment`, `storage.backupDir`, `server.authMode`, and `server.localToken` currently have no env/CLI override path.
- `projects[]` is part of the config compatibility boundary and follows replace-not-merge array semantics.
- Tool path auto-detection is part of the effective-config contract because validation requires resolved `bun`, `git`, and `gh`, and conditionally `osascript`.
