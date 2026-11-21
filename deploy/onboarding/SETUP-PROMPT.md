# Prompt — set up looper (Plane/GitHub HITL + Feishu notifications)

Send the whole block below to your coding agent (Claude Code / codex / …) and run it **inside this unzipped directory**.

---

You are helping me set up and start **looper** — a daemon that runs an autonomous dev agent. Plane/GitHub hold all decisions; Feishu is a one-way notification channel. Work **interactively**: confirm with me before any outward-facing (GitHub / Feishu) or irreversible action.

## What's in this bundle
- `config.hitl.example.json` — the config template, with placeholders.
- `hitl.env` — the team's shared Feishu app credentials (already filled; **do not print it, do not commit it to git, do not paste it into chat**).
- `GUIDE-hitl-setup.md` — reference doc; read it first.

## Do this
1. **Check prerequisites** and tell me what's missing:
   - `looperd` and `looper` are on PATH (if not, ask me for the path / how to install or build them).
   - My coding-agent binary (`codex` or `claude`) is installed and **logged in / authorized** (run it once to confirm it works).
   - The GitHub repo I want looper to work on is already cloned locally.
2. **Collect my settings** (ask me, don't guess). First open `config.hitl.example.json` and see which fields are still `REPLACE_...` placeholders — the distributor may have **pre-filled** some (typically the Feishu **group chat id** and the `productOwner` / `qa` open_ids). Whatever is already filled, **leave it as-is**; only collect what's still a placeholder:
   - GitHub repo (`owner/repo`) plus the **absolute path** of its local clone.
   - My Feishu **group chat id** (`oc_...`) — only if it's still a placeholder.
   - The `productOwner` and `qa` open_ids (**team-wide**; only if still placeholders — otherwise the distributor already set them).
   - **You don't need to hunt for my own open_id** — step 5 grabs it automatically via `looper login` (a quick browser authorization; it writes it into the project `owner`, and prints it so you can also set `notifications.webhook.mentionOpenIds`).
   - The absolute path of my coding-agent binary, and whether I use `codex` or `opencode` (see "Backend" below).
   - Where looper keeps its data/logs (default `~/.looper`).
   - If the group chat id is pre-filled to a **shared** team group, make sure I'm actually a member of it (otherwise I won't see the cards or get @-mentioned).
3. **Write my config**: copy `config.hitl.example.json` to `~/.looper/config.json` and replace every **remaining** `REPLACE_...` / `/ABSOLUTE/...` / `OWNER/REPO` / `FILLED_BY_looper_login_...` placeholder with the values above — do not touch fields the distributor already filled (leave the `owner` one for now — `looper login` fills it in the next step). When done, **show me the final config** to confirm (it holds no secrets, so it's safe to display).
   - **Backend**: the template defaults to `codex` (`-c model=gpt-5.4 …`). If I use `opencode`, change `agent.vendor` to `opencode`, point `command` at the opencode binary, set `model` to `openai/gpt-5.4`, and replace `args` with `["--pure","--dangerously-skip-permissions"]` (codex's `-c/-s` flags mean different things in opencode — don't mix them up).
4. **Load the shared secrets**: `source` the `hitl.env` in this bundle. **Do not** print the secret values, and **do not** copy them into the config file (the config only references variable names).
5. **Log into Feishu to get my open_id** (automatic; `source hitl.env` first so the app credentials are in the shell):
   ```sh
   source <bundle-path>/hitl.env                  # app_id/secret etc. enter this shell
   looper login --config ~/.looper/config.json    # browser authorization; writes my open_id into projects[].owner and prints it
   ```
   `looper login` writes only the project `owner.feishuOpenId`. Take the open_id it prints and also put it in `notifications.webhook.mentionOpenIds` (replace the `REPLACE_WITH_YOUR_OPEN_ID_ou_xxx` placeholder) so decision cards actually @-mention me. Then show me the final config to confirm.
   (Prerequisite: the shared Feishu app already has `http://127.0.0.1:53682/callback` in its "redirect URL allowlist" — that's a **one-time, app-level** setup the distributor does, not your concern; if you hit `20029 redirect URL is invalid`, ask the distributor to add it.)
6. **Install it as a resident service and start it** (so it auto-restarts on sleep/wake, reboot, or crash — don't run it bare with `nohup`):
   ```sh
   looper daemon install
   looper daemon start --config ~/.looper/config.json --daemon-restart-policy on-failure
   ```
   Confirm it's alive: `looper daemon status --config ~/.looper/config.json`. (The secrets come from the `hitl.env` you sourced above and get baked into the launchd service, so they survive restarts.)
7. **Smoke test** (ask me first): create a small issue with the `looper:plan` label and confirm an ambiguity is written to Plane/GitHub, while Feishu only sends an @-mention and deep link to that exact location. Do not configure any Feishu callback or event subscription.

## Guardrails
- Never commit `hitl.env` or the config into git; never paste secrets into chat.
- **Ask me first** before creating a GitHub issue/PR or posting anything to Feishu.
- If a prerequisite is missing, stop and tell me — don't pretend it's set up.
