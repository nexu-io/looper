# Installation and Upgrade Guide

This document contains the detailed install, upgrade, uninstall, and source-build flows for Looper.

## Requirements

### Platform support

| Platform | Architecture | Install | Daemon supervision |
|---|---|---|---|
| macOS | arm64, amd64 | install.sh / manual | launchd |
| Linux | amd64, arm64 | install.sh / manual | systemd (user unit) |
| Windows | amd64, arm64 | manual | Windows Service (`sc.exe`) |

### Common dependencies

- `git`
- `gh` (GitHub CLI, authenticated)

### Platform-specific

- **macOS**: `osascript` if macOS notifications stay enabled
- **Linux**: `notify-send` (libnotify) for desktop notifications; `systemctl --user` for systemd supervision
- **Windows**: PowerShell (for toast notifications); `sc.exe` for Windows Service supervision

### Source development

- Go `1.22`

`looperd` auto-detects tool paths from `PATH`, but startup validation fails if required tools cannot be resolved.

## Install

Looper uses Go binaries as the default supported implementation.

### Quick install

Pick your platform:

#### macOS / Linux

```bash
curl -fsSL https://raw.githubusercontent.com/nexu-io/looper/main/scripts/install.sh | sh
looper bootstrap --yes --project-path /path/to/repo --agent-vendor opencode
```

#### Windows

```powershell
Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
irm https://raw.githubusercontent.com/nexu-io/looper/main/scripts/install.ps1 | iex
looper bootstrap --yes --project-path C:\path\to\repo --agent-vendor opencode
```

`looper bootstrap` creates an initial config, installs or reuses the managed daemon, optionally registers a project, and starts the daemon.

### Install the CLI manually

GitHub Releases publish standalone Go binaries (and `.tar.gz` archives with SHA-256 checksums) for both `looper` and `looperd` on all supported platforms.

**Download URLs** (replace `vX.Y.Z` with the desired tag or `latest`):

```
https://github.com/nexu-io/looper/releases/download/vX.Y.Z/looper-{target}
https://github.com/nexu-io/looper/releases/download/vX.Y.Z/looper-{target}.sha256
https://github.com/nexu-io/looper/releases/download/vX.Y.Z/looper-{target}.tar.gz
https://github.com/nexu-io/looper/releases/download/vX.Y.Z/looper-{target}.tar.gz.sha256
```

Same pattern for `looperd-{target}`.

Supported `{target}` values:

| Target | OS | Architecture |
|---|---|---|
| `darwin-arm64` | macOS | Apple Silicon (M1+) |
| `darwin-amd64` | macOS | Intel |
| `linux-amd64` | Linux | x86_64 |
| `linux-arm64` | Linux | ARM64 |
| `windows-amd64` | Windows | x86_64 |
| `windows-arm64` | Windows | ARM64 |

**Steps:**

