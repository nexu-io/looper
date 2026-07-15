import { useCallback, useEffect, useRef, useState } from "react";

export type UsePollingOptions<T> = {
  intervalMs: number;
  enabled?: boolean;
  fetcher: (signal: AbortSignal) => Promise<T>;
  /**
   * When this changes, data/error reset immediately and a fresh fetch runs.
   * Use for route params (e.g. loop selector) so stale rows never flash.
   */
  key?: string | number;
};

export type UsePollingResult<T> = {
  data: T | null;
  error: string | null;
  loading: boolean;
  /** True when the last successful data was cleared due to a later error. */
  stale: boolean;
  refresh: () => void;
  /**
   * Guaranteed post-mutation refresh: aborts any in-flight poll, always runs a
   * fetch (even when the tab is hidden), and resolves when that fetch finishes
   * (success or error). Rejects only if the hook unmounts mid-flight.
   */
  forceRefresh: () => Promise<void>;
};

type RunOptions = {
  isInitial?: boolean;
  /** Abort in-flight work and always run (ignore visibility / overlap). */
  force?: boolean;
};

/**
 * Interval polling with:
 * - pause when document.visibilityState === 'hidden'
 * - no overlapping requests (unless forceRefresh)
 * - AbortController cancel on unmount / supersede
 * - immediate refresh when tab becomes visible
 * - clear data on error (lists never show failed-as-fresh)
 * - reset when `key` changes
 */
export function usePolling<T>({
  intervalMs,
  enabled = true,
  fetcher,
  key,
}: UsePollingOptions<T>): UsePollingResult<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [stale, setStale] = useState(false);

  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;

  const inFlightRef = useRef(false);
  const abortRef = useRef<AbortController | null>(null);
  const mountedRef = useRef(true);
  const generationRef = useRef(0);

  const run = useCallback(async (opts: RunOptions = {}): Promise<void> => {
    const isInitial = opts.isInitial === true;
    const force = opts.force === true;

    if (!force) {
      if (inFlightRef.current) {
        return;
      }
      if (
        typeof document !== "undefined" &&
        document.visibilityState === "hidden"
      ) {
        return;
      }
    } else if (inFlightRef.current) {
      // Supersede in-flight poll so post-mutation data is not dropped.
      abortRef.current?.abort();
      inFlightRef.current = false;
    }

    const generation = ++generationRef.current;
    inFlightRef.current = true;
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;

    if (isInitial) {
      setLoading(true);
    }

    try {
      const next = await fetcherRef.current(controller.signal);
      if (
        !mountedRef.current ||
        controller.signal.aborted ||
        generation !== generationRef.current
      ) {
        return;
      }
      setData(next);
      setError(null);
      setStale(false);
    } catch (err) {
      if (
        !mountedRef.current ||
        controller.signal.aborted ||
        generation !== generationRef.current
      ) {
        return;
      }
      // AbortError from supersede/unmount — ignore
      if (err instanceof DOMException && err.name === "AbortError") {
        return;
      }
      if (err instanceof Error && err.name === "AbortError") {
        return;
      }
      // Clear data on error so UI never treats failed health/lists as fresh/green.
      setData(null);
      setStale(true);
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      if (abortRef.current === controller) {
        inFlightRef.current = false;
      }
      if (
        mountedRef.current &&
        !controller.signal.aborted &&
        generation === generationRef.current
      ) {
        setLoading(false);
      }
    }
  }, []);

  const refresh = useCallback(() => {
    void run({ isInitial: false });
  }, [run]);

  const forceRefresh = useCallback(async () => {
    await run({ isInitial: false, force: true });
  }, [run]);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      generationRef.current += 1;
      abortRef.current?.abort();
      abortRef.current = null;
      inFlightRef.current = false;
    };
  }, []);

  // Selector / key change: drop stale payload immediately.
  useEffect(() => {
    setData(null);
    setError(null);
    setStale(false);
    setLoading(enabled);
    generationRef.current += 1;
    abortRef.current?.abort();
    abortRef.current = null;
    inFlightRef.current = false;
  }, [key, enabled]);

  useEffect(() => {
    if (!enabled) {
      generationRef.current += 1;
      abortRef.current?.abort();
      abortRef.current = null;
      inFlightRef.current = false;
      setLoading(false);
      return;
    }

    void run({ isInitial: true });

    const id = window.setInterval(() => {
      void run({ isInitial: false });
    }, intervalMs);

    const onVisibility = () => {
      if (document.visibilityState === "visible") {
        void run({ isInitial: false });
      } else {
        // Pause: abort in-flight so we don't hold work while hidden
        generationRef.current += 1;
        abortRef.current?.abort();
        abortRef.current = null;
        inFlightRef.current = false;
      }
    };
    document.addEventListener("visibilitychange", onVisibility);

    return () => {
      window.clearInterval(id);
      document.removeEventListener("visibilitychange", onVisibility);
      generationRef.current += 1;
      abortRef.current?.abort();
      abortRef.current = null;
      inFlightRef.current = false;
    };
  }, [enabled, intervalMs, run, key]);

  return { data, error, loading, stale, refresh, forceRefresh };
}
