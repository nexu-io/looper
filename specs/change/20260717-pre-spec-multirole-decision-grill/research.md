# Contract Spike Evidence

Date: 2026-07-17

## Plane work item comments

Environment: `plane.powerformer.net`, workspace `open-design`, isolated temporary work item with `[looper-e2e]` prefix and no Looper trigger labels.

Observed contract:

- Work item comment list returns a cursor envelope with `results`.
- A comment contains stable UUID `id`, Plane member UUID `actor`, `created_at`, `updated_at`, `comment_html`, `description`, and edit metadata.
- Work item comments do not return `display_name`; identity authority must use `actor`, never display text.
- Typed Plane CLI supports comment `create`, `list`, `get`, `update`, and `delete`.
- Create returned a comment UUID; list/get returned the same UUID; update retained the UUID and returned the new revision marker.
- The temporary comment and work item were deleted by the command's cleanup trap.

Conclusion: V2 can keep one request comment per role and update it in place with a revision marker. The persisted authority tuple is `(comment UUID, actor UUID, revision, updated_at)`.

## Plane page comments

Read-only inspection of an existing technical Spec page returned an array of page comments. Each comment includes stable UUID `id`, member UUID `actor`, `display_name`, `created_at`, `updated_at`, `edited_at`, `comment_html`, and `comment_stripped`.

Conclusion: owner-only technical Spec approval can be enforced by Plane actor UUID and constrained to comments after the current review request/revision.

## Feishu target safety

Read-only chat lookup established:

- Allowed E2E target: `agent 通知`, chat ID `oc_4d1e7726439964664fd2b0401b266632`.
- Forbidden target: `Looper 协作`, chat ID `oc_70c70fc04837282f406e1322a29189eb`.

The production-like isolated Looper config was pointing at the forbidden target before this change. E2E must use an explicit allow/deny preflight before any send.

An expected mention open_id does not need to be a member of `agent 通知`; a grey mention is acceptable. Membership is not a send gate.

## Feishu message idempotency and images

Live contract spike ran only in `agent 通知`; all temporary messages were withdrawn after observation.

- Creating a top-level text message twice with the same request `uuid` returned code `0` both times and the same `message_id`.
- Replying in a thread twice with the same request `uuid` returned code `0` both times and the same `message_id`.
- `POST /open-apis/im/v1/images` accepted an existing 1672×941 PNG with `image_type=message` and returned an `image_key`.
- Sending the uploaded image as a thread reply twice with the same request `uuid` returned the same `message_id`.

Conclusion: visible-message dedupe can rely on Feishu's server-side message UUID. Image uploads may be repeated invisibly after a crash, but the visible message remains exactly-once; persist the returned `message_id` immediately after each send. V1 intentionally does not persist `image_key`, so a crash in the narrow upload→message/checkpoint window may repeat only the invisible upload.

## Renderer decision

Local contract prerequisites:

- Google Chrome is installed at `/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`.
- V1 will not execute an arbitrary renderer command and will not load agent HTML directly from `file://`.
- Agent output is restricted to a small static HTML subset. Looper parses it, rejects scripts, event handlers, network/resource tags, JavaScript URLs, CSS `url()` / `@import` / expression-like constructs, and any non-whitelisted tag/attribute.
- The accepted document is served from a tokenized loopback-only HTTP server and rendered by a Looper-controlled headless Chrome process with a temporary profile and fixed viewport.

The renderer contract becomes green only when its integration test proves deterministic PNG output plus rejection of script/network/file-access payloads.

## Isolated end-to-end evidence

Environment:

- isolated config and SQLite root: `/Users/elian/.looper-v2-e2e`
- isolated repo/worktree root: `/Users/elian/.looper-e2e/repos/open-design` and `/Users/elian/.looper-v2-e2e/worktrees`
- dedicated daemon port: `14682`
- scheduler concurrency: `1`
- Feishu thread chat ID: `oc_4d1e7726439964664fd2b0401b266632` (`agent 通知`)
- forbidden chat ID: `oc_70c70fc04837282f406e1322a29189eb` (`Looper 协作`)
- Plane temporary work item: `#1593`, UUID `9e02ceec-bc56-4071-bf46-3084b0f99b5a`
- planner loop: `52830199-29cb-400f-9696-b30181d6dcdf`
- Feishu root message: `om_x100b6a94b75640b8c001e510440a94e`

Observed sequence:

