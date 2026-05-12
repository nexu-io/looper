# Global config architecture refactor

Base branch: `main`

## Problem

Looper's config model has grown around implementation boundaries rather than a stable user-facing taxonomy.

Today, users have to infer where config belongs from code history:

- some behavior lives in top-level roots that describe a subsystem implementation rather than a user concern
- some role behavior lives under `roles.<role>` while closely related knobs live elsewhere
- project-level overrides mirror only parts of the global config tree
- config-file support is JSON-first and the default on-disk path still assumes `~/.looper/config.json`

This creates four UX problems:

1. **Poor discoverability** — users cannot predict where a setting should live.
2. **Poor consistency** — similar concerns use different nesting styles and naming rules.
3. **Poor override ergonomics** — project-level overrides do not reliably mirror the global shape.
4. **Migration drag** — expanding config shape and config format at the same time is harder when there is no canonical model.

The current role/config split is only one symptom of a broader taxonomy problem. The product now needs a global config refactor that defines one canonical taxonomy for the entire config surface.

## Goals

- Define one user-facing canonical config taxonomy for all supported config domains.
- Make top-level config reflect cross-cutting product concerns rather than implementation details.
- Make role config consistently live under `roles.<role>`.
- Make project-level overrides mirror the global config shape wherever override semantics are supported.
- Add TOML and YAML config-file support in addition to JSON compatibility.
- Make TOML the default documented and generated config format.
- Preserve the current layer precedence model: defaults → config file → env → CLI.
- Provide a migration path from legacy roots to canonical roots with low breakage and actionable warnings.

## Non-goals

- Do not intentionally change runtime behavior, scheduling behavior, or effective defaults except where required by canonicalization, validation, or unambiguous precedence rules.
- Do not remove JSON support in this change.
- Do not require users to migrate file format and schema in the same step.
- Do not invent per-project overrides for config that is not meaningfully project-scoped today.
- Do not redesign unrelated runtime semantics under the guise of config cleanup.

## Current-state summary

The current config surface mixes at least four dimensions:

- **system concerns** such as daemon/runtime/storage/logging/tool paths
- **workflow concerns** such as scheduling, discovery, and automation policy
- **role concerns** such as role behavior, discovery policy, and role instructions
- **scope concerns** such as global defaults versus project-local overrides

Those dimensions are not expressed uniformly in either code or docs. As a result:

- users cannot infer the canonical path for a setting from first principles
- docs and generated config have to teach exceptions
- normalization logic carries product-model debt in addition to format parsing debt
- introducing TOML/YAML support without a taxonomy cleanup would preserve the same confusion in more formats

## Design principles

The refactor should follow these rules:

1. **One concept, one canonical path**
   - Each supported setting has exactly one canonical home.

2. **Top-level config is for cross-cutting product concerns**
   - Top-level roots should describe stable product domains, not transient implementation seams.

3. **Role config lives under `roles.<role>`**
   - Role-specific behavior, discovery, instructions, and policy do not belong in unrelated top-level roots.

4. **Project overrides mirror the global shape**
   - When a setting is overrideable per project, the project path should use the same local shape as the global path.

5. **Normalization happens before merging across layers**
   - Legacy schema handling is resolved inside each layer before precedence is applied across layers.

6. **Primary docs show only canonical config**
   - Legacy paths belong in migration guidance and warnings, not in the main examples.

7. **One canonical default format for new users**
   - New documentation, generated config, and bootstrap flows should prefer TOML.

## Canonical taxonomy

This change should freeze a canonical top-level taxonomy for all config. The exact field inventory inside each root can still be refined during implementation, but the canonical root model itself should be:

- `server` — network-facing API/server configuration
- `daemon` — daemon lifecycle, runtime paths, and local process behavior
- `storage` — sqlite/database/backups/history retention and storage-specific settings
- `scheduler` — loop scheduling, concurrency, polling, and timing policy that is not role-specific
- `agent` — model/provider/executor defaults that apply across roles unless overridden more locally
- `logging` — logs, verbosity, sinks, and diagnostic controls
- `notifications` — user notifications such as osascript or future notifier integrations
- `disclosure` — disclosure/stamping policy for outward-facing automation output
- `tools` — external tool paths and tool-specific execution settings such as `git`, `gh`, and `osascript`
- `package` — packaging, upgrade, and distribution policy
- `defaults` — user-facing default policy that does not belong to a narrower domain
- `instructions` — global instruction-system settings that are not role-specific instruction content
- `roles` — role-specific config grouped by role name, for example `roles.<role>`
- `projects` — per-project metadata and supported project-scoped overrides

New canonical roots should only be added when they represent a stable cross-cutting product concern.

### Current-root inventory and intended canonical destinations

The current top-level config roots that must be accounted for in this refactor are:

- `server`
- `storage`
- `scheduler`
- `agent`
- `logging`
- `notifications`
- `disclosure`
- `tools`
- `daemon`
- `package`
- `defaults`
- `reviewer`
- `instructions`
- `roles`
- `projects`

