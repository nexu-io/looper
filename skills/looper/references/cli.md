# Looper CLI reference for agents

This skill is for end-user Looper operation. Prefer the installed `looper` CLI and managed `looperd` daemon; do not suggest developer-only commands to end users.

## Install and uninstall

Install Looper with the script shown in the project README:

```bash
curl -fsSL https://raw.githubusercontent.com/powerformer/looper/main/scripts/install.sh | sh
```

Uninstall Looper with:

```bash
curl -fsSL https://raw.githubusercontent.com/powerformer/looper/main/scripts/uninstall.sh | sh
```

The uninstall script removes the CLI binary, managed daemon binary, and updater state. It asks before deleting config, database, backups, logs, and worktrees.

## Installed CLI checks

Use read-only commands before changing state:

```bash
looper status
looper daemon status
looper daemon status --json
looper daemon logs
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
