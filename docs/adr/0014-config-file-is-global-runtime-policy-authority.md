# ADR-0014: The layered config file is global runtime-policy authority

## Context

`looperd` historically loaded configuration once during bootstrap. Editing the
selected TOML, YAML, or JSON file therefore left the running daemon on stale
policy until it was restarted. The local dashboard also had no safe way to
manage configuration.

Some settings are policy read when work is claimed, while others own process
resources. Agent, role, default/safety, in-app/osascript notification,
disclosure, and selected scheduler settings can be snapshotted for new work.
The HTTP listener, SQLite coordinator, logger, webhook/network topology, daemon
supervision, runtime paths, provider/project bindings, and external startup
preflights cannot be replaced safely in place.

Projects have a separate existing authority: active SQLite project records, as
defined by ADR-0012. Treating `projects[]` as another live configuration source
would reintroduce the split-brain project model that ADR-0012 removed.

## Decision

The selected config file, overlaid by the daemon's captured startup environment
and CLI flags, is authority for global runtime policy. `looperd` reparses that
same fixed layer stack while running and publishes a candidate only when every
changed effective field is on the explicit hot-safe allowlist. Any invalid or
restart-bound change rejects the entire candidate and leaves the last-known-good
snapshot active.

The hot-safe allowlist contains agent vendor/model/environment and canonical
idle/max-runtime timeouts, named `agent.profiles.<id>` vendor/model identity
(and whole-profile set/unset of a single id), coding-role
`roles.{planner,worker,reviewer,fixer}.agent.{profile,vendor,model}` bindings,
selected scheduler concurrency/slow-lane
fields, in-app and osascript notifications, disclosure, selected default-policy
fields, `instructions.enabled`, curated role policy, and the Looper/osascript
tool paths. Profile and role agent vendor/model are hot-safe curated identity
fields for new claims; in-flight runs keep their frozen snapshot. Deprecated timeout and default-policy aliases remain watcher-hot
file-only compatibility representations of those canonical fields; they are
not dashboard-editable. Durable queue scheduling inputs—including the scheduler
retry budget/base delay, Reviewer quiet/minimum-publish timing, and Reviewer
retry-delay cap—remain restart-bound, as do the persisted Coordinator transient
retry budget and the materialized default base branch. The allowlist deliberately excludes
`agent.nativeResume`, `agent.params`, notification webhooks and Feishu, HITL,
`instructions.maxBytes`, Reviewer auto-merge, and Coordinator dependencies, in
addition to Coordinator activation and other resource-owning settings. New
fields are restart-bound until they are classified explicitly.

Leaving a configured vendor—by switching it or clearing it—is hot-safe only
when `agent.params` is empty and any explicit `agent.model` is changed or
cleared in the same candidate. This prevents an old profile from being
laundered through an intermediate nil vendor. Nil-to-vendor activation may use
a prepared profile when no previously configured vendor is being left. Work
that continues after a vendor transition retains durable checkpoint, worktree,
HITL answer, and human-inbox state but does not attach the old vendor's native
session ID to the new executable.

Each active run keeps the immutable configuration snapshot it started with. A
shared in-process publication boundary orders global config publication,
project-catalog mutation, and claim capture. A claim refreshes from the latest
catalog and config while holding the read side of that boundary, so every claim
made after publication constructs its run from the newly published snapshot.
The boundary is released before the run executes.

Native GitHub and Forgejo reviewer runs also bind `looper review submit` to that
same snapshot. The daemon sanitizes and JSON-encodes the complete materialized
project catalog and run policy in memory, with `server.localToken`, `agent.env`,
`agent.params`, and `daemon.environment` removed. Each trusted child receives
those bytes through an inherited read-only pipe plus the captured credential
environment; the agent receives only the per-run socket capability. A dedicated
child loader decodes that exact snapshot and deliberately bypasses the ordinary
file, environment, and CLI precedence layers. A missing, malformed, or oversized
descriptor snapshot fails closed.

