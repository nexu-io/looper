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
- For GitHub projects, check `gh auth status` and ensure `git`/`gh` resolve in the environment that will run `looperd`.
- For Forgejo-only configs, ensure `git` resolves and either the provider `tokenEnv` is exported or the configured `teaLogin` works for the daemon user; `gh` is not required.
- If a required `git` or `gh` binary is missing, ask before installing it. On macOS with Homebrew, the usual repair is `brew install git gh`; otherwise use the user's OS/package manager.
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

If Looper is already installed and the user wants to watch another repository on GitHub or on an **already-configured** Forgejo provider, use `looper project add` as the default **zero-restart** workflow instead of re-running first-time bootstrap or editing `~/.looper/config.toml`. The binding is saved to the runtime Project Catalog in SQLite, inherits the active global `roles.*` and `defaults.*` policy, and begins polling immediately without restarting `looperd`.

> **Exception — creating a new Forgejo provider:** Passing `--forgejo-url` to create a brand-new Forgejo provider is **not** zero-restart. `looper project add` writes `[[providers]]` to the config file and restarts `looperd` automatically so the daemon can load the new provider. Always confirm the daemon restart with the user before running `--forgejo-url` against a running daemon.

### 1. Preflight before adding

- Inspect current runtime state (`looper status`, `looper project list`, `looper config show`) to see existing projects, configured Forgejo providers, and whether global `roles.*` defaults already match what the new project needs.
- Confirm `/absolute/path/to/repo` is the intended local Git repository (prefer an absolute path; clone the repo first if it is not yet on disk, and avoid registering guessed or relative paths without confirmation).
- Confirm the repository slug (`owner/repo`), `--id` (omit to let Looper infer one, or pass `--id` when a stable, memorable id is needed), and optional `--name`, `--base-branch`, or `--worktree-root`.
- Verify provider access:
  - **GitHub**: run `gh auth status` and ensure the authenticated account can access `owner/repo`.
  - **Forgejo**: confirm the provider in `looper status` (or its `teaLogin` / `tokenEnv` credentials) and verify remote access with `git ls-remote <clone-url> HEAD`.
- Use `--snapshot-mode async`, `full`, or `off` only when the user asks to control initial PR snapshot behavior; default behavior is configured by `defaults.addSnapshotMode`.

### 2. Register the project (`looper project add`)

**GitHub:**

```bash
looper project add /absolute/path/to/repo --id myproj --repo owner/repo
```

**Forgejo (reusing an existing provider):**

```bash
looper project add /absolute/path/to/repo \
  --id myproj \
  --name owner/name \
  --provider forgejo-main \
  --repo owner/name \
  --base-branch main
```

The repository slug is detected from an origin matching the selected provider when possible; otherwise pass `--repo owner/name`. The binding is saved and activated immediately through the runtime Project Catalog, so background automation may begin as soon as the command succeeds.

**Forgejo (creating the provider and binding the project in the same flow — restarts `looperd`):**

> **Warning:** Because `[[providers]]` is restart-bound, running `looper project add` with `--forgejo-url` (when that provider ID does not already exist with matching settings) writes the provider to config and invokes a daemon restart automatically. Confirm with the user before running this against an active daemon.

```bash
export FORGEJO_TOKEN=<forgejo-token>
looper project add /absolute/path/to/repo \
  --provider forgejo-main \
  --forgejo-url https://code.example.com \
  --forgejo-token-env FORGEJO_TOKEN

# Or reuse an explicit tea login (no tokenEnv):
# looper project add /absolute/path/to/repo \
#   --provider forgejo-main \
#   --forgejo-url https://code.example.com \
#   --auth tea --tea-login powerformer-code
```

### 3. Validate after adding (no daemon restart for GitHub or an existing Forgejo provider)

```bash
looper project list
looper status
```

Confirm the project appears in both outputs and `looperd` remains healthy without restarting the daemon. After a project is registered, Looper can often infer it from commands run inside that repo. If no project matches the current directory, or multiple projects match, pass `--project <id>` explicitly.

### 4. When (and only when) to edit `~/.looper/config.toml`

- Do **not** append a `[[projects]]` block to `~/.looper/config.toml` just to register a project or mirror existing project blocks when global defaults already suffice.
- `projects` in `config.toml` is a **restart-bound** setting (`configuration changes require a daemon restart: projects`). Adding or modifying `[[projects]]` in `config.toml` causes `looperd`'s file watcher to reject config reloads (`rejectedPaths: ["projects"]`) until the edit is reverted or `looperd` is restarted.
- Only add a `[[projects]]` block in `config.toml` when the user explicitly wants per-project overrides that `looper project add` cannot express (such as project-specific `roles.<role>.instructions`, `roles.reviewer.skills`, or custom discovery/trigger overrides), and confirm with the user beforehand that applying those overrides requires a daemon restart (or waiting for the next restart).

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
looper describe <id>
looper logs <id> --follow
looper jump <id>
looper stop <id>
```

`looper describe` is the top-level alias for `looper loop inspect` and shows diagnosis detail for blocked or failed loops.

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
