# Looper Quick User Guide

This guide is for everyday users. It focuses on how `coordinator`, `planner`, `reviewer`, `fixer`, and `worker` interact with GitHub issues and PRs.

## 1. Prerequisites

Make sure these work first:

```bash
looper status
looper project list
gh auth status
```

If the project is not registered in Looper yet:

```bash
looper project add /path/to/repo --id myproj --repo owner/repo
```

If webhook mode reports `degraded`, inspect stale GitHub CLI forwarder hooks first:

```bash
looper webhook status
looper webhook cleanup owner/repo
# if the dry run shows stale GitHub CLI hooks:
looper webhook cleanup owner/repo --confirm
```

Also make sure:

- `looperd` is running
- your local repo can `git fetch` and `git push`
- `gh` is authenticated with the target GitHub account
- `config.agent.vendor` is set (for example via `looper bootstrap --agent-vendor opencode`)

## 1a. Local-only vs Routed projects

Looper supports two project modes:

- `network.mode=off` — local-only. Worker claims `looper:worker-ready` Issues assigned to the local GitHub user, Reviewer claims PRs with a review request for the local GitHub user, and `looper:target:*` labels do nothing.
- `network.mode=routed` — multi-Node. `loopernet` receives centralized webhook ingress, fans out wakeups to Nodes, and exposes the Coordinator lease used for fencing.

Routed mode keeps authorities separate:

- GitHub remains the work-intent authority.
- `looper:target:<node_name>` is only the exact-Node authority.
- the lease only decides which Coordinator may mutate GitHub.

That means `loopernet` never becomes the source of truth for Issue admission or PR review assignment, and it must not mutate GitHub directly.

## 1b. Routed setup and recovery

Typical Routed rollout:

1. join each Node to `loopernet` with `looper network join <url> --key ... --name <node>`
2. disable unsupported routed auto-discovery (`planner` and `fixer`) before opting projects into `network.mode=routed`
3. keep Worker and Reviewer identities stable per Node; duplicate GitHub identities are safe only because the exact target label disambiguates which Node may claim
4. restart `looperd` and confirm `looper network status --verbose` shows membership, identity, and lease state

Operator recovery rules:

- if the target label is removed, duplicated, changed, or stale before work starts, Worker/Reviewer will not claim it
- if `looper:worker-ready`, the GitHub assignee, or the review request disappears before processing starts, the queued item becomes unclaimable
- if webhook ingress or SSE wakeups degrade, polling continues as a fallback so Coordinator can repair drift
- if a stale target label remains after lease loss or a partial GitHub mutation, let Coordinator reconciliation repair or remove it before retrying

## 2. Project auto-detection from the current directory

Looper can often infer the target project from your current working directory.

In practice, this means that if you run commands from inside a registered project repo, you can usually omit `--project`.

This works best when:

- your current directory is inside exactly one registered project repo
- that project has a configured GitHub repo mapping

If no project matches the current directory, or multiple projects match, pass `--project` explicitly.

## 3. What each role does

| Role | Purpose | Common entrypoint |
| --- | --- | --- |
| `coordinator` | Proactively triages fresh issues and commits a Disposition with durable labels | runs automatically inside `looperd` |
| `planner` | Generates a spec from an issue and opens a spec PR | `looper plan --project <id> --issue <num>` |
| `reviewer` | Reviews a PR or spec PR and publishes GitHub reviews | `looper review <repo>#<pr> [--loop]` or `looper review <pr> [--loop]` from inside the repo |
| `fixer` | Fixes PR issues based on review comments and tries to resolve threads | `looper fix <repo>#<pr>` |
| `worker` | Implements the actual work from a spec or issue, and can reuse an existing PR | `looper work --issue <num>` or `looper work --project <id> --issue <num>` |

## 4. Recommended flow

### Overview

