import { useCallback, useEffect, useState } from "react";

export type ThemeMode = "light" | "dark" | "system";

export const THEME_STORAGE_KEY = "looper.dashboard.theme";
export const THEME_MODES: readonly ThemeMode[] = ["light", "dark", "system"];

/** Narrow arbitrary strings to a valid mode. Falls back to "system". */
export function normalizeThemeMode(value: unknown): ThemeMode {
  if (value === "light" || value === "dark" || value === "system") {
    return value;
  }
  return "system";
}

/** Reads persisted mode; safe on SSR / private-mode / non-DOM environments. */
export function getTheme(): ThemeMode {
  try {
    if (typeof window === "undefined") return "system";
    const raw = window.localStorage.getItem(THEME_STORAGE_KEY);
    return normalizeThemeMode(raw);
  } catch {
    return "system";
  }
}

/**
 * Writes `data-theme` on <html>. Uses "system" (not attribute removal) so
 * consumers can still detect an explicit user preference via the DOM.
 * The CSS also matches "no attribute" for the initial paint fallback.
 */
export function applyTheme(mode: ThemeMode): void {
  if (typeof document === "undefined") return;
  document.documentElement.dataset.theme = mode;
}

/** Persists and applies. Notifies subscribers via a custom event. */
export function setTheme(mode: ThemeMode): void {
  const next = normalizeThemeMode(mode);
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, next);
  } catch {
    // Ignore quota/private-mode errors; visual apply still succeeds.
  }
  applyTheme(next);
  if (typeof window !== "undefined") {
    window.dispatchEvent(
      new CustomEvent<ThemeMode>("looper:theme-change", { detail: next }),
    );
  }
}

/**
 * Resolve the concrete palette a given mode currently displays as. Used only
 * for icon hints; the actual paint is CSS-driven via data-theme + media query.
 */
export function resolvedTheme(mode: ThemeMode): "light" | "dark" {
  if (mode === "light" || mode === "dark") return mode;
  if (
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function"
  ) {
    return window.matchMedia("(prefers-color-scheme: dark)").matches
      ? "dark"
      : "light";
  }
  return "light";
}

/**
 * React hook: current mode + setter. Keeps in sync with:
 *  - direct setTheme() calls in this tab (custom event)
 *  - localStorage writes from other tabs (storage event)
 *  - OS color-scheme changes while mode="system" (matchMedia)
 * The last one only re-renders so `resolvedTheme` icons flip; CSS handles paint.
 */
export function useTheme(): {
  mode: ThemeMode;
  resolved: "light" | "dark";
  setMode: (mode: ThemeMode) => void;
} {
  const [mode, setMode] = useState<ThemeMode>(() => getTheme());
  const [resolved, setResolved] = useState<"light" | "dark">(() =>
    resolvedTheme(mode),
  );

  // Keep data-theme aligned with React state on mount and mode changes.
  // Required because production CSP blocks inline HTML init scripts.
  useEffect(() => {
    applyTheme(mode);
    setResolved(resolvedTheme(mode));
  }, [mode]);

  useEffect(() => {
    const onCustom = (event: Event) => {
      const detail = (event as CustomEvent<ThemeMode>).detail;
      setMode(normalizeThemeMode(detail));
    };
    const onStorage = (event: StorageEvent) => {
      if (event.key !== THEME_STORAGE_KEY) return;
      const next = normalizeThemeMode(event.newValue);
      applyTheme(next);
      setMode(next);
    };
    window.addEventListener("looper:theme-change", onCustom);
    window.addEventListener("storage", onStorage);
    return () => {
      window.removeEventListener("looper:theme-change", onCustom);
      window.removeEventListener("storage", onStorage);
    };
  }, []);

  useEffect(() => {
    if (mode !== "system") return;
    if (typeof window === "undefined" || !window.matchMedia) return;
    const mql = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => setResolved(mql.matches ? "dark" : "light");
    // Safari <14 uses addListener; modern browsers use addEventListener.
    if (typeof mql.addEventListener === "function") {
      mql.addEventListener("change", onChange);
      return () => mql.removeEventListener("change", onChange);
    }
    mql.addListener(onChange);
    return () => mql.removeListener(onChange);
  }, [mode]);

  const update = useCallback((next: ThemeMode) => {
    setTheme(next);
    setMode(normalizeThemeMode(next));
  }, []);

  return { mode, resolved, setMode: update };
}
