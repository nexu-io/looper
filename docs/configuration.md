# Configuration guide

This document explains how `looper` and `looperd` configuration works, where the config file lives, which values can be overridden by environment variables and CLI flags, and what a complete config file looks like.

## Install layout notes

For the default supported macOS install flow:

- `looper` is installed from a GitHub Release Go binary
- `looper daemon install` installs the managed daemon binary to `~/.looper/bin/looperd`
- `looper daemon start` writes its pid file to `~/.looper/looperd.pid`

The daemon lookup order used by the CLI is `~/.looper/bin/looperd`, then `$PATH`.

## How config loading works

`looperd` loads configuration in this order:

1. built-in defaults
2. config file
3. environment variables
4. CLI flags

Later layers override earlier ones.

Default config file path:

- `~/.looper/config.json`

You can point to a different file with either:

- `LOOPER_CONFIG=/absolute/or/relative/path/to/config.json`
- `looperd --config /absolute/or/relative/path/to/config.json`

Relative config paths are resolved from the current working directory used to start `looperd`.

## Minimal setup

In the simplest setup, you can rely on defaults and only create a config file when you need to customize behavior.

Example minimal `~/.looper/config.json`:

```json
{
  "agent": {
    "vendor": "opencode"
  },
  "projects": [
    {
      "id": "looper",
      "name": "Looper",
      "repoPath": "/absolute/path/to/repo"
    }
  ]
}
```

## Full example

```json
{
  "server": {
    "host": "127.0.0.1",
    "port": 4310,
    "authMode": "local-token",
    "localToken": "replace-me"
  },
  "storage": {
    "mode": "sqlite",
    "dbPath": "/Users/you/.looper/looper.sqlite",
    "backupDir": "/Users/you/.looper/backups"
  },
  "scheduler": {
    "pollIntervalSeconds": 30,
    "maxConcurrentRuns": 3,
    "retryMaxAttempts": 5,
    "retryBaseDelayMs": 5000
  },
  "agent": {
    "vendor": "opencode",
    "model": "gpt-5.4",
    "params": {
      "reasoning": "medium"
    },
    "env": {
      "OPENAI_API_KEY": "replace-me"
    }
  },
  "logging": {
    "level": "info",
    "maxSizeMB": 10,
    "maxFiles": 5
  },
  "notifications": {
    "inApp": true,
    "osascript": {
      "enabled": true,
      "soundForLevels": ["action_required", "failure"],
      "throttleWindowSeconds": 60
    }
  },
  "tools": {
    "gitPath": "/usr/bin/git",
    "ghPath": "/opt/homebrew/bin/gh",
    "osascriptPath": "/usr/bin/osascript"
  },
  "daemon": {
    "mode": "foreground",
    "logDir": "/Users/you/.looper/logs",
    "workingDirectory": "/absolute/path/to/where/you/start/looperd",
    "environment": {
      "EXAMPLE_FLAG": "1"
    }
  },
  "package": {
    "distribution": "github-release",
    "autoMigrateOnStartup": true,
    "requireBackupBeforeMigrate": false
  },
  "defaults": {
    "baseBranch": "main",
    "allowAutoCommit": true,
    "allowAutoPush": true,
    "allowAutoApprove": false,
    "allowAutoMerge": false,
    "allowRiskyFixes": false,
    "openPrStrategy": "manual"
  },
  "projects": [
    {
      "id": "looper",
      "name": "Looper",
      "repoPath": "/absolute/path/to/looper",
      "baseBranch": "main",
      "worktreeRoot": "/Users/you/.looper/worktrees/looper"
    }
  ]
}
```

## Field reference

### `server`

- `host`: bind host, default `127.0.0.1`
- `port`: bind port, default `4310`
- `authMode`: `none` or `local-token`
- `localToken`: required when `authMode` is `local-token`

### `storage`

- `mode`: currently must be `sqlite`
- `dbPath`: SQLite database path
- `backupDir`: backup output directory

Default storage paths:

- DB: `~/.looper/looper.sqlite`
- backups: `~/.looper/backups`

### `scheduler`

- `pollIntervalSeconds`: queue poll interval, must be an integer `>= 10`
- `maxConcurrentRuns`: positive integer
- `retryMaxAttempts`: positive integer
- `retryBaseDelayMs`: positive integer

### `agent`

- `vendor`: one of `claude-code`, `codex`, `opencode`, `cursor-cli`
- `model`: optional model identifier
- `params`: free-form vendor-specific parameters
- `env`: environment variables passed to the agent process

### `logging`

- `level`: one of `debug`, `info`, `warn`, `error`
- `maxSizeMB`: positive integer log rotation size
- `maxFiles`: positive integer retained file count, including the active `looperd.log`

