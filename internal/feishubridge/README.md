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
`test/external-dispatch.test.ts` into upstream `test/`. Run upstream's
`npm ci --ignore-scripts`, `npm run typecheck`, and:

```sh
npx vitest run test/external-dispatch.test.ts test/message-queue-races.test.ts \
  test/run-card-stream.test.ts test/queued-card.test.ts test/run-process.test.ts
```

`go test ./internal/feishubridge ./internal/collab` checks live sidecar replacement
and durable task/conversation transitions without contacting Feishu or an LLM.

One bot should have one active collaboration consumer. Colleagues using separate
local instances should configure separate test bots; two independent processes
using one bot do not share topic/session state.
