---
name: looper-review
description: Default Looper review method for implementation and spec PRs. Read before reviewing. Covers correctness, tests, concurrency, contracts, and comment quality.
---

# Looper review method

Read this file, then read every referenced file below before reviewing.

Do not treat skill selection as a scope reduction: if no specialty skill matches, still complete the base review using this looper-review method. Apply the disposition schema, publish split, and repair-frontier contracts from the Looper reviewer prompt; this skill does not restate those execution contracts.

## Procedure

1. Understand intent from the PR seed, title/body, linked spec, and stated goals/non-goals.
2. Enumerate in-scope changes with local Git at the seeded base/head SHAs.
3. Read the surrounding context needed to judge each change (callers, tests, contracts, lifecycle).
4. Accumulate independent in-scope issues internally. Group only the same root cause; keep unrelated concerns separate.
5. Apply the disposition schema from the Looper prompt (`must_fix` | `follow_up` | `needs_human`, severity, scopeBasis, scopeEvidence).
6. Write comments that meet the quality bar in the reference files.

## References

- [Comment quality](references/comment-quality.md)
- [Implementation review rubric](references/implementation-rubric.md)
- [Spec/docs review rubric](references/spec-rubric.md)
