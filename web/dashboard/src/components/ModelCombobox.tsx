import {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
} from "react";
import {
  fetchAgentModels,
  type AgentModelEntry,
  type AgentModelsData,
} from "@/lib/api";

const controlClass =
  "w-full rounded border border-[var(--border)] bg-[var(--bg)] px-2 py-1 text-[12px] text-[var(--text)] disabled:cursor-not-allowed disabled:opacity-60";

/**
 * Advisory suggestion cache. Backend already caches ~60s per vendor; this just
 * avoids extra round trips on repeated focus and keeps the previous list on
 * screen while a refresh probe is in flight. Cache freshness (timestamp) is
 * the sole authority for whether we skip a fetch — no separate "fetched"
 * ref that could strand the control after an aborted request.
 */
const CACHE_TTL_MS = 60_000;
const cache = new Map<string, { at: number; data: AgentModelsData }>();

type SpecialKind = "inherit" | "unbound" | "vendor-default";
type Row =
  | { kind: "special"; special: SpecialKind; label: string; hint?: string }
  | { kind: "model"; entry: AgentModelEntry }
  | { kind: "custom"; value: string };

/**
 * Stable logical identity for a listbox row. Custom free-entry rows and catalog
 * model rows that share the same id map to the same key so keyboard `active`
 * can be remapped when a temporary custom binding row is replaced by the real
 * catalog entry (or vice versa) without retaining a stale numeric index.
 */
function rowIdentity(row: Row): string {
  if (row.kind === "special") return `special:${row.special}`;
  if (row.kind === "model") return `value:${row.entry.id}`;
  return `value:${row.value}`;
}

export type ModelComboboxProps = {
  id?: string;
  ariaLabel?: string;
  /** Effective vendor resolved by the caller (nullable). */
  vendor: string | null;
  /**
   * Current draft (or published) binding:
   * - `null` = persisted absence (not the same as `""`)
   *   - with allowInherit: Inherit from previous layer
   *   - without (global): unbound — params/CLI model may still apply
   * - `""` = explicit vendor-default suppress
   * - non-empty = model id
   */
  value: string | null;
  /** True when caller has staged an unset (inherit). Displayed as read-only. */
  unset: boolean;
  disabled: boolean;
  /** True to include an "Inherit" row (profile / role scopes). */
  allowInherit: boolean;
  placeholder?: string;
  /**
   * Stage a value as free entry / id pick / vendor default. Caller decides how
   * to map "" (vendor default suppress vs no-op) based on scope semantics — see
   * buildConfigPatch tri-state rules.
   */
  onCommitValue: (value: string) => void;
  /**
   * Stage an inherit (unset). Required when allowInherit=true.
   */
  onInherit?: () => void;
  /**
   * Restore global absence (null). Required for the global scope where Inherit
   * is not offered: clears a draft back to published null, or stages unset when
   * a binding exists. Distinct from Vendor default (`""` suppress).
   */
  onUnbound?: () => void;
};

