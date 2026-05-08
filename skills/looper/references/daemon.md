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

On macOS, supervised LaunchAgent mode is available:

```bash
looper daemon start --daemon-mode launchd
looper daemon status
looper daemon logs
```

Launchd mode creates or uses a user LaunchAgent plist and stores logs under `~/.looper/logs/`.

## Troubleshooting startup failures

Check these before changing config:

1. `git` and `gh` are installed and resolvable.
2. `gh` is authenticated for the target repositories.
3. `~/.looper/` and configured storage/log/backup/worktree paths are writable.
4. `~/.looper/config.json` is valid JSON and passes Looper validation.
5. If notifications enable osascript, `osascript` resolves.
6. The managed daemon binary exists at `~/.looper/bin/looperd` or `looperd` resolves on `PATH`.

Useful checks:

```bash
command -v git
command -v gh
gh auth status
command -v osascript
test -w ~/.looper
```

If a tool resolves in your shell but not for `looperd`, set explicit `tools.gitPath`, `tools.ghPath`, or `tools.osascriptPath` in config after confirming with the user.

If `git` or `gh` are missing, ask before installing them. On macOS with Homebrew, the usual repair is:

```bash
brew install git gh
```

On other systems, use the user's OS/package manager.

Useful repair command after daemon binary issues:

```bash
looper daemon install --force
```

After any repair, re-run read-only checks and only run repair or restart commands after confirming with the user.
