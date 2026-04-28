# Looper Quick User Guide

This guide is for everyday users. It focuses on how to use `planner`, `reviewer`, `fixer`, and `worker`, and how they interact with GitHub issues and PRs.

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

Also make sure:

- `looperd` is running
- your local repo can `git fetch` and `git push`
- `gh` is authenticated with the target GitHub account
- `config.agent.vendor` is set (for example via `looper bootstrap --agent-vendor opencode`)

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
| `planner` | Generates a spec from an issue and opens a spec PR | `looper plan --project <id> --issue <num>` |
| `reviewer` | Reviews a PR or spec PR and publishes GitHub reviews | `looper review <repo>#<pr> [--loop]` or `looper review <pr> [--loop]` from inside the repo |
| `fixer` | Fixes PR issues based on review comments and tries to resolve threads | `looper loop start --type fixer --pr <repo>#<pr>` |
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

## 5. Planner: from issue to spec PR

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

- create a worktree
- write the spec file
- push a spec PR
- add the `looper:spec-reviewing` label to that PR
- request reviewers when appropriate

## 6. Reviewer: review a spec PR or a normal PR

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

## 7. Fixer: repair a PR based on review feedback

The most direct way to use fixer is to start it for a specific PR:

```bash
looper loop start --type fixer --pr owner/repo#42
```

Fixer will:

- read pending review comments and threads on the PR
- create a worktree and apply fixes
- run validation
- push back to the same PR branch
- try to resolve handled review threads

If the PR is still in the spec review phase and the review becomes clean, fixer can also move the labels from:

- `looper:spec-reviewing` → `looper:spec-ready`

In practice, `reviewer` and `fixer` often alternate until the spec PR is ready for `worker`.

## 8. Worker: do the actual implementation

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

### Start directly from a spec

```bash
looper work --project myproj --title "Implement feature" --spec specs/2026-04-23-123-my-feature.md
```

### What happens when worker takes over a `spec-ready` PR

When worker starts against a PR target, it first removes:

- `looper:spec-ready`

That signals the PR has been claimed for implementation.

## 9. How the GitHub label system works

These are the most important labels right now:

| Label | Used on | Meaning |
| --- | --- | --- |
| `looper:plan` | issue | This issue is eligible for planner auto-pickup |
| `looper:spec-reviewing` | PR | This PR is in the spec review phase |
| `looper:spec-ready` | PR | The spec is approved and ready for worker |
| `looper:needs-human` | PR | Reserved for manual intervention cases |

Treat these as stage signals, not just descriptive labels.

## 10. How assign and review-request trigger automation

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

## 11. Common GitHub / PR commands

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

## 16. One important clarification

In the current implementation, "automatic triggering" is closer to:

- the daemon continuously polling GitHub state
- loops discovering targets based on labels, assignees, and review requests

It is not a GitHub webhook-driven instant trigger.

So if you want the automation chain to work reliably, the most important things are:

- keep `looperd` running
- keep `gh` working
- set labels and assignments correctly
