# Shared Feishu inbox

One Feishu app can serve multiple local `looper collab` hosts. This restores the
historical Worker + D1 transport while preserving feishu-codex-bridge's current
conversation behavior. It does not start Friday or the full Looper daemon.

Feishu sends callbacks here. Each trusted local host reads the same durable
inbox and forwards only its existing task topics to the upstream SDK. No
per-device cloud forwarding table is maintained. Plane device binding still
determines which local host receives a newly dispatched task.

## Deploy once for the team

Copy `wrangler.toml` to a deployment config, select the correct Cloudflare
account, create a D1 database and fill its ID. Set `FEISHU_APP_ID` under `[vars]`.
If mounting under an existing hostname, set the route and `PATH_PREFIX`, e.g.
`/collab`; callback path is then `/collab/` and reads use `/collab/events`.

For a new database, apply `schema.sql` first, then the migration. For the old
Looper inbox database, apply only the migration; do not recreate its tables.
From this directory (replace the config/database arguments with your values):

```sh
wrangler d1 execute looper-collab-inbox --remote --file schema.sql
wrangler d1 migrations apply looper-collab-inbox --remote
wrangler secret put FEISHU_VERIFY_TOKEN
wrangler secret put POLL_TOKEN
wrangler deploy
```

Use the app's Verification Token, and a newly generated shared poll secret.
If the app encrypts callbacks, also set `FEISHU_ENCRYPT_KEY`. The receiver checks
the token and app ID, strips verification material from stored events, and
deduplicates retries by Feishu event ID. Raw events preserve mentions, media
references and message IDs required by the SDK.

In the Feishu app's event configuration, select developer-server delivery and
save the HTTPS callback URL; publish the app version. Retain the existing
message and reaction subscriptions. If using upstream interactive controls,
configure `card.action.trigger` callback delivery to the same URL as well.
`GET /healthz` checks reachability; `/events` requires the poll bearer.

## Configure each local host

Inject both values using your team's secret distribution mechanism, then run
the normal entrypoint:

```sh
export LOOPER_FEISHU_INBOX_URL=https://your-inbox.example/events
# Inject LOOPER_FEISHU_INBOX_TOKEN without committing or pasting it into chat.
looper collab start
```

Existing `hitl.feishu.eventInboxUrlEnv` / `eventInboxTokenEnv` can name custom
environment variables. Omit both inbox values to retain single-consumer
WebSocket mode. An explicit but incomplete inbox configuration fails startup;
it cannot silently fall back to a competing WebSocket consumer.

Each instance retains its own SQLite cursor and topic records. A first start
scans stored events from zero and skips topics it does not own; later starts
resume the saved cursor. Transport failures retry from the last accepted row.
Delivery is at least once; a crash between dispatch and saving the cursor may
replay an event. The SDK's deduplication is not an exactly-once guarantee across
all process restarts. No retention policy is added by this change.

The shared bearer grants access to all stored inbound events, even though local
routing ignores other hosts' topics. Distribute it only among trusted instances.
This is not a per-user authorization boundary. Unknown topics, DMs and new group
messages do not choose a host automatically. Hosts must not run concurrently
from copied state directories that claim the same task topics.

## Verify

```sh
node --test worker.test.ts
```

Use two independent local state directories, one private group and two task
topics. Have a human @bot in each topic: only its owning instance should resume
the original Agent session. Ordinary conversation stays silent. Restart a host
and check that saved events do not trigger another completed turn. Internal
callback fixtures are useful regression coverage but do not substitute for
this real Feishu test.

## Standalone collaboration

The Worker only receives Feishu callbacks and serves the durable inbox. It does
not call Plane or track device presence. Local Looper sends signed heartbeats
directly to its paired Plane, which records receipt time and computes expiry.
No corporate-network exception or presence secret is needed for this Worker.

The former `/devices/heartbeat` and `/devices/<device-id>` endpoints have been
removed. Upgrade Plane and local clients before publishing this Worker. The
historical `0002_device_presence.sql` migration can remain applied; its table is
no longer used and is not needed for new installations. Existing events remain.

See [the collaboration connection guide](../../docs/collab-cf-relay.zh-CN.md) for
rollout, legacy compatibility and remaining onboarding limitations.
