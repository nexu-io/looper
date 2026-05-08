# Looper configuration reference for agents

Use this reference before inspecting or changing `~/.looper/config.json`. Do not overwrite user config; make targeted edits only after confirmation and redact secrets in summaries.

## Install layout and config loading

Default config path:

```text
~/.looper/config.json
```

For the default macOS install flow:

- `looper` is installed from a GitHub Release binary.
- `looper daemon install` installs the managed daemon binary to `~/.looper/bin/looperd`.
- Daemon lookup order is `~/.looper/bin/looperd`, then `$PATH`.
- `looper daemon start` writes `~/.looper/looperd.pid` and lifecycle state to `~/.looper/looperd.state.json`.

`looperd` loads configuration in this order:

1. built-in defaults
2. config file
3. environment variables
4. CLI flags

Later layers override earlier ones. Objects are merged deeply, arrays are replaced as a whole, and omitted fields keep the previous-layer value.

Custom config path:

- `LOOPER_CONFIG=/absolute/or/relative/path/to/config.json`
- `looperd --config /absolute/or/relative/path/to/config.json`

Relative config paths resolve from the working directory used to start `looperd`.

## Minimal config

`agent.vendor` has no built-in default. Set it when planner, reviewer, fixer, or worker loops should run.

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

## Full config shape

Use this as a map of supported sections, not as a template to paste wholesale:

```json
{
  "server": {
    "host": "127.0.0.1",
    "port": 17310,
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
    "model": "your-model-if-needed",
    "params": {
      "reasoning": "medium"
    },
    "env": {
      "OPENAI_API_KEY": "replace-me"
    },
    "nativeResume": {
      "enabled": true
    },
    "timeouts": {
      "plannerSeconds": 3600,
      "workerSeconds": 10800,
      "reviewerSeconds": 5400,
      "fixerSeconds": 7200
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
  "disclosure": {
    "enabled": true,
    "includeAgent": true,
    "includeOS": false,
    "channels": {
      "gitCommit": true,
      "pullRequest": true,
      "issueComment": true,
      "reviewComment": true,
      "inlineCommentVisible": false
    }
  },
  "tools": {
    "gitPath": "/usr/bin/git",
    "ghPath": "/opt/homebrew/bin/gh",
    "osascriptPath": "/usr/bin/osascript"
  },
  "daemon": {
    "mode": "foreground",
    "restartPolicy": "on-failure",
    "restartThrottleSeconds": 10,
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
    "openPrStrategy": "all_done",
    "addSnapshotMode": "async"
  },
  "reviewer": {
    "reviewEvents": {
      "clean": "COMMENT",
      "blocking": "COMMENT"
    }
  },
  "roles": {
    "planner": {
      "autoDiscovery": true,
      "triggers": {
        "labels": ["looper:plan"],
        "labelMode": "all",
        "requireAssigneeCurrentUser": true
      }
    },
    "reviewer": {
      "autoDiscovery": true,
      "triggers": {
        "includeDrafts": false,
        "requireReviewRequest": true,
        "labels": [],
        "labelMode": "all"
      },
      "specReview": {
        "includeReviewingLabel": true,
        "reviewingLabel": "looper:spec-reviewing"
      }
    },
    "fixer": {
      "autoDiscovery": true,
      "triggers": {
        "includeDrafts": false,
        "authorFilter": "current_user",
        "labels": [],
        "labelMode": "all"
      }
    },
    "worker": {
      "autoDiscovery": true,
      "triggers": {
        "labels": ["looper:worker-ready"],
        "labelMode": "all",
        "requireAssigneeCurrentUser": true
      }
    }
  },
  "projects": [
    {
      "id": "looper",
      "name": "Looper",
      "repoPath": "/absolute/path/to/looper",
      "baseBranch": "main",
      "worktreeRoot": "/Users/you/.looper/worktrees/looper",
      "roles": {
        "worker": {
          "triggers": {
            "labels": ["team:alpha", "worker-ready"],
            "labelMode": "any"
          }
        }
      }
    }
  ]
}
```

## Runtime paths

Default runtime artifacts live under `~/.looper/`:

- `config.json`
- `looper.sqlite`
- `backups/`
- `logs/`
- `worktrees/`
- `bin/looperd`
- `looperd.pid`
- `looperd.state.json`

Default storage paths:

- DB: `~/.looper/looper.sqlite`
- backups: `~/.looper/backups`

## Daemon supervision config

Supervision applies only to `looperd` lifecycle. Looper does not supervise arbitrary user commands or agent subprocesses.

