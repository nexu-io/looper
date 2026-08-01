# Agent model catalog (dashboard suggestions + vendor probe)

## 1. Background

Looper already supports per-binding agent **vendor** and **model**:

- Global: `agent.vendor`, `agent.model`
- Profiles: `agent.profiles.<id>.{vendor,model}`
- Coding roles: `roles.{planner,worker,reviewer,fixer}.agent.{profile,vendor,model}`

Resolve order, empty-string suppress → vendor default, params `--model` strip,
hot-reload for new claims, and sticky `agent_snapshot` freeze are implemented
(see `docs/configuration.md`, ADR-0014, ADR-0016).

Dashboard Config today uses a **vendor select** and a **free-text model** field.
Operators who want Looper to **override vendor-local CLI defaults** and run a
chosen model stably often do not know valid ids, or leave model unset and
silently inherit home-config defaults.

**Product intent:** make explicit model configuration easy and aligned with
vendor configuration surfaces — not introduce a new identity model or a
validation gate.

Formal decisions: **[ADR-0016](../../docs/adr/0016-agent-model-catalog-is-advisory.md)**.

## 2. Goals

1. Expose a **suggested model list** per agent vendor, merged from:
   - a small **static** embedded catalog, and
   - **on-demand CLI probe** when the vendor supports non-interactive listing.
2. Let the **dashboard** configure model wherever vendor is configurable
   (global, profiles, four coding roles) via a **searchable combobox**, while
   **still allowing arbitrary hand-entered ids**.
3. Keep existing identity semantics: tri-state model (`unset` / `""` / id),
   ResolveAgent overlay, snapshot freeze, argv override strength.
4. Fail soft: probe errors never block config read/write or daemon work.

## 3. Non-goals (kill list)

1. Hard-require model ∈ catalog on save or claim
2. Require non-empty model to run coding roles
3. Project-scoped agent vendor/model
4. Coordinator-specific agent binding (remains global-only)
5. Anthropic / OpenAI / other **cloud Models HTTP** as catalog source
6. Changing sticky resume/retry to re-resolve live config model
7. Run detail / execution record “actual model used” observability (later)
8. User-editable `~/.looper/models.json` override files (later if needed)
9. Rewriting config IA to push profiles-only or global-only workflows
10. Proving per-vendor that CLI flag always beats home config (document assumption only)
11. Interactive TUI scraping (`claude` `/model`, Codex `/model`)
12. Dashboard editing of `agent.params` (still file/restart-bound)

## 4. Decisions (locked)

| Topic | Decision |
| --- | --- |
| Authority of list | **Advisory only** (ADR-0016) |
| Sources | Static embed + on-demand probe; probe failure → static |
| Claude Code | **Static only** (no list CLI) |
| Other vendors | Probe when possible: opencode, codex, cursor-cli, grok-build |
| Codex probe | Prefer `codex debug models --bundled` (stable/offline); treat as experimental surface |
| Binary resolution | **Same as spawn** (default name or `agent.params.command`); absolute path; never bare ambiguous `agent` |
| Cache | In-process memory, short TTL, key = vendor + resolved binary path |
| When probe runs | On demand when UI/API requests models for a vendor (not full five-vendor startup sweep) |
| Validation | None on save; free text always allowed |
| UI control | Searchable combobox; full list (including long OpenCode output) |
| Special values | **Inherit** (unset) where overlay applies; **Vendor default** (explicit `""`); concrete ids |
| Which vendor for list | **Effective vendor** after same overlay as ResolveAgent for that control |
| Config surfaces | Parity with vendor: global + profiles + planner/worker/reviewer/fixer |
| Apply timing | New claims only; in-flight + sticky keep snapshot |
| Strength bar | Configuration reachability — not force-explicit, not run observability |
| IA / narrative | No preferred path; same logic as vendor fields |

## 5. Model tri-state (unchanged; UI must map explicitly)

| Binding | Meaning |
| --- | --- |
| Field absent / unset (`nil`) | Inherit previous layer (role → profile → global → vendor CLI default) |
| Explicit empty string `""` | Suppress inherit; force vendor CLI default; strip params `--model`/`-m` |
| Non-empty string | Explicit model id passed to CLI |

Global has no Inherit (top of overlay). Profile/role show Inherit when unset.

Custom values not present in the catalog remain selectable/displayable once set.

## 6. Effective vendor for catalog requests

When the operator edits model on a control, the catalog vendor is the **resolved**
vendor for that binding — not only the leaf `vendor` field:

1. Inline `vendor` on that binding if set
2. Else profile’s vendor if `profile` set
3. Else global `agent.vendor`

If no vendor resolves: return static empty or vendor-agnostic message; do not
probe; UI prompts to select a vendor first.

## 7. HTTP API (looperd)

### 7.1 Endpoint

```
GET /api/v1/agent/models?vendor=<AgentVendor>
```

- `vendor` required; must be a known `AgentVendor` enum value.
- Auth: same as other `/api/v1/*` (loopback / local-token).
- Not a config mutation; read-only; safe to poll subject to server-side cache.

### 7.2 Response shape (normative intent)

```json
{
  "vendor": "codex",
  "models": [
    { "id": "gpt-5.4", "label": "GPT-5.4", "source": "probe" }
  ],
  "sources": {
    "static": true,
    "probe": "ok"
  },
  "probedAt": "2026-08-01T12:00:00Z"
}
```

