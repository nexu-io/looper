# Looper configuration reference for agents

Use this reference before inspecting or changing `~/.looper/config.toml`. Do not overwrite user config; make targeted edits only after confirmation and redact secrets in summaries.

## Canonical loading summary

`looperd` loads configuration in this order:

1. built-in defaults
2. config file
3. environment variables
4. CLI flags

Later layers override earlier ones. Objects are merged deeply, arrays are replaced as a whole, and omitted fields keep the previous-layer value.

Supported config formats:

- `.toml`
- `.yaml`
- `.yml`
- `.json`

Canonical default path:

```text
~/.looper/config.toml
```

For webhook-mode troubleshooting in this repository, users may also have a JSON config at `~/.looper/config.json`. Respect whichever supported config file Looper is actually loading.

Config source selection precedence is:

1. `--config`
2. `LOOPER_CONFIG`
3. default-path discovery

Default-path discovery checks, in order:

1. `~/.looper/config.toml`
2. `~/.looper/config.yaml`
3. `~/.looper/config.yml`
4. `~/.looper/config.json`

Behavior:

- if exactly one supported default config file exists, Looper loads it
- if multiple supported default config files exist, Looper fails clearly instead of guessing
- if none exist, Looper continues with built-in defaults and treats `~/.looper/config.toml` as the canonical path for newly generated config

Custom config path examples:

- `LOOPER_CONFIG=/absolute/or/relative/path/to/config.toml`
- `looperd --config /absolute/or/relative/path/to/config.toml`

Relative config paths resolve from the working directory used to start `looperd`.

## Canonical taxonomy

Looper's frozen canonical top-level config roots are:

- `server`
- `daemon`
- `storage`
- `scheduler`
- `agent`
- `logging`
- `notifications`
- `disclosure`
- `tools`
- `package`
- `defaults`
- `instructions`
- `roles`
- `projects`

Legacy top-level `reviewer.*` input is compatibility-only. The canonical reviewer behavior home is `roles.reviewer.behavior.*`.

Schema migration is independent from config-file format migration: precedence stays `defaults → config file → environment variables → CLI flags` regardless of whether a file still uses legacy reviewer paths or legacy JSON defaults.

## Minimal canonical config

`agent.vendor` has no built-in default. Set it when planner / reviewer / fixer / worker loops should run.

```toml
[agent]
vendor = "opencode"

[[projects]]
id = "looper"
name = "Looper"
repoPath = "/absolute/path/to/repo"
```

## Role model guidance

All role-specific config lives under `roles.<role>`.

- shared role instructions live at `roles.<role>.instructions`
- discovery policy lives at `roles.<role>.discovery.*`
- runtime behavior lives at `roles.<role>.behavior.*` when that split is useful for the role

Reviewer migration rules:

- legacy top-level `reviewer.*` is compatibility input only
- legacy reviewer discovery paths such as `roles.reviewer.autoDiscovery`, `roles.reviewer.triggers.*`, and `roles.reviewer.specReview.*` are compatibility input only
- canonical reviewer discovery lives at `roles.reviewer.discovery.*`
- canonical reviewer behavior lives at `roles.reviewer.behavior.*`

Canonical reviewer example:

This is a standalone reviewer-only snippet. Do not paste it together with the full config example below as a single TOML file, or table headers such as `[roles.reviewer.behavior.reviewEvents]` would be duplicated.

```toml
[roles.reviewer]
instructions = "Review for correctness, regressions, and migration safety."

[roles.reviewer.discovery]
autoDiscovery = true

[roles.reviewer.discovery.triggers]
includeDrafts = false
requireReviewRequest = true
enableSelfReview = false
labels = []
labelMode = "all"

[roles.reviewer.discovery.specReview]
includeReviewingLabel = true
reviewingLabel = "looper:spec-reviewing"

[roles.reviewer.behavior]
scope = "changed_ranges"
publishMode = "single_review"

[roles.reviewer.behavior.reviewEvents]
clean = "APPROVE"
blocking = "REQUEST_CHANGES"
```

