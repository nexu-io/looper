# Configuration guide

This document explains Looper's canonical config taxonomy, default config location, supported file formats, project override rules, and the legacy-to-canonical migration story.

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

Later layers override earlier ones. Objects are merged deeply, arrays are replaced as a whole, and omitted fields keep the previous-layer value.

## Supported formats and default path

Looper accepts config files in these formats:

- `.toml`
- `.yaml`
- `.yml`
- `.json`

Canonical default path:

- `~/.looper/config.toml`

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

Relative config paths are resolved from the current working directory used to start `looperd`.

## Canonical taxonomy

Looper's frozen canonical top-level config roots are:

| Root | Purpose |
| --- | --- |
| `server` | network-facing API/server configuration |
| `daemon` | daemon lifecycle, runtime paths, and local process behavior |
| `storage` | sqlite/database/backups/history retention and storage-specific settings |
| `scheduler` | loop scheduling, concurrency, polling, and timing policy that is not role-specific |
| `agent` | model/provider/executor defaults that apply across roles unless overridden more locally |
| `logging` | logs, verbosity, sinks, and diagnostic controls |
| `notifications` | user notifications such as osascript or future notifier integrations |
| `disclosure` | disclosure/stamping policy for outward-facing automation output |
| `tools` | external tool paths and tool-specific execution settings such as `git`, `gh`, and `osascript` |
| `package` | packaging, upgrade, and distribution policy |
| `defaults` | user-facing default policy that does not belong to a narrower domain |
| `instructions` | global instruction-system settings that are not role-specific instruction content |
| `roles` | role-specific config grouped by role name, for example `roles.<role>` |
| `projects` | per-project metadata and supported project-scoped overrides |

Legacy top-level `reviewer.*` input is compatibility-only. The canonical reviewer behavior home is `roles.reviewer.behavior.*`.

Schema migration is independent from config-file format migration: precedence stays `defaults → config file → environment variables → CLI flags` regardless of whether a file still uses legacy reviewer paths or legacy JSON defaults.

## Minimal setup

In the simplest setup, you can rely on defaults and only create a config file when you need to customize behavior.

`agent.vendor` does not have a built-in default. If you want planner / reviewer / fixer / worker loops to run, set it explicitly.

Example minimal `~/.looper/config.toml`:

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

Reviewer is the main migration example:

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

[roles.reviewer.behavior.loop]
enabledByDefault = true
quietPeriodSeconds = 60
minPublishIntervalSeconds = 300

[roles.reviewer.behavior.reviewEvents]
clean = "APPROVE"
blocking = "REQUEST_CHANGES"

[roles.reviewer.behavior.nativeResume]
onHeadChange = false
reReviewPromptOnHeadChange = false
```

The reviewer defaults above are intentionally aggressive: clean reviews publish `APPROVE`, blocking reviews publish `REQUEST_CHANGES`, and `enableSelfReview` still defaults to `false`.

## Project override rules

Project entries stay in `projects[]`, but any override-bearing config must mirror the same local shape it uses globally.

Project entries are split into:

- **project metadata**: `id`, `name`, `repoPath`, `baseBranch`, `worktreeRoot`
- **project-scoped override config**: canonical override-bearing domains such as `roles.<role>...`
- **project-local role instructions**: `projects[].roles.<role>.instructions`

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

## Full canonical example

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
shutdownTimeoutMs = 1000

[daemon.environment]
EXAMPLE_FLAG = "1"

[storage]
mode = "sqlite"
dbPath = "/Users/you/.looper/looper.sqlite"
backupDir = "/Users/you/.looper/backups"

[scheduler]
pollIntervalSeconds = 30
maxConcurrentRuns = 3
retryMaxAttempts = 5
retryBaseDelayMs = 5000

[agent]
vendor = "opencode"
model = "your-model-if-needed"

[agent.params]
reasoning = "medium"

[agent.env]
OPENAI_API_KEY = "replace-me"

[agent.nativeResume]
enabled = true

[agent.timeouts]
plannerSeconds = 1800
workerSeconds = 3600
reviewerSeconds = 1800
fixerSeconds = 1800

[logging]
level = "info"
maxSizeMB = 10
maxFiles = 5

[notifications]
inApp = true

[notifications.osascript]
enabled = true
soundForLevels = ["action_required", "failure"]
throttleWindowSeconds = 60

[disclosure]
enabled = true
includeAgent = true
includeOS = false

[disclosure.channels]
gitCommit = true
pullRequest = true
issueComment = true
reviewComment = true
inlineCommentVisible = true

[tools]
gitPath = "/usr/bin/git"
ghPath = "/opt/homebrew/bin/gh"
osascriptPath = "/usr/bin/osascript"

[package]
distribution = "github-release"
autoMigrateOnStartup = true
requireBackupBeforeMigrate = false

[defaults]
baseBranch = "main"
allowAutoCommit = true
allowAutoPush = true
allowAutoApprove = true
allowAutoMerge = false
allowRiskyFixes = false
openPrStrategy = "all_done"
addSnapshotMode = "async"

# `allowAutoApprove` is a legacy compatibility alias.
# Prefer `roles.reviewer.behavior.reviewEvents.clean = "APPROVE"` in new config.

[roles.planner.discovery]
autoDiscovery = true

[roles.planner.discovery.triggers]
labels = ["looper:plan"]
labelMode = "all"
requireAssigneeCurrentUser = true

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

[roles.reviewer.behavior.loop]
enabledByDefault = true
quietPeriodSeconds = 60
minPublishIntervalSeconds = 300

[roles.reviewer.behavior.reviewEvents]
clean = "APPROVE"
blocking = "REQUEST_CHANGES"

[roles.reviewer.behavior.nativeResume]
onHeadChange = false
reReviewPromptOnHeadChange = false

[roles.fixer.discovery]
autoDiscovery = true

[roles.fixer.discovery.triggers]
includeDrafts = false
authorFilter = "current_user"
labels = []
labelMode = "all"

[roles.worker.discovery]
autoDiscovery = true

[roles.worker.discovery.triggers]
labels = ["looper:worker-ready"]
labelMode = "all"
requireAssigneeCurrentUser = true

[roles.sweeper.discovery]
autoDiscovery = false

[roles.sweeper.behavior]
dryRun = true

[roles.sweeper.discovery.triggers]
includeIssues = true
includePullRequests = true
includeDrafts = false
excludeLabels = ["pinned", "security", "looper:sweep-keep"]
excludeAuthors = []
excludeAuthorAssociations = ["OWNER", "MEMBER", "COLLABORATOR"]
looperInternalLabels = ["looper:plan", "looper:worker-ready", "looper:spec-reviewing", "looper:swept"]
reopenCooldownDays = 30
maxPerTick = 10

[roles.sweeper.behavior.lifecycle]
pendingLabel = "looper:sweep-pending"
closedLabel = "looper:swept"
keepLabel = "looper:sweep-keep"

[roles.sweeper.behavior.limits]
maxWarningsPerRepoPerDay = 25
maxClosesPerRepoPerDay = 25
globalKillSwitch = false

[roles.sweeper.behavior.security]
quarantineLabel = "looper:sweeper-route-security"
notifyAssignees = []

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
labels = ["team:alpha", "needs-review"]
labelMode = "any"
requireReviewRequest = false
```

