# Legacy-to-canonical mapping inventory

This inventory freezes the forward mapping for the currently supported config surface. It follows `spec.md` as the source of truth for canonical destinations, even where the implementation still accepts compatibility shapes during the migration window.

Status meanings:

- `unchanged`: already on the canonical path/shape
- `moved`: canonical destination changes to a different config path
- `normalized-only`: accepted compatibility input that normalizes to a canonical target without being the preferred documented surface
- `deprecated`: compatibility-only surface that should be replaced in user config/docs

## Top-level roots and major sub-path moves

| Current surface | Canonical target | Status | Notes |
| --- | --- | --- | --- |
| `server` | `server` | unchanged | Canonical top-level root. |
| `daemon` | `daemon` | unchanged | Canonical top-level root. |
| `storage` | `storage` | unchanged | Canonical top-level root. |
| `scheduler` | `scheduler` | unchanged | Canonical top-level root for non-role scheduling policy. |
| `agent` | `agent` | unchanged | Canonical top-level root. |
| `logging` | `logging` | unchanged | Canonical top-level root. |
| `notifications` | `notifications` | unchanged | Canonical top-level root. |
| `disclosure` | `disclosure` | unchanged | Canonical top-level root. |
| `tools` | `tools` | unchanged | Canonical top-level root. |
| `package` | `package` | unchanged | Canonical top-level root. |
| `defaults` | `defaults` | unchanged | Canonical top-level root for user-facing defaults. |
| `instructions` | split by concern | normalized-only | `instructions.*` system controls stay top-level; role instruction text maps to `roles.<role>.instructions`; project-local role instruction text maps to `projects[].roles.<role>.instructions`. |
| `roles` | `roles` | unchanged | Canonical root for all role config. |
| `projects` | `projects` | unchanged | Canonical root for project metadata plus supported project overrides. |
| `reviewer` | `roles.reviewer.behavior` | deprecated | Top-level reviewer config is compatibility-only after the refactor. |
| `reviewer.reviewEvents.*` | `roles.reviewer.behavior.reviewEvents.*` | moved | Canonical reviewer behavior target. |
| `reviewer.loop.*` | `roles.reviewer.behavior.loop.*` | moved | Canonical reviewer behavior target. |
| `reviewer.nativeResume.*` | `roles.reviewer.behavior.nativeResume.*` | moved | Canonical reviewer behavior target. |
| `reviewer.threadResolution.*` | `roles.reviewer.behavior.threadResolution.*` | moved | Canonical reviewer behavior target. |
| `reviewer.scope` | `roles.reviewer.behavior.scope` | moved | Canonical reviewer behavior target. |
| `reviewer.publishMode` | `roles.reviewer.behavior.publishMode` | moved | Canonical reviewer behavior target. |
| `roles.reviewer.autoDiscovery` | `roles.reviewer.discovery.autoDiscovery` | moved | Frozen canonical reviewer discovery home from the spec. |
| `roles.reviewer.triggers.*` | `roles.reviewer.discovery.triggers.*` | moved | Frozen canonical reviewer discovery home from the spec. |
| `roles.reviewer.specReview.*` | `roles.reviewer.discovery.specReview.*` | moved | Frozen canonical reviewer discovery home from the spec. |

## Legacy aliases and compatibility sub-paths

| Current surface | Canonical target | Status | Notes |
| --- | --- | --- | --- |
| `defaults.allowAutoApprove` | `roles.reviewer.behavior.reviewEvents.clean=APPROVE` | deprecated | Compatibility alias for reviewer clean approvals when the canonical reviewer event is omitted. |
| `defaults.fixAllPullRequests` | `roles.fixer.triggers.authorFilter=any` | deprecated | Compatibility alias when canonical fixer author filter is omitted. |
| `projects[].path` | `projects[].repoPath` | normalized-only | `path` remains a compatibility alias for the canonical repo path field. |
| `projects[].instructions.<role>` | `projects[].roles.<role>.instructions` | normalized-only | Convenience project instruction map must normalize to the canonical project role instruction target or be removed later. |

## Role-specific roots

