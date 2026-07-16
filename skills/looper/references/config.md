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

## Live reload and dashboard management

`looperd` watches the selected config file and reparses the complete, captured precedence stack while it runs. A valid candidate is published atomically only when every changed effective field is hot-safe. A claim made after publication uses the new snapshot; an already active run keeps the snapshot it started with.

If the file is invalid, or if one changed field is restart-bound, the entire candidate is rejected. The daemon keeps running on its last-known-good snapshot and exposes the error and rejected paths on the Configuration page at `/dashboard/config`. Do not restart merely to apply a hot-safe edit. Correct a rejected file, or restart only when the change is intentionally restart-bound.

Hot-safe fields are deliberately curated:

- `scheduler.maxConcurrentRuns` and `scheduler.slowLaneWarnThresholdMs`
- `agent.vendor`, `agent.model`, individual `agent.env` entries, and the canonical idle/max-runtime fields under `agent.timeouts.*`; configuring the first vendor after startup is supported
- `notifications.inApp`, `notifications.osascript.enabled`, `notifications.osascript.soundForLevels`, and `notifications.osascript.throttleWindowSeconds`
- every current `disclosure.*` field
- `defaults.allowAutoCommit`, `defaults.allowAutoPush`, `defaults.allowRiskyFixes`, `defaults.openPrStrategy`, and `defaults.addSnapshotMode`; `defaults.baseBranch` is restart-bound because configured project records materialize it
- `instructions.enabled` only
- the current Planner `autoDiscovery`, `instructions`, and trigger fields except `roles.planner.triggers.planeAssigneeId`; all current Worker `autoDiscovery`, `triggers.*`, and `instructions` fields; the equivalent current Fixer fields; current Reviewer `discovery.*`, most `behavior.*`, and `instructions`; and current Coordinator `pollInterval`, `triage.*`, `dispatch.*`, and merge-watch policy except `mergeWatch.transientRetries`
- `tools.looperPath` and `tools.osascriptPath`

`agent.vendor` can switch from one configured vendor to another when `agent.params` is empty and an explicit `agent.model` is changed or unset in the same edit instead of being silently carried across vendors. Clearing a configured vendor uses the same guard, preventing a retained profile from passing through an intermediate `null`. Configuring the first vendor may use an already prepared model/params profile. Cross-vendor continuations keep their checkpoint, worktree, HITL answer, and human instructions but start a fresh native session.

Everything else is restart-bound. Important exclusions are `agent.nativeResume.*`, arbitrary `agent.params`, `roles.planner.triggers.planeAssigneeId`, `roles.coordinator.enabled`, `notifications.webhook.*` (including its Feishu transport), all `hitl.*`, `instructions.maxBytes`, `roles.reviewer.autoMerge.*`, `roles.reviewer.behavior.loop.quietPeriodSeconds`, `roles.reviewer.behavior.loop.minPublishIntervalSeconds`, `roles.reviewer.behavior.retry.maxDelayMs`, `roles.coordinator.mergeWatch.transientRetries`, and `roles.coordinator.dependencies.*`. The Planner Plane-assignee field is file-only; Worker `roles.worker.triggers.planeAssigneeId` remains a supported hot-safe control. The scheduler retry budget/base delay and these Reviewer timing fields are durable queue-scheduling inputs; Coordinator transient retries are persisted as a remaining budget. The Reviewer-specific `roles.reviewer.behavior.nativeResume.*` fields are part of the curated Reviewer behavior surface; the global `agent.nativeResume.*` field is not. Listener/storage/runtime ownership, polling/cache topology, logger ownership, providers, projects, `tools.gitPath`, and `tools.ghPath` also require restart. New fields default to restart-bound until explicitly classified. Mixed hot-safe and restart-bound edits are rejected as one candidate; Looper never applies only the convenient half.

Deprecated file-only aliases for `agent.timeouts.{planner,worker,reviewer,fixer}Seconds`, `defaults.allowAutoApprove`, and `defaults.fixAllPullRequests` are watcher-hot compatibility representations of canonical fields. They are never dashboard controls. A canonical dashboard edit retires the matching alias leaf so unsetting the canonical value later cannot reveal stale compatibility policy.

The dashboard provides curated field-level controls rather than a raw config editor:

- environment- or CLI-overridden fields are visible but read-only because those higher-precedence layers remain authoritative
- `agent.env` values are write-only: the dashboard shows configured key names and supports set/replace/remove, but the API never returns values
- `server.localToken`, `daemon.environment`, and arbitrary `agent.params` remain file-only
- projects remain managed by the Projects API and SQLite catalog, not the generic Configuration page
- each config read returns the revision captured with its published values; each patch submits it and repeats an identity/mode/byte check immediately before atomic rename, catching changes present before that final check
- portable filesystems leave a tiny final-check-to-rename race, so do not combine simultaneous manual and dashboard writes
- in `local-token` mode, PATCH uses normal token authentication; without token authentication, `PATCH /api/v1/config` requires a direct loopback peer and Host authority and rejects proxy-forwarding headers
- dashboard writes preserve the selected TOML/YAML/JSON format, unknown top-level extension sections and their native scalar values, and ordinary permission bits, and are validated before atomic replacement; ACLs and extended filesystem metadata are not guaranteed to survive
- serialization may canonicalize comments, quoting, key/table order, and other lexical formatting
- a symlinked config path can be watched for external edits, but dashboard PATCH refuses to replace it; edit the symlink target directly

