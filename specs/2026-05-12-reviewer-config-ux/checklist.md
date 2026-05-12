# Reviewer config UX consolidation checklist

## Phase 0 - Lock the user model

- [ ] Confirm reviewer config should have one canonical root
- [ ] Confirm the canonical root is `roles.reviewer`
- [ ] Confirm reviewer config should be grouped by user-facing concern rather than implementation boundary
- [ ] Confirm the target grouping is:
  - [ ] `roles.reviewer.discovery`
  - [ ] `roles.reviewer.behavior`
  - [ ] `roles.reviewer.instructions`
- [ ] Confirm top-level `reviewer.*` becomes legacy/deprecated rather than canonical
- [ ] Confirm project reviewer overrides should mirror the global reviewer shape
- [ ] Confirm this change does **not** alter effective reviewer behavior by default
- [ ] Confirm this change does **not** alter config precedence semantics
- [ ] Confirm YAML and TOML become supported config-file formats
- [ ] Confirm TOML becomes the default generated and documented config format
- [ ] Confirm JSON remains supported for backward compatibility
- [ ] Confirm schema normalization happens within each layer before cross-layer merging
- [ ] Confirm project reviewer overrides remain part of the config-file layer, not a new layer above env/CLI

## Phase 1 - Freeze canonical schema

- [ ] Define canonical global reviewer paths
- [ ] Define canonical project-level reviewer override paths
- [ ] Freeze naming for `discovery` vs `behavior`
- [ ] Decide whether `instructions` stays at `roles.reviewer.instructions`
- [ ] Decide whether all current top-level reviewer behavior fields move under `behavior`
- [ ] Decide whether any current `roles.reviewer.*` field should stay at reviewer root besides `instructions`
- [ ] Freeze supported config-file suffixes: `.toml`, `.yaml`, `.yml`, `.json`
- [ ] Freeze default config path as `~/.looper/config.toml`
- [ ] Freeze behavior when zero / one / multiple default config files exist
- [ ] Document the canonical reviewer shape in `docs/configuration.md`
- [ ] Ensure all primary examples use only canonical reviewer paths
- [ ] Ensure all primary examples use TOML unless format comparison is the point

## Phase 2 - Config types and in-memory model

- [ ] Add/reshape config types for reviewer `discovery`
- [ ] Add/reshape config types for reviewer `behavior`
- [ ] Keep partial-config pointer semantics for reviewer fields
- [ ] Ensure the effective in-memory config can represent the full current reviewer feature set
- [ ] Ensure the effective in-memory config still supports project-level reviewer overrides
- [ ] Ensure the effective in-memory config is format-agnostic across JSON/YAML/TOML inputs

## Phase 3 - Legacy compatibility mapping

- [ ] Map `reviewer.loop.*` → `roles.reviewer.behavior.loop.*`
- [ ] Map `reviewer.scope` → `roles.reviewer.behavior.scope`
- [ ] Map `reviewer.publishMode` → `roles.reviewer.behavior.publishMode`
- [ ] Map `reviewer.reviewEvents.*` → `roles.reviewer.behavior.reviewEvents.*`
- [ ] Map `reviewer.detectDuplicateFindings` and legacy aliases → `roles.reviewer.behavior.detectDuplicateFindings`
- [ ] Map `reviewer.nativeResume.*` → `roles.reviewer.behavior.nativeResume.*`
- [ ] Map `reviewer.threadResolution.*` → `roles.reviewer.behavior.threadResolution.*`
- [ ] Map legacy `roles.reviewer.autoDiscovery` → `roles.reviewer.discovery.autoDiscovery`
- [ ] Map legacy `roles.reviewer.triggers.*` → `roles.reviewer.discovery.triggers.*`
- [ ] Map legacy `roles.reviewer.specReview.*` → `roles.reviewer.discovery.specReview.*`
- [ ] Map legacy `projects[].roles.reviewer.*` → canonical project reviewer paths
- [ ] Preserve `roles.reviewer.instructions` as-is during migration
- [ ] Define JSON → TOML migration behavior for default-path users
- [ ] Define YAML/TOML support as format-only, not schema-altering

