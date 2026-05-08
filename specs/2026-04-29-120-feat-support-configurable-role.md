# Issue 120: Configurable role trigger conditions

Issue: [nexu-io/looper#120](https://github.com/nexu-io/looper/issues/120)
Base branch: `main`

## Problem

Looper's automatic role discovery is currently encoded in each runner and in scheduler wiring instead of being represented as configuration. The effective behavior is:

- `planner`: claim open issues labeled `looper:plan` and assigned to the current GitHub user.
- `worker`: claim open issues labeled `looper:worker-ready` and assigned to the current GitHub user.
- `reviewer`: claim open non-draft PRs where the current GitHub user has a review request, plus existing spec-review follow-up behavior tied to the spec-reviewing label.
- `fixer`: claim open non-draft PRs with actionable review items, limited to PRs authored by the current GitHub user unless `defaults.fixAllPullRequests=true`.

These defaults are useful but hard to adapt. Teams may use different labels, want any-label versus all-label matching, permit draft PR review/fix discovery, route fixer work across all PRs, or disable only one role's automatic discovery while leaving manual commands and queued work available.

## Goals

- Add a top-level `roles` configuration section for `planner`, `reviewer`, `fixer`, and `worker` discovery policy.
- Preserve current default behavior exactly when no new configuration is provided.
- Keep `defaults.fixAllPullRequests` as a backward-compatible legacy field.
- Map `defaults.fixAllPullRequests=true` to `roles.fixer.authorFilter=any` only when `roles.fixer.authorFilter` is not explicitly configured.
- Prefer `roles.fixer.authorFilter` over `defaults.fixAllPullRequests` when both are present.
- Make `autoDiscovery=false` stop only scheduler-driven discovery for that role; it must not block manual commands, direct processing, retries, or already queued work.
- Move hardcoded trigger checks behind explicit per-runner discovery policy structs.
- Provide validation and documentation so invalid policies fail fast at startup/config validation time.

## Non-goals

- Do not change default labels, reviewer/fixer algorithms, queue semantics, or run processing behavior beyond the explicit reviewer/fixer PR eligibility gates controlled by the new policy fields.
- `includeDrafts` and reviewer `requireReviewRequest` are PR eligibility policy fields, not scheduler-only hints: scheduler discovery and any reviewer/fixer processing-time eligibility checks for queued PR work must honor the same effective values so discovered work is not later skipped by stale hardcoded gates. `autoDiscovery=false` remains scheduler-only and must not block processing of already queued work.
- Do not remove or rename `defaults.fixAllPullRequests` in this change.
- Do not introduce per-project role overrides unless they already fall out naturally from existing config layering; this spec targets global role defaults.
- Do not change manual role commands or direct `ProcessNext`/`ProcessClaimedQueueItem` behavior.
- Do not add merge/approval policy configuration.

## Proposed approach

### 1. Add role configuration types

Extend `internal/config/types.go` with a top-level `Roles RoleConfigs` field on `Config` and matching partial config types. Suggested shape:

```go
type LabelMode string

const (
	LabelModeAll LabelMode = "all"
	LabelModeAny LabelMode = "any"
)

type FixerAuthorFilter string

const (
	FixerAuthorFilterCurrentUser FixerAuthorFilter = "current_user"
	FixerAuthorFilterAny         FixerAuthorFilter = "any"
)

type IssueRoleTriggersConfig struct {
	Labels                     []string  `json:"labels"`
	LabelMode                  LabelMode `json:"labelMode"`
	RequireAssigneeCurrentUser bool      `json:"requireAssigneeCurrentUser"`
}

type PullRequestRoleTriggersConfig struct {
	IncludeDrafts        bool `json:"includeDrafts"`
	RequireReviewRequest bool `json:"requireReviewRequest,omitempty"`
}

type PlannerRoleConfig struct {
	AutoDiscovery bool                    `json:"autoDiscovery"`
	Triggers      IssueRoleTriggersConfig `json:"triggers"`
}

type WorkerRoleConfig struct {
	AutoDiscovery bool                    `json:"autoDiscovery"`
	Triggers      IssueRoleTriggersConfig `json:"triggers"`
}

type ReviewerRoleConfig struct {
	AutoDiscovery             bool                          `json:"autoDiscovery"`
	Triggers                  PullRequestRoleTriggersConfig `json:"triggers"`
	IncludeSpecReviewingLabel bool                          `json:"includeSpecReviewingLabel"`
	SpecReviewingLabel        string                        `json:"specReviewingLabel"`
}

type FixerRoleConfig struct {
	AutoDiscovery bool                          `json:"autoDiscovery"`
	Triggers      PullRequestRoleTriggersConfig `json:"triggers"`
	AuthorFilter  FixerAuthorFilter             `json:"authorFilter"`
}

type RoleConfigs struct {
	Planner  PlannerRoleConfig  `json:"planner"`
	Reviewer ReviewerRoleConfig `json:"reviewer"`
	Fixer    FixerRoleConfig    `json:"fixer"`
	Worker   WorkerRoleConfig   `json:"worker"`
}
```

Use pointer fields in partial role config structs so the loader can distinguish omitted values from explicit false/empty values. That distinction is required for compatibility mapping from `defaults.fixAllPullRequests` and for booleans such as `autoDiscovery` and `includeDrafts`.

### 2. Define defaults that mirror current behavior

Update `internal/config/defaults.go` so the default config is equivalent to today's hardcoded discovery:

```json
{
  "roles": {
    "planner": {
      "autoDiscovery": true,
      "triggers": {
        "labels": ["looper:plan"],
        "labelMode": "all",
        "requireAssigneeCurrentUser": true
      }
    },
    "reviewer": {
      "autoDiscovery": true,
      "triggers": {
        "includeDrafts": false,
        "requireReviewRequest": true
      },
      "includeSpecReviewingLabel": true,
      "specReviewingLabel": "looper:spec-reviewing"
    },
    "fixer": {
      "autoDiscovery": true,
      "triggers": {
        "includeDrafts": false
      },
      "authorFilter": "current_user"
    },
    "worker": {
      "autoDiscovery": true,
      "triggers": {
        "labels": ["looper:worker-ready"],
        "labelMode": "all",
        "requireAssigneeCurrentUser": true
      }
    }
  }
}
```

Keep the legacy default `defaults.fixAllPullRequests=false` unchanged. During normalization, if `roles.fixer.authorFilter` is omitted and `defaults.fixAllPullRequests=true` is supplied by file, env, or CLI, set the effective fixer author filter to `any`. If both the legacy and new field are supplied, leave `roles.fixer.authorFilter` as the effective value.

### 3. Normalize and validate role config

Update `internal/config/normalize.go`, `internal/config/load.go`, and `internal/config/validate.go` to merge, detect explicit fields, and validate role policies.

Validation should reject:

- unknown enum values for `labelMode` and `authorFilter`;
- empty strings in label lists;
- duplicate labels after exact string comparison;
- `labelMode` values other than `all` or `any`;
- empty `specReviewingLabel` when `includeSpecReviewingLabel=true`;
- role trigger settings that are structurally invalid even when `autoDiscovery=false`.

Empty issue label lists should be allowed only if the intent is to discover by assignee alone or by all open issues, depending on `requireAssigneeCurrentUser`; document that carefully because it can broaden discovery substantially.

### 4. Introduce runner discovery policy structs

Each runner should accept a typed discovery policy rather than reading config directly or continuing to embed hardcoded constants in discovery checks.

Suggested package-local structs:

- `planner.DiscoveryPolicy`: `AutoDiscovery`, `Labels`, `LabelMode`, `RequireAssigneeCurrentUser`.
- `worker.DiscoveryPolicy`: same issue-trigger fields.
- `reviewer.DiscoveryPolicy`: `AutoDiscovery`, `IncludeDrafts`, `RequireReviewRequest`, `IncludeSpecReviewingLabel`, `SpecReviewingLabel`.
- `fixer.DiscoveryPolicy`: `AutoDiscovery`, `IncludeDrafts`, `AuthorFilter`.

The scheduler/runtime wiring should convert `config.Config.Roles` into these runner policies. Runners should then use policy fields in existing `DiscoverIssues` and `DiscoverPullRequests` methods. This keeps config parsing in the config/runtime layer and lets runner tests exercise behavior without constructing full daemon config.

### 5. Gate scheduler auto-discovery by role

Update `internal/runtime/scheduler.go` so scheduler ticks skip automatic discovery for a role when that role's effective `AutoDiscovery` is false.

Important semantics:

- Only skip discovery calls made by the scheduler tick.
- Do not skip `ProcessNext`, `ProcessClaimedQueueItem`, retry handling, or processing of queue items already claimed/enqueued.
- If a worker implementation does not support issue discovery, preserve today's optional interface behavior.
- Log a debug-level message when auto-discovery is disabled for a role so operators can diagnose why new work is not being claimed.

### 6. Apply policies in each runner

#### Planner and worker issue discovery

Replace hardcoded label/assignee checks with policy-based matching:

- Fetch candidate issues broad enough to evaluate the configured policy. If the GitHub gateway supports only one label/assignee filter per request, use the most selective safe filter and apply the remaining checks locally.
- Support `labelMode=all` by requiring every configured label.
- Support `labelMode=any` by requiring at least one configured label when labels are configured.
- Support `requireAssigneeCurrentUser=false` by not requiring the current GitHub login in the assignee list.
- Preserve current behavior as the default: one configured label, `all`, and current-user assignee required.

#### Reviewer PR discovery

Replace hardcoded draft and review-request checks with policy fields in both scheduler-driven discovery and processing-time reviewer PR eligibility checks:

- `includeDrafts=false` preserves current draft skipping.
- `requireReviewRequest=true` preserves current requirement that the current user is requested.
- If `includeDrafts=true`, reviewer processing must not reject an otherwise eligible queued PR only because it is a draft.
- If `requireReviewRequest=false`, reviewer processing must not reject an otherwise eligible queued PR only because the current user is not requested for review.
- `includeSpecReviewingLabel=true` with the default label preserves the existing spec-reviewing scan/follow-up path.
- If `includeSpecReviewingLabel=false`, skip the additional spec-reviewing-label discovery path while leaving normal review-request discovery intact.

#### Fixer PR discovery

Replace `FixAllPullRequests` plumbing in the fixer runner with the new policy in both scheduler-driven discovery and processing-time fixer PR eligibility checks:

- `authorFilter=current_user` preserves the current default.
- `authorFilter=any` preserves the legacy behavior of `defaults.fixAllPullRequests=true`.
- `includeDrafts=false` preserves current draft skipping.
- If `includeDrafts=true`, fixer processing must not reject an otherwise eligible queued PR only because it is a draft.
- Actionable review item detection, PR lock checks, and non-open PR skipping should remain unchanged.

### 7. Add CLI/env support for common fields

Update `internal/cliapp/config_commands.go` so `looper config get/set/unset/show --source` understands commonly adjusted role settings. At minimum include:

- `roles.<role>.autoDiscovery`
- `roles.planner.triggers.labels`
- `roles.planner.triggers.labelMode`
- `roles.planner.triggers.requireAssigneeCurrentUser`
- `roles.worker.triggers.labels`
- `roles.worker.triggers.labelMode`
- `roles.worker.triggers.requireAssigneeCurrentUser`
- `roles.reviewer.triggers.includeDrafts`
- `roles.reviewer.triggers.requireReviewRequest`
- `roles.reviewer.includeSpecReviewingLabel`
- `roles.reviewer.specReviewingLabel`
- `roles.fixer.triggers.includeDrafts`
- `roles.fixer.authorFilter`

Environment variables must use the exact names below for the supported role fields. Precedence remains `defaults -> config file -> env -> CLI`, including the legacy `LOOPER_FIX_ALL_PULL_REQUESTS`/CLI behavior. CLI `looper config set` values and environment variable values should parse identically.

| Role field / config command path | Environment variable | Accepted value format | Parse behavior |
| --- | --- | --- | --- |
| `roles.planner.autoDiscovery` | `LOOPER_ROLES_PLANNER_AUTO_DISCOVERY` | boolean: `true` or `false` | Trim ASCII whitespace; parse case-insensitively; reject any other value. |
| `roles.planner.triggers.labels` | `LOOPER_ROLES_PLANNER_TRIGGERS_LABELS` | comma-separated string list, for example `looper:plan,team:alpha`; empty string means an empty list | Split on commas, trim ASCII whitespace from each item, reject empty items after trimming unless the whole value is empty, then apply normal duplicate-label validation. |
| `roles.planner.triggers.labelMode` | `LOOPER_ROLES_PLANNER_TRIGGERS_LABEL_MODE` | enum: `all` or `any` | Trim ASCII whitespace; parse case-sensitively as the documented enum; reject any other value. |
| `roles.planner.triggers.requireAssigneeCurrentUser` | `LOOPER_ROLES_PLANNER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER` | boolean: `true` or `false` | Trim ASCII whitespace; parse case-insensitively; reject any other value. |
| `roles.worker.autoDiscovery` | `LOOPER_ROLES_WORKER_AUTO_DISCOVERY` | boolean: `true` or `false` | Trim ASCII whitespace; parse case-insensitively; reject any other value. |
| `roles.worker.triggers.labels` | `LOOPER_ROLES_WORKER_TRIGGERS_LABELS` | comma-separated string list, for example `looper:worker-ready,team:alpha`; empty string means an empty list | Split on commas, trim ASCII whitespace from each item, reject empty items after trimming unless the whole value is empty, then apply normal duplicate-label validation. |
| `roles.worker.triggers.labelMode` | `LOOPER_ROLES_WORKER_TRIGGERS_LABEL_MODE` | enum: `all` or `any` | Trim ASCII whitespace; parse case-sensitively as the documented enum; reject any other value. |
| `roles.worker.triggers.requireAssigneeCurrentUser` | `LOOPER_ROLES_WORKER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER` | boolean: `true` or `false` | Trim ASCII whitespace; parse case-insensitively; reject any other value. |
| `roles.reviewer.autoDiscovery` | `LOOPER_ROLES_REVIEWER_AUTO_DISCOVERY` | boolean: `true` or `false` | Trim ASCII whitespace; parse case-insensitively; reject any other value. |
| `roles.reviewer.triggers.includeDrafts` | `LOOPER_ROLES_REVIEWER_TRIGGERS_INCLUDE_DRAFTS` | boolean: `true` or `false` | Trim ASCII whitespace; parse case-insensitively; reject any other value. |
| `roles.reviewer.triggers.requireReviewRequest` | `LOOPER_ROLES_REVIEWER_TRIGGERS_REQUIRE_REVIEW_REQUEST` | boolean: `true` or `false` | Trim ASCII whitespace; parse case-insensitively; reject any other value. |
| `roles.reviewer.includeSpecReviewingLabel` | `LOOPER_ROLES_REVIEWER_INCLUDE_SPEC_REVIEWING_LABEL` | boolean: `true` or `false` | Trim ASCII whitespace; parse case-insensitively; reject any other value. |
| `roles.reviewer.specReviewingLabel` | `LOOPER_ROLES_REVIEWER_SPEC_REVIEWING_LABEL` | non-empty string | Trim ASCII whitespace; reject empty values when `includeSpecReviewingLabel=true`. |
| `roles.fixer.autoDiscovery` | `LOOPER_ROLES_FIXER_AUTO_DISCOVERY` | boolean: `true` or `false` | Trim ASCII whitespace; parse case-insensitively; reject any other value. |
| `roles.fixer.triggers.includeDrafts` | `LOOPER_ROLES_FIXER_TRIGGERS_INCLUDE_DRAFTS` | boolean: `true` or `false` | Trim ASCII whitespace; parse case-insensitively; reject any other value. |
| `roles.fixer.authorFilter` | `LOOPER_ROLES_FIXER_AUTHOR_FILTER` | enum: `current_user` or `any` | Trim ASCII whitespace; parse case-sensitively as the documented enum; reject any other value. |

### 8. Update documentation and examples

Document the new `roles` section in the config documentation and include:

- the full default role configuration;
- examples for alternate planning/worker labels;
- an example disabling only reviewer auto-discovery;
- an example fixer migration from `defaults.fixAllPullRequests=true` to `roles.fixer.authorFilter=any`;
- an explanation that manual commands and queued work are unaffected by `autoDiscovery=false`.

## Compatibility and migration

Default behavior must be byte-for-byte equivalent at the discovery decision level:

- planner and worker still require the current default label and current-user assignment;
- reviewer still ignores drafts and requires a current-user review request;
- reviewer still includes the spec-reviewing label follow-up path by default;
- fixer still ignores drafts and defaults to current-user-authored PRs;
- `defaults.fixAllPullRequests=true` still broadens fixer discovery to all authors unless the new `roles.fixer.authorFilter` is explicitly configured.

Migration path:

1. Existing config files continue to load without adding `roles`.
2. Operators may keep using `defaults.fixAllPullRequests` temporarily.
3. New config should prefer `roles.fixer.authorFilter`.
4. Documentation should identify `defaults.fixAllPullRequests` as legacy but supported.

## Risks and mitigations

- **Accidentally broader discovery**: validate and document empty label lists and `authorFilter=any`; preserve conservative defaults.
- **Legacy/new field conflicts**: track explicit partial config fields and define precedence clearly: new `roles.fixer.authorFilter` wins.
- **Scheduler gate blocks queued work**: place `autoDiscovery` gates only around discovery calls, not processing calls.
- **GitHub query limitations**: fetch a safe candidate set and apply multi-label/assignee logic locally when the gateway cannot express the policy directly.
- **Runner/config coupling**: convert config to package-local policy structs in runtime wiring rather than importing config into all runners.
- **Spec-reviewing regression**: keep reviewer spec-label behavior enabled by default and cover both enabled/disabled paths in tests.
- **CLI/env sprawl**: start with documented common fields and keep the generic config file as the complete interface.

## Validation plan

Automated tests should cover:

- config defaults produce the same effective role policies as current behavior;
- partial config merging for every role preserves omitted default fields;
- validation rejects invalid enum values, empty labels, duplicate labels, and missing spec-reviewing label when enabled;
- `defaults.fixAllPullRequests=true` maps to fixer `authorFilter=any` when the new field is omitted;
- explicit `roles.fixer.authorFilter=current_user` wins over legacy `defaults.fixAllPullRequests=true`;
- `autoDiscovery=false` prevents scheduler discovery calls for each role while queued item processing still runs;
- planner and worker issue matching for all labels, any labels, no assignee requirement, and default behavior;
- reviewer draft inclusion, review-request requirement, and spec-reviewing-label inclusion/exclusion in both discovery and processing-time PR eligibility gates;
- fixer draft inclusion in both discovery and processing-time PR eligibility gates, plus both author filters while preserving actionable-item and lock checks;
- CLI config get/set/unset behavior for newly registered role fields.

Repository-level verification should use the standard Go-first command set:

```sh
go test ./...
go vet ./...
go build ./...
```

## Implementation checklist

- [ ] Add `roles` config and partial config models.
- [ ] Add defaults that exactly preserve existing discovery behavior.
- [ ] Normalize legacy `defaults.fixAllPullRequests` into fixer author policy when needed.
- [ ] Validate role trigger and enum values.
- [ ] Add runner discovery policy structs.
- [ ] Wire effective config policies into planner, reviewer, fixer, and worker runners.
- [ ] Add scheduler `autoDiscovery` gates for each role.
- [ ] Replace hardcoded planner/worker issue label and assignee checks with policy matching.
- [ ] Replace hardcoded reviewer draft/review-request/spec-label checks with policy matching.
- [ ] Ensure reviewer processing-time PR eligibility gates honor `includeDrafts` and `requireReviewRequest`.
- [ ] Replace fixer `FixAllPullRequests` discovery filtering with `authorFilter` policy.
- [ ] Ensure fixer processing-time PR eligibility gates honor `includeDrafts`.
- [ ] Register common role config fields in CLI/env handling.
- [ ] Register documented role environment variables with deterministic parsing for booleans, enums, and label lists.
- [ ] Update docs with defaults, examples, and migration notes.
- [ ] Add unit tests and run `go test ./...`, `go vet ./...`, and `go build ./...`.