The intended canonical destination of each current root is:

| Current root | Intended canonical destination | Notes |
| --- | --- | --- |
| `server` | `server` | Keep canonical. |
| `storage` | `storage` | Keep canonical. |
| `scheduler` | `scheduler` | Keep canonical for non-role scheduling policy. |
| `agent` | `agent` | Keep canonical for cross-role agent defaults. |
| `logging` | `logging` | Keep canonical. |
| `notifications` | `notifications` | Keep canonical. |
| `disclosure` | `disclosure` | Keep canonical; easy to miss but currently user-facing. |
| `tools` | `tools` | Keep canonical for explicit external tool path/config settings. |
| `daemon` | `daemon` | Keep canonical. |
| `package` | `package` | Keep canonical. |
| `defaults` | `defaults` | Keep canonical for user-facing default policy. |
| `reviewer` | `roles.reviewer.behavior` | Legacy-only after migration; no longer canonical at top level. |
| `instructions` | exact split by concern | Existing top-level `instructions.*` system controls stay under canonical top-level `instructions.*`; role instruction text maps to `roles.<role>.instructions`; project-local role instruction text maps to `projects[].roles.<role>.instructions`; any convenience project instruction map that does not follow that shape must normalize to that target or be explicitly deprecated. |
| `roles` | `roles` | Keep canonical root for all role-specific config. |
| `projects` | `projects` | Keep canonical root for project metadata plus project-scoped overrides. |

This table is the minimum freeze required before implementation starts. If any row changes, the spec, checklist, and migration mapping inventory must change with it.

## Role model

All role-specific config should live under `roles.<role>`.

For roles that have distinct selection policy vs runtime policy, the preferred structure is:

```toml
[roles.<role>]
instructions = "..."

[roles.<role>.discovery]

[roles.<role>.behavior]
```

When a role has distinct selection policy and runtime policy, those concerns should stay inside that role's canonical root rather than spilling into unrelated top-level config.

For example, a role may keep:

- discovery policy under `roles.<role>.discovery`
- runtime behavior under `roles.<role>.behavior`
- shared instructions at `roles.<role>.instructions`

Other roles do not need to be forced into identical subgroups unless the taxonomy is actually useful for that role, but every role must end this change with one canonical root under `roles.<role>`.

### Worked migration example: reviewer

Reviewer is the highest-risk migration example because it currently spans both a legacy top-level root and a role root.

Current user-visible shape may combine:

```json
{
  "reviewer": {
    "scope": "changed_files",
    "publishMode": "single_review",
    "loop": { "quietPeriodSeconds": 120 },
    "reviewEvents": { "clean": "APPROVE", "blocking": "REQUEST_CHANGES" }
  },
  "roles": {
    "reviewer": {
      "autoDiscovery": true,
      "triggers": { "requireReviewRequest": true },
      "specReview": { "reviewingLabel": "looper:spec-reviewing" },
      "instructions": "..."
    }
  }
}
```

Canonical target shape becomes:

```toml
[roles.reviewer]
instructions = "..."

[roles.reviewer.discovery]
autoDiscovery = true

[roles.reviewer.discovery.triggers]
requireReviewRequest = true

[roles.reviewer.discovery.specReview]
reviewingLabel = "looper:spec-reviewing"

[roles.reviewer.behavior]
scope = "changed_files"
publishMode = "single_review"

[roles.reviewer.behavior.loop]
quietPeriodSeconds = 120

[roles.reviewer.behavior.reviewEvents]
clean = "APPROVE"
blocking = "REQUEST_CHANGES"
```

The migration rule is:

- legacy top-level `reviewer.*` becomes compatibility input only
- canonical `roles.reviewer.*` becomes the only documented destination
- if canonical and legacy reviewer paths both target the same effective field in one layer, canonical wins and legacy emits a deprecation warning

## Project override model

Project-level overrides should stop being an exception-oriented side branch.

The canonical rule should be:

> If a field is overrideable per project, the project-level path uses the same local shape as the global canonical path.

That means project entries should preserve the existing `projects[]` list, but the override-bearing parts of each project entry must mirror the canonical config tree for the domains they are allowed to override.

Project entries must be structurally separated into:

- **project metadata** — identity and repo-local facts such as `id`, `name`, `repoPath`, `path` compatibility aliases, `baseBranch`, and `worktreeRoot`
- **project-scoped override config** — only the domains that are explicitly supported for per-project override in this refactor
- **project-local instructions** — if kept distinct from generic override config, they must still map cleanly onto canonical role instruction targets and have explicit precedence semantics

Examples:

- role overrides remain role-shaped under `projects[].roles.<role>...`
- if scheduler or agent settings are made project-overrideable, they must mirror the same local schema they use globally
- project-local metadata that is not an override should stay clearly separated from project-local override config

This refactor must explicitly decide which current project fields are metadata-only, which are override-bearing, and whether project instruction maps remain a separate convenience surface or are fully absorbed into canonical override paths.