| Current surface | Canonical target | Status | Notes |
| --- | --- | --- | --- |
| `roles.planner.*` | `roles.planner.*` | unchanged | Current planner role root remains canonical. |
| `roles.worker.*` | `roles.worker.*` | unchanged | Current worker role root remains canonical. |
| `roles.fixer.*` | `roles.fixer.*` | unchanged | Current fixer role root remains canonical. |
| `roles.reviewer.behavior.*` | `roles.reviewer.behavior.*` | unchanged | Canonical reviewer behavior target. |
| `roles.reviewer.instructions` | `roles.reviewer.instructions` | unchanged | Canonical reviewer instruction target. |

## Project override paths

| Current surface | Canonical target | Status | Notes |
| --- | --- | --- | --- |
| `projects[].id` | `projects[].id` | unchanged | Project metadata. |
| `projects[].name` | `projects[].name` | unchanged | Project metadata. |
| `projects[].repoPath` | `projects[].repoPath` | unchanged | Canonical project metadata path. |
| `projects[].baseBranch` | `projects[].baseBranch` | unchanged | Project metadata. |
| `projects[].worktreeRoot` | `projects[].worktreeRoot` | unchanged | Project metadata. |
| `projects[].roles.planner.*` | `projects[].roles.planner.*` | unchanged | Canonical project role override shape. |
| `projects[].roles.worker.*` | `projects[].roles.worker.*` | unchanged | Canonical project role override shape. |
| `projects[].roles.fixer.*` | `projects[].roles.fixer.*` | unchanged | Canonical project role override shape. |
| `projects[].roles.reviewer.instructions` | `projects[].roles.reviewer.instructions` | unchanged | Canonical project reviewer instruction target. |
| `projects[].roles.reviewer.autoDiscovery` | `projects[].roles.reviewer.discovery.autoDiscovery` | moved | Frozen canonical reviewer discovery home from the spec. |
| `projects[].roles.reviewer.triggers.*` | `projects[].roles.reviewer.discovery.triggers.*` | moved | Frozen canonical reviewer discovery home from the spec. |
| `projects[].roles.reviewer.specReview.*` | `projects[].roles.reviewer.discovery.specReview.*` | moved | Frozen canonical reviewer discovery home from the spec. |
| `projects[].instructions.reviewer` | `projects[].roles.reviewer.instructions` | normalized-only | Convenience project instruction map entry. |
| `projects[].instructions.planner` | `projects[].roles.planner.instructions` | normalized-only | Convenience project instruction map entry. |
| `projects[].instructions.worker` | `projects[].roles.worker.instructions` | normalized-only | Convenience project instruction map entry. |
| `projects[].instructions.fixer` | `projects[].roles.fixer.instructions` | normalized-only | Convenience project instruction map entry. |

## Environment variables

