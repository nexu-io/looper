# Sweeper role: auto-close stale, fixed, unrelated, and abandoned issues/PRs

Issue: TBD
Base branch: `main`

## Problem

Open-source repos and active project trackers accumulate issues and PRs that should not stay open:

- **Stale** issues/PRs with no activity for an extended period.
- **Already-fixed** issues that were resolved by a merged PR but never linked with a `Closes #N` reference, so GitHub never auto-closed them.
- **Unrelated / out-of-scope** issues (spam, off-topic, wrong repo, support questions in a bug tracker).
- **Superseded / duplicate** issues or PRs replaced by a newer one.
- **Abandoned PRs** where the author stopped responding to review, the branch has unresolvable conflicts, or a draft has sat untouched for weeks.

Today, Looper has four roles — `planner`, `worker`, `reviewer`, `fixer` — that all *create* or *advance* work. Nothing in the system is responsible for *retiring* work. Maintainers do this manually, infrequently, and inconsistently. Bots like `actions/stale` cover only the time-based slice and cannot reason about whether an issue is already fixed, unrelated, or duplicated.

A new `sweeper` role should own this lifecycle: scan open issues/PRs on a schedule, classify each candidate, post a transparent warning comment with rationale, and close the item after a grace period if nobody objects.

## Goals

- Add a first-class `sweeper` role alongside `planner`, `worker`, `reviewer`, `fixer`.
- Detect five close-worthy categories: `stale`, `already_fixed`, `unrelated`, `superseded`, `abandoned_pr`, plus a non-mutating `route_security` outcome that quarantines the item without closing or commenting.
- Use a two-phase **warn → grace-period → close** lifecycle so humans always have a chance to push back before anything is closed.
- Strictly separate a **propose lane** (read-only classification, never mutates GitHub) from an **apply lane** (the only path that comments / labels / closes), and require the apply lane to re-fetch live GitHub state immediately before every mutation.
- Make every close auditable: every close action ties back to a prior warning comment by the same actor, with a rationale, and is mirrored to a durable per-item markdown report in addition to structured queue metadata.
- Be conservative by default: small batch sizes, opt-in per project, configurable thresholds, dry-run mode.
- Honor existing scheduler/queue/runner architecture — sweeper looks like every other role to `looperd`.
- Add `CloseIssue` and `ClosePullRequest` to `internal/infra/github/gateway.go` so closing is a typed, testable gateway operation, not a free-form shell call.
- Preserve existing roles' behavior exactly. No discovery, processing, or queue semantics for `planner`/`reviewer`/`fixer`/`worker` change.

## Non-goals

- Do **not** reopen, merge, rebase, or modify code/branches. Sweeper only comments, labels, and closes.
- Do **not** delete or hide content. Closed issues/PRs remain visible.
- Do **not** override human decisions. Once a maintainer comments on, labels, or interacts with a candidate after the warning, sweeper steps back.
- Do **not** close items the agent is uncertain about. Low confidence → no action.
- Do **not** close, comment on, or label items the classifier flags as security-class. Those are routed to a quarantine outcome and surfaced for human handling only.
- Do **not** close locked issues/PRs, items pinned by GitHub, or items whose author is a current maintainer/collaborator on the repo.
- Do **not** close an item that is paired with the other side of an open issue/PR link (e.g. an issue with an open PR that references it via `Closes #N`, or a PR whose linked issue is still actively being worked).
- Do **not** mutate GitHub from the propose lane under any circumstance — propose is strictly read-only.
- Do **not** introduce per-issue config; sweeper is configured at the project/role level only.
- Do **not** auto-merge anything. That is out of scope and explicitly forbidden.
- Do **not** change `defaults.fixAllPullRequests`, label conventions, or other roles' triggers.

## Proposed approach

### 1. Add `sweeper` as a registered role

Sweeper lands as a peer of the existing four roles, following the patterns established by issue 120 (configurable role triggers).

- Create `internal/sweeper/` with `runner.go`, `runner_test.go`, `doc.go`, mirroring `internal/planner/` layout.
- Add `"sweeper"` to the role allowlist in `internal/config/validate.go` (alongside `planner`, `reviewer`, `fixer`, `worker`).
- Extend `internal/config/project_roles.go` so projects can enable/disable sweeper per project, identical to other roles.
- Extend `RoleConfigs` in `internal/config/types.go`:

```go
type RoleConfigs struct {
    Planner  PlannerRoleConfig  `json:"planner"`
    Reviewer ReviewerRoleConfig `json:"reviewer"`
    Fixer    FixerRoleConfig    `json:"fixer"`
    Worker   WorkerRoleConfig   `json:"worker"`
    Sweeper  SweeperRoleConfig  `json:"sweeper"`
}
```

- Add `SweeperRoleConfig` (see §3 for field definitions).
- Add defaults in `internal/config/defaults.go` with `autoDiscovery=false` so sweeper is **opt-in** at first rollout.

### 2. Two-phase lifecycle (warn → grace → close) and propose/apply lanes

Every close goes through two scheduler ticks separated by a grace period **and** through two strictly separated lanes within each tick. This is the central safety property of the role.

**Propose lane (read-only):**

- Loads the item, builds the prompt, runs the agent, parses the structured decision.
- Never calls `gh` write operations, never posts comments, never applies labels, never closes anything.
- Emits a `SweeperProposal` record (in-memory + durable per-item report) describing the intended action.

**Apply lane (the only mutator — including for `route_security`):**

- Consumes a `SweeperProposal`.
- **Marker pre-check (idempotency).** Before any mutation, the apply lane lists comments on the target and searches for an existing marker UUID matching the proposal's intended marker. If one exists, the apply lane treats the warning as already posted: it skips `AddComment`, reuses the existing comment ID, and idempotently reconciles labels. This protects against crash-after-comment-before-label and double-tick races.
- Re-fetches the item's live state (issue/PR JSON + recent timeline + comment list with markers + reactions on the warning comment) immediately before mutation.
- Computes a drift check (see drift fingerprint below). If drift is detected, the apply step **aborts** the mutation, records `outcome=stale_proposal`, and re-enqueues a fresh propose pass.
- Only on a clean drift check does it call gateway methods (`AddComment`, `UpdateIssueComment`, `AddLabels`, `RemoveLabels`, `CloseIssue`, `ClosePullRequest`).
- **Mutation ordering** (must match across all apply paths):
  - Phase A apply: (1) check marker, (2) post comment if absent, (3) persist comment ID + marker, (4) add `pendingLabel`. If step 2 succeeds but 3 or 4 fails, the next tick's marker pre-check finds the comment and resumes from step 3.
  - Phase B close apply: (1) post final close comment with `close` marker, (2) persist close-comment ID, (3) `CloseIssue`/`ClosePullRequest`, (4) remove `pendingLabel`, (5) add `closedLabel`. Steps 3–5 are individually idempotent (gateway close is no-op on already-closed; label add/remove are idempotent). Recovery re-enters at the first uncompleted step, identified by which side-effects are visible on GitHub.
  - Phase B cancel apply: (1) `UpdateIssueComment` to append cancellation note onto the existing warning, (2) remove `pendingLabel`. Both idempotent.

**Drift fingerprint** (computed at propose, re-verified at apply):