1. Create a GitHub issue
2. Add the `looper:plan` label
3. Assign the issue to the currently authenticated `gh` user
4. Start `planner` so it creates a spec PR
5. Let `reviewer` review the spec PR
6. Let `fixer` address review comments until the review is clean
7. The PR gets the `looper:spec-ready` label
8. `worker` takes over that PR and continues implementation

This is the smoothest current Looper workflow.

## 5. Coordinator: proactive triage on fresh issues

Coordinator is Looper's intake role. It is proactive, not trigger-driven: on each poll it scans fresh open issues, runs a shallow repository-aware triage pass, then commits a durable Disposition back to GitHub.

### What Coordinator writes

For each fresh issue inside the configured bootstrap window, Coordinator picks one Disposition:

- `valid`
- `out-of-scope`
- `unclear`

It then:

1. clears any prior coordinator-owned labels (`kind/*`, `area/*`, `complexity/*`, `dispatch/*`, `wontfix`, `needs-info`)
2. applies the new labels for the chosen Disposition
3. posts or edits a triage comment marked with `<!-- looper:coordinator:triage -->`
4. applies `triaged` last as the durability commit

The `triaged` label means Coordinator has formed an opinion about the issue. Because that label is written last, the triage action is safe to re-run after a partial failure.

### Current triage outcomes

- `valid` adds one each of `kind/*`, `area/*`, `complexity/*`, and `dispatch/*`
- `out-of-scope` reuses the existing `wontfix` label and leaves the issue open
- `unclear` adds `needs-info` and asks the author for clarification

### Re-triage loop for `needs-info`

If an issue is in the `unclear` state and the original author replies after `needs-info` was applied, Coordinator removes both `needs-info` and `triaged` and immediately re-runs triage in the same poll. That lets the issue move back through triage without requiring the author to know Looper's label vocabulary.

### Cross-role boundary

Coordinator stays out of issues already under Sweeper lifecycle control. It skips issues carrying:

- `roles.sweeper.lifecycle.pendingLabel`
- `roles.sweeper.lifecycle.closedLabel`
- `roles.sweeper.security.quarantineLabel`

Sweeper, in turn, exempts active coordinator-managed issues such as `dispatch/*`, `needs-info`, and `looper:hold`.

### Dispatch after triage

Once an issue is already `triaged` and carries exactly one `dispatch/*` label, Coordinator can hand it off in one of two modes:

- **human-gated** (default)
- **autonomous**

#### Human-gated slash commands

Coordinator watches issue comments for slash commands at the **start of a line**:

- `/plan` → applies the planner trigger label from `roles.planner.triggers.labels[0]`
- `/implement` → applies the worker trigger label from `roles.worker.triggers.labels[0]`

The commenter must either:

- have repository permission `write`, `maintain`, or `admin`, or
- be listed in `roles.coordinator.dispatch.humanGate.allowedUsers`

On success, Coordinator:

1. assigns `roles.coordinator.dispatch.assignTo` when configured
2. applies the derived trigger label as the durability commit
3. reacts 👍 on the slash-command comment

If the trigger label is already present, Coordinator treats the command as an idempotent re-issue and still reacts 👍.

If the issue is missing `triaged` or a matching `dispatch/*`, Coordinator reacts with GitHub's `confused` reaction and posts one short failure comment marked with `<!-- looper:coordinator:dispatch-failure -->`.

#### Autonomous mode

When `roles.coordinator.dispatch.mode = "autonomous"`, Coordinator no longer waits for a slash command. Instead it dispatches after the issue has stayed `triaged` for `roles.coordinator.dispatch.autonomous.delayMinutes`.

Autonomous dispatch still derives the trigger label from Planner or Worker config and still writes that trigger label last.

#### Veto signals

Autonomous dispatch stops immediately when any veto signal is present:

- the `dispatch/*` label is gone
- the global hold label `looper:hold` (or the configured override) is present
- the destination trigger label is already present because a human dispatched manually