| Current env var | Canonical target | Status | Notes |
| --- | --- | --- | --- |
| `LOOPER_CONFIG` | config source selection | unchanged | Explicit config-file selection; not a config field. |
| `LOOPER_HOST` | `server.host` | unchanged | |
| `LOOPER_PORT` | `server.port` | unchanged | |
| `LOOPER_DB_PATH` | `storage.dbPath` | unchanged | |
| `LOOPER_LOG_DIR` | `daemon.logDir` | unchanged | |
| `LOOPER_DAEMON_MODE` | `daemon.mode` | unchanged | |
| `LOOPER_DAEMON_RESTART_POLICY` | `daemon.restartPolicy` | unchanged | |
| `LOOPER_DAEMON_RESTART_THROTTLE_SECONDS` | `daemon.restartThrottleSeconds` | unchanged | |
| `LOOPER_WORKING_DIRECTORY` | `daemon.workingDirectory` | unchanged | |
| `LOOPER_IN_APP_NOTIFICATIONS` | `notifications.inApp` | unchanged | |
| `LOOPER_OSASCRIPT_ENABLED` | `notifications.osascript.enabled` | unchanged | |
| `LOOPER_AUTO_UPGRADE_ENABLED` | `package.autoUpgradeEnabled` | unchanged | |
| `LOOPER_AGENT_NATIVE_RESUME_ENABLED` | `agent.nativeResume.enabled` | unchanged | |
| `LOOPER_AGENT_TIMEOUTS_PLANNER_SECONDS` | `agent.timeouts.plannerSeconds` | unchanged | |
| `LOOPER_AGENT_TIMEOUTS_WORKER_SECONDS` | `agent.timeouts.workerSeconds` | unchanged | |
| `LOOPER_AGENT_TIMEOUTS_REVIEWER_SECONDS` | `agent.timeouts.reviewerSeconds` | unchanged | |
| `LOOPER_AGENT_TIMEOUTS_FIXER_SECONDS` | `agent.timeouts.fixerSeconds` | unchanged | |
| `LOOPER_AGENT_TIMEOUTS_PLANNER_IDLE_TIMEOUT_SECONDS` | `agent.timeouts.plannerIdleTimeoutSeconds` | unchanged | |
| `LOOPER_AGENT_TIMEOUTS_PLANNER_MAX_RUNTIME_SECONDS` | `agent.timeouts.plannerMaxRuntimeSeconds` | unchanged | |
| `LOOPER_AGENT_TIMEOUTS_WORKER_IDLE_TIMEOUT_SECONDS` | `agent.timeouts.workerIdleTimeoutSeconds` | unchanged | |
| `LOOPER_AGENT_TIMEOUTS_WORKER_MAX_RUNTIME_SECONDS` | `agent.timeouts.workerMaxRuntimeSeconds` | unchanged | |
| `LOOPER_AGENT_TIMEOUTS_REVIEWER_IDLE_TIMEOUT_SECONDS` | `agent.timeouts.reviewerIdleTimeoutSeconds` | unchanged | |
| `LOOPER_AGENT_TIMEOUTS_REVIEWER_MAX_RUNTIME_SECONDS` | `agent.timeouts.reviewerMaxRuntimeSeconds` | unchanged | |
| `LOOPER_AGENT_TIMEOUTS_FIXER_IDLE_TIMEOUT_SECONDS` | `agent.timeouts.fixerIdleTimeoutSeconds` | unchanged | |
| `LOOPER_AGENT_TIMEOUTS_FIXER_MAX_RUNTIME_SECONDS` | `agent.timeouts.fixerMaxRuntimeSeconds` | unchanged | |
| `LOOPER_ALLOW_AUTO_COMMIT` | `defaults.allowAutoCommit` | unchanged | |
| `LOOPER_ALLOW_AUTO_PUSH` | `defaults.allowAutoPush` | unchanged | |
| `LOOPER_ALLOW_AUTO_APPROVE` | `defaults.allowAutoApprove` | deprecated | Legacy alias for reviewer clean approvals. |
| `LOOPER_FIX_ALL_PULL_REQUESTS` | `defaults.fixAllPullRequests` | deprecated | Legacy alias for fixer author filter widening. |
| `LOOPER_REVIEWER_LOOP_ENABLED` | `roles.reviewer.behavior.loop.enabledByDefault` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `LOOPER_REVIEWER_REVIEW_EVENTS_CLEAN` | `roles.reviewer.behavior.reviewEvents.clean` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `LOOPER_REVIEWER_REVIEW_EVENTS_BLOCKING` | `roles.reviewer.behavior.reviewEvents.blocking` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `LOOPER_REVIEWER_QUIET_PERIOD_SECONDS` | `roles.reviewer.behavior.loop.quietPeriodSeconds` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `LOOPER_REVIEWER_MIN_PUBLISH_INTERVAL_SECONDS` | `roles.reviewer.behavior.loop.minPublishIntervalSeconds` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `LOOPER_REVIEWER_MAX_ITERATIONS_PER_PR` | `roles.reviewer.behavior.loop.maxIterationsPerPR` | deprecated | Reviewer filter ignores this legacy loop budget knob. |
| `LOOPER_REVIEWER_MAX_ITERATIONS_PER_HEAD` | `roles.reviewer.behavior.loop.maxIterationsPerHead` | deprecated | Reviewer filter ignores this legacy loop budget knob. |
| `LOOPER_REVIEWER_NATIVE_RESUME_ON_HEAD_CHANGE` | `roles.reviewer.behavior.nativeResume.onHeadChange` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `LOOPER_REVIEWER_NATIVE_RESUME_REREVIEW_PROMPT_ON_HEAD_CHANGE` | `roles.reviewer.behavior.nativeResume.reReviewPromptOnHeadChange` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `LOOPER_REVIEWER_THREAD_RESOLUTION_ENABLED` | `roles.reviewer.behavior.threadResolution.enabled` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `LOOPER_REVIEWER_THREAD_RESOLUTION_MODE` | `roles.reviewer.behavior.threadResolution.mode` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `LOOPER_REVIEWER_THREAD_RESOLUTION_MAX_THREADS_PER_RUN` | `roles.reviewer.behavior.threadResolution.maxThreadsPerRun` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `LOOPER_GIT_PATH` | `tools.gitPath` | unchanged | |
| `LOOPER_GH_PATH` | `tools.ghPath` | unchanged | |
| `LOOPER_LOOPER_PATH` | `tools.looperPath` | unchanged | |
| `LOOPER_OSASCRIPT_PATH` | `tools.osascriptPath` | unchanged | |
| `LOOPER_ROLES_PLANNER_AUTO_DISCOVERY` | `roles.planner.autoDiscovery` | unchanged | |
| `LOOPER_ROLES_PLANNER_TRIGGERS_LABELS` | `roles.planner.triggers.labels` | unchanged | |
| `LOOPER_ROLES_PLANNER_TRIGGERS_LABEL_MODE` | `roles.planner.triggers.labelMode` | unchanged | |
| `LOOPER_ROLES_PLANNER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER` | `roles.planner.triggers.requireAssigneeCurrentUser` | unchanged | |
| `LOOPER_ROLES_WORKER_AUTO_DISCOVERY` | `roles.worker.autoDiscovery` | unchanged | |
| `LOOPER_ROLES_WORKER_TRIGGERS_LABELS` | `roles.worker.triggers.labels` | unchanged | |
| `LOOPER_ROLES_WORKER_TRIGGERS_LABEL_MODE` | `roles.worker.triggers.labelMode` | unchanged | |
| `LOOPER_ROLES_WORKER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER` | `roles.worker.triggers.requireAssigneeCurrentUser` | unchanged | |
| `LOOPER_ROLES_REVIEWER_AUTO_DISCOVERY` | `roles.reviewer.discovery.autoDiscovery` | moved | Current overrideable reviewer discovery field moves under the canonical discovery subgroup. |
| `LOOPER_ROLES_REVIEWER_TRIGGERS_INCLUDE_DRAFTS` | `roles.reviewer.discovery.triggers.includeDrafts` | moved | Current overrideable reviewer discovery field moves under the canonical discovery subgroup. |
| `LOOPER_ROLES_REVIEWER_TRIGGERS_REQUIRE_REVIEW_REQUEST` | `roles.reviewer.discovery.triggers.requireReviewRequest` | moved | Current overrideable reviewer discovery field moves under the canonical discovery subgroup. |
| `LOOPER_ROLES_REVIEWER_TRIGGERS_ENABLE_SELF_REVIEW` | `roles.reviewer.discovery.triggers.enableSelfReview` | moved | Current overrideable reviewer discovery field moves under the canonical discovery subgroup. |
| `LOOPER_ROLES_REVIEWER_TRIGGERS_LABELS` | `roles.reviewer.discovery.triggers.labels` | moved | Current overrideable reviewer discovery field moves under the canonical discovery subgroup. |
| `LOOPER_ROLES_REVIEWER_TRIGGERS_LABEL_MODE` | `roles.reviewer.discovery.triggers.labelMode` | moved | Current overrideable reviewer discovery field moves under the canonical discovery subgroup. |
| `LOOPER_ROLES_REVIEWER_SPEC_REVIEW_INCLUDE_REVIEWING_LABEL` | `roles.reviewer.discovery.specReview.includeReviewingLabel` | moved | Current overrideable reviewer discovery field moves under the canonical discovery subgroup. |
| `LOOPER_ROLES_REVIEWER_SPEC_REVIEW_REVIEWING_LABEL` | `roles.reviewer.discovery.specReview.reviewingLabel` | moved | Current overrideable reviewer discovery field moves under the canonical discovery subgroup. |
| `LOOPER_ROLES_FIXER_AUTO_DISCOVERY` | `roles.fixer.autoDiscovery` | unchanged | |
| `LOOPER_ROLES_FIXER_TRIGGERS_INCLUDE_DRAFTS` | `roles.fixer.triggers.includeDrafts` | unchanged | |
| `LOOPER_ROLES_FIXER_TRIGGERS_LABELS` | `roles.fixer.triggers.labels` | unchanged | |
| `LOOPER_ROLES_FIXER_TRIGGERS_LABEL_MODE` | `roles.fixer.triggers.labelMode` | unchanged | |
| `LOOPER_ROLES_FIXER_TRIGGERS_AUTHOR_FILTER` | `roles.fixer.triggers.authorFilter` | unchanged | Canonical fixer discovery override. |