`daemon.mode` values:

- `foreground` (default): starts `looperd` as a detached background process. It writes pid/state files but is not actively supervised and will not automatically restart after crash, logout, or reboot.
- `launchd`: on macOS, installs and bootstraps a user LaunchAgent. It can restart according to `daemon.restartPolicy` and start at login. Unsupported platforms return an actionable error.

Restart options:

- `daemon.restartPolicy`: `never`, `on-failure`, or `always` (default `on-failure`).
- `daemon.restartThrottleSeconds`: positive integer throttle, default `10`.
- `daemon.plistPath`: optional macOS LaunchAgent plist path. Default: `~/Library/LaunchAgents/com.powerformer.looper.looperd.plist`.

Runtime diagnostics:

- state: `~/.looper/looperd.state.json`
- pid: `~/.looper/looperd.pid`
- main log: `~/.looper/logs/looperd.log`
- startup logs: `~/.looper/logs/startup/`
- launchd logs: `~/.looper/logs/launchd/looperd.stdout.log`, `~/.looper/logs/launchd/looperd.stderr.log`

Use `looper daemon status`, `looper daemon status --json`, `looper daemon logs`, and `looper daemon logs --startup` to inspect daemon state.

## Field reference

### `server`

- `host`: bind host, default `127.0.0.1`.
- `port`: bind port, default `17310`.
- `authMode`: `none` or `local-token`.
- `localToken`: required when `authMode` is `local-token`. If enabled, export `LOOPER_TOKEN` before using the CLI.

### `storage`

- `mode`: currently must be `sqlite`.
- `dbPath`: SQLite database path.
- `backupDir`: backup output directory.

### `scheduler`

- `pollIntervalSeconds`: queue poll interval, integer `>= 10`.
- `maxConcurrentRuns`: positive integer.
- `retryMaxAttempts`: positive integer.
- `retryBaseDelayMs`: positive integer.

### `agent`

- `vendor`: one of `claude-code`, `codex`, `opencode`, `cursor-cli`.
- `model`: optional model identifier.
- `params`: free-form vendor-specific parameters.
- `env`: environment variables passed to the agent process. Redact secrets.
- `nativeResume.enabled`: local-machine native session resume after daemon restart, default `true`.
- `timeouts`: role-specific agent execution timeout seconds. Defaults: planner `3600`, worker `10800`, reviewer `5400`, fixer `7200`.

`vendor` is required for agent-driven loops. Without it, the daemon can run but planner/reviewer/fixer/worker loops cannot be created or started.

Native resume is local-machine only. Session stores are not portable across hosts or users, and resume can fall back if the vendor CLI cannot reattach. Set `LOOPER_AGENT_NATIVE_RESUME_ENABLED=false` to disable it.

### `logging`

- `level`: `debug`, `info`, `warn`, or `error`.
- `maxSizeMB`: positive integer log rotation size.
- `maxFiles`: positive integer retained file count, including active `looperd.log`.

### `notifications`

- `inApp`: enables in-app notifications.
- `osascript.enabled`: enables macOS notifications.
- `osascript.soundForLevels`: subset of `action_required`, `failure`.
- `osascript.throttleWindowSeconds`: positive integer.

Defaults:

- macOS: `notifications.osascript.enabled=true`.
- non-macOS: `notifications.osascript.enabled=false`.

If `notifications.osascript.enabled=true`, `tools.osascriptPath` must resolve.

Disable osascript notifications only after confirmation:

```json
{
  "notifications": {
    "osascript": {
      "enabled": false
    }
  }
}
```

### `disclosure`

`looperd` adds local attribution to generated GitHub text and commit messages. This is not telemetry.

- `enabled`: disclosure stamps, default `true`.
- `includeAgent`: include configured agent vendor/model, default `true`.
- `includeOS`: include OS family only, default `false`.
- `channels.gitCommit`: add `Generated-By:` trailer.
- `channels.pullRequest`: add PR body footer.
- `channels.issueComment`: add issue/PR comment footer.
- `channels.reviewComment`: disclose generated review summaries and inline comments.
- `channels.inlineCommentVisible`: visible footer for inline comments when `true`; hidden marker when `false`.

Disclosure stamps use an allowlist: product, version, runner role, configured agent vendor/model, and optionally OS family. They do not include hostnames, usernames, local paths, IP/MAC addresses, kernel versions, env vars, tokens, endpoints, or machine identifiers.

### `tools`

- `gitPath`
- `ghPath`
- `osascriptPath`