export function ModelCombobox({
  id,
  ariaLabel,
  vendor,
  value,
  unset,
  disabled,
  allowInherit,
  placeholder,
  onCommitValue,
  onInherit,
  onUnbound,
}: ModelComboboxProps) {
  const generatedId = useId();
  const listboxId = `${id ?? generatedId}-listbox`;
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState<string | null>(null);
  // null = no keyboard navigation yet. Enter with null commits the typed
  // value (or does nothing) rather than blindly selecting row zero.
  const [active, setActive] = useState<number | null>(null);
  const [entries, setEntries] = useState<AgentModelEntry[] | null>(null);
  const [probeError, setProbeError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const rootRef = useRef<HTMLDivElement | null>(null);
  const listRef = useRef<HTMLUListElement | null>(null);
  // Previous row snapshot for remapping `active` by identity when the list
  // changes under an open popup (catalog load, filter, custom-row collapse).
  // Tracked in state (not a ref) so concurrent render discard stays correct.
  const [prevRows, setPrevRows] = useState<Row[]>([]);

  // Single close path: clear popup, transient query, and navigation state.
  // Parent `value` is authoritative once closed. Escape uses this path without
  // staging, so discarded keystrokes never reach the draft.
  const close = useCallback(() => {
    setOpen(false);
    setQuery(null);
    setActive(null);
  }, []);

  // Commit free-entry text (or empty clear) then close. Used by Enter, Tab,
  // and blur — not Escape, which cancels without staging.
  const commitTypedAndClose = useCallback(() => {
    if (query !== null) {
      onCommitValue(query.trim());
    }
    close();
  }, [query, onCommitValue, close]);

  // Load / refresh suggestions when the popup opens (or vendor changes while
  // open). Clear prior UI state first, then restore from the module cache or
  // fetch — a separate vendor-clear effect used to run *after* a fresh cache
  // hit and wipe the restored list for the rest of the open session.
  useEffect(() => {
    setEntries(null);
    setProbeError(null);
    setLoading(false);

    if (!open || !vendor) return;

    const cached = cache.get(vendor);
    if (cached) setEntries(cached.data.models);
    if (cached && Date.now() - cached.at < CACHE_TTL_MS) {
      setProbeError(
        cached.data.sources.probe === "error"
          ? cached.data.sources.probeError ??
              "Using built-in list; CLI probe failed"
          : null,
      );
      return;
    }
    const controller = new AbortController();
    setLoading(true);
    fetchAgentModels(vendor, { signal: controller.signal })
      .then((data) => {
        if (controller.signal.aborted) return;
        cache.set(vendor, { at: Date.now(), data });
        setEntries(data.models);
        setProbeError(
          data.sources.probe === "error"
            ? data.sources.probeError ??
                "Using built-in list; CLI probe failed"
            : null,
        );
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setProbeError(
          err instanceof Error
            ? `Model suggestions unavailable (${err.message})`
            : "Model suggestions unavailable",
        );
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => {
      controller.abort();
      setLoading(false);
    };
  }, [open, vendor]);

  // Outside click stages free-entry text then closes (not cancel). Escape is
  // the only path that discards the local query without committing.
  useEffect(() => {
    if (!open) return;
    const handler = (event: MouseEvent) => {
      if (!rootRef.current) return;
      if (!rootRef.current.contains(event.target as Node)) commitTypedAndClose();
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, [open, commitTypedAndClose]);

  // Tri-state binding:
  // - null = persisted absence (not the same as explicit "")
  // - ""   = explicit vendor-default suppress
  // - id   = model id
  // When Inherit is offered (profile/role), absence is Inherit. For global
  // (!allowInherit), absence stays unbound: agent.params.args --model/-m may
  // still apply. Only explicit "" is Vendor default; collapsing null → ""
  // would reaffirm as suppress and strip params model flags on save.
  // Computed before `rows` so a concrete binding missing from the catalog
  // can still be represented as a current-state custom row.
  const absenceState = unset || value === null;
  const isInheritState = allowInherit && absenceState;
  const isUnboundState = !allowInherit && absenceState;
  const isVendorDefaultState = !unset && value === "";
  const boundValue = value ?? "";
  const hasConcreteBinding =
    !isInheritState && !isUnboundState && !isVendorDefaultState && boundValue !== "";

  const rows = useMemo<Row[]>(() => {
    const specials: Row[] = [];
    if (allowInherit) {
      specials.push({
        kind: "special",
        special: "inherit",
        label: "Inherit",
        hint: "Fall back to the previous layer (profile / global / vendor default).",
      });
    } else if (onUnbound) {
      // Global scope: restore absence (null), not layer inherit. Needed when
      // the published leaf is default-sourced so FieldFrame has no Unset.
      specials.push({
        kind: "special",
        special: "unbound",
        label: "Unbound",
        hint: "No agent.model binding; agent.params.args --model/-m may still apply.",
      });
    }
    specials.push({
      kind: "special",
      special: "vendor-default",
      label: "Vendor default",
      hint: "Suppress inherited model; use the vendor CLI default.",
    });

    const q = (query ?? "").trim().toLowerCase();
    const source = entries ?? [];
    const filtered = q
      ? source.filter(
          (m) =>
            m.id.toLowerCase().includes(q) ||
            m.label.toLowerCase().includes(q),
        )
      : source;

    const modelRows: Row[] = filtered.map((entry) => ({
      kind: "model",
      entry,
    }));

    // Custom / free-entry row when the typed query is a novel id, or when the
    // parent already binds a concrete id that is absent from the (possibly
    // still-loading) catalog. Without the latter, stateRowIndex is null and
    // first ArrowDown falls through to special row 0 (Inherit/Unbound).
    let customRow: Row | null = null;
    if (q) {
      if (
        !filtered.some((m) => m.id.toLowerCase() === q) &&
        // Skip "custom" when the query happens to equal a special label.
        q !== "inherit" &&
        q !== "unbound" &&
        q !== "vendor default"
      ) {
        customRow = { kind: "custom", value: (query ?? "").trim() };
      }
    } else if (
      hasConcreteBinding &&
      !source.some((m) => m.id === boundValue)
    ) {
      customRow = { kind: "custom", value: boundValue };
    }

    return customRow ? [...specials, customRow, ...modelRows] : [...specials, ...modelRows];
  }, [
    entries,
    query,
    allowInherit,
    onUnbound,
    hasConcreteBinding,
    boundValue,
  ]);

  // Keep keyboard `active` on the same logical option when the row set
  // changes. Range-only clamping is insufficient: while the catalog loads we
  // may insert a temporary custom row for the saved binding; ArrowDown can
  // land on it by index, then the real catalog entry replaces that custom
  // row and the same numeric index points at a different model. Remap by
  // identity (custom value ≡ model id); if the option disappeared, clear
  // navigation so Enter does not commit an unrelated row.
  //
  // Adjust during render (React "adjusting state when a prop changes") so the
  // highlighted option matches the new list in the same commit as the catalog
  // swap. An effect-based remap painted one frame with a stale index and made
  // the identity-remap test flake under CI timing.
  if (rows !== prevRows) {
    setPrevRows(rows);
    if (active !== null) {
      const prev = prevRows[active];
      if (prev) {
        const id = rowIdentity(prev);
        const nextIdx = rows.findIndex((r) => rowIdentity(r) === id);
        if (nextIdx >= 0) {
          if (nextIdx !== active) setActive(nextIdx);
        } else {
          setActive(null);
        }
      } else if (active >= rows.length) {
        setActive(rows.length > 0 ? rows.length - 1 : null);
      } else {
        setActive(null);
      }
    }
  }

  // Scroll the active option into view on keyboard navigation.
  useEffect(() => {
    if (!open || active === null || !listRef.current) return;
    const el = listRef.current.children[active] as HTMLElement | undefined;
    // scrollIntoView is a browser API; jsdom stubs it out — guard for tests.
    if (typeof el?.scrollIntoView === "function") {
      el.scrollIntoView({ block: "nearest" });
    }
  }, [active, open]);

  // What to render inside the input. When closed, defer to the controlled
  // `value` so a discarded/rebased parent draft never leaves stale query
  // text on screen. When open, show the current query (or fall back to value).
  const displayValue = open
    ? (query ?? boundValue)
    : isInheritState || isUnboundState
      ? ""
      : boundValue;

  // Which row represents the current parent state (for aria-selected when the
  // operator hasn't navigated yet). Matches model suggestions and the
  // synthetic custom row for out-of-catalog / still-loading bindings.
  const stateRowIndex = useMemo<number | null>(() => {
    if (isInheritState) {
      const idx = rows.findIndex(
        (r) => r.kind === "special" && r.special === "inherit",
      );
      return idx >= 0 ? idx : null;
    }
    if (isUnboundState) {
      const idx = rows.findIndex(
        (r) => r.kind === "special" && r.special === "unbound",
      );
      return idx >= 0 ? idx : null;
    }
    if (isVendorDefaultState) {
      const idx = rows.findIndex(
        (r) => r.kind === "special" && r.special === "vendor-default",
      );
      return idx >= 0 ? idx : null;
    }
    const idx = rows.findIndex(
      (r) =>
        (r.kind === "model" && r.entry.id === value) ||
        (r.kind === "custom" && r.value === value),
    );
    return idx >= 0 ? idx : null;
  }, [rows, value, isInheritState, isUnboundState, isVendorDefaultState]);

  const commitRow = useCallback(
    (row: Row) => {
      if (row.kind === "special") {
        if (row.special === "inherit") {
          if (onInherit) onInherit();
        } else if (row.special === "unbound") {
          if (onUnbound) onUnbound();
        } else {
          onCommitValue("");
        }
      } else if (row.kind === "model") {
        onCommitValue(row.entry.id);
      } else {
        onCommitValue(row.value);
      }
      close();
    },
    [onCommitValue, onInherit, onUnbound, close],
  );

  // First non-special row (custom free-entry or model suggestion). Used when
  // a filter is active or when a concrete binding has no state row yet.
  const firstChoiceIndex = (): number => {
    const idx = rows.findIndex((r) => r.kind === "model" || r.kind === "custom");
    return idx >= 0 ? idx : 0;
  };

  // First keyboard step when the operator has not arrow-navigated yet.
  // With a typed filter, start on the first custom/model suggestion so
  // ArrowDown+Enter does not land on Inherit / Vendor default (specials
  // always precede filtered rows, and stateRowIndex is often null once the
  // current binding is filtered out). Without a filter, start on the row
  // that reflects parent state; if that is still missing for a concrete
  // binding, prefer the first non-special row over special row 0 so Enter
  // cannot unexpectedly clear the binding to Inherit/Unbound.
  const initialNavIndex = (): number => {
    const hasFilter = query !== null && query.trim() !== "";
    if (hasFilter) return firstChoiceIndex();
    if (stateRowIndex !== null) return stateRowIndex;
    if (hasConcreteBinding) return firstChoiceIndex();
    return 0;
  };

  const onKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setOpen(true);
      setActive((idx) => {
        if (idx === null) return initialNavIndex();
        return Math.min(rows.length - 1, idx + 1);
      });
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      setOpen(true);
      setActive((idx) => {
        if (idx === null) return initialNavIndex();
        return Math.max(0, idx - 1);
      });
    } else if (event.key === "Enter") {
      // Only commit a row when the operator has explicitly arrow-navigated
      // to it. An untouched Enter commits any edited query — including an
      // empty clear (same as blur/Tab). query === null means no edit, so
      // keep the parent value.
      if (open && active !== null && rows[active]) {
        event.preventDefault();
        commitRow(rows[active]);
      } else if (query !== null) {
        event.preventDefault();
        commitTypedAndClose();
      } else if (open) {
        event.preventDefault();
        close();
      }
    } else if (event.key === "Escape") {
      // Cancel: discard local query without staging. Parent draft stays at
      // the pre-edit value because keystrokes no longer call onCommitValue.
      if (open) {
        event.preventDefault();
        close();
      }
    } else if (event.key === "Tab") {
      // Tab away stages free-entry text (same as blur) but does not pick a row.
      if (open) commitTypedAndClose();
    }
  };

  // aria-activedescendant: prefer explicit keyboard-navigated active row.
  // When the operator has not navigated, expose the row that reflects current
  // parent state (if any) so screen readers announce the current selection.
  const ariaActiveIdx = active ?? stateRowIndex;
  const activeId =
    open && ariaActiveIdx !== null && rows[ariaActiveIdx]
      ? `${listboxId}-opt-${ariaActiveIdx}`
      : undefined;

  // Placeholder reflects tri-state when the input renders empty and closed.
  // Staged unset locks the control; natural absence still shows Inherit /
  // Unbound but remains editable so operators can pick a model without Undo.
  const closedPlaceholder = isInheritState
    ? "Inherit"
    : isUnboundState
      ? "Unbound"
      : isVendorDefaultState
        ? "Vendor default"
        : placeholder;

  return (
    <div ref={rootRef} className="relative min-w-0">
      <input
        id={id}
        aria-label={ariaLabel}
        role="combobox"
        aria-expanded={open}
        aria-controls={listboxId}
        aria-autocomplete="list"
        aria-activedescendant={activeId}
        className={`${controlClass} mono ${unset ? "opacity-50" : ""}`}
        type="text"
        spellCheck={false}
        autoComplete="off"
        placeholder={open ? placeholder : closedPlaceholder}
        disabled={disabled || unset}
        value={displayValue}
        onFocus={() => setOpen(true)}
        onClick={() => setOpen(true)}
        onBlur={(event) => {
          // Ignore blur when focus moves to our own popup (listbox mousedown).
          if (
            rootRef.current &&
            event.relatedTarget instanceof Node &&
            rootRef.current.contains(event.relatedTarget)
          ) {
            return;
          }
          // Stage free-entry text on leave so Save without Enter still works;
          // Escape uses close() only and never reaches here with a pending query
          // that should be discarded (Escape closes first without blur commit
          // when the input keeps focus).
          commitTypedAndClose();
        }}
        onChange={(event) => {
          const next = event.currentTarget.value;
          // Keep typing local until commit (Enter / pick / Tab / blur). Staging
          // every keystroke made Escape unable to cancel: parent draft already
          // held the half-edited id, so closing only cleared the local query.
          setQuery(next);
          setOpen(true);
          setActive(null);
        }}
        onKeyDown={onKeyDown}
      />
      {open && !disabled && !unset ? (
        <div
          className="absolute left-0 right-0 z-30 mt-1 max-h-64 overflow-auto rounded border border-[var(--border)] bg-[var(--bg-elevated)] shadow-lg"
          data-testid="model-combobox-popup"
        >
          {!vendor ? (
            <p className="m-0 px-2 py-1.5 text-[11px] text-[var(--text-muted)]">
              Select a vendor first — suggestions will load automatically. You
              can still type any model id.
            </p>
          ) : null}
          {probeError ? (
            <p
              className="m-0 border-b border-[var(--border)] px-2 py-1 text-[10px] text-[var(--warn)]"
              role="status"
            >
              {probeError}
            </p>
          ) : null}
          {loading && (!entries || entries.length === 0) ? (
            <p className="m-0 px-2 py-1.5 text-[11px] text-[var(--text-muted)]">
              Loading suggestions…
            </p>
          ) : null}
          <ul
            id={listboxId}
            ref={listRef}
            role="listbox"
            className="m-0 list-none p-0"
          >
            {rows.map((row, idx) => {
              const isActive = idx === active;
              const isCurrent = active === null && idx === stateRowIndex;
              const label =
                row.kind === "special"
                  ? row.label
                  : row.kind === "model"
                    ? row.entry.label || row.entry.id
                    : `Use "${row.value}"`;
              const sub =
                row.kind === "special"
                  ? row.hint
                  : row.kind === "model"
                    ? row.entry.id !== label
                      ? row.entry.id
                      : row.entry.source
                    : "Custom model id";
              return (
                <li
                  key={
                    row.kind === "special"
                      ? `sp-${row.special}`
                      : row.kind === "model"
                        ? `m-${row.entry.id}`
                        : `c-${row.value}`
                  }
                  id={`${listboxId}-opt-${idx}`}
                  role="option"
                  aria-selected={isActive || isCurrent}
                  className={`flex cursor-pointer flex-col gap-0.5 px-2 py-1 text-[12px] ${
                    isActive
                      ? "bg-[color-mix(in_srgb,var(--accent)_20%,transparent)]"
                      : isCurrent
                        ? "bg-[color-mix(in_srgb,var(--accent)_10%,transparent)]"
                        : "hover:bg-[var(--bg-muted)]"
                  }`}
                  // Use mousedown so the input's blur does not close first.
                  onMouseDown={(event) => {
                    event.preventDefault();
                    commitRow(row);
                  }}
                  onMouseEnter={() => setActive(idx)}
                >
                  <span
                    className={
                      row.kind === "model" ? "mono" : "font-medium"
                    }
                  >
                    {label}
                  </span>
                  {sub ? (
                    <span className="text-[10px] text-[var(--text-muted)]">
                      {sub}
                    </span>
                  ) : null}
                </li>
              );
            })}
            {rows.length === 0 && vendor && !loading ? (
              <li
                role="option"
                aria-selected={false}
                aria-disabled
                className="px-2 py-1.5 text-[11px] text-[var(--text-muted)]"
              >
                No suggestions. Type a model id to use it directly.
              </li>
            ) : null}
          </ul>
        </div>
      ) : null}
    </div>
  );
}
