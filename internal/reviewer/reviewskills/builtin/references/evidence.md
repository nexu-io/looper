# Evidence and counterexamples

Use existing comment `body` plus `scopeEvidence`. Do not invent evidence records, confidence scores, finding quotas, or a second filter model.

Comment shape for every finding: trigger condition → actual path → wrong consequence → suggested fix.

## Data race

Establish a real concurrent call path, the shared state, and the synchronization that is supposed to protect it. Do not infer a race from a mutex, channel, or `go` keyword alone.

Positive: two goroutines have conflicting concurrent accesses to the same shared memory location (for example a struct field, map, slice header, ordinary variable, pointer target, array or slice element, interface value, or an object's internal storage), with at least one access a write, no happens-before ordering between the conflicting accesses, and at least one access that is not a compatible atomic operation.

Negative: a per-call local that is never shared with another goroutine, a copied value that does not reference shared mutable storage, or a field whose reads and writes are all confined to one owner goroutine. A local captured by multiple goroutines or a single writer with unsynchronized readers is not a negative example.

Synchronization is negative evidence only when it actually orders the conflicting accesses. Different mutexes alone, unrelated channel operations, or an atomic operation paired with an unsynchronized plain access do not establish safety. An atomic-based drop requires compatible atomic operations for all concurrent accesses to that location; plain accesses in an ordered initialization or teardown phase are a separate case. A type documented as not concurrent does not disprove a race; report the caller's unsynchronized shared use (for example concurrent writes to a shared `bytes.Buffer`).

## Deadlock / permanently blocked operation

Establish a reachable wait cycle or an operation that cannot complete, the progress the contract requires, and why the relevant release or cancellation paths cannot unblock it. This can happen in one goroutine and does not require shared state: recursively locking a non-reentrant mutex or sending/receiving on a nil channel can block indefinitely.

Distinguish a defect from intended waiting. A nil-channel case disabled inside a select is not itself a deadlock when another case can make the required progress; a reachable release, handoff, or cancellation path may also disprove the claimed permanent block.

## Resource leak

Name the owner, the lifecycle, and whether ownership transferred.

Track each acquired resource until release or ownership transfer. Positive: a resource (for example a handle, file, connection, or context created with cancellation) stays allocated beyond its intended lifetime because its owner is lost, overwritten, or fails to release it. Check normal/error exits, partial initialization, replacement, and repeated acquisition in long-running or non-returning loops; accumulation need not wait for a return. For timers and tickers, also establish the effective Go version semantics and reachability described below before claiming a leak.

Negative: ownership is transferred to an identified owner with an established release responsibility (for example `io.ReadCloser` or a caller that closes the result), or a reachable release keeps resource use within its intended lifetime and bounds. A function-scoped `defer` alone does not disprove accumulation across loop iterations; establish when it actually runs. Intentional bounded pools or caches need an ownership/lifetime analysis, not a missing-close keyword finding.

## Security

Name the untrusted input, a reachable path from that input, and the actual trust boundary.

Positive (unsafe interpretation): attacker-controlled bytes reach a sink (exec, query, path, template) that interprets them unsafely, such as missing parameterization, contextual escaping, or path confinement. Crossing a component or trust boundary does not sanitize those bytes; require an effective, sink-appropriate transformation before treating them as safe.

Positive (authorization): a reachable path lets a principal perform an action on a resource that the policy forbids, because an ownership, tenant, or other authorization check is missing or applied to the wrong principal/resource. A valid, sink-constrained identifier (for example a UUID in a parameterized query) is not a drop reason for this case.

Negative: the specific value has already been constrained for the destination sink, comes from a trusted config/operator surface, or never reaches the sink on the claimed path. Authentication or authorization to invoke an operation does not make user-supplied repository names, query strings, or template values trusted. Sink-constrained bytes do not disprove a missing or misapplied authorization check.

## Go loop variable / timer

Check the module's Go version and whether the loop declares the captured variable.

Declaration (`for _, v := range xs { go func() { use(v) }() }`): on Go 1.22+ each iteration has its own variable; this is a bug only on Go 1.21 and earlier, or a vendored older compiler.

Assignment (`var v T; for _, v = range xs { go func() { use(v) }() }`): the preexisting `v` is reused on every Go version; a later-running closure can race.

Direct call arguments (`go f(v)`, `defer f(v)`) are evaluated immediately and were safe even before Go 1.22. Do not flag those as loop-variable capture.

Timer findings depend on the module's Go version and whether the timer stays reachable under load, not on loop versus one-shot shape.

Before Go 1.23, `time.After` keeps the timer alive until it fires. Flag it when a long-lived loop or a canceled request can accumulate or retain those timers. A short one-shot that remains referenced until fire is not automatically a leak, but a long-duration `time.After` after cancel can still retain the timer.

With Go 1.23+ timer semantics, the garbage collector can recover unreferenced, unstopped timers and tickers, so `time.After` in a loop or an unreachable `time.Ticker` without `Stop` is not inherently a leak. Check the module version and any runtime override of those semantics. Still report retained timers/tickers or ticking beyond the intended lifecycle when a concrete resource or behavior consequence is reachable. Prefer `time.NewTimer` with `Stop`/`Reset` only when the code still holds the timer, needs cancel/reset, or uses pre-1.23 timer semantics.

## Contract errors

Distinguish caller-dependent contracts from intentional best-effort.

Establish the error contract from documentation, the signature together with caller behavior, or a required invariant. Positive: an operation required for success fails, but its wrapper logs-and-continues, returns `nil`, or wraps the failure into a success object, causing a caller to act on an incorrect success. A failed write, commit, or initialization can establish this chain even when the wrapper has no comment or spec.

Negative: an established best-effort or optional-operation contract explicitly allows the failure (retry, optional hook, cache fill), or a higher frame already handles it. Missing documentation alone neither proves best-effort behavior nor requires propagation of every error.

Use the established contract (from documentation, API semantics, callers, or required invariants) to identify who must enforce each precondition, then trace the reachable call paths. Report a concrete caller that violates its obligation, or a callee that omits validation its contract promises. Public/exported visibility alone neither establishes nor excludes a defect: an exported API may require callers to satisfy a precondition, and an unexported caller can still violate it. When the relevant callers satisfy the contract, do not demand a redundant callee check.

## Test suggestions

Never write only "add tests". Name the failing behavior, the test file or package where it belongs, and the appropriate level (unit, fixture-matrix, integration). If you cannot name those three, do not suggest a test.

## Counterexample pass

Run this pass in the same review context before finalizing. It is a conservative self-check, not a second model stage. The authority for a drop is that same context (diff, callers, tests, contracts), not the first structured candidate list.

First establish the candidate's concrete trigger → actual path → wrong consequence. If that chain is uncertain, investigate it; if it still cannot be established, omit the unsupported claim from published findings. Source, contracts, and tests can establish the chain without executing a reproduction. A missing reproduction is not disproof.

For an evidence-backed candidate, drop it in this counterexample pass only when evidence proves it factually wrong, or when it is the same root cause as another finding (keep one representative).

Do not silently drop an evidence-backed finding because the finding count is high or the issue looks low-value. An unproven counterargument does not invalidate established evidence.

For those evidence-backed candidates, check:

- Do callers already guarantee the precondition on every reachable path?
- Does existing synchronization exclude the claimed race/deadlock?
- Is the failure explicitly allowed (best-effort, optional, documented skip)?
