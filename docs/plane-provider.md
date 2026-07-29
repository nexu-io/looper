# Plane provider + Feishu HITL setup

This guide shows how to run Looper with [Plane](https://plane.so) as the **task source** while GitHub stays the **code forge**, and how to route human-in-the-loop (HITL) notifications to a Feishu webhook.

## What it is

Plane is a task tracker: it owns work-items (issues), labels, comments, and assignees, but it has no pull requests, diffs, or code reviews. The `plane` provider splits those two concerns:

- **Issue side → Plane.** Planner and Worker read work-items directly from a Plane project (filtered by a trigger label) via Plane's REST API. No redundant GitHub issue is created.
- **Code side → GitHub.** Pull requests are opened, reviewed, and merged on the project's GitHub `repo`, exactly like a normal GitHub project. This still requires an authenticated `gh`.

Concretely, for a `plane` project Looper maps each Plane work-item onto its internal issue type:

| Looper issue field | Plane work-item source |
| --- | --- |
| `Number` | `sequence_id` (per-project integer) |
| `Title` | `name` |
| `Body` | `description_html` (HTML stripped to text) |
| `Labels` | label UUIDs resolved to names |
| `Assignees` | assignee UUIDs |
| `URL` | Plane work-item web page |

Provider capabilities: issues, labels, comments, and assignees are supported; pull requests, diffs, and native reviews are **not** (they are delegated to GitHub). Review discovery is by label, review publishing is comment-only, and issue discovery is by polling.

## Recommended Plane connection

For a teammate, prefer Plane's browser flow over asking them to copy workspace and project UUIDs:

1. Install Looper and join the team's loopernet.
2. Open the repository checkout.
3. In a Plane work item, choose **Connect my Looper** and run the generated one-time command.
4. Run `looper plane doctor` until every readiness check passes.

`looper plane connect` now creates or extends the local config when the matching provider is absent. It reads the Plane workspace/project from the short-lived connection, detects `owner/repo` from the local GitHub origin, and scopes planner/worker trigger overrides to that project. Use `--project-path` and `--code-repo` when running outside the checkout. Existing matching configurations are reused unchanged.

## Explicit bootstrap (automation and legacy servers)

```bash
looper bootstrap --yes \
  --provider plane \
  --project-path /absolute/path/to/open-design \
  --code-repo acme/open-design \
  --plane-workspace acme-design \
  --plane-project 49832a02-3158-4faf-bf2f-d0e39c40c7e6 \
  --trigger-label looper:plan \
  --feishu-webhook-env LOOPER_FEISHU_WEBHOOK_URL
```

Flags:

- `--provider plane` — select the Plane task source (default is `github`, unchanged).
- `--project-path` — the local checkout of the GitHub code repo (becomes `repoPath`). Required for plane.
- `--code-repo owner/repo` — the GitHub repo where PRs are opened. If omitted, Looper detects it from the checkout's `github.com` origin remote.
- `--plane-workspace` — the Plane workspace slug (required).
- `--plane-project` — the Plane project UUID (required).
- `--plane-base-url` — Plane REST API base (optional; defaults to the public Plane API base `https://plane.powerformer.net/api/v1`).
- `--plane-token-env` — env var holding the Plane API key (optional; defaults to `PLANE_API_KEY`).
- `--trigger-label` — the issue label that triggers Planner/Worker discovery (optional; defaults to `looper:plan`).
- `--feishu-webhook-env ENV_NAME` — when set, adds a `notifications.webhook` block (`enabled: true`, `format: "feishu"`, `levels: ["action_required", "failure"]`) pointed at that env var. Works with any provider.

`--provider plane` can create a fresh config. The self-service `plane connect` path can also add the Plane provider and project to an existing valid config without replacing unrelated providers or projects.

## Environment variables to export

Both are read from the daemon environment and are never written into the config file:

```bash
# The Plane API key (name matches --plane-token-env; default PLANE_API_KEY).
export PLANE_API_KEY='plane_api_xxx'

# The Feishu (or generic) webhook URL (name matches --feishu-webhook-env).
export LOOPER_FEISHU_WEBHOOK_URL='https://open.feishu.cn/open-apis/bot/v2/hook/xxxx'
```

You still need an authenticated `gh` for the GitHub code side (`gh auth login`).

## Example `config.json`

The bootstrap command above produces a config equivalent to the following (defaults elided for brevity):

```json
{
  "providers": [
    {
      "id": "plane-acme-design",
      "kind": "plane",
      "baseUrl": "https://plane.powerformer.net/api/v1",
      "tokenEnv": "PLANE_API_KEY",
      "workspace": "acme-design",
      "projectId": "49832a02-3158-4faf-bf2f-d0e39c40c7e6"
    }
  ],
  "projects": [
    {
      "id": "open-design",
      "name": "open-design",
      "provider": "plane-acme-design",
      "repo": "acme/open-design",
      "repoPath": "/absolute/path/to/open-design",
      "roles": {
        "planner": {
          "preSpecDecisionGrill": true,
          "triggers": { "labels": ["looper:plan"], "labelMode": "all", "requireAssigneeCurrentUser": false }
        },
        "worker": {
          "triggers": { "labels": ["looper:plan"], "labelMode": "all", "requireAssigneeCurrentUser": false }
        }
      },
      "productOwner": { "feishuOpenId": "ou_product", "planeId": "<product-member-uuid>" },
      "designOwner": { "feishuOpenId": "ou_design", "planeId": "<design-member-uuid>" },
      "owner": { "feishuOpenId": "ou_local-owner", "planeId": "<local-owner-member-uuid>" }
    }
  ],
  "notifications": {
    "webhook": {
      "enabled": true,
      "urlEnv": "LOOPER_FEISHU_WEBHOOK_URL",
      "format": "feishu",
      "levels": ["action_required", "failure"]
    }
  }
}
```

Validation rules for a plane provider/project:

- `providers[].kind = "plane"` requires `tokenEnv`, `workspace`, and `projectId`. `baseUrl` is optional (defaults to the public Plane API base) but must be an absolute `http(s)` URL when set.
- The bound project requires `provider` (the plane provider id) and `repo` (the GitHub code repo, `owner/name`).

## Pre-Spec multi-role decision flow (opt-in)

Set `roles.planner.preSpecDecisionGrill: true` to make new Plane planner loops investigate and converge requirements before writing a technical Spec. The flag is frozen when a loop is created: existing V1 loops keep their old sequence, so switching the flag does not reinterpret paused work.

The V2 flow is:

1. Looper creates an isolated worktree, reads code/tests/existing UI patterns, and writes a structured decision brief.
2. A fresh requirement GRILL removes questions answerable from facts or normal engineering judgment.
3. Remaining questions are routed by authority: `PROD-*` → `productOwner`, `DESIGN-*` → `designOwner`, `ENG-*` → this installation's `owner`.
4. Product converges first. Design and engineering may then answer in parallel. Large cross-flow/high-risk requirements wait for a formal product Spec; small independent questions can be answered directly.
5. After a final fresh GRILL reports no blockers, Looper writes a Chinese technical Spec, runs the existing technical GRILL + REVIEW, and waits for the local `owner` to approve the current Spec revision.

Plane is the only inbound source for this flow. Each responsible person posts a **new comment on the work item** using the question ID, for example:

```text
DESIGN-001: DESIGN-001-B
ENG-001: 自定义: 采用后台队列并保留三次重试
```

Use the exact option ID when one fits; when none fits, the responsible person may write `问题ID: 自定义: 清晰决定`. `待定` / “later” and malformed option IDs invalidate any older answer and keep the gate closed. A large new page, multi-step flow or information-architecture change uses a design-document gate instead: answer `问题ID: https://...` with the design file/document URL. Formal product Specs must be a non-empty native Plane page attached in the same work item's Links with the title `looper:product-spec`; an external link, blank placeholder page or ordinary comment does not satisfy that formal gate. Technical-Spec approval is posted on the technical Spec page. Feishu only sends notifications, screenshots and Plane links; replies/cards in Feishu do not answer requirement questions or approve a technical Spec. This rule is separate from generic coding-agent HITL, which may still use its configured callback transport.

Identity is fail-closed. `planeId` must be the Plane member UUID returned by `plane api me` (for yourself) or `plane api member workspace-list` (for teammates). A wrong role, an unconfigured actor, a comment before the current request, or an approval for an older Spec revision cannot unblock the loop. A Feishu `open_id` may be @-mentioned even when that user is not in the notification group; a grey @ is acceptable and membership is not a gate.

When several teammates run Looper locally and send into one notification group, every task anchor, live-progress card and HITL card includes a small `来自 @owner 的 Looper` footer. It reuses the current installation's project `owner.feishuOpenId`; this attribution does not change who is authorized to answer the card.

`LOOPER_PLANE_AUTO_INTAKE=1` is a separate, process-wide mutation switch for `looper:auto` classification. Keep it unset in an isolated/planner-only verification process. Looper also skips auto-intake for projects whose effective planner and worker `autoDiscovery` values are both false, so an inherited production environment cannot route unrelated Plane items from an isolated project.

For real UI choices, Looper produces 2–3 restricted static HTML options, renders fixed-size PNGs with locally installed Chrome/Chromium, and posts them in the task thread. The renderer rejects scripts, event handlers, frames/forms, external/file/data URLs and CSS network primitives. It never runs an agent-provided render command, downloads a browser, or serves the business worktree. Runtime artifacts live beside `storage.dbPath` under `decision-artifacts/`. If automatic probing cannot find Chrome, set `tools.browserPath` to the absolute Chrome/Chromium executable path.

## Interactive HITL (mid-run ask / answer)

The `notifications.webhook` block above is **send-only** (an incoming-webhook URL): it posts run updates but can't collect an answer. To let an agent **pause mid-run, ask a human, and resume the same session with the reply**, enable HITL and switch the webhook to Feishu **app-bot** mode, which sends an interactive card with option buttons and receives the click back.

```jsonc
{
  "hitl": { "enabled": true },
  "notifications": {
    "webhook": {
      "enabled": true,
      "mode": "app",                                // app-bot instead of incoming-webhook
      "appIdEnv": "LOOPER_FEISHU_APP_ID",           // env var NAMES, never the secret values
      "appSecretEnv": "LOOPER_FEISHU_APP_SECRET",
      "chatId": "oc_xxxxxxxxxxxxxxxx"               // target group/chat for one-way notifications
    }
  }
}
```

How it works:

1. When an agent hits a genuine blocker it writes `.looper/ask.json` and stops. The worker posts the question to Plane/GitHub and suspends the loop as `awaiting_human`.
2. Feishu sends a one-way, targeted notification with a deep link to the exact Plane/GitHub location. Feishu card clicks and thread replies are never consumed.
3. The human answers in Plane/GitHub. The blocked-condition reconciler observes that source-of-truth update and resumes the same vendor session. The authenticated `/respond` API remains available for dashboard/operator control.

## How discovery maps Plane labels to roles

- Planner and Worker discovery poll the Plane project and keep only work-items that carry **all** of `roles.<role>.triggers.labels` (label UUIDs are resolved to names first). With the bootstrap defaults both roles trigger on a single `--trigger-label` (default `looper:plan`).
- Because Plane assignees are UUIDs — not GitHub logins — discovery keys on the label only. The bootstrap sets `requireAssigneeCurrentUser: false`; do not set it to `true` for plane projects or discovery will never match.
- Coordinator and Fixer discovery lanes are skipped for plane projects (they are GitHub-only). Reviewer runs against the GitHub PRs that Worker opens.
- Label and comment mutations Looper performs during a run go to Plane: adding a label creates it if missing and merges (never clobbers) the work-item's existing labels; comments are posted as `comment_html`.

## Known follow-ups

These are documented gaps, not blockers:

- **Discovery cache.** Each discovery tick re-fetches all labels and all work-items for both Planner and Worker, so large projects make the discovery lane slow. A short-lived per-tick cache is a planned optimization.
- **State-group filtering.** The provider currently reports every returned work-item as `open` and does not drop Plane `completed`/`cancelled` items. A work-item that reaches a done state but still carries the trigger label would be re-discovered until the label is removed. Planned: resolve Plane state groups and filter out done/cancelled items (fail-open so active items are never dropped).
- **Legacy Worker comments.** Worker progress still uses the legacy generic comment model. The V2 decision flow uses its own UUID-preserving work-item comment contract for actor authority and revision boundaries.

## Verify

1. Export `PLANE_API_KEY` (and the Feishu webhook env if used), then start the daemon: `looper daemon start`.
2. Confirm the plane project is discovered and claims a labelled work-item — watch for a planner/worker loop and queue item:
   ```bash
   looper ps
   looper status
   ```
   A work-item carrying the trigger label (e.g. `looper:plan`) should produce a planner loop within one poll interval, and Worker opens the PR on the GitHub `repo`.
