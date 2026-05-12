# Reviewer config UX consolidation

Issue: TBD
Base branch: `main`

## Problem

Looper currently exposes reviewer configuration across multiple namespaces that all read like “reviewer config” to users, but mean different things:

- top-level `reviewer.*` controls reviewer runtime behavior such as `loop`, `reviewEvents`, and `threadResolution`
- `roles.reviewer.*` controls reviewer discovery and role policy such as `autoDiscovery`, `triggers`, `specReview`, and `instructions`
- `projects[].roles.reviewer.*` can override only the role-policy branch, not the top-level reviewer branch

This is internally coherent but externally confusing. Users expect reviewer configuration to live in one place. Instead, they must learn an implementation split:

1. behavior lives in `reviewer`
2. discovery lives in `roles.reviewer`
3. project overrides live only under `projects[].roles.reviewer`

That creates three UX problems:

- poor discoverability: users do not know which reviewer knob lives where
- poor predictability: project-level overrides do not mirror the full global reviewer shape
- high surprise under deep merge: two partial reviewer trees combine into one effective behavior

## Goals

- Give reviewer configuration one canonical home.
- Make global and project-level reviewer config follow the same mental model.
- Preserve the existing behavioral distinction between discovery policy and review behavior without forcing users to learn two root namespaces.
- Add YAML and TOML config-file support in addition to existing JSON compatibility.
- Make TOML the default config format and default generated config file going forward.
- Provide a migration path with low breakage and clear deprecation messaging.
- Keep config layering semantics unchanged: defaults → config file → env → CLI flags.

## Non-goals

- Do not redesign non-reviewer roles in this change.
- Do not change reviewer runtime behavior, scheduler semantics, or effective defaults beyond config shape and override rules.
- Do not remove project-level reviewer overrides.
- Do not change deep-merge versus array-replace semantics.
- Do not immediately remove JSON config-file support.

## Current state summary

The split is intentional in code today:

- `internal/config/types.go` defines top-level `ReviewerConfig` separately from `ReviewerRoleConfig`
- `internal/config/normalize.go` merges them separately via `mergeReviewerConfig` and `mergeReviewerRoleConfig`
- `internal/config/project_roles.go` applies project overrides only to `roles`
- `docs/configuration.md` documents `reviewer` and `roles` as separate sections

The problem is therefore not accidental inconsistency. It is a product-model mismatch between implementation boundaries and user expectations.

Separately, config-file format support is currently JSON-only:

- `internal/config/load.go` uses JSON-specific decoding with no suffix-based format dispatch
- `internal/config/defaults.go` hardcodes the default config path as `~/.looper/config.json`
- `docs/configuration.md` and `skills/looper/references/config.md` assume `config.json` and JSON examples only
- config tests and parity fixtures are JSON-oriented

That means reviewer UX consolidation and config format expansion are coupled in practice: if Looper is changing the canonical config shape, it should also define the future canonical on-disk format.

## Proposed approach

### 1. Make `roles.reviewer` the only canonical reviewer root

All reviewer-specific configuration should live under `roles.reviewer`.

That means the top-level `reviewer` section becomes deprecated and eventually removed.

This creates a single rule users can remember:

> If you are configuring the reviewer, go to `roles.reviewer`.

### 2. Separate reviewer config by user-facing concern, not implementation layer

Within `roles.reviewer`, split fields into two explicit groups:

- `discovery`: how Looper decides a PR is eligible for reviewer work
- `behavior`: how the reviewer loop behaves once it runs

Keep `instructions` at the reviewer root because it is conceptually cross-cutting and already common across roles.

Recommended target shape:

