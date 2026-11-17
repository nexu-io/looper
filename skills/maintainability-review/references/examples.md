# Maintainability examples

These are hypothetical reasoning examples, not findings about this repository. Use actual paths, symbols, requirements, and counterexample checks from the reviewed PR. Priority, disposition, and severity remain separate.

## Supported in-scope, merge-safe feedback

**[P3] Reuse the candidate filter builder**

The new export handler in `export.go:buildExportQuery` reimplements the round and feedback predicates from `filters.go:buildCandidateFilter`, although the spec requires export and listing to share those semantics. Both callers use the same tenant predicate and transaction context, but now each filter change needs two implementations and two sets of combination checks. Reuse the existing builder while retaining export-specific pagination; this preserves the authorization boundary and avoids a new dependency.

Disposition: `must_fix`; severity: `non_blocking`; publish: COMMENT. The hypothetical spec and verified caller semantics make this in-scope. Do not claim current results are wrong without evidence.

## Supported blocking defect

**[P1] Preserve tenant filtering in the export query**

The new export query in `export.go:buildExportQuery` duplicates the list predicates but omits the mandatory tenant constraint from `filters.go:buildCandidateFilter`. With two tenants containing matching candidate IDs, the export can return another tenant's data. Reuse the tenant-scoped builder without changing the export transaction or pagination, and add an export isolation regression test with overlapping IDs.

Disposition: `must_fix`; severity: `blocking`. The concrete isolation defect, not duplication itself, justifies blocking and the priority.

## Responsibility boundaries

A changed request handler now independently parses input, constructs the same business policy used elsewhere, and manages a resource transaction. A finding needs to identify which duty violates an existing boundary and how it concretely increases modification/testing cost. Prefer the existing policy helper when its error and lifecycle contracts fit; do not prescribe a new service hierarchy merely because the handler is long.

A long generated declaration, cohesive rules table, or ordered handshake protocol is not evidence of mixed responsibilities. Splitting a protocol into many wrappers can make its invariant harder to see.

## Complexity and abstraction

A new forwarding wrapper has no callers outside the adjacent implementation, no compatibility contract, and adds a separate error translation that the PR duplicates at each call site. If removing the wrapper preserves error behavior and satisfies the PR's stated simplification goal, propose deletion with those concrete locations and costs.

Conversely, a wrapper that owns retry cleanup, compatibility translation, or authorization is not redundant. Do not remove it without checking its callers and lifecycle. Do not unify two similar state machines when one retries on timeout and the other must fail immediately; shared text is not shared semantics.

## Test maintainability

If new test setup duplicates an existing required account-isolation fixture but omits its tenant initialization, cite the fixture invariant and actual affected assertions. Prefer reuse that keeps scenario names and failures clear.

Do not demand a table-driven rewrite of explicit independent tests just to reduce lines. Conversely, flag parameterization only when it demonstrably hides relevant scenarios or assertions, not because table-driven tests are inherently bad.

## Out-of-scope improvement

A helper predating the PR could be generalized for a hypothetical third caller. The current change does not touch that rule, violate a repository requirement, or introduce a regression.

Disposition: `follow_up`; structured results only. Do not publish it as a required refactor or ask the Fixer to broaden this PR.

## Human decision

The spec says exports share list filters, while a newer authoritative decision explicitly excludes one filter. No source establishes which rule supersedes the other.

Disposition: `needs_human`; use the existing human flow. Do not hide the decision inside a helper extraction or test expectation.

## Unsupported claims to omit

- “This function exceeds 100 lines; introduce a strategy pattern.” No duty boundary, impact, or scope evidence.
- “These functions look identical; merge them.” Permission, transaction, platform, error, and lifecycle differences have not been checked.
- “There is no reusable implementation anywhere.” A limited search cannot establish this claim.
- “The code needs abstraction for future extensions.” No current requirement or affected behavior.
- “Only report the three most important problems.” Independent supported findings are not capped.
