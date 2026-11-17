---
name: maintainability-review
description: Supplemental Looper review method for duplicated business rules, reuse, responsibility boundaries, unnecessary complexity, abstractions, and test maintainability. Use alongside the base correctness review, not instead of it.
---

# Maintainability review

Read this method and [the examples](references/examples.md) before reviewing. Follow the existing Looper prompt's output, publication, scope, and repair-frontier contracts. This method adds no output fields, tool requirements, agents, or publish permissions.

## Boundaries

- Keep the full base correctness review. On the first pass, cover all in-scope changes, not just code that looks repetitive or long.
- On re-review, check existing findings, new commits, and directly affected code. Do not restart a refactoring audit of unchanged historical code.
- The agent's structured findings are the review conclusions; repository rules and PR intent supply their basis. Infrastructure signals do not decide whether a semantic concern is valid.
- Prefer reuse, consolidation, or deletion over a new abstraction. Reducing lines or increasing reuse is not a goal by itself.

## Investigation

1. Read PR goals/non-goals, linked requirements, and repository rules. Identify the changed behavior and its intended boundaries.
2. Inspect changed code plus relevant existing implementations, callers, and tests. Search for the same business invariant, not just matching text. State the limits of the search: failure to find a reusable helper does not prove none exists.
3. Examine these dimensions, with their counterexamples:

   | Dimension | Investigate | Do not report merely because |
   | --- | --- | --- |
   | Duplicated logic | Multiple implementations of one rule that must evolve together | Similar text exists with different semantics |
   | Existing capability | An applicable helper/component/service already implements the new behavior | A helper has a similar name but different permissions, transactions, errors, or dependencies |
   | Responsibilities | Independent duties make understanding, modification, or testing concretely harder | A cohesive declaration, rule table, sequential protocol, or explicit test is long |
   | Complexity | State, branches, wrappers, or forwarding layers can be removed without changing behavior | A layer performs necessary authorization, compatibility, retries, or resource management |
   | Abstraction boundaries | One shared invariant is scattered, or different rules are forced into one abstraction | A repetition count, implementation count, or line threshold has been reached |
   | Test maintainability | Repeated setup forks an invariant, or parameterization hides scenarios, assertions, and failure causes | Independent tests could be shorter at the cost of diagnosis |

4. For each candidate, establish all six content requirements below. These are reasoning requirements, not new schema fields or a scoring system:
   - **Location:** changed file/symbol or inline anchor, plus the related duplicate or existing capability when applicable.
   - **Semantics/responsibilities:** the shared business rule or independent duty; text similarity alone is insufficient.
   - **Impact:** the violated rule or specific synchronized maintenance, testing, or repair cost.
   - **Scope basis:** the PR goal, spec, repository rule, or regression that makes this work belong in this PR.
   - **Counterexamples:** check permission, transaction, error, platform, and lifecycle differences before recommending reuse or deletion.
   - **Minimal change:** explain the smallest reuse/consolidation/deletion and its behavioral, dependency, and compatibility effects.
5. Investigate insufficient evidence; omit claims that remain unsupported. Keep investigation notes out of published comments. Deduplicate only a common root cause; report independent supported problems separately without a comment-count cap.

No mandatory function-length, duplication, or complexity thresholds. No new static-analysis toolchain is required.

## Disposition and publication

Use the existing structured output and concise comment format. Include the six evidence requirements naturally in the comment; do not create extra score or evidence fields.

- A real defect, broken necessary contract, or mandatory-rule violation introduced by this PR is `must_fix`; determine severity and P0–P3 priority from demonstrated impact. It may block only with concrete impact evidence.
- A supported, in-scope maintainability issue that is safe to merge is `must_fix` with `severity=non_blocking`; publish as COMMENT through the existing controlled entry point. `must_fix` means this PR should address it, not that it blocks merging.
- Independent refactors, unrelated historical debt, and future extensions are `follow_up`: retain them only in structured results, not in actionable remote comments or the visible review body.
- Conflicting requirement intent or design decisions are `needs_human`: use the existing human-decision flow, not a remote modification request.
- Ordinary style preferences, optional simplifications, and code length alone never justify blocking. Do not inflate priority to justify a maintainability comment.

The Fixer independently evaluates repository rules and PR intent. A suggested abstraction does not authorize it to expand scope.