`looper:hold` is the operator-facing global hold contract for Coordinator dispatch.

`looper:hold` blocks Coordinator's autonomous Dispatch, but it does not directly block Reviewer's `gh pr merge --auto` call once a PR is already open. If `looper:hold` causes the linked Issue to skip re-Triage, merge-watch's `BranchProtectionChanged` re-Triage action becomes a silent no-op, which is the intended hold semantic.

## 6. Planner: from issue to spec PR

### Start it manually

```bash
looper plan --project myproj --issue 123
```

This creates a `planner` loop targeting that issue.

For `plan`, it is safest to pass `--project` explicitly.

### Auto-discovery conditions

For planner auto-discovery, an issue must:

- have the `looper:plan` label
- be assigned to the current GitHub user
- belong to a repo that maps uniquely to a local project

So the most common GitHub-side trigger is:

1. create an issue
2. add `looper:plan`
3. assign it to yourself

### What planner does

Planner will:

- add the current GitHub user as an issue assignee when the issue is claimed, preserving any existing assignees
- create a worktree
- write the spec file
- push a spec PR
- add the `looper:spec-reviewing` label to that PR
- request reviewers when appropriate

If planner cannot assign the issue in GitHub, it reports a retryable failure rather than continuing with ambiguous ownership.

## 7. Reviewer: review a spec PR or a normal PR

### One-time review

```bash
looper review owner/repo#42
```

If you are already inside the registered repo, this usually also works:

```bash
looper review 42
```

### Continuous review

```bash
looper review owner/repo#42 --loop
```

Use this when new commits are expected to keep landing on the PR.

### Reviewer auto-discovery rules

Reviewer mainly watches two kinds of PRs:

- open PRs where the current GitHub user was requested as a reviewer
- manually-started reviewer loops from this machine, including `looper review owner/repo#42 --loop`

For spec PRs, `looper:spec-reviewing` marks the review phase, but it does not by itself authorize other users' Looper instances to run. Request review from the intended GitHub user to trigger that user's automatic reviewer.

### What happens after reviewer finishes

If reviewer considers the spec review clean, it will:

- remove `looper:spec-reviewing`
- add `looper:spec-ready`

"Clean" means:

- there are no unresolved review threads
- the review decision is not `CHANGES_REQUESTED`

### Reviewer auto-merge

Reviewer can close a Looper code Issue end-to-end without a human pressing Merge.

Prerequisites:

- branch protection on the base branch with required checks
- repo-level **Allow auto-merge** enabled
- repo-level merge strategy enabled for the configured strategy (`squash`, `merge`, or `rebase`)
- linked Issue body includes a `## Acceptance criteria` section

Configuration lives under `roles.reviewer.autoMerge.*`, with the usual `projects[].roles.reviewer.autoMerge.*` overrides for project-specific repos.

When auto-merge is enabled and the PR is in scope, Reviewer verifies every acceptance-criteria checkbox against the diff before it submits APPROVE. The approval body includes a per-criterion evidence section pointing at files and lines. If every criterion passes, Reviewer calls `gh pr merge --auto`; GitHub branch protection remains the authority for whether the PR actually merges.

Comment markers used by this flow:

- `<!-- looper:reviewer:criteria-fail -->` — Reviewer found at least one acceptance criterion without satisfying evidence in the diff and returned the linked Issue to re-Triage
- `<!-- looper:reviewer:automerge-refused -->` — Reviewer approved the PR, but GitHub repo settings or branch protection refused the auto-merge opt-in
- `<!-- looper:coordinator:merge-watch retries=N -->` — Coordinator is watching a merge-pending PR and carrying retry state on the linked Issue

Human override is silent: if someone clicks **Disable auto-merge** on the PR, Looper respects it and does not re-enable auto-merge just because an earlier Reviewer pass opted in.

Auto-merge is not engaged for Spec PRs, PRs whose linked Issue has no `## Acceptance criteria` section, or PRs outside the configured auto-merge scope.

