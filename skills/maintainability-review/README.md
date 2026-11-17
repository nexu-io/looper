# Maintainability review pilot

An **opt-in supplemental method**, not an embedded Looper skill or a new review gate. Source: [Reviewer maintainability proposal](https://powerformer.feishu.cn/wiki/IEfTwk5saiH432ktWOfc7zoRnP6), sections 2–7.

## Alignment status: PARTIALLY BLOCKED

Baseline: the Feishu proposal above, exported via `feishu-cli wiki export IEfTwk5saiH432ktWOfc7zoRnP6`; implementation base `a87830b`. The local design copy is informative. Implement only method authoring and existing-mechanism verification (proposal §7 step 2); no Spark changes, default enablement, new runtime state, gates, or scoring fields.

| ID | Confirmed behavior / acceptance | Source | Code / verification |
| --- | --- | --- | --- |
| M1 | Supplemental review covers six dimensions without weakening correctness or using length thresholds | §2–3 | `SKILL.md`, `references/examples.md`; method content inspection |
| M2 | Every published claim has location, semantics, impact, scope, counterexamples, and a minimal change | §3.2 | Method and examples; model adherence requires isolated evaluation |
| M3 | Only in-scope items publish; merge-safe maintenance stays non-blocking; follow-up/human items retain existing handling | §4 | Existing disposition, summary-filter, severity, and publication contract tests |
| M4 | Opt-in extend keeps builtin; required missing/invalid method fails; no selection stays builtin-only | §5–6.1 | `internal/reviewer/maintainability_skill_test.go`, real resolver → prompt contract |
| M5 | Re-review stays on the repair frontier rather than unrelated historical refactoring | §3.3, §6.1 | Method; existing later-pass and native-resume repair-frontier tests |
| M6 | Wiring tests are not quality proof; evaluation is isolated and rollout separately authorized | §6–7 | Evaluation procedure below; no real PR publication or rollout |

Decisions needed (block only evaluation/enablement): deployment owner confirms pilot projects and project-versus-user installation; maintainers confirm sample labels/adjudicators and proposed quality targets. The distributable lives in `skills/maintainability-review/`, following the repository's existing optional-skill layout; this does not choose or install a runtime location. No executable 20-sample corpus or human labels are claimed by this change.

Trade-off: a short reviewer instruction would avoid a separate method, but cannot conveniently carry this reusable investigation procedure and examples. The additional skill costs reading/search tokens and rule/example maintenance, with potential duplication and false positives. No additional agent, output schema, persisted evidence record, or gate is introduced; existing agent structured output remains authoritative for conclusions. If pilot value is insufficient, shrink or remove the selection rather than adding an inference layer.

## Implementation verification

Verified on branch `feat/reviewer-maintainability`, working tree based on `a87830b` (uncommitted method/docs/test changes):

- `go test ./internal/reviewer/... -count=1`: passed, including the new real-file selection/prompt contract and existing publication/frontier contracts.
- `go vet ./...`: passed.
- `go test ./...`: passed on rerun. The first attempt timed out at the command's 120-second limit and also reported an agent native-resume timing failure; the complete rerun with a longer command timeout passed.
- `go build ./...` and `git diff --check`: passed.
- The new Go file is gofmt-clean. Repository-wide `gofmt -l .` still reports pre-existing `internal/config/value_sources_test.go`, unchanged by this work.

No live model comparison, human-labeled corpus, Spark change, project configuration enablement, or remote publication has been performed. This is method implementation and integration verification, not acceptance of pilot effectiveness.

## Install and select

Copy this directory, including `references/`, into the chosen business repository as `.agents/skills/maintainability-review/`. Alternatively, a deployment owner may install it in the daemon user's `~/.agents/skills/maintainability-review/`. Merely shipping it in Looper's `skills/` does not install or enable it. Keep the location and pilot projects explicit; do not enable it globally by default.

In the **selected project's** configuration, merge this into `roles` (do not overwrite other role settings):

```json
{
  "reviewer": {
    "skills": {
      "mode": "extend",
      "required": ["maintainability-review"],
      "optional": []
    }
  }
}
```

Keep any other required/optional methods: arrays replace inherited lists as a whole. `extend` preserves builtin `looper-review`. Missing files, invalid frontmatter, or a mismatched skill name fail the review rather than silently skipping a required method. Use a Looper version supporting `roles.reviewer.skills`; see [configuration](../../docs/configuration.md#reviewer-skills).

Rollback: remove only this skill's selection from the project's required list. Retain `extend` and the base method. No new binary, database migration, or production remote write is needed to install or remove this method on a compatible version.

## Verification versus effectiveness

The Go contract test `TestMaintainabilitySkillSelectionAndPrompt` exercises the shipped file through the existing resolver and prompt builder, including disabled-by-default, extend, missing-file, and invalid-name behavior. Existing reviewer tests cover disposition filtering, non-blocking publication contracts, and the re-review repair frontier. These prove wiring and boundaries, **not that the model follows the method or improves review quality**.

Before enabling a pilot, the owner must confirm target projects, installation location, label adjudicators, and evaluation goals. Prepare at least 20 fixed base/head samples (10 positive, 10 counterexamples), covering all six review dimensions. Include permission/transaction/error/platform/lifecycle differences, unrelated historical refactors, conflicting intent, and existing correctness defects. The prose examples in `references/examples.md` are guidance, not executable fixed code samples or human-approved labels.

For each sample, run base-only and base-plus-maintainability using the same agent, model, execution settings, code revisions, and scope. Collect existing structured results in isolation; never automatically publish to real PRs. Have maintainers pre-label independent root causes, scope, and severity and adjudicate disagreements. Record code and skill revisions, agent/model versions, execution settings, outputs, duration, and token usage when available. Report failed runs, no output, missing labels, and missing telemetry separately; do not turn them into successful or zero-cost samples.

Report:

- Precision: confirmed, correctly disposed proposed maintainability findings / all proposed maintainability findings (N/A when none).
- Recall: correctly detected labeled root causes / all labeled root causes.
- Evidence completeness: human-verified fulfillment of all six reasoning requirements / all proposed maintainability findings.
- Counts of scope expansion, wrong blocking, and inappropriate abstraction suggestions.
- Base correctness regressions detected by control but missed by treatment, plus overall results for both groups.
- Paired duration/token changes, median and P90. Missing tokens stay missing, not estimated from characters.
- Actionability: in-scope, behavior-preserving executable suggestions / confirmed maintainability suggestions, with reasons for failures.

The proposal's ≥90% precision, ≥80% recall, 100% evidence completeness and zero counterexample/regression counts are **proposed pilot targets, not approved production gates**. Twenty samples are only an initial check; expand and repeat before wider rollout. Actual model evaluation and project enablement require separate evidence and authorization.
