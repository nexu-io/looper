# Looper CLI reference for agents

This skill is for end-user Looper operation. Prefer the installed `looper` CLI and managed `looperd` daemon; do not suggest developer-only commands to end users.

## Install and uninstall

Install Looper with the script shown in the project README:

```bash
curl -fsSL https://raw.githubusercontent.com/nexu-io/looper/main/scripts/install.sh | sh
```

Uninstall Looper with:

```bash
curl -fsSL https://raw.githubusercontent.com/nexu-io/looper/main/scripts/uninstall.sh | sh
```

The uninstall script removes the CLI binary, managed daemon binary, and updater state. It asks before deleting config, database, backups, logs, and worktrees.

## Installed CLI checks

Use read-only commands before changing state:

```bash
looper status
looper daemon status
looper daemon status --json
looper daemon logs
looper webhook status
```

If webhook mode is degraded because `gh webhook forward` left conflicting GitHub CLI hooks behind, inspect them with:

```bash
looper webhook cleanup owner/repo
```

That command is dry-run by default. Only after confirming the listed `cli` hooks are stale should you delete them:

```bash
looper webhook cleanup owner/repo --confirm
```

## Common operations

Initial setup normally uses:

Before bootstrap:

- Confirm the target repo path is absolute and points to a Git repository.
- Confirm the intended `agent.vendor` (for example `opencode`).
- Check `gh auth status` and ensure `git`/`gh` resolve in the environment that will run `looperd`.
- If `git` or `gh` are missing, ask before installing them. On macOS with Homebrew, the usual repair is `brew install git gh`; otherwise use the user's OS/package manager.
- Ask before using `--yes`; bootstrap may create config, install or reuse the managed daemon, register a project, and start the daemon.
- If `~/.looper/config.json` already exists, inspect targeted fields first and avoid overwriting user configuration.

```bash
looper bootstrap --yes --project-path /path/to/repo --agent-vendor opencode
```

After bootstrap, validate with:

```bash
looper status
looper daemon status
looper daemon logs --startup
```

## Webhook mode lifecycle

Webhook mode is optional. It improves reaction latency by running local `gh webhook forward` subprocesses and keeping polling as a correctness fallback.

### 1. Enable webhook mode

Enable webhook mode in config:

```bash
looper webhook enable
```

If `gh webhook` is not available yet, either install it yourself:

```bash
gh extension install cli/gh-webhook
```

or let Looper do it during enable:

```bash
looper webhook enable --install-gh-webhook
```

After enabling, restart `looperd` only after confirming with the user because restart can trigger background automation:

```bash
looper daemon restart
```

### 2. Verify a healthy webhook runtime

Use:

```bash
looper webhook status
looper webhook status --verbose
```

A healthy runtime usually shows:

- `enabled: yes`
- `degraded: no`
- one running forwarder per configured GitHub repo
- `endpointUrl` pointing at the local daemon listener

### 3. Understand the fallback contract

Webhook mode does **not** replace polling. If webhook forwarders fail, Looper still falls back to `webhook.fallbackPollIntervalSeconds` polling. That means correctness should remain intact while latency worsens.

### 4. Common degraded states

Start with:

```bash
looper webhook status --verbose
looper daemon status --json
looper daemon logs
gh auth status
```

Typical causes and first actions:

- `gh could not be resolved` → set `tools.ghPath` explicitly or fix the daemon environment.
- `server.host is not loopback` → webhook forwarders require a loopback daemon endpoint such as `127.0.0.1` or `localhost`.
- `Hook already exists on this repository` / repeated forwarder exits → inspect stale GitHub CLI hooks with `looper webhook cleanup owner/repo`.
- `HTTP 401`, `HTTP 403`, `authentication required`, `gh auth login` → fix GitHub auth first.
- `HTTP 404` or other GitHub API failures → verify repo access and the authenticated account.

### 5. Clean up stale GitHub CLI hooks

If `gh webhook forward` left conflicting remote `cli` hooks behind, inspect them first:

```bash
looper webhook cleanup owner/repo
```

That command is dry-run by default. If the listed hooks are clearly stale GitHub CLI forwarder hooks, delete them explicitly:

```bash
looper webhook cleanup owner/repo --confirm
```

Notes:

- This command only targets GitHub CLI `cli` hooks that point at `https://webhook-forwarder.github.com/hook`.
- It does not run automatically from the daemon.
- In multi-repo setups, run it once per affected `owner/repo`.
- `--confirm` can delete another developer's active GitHub CLI forwarder hook on a shared repo; only use it when the user understands that risk.
- Listing and deleting repo hooks requires sufficient GitHub permissions for that repository; if the command fails, verify `gh auth status` and repo access first.

### 6. After cleanup: when restart is needed

Cleanup alone may be enough if `looperd` is already healthy enough to retry and recreate forwarders on its own. Re-check first:

```bash
looper webhook status
```

If `looper webhook status --verbose` shows `latched: true`, cleanup first and then restart `looperd` after confirming with the user because a latched forwarder will not respawn on its own.

If webhook mode is still degraded after cleanup, then restart `looperd` after confirming with the user:

```bash
looper daemon restart
```

### 7. Useful verbose fields

`looper webhook status --verbose` may show these per-forwarder fields:

- `adopted` — Looper adopted a still-running local forwarder from persisted local state.
- `latched` — Looper saw a terminal forwarder failure and stopped respawning that forwarder until restart/reconfigure.
- `latchReason` — remediation-oriented explanation for a latched forwarder.
- `fingerprint` — normalized command identity for the local forwarder.
- `spawnedAt` / `lastStartedAt` / `lastExitAt` — timing data for local process lifecycle.
- `stdoutTail` / `stderrTail` — recent captured subprocess output; adopted processes may not have captured tails.

## Add an existing project/repo

If Looper is already installed and the user has an existing local repository, register it explicitly instead of re-running first-time bootstrap:

```bash
looper project add /absolute/path/to/repo --id myproj --repo owner/repo
```

Preflight before adding:

- Confirm `/absolute/path/to/repo` is the user's intended local Git repository.
- Prefer an absolute path; avoid registering a guessed or relative path without confirmation.
- Confirm the GitHub repository slug (`owner/repo`) or let the user provide it.
- Check `gh auth status` and ensure the authenticated account can access the repository.
- If `--id` is omitted, Looper generates or infers a project id; use `--id` when the user needs a stable, memorable project id.
- Use `--name`, `--base-branch`, and `--worktree-root` only when the user wants non-default values.
- Use `--snapshot-mode async`, `full`, or `off` only when the user asks to control initial PR snapshot behavior; default behavior is configured by `defaults.addSnapshotMode`.

Validate after adding:

```bash
looper project list
looper status
```

After a project is registered, Looper can often infer it from commands run inside that repo. If no project matches the current directory, or multiple projects match, pass `--project <id>` explicitly.

Daemon lifecycle commands:

```bash
looper daemon install
looper daemon start
looper daemon status
looper daemon logs
looper daemon restart
looper daemon stop
```

Loop inspection commands:

```bash
looper ps
looper logs <id> --follow
looper jump <id>
looper stop <id>
```

Manual fixer trigger:

```bash
looper fix owner/repo#42
looper fix 42
```

Use `looper fix` when you want to trigger the fixer pipeline for a specific pull request on demand instead of waiting for automatic pickup.

Upgrade commands:

```bash
looper upgrade --check
looper upgrade
looper upgrade --cli
looper upgrade --daemon
```

## Agent behavior

- Prefer status/log commands first.
- Use installed `looper` commands for end-user setup and troubleshooting.
- Do not rewrite user config or delete runtime artifacts unless explicitly confirmed.
