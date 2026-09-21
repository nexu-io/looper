# fixture-unprotected-counter-race

Suggested public SHAs are race *fixes* (for example #635 mapping UNIQUE createRun races to retryable busy). None of those diffs still contain an unprotected shared mutation.

This sample is a tiny committed fixture so the concurrency/resource category has a remaining in-diff defect. Labels come from reading `proposed.diff` and `counter.go`. The unique finding title/body must not appear in the reviewer prompt.