1. A real author agent researched the work item and a fresh requirement GRILL kept the explicitly unresolved `PROD-001` question instead of answering it from code conventions.
2. Product answer comment `a864e2af-f0b6-43c6-9248-e56eb0533ce8` was consumed from Plane; Feishu was never used as inbound authority.
3. A fresh downstream GRILL generated `DESIGN-001` and `ENG-001`. Plane request comments were `c552ec52-d58b-4f35-bca1-a9e347b760af` and `b16a0eb3-cd86-41fe-81e4-cbdf16a8d895`.
4. The two design options rendered as real 1200×800 PNGs and were posted as Feishu thread images with message IDs `om_x100b6a95f28cd8a0def98a65fae3968` and `om_x100b6a95f29d18a4dfa0440f00a0631`. SHA-256 values were `217401ca0189b455e6b7e07c7abb3450e2673b48c36889463b8b861f274457f6` and `a300ceed8d74da433d36aa46643975f8efd2aa39f35ca43212dd640af412458d`.
5. After only the design answer (`532a9910-794f-47ec-8ebb-5ac70b8185b5`) was present, the loop remained paused at `awaiting_downstream`. After the engineering answer (`f900b40a-a0eb-4c22-bf39-fdc0054ad287`) arrived, the 30-second human-gate reconciler resumed the loop from `grill-final-decisions`.
6. The final fresh requirement GRILL produced `questions=[]`; the author then wrote a Chinese technical Spec that cited all three audited role answers.
7. The technical GRILL revised the Spec using real source evidence. Plane page `030866ab-34b6-4322-b447-84227763a473` was re-published with the converged content (including the source-corrected 19-locale count). The independent reviewer returned `VERDICT: READY`.
8. Revision-bound approval request comment `42c18665-1fd9-4b2d-bbbe-55d0f19d86e0` bound approval v1 to SHA-256 `b9920634bacebff08dca906780e7ade562fc926ed26887e26fb578fa88b3068d`. Owner approval comment `52514d36-686c-4e82-8f2e-831ad7e97226` caused exactly one `looper:worker-ready` handoff and moved the Plane state to `In Progress`.

The E2E exposed and fixed several issues before rollout: fresh V2 loops initially froze a V1 checkpoint, prompts omitted the work-item body, Plane comment parsing preferred a content UUID over `comment_html`, partial human gates polled too slowly, requirement GRILL could invent non-canonical IDs, safe prototype HTML was over-rejected, a BLOCKED independent review could incorrectly open approval, and completed planner metadata omitted the issue URL needed by the approval reconciler. The last fix includes backward-compatible recovery from the latest planner checkpoint and was proven by restarting the isolated daemon before consuming the already-posted owner approval.

Final repository verification after these fixes: `go test ./...`, `go vet ./...`, `go build ./...`, and `git diff --check` all passed.

## Post-E2E adversarial review hardening

Fresh implementation audit after the live E2E found and closed additional crash/authority edges without changing the validated user flow:

- Every partial role answer is checkpointed immediately and writes an append-only, snapshot-hashed Decision Log entry. Design-first / engineering-later plus daemon-style repeat ticks converge on one active queue.
- A newer authorized product reply that is contradictory re-opens `awaiting_product`; it cannot be mislabeled `product_resolved` merely because the answer map changed.
- Agent JSONL tool output cannot inject a forged `__LOOPER_RESULT__` decision brief; only known assistant-message fields are trusted.
- Owner approval side effects retry the same comment after transient label/state/audit failures and mark `dispatched` only after all effects converge.
- Each Feishu image `message_id` is persisted immediately; stable logical UUIDs keep visible messages exactly-once even when an invisible upload repeats.
- Renderer output paths canonicalize trusted operating-system ancestors, reject a symlink root or any nested symlink, and reject lexical traversal. Browser defense is sanitizer + CSP + JavaScript disabled + closed local proxy + restrictive host resolver.
- A Plane technical-Spec page edit after REVIEW invalidates approval visibly and fails the planner with a manual fresh GRILL + REVIEW instruction.
- The independent REVIEW is now bound to the rendered Plane-page hash captured after GRILL publication. If the page changes while REVIEW is running, Looper clears the receipt and must re-publish + run a fresh REVIEW before an owner gate can open; the gate itself re-reads and compares the same hash.
- The normal `looper:auto` intake path now honors V2: a feature without a pre-existing product Spec enters planner research first, and a legacy `looper:awaiting-product-spec` hold is retired when V2 is enabled. V1 retains its old gate.
- `ProductAsk` is durable across Git reconciliation retries. V2 accepts only an empty value or `RETURN_TO_REQUIREMENTS:`; any other non-empty value fails closed instead of falling into legacy Feishu inbound. Reopen clears every stale authority receipt and derived artifact.
- Queued `product_resolved` / `downstream_resolved` checkpoints still consume newer authorized Plane comments. A late conflicting decision reopens and pauses the barrier and cancels its active queue.
- Final requirement GRILL requires `questions=[]`; formal product-Spec requests explicitly require a linked `looper:product-spec` Plane page, while normal role answers accept a legal option ID or a clear custom decision from the correct actor.
- Approval content is re-read after the potentially long LLM judgment and before worker dispatch. Missing remote request timestamps fail closed; no local clock fallback can widen the revision boundary.
- Renderer completion now validates PNG chunks, CRCs, terminal IEND and full decode, waits for a stable content snapshot before stopping a still-live browser, and persists an audited `.render.json` manifest per option.
- Shared-group Feishu cards identify the emitting local installation: task anchors, live progress, and HITL cards render `来自 @owner 的 Looper` from the per-project `owner.feishuOpenId`. Chat membership is intentionally not queried, so an out-of-chat owner remains a useful grey mention; missing configuration is made visible.

Latest clean verification on the final working tree: `go test -count=1 ./...`, `go vet ./...`, `go build ./...`, focused planner race tests, and `git diff --check` all passed. The isolated E2E daemon/root and temporary #1592/#1593 Plane artifacts were removed; the shared historical `/Users/elian/.looper-e2e` harness was intentionally preserved.
