# Baseline gaps

- No live model run. `LOOPER_EVAL_AGENT` was unset; no agent was started.
- No token, latency, or cost data. Those values are unknown, not zero.
- Labels are human-from-diff by one reader of public `nexu-io/looper` history (plus one tiny fixture). They are not an independent second reviewer.
- Existing GitHub review comments were not treated as ground truth.
- Repair-frontier history for #665/#664 is reconstructed from public commit messages and the production diff, not from a stored Looper session.
- Large samples (#663, #644) were not line-audited hunk-by-hunk for residual defects; clean labels mean “no remaining in-scope must_fix observed,” not “proven empty.”
- Concurrency category uses a committed fixture because suggested public SHAs are race *fixes*, not remaining racy diffs.
