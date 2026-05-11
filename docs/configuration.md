# Configuration guide

This document explains how `looper` and `looperd` configuration works, where the config file lives, which values can be overridden by environment variables and CLI flags, and what a complete config file looks like.

## Install layout notes

For the default supported macOS install flow:

- `looper` is installed from a GitHub Release Go binary
- `looper daemon install` installs the managed daemon binary to `~/.looper/bin/looperd`
- `looper daemon start` writes its pid file to `~/.looper/looperd.pid`
- `looper daemon start` writes lifecycle diagnostics to `~/.looper/looperd.state.json`

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

`agent.vendor` does not have a built-in default. If you want planner / reviewer / fixer / worker loops to run, set it explicitly.

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
    "timeouts": {
      "plannerSeconds": 1800,
      "workerSeconds": 3600,
      "reviewerSeconds": 1800,
      "fixerSeconds": 1800
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
        "enableSelfReview": false,
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
    },
    "sweeper": {
      "autoDiscovery": false,
      "dryRun": true,
      "triggers": {
        "includeIssues": true,
        "includePullRequests": true,
        "includeDrafts": false,
        "excludeLabels": ["pinned", "security", "looper:sweep-keep"],
        "excludeAuthors": [],
        "excludeAuthorAssociations": ["OWNER", "MEMBER", "COLLABORATOR"],
        "looperInternalLabels": ["looper:plan", "looper:worker-ready", "looper:spec-reviewing", "looper:swept"],
        "reopenCooldownDays": 30,
        "maxPerTick": 10
      },
      "lifecycle": {
        "pendingLabel": "looper:sweep-pending",
        "closedLabel": "looper:swept",
        "keepLabel": "looper:sweep-keep"
      },
      "limits": {
        "maxWarningsPerRepoPerDay": 25,
        "maxClosesPerRepoPerDay": 25,
        "globalKillSwitch": false
      },
      "security": {
        "quarantineLabel": "looper:sweeper-route-security",
        "notifyAssignees": []
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

`roles.sweeper.autoDiscovery` defaults to `false` and `roles.sweeper.dryRun` defaults to `true`, so the sweeper role stays opt-in and observe-only until you explicitly enable it.

## Daemon supervision

Supervision applies only to the `looperd` daemon lifecycle. Looper does not supervise arbitrary user commands or agent subprocesses.

`daemon.mode` controls how `looper daemon start` manages `looperd`:

- `foreground` (default): starts `looperd` as a detached background process. This mode writes `~/.looper/looperd.pid` and `~/.looper/looperd.state.json`, but it is not actively supervised and will not automatically restart after a crash, logout, or reboot.
- `launchd`: on macOS, installs and bootstraps a user LaunchAgent for `looperd`. This mode can restart according to `daemon.restartPolicy` and can start again at login through launchd. On non-macOS platforms it returns an actionable unsupported-platform error.

Restart options:

- `daemon.restartPolicy`: `never`, `on-failure`, or `always` (default `on-failure`). For launchd, `on-failure` maps to `KeepAlive` with unsuccessful-exit semantics, and `always` maps to `KeepAlive=true`.
- `daemon.restartThrottleSeconds`: positive integer throttle passed to the supervisor to avoid tight crash loops (default `10`).
- `daemon.plistPath`: optional macOS LaunchAgent plist path. If omitted, Looper uses `~/Library/LaunchAgents/io.nexu.looper.looperd.plist`.

Runtime diagnostics are discoverable from `looper daemon status` and `looper daemon status --json`:

- state file: `~/.looper/looperd.state.json`
- compatibility pid file: `~/.looper/looperd.pid`
- main daemon log: `~/.looper/logs/looperd.log`
- startup logs: `~/.looper/logs/startup/`
- launchd stdout/stderr logs: `~/.looper/logs/launchd/looperd.stdout.log` and `~/.looper/logs/launchd/looperd.stderr.log`

`looper daemon status` reports mode, PID, start time, supervisor source, restart policy, stale/exited process detection, last exit reason when known or inferred, and log paths. Some abnormal exits, such as SIGKILL, OOM, or reboot, cannot be recorded by the exiting process; the next status command infers them from stale state/PID records.

Use `looper daemon logs` for the main retained daemon log and `looper daemon logs --startup` for recent startup logs.

## Field reference

### `server`

- `host`: bind host, default `127.0.0.1`
- `port`: bind port, default `17310`
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
- `nativeResume.enabled`: enables local-machine native session resume after daemon restart; defaults to `true`
- `timeouts`: role-specific agent execution timeout seconds; defaults are planner `1800`, worker `3600`, reviewer `1800`, fixer `1800`

`vendor` is required for agent-driven loops. If it is omitted, the daemon can still run, but planner / reviewer / fixer / worker loops cannot be created or started.

All timeout values must be positive integers. If a run exceeds its configured role timeout, Looper uses the existing timeout failure and retry behavior.

Native resume is intentionally local-machine only. When supported vendors expose a session/chat identifier (`claude-code`, `codex`, `opencode`, or `cursor-cli`), Looper persists that identifier in SQLite and, after `looperd` recovery, prefers the vendor's resume command before falling back to the normal checkpoint restart path. Vendor session stores are not portable across hosts or users, and resume can still fall back if the underlying CLI cannot reattach. Session capture depends on vendor CLI output that includes a structured session/chat id; use vendor-specific JSON or streaming output flags in `agent.params.args` if the default CLI output does not expose one. Set `LOOPER_AGENT_NATIVE_RESUME_ENABLED=false` to disable native resume via the environment.

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

Default behavior:

- on macOS, `notifications.osascript.enabled` defaults to `true`
- on non-macOS platforms, it defaults to `false`

If `notifications.osascript.enabled` is `true`, `tools.osascriptPath` must resolve.

### `disclosure`

`looperd` adds local text attribution to externally visible content it generates so collaborators can distinguish agent-assisted actions from human-authored actions. This is only a footer or Git trailer written into GitHub text / commit messages; it is not telemetry and does not send additional machine data anywhere.

- `enabled`: enables disclosure stamps, default `true`
- `includeAgent`: includes the configured agent vendor and configured model, default `true`
- `includeOS`: includes only the OS family (`macOS`, `Linux`, or `Windows`), default `false`
- `channels.gitCommit`: add a `Generated-By:` trailer to generated commit bodies without changing commit subjects
- `channels.pullRequest`: add a Markdown footer to generated PR bodies
- `channels.issueComment`: add a Markdown footer to generated issue / PR comments
- `channels.reviewComment`: disclose generated review summaries and inline review comments
- `channels.inlineCommentVisible`: when `false`, inline review comments receive only the hidden marker; when `true`, they receive the visible Markdown footer

Disclosure stamps use an explicit allowlist: product (`looper`), version, runner role, configured agent vendor, configured agent model, and optionally OS family. They do not include hostnames, usernames, local paths, IP or MAC addresses, detailed kernel versions, environment variables, tokens, endpoints, or machine identifiers.

### `tools`

- `gitPath`
- `ghPath`
- `osascriptPath`

If these are omitted, `looperd` tries to detect them from `PATH`. Startup validation fails when required tools cannot be resolved.

### `daemon`

- `mode`: `foreground` or `launchd`
- `restartPolicy`: `never`, `on-failure`, or `always`; applies to supervised modes such as `launchd`
- `restartThrottleSeconds`: positive supervisor restart throttle in seconds
- `plistPath`: optional macOS user LaunchAgent plist path for `launchd` mode
- `logDir`: daemon log directory
- `shutdownTimeoutMs`: graceful shutdown timeout in milliseconds
- `workingDirectory`: working directory used by the daemon
- `environment`: reserved daemon environment map; currently part of the config surface, but not a primary user-facing runtime control in the documented flow

`foreground` starts a detached process only and does not survive crashes or reboot. `launchd` is the supported supervised mode on macOS; unsupported platforms return an actionable error instead of silently falling back.

Defaults:

- `mode`: `foreground`
- `restartPolicy`: `on-failure`
- `restartThrottleSeconds`: `10`
- `logDir`: `~/.looper/logs`
- `shutdownTimeoutMs`: `1000`
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
- `addSnapshotMode`: project-add PR snapshot behavior: `async`, `full`, or `off`; `looper project add --snapshot-mode` overrides this per request. The default is `async`, which queues PR snapshots for background capture so project registration can complete quickly. Use `full` to restore the previous synchronous capture behavior.

`defaults.allowAutoApprove=true` is a legacy alias for reviewer clean approvals. If `reviewer.reviewEvents.clean` is not explicitly configured, it maps clean reviewer outcomes to `APPROVE`; an explicit `reviewer.reviewEvents.clean` value wins.

Default values:

- `baseBranch`: `main`
- `allowAutoCommit`: `true`
- `allowAutoPush`: `true`
- `allowAutoApprove`: `false`
- `allowAutoMerge`: `false`
- `allowRiskyFixes`: `false`
- `fixAllPullRequests`: `false`; legacy fixer discovery switch. Prefer `roles.fixer.triggers.authorFilter` for new config.
- `openPrStrategy`: `all_done`
- `addSnapshotMode`: `async`

### `reviewer`

- `reviewEvents.clean`: review event for clean reviewer outcomes. Allowed values: `COMMENT`, `APPROVE`. Default: `COMMENT`.
- `reviewEvents.blocking`: review event for blocking reviewer outcomes. Allowed values: `COMMENT`, `REQUEST_CHANGES`. Default: `COMMENT`.
- Reviewer loop budget options (`maxIterationsPerPR`, `maxIterationsPerHead`, `maxWallClockSeconds`, `maxConsecutiveFailures`, and `maxAgentExecutionsPerPR`) are deprecated and ignored by the reviewer filter. Reviewer loops keep following PR updates until a clear terminal product state such as the PR closing/merging, an approved Looper review for the current head, or the ready label.

Default reviewer behavior is safe and comment-only:

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

To allow reviewer decision reviews:

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
|---|---:|---:|---|
| `clean` | `COMMENT` | any | `COMMENT` |
| `clean` | `APPROVE` | any | `APPROVE` |
| `non_blocking` | any | any | `COMMENT` |
| `blocking` | any | `COMMENT` | `COMMENT` |
| `blocking` | any | `REQUEST_CHANGES` | `REQUEST_CHANGES` |
| legacy `actionable` | any | any | `COMMENT` |

One-off reviewer jobs can snapshot the policy into loop metadata so queued work is not affected by later daemon config changes:

```bash
looper review owner/repo#123 \
  --clean-review-event APPROVE \
  --blocking-review-event REQUEST_CHANGES
```

To restore the previous synchronous `project add` behavior for one command:

```bash
looper project add --snapshot-mode full /absolute/path/to/repo
```

To restore it by default for all project additions:

```json
{
  "defaults": {
    "addSnapshotMode": "full"
  }
}
```

### `roles`

The `roles` section controls scheduler-driven auto-discovery for planner, reviewer, fixer, worker, and sweeper. It does not block manual commands, direct processing, retries, or already queued work.

Defaults preserve Looper's historical behavior:

- planner discovers open issues labeled `looper:plan` assigned to the current GitHub user
- worker discovers open issues labeled `looper:worker-ready` assigned to the current GitHub user
- reviewer discovers open non-draft PRs where the current user is requested for review, skips self-authored PRs by default, and includes the `looper:spec-reviewing` follow-up path
- fixer discovers open non-draft PRs authored by the current user that have actionable review items
- sweeper is opt-in (`autoDiscovery=false`) and dry-run by default; the current implementation wires the runner skeleton and config surface, while the full warn/close lifecycle lands in a later task

Common fields:

- `roles.<role>.autoDiscovery`: when `false`, the scheduler skips new discovery for that role only
- issue roles (`planner`, `worker`): `triggers.labels`, `triggers.labelMode` (`all` or `any`), and `triggers.requireAssigneeCurrentUser`
- reviewer: `triggers.includeDrafts`, `triggers.requireReviewRequest`, `triggers.enableSelfReview`, `triggers.labels`, `triggers.labelMode`, `specReview.includeReviewingLabel`, `specReview.reviewingLabel`
- fixer: `triggers.includeDrafts`, `triggers.authorFilter` (`current_user` or `any`), `triggers.labels`, `triggers.labelMode`

Trigger fields are combined with logical AND. Label lists use `labelMode=all` or `labelMode=any`; an empty labels list means no label constraint.

For reviewer discovery, `triggers.enableSelfReview` defaults to `false`. When omitted or falsy, non-manual reviewer loops skip pull requests whose normalized PR author login matches the current authenticated GitHub login. Set it to `true` to allow those loops to review self-authored PRs.

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

`defaults.fixAllPullRequests=true` remains supported and maps to `roles.fixer.triggers.authorFilter=any` when `roles.fixer.triggers.authorFilter` is not explicitly configured. If both are present, `roles.fixer.triggers.authorFilter` wins.

Project entries can override supported role settings with `projects[].roles`. Looper resolves these values as built-in defaults → global config/env/CLI `roles` → matching `projects[].roles`; fields omitted from a project role fall back to the effective global role value. Set a project role `instructions` value to an empty string to clear inherited global role instructions for that project.

Supported project role keys match the global role keys for the built-in roles: `planner`, `worker`, `reviewer`, `fixer`, and `sweeper`. Unknown role keys are rejected during config loading. The initially role-overridable settings are auto-discovery, trigger settings, reviewer spec-review label settings, and role instructions. Project role overrides affect scheduler auto-discovery and the role-specific eligibility checks that use those same trigger settings for the matching project.

### `projects`

Each entry registers a repo that `looper` can target.

- `id`: stable project identifier; must be unique
- `name`: display name
- `repoPath`: absolute repository path
- `baseBranch`: optional per-project override
- `worktreeRoot`: optional per-project worktree root
- `roles`: optional per-project role overrides for `planner`, `worker`, `reviewer`, `fixer`, and `sweeper`; absent fields fall back to global `roles`

Example:

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
- `LOOPER_ROLES_REVIEWER_TRIGGERS_ENABLE_SELF_REVIEW`
- `LOOPER_ROLES_REVIEWER_TRIGGERS_LABELS`
- `LOOPER_ROLES_REVIEWER_TRIGGERS_LABEL_MODE`
- `LOOPER_ROLES_REVIEWER_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL`
- `LOOPER_ROLES_REVIEWER_SPEC_REVIEW_REVIEWING_LABEL`
- `LOOPER_ROLES_FIXER_AUTO_DISCOVERY`
- `LOOPER_ROLES_FIXER_TRIGGERS_INCLUDE_DRAFTS`
- `LOOPER_ROLES_FIXER_TRIGGERS_LABELS`
- `LOOPER_ROLES_FIXER_TRIGGERS_LABEL_MODE`
- `LOOPER_ROLES_FIXER_TRIGGERS_AUTHOR_FILTER`

Boolean environment variables accept:

- truthy: `1`, `true`, `yes`, `on`
- falsy: `0`, `false`, `no`, `off`

Example:

```bash
LOOPER_CONFIG="$HOME/custom-looper/config.json" \
LOOPER_PORT=4321 \
LOOPER_ALLOW_AUTO_PUSH=false \
looperd
```

Migration note: the default looperd port changed from `4310` to `17310` to reduce conflicts with other local services. Existing config files, `LOOPER_PORT`, and `--port` values continue to take precedence, so users with an explicit port setting keep their current port.

Example precedence:

- if the config file sets port `4310`
- and `LOOPER_PORT=5000` is exported
- and `looperd --port 6000` is passed

then the daemon uses port `6000`.

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
- `--reviewer-enable-self-review`
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

If you enable `server.authMode=local-token`, also export `LOOPER_TOKEN` before using the CLI.

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
