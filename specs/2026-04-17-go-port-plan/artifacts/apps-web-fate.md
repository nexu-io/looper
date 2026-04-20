# Fate of `apps/web`

Date: 2026-04-21

## Decision

`apps/web` remains in the repository as an **unsupported placeholder workspace package**, not as part of the current supported product surface.

## Why

- The supported product is the Go `looper` CLI plus the Go `looperd` daemon.
- `apps/web` has no implemented product behavior today; it is only a stub entrypoint.
- The current release workflow does not publish any web artifact.
- Removing it immediately would create extra workspace and compatibility churn during the cutover without delivering any user-facing value.

## What this means

- Keep `apps/web` as a reserved location for a future web UI if product work later needs it.
- Treat it as out of scope for the Go cutover and out of scope for supported install, upgrade, and runtime docs.
- Do not describe it as a supported application, release artifact, or production dependency.
- Revisit whether to remove, replace, or implement it when the remaining TypeScript retirement work is completed or when an actual web product is scoped.

## Evidence

- `apps/web/src/index.ts` only logs a placeholder message.
- `README.md` and `AGENTS.md` already describe the current product as the daemon + CLI.
- `.github/workflows/release.yml` publishes Go binaries only, with no web artifact.
