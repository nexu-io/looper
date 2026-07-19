# Looper daemon reference for agents

`looperd` is the background daemon that polls GitHub or Forgejo and runs configured Looper roles. Starting or restarting it can trigger repository automation, so confirm user intent before changing lifecycle state.

## Read-only checks

Start with:

```bash
looper daemon status
looper daemon status --json
looper status
looper daemon logs
looper daemon logs --startup
```

## Config reload versus restart

The daemon watches its selected TOML, YAML, or JSON config while running. Hot-safe policy changes are validated and published without changing the daemon PID. Claims made after publication use the new snapshot; active runs retain the snapshot they started with.

Use the Configuration page at `/dashboard/config` to see the selected path, winning source for each dashboard-managed hot field, last reload attempt, last successful application, and any rejected paths. File-only, project, and sensitive metadata families are intentionally omitted. Daemon logs also record a rejected reload. Invalid candidates and candidates containing a restart-bound change leave the last-known-good runtime configuration active.

Do not restart after every config edit. The hot-safe allowlist covers `agent.vendor` (including configuring the first vendor after startup), model, individual env entries, and canonical idle/max-runtime timeouts; scheduler concurrency/slow-lane fields; in-app and osascript notifications; disclosure; auto-commit/push, risky-fix, PR-strategy, and snapshot-mode defaults; `instructions.enabled`; curated role policy; and Looper/osascript paths. Planner `roles.planner.triggers.planeAssigneeId` remains file-only and restart-bound; Worker `roles.worker.triggers.planeAssigneeId` remains hot-safe. `defaults.baseBranch` remains restart-bound because configured project records materialize it.

Leaving one configured vendor—by switching or clearing it—requires empty `agent.params`; if `agent.model` is explicit, change or unset it in the same edit. Configuring the first vendor may use a prepared profile. Work continued after a cross-vendor switch uses the existing checkpoint and worktree in a fresh native session.

Restart is required for process-owned settings such as the HTTP listener, storage/runtime paths, logger, webhook/network topology, providers/projects, polling/cache cadence, and `git`/`gh` tool paths. It is also required for `agent.nativeResume`, `agent.params`, notification webhooks/Feishu, HITL, `instructions.maxBytes`, scheduler retry budget/base delay, Reviewer auto-merge and queue timing, `roles.coordinator.enabled`, Coordinator merge-watch transient retries, and Coordinator dependencies. See `references/config.md` for the exact current field list.

The Configuration page sends the file revision captured with its published values and performs a final identity/mode/byte check before atomic rename. A conflict normally means another editor changed the file, even if that generation has not yet passed reload validation; wait for it to publish and refresh, or fix its displayed diagnostics in the file before retrying. Portable filesystems leave a tiny final-check-to-rename race, so avoid simultaneous manual and dashboard writes. Without token authentication, config PATCH requires a direct loopback peer and Host authority and rejects forwarding headers; proxied access requires `local-token`. Dashboard writes retain unknown top-level extension sections and their native scalar values but may canonicalize comments and lexical ordering, and they reject symlinked config paths.

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

1. `git` is installed and resolvable.
2. For GitHub projects, `gh` is installed, resolvable, and authenticated for the target repositories.
3. For Forgejo projects, either the configured provider `tokenEnv` is present in the daemon environment (`token-env` auth), or the explicit `teaLogin` is available to the daemon user (`tea` auth).
4. `~/.looper/` and configured storage/log/backup/worktree paths are writable.
5. `~/.looper/config.toml` or the selected config file is valid and passes Looper validation.
6. If notifications enable osascript, `osascript` resolves.
7. The managed daemon binary exists at `~/.looper/bin/looperd` or `looperd` resolves on `PATH`.

Useful checks:

```bash
command -v git
command -v gh
gh auth status
command -v osascript
test -w ~/.looper
```

If a tool resolves in your shell but not for `looperd`, set explicit `tools.gitPath`, `tools.ghPath`, or `tools.osascriptPath` in config after confirming with the user.

`tools.ghPath` is only required for configs with GitHub projects. Do not add `ghPath` to fix a Forgejo-only startup unless the config also contains GitHub projects.

If a required `git` or `gh` binary is missing, ask before installing it. On macOS with Homebrew, the usual repair is:

```bash
brew install git gh
```

On other systems, use the user's OS/package manager.

Useful repair command after daemon binary issues:

```bash
looper daemon install --force
```

After any repair, re-run read-only checks and only run repair or restart commands after confirming with the user.

## Webhook-mode daemon checks

When the user is troubleshooting webhook mode specifically, add these read-only checks before restarting anything:

```bash
looper webhook status
looper webhook status --verbose
looper daemon status --json
looper daemon logs
```

Key daemon/runtime facts for webhook mode:

- `looperd` runs local `gh webhook forward` subprocesses; it does not rely on webhook mode for correctness because polling remains the fallback.
- Webhook forwarders require a loopback daemon endpoint. Non-loopback `server.host` will degrade webhook mode.
- If webhook runtime becomes degraded because stale remote GitHub CLI hooks already exist, prefer `looper webhook cleanup owner/repo` before restarting the daemon.
- Restart is only the next step if status still shows degraded after cleanup or after fixing auth/path/config problems.

### Admission degraded after agent execution persistence failure

When SQLite cannot durable-publish `agent_executions` observations (initial ownership, mid-life heartbeat/output, or terminal), looperd closes the single sticky **admission** state (`degraded`) so new work cannot continue with split-brain observations (ADR-0015 / #578).

The same sticky degrade applies when durable **queue claim finalization** (complete / cancel / requeue / typed fail) fails after a Supervisor operation lease owns a `running` claim (ADR-0015 / #579). Ownership is retained rather than pretending release succeeded.

Operator recovery:

1. Read `looper daemon logs` / `looper daemon status --json` for admission `degraded` and a reason mentioning agent execution persistence or queue finalize failure.
2. Repair storage under the runtime home (default `~/.looper/`): disk space, permissions, SQLite integrity.
3. **Restart `looperd`** — normal recovery. Admission stays degraded until process restart (or an explicit clear hook used only in tests/ops tools).
4. After restart, startup recovery classifies durable rows as **confirmed_dead** / **observed_live** / **uncertain** (ADR-0015 / #581). PID/PGID probes are evidence only — never confirmed-dead Authority. Uncertain and observed-live work is quarantined (`manual_intervention` / paused); do not force-requeue it.
