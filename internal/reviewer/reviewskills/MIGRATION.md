# Reviewer skill extraction inventory

Equivalent METHOD migration from `buildReviewPromptWithInstructions`. Execution contracts stay in the Looper core prompt. Byte counts are inventory, not a quality claim.

## KEEP (core prompt)

| Item | Notes |
| --- | --- |
| `reviewerLocalDiffContract`, minimal PR seed, fetch/host contracts | Source of truth remains local git at seeded SHAs |
| Phase + scope | implementation vs spec; full_pr / changed_files / changed_ranges |
| Publish / outcome / idempotency / marker / freshness / review-request / wrapper fail-closed | Native and comment-only publish splits unchanged |
| Disposition field names | `must_fix` / `follow_up` / `needs_human`; severity enum names `blocking` / `non_blocking` / `nit`; `scopeBasis` / `scopeEvidence` |
| `repairFrontierPassContract` + first-pass "complete one full review pass" | Later-pass vs first-pass still in core |
| Fixer decline adjudication | Unchanged |
| Anchor / submit payload / worktree hygiene / shell safety / content safety | Unchanged |
| Disclosure, reactions, spec labels | Unchanged |
| Inline / thread publish requirements | Wrapper still rejects body-only must_fix |
| Custom instructions hook + `AppendCompletionInstruction` | Unchanged |
| One-sentence accumulator | "Group findings only when they share the same root cause" / "Accumulate every independent in-scope must_fix" |

## MOVED (builtin `looper-review` skill)

| Item | Destination |
| --- | --- |
| "Every comment MUST include" / "Every finding MUST include" | `builtin/references/comment-quality.md` |
| Invalid comment criteria | `builtin/references/comment-quality.md` |
| Bad comment example / Good spec/docs comment example | `builtin/references/comment-quality.md` |
| "Write substantially more detail" | `builtin/references/comment-quality.md` |
| "Do not repeat the overall body/summary as a comment" | `builtin/references/comment-quality.md` |
| fixture-matrix paragraph | `builtin/references/comment-quality.md` |
| CSS linting (on-demand for CSS parser/linter implementation changes, including non-style files; not ordinary CSS-only edits) | `builtin/references/css-lint.md` |
| Evidence rules (race/leak/security/loop-var/contract/test suggestions) | `builtin/references/evidence.md` |
| Counterexample pass on evidence-backed candidates (drop disproved findings or same-root-cause duplicates) | `builtin/references/evidence.md` |
| Implementation review rubric | `builtin/references/implementation-rubric.md` |
| Long severity rubric ("mark a finding as BLOCKING only when…") | `builtin/references/implementation-rubric.md` |
| Long finding-accumulator essay | `builtin/references/implementation-rubric.md` |
| Spec/docs review rubric | `builtin/references/spec-rubric.md` |
| Procedure (intent → enumerate without shrinking → surrounding context → large-PR related-file plan → establish findings with concrete evidence → same-context counterexample pass → existing schema) | `builtin/SKILL.md` |

## DELETED-DUPLICATE

Native and comment-only copies of the same METHOD essays were collapsed to one skill/reference copy. Core prompt keeps a single short accumulator sentence instead of the longer "Finding accumulator contract" paragraphs.

Publish wrapper text, finding JSON schema, and disposition field names were not deleted.

## Prompt byte sizes

Measured from the migrated METHOD string bodies (UTF-8) plus the injected skill-index section. Not a runtime `len(prompt)` golden; a builder test can re-measure.

| Path | Before (METHOD essays in prompt) | After (essays removed, index injected) | Delta |
| --- | ---: | ---: | ---: |
| Native first-pass METHOD body | 3863 | 0 | -3863 |
| Skill index placeholder | 0 | 495 | +495 |
| Native first-pass net | | | **-3368** |
| Comment-only extra METHOD body (non-overlapping) | 661 | 0 | -661 |
| Embedded skill tree (`builtin/`) | 0 | 18190 | +18190 (not in the core prompt) |

Approximate assembled native first-pass prompt with typical seed/disclosure: **~21.5 KiB before**, **~18.2 KiB after** (net about -3.3 KiB from METHOD extraction, plus a ~0.5 KiB index). Comment-only prompts lose the extra ~0.6 KiB duplicate METHOD copy. Runtime materialized indexes add an absolute temp path (~80–120 bytes) instead of the placeholder.

The builtin skill includes evidence, counterexample, on-demand CSS lint, and large-PR related-file planning guidance. Model-quality evaluations are maintained in a separate system. This repository keeps functional and regression tests for the review contracts; method-text changes alone do not establish a quality improvement.
