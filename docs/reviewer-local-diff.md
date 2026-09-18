# Local Git review content

Reviewer discovery captures PR metadata, not a remote patch. PR size does not decide whether a review starts. The agent reviews the prepared checkout with `git diff <base_sha>...<head_sha> -- <path>`, enumerating changes first and paging large outputs. Both SHAs come from the snapshot; worktree preparation fetches the base commit before fetching/checking out the PR head. Missing commit history remains an explicit error, never an empty change set.

Snapshots retain the existing base/head SHAs, title, body, author and available check/review summaries. The JSON payload contains only state/draft/conflict metadata needed by the API. New snapshots do not write diff text, diff references, truncation flags, or full PR conversations. Reviewer checkpoints no longer copy snapshot JSON. Existing database columns and historical snapshots remain readable; this change does not rewrite or delete historical records.

Both GitHub and Forgejo review submission resolve inline anchors from the same local, path-scoped Git diff. Rename partners are included so LEFT and RIGHT anchors retain PR semantics. Body-only reviews require no diff. Remote patch APIs remain available for explicit host reads, but are not a review prerequisite or fallback. The existing automatic-merge acceptance-criteria check obtains its diff locally on demand, without storing it in a snapshot.

## Trade-offs

The removed layer is whole-PR remote patch acquisition and storage. Increasing HTTP limits alone would retain repeated downloads, large database records, and provider-specific failure behavior. No new ledger, approval gate, or recovery state is introduced.

The agent's structured findings remain the review decision. The existing inline-coordinate check uses the exact base/head Git change set because provider comment coordinates refer to that change set; it does not infer whether the agent's finding is valid.

Git writes requested diff output to a private temporary file, which is parsed incrementally and removed on success or failure. This removes the previous 32 MiB shell capture ceiling without keeping the entire inline patch in memory. The cost is temporary disk I/O and possible disk/process failures, which surface as errors. A hard process crash may leave a file in the system temporary directory. Acceptance-criteria verification still materializes its local diff because its existing verifier consumes file patches; it runs only when that feature is enabled and criteria exist.

## Verification

Contract tests cover metadata capture without a remote diff, GitHub and Forgejo inline publication from local commits, missing objects, renamed/deleted paths, base fetching without displacing the PR head, and a single-file patch larger than 32 MiB with long lines and a comment at its end. Ordinary tool display limits and command timeouts still apply; truncated display is not complete review coverage.
