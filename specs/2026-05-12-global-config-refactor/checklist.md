# Global config architecture refactor checklist

## Phase 0 - Lock the product model

- [ ] Confirm this spec is a global config refactor, not a single-domain cleanup
- [ ] Confirm the goal is one canonical taxonomy for the full supported config surface
- [ ] Confirm top-level config should represent stable cross-cutting product concerns rather than implementation seams
- [ ] Confirm role-specific config should live under `roles.<role>`
- [ ] Confirm project-level overrides should mirror the global canonical shape wherever per-project overrides are supported
- [ ] Confirm this change does **not** intentionally alter runtime behavior or effective defaults except where required by canonicalization, validation, or explicit precedence decisions
- [ ] Confirm config precedence semantics remain defaults → config file → env → CLI
- [ ] Confirm YAML and TOML become supported config-file formats
- [ ] Confirm TOML becomes the default generated and documented config format
- [ ] Confirm JSON remains supported for backward compatibility
- [ ] Confirm schema migration and file-format migration are independent

## Phase 1 - Freeze the canonical taxonomy

- [ ] Define the canonical top-level roots
- [ ] Confirm which current top-level roots remain canonical as-is
- [ ] Decide which current top-level roots are legacy/deprecated and where they move
- [ ] Define the canonical role model under `roles.<role>`
- [ ] Decide which roles use subgroups like `discovery`, `behavior`, and `instructions`
- [ ] Define the canonical project override model
- [ ] Decide how project-local metadata is separated from project-local override config
- [ ] Freeze supported config-file suffixes: `.toml`, `.yaml`, `.yml`, `.json`
- [ ] Freeze default config path as `~/.looper/config.toml`
- [ ] Freeze behavior when zero / one / multiple default config files exist
- [ ] Document the canonical taxonomy in `docs/configuration.md`
- [ ] Ensure all primary examples use only canonical paths
- [ ] Ensure all primary examples use TOML unless format comparison is the point

## Phase 2 - Inventory current config and map it forward

- [ ] Inventory every current top-level config root
- [ ] Inventory every current role-specific config root
- [ ] Inventory every current project-level override path
- [ ] Inventory current env var names for config overrides
- [ ] Inventory current CLI flag names for config overrides
- [ ] Map every supported legacy path to a canonical path
- [ ] Map every supported legacy env var name to a canonical env var name or compatibility alias
- [ ] Map every supported legacy CLI flag name to a canonical CLI flag name or compatibility alias
- [ ] Identify current config that should remain unchanged vs moved vs deprecated
- [ ] Identify config that is currently overrideable per project vs not overrideable per project

## Phase 3 - Config types and effective in-memory model

- [ ] Reshape config types to represent the canonical taxonomy
- [ ] Preserve partial-config pointer semantics where they matter today
- [ ] Ensure the effective in-memory config can represent the full supported config surface
- [ ] Ensure the in-memory model stays format-agnostic across JSON/YAML/TOML inputs
- [ ] Decide whether any current internal config type should be split or merged to match the canonical product model

## Phase 4 - Loader, formats, and source selection

- [ ] Dispatch config loading by file suffix
- [ ] Add `.toml` support
- [ ] Add `.yaml` / `.yml` support
- [ ] Keep `.json` support
- [ ] Freeze explicit source-selection precedence: `--config` > `LOOPER_CONFIG` > default-path discovery
- [ ] Add default-path probing for `config.toml`, `config.yaml`, `config.yml`, `config.json`
- [ ] Fail clearly when multiple supported default config files exist without explicit selection
- [ ] Define behavior when no supported default config file exists
- [ ] Ensure config-writing/bootstrap/init flows preserve an explicitly selected path and format

## Phase 5 - Schema normalization and canonicalization

- [ ] Normalize accepted legacy schema into the canonical taxonomy within each layer before cross-layer merging
- [ ] Normalize role-specific legacy config into `roles.<role>`
- [ ] Normalize legacy project override shapes into canonical project override shapes
- [ ] Normalize legacy env var names into canonical env var targets
- [ ] Normalize legacy CLI flag names into canonical CLI flag targets
- [ ] Ensure normalization is deterministic for valid mixed-schema inputs

## Phase 6 - Merge and precedence rules

- [ ] Preserve deep-merge behavior for objects unless a domain documents a different rule
- [ ] Preserve array replacement behavior unless a domain documents a different rule
- [ ] Ensure omitted fields inherit from earlier layers
- [ ] Ensure project overrides remain inside the config-file layer, not a new layer above env/CLI
- [ ] Freeze precedence between canonical and legacy config paths that target the same effective field
- [ ] Freeze precedence between canonical and legacy project override paths that target the same effective field
- [ ] Freeze precedence between canonical and legacy env var names when both are present
- [ ] Freeze precedence between canonical and legacy CLI flag names when both are present
- [ ] Define which mixed-schema combinations are valid
- [ ] Define which mixed-schema combinations are ambiguous/invalid and should be rejected

## Phase 7 - Project override model

- [ ] Confirm which global config domains are legitimately overrideable per project
- [ ] Extend project override parsing to every supported canonical project override domain
- [ ] Ensure project override paths mirror the same local shape as global canonical paths
- [ ] Ensure omitted project fields inherit from global config
- [ ] Ensure project arrays replace rather than merge element-wise where that is the current rule
- [ ] Preserve any existing project-specific clear/override semantics that already exist today
- [ ] Document project override examples using the canonical shape

