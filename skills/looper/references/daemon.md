# Looper daemon reference for agents

`looperd` is the background daemon that polls GitHub and runs configured Looper roles. Starting or restarting it can trigger repository automation, so confirm user intent before changing lifecycle state.

## Read-only checks

Start with:

```bash
looper daemon status
looper daemon status --json
looper status
looper daemon logs
looper daemon logs --startup
```

## Install and start

Managed daemon install:

```bash
looper daemon install
looper daemon start
```

By default, `looper daemon start` launches `looperd` detached. Detached mode writes `~/.looper/looperd.pid` and `~/.looper/looperd.state.json`, but it is not supervised across crashes, logout, or reboot.

Supervised daemon modes are available per platform:

**macOS — launchd:**
```bash
looper daemon start --daemon-mode launchd
looper daemon status
looper daemon logs
```
Creates a user LaunchAgent plist and stores logs under `~/.looper/logs/`.

**Linux — systemd (user unit):**
```bash
looper daemon start --daemon-mode systemd
looper daemon status
```
Creates `~/.config/systemd/user/looperd.service` and uses `systemctl --user`.

**Windows — Windows Service:**
```powershell
looper daemon start --daemon-mode windows-service
looper daemon status
```
Registers `looperd` as a Windows Service via `sc.exe`.

## Troubleshooting startup failures

Check these before changing config:

1. `git` and `gh` are installed and resolvable.
2. `gh` is authenticated for the target repositories.
3. `~/.looper/` and configured storage/log/backup/worktree paths are writable.
4. `~/.looper/config.json` is valid JSON and passes Looper validation.
5. Platform notification tools resolve:
   - macOS: `osascript` (if `notifications.osascript.enabled` is true)
   - Linux: `notify-send` (auto-detected, soft requirement)
   - Windows: PowerShell (included with OS, hard requirement for toast)
6. The managed daemon binary exists at `~/.looper/bin/looperd` (Unix) or `%APPDATA%\Looper\bin\looperd.exe` (Windows), or `looperd` resolves on `PATH`.

Useful checks — adapt to platform:

```bash
command -v git
command -v gh
gh auth status
test -w ~/.looper
# macOS only:
command -v osascript
# Linux only:
command -v notify-send
```

If a tool resolves in your shell but not for `looperd`, set explicit tool paths in config: `tools.gitPath`, `tools.ghPath` (all platforms), or `tools.osascriptPath` (macOS only).

If `git` or `gh` are missing, ask before installing them. Common install commands:

- **macOS**: `brew install git gh`
- **Linux (Debian/Ubuntu)**: `sudo apt install git gh`
- **Linux (Fedora)**: `sudo dnf install git gh`
- **Windows**: `winget install Git.Git GitHub.cli`

Useful repair command after daemon binary issues:

```bash
looper daemon install --force
```

After any repair, re-run read-only checks and only run repair or restart commands after confirming with the user.