## Migration guide

This refactor is a warning-only migration release.

- Looper does **not** add `looper config migrate` in this change set.
- Looper does **not** rewrite, rename, convert, or delete user config files during startup.
- Loading legacy `~/.looper/config.json` emits one informational note per process telling users that `~/.looper/config.toml` is now the preferred default path.
- Accepted legacy config paths, legacy environment variable names, and legacy CLI flags still load during this release, but they emit actionable replacement guidance.

### Deprecated reviewer migration example

Deprecated legacy JSON:

```json
{
  "reviewer": {
    "scope": "changed_files",
    "publishMode": "single_review",
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
      },
      "specReview": {
        "reviewingLabel": "looper:spec-reviewing"
      },
      "instructions": "Review carefully."
    }
  }
}
```

Canonical replacement:

```toml
[roles.reviewer]
instructions = "Review carefully."

[roles.reviewer.discovery]
autoDiscovery = true

[roles.reviewer.discovery.triggers]
requireReviewRequest = true

[roles.reviewer.discovery.specReview]
reviewingLabel = "looper:spec-reviewing"

[roles.reviewer.behavior]
scope = "changed_files"
publishMode = "single_review"

[roles.reviewer.behavior.reviewEvents]
clean = "APPROVE"
blocking = "REQUEST_CHANGES"
```

### Deprecated project reviewer discovery example

Deprecated legacy JSON:

```json
{
  "projects": [
    {
      "id": "looper",
      "name": "Looper",
      "repoPath": "/absolute/path/to/looper",
      "roles": {
        "reviewer": {
          "autoDiscovery": true,
          "triggers": {
            "labels": ["needs-review"]
          }
        }
      }
    }
  ]
}
```

Canonical replacement:

```toml
[[projects]]
id = "looper"
name = "Looper"
repoPath = "/absolute/path/to/looper"

[projects.roles.reviewer.discovery]
autoDiscovery = true

[projects.roles.reviewer.discovery.triggers]
labels = ["needs-review"]
```

## Environment variables and CLI flags

Canonical environment variables and CLI flags override the config-file layer. Legacy names remain accepted only as compatibility aliases during the migration window.

Examples:

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
- required tool paths must resolve
- `notifications.osascript.enabled=true` requires `tools.osascriptPath` to resolve

## Recommended first-time setup

1. Install `git` and `gh`
2. Create `~/.looper/config.toml`
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

```toml
[notifications.osascript]
enabled = false
```

### A runtime path is not writable

Make sure the daemon user can write to:

- the parent directory of `storage.dbPath`
- `daemon.logDir`
- `daemon.workingDirectory`
- the default worktree root under `~/.looper/worktrees`