```json
{
  "roles": {
    "reviewer": {
      "discovery": {
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
      "behavior": {
        "scope": "changed_files",
        "publishMode": "review",
        "loop": {
          "quietPeriodSeconds": 120
        },
        "reviewEvents": {
          "clean": "APPROVE",
          "blocking": "REQUEST_CHANGES"
        },
        "detectDuplicateFindings": true,
        "nativeResume": {
          "onHeadChange": false,
          "reReviewPromptOnHeadChange": false
        },
        "threadResolution": {
          "enabled": true,
          "mode": "resolve_objective"
        }
      },
      "instructions": "..."
    }
  }
}
```

### 3. Make project overrides mirror the global reviewer shape

Project reviewer overrides should remain under `projects[].roles.reviewer`, but use the same nested structure:

```json
{
  "projects": [
    {
      "id": "open-design",
      "roles": {
        "reviewer": {
          "discovery": {
            "triggers": {
              "requireReviewRequest": true
            }
          },
          "behavior": {
            "reviewEvents": {
              "blocking": "COMMENT"
            }
          }
        }
      }
    }
  ]
}
```

Users should be able to rely on this simple model:

> Project reviewer config is a partial override of global reviewer config with the same shape.

Project reviewer overrides are part of the config-file layer. They do not create a new precedence layer outside the existing config loading model. Environment variables and CLI flags continue to apply after file loading and therefore remain capable of overriding file-backed defaults, including values that originated from a project entry when the runtime is computing effective config.

### 4. Preserve existing merge rules

The change should keep the existing layering and merge behavior:

- built-in defaults
- global config/env/CLI
- project override for the matching project

Within that layering:

- objects merge deeply
- arrays replace wholly
- omitted fields inherit from earlier layers

This preserves implementation stability while improving the visible schema.

### 5. Add multi-format config support, with TOML as the default

Looper should accept config files in:

- JSON
- YAML
- TOML

The future default should be TOML.

That means:

- the default generated config path should move from `~/.looper/config.json` to `~/.looper/config.toml`
- primary documentation and examples should use TOML
- bootstrap / init / migration flows should prefer TOML when creating or rewriting config files
- JSON remains supported for backward compatibility during and after the reviewer-schema migration unless explicitly removed in a later change

Recommended file-resolution behavior:

1. if `--config` is provided, load exactly that path and infer format from suffix
2. otherwise if `LOOPER_CONFIG` is provided, load exactly that path and infer format from suffix
3. otherwise inspect the supported default config paths
4. if exactly one supported default config file exists, load it
5. if multiple supported default config files exist, fail fast instead of guessing
6. if none exist, prefer `config.toml` as the canonical path for newly generated config

Recommended default-path lookup order:

1. `~/.looper/config.toml`
2. `~/.looper/config.yaml`
3. `~/.looper/config.yml`
4. `~/.looper/config.json`

If multiple default config files exist at once, Looper should fail fast with a clear error instead of guessing, unless the user explicitly selected one via `--config` or `LOOPER_CONFIG`.

### 6. Treat schema migration and format migration as independent layers

Two migrations are happening at once:

1. reviewer schema migration
   - from top-level `reviewer.*` plus flat `roles.reviewer.*`
   - to canonical nested `roles.reviewer.discovery|behavior.*`

2. file-format migration
   - from JSON-default
   - to TOML-default, with YAML and JSON still accepted

These should be implemented independently so users can migrate one without being forced to migrate the other immediately.

Examples of valid transition states:

- legacy reviewer schema in `config.json`
- canonical reviewer schema in `config.json`
- legacy reviewer schema in `config.toml`
- canonical reviewer schema in `config.toml`
- canonical reviewer schema in `config.yaml`

## Schema rules

The following rules should guide this change and future config work:

1. **One concept, one canonical path**
   - Reviewer settings must not live under multiple root namespaces.

2. **Top-level config is only for cross-cutting system concerns**
   - Examples: `server`, `storage`, `scheduler`, `agent`, `logging`, `notifications`, `tools`, `daemon`, `defaults`, `projects`
   - Role-specific behavior should not live at top level.

3. **Global and project-level structures should mirror each other**
   - If a field is configurable globally, it should use the same path shape when overridden per project whenever practical.