- target `updated_at`,
- target `state` and `state_reason`,
- target `locked` flag,
- label set (sorted),
- assignees set (sorted),
- comment count,
- for PRs: head SHA, base SHA, `mergeable_state`, draft flag,
- for warned items in Phase B: warning comment's `updated_at` (detects human edits) and reaction summary,
- linked-work fingerprint: sorted list of `(referenced_PR_number, state)` pairs from `Closes #N` / `Fixes #N` cross-references.

**Queue model.** Sweeper uses **two** queue item types, both reusing `storage.QueueItemRecord`:

- `sweeper:warn` — covers Phase A. The runner internally executes propose then apply within the same claim. The propose step writes its `SweeperProposal` to `PayloadJSON`; the apply step re-fetches state and runs the marker pre-check + drift check + mutation ordering above. If apply aborts on drift, the queue item is requeued (`Status=retrying`) so the next tick redoes propose+apply with fresh state.
- `sweeper:close` — covers Phase B. Same shape: propose (re-evaluate, decide `close | cancel | extend | route_security`), then apply.

The scheduler dispatches both via the same `Sweeper.ProcessClaimedQueueItem` entry point. Lanes remain logically separate inside the runner — the propose function never touches `gh` write APIs and is the only path that calls the agent.

**Dedupe and lock keys.** Sweeper sets:

- `DedupeKey = "sweeper:" + phase + ":" + repo + "#" + number` so duplicate discovery ticks collapse to one queue item per (phase, target).
- `LockKey = "sweeper:" + repo + "#" + number` so Phase A and Phase B for the same target serialize, preventing concurrent comment/label mutations.

**Phase B cap.** Phase B re-evaluation is capped at `triggers.maxPerTick * 3` per tick (3× the warn cap). This keeps re-evaluation ahead of warning intake without monopolizing scheduler slots (`internal/runtime/scheduler.go:981-988`).

**Global mutation ceilings.** Independent of `maxPerTick`, sweeper enforces hard daily ceilings per repo before any apply mutation:

- `roles.sweeper.limits.maxWarningsPerRepoPerDay` (default `25`)
- `roles.sweeper.limits.maxClosesPerRepoPerDay` (default `25`)
- `roles.sweeper.limits.globalKillSwitch` (default `false`) — when `true`, the apply lane unconditionally aborts every mutation across all projects. Propose continues so audit records still accumulate.

Counters reset at UTC midnight and are read from `PayloadJSON` audit metadata. When a ceiling is hit, the apply lane records `outcome=ceiling_reached` and the queue item is deferred to the next day.

**Phase A — Warn:**

1. *Propose:* Sweeper scans open issues/PRs (see §4) and enqueues `sweeper:warn` items. The runner's propose step builds the fact bundle and runs the agent before the apply step touches GitHub.
2. *Propose:* For each candidate, the agent classifies it into one of the configured outcomes with a confidence score and rationale.
3. *Propose:* If confidence passes the per-category threshold and no "never-close" rule fires (see below), the runner emits an apply step containing the warning comment body, the target labels, and the snapshot fingerprint used for drift detection.
4. *Apply:* The apply lane re-fetches state, verifies the drift fingerprint, then posts a single comment with:
   - the category (e.g. `stale`, `already_fixed`),
   - the rationale (specific evidence — last activity date, the merged PR that fixed it, the duplicate it points at, etc.),
   - the close-by date (now + `gracePeriodDays`),
   - a short instruction for how to keep the item open: "Comment, push, or remove the `looper:sweep-pending` label to cancel.",
   - a stable HTML-comment marker (e.g. `<!-- looper:sweeper:warn id=<uuid> -->`) so future ticks can locate and edit the same comment in place rather than posting duplicates.
5. *Apply:* Sweeper applies a tracking label (default: `looper:sweep-pending`) and persists the warning comment ID, marker UUID, snapshot fingerprint, and decision metadata in the queue/checkpoint record and the per-item markdown report.

**Phase B — Close:**

1. On a later tick (after the grace period elapses), sweeper enqueues `sweeper:close` for every item carrying `looper:sweep-pending` whose warning comment is older than `gracePeriodDays`.
2. *Propose:* Re-evaluates the item. If any of the following happened since the warning, the proposal is `cancel`:
   - any new human comment, commit, push, label change, or assignee change;
   - the warning comment received a thumbs-down reaction;
   - the item received a `looper:sweep-keep` label;
   - the item is now locked, pinned, or assigned to a maintainer;
   - the original close criteria no longer hold (e.g. activity occurred, the duplicate was unmerged, the linked-fixing PR was reverted);
   - any "never-close" rule fires (see list below).
3. *Apply:* On `cancel`, removes `pendingLabel`, edits the warning comment (located via the marker) to append a cancellation note instead of deleting it (preserves audit trail), and persists `cancelled` outcome metadata.
4. *Apply:* On `close`, after a clean drift re-check, sweeper:
   - calls the new `CloseIssue` / `ClosePullRequest` gateway methods with the appropriate `state_reason` (`completed` for `already_fixed`, `not_planned` for everything else),
   - posts a final close comment using the close-comment template (see §5b) referencing the original warning,
   - removes `looper:sweep-pending`, applies a terminal label (e.g. `looper:swept`).

The grace period is configured per category and defaults to **7 days**.

**"Never close" rules (hard gates evaluated in both propose and apply lanes):**

- Item is locked.
- Item is pinned (issues only).
- Item author is a current maintainer/collaborator on the repo.
- Item carries any label in `excludeLabels` (re-checked at apply time, not just discovery).
- Item is referenced as the target of an open PR (`Closes #N` / `Fixes #N`) that is not itself in `abandoned_pr` state.
- For PRs: the PR is the open implementation of an actively-worked issue (issue was updated within the last `inactivityDays / 2` window).
- Item carries any looper-internal label (`looper:plan`, `looper:worker-ready`, `looper:spec-reviewing`, `looper:swept`).
- Item's classification is `route_security` (see §5a).
- The agent's `confidence` is below `category.minConfidence`.

### 3. `SweeperRoleConfig`

Following the partial-config pointer pattern used by issue 120:

