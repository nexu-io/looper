# Local operator dashboard v1 (`looper dashboard`)

## 1. Background

Looper is a local AI dev-team daemon (`looperd`) plus CLI (`looper`). Operators observe and control work today via CLI (`status`, `ps`, `loop *`, `logs`, …) against a JSON HTTP API on loopback (default `127.0.0.1:17310`).

There is no product web UI. An old unsupported `apps/web` placeholder was removed. A separate **Hub** admin UI (multi-node, Postgres) is a different product and is out of scope here.

North-star product intent: the dashboard is a **full mirror of looper CLI capabilities** over time. v1 is intentionally a smaller, shippable milestone.

## 2. Goals

1. Provide a **local browser operator console** opened by `looper dashboard`.
2. Make **current multi-project state glanceable** and allow **safe intervention** on existing loops/runs.
3. Reuse the existing **`/api/v1/*` JSON API** as the sole domain contract (plus minimal auth bootstrap + static/SPA serving).
4. Ship as **one Go binary**: React SPA built assets embedded into `looperd`.
5. Stay clearly **not Hub** and **not a second authority** over forge labels or agent structured output.

## 3. Non-goals (v1 kill list)

1. Hub / multi-node admin
2. Queue, PR browser, run-history pages
3. Config / network / webhook admin UIs
4. Role wizards / create-work flows (committed as **v1.1**, not vague backlog)
5. Project add/remove from UI
6. Daemon start/stop/restart from the browser (CLI ensures daemon on open)
7. SSE / WebSocket push channels
8. Mobile-first layout
9. i18n
10. Metrics / charts / analytics
11. Feishu / HITL card management inside the dashboard
12. Editing agent prompts or provider secrets in UI
13. Playwright / full UI e2e in CI
14. Multi-user RBAC or shared-host team design

## 4. Decisions (locked)

| Topic | Decision |
| --- | --- |
| Product intent | Full CLI mirror is **north star**; v1 is observe + control |
| Surface | Browser UI (not TUI) |
| Serving | **looperd** serves UI; CLI opens it |
| SPA stack | React + Vite + TypeScript |
| UI kit | **Tailwind CSS v4 + shadcn/ui** components |
| Aesthetic | Dense, terminal-like ops UI (compact tables, monospace IDs/seqs); theme follows **system** light/dark |
| API | Existing `/api/v1/*` only for domain data/actions |
| Live updates | Polling (tiered); log follow via existing stream; pause when tab hidden |
| Auth | Follow daemon `server.authMode` (`none` \| `local-token`) |
| Token bootstrap | One-shot short-lived **code** exchange when `local-token` (no long-lived token in query string) |
| Address | Normal CLI config/flags; overrides allowed; non-loopback allowed if configured |
| Operator model | Single local operator |
| Naming | `dashboard` everywhere (`looper dashboard`, `/dashboard`, `web/dashboard`) |
| Hub | Explicitly separate; ignore for this work |
| Browsers | Modern evergreen only (Chrome/Edge/Firefox/Safari) |
| Docs | This spec + thin ADR |
| Ship order | Few large PRs (see §10) |
| Done bar | Checklist in §11 |

## 5. CLI: `looper dashboard`

```bash
looper dashboard
```

Behavior:

1. Resolve daemon address from the same config/env/flag precedence as other CLI daemon clients.
2. **Ensure daemon is up** (start if needed; wait until healthy).
3. If `local-token` mode: obtain a short-lived bootstrap code from looperd.
4. Open the default browser to the dashboard home URL (with `?code=…` only when needed).
5. Print the URL to stdout.
6. Exit (no long-lived CLI attach; UI lifetime is looperd’s).

v1 does **not** deep-link into specific loops/projects (`looper dashboard` always opens home).

Optional escape hatch (small, allowed): `--no-open` to print URL only (SSH/headless). Not required for first slice if it delays PR1, but preferred.

## 6. HTTP surface (looperd)

### 6.1 Static + SPA

- Mount prefix: **`/dashboard/`** (History API client router).
- Serve embedded production assets from the Go binary.
- SPA fallback: non-file routes under `/dashboard/*` → `index.html`.
- Domain API remains **`/api/v1/*`** (unchanged contract family).

### 6.2 Auth bootstrap (allowed minimal API)

Only when `server.authMode=local-token`:

1. CLI (or equivalent trusted local caller) requests a **short-lived one-shot code**.
2. Browser opens `/dashboard/?code=…`.
3. SPA exchanges code for a session usable on subsequent `/api/v1/*` calls (Bearer or same-origin session as implemented).
4. SPA removes `code` from the URL after exchange.

Rules:

- Never put the long-lived API token in the query string.
- Codes expire quickly and are single-use.
- When `authMode=none`, no bootstrap; SPA calls API directly on loopback.

### 6.3 Domain API (consume existing)

Primary consumers for v1 pages/actions (illustrative; exact handlers already exist under `internal/api`):

| Need | API (existing) |
| --- | --- |
| Health / overview | `GET /api/v1/healthz`, `GET /api/v1/status` |
| Running now | `GET /api/v1/runs/active` |
| Loops list/detail | `GET /api/v1/loops`, `GET /api/v1/loops/{id\|seq}` |
| Logs follow | `GET /api/v1/loops/{id}/logs` (+ follow) |
| Projects list | `GET /api/v1/projects` |
| Control | `POST .../pause`, `retry`, `start`, `takeover`, `handback`, active-run stop, etc. as already exposed |

No parallel `/dashboard/*` domain resources. No aggregate BFF unless a later milestone proves pain.

## 7. v1 information architecture

### 7.1 Shell chrome

