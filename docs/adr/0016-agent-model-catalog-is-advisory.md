# ADR-0016: Agent model catalog is advisory (static + probe); config identity unchanged

## Context

Coding roles already bind agent **identity** as vendor + model
(`agent`, `agent.profiles.*`, `roles.{planner,worker,reviewer,fixer}.agent`) with
resolve overlay, hot-reload for new claims, and per-run `agent_snapshot` freeze
(ADR-0014). Dashboard and file config accept model as free text.

Operators want to **override vendor-local defaults** (CLI home config / last-used
model) so Looper stably launches with an explicit model. The missing piece is not
a new identity field — it is **discoverability**: which model ids a given vendor
CLI accepts on this machine, surfaced where vendor is already configurable.

Vendor CLIs are heterogeneous (verified 2026-08-01):

| Vendor | Scriptable model list |
| --- | --- |
| opencode | `opencode models` (line-oriented; strong) |
| codex | `codex debug models [--bundled]` (JSON; experimental) |
| cursor-cli | `cursor-agent models` (text; auth-scoped) |
| grok-build | `grok models` (text; optional cache file) |
| claude-code | **none** (interactive `/model` only) |

Bare `agent` is ambiguous across installs (Cursor vs Grok). Looper must not treat
a model directory as a gate over the agent CLI.

## Decision

1. **Catalog is advisory UX, not authority.**  
   Config continues to store arbitrary model strings (including unset / explicit
   empty → vendor default). Saving never requires membership in the catalog.
   Runtime spawn authority remains: resolved/snapshot model → argv `--model`/`-m`
   (and params model-flag strip) as today. The catalog does not validate runs.

2. **Hybrid source: static baseline + on-demand CLI probe.**  
   - Static: small embedded per-vendor id (and optional label) table shipped with
     Looper — especially required for **claude-code**.  
   - Probe: daemon invokes the **same resolved binary** used for spawn (including
     `agent.params.command` when present), short timeout, **in-process TTL cache**
     keyed by vendor + binary path. Probe failure → static only; never blocks
     config load or save.  
   - No Anthropic (or other cloud) Models HTTP API in v1 — identity is the local
     agent vendor CLI, not a second credential plane.

3. **Per-vendor probe policy (v1).**  
   Probe where a non-interactive list exists (opencode, codex bundled debug JSON,
   cursor-agent, grok). **claude-code: static only.** Prefer absolute resolved
   paths; never discover via ambiguous bare `agent`.

4. **API serves merged suggestions for dashboard (and any future clients).**  
   One read endpoint parameterized by vendor (and using spawn-equivalent binary
   resolution). Response may include ids, optional labels, and probe status
   metadata for non-blocking UI hints. Implementation details live in the spec.

5. **Configuration surface parity with vendor.**  
   Wherever the product already allows configuring agent **vendor** (global,
   named profiles, coding-role bindings), model keeps the same editability and
   the same suggestion UX. No new project-scoped agent identity; coordinator
   triage remains global-only. No IA push toward “profiles only” or “global only.”

6. **Lifecycle unchanged.**  
   Model (like vendor) is hot-safe for **new claims** only. In-flight runs and
   sticky resume/retry keep frozen `agent_snapshot` identity (ADR-0014).

7. **Strength of “stable model” for v1.**  
   Product bar is **configuration reachability**: operators can find and set a
   non-empty model so argv override applies. v1 does **not** require explicit
   model to run, does not re-resolve model on sticky retry, and does not add
   run-detail “actual model” observability (may be a later slice).

## Trade-off

**Prevents:** Operators guessing model ids; silent reliance on vendor home-config
defaults when they intended a Looper-pinned model; blocking config on flaky CLI
probe or stale hard gates; inventing cloud API identity parallel to CLI vendors.

**Costs:** Static tables drift and need occasional refresh; probe adapters and
text/JSON parsers per vendor; experimental Codex debug surface may change;
long OpenCode lists need searchable UI; catalog never proves the child process
honored the flag (CLI/session semantics remain outside Looper).

**Why not simpler / stronger alternatives:**

- **Static-only:** fails the “extract from vendor” goal where CLIs already list
  models well (especially OpenCode/Codex).
- **Probe-only:** Claude has no list command; unauthenticated Cursor returns empty;
  dashboard would often be blank.
- **Hard-validate model ∈ catalog:** rejects valid new models ahead of Looper
  tables/CLIs; contradicts “agent CLI is the runner.”
- **Require non-empty model to claim work:** breaks users who intentionally want
  vendor default; large migration hammer for a discoverability feature.
- **Anthropic HTTP for Claude models:** different auth than Claude Code OAuth;
  list ≠ Code aliases/subscription set; second authority plane.
- **Sticky retry adopts live config model:** undermines run-lineage stability
  that snapshots exist to provide.

## Authority

| Question | Authority |
| --- | --- |
| What vendor/model identity a **new** claim uses | Config file overlay (global → profile → role) per existing resolve; ADR-0014 hot-safe publication |
| What vendor/model an **in-flight / sticky** run uses | Durable `agent_snapshot` on the run |
| Whether a model string is “allowed” | **Not Looper** — advisory catalog only; operator + vendor CLI |
| Whether spawn passes `--model` | Non-empty resolved/snapshot model on the existing executor path |
| Catalog contents | Looper static embed ∪ best-effort local CLI probe (non-authoritative) |

## Consequences

- New read API + probe/static merge in looperd; dashboard model fields become
  searchable comboboxes with Inherit / Vendor default / id (tri-state preserved).
- Spec: `specs/2026-08-01-agent-model-catalog/spec.md`.
- Docs (`docs/configuration.md`) should describe catalog as suggestions and
  restate model tri-state + snapshot freeze; no new gate language.
- Follow-ups explicitly out of ADR scope: force-explicit-model policy, run-level
  model observability, project agent bindings, Claude API catalog, sticky
  re-resolve on retry.