When `looperd.log` would exceed `maxSizeMB`, `looperd` rotates it to `looperd.log.1`, shifts older archives to `.2`, `.3`, and so on, and keeps at most `maxFiles` total log files.

### `notifications`

- `inApp`: enables in-app notifications
- `osascript.enabled`: enables macOS notifications
- `osascript.soundForLevels`: subset of `action_required`, `failure`
- `osascript.throttleWindowSeconds`: positive integer

If `notifications.osascript.enabled` is `true`, `tools.osascriptPath` must resolve.

### `tools`

- `gitPath`
- `ghPath`
- `osascriptPath`

If these are omitted, `looperd` tries to detect them from `PATH`. Startup validation fails when required tools cannot be resolved.

### `daemon`

- `mode`: `foreground` or `launchd`
- `logDir`: daemon log directory
- `workingDirectory`: working directory used by the daemon
- `environment`: extra environment variables for the daemon process

Current packaged install and CLI management flows document `foreground` as the active Phase 1 path. `launchd` remains part of the configuration surface, but full launchd-oriented lifecycle management is not the primary documented install flow here.

Defaults:

- `mode`: `foreground`
- `logDir`: `~/.looper/logs`
- `workingDirectory`: current working directory when config is loaded

### `package`

- `distribution`: install-channel metadata; current supported installs use `github-release`
- `autoMigrateOnStartup`: run DB migrations on startup
- `requireBackupBeforeMigrate`: require a backup before migrations

### `defaults`

- `baseBranch`: default project branch, usually `main`
- `allowAutoCommit`
- `allowAutoPush`
- `allowAutoApprove`
- `allowAutoMerge`
- `allowRiskyFixes`
- `openPrStrategy`: `all_done`, `first_commit`, or `manual`

Default values:

- `baseBranch`: `main`
- `allowAutoCommit`: `true`
- `allowAutoPush`: `true`
- `allowAutoApprove`: `false`
- `allowAutoMerge`: `false`
- `allowRiskyFixes`: `false`
- `openPrStrategy`: `manual`

### `projects`

Each entry registers a repo that `looper` can target.

- `id`: stable project identifier; must be unique
- `name`: display name
- `repoPath`: absolute repository path
- `baseBranch`: optional per-project override
- `worktreeRoot`: optional per-project worktree root

Example:

```json
{
  "projects": [
    {
      "id": "looper",
      "name": "Looper",
      "repoPath": "/Users/you/src/looper",
      "baseBranch": "main",
      "worktreeRoot": "/Users/you/.looper/worktrees/looper"
    }
  ]
}
```

## Environment variable overrides

Supported environment overrides:

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

Boolean environment variables accept:

- truthy: `1`, `true`, `yes`, `on`
- falsy: `0`, `false`, `no`, `off`

Example:

```bash
LOOPER_CONFIG="$HOME/.looper/config.json" \
LOOPER_PORT=4321 \
LOOPER_ALLOW_AUTO_PUSH=false \
looperd
```

## CLI flag overrides

Supported `looperd` flags:

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

Example:

```bash
looperd \
  --config "$HOME/.looper/config.json" \
  --port 4321 \
  --allow-auto-push=false
```

## Validation rules and startup failures

`looperd` fails fast on invalid config. Common validation rules:

- required strings must be non-empty
- numeric fields must be positive integers where applicable
- `server.port` must be between `1` and `65535`
- `scheduler.pollIntervalSeconds` must be at least `10`
- `authMode=local-token` requires `server.localToken`
- `projects[].id` must be valid and unique
- `storage.dbPath` parent directory must be writable
- `daemon.logDir` must be writable
- `daemon.workingDirectory` must be writable
- the default worktree root must be writable

## Merge behavior details

- objects are merged deeply
- arrays are replaced as a whole, not merged item-by-item
- omitted fields keep their previous-layer value

That means if you set `projects` in the config file, the entire projects array comes from that layer.

## Recommended first-time setup

1. Install `git` and `gh`
2. Create `~/.looper/config.json`
3. Add at least one project in `projects`
4. Set `agent.vendor`
5. Start the daemon with your installed `looperd` (or `go run ./cmd/looperd` while developing)
6. Run `looper config show` to inspect the effective config

## Troubleshooting

### `tools.gitPath` or `tools.ghPath` could not be resolved

Set explicit paths in the config file, or make sure the binaries are on `PATH` for the environment that starts `looperd`.

### `tools.osascriptPath is required when osascript notifications are enabled`

Either:

- install or expose `osascript`, or
- disable macOS notifications with:

```json
{
  "notifications": {
    "osascript": {
      "enabled": false
    }
  }
}
```

### A runtime path is not writable

Make sure the daemon user can write to:

- the parent directory of `storage.dbPath`
- `daemon.logDir`
- `daemon.workingDirectory`
- the default worktree root under `~/.looper/worktrees`
