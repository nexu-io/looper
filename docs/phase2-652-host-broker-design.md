# Hosting identity execution boundary (#652)

Implemented design, 2026-09-12. Initial authentication research is in
[research-652-hosting-identities.md](research-652-hosting-identities.md); operator
configuration is in [configuration.md](configuration.md#hosting-bot-identities).
Auditor is excluded by the user's scope decision.

## Remove agent-owned remote publication

Bot-mode agents commit locally and return the existing structured task result.
The existing daemon runners publish through their Git and hosting gateways:

| Role | Daemon publication path |
| --- | --- |
| Planner | Reconcile local commits, push, find/adopt/create the spec PR, apply spec labels and request reviewers. |
| Worker | Reconcile local commits, push, find/adopt/create the task PR, link the claim and request configured reviewers. Bot publication errors propagate instead of being mistaken for absent PRs or ignored metadata updates. |
| Fixer | Reconcile commits, push with expected-head protection, then consume structured repair results and observed thread fingerprints for replies/resolution. |
| Reviewer | Existing trusted native-review submission; summary comments, reactions and spec transitions remain daemon-owned. |
| Coordinator | Existing GitHub triage and dispatch adapters use the coordinator identity. This change does not add Forgejo coordinator support. |

Only prompt delegation changes. Runner `AllowAutoPush` and daemon hosting-tool
availability retain their existing meaning. `git_pr_lifecycle` is already
optional in completion parsing; no new push or PR mutation capability is
necessary. Existing task/summary-derived PR metadata and fixed role labels remain
the runner contract. Arbitrary agent-authored PR metadata is not a new capability.

This deletion prevents two writers from maintaining the same mutable PR
lifecycle. Its cost is accurate bot-specific prompts and coverage of daemon
fallbacks; adding agent-facing writes would instead duplicate publication policy.
Legacy runs retain their existing agent-managed lifecycle.

Shared hosting instructions describe authentication and available operations.
Planner reads its actual issue target; tasks without a PR receive no mandatory
PR reads. Reviewer/fixer retain their existing seed and drift instructions.
Ordinary worker PR reads do not assume reviewer-specific seed fields.

## One captured identity across a run

Discovery, targeted/webhook discovery and claimed execution bind a copied
identity definition and repository target into context. Capturing legacy mode
also prevents later config changes from turning an active personal run into a
bot run. A subsequent execution resolves the current configuration.

`hostingidentity.Manager` caches credentials in daemon memory and refreshes the
same captured App installation. `Session` validates the actor, target and
credential before each operation. Gateway and per-tick discovery caches include
the binding so different roles sharing a checkout cannot reuse another bot's
current-user or discovery results. No token file or identity database is added.

The authority is the operator's identity configuration and repository binding,
because credentials are execution policy rather than an agent task decision.
The copied context and cache partitioning cost propagation code and concurrent
refresh handling. Reading live configuration per operation would change active
accounts; a persisted ledger would add recovery state without solving a further
requirement.

## Agent capabilities reuse the review socket

`internal/forge/host_broker.go` reuses the private Unix-socket transport in
`trusted_review_proxy.go`, including bounded requests, four concurrent
connections, a two-minute operation deadline, cancellation and cleanup. The
executor supplies an absolute `LOOPER_HOST_CLI` plus the private socket path.
The socket captures the run identity, checkout and trusted executable paths;
callers cannot replace configuration, environment, provider, repository or CWD.

| Agent command | Operation and result |
| --- | --- |
| `"$LOOPER_HOST_CLI" host whoami` | Verified login, numeric ID, commit name/email, provider, repository and instance; no credentials. |
| `host api pulls/42` | GET a supported repository-relative endpoint. `--paginate` returns the complete bounded collection; `--diff` returns a PR patch. |
| `host threads 42` | All GitHub review threads with complete paginated comments. |
| `host thread 42 THREAD_ID` | One thread after verifying membership in the bound repository and PR. |
| `host git fetch REF` | Fetch one validated ref from the bound repository into the prepared checkout. |
| Existing `review submit REPO#NUMBER ...` | Native publication only when the daemon supplied the existing reviewer authority. |

The read allowlist in `host_reads.go` covers PR/issue context, reviews/comments,
requested reviewers, GitHub files/checks/status/comparisons, Forgejo diff/status,
and provider Actions runs/jobs/logs. JSON and text output are capped at 1 MiB;
pagination is capped at 50 pages and fails explicitly rather than truncating.
Absolute URLs, traversal, unsupported query parameters, arbitrary GraphQL and
API mutation methods are rejected. GitHub thread queries are server-authored and
preserve comment node IDs and `updatedAt` for the existing fixer fingerprint.
Provider limitations return explicit errors; Forgejo uses native review reads.

Ordinary API redirects are rejected. A job-log redirect is downloaded once by
the daemon over HTTPS without API authorization, with the same output limit;
the signed download URL is not returned to the agent.

The capability costs route validation, bounded pagination and socket lifecycle
code. Passing a publication token to the agent would violate the credential
boundary, while a general privileged CLI/API proxy would grant unrelated
operations. Removing agent-owned writes lets this remain a small read surface.

## Preserve native-review authority

`review.submit` retains the existing PR/worktree binding, expected head,
clean/blocking event policy, manual-run authorization, run marker, idempotency,
content and anchor checks. Read-only phases and non-reviewer roles receive no
review publication capability. There is no raw `POST .../reviews` alternative.
The authority remains the existing daemon-created `TrustedReviewProxyPolicy`;
this feature changes authentication, not task conclusions or review permission.

A trusted review child receives the captured definition through the existing
config file descriptor and calls `BindResolved`. GitHub children perform their
own same-installation token exchange/refresh through the shared credential
module. Forgejo children receive only the selected token reference value. Agent
processes receive neither the config descriptor nor the child's environment.
This preserves the existing CLI validation implementation without freezing a
token for the entire child or duplicating the submission service.

## Git and subprocess credentials

`hostingidentity.RunGH` and `RunGit` preserve legacy behavior when no bot is
selected. Bot calls use an explicit environment with only the selected
credential and private empty HOME/config directories, excluding personal CLI
credentials, `.netrc`, SSH agents and arbitrary inherited token variables.
Daemon Git uses HTTPS Basic authentication and keeps on-disk SSH origins intact.
Process-local rewrites of the verified origin and push URL preserve native Git
tracking semantics on supported older Git versions.

Bot GitHub CLI commands also run from the private empty directory. Their
repository and resource targets are explicit, so they do not need repository
CWD. Removing that dependency prevents GH versions that inspect local Git state
from invoking repository helpers with the selected token. It avoids maintaining
another list of repository helper settings to disable. Literal option values,
including review bodies and titles beginning with a dash, remain content;
repository/control validation applies to actual option positions.
An explicit `GIT_DIR` points to a nonexistent path inside the canonical private
directory. This prevents parent-repository discovery even when `TMPDIR` is inside
a checkout or reaches it through a symlink. It avoids path-list encoding and
adds no repository, persisted record or helper configuration checks.

Repository URL/HTTP/credential/custom-helper/partial-clone configuration that
could redirect the selected transport is rejected explicitly. Tokens never enter
remote URLs, command arguments or files. Credential-bearing GH/network-Git
commands are limited to the caller's deadline, four minutes, and the actual
installation-token lifetime minus clock slack, whichever is shortest. A
five-minute refresh window avoids starting normal operations with an expiring
token. Platform expiration is the authority for the limit; its cost is a bot-only
ceiling on individual network commands, without adding a background refresh job.

Local Git attribution sets bot user defaults and committer identity. It does not
force `GIT_AUTHOR_*`, so amend/cherry-pick preserve the original author; configured
commit name/email can override bot defaults. Local commands remain unprivileged.

Final agent environment sanitation runs after all configuration and per-run
overrides, including native-executor fallback. It removes named credential
references and hosting/config/trusted-channel variables, then injects only this
execution's capabilities and nonsecret attribution. Local Git/gh/tea command
routing prevents ordinary commands from silently using personal authentication.
Worker/fixer validation commands share the same credential-free environment
policy. Explicit empty shell environments no longer mean inherit everything.
Validation routing directories are retained when existing process containment
reports that descendant death is unconfirmed, matching agent-executor cleanup.
The original shell error is the authority for this exception. Its cost is a
retained temporary directory on a containment failure; deleting it would let a
surviving process fall through to personal command routing. No new recovery or
cleanup registry is introduced.

This is a credential routing boundary, not an operating-system sandbox against
arbitrary same-user code. It costs private temporary directories and routing
cleanup; final environment filtering alone would still permit stored personal
CLI authentication during ordinary subprocess execution.

## Verification and external limits

Local contracts cover real RSA signing and token refresh, all role inheritance,
discovery-to-publication bindings, configuration updates, identity-isolated
failures and caches, broker reads/rejections, trusted review compatibility,
executor cleanup, real HTTPS Git authentication/attribution and unchanged remote
configuration. A real local daemon test proves an unavailable App identity does
not prevent another project's legacy worker or daemon admission.

The separate `TestGitHubSandboxAppIdentity` CI scenario takes App ID,
installation ID and a private-key file reference. It mints a token itself,
checks repository scope, runs a daemon worker with credential-free agent reads
and an SSH-configured origin, checks PR/commit attribution, then refreshes and
checks comment/native COMMENT review attribution. The key stays outside test
artifacts and is always removed by the workflow.

No local App credentials were available, so local live execution is explicitly
excluded from this session's verification. App review-request eligibility and
search behavior, platform self-approval restrictions and GHES bot email remain
external constraints; this change does not claim to solve #598, #602 or #603.