| Field | Notes |
| --- | --- |
| `models[].id` | String passed through as config model / CLI flag value |
| `models[].label` | Optional display string; may equal id |
| `models[].source` | `static` \| `probe` (or `merged` if both); UI may ignore |
| `sources.probe` | `ok` \| `skipped` \| `error` \| `unsupported` |
| `sources.probeError` | Optional short message when `error` (never fatal to client) |
| `probedAt` | Set when probe attempted or served from cache |

Merge rules:

- Union by `id`; probe label wins on conflict when present.
- Stable-enough ordering: static recommendations first, then probe-only ids
  (implementation may sort alpha within buckets).
- Deduplicate exact ids.

### 7.3 Probe behavior

| Vendor | Command / source (v1 intent) | Notes |
| --- | --- | --- |
| `opencode` | `opencode models` | One id per line (`provider/model`) |
| `codex` | `codex debug models --bundled` | Parse JSON `models[]`; prefer `visibility=list` when field exists |
| `cursor-cli` | resolved `cursor-agent`/`agent` binary `models` or `--list-models` | Text parse; empty if logged out |
| `grok-build` | `grok models` | Text parse; optional fallback read of well-known cache only if needed |
| `claude-code` | none | `probe: unsupported`; static only |

Shared rules:

- Timeout short (seconds); kill process group on expiry.
- Do not pass the operator prompt or start an agent session.
- Inherit env only as required for CLI auth (same machine user as looperd).
- Cache hit skips subprocess until TTL expires; optional `?refresh=1` may bypass cache (nice-to-have).

### 7.4 Static catalog

- Embedded in the binary (Go embed JSON or equivalent).
- Per vendor: small set of common ids/aliases (Claude aliases like `sonnet` /
  `opus` / `haiku` are high value).
- Not aiming for completeness; probe supplies breadth where available.
- Updating the table is a normal code change / release, not a runtime user file.

## 8. Dashboard UX

### 8.1 Surfaces

Replace free-text model inputs on Config for:

- Global agent model
- Each agent profile model
- Each coding role agent model

Vendor controls unchanged in meaning; switching vendor keeps existing
clear/pair guards in `configForm` and reloads suggestions for the new effective
vendor.

### 8.2 Combobox

- Searchable filter over `id` and `label`.
- Always include special rows where applicable: Inherit, Vendor default.
- Always allow commit of a custom string (create option / free entry).
- If current saved value ∉ list, still show it as the selected value.
- Loading: show static or previous list immediately if cached; probe may populate
  asynchronously without blocking the form.
- Probe error: optional non-blocking hint (“Using built-in list; CLI probe
  failed”) — must not use modal blockers or disable Save.

### 8.3 OpenCode scale

Return and search the **full** probed list. No magic provider filter in v1.
Native `<select>` is insufficient; use a combobox/command palette pattern
consistent with dashboard component library.

## 9. Runtime override (existing; no behavior change required for v1)

When resolved/snapshot model is non-empty, executor strips conflicting params
model flags and prepends vendor model flags. Catalog work must **not** weaken
this path.

Unset model continues to defer to vendor local defaults — by design for v1
strength bar (reachability, not mandate).

## 10. Docs

- `docs/configuration.md`: note model suggestions API/dashboard; restate
  advisory nature; link ADR-0016.
- Do not document the catalog as a allowlist.

## 11. Verification

Minimum credible evidence for implementation PRs:

| Check | Intent |
| --- | --- |
| Unit: static catalog load + merge/dedupe | Pure logic |
| Unit: per-vendor probe parsers with fixtures | No live CLI in CI required |
| Unit/API: unknown vendor → 4xx; missing vendor → 4xx | Contract |
| Unit/API: probe subprocess failure → 200 + static + `probe=error` | Fail soft |
| Config form: tri-state still patches unset vs `""` vs id | No regression |
| Manual or component: combobox custom value + search | UX |
| `go test` for touched packages; dashboard typecheck/build as today | CI |

Out of scope for mandatory CI: live `codex`/`opencode`/`claude` on runners.

## 12. Ship shape

Suggested slices (flexible):

1. **ADR + this spec** (docs only)
2. **Backend:** static catalog + probe adapters + `GET .../agent/models` + tests
3. **Dashboard:** combobox wired to API on all vendor-parity model fields
4. **Docs:** configuration.md touch-up

## 13. Done bar

- [ ] ADR-0016 merged
- [ ] API returns merged suggestions per vendor; claude static-only; fail soft
- [ ] Dashboard model fields are searchable suggestions with free entry + tri-state
- [ ] Effective vendor drives the list for profile/role controls
- [ ] No new validation rejecting unknown models
- [ ] Snapshot / hot-reload / resolve semantics unchanged (regression tests green)
- [ ] configuration.md updated

## 14. Open implementation choices (not product locks)

Left to implementer unless they force a product change:

- Exact TTL duration and `refresh` query
- JSON field names beyond §7.2 intent
- Combobox component choice within existing design system
- Whether Grok reads `~/.grok/models_cache.json` as secondary
- Codex filter on `visibility`
- Static file format (JSON vs Go literals)
