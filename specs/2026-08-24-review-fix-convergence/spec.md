# Review-fix convergence: scope-aware review, reactive dispositions, and bounded automation

## 1. Background

Looper already has several useful pieces:

- Reviewer prompts require a complete pass and finding accumulation.
- Reviewer and Fixer have independent live caps:
  `maxPublishesPerPR` and `maxPushesPerPR`.
- Fixer treats repository instructions and documented PR intent as scope
  authority, and can decline an out-of-scope review item.
- Native review data carries thread id/resolution state plus comment-level
  `updatedAt`, replies, and author association. GitHub's ReviewThread node does
  not itself expose `updatedAt`.
- Reviewer has a thread-resolution phase, while Fixer already fingerprints
  observed thread comments before mutating a thread.

Those pieces do not yet form a convergence protocol.

Two relevant features are disabled by default today. `HITL.Enabled` is `false`,
and both role budgets are skipped entirely when HITL is off. Reviewer
`ThreadResolution.Enabled` is also `false`; its default mode is report-only,
`RequireNewHeadSinceThread` is `true`, and `MaxThreadsPerRun` is `10`. A bounded
default cannot depend on operators discovering and enabling either feature.

[powerformer/nexus#56](https://github.com/powerformer/nexus/pull/56) is a useful
failure case: the PR received 17 Codex review passes, 29 inline comments, and 16
manual `@codex review` triggers over roughly 5 hours 22 minutes. The PR was also
large (83 files and about 9.9k added lines), so this spec does not claim every
comment was invalid. The relevant signal is the shape of the interaction:
independent findings arrived in small batches, later passes expanded into edge
hardening, and the system had no bounded clean-or-handoff outcome.

There is a second same-head failure mode. A human or Fixer can edit/reply to an
unresolved thread with a `wontfix`/decline decision, but Reviewer discovery
currently skips a PR when `lastPublishedHeadSha == currentHeadSha`. Existing
thread-resolution audit is also keyed primarily by head, not by the current
thread content. In addition, Fixer currently replies to a validated `declined`
decision, resolves the thread itself, and suppresses the same decline by a
head-bound fingerprint. Reviewer candidate selection then ignores the resolved
thread. Therefore an updated comment can be invisible until another commit, or
can disappear before Reviewer adjudicates it at all. The review-fix pair can
wait, repeat, or converge on the wrong authority even though the relevant input
has changed.

## 2. Product outcome

For each continuously followed PR with finite role caps, Looper must reach one
of these outcomes within the configured independent role budgets, whether HITL
is enabled or disabled. Setting a cap to `0` explicitly waives the finite-bound
guarantee for that role:

1. **Clean convergence:** every in-scope finding is fixed, explicitly disposed,
   or resolved, and Reviewer publishes the configured clean outcome.
2. **Human handoff:** the pair is non-runnable—parked with a Continue/Stop ask
   when HITL is enabled, or paused in a manual hold without an ask when it is
   disabled—with both meters, every remaining actionable thread, every pending
   scope dispute, and the exact next action.

Budget exhaustion is operational convergence, not code approval. Looper must
never publish a clean review, resolve a blocker, or enable auto-merge merely
because a budget was exhausted.

The full same-head thread-disposition guarantee applies to GitHub projects with
native review-thread capabilities. Providers without those capabilities still
receive bounded review-publication/Fixer-push behavior, but cannot claim the
same-head `wontfix` convergence path. In particular, Forgejo remains explicitly
exempt until its provider capabilities can supply the required thread identity,
authorship, edit timestamp, reply, and resolve operations.

## 3. Goals

1. Make the first Reviewer publication contain every independent, concrete,
   in-scope finding visible in the configured review scope.
2. Prevent follow-up passes from reopening untouched old diff for ordinary
   P2/P3 improvements after the first complete pass.
3. Prevent out-of-scope suggestions from becoming automatic Fixer work or
   resolvable PR threads by default.
4. Keep Reviewer and Fixer caps independently configurable and independently
   counted, while pausing/stopping the pair together when either cap trips.
5. React to review-thread edits/replies on an unchanged PR head, especially
   explicit human `wontfix` decisions and structured Fixer declines.
6. Make same-head disposition handling idempotent across polling, webhooks,
   retries, and daemon restarts.
7. Reuse existing loop metadata, event log, HITL, review markers, and thread
   resolution instead of adding a review-round ledger or workflow engine.
8. Enforce finite caps in the default `HITL.Enabled=false` configuration without
   silently dropping a finding that needs human judgment.

## 4. Non-goals (kill list)

1. A total comment-count cap that hides independent blockers
2. A single opaque `maxReviewFixRounds` replacing the two role caps
3. Automatic approval, merge, or thread resolution on budget exhaustion
4. Treating every issue found near changed code as required PR scope
5. Inferring product scope from changed ranges, test failures, labels, or GitHub
   state when documented intent says otherwise
6. Automatically creating GitHub issues for every suppressed follow-up
7. A new SQL table, epoch id, finding ledger, or persisted scope-status machine
8. Full rescans of the original PR diff after every Fixer push
9. Trusting arbitrary commenters or bot text as a `wontfix` authority
10. Forgejo same-head thread-disposition parity before Forgejo supports the
    required native thread identity, authorship, edit timestamp, reply, and
    resolve capabilities; Forgejo is exempt only from that part of §2, not from
    role-budget enforcement
11. Solving oversized-PR policy; PR size may be reported in handoff evidence but
    is not a new admission gate in this change

## 5. Authority

> The authority for whether a finding must be fixed in this PR is repository
> instructions, the PR's documented intent and linked issue/specification, and
> correctness or safety invariants directly affected by the PR—not the
> Reviewer's structured output.

The authority order is:

1. Repository instructions, including `AGENTS.md`
2. PR title/body, explicit goals and non-goals, and linked issue/specification
3. A regression introduced by the PR or an invariant directly affected by its
   changed behavior
4. Existing intentional PR decisions that predate the current review/fix dispute

Reviewer output is a structured claim with evidence. Fixer must still verify it.
Changed ranges are location scope, not product scope.

For an explicit human disposition, the authority is the PR author or a provider
identity whose author association is `OWNER`, `MEMBER`, or `COLLABORATOR`.
GitHub exposes `authorAssociation` and `updatedAt` on
[PullRequestReviewComment](https://docs.github.com/en/graphql/reference/pulls#pullrequestreviewcomment).
Arbitrary external users and bots are not disposition authorities. A Looper
Fixer reply with a validated `declined` marker is a scope-dispute signal, not
human authority; Reviewer must adjudicate it.

## 6. Decisions (locked)

| Topic | Decision |
| --- | --- |
| Reviewer/Fixer meters | Remain separate: publications vs pushes |
| Default caps | Lower both defaults from `8` to `3`; explicit config is unchanged |
| Disabled cap | `0` still disables only that role's cap |
| Pair behavior | Either exhausted role halts both roles in the same lane: park with a HITL ask or pause in a no-ask manual hold |
| Continue | Replenish only meters currently at/over their live cap; preserve unused sibling budget |
| Stop | Terminate both roles in the lane |
| Budget enforcement | Independent of `HITL.Enabled`; HITL selects answer delivery, not whether the pair halts |
| HITL disabled at cap | Pause both roles in a paired manual hold, cancel queues, persist counters/reason + handoff event, and notify action-required; no ask or automatic resume |
| No-HITL resume | `looper unpause` on either budget-held role delegates to paired Continue; `looper stop` delegates to paired Stop |
| `needs_human` with HITL disabled | Pause the pair with reason `review_scope_human_required` and the same handoff evidence; never suppress and continue |
| First pass | Full configured PR scope; accumulate before publishing |
| Later code pass | Prior findings + new head delta + affected invariants |
| Later thread pass | Changed thread feedback only; no full code rescan by default |
| Finding disposition | `must_fix`, `follow_up`, or `needs_human` |
| Remote comments | Only `must_fix` becomes actionable PR feedback by default |
| `follow_up` | Retained in the run result/event; no remote comment or Fixer item by default |
| Scope ambiguity | `needs_human`; no code or thread mutation until answered |
| Human command | Canonical `/looper wontfix <reason>`; exact standalone `wontfix`, `won't fix`, and `won’t fix` are compatibility aliases |
| Fixer decline | Reply with evidence and leave unresolved; Reviewer owns accept/reject/resolve |
| Disposition feature gate | Always on for continuous Looper-authored GitHub threads; legacy ThreadResolution config continues to govern objective-fix reconciliation only |
| Same-head gate | `RequireNewHeadSinceThread` never blocks a human/Fixer disposition change |
| Same-head reactivity | Review signal includes current thread feedback, not only head SHA |
| Budget at human reply | Either hold may run disposition-only reconciliation, but it cannot release the hold, refill a meter, publish a review, or push code |
| Persistence | One review-signal fingerprint in existing loop metadata; no new table/ledger |
| Queue dedupe | Keep the stable per-loop/PR dedupe key; coalesce the latest signal into one active item |

## 7. Reviewer convergence protocol

### 7.1 First pass: exhaustive within authority scope

A pass is an initial pass when no validated prior Looper review marker exists for
the PR lane.

Before publication, Reviewer must:

1. Read repository instructions, PR intent, linked specification when available,
   complete configured diff scope, prior review history, and necessary context.
2. Accumulate candidate findings without publishing.
3. Classify every candidate by disposition, severity, scope basis, and concrete
   evidence.
4. Deduplicate only the same root cause or a genuinely repeated pattern.
5. Keep unrelated concerns separate even when there are many of them.
6. Submit one review containing every independent `must_fix` finding found in
   that pass.

Remove prompt language that says to “prefer fewer” comments and remove numeric
comment-flood thresholds as completeness signals. Grouping is valid only for a
shared root cause with representative locations; it must not hide unrelated
findings. The budget limits review publications, not findings per publication.

The Reviewer is not required to find unknowable issues. It is required not to
intentionally defer an already observed in-scope issue to a later publication.

### 7.2 Follow-up code pass: review the repair frontier

When a validated prior Looper review exists and the head changed, Reviewer must
inspect:

- every unresolved prior `must_fix` thread;
- the diff from the last reviewed head to the current head;
- directly affected call sites, contracts, tests, and lifecycle invariants;
- evidence that the Fixer actually addressed each prior finding.

Reviewer must not rescan untouched original diff merely to invent ordinary new
hardening work. A newly discovered issue in untouched old diff is handled as:

| Late finding | Disposition |
| --- | --- |
| Clear security, data corruption/loss, broken public contract, or P0/P1 correctness blocker | Publish as `must_fix`, mark as late discovery, and include it in convergence evidence |
| Ordinary P2/P3 robustness, cleanup, style, or independent improvement | `follow_up`; do not feed the current Fixer loop |
| Scope or severity is genuinely ambiguous | `needs_human` |

Continue/reset of a budget meter does not reset this review frontier.

### 7.3 Finding disposition contract

Every candidate uses this logical shape:

```json
{
  "disposition": "must_fix | follow_up | needs_human",
  "severity": "blocking | non_blocking | nit",
  "scopeBasis": "stated_intent | introduced_regression | required_invariant | independent_improvement | ambiguous_intent",
  "scopeEvidence": "specific repository rule, PR goal/non-goal, linked spec section, or regression evidence",
  "path": "internal/example.go",
  "line": 42,
  "problem": "concrete problem",
  "why": "observable consequence",
  "suggestedChange": "smallest complete repair"
}
```

Publication rules:

- `must_fix`: publish as inline feedback when anchorable; it becomes Fixer input.
- `follow_up`: retain in the structured completion/event record. Do not create a
  GitHub review thread or top-level action item unless repository instructions
  explicitly require all such observations to be published.
- `needs_human`: do not publish it as a change request and do not let Fixer edit.
  With HITL enabled, park the pair with the conflicting evidence and exact
  question. With HITL disabled, pause the pair in a manual hold with reason
  `review_scope_human_required`, the same structured handoff evidence, and an
  action-required event/notification. The handoff must say which authority
  input must change before `looper unpause`; never suppress the finding and
  continue.

For native review submission, each actionable comment must carry transient
`disposition`, `scopeBasis`, and `scopeEvidence` fields. The trusted review-submit
Seam rejects missing fields and rejects `follow_up`/`needs_human` in an
actionable comment payload. The provider Adapter removes Looper-only fields
before calling GitHub/Forgejo. The fields are a declaration and validation
surface, not the authority itself.

The actionable review body may summarize published `must_fix` findings but must
not smuggle suppressed findings into unstructured top-level prose. Comment-only
publish mode must apply the same dispositions before it posts remote content.

### 7.4 Fixer remains a second scope check

Fixer keeps its current scope authority and may return `declined` or
`needs_human` even when Reviewer emitted `must_fix`.

When Fixer declines with concrete scope evidence:

- it makes no code change for that item;
- it replies on the existing thread through the structured response path;
- it does **not** resolve the thread or treat its own output as dismissal
  authority;
- it records the existing decline marker as pending Reviewer adjudication, not
  as a terminal suppression for the current head;
- Reviewer treats the reply as a changed disposition signal;
- Reviewer must not open a duplicate thread for the same fingerprint;
- Reviewer either accepts the decline, rejects it with new authority evidence,
  or requests HITL.

Only Reviewer `accept_wontfix` (or an already-resolved human action) makes the
decline terminal. Reviewer `reject_wontfix` invalidates Fixer's pending decline
suppression for that exact feedback fingerprint and returns the existing thread
to Fixer; it does not create a new thread.

A second attempted fix to the same subsystem or a reviewer/fixer scope conflict
that repeats without new evidence is `needs_human`, not another automatic round.

## 8. Same-head `wontfix` and decline reactivity

### 8.1 Feature ownership and existing ThreadResolution config

Same-head scope disposition is a narrow convergence path, not an implicit
enablement of the broader optional ThreadResolution feature.

It runs only when all are true:

- the Reviewer loop is continuous;
- the provider exposes the required native thread capabilities;
- the thread is Looper-authored; and
- there is a changed trusted-human disposition or validated Fixer decline.

Existing settings retain these meanings:

| Existing setting | Locked meaning after this change |
| --- | --- |
| `ThreadResolution.Enabled` | Gates general objective-fix reconciliation only; it does not disable the narrow disposition path |
| `Mode`, `AutoResolve`, `RequireAuditComment` | Govern objective-fix report/reply/resolve behavior only |
| `RequireNewHeadSinceThread` | Applies to objective code-fix detection only; a changed disposition signal bypasses it |
| `RequireCurrentReviewRequest` | Applies to general objective reconciliation/full review; an explicit Looper-thread disposition is targeted work intent and may receive the narrow adjudication without a fresh review request |
| `MaxThreadsPerRun` | Shared batch-size ceiling for classifier work; default remains `10` |

Therefore the defaults remain `Enabled=false`, report-only,
`RequireNewHeadSinceThread=true`, and `MaxThreadsPerRun=10`. The new path does
not start scanning or resolving arbitrary stale threads. Its only remote
mutations are an audited accept/reject reply and, for `accept_wontfix`, resolution
of that same Looper-authored thread.

### 8.2 Review signal identity

`lastPublishedHeadSha` remains publication/audit metadata, but it is no longer
the sole discovery identity for a continuous Reviewer loop.

Define:

```text
threadFeedbackFingerprint = sha256(canonical Looper-authored review threads)
reviewSignalFingerprint   = sha256(headSha + threadFeedbackFingerprint)
```

The canonical thread input includes, in stable thread/comment order:

- thread id and `isResolved`;
- each comment id, author, author association, `createdAt`, `updatedAt`, and a
  hash of normalized body content;
- original/current commit ids when available.

Every Reviewer thread-resolution or disposition reply must carry the extended
`looper:thread-resolution` audit marker. Exclude all such Reviewer audit replies
from the fingerprint, otherwise accept/reject replies trigger themselves.
Include the original Looper review comment, trusted human edits/replies, and
validated Fixer fixed/declined replies. Keep `isResolved` in the input because a
human resolve/reopen is real state change.

Store `lastReviewedSignalFingerprint` in existing Reviewer loop metadata, plus
`lastResolvedReviewThreadIDs` as the set of thread IDs that were resolved in
that same snapshot (reopen detection only). Do not persist comment bodies or a
per-thread decision ledger. The queue dedupe key stays stable at the existing
per-loop/PR identity and does **not** include the fingerprint. The signal
fingerprint travels in queue payload/checkpoint data and in remote
idempotency/audit markers.

For a single-batch disposition, after Reviewer performs its own reply/resolve
mutation, it must:

1. refetch the authoritative, fully paginated relevant thread set;
2. recompute the post-mutation fingerprint, including the new `isResolved`;
3. persist the checkpoint and promote the post-mutation fingerprint to
   `lastReviewedSignalFingerprint` in one fail-closed local commit before
   completing the run.

If the refetch or local persistence fails, do not run the classifier again just
because Reviewer's own mutation changed the signal. Retry observes the existing
remote audit marker, completes the missing post-mutation fingerprint commit,
and remains idempotent.

A webhook caused by Reviewer's own mutation may race that local commit. It may
coalesce discovery work, but after the commit the Reviewer admission check must
recognize the already-recorded post-mutation signal and exit before starting an
agent execution.

When more than `MaxThreadsPerRun` candidates exist, process stable batches. Each
partial batch stores its refetched post-mutation fingerprint and completed
thread ids in the active run checkpoint, but does **not** promote it to loop
`lastReviewedSignalFingerprint`. Requeue a continuation through the same stable
queue item. Promote only after every candidate from the captured signal is
adjudicated or has a terminal per-thread outcome and a final full refetch is
stable. A partial batch must not make the remaining threads invisible.

The active checkpoint is a crash-recovery cursor, not decision authority.
Current provider thread state plus Looper's remote audit markers remain the
authority for whether a thread mutation already happened; any freshness drift
invalidates the cursor before another mutation.

If external feedback arrives between batches, freshness detection invalidates
the captured set, retains already idempotent remote decisions, and coalesces the
latest complete signal. It must not promote either the stale captured signal or
an intermediate post-mutation signal.

### 8.3 Disposition directives

The canonical command is:

```text
/looper wontfix <reason>
```

Directive parsing applies only inside Looper-authored review threads. For
compatibility, a trusted human comment whose entire non-quoted content is
`wontfix`, `won't fix`, `won’t fix` (Unicode apostrophe), or any of those
followed by `: <reason>` is equivalent. Do not interpret incidental prose
containing the word as a command.

`/looper reconsider <reason>` cancels the latest accepted disposition for an
unresolved/reopened thread. The latest valid trusted-human directive after the
last Reviewer audit is the input to adjudicate.

### 8.4 Reviewer decisions

Extend thread reconciliation beyond objective code resolution:

| Decision | Required evidence | Remote action |
| --- | --- | --- |
| `objectively_fixed` | Requested behavior is verifiably present at current head | Existing audit/resolve policy |
| `accept_wontfix` | Finding is outside documented PR scope, optional, incorrect, or superseded by trusted intent | Reply with scope evidence and resolve thread |
| `reject_wontfix` | Finding is required by repository rule/PR intent or is a directly introduced correctness/safety regression | Reply on same thread with concrete authority; keep unresolved |
| `needs_human` | Authority conflicts, reason is missing, or evidence is ambiguous | Halt pair; ask if HITL is enabled, no-ask manual hold otherwise; no later mutation |
| `not_fixed` | No disposition applies and requested behavior is still absent | Keep unresolved; Fixer remains eligible |

The classifier prompt must receive PR title/body, relevant linked intent when
available, repository instructions, current head, and full candidate thread
history. An author reply like “fixed” remains insufficient objective evidence;
an explicit trusted `wontfix` is a request for scope adjudication, not automatic
acceptance.

The existing `looper:thread-resolution` audit identity becomes keyed by
`threadId + headSha + threadFeedbackFingerprint + decision`. Every remote
Reviewer adjudication reply (`accept_wontfix`, `reject_wontfix`,
`objectively_fixed`, or `not_fixed`) carries the extended marker and is excluded
from the next fingerprint. A `needs_human` outcome writes the same identity to
the local checkpoint/handoff but posts no remote adjudication reply. A prior
audit for the same head but older comment fingerprint must not suppress a new
decision.

The automatic dispute quota is one Reviewer rejection per unchanged adjudicated
input. If Fixer emits `declined` again after `reject_wontfix`, and the head plus
canonical human/original-finding feedback are unchanged after excluding Looper
Fixer-decline and Reviewer-audit coordination replies, force `needs_human`.
Another Reviewer/Fixer argument is forbidden. A changed head or changed trusted
human directive creates a new adjudicated input.

### 8.5 Discovery, coalescing, and responsiveness

Correctness must not depend on webhooks:

1. Polling discovery checks thread feedback for active continuous Reviewer
   loops, even when the code head is unchanged.
2. GitHub `pull_request_review_comment` create/edit/delete and
   `pull_request_review_thread` resolve events trigger targeted discovery as an
   acceleration path; these are documented
   [GitHub webhook events](https://docs.github.com/en/webhooks/webhook-events-and-payloads#pull_request_review_comment).
3. A changed review signal queues a thread-disposition pass after the configured
   **Reviewer** quiet period (default `60s`); it does not use the Fixer quiet
   period (default `0`) and does not wait for another commit.
4. The thread-disposition pass does not perform a full code rescan unless the
   decision itself exposes a code-scope conflict requiring one.
5. `minPublishIntervalSeconds` applies to a new PR review publication, not to a
   disposition-only classifier run.

Responsiveness target: absent provider/rate-limit failure, a same-head trusted
directive should start adjudication within one discovery interval plus the
configured quiet period. Webhook mode should normally be faster.

Keep exactly one active Reviewer queue item for a PR/lane:

- Keep the existing stable dedupe key
  `reviewer:{project}:{loop}:{repo}:{pr}`.
- If the item is queued/retry-wait, update it in place to the latest head/signal
  and extend its eligibility with
  `DebounceSchedule(now, reviewerQuietPeriod, existingAvailableAt)`.
- If the item is running, retain only the latest pending signal. The running
  pass must freshness-check before mutation; on completion/drift it schedules
  one continuation for the latest signal.
- Never create old-signal and new-signal active items side by side. Because the
  fingerprint represents complete current state, coalescing intermediate
  signals cannot lose work.

Polling may use PR `updatedAt` and a short-lived thread-list cache as a
non-authoritative negative-cache hint. An unchanged hint may skip a thread list
only until the cache TTL; a forced periodic full refresh remains required.
`updatedAt` never advances `lastReviewedSignalFingerprint` and never proves that
thread state is unchanged.

Webhook implementation is not a one-case addition:

- route `pull_request_review_comment` to both Reviewer and Fixer lanes instead
  of the current Fixer-only route;
- add `pull_request_review_thread` to `routeDelivery` and route `resolved` to
  both lanes;
- add the event to every runtime forwarding/subscription list, including
  `internal/runtime/webhook.go`, `internal/runtime/webhook_forwarder.go`, and
  the CLI/netadmin hook event lists;
- update advertised event lists and forwarder contract tests together so a
  managed hook and an external forwarder subscribe to the same events.

### 8.6 Fixer race and same-head clean outcome

An unresolved thread with a new, unaudited trusted disposition directive or a
validated Fixer decline is temporarily withheld from further Fixer edits.
Reviewer has first right to adjudicate it.

- Accepted `wontfix` removes the thread from Fixer input by resolving it.
- Rejected `wontfix` restores it as actionable without opening a new thread.
- A Fixer decline follows the same accept/reject path; Fixer never resolves its
  own scope dispute.
- `needs_human` parks the pair in both modes: with an ask when HITL is enabled,
  or with a structured no-ask manual hold when it is disabled.
- If all blockers disappear without a code-head change, Reviewer may run a
  same-head convergence pass and publish the configured clean outcome. Review
  admission is keyed by the new review signal, so head-only idempotency must not
  suppress it.

Fixer's existing pre-mutation thread snapshot/drift check remains mandatory. If
a comment changes after Fixer starts, Fixer must make no thread mutation from
the stale decision and must return to discovery.

### 8.7 Interaction with budget hold

A budget-held pair may run a disposition-only reconciliation in response to a
new trusted human directive or validated Fixer decline, whether or not HITL is
enabled. This is allowed because it cannot push code or publish a new PR review
and can only reduce or clarify the outstanding set.

It does not reset either meter, release the pair, or imply `Continue`. A new
clean/finding review publication and every Fixer push remain held until an
explicit Continue. The handoff brief must refresh when disposition state
changes.

When HITL is disabled, no background disposition run may revive the pair. A
trusted human edit can resolve a scope dispute and update the handoff, but
continuing counted automation still requires `looper unpause` on the held pair.
Polling, webhooks, daemon restart, and enabling HITL are never interpreted as an
implicit Continue.

## 9. Independent budgets with a paired circuit breaker

### 9.1 Configuration and counting

| Path | Unit | New default |
| --- | --- | --- |
| `roles.reviewer.behavior.loop.maxPublishesPerPR` | Successful native/top-level review publications | `3` |
| `roles.fixer.behavior.loop.maxPushesPerPR` | Successful head-changing Fixer pushes | `3` |

These are independent meters because the work units and risk differ. A project
may configure Reviewer `2` / Fixer `5`, Reviewer `5` / Fixer `2`, or disable one
cap with `0`.

Do not introduce a “round” counter. Reviewer and Fixer are asynchronous: a
review may produce no Fixer push, one push may address many reviews, and a human
thread disposition may change convergence without a commit. “Round” has no
stable authority.

Thread-disposition audit replies are not Reviewer review publications. Failed
agent executions, rejected outbound payloads, no-op Fixer runs, and failed
pushes do not consume these meters; their existing retry/terminal policies still
apply.

### 9.2 Which loops participate

Budget participation is based on continuity, not only `manual`:

```text
participates = automatic || followUpdates == true
```

- Automatic Reviewer/Fixer loops participate.
- `looper takeover` loops (`manual=true`, `followUpdates=true`) participate.
- One-shot manual loops (`manual=true`, `followUpdates=false`) remain exempt.

Pair only loops in the same derived lane:

- `automatic`
- `continuous_manual`

Do not cross-pair a takeover loop with an automatic loop. Preserve the invariant
of at most one non-terminal loop per role per PR/lane; discovery coalesces
duplicates. No persisted pair id is added unless implementation evidence proves
that invariant cannot be maintained.

### 9.3 Exhaustion

Before any counted mutation and after every successful counted mutation, read
live caps and current counters. Product terminal states (closed/merged PR,
accepted clean outcome, explicit stop) win over opening a budget ask.

Budget admission does not check `HITL.Enabled`; caps are always enforced. When
either role is at its live cap and more counted work is required:

1. Cancel pending queue entries for both roles in the lane.
2. Persist counters/reason and append the handoff event described below.
3. Emit an event and action-required notification.
4. Do not allow discovery or daemon restart to revive either side.

The hold presentation depends only on whether HITL answer delivery is enabled:

| `HITL.Enabled` | Exhausted role | Sibling | Resume path |
| --- | --- | --- | --- |
| `true` | `awaiting_human` with one Continue/Stop budget ask | budget-paused | Answer the existing ask |
| `false` | `paused`, reason `review_fix_budget_exhausted`, no ask | paired budget-paused | `looper unpause <either-seq>` for Continue or `looper stop <either-seq>` for Stop |

Do not use `human_takeover` for the HITL-disabled case. That status means a real
interactive agent session is pinned and can be handed back; a budget hold has
no such session. Do not use `terminated` for the initial no-HITL handoff
either: it is irreversible and would force the operator to reconstruct the
continuous lane even though the existing paired pause is sufficient.

### 9.4 Continue and Stop

`Continue` is a role-specific refill with pair-wide release. It comes from the
HITL answer when enabled; when disabled, `looper unpause` on either member of a
budget-held pair must delegate to the same budget Continue operation instead of
the generic single-loop resume:

- Reset every meter that is currently at/over its live cap.
- Preserve a sibling meter that still has unused budget.
- Clear the budget ask and pair pause only after all required resets succeed.
- Requeue both eligible sides against current PR/thread state.

This avoids wasting unused Fixer budget when Reviewer alone exhausts, while also
avoiding an immediate second HITL when both meters are already exhausted.

`Stop` terminates both roles in the lane and cancels their queue work. It does
not resolve threads, approve, merge, or delete worktrees outside existing
terminal cleanup policy. For a no-HITL budget hold, `looper stop` on either role
must delegate to the paired Stop operation.

For a no-HITL `review_scope_human_required` hold, the handoff names the
repository/PR/thread authority evidence that must be clarified. `looper
unpause` resumes the pair against current evidence without resetting a meter
unless one is also exhausted. If the authority input is unchanged, admission
returns to the same manual hold without a counted mutation.

Pair transitions must be idempotent and fail closed. A partial storage/queue
failure must leave both roles non-runnable until reconciliation completes. Do
not unpause a sibling before the exhausted counters and pair transition are in
a safe state. In HITL-disabled mode, a partial paired hold likewise cannot leave
one role runnable.

### 9.5 Handoff brief

The budget ask (when enabled), event/notification, and CLI/dashboard detail must
show:

- PR, lane, current head, and last reviewed signal;
- Reviewer publications: `count / live cap`;
- Fixer pushes: `count / live cap`;
- which role(s) are exhausted;
- unresolved `must_fix` thread links/titles;
- pending `wontfix`/decline decisions and their latest trusted reason;
- late-discovery blockers;
- last Reviewer/Fixer outcome and whether progress decreased the outstanding
  set;
- exact semantics: Continue refills exhausted meter(s); Stop terminates both.

When HITL is enabled, the only required ask choices remain `Continue` and
`Stop`. When HITL is disabled, the handoff has no answer widget; it shows the
exact `looper unpause` and `looper stop` commands instead. Better evidence is
more valuable than another configurable answer matrix.

Do not add a persisted handoff record. The action-required event payload
captures the decision-time snapshot; loop metadata remains authority for
counters/reason; CLI/dashboard may refresh thread links and PR state from the
provider. This keeps historical evidence without another state copy to
reconcile.

## 10. Module design

### 10.1 Reviewer convergence Module

Deepen the existing Reviewer Module instead of adding a workflow layer.

- **Interface:** phase-specific review instructions, structured finding
  validation, review-signal calculation, and thread-disposition decisions.
- **Implementation:** full-pass accumulator, repair frontier, scope evidence,
  same-head signal discovery, and audit idempotency stay local to Reviewer.
- **Seam:** trusted review-submit accepts only validated actionable
  findings.
- **Adapter:** existing GitHub/Forgejo publishers map accepted fields to provider
  payloads; no new provider interface is justified.
- **Depth:** callers see one review signal and one disposition result, while the
  Module hides prompt, fingerprint, frontier, and provider-publication details.
- **Leverage:** the same finding rules serve native review and comment-only
  modes; the same signal drives polling and webhook-targeted discovery.
- **Locality:** scope and convergence changes should not require edits across
  Reviewer, Fixer, scheduler, and every provider Adapter.

The deletion test is positive: delete head-only discovery authority, numeric
comment batching language, and direct publication of unclassified findings.

### 10.2 Review-fix budget Module

Deepen `internal/loops/review_fix_budget.go`.

- **Interface:** check admission, halt a lane, apply Continue/Stop when
  available, and build the handoff brief.
- **Implementation:** lane matching, independent meter reads/resets, queue
  cancellation, HITL ask vs no-ask paired pause, pair status
  transitions, handoff evidence, and idempotent recovery live together.
- Reviewer and Fixer runners report successful counted mutations; they do not
  implement their own sibling/pause/reset ordering.

Do not add a Go interface with one implementation. Exported functions on the
existing Module are sufficient until a second implementation exists.

## 11. New-concept trade-offs

### 11.1 Structured finding disposition

**Failure prevented:** Reviewer publishes a plausible but independent
improvement; Fixer treats the remote thread as mandatory; both roles spend
rounds arguing about work the PR never promised.

**Cost:** new transient result fields, validator branches, prompt fixtures, and
provider payload stripping must remain compatible across native and
comment-only publication. The model may still provide bad evidence, so Fixer
must retain its independent check.

**Why simpler alternatives are insufficient:** prompt-only wording is already
present and is not an enforceable outbound Seam. Inferring scope from diff
or GitHub state would make infrastructure pretend to be product authority.
Structured agent output plus explicit documented evidence is the smaller path.

### 11.2 Review-signal fingerprint

**Failure prevented:** a human edits/replies `wontfix` or Fixer posts a decline
on an unchanged head; Reviewer keeps skipping by head SHA; unresolved state and
the review-fix loop never converge.

**Cost:** polling active thread lists consumes provider quota; canonicalization
must handle edits, deletes, pagination, bot/audit exclusions, Reviewer
self-mutations, partial `MaxThreadsPerRun` batches, and provider timestamp
quirks; one new loop-metadata field plus queue payload/coalescing state must
remain consistent across restart and webhook/poll races. Existing active-run
checkpoint storage also carries a partial batch's completed thread ids and
post-mutation fingerprint until final promotion; this is another retry path to
test, but not a new per-thread history ledger.

**Why simpler alternatives are insufficient:** head SHA cannot represent thread
state. PR `updatedAt` is too broad and is not a reliable per-thread authority.
Webhook-only detection loses events during daemon/network downtime. Persisting a
full thread ledger would add much more state and recovery surface than one
content fingerprint. A bounded cache and PR `updatedAt` are useful negative
cache hints, but a periodic full refresh is still required.

### 11.3 Always-on narrow disposition path

**Failure prevented:** the same-head protocol is implemented inside the
existing ThreadResolution feature, which remains disabled by default, so the
documented convergence behavior never runs for default users.

**Cost:** continuous GitHub Reviewer loops gain a narrow default thread query,
classifier, reply, and accepted-disposition resolve path. It must coexist with
legacy ThreadResolution modes without making arbitrary stale threads eligible.

**Why simpler alternatives are insufficient:** enabling all ThreadResolution by
default would broaden behavior far beyond `wontfix`/decline convergence and
would change report/resolve policy for existing users. Leaving the new behavior
behind `Enabled=false` makes §2 false. A capability-checked, Looper-authored,
changed-disposition-only path is the smaller Interface.

### 11.4 Fixer-decline handshake

**Failure prevented:** Fixer decides a Reviewer request is out of scope, then
uses its own structured output as authority to resolve and suppress the thread.
Reviewer cannot accept or reject that scope dispute, and the PR can become
stuck or falsely clean.

**Cost:** a declined thread stays unresolved for one additional adjudication
pass; Fixer suppression must distinguish pending, accepted, and rejected audit
evidence; retry tests must cover a crash between reply and Reviewer action.

**Why simpler alternatives are insufficient:** allowing Fixer to reply and
resolve is fast but assigns authority to the party disputing the request.
Leaving the thread unresolved forever just moves the deadlock. Reusing the
existing decline reply and thread-resolution audit markers creates a two-phase
handshake without a new persisted status field or table.

### 11.5 Paired circuit breaker over independent meters

**Failure prevented:** one role exhausts but the sibling continues publishing or
pushing, or two nearly simultaneous exhaustions produce contradictory HITL
asks.

**Cost:** pair-wide queue/status transitions and partial-failure recovery are
cross-loop lifecycle behavior and require integration coverage. Default
HITL-disabled users now enter a paired manual hold at a finite cap instead of
silently ignoring it, so migration, pair-aware `unpause`/`stop`, and
action-required observability are product-visible.

**Why simpler alternatives are insufficient:** fully independent pauses permit
the other role to mutate stale state. One shared counter loses the flexible
Reviewer/Fixer risk budgets requested by operators. Keeping enforcement behind
HITL would preserve compatibility by making the default cap fictional. A
notification without halting would still allow unbounded mutation. Independent
meters with one paired halt keep flexibility and the finite guarantee.
Terminating a no-HITL pair would discard an otherwise resumable lane; a generic
single-loop unpause would either trip the same exhausted cap immediately or
revive only one side. Reusing the paired budget pause and routing unpause/stop
through the existing budget Module avoids both failures. No epoch id or ledger
is needed; existing counters plus event history are sufficient.

## 12. Failure modes and required behavior

| Failure | Required behavior |
| --- | --- |
| Reviewer omits disposition/scope evidence | Trusted Seam rejects before remote publication; retry same signal |
| Comment changes while Reviewer/Fixer runs | Freshness check detects fingerprint drift; no stale reply/resolve; rediscover |
| Same-head directive arrives after prior audit | New fingerprint invalidates old audit and queues adjudication |
| Reviewer reply/resolve changes its own thread | Refetch and commit post-mutation fingerprint; do not spend another classifier run |
| More than `MaxThreadsPerRun` candidates | Continue stable batches; do not advance stored signal until all captured candidates terminate |
| Several signals arrive while one item is queued/running | Coalesce latest signal into the one stable-dedupe work item; never run stale items side by side |
| Untrusted user/bot says `wontfix` | Treat as context, not authority; do not auto-resolve |
| Trusted `wontfix` conflicts with repository safety rule | `reject_wontfix` or `needs_human`; never silently accept |
| Fixer emits `declined` | Reply with evidence, leave unresolved, and queue Reviewer adjudication |
| Fixer declines again after one unchanged-input rejection | Force `needs_human`; no third automatic argument |
| Thread is manually resolved before mutation | Treat as idempotent success; do not recreate it |
| Thread is reopened or accepted directive edited/deleted | Fingerprint changes; adjudicate current state |
| Provider thread listing is partial/rate-limited | Fail retryably; do not advance stored review signal |
| Budget config lowers below current count | Halt on next admission check: ask-backed park with HITL, no-ask paired pause without it |
| HITL-disabled budget exhausts | Pause pair, persist handoff/event, notify, and require pair-aware unpause/stop; never continue silently |
| HITL-disabled `needs_human` occurs | Pause pair with unresolved evidence; never discard the finding or auto-resume |
| Handoff notification delivery fails | Pair remains non-runnable; CLI/dashboard/event evidence remains retryable/visible |
| Continue partially fails | Keep pair non-runnable and original ask retryable |
| Generic `looper unpause` targets a budget-held role | Delegate to paired Continue; never resume only one role or preserve an exhausted counter |
| Daemon restarts after a halt | Discovery cannot revive either held side; ask/handoff remains authoritative |
| No sibling exists | Halt the existing role idempotently using the configured HITL mode; do not invent a phantom loop |
| `ThreadResolution.Enabled=false` | Narrow changed-disposition path still runs; objective stale-thread reconciliation remains off |
| Provider lacks native thread capabilities | Enforce budgets but do not claim same-head disposition convergence |
| Legacy Looper comment has no new disposition fields | Fixer applies existing scope classification; no comment is lost |

## 13. Migration and compatibility

1. Existing explicit numeric cap values are preserved. Their enforcement is no
   longer conditional on HITL, which is an intentional behavior change for
   HITL-disabled installations.
2. The normalized defaults change from `8/8` to `3/3`. A running loop already
   over the live cap halts on its next admission check: `awaiting_human`/paused
   with HITL enabled, or a no-ask paired pause with HITL disabled. Neither path
   approves, resolves, or merges.
3. Existing Reviewer `iterationCount` and Fixer
   `reviewFixBudget.pushCount` remain the meter storage.
4. `HITL.Enabled` remains `false` by default. The change does not silently turn
   on GitHub/Feishu asks; it adds an action-required event/notification/dashboard
   handoff and pair-aware `looper unpause`/`looper stop` path when no answer
   transport is enabled.
5. Continue resets only exhausted meters. A no-HITL budget `unpause` delegates
   to this same operation. Existing event history remains the audit trail; no
   epoch history is added.
6. Existing automatic loops retain pairing. Continuous manual/takeover loops
   become budget participants based on `followUpdates=true`.
7. Existing ThreadResolution defaults remain unchanged. `Enabled`, mode,
   auto-resolve, new-head, current-review-request, and max-thread settings keep
   governing objective reconciliation as defined in §8.1. Only the narrow
   Looper-authored changed-disposition path becomes default behavior on capable
   providers.
8. `lastReviewedSignalFingerprint` is absent on upgrade. For a same-head loop:
   if it has unresolved Looper-authored threads, run one bounded reconciliation
   pass; otherwise store the baseline without publishing a new review.
   Upgrade backfill must use the normal stable-dedupe queue, Reviewer quiet
   period, scheduler concurrency limit, and discovery limits; it must not launch
   all legacy PR reconciliations outside normal admission.
9. The Reviewer queue dedupe key remains unchanged. New signal data is added to
   payload/checkpoint/coalescing state, so no old/new fingerprint queue pair can
   coexist.
10. Managed webhook reconciliation adds `pull_request_review_thread` to existing
    GitHub hooks and advertised forwarder event lists; review-comment events are
    re-routed to Reviewer as well as Fixer. Polling remains the correctness
    fallback during rollout.
11. `lastPublishedHeadSha` remains for provider publication history and backwards
   compatibility but no longer suppresses a changed thread signal.
12. Existing `looper:thread-resolution` markers without a feedback fingerprint
   are valid historical audits only; they do not suppress newer comment state.
13. Legacy Fixer declines that are already remotely resolved remain resolved;
   migration does not reopen historical threads. New declines use the Reviewer
   handshake.
14. Forgejo keeps its existing ThreadResolution/provider limitations and is
    explicitly exempt from same-head disposition convergence; its Reviewer and
    Fixer budgets are still enforced independently of HITL.
15. Unmarked third-party review comments retain current Fixer behavior and scope
   checks.

## 14. Verification

Minimum credible evidence:

| Level | Required case |
| --- | --- |
| Unit | Finding disposition/schema validation and provider-field stripping |
| Unit | Same-root grouping does not merge independent findings |
| Unit | Initial vs changed-head vs same-head-thread prompt/frontier selection |
| Unit | Review-signal canonicalization: edit, reply, delete, resolve, reopen, audit exclusion |
| Unit | Reviewer post-mutation refetch commits the new signal; self-webhook starts no agent |
| Unit | Trusted directive parser, aliases, incidental-word rejection, and author authority |
| Unit | Directive parser is limited to Looper-authored threads and accepts ASCII/Unicode apostrophes |
| Unit | Thread decision matrix for fixed/accept/reject/needs-human/not-fixed |
| Unit | ThreadResolution gate matrix: legacy disabled/report-only/new-head settings do not disable narrow disposition |
| Unit | `MaxThreadsPerRun+1` candidates require continuation and do not advance the signal early |
| Unit | Independent cap matrix, `0`, live cap changes, and “reset exhausted only” |
| Unit | HITL-disabled cap/needs-human produces a no-ask paired hold; HITL-enabled produces a resumable ask |
| Unit | No-HITL scope hold with unchanged evidence re-holds without consuming a meter |
| Unit | Stable queue dedupe coalesces queued/retry-wait signals with Reviewer debounce |
| Integration | First pass publishes all independent in-scope findings in one review |
| Integration | `follow_up` never becomes a remote thread or Fixer item by default |
| Integration | Fixer decline triggers same-head Reviewer adjudication and no duplicate thread |
| Integration | Fixer decline reply does not resolve; accept resolves; reject re-enables the existing Fixer item |
| Integration | Edited `wontfix` comment queues within one polling cycle on unchanged head |
| Integration | Accepted `wontfix` resolves, rejected remains actionable, ambiguous halts pair according to HITL mode |
| Integration | Same-head accepted disposition can lead to a clean convergence review |
| Integration | Comment drift between classification and mutation performs no stale mutation |
| Integration | Running old signal plus two new signals yields one latest-signal continuation, not parallel/back-to-back stale work |
| Integration | Second unchanged-input Fixer decline after Reviewer reject goes directly to `needs_human` |
| Integration | HITL-off default enforces `3/3`, pauses pair, preserves handoff, and cannot be rediscovered |
| Lifecycle integration | Reviewer cap halts Fixer; Fixer cap halts Reviewer; ask/no-ask hold follows HITL mode and restart cannot revive pair |
| Lifecycle integration | Continue refills only exhausted meter(s) and releases pair fail-closed |
| Lifecycle integration | No-HITL `looper unpause` delegates to paired Continue; `looper stop` delegates to paired Stop |
| Lifecycle integration | `manual=true,followUpdates=true` participates; one-shot manual remains exempt |
| Lifecycle integration | Upgrade baseline reconciliation respects queue dedupe, Reviewer quiet period, scheduler concurrency, and discovery limits |
| GitHub contract | Review-comment create/edit/delete routes Reviewer+Fixer; thread resolved routes both; polling remains fallback |
| GitHub contract | Runtime, forwarder, CLI, and netadmin managed-hook event lists remain identical |
| GitHub contract | Paginated thread timestamps/body hashes and audit markers preserve idempotency |
| GitHub contract | `updatedAt` negative cache expires into forced full refresh and never advances authority state |
| Provider contract | Forgejo enforces role budgets but advertises no same-head disposition guarantee |

Because this changes cross-Module lifecycle, GitHub command, worktree review,
and resolve-comments behavior, unit-only coverage is insufficient. Prefer the
existing fake-`gh` contract and invariant integration harness. Escalate to real
GitHub sandbox E2E only if implementation changes auth, scopes, provider thread
mutation shape, or rate-limit behavior; adding transient Looper fields that are
stripped before the existing provider call does not by itself require sandbox
E2E.

Repository verification for implementation PRs:

```sh
gofmt -l .
go vet ./...
go test ./...
go build ./...
```

## 15. Ship slices

1. **Budget semantics:** HITL-independent enforcement, default `3/3`,
   continuous-manual participation, ask/no-ask paired holds, independent
   refill, and lifecycle integration tests.
2. **Reviewer finding contract:** scope authority, dispositions, complete first
   pass, trusted outbound validation, prompt/contract tests.
3. **Repair frontier:** changed-head delta rules, late-discovery handling, and
   duplicate decline protection.
4. **Thread signal:** narrow feature gate, fingerprint, stable queue coalescing,
   same-head polling/webhook trigger, extended decisions, post-mutation signal
   commit, and stale-mutation guard tests.
5. **Docs/observability:** configuration guide, user guide, dashboard/CLI handoff
   detail, metrics/events for disposition and budget outcomes.

Each slice must leave existing authority and lifecycle invariants intact. Do not
merge a prompt-only partial that publishes new disposition words without the
trusted outbound gate, or a thread fingerprint that can enqueue but cannot
dedupe across restart.

## 16. Done bar

- [ ] Reviewer and Fixer retain separately configurable caps and counters
- [ ] Defaults are `3` Reviewer publications and `3` Fixer pushes
- [ ] Caps enforce with HITL on or off; default HITL-off produces a no-ask paired hold
- [ ] HITL-on exhaustion parks the pair; Continue refills exhausted meters only
- [ ] HITL-off budget hold resumes only through pair-aware unpause; stop terminates both
- [ ] HITL-off `needs_human` pauses with evidence instead of dropping the finding
- [ ] Takeover continuous loops participate; one-shot manual loops do not
- [ ] First Reviewer pass reports every independent in-scope finding observed
- [ ] Later passes use repair/thread frontiers instead of full old-diff rescans
- [ ] Out-of-scope follow-ups do not become remote actionable comments by default
- [ ] Fixer still independently verifies scope and may decline/ask for human
- [ ] Fixer cannot resolve its own decline; Reviewer adjudicates the existing thread
- [ ] Same-head thread edits/replies change Reviewer discovery identity
- [ ] Legacy ThreadResolution defaults remain unchanged; narrow disposition works anyway
- [ ] Reviewer adjudication replies cannot self-trigger another agent execution
- [ ] More than ten changed threads complete in bounded batches without signal loss
- [ ] Stable queue dedupe coalesces latest signal; fingerprint is not part of the key
- [ ] Trusted `wontfix` is accepted, rejected, or escalated with evidence
- [ ] One unchanged-input reject/decline recurrence forces `needs_human`
- [ ] Fixer cannot race an unaudited disposition or mutate a stale thread
- [ ] Accepted same-head dispositions can reach a clean review without a new commit
- [ ] Budget exhaustion never implies approve/resolve/merge
- [ ] No new SQL table, epoch id, pair id, or finding ledger is introduced
- [ ] Forgejo budget guarantee and same-head disposition exemption are explicit
- [ ] Contract/invariant integration coverage passes with Go CI commands

## 17. Open implementation choices (not product locks)

- Exact metadata key spelling for `lastReviewedSignalFingerprint`
- Whether polling computes the aggregate hash directly or through a small cache
- Exact event names and dashboard presentation of the handoff brief
- Whether `/looper reconsider` ships in the same PR as `wontfix` or immediately
  after, provided edit/delete/reopen still invalidates prior audit
- Exact default quiet-period interaction for webhook-triggered thread changes,
  provided the responsiveness target and burst dedupe hold
