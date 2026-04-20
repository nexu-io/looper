# Status/config foundations checkpoint

This checkpoint closes the preferred execution-order item:

- `Port status/config foundations before deeper runtime automation`

The goal of the checkpoint is not new runtime automation behavior. It is to prove that the Go port already has the stable read-path foundations that deeper runtime automation depends on:

1. normalized daemon config loading and validation
2. frozen `/api/v1/status` and `/api/v1/config` surfaces
3. CLI commands that consume those surfaces in both human and JSON modes

## Implemented Go foundations

### Config loading and parity

- `internal/config/defaults.go` defines the baseline config shape and defaults.
- `internal/config/types.go` defines the normalized config schema used by the daemon and CLI.
- `internal/config/load.go` ports file loading, env/CLI override precedence, path resolution, tool auto-detection, and validation.
- `internal/config/config_test.go` covers precedence, normalization, validation, and tool resolution.
- `internal/config/config_parity_test.go` locks the Go loader to the frozen TypeScript compatibility fixtures.

### Daemon read-only API surface

- `internal/api/handler.go` serves:
  - `GET /api/v1/healthz`
  - `GET /api/v1/status`
  - `GET /api/v1/config`
- These routes use the shared JSON success/error envelope and the same auth gate as the rest of `/api/v1/*`.

### CLI consumption of the foundations

- `internal/cliapp/app.go` exposes:
  - `looper status`
  - `looper config show`
  - `looper daemon status`
- `internal/cliapp/json_output.go` and `internal/cliapp/daemon_runtime.go` consume the status/config endpoints for machine-readable and human-readable output.

## Why this is sufficient before deeper runtime automation

Deeper runtime automation work (project management, loop/run control, scheduler/runtime orchestration, agent execution) depends on a stable way to:

- load and validate runtime config
- inspect daemon health and readiness
- inspect the effective config snapshot
- surface that information through the CLI

Those pieces are already ported and covered by tests, so later runtime automation work can build on them instead of redefining them.

## Validation evidence

- `internal/config/config_parity_test.go` proves Go config loading matches the frozen parity fixtures.
- `internal/api/handler_test.go` covers status/config responses plus auth and method handling.
- `internal/cliapp/app_test.go` covers `looper status --json` and `looper config show --json`.
- `internal/cliapp/daemon_runtime_test.go` covers daemon status reporting paths.

Recommended verification commands for this checkpoint:

```sh
go test ./internal/config ./internal/api ./internal/cliapp
```

## Sequencing conclusion

This preferred-order checkpoint is satisfied.

Remaining runtime work should treat config loading plus the status/config API and CLI read paths as established foundations, then continue with:

1. project management before deeper loop/run automation
2. delayed process execution and agent orchestration until storage/contracts are stable
