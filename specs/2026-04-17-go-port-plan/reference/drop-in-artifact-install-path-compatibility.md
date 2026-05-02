## Go drop-in artifact/install-path compatibility

Date: 2026-04-21

This artifact closes the Ralph task to preserve drop-in artifact naming, install path, and executable naming where possible during the Go cutover.

## Compatibility anchors kept

- GitHub release assets keep the predictable binary names consumed by existing install/upgrade flows:
  - `looper-darwin-arm64`
  - `looperd-darwin-arm64`
- Checksum sidecars keep the existing `<asset>.sha256` naming.
- The managed daemon install target remains `~/.looper/bin/looperd`.
- CLI daemon lookup order remains `~/.looper/bin/looperd`, then `$PATH`.
- The executable name on disk remains `looperd` for the managed daemon install.

## Evidence in repo

- `.github/workflows/release.yml`
  - builds release assets with the preserved `looper-*` and `looperd-*` names
  - verifies those exact names before publishing
  - publishes the assets under those same names
- `internal/cliapp/daemon_install.go`
  - installs the managed daemon to `filepath.Join(homeDir, ".looper", "bin", "looperd")`
- `internal/cliapp/daemon_runtime.go`
  - resolves the daemon binary by checking `~/.looper/bin/looperd` first, then `looperd` from `$PATH`
- `internal/cliapp/daemon_install_test.go`
  - verifies the managed install path and expected `looperd-darwin-*` release asset names
- `internal/cliapp/upgrade_test.go`
  - verifies upgrade/install flows continue to use the same release asset names and managed install location
- `docs/configuration.md`
  - already documents `~/.looper/bin/looperd` and the daemon lookup order

## Command run

- `go test ./internal/cliapp -run 'TestInstallManagedDaemonInstallsBinary|TestUpgradeDaemonInstallsManagedBinaryWhenOnlyPathBinaryExists|TestManagedDaemonInstallUpgradeLifecycleEndToEnd' -count=1`

The command passed.

## Result

The Go release and managed-daemon flows preserve the existing drop-in compatibility surface that matters before default cutover: published asset names stay stable, the managed install path stays `~/.looper/bin/looperd`, and the executable naming/lookup behavior remains `looperd`-compatible.
