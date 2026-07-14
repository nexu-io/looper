# Looper

[![CI](https://github.com/nexu-io/looper/actions/workflows/ci.yml/badge.svg)](https://github.com/nexu-io/looper/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.22-00ADD8?logo=go)](go.mod)

**An autonomous AI dev team for your GitHub and Forgejo repos — plan, review, fix, and ship PRs, on a loop.**

> *"LLMs are exceptionally good at looping until they meet specific goals... Don't tell it what to do, give it success criteria and watch it go."*
> — Andrej Karpathy

Looper turns that idea into a local AI dev team. Register the repos you want it to watch; Looper picks up assigned, labeled issues and runs specialized agents — **planner → reviewer ↔ fixer → worker** — each looping against its own success criteria until the PR is ready for human merge. Your forge stays the source of truth; Looper handles the spec, review cycle, and implementation in isolated worktrees.

![Looper technical architecture](assets/looper-technical-architecture.png)

Looper ships two binaries:

- `looperd` — the background daemon that polls GitHub or Forgejo, runs loops, and manages worktrees
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
- 🐙 **The forge is the source of truth.** Issues, PRs, labels, reviews, and assignees *are* the workflow — no external task tracker, no YAML pipeline. GitHub is fully supported; Forgejo supports planner, worker, native reviewer requests/reviews, and summary-comment compatibility flows.
- 🛰️ **Many repos, one daemon.** Register your projects once — Looper watches them together and runs loops across repos in parallel.
- 🌳 **Parallel-safe by design.** Every loop runs in its own git worktree, so agents work across issues and repos without stepping on each other.
- 🤖 **Bring your own agent.** Pluggable vendor layer (`opencode`, `claude-code`, `codex`, `cursor-cli`, `grok-build`) so you're not locked into one model or CLI.
- 🧰 **Local, inspectable, stoppable.** Daemon on your machine, thin CLI to drive it. `looper ps`, `looper logs`, `looper stop` — no hosted control plane.

## Quick start

### For agents

If you're an AI coding agent (Claude Code, OpenCode, Codex, Cursor, etc.) helping a user set up Looper, fetch and follow the install + configure tutorial in the bundled skill:

```
https://github.com/nexu-io/looper/blob/main/skills/looper/SKILL.md
```

It contains a one-shot, step-by-step flow (preflight → install → bootstrap → vendor credentials → verify → first loop) plus a troubleshooting matrix. Confirm destructive steps with the user before running them.

### For humans

Fast path (macOS `darwin-arm64` or Linux `linux-amd64`):

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

Once `looper status` succeeds and your forge credentials are configured (`gh auth status` for GitHub, or a configured Forgejo token environment variable), drive loops manually:

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

## Take over a single PR

Want to babysit *one* pull request — review it, fix review threads, dismiss unreasonable change requests, and keep going until it merges?

**The simplest path is one prompt.** Paste this into whatever coding agent you already run (Claude Code, Codex, opencode, Gemini, …):

> Take over this PR until it merges — read https://raw.githubusercontent.com/nexu-io/looper/main/skills/pr-takeover/SKILL.md and follow it.

That points the agent at the [`pr-takeover` skill](skills/pr-takeover/SKILL.md), which decides — confirming with you when unclear — between two modes:

- **Live (default, zero install)** — your own session drives the PR with `gh` + `git`. Uses your already-authenticated agent; runs while your session is alive.
- **Background (unattended)** — hands the PR to the `looper takeover` command below, so the `looperd` daemon runs the reviewer + fixer loops on their own and it survives you closing your terminal.

The rest of this section covers the `looper takeover` command that powers the background mode. Run it from inside the checkout:

```bash
# detect the current branch's PR automatically
looper takeover

# or target an explicit PR
looper takeover acme/repo#42
```

`takeover` is the one-command path that, behind the scenes:

- installs/starts the managed `looperd` daemon if it isn't already running;
- registers the repository **scoped to that single PR** — every autonomous discovery loop is disabled, so Looper only touches the PR you named, never the rest of the repo;
- starts a continuous **reviewer** loop and **fixer** loop on the PR, which ping-pong until the review comes back clean;
- with `--merge`, lets the reviewer enable GitHub auto-merge so the PR merges itself once it's approved and green.

It picks your coding agent automatically when exactly one of `claude` / `codex` / `grok` / `opencode` is on `PATH`, prompts when it's ambiguous, and accepts `--agent-vendor` / `--yes` for non-interactive (agent-driven) runs:

```bash
looper takeover acme/repo#42 --merge --agent-vendor claude-code
looper takeover --yes --agent-vendor codex          # fully non-interactive
```

List and stop takeovers:

```bash
looper takeover list                 # active takeovers + live loop status
looper takeover stop acme/repo#42    # stop this takeover's reviewer + fixer loops
looper takeover stop --all
```

Requirements: `git`, an authenticated `gh`, and one supported agent CLI installed locally (the agent runs on your machine with your own credentials). Grok Build from xAI uses `agent.vendor = "grok-build"` and the `grok` executable; see [Grok Build configuration](docs/configuration.md#grok-build-xai) for daemon authentication and automation limits.

### One-liner for not-yet-installed users

For someone who doesn't have Looper yet, a single piped command installs the CLI and hands off to `takeover`. This is what an automation/bot can drop into a PR comment so the author can adopt the PR in one step (run it from inside the repo checkout):

```bash
curl -fsSL https://raw.githubusercontent.com/nexu-io/looper/main/scripts/takeover.sh | sh -s -- acme/repo#42
```

Everything after `--` is forwarded to `looper takeover`, so `--merge` and `--agent-vendor` work there too.

## Agent skills

Looper ships installable agent skills:

- **`looper`** — setup, status, config, daemon lifecycle, and troubleshooting guidance.
- **`pr-takeover`** — drive a single PR to merge (read review feedback → fix → resolve threads → dismiss unreasonable change requests → merge when approved and green). One skill, two modes: it runs **live** in your own agent session (`gh` + `git`, zero install) or hands off to the **background** `looper takeover` daemon, confirming with you when unclear. Works in any agent via one universal prompt — see [`skills/pr-takeover/SKILL.md`](skills/pr-takeover/SKILL.md).

```bash
npx skills add ./skills/looper
npx skills add ./skills/pr-takeover
```

Or install directly from GitHub:

```bash
npx skills add https://github.com/nexu-io/looper/tree/main/skills/looper
npx skills add https://github.com/nexu-io/looper/tree/main/skills/pr-takeover
```

See [`skills/looper/SKILL.md`](skills/looper/SKILL.md) and [`skills/pr-takeover/SKILL.md`](skills/pr-takeover/SKILL.md) for details.

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

Looper is poll-driven by default: keep `looperd` running and forge credentials available for the loop to fire. GitHub projects still use `gh`; Forgejo projects use the configured REST provider and do not require `gh` in Forgejo-only installs. Everything runs locally — no hosted control plane required.

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

For setup, identity strategy, recovery steps, and `loopernet` deployment, see **[docs/users-guide.md](docs/users-guide.md)**, **[docs/configuration.md](docs/configuration.md)**, and **[docs/loopernet-deployment.md](docs/loopernet-deployment.md)**. The formal authority rules live in ADRs **[0007](docs/adr/0007-coordinator-admission-assignment-authority.md)** through **[0011](docs/adr/0011-coordinator-control-plane-for-routed-projects-v1.md)**.

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
looper takeover [<owner>/<repo>#<pr>] [--merge]   # adopt one PR until it merges
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
looper run reconcile-stale  # recover stale running loops after sleep/wake
```

**Daemon control**

```bash
looper daemon install|start|stop|restart|status
```

If `looper ps` shows stale `running` work with no live agent after sleep/wake, run `looper run reconcile-stale` first. `looper daemon restart` remains a reasonable fallback when you want a full daemon restart.

## Configuration

- Canonical default path: `~/.looper/config.toml`
- Supported formats: `.toml`, `.yaml`, `.yml`, `.json`
- Config source selection precedence: `--config` → `LOOPER_CONFIG` → default-path discovery
- Provider support: legacy GitHub projects keep working through `gh`; Forgejo projects require an explicit provider, `baseUrl`, `repo`, and either `tokenEnv` (`auth=token-env`) or `teaLogin` (`auth=tea`)
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

Provider e2e checks:

```bash
go test ./internal/e2e/forgejocontract -count=1
go test ./internal/e2e -run 'Forgejo|Smoke|FailsFast|GitHubSandboxRepoEnv' -count=1
```

Forgejo live sandbox e2e is local/manual only and skipped unless explicitly enabled. Use a dedicated existing sandbox repo; tests create and clean run-scoped issues, branches, PRs, labels, and comments:

```bash
LOOPER_E2E_FORGEJO=1 \
LOOPER_E2E_FORGEJO_BASE_URL=https://code.example.com \
LOOPER_E2E_FORGEJO_SANDBOX_REPO=owner/repo \
LOOPER_E2E_FORGEJO_TOKEN=$TOKEN \
go test ./internal/e2e -run '^TestForgejoSandbox' -count=1
```

GitHub live sandbox tests prefer `LOOPER_E2E_GITHUB_SANDBOX_REPO`; legacy `LOOPER_E2E_SANDBOX_REPO` remains accepted only as a compatibility alias, and conflicting values fail fast.

Build artifacts go to `dist/` and are gitignored — don't edit generated files.

## Runtime notes

- `looperd` fails fast on invalid config; runtime paths must be writable
- The managed daemon binary lives at `~/.looper/bin/looperd`
- Daemon-managed worktrees live under `~/.looper/worktrees/`, grouped by repo and project
- `looper worktree cleanup` dry-runs Looper-managed worktree cleanup; `--confirm` removes eligible clean terminal worktrees without deleting branches
- When `notifications.osascript.enabled=true`, `osascript` must resolve on startup
- Automation is poll-driven by default — keep `looperd` running and provider credentials available; GitHub projects require `gh`, while Forgejo-only installs do not
