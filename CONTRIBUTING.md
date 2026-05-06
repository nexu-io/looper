# Contributing to Looper

Thanks for your interest in improving Looper! This guide covers how to set up a development environment, the conventions we follow, and how to get changes merged.

## Code of conduct

Be respectful, assume good faith, and keep discussions focused on the work. Harassment of any kind is not tolerated.

## Ways to contribute

- Report bugs and regressions via GitHub Issues
- Propose features or design changes via an issue before opening a large PR
- Improve documentation in `README.md`, `docs/`, or `specs/`
- Submit fixes and features as pull requests

For non-trivial changes, please open an issue first so we can align on scope before you invest time in a PR.

## Prerequisites

- Go 1.22+ (see `go.mod`)
- `git`
- `gh` (GitHub CLI), authenticated — Looper relies on it at runtime
- macOS or Linux. macOS notification features additionally require `osascript`

## Project layout

- `cmd/looperd` — daemon entrypoint
- `cmd/looper` — CLI entrypoint
- `internal/`, `pkg/` — Go implementation packages
- `configs/` — sample configuration
- `docs/` — user-facing documentation
- `specs/` — design specs
- `scripts/` — install and dev helpers
- `dist/` — build output (gitignored, do not edit)

## Getting started

```bash
git clone https://github.com/powerformer/looper.git
cd looper
go build ./...
go test ./...
```

Common dev loop from the repo root:

```bash
go run ./cmd/looperd          # run the daemon
go run ./cmd/looper <args>    # drive the CLI
go vet ./...
go test ./...
```

Default runtime artifacts land in `~/.looper/` (`looper.sqlite`, `backups/`, `logs/`). The default config path is `~/.looper/config.json`. Configuration precedence is: defaults → config file → environment → CLI flags. See `docs/configuration.md` for every field.

## Branching and commits

- Branch off `main`. Use short, descriptive names (e.g. `fix/reviewer-loop-deadlock`, `feat/cli-jump`).
- Keep commits focused; squash noisy WIP commits before requesting review.
- Commit messages and PR titles **must use semantic prefixes**:
  - `feat:` new user-visible feature
  - `fix:` bug fix
  - `docs:` documentation only
  - `test:` tests only
  - `refactor:` no behavior change
  - `chore:` tooling, deps, housekeeping
  - `ci:` CI configuration
- Imperative mood, lowercase after the prefix. Example: `fix: avoid panic when worktree path is missing`.

## Coding conventions

- Format code with `gofmt`. CI runs `gofmt -l .` and fails on any diff.
- Run `go vet ./...` before pushing.
- Prefer small, well-named packages in `internal/`. Only put genuinely reusable code in `pkg/`.
- Don't edit generated files or anything in `dist/`.
- Tool paths (`git`, `gh`, `osascript`) are auto-detected — don't hard-code them.
- `looperd` should fail fast on invalid configuration; preserve that behavior when adding new config.

## Tests

- Add or update tests for any behavior change.
- Keep tests hermetic: no network, no real GitHub calls, no writes outside `t.TempDir()`.
- Run `go test ./...` locally before opening a PR.

## Documentation

- Update `README.md` when you change user-visible commands or flags.
- Update `docs/` (installation, users-guide, configuration) when behavior or config changes.
- For larger features, add or update a spec in `specs/` and link it from the PR.

## Pull requests

1. Fork the repo (or branch directly if you have write access).
2. Make your changes on a feature branch.
3. Ensure `gofmt -l .`, `go vet ./...`, `go test ./...`, and `go build ./...` are clean — these are exactly what CI runs.
4. Open a PR against `main` with:
   - A semantic title (same rules as commits)
   - A short description of the change and motivation
   - A link to the related issue, if any
   - Notes on testing and any user-facing impact
5. Be responsive to review feedback. Push fixes as additional commits; we'll squash on merge if appropriate.

CI (`.github/workflows/ci.yml`) must be green before a PR can merge.

## Reporting bugs

A good bug report includes:

- Looper version (`looper version`) and OS / architecture
- The command you ran and the full output
- Relevant excerpts from `~/.looper/logs/`
- Your `~/.looper/config.json` with secrets redacted
- Steps to reproduce, ideally minimal

## Security

Please **do not** open public issues for security vulnerabilities. Instead, report them privately via GitHub's "Report a vulnerability" flow on the repository's Security tab.

## License

By contributing, you agree that your contributions will be licensed under the MIT License (see `LICENSE`).
