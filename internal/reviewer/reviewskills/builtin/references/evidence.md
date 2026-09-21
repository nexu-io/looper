# Evidence and counterexamples

Use existing comment `body` plus `scopeEvidence`. Do not invent evidence records, confidence scores, finding quotas, or a second filter model.

Comment shape for every finding: trigger condition → actual path → wrong consequence → suggested fix.

## Race / deadlock

Establish a real concurrent call path, the shared state, and the synchronization that is supposed to protect it. Do not infer a race from a mutex, channel, or `go` keyword alone.

Positive: two goroutines have conflicting concurrent accesses to the same struct field / map / slice header, with at least one access a write and no lock, atomic, or channel handoff on that path.

Negative: a per-call local variable, a value copied into a closure, or a field only mutated on a single-owner goroutine.

Drop a race finding only when existing synchronization or single-owner access already excludes the claimed interleaving. A type documented as not concurrent does not disprove a race; report the caller's unsynchronized shared use (for example concurrent writes to a shared `bytes.Buffer`).

## Resource leak

Name the owner, the lifecycle, and whether ownership transferred.

Positive: a handle, file, connection, ticker, or context cancel is created on a success path that can return without close/stop, and no caller is documented to take ownership.

Negative: ownership is returned to the caller (`io.ReadCloser`, constructor that documents Close), or a `defer` on every return path already releases it.

## Security

Name the untrusted input, a reachable path from that input, and the actual trust boundary.

Positive: attacker-controlled bytes reach a sink (exec, query, path, template, authz check) that interprets them unsafely—missing parameterization, contextual escaping, or path confinement—without validation or a trust-boundary change.

Negative: the specific value has already been constrained for the destination sink, comes from a trusted config/operator surface, or never reaches the sink on the claimed path. Authentication or authorization to invoke an operation does not make user-supplied repository names, query strings, or template values trusted.

## Go loop variable / timer

Check the module's Go version and whether the loop declares the captured variable.

Declaration (`for _, v := range xs { go func() { use(v) }() }`): on Go 1.22+ each iteration has its own variable; this is a bug only on Go 1.21 and earlier, or a vendored older compiler.

Assignment (`var v T; for _, v = range xs { go func() { use(v) }() }`): the preexisting `v` is reused on every Go version; a later-running closure can race.

Direct call arguments (`go f(v)`, `defer f(v)`) are evaluated immediately and were safe even before Go 1.22. Do not flag those as loop-variable capture.

Timer findings depend on the module's Go version and whether the timer stays reachable under load, not on loop versus one-shot shape.

Before Go 1.23, `time.After` keeps the timer alive until it fires. Flag it when a long-lived loop or a canceled request can accumulate or retain those timers. A short one-shot that remains referenced until fire is not automatically a leak, but a long-duration `time.After` after cancel can still retain the timer.

On Go 1.23+, the garbage collector can recover unreferenced, unstopped timers, so `time.After` in a loop is not inherently a leak. Prefer `time.NewTimer` with `Stop`/`Reset` only when the code still holds the timer, needs cancel/reset, or the module targets a pre-1.23 toolchain.

## Contract errors

Distinguish caller-dependent contracts from intentional best-effort.

Positive: a function whose documented contract is to return the error (or fail closed) instead logs-and-continues, returns `nil`, or wraps into a success object, and a caller would act on the missing failure.

Negative: the comment/spec says best-effort, the error is explicitly allowed (retry, optional hook, cache fill), or a higher frame already handles it.

Do not treat "callers already guarantee this precondition" as a defect unless a public/exported caller can violate it.

## Test suggestions

Never write only "add tests". Name the failing behavior, the test file or package where it belongs, and the appropriate level (unit, fixture-matrix, integration). If you cannot name those three, do not suggest a test.

## Counterexample pass

Run this pass in the same review context before finalizing. It is a conservative self-check, not a second model stage.

Drop a candidate only when evidence proves it factually wrong, or when it is the same root cause as another finding (keep one representative).

Unverified is not wrong. Do not silent-drop because the finding count is high or the issue looks low-value.

Ask, then keep unless the answer is proven:

- Do callers already guarantee the precondition on every reachable path?
- Does existing synchronization exclude the claimed race/deadlock?
- Is the failure explicitly allowed (best-effort, optional, documented skip)?
