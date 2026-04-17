# CLI dependency-injection and testing pattern

## Decision

Use an instance-based Cobra app that accepts a single injected `Deps` struct and builds a fresh root command tree for each invocation.

## Required shape

1. `cmd/looper/main.go` stays a thin adapter that calls an importable CLI app entrypoint and is the only place allowed to call `os.Exit`.
2. CLI wiring lives under an internal package (for example `internal/cliapp`) with an `App`/`CLI` struct and `New` constructor.
3. `App.Run(ctx, argv) int` builds a new Cobra root command per call rather than reusing package-global commands or `init()` registration.
4. Command handlers receive a per-invocation context carrying parsed flags, writers, loaded config, and the injected dependencies needed for side effects.
5. Side-effecting operations stay behind injected function fields or very small interfaces on `Deps`; command handlers must not call `os`, `exec`, `http.DefaultClient`, `time.Now`, or environment lookups directly.

## Minimum dependency surface to inject

The Go CLI should preserve the same isolation boundaries as the TypeScript `runCli(argv, deps)` model.

- config loading
- daemon/API client construction
- filesystem reads/writes and directory creation
- process execution, detached spawn, signaling, and shell launch
- environment snapshot and working-directory inputs
- stdout/stderr writers and TTY detection
- time/sleep hooks needed by retry and polling flows
- daemon install / upgrade helpers
- version/build metadata where the CLI surfaces it

Keep these as explicit fields on a single `Deps` struct first. Introduce a named interface only when a dependency has more than one real implementation or its fake becomes complex enough to justify one.

## Testing pattern

1. Primary CLI tests call `App.Run(context.Background(), argv)` directly with fake dependencies and buffer-backed stdout/stderr.
2. Help and usage tests also run in-process by capturing Cobra output; they should not need subprocesses.
3. Golden tests should verify help text, JSON output, and selected human-readable tables against the frozen CLI contract artifacts.
4. Command-specific tests should assert dependency interactions through fakes instead of relying on real files, real processes, or real HTTP servers unless the test is explicitly integration or end-to-end.
5. End-to-end smoke tests may build and run the real binary separately, but they are not the primary mechanism for CLI correctness.

## Rejected patterns

- Cobra package globals or `init()`-driven command registration
- a DI container such as `fx` or `wire`
- storing CLI dependencies in `context.Context`
- direct calls to process, filesystem, HTTP, or environment APIs from handler code

## Why this pattern

- Preserves parity with the existing TypeScript `runCli(argv, deps)` test model.
- Keeps Cobra isolated to command wiring instead of application logic.
- Makes command tests deterministic and parallel-safe.
- Minimizes framework lock-in and keeps future refactors local to the CLI package.