4. **Group by user-facing task**
   - Prefer names like `discovery` and `behavior` over implementation-driven split across unrelated roots.

5. **Docs and examples must show only the canonical path**
   - Deprecated paths may be mentioned only in migration notes.

6. **One canonical format for new users**
   - New docs, generated config, and examples should prefer TOML.

7. **Format support must not change semantic config behavior**
   - JSON, YAML, and TOML should normalize into the same effective config model.

## Data model changes

### Recommended config shape

Replace the current reviewer split:

- current global behavior root: `reviewer.*`
- current global role root: `roles.reviewer.*`

with:

- canonical global root: `roles.reviewer.discovery.*`
- canonical global root: `roles.reviewer.behavior.*`
- canonical per-project root: `projects[].roles.reviewer.discovery.*`
- canonical per-project root: `projects[].roles.reviewer.behavior.*`

### Compatibility mapping

During migration, map old fields as follows:

- `reviewer.loop.*` → `roles.reviewer.behavior.loop.*`
- `reviewer.scope` → `roles.reviewer.behavior.scope`
- `reviewer.publishMode` → `roles.reviewer.behavior.publishMode`
- `reviewer.reviewEvents.*` → `roles.reviewer.behavior.reviewEvents.*`
- `reviewer.detectDuplicateFindings` / legacy aliases → `roles.reviewer.behavior.detectDuplicateFindings`
- `reviewer.nativeResume.*` → `roles.reviewer.behavior.nativeResume.*`
- `reviewer.threadResolution.*` → `roles.reviewer.behavior.threadResolution.*`
- `roles.reviewer.autoDiscovery` → `roles.reviewer.discovery.autoDiscovery`
- `roles.reviewer.triggers.*` → `roles.reviewer.discovery.triggers.*`
- `roles.reviewer.specReview.*` → `roles.reviewer.discovery.specReview.*`
- `roles.reviewer.instructions` stays at `roles.reviewer.instructions`

### Config format compatibility

During migration:

- continue accepting `.json`
- add acceptance for `.yaml`, `.yml`, and `.toml`
- make `.toml` the default path and primary documented format
- ensure equivalent JSON/YAML/TOML config normalizes to the same effective runtime config

Across all supported formats:

- unknown fields should remain errors
- omitted fields and explicit empty values must keep the same semantic meaning after normalization
- explicit null handling must be specified and tested so YAML/TOML/JSON do not diverge silently

## Migration strategy

This change combines two migrations that should be coordinated but not coupled:

1. reviewer schema migration
2. config-file format migration

Users should be able to complete either migration independently.

### 1. Core migration rules

- Support both legacy reviewer schema and canonical nested reviewer schema during the migration window.
- Support JSON, YAML, and TOML during the migration window.
- Legacy-to-canonical normalization must happen independently within each input layer before cross-layer merging.
- Overall precedence remains unchanged: `defaults → config file → env → CLI`.
- Project reviewer overrides remain part of the config-file layer; they do not create a new layer above env/CLI.

### 2. Canonical target state

Reviewer config converges on:

- `roles.reviewer.discovery.*`
- `roles.reviewer.behavior.*`
- `roles.reviewer.instructions`

Project overrides mirror the same structure:

- `projects[].roles.reviewer.discovery.*`
- `projects[].roles.reviewer.behavior.*`
- `projects[].roles.reviewer.instructions`

Config-file format converges on:

- supported suffixes: `.toml`, `.yaml`, `.yml`, `.json`
- canonical default path for new config: `~/.looper/config.toml`
- TOML-first docs, templates, and generated config

### 3. Schema precedence within a layer

When overlapping reviewer values target the same effective canonical field within the same layer, use this precedence:

1. canonical project reviewer keys: `projects[].roles.reviewer.discovery|behavior.*`
2. legacy flat project reviewer keys: `projects[].roles.reviewer.*`
3. canonical global reviewer keys: `roles.reviewer.discovery|behavior.*`
4. legacy flat global reviewer keys: `roles.reviewer.*`
5. legacy top-level reviewer keys: `reviewer.*`
6. built-in defaults