## Phase 4 - Loader and normalization

- [ ] Teach config loading to accept canonical nested reviewer config
- [ ] Dispatch config loading by file suffix
- [ ] Add `.toml` support
- [ ] Add `.yaml` / `.yml` support
- [ ] Keep `.json` support
- [ ] Keep loading legacy top-level `reviewer.*`
- [ ] Keep loading legacy flat `roles.reviewer.*` reviewer role keys during migration
- [ ] Keep loading legacy flat `projects[].roles.reviewer.*` reviewer keys during migration
- [ ] Normalize all accepted reviewer inputs into one effective canonical shape
- [ ] Preserve env override behavior for reviewer fields
- [ ] Preserve CLI override behavior for reviewer fields
- [ ] Define canonical reviewer env var names and/or compatibility aliases
- [ ] Define canonical reviewer CLI flag names and/or compatibility aliases
- [ ] Add default-path probing for `config.toml`, `config.yaml`, `config.yml`, `config.json`
- [ ] Fail clearly when multiple supported default config files exist without explicit selection
- [ ] Define behavior when no supported default config file exists
- [ ] Ensure deep-merge behavior still applies to reviewer objects
- [ ] Ensure array replacement behavior still applies to reviewer arrays

## Phase 5 - Precedence and ambiguity rules

- [ ] Freeze precedence between canonical nested reviewer fields and legacy fields
- [ ] Freeze precedence for legacy project reviewer overrides relative to canonical global/project reviewer settings
- [ ] Ensure project canonical reviewer overrides beat global canonical reviewer settings
- [ ] Ensure global canonical reviewer settings beat legacy flat `roles.reviewer.*`
- [ ] Ensure legacy flat `roles.reviewer.*` beats top-level legacy `reviewer.*`
- [ ] Ensure env/CLI overrides can still beat file-backed canonical and legacy reviewer values
- [ ] Decide and document behavior when users mix canonical and legacy reviewer paths in one file
- [ ] Add examples of valid mixed-schema reviewer configs
- [ ] Add examples of invalid mixed-schema reviewer configs
- [ ] Reject only genuinely ambiguous/invalid mixed reviewer shapes
- [ ] Freeze precedence between explicit `--config`, `LOOPER_CONFIG`, and default-path discovery

## Phase 6 - Project-level overrides

- [ ] Extend project role override parsing for reviewer `discovery`
- [ ] Extend project role override parsing for reviewer `behavior`
- [ ] Ensure omitted project reviewer fields inherit from global reviewer config
- [ ] Ensure project reviewer arrays replace rather than merge element-wise
- [ ] Ensure project reviewer instructions can still override or clear inherited instructions if supported today
- [ ] Ensure project reviewer override semantics are documented without implying a new layer above env/CLI
- [ ] Document project reviewer override examples using the canonical nested shape

## Phase 7 - Validation and deprecation warnings

- [ ] Validate canonical reviewer nested config
- [ ] Keep validating accepted legacy reviewer paths during migration
- [ ] Emit warnings for top-level `reviewer.*`
- [ ] Emit warnings for legacy flat reviewer role keys at `roles.reviewer.*`
- [ ] Emit warnings for legacy flat project reviewer keys at `projects[].roles.reviewer.*`
- [ ] Reject unsupported config-file suffixes with clear errors
- [ ] Decide whether loading legacy `~/.looper/config.json` should emit an informational migration note
- [ ] Make warning messages point to exact replacement paths
- [ ] Ensure deprecation warnings are actionable and non-spammy
- [ ] Ensure deprecation warnings are emitted once per logical field/path rather than noisily per nested leaf
- [ ] Define the release window before legacy reviewer paths become errors
- [ ] Define the future validation error text for removed reviewer paths

## Phase 8 - Documentation and migration UX

