---
name: looper
description: Use when installing, bootstrapping, configuring, starting, verifying, operating, or troubleshooting Looper, looperd, the looper CLI, ~/.looper config, or runtime paths; when setting up Looper with opencode, claude-code, codex, or cursor-cli; when registering repos or configuring planner/reviewer/fixer/worker loops; or when diagnosing status, logs, osascript, git, gh, LOOPER_TOKEN, writable path, or daemon startup issues.
---

# Looper

Use this skill when an agent needs to install, configure, start, check, operate, or troubleshoot Looper (`looper` CLI, `looperd` daemon, or files under `~/.looper`).

It also covers the full webhook-mode lifecycle — turning it on, installing or validating the `gh webhook` extension, confirming forwarders are healthy, diagnosing a degraded runtime, clearing stale GitHub CLI hooks with `looper webhook cleanup`, and judging when a daemon restart is actually needed.

**When NOT to use this skill:** developing on the Looper codebase itself (Go sources at `cmd/`, `internal/`, `pkg/`). For that, follow `AGENTS.md` and standard Go tooling.

## Looper in one paragraph

Looper is a local daemon (`looperd`) that polls GitHub and runs four agent loops in their own git worktrees. Each loop is gated by GitHub labels:

| Role | Default discovery | Hands off via |
| --- | --- | --- |
| 🧭 **Planner** | Open issues with `looper:plan`, assigned to current user | Opens spec PR labeled `looper:spec-reviewing` |
| 🔍 **Reviewer** | PRs where current user is review-requested, plus `looper:spec-reviewing` follow-up | A clean review on a `looper:spec-reviewing` PR promotes it to `looper:spec-ready` |
| 🔧 **Fixer** | Open non-draft PRs authored by current user with actionable threads | Pushes fixes; reviewer re-runs |
| 🚢 **Worker** | Open issues with `looper:worker-ready` (assigned), or PRs labeled `looper:spec-ready` | Implements on the same PR until checks pass |

All trigger fields combine with logical AND; empty label lists mean "no label constraint." Triggers are customizable per role and per project — see [`references/config.md`](references/config.md) for the full schema, validation rules, and override examples.

Manual loop starts always work, even when `roles.<role>.autoDiscovery=false`:

```bash
looper plan   --project <id> --issue <num>
looper review <owner>/<repo>#<pr> [--loop]
looper work   --project <id> --issue <num>
looper loop start --type fixer --pr <owner>/<repo>#<pr>
```

## One-shot install and configuration

Use this when the user wants Looper installed, configured, and running end-to-end. Confirm each destructive step before running it (config writes, daemon start, project add).

### Step 0 — Preflight (read-only)

Looper currently supports macOS (`darwin-arm64`) and Linux (`linux-amd64`). Stop and ask the user how to proceed if the host is not supported:

```bash
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64|Darwin-aarch64) ;;
  Linux-x86_64|Linux-amd64) ;;
  *) echo "Looper supports macOS arm64 and Linux x64 only; stop and confirm with the user before continuing." >&2 ;;
esac
```

On Linux, use detached foreground-mode daemon management; launchd supervision is macOS-only.

```bash
case "$(uname -s)" in
  Darwin) ;;
  *) echo "Skip macOS launchd assumptions on this host." >&2 ;;
esac
```

Then check required tools:

```bash
command -v git
command -v gh
gh auth status
command -v osascript   # required if osascript notifications stay enabled
```

If `git` or `gh` are missing, ask the user before installing them. On macOS with Homebrew:

```bash
brew install git gh
```

If `gh auth status` is not authenticated, ask the user to run `gh auth login`.

For deeper preflight detail, see [`references/daemon.md`](references/daemon.md).

### Step 1 — Detect available agent vendors

Auto-detect installed agent CLIs in parallel before asking:

| `agent.vendor` | Detect with |
| --- | --- |
| `claude-code` | `command -v claude` |
| `codex` | `command -v codex` |
| `opencode` | `command -v opencode` |
| `cursor-cli` | `command -v agent` |

Use the `question` tool to let the user pick one. List detected vendors first, marked `(installed)`, with undetected ones appended as `(not installed — needs setup)`. If multiple are installed, do not impose an opinionated default — present them in detection order and let the user choose.

If none are installed, ask the user which one they want to install before continuing; do not proceed to bootstrap with a vendor whose CLI is missing.