After an external edit, use `/dashboard/config` to confirm the last attempt and last applied time. For a rejected candidate, inspect the displayed paths or daemon logs, then make a targeted correction. Do not expose config secrets while troubleshooting.

## Canonical taxonomy

Looper's frozen canonical top-level config roots are:

- `server`
- `daemon`
- `storage`
- `scheduler`
- `webhook`
- `network`
- `agent`
- `logging`
- `notifications`
- `disclosure`
- `tools`
- `package`
- `defaults`
- `instructions`
- `hitl`
- `roles`
- `providers`
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

## Provider config

Provider kinds:

- `github` — legacy default, backed by `gh`. Projects without `provider` keep GitHub autodetection/metadata behavior.
- `forgejo` — REST-backed MVP. Forgejo-only configs do not require `gh` when `git` and the provider auth are valid (`token-env` or `tea`).

Minimal Forgejo project (token-env):

```toml
[agent]
vendor = "opencode"

[[providers]]
id = "forgejo-main"
kind = "forgejo"
baseUrl = "https://code.example.com"
auth = "token-env"
tokenEnv = "LOOPER_FORGEJO_TOKEN"

# Or reuse an explicit tea login (no tokenEnv):
# auth = "tea"
# teaLogin = "powerformer-code"

[[projects]]
id = "example"
name = "Example"
repoPath = "/absolute/path/to/example"
provider = "forgejo-main"
repo = "acme/example"
```

Forgejo validation notes:

- `baseUrl` must be an absolute `http(s)` URL.
- Choose `auth = "token-env"` with `tokenEnv`, or `auth = "tea"` with explicit `teaLogin` matching `baseUrl`. Never rely on tea's default login when multiple identities exist. Do not write token values into config.
- Forgejo projects require a `provider` and repo. Configure them in `[[projects]]`, or persist and activate them immediately with `looper project add --provider <id>`; the repo may be detected only from an origin matching that provider.
- Duplicate `repo` values are rejected case-insensitively, even across providers.
- Forgejo uses polling only; omit project `webhook.mode` and keep `network.mode` off.
- The provider profile disables unsupported GitHub-shaped defaults. Explicit opt-ins to Forgejo-unsupported behavior fail fast.

Forgejo MVP role limits:

- planner and worker are supported
- worker only processes issues already assigned to the current Forgejo user; it does not self-assign
- reviewer discovers by labels and publishes a top-level Reviewer Summary PR comment; default normal-review label is `looper:review`, while spec PRs use `looper:spec-reviewing`
- fixer consumes open items from the Reviewer Summary and publishes a top-level Fixer Summary PR comment; it does not resolve native review threads
- coordinator, auto-merge, native reviews, review requests, thread resolution, routed network mode, and webhooks are unsupported for Forgejo

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
plannerIdleTimeoutSeconds = 600
plannerMaxRuntimeSeconds = 3600
workerIdleTimeoutSeconds = 900
workerMaxRuntimeSeconds = 10800
reviewerIdleTimeoutSeconds = 600
reviewerMaxRuntimeSeconds = 5400
fixerIdleTimeoutSeconds = 600
fixerMaxRuntimeSeconds = 7200

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

[[providers]]
id = "forgejo-main"
kind = "forgejo"
baseUrl = "https://code.example.com"
tokenEnv = "LOOPER_FORGEJO_TOKEN"

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

[[projects]]
id = "forgejo-example"
name = "Forgejo Example"
repoPath = "/absolute/path/to/forgejo-example"
provider = "forgejo-main"
repo = "acme/forgejo-example"

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
- `gh` must resolve for GitHub projects; Forgejo-only configs do not require `gh`
- Forgejo providers require `baseUrl` and either `tokenEnv` (`token-env` auth) or `teaLogin` (`tea` auth); token-env needs the named env var at runtime, tea needs a matching login for the daemon user
- `notifications.osascript.enabled=true` requires `tools.osascriptPath` to resolve

## Safety notes

- Ask before creating, overwriting, or deleting `~/.looper/config.toml`.
- Never expose secrets from `agent.env`, tokens, or local environment variables.
- Prefer targeted TOML edits over rewriting the whole config.
- After a targeted edit, verify its reload result before proposing a restart. Active runs intentionally retain their original snapshot.
- Confirm before changing configured projects, worktree roots, defaults that allow auto-push or risky fixes, reviewer approval behavior, or notification settings.