- [ ] Update `docs/configuration.md` reviewer section to the canonical nested structure
- [ ] Update `docs/configuration.md` default config path to `~/.looper/config.toml`
- [ ] Update `docs/configuration.md` to document JSON/YAML/TOML support
- [ ] Update CLI help/examples that mention config paths or config formats
- [ ] Update generated config templates and bootstrap/init output to use TOML-first paths and examples
- [ ] Update sample config snippets to remove top-level `reviewer` from primary examples
- [ ] Update `skills/looper/references/config.md` to use `~/.looper/config.toml`
- [ ] Update `skills/looper/references/config.md` to document JSON/YAML/TOML support
- [ ] Update `skills/looper/references/config.md` to use the canonical reviewer nested structure
- [ ] Add a migration guide from old reviewer paths to new reviewer paths
- [ ] Add a migration guide from `config.json` to `config.toml`
- [ ] Add before/after config examples
- [ ] Clarify the mental model: discovery = how reviewer work is selected; behavior = how reviewer runs
- [ ] Ensure project override docs mirror the same mental model
- [ ] Ensure product docs, skill docs, CLI help, and generated templates stay aligned on path, suffixes, examples, and migration guidance

## Phase 9 - Optional migration helper

- [ ] Decide whether to add `looper config migrate`
- [ ] If added, scope it to rewrite only known reviewer legacy paths
- [ ] If added, decide whether it can also convert `config.json` to `config.toml`
- [ ] If added, decide whether conversion renames/removes the old file or requires explicit confirmation
- [ ] Ensure the migration helper preserves unrelated formatting/content where practical
- [ ] Ensure the migration helper never deletes unknown user config
- [ ] Ensure the migration helper does not leave multiple default config files behind by default
- [ ] If no helper is added, provide startup suggestions with explicit replacement paths

## Phase 10 - Tests

- [ ] Add/update tests for legacy-only reviewer config
- [ ] Add/update tests for canonical-only reviewer config
- [ ] Add/update tests for mixed reviewer config where canonical wins
- [ ] Add/update tests for equivalent JSON/YAML/TOML reviewer config parity
- [ ] Add/update tests for explicit null / empty / omitted reviewer value parity across formats
- [ ] Add/update tests for TOML default-path loading
- [ ] Add/update tests for YAML default-path loading
- [ ] Add/update tests for explicit `--config` / `LOOPER_CONFIG` precedence over discovered defaults
- [ ] Add/update tests for multiple-default-config ambiguity errors
- [ ] Add/update tests for no-default-config behavior
- [ ] Add/update tests for project reviewer override inheritance
- [ ] Add/update tests for project reviewer override precedence
- [ ] Add/update tests for legacy project reviewer overrides conflicting with canonical global/project reviewer config
- [ ] Add/update tests for deep merge of reviewer nested objects
- [ ] Add/update tests for array replacement in reviewer nested config
- [ ] Add/update tests for deprecation warnings
- [ ] Add/update tests for legacy env/CLI reviewer overrides beating canonical config-file values
- [ ] Add/update tests for config-writing/bootstrap/init flows preserving the selected config path/format and not creating a second default config file
- [ ] Add/update tests that default behavior remains unchanged when users do not migrate

## Phase 11 - Verification

- [ ] Verify a user can find all reviewer config under one canonical root in docs/examples
- [ ] Verify the effective config produced by old and new reviewer shapes is identical where mappings are equivalent
- [ ] Verify equivalent JSON/YAML/TOML config produces identical effective runtime config
- [ ] Verify project reviewer overrides are predictable and mirror the global shape
- [ ] Verify no existing supported reviewer behavior is lost in the canonical nested schema
- [ ] Verify TOML is the default documented and generated config format
- [ ] Verify `skills/looper/references/config.md` matches product docs on default path and supported formats
- [ ] Verify CLI help and generated templates match product docs on path, format, and migration guidance
- [ ] Verify config validation errors remain clear
- [ ] Run relevant config tests
- [ ] Run full `go test ./...`

## Out of scope for this checklist

- Non-reviewer role config redesign
- Broad top-level config taxonomy changes outside reviewer-related paths
- Changing reviewer scheduling or runtime semantics beyond schema normalization and migration