The proxy replaces agent-supplied review-policy, expected-head, and manual-run
identity flags with the daemon-selected policy, checkpoint head, persisted loop
mode, and run ID. The review event itself remains agent-selected within the
bound policy. Connections, request/stdin bytes, and child output are bounded.
Cleanup closes partial connections, cancels active handlers, and kills an
already spawned child's process group before removing the socket. If the
snapshot or proxy cannot be installed, agent execution fails closed instead of
falling back to the live config file.

Project catalog/webhook/network snapshots publish inside that boundary after a
SQLite mutation commits. Scheduler wakeups and webhook-forwarder reconciliation
run afterward, outside the boundary, because reconciliation may issue multiple
external `gh api` calls. A global-policy publication also pushes the coherent
post-publication catalog snapshot into the long-lived network manager so its
next heartbeat advertises current role capabilities. The follow-up observes the
latest published snapshot.

Snapshot-built Coordinator runners share one daemon-lifetime `RuntimeState`
containing only synchronized poll timestamps and merge-watch mutexes. Policy
and gateways remain snapshot-local. This prevents rebuilding a runner for a
new configuration snapshot from resetting the Coordinator poll interval or
allowing duplicate merge-watch work.

Each immutable config snapshot builds its own notification gateway so active
runs cannot observe later notification policy. A scheduler-owned gateway
factory injects one synchronized, process-memory transport state into all of
those snapshot-specific gateways. The state contains only the cached Feishu
token and the message/card identifiers and live-tail data required to continue
updating notification artifacts created earlier; it contains no mutable
notification policy.

The dashboard edits only curated, field-level settings in the file layer.
Fields overridden by environment variables or CLI flags are read-only. Agent
environment values are write-only: the API returns configured key names, never
their values. Server tokens, daemon environment values, and arbitrary agent
parameters remain file-only. Project management remains on the Projects API and
SQLite catalog.

Every configuration read reports the content revision captured with the exact
file generation that produced its published snapshot. A dashboard patch must
submit that revision; a mismatch with the current source returns a conflict,
including when a newer on-disk edit has not yet been accepted by the reload
loop. The writer validates the candidate, writes and syncs a temporary file,
verifies source presence, regular-file identity, mode, and bytes immediately
before atomically replacing the source. Portable filesystems do not provide a
conditional compare-and-rename, so a manual editor racing in the tiny interval
between that final check and rename can still be replaced. The writer retains
unknown top-level extension sections,
their native scalar values, and the selected file format, but serialization may
canonicalize comments, quoting, and lexical key/table order. Ordinary permission
bits are retained,
but ACLs and extended filesystem metadata are not guaranteed to survive the
atomic replacement. A symlinked config path is readable for reload but cannot
be replaced through the dashboard.

When `local-token` authentication is configured, config mutation uses that
authentication. Without token authentication, `PATCH /api/v1/config` requires
both a loopback peer and a literal loopback Host authority and rejects
proxy-forwarding headers; request hostnames or a local reverse proxy alone do
not grant mutation authority.

## Trade-offs

### Vendor transition compatibility

- **Concrete failure prevented:** a Codex-to-Claude (or similar) switch must not
  run the new executable with an old vendor's wrapper/arguments, silently carry
  an old explicit model, or issue `--resume` with an incompatible native
  session identifier.
- **Cost:** edits that leave a configured vendor, including clearing it, are
  rejected when `agent.params` is non-empty or an explicit model was not
  changed. A cross-vendor
  continuation loses native conversation attachment and starts from its durable
  prompt/checkpoint and worktree instead. This adds no persisted state.
- **Why a simpler alternative is insufficient:** blindly retaining the profile
  is unsafe, auto-clearing fields would make the file stop reflecting the
  user's edit, and making every vendor selection restart-bound would also block
  the safe nil-to-vendor and implicit-profile cases.
- **Authority:** the newly published config snapshot is authority for the new
  run's vendor and profile. The runtime-authored vendor on the persisted HITL or
  agent-execution record is authority for which executable owns a captured
  native session; it is not inferred from the session string or agent prose.