## CLI flags

| Current CLI flag | Canonical target | Status | Notes |
| --- | --- | --- | --- |
| `--config` | config source selection | unchanged | Explicit config-file selection; not a config field. |
| `--host` | `server.host` | unchanged | |
| `--port` | `server.port` | unchanged | |
| `--db-path` | `storage.dbPath` | unchanged | |
| `--log-dir` | `daemon.logDir` | unchanged | |
| `--daemon-mode` | `daemon.mode` | unchanged | |
| `--daemon-restart-policy` | `daemon.restartPolicy` | unchanged | |
| `--daemon-restart-throttle-seconds` | `daemon.restartThrottleSeconds` | unchanged | |
| `--git-path` | `tools.gitPath` | unchanged | |
| `--gh-path` | `tools.ghPath` | unchanged | |
| `--looper-path` | `tools.looperPath` | unchanged | |
| `--osascript-path` | `tools.osascriptPath` | unchanged | |
| `--planner-agent-timeout-seconds` | `agent.timeouts.plannerSeconds` | unchanged | |
| `--worker-agent-timeout-seconds` | `agent.timeouts.workerSeconds` | unchanged | |
| `--reviewer-agent-timeout-seconds` | `agent.timeouts.reviewerSeconds` | unchanged | |
| `--fixer-agent-timeout-seconds` | `agent.timeouts.fixerSeconds` | unchanged | |
| `--allow-auto-commit` | `defaults.allowAutoCommit` | unchanged | |
| `--allow-auto-push` | `defaults.allowAutoPush` | unchanged | |
| `--allow-auto-approve` | `defaults.allowAutoApprove` | deprecated | Legacy alias for reviewer clean approvals. |
| `--fix-all-pull-requests` | `defaults.fixAllPullRequests` | deprecated | Legacy alias for fixer author filter widening. |
| `--reviewer-loop-enabled` | `roles.reviewer.behavior.loop.enabledByDefault` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `--reviewer-enable-self-review` | `roles.reviewer.discovery.triggers.enableSelfReview` | moved | Reviewer discovery setting retains a legacy flag name today. |
| `--reviewer-clean-review-event` | `roles.reviewer.behavior.reviewEvents.clean` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `--reviewer-blocking-review-event` | `roles.reviewer.behavior.reviewEvents.blocking` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `--reviewer-quiet-period-seconds` | `roles.reviewer.behavior.loop.quietPeriodSeconds` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `--reviewer-min-publish-interval-seconds` | `roles.reviewer.behavior.loop.minPublishIntervalSeconds` | normalized-only | Legacy reviewer top-level name targeting canonical reviewer behavior. |
| `--reviewer-max-iterations-per-pr` | `roles.reviewer.behavior.loop.maxIterationsPerPR` | deprecated | Reviewer filter ignores this legacy loop budget knob. |
| `--reviewer-max-iterations-per-head` | `roles.reviewer.behavior.loop.maxIterationsPerHead` | deprecated | Reviewer filter ignores this legacy loop budget knob. |
| `--no-auto-upgrade` | `package.autoUpgradeEnabled=false` | normalized-only | Inverted compatibility flag targeting the canonical package setting. |
| `--no-custom-instructions` | `instructions.enabled=false` | normalized-only | Inverted compatibility flag targeting the canonical instruction-system setting. |

## Representative parity coverage

The repository-level parity checks for this inventory live in `internal/config/config_test.go` and cover representative equivalents for:

- legacy top-level `reviewer.*` vs canonical `roles.reviewer.behavior.*`
- legacy `defaults.allowAutoApprove` vs canonical reviewer clean-review behavior
- legacy `defaults.fixAllPullRequests` vs canonical fixer author filter
- legacy `projects[].path` vs canonical `projects[].repoPath`
- legacy `LOOPER_FIX_ALL_PULL_REQUESTS` vs canonical `LOOPER_ROLES_FIXER_TRIGGERS_AUTHOR_FILTER`