A worked canonical project shape should look like:

```toml
[[projects]]
id = "open-design"
name = "Open Design"
repoPath = "/repos/open-design"
baseBranch = "main"
worktreeRoot = "/tmp/looper-worktrees"

[projects.roles.reviewer]
instructions = "Project-specific reviewer guidance"

[projects.roles.reviewer.discovery.triggers]
requireReviewRequest = true

[projects.roles.reviewer.behavior.reviewEvents]
blocking = "COMMENT"
```

In that shape:

- `id`, `name`, `repoPath`, `baseBranch`, and `worktreeRoot` are metadata
- `projects[].roles.<role>...` is override-bearing config
- project-local role instruction text lives at `projects[].roles.<role>.instructions`

Project overrides remain part of the config-file layer. They do not create a new precedence layer above environment variables or CLI flags.

## Migration model

Two migrations are coupled in delivery but must remain logically independent:

1. **Schema migration**
   - from legacy roots and mixed naming styles
   - to one canonical global taxonomy and canonical role roots

2. **Format migration**
   - from JSON-default
   - to TOML-default with YAML and JSON still supported

Users must be able to do either of these independently:

- keep legacy schema in JSON during the migration window
- adopt canonical schema in JSON first
- adopt canonical schema in TOML or YAML later
- adopt TOML while still relying on compatibility normalization for legacy paths

## Config-file format support

Looper should accept config files in:

- `.toml`
- `.yaml`
- `.yml`
- `.json`

The canonical default generated path should become:

- `~/.looper/config.toml`

Source-selection precedence should be:

1. `--config`
2. `LOOPER_CONFIG`
3. default-path discovery

Default-path discovery should check, in order:

1. `~/.looper/config.toml`
2. `~/.looper/config.yaml`
3. `~/.looper/config.yml`
4. `~/.looper/config.json`

Behavior should be:

- if exactly one supported default config file exists, load it
- if multiple supported default config files exist, fail clearly instead of guessing
- if none exist, continue with built-in defaults and treat `~/.looper/config.toml` as the canonical path for newly generated config

## Precedence and merge rules

This refactor should preserve the mental model:

- defaults
- config file
- environment
- CLI

Inside that model:

- accepted legacy schema should be normalized to canonical schema within each layer before cross-layer merge
- objects should continue to deep-merge unless a domain explicitly documents a different rule
- arrays should continue to replace rather than merge element-wise unless a domain explicitly documents a different rule
- omitted fields inherit from earlier layers

Project overrides remain inside the config-file layer and are applied according to the canonical override policy for the matching project.

Canonical-vs-legacy conflict behavior is part of the spec, not an implementation detail:

- within a single layer, accepted legacy inputs must first normalize to canonical targets
- if canonical and legacy paths in the same layer target the same effective field, canonical wins
- the losing legacy path should emit a deprecation warning rather than silently changing precedence semantics
- mixed-schema inputs are valid only when normalization is deterministic
- structurally incompatible inputs for the same canonical target must be rejected with a clear validation error

## Validation and deprecation policy

This change should introduce a global config migration policy rather than one-off migration rules for a single role or subsystem.

The implementation should:

- validate canonical config directly
- continue accepting supported legacy paths during the migration window
- emit actionable warnings for deprecated legacy paths with exact replacement paths
- reject unsupported file suffixes with clear errors
- define the deprecation window for legacy paths before they become hard errors
- define future error text for removed legacy paths

Warnings should be emitted once per logical deprecated path, not once per nested leaf.

## Documentation expectations

After this refactor:

- `docs/configuration.md` should explain the canonical taxonomy first
- `skills/looper/references/config.md` should match product docs on roots, formats, and default path
- primary examples should use canonical TOML
- migration sections may show legacy JSON/YAML/TOML examples, but only as explicitly labeled migration material
- generated templates, help text, and bootstrap output should use the same canonical taxonomy and terminology as the docs

## Deliverables

Implementation must produce:

1. a canonical config taxonomy table
2. a legacy-to-canonical mapping inventory for every supported legacy path
3. config loading support for TOML/YAML/JSON
4. canonical env-var and CLI-flag naming rules with any compatibility aliases
5. updated validation, warnings, and migration guidance
6. updated docs/help/templates/examples
7. automated tests that prove schema parity, format parity, precedence, and migration behavior

## Open questions to resolve during implementation

- Which config domains deserve their own canonical subgroup vs staying flat within a top-level concern?
- Which global settings, beyond role config, are legitimately project-overrideable?
- Migration UX decisions for helper-vs-guidance, legacy `config.json` notes, file-safety guarantees, and hard-error timing are recorded in `reference/migration-ux-decision.md`.

## Recommended outcome

Adopt a global canonical config taxonomy with TOML as the default generated format, roles consistently nested under `roles.<role>`, project overrides that mirror the global shape wherever supported, and a compatibility layer that lets legacy schema and legacy file formats keep working during a documented migration window.
