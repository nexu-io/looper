# Reviewer quality evaluation kit

Offline, re-runnable samples so later reviewer skill or config changes can be compared against a frozen human-labeled set. This kit does **not** start the daemon, call live GitHub, run `review submit`, or mutate threads.

## Layout

- `samples/index.json` — machine-readable catalog (8–12 samples).
- `samples/<id>/meta.json` — identity, frozen SHAs, pass kind, scope, history.
- `samples/<id>/labels.json` — human labels. **Never** feed this file to the agent.
- `samples/<id>/notes.md` — why the sample was labeled this way, including ambiguity.
- `fixtures/<id>/` — tiny committed diffs used only when public history has no remaining in-diff example of that category.
- `baseline/` — recorded baseline or explicit gaps. No fabricated metrics.
- `runs/` — local replay artifacts. Gitignored.

## Counting rules (when a model is later run)

- Precision and recall are grouped by `rootCauseKey`. Duplicate comments for the same key collapse to one predicted finding.
- A clean review (`expectedFindings: []`) is not an execution failure.
- Agent crash, timeout, missing completion marker, or tool/transport error is an **execution failure**, counted separately from “no findings”.
- Unknown stays unknown. Do not treat a skipped sample as precision 0 or as a clean pass.

## Prompt-construction replay (CI-safe)

This does not invoke a model. It checks that first-pass vs repair-frontier prompts are built from `meta.json` only, and that `labels.json` title/body strings do not leak into the prompt.

```
go test ./internal/reviewer -run TestEvalReplayPromptConstruction -count=1
```

Also:

```
go test ./internal/reviewer -run TestEvalSampleIndexValid -count=1
```

## Live model baseline (optional, not a CI gate)

If `LOOPER_EVAL_AGENT=1` is unset, do **not** start an agent or invent token counts (including zero). Live model replay is maintainer-only, gated on that env, and is **not** a CI hard gate.

When that env is set, a future runner may execute one first-pass and one repair-frontier sample against a real agent. Record the actual model, effort, budget, and token usage in `baseline/`. Do not write placeholder zeros.

## Limitations

- Labels are one human reading of the public diff, not an independent second reviewer.
- Merged history PRs that look correct are labeled clean; that is not a claim that no reviewer would ever comment.
- GitHub review comments on those PRs are material to verify, not ground truth.
- No production telemetry, no daemon benchmark mode.
