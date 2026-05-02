# Go managed install/upgrade lifecycle validation

Date: 2026-04-21

This artifact closes the Ralph task to validate Go managed daemon install/upgrade behavior end to end (in-process test harness).

## Command run

- `go test ./internal/cliapp -run TestManagedDaemonInstallUpgradeLifecycleEndToEnd -count=1`

The command passed.

## Coverage confirmed

- runs one realistic lifecycle scenario with shared mutable state across steps:
  - `looper daemon install --force` (managed install at pinned old release)
  - `looper daemon start`
  - `looper daemon status --json` (API reachable, reports old daemon version)
  - `looper upgrade --daemon` (latest release changes, managed binary replaced)
  - `looper daemon restart`
  - `looper daemon status --json` (API reachable, reports upgraded daemon version)
- validates install/upgrade download + checksum behavior by serving distinct release metadata and binary payloads for `v0.2.0` then `v0.3.0`
- validates restart semantics by modeling process liveness and daemon-reported running version from the binary active at spawn time

## Caveats

- This is an in-process end-to-end style test using dependency injection for process control and HTTP calls; it does not spawn a real external looperd process.
- The flow still validates cross-command state transitions (binary bytes on disk, daemon PID lifecycle, status surface, and upgrade replacement behavior) in a single scenario.

## Result

Go CLI coverage now includes a focused managed install/upgrade lifecycle validation that spans install, start/status, upgrade, and restart/status in one repeatable test.
