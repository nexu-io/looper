import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  applyTheme,
  getTheme,
  normalizeThemeMode,
  resolvedTheme,
  setTheme,
  THEME_STORAGE_KEY,
} from "./theme";

describe("normalizeThemeMode", () => {
  it("accepts the three known modes verbatim", () => {
    expect(normalizeThemeMode("light")).toBe("light");
    expect(normalizeThemeMode("dark")).toBe("dark");
    expect(normalizeThemeMode("system")).toBe("system");
  });

  it("falls back to system for anything else", () => {
    expect(normalizeThemeMode(null)).toBe("system");
    expect(normalizeThemeMode(undefined)).toBe("system");
    expect(normalizeThemeMode("")).toBe("system");
    expect(normalizeThemeMode("hi-contrast")).toBe("system");
    expect(normalizeThemeMode(42)).toBe("system");
  });
});

describe("getTheme / setTheme / applyTheme", () => {
  beforeEach(() => {
    window.localStorage.clear();
    delete document.documentElement.dataset.theme;
  });

  afterEach(() => {
    window.localStorage.clear();
    delete document.documentElement.dataset.theme;
  });

  it("defaults to system when nothing is stored", () => {
    expect(getTheme()).toBe("system");
  });

  it("round-trips explicit modes through localStorage", () => {
    setTheme("dark");
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe("dark");
    expect(getTheme()).toBe("dark");
    expect(document.documentElement.dataset.theme).toBe("dark");

    setTheme("light");
    expect(getTheme()).toBe("light");
    expect(document.documentElement.dataset.theme).toBe("light");
  });

  it("normalizes bad stored values to system", () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, "neon");
    expect(getTheme()).toBe("system");
  });

  it("applyTheme mutates the html dataset without touching storage", () => {
    applyTheme("dark");
    expect(document.documentElement.dataset.theme).toBe("dark");
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBeNull();
  });

  it("setTheme dispatches a custom event with the new mode", () => {
    const spy = vi.fn();
    window.addEventListener("looper:theme-change", spy as EventListener);
    setTheme("light");
    expect(spy).toHaveBeenCalledTimes(1);
    const detail = (spy.mock.calls[0][0] as CustomEvent).detail;
    expect(detail).toBe("light");
    window.removeEventListener("looper:theme-change", spy as EventListener);
  });

  it("getTheme + applyTheme restores an explicit stored preference onto the DOM", () => {
    // Mirrors main.tsx startup and CSP-safe theme-init.js: without this,
    // reloading with a saved light/dark mode leaves data-theme unset and the
    // palette follows the OS while the toggle reports the stored value.
    window.localStorage.setItem(THEME_STORAGE_KEY, "light");
    expect(document.documentElement.dataset.theme).toBeUndefined();
    applyTheme(getTheme());
    expect(document.documentElement.dataset.theme).toBe("light");
  });
});

describe("resolvedTheme", () => {
  const originalMatchMedia = window.matchMedia;

  afterEach(() => {
    window.matchMedia = originalMatchMedia;
  });

  it("passes explicit modes through unchanged", () => {
    expect(resolvedTheme("light")).toBe("light");
    expect(resolvedTheme("dark")).toBe("dark");
  });

  it("uses matchMedia to resolve system mode", () => {
    window.matchMedia = vi.fn().mockReturnValue({
      matches: true,
      media: "(prefers-color-scheme: dark)",
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
      onchange: null,
    }) as unknown as typeof window.matchMedia;
    expect(resolvedTheme("system")).toBe("dark");

    window.matchMedia = vi.fn().mockReturnValue({
      matches: false,
      media: "(prefers-color-scheme: dark)",
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
      onchange: null,
    }) as unknown as typeof window.matchMedia;
    expect(resolvedTheme("system")).toBe("light");
  });
});