- Title: **Looper**
- Connection: `host:port`
- Health indicator
- Active run counts (summary)
- Version in footer/about (not a second overview)

### 7.2 Pages

| Page | Purpose | CLI analogue |
| --- | --- | --- |
| **Overview** | Daemon/provider/scheduler health summary | `status` |
| **Running** | Active runs table | `ps` |
| **Loops** | List + detail | `loop list` / `inspect` |
| **Loop logs** | Follow/tail pane on detail | `logs` |
| **Projects** | List + **filter hub** only | `project list` (read) |

### 7.3 Projects as filter

- Default view is **global** (all projects).
- Selecting a project filters Running/Loops.
- Last selected filter is sticky in `localStorage`.
- No project create/archive UI in v1.

### 7.4 Actions (v1)

On loop/run detail (where API supports):

- pause / unpause (or start, per existing API semantics)
- retry
- resume (where applicable)
- stop
- takeover
- handback

Confirm dialogs **only** for high-impact: **stop**, **takeover**, **handback**.  
Pause/retry/unpause/resume: one click + toast.

Action enablement: **conservative client-side** map from known status rules; never invent permissions. API 4xx still shown. No new `allowedActions` field in v1.

### 7.5 Logs pane

- Follow/tail stream
- Pause auto-scroll
- Clear view
- Copy
- No search, no multi-run history browser in v1

### 7.6 Live updates

| Surface | Cadence (tab visible) |
| --- | --- |
| Running | ~2s |
| Loop/run detail | ~3s |
| Overview / Loops list | ~5s |
| Tab hidden | pause polling |

### 7.7 Failure UX

- Daemon/health unreachable → **full-page** error with CLI recovery hints.
- Individual resource failures → **per-panel** error + retry.
- No “show stale cached data as live” behavior.

## 8. Frontend package

- Path: **`web/dashboard/`**
- Stack: React + Vite + TypeScript
- Styling: **Tailwind CSS v4**
- Components: **shadcn/ui** (copy-in components; keep density high—prefer compact tables over marketing card layouts)
- Router: History API under `/dashboard/`
- Daily dev: Vite dev server proxies `/api/v1` to a running looperd
- Release: `pnpm install --frozen-lockfile && pnpm run build` → embed dist into looperd

Aesthetic constraint: shadcn defaults trend “product UI”; theme tokens and layout must still read as **dense ops console** (tight spacing, monospace for ids/seqs/paths, table-first).

## 9. Build, CI, test

### 9.1 CI

1. Build `web/dashboard` (`pnpm install --frozen-lockfile && pnpm run build`)
2. `gofmt` / `go vet` / `go test` / `go build` as today
3. Fail if embed assets are missing when dashboard is expected in the release binary

Do **not** commit `dist/` to git.

### 9.2 Tests (v1 bar)

- **Go**: static/SPA fallback serving; auth bootstrap code lifecycle; `looper dashboard` ensure/open behavior (as testable without a real browser where practical)
- **API contracts**: keep/extend frozen contracts for routes the UI depends on (`internal/api/testdata/contracts/`)
- **Not required**: Playwright, broad frontend unit suite

### 9.3 Local embed smoke

Occasionally run full `pnpm run build` + looperd embed path to verify base path, SPA fallback, and auth headers—not the daily UI loop.

## 10. Delivery plan (few large PRs)

| PR | Scope |
| --- | --- |
| **PR1** | `web/dashboard` skeleton (React/Vite/TS, Tailwind v4, shadcn), embed into looperd, `/dashboard` SPA fallback, CI frontend build, `looper dashboard` ensure/open, Overview/health home, auth bootstrap if token mode |
| **PR2** | Read-only Running, Loops list/detail/logs, Projects filter hub, polling, failure states, chrome counts |
| **PR3** | Mutations: pause/retry/stop/takeover/handback + confirms + toasts + conservative enablement |

v1.1 (separate, committed intent): full role wizards (plan/work/review/fix) mirroring CLI create flows.

## 11. Done definition (v1)

Shippable when:

- [ ] `looper dashboard` ensures daemon, opens browser, prints URL
- [ ] Overview, Running, Loops (+ detail/logs), Projects filter work against live API
- [ ] v1 actions work with correct confirm rules
- [ ] Daemon-down full-page state works
- [ ] Embed + CI frontend build green
- [ ] Contracts cover UI-used API routes
- [ ] Auth: `none` works; `local-token` uses one-shot code bootstrap
- [ ] Non-goals above remain out

## 12. Authority note

The dashboard is a **client** of looperd. It must not introduce a new source of truth for:

- forge work eligibility (labels, PR/issue state)
- loop/run execution authority (SQLite via daemon)
- agent decisions (structured agent output remains authority for agent-driven side effects)

UI actions only invoke existing daemon control endpoints; forge remains source of truth for remote work state.

## 13. Open implementation details (non-blocking)

These may be resolved during PR1 without reopening product scope:

- Exact bootstrap endpoint paths and session storage mechanism (cookie vs memory Bearer)
- Precise mapping of CLI “unpause/resume” verbs to existing POST routes
- shadcn component set subset and token theme for dense dark/light
- Whether `--no-open` lands in PR1 or a tiny follow-up

## 14. References

- ADR-0013: Local operator dashboard (embed SPA on looperd; not Hub)
- Existing API: `internal/api/handler.go`, `internal/api/server.go`
- CLI daemon client: `internal/cliapp/daemon_client.go`
- Prior signal: `specs/2026-04-11-running-task-ps/spec.md` (`/runs/active` for future web “Running Now”)
- Hub (out of scope): `specs/2026-07-07-looper-hub-v1/spec.md`
