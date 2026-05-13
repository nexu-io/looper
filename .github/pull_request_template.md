# Summary

- 

# Testing

- [ ] `go test ./...`
- [ ] Additional targeted checks:

# E2E / Invariant Risk

- [ ] No Looper E2E / invariant risk
- [ ] Daemon boot / config / runtime path risk
- [ ] Worktree / repo isolation / lifecycle risk
- [ ] GitHub CLI contract / auth / scope risk
- [ ] Resolve-comments / PR thread mutation risk
- [ ] Real sandbox GitHub behavior risk

# Regression Policy

- [ ] This is not a P0/P1 bug fix
- [ ] This is a P0/P1 bug fix and includes a regression test that fails before the fix
- [ ] Cross-component lifecycle / worktree / GitHub command / daemon / resolve-comments regression is covered by a contract/invariant integration scenario
- [ ] Real GitHub auth / scope / thread mutation / rate-limit regression is covered by sandbox E2E

# Regression Mapping

- PR / issue links for regression coverage:
  - 

# Reviewer Checklist

- [ ] Tests validate user-visible invariants, not only implementation details
- [ ] P0/P1 regression policy requirements above are satisfied