## 8. Fixer: repair a PR based on review feedback

The most direct way to use fixer is to start it for a specific PR:

```bash
looper fix owner/repo#42
```

If you are already inside the registered repo, you can usually use the PR number by itself:

```bash
looper fix 42
```

Use this when you want to force a repair pass on demand before waiting for any automatic fixer trigger.

Fixer will:

- read pending review comments and threads on the PR
- create a worktree and apply fixes
- run validation
- push back to the same PR branch
- after validation and push succeed, try to resolve only the review threads that were both verified by Looper and explicitly confirmed by the fixer agent

If the PR is still in the spec review phase and the review becomes clean, fixer can also move the labels from:

- `looper:spec-reviewing` → `looper:spec-ready`

In practice, `reviewer` and `fixer` often alternate until the spec PR is ready for `worker`.

## 9. Worker: do the actual implementation

### Start from an issue

```bash
looper work --project myproj --issue 123
```

This is the recommended entrypoint.

If you are already inside the target repo, you can usually omit `--project`:

```bash
looper work --issue 123
```

If that issue already has a related planner loop, worker will try to reuse planner output, including:

- `specPath`
- an existing open PR

That means issue → planner → worker can flow through without manually copying the spec path.

When worker claims an issue, it adds the current GitHub user as an assignee and preserves any existing assignees. If GitHub assignment fails, the claim reports a retryable failure instead of silently continuing with ambiguous ownership.

### Start directly from a spec

```bash
looper work --project myproj --title "Implement feature" --spec specs/2026-04-23-123-my-feature.md
```

### What happens when worker takes over a `spec-ready` PR

When worker starts against a PR target, it first removes:

- `looper:spec-ready`

That signals the PR has been claimed for implementation.

## 10. How the GitHub label system works

These are the most important labels right now:

| Label | Used on | Meaning |
| --- | --- | --- |
| `triaged` | issue | Coordinator finished triage and committed a Disposition |
| `needs-info` | issue | Coordinator marked the issue `unclear` and is waiting for the author |
| `dispatch/*` | issue | Coordinator's durable dispatch intent for later hand-off |
| `looper:plan` | issue | This issue is eligible for planner auto-pickup |
| `looper:spec-reviewing` | PR | This PR is in the spec review phase |
| `looper:spec-ready` | PR | The spec is approved and ready for worker |
| `looper:needs-human` | PR | Reserved for manual intervention cases |

Treat these as stage signals, not just descriptive labels.

## 11. How assign and review-request trigger automation

The two most important assignment-related rules are:

### Planner

For planner auto-discovery, the issue must:

- be assigned to the current GitHub user
- also have `looper:plan`

So:

> Adding only the label is not enough. The issue also needs to be assigned to the right person.

### Reviewer

Reviewer automatically pays attention to:

- PRs where the current GitHub user has a review request
- manual reviewer loops created on this machine

The `looper:spec-reviewing` label is a phase marker; automatic review still requires a review request unless the loop was explicitly started locally.

## 12. Common GitHub / PR commands

Inspect PRs:

```bash
looper pr list
looper pr show owner/repo#42
looper pr status owner/repo#42
```

Create a reviewer task:

```bash
looper review owner/repo#42
looper review owner/repo#42 --loop
looper review 42 --loop
```

Start fixer for an existing PR:

```bash
looper loop start --type fixer --pr owner/repo#42
```

## 12. How to inspect current activity

```bash
looper ps
looper logs 12 --follow
looper jump 12
looper stop 12
```

Typical usage:

- `looper ps`: see which loops are currently running
- `looper logs <id> --follow`: stream logs live
- `looper jump <id>`: print the shell command for the loop's worktree; use `eval "$(looper jump 12)"` to actually change directories, or pass `--print-path` to print just the path
- `looper worktree cleanup`: inspect Looper-managed worktree cleanup candidates without deleting anything; add `--confirm` for one immediate cleanup pass or `--json` for structured output
- `looper stop <id>`: stop an active loop

