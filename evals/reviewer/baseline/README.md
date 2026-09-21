# Reviewer eval baseline

## Runtime / model / effort / budget

**unknown / not run**

This baseline records the sample kit and human labels only. No live model was executed for this PR. Token counts are therefore unknown — not zero.

## Counting rules

- Precision and recall by `rootCauseKey`.
- Duplicate comments for the same `rootCauseKey` collapse to one predicted finding.
- Execution failure (crash, timeout, missing marker, transport error) is counted separately from a clean “no findings” review.
- Unknown stays unknown.

## What this baseline does not claim

It does not claim quality improvement over any prior reviewer prompt or skill. Comparison is possible only after a later PR actually runs a model against these frozen samples.

See `GAPS.md`.