After the user picks a vendor, verify it is authenticated (run the vendor's own status command, e.g. `claude --version` followed by a quick auth check, or `agent status`). If the vendor CLI exits with an auth error, surface it and ask the user to log in via the vendor's own flow before continuing.

Looper inherits the vendor's own authentication (e.g. `claude login`, `agent login`, or env vars in the user's shell). **Do not** store agent credentials in the Looper config file (commonly `~/.looper/config.toml`, with some existing installs still using `~/.looper/config.json`).

### Step 2 — Pick the first project to watch

Use the `question` tool with exactly these three options:

1. **Use the current directory** — "Register the repo at the current working directory (must be a git checkout)."
2. **Enter a project path** — "Provide an absolute path to a local git repository on disk."
3. **Skip for now** — "Bootstrap Looper without a project; add one later with `looper project add`."

Resolution rules per choice — bind the user-provided path to a shell variable (e.g. `REPO=...`) so it does not collide with `$PATH`:

- **Current directory**: `REPO="$(git -C "$PWD" rev-parse --show-toplevel)"`. If `git` errors, the directory is not a git repo — fall back to asking for an explicit path.
- **Project path**: validate the path is absolute and contains a `.git` entry: `test -d "$REPO/.git" || test -f "$REPO/.git"` (the file form supports git worktrees). Reject relative paths and ask again.
- **Skip for now**: continue to Step 3 with no `--project-path` flag.

Save the resolved absolute path (if any) for Step 4. See [`references/cli.md`](references/cli.md) for `looper project add` semantics.

### Step 3 — Install the `looper` CLI

```bash
curl -fsSL https://raw.githubusercontent.com/nexu-io/looper/main/scripts/install.sh | sh
looper --version
```

If `looper --version` fails, do not guess a new install location. The installer controls placement; the typical fix is a `PATH` problem in the user's shell. Determine where `install.sh` placed the binary, then ask the user whether to add that directory to their shell's `PATH` (e.g. by editing `~/.zshrc`).

### Step 4 — Bootstrap config, daemon, and first project

`looper bootstrap` writes the active Looper config file (usually `~/.looper/config.toml`; some existing installs still use `~/.looper/config.json`), installs the managed daemon to `~/.looper/bin/looperd`, optionally registers a project, and starts `looperd`.

**If a Looper config file already exists (commonly `~/.looper/config.toml` or legacy `~/.looper/config.json`), do NOT pass `--yes`.** Inspect first with `looper config show`, then triage by what is missing or wrong:

| Existing-config state | Action |
| --- | --- |
| Config exists, daemon healthy, no projects yet | Run `looper project add` for the chosen path; skip bootstrap |
| Config exists with wrong/missing `agent.vendor` | Targeted edit to `agent.vendor` after confirming with the user |
| Config exists, daemon unhealthy | Triage with `looper daemon status` and `looper daemon logs --startup` first; do not re-bootstrap blindly |
| Config exists and is correct | Skip bootstrap; go to Step 5 verification |

When the config does not yet exist and you have the user's selections from Steps 1–2:

```bash
# With a project path from Step 2
looper bootstrap --yes \
  --project-path "$REPO" \
  --agent-vendor "<selected-vendor>"

# Skipped project in Step 2
looper bootstrap --yes \
  --agent-vendor "<selected-vendor>"
```

If the user prefers to drive bootstrap themselves: `looper bootstrap` (interactive). See [`references/cli.md`](references/cli.md) for every supported flag.

### Step 5 — Verify the install

Run all of these and report the results. Do not restart the daemon if status is healthy.

```bash
looper status
looper daemon status
looper daemon logs --startup
looper config show
looper project list
```

The bundled diagnostic helper is read-only and safe to run; invoke it via its absolute skill path (it is not on `PATH`):

```bash
bash <skill-bundle>/scripts/check.sh
```

Replace `<skill-bundle>` with the actual install location of this skill (commonly under `~/.claude/skills/looper/` or wherever the skill installer placed it). If the path is unknown, skip the helper and rely on the `looper`/`gh` checks above.

A healthy install shows:

- `looper status` reports daemon running and config valid.
- `looper daemon status` shows a PID, recent start time, no `last error`.
- `looper config show` lists the expected `agent.vendor` and projects.
- `looper project list` lists every repo the user expects.
- `gh auth status` is authenticated for those repos.

If `server.authMode` is `local-token`, the user needs to export the token in their shell before running CLI commands:

```bash
export LOOPER_TOKEN="<value-of-server.localToken>"
```

For daemon log layout and supervised vs detached mode, see [`references/daemon.md`](references/daemon.md).

### Step 6 — Add additional projects (optional)