If omitted, `looperd` detects from `PATH`. Startup validation fails when required tools cannot resolve.

Use targeted edits like these only after confirmation:

```json
{
  "tools": {
    "gitPath": "/opt/homebrew/bin/git",
    "ghPath": "/opt/homebrew/bin/gh",
    "osascriptPath": "/usr/bin/osascript"
  }
}
```

If a tool exists in an interactive shell but not for `looperd`, suspect a daemon environment `PATH` mismatch. Prefer explicit `tools.*Path` over shell-profile changes.

### `daemon`

- `mode`: `foreground` or `launchd`.
- `restartPolicy`: `never`, `on-failure`, or `always`.
- `restartThrottleSeconds`: positive supervisor restart throttle in seconds.
- `plistPath`: optional macOS user LaunchAgent plist path for `launchd` mode.
- `logDir`: daemon log directory.
- `shutdownTimeoutMs`: graceful shutdown timeout in milliseconds.
- `workingDirectory`: daemon working directory.
- `environment`: reserved daemon environment map; part of config surface but not a primary user-facing runtime control.

Defaults: `mode=foreground`, `restartPolicy=on-failure`, `restartThrottleSeconds=10`, `logDir=~/.looper/logs`, `shutdownTimeoutMs=1000`, `workingDirectory` is the current working directory when config loads.

### `package`

- `distribution`: install-channel metadata; current supported installs use `github-release`.
- `autoMigrateOnStartup`: run DB migrations on startup.
- `requireBackupBeforeMigrate`: require a backup before migrations.

### `defaults`

- `baseBranch`: default project branch, usually `main`.
- `allowAutoCommit`: default `true`.
- `allowAutoPush`: default `true`.
- `allowAutoApprove`: default `false`.
- `allowAutoMerge`: default `false`.
- `allowRiskyFixes`: default `false`.
- `fixAllPullRequests`: legacy fixer discovery switch, default `false`; prefer `roles.fixer.triggers.authorFilter`.
- `openPrStrategy`: `all_done`, `first_commit`, or `manual`; default `all_done`.
- `addSnapshotMode`: `async`, `full`, or `off`; default `async`. `looper project add --snapshot-mode` overrides per request.

`defaults.allowAutoApprove=true` is a legacy alias for reviewer clean approvals. Explicit `reviewer.reviewEvents.clean` wins.

### `reviewer`

- `reviewEvents.clean`: `COMMENT` or `APPROVE`, default `COMMENT`.
- `reviewEvents.blocking`: `COMMENT` or `REQUEST_CHANGES`, default `COMMENT`.
- Deprecated reviewer budget options are ignored by the reviewer filter.

Safe default comment-only behavior:

```json
{
  "reviewer": {
    "reviewEvents": {
      "clean": "COMMENT",
      "blocking": "COMMENT"
    }
  }
}
```

Decision reviews require explicit opt-in:

```json
{
  "reviewer": {
    "reviewEvents": {
      "clean": "APPROVE",
      "blocking": "REQUEST_CHANGES"
    }
  }
}
```

Reviewer behavior matrix:

| Reviewer outcome | `reviewEvents.clean` | `reviewEvents.blocking` | GitHub event |
| --- | --- | --- | --- |
| `clean` | `COMMENT` | any | `COMMENT` |
| `clean` | `APPROVE` | any | `APPROVE` |
| `non_blocking` | any | any | `COMMENT` |
| `blocking` | any | `COMMENT` | `COMMENT` |
| `blocking` | any | `REQUEST_CHANGES` | `REQUEST_CHANGES` |
| legacy `actionable` | any | any | `COMMENT` |

One-off reviewer jobs can snapshot policy into loop metadata:

```bash
looper review owner/repo#123 \
  --clean-review-event APPROVE \
  --blocking-review-event REQUEST_CHANGES
```

### `roles`

The `roles` section controls scheduler auto-discovery for planner, reviewer, fixer, and worker. It does not block manual commands, direct processing, retries, or already queued work.

Default discovery:

- planner: open issues labeled `looper:plan` assigned to current GitHub user.
- worker: open issues labeled `looper:worker-ready` assigned to current GitHub user.
- reviewer: open non-draft PRs where current user is requested for review, plus spec-review follow-up.
- fixer: open non-draft PRs authored by current user that have actionable review items.

Common fields:

