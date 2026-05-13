# Code Review Checklist

## Regression policy

- [ ] Every P0/P1 bug fix includes a regression test
- [ ] Cross-component lifecycle, worktree, GitHub command, daemon boot, and resolve-comments regressions prefer contract/invariant integration scenarios over unit-only coverage
- [ ] Real GitHub auth, scope, thread mutation, and rate-limit regressions use sandbox E2E when needed
- [ ] Missing regression coverage for a P0/P1 fix is a review blocker
- [ ] Regression tests and scenarios are mapped to the relevant PR / issue IDs

## Risk areas

- [ ] Worktree isolation and repo cleanliness are preserved
- [ ] looperd boot/config/runtime path behavior is covered when touched
- [ ] GitHub CLI/API field and route contracts remain valid
- [ ] resolve-comments state transitions and thread resolution behavior are covered when touched
- [ ] User-visible invariants are asserted, not just internal helper behavior
