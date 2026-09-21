# Evidence and counterexamples

Use existing comment `body` plus `scopeEvidence`. Do not invent evidence records, confidence scores, finding quotas, or a second filter model.

Comment shape for every finding: trigger condition → actual path → wrong consequence → suggested fix.

## Race / deadlock

Establish a real concurrent call path, the shared state, and the synchronization that is supposed to protect it. Do not infer a race from a mutex, channel, or `go` keyword alone.

Positive: two goroutines can write the same struct field / map / slice header with no lock, atomic, or channel handoff on that path.

Negative: a per-call local variable, a value copied into a closure, or a field only mutated on a single-owner goroutine.

Drop a race finding when existing synchronization already excludes the claimed interleaving, or when the type is documented as not concurrent.

## Resource leak

Name the owner, the lifecycle, and whether ownership transferred.

Positive: a handle, file, connection, ticker, or context cancel is created on a success path that can return without close/stop, and no caller is documented to take ownership.

Negative: ownership is returned to the caller (`io.ReadCloser`, constructor that documents Close), or a `defer` on every return path already releases it.

## Security

Name the untrusted input, a reachable path from that input, and the actual trust boundary.

Positive: attacker-controlled bytes reach a sink (exec, query, path, template, authz check) without validation or a trust-boundary change.

Negative: the value is already authenticated/authorized, comes from a trusted config/operator surface, or never reaches the sink on the claimed path.

## Go loop variable / timer

Check the module's Go version and current runtime semantics before claiming a loop-variable capture. On Go 1.22+ each iteration has its own variable; do not flag `go f(v)` / `defer f(v)` capture unless the module (or a vendored older compiler) still reuses the loop variable.

`for i, v := range s { go func() { use(i, v) }() }` is a bug on Go 1.21 and earlier, not on 1.22+.

`time.After` in a long-lived loop can leak timers until they fire; prefer `time.NewTimer` with `Stop`/`Reset` when the loop can outlive the interval. A one-shot `time.After` on a request path that returns is not a leak.

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
