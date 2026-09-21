# hollow-worktree-recovery

Public SHA `8959f738` (#622). Resource ownership / lifecycle: empty or metadata-only unusable managed worktrees are cleared and recreated; populated unusable paths park as manual intervention. Shared `worktreesafety.LocalCheckoutUsable` is the local-git-metadata authority. Reviewer, worker, and fixer resume paths must probe before reuse so hollow dirs cannot infinite-retry.

Cross-module: `internal/worktreesafety/checkout.go` plus caller changes in worker, reviewer, fixer, git gateway, and failureclass. No remaining in-scope defect observed in the new probe/clear contract. This is the real-history resource-ownership sample; the fixture covers a still-racy shared counter.
