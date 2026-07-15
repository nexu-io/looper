# ADR-0013: Local operator dashboard is an embedded SPA on looperd (not Hub)

## Context

Operators need a browser view of local Looper state beyond CLI. The repo is Go-first with a mature loopback JSON API (`/api/v1/*`) and no supported frontend. A separate Hub admin UI (multi-node, central control plane) is specified elsewhere and must not be conflated with single-daemon local ops.

Options considered:

1. **TUI** in the CLI process
2. **CLI-served** short-lived HTTP UI proxying looperd
3. **Hub-seed** SPA sharing architecture with future Hub
4. **Embedded SPA on looperd** opened by `looper dashboard`
5. **Server-rendered** Go templates / HTMX only

## Decision

Ship a **local operator dashboard** as a **React + Vite + TypeScript SPA** (Tailwind CSS v4 + shadcn/ui), built assets **`//go:embed`’d into looperd**, served under **`/dashboard/`**, consuming the **existing `/api/v1/*` API**.

`looper dashboard` ensures the daemon is healthy and opens the browser. Domain authority remains forge + SQLite via looperd; the UI is presentation and control only.

This is **not** the Hub Admin UI and must not become a second control plane or shared Hub codebase in v1.

## Trade-off

**Prevents:** CLI-only ops friction for multi-panel live state; confusion between local ops and Hub; inventing a parallel dashboard domain API.

**Costs:** Node toolchain in CI/release; SPA embed/base-path complexity; auth bootstrap for `local-token` browser sessions; ongoing dual-surface maintenance (CLI + UI) toward full CLI parity.

**Why not simpler alternatives:**

- TUI: poor fit for full CLI-mirror north star (logs + multi-panel + forms).
- CLI-served UI: duplicates HTTP/auth lifetime; “dashboard up while daemon down” is wrong for ops.
- Hub-seed: different authority model and product; warps local UX.
- Pure Go templates: fights reactive multi-panel + log follow for little CI savings once SPA is chosen.

## Authority

Authority for dashboard-visible runtime state and control actions is **looperd** (SQLite + existing control endpoints). Forge remains authority for remote work eligibility. The dashboard never becomes authority.

## Consequences

- New tree: `web/dashboard/`; CI builds frontend before Go release artifacts.
- looperd gains static/SPA routes and minimal auth bootstrap endpoints; no parallel domain API.
- Product docs and naming use **dashboard**, never Hub, for this surface.
- Full CLI parity is north star; v1 is observe + control only (see `specs/2026-07-15-local-dashboard-v1/spec.md`).
