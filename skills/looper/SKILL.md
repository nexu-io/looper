---
name: looper
description: Use when configuring, starting, checking, or troubleshooting looper, looperd, ~/.looper config, or runtime paths; when setting up Looper for agents; or when diagnosing status, logs, osascript, git, gh, writable path, or daemon startup issues.
---

# Looper

Use this skill when an agent needs to configure, start, check, operate, or troubleshoot Looper (`looper` CLI, `looperd` daemon, or files under `~/.looper`).

## Quick start for agents

1. Read the relevant reference before acting:
   - `references/cli.md` for installation, uninstall, and installed CLI workflows.
   - `references/config.md` before reading or changing `~/.looper/config.json`.
   - `references/daemon.md` before starting, stopping, or debugging `looperd`.
2. Prefer read-only checks first:

   ```bash
   looper status
   looper daemon status
   looper daemon status --json
   ```

3. For local environment diagnostics, run the bundled read-only helper:

   ```bash
   bash scripts/check.sh
   ```

## Quick reference

| Situation | Use |
| --- | --- |
| Need install, uninstall, bootstrap, project registration, or CLI commands | `references/cli.md` |
| Need to inspect or edit config | `references/config.md` |
| Need daemon startup, status, logs, or restart help | `references/daemon.md` |
| Need read-only local diagnostics | `scripts/check.sh` |

## Install this skill

From a checkout of this repository, users can add the skill with:

```bash
npx skills add ./skills/looper
```

Or directly from the repository path supported by the skill installer:

```bash
npx skills add https://github.com/nexu-io/looper/tree/main/skills/looper
```

Verify installation by confirming `looper` appears in the skill installer's list output, then ask the agent to “check looper status” and ensure it invokes this skill.

## Safety rules

- Do not overwrite or rewrite `~/.looper/config.json` without explicit user confirmation.
- Do not delete runtime artifacts (`~/.looper/looper.sqlite`, `backups/`, `logs/`, `worktrees/`) unless the user explicitly asks and understands the impact.
- Starting or restarting `looperd` can launch background automation against configured GitHub repositories; confirm intent before doing so.
- Prefer `looper daemon status`, `looper daemon logs`, and `scripts/check.sh` before making changes.
- Never print secrets from config or environment. Redact tokens and API keys in summaries.

## Common mistakes

- Starting with `cat ~/.looper/config.json`: use targeted inspection and redact secrets instead.
- Restarting immediately under pressure: check status/logs first and confirm because automation may run.
- Disabling `notifications.osascript.enabled` silently: confirm the change or set an explicit `tools.osascriptPath`.
- Rewriting the whole config for one fix: make targeted edits and preserve existing settings.
- Treating missing `~/.looper/` as permission to create/delete data: explain the impact and ask first.

## Core facts

- Default config path: `~/.looper/config.json`.
- Config precedence: defaults → config file → environment → CLI flags.
- Default runtime artifacts live under `~/.looper/`.
- `looperd` fails fast on config-validation errors and requires writable runtime paths.
- Tool paths for `git`, `gh`, and `osascript` are auto-detected unless explicitly configured.
- If `notifications.osascript.enabled` is true, `osascript` must resolve or startup fails.

## Diagnostic helper

`scripts/check.sh` is read-only and deterministic. It checks required tools (`git`, `gh`), `gh auth status`, optional `osascript`, `looper --version`, config presence, and `~/.looper` writability. It exits nonzero only for missing required tools, an unset `HOME`, or unwritable runtime paths.
