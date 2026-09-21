# fixer-pr-scope-contract

Public SHA `4f4fcb81` (#629). P0/P1 bugfix: keep fixer repairs within PR scope. Production hunks in `internal/fixer/runner.go` and `internal/fixer/hitl.go` rewrite `fixerRepairScopeInstruction` so repository rules and documented PR intent outrank listed review items. Out-of-scope items must be declined; uncertain items stop before edits when HITL is on. `.looper/dismiss.json` is no longer the decline path for out-of-scope comments.

Regression coverage is present (`internal/fixer/runner_test.go`, `hitl_test.go`). No remaining in-scope defect observed. Implementation review of a scope-contract fix — pair with `forgejo-provider-spec` for spec vs impl.