### Publication boundary

- **Concrete failure prevented:** without one ordering boundary, a scheduler
  tick could begin before config publication but claim afterward with stale
  vendor/model/environment handlers. A project SQLite commit/catalog publish
  could also interleave with global config publication and expose a config and
  catalog pairing that was never validated together.
- **Cost:** the lock adds an ordering rule, a potential deadlock if future code
  violates that order, and brief contention among reloads, project mutations,
  and claim creation. External webhook reconciliation is eventual and may
  observe a newer published snapshot if another reload lands immediately after
  release. It adds no persisted state and is not held while an agent or `gh api`
  call runs.
- **Why a simpler alternative is insufficient:** a file compare-and-swap only
  orders file writers; it cannot order in-memory publication against claims or
  SQLite/catalog publication. Refreshing immediately before a claim without a
  boundary still leaves a race between the refresh and durable claim creation.

### Trusted review-submit snapshot

- **Concrete failure prevented:** an active reviewer agent can invoke the
  Looper CLI after the global file has changed. Letting that child reread the
  live file could publish with a different disclosure identity, project role
  gate, review-event policy, expected head, or manual-review identity than the
  run that authorized it. A named snapshot writable by the same UID could also
  be replaced before a credential-bearing child read it, while unbounded or
  partial socket requests could pin cleanup and child processes.
- **Cost:** every native review/publish agent execution owns one Unix socket,
  bounded connection bookkeeping, and an in-memory sanitized snapshot; every
  submit uses an inherited pipe and a separately cancellable process group.
  This relies on Unix descriptor/process-group semantics. Legitimate requests
  or diagnostic output above the fixed limits are rejected or truncated, and
  the explicit secret-clearing list must remain correct as config grows.
  Config-only provider credentials are copied into the trusted child
  environment, not the snapshot; this adds no persisted state or crash-leftover
  snapshot file.
- **Why a simpler alternative is insufficient:** binding only the two
  review-event flags leaves disclosure, provider/project resolution, and
  project-level publish gates live. A permission-restricted named file is still
  mutable by the same UID, the ordinary loader would reapply ambient or
  `agent.env` `LOOPER_*` values over the captured snapshot, and closing only the
  listener cannot stop partial connections or spawned descendants. Passing
  policy through an agent-visible environment variable remains forgeable.
- **Authority:** the scheduler's catalog/config snapshot captured for the claim
  is authority for the trusted child; the reviewer checkpoint head, persisted
  loop manual bit, and `AgentRunInput` run ID authorize expected-head/manual
  identity, while explicit loop review-event choices authorize the clean and
  blocking limits. Agent argv/output cannot select those values; it selects only
  the requested event within the bound policy.

### Coordinator runtime scheduling state

- **Concrete failure prevented:** rebuilding snapshot-specific Coordinator
  runners on every scheduler tick would reset the five-minute poll timestamp
  and per-issue merge-watch locks, causing discovery to run at scheduler cadence
  and allowing duplicate/costly external work.
- **Cost:** one daemon-lifetime object owns synchronized process-memory maps for
  project timestamps and issue mutexes. Like the pre-reload long-lived runner,
  it resets on daemon restart and retains entries for the daemon lifetime. It
  contains no policy and adds no persisted recovery state.
- **Why a simpler alternative is insufficient:** retaining the whole Runner
  would pin stale configuration and gateways, while rebuilding it without shared
  lifecycle state recreates the throttle/locking bug.
- **Authority:** the immutable config snapshot on each Runner remains authority
  for interval and Coordinator policy. Shared state records only when/where
  that authorized work last ran; it cannot enable work.

### File-content revision

- **Concrete failure prevented:** a dashboard left open on revision A must not
  overwrite a newer external edit B when its patch is submitted later.
- **Cost:** each successfully loaded file generation is hashed, clients need a
  conflict-and-refresh path, and a rejected on-disk generation cannot be edited
  through a dashboard snapshot that still shows the last-known-good values.
  The revision is only an equality signal. It is not persisted history, a lock,
  or proof that the proposed values are safe. A final identity/mode/byte check
  narrows but cannot eliminate the portable compare-to-rename race; operators
  must avoid simultaneous manual and dashboard writes.