- `roles.<role>.autoDiscovery`: when `false`, scheduler skips new discovery for that role.
- issue roles (`planner`, `worker`): `triggers.labels`, `triggers.labelMode` (`all` or `any`), `triggers.requireAssigneeCurrentUser`.
- reviewer: `triggers.includeDrafts`, `triggers.requireReviewRequest`, `triggers.labels`, `triggers.labelMode`, `specReview.includeReviewingLabel`, `specReview.reviewingLabel`.
- fixer: `triggers.includeDrafts`, `triggers.authorFilter` (`current_user` or `any`), `triggers.labels`, `triggers.labelMode`.

Trigger fields are combined with logical AND. Empty label lists mean no label constraint.

Examples:

```json
{
  "roles": {
    "planner": {
      "triggers": {
        "labels": ["team:alpha", "needs-plan"],
        "labelMode": "any",
        "requireAssigneeCurrentUser": false
      }
    }
  }
}
```

```json
{
  "roles": {
    "reviewer": {
      "autoDiscovery": false
    },
    "fixer": {
      "triggers": {
        "authorFilter": "any"
      }
    }
  }
}
```

`defaults.fixAllPullRequests=true` maps to `roles.fixer.triggers.authorFilter=any` only when that role field is not explicitly configured.

### `projects`

Each project registers a repo Looper can target.

Prefer `looper project add /absolute/path/to/repo --id <id> --repo owner/repo` for adding an existing local repository. Edit `projects[]` manually only when the CLI flow is not suitable and after confirming the exact config change.

- `id`: stable unique identifier.
- `name`: display name.
- `repoPath`: absolute repository path.
- `baseBranch`: optional per-project override.
- `worktreeRoot`: optional per-project worktree root.
- `roles`: optional per-project role overrides for `planner`, `worker`, `reviewer`, and `fixer`.

Project role overrides fall back to global role values when fields are absent. Unknown role keys are rejected during config loading.

```json
{
  "projects": [
    {
      "id": "looper",
      "name": "Looper",
      "repoPath": "/Users/you/src/looper",
      "baseBranch": "main",
      "worktreeRoot": "/Users/you/.looper/worktrees/looper",
      "roles": {
        "reviewer": {
          "triggers": {
            "labels": ["needs-review"],
            "requireReviewRequest": false
          }
        },
        "worker": {
          "autoDiscovery": false
        }
      }
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
- `LOOPER_DAEMON_RESTART_POLICY`
- `LOOPER_DAEMON_RESTART_THROTTLE_SECONDS`
- `LOOPER_WORKING_DIRECTORY`
- `LOOPER_GIT_PATH`
- `LOOPER_GH_PATH`
- `LOOPER_OSASCRIPT_PATH`
- `LOOPER_OSASCRIPT_ENABLED`
- `LOOPER_IN_APP_NOTIFICATIONS`
- `LOOPER_AGENT_NATIVE_RESUME_ENABLED`
- `LOOPER_AGENT_TIMEOUTS_PLANNER_SECONDS`
- `LOOPER_AGENT_TIMEOUTS_WORKER_SECONDS`
- `LOOPER_AGENT_TIMEOUTS_REVIEWER_SECONDS`
- `LOOPER_AGENT_TIMEOUTS_FIXER_SECONDS`
- `LOOPER_ALLOW_AUTO_COMMIT`
- `LOOPER_ALLOW_AUTO_PUSH`
- `LOOPER_ALLOW_AUTO_APPROVE`
- `LOOPER_REVIEWER_REVIEW_EVENTS_CLEAN`
- `LOOPER_REVIEWER_REVIEW_EVENTS_BLOCKING`
- `LOOPER_FIX_ALL_PULL_REQUESTS`
- `LOOPER_ROLES_PLANNER_AUTO_DISCOVERY`
- `LOOPER_ROLES_PLANNER_TRIGGERS_LABELS`
- `LOOPER_ROLES_PLANNER_TRIGGERS_LABEL_MODE`
- `LOOPER_ROLES_PLANNER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER`
- `LOOPER_ROLES_WORKER_AUTO_DISCOVERY`
- `LOOPER_ROLES_WORKER_TRIGGERS_LABELS`
- `LOOPER_ROLES_WORKER_TRIGGERS_LABEL_MODE`
- `LOOPER_ROLES_WORKER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER`
- `LOOPER_ROLES_REVIEWER_AUTO_DISCOVERY`
- `LOOPER_ROLES_REVIEWER_TRIGGERS_INCLUDE_DRAFTS`
- `LOOPER_ROLES_REVIEWER_TRIGGERS_REQUIRE_REVIEW_REQUEST`
- `LOOPER_ROLES_REVIEWER_TRIGGERS_LABELS`
- `LOOPER_ROLES_REVIEWER_TRIGGERS_LABEL_MODE`
- `LOOPER_ROLES_REVIEWER_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL`
- `LOOPER_ROLES_REVIEWER_SPEC_REVIEW_REVIEWING_LABEL`
- `LOOPER_ROLES_FIXER_AUTO_DISCOVERY`
- `LOOPER_ROLES_FIXER_TRIGGERS_INCLUDE_DRAFTS`
- `LOOPER_ROLES_FIXER_TRIGGERS_LABELS`
- `LOOPER_ROLES_FIXER_TRIGGERS_LABEL_MODE`
- `LOOPER_ROLES_FIXER_TRIGGERS_AUTHOR_FILTER`

Boolean environment variables accept truthy `1`, `true`, `yes`, `on` and falsy `0`, `false`, `no`, `off`.

Example:

```bash
LOOPER_CONFIG="$HOME/custom-looper/config.json" \
LOOPER_PORT=4321 \
LOOPER_ALLOW_AUTO_PUSH=false \
looperd
```

Migration note: the default `looperd` port changed from `4310` to `17310`. Explicit config, `LOOPER_PORT`, and `--port` keep precedence.

Precedence example: config sets port `4310`, `LOOPER_PORT=5000`, and `looperd --port 6000` means daemon uses `6000`.

## CLI flag overrides

Supported `looperd` flags:

- `--config`
- `--host`
- `--port`
- `--db-path`
- `--log-dir`
- `--daemon-mode`
- `--daemon-restart-policy`
- `--daemon-restart-throttle-seconds`
- `--git-path`
- `--gh-path`
- `--osascript-path`
- `--planner-agent-timeout-seconds`
- `--worker-agent-timeout-seconds`
- `--reviewer-agent-timeout-seconds`
- `--fixer-agent-timeout-seconds`
- `--allow-auto-commit`
- `--allow-auto-push`
- `--allow-auto-approve`
- `--reviewer-clean-review-event`
- `--reviewer-blocking-review-event`

Example:

```bash
looperd \
  --config "$HOME/custom-looper/config.json" \
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
- default worktree root must be writable
- required tool paths must resolve
- `osascript` must resolve when osascript notifications are enabled

