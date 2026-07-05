# GUIDE — configuring looper's human-in-the-loop (HITL) capabilities

A copy-paste setup guide for the HITL features: decision cards, multi-round
conversation, the anytime message queue, the live Feishu thread (progress +
milestones), and one-command local takeover. For the design rationale see
[`DESIGN-hitl-transports.md`](DESIGN-hitl-transports.md) and
[`DESIGN-human-takeover.md`](DESIGN-human-takeover.md).

**Everything here is off by default** — a looper with none of these flags behaves
exactly as before. Turn them on one at a time.

---

## What each capability needs (at a glance)

| Capability | Config to enable | Extra infra |
| --- | --- | --- |
| Agent pauses to ask a human (decision brief) | `hitl.enabled: true` | — |
| Multi-round conversation + anytime message queue | `hitl.enabled: true` (same core) | — |
| Answers via **GitHub PR comment** | `hitl.answerTransport: "github"` | none |
| Answers via **Feishu** (cards + typed replies) | `hitl.answerTransport: "feishu"` + `notifications.webhook.mode: "app"` | Feishu app + [CF inbox worker](#4-cloudflare-inbox-worker-shared-feishu-app) |
| Feishu thread: anchor · milestones · @-mention | `notifications.webhook.mode: "app"` | Feishu app |
| Live tool-call feed inside the thread | env `LOOPER_CODEX_JSON_EVENTS=1` | codex vendor |
| Takeover handback resumes the human's session | `agent.nativeResume.enabled: true` | — |

---

## Teammate quick start (two files)
A teammate joining the shared Feishu setup needs exactly two files:

1. **The shared env file** — the team's Feishu/worker secrets (same for everyone).
   Get the filled copy from whoever set it up, then `source` it.
   ```sh
   source ~/.looper/hitl.env
   ```
2. **Their own config** — copy the template and fill the few placeholders (agent
   path, their group `chatId`, their `open_id`, the repo + local clone path):
   ```sh
   cp deploy/config.hitl.example.json ~/.looper/config.json
   $EDITOR ~/.looper/config.json          # replace the REPLACE_/ABSOLUTE_ placeholders
   looperd --config ~/.looper/config.json
   ```
The env file carries the shared **secret values**; the config carries the
**feature switches + per-person settings**. You need both.

## 0. Prerequisites
- A coding agent configured (`agent.vendor` = `codex` / `claude-code` / …).
- For Feishu: a Feishu **custom app** (with `im:message` send + event subscription)
  and, for a *shared* app across teammates, the Cloudflare inbox worker (§4).
- Takeover (`looper resume`/`handback`) runs on the **same machine as the daemon**
  (the worktree + agent session live there). Verified for codex & claude-code.

## 1. Simplest: HITL over GitHub (zero extra infra)
Everyone already has GitHub, so this is the batteries-included default.

```jsonc
{
  "agent": { "vendor": "codex", "nativeResume": { "enabled": true } },
  "hitl": {
    "enabled": true,
    "answerTransport": "github"   // ask posted as a PR comment; reply on the PR
  }
}
```
The agent asks by commenting on a (draft) PR; a human replies on the PR; the poll
lane resumes the loop. No Feishu, no worker.

## 2. Full experience: HITL over Feishu
Adds the interactive decision cards, the live thread, milestones, @-mention, and
typed multi-round replies.

```jsonc
{
  "agent": {
    "vendor": "codex",
    "nativeResume": { "enabled": true }        // handback resumes the same session
  },
  "hitl": {
    "enabled": true,                            // decision cards + conversation + queue
    "answerTransport": "feishu",
    "feishu": {
      "inbound": "cf-inbox",                    // read replies from the CF worker
      "eventInboxUrlEnv": "LOOPER_FEISHU_INBOX_URL",
      "eventInboxTokenEnv": "LOOPER_FEISHU_INBOX_TOKEN"
    }
  },
  "notifications": {
    "webhook": {
      "enabled": true,
      "mode": "app",                            // thread · anchor · milestones · live feed
      "format": "feishu",
      "appIdEnv": "LOOPER_FEISHU_APP_ID",       // NAMES of env vars, not the values
      "appSecretEnv": "LOOPER_FEISHU_APP_SECRET",
      "verificationTokenEnv": "LOOPER_FEISHU_VERIFY_TOKEN",
      "chatId": "oc_xxxxxxxx",                  // the target group chat id
      "mentionOpenIds": ["ou_xxxxxxxx"]         // who to @ on a decision (open_id)
    }
  }
}
```

## 3. Environment variables
The config stores env-var **names**; the secret values live in the environment
(never in the config file, so the repo stays shareable). Because the Feishu app +
inbox worker are shared, these values are the **same for the whole team**: fill
[`deploy/hitl.env.example`](../deploy/hitl.env.example) once, distribute the filled
copy privately, and each teammate `source`s it before running looperd. (Per-person
settings — `chatId`, `mentionOpenIds` — go in each person's config file, not here.)

```sh
export LOOPER_FEISHU_APP_ID=cli_xxx            # Feishu app credentials
export LOOPER_FEISHU_APP_SECRET=xxx
export LOOPER_FEISHU_VERIFY_TOKEN=xxx          # app Event Subscription verification token
export LOOPER_FEISHU_INBOX_URL=https://<worker-domain>/events   # CF worker /events
export LOOPER_FEISHU_INBOX_TOKEN=xxx           # shared secret to read /events (= POLL_TOKEN)
export LOOPER_CODEX_JSON_EVENTS=1              # optional: live tool-call feed in the thread
```

## 4. Cloudflare inbox worker (shared Feishu app)
So one shared Feishu app can reach many NAT'd looper daemons, Feishu posts events
to a small Cloudflare Worker; each daemon polls it. Setup in
[`deploy/feishu-inbox-worker/README.md`](../deploy/feishu-inbox-worker/README.md):
create the D1 db, set its id in `wrangler.toml`, then:
```sh
wrangler secret put FEISHU_VERIFY_TOKEN   # = the app's Event verification token
wrangler secret put POLL_TOKEN            # = LOOPER_FEISHU_INBOX_TOKEN above
wrangler deploy
```
Point the Feishu app's Event Subscription URL at the worker; turn the Encrypt Key
**off** (the inbox reads plain JSON).

## 5. Roll it out one flag at a time
Recommended order (observe at each step before the next):
1. `hitl.enabled` + `answerTransport: "github"` — HITL with zero infra.
2. Switch to Feishu: `notifications.webhook.mode: "app"` + the app env vars.
3. `LOOPER_CODEX_JSON_EVENTS=1` — the live tool feed.
4. `agent.nativeResume.enabled: true` — so handback continues the human's session.

## 6. Verify
- Drop an issue with a genuinely ambiguous, ask-worthy decision → a decision card
  appears in the group (@-mentioning your `mentionOpenIds`).
- Click an option → the card marks "✅ 已选"; or type a follow-up question in the
  thread → the agent answers and, if it still needs a decision, asks again.
- On completion the anchor shows a timestamped milestone (`🔀 已开 PR #N`).
- `looper resume <seq>` drops you into the loop's agent session; `looper handback
  <seq>` returns it to the daemon.