- **Why a simpler alternative is insufficient:** rereading immediately before
  rename catches an editor that writes during the patch, but it cannot detect an
  edit made after the dashboard GET and before the PATCH begins.

### Canonical alias retirement

- **Concrete failure prevented:** unsetting a canonical dashboard field must not
  reveal a deprecated file-layer alias and silently resurrect the policy the
  user just removed.
- **Cost:** a canonical patch performs a small, targeted lexical migration by
  deleting only mapped alias leaves. Those deprecated spellings are not
  preserved when their canonical field is edited; unrelated legacy fields and
  extension sections remain intact. No new state is persisted.
- **Why a simpler alternative is insufficient:** normalization alone keeps the
  alias active after a canonical unset, while rejecting all legacy files would
  turn an otherwise safe compatibility edit into an unnecessary restart/migration
  event.
- **Authority:** the submitted canonical field operation is explicit user
  authority to retire only its known compatibility representations; no alias is
  inferred from effective runtime values.

### Shared notification transport state

- **Concrete failure prevented:** rebuilding a notification gateway for every
  immutable run snapshot would otherwise discard the token cache, HITL ask-card
  identity, live-feed identity, and final live tail. A later answer, progress
  update, or completion emitted through another snapshot could then fail to
  update the card or thread created earlier.
- **Cost:** the daemon owns synchronized process-memory maps and token state.
  Loop-scoped entries use a 4,096-loop least-recently-used cap, so an extremely
  old in-flight card may lose in-place update continuity after enough other
  loops become active. The state is not persisted, so continuity also ends when
  the daemon exits.
- **Why simpler alternatives are insufficient:** one mutable notification
  gateway would make active runs observe newly published policy and violate the
  immutable-snapshot contract. Persisting transport state in SQLite would add
  schema, recovery, retention, and stale-artifact failure modes—and risk keeping
  credentials or short-lived tokens at rest—for continuity that is only needed
  within one daemon process.
- **Authority:** the config snapshot captured for a run remains authority for
  whether and how that run uses notifications. Shared transport state cannot
  enable a channel or choose policy; it only addresses and updates transport
  artifacts that a config-authorized gateway previously created.

The remaining cost is a polling/reparse loop, a last-known-good in-memory
snapshot, an allowlist that must be maintained as configuration grows, and
field-level mutation code. A rejected external edit may remain on disk while
the daemon continues on the prior snapshot, so diagnostics must make the split
visible. File-layer values may also remain shadowed by captured environment or
CLI overrides.

Reading the file ad hoc in every consumer would avoid a publication mechanism
but permits one operation to observe incompatible versions. Restarting or
rebuilding resource-owning subsystems would expand failure modes around
listeners, database ownership, webhook locks, startup preflights, and active
agents. Applying only the hot-safe portion of a mixed candidate would make the
file cease to describe any coherent runtime state, so mixed candidates are
rejected as a whole.

## Authority

The authority for global runtime policy is the selected config file after the
captured startup environment and CLI overlays; active SQLite project records
remain authority for project existence and binding, and the dashboard is only a
validated editor of the global file layer.

For dashboard mutation specifically, the selected config file bytes are the
authority for what is being edited. The content revision identifies the bytes
that produced the published values the client observed; it is neither policy
authority nor authorization for the requested values.

## Consequences

- Safe external edits apply without changing the daemon PID or start time.
- Active runs retain their original config; claims after publication use the
  new snapshot.
- Dashboard writes are targeted, validated, revision-checked, and atomically
  replace a regular selected source file without materializing defaults,
  overrides, projects, or secrets.
- Environment/CLI-overridden fields are visible but not editable in the
  dashboard.
- Restart-bound or invalid edits keep running work healthy and produce a visible
  error with rejected field paths.
- New configuration fields must be classified explicitly before they become
  dashboard-editable or hot-reloadable.