This lets canonical keys beat legacy keys without changing cross-layer precedence.

### 4. Config-file selection rules

1. if `--config` is provided, load exactly that path
2. else if `LOOPER_CONFIG` is provided, load exactly that path
3. else inspect supported default paths:
   - `~/.looper/config.toml`
   - `~/.looper/config.yaml`
   - `~/.looper/config.yml`
   - `~/.looper/config.json`

Behavior:

- if exactly one supported default config file exists, load it
- if more than one exists, fail fast with an actionable error
- if none exist, continue with defaults and treat `~/.looper/config.toml` as the canonical path for newly generated config

### 5. Config-writing rules

- Commands that write or update config must preserve the currently selected config path and format unless the user explicitly requests migration or conversion.
- Config-writing flows must not create a second default config file implicitly.
- `looper config migrate` should exist by the time TOML-first docs/bootstrap flows ship.
- If it converts `config.json` to `config.toml`, it must avoid leaving multiple default config files behind unless the user explicitly requests a non-destructive copy flow.

### 6. Deprecation and removal

During the migration window:

- accept legacy `reviewer.*`
- accept legacy flat `roles.reviewer.*`
- accept legacy flat `projects[].roles.reviewer.*`
- emit warnings with exact replacement paths

Example warnings:

- `reviewer.reviewEvents.clean is deprecated; use roles.reviewer.behavior.reviewEvents.clean`
- `roles.reviewer.triggers is deprecated; use roles.reviewer.discovery.triggers`

After at least one stable release cycle with warnings:

- reject top-level `reviewer.*`
- reject all legacy flat reviewer keys at `roles.reviewer.*`
- reject all legacy flat project reviewer keys at `projects[].roles.reviewer.*`

### 7. Documentation migration

The following user-facing config references must stay aligned:

- `docs/configuration.md`
- `skills/looper/references/config.md`
- CLI help and command examples that mention config paths or formats
- generated config templates and bootstrap/init output
- checked-in setup docs or sample configs that show config snippets

Primary examples should use:

- TOML
- canonical nested reviewer paths

JSON should be documented as supported legacy-compatible format. YAML should be documented as a supported alternative format.

### 8. Rollout

#### Release 1

- add YAML/TOML read support
- add canonical nested reviewer schema support
- keep JSON and legacy reviewer schema compatible
- begin emitting deprecation warnings

#### Release 2

- switch docs/examples/bootstrap flows to TOML-first
- switch reviewer examples to canonical nested paths
- ship `looper config migrate`

#### Release 3+

- consider upgrading legacy reviewer schema from warning to error
- keep JSON read support unless there is a strong reason to remove it later

### 9. Success criteria

Migration is successful when:

- users can adopt the new reviewer schema without changing file format
- users can adopt TOML without changing reviewer schema immediately
- equivalent JSON/YAML/TOML configs produce the same effective runtime config
- product docs, `skills/looper` docs, CLI help, and generated templates stay aligned on default path, supported suffixes, example shape, and migration guidance

## Implementation outline

### 1. Config types

Refactor `internal/config/types.go` so reviewer role config can express:

- `ReviewerDiscoveryConfig`
- `ReviewerBehaviorConfig`
- `ReviewerUnifiedConfig` or equivalent nested under `RoleConfigs.Reviewer`

Keep partial config pointer semantics so omitted values remain distinguishable from explicit false/empty values.

The normalized in-memory config model should remain format-agnostic.

### 2. Loader and normalization

Update `internal/config/load.go` and `internal/config/normalize.go` to:

- dispatch config parsing by file suffix
- support `.json`, `.yaml`, `.yml`, and `.toml`
- read legacy and canonical reviewer paths
- normalize them into one effective in-memory reviewer config shape within each layer before cross-layer merge
- apply precedence in favor of canonical nested paths
- keep existing env/CLI layering behavior intact

