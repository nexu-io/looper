# Looper

[![CI](https://github.com/nexu-io/looper/actions/workflows/ci.yml/badge.svg)](https://github.com/nexu-io/looper/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.22-00ADD8?logo=go)](go.mod)

**An autonomous AI dev team for your GitHub repos — plan, review, fix, and ship PRs, on a loop.**

> *"LLMs are exceptionally good at looping until they meet specific goals... Don't tell it what to do, give it success criteria and watch it go."*
> — Andrej Karpathy

Looper turns that idea into a local AI dev team. Register the repos you want it to watch; Looper picks up assigned, labeled issues and runs specialized agents — **planner → reviewer ↔ fixer → worker** — each looping against its own success criteria until the PR is ready for human merge. GitHub stays the source of truth; Looper handles the spec, review cycle, and implementation in isolated worktrees.

![Looper technical architecture](assets/looper-technical-architecture.png)

Looper ships two binaries:

- `looperd` — the background daemon that polls GitHub, runs loops, and manages worktrees
- `looper` — the CLI for setup, control, inspection, and manual loop starts

## Four loops, four success criteria

Each role is an agent that keeps looping until *its* exit condition is met — no fixed step counts, just goals.

- 🧭 **Planner** — *loops until the spec is reviewable.* Reads the issue, explores the repo, drafts a spec, critiques it, and revises until the plan is concrete enough to open a spec PR. Done when the spec PR is open and labeled `looper:spec-reviewing`.
- 🔍 **Reviewer** — *loops until the PR meets the bar.* Re-reads the PR on every new commit, posts inline threads, and keeps re-reviewing as the fixer pushes changes. Done when no actionable threads remain and the review comes back clean.
- 🔧 **Fixer** — *loops until reviewer threads are handled.* Pulls open review comments, addresses them in the worktree, pushes, and waits for the reviewer's next pass. Ping-pongs with the reviewer until the PR converges. Done when every actionable thread is resolved, or replied to when human input is needed.
- 🚢 **Worker** — *loops until the PR is ready for merge.* Takes the `looper:spec-ready` spec PR, implements the spec on top of it, runs checks, and iterates on its own output. Done when checks pass and the PR is ready for human review and merge.

The loops compose: planner hands off to reviewer↔fixer, reviewer↔fixer hands off to worker, and `looperd` gates each transition on GitHub labels — so you can pause, intervene, or take over at any boundary.

## Features

- 🚢 **Start from an issue, not a prompt.** Label an issue `looper:plan`, assign it to yourself, and a spec PR shows up. Once it reaches `looper:spec-ready`, implementation begins.
- 🐙 **GitHub is the only source of truth.** Issues, PRs, labels, reviews, and assignees *are* the workflow — no external task tracker, no YAML pipeline, no project-specific config. If you can use GitHub, you can drive Looper.
- 🛰️ **Many repos, one daemon.** Register your projects once — Looper watches them together and runs loops across repos in parallel.
- 🌳 **Parallel-safe by design.** Every loop runs in its own git worktree, so agents work across issues and repos without stepping on each other.
- 🤖 **Bring your own agent.** Pluggable vendor layer (`opencode`, `claude-code`, `codex`, `cursor-cli`) so you're not locked into one model or CLI.
- 🧰 **Local, inspectable, stoppable.** Daemon on your machine, thin CLI to drive it. `looper ps`, `looper logs`, `looper stop` — no hosted control plane.

## Quick start

### For agents

If you're an AI coding agent (Claude Code, OpenCode, Codex, Cursor, etc.) helping a user set up Looper, fetch and follow the install + configure tutorial in the bundled skill:

```
https://github.com/nexu-io/looper/blob/main/skills/looper/SKILL.md
```

It contains a one-shot, step-by-step flow (preflight → install → bootstrap → vendor credentials → verify → first loop) plus a troubleshooting matrix. Confirm destructive steps with the user before running them.

### For humans

Fast path (macOS, `darwin-arm64`):

```bash
curl -fsSL https://raw.githubusercontent.com/nexu-io/looper/main/scripts/install.sh | sh
looper bootstrap
looper project add /path/to/your/local/repo
```

`bootstrap` interactively writes your config, installs the managed daemon, and starts `looperd`. Use `--yes` only for scripts or other non-interactive installs.

`/path/to/your/local/repo` means the local git checkout you want Looper to watch — the directory that contains that repo's `.git` folder, not a GitHub URL. For example:

```bash
looper project add ~/src/my-app
# or, from inside the repo:
looper project add .
```

Add each repo you want Looper to watch after bootstrap. Full install, upgrade, uninstall, and from-source instructions: **[docs/installation.md](docs/installation.md)**.

Once `looper status` succeeds and `gh auth status` shows an authenticated account, drive loops manually:

```bash
# plan a spec from an issue
looper plan --project <id> --issue <num>

# review a PR — one-shot, or keep looping as new commits land
looper review <owner>/<repo>#<pr>
looper review <owner>/<repo>#<pr> --loop

# implement from an issue (reuses planner's spec PR if one exists)
looper work --project <id> --issue <num>
```

Inside a registered repo, `--project` is usually optional for `review` and `work`, and you can drop the `<owner>/<repo>` prefix on PR refs. Pass them explicitly from outside the repo or when multiple projects could match.

The full workflow — label conventions, assignment rules, how planner / reviewer / fixer / worker hand off — is in **[docs/users-guide.md](docs/users-guide.md)**.

## Agent skill

Looper includes an installable agent skill for setup, status, config, daemon lifecycle, and troubleshooting guidance:

```bash
npx skills add ./skills/looper
```

Or install it directly from GitHub:

```bash
npx skills add https://github.com/nexu-io/looper/tree/main/skills/looper
```

See [`skills/looper/SKILL.md`](skills/looper/SKILL.md) for install and verification details.

## How it works

The four loops above are the conceptual model. Here's the GitHub label state machine `looperd` actually drives:

```
issue (looper:plan, assigned)
       │
       ▼
   planner ──► spec PR (looper:spec-reviewing)
                       │
                       ▼
                reviewer ⇄ fixer
                       │  clean
                       ▼
              PR labeled looper:spec-ready
                       │
                       ▼
                    worker
                       │
                       ▼
              PR ready for human merge  🎉
```

Each role runs in its own worktree, coordinated by `looperd` and gated by labels. The planner opens the spec PR, the reviewer and fixer loop on it until it's clean, and `looper:spec-ready` is the signal that hands work to the worker — which implements on the same PR rather than opening a new one.

Looper is poll-driven, not webhook-driven: keep `looperd` running and `gh` authenticated for the loop to fire. Everything runs locally — no hosted control plane required.

## Networked operation

Looper supports two project modes:

- `network.mode=off` — local-only behavior. Worker still claims `looper:worker-ready` Issues assigned to the local GitHub user, Reviewer still claims review requests for the local GitHub user, and any `looper:target:*` labels are ignored.
- `network.mode=routed` — multi-Node behavior. `loopernet` centralizes webhook ingress and event fan-out, but GitHub remains the authority for work intent.

In Routed mode:

- Coordinator, not `loopernet`, mutates GitHub for Issue admission and PR review assignment.
- `looper:worker-ready` and GitHub review requests express work intent.
- exactly one `looper:target:<node_name>` label is the exact-Node authority, and Coordinator writes it last.
- the `loopernet` Coordinator lease is only a fencing gate for mutation rights; if the lease is stale, Coordinator must stop mutating GitHub.
- polling stays enabled as drift recovery if webhook ingress or SSE wakeups are missed; it is not the primary wakeup path.

For setup, identity strategy, and recovery steps, see **[docs/users-guide.md](docs/users-guide.md)** and **[docs/configuration.md](docs/configuration.md)**. The formal authority rules live in ADRs **[0007](docs/adr/0007-coordinator-admission-assignment-authority.md)** through **[0011](docs/adr/0011-coordinator-control-plane-for-routed-projects-v1.md)**.

## Command cheatsheet

**Setup & health**

```bash
looper bootstrap            # first-run setup
looper status               # daemon + config health
looper version
looper project list
looper project add /path/to/repo
```

**Start loops manually**

```bash
looper plan   --project <id> --issue <num>
looper review <owner>/<repo>#<pr> [--loop]
looper work   --project <id> --issue <num>
looper loop start --type fixer --pr <owner>/<repo>#<pr>
```

`--project` can be omitted for `plan` / `work` when run from inside a uniquely registered repo; `review` can also omit the `<owner>/<repo>` prefix in that case, but `loop start --pr` always requires `<owner>/<repo>#<pr>`.

**Inspect PRs**

```bash
looper pr list
looper pr show   <owner>/<repo>#<pr>
looper pr status <owner>/<repo>#<pr>
```

**Manage running loops**

```bash
looper ps                   # list active loops
looper logs <id> --follow   # stream logs
looper jump <id>            # jump into a loop's worktree
looper stop <id>
```

**Daemon control**

```bash
looper daemon install|start|stop|restart|status
```

## Configuration

- Canonical default path: `~/.looper/config.toml`
- Supported formats: `.toml`, `.yaml`, `.yml`, `.json`
- Config source selection precedence: `--config` → `LOOPER_CONFIG` → default-path discovery
- All role-specific config lives under `roles.<role>`; canonical reviewer behavior lives under `roles.reviewer.behavior.*`
- Loading legacy `~/.looper/config.json` emits one informational note per process telling users that `~/.looper/config.toml` is now the preferred default path
- `agent.vendor` is required to run loops (no default)
- If `server.authMode=local-token`, set `server.localToken` and export `LOOPER_TOKEN` for the CLI

Every field, env var, CLI flag, validation rule, and troubleshooting note lives in **[docs/configuration.md](docs/configuration.md)**.

## Development

From the repo root:

```bash
go run ./cmd/looperd
go run ./cmd/looper <args>
go build ./...
go vet ./...
go test ./...
```

Build artifacts go to `dist/` and are gitignored — don't edit generated files.

## Runtime notes

- `looperd` fails fast on invalid config; runtime paths must be writable
- The managed daemon binary lives at `~/.looper/bin/looperd`
- Daemon-managed worktrees live under `~/.looper/worktrees/`, grouped by repo and project
- `looper worktree cleanup` dry-runs Looper-managed worktree cleanup; `--confirm` removes eligible clean terminal worktrees without deleting branches
- When `notifications.osascript.enabled=true`, `osascript` must resolve on startup
- Automation is poll-driven, not webhook-driven — keep `looperd` running and `gh` installed and authenticated for the loop to fire
