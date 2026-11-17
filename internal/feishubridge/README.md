# Feishu Codex Bridge integration

`looper collab start` prepares and owns the actual MIT-licensed
[modelzen/feishu-codex-bridge](https://github.com/modelzen/feishu-codex-bridge)
0.6.15 runtime at commit `a9ff5d2d028522b41d7e8b1a4837e83be5f2f6d3`.
Node.js 20+, npm and git are needed on first start. The pinned archive is verified
and built in the collaboration state directory; upstream source and node_modules
are not added to this repository. `UPSTREAM-LICENSE` preserves its license.

The patch exposes task submission, cancellation and native-session callbacks.
Cards, streaming, message intake, reactions, run queues, file delivery and native
Agent processes stay upstream. Go owns Plane dispatch, group scope, task records
and PR observation. The neutral workspace lists available repositories; neither
a task nor its Feishu topic is bound to one repository. Existing Worker/daemon
callers retain their executor; CLI collaboration starts the new bridge path.

Secrets are passed over an inherited pipe, not command arguments or saved app
settings. Pipe EOF also shuts down the sidecar after a host crash. A replacement
uses its private IPC shutdown endpoint; it never kills a recovered PID.

For patch verification, unpack the pinned upstream source, apply
`assets/upstream.patch`, overlay `assets/external.ts` to `src/bot/external.ts` and
`assets/looper-entry.ts` to `src/looper-entry.ts`, and copy
the files in `test/` into upstream `test/`. Run upstream's
`npm ci --ignore-scripts`, `npm run typecheck`, and:

```sh
npx vitest run test/external-dispatch.test.ts test/looper-inline-files.test.ts test/inline-files.test.ts test/outbound-files.test.ts \
  test/message-queue-races.test.ts \
  test/run-card-stream.test.ts test/queued-card.test.ts test/run-process.test.ts
```

`go test ./internal/feishubridge ./internal/collab` checks live sidecar replacement
and durable task/conversation transitions without contacting Feishu or an LLM.

Without an inbox configured, one bot has one WebSocket consumer. For several
local Loopers sharing one app, deploy [the shared inbox](../../deploy/feishu-inbox-worker/README.md)
and set `LOOPER_FEISHU_INBOX_URL` and `LOOPER_FEISHU_INBOX_TOKEN` before starting
each host. The sidecar uses the upstream webhook transport instead of opening
competing WebSockets. Go forwards owned task topics to the same SDK dispatcher;
upstream still owns mentions, deduplication, steering, queues and cards.

Ownership uses existing chat/root/native-thread records. Each host persists its
own cursor, so one reader cannot consume another reader's messages. Unknown
topics and free-form DMs are not dispatched to an arbitrary device. Do not clone
an active host's state directory onto a second online host: it would duplicate
topic ownership. This is routing among trusted team instances, not per-device
isolation: the shared bearer can read all inbox events.

`node --test deploy/feishu-inbox-worker/worker.test.ts` (Node 24) verifies the
receiver with SQLite. `TestSharedInbox` runs two SQLite hosts and real Node IPC
sidecars to verify independent routing, retry, cursor persistence and secret
filtering. The upstream test also dispatches webhook events through the actual
SDK, with ordinary-chat silence, @bot steering and session resume.
