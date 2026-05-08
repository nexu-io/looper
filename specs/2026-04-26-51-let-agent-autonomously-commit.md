# Issue 51: Agent-managed commit, push, and PR lifecycle

Issue: [nexu-io/looper#51](https://github.com/nexu-io/looper/issues/51)  
Base branch: `main`

## Problem

Looper currently splits implementation-run completion between two ownership models:

- `looperd` can programmatically commit, push, and create PRs as part of managed flows.
- Agent prompts can ask the agent to perform some of the same git/GitHub operations directly.

That mixed ownership makes worker, fixer, and planner behavior depend on runner details and prompt conventions instead of a first-class lifecycle policy. It also means the actor with the best context for commit messages and PR descriptions—the implementation agent—does not consistently own the final implementation lifecycle.

The desired model is agent-owned by default: for eligible implementation runs, the agent should inspect repo state, commit, push, open or adopt a PR, and keep that PR synchronized as work evolves. `looperd` remains responsible for deterministic completion semantics by reconciling the repo and GitHub state after the agent exits and filling in any missing lifecycle steps.

## Goals

- Support an explicit agent-managed git/PR lifecycle mode for `worker`, `fixer`, and `planner` runs.
- Treat agent commit, push, PR creation, PR adoption, and PR synchronization as structured behavior rather than prompt-only side effects.
- Persist structured lifecycle metadata from both agent-completed and fallback-completed actions.
- Deduplicate PR creation by adopting an existing open PR for the branch when present.
- Make retries and resumes safe after partial success, including cases where commit, push, or PR creation succeeded before a later failure.
- Preserve existing reviewer, label, and issue-closing-reference behavior.
- Keep `looperd` as a safety net that programmatically commits, pushes, and creates or adopts the PR when the agent leaves the run incomplete.

## Non-goals

- Do not change review policy or merge policy.
- Do not add automatic merge behavior.
- Do not broaden the work beyond implementation lifecycle ownership for worker, fixer, and planner runs.

## Proposed approach

### 1. Add a first-class lifecycle policy

Introduce a policy such as `agent_managed_with_fallback` for implementation-capable runs. The policy should be represented in configuration and persisted with the run/checkpoint so retries can resume with the same semantics.

The policy should define:

- whether the agent is expected to commit;
- whether the agent is expected to push;
- whether the agent is expected to create or adopt a PR;
- whether `looperd` fallback is allowed or required when state is incomplete;
- which runner types may use the mode (`worker`, `fixer`, `planner`).

Default rollout should be conservative: make the mode explicit and documented before making it the global default. If the project already has per-runner strategy configuration, map this policy into that configuration instead of introducing a second independent switch.

### 2. Define the agent result contract

Agents should leave structured output that `looperd` can parse independent of free-form logs. A minimal contract should include:

```json
{
  "git_pr_lifecycle": {
    "branch": "looper/...",
    "commit_shas": ["..."],
    "pushed": true,
    "pr_number": 123,
    "pr_url": "https://github.com/nexu-io/looper/pull/123",
    "pr_adopted": false,
    "actions": {
      "commit": "agent",
      "push": "agent",
      "pr": "agent"
    }
  }
}
```

The exact envelope can reuse the existing agent execution result/checkpoint format, but it must distinguish:

- missing vs completed actions;
- agent-completed vs fallback-completed actions;
- newly created PR vs adopted existing PR;
- branch name and commit SHAs known at each checkpoint.

The contract should be append/update-friendly so a later agent pass can add commits or synchronize an existing PR without losing earlier metadata.

### 3. Update prompts and runner permissions around the contract

For eligible worker, fixer, and planner runs, prompts should instruct the agent to:

1. inspect `git status`, relevant diffs, and recent commit style before committing;
2. create accurate commit messages based on actual changes;
3. push the current branch;
4. query for an existing PR for the branch;
5. create a PR only if no branch PR exists;
6. update or reuse the PR when follow-up changes are made;
7. emit the structured lifecycle result.

This should be backed by runner policy and available tools, not merely by prompt text. If a runner cannot safely provide push or PR permissions in a given environment, it should not advertise agent-managed PR ownership for that run.

### 4. Add post-agent reconciliation in `looperd`

After the agent exits, `looperd` should run a deterministic reconciliation phase for runs using the new policy.

Reconciliation should inspect actual state rather than trusting agent output alone:

- working tree status, including tracked and untracked files that are allowed to be committed;
- current branch and upstream tracking state;
- local commits ahead of base/upstream;
- whether the branch has been pushed;
- whether an open PR already exists for the branch;
- labels, reviewers, and closing references expected by the run.

Then it should complete missing steps if policy permits:

- create a fallback commit when commit-worthy changes remain;
- push the branch when commits exist locally but are not on the remote;
- adopt an existing PR for the branch or create one when missing;
- apply required reviewers, labels, and issue references;
- update persisted lifecycle metadata with fallback actions.

Fallback commits and PR bodies should use available structured agent output first, then run metadata and issue/spec context, and finally deterministic fallback templates. They should be recognizable as fallback-generated without obscuring the actual work performed.

### 5. Make retry and resume idempotent

Retry and resume logic should treat git/GitHub operations as state reconciliation, not as blind repeats.

Required semantics:

- If a commit SHA is already recorded and present, do not create a duplicate fallback commit for the same clean state.
- If local commits were already pushed, do not push a different branch or rewrite history.
- If a PR exists for the head branch, adopt it rather than creating another PR.
- If PR creation failed after push, resume from the pushed branch and create/adopt the PR.
- If metadata is missing but external state exists, reconstruct metadata from git and GitHub queries.
- If fallback created any action, persist that fact separately from agent-completed actions.

### 6. Persist lifecycle metadata

Extend checkpoint/loop/run records to store normalized PR lifecycle state. The state should be available both immediately after agent execution and after reconciliation.

Suggested fields:

- lifecycle policy and policy version;
- branch name and base branch;
- commit SHAs known for the run;
- pushed branch/ref state;
- PR number and URL;
- PR adoption/creation status;
- per-action completion source: `agent`, `fallback`, or `none`;
- reconciliation attempts and last reconciliation error;
- timestamps for agent result ingestion and fallback reconciliation.

This metadata can start in checkpoint JSON if that is the lowest-risk path, but the shape should be stable enough to promote to typed storage or queryable columns later.

### 7. Preserve labels, reviewers, and closing references

The new lifecycle owner should not bypass existing GitHub conventions. PR creation or adoption must still ensure:

- configured reviewers are requested;
- configured labels are applied;
- issue-closing references are present in the PR body or another configured location;
- existing PR metadata is not destructively overwritten when adopting a PR.

For adopted PRs, prefer additive synchronization: add missing labels/reviewers/references, but do not replace a human-edited PR body unless policy explicitly allows body synchronization.

## Runner-specific notes

### Worker

Worker runs should be the primary target for full agent-managed commit/push/PR ownership. The agent owns the implementation branch lifecycle, while `looperd` reconciles missing commit, push, and PR state before finalizing the run.

### Fixer

Fixer runs usually operate on an existing PR branch. In agent-managed mode, the agent should commit and push fixes to that branch and emit updated commit metadata. PR creation should normally be adoption-only unless fixer is allowed to recover from a missing PR for the branch.

### Planner

Planner runs should be able to create a spec commit, push the planning branch, and open or adopt the spec PR. Planner permissions may be narrower than worker permissions, but the structured lifecycle contract should be the same wherever possible.

## Risks and mitigations

- **Duplicate commits on retry**: mitigate by comparing working tree state, recorded SHAs, and branch history before fallback commits.
- **Duplicate PRs**: mitigate by always querying PRs by head branch before creation and persisting adopted PR metadata.
- **Prompt-only drift**: mitigate by backing the behavior with an explicit policy, result schema, and reconciliation phase.
- **Unsafe fallback commits**: mitigate by preserving existing secret/file exclusion rules and requiring deterministic status inspection before committing.
- **Overwriting human PR edits**: mitigate by making adopted PR synchronization additive unless configured otherwise.
- **Runner permission mismatch**: mitigate by validating git/GitHub capabilities at run start and disabling agent-managed ownership when required tools or credentials are unavailable.
- **Inconsistent metadata after partial failures**: mitigate by reconstructing state from git and GitHub during reconciliation and recording reconciliation errors explicitly.

## Validation plan

Automated tests should cover:

- worker success path where the agent commits, pushes, creates a PR, and emits structured metadata;
- fixer success path where the agent commits and pushes to an existing PR branch;
- planner success path where the agent creates a spec commit and spec PR;
- fallback commit creation when tracked commit-worthy changes remain after agent exit;
- fallback push when local commits exist but the branch is not pushed;
- fallback PR creation when no PR exists for the pushed branch;
- existing PR adoption without duplicate PR creation;
- partial success resume: commit only, commit plus push, and push plus failed PR creation;
- retry behavior that does not duplicate commits or PRs;
- persistence of agent-completed vs fallback-completed action sources;
- preservation of reviewers, labels, and issue-closing references.

Manual or integration validation should include a dry-run repository flow for each runner type using `gh` and `git` credentials comparable to production use.

Repository-level verification remains the standard Go-first command set:

```sh
go test ./...
go vet ./...
go build ./...
```

## Implementation checklist

- [ ] Add lifecycle policy/configuration for agent-managed git/PR with fallback.
- [ ] Define and parse structured agent lifecycle output.
- [ ] Update worker prompts and execution flow for agent-managed lifecycle mode.
- [ ] Update fixer prompts and execution flow for agent-managed lifecycle mode.
- [ ] Update planner prompts and execution flow for agent-managed lifecycle mode.
- [ ] Add reconciliation phase after agent execution.
- [ ] Add PR-by-branch discovery and adoption behavior where missing.
- [ ] Persist normalized lifecycle metadata in checkpoints/loop records.
- [ ] Make retry/resume idempotent for commit, push, and PR operations.
- [ ] Preserve reviewer, label, and closing-reference application on created and adopted PRs.
- [ ] Document configuration, runner eligibility, and fallback triggers.
- [ ] Add tests for success, partial success, fallback, retry, and duplicate-PR scenarios.
