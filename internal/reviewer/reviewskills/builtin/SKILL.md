---
name: looper-review
description: Default Looper review method for implementation and spec PRs. Read before reviewing. Covers correctness, tests, concurrency, contracts, and comment quality.
---

# Looper review method

Read this file, then read every always-required referenced file below before reviewing. Read on-demand references only when their trigger matches the changed set.

Do not treat skill selection as a scope reduction: if no specialty skill matches, still complete the base review using this looper-review method. Apply the disposition schema, publish split, and repair-frontier contracts from the Looper reviewer prompt; this skill does not restate those execution contracts.

## Procedure

1. Understand stated intent from the PR seed, title/body, linked spec, and stated goals/non-goals.
2. Enumerate in-scope changed files/ranges with local Git at the seeded base/head SHAs. Do not shrink that set because a specialty skill matches.
3. For large PRs (roughly ≥15 changed files, or changes that cross packages), write an internal related-file plan grouping interface/impl/callers, db/readers, and prod/tests. Stay one reviewer; do not spawn sub-agents or subtasks.
4. Accumulate independent in-scope findings. Group only the same root cause; keep unrelated concerns separate.
5. Run a counterexample pass in the same context before finalizing. Drop a candidate only when evidence proves it factually wrong or a same-root-cause duplicate. Unverified is not wrong.
6. Deliver the existing disposition schema from the Looper prompt (`must_fix` | `follow_up` | `needs_human`, severity, scopeBasis, scopeEvidence). Write comments that meet the quality bar in the reference files.

## References

Always read:

- [Comment quality](references/comment-quality.md)
- [Evidence and counterexamples](references/evidence.md)
- [Implementation review rubric](references/implementation-rubric.md)
- [Spec/docs review rubric](references/spec-rubric.md)

Read on demand:

- [CSS linting](references/css-lint.md) — only if CSS or style files are in the changed set
