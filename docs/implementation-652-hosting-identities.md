# Implementation plan: hosting identities (#652)

Source: https://github.com/nexu-io/looper/issues/652. Base: `c610a504`.

## Audit before implementation

- Role runners resolve project policy from a captured configuration. `runtime/scheduler.go` constructs planner, reviewer, worker, fixer and coordinator adapters; discovery, webhook discovery and claimed queue execution all need the same binding. There is no independent auditor runner in the current main branch; the user explicitly excluded auditor from this implementation.
- GitHub operations converge on `internal/infra/github.Gateway`, including current-user lookups and discovery caches. An installation token cannot use the personal `/user` endpoint. Identity-dependent cache entries must not cross bot boundaries.
- Forgejo runtime adapters currently build clients from provider credentials for every call. Both native token and explicit tea login paths must remain unchanged for legacy projects; explicit identities must bypass legacy credential lookup entirely.
- Git network and commit operations converge on `internal/infra/git.Gateway`; GitHub review anchoring also runs Git. Git fetch, push and remote inspection need ephemeral HTTPS authentication, while local repository remote settings remain intact.
- The agent executor allowlists inherited environment variables, then merges configured env. Consequently a bot-mode final scrub is necessary even for credentials accidentally configured in `agent.env`. Worker/fixer validation commands also execute repository code and currently inherit the daemon environment. Forgejo prompts currently tell agents to read a token or invoke tea directly.
- Reviewer publication already uses a bounded Unix-socket proxy with run/PR/worktree/policy binding and supervisor-owned subprocesses. Other role operations need daemon-side entry points too. Reuse this protocol and review policy rather than passing tokens to agents.
- Existing sandbox CI already mints GitHub App installation tokens, but cannot establish daemon-side JWT exchange/refresh and injects a token into a legacy fake-agent environment. Add separate bot-mode coverage.
- #598, #602 and #603 remain distinct review-policy problems: distinct bots avoid some self-identity collisions but do not authorize self-approval or change platform restrictions.

## Delivery phases

1. **Configuration and credentials.** Add reusable `identities`, project defaults and role overrides; static reference/target validation and hot reload; immutable run binding; GitHub App JWT exchange/refresh and bot attribution; Forgejo env-reference authentication. Verify with deterministic HTTP/crypto integration tests and configuration compatibility tests. Independent review checks selection authority, refresh and fail-closed behavior before runtime integration.
2. **Complete execution path.** Bind discovery and claimed runs; route GitHub/Forgejo/Git operations through the selected binding; provide bounded daemon-side agent operations; apply commit attribution and scrub agent credentials; isolate startup/runtime auth errors. Add cross-component and sandbox contracts, configuration documentation, run all repository checks and review the complete flow. Review focuses on accidental personal fallback, scope escapes, lifecycle cleanup and legacy behavior.

These phases are implementation dependencies, not independently deployable feature slices. Keep the complete change together for delivery.

## Intended interfaces

- `config.HostingIdentityConfig`: kind (`github-app` or `forgejo-token`), base URL, App/installation IDs and private-key path or token-env reference, optional commit name/email overrides. No credential values in configuration.
- `config.ResolveHostingIdentity`: effective role identity → project default → legacy authentication. The result contains a copied definition and the project's existing repository target.
- `hostingidentity.Manager`: daemon-local credential cache, with immutable sessions carried through context. Refreshes credentials for the same captured definition; no new database table or persisted token file.
- Gateways consume the context binding. Legacy requests without a binding retain their existing transport. Agent-side capabilities refer to a daemon socket, not credentials.
- Bot prompts delegate remote publication to existing daemon reconciliation. `git_pr_lifecycle` is optional in the current runners, so no additional PR mutation or push proxy is needed. The broker exposes bounded reads, fixed thread queries, scoped fetch where required, and the existing review-submit gate. Keep runner `AllowAutoPush` and daemon GitHub availability checks enabled; only remove agent-side remote delegation.

## Authority and costs

The authority for execution identity is the user's explicit configuration and inheritance order, not agent output, because credential authorization is operator policy rather than a task decision.

Consolidate credential selection at transport boundaries instead of maintaining an identity-specific fork of each role. Removing duplicated per-operation auth selection prevents discovery/publication/Git from drifting between accounts. Immutable in-memory bindings cost context propagation and cache lifecycle code, but avoid a persisted identity ledger; re-reading live config for every operation would switch an active run's account.

The agent proxy prevents tokens/private keys entering agent execution and provides scoped Git/API reads. It costs request validation, cancellation, bounded I/O and socket cleanup. Removing agent-managed remote publication in bot mode lets existing daemon code own push and PR mutations, avoiding a second mutable PR API and duplicated role permissions. Reusing the existing review proxy retains its authority checks; publication-token replacement or environment injection alone cannot meet the issue's complete-run requirement.

## Verification ownership and budget

- Configuration lane: JSON/TOML/YAML load/clone/hot reload, every role, invalid references/target mismatch, legacy defaults.
- Credential lane: real RSA signing against a local HTTP server; expiry/refresh, concurrent use, revocation, isolated identities, safe errors and correct bot attribution.
- Runtime lane: discovery→claim→agent→publish/Git contracts, configuration update during an active run, role isolation and webhook paths.
- Agent/Git lane: real temporary Git repositories plus a local broker/client, remote preservation, new and amended commit attribution, no secrets in agent env/output, scope and cancellation.
- Final owner: orchestrator. Run `gofmt -l .`, `go vet ./...`, `go test ./...`, `go build ./...`. Real GitHub auth/scope is covered by sandbox E2E only; report any missing sandbox credentials explicitly rather than treating mocks as live evidence.

The user confirmed that this machine has no test App credentials and requested implementation plus CI sandbox wiring. Local live sandbox execution is therefore excluded from this session's completion criteria; enabled CI must exercise the App credentials held there. The unrelated disk-cleanup design is outside this change.
