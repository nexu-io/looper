# worker-planner-spec-pr-inherit

Public SHA `23dac66e` (#662). Cross-module contract: worker issue discovery now looks up the planner loop for `issue:<repo>:<N>`, inherits `specPath`, and when the planner PR is still open (`PullRequestSnapshots`) creates the worker as a `pull_request` target so it pushes onto the spec PR. Queue payload, dedupe key, and lock key follow the loop target.

Observed in `internal/worker/runner.go`: `plannerOutputForDiscoveredIssue` and `plannerPullRequestOpenState`. When a snapshot is missing, `known` is false and the worker still inherits `prNumber` (fail-open). That is an ambiguity, not labeled must_fix — the commit message treats snapshot-open as the documented path, and missing snapshots are not clearly a closed PR.

No remaining in-scope defect observed in the production hunks. Tests were added in `internal/worker/runner_test.go`. Implementation review, not spec review.
