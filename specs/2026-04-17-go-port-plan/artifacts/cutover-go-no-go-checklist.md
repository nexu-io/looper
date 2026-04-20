# Go cutover go/no-go checklist

Date: 2026-04-21

This checklist defines the explicit decision gates that must be reviewed before the Go binaries replace the TypeScript binaries as the default supported implementation.

## Current decision

- Status: **NO-GO** for replacing the TypeScript binaries today.
- Reason: parity and packaging work is complete enough to stage a cutover, but the repository is not yet operationally Go-first.
- Open cutover blockers tracked in `.ralph/ralph-tasks.md`:
  1. Make Go binaries the default supported implementation.
  2. Switch CI and release pipelines to Go-first.
  3. Remove Bun from the required production runtime path.
  4. Retire or archive the TypeScript implementation after parity is proven.

## How to use this checklist

- A **GO** decision requires every gate in the next section to be checked and no unresolved blocker in the final sign-off section.
- A **NO-GO** decision is required if any contract-compatibility gate fails, if production/runtime docs still point operators to the TypeScript path, or if rollback/release ownership is unclear.
- Evidence should be attached by linking the relevant artifact, test, workflow, or doc change in this spec folder.

## Gate 1 — Compatibility and parity evidence

- [x] Config loading, env/CLI precedence, and validation are frozen and revalidated.
  - Evidence: `artifacts/status-config-foundations.md`, `artifacts/config-surface.md`
- [x] The `/api/v1/*` contract, JSON envelopes, and error codes remain frozen and machine-verifiable.
  - Evidence: `artifacts/daemon-http.compat.json`, `artifacts/daemon-http.requests.compat.json`, `artifacts/daemon-http.responses.compat.json`, `artifacts/daemon-http.errors.compat.json`, `artifacts/daemon-http-endpoints.md`
- [x] CLI commands, help surfaces, and flag semantics are frozen for the current supported workflows.
  - Evidence: `artifacts/cli-commands.md`, `artifacts/cli-flags.compat.json`
- [x] Daemon startup, recovery, shutdown, and run-lifecycle parity are documented and revalidated.
  - Evidence: `artifacts/daemon-lifecycle-parity.md`, `artifacts/daemon-lifecycle-notes.md`
- [x] SQLite schema reuse, migration ordering, backup behavior, and end-state schema parity are documented and revalidated.
  - Evidence: `artifacts/sqlite-schema-reuse-decision.md`, `artifacts/sqlite-migration-sequence.md`, `artifacts/sqlite-schema.snapshot.sql`
- [x] Git, GitHub, spec-PR, and agent completion-marker behavior have been revalidated at the end of the rewrite.
  - Evidence: `artifacts/git-github-specpr-revalidation.md`, `artifacts/spec-pr-and-agent-completion.md`
- [x] Reviewer, fixer, planner, and worker orchestration flows have repeatable validation coverage.
  - Evidence: `artifacts/reviewer-fixer-planner-worker-e2e-validation.md`, `artifacts/sample-repo-workflow-validation.md`

## Gate 2 — Packaging, install, and upgrade readiness

- [x] Release artifacts exist for `looper` and `looperd` under the preserved asset names expected by install/upgrade flows.
  - Evidence: `artifacts/drop-in-artifact-install-path-compatibility.md`, `.github/workflows/release.yml`
- [x] Managed daemon install and upgrade flows still target `~/.looper/bin/looperd` and verify release checksums.
  - Evidence: `artifacts/release-download-local-install-validation.md`, `artifacts/go-install-upgrade-lifecycle-validation.md`
- [x] `looperd --version` still matches the preserved output shape used by release validation.
  - Evidence: `artifacts/release-download-local-install-validation.md`, `.github/workflows/release.yml`
- [x] Install and configuration docs describe the Go release/install path without breaking existing operators.
  - Evidence: `README.md`, `docs/configuration.md`

## Gate 3 — Operational cutover prerequisites

- [x] The Go binaries are the default supported implementation in user-facing docs and operator guidance.
  - Blocking task: `Make Go binaries the default supported implementation`
- [x] Default CI is Go-first, with any remaining TypeScript verification clearly demoted to compatibility coverage instead of the primary path.
  - Blocking task: `Switch CI and release pipelines to Go-first`
  - Evidence: `.github/workflows/ci.yml`, `.github/workflows/release.yml`
- [ ] Bun is no longer required on the production runtime path for supported installs and upgrades.
  - Blocking task: `Remove Bun from the required production runtime path`
  - Current status: supported docs now point to the Go release binaries, but `.github/workflows/release.yml` still publishes the CLI through npm and production runtime assumptions still need cleanup.
- [x] The fate of `apps/web` is explicitly documented so the cutover does not leave an ambiguous supported surface.
  - Blocking task: `Decide and document the fate of apps/web`
  - Current status: `apps/web` is retained only as an unsupported placeholder workspace package and is not part of the supported release surface.
  - Evidence: `artifacts/apps-web-fate.md`, `README.md`
- [ ] The TypeScript implementation has an explicit maintenance, archive, or removal plan.
  - Blocking task: `Retire or archive the TypeScript implementation after parity is proven`

## Gate 4 — Cutover execution and rollback plan

The cutover owner should not announce the Go binaries as the default until all of the following are prepared in the same change window:

- [x] A release note or migration note tells existing users whether anything changes for install, upgrade, PATH layout, or operational commands.
- [ ] A rollback plan exists that restores the previous default binaries/artifacts if post-release validation fails.
- [ ] The first post-cutover validation run includes at minimum: CI green, release dry-run/validation green, managed install green, daemon start/status green, one sample workflow green.
- [ ] Ownership is explicit for cutover execution, rollback approval, and post-release monitoring.

## Final sign-off template

Use this section at the moment of cutover.

- Decision date:
- Release/tag:
- Cutover owner:
- Reviewer/approver:
- Result: GO / NO-GO
- Blocking issues or follow-ups:

Do not mark the result **GO** while any Gate 3 or Gate 4 checkbox remains open.

## Summary

The rewrite has cleared the parity and release-readiness gates needed to prepare for cutover, but it has **not** yet cleared the operational cutover gates. The correct decision today is **NO-GO until the remaining Phase 10 tasks are complete**.
