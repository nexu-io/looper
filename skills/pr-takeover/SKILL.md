---
name: pr-takeover
description: Use when asked to take over, adopt, or babysit a GitHub pull request until it merges — read review feedback, fix it, resolve threads, dismiss unreasonable change requests, and merge once approved and green. Picks between driving the PR live in this session or handing it to the Looper daemon for unattended background runs, confirming with the user when unclear. Triggers on "take over this PR", "接管这个 PR", "持续修复 review 直到合并", or a Looper takeover bot comment. Works in any coding agent (Claude Code, Codex, opencode, Gemini, …) using gh + git.
---

# PR Takeover

Drive one pull request to merge: continuously read the live review state, fix what reviewers ask for, reply to and resolve threads, dismiss change requests you can justify as wrong, and merge once the PR is approved and all required checks pass — looping until it lands.

There are **two ways to run this**. Pick one in Step 0, then execute it.

| Mode | Who runs the agent | Lifetime | Needs |
| --- | --- | --- | --- |
| **A · Live** (default) | *you*, in this session | until your session ends | `gh` + `git` only |
| **B · Background** | the Looper daemon | survives you leaving; runs for days | Looper installed |

## Step 0 — Choose the mode (confirm with the user only if unclear)

1. If the user already signalled a preference, honor it:
   - "in the background", "while I'm away", "even after I close this", "set and forget" → **Mode B**.
   - "watch it", "do it now", "in here" → **Mode A**.
2. Otherwise default to **Mode A** (zero install, uses your already-authenticated session). Mention in one line that Mode B exists for unattended runs, and switch only if they ask.
3. Choose **Mode B** only if `command -v looper` succeeds *and* the user wants it unattended. If they want unattended but Looper isn't installed, tell them to install it (`https://github.com/nexu-io/looper`) or fall back to Mode A.

Keep this lightweight — ask at most one short question, and only when the user gave no signal.

## Prerequisites

```bash
gh auth status                  # authenticated, with push access to the PR branch
git rev-parse --show-toplevel   # run from inside the repo checkout
```

