# ADR 0006: Webhook delivery via user-owned tunnel

`looperd` gains a second webhook delivery mode, `tunnel`, alongside the existing `gh-forward` mode from ADR 0005. In `tunnel` mode, GitHub delivers webhooks to a public URL that the user fronts with their own tunnel (for example `cloudflared`, `ngrok`, Tailscale Funnel, or a self-hosted reverse proxy), and the tunnel forwards to a `looperd` HTTP listener on `127.0.0.1`.

The motivation is the single-instance limit of `gh webhook forward`: GitHub only permits one active forwarding session per repository or organization, so two engineers cannot both run Looper against the same repo. Tunnel mode replaces the forwarding session with an ordinary repository webhook, which GitHub already supports in the plural.

## Authority

`tunnel` mode introduces a new authority surface: a remote GitHub repository webhook managed by `looperd`. Authority is the locally persisted hook id created by this daemon. The persisted record is a bearer capability: copying or restoring local state copies authority over that hook.

- `looperd` creates the hook via the GitHub REST API and persists `(repo, hook_id, secret)` in local state.
- All subsequent reads, updates, re-enables, and deletes address the hook by id.
- The persisted `managed_url`, disable latch counters, and orphaned bit are authority-bearing guard state. They prevent mutating a hook whose URL drifted away from Looper, prevent infinite re-enable loops after repeated GitHub disables, and preserve explicit delete-by-id authority after the repo leaves config. The cost is extra persisted lifecycle state that must remain in sync with remote hook status; a simpler stateless design was rejected because it would have to infer ownership from mutable remote fields.
- Before any mutating call, `looperd` re-reads the hook by id and verifies that `config.url` still matches the configured `public_base_url` for that repo. This is a drift guard, not ownership proof; matching URL alone never authorizes adopting or deleting a hook.
- During a `public_base_url` rotation, `looperd` also accepts the last locally persisted managed URL for that same hook id long enough to patch the hook in place to the newly configured URL. This transition still relies on the existing locally created hook id; URL matching alone never grants authority.
- `looperd` never deletes a remote hook it did not create. There is no inference from URL, name, events, or any other field.
- If two daemons run with copied or restored tunnel state, they are the same authority identity from Looper's perspective and may race on the same hook. Shared tunnel state across machines is unsupported unless the operator intentionally wants shared control of that hook.

This is a deliberate departure from ADR 0005's local-only authority stance, scoped narrowly: authority is grounded in a hook id that `looperd` itself generated, not in pattern-matching against remote state.

## Lifecycle

- Daemon start: for each repo with `mode: tunnel`, ensure the locally recorded hook exists. If a recorded `hook_id` returns `404`, create a replacement and store the new id. If the hook is disabled and still matches the managed URL, re-enable it, subject to a repeated-disable latch: after N consecutive auto-disables within a window, `looperd` stops re-enabling and surfaces `degraded` until the operator intervenes.
- Daemon stop and crash: the hook is left in place. GitHub will eventually auto-disable a hook that fails persistently; `looperd` re-enables on next start, subject to the latch above.
- Repo removed from config: the remote hook is left in place, and local state is retained as an orphaned hook record until the operator explicitly deletes the hook by id or explicitly forgets the record. Clearing the local record without deleting the remote hook is the lossy path and is not done automatically. Re-adding that repo to config is an explicit operator action to resume control of the same persisted hook-id authority if the hook still matches the recorded id.
- Mode change between `gh-forward` and `tunnel`: not migrated automatically. Tunnel hook state is retained as an orphaned record so that explicit deletion remains safe by id; automatic cross-mode cleanup is rejected for the same reason ADR 0005 rejects daemon-driven remote deletion on inference.
- Manual UI edits: the hook is daemon-managed. A manual disable in the GitHub UI is not respected while the repo is configured in `tunnel` mode and will be re-enabled on next reconcile, subject to the latch. Manual edits to events or content type drift back on next reconcile. To pause delivery, the operator removes the repo from config or switches to `gh-forward`.

## Oracle review

`@oracle` reviewed the authority-bearing hook-id design for this PR after the persisted tunnel-hook record and mutation gates were added. Review outcome: no blockers after removing URL-adoption helpers, requiring latch-state persistence before reporting a durable latch, and documenting the explicit authority for orphan reactivation and managed-URL rotation.

## Delivery path

- `looperd` listens on `127.0.0.1:<webhook.listen_port>`. Binding is fail-fast on conflict.
- Route shape is `POST /webhook/{owner}/{repo}`. Unknown `(owner, repo)` returns `404`.
- The hook's `config.url` is `{public_base_url}/webhook/{owner}/{repo}`. The path encodes routing authority; payload fields are not used for routing.
- HMAC verification uses a per-repo secret generated by `looperd` at hook creation time. Failed verification returns `401` and is logged without payload contents. A manual rotation command exists; automatic rotation does not.
- `ping` events are recognised by header, answered `200`, and not enqueued.
- Health is observed passively. The first `ping` marks the hook healthy. Persistent delivery failure surfaces through GitHub's auto-disable, which `looperd` detects on next reconcile and re-enables. Active outbound probing of the public URL from `looperd` is rejected.

## Coexistence with `gh-forward`

`webhook.mode` is set globally in config and may be overridden per repo. A given repo runs in exactly one mode at a time. Running both modes against the same repo is rejected at config validation. The `gh-forward` path from ADR 0005 is unchanged.

## Tunnel ownership

`looperd` does not manage the tunnel. The user runs `cloudflared` (or equivalent) and configures `webhook.public_base_url` to a stable HTTPS URL that fronts the local listener. An embedded tunnel was rejected because tunnel lifecycle, credentials, DNS, and reconnection are an unrelated daemon concern.

## Failure mode

If hook creation, re-enable, or HMAC verification fails terminally, the repo's webhook path latches into `degraded` and falls back to the existing scheduler poll interval, matching the ADR 0005 fallback contract. Correctness remains with the poller.

## Rejected alternatives

- A single shared URL routed by payload `repository.full_name`, because routing authority would depend on payload fields that vary across event types.
- A global HMAC secret shared across repos, because compromise of one repo would compromise all.
- Tunnel-layer authentication (for example Cloudflare Access service tokens) as a substitute for HMAC, because GitHub webhooks cannot send custom headers.
- Daemon-managed `cloudflared` lifecycle, because it doubles `looperd`'s responsibility surface for no Looper-specific gain.
- Automatic deletion of remote hooks on repo removal, mode change, or daemon shutdown, because the trigger does not prove the user wants the remote resource gone.
- Active outbound health probes of the public URL from `looperd`, because they detect "I can reach myself" rather than "GitHub can reach me", and they race tunnel startup.
- Adopting an existing hook by URL, name, events, or user-supplied hook id without a local creation record, because that collapses back into remote-state inference rather than Looper-created authority.
- Manual-only webhook setup where Looper never creates or deletes hooks and only verifies HMAC, because it minimises daemon authority but makes first-run setup, secret generation, and drift diagnostics operator-owned. Tunnel mode accepts narrow hook-id authority to automate those Looper-specific tasks; the manual path remains available by simply not setting `mode: tunnel`.
- Polling-only as a permanent answer to the `gh-forward` single-instance limit, because the existing 300s poll interval is the degraded fallback, not a target steady state.