```go
type SweeperCategoryConfig struct {
    Enabled               bool `json:"enabled"`
    InactivityDays        int  `json:"inactivityDays,omitempty"`        // for stale/abandoned
    GracePeriodDays       int  `json:"gracePeriodDays"`
    MinConfidence         int  `json:"minConfidence"`                   // 0-100
}

type SweeperTriggersConfig struct {
    IncludeIssues                 bool     `json:"includeIssues"`
    IncludePullRequests           bool     `json:"includePullRequests"`
    IncludeDrafts                 bool     `json:"includeDrafts"`
    ExcludeLabels                 []string `json:"excludeLabels"`               // e.g. ["pinned","keep-open","security"]
    ExcludeAuthors                []string `json:"excludeAuthors"`              // e.g. trusted maintainers
    ExcludeAuthorAssociations     []string `json:"excludeAuthorAssociations"`   // ["OWNER","MEMBER","COLLABORATOR"]
    LooperInternalLabels          []string `json:"looperInternalLabels"`        // never-close labels managed elsewhere in Looper
    ReopenCooldownDays            int      `json:"reopenCooldownDays"`          // default 30
    MaxPerTick                    int      `json:"maxPerTick"`                  // batch cap (warn phase)
}

type SweeperLimitsConfig struct {
    MaxWarningsPerRepoPerDay int  `json:"maxWarningsPerRepoPerDay"` // default 25
    MaxClosesPerRepoPerDay   int  `json:"maxClosesPerRepoPerDay"`   // default 25
    GlobalKillSwitch         bool `json:"globalKillSwitch"`         // default false
}

type SweeperSecurityConfig struct {
    QuarantineLabel      string   `json:"quarantineLabel"`   // default looper:sweeper-route-security
    NotifyAssignees      []string `json:"notifyAssignees"`   // optional: GitHub usernames to assign
}

type SweeperReportingConfig struct {
    DurableReportsDir    string   `json:"durableReportsDir"` // default ~/.looper/sweeper/<repo-slug>/items/<n>.md; "" disables
}

type SweeperLifecycleConfig struct {
    PendingLabel  string `json:"pendingLabel"`  // default looper:sweep-pending
    ClosedLabel   string `json:"closedLabel"`   // default looper:swept
    KeepLabel     string `json:"keepLabel"`     // default looper:sweep-keep
}

type SweeperRoleConfig struct {
    AutoDiscovery     bool                     `json:"autoDiscovery"`
    DryRun            bool                     `json:"dryRun"`
    Triggers          SweeperTriggersConfig    `json:"triggers"`
    Lifecycle         SweeperLifecycleConfig   `json:"lifecycle"`
    Limits            SweeperLimitsConfig      `json:"limits"`
    Categories        struct {
        Stale         SweeperCategoryConfig    `json:"stale"`
        AlreadyFixed  SweeperCategoryConfig    `json:"alreadyFixed"`
        Unrelated     SweeperCategoryConfig    `json:"unrelated"`
        Superseded    SweeperCategoryConfig    `json:"superseded"`
        AbandonedPR   SweeperCategoryConfig    `json:"abandonedPR"`
    } `json:"categories"`
    Security          SweeperSecurityConfig    `json:"security"`
    Reporting         SweeperReportingConfig   `json:"reporting"`
    Instructions      string                   `json:"instructions,omitempty"`
}
```

**v1 deferrals (explicit non-features).** The following were considered and deferred to a follow-up spec to keep v1 shippable:

- *Localization / language mirroring of comments.* v1 posts comments in English. Item-body language detection and mirrored prose are deferred.
- *Low-signal-PR mode.* Heuristic-based PR closing (blank template, docs-only typo, churn, etc.) is too subjective without operator data. Add later, gated behind its own opt-in config block.
- *Extension flow.* Phase B decisions are limited to `close | cancel | route_security`. No `extend` action; if the situation is borderline, the agent emits `cancel` with rationale and a fresh discovery pass will re-warn after another `inactivityDays` window. Removing `extend` eliminates `MaxExtensions` state and a class of unbounded-deferral bugs.
- *Per-category copy override files.* Only `roles.sweeper.instructions` and auto-injected project metadata (project name, default branch, optional `CONTRIBUTING.md` URL) are honored.

**Defaults (conservative, opt-in):**

```json
{
  "sweeper": {
    "autoDiscovery": false,
    "dryRun": true,
    "triggers": {
      "includeIssues": true,
      "includePullRequests": true,
      "includeDrafts": false,
      "excludeLabels": ["pinned", "security", "looper:sweep-keep"],
      "excludeAuthors": [],
      "excludeAuthorAssociations": ["OWNER", "MEMBER", "COLLABORATOR"],
      "looperInternalLabels": ["looper:plan", "looper:worker-ready", "looper:spec-reviewing", "looper:swept"],
      "reopenCooldownDays": 30,
      "maxPerTick": 10
    },
    "lifecycle": {
      "pendingLabel": "looper:sweep-pending",
      "closedLabel":  "looper:swept",
      "keepLabel":    "looper:sweep-keep"
    },
    "limits": {
      "maxWarningsPerRepoPerDay": 25,
      "maxClosesPerRepoPerDay":   25,
      "globalKillSwitch":         false
    },
    "categories": {
      "stale":        {"enabled": true,  "inactivityDays": 90, "gracePeriodDays": 7, "minConfidence": 70},
      "alreadyFixed": {"enabled": true,                          "gracePeriodDays": 7, "minConfidence": 80},
      "unrelated":    {"enabled": false,                         "gracePeriodDays": 7, "minConfidence": 90},
      "superseded":   {"enabled": true,                          "gracePeriodDays": 7, "minConfidence": 85},
      "abandonedPR":  {"enabled": true,  "inactivityDays": 30, "gracePeriodDays": 7, "minConfidence": 75}
    },
    "security": {
      "quarantineLabel": "looper:sweeper-route-security",
      "notifyAssignees": []
    },
    "reporting": {
      "durableReportsDir": ""
    }
  }
}
```

`unrelated` defaults off because it is the most subjective category; operators must opt in. `dryRun=true` defaults on at the role level so first-run behavior is observe-only — operators flip it off explicitly once they trust the classifier. `durableReportsDir` defaults empty (disabled); `PayloadJSON` audit on the queue record is the v1 source of truth.

Validation (in `internal/config/validate.go`) must reject:

