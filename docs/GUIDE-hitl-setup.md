# HITL setup: Plane/GitHub decisions, one-way Feishu notifications

Looper keeps collaboration in the system of record:

- product specs, approvals, and product answers live in Plane;
- code, review, and implementation answers live in GitHub;
- Feishu only delivers targeted notifications and never accepts an answer;
- the authenticated `/respond` API is an operator/dashboard fallback.

There is no Feishu event subscription, callback route, Cloudflare inbox worker,
thread-reply mirror, or card-action receiver to deploy.

## Minimal configuration

```json
{
  "hitl": {
    "enabled": true,
    "answerTransport": "github",
    "github": {
      "awaitingLabel": "looper:awaiting-human",
      "mentionLogins": ["github-owner"],
      "answerAuthors": ["github-owner"]
    }
  },
  "notifications": {
    "webhook": {
      "enabled": true,
      "mode": "app",
      "format": "feishu",
      "appIdEnv": "LOOPER_FEISHU_APP_ID",
      "appSecretEnv": "LOOPER_FEISHU_APP_SECRET",
      "chatId": "oc_xxx",
      "mentionOpenIds": ["ou_xxx"]
    }
  }
}
```

Only the two app credentials are secrets:

```sh
export LOOPER_FEISHU_APP_ID=cli_xxx
export LOOPER_FEISHU_APP_SECRET=xxx
```

`chatId`, GitHub logins, and Feishu open IDs are identifiers and may remain in the
config. Keep the credentials in the service environment, never in the JSON file.

## Runtime flow

1. An agent surfaces a genuine blocker.
2. Looper creates the exact question/comment in Plane or GitHub and persists a
   named blocked condition.
3. A one-owner Feishu card points to that exact source location.
4. The human follows the link and answers there.
5. The source-of-truth watcher observes the answer, clears the condition, and
   resumes the same loop/session.

Posting in the Feishu thread, clicking old interactive buttons, or mentioning a
bot does nothing by design.

## Per-person Plane configuration

If your tasks live in Plane, use the self-developed [`plane` CLI](https://github.com/powerformer/plane-cli)
with your own API key. Configure it once in `~/.plane/plane.toml` (or export
`PLANE_API_KEY` / `PLANE_WORKSPACE_SLUG`), then:

| Config value | Command to get it |
| --- | --- |
| **`PLANE_API_KEY`** (env; each person their **own** key) | Plane → workspace settings → **API Tokens** → create one |
| **`planeAssigneeId`** = your member UUID (`roles.worker.triggers.planeAssigneeId`) | `plane api me` → the `id:` line |
| provider **`projectId`** (Plane project UUID) | `plane api project list` → first column of your project |
| provider **`workspace`** (slug) | it's your workspace slug (e.g. `open-design`); `plane api me` echoes the base |
| provider **`baseUrl`** | `https://plane.powerformer.net/api/v1` (the default) |

`plane api me` doubles as an auth smoke-test. An admin can hand out anyone's UUID with `plane api member workspace-list`.
Put your UUID in `planeAssigneeId` so your looper only picks up Plane work-items **assigned to you**; leaving it empty
falls back to label-only discovery (every looper watching that project grabs every item — use one central looper then).
Each teammate should use **their own** Plane API key (correct attribution + `plane api me` gives their UUID), not a
shared one. Full Plane setup: [`skills/looper/references/plane.md`](../skills/looper/references/plane.md).

For the opt-in Plane pre-Spec decision flow, configure all three authorities on the project:

```jsonc
"productOwner": { "feishuOpenId": "ou_...", "planeId": "<product UUID>" },
"designOwner":  { "feishuOpenId": "ou_...", "planeId": "<design UUID>" },
"owner":        { "feishuOpenId": "ou_...", "planeId": "<your UUID from plane api me>" }
```

Requirement answers are posted as new Plane work-item comments. If Looper requires a formal product Spec, link a non-empty native Plane page from the work item with title `looper:product-spec`; an external URL, blank page or normal comment does not satisfy the formal gate. Final technical-Spec approval is posted on the Plane Spec page. Feishu is notification-only for these decisions. A user's `open_id` can still be mentioned if they are not in the notification group (it may render grey); group membership is not validated.

### looper's own data
- **`storage` / `daemon` paths** — anywhere you like; default `~/.looper` (`looper.sqlite`, `backups/`, `logs/`).

## Operator fallback

For local testing or a dashboard integration, an authenticated operator may use:

```sh
curl -X POST http://127.0.0.1:7788/api/v1/loops/<seq>/respond \
  -H 'Content-Type: application/json' \
  -d '{"answer":"approved option"}'
```

This route is governed by `server.authMode`. It is not a Feishu integration.

## Smoke test

Use an isolated Looper home and test repository. Trigger a task that asks a
question, then verify:

- the question exists in Plane/GitHub;
- the Feishu card contains the expected owner and exact deep link;
- `POST /api/v1/hitl/feishu` returns 404;
- a Feishu thread reply does not change loop state;
- answering in Plane/GitHub resumes the loop.