Identify the PR (explicit `<owner>/<repo>#<num>` wins; else the current branch's):

```bash
gh pr view --json number,headRefName,baseRefName,url
```

If no PR exists for the branch, stop and ask the user to open one.

---

## Mode B — Background (Looper daemon)

Hand the PR to Looper and you're done; it runs the reviewer + fixer loops itself.

```bash
looper takeover <owner>/<repo>#<num> --merge   # --merge enables auto-merge once approved + green
looper takeover list                           # check status later
looper takeover stop <owner>/<repo>#<num>      # stop it
```

Scopes Looper to just this PR (it won't touch other PRs). Note: Looper runs **its own** agent headlessly, so its configured agent vendor must be authenticated for non-interactive use. Report the loop ids it prints, then stop — the rest happens in the background.

---

## Mode A — Live (this session)

Loop until the PR is **merged** or a **hard blocker** needs a human. Each iteration:

### 1. Snapshot the live state (never act on stale data)

```bash
gh pr view <num> --json state,isDraft,mergeable,mergeStateStatus,reviewDecision,statusCheckRollup,headRefOid
```

List **unresolved review threads**:

```bash
gh api graphql -f query='
query($owner:String!,$repo:String!,$num:Int!){
  repository(owner:$owner,name:$repo){ pullRequest(number:$num){
    reviewThreads(first:100){ nodes{ id isResolved
      comments(first:20){ nodes{ author{login} body path line } } } } } }
}' -F owner=<owner> -F repo=<repo> -F num=<num> \
  --jq '.data.repository.pullRequest.reviewThreads.nodes[] | select(.isResolved==false)
        | {id, file:.comments.nodes[0].path, line:.comments.nodes[0].line, body:.comments.nodes[-1].body, author:.comments.nodes[0].author.login}'
```

Pick the iteration's action:
- `state==MERGED` → **done**. `state==CLOSED` → stop, tell the user.
- actionable unresolved threads → **2**.
- none, `reviewDecision==APPROVED`, all required checks `SUCCESS`, `mergeable==MERGEABLE` → **4**.
- else (CI pending / awaiting re-review) → **5**.

### 2. Fix each actionable thread

Make the change (scoped to what the thread asks — don't rewrite unrelated code), run quick tests/linters, then:

```bash
git add -A && git commit -m "fix: <addresses review comment>"
git push        # if rejected non-fast-forward: git pull --rebase && git push. Never --force.
```

### 3. Reply, then resolve — or dismiss

Reply with how you fixed it, then resolve:

```bash
# reply
gh api graphql -f query='mutation($t:ID!,$b:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$t,body:$b}){comment{id}}}' -F t=<threadId> -F b="Fixed in <sha>: <summary>."
# resolve
gh api graphql -f query='mutation($t:ID!){resolveReviewThread(input:{threadId:$t}){thread{isResolved}}}' -F t=<threadId>
```

For a change request you can justify as **incorrect or out of scope**: reply with your reasoning first, then dismiss it (message is mandatory — state *why*):

```bash
# get the CHANGES_REQUESTED review's node id (PRR_…)
gh api graphql -f query='query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){reviews(first:50){nodes{id state author{login}}}}}}' -F o=<owner> -F r=<repo> -F n=<num> --jq '.data.repository.pullRequest.reviews.nodes[]|select(.state=="CHANGES_REQUESTED")'
# dismiss with a reason
gh api graphql -f query='mutation($id:ID!,$m:String!){dismissPullRequestReview(input:{pullRequestReviewId:$id,message:$m}){pullRequestReview{state}}}' -F id=<reviewNodeId> -F m="Dismissing: <why this change request is wrong/out of scope>."
```

### 3b. Re-request the reviewer after pushing a new head

GitHub does **not** auto-re-request a reviewer when you push fixes — their `CHANGES_REQUESTED` stays on the record, the PR keeps showing as blocked even though you addressed everything, and the reviewer gets **no notification** that there's a new head to look at. So after pushing fixes that address a reviewer's change request, re-request them:

```bash
# for each reviewer whose requested changes this push addresses
gh pr edit <num> --add-reviewer <login>
```

This is what flips a stale `CHANGES_REQUESTED` back into an active review request and notifies the reviewer. Skip only if they already re-reviewed the current head.

### 4. Merge when approved and green

Only when `reviewDecision==APPROVED`, every required check is `SUCCESS` (none pending/failing), `mergeable==MERGEABLE`, and not a draft:

```bash
gh pr merge <num> --squash --delete-branch     # use --merge/--rebase to match repo convention
# or: gh pr merge <num> --squash --auto         # let GitHub merge as soon as requirements are met
```

Then report and stop.

### 5. Wait, then re-loop

Poll on an interval instead of spinning. Use your agent's own loop/scheduler if it has one:
- **Claude Code**: `/loop 5m <this instruction>`, or schedule a wake-up; idle between ticks.
- **Codex / opencode / Gemini / others**: re-run this instruction on a timer, or iterate with a `sleep 180` between checks.

---

## Safety rails (apply even in full-auto)

- **Never merge with a failing or pending required check.** Green-and-approved is the only gate. Never `--admin`-bypass.
- **Never force-push**, rewrite others' commits, or delete the base branch.
- **Dismiss only with a written reason.** If you can't articulate why a change request is wrong, treat it as valid and fix it.
- **Stop and ask a human** when: someone says "hold" / "do not merge" / adds a hold label; a fix needs a product or design decision; the same thread reopens after 2 fix attempts; or a merge conflict needs real judgement.
- **Cap no-progress loops**: after N iterations with no new commit and no state change, stop and summarize the blocker.

## More detail

Extended `gh` / GraphQL recipes: `references/github-commands.md` (when installed as a skill), or fetch
`https://raw.githubusercontent.com/nexu-io/looper/main/skills/pr-takeover/references/github-commands.md`.

## One universal prompt (for a bot to post under a PR)

Works in any agent, with or without this skill installed — it just points the agent at this file:

> Take over this PR until it merges — read https://raw.githubusercontent.com/nexu-io/looper/main/skills/pr-takeover/SKILL.md and follow it.

If your agent can't fetch URLs, paste this instead:

> Take over this PR until it merges. Loop: read the open review threads (`gh`), fix what they ask and push, re-request the reviewer after each push (GitHub won't auto-re-request, so the reviewer won't otherwise know there's a new head), reply + resolve each thread, dismiss any change request you can justify as wrong (with a written reason), and merge once it's approved and all required checks are green. Never merge on red/pending checks, never force-push, and stop to ask me if someone says hold or a fix needs a product decision.