- empty/duplicate label values for `lifecycle.pendingLabel`, `lifecycle.closedLabel`, `lifecycle.keepLabel`, `security.quarantineLabel`;
- overlap between `lifecycle.*` labels and `triggers.excludeLabels` (would create permanent self-exclusion);
- non-positive `inactivityDays` when the corresponding category is enabled and uses inactivity;
- non-positive `gracePeriodDays`;
- non-positive `triggers.reopenCooldownDays`;
- `minConfidence` outside `[0, 100]`;
- `maxPerTick` <= 0;
- `limits.maxWarningsPerRepoPerDay` or `limits.maxClosesPerRepoPerDay` < 0 (zero = freeze the lane);
- both `includeIssues=false` and `includePullRequests=false` while sweeper is enabled;
- unknown values in `excludeAuthorAssociations` (must be one of GitHub's `OWNER|MEMBER|COLLABORATOR|CONTRIBUTOR|FIRST_TIME_CONTRIBUTOR|FIRST_TIMER|MANNEQUIN|NONE`).

### 4. Discovery

Sweeper does a **periodic full scan** of open issues and PRs. It does not require an opt-in label per item; instead it uses exclude rules.

`sweeper.DiscoveryInput` and `sweeper.DiscoveryResult` mirror the existing per-runner shape. The sweeper runner exposes:

```go
DiscoverIssues(context.Context, sweeper.DiscoveryInput) (sweeper.DiscoveryResult, error)
DiscoverPullRequests(context.Context, sweeper.DiscoveryInput) (sweeper.DiscoveryResult, error)
DiscoverReconcile(context.Context, sweeper.DiscoveryInput) (sweeper.DiscoveryResult, error)
ProcessNext(context.Context, string) (*sweeper.ProcessResult, error)
ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*sweeper.ProcessResult, error)
```

The scheduler in `internal/runtime/scheduler.go` gains a sweeper branch in its discovery tick (lines 995-1033 region), gated on `roles.sweeper.autoDiscovery && discoveryEnabled(...)`, identical in shape to the four existing role gates.

Discovery walks open issues and open PRs (both via existing `ListOpenIssues` / `ListOpenPullRequests`), then locally filters out anything that:

- carries any label in `triggers.excludeLabels` (matched exact-string, case-sensitive);
- carries any label in `triggers.looperInternalLabels`;
- carries `security.quarantineLabel`;
- was authored by a user in `triggers.excludeAuthors`;
- has `authorAssociation` in `triggers.excludeAuthorAssociations`;
- is a draft when `includeDrafts=false`;
- is already labeled `lifecycle.pendingLabel` (those are evaluated in Phase B instead);
- is already labeled `lifecycle.closedLabel` and somehow still open (defensive skip + warn log);
- was closed by sweeper less than `triggers.reopenCooldownDays` ago (read from queue audit history).

Three queue types are produced per tick:

- **`sweeper:warn`** — Phase A candidates (no `pendingLabel`). Capped at `triggers.maxPerTick`. Each item carries `DedupeKey = "sweeper:warn:" + repo + "#" + number` and `LockKey = "sweeper:" + repo + "#" + number`.
- **`sweeper:close`** — Phase B candidates (items with `pendingLabel` whose warning is older than the relevant `gracePeriodDays`). Capped at `triggers.maxPerTick * 3`. Same dedupe/lock pattern with phase prefix `close`.
- **`sweeper:reconcile`** — items with active proposals whose `pendingLabel` may have been removed manually, or whose warning comment may have been deleted/edited. Runs once per discovery tick over current `outcome=pending` records. Capped at `triggers.maxPerTick`.

All three reuse the existing `storage.QueueItemRecord` shape (per `internal/storage/repositories.go:163-215`). Claim/retry/fail/complete semantics are unchanged. The scheduler routes all three through `Sweeper.ProcessClaimedQueueItem`; the runner branches by `item.Type` internally.

### 5. Agent contract for classification

Sweeper, like other implementation roles, is fundamentally agent-driven for the judgment call. The runner assembles a **structured fact bundle** (see §5b), passes it to the agent, and the agent returns a structured decision whose comment prose must be grounded in those facts.

**Phase A prompt (warn) input — the structured fact bundle:**

- *Item facts:* repo full name, item type (issue/PR), number, URL, title, body (preserving original language), labels, assignees, author + author association (`OWNER`/`MEMBER`/`COLLABORATOR`/`CONTRIBUTOR`/`FIRST_TIME_CONTRIBUTOR`/`NONE`), created/updated timestamps, locked/pinned state.
- *Timeline facts:* last 20 timeline events (comments, label changes, commits if PR, review requests), with `last_human_comment_at`, `last_commit_at`, `last_label_change_at`, `last_assignee_change_at` pre-computed so the agent does not have to scan.
- *Linked-work facts:* for issues — referencing PRs (via `gh issue view --json projectItems,timelineItems` cross-references) with their state and merged_at; for PRs — head branch, base branch, mergeable state, draft state, review state, last review submitted_at, head SHA, conflicts, linked issues from `Closes #N` parsing.
- *Project facts:* project display name, default branch, configured `instructions` block, optional `contributingURL` (auto-detected from `CONTRIBUTING.md` presence at repo root), repo description.
- *Author-relationship facts:* `is_first_time_contributor`, `is_bot` (heuristic: login ends in `[bot]`), `prior_merged_prs_count`, `prior_closed_issues_count`.
- *Language facts:* detected language code of the item body (e.g. `en`, `ja`, `zh`); the agent is instructed to mirror this language for prose unless the project config forces English.
- *Policy facts:* the configured category set with their confidence thresholds; the "never-close" rules already evaluated and any that fired (so the agent never argues against a hard gate); exclusion rules already applied.

**Phase A agent output contract (parsed by runner via the existing completion-marker scheme in `internal/agent/prompt.go`):**

```json
{
  "sweeper_decision": {
    "category": "stale | already_fixed | unrelated | superseded | abandoned_pr | route_security | none",
    "confidence": 0-100,
    "rationale": "human-readable explanation, 1-3 sentences",
    "evidence": {
      "last_activity_at": "2026-01-15T...",
      "last_human_comment_at": "2025-11-02T...",
      "linked_fixing_pr": 1234,           // only for already_fixed
      "linked_fixing_pr_merged_at": "...",
      "duplicate_of": 5678,               // only for superseded
      "merge_conflict": true,             // only for abandoned_pr
      "head_sha": "abc123",               // only for PRs (used for drift check)
      "snapshot_updated_at": "..."        // item updated_at at proposal time (used for drift check)
    },
    "warning_comment": {
      "body": "the exact comment body to post, in English; must include category-required evidence references (see §5b)",
      "addressed_to": "@octocat",
      "tone": "warm | neutral | terse"
    }
  }
}
```

If `category=none` or `confidence < category.minConfidence`, runner records "no action" and completes the queue item.
If `category=route_security`, the **apply lane** (not propose) applies `security.quarantineLabel`, optionally assigns `security.notifyAssignees`, and adds the quarantine label to the runtime exclusion set so future discovery passes skip the item. The apply lane also follows the marker pre-check + drift check rules above. **No comment is posted and no close is performed.** If the item already carries `pendingLabel` from a prior warning, the apply lane additionally removes `pendingLabel` and edits the original warning comment via marker to append a quarantine notice (`This item has been routed to security review; the previous sweeper notice no longer applies.`).
Otherwise the runner validates the comment (see §5b grounding rules), then the apply lane re-fetches state and posts.

**Missing-state reconciliation** (covers the gaps the lifecycle leaves open):

- *Pending label removed manually:* a separate reconciliation discovery pass — `sweeper:reconcile` queue type, run once per scheduler tick — looks at queue records whose `outcome=pending` and re-fetches the target. If `pendingLabel` is no longer present, the runner edits the warning comment via marker to append a "cancellation acknowledged (label removed)" note and persists `outcome=cancelled_by_label_removal`.
- *Warning comment deleted by a human:* on Phase B propose, if the runner cannot locate the marker via the recorded comment ID *or* via comment search, it persists `outcome=cancelled_warning_deleted`, drops `pendingLabel`, and does **not** close.
- *Warning comment edited beyond recognition:* if the marker is still present but the body no longer contains the original close-by date, the runner treats this as cancellation (same path as deletion) — humans editing a warning is an opt-out signal.
- *Item already closed by a human:* Phase B propose re-fetches state; if the item is closed, persists `outcome=already_closed_by_human`, drops `pendingLabel`, no further action.
- *Item reopened after a sweeper close:* the reopened item is treated as a fresh candidate. The `swept` label is left in place as historical signal but is added to the runtime exclusion set for one full grace period (configurable via `triggers.reopenCooldownDays`, default `30`) so sweeper does not immediately re-warn.
- *Item becomes security-class after a warning:* covered above by the `route_security` apply path — pending label removed, warning comment edited to a quarantine notice, quarantine label applied.
- *Label bootstrap:* before any tick that would post a warning or apply a label, the runner ensures `pendingLabel`, `keepLabel`, `closedLabel`, and `security.quarantineLabel` exist on each enabled repo via a one-time-per-process `EnsureLabel(repo, name, color, description)` call (gateway addition). If creation fails (permission denied), sweeper disables itself for that repo this run with a clear log line; it does not retry forever.

**Phase B prompt (close) input includes:**

- everything from Phase A (re-fetched, not cached, so the agent reasons on live state),
- the original warning comment id, marker UUID, body, and timestamp,
- every event since the warning (comments, reactions on the warning comment, label changes, pushes for PRs).

**Phase B agent output contract:**

```json
{
  "sweeper_followup": {
    "action": "close | cancel | route_security",
    "reason": "human-readable explanation",
    "close_comment": {
      "body": "final comment body if action=close, following the close-comment template (§5b) and category-required evidence rules",
      "addressed_to": "@octocat"
    },
    "cancel_comment": {
      "body": "optional comment body if action=cancel — appended to the original warning via in-place edit, not a new comment"
    }
  }
}
```

There is no `extend` action. If the situation is borderline, the agent emits `cancel` with rationale; a fresh discovery pass after another `inactivityDays` window will produce a new warning if conditions still apply. This eliminates unbounded-deferral state.

### 5b. Context-aware comment composition

The warning and close comments must read as *informed by this specific item*, not generic boilerplate. The runner enforces this through three mechanisms.

**(a) Structured fact bundle (above) — the agent never invents facts.** The agent works only from the provided bundle. The runner does not trust prose alone — it validates each generated comment against **category-specific required-evidence rules** (below), not a generic regex. If a comment fails the required-evidence check for its category, the runner downgrades the decision to `none`, persists the failure with the offending body for tuning, and never silently posts a generic comment.

**Required-evidence rules (mechanically verified against `warning_comment.body` and `close_comment.body`):**

| Category | Required references in body |
|---|---|
| `stale` | At least one ISO-8601 date that matches `evidence.last_human_comment_at` or `last_activity_at` (within tolerance), and the literal string for `inactivity_days` |
| `already_fixed` | The string `#<linked_fixing_pr>` and an ISO-8601 date matching `evidence.linked_fixing_pr_merged_at` |
| `superseded` | The string `#<duplicate_of>` |
| `unrelated` | One of: a redirect URL, a `@<handle>` mention of a maintainer, or a phrase from the project's `instructions` block |
| `abandoned_pr` | The string for `evidence.head_sha` (first 7+ hex) and an ISO-8601 date matching `last_commit_at` |

In addition, every body must include the close-by ISO date and the comment marker token. These are mechanical string checks, not regex heuristics. A `@handle` or a backticked `` `label` `` alone never satisfies the evidence rule by itself.

**(b) Per-category copy spine.** Each category has a single default copy spine per item type (no `tone` axis in v1) that the agent fills using bundle facts. Spines live in `internal/sweeper/copy/` as Go strings keyed by `(category, item_type)`. The agent receives the spine as a soft template — it may rewrite for readability, but the runner verifies the resulting body still satisfies the required-evidence rule above and includes the standard "how to keep open" instruction line plus the comment marker.

Example category spines (illustrative, not the final wording):

| Category | Item type | Spine slots |
|---|---|---|
| `stale` | issue | `{addressed_to}`, `{last_human_comment_at}`, `{inactivity_days}`, `{close_by}`, `{keep_open_instructions}` |
| `already_fixed` | issue | `{addressed_to}`, `{linked_fixing_pr}`, `{linked_fixing_pr_merged_at}`, `{verification_hint}`, `{close_by}` |
| `superseded` | issue | `{addressed_to}`, `{duplicate_of}`, `{rationale_one_sentence}`, `{close_by}` |
| `unrelated` | issue | `{addressed_to}`, `{out_of_scope_reason}`, `{redirect_target_url_or_none}`, `{close_by}` |
| `abandoned_pr` | PR | `{addressed_to}`, `{last_commit_at}`, `{head_sha}`, `{review_state}`, `{close_by}`, `{credit_preservation_line}` |

**(c) Per-dimension adaptation rules.** The agent prompt instructs the agent to vary prose along these dimensions, and the runner verifies a few of them mechanically:

- **Per-category copy & evidence:** category-specific spine (above); `evidence` block must populate the fields relevant to the category (mechanically verified).
- **Issue vs PR phrasing:** PR comments mention head branch (by SHA), conflicts, and review state; address the PR author. Issue comments mention reproduction status and last activity; address the reporter. The spine is selected by item type; runner forbids cross-use.
- **Project-level voice & links:** the project's display name appears at most once; `contributingURL` is appended as a footer line *only if configured*; the project's `instructions` block is injected verbatim above the close-by line.
- **Author-relationship aware:** if `is_first_time_contributor`, tone defaults to warm and the comment includes a one-line thank-you and a link to `contributingURL` if set; if `is_bot`, the runner **skips the item entirely** — sweeper does not act on bot-authored items in v1 (no comment, no label, no close); for `OWNER`/`MEMBER`/`COLLABORATOR` authors, sweeper does not act at all (a "never-close" rule, evaluated upstream).
- **Recency & timeline aware:** required-evidence rules above mechanically enforce the right date references per category.
- **Localization:** v1 posts comments in English. Item-body language detection is captured in the fact bundle for future use but is not used to switch comment language.
- **Project-specific instructions block:** `roles.sweeper.instructions` (already in `SweeperRoleConfig`) is passed into the bundle and rendered verbatim immediately before the close-by line. Maintainers use this for project-specific guidance (e.g., "Please open Q&A in Discussions instead.").

**Close-comment template** (used by Phase B `close_comment.body`):

```
{addressed_to}, closing this {issue|pull request} as **{category}**.

{rationale_one_sentence_grounded_in_evidence}

{credit_preservation_line_if_PR_and_has_commits}

If this was wrong, please reopen — your feedback helps us tune the sweeper.
Original notice: {warning_comment_permalink}

—
Looper sweeper · {project_name}{footer_links_if_configured}
<!-- looper:sweeper:close id={uuid} category={category} -->
```

The credit-preservation line for PRs explicitly thanks the author for the work and notes that the commits remain in the branch history, addressing the contributor-attribution concern raised by clawsweeper's closure policy.

**v1 customization surface** is intentionally small:

- `roles.sweeper.instructions` (free-form maintainer preamble, already present).
- Auto-injected project metadata: project name, default branch, `contributingURL` (if `CONTRIBUTING.md` exists at repo root).
- No template override files. No per-category overrides. Future spec can add `roles.sweeper.copyOverrides.<category>.<itemType>` if real demand emerges.

### 6. Extend the GitHub gateway

`internal/infra/github/gateway.go` already provides `UpdateIssueComment` (lines 129-134, 554-557), `AddIssueLabels` / `RemoveIssueLabel` (lines 1316, 1329), `RequestReviewers`, and reaction helpers. Sweeper still needs these new typed operations:

```go
type CloseIssueInput struct {
    Repo        string
    IssueNumber int64
    StateReason string // "completed" or "not_planned"
    CWD         string
}

func (g *Gateway) CloseIssue(ctx context.Context, input CloseIssueInput) error

type ClosePullRequestInput struct {
    Repo     string
    PRNumber int64           // matches existing convention (gateway.go:138, 160, 180, ...)
    CWD      string
}

func (g *Gateway) ClosePullRequest(ctx context.Context, input ClosePullRequestInput) error

type EnsureLabelInput struct {
    Repo        string
    Name        string
    Color       string  // 6-char hex without leading '#'
    Description string
    CWD         string
}

// EnsureLabel is idempotent: creates the label if missing, updates color/description
// if drifted, returns nil if already correct. Used at startup and before each apply
// mutation that depends on a sweeper-managed label.
func (g *Gateway) EnsureLabel(ctx context.Context, input EnsureLabelInput) error
```

Implementation goes through `gh` CLI (`gh issue close --reason ...`, `gh pr close`, `gh label create|edit|view`) consistent with the rest of the gateway. All three methods must:

- be idempotent: closing an already-closed item returns `nil` (after a status check), not an error; `EnsureLabel` is also idempotent;
- never delete branches or comments (sweeper never passes destructive flags); explicitly: `ClosePullRequest` must **not** support `--delete-branch`;
- surface auth failures as the same wrapped error type as other gateway methods so retry/fail logic in the queue layer is unchanged.

Reactions on the warning comment are needed for the cancel-on-thumbs-down rule. Add `ListIssueCommentReactions(ctx, ListIssueCommentReactionsInput) ([]Reaction, error)` to gateway. v1 may also fall back to "any new human comment cancels" if reaction-listing proves rate-limit-heavy in practice, but the contract above is the target.

### 7. Scheduler wiring

`internal/runtime/scheduler.go` changes:

- Add `sweeperScheduler` interface alongside `plannerScheduler`/`workerScheduler`/etc. (currently at lines 28-57).
- Wire a `Sweeper` field into the scheduler `Input` and runtime composition root (around lines 749-920).
- In the discovery tick (around lines 995-1033), add:

```go
if input.Sweeper != nil && discoveryEnabled(input.SweeperDiscoveryEnabled) {
    if cfg.Roles.Sweeper.Triggers.IncludeIssues {
        _, _ = input.Sweeper.DiscoverIssues(ctx, sweeper.DiscoveryInput{ProjectID: project.ID, Repo: repo})
    }
    if cfg.Roles.Sweeper.Triggers.IncludePullRequests {
        _, _ = input.Sweeper.DiscoverPullRequests(ctx, sweeper.DiscoveryInput{ProjectID: project.ID, Repo: repo})
    }
    _, _ = input.Sweeper.DiscoverReconcile(ctx, sweeper.DiscoveryInput{ProjectID: project.ID, Repo: repo})
}
```

- Queue processing (lines 1063-1171) gains a sweeper claim path that calls `Sweeper.ProcessClaimedQueueItem`. All three queue item types (`sweeper:warn`, `sweeper:close`, `sweeper:reconcile`) route through the same processor; the runner branches by `item.Type` internally.

**All Looper integration sites that must register `"sweeper"`** (the council review identified these beyond the role allowlist; spec must land each):

- `internal/config/validate.go:188-191` (role allowlist) and `:374-427` (role-specific validation).
- `internal/config/project_roles.go:18-31` (`ProjectRoleAutoDiscoveryEnabled` and per-role enable/disable accessors).
- `internal/config/load.go:836-906` (config load/merge sites that walk known roles).
- `internal/config/normalize.go:512-576` (config normalization that walks known roles).
- `internal/cliapp/prompt_commands.go:24-26, 62-68, 165-177` (prompt-preview role allowlist and dispatch).
- `internal/cliapp/app.go:178-179, 300-345, 429-470` (role command registration).

Implementation must touch each of these sites; missing any will silently disable sweeper in a subsystem.

### 8. CLI surface

`internal/cliapp/app.go` (around 300-345 where role commands are registered) adds:

- `looper sweep` — manually trigger one sweeper discovery tick + processing pass for the current project (mirrors `looper plan`/`review`/`work`).
- `looper sweep --dry-run` — force `dryRun=true` for this invocation regardless of config; useful for previewing what *would* be warned/closed.
- `looper sweep --kill-switch` (and `--no-kill-switch`) — flip `roles.sweeper.limits.globalKillSwitch` for this project at runtime; persists to config.
- `looper prompt preview --role sweeper --issue <N>` / `--pr <N>` — print the assembled fact bundle and selected spine for a given target. The sweeper preview requires a target (issue or PR number); without one it errors out, because sweeper prompts are item-specific. Extends `internal/cliapp/prompt_commands.go`.

Config commands (`looper config get/set/unset/show`) must register the common sweeper fields:

- `roles.sweeper.autoDiscovery`
- `roles.sweeper.dryRun`
- `roles.sweeper.triggers.includeIssues`
- `roles.sweeper.triggers.includePullRequests`
- `roles.sweeper.triggers.includeDrafts`
- `roles.sweeper.triggers.excludeLabels`
- `roles.sweeper.triggers.excludeAuthors`
- `roles.sweeper.triggers.excludeAuthorAssociations`
- `roles.sweeper.triggers.looperInternalLabels`
- `roles.sweeper.triggers.reopenCooldownDays`
- `roles.sweeper.triggers.maxPerTick`
- `roles.sweeper.lifecycle.pendingLabel`
- `roles.sweeper.lifecycle.closedLabel`
- `roles.sweeper.lifecycle.keepLabel`
- `roles.sweeper.limits.maxWarningsPerRepoPerDay`
- `roles.sweeper.limits.maxClosesPerRepoPerDay`
- `roles.sweeper.limits.globalKillSwitch`
- `roles.sweeper.security.quarantineLabel`
- `roles.sweeper.security.notifyAssignees`
- `roles.sweeper.reporting.durableReportsDir`
- `roles.sweeper.categories.<name>.enabled`
- `roles.sweeper.categories.<name>.inactivityDays`
- `roles.sweeper.categories.<name>.gracePeriodDays`
- `roles.sweeper.categories.<name>.minConfidence`

Environment variables follow the existing `LOOPER_ROLES_*` shape established by issue 120 (e.g. `LOOPER_ROLES_SWEEPER_AUTO_DISCOVERY`, `LOOPER_ROLES_SWEEPER_DRY_RUN`, `LOOPER_ROLES_SWEEPER_TRIGGERS_MAX_PER_TICK`, `LOOPER_ROLES_SWEEPER_LIMITS_GLOBAL_KILL_SWITCH`, etc.). Booleans, integers, and comma-separated lists parse identically to the existing role envs.

### 9. Persistence and audit

Sweeper decisions must be queryable later, both for debugging false positives and for "show me what looper closed last week" reporting.

The audit lives in `QueueItemRecord.PayloadJSON` (no sibling table needed for v1 — `internal/storage/repositories.go:163-215` already exposes `PayloadJSON`, `DedupeKey`, `LockKey`). Sweeper writes a stable JSON structure:

```json
{
  "sweeper": {
    "phase": "warn | close | reconcile",
    "category": "stale | already_fixed | unrelated | superseded | abandoned_pr | route_security | none",
    "confidence": 78,
    "rationale": "...",
    "warning_comment_id": 123456,
    "warning_marker_uuid": "...",
    "warning_posted_at": "2026-05-16T00:00:00Z",
    "close_by": "2026-05-23T00:00:00Z",
    "fingerprint": {
      "updated_at": "...",
      "state": "open",
      "labels": ["bug","triage"],
      "assignees": ["alice"],
      "comment_count": 12,
      "head_sha": "abc1234",
      "linked_work": [{"number": 1234, "state": "merged"}],
      "warning_updated_at": "...",
      "warning_reactions": {"-1": 0, "+1": 0}
    },
    "outcome": "pending | warned | closed | cancelled | cancelled_by_label_removal | cancelled_warning_deleted | already_closed_by_human | stale_proposal | ceiling_reached | route_security | skipped | failed",
    "outcome_reason": "...",
    "dry_run": false
  }
}
```

**All times are UTC** (ISO-8601 with `Z`). `close_by` is formatted in UTC and rendered in user-facing comments as `YYYY-MM-DD UTC` to avoid time-zone ambiguity.

**Daily counters** for `limits.maxWarningsPerRepoPerDay` / `maxClosesPerRepoPerDay` are computed by counting matching audit records with `outcome IN ('warned','closed')` and `warning_posted_at >= <UTC midnight>`; no separate counter table.

This is the audit trail. It powers a future `looper sweep history` command and is the source of truth when reconciling Phase B and the daily ceilings.

### 10. Dry-run behavior

When `dryRun=true` (role-level or per-invocation), the runner performs every classification step and persists the same metadata, but **does not** post comments, apply labels, or call `CloseIssue`/`ClosePullRequest`. Logs and the audit record show `dry_run: true` so operators can see what would have happened.

This is the primary safe-rollout mechanism: enable sweeper with `dryRun=true`, observe a few ticks, then flip to `false`. The global kill switch (`limits.globalKillSwitch`) is the matching emergency-stop after rollout.

## Risks and mitigations

- **Closing something a maintainer wanted to keep.** Mitigations: two-phase lifecycle, grace period, `keepLabel` exclude, cancel on any human activity, dry-run default, conservative confidence thresholds, `unrelated` category off by default, never-close rule for maintainer-authored items.
- **Confidence inflation by the agent.** Mitigations: per-category `minConfidence`; log every decision (including ones below threshold) so operators can tune thresholds with real data; review in `dryRun` mode before trusting the classifier.
- **Comment/label spam.** Mitigations: `maxPerTick` cap; marker-pre-check guarantees one warning comment per item; daily ceilings (`maxWarningsPerRepoPerDay`).
- **Closing already-closed items.** Mitigations: gateway `CloseIssue`/`ClosePullRequest` idempotency; runner re-checks state before acting; `outcome=already_closed_by_human` reconciliation path.
- **Race with humans during grace period.** Mitigations: drift fingerprint covers `updated_at`, labels, assignees, comments, reactions, head SHA, linked-work; apply lane aborts on drift and re-proposes.
- **Crash mid-apply leaves duplicates or inconsistent state.** Mitigations: marker pre-check + documented mutation ordering + idempotent gateway methods. Each apply step is independently safe to retry; recovery re-enters at first uncompleted step identified by visible side-effects on GitHub.
- **Permission gaps and missing labels.** Mitigations: `EnsureLabel` at startup per-repo; sweeper disables itself for that repo on permission failure with a clear log line, does not retry forever.
- **Coupling with future agent-managed lifecycle work (issue 51).** Mitigations: keep sweeper closes inside the typed gateway; do not push close ownership into the agent's tool environment unless/until the agent-managed lifecycle policy formally extends to sweeper.
- **Spec-reviewing/looper-internal items getting swept.** Mitigations: `triggers.looperInternalLabels` excludes them by default; `excludeAuthorAssociations` defaults skip OWNER/MEMBER/COLLABORATOR.
- **Phase B starving other roles.** Mitigations: Phase B cap at `maxPerTick * 3`; sweeper queue items share scheduler slots fairly with other roles.
- **Daily ceiling miscounted on crash.** Mitigations: counters derived from `PayloadJSON` audit records, not in-memory state; survives restarts.
- **Operator wants emergency stop.** Mitigations: `limits.globalKillSwitch` flips all apply mutations off without touching the rest of Looper.

## Validation plan

Automated tests in `internal/sweeper/runner_test.go`, plus config and scheduler tests:

- config defaults are validated; invalid combinations rejected (including label-overlap and unknown author associations);
- partial config merging preserves omitted defaults;
- discovery filters honor `excludeLabels`, `excludeAuthors`, `excludeAuthorAssociations`, `looperInternalLabels`, `security.quarantineLabel`, `includeDrafts`, `pendingLabel`, `closedLabel`, and `reopenCooldownDays`;
- Phase A: each category's classifier produces the right decision shape; below-threshold confidence becomes "no action"; warnings are posted and labels applied;
- **Required-evidence rules (§5b):** `stale` / `already_fixed` / `superseded` / `unrelated` / `abandoned_pr` comments each contain their category's mandatory references; failing comments are downgraded to `none` and not posted;
- **Per-category spine selection:** the right spine is chosen for each `(category, item_type)` pair; cross-use (e.g., PR spine for an issue) is rejected;
- **Author branching:** first-time-contributor items get the warm spine variant with `contributingURL` link when configured; bot-authored items are skipped entirely (no comment, no label, no close); maintainer-authored items skipped upstream;
- **Drift detection:** apply lane aborts and re-enqueues a fresh proposal when any of `{updated_at, state, labels, assignees, comment_count, head SHA, warning comment updated_at, warning reactions, linked_work state}` changed between propose and apply;
- **Marker pre-check / idempotency:** if a warning comment with the proposal's marker UUID already exists on the target, apply skips `AddComment`, reuses the existing ID, and reconciles labels;
- **Crash recovery:**
  - crash after warning comment posted but before `pendingLabel` applied → next tick finds marker, applies label only;
  - crash after `CloseIssue` but before final comment / label cleanup → next tick is a no-op for `CloseIssue` (idempotent) and completes remaining steps;
  - crash after Phase B close-comment posted but before `CloseIssue` → next tick uses marker pre-check to avoid duplicate close-comment and proceeds;
- **Reconciliation:**
  - `pendingLabel` removed by human → `outcome=cancelled_by_label_removal`, warning comment edited via marker;
  - warning comment deleted → `outcome=cancelled_warning_deleted`, label dropped, no close;
  - warning comment edited beyond recognition → same as deletion path;
  - item already closed by human → `outcome=already_closed_by_human`, label dropped;
  - item reopened after sweep → not re-warned for `reopenCooldownDays`;
  - item becomes security-class after warning → `outcome=route_security`, pending label dropped, warning edited to quarantine notice, quarantine label applied;
- **`route_security`:** classification routes through the apply lane (not propose), applies `quarantineLabel`, optionally assigns reviewers, never posts a comment or closes;
- **Hard "never close" gates:** locked, pinned, maintainer-authored, paired-with-open-PR, looper-internal-labeled, quarantine-labeled, and sub-threshold-confidence items are skipped in both lanes;
- **Drift fingerprint** specifically detects: human-edited warning comment, assignee-only changes, reaction-only changes, linked-PR state changes;
- Phase B cancel path: edits the warning comment via marker (does not delete) and persists `cancelled` outcome;
- Phase B close path calls `CloseIssue`/`ClosePullRequest` with the right `state_reason`, posts the close comment, removes `pendingLabel`, applies `closedLabel`;
- No `extend` action is ever produced or accepted (test that runner rejects it);
- `dryRun=true` performs no GitHub mutations but produces full audit records in `PayloadJSON`;
- `triggers.maxPerTick` caps `sweeper:warn` only; `sweeper:close` is capped at `maxPerTick * 3`; `sweeper:reconcile` capped at `maxPerTick`;
- **Daily ceilings:** `maxWarningsPerRepoPerDay` and `maxClosesPerRepoPerDay` block apply mutations once reached; counters reset at UTC midnight; defer to next day with `outcome=ceiling_reached`;
- **Global kill switch:** when `limits.globalKillSwitch=true`, propose runs but apply unconditionally aborts every mutation;
- gateway `CloseIssue`/`ClosePullRequest`/`EnsureLabel` are idempotent;
- `DedupeKey` collapses duplicate discovery enqueues; `LockKey` serializes Phase A and Phase B for the same target;
- queue fairness: many `sweeper:close` items do not starve other roles' queue items;
- scheduler skips sweeper discovery when `autoDiscovery=false`, but still processes already-claimed sweeper queue items;
- per-project override + env var merge for sweeper config works at every integration site listed in §7;
- CLI: `looper sweep`, `looper sweep --dry-run`, `looper sweep --kill-switch`, `looper prompt preview --role sweeper --issue <N>`, and the new `looper config` registrations;
- the bot-author and maintainer-author skip paths do not increment daily ceilings;
- sweeper does not falsely cancel its own warnings on its next tick (own actor's marker comments are recognized).

Repository-level verification uses the standard Go-first command set per AGENTS.md:

```sh
go test ./...
go vet ./...
go build ./...
```

Manual validation (narrow-to-broad rollout, blast-radius first):

1. Enable sweeper on a low-traffic test project with `dryRun=true` and only `categories.stale.enabled=true`. Run for one full grace cycle (default 7 days). Inspect `PayloadJSON` audit records.
2. Sample 20 stale decisions, verify rationale matches reality, tune `minConfidence`. Confirm `EnsureLabel` created the four sweeper-managed labels.
3. Flip `dryRun=false` for `stale` only. Watch the first warn batch, then the first close batch one grace period later. Confirm cancellation paths work by commenting on a pending item, removing the pending label, and editing the warning comment.
4. Enable `already_fixed`, repeat the cycle. Then `superseded`. Then `abandoned_pr`. Leave `unrelated` off until each prior category has produced at least 50 decisions with no false positives in audit review.
5. Verify daily ceilings (`maxWarningsPerRepoPerDay`, `maxClosesPerRepoPerDay`) by temporarily lowering them and confirming `outcome=ceiling_reached` records appear.
6. Verify `globalKillSwitch=true` halts every apply mutation in flight while propose continues.
7. Roll out to wider projects only after one full grace cycle has passed cleanly with all enabled categories.

## Implementation checklist

- [ ] Create `internal/sweeper/` package with `runner.go`, `runner_test.go`, `doc.go`.
- [ ] Create `internal/sweeper/copy/` with one spine per `(category, item_type)` pair and a `Spine(category, itemType)` lookup. Include a warm-variant for `is_first_time_contributor=true`.
- [ ] Create `internal/sweeper/factbundle.go` that builds the structured fact bundle (item, timeline, linked-work, project, author-relationship, policy facts). Language is captured but not used to switch comment language in v1.
- [ ] Add `SweeperRoleConfig` and partial-config types in `internal/config/types.go` (`Triggers`, `Lifecycle`, `Limits`, `Categories`, `Security`, `Reporting`).
- [ ] Add sweeper defaults in `internal/config/defaults.go` per §3 (opt-in, dry-run on, `excludeAuthorAssociations=["OWNER","MEMBER","COLLABORATOR"]`, daily ceilings 25/25, `globalKillSwitch=false`).
- [ ] Add sweeper validation in `internal/config/validate.go` per §3 (label overlap, unknown author associations, non-positive integers, etc.).
- [ ] Register `"sweeper"` at every integration site listed in §7: `internal/config/validate.go:188-191, 374-427`, `internal/config/project_roles.go:18-31`, `internal/config/load.go:836-906`, `internal/config/normalize.go:512-576`, `internal/cliapp/prompt_commands.go:24-26, 62-68, 165-177`, `internal/cliapp/app.go:178-179, 300-345, 429-470`.
- [ ] Add `CloseIssue`, `ClosePullRequest`, `EnsureLabel`, `ListIssueCommentReactions` to `internal/infra/github/gateway.go` with idempotency. (Note: `UpdateIssueComment` already exists at gateway.go:129-134, 554-557 — reuse, do not re-add.)
- [ ] Implement propose lane: read-only fact-bundle build, agent classification, decision parsing, snapshot fingerprint capture; never mutates `gh` write APIs.
- [ ] Implement apply lane: marker pre-check, live re-fetch, drift check against fingerprint, documented mutation ordering with idempotent recovery, then mutation calls.
- [ ] Implement category-specific required-evidence validation per §5b; downgrade-to-`none` on failure; log every failure with the offending body and category for tuning.
- [ ] Implement Phase A discovery → `sweeper:warn` queue items with `DedupeKey` and `LockKey` per §2.
- [ ] Implement Phase B discovery → `sweeper:close` queue items, capped at `triggers.maxPerTick * 3`.
- [ ] Implement reconcile discovery → `sweeper:reconcile` queue items covering label-removed, comment-deleted, comment-edited, already-closed-by-human, item-reopened, and security-after-warn paths.
- [ ] Implement Phase A apply processor: marker pre-check, post comment with marker UUID, persist metadata in `PayloadJSON`, apply `pendingLabel`. Mutation ordering survives crash mid-sequence.
- [ ] Implement Phase B apply processor: re-fetch, drift-check, then `close` (close-comment → CloseIssue/ClosePR → remove pending → add closed) or `cancel` (UpdateIssueComment via marker → remove pending). No `extend` action.
- [ ] Implement `route_security` outcome via the apply lane: apply `security.quarantineLabel`, optionally assign reviewers, drop `pendingLabel` and edit warning to quarantine notice if previously warned, never close.
- [ ] Implement bot-author and maintainer-author skip branches (sweeper takes no action; does not increment daily ceilings).
- [ ] Implement daily ceilings (`maxWarningsPerRepoPerDay`, `maxClosesPerRepoPerDay`) by counting matching `PayloadJSON` audit records since UTC midnight; emit `outcome=ceiling_reached` when hit.
- [ ] Implement `globalKillSwitch`: propose runs, apply unconditionally aborts every mutation.
- [ ] Implement `EnsureLabel` startup pass per enabled repo for the four sweeper-managed labels; disable sweeper for that repo on permission failure with a clear log line.
- [ ] Implement `reopenCooldownDays`: discovery skips items whose most recent sweeper close is younger than the cooldown.
- [ ] Wire sweeper into scheduler (`internal/runtime/scheduler.go`) discovery tick and queue processing for all three queue types (`sweeper:warn`, `sweeper:close`, `sweeper:reconcile`).
- [ ] Add `looper sweep`, `looper sweep --dry-run`, `looper sweep --kill-switch` / `--no-kill-switch` commands.
- [ ] Extend `looper prompt preview --role sweeper --issue <N>` / `--pr <N>` to print the assembled fact bundle and selected spine. Error if neither flag is supplied (sweeper prompts are item-specific).
- [ ] Register sweeper config fields in `internal/cliapp/config_commands.go` and matching `LOOPER_ROLES_SWEEPER_*` env vars (full list in §8).
- [ ] Persist sweeper decision metadata in `QueueItemRecord.PayloadJSON` (UTC timestamps; `outcome` enum per §9). Optional per-item markdown reports under `reporting.durableReportsDir` when configured.
- [ ] Add tests covering: required-evidence rules, spine selection, author branching, drift detection, marker pre-check, all crash-recovery scenarios, all reconciliation scenarios, `route_security`, daily ceilings, `globalKillSwitch`, `reopenCooldownDays`, dedupe/lock keys, queue fairness, `EnsureLabel` permission failure, sweeper not falsely cancelling its own warnings.
- [ ] Document sweeper config, defaults, full example, rollout guide (stale-only → all categories), comment composition model, and the warn → grace → close lifecycle.
- [ ] Run `go test ./...`, `go vet ./...`, `go build ./...`.