1. Download the matching `looper` and `looperd` artifacts (raw binary or `.tar.gz`) for your platform from [GitHub Releases](https://github.com/nexu-io/looper/releases/latest).
2. Verify the SHA-256 checksum:
   ```bash
   shasum -a 256 -c looper-{target}.sha256    # macOS / Linux
   certutil -hashfile looper-{target} SHA256   # Windows
   ```
3. Place both binaries on your `PATH`:
   - macOS / Linux: `/usr/local/bin/looper` or `~/.local/bin/looper`
   - Windows: `%APPDATA%\Looper\bin\looper.exe` (or any directory in `PATH`)

### Install the daemon manually

If you prefer the manual daemon flow instead of `looper bootstrap`:

```bash
looper daemon install
looper daemon start
looper status
```

This flow:

- detects the current platform and architecture
- downloads the matching GitHub Release artifact
- installs it to `~/.looper/bin/looperd` (Unix) or `%APPDATA%\Looper\bin\looperd.exe` (Windows)

Current release binaries are unsigned. On macOS, if Gatekeeper blocks the first launch, you may need to allow the binary manually in System Settings.

Manual fallback:

- download the matching `looperd` release artifact yourself
- place it at `~/.looper/bin/looperd` or somewhere on your `PATH`

Daemon lookup order is fixed to `~/.looper/bin/looperd`, then `$PATH`.

By default, `looper daemon start` launches `looperd` detached and prints `started detached, not supervised`. Detached mode writes `~/.looper/looperd.pid` and `~/.looper/looperd.state.json`, but it does not restart after crashes, logout, or reboot.

### Supervised daemon mode on macOS

For actively supervised `looperd` lifecycle management on macOS, use the user LaunchAgent mode:

```bash
looper daemon start --daemon-mode launchd
looper daemon status
looper daemon status --json
looper daemon logs
```

Launchd mode:

- creates a user LaunchAgent plist at `~/Library/LaunchAgents/com.nexu-io.looper.looperd.plist` unless `daemon.plistPath` is set
- stores launchd stdout/stderr logs under `~/.looper/logs/launchd/`
- stores startup logs under `~/.looper/logs/startup/`
- stores lifecycle state in `~/.looper/looperd.state.json`
- maps `daemon.restartPolicy` to launchd `KeepAlive` behavior
- uses `daemon.restartThrottleSeconds` as the launchd `ThrottleInterval`
- may recover after login/system restart when launchd loads the user agent

Supported restart policies are `never`, `on-failure`, and `always`; the default is `on-failure` with a 10 second throttle.

### Supervised daemon mode on Linux

For actively supervised `looperd` lifecycle management on Linux, use systemd user mode:

```bash
looper daemon start --daemon-mode systemd
looper daemon status
looper daemon status --json
```

Systemd mode:

- creates a user systemd unit at `~/.config/systemd/user/looperd.service`
- stores lifecycle state in `~/.looper/looperd.state.json`
- maps `daemon.restartPolicy` to systemd `Restart=` behavior
- may recover after login when the user manager is available
- uses `systemctl --user` for install, start, stop, and status queries

Supported restart policies are `never`, `on-failure`, and `always`; the default is `on-failure` with a 10 second throttle.

### Supervised daemon mode on Windows

For actively supervised `looperd` lifecycle management on Windows, use the Windows Service mode (requires Administrator privileges):

```powershell
looper daemon start --daemon-mode windows-service
looper daemon status
looper daemon status --json
```

Windows Service mode:

- registers `looperd` as a Windows Service named `looperd` via `sc.exe`
- stores lifecycle state in `%APPDATA%\Looper\looperd.state.json`
- uses `sc.exe` for install, start, stop, and status queries
- may recover after reboot when the service auto-start is configured

Supported restart policies are `never`, `on-failure`, and `always`; the default is `on-failure` with a 10 second throttle.

On unsupported platforms, platform-specific daemon modes return an actionable error instead of silently falling back.

Troubleshooting commands:

```bash
looper daemon status
looper daemon status --json
looper daemon logs --startup
```

Status output distinguishes detached mode from launchd-supervised mode and includes PID, start time, supervisor, restart policy, stale/exited state, last error/reason, and log locations.

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

### Unix (macOS / Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/nexu-io/looper/main/scripts/uninstall.sh | sh
```

The uninstall script removes the CLI binary, the managed daemon binary, and updater state. It asks before deleting config, the SQLite DB, backups, logs, and worktrees.

### Windows

```powershell
Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
irm https://raw.githubusercontent.com/nexu-io/looper/main/scripts/uninstall.ps1 | iex
```

The uninstall script removes the CLI and daemon binaries, updater state, and asks before deleting config, the SQLite DB, backups, logs, and worktrees. If the daemon is running as a Windows Service, stop and remove it first:

```powershell
looper daemon stop --daemon-mode windows-service
sc.exe delete looperd
```

Manual fallback: remove the CLI binary and daemon binary under `%APPDATA%\Looper\bin\`, then delete `%APPDATA%\Looper`.

## From source

Clone the repo:

```bash
git clone https://github.com/nexu-io/looper.git
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