`defaults.allowAutoApprove` is still accepted as a legacy compatibility alias, but the canonical way to control reviewer publishing is `roles.reviewer.behavior.reviewEvents.*`.

## Project override rules

Project entries stay in `projects[]`, but any override-bearing config must mirror the same local shape it uses globally.

Project entries are split into:

- metadata: `id`, `name`, `repoPath`, `baseBranch`, `worktreeRoot`
- project-scoped override config: canonical override-bearing domains such as `roles.<role>...`
- project-local role instructions: `projects[].roles.<role>.instructions`

Project override rules:

- if a field is overrideable per project, the project path uses the same local canonical shape as the global path
- project overrides remain part of the config-file layer; they do not create a new precedence layer above environment variables or CLI flags
- omitted project fields inherit the effective global value
- project-local role instructions may be set to an empty string to clear inherited global role instructions for that project
- legacy project reviewer discovery paths are compatibility-only; canonical reviewer project overrides live under `projects[].roles.reviewer.discovery.*`

Canonical project override example:

```toml
[[projects]]
id = "looper"
name = "Looper"
repoPath = "/absolute/path/to/looper"
baseBranch = "main"
worktreeRoot = "/Users/you/.looper/worktrees/looper"

[projects.roles.worker.discovery]
autoDiscovery = false

[projects.roles.reviewer]
instructions = "Project-specific reviewer guidance"

[projects.roles.reviewer.discovery.triggers]
labels = ["needs-review"]
labelMode = "any"
requireReviewRequest = false
```

## Full canonical config shape

Use this as a map of supported sections, not as a template to paste wholesale:

```toml
[server]
host = "127.0.0.1"
port = 17310
authMode = "local-token"
localToken = "replace-me"

[daemon]
mode = "foreground"
restartPolicy = "on-failure"
restartThrottleSeconds = 10
logDir = "/Users/you/.looper/logs"
workingDirectory = "/absolute/path/to/where/you/start/looperd"

[storage]
mode = "sqlite"
dbPath = "/Users/you/.looper/looper.sqlite"
backupDir = "/Users/you/.looper/backups"

[scheduler]
pollIntervalSeconds = 30
maxConcurrentRuns = 3
retryMaxAttempts = 5
retryBaseDelayMs = 5000

[webhook]
enabled = false
fallbackPollIntervalSeconds = 300

[agent]
vendor = "opencode"

[agent.timeouts]
plannerSeconds = 1800
workerSeconds = 3600
reviewerSeconds = 1800
fixerSeconds = 1800

[logging]
level = "info"

[notifications]
inApp = true

[notifications.osascript]
enabled = true

[disclosure]
enabled = true

[tools]
gitPath = "/usr/bin/git"
ghPath = "/opt/homebrew/bin/gh"
osascriptPath = "/usr/bin/osascript"

[defaults]
baseBranch = "main"
openPrStrategy = "all_done"
addSnapshotMode = "async"

[roles.planner.discovery]
autoDiscovery = true

[roles.planner.discovery.triggers]
labels = ["looper:plan"]
labelMode = "all"
requireAssigneeCurrentUser = true

[roles.reviewer.discovery]
autoDiscovery = true

[roles.reviewer.discovery.triggers]
requireReviewRequest = true
enableSelfReview = false

[roles.reviewer.discovery.specReview]
includeReviewingLabel = true
reviewingLabel = "looper:spec-reviewing"

[roles.reviewer.behavior]
scope = "changed_ranges"
publishMode = "single_review"

[roles.reviewer.behavior.reviewEvents]
clean = "APPROVE"
blocking = "REQUEST_CHANGES"

[roles.fixer.discovery]
autoDiscovery = true

[roles.fixer.discovery.triggers]
authorFilter = "current_user"

[roles.worker.discovery]
autoDiscovery = true

[roles.worker.discovery.triggers]
labels = ["looper:worker-ready"]
labelMode = "all"
requireAssigneeCurrentUser = true

[[projects]]
id = "looper"
name = "Looper"
repoPath = "/absolute/path/to/looper"

[projects.roles.reviewer.discovery.triggers]
labels = ["needs-review"]
labelMode = "any"
requireReviewRequest = false
```

