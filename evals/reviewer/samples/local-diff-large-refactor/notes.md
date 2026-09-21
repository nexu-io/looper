# local-diff-large-refactor

Public SHA `ad51f777` (#663). Large local-diff refactor: 35 files, 794 insertions / 555 deletions. Reviewer now treats prepared worktree + fixed base/head SHAs as the change source instead of embedding a complete remote diff. Also touches Forgejo tea capture, GitHub review anchors, scheduler, and snapshot slimming.

Sample purpose is coverage completeness on a large changed-ranges pass. Hunks were not line-audited for residual defects. Clean label means no remaining must_fix was observed in the commit message, file list, and production-facing summary — not a proof of emptiness.
