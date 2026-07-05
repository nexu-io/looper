# Plane task-source + Feishu HITL reference for agents

Use this when the user's issues live in a [Plane](https://plane.so) project instead of GitHub, but the code and pull requests stay on GitHub. Looper reads work-items from Plane (the task source) and opens/reviews PRs on the GitHub `repo` (the code forge). For end-user prose, see [`docs/plane-provider.md`](../../../docs/plane-provider.md).

## When to use

- Issues/tasks are tracked in Plane, and the team does not want a redundant GitHub issue mirror.
- Code, PRs, and reviews remain on GitHub (so an authenticated `gh` is still required).

If issues already live on GitHub, use the default `--provider github` flow instead.

## One-shot bootstrap flags

`--provider plane` generates a **fresh** config. Only run it when no Looper config exists yet; otherwise it errors (remove the existing config or pass `--config <new-path>`).

```bash
looper bootstrap --yes \
  --provider plane \
  --project-path "$REPO" \
  --code-repo <owner>/<repo> \
  --plane-workspace <workspace-slug> \
  --plane-project <plane-project-uuid> \
  --trigger-label looper:plan \
  --feishu-webhook-env LOOPER_FEISHU_WEBHOOK_URL \
  --agent-vendor "<selected-vendor>"
```

| Flag | Required | Default | Meaning |
| --- | --- | --- | --- |
| `--provider plane` | yes | `github` | Select the Plane task source. |
| `--project-path` | yes | — | Local checkout of the GitHub code repo (`repoPath`). |
| `--code-repo owner/repo` | no | detected from `--project-path` git origin | GitHub repo where PRs are opened. |
| `--plane-workspace` | yes | — | Plane workspace slug. |
| `--plane-project` | yes | — | Plane project UUID. |
| `--plane-base-url` | no | `https://plane.powerformer.net/api/v1` | Plane REST API base. |
| `--plane-token-env` | no | `PLANE_API_KEY` | Env var holding the Plane API key. |
| `--trigger-label` | no | `looper:plan` | Issue label that triggers Planner/Worker discovery. |
| `--feishu-webhook-env` | no | — | Env var holding a Feishu/generic webhook URL; adds a `notifications.webhook` feishu block. Works with any provider. |

## Two env vars to export

These are read from the daemon environment and never written to the config. Export them before starting `looperd`:

```bash
export PLANE_API_KEY="<plane-api-key>"                      # matches --plane-token-env
export LOOPER_FEISHU_WEBHOOK_URL="<feishu-bot-webhook-url>" # matches --feishu-webhook-env
```

Redact both as `***` in any summary. An authenticated `gh` is still needed for the GitHub code side.

## Generated config shape

```json
{
  "providers": [
    { "id": "plane-<workspace>", "kind": "plane",
      "baseUrl": "https://plane.powerformer.net/api/v1",
      "tokenEnv": "PLANE_API_KEY",
      "workspace": "<workspace-slug>",
      "projectId": "<plane-project-uuid>" }
  ],
  "projects": [
    { "id": "<repo-name>", "name": "<repo-name>",
      "provider": "plane-<workspace>",
      "repo": "<owner>/<repo>",
      "repoPath": "<project-path>" }
  ],
  "roles": {
    "planner": { "autoDiscovery": true, "triggers": { "labels": ["looper:plan"], "labelMode": "all", "requireAssigneeCurrentUser": false } },
    "worker":  { "autoDiscovery": true, "triggers": { "labels": ["looper:plan"], "labelMode": "all", "requireAssigneeCurrentUser": false } }
  },
  "notifications": { "webhook": { "enabled": true, "urlEnv": "LOOPER_FEISHU_WEBHOOK_URL", "format": "feishu", "levels": ["action_required", "failure"] } }
}
```

Key facts an agent must not get wrong:

- Plane assignees are UUIDs, not GitHub logins, so discovery keys on the label only. Keep `requireAssigneeCurrentUser: false`; setting it `true` makes discovery match nothing.
- `repo` is the GitHub code repo where PRs land; `workspace`/`projectId` on the provider point at Plane.
- Coordinator and Fixer lanes are skipped for plane projects; Reviewer runs against the GitHub PRs Worker opens.

## Verify (confirm Plane discovery)

After exporting the env vars and starting the daemon:

```bash
looper daemon start
looper status
looper project list      # the plane project should be listed
looper ps                # a labelled Plane work-item yields a planner/worker loop within one poll interval
```

A Plane work-item that carries the trigger label (e.g. `looper:plan`) should produce a planner loop, and Worker opens the PR on the GitHub `repo`. If nothing is discovered, confirm: `PLANE_API_KEY` is exported and valid, the work-item carries the exact trigger label, and `requireAssigneeCurrentUser` is `false`.

## Known follow-ups

- Discovery re-fetches all Plane labels + work-items each tick (slow on large projects); a per-tick cache is planned.
- The provider reports all work-items as `open` and does not yet drop Plane `completed`/`cancelled` items — a done item that keeps the trigger label is re-discovered until the label is removed.
- Worker posts a fresh progress comment per status transition (Plane comment ids are UUIDs, so in-place edits are not supported).
