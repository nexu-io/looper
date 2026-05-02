# Release download and local install validation

Date: 2026-04-21

This artifact closes the Ralph task to validate release downloads and local install flows for the Go cutover.

## Commands run

- `go test ./internal/cliapp -run 'TestInstallManagedDaemonInstallsBinary|TestDaemonInstallCommandPrintsHumanOutput|TestUpgradeCheckPrintsSummary|TestUpgradeDaemonInstallsManagedBinaryWhenOnlyPathBinaryExists|TestManagedDaemonInstallUpgradeLifecycleEndToEnd' -count=1`
- `export LOOPER_BUILD_TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)" LOOPER_BUILD_GIT_SHA="local-validation" LOOPER_FORCE_COMPILE="1" && go build -trimpath -ldflags "$(go run ./tools/go-build-flags)" -o dist/release/looper-darwin-arm64 ./cmd/looper && GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$(go run ./tools/go-build-flags)" -o dist/release/looperd-darwin-arm64 ./cmd/looperd && dist/release/looperd-darwin-arm64 --version`

Both commands passed locally.

## Coverage confirmed

- managed daemon install still downloads the expected GitHub release metadata, binary asset, and `.sha256` sidecar for the current macOS target
- local install still writes the managed daemon to `~/.looper/bin/looperd`
- install output still surfaces the downloaded release URL for human verification
- `upgrade --check` still reports the installed managed daemon version versus the latest GitHub release version
- `upgrade --daemon` still installs the managed binary when only a `$PATH` `looperd` exists
- one end-to-end lifecycle test still covers install → start → status → upgrade → restart → status with shared mutable state
- a local release-style `looperd-darwin-arm64` build still executes and prints its version successfully

## Evidence in repo

- `internal/cliapp/daemon_install.go`
  - fetches GitHub release metadata from `/releases/latest` or `/releases/tags/<tag>`
  - downloads the exact `looperd-darwin-*` asset and checksum sidecar
  - verifies SHA-256 before atomically renaming into `~/.looper/bin/looperd`
- `internal/cliapp/daemon_install_test.go`
  - verifies direct managed install behavior and user-facing `looper daemon install` output
- `internal/cliapp/upgrade_test.go`
  - verifies `upgrade --check`, managed install fallback, and the install/start/status/upgrade/restart/status lifecycle
- `.github/workflows/release.yml`
  - builds the preserved `looper-*` and `looperd-*` assets used by the install flow
  - verifies `looperd --version` during release builds before publishing

## Result

Release download behavior and local managed install flows are now re-validated for the Go implementation: the CLI still targets the preserved GitHub asset names, still installs to the managed daemon path, and still works with release-style binaries built locally.