The design should explicitly define reviewer-related env-var and CLI-flag compatibility:

- whether legacy reviewer env vars remain canonical for now
- whether canonical aliases are added
- whether legacy env/CLI names emit warnings
- and how those aliases map onto the new canonical internal reviewer shape

Unknown-field behavior should remain strict across all supported formats.

### 3. Project overrides

Extend project role override parsing so reviewer project overrides can target both:

- `discovery`
- `behavior`

Normalization should apply project reviewer overrides after global reviewer normalization, preserving inherited defaults.

Project reviewer overrides must also define legacy compatibility behavior for `projects[].roles.reviewer.*` during the migration window.

### 4. Validation

Update `internal/config/validate.go` to:

- validate the canonical nested reviewer schema
- keep validating accepted legacy fields during the transition
- emit deprecation warnings for legacy usage
- reject mixed invalid shapes that would be ambiguous after migration
- reject unsupported config-file suffixes with a clear error
- fail clearly when multiple default config files are present without explicit selection
- define the behavior of explicit nulls and empty values across JSON/YAML/TOML

### 5. Documentation and examples

Update docs and sample configs so the only reviewer examples shown in primary documentation are under:

- `roles.reviewer.discovery`
- `roles.reviewer.behavior`

If legacy config must be mentioned, keep it in a clearly marked migration section only.

Also update config documentation so:

- TOML is the primary example format
- YAML is documented as supported
- JSON is documented as supported legacy-compatible format
- `skills/looper/references/config.md` matches the same default path, supported suffixes, example shape, and migration guidance

### 6. Tests

Add or update config tests to cover:

- legacy-only config
- canonical-only config
- mixed config where canonical wins
- project override inheritance for `discovery` and `behavior`
- legacy project override conflicts against canonical global and canonical project keys
- deep-merge behavior for nested reviewer objects
- deprecation warnings
- JSON/YAML/TOML semantic parity for equivalent reviewer config
- default-path resolution order and ambiguity errors
- TOML default-path behavior
- explicit null / empty / omitted value parity across formats
- env/CLI reviewer override compatibility and precedence
- config-writing flows preserving the selected path/format without creating a second default config file

## Alternatives considered

### A. Keep the split and only rename top-level `reviewer`

Example: `reviewerRuntime` + `roles.reviewer`

Rejected because it still forces users to learn two reviewer roots and does not fix project-level asymmetry.

### B. Merge everything into flat `roles.reviewer` without subgroups

This is much better than today and remains a viable fallback if implementation simplicity dominates.

Example:

```json
{
  "roles": {
    "reviewer": {
      "autoDiscovery": true,
      "triggers": {},
      "specReview": {},
      "loop": {},
      "reviewEvents": {},
      "threadResolution": {},
      "instructions": "..."
    }
  }
}
```

Not recommended as the primary target because `discovery` versus `behavior` is a useful distinction for humans; flattening makes the section larger and less scannable over time.

## Recommended decision

Adopt a single canonical reviewer root at `roles.reviewer`, structured as:

- `roles.reviewer.discovery`
- `roles.reviewer.behavior`
- `roles.reviewer.instructions`

This keeps the existing conceptual distinction but expresses it in a way users can understand and predict.

## Acceptance criteria

- Users can configure reviewer behavior and discovery without touching more than one global reviewer root.
- Project reviewer overrides use the same conceptual structure as the global reviewer config.
- Canonical nested reviewer config fully represents the current effective reviewer feature set.
- Legacy reviewer paths remain supported during migration, with clear warnings.
- Primary documentation and example configs use only the canonical nested shape.
- TOML is the default documented and generated config format.
- YAML and JSON configs normalize to the same effective config behavior as TOML.
- `skills/looper/references/config.md`, CLI help, generated templates, and product docs stay aligned on default config-path behavior and migration guidance.