## Webhook-related config guidance

When diagnosing or editing webhook mode, pay attention to:

- `webhook.enabled` — turns webhook mode on or off.
- `webhook.fallbackPollIntervalSeconds` — polling fallback cadence when forwarders are unavailable.
- `server.host` — must remain loopback for local forwarders.
- `server.host` and `server.port` — webhook forwarders derive the local listener endpoint from these fields, so `server.host` must stay loopback and `server.port` must match the running daemon.
- `tools.ghPath` — set explicitly when `looperd` cannot resolve `gh` from its runtime environment.

Targeted checks:

- If `looper webhook status` warns about loopback, inspect `server.host` first.
- If it warns that `gh` could not be resolved, set `tools.ghPath` explicitly instead of rewriting unrelated config.
- If webhook mode is degraded but config is correct, prefer `looper webhook cleanup owner/repo` or auth troubleshooting before editing config.

## Runtime paths

Default runtime artifacts live under `~/.looper/`:

- `config.toml` (canonical default)
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

## Migration story

This refactor is a warning-only migration release.

- Looper does **not** add `looper config migrate` in this change set.
- Looper does **not** rewrite, rename, convert, or delete user config files during startup.
- Loading legacy `~/.looper/config.json` emits one informational note per process telling users that `~/.looper/config.toml` is now the preferred default path.
- Accepted legacy config paths, legacy environment variable names, and legacy CLI flags still load during this release, but they emit actionable replacement guidance.

Deprecated legacy reviewer example:

```json
{
  "reviewer": {
    "reviewEvents": {
      "clean": "APPROVE",
      "blocking": "REQUEST_CHANGES"
    }
  },
  "roles": {
    "reviewer": {
      "autoDiscovery": true,
      "triggers": {
        "requireReviewRequest": true
      }
    }
  }
}
```

Canonical replacement:

```toml
[roles.reviewer.discovery]
autoDiscovery = true

[roles.reviewer.discovery.triggers]
requireReviewRequest = true

[roles.reviewer.behavior.reviewEvents]
clean = "APPROVE"
blocking = "REQUEST_CHANGES"
```

Note: the snippets above show the current aggressive reviewer defaults. `defaults.allowAutoApprove` remains a compatibility alias for `roles.reviewer.behavior.reviewEvents.clean = "APPROVE"`, but new config should prefer the canonical `reviewEvents` fields.

## Override examples

Canonical environment and CLI names should be preferred. Legacy names remain compatibility aliases during the migration window.

```bash
LOOPER_CONFIG="$HOME/custom-looper/config.toml" \
LOOPER_PORT=4321 \
LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_ENABLE_SELF_REVIEW=true \
looperd
```

```bash
looperd \
  --config "$HOME/custom-looper/config.toml" \
  --port 4321 \
  --roles-reviewer-discovery-triggers-enable-self-review=true
```

## Validation notes

`looperd` fails fast on invalid config. Common checks:

- required strings must be non-empty
- numeric fields must be positive integers where applicable
- `server.port` must be between `1` and `65535`
- `scheduler.pollIntervalSeconds` must be at least `10`
- `authMode=local-token` requires `server.localToken`
- `projects[].id` must be valid and unique
- storage, log, working-directory, and worktree paths must be writable
- required tool paths must resolve
- `notifications.osascript.enabled=true` requires `tools.osascriptPath` to resolve

## Safety notes

- Ask before creating, overwriting, or deleting `~/.looper/config.toml`.
- Never expose secrets from `agent.env`, tokens, or local environment variables.
- Prefer targeted TOML edits over rewriting the whole config.
- Confirm before changing configured projects, worktree roots, defaults that allow auto-push/auto-merge, reviewer approval behavior, or notification settings.
