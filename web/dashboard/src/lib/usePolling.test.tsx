import { act, cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { usePolling } from "./usePolling";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    get: () => "visible",
  });
});

function Probe({
  fetcher,
  intervalMs = 60_000,
  enabled = true,
  pollKey,
  onResult,
}: {
  fetcher: (signal: AbortSignal) => Promise<string>;
  intervalMs?: number;
  enabled?: boolean;
  pollKey?: string;
  onResult: (r: ReturnType<typeof usePolling<string>>) => void;
}) {
  const result = usePolling({
    intervalMs,
    enabled,
    fetcher,
    key: pollKey,
  });
  onResult(result);
  return null;
}

describe("usePolling", () => {
  it("clears data and sets stale on error", async () => {
    let calls = 0;
    const fetcher = vi.fn(async () => {
      calls += 1;
      if (calls === 1) return "ok";
      throw new Error("boom");
    });

    let latest: ReturnType<typeof usePolling<string>> | null = null;
    const { rerender } = render(
      <Probe
        fetcher={fetcher}
        onResult={(r) => {
          latest = r;
        }}
      />,
    );

    await waitFor(() => {
      expect(latest?.data).toBe("ok");
      expect(latest?.error).toBeNull();
      expect(latest?.stale).toBe(false);
    });

    await act(async () => {
      latest?.refresh();
    });

    // Force a second render so refresh result is observed.
    rerender(
      <Probe
        fetcher={fetcher}
        onResult={(r) => {
          latest = r;
        }}
      />,
    );

    await waitFor(() => {
      expect(latest?.error).toBe("boom");
      expect(latest?.data).toBeNull();
      expect(latest?.stale).toBe(true);
    });
  });

  it("resets data immediately when key changes", async () => {
    const fetcher = vi.fn(async (signal: AbortSignal) => {
      // Delay so we can observe the cleared intermediate state.
      await new Promise<void>((resolve, reject) => {
        const t = window.setTimeout(() => resolve(), 30);
        signal.addEventListener("abort", () => {
          window.clearTimeout(t);
          reject(new DOMException("aborted", "AbortError"));
        });
      });
      return "value-a";
    });

    let latest: ReturnType<typeof usePolling<string>> | null = null;
    const { rerender } = render(
      <Probe
        fetcher={fetcher}
        pollKey="a"
        onResult={(r) => {
          latest = r;
        }}
      />,
    );

    await waitFor(() => {
      expect(latest?.data).toBe("value-a");
    });

    const fetcherB = vi.fn(async () => "value-b");
    rerender(
      <Probe
        fetcher={fetcherB}
        pollKey="b"
        onResult={(r) => {
          latest = r;
        }}
      />,
    );

    // After key change, previous data must not linger.
    expect(latest?.data).toBeNull();

    await waitFor(() => {
      expect(latest?.data).toBe("value-b");
    });
  });

  it("refreshes when the tab becomes visible", async () => {
    let visibility = "visible";
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      get: () => visibility,
    });

    const fetcher = vi.fn(async () => "ok");
    let latest: ReturnType<typeof usePolling<string>> | null = null;
    render(
      <Probe
        fetcher={fetcher}
        onResult={(r) => {
          latest = r;
        }}
      />,
    );

    await waitFor(() => expect(latest?.data).toBe("ok"));
    const afterMount = fetcher.mock.calls.length;

    visibility = "hidden";
    act(() => {
      document.dispatchEvent(new Event("visibilitychange"));
    });

    visibility = "visible";
    act(() => {
      document.dispatchEvent(new Event("visibilitychange"));
    });

    await waitFor(() => {
      expect(fetcher.mock.calls.length).toBeGreaterThan(afterMount);
    });
  });

  it("forceRefresh aborts in-flight poll and always completes a new fetch", async () => {
    let resolveSlow: ((value: string) => void) | null = null;
    let calls = 0;
    const fetcher = vi.fn(async (signal: AbortSignal) => {
      calls += 1;
      if (calls === 1) {
        return "initial";
      }
      if (calls === 2) {
        return await new Promise<string>((resolve, reject) => {
          resolveSlow = resolve;
          signal.addEventListener("abort", () => {
            reject(new DOMException("aborted", "AbortError"));
          });
        });
      }
      return "forced";
    });

    let latest: ReturnType<typeof usePolling<string>> | undefined;
    render(
      <Probe
        fetcher={fetcher}
        onResult={(r) => {
          latest = r;
        }}
      />,
    );

    await waitFor(() => expect(latest?.data).toBe("initial"));

    // Start a slow non-force refresh that would otherwise block overlap.
    act(() => {
      latest?.refresh();
    });
    await waitFor(() => expect(calls).toBe(2));

    let forceDone = false;
    await act(async () => {
      const force = latest?.forceRefresh;
      expect(force).toBeTypeOf("function");
      await force!();
      forceDone = true;
    });

    expect(forceDone).toBe(true);
    await waitFor(() => {
      expect(latest?.data).toBe("forced");
    });
    // Slow resolver must not clobber forced result if it somehow settles.
    resolveSlow?.("stale-slow");
    expect(latest?.data).toBe("forced");
  });
});