## Phase 8 - Env and CLI naming model

- [ ] Define canonical env var names for overrideable config
- [ ] Define canonical CLI flag names for overrideable config
- [ ] Decide which legacy env var names remain accepted as compatibility aliases
- [ ] Decide which legacy CLI flag names remain accepted as compatibility aliases
- [ ] Ensure canonical and legacy env/CLI names resolve to the same canonical config targets
- [ ] Ensure env/CLI overrides still beat file-backed canonical and legacy values

## Phase 9 - Validation and deprecation policy

- [ ] Validate canonical config directly
- [ ] Keep validating accepted legacy config during the migration window
- [ ] Reject unsupported config-file suffixes with clear errors
- [ ] Emit warnings for deprecated top-level roots
- [ ] Emit warnings for deprecated role-specific roots
- [ ] Emit warnings for deprecated project override roots
- [ ] Emit warnings for deprecated env var names
- [ ] Emit warnings for deprecated CLI flag names
- [ ] Make warning messages point to exact replacement paths or names
- [ ] Ensure deprecation warnings are emitted once per logical field/name, not noisily per nested leaf
- [ ] Define the release window before legacy config paths become hard errors
- [ ] Define the future validation error text for removed legacy config paths

## Phase 10 - Migration UX

Implementation note: follow `reference/migration-ux-decision.md` for the resolved migration-UX scope, safety guarantees, informational-note behavior, and hard-error timing.

- [ ] Decide whether to add `looper config migrate`
- [ ] If added, scope it to rewrite only known legacy config paths/names
- [ ] If added, decide whether it can convert `config.json` to `config.toml`
- [ ] If added, decide whether conversion renames/removes the old file or requires explicit confirmation
- [ ] If added, ensure the helper preserves unrelated formatting/content where practical
- [ ] If added, ensure the helper never deletes unknown user config
- [ ] If added, ensure the helper does not leave multiple default config files behind by default
- [ ] If no helper is added, provide startup suggestions with explicit replacement paths/names
- [ ] Decide whether loading legacy `~/.looper/config.json` should emit an informational migration note

## Phase 11 - Documentation and generated UX

- [ ] Update `docs/configuration.md` to lead with the canonical taxonomy
- [ ] Update `docs/configuration.md` default config path to `~/.looper/config.toml`
- [ ] Update `docs/configuration.md` to document JSON/YAML/TOML support
- [ ] Update `skills/looper/references/config.md` to match product docs on taxonomy, path, and formats
- [ ] Add a migration guide from legacy config roots to canonical config roots
- [ ] Add a migration guide from `config.json` to `config.toml`
- [ ] Add before/after config examples
- [ ] Update CLI help/examples that mention config paths, roots, or formats
- [ ] Update sample config snippets to remove legacy paths from primary examples
- [ ] Update generated config templates and bootstrap/init output to use canonical TOML-first examples
- [ ] Ensure product docs, skill docs, help text, and generated templates stay aligned on taxonomy, path, suffixes, examples, and migration guidance

## Phase 12 - Tests

- [ ] Add/update tests for legacy-only config
- [ ] Add/update tests for canonical-only config
- [ ] Add/update tests for mixed config where canonical wins
- [ ] Add/update tests for equivalent JSON/YAML/TOML config parity
- [ ] Add/update tests for explicit null / empty / omitted value parity across formats
- [ ] Add/update tests for TOML default-path loading
- [ ] Add/update tests for YAML default-path loading
- [ ] Add/update tests for explicit `--config` / `LOOPER_CONFIG` precedence over discovered defaults
- [ ] Add/update tests for multiple-default-config ambiguity errors
- [ ] Add/update tests for no-default-config behavior
- [ ] Add/update tests for project override inheritance
- [ ] Add/update tests for project override precedence
- [ ] Add/update tests for legacy project override conflicts with canonical global/project config
- [ ] Add/update tests for deep merge of canonical nested objects
- [ ] Add/update tests for array replacement in canonical nested config
- [ ] Add/update tests for deprecated env var names and CLI flag aliases
- [ ] Add/update tests for deprecation warnings and validation errors
- [ ] Add/update tests for config-writing/bootstrap/init flows preserving the selected config path/format and not creating a second default config file
- [ ] Add/update tests that default behavior remains unchanged when users do not migrate

## Phase 13 - Verification

- [ ] Verify a user can find each supported config domain under one canonical taxonomy
- [ ] Verify old and new config shapes produce identical effective config where mappings are equivalent
- [ ] Verify equivalent JSON/YAML/TOML config produces identical effective runtime config
- [ ] Verify project overrides are predictable and mirror the global shape wherever supported
- [ ] Verify no supported config behavior is lost in the canonical taxonomy
- [ ] Verify TOML is the default documented and generated config format
- [ ] Verify `skills/looper/references/config.md` matches product docs on taxonomy, path, and formats
- [ ] Verify CLI help and generated templates match product docs on taxonomy, path, format, and migration guidance
- [ ] Verify config validation errors remain clear
- [ ] Run relevant config tests
- [ ] Run `gofmt -l .`
- [ ] Run `go vet ./...`
- [ ] Run full `go test ./...`
- [ ] Run `go build ./...`

## Out of scope for this checklist

- Removing JSON support entirely
- Changing runtime semantics that are unrelated to config canonicalization
- Introducing project-level overrides for domains that are not meaningfully project-scoped
- Replacing the existing layer model with a new precedence model
