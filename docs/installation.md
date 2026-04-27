# Installation and Upgrade Guide

This document contains the detailed install, upgrade, uninstall, and source-build flows for Looper.

## Requirements

For the default supported install path:

- macOS (`darwin-arm64`)
- `git`
- `gh`

For source development:

- Go `1.22`
- `git`
- `gh`
- `osascript` if macOS notifications stay enabled

`looperd` auto-detects tool paths from `PATH`, but startup validation fails if required tools cannot be resolved.

## Install

Looper uses Go binaries as the default supported implementation.

The quickest first-time setup is:

```bash
curl -fsSL https://raw.githubusercontent.com/powerformer/looper/main/scripts/install.sh | sh
looper bootstrap --yes --project-path /path/to/repo --agent-vendor opencode
```

`looper bootstrap` creates an initial config, installs or reuses the managed daemon, optionally registers a project, and starts the daemon.

### Install the CLI manually

1. Download the matching `looper` release artifact for your macOS architecture from GitHub Releases.
2. Rename it to `looper` if needed.
3. Place it on your `PATH`, for example `/usr/local/bin/looper` or `~/.local/bin/looper`.

GitHub Releases publish standalone Go binaries for both `looper` and `looperd` on `darwin-arm64`.

Linux is not currently supported for the managed daemon flow.

### Install the daemon manually

If you prefer the manual daemon flow instead of `looper bootstrap`:

```bash
looper daemon install
looper daemon start
looper status
```

This flow:

- detects the current macOS architecture
- downloads the matching GitHub Release artifact
- installs it to `~/.looper/bin/looperd`

Current release binaries are unsigned. If macOS Gatekeeper blocks the first launch, you may need to allow the binary manually in System Settings.

Manual fallback:

- download the matching `looperd` release artifact yourself
- place it at `~/.looper/bin/looperd` or somewhere on your `PATH`

Daemon lookup order is fixed to `~/.looper/bin/looperd`, then `$PATH`.

`looper daemon start` writes a pid file and launches the daemon, but it does not provide full background supervision.

## Verify the install

In another shell:

```bash
looper status
looper daemon status
```

## Upgrade

Unified upgrade entrypoint:

```bash
looper upgrade
looper upgrade --check
looper upgrade --cli
looper upgrade --daemon
```

Current behavior:

- `looper upgrade --check` shows current/latest CLI and daemon versions
- `looper upgrade` attempts CLI self-upgrade when safe, then upgrades the managed daemon
- `looper upgrade --cli` upgrades the `looper` binary only when the current install looks like a release-binary install
- `looper upgrade --daemon` installs or upgrades the managed daemon binary
- Homebrew and dev / `go install` installs refuse CLI self-upgrade and print the matching manual command instead
- after a daemon upgrade, restart manually with `looper daemon restart`
- manifest-gated upgrade, rollback, and channel switching are not implemented yet

## Compatibility and version policy

- CLI and daemon are published from the same git tag and should normally share the same version
- short-lived version skew is allowed while the HTTP API remains compatible
- management endpoints stay under `/api/v1/*`
- if the daemon is running, the CLI reads its current version from `/api/v1/status`; otherwise it falls back to `looperd --version`
- `looper upgrade --check` reads the latest CLI and daemon versions from GitHub Releases metadata
- release builds are tag-driven (`vX.Y.Z` / `vX.Y.Z-rc.N`); local default builds use `0.0.0-dev`

## Uninstall

```bash
curl -fsSL https://raw.githubusercontent.com/powerformer/looper/main/scripts/uninstall.sh | sh
```

The uninstall script removes the CLI binary, the managed daemon binary, and updater state. It asks before deleting config, the SQLite DB, backups, logs, and worktrees.

## From source

Clone the repo:

```bash
git clone https://github.com/powerformer/looper.git
cd looper
```

Then build or run the Go binaries:

```bash
go build ./cmd/looper
go build ./cmd/looperd
go run ./cmd/looperd
```

In another shell, run the CLI from source:

```bash
go run ./cmd/looper status
```