Writable path recovery:

1. Stop or avoid starting the daemon before changing runtime files.
2. Check ownership and permissions of `~/.looper` and configured storage/log/backup/worktree paths.
3. If paths contain important data (`looper.sqlite`, `backups/`, `worktrees/`), back up or move them before repairs.
4. Prefer fixing ownership/permissions or relocating configured paths over deleting runtime state.
5. If the user still wants deletion, explain it can remove config, DB state, backups, logs, worktrees, daemon binaries, pid, and lifecycle state.

## Recommended first-time setup

1. Install `git` and `gh`.
2. Create `~/.looper/config.json` or run confirmed bootstrap flow.
3. Add at least one project in `projects`.
4. Set `agent.vendor`.
5. Start the daemon with installed `looper` / managed `looperd`.
6. Run `looper config show` to inspect effective config.

If `server.authMode=local-token`, export `LOOPER_TOKEN` before using the CLI.

## Troubleshooting

After config or path changes, validate in this order:

```bash
bash scripts/check.sh
looper daemon status
looper daemon logs --startup
```

Restart only after confirming with the user because `looperd` may launch configured repository automation.

### `tools.gitPath` or `tools.ghPath` could not be resolved

Set explicit paths in config or ensure binaries are on `PATH` for the environment that starts `looperd`. Check `command -v git`, `command -v gh`, and `gh auth status`.

### `tools.osascriptPath is required when osascript notifications are enabled`

Either install/expose `osascript`, set `tools.osascriptPath`, or disable macOS notifications after confirmation.

### A runtime path is not writable

Make sure the daemon user can write to:

- parent directory of `storage.dbPath`
- `daemon.logDir`
- `daemon.workingDirectory`
- default worktree root under `~/.looper/worktrees`

### `server.authMode=local-token` CLI failures

If local-token auth is enabled, `server.localToken` must be configured and CLI calls need matching `LOOPER_TOKEN` in the environment.

## Safety notes

- Ask before creating, overwriting, or deleting `~/.looper/config.json`.
- Never expose secrets from `agent.env`, tokens, or local environment variables.
- Prefer targeted JSON edits over rewriting the whole config.
- Confirm before changing configured projects, worktree roots, defaults that allow auto-push/auto-merge, reviewer approval behavior, or notification settings.