```bash
looper project add /absolute/path/to/repo --id <stable-id> --repo <owner>/<repo>
```

Always prefer absolute paths and confirm the GitHub slug (`owner/repo`) before running.

### Step 7 — First loop (smoke test)

Suggest a smoke test only after explicit confirmation, since this triggers automation against the user's GitHub repo. **Pick a non-production repo and a low-risk issue the user is happy to plan.** Do not run smoke tests against critical production workflows.

```bash
looper plan --project <id> --issue <num>
looper ps
looper logs <id> --follow
```

### Common install failures

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `tools.gitPath` or `tools.ghPath` could not be resolved | `looperd` cannot find binaries in its env | Set explicit `tools.gitPath` / `tools.ghPath` in config |
| `tools.osascriptPath is required when osascript notifications are enabled` | macOS notifications enabled but `osascript` not resolvable | Set `tools.osascriptPath`, or disable `notifications.osascript.enabled` after confirming |
| `authMode=local-token requires server.localToken` | Token mode without token | Add `server.localToken` and export `LOOPER_TOKEN` for the CLI |
| `agent.vendor` missing | No agent configured | Set `agent.vendor` to a supported vendor whose CLI is installed locally |
| Runtime path not writable | `~/.looper/`, `logs/`, `backups/`, or worktree root not writable | Fix ownership/permissions, do not delete data without explicit confirmation |
| Daemon binary missing | `~/.looper/bin/looperd` not installed | `looper daemon install --force`, then `looper daemon start` |
| `looper --version` not found | Installer placed binary outside `PATH` | Identify install dir, ask user to add it to shell `PATH` |

## References

For deeper detail, consult these bundled docs before acting:

- [`references/cli.md`](references/cli.md) — installed `looper` CLI commands, install/uninstall scripts, `looper bootstrap`, `looper project add`, daemon lifecycle, loop inspection.
- [`references/config.md`](references/config.md) — full Looper config shape, every field, validation rules, env var overrides, CLI flag overrides, role trigger customization, reviewer event mapping.
- [`references/daemon.md`](references/daemon.md) — `looperd` startup, supervised vs detached mode, launchd integration, log locations, startup-failure triage.
- [`scripts/check.sh`](scripts/check.sh) — read-only local diagnostic. Verifies `git`, `gh`, `gh auth status`, optional `osascript`, `looper --version`, config presence, and `~/.looper` writability. Invoke via absolute skill path.

When in doubt, prefer read-only checks first:

```bash
looper status
looper daemon status --json
looper daemon logs --startup
looper config show
looper webhook status
```

When webhook mode is degraded, inspect stale GitHub CLI forwarder hooks before restarting the daemon:

```bash
looper webhook cleanup owner/repo
```

Only run deletion after the dry run shows stale `cli` hooks and the user confirms:

```bash
looper webhook cleanup owner/repo --confirm
```

## Safety rules

- Do not overwrite or rewrite the Looper config file (`~/.looper/config.toml` on new installs; `~/.looper/config.json` on some existing installs) without explicit user confirmation. Prefer targeted edits.
- Do not delete runtime artifacts (`~/.looper/looper.sqlite`, `backups/`, `logs/`, `worktrees/`) unless the user explicitly asks and understands the impact.
- Starting or restarting `looperd` can launch background automation against configured GitHub repositories — confirm intent before doing so.
- Do not toggle `daemon.mode` (foreground ↔ launchd) without confirming; supervised mode persists across login/reboot.
- Reviewer defaults are intentionally action-taking (`clean=APPROVE`, `blocking=REQUEST_CHANGES`) while `enableSelfReview` stays off. Do not broaden reviewer authority further without explicit user opt-in (for example enabling self-review or auto-merge, or relaxing review-event guardrails).
- Do not overwrite or delete existing `looper:*` labels in user repos without confirmation; they may have local customizations.
- Never print secrets from config or environment. Redact tokens and API keys as `***` in summaries.
- Prefer `looper daemon status`, `looper daemon logs`, and the read-only checks above before making changes.

## Common mistakes

- `cat ~/.looper/config.*` as a first move: use `looper config show` instead and redact secrets.
- Restarting the daemon under pressure: check status/logs first; restart can re-trigger automation.
- Disabling `notifications.osascript.enabled` silently: confirm the change or set an explicit `tools.osascriptPath`.
- Rewriting the whole config for one fix: make targeted edits and preserve existing settings.
- Treating a missing `~/.looper/` directory as permission to create or delete data: explain impact and ask first.
- Running smoke tests against production repos: pick low-risk issues only.
