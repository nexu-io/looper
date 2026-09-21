# reviewer-eyes-repair-frontier

Repair frontier pair from public history:

- last published head: `b44bf5cb` (#664) restore start-of-review eyes
- current head: `a14f17aa` (#665) drop in-progress eyes when the queue stops retrying

Read `internal/reviewer/runner.go` on #665: `clearInProgressReactionIfQueueStopped` returns early only while `failedQueue.Status == "queued"`; otherwise it clears. Terminal success/skip already clears via `finalizeSuccessfulReviewerQueue`. Mid-publish clears were removed so retryable publish failures keep the marker.

The prior must_fix (stale eyes on a stopped queue) appears addressed in this delta, with regression tests in `runner_test.go` / `runner_integration_test.go`. Expected findings empty: later-pass should confirm the thread, not invent new P2/P3 on untouched #664 code.

History notes are reconstructed from commit messages and the production diff, not a stored Looper session.