## 13. Minimal end-to-end example

### Option A: start from an issue

1. Create GitHub issue `#123`
2. Add the `looper:plan` label
3. Assign it to the current `gh` user
4. Run:

```bash
looper plan --project myproj --issue 123
```

5. Wait for planner to open a spec PR
6. Run reviewer:

```bash
looper review owner/repo#<spec-pr>
```

7. If comments appear, start fixer:

```bash
looper loop start --type fixer --pr owner/repo#<spec-pr>
```

8. Once the PR reaches `looper:spec-ready`, start worker:

```bash
looper work --issue 123
```

### Option B: manage an existing PR directly

```bash
looper review owner/repo#42 --loop
looper loop start --type fixer --pr owner/repo#42
```

This is useful when you already have a PR and only want Looper to handle the review/fix cycle.

## 14. Quick decision guide

- You have an issue but no spec yet: use `planner`
- You have a PR that needs review: use `reviewer`
- A PR already has review comments to address: use `fixer`
- The spec is ready and implementation should begin: use `worker`

As a rule of thumb:

- inside a registered repo, you can usually omit `--project` for `review` and `work`
- use `--project` when you are outside the repo, or when Looper cannot infer the project uniquely
- for `plan`, prefer passing `--project`

## 15. Authentication

Looper uses `gh` for GitHub access, so `gh auth status` should succeed before you start planner / reviewer / fixer / worker workflows.

If the daemon is configured with `server.authMode=local-token`, the CLI also needs a matching local token. In that setup, export `LOOPER_TOKEN` before running CLI commands.

Example:

```bash
export LOOPER_TOKEN=replace-me
looper status
```

This is separate from GitHub authentication.

## 16. Webhook delivery status

When webhook mode is enabled, `looper webhook status --json` reports the active delivery mode.

In `gh-forward` mode it reports each local `gh webhook forward` subprocess.

- `adopted=true` means this daemon boot safely reattached to a forwarder spawned by a previous boot. Adopted processes do not have stdout/stderr tails because the new daemon did not create their pipes.
- `latched=true` means `gh webhook forward` exited with a terminal error and Looper will not respawn-loop it. `latchReason` contains the matched error and remediation. For `Hook already exists`, delete the conflicting GitHub webhook or fix `gh` authentication, then restart `looperd`.
- polling remains the correctness fallback while a forwarder is latched or degraded.

In `tunnel` mode Looper creates a GitHub repository webhook and listens on `127.0.0.1:<webhook.listenPort>`. You run your own tunnel to that listener:

```bash
cloudflared tunnel --url http://127.0.0.1:8765
```

Then set `webhook.publicBaseUrl` to the stable HTTPS URL for that tunnel and restart `looperd`. GitHub deliveries go to:

```text
{publicBaseUrl}/webhook/{owner}/{repo}
```

Useful tunnel commands:

```bash
looper webhook status --verbose
looper webhook rotate owner/repo
looper webhook list-orphans
looper webhook delete owner/repo --confirm
```

- `latched=true` on a tunnel hook means GitHub disabled it repeatedly and Looper stopped re-enabling it; polling fallback continues.
- `orphaned=true` means the repo was removed from config or switched away from `tunnel`. Looper retained the hook id locally so explicit deletion is still by id.
- `rotate` changes the per-repo HMAC secret and updates the existing hook by id.
- `delete --confirm` is the only command that removes a Looper-managed tunnel hook from GitHub.

## 17. One important clarification

In the current implementation, "automatic triggering" is closer to:

- the daemon continuously polling GitHub state
- loops discovering targets based on labels, assignees, and review requests

It is not a GitHub webhook-driven instant trigger.

So if you want the automation chain to work reliably, the most important things are:

- keep `looperd` running
- keep `gh` working
- set labels and assignments correctly
