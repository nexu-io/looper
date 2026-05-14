# Migration UX decision record

Date: 2026-05-12

## Scope

This record closes the migration-UX decisions that were left open in `spec.md` and `checklist.md` for the global config refactor.

## Decisions

### 1. `looper config migrate` is not part of this refactor

The migration path for this refactor is startup guidance plus documentation, not a config-rewriting helper.

- Do not add `looper config migrate` in this change set.
- Do not mutate user config files during daemon startup or CLI startup.
- The implementation work for this refactor should focus on canonical loading, compatibility aliases, warnings, and clear guidance.

Rationale:

- The refactor already changes schema, formats, docs, flags, env names, and project overrides; adding a writer increases scope and safety risk.
- Startup guidance is enough to unblock migration while keeping the compatibility layer in place.

### 2. Loading legacy `~/.looper/config.json` should emit an informational migration note

When the effective config file loaded for the process is exactly the legacy default path `~/.looper/config.json`, emit one informational migration note for that file load.

- The note is informational, not a validation warning and not a startup failure.
- Emit it once per process.
- The note should explain that TOML is now the preferred default format/path and point users to `~/.looper/config.toml` plus the canonical taxonomy docs.
- This note is allowed whether the file was discovered by default-path probing or selected explicitly via `LOOPER_CONFIG` / `--config`; the trigger is the loaded path, not the source-selection mechanism.

### 3. `config.json` → `config.toml` conversion is out of scope for this refactor

- Do not automatically rewrite or convert legacy JSON config to TOML.
- Do not add partial conversion logic behind startup guidance.
- Documentation may provide manual before/after migration examples, but the product should not write the converted file in this change set.

### 4. Confirmation is not applicable in this refactor

Because no migration helper will be added and no automatic file mutation is allowed, there is no confirmation flow to implement now.

If a future dedicated migration helper is proposed, it must define its own explicit confirmation/overwrite contract in a follow-up spec.

### 5. Old-file handling and safety guarantees

This refactor must preserve the following guarantees:

- Never delete legacy user config files.
- Never rename legacy user config files automatically.
- Never overwrite user config files as part of startup guidance.
- Never remove unknown fields or unrelated content from user config files.
- Never leave multiple default config files behind as a side effect of automatic migration, because automatic migration is not performed.
- Keep legacy config loading support during the migration window so existing users continue to start successfully unless they hit a separately-defined validation error.

### 6. Legacy paths/names do not become hard errors in this refactor

The release that lands this refactor is a warning-only migration release.

- Accepted legacy config paths, legacy env names, and legacy CLI names remain compatibility-supported in this refactor.
- They must emit actionable deprecation guidance according to the warning policy.
- They must not become hard errors anywhere in this change set.

Earliest removal policy:

- Hard errors are deferred until a later follow-up release.
- The earliest allowed hard-error point is the first release after at least one full shipped release cycle where canonical docs, help text, templates, and startup guidance are already in place.
- A follow-up spec must name the exact removal release and error text before implementation flips from warn-only to hard-error.

## Deprecation policy for this refactor

During the warning-only migration window, deprecation handling must follow these rules:

- Validate the effective canonical config model after accepted legacy config has been normalized into canonical targets.
- Continue accepting supported legacy config-file paths, legacy environment variable names, and legacy CLI flag names during this refactor.
- Reject unsupported config-file suffixes with a clear error that names the provided suffix, the file path, and the supported suffix list.
- Emit at most one warning per deprecated logical surface per process load:
  - deprecated config-file paths warn by deprecated canonical path name
  - deprecated environment variables warn by legacy env var name
  - deprecated CLI flags warn by legacy flag name
- Each warning must include the exact canonical replacement surface.

Warning text templates for this release:

- Config path: `deprecated config path "<legacy-path>" is accepted for now; use "<canonical-path>" instead`
- Environment variable: `deprecated environment variable "<legacy-env>" is accepted for now; use "<canonical-env>" instead`
- CLI flag: `deprecated CLI flag "<legacy-flag>" is accepted for now; use "<canonical-flag>" instead`

Future hard-error text templates reserved for the follow-up removal release:

- Config path: `legacy config path "<legacy-path>" is no longer supported; use "<canonical-path>" instead`
- Environment variable: `legacy environment variable "<legacy-env>" is no longer supported; use "<canonical-env>" instead`
- CLI flag: `legacy CLI flag "<legacy-flag>" is no longer supported; use "<canonical-flag>" instead`

## Implementation notes

- Treat this record as the source of truth for the migration UX task and the later implementation task for startup guidance / warnings.
- The implementation should prefer startup messaging that tells users the exact replacement path or name instead of attempting silent mutation.
- Future work may add a dedicated migration helper, but that is intentionally outside the scope of this spec.
