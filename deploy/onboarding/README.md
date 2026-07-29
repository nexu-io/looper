# looper HITL onboarding bundle

A one-shot package that lets a teammate's coding agent configure and start looper
with Plane/GitHub HITL and one-way Feishu notifications.

## Distributor (you, who has the shared secrets)
1. Make sure the shared Feishu app can send messages and you have a filled `hitl.env` (from
   [`deploy/hitl.env.example`](../hitl.env.example)). One-time, app-level: whitelist
   `http://127.0.0.1:53682/callback` as a redirect URL in the shared Feishu app's
   security settings, so every teammate's `looper login` can capture their open_id
   (else it fails with `20029 重定向 URL 有误`). Also decide the team-wide
   `productOwner` + `qa` open_ids and give them to teammates for the config template.
2. Build the bundle:
   ```sh
   deploy/onboarding/build-bundle.sh /path/to/your/hitl.env
   # → looper-hitl-onboarding.zip
   ```
3. Send the zip **privately** (it contains the app secrets). An internal chat
   or a private link is fine; don't put it in a public repo.

## Recipient (the teammate)
1. Unzip it.
2. Make sure `looperd` / `looper` and your coding agent (`codex` / `claude`) are
   installed and authenticated, and the target repo is cloned locally.
3. Open your coding agent **in the unzipped folder** and paste the contents of
   `SETUP-PROMPT.md`. It will ask for your repo path, your Feishu group + open_id,
   fill your config, load the shared secrets, and start looperd — confirming with
   you along the way.

That's it — the agent walks through the rest.
