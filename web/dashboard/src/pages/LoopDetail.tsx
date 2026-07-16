import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Link, useParams } from "react-router-dom";
import { LoopActionBar } from "@/components/LoopActionBar";
import { PanelError } from "@/components/PanelError";
import { StatusChip } from "@/components/StatusChip";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  fetchLoop,
  openLoopLogsStream,
  type Loop,
  type LoopLogsChunk,
  type LoopLogsSnapshot,
} from "@/lib/api";
import { useDashboardData } from "@/lib/DashboardDataContext";
import { formatTs } from "@/lib/format";
import { capLogChunk, capLogSeed, trimLogBuffer } from "@/lib/logBuffer";
import {
  type LogsStreamPhase,
  formatLiveStderrChunk,
  needsSeparateStderrFollow,
  nextReconnectDelayMs,
  resolveLogsStreamStatus,
  stderrGapFromSecondarySnapshot,
} from "@/lib/logsStream";
import { consumeSSE } from "@/lib/sse";
import { usePolling } from "@/lib/usePolling";

function seedFromSnapshot(snap: LoopLogsSnapshot): string {
  const agent = snap.agent;
  if (!agent) {
    return "(no agent output yet)\n";
  }
  // Cap string log fields after parse (not the raw SSE JSON envelope).
  const stdout = agent.stdout ? capLogSeed(agent.stdout) : "";
  const stderr = agent.stderr ? capLogSeed(agent.stderr) : "";
  const parts: string[] = [];
  if (stdout) parts.push(stdout);
  if (stderr) {
    if (parts.length && !parts[parts.length - 1].endsWith("\n")) {
      parts.push("\n");
    }
    parts.push("--- stderr ---\n");
    parts.push(stderr);
  }
  if (parts.length === 0) {
    return "(empty snapshot)\n";
  }
  return parts.join("");
}

function Kv({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="grid grid-cols-[110px_1fr] gap-2 py-0.5 text-[12px]">
      <dt className="text-[var(--text-muted)]">{label}</dt>
      <dd className="m-0 break-all mono">{value}</dd>
    </div>
  );
}

function LogsPane({ selector }: { selector: string }) {
  const [text, setText] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [phase, setPhase] = useState<LogsStreamPhase>("idle");
  const [autoScroll, setAutoScroll] = useState(true);
  const [ended, setEnded] = useState(false);
  const preRef = useRef<HTMLPreElement | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const autoScrollRef = useRef(autoScroll);
  autoScrollRef.current = autoScroll;
  const explicitEndRef = useRef(false);
  const reconnectAttemptRef = useRef(0);
  const reconnectTimerRef = useRef<number | null>(null);
  const generationRef = useRef(0);

  const scrollToBottom = useCallback(() => {
    if (!autoScrollRef.current) return;
    const el = preRef.current;
    if (!el) return;
    requestAnimationFrame(() => {
      el.scrollTop = el.scrollHeight;
    });
  }, []);

  const replaceText = useCallback(
    (next: string) => {
      setText(trimLogBuffer(next));
      scrollToBottom();
    },
    [scrollToBottom],
  );

  const appendText = useCallback(
    (chunk: string) => {
      const capped = capLogChunk(chunk);
      setText((prev) => trimLogBuffer(prev + capped));
      scrollToBottom();
    },
    [scrollToBottom],
  );

  const clearReconnectTimer = useCallback(() => {
    if (reconnectTimerRef.current != null) {
      window.clearTimeout(reconnectTimerRef.current);
      reconnectTimerRef.current = null;
    }
  }, []);

  const stopStream = useCallback(() => {
    clearReconnectTimer();
    abortRef.current?.abort();
    abortRef.current = null;
    setPhase("idle");
  }, [clearReconnectTimer]);

  const startStream = useCallback(
    (opts?: { isReconnect?: boolean }) => {
      clearReconnectTimer();
      abortRef.current?.abort();
      abortRef.current = null;

      if (!opts?.isReconnect) {
        reconnectAttemptRef.current = 0;
      }

      explicitEndRef.current = false;
      setError(null);
      setEnded(false);
      // Connecting until first successful snapshot/chunk on this connection.
      // Retained prior log text must not imply "live".
      setPhase("connecting");

      const generation = ++generationRef.current;
      const controller = new AbortController();
      abortRef.current = controller;

      const scheduleReconnect = () => {
        if (explicitEndRef.current) return;
        if (
          typeof document !== "undefined" &&
          document.visibilityState === "hidden"
        ) {
          return;
        }
        if (generation !== generationRef.current) return;

        const attempt = reconnectAttemptRef.current;
        const delay = nextReconnectDelayMs(attempt);
        reconnectAttemptRef.current = attempt + 1;
        clearReconnectTimer();
        setPhase("connecting");
        reconnectTimerRef.current = window.setTimeout(() => {
          if (generation !== generationRef.current) return;
          if (document.visibilityState === "hidden") return;
          startStream({ isReconnect: true });
        }, delay);
      };

      const startStderrFollow = (snap: LoopLogsSnapshot) => {
        // Always open stderr=1. Default follow may track stderr while stdout is
        // blank then switch to stdout, dropping later stderr without this stream.
        if (!needsSeparateStderrFollow(snap.agent)) return;

        const primaryStderr = snap.agent?.stderr ?? "";
        let sectionHeaderPresent = Boolean(primaryStderr.trim());

        void (async () => {
          try {
            const response = await openLoopLogsStream(
              selector,
              controller.signal,
              { stderr: true },
            );
            await consumeSSE(
              response,
              (event, rawData) => {
                if (generation !== generationRef.current) return;
                // Secondary snapshot is the server baseline for later chunks.
                // Apply any stderr written after the primary seed and before
                // this connection's snapshot; pure chunks alone miss that gap.
                if (event === "snapshot") {
                  try {
                    const secondary = JSON.parse(rawData) as LoopLogsSnapshot;
                    const gap = stderrGapFromSecondarySnapshot(
                      primaryStderr,
                      secondary.agent?.stderr ?? "",
                    );
                    if (gap) {
                      appendText(
                        formatLiveStderrChunk(gap, sectionHeaderPresent),
                      );
                      sectionHeaderPresent = true;
                      setPhase("live");
                    } else if (secondary.agent?.stderr?.trim()) {
                      sectionHeaderPresent = true;
                    }
                  } catch {
                    // Keep primary stream alive; soft-fail malformed stderr only.
                  }
                  return;
                }
                if (event !== "chunk") return;
                try {
                  const chunk = JSON.parse(rawData) as LoopLogsChunk;
                  if (typeof chunk.content === "string" && chunk.content) {
                    appendText(
                      formatLiveStderrChunk(
                        chunk.content,
                        sectionHeaderPresent,
                      ),
                    );
                    sectionHeaderPresent = true;
                    setPhase("live");
                  }
                } catch {
                  // Keep primary stream alive; soft-fail malformed stderr only.
                }
              },
              controller.signal,
            );
          } catch (err) {
            if (
              controller.signal.aborted ||
              generation !== generationRef.current
            ) {
              return;
            }
            if (err instanceof Error && err.name === "AbortError") return;
            if (err instanceof DOMException && err.name === "AbortError") return;
            // Soft-fail: stdout stream remains authoritative for phase/errors.
          }
        })();
      };

      void (async () => {
        try {
          const response = await openLoopLogsStream(selector, controller.signal);
          await consumeSSE(
            response,
            (event, rawData) => {
              if (event === "snapshot") {
                try {
                  const snap = JSON.parse(rawData) as LoopLogsSnapshot;
                  replaceText(seedFromSnapshot(snap));
                  setPhase("live");
                  startStderrFollow(snap);
                } catch {
                  setError("Malformed snapshot event (invalid JSON)");
                  setPhase("idle");
                }
                return;
              }
              if (event === "chunk") {
                try {
                  const chunk = JSON.parse(rawData) as LoopLogsChunk;
                  if (typeof chunk.content === "string" && chunk.content) {
                    appendText(chunk.content);
                  }
                  setPhase("live");
                } catch {
                  setError("Malformed chunk event (invalid JSON)");
                  setPhase("idle");
                }
                return;
              }
              if (event === "end") {
                explicitEndRef.current = true;
                setEnded(true);
                setPhase("idle");
              }
            },
            controller.signal,
          );
          if (
            controller.signal.aborted ||
            generation !== generationRef.current
          ) {
            return;
          }
          setPhase("idle");
          // Unexpected stream end (no explicit end event) → reconnect while visible.
          if (!explicitEndRef.current) {
            scheduleReconnect();
          }
        } catch (err) {
          if (
            controller.signal.aborted ||
            generation !== generationRef.current
          ) {
            return;
          }
          if (err instanceof Error && err.name === "AbortError") return;
          if (err instanceof DOMException && err.name === "AbortError") return;
          setError(err instanceof Error ? err.message : String(err));
          setPhase("idle");
          if (!explicitEndRef.current) {
            scheduleReconnect();
          }
        }
      })();
    },
    [appendText, clearReconnectTimer, replaceText, selector],
  );

  // Start / stop based on visibility; cancel reconnects when hidden/unmount.
  useEffect(() => {
    const onVisibility = () => {
      if (document.visibilityState === "hidden") {
        stopStream();
      } else {
        startStream();
      }
    };

    if (document.visibilityState === "visible") {
      startStream();
    }

    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      document.removeEventListener("visibilitychange", onVisibility);
      stopStream();
    };
  }, [startStream, stopStream]);

  const onClear = () => setText("");

  const onCopy = async () => {
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      const ta = document.createElement("textarea");
      ta.value = text;
      document.body.appendChild(ta);
      ta.select();
      document.execCommand("copy");
      document.body.removeChild(ta);
    }
  };

  const status = resolveLogsStreamStatus({ phase, ended, error });

  return (
    <Card
      title="Logs"
      actions={
        <div className="flex flex-wrap items-center gap-1">
          <span className="mr-1 text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
            {status}
          </span>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setAutoScroll((v) => !v)}
          >
            {autoScroll ? "Pause scroll" : "Resume scroll"}
          </Button>
          <Button variant="ghost" size="sm" onClick={onClear}>
            Clear
          </Button>
          <Button variant="ghost" size="sm" onClick={() => void onCopy()}>
            Copy
          </Button>
          <Button variant="ghost" size="sm" onClick={() => startStream()}>
            Reconnect
          </Button>
        </div>
      }
    >
      {error ? (
        <div className="mb-2">
          <PanelError message={error} onRetry={() => startStream()} />
        </div>
      ) : null}
      <pre
        ref={preRef}
        className="mono m-0 max-h-[min(60vh,520px)] overflow-auto whitespace-pre-wrap break-words rounded border border-[var(--border)] bg-[var(--bg)] p-2 text-[11px] leading-snug text-[var(--text)]"
      >
        {text || (phase === "connecting" ? "Connecting…" : "—")}
      </pre>
    </Card>
  );
}

export function LoopDetailPage() {
  const { selector = "" } = useParams<{ selector: string }>();
  const { activeRuns } = useDashboardData();

  const fetcher = useCallback(
    (signal: AbortSignal) => fetchLoop(selector, signal),
    [selector],
  );
  const { data, error, loading, refresh, forceRefresh } = usePolling<Loop>({
    intervalMs: 3000,
    enabled: Boolean(selector),
    fetcher,
    key: selector,
  });

  const activeRunItems = activeRuns.data?.items;
  const forceRefreshActiveRuns = activeRuns.forceRefresh;

  const hasActiveRun = useMemo(() => {
    if (!data) return false;
    const items = activeRunItems ?? [];
    return items.some(
      (r) => r.loopId === data.id || r.seq === data.seq,
    );
  }, [activeRunItems, data]);

  const onMutated = useCallback(async () => {
    await Promise.all([forceRefresh(), forceRefreshActiveRuns()]);
  }, [forceRefresh, forceRefreshActiveRuns]);

  if (!selector) {
    return <PanelError message="Missing loop selector" />;
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <Link
            to="/loops"
            className="text-[12px] text-[var(--text-muted)] hover:text-[var(--text)]"
          >
            ← Loops
          </Link>
          <h1 className="m-0 text-[15px] font-semibold">
            Loop{" "}
            <span className="mono">{data ? `#${data.seq}` : selector}</span>
          </h1>
          {data ? <StatusChip status={data.status} /> : null}
        </div>
        <Button variant="ghost" size="sm" onClick={refresh}>
          Refresh
        </Button>
      </div>

      {data ? (
        <Card title="Actions">
          <LoopActionBar
            selector={String(data.seq)}
            status={data.status}
            hasActiveRun={hasActiveRun}
            onMutated={onMutated}
            mode="full"
          />
        </Card>
      ) : null}

      <Card title="Metadata">
        {error && !data ? (
          <PanelError message={error} onRetry={refresh} />
        ) : loading && !data ? (
          <p className="m-0 text-[12px] text-[var(--text-muted)]">
            Loading loop…
          </p>
        ) : data ? (
          <>
            {error ? (
              <div className="mb-2">
                <PanelError message={error} onRetry={refresh} />
              </div>
            ) : null}
            <dl className="m-0 columns-1 gap-x-6 md:columns-2">
              <Kv label="Seq" value={data.seq} />
              <Kv label="ID" value={data.id} />
              <Kv label="Type" value={data.type} />
              <Kv label="Status" value={<StatusChip status={data.status} />} />
              <Kv label="Project" value={data.projectId} />
              <Kv label="Target type" value={data.targetType} />
              <Kv label="Target ID" value={data.targetId ?? "—"} />
              <Kv label="Repo" value={data.repo ?? "—"} />
              <Kv
                label="PR"
                value={data.prNumber != null ? String(data.prNumber) : "—"}
              />
              <Kv label="Last run" value={formatTs(data.lastRunAt)} />
              <Kv label="Next run" value={formatTs(data.nextRunAt)} />
              <Kv label="Created" value={formatTs(data.createdAt)} />
              <Kv label="Updated" value={formatTs(data.updatedAt)} />
            </dl>
          </>
        ) : (
          <p className="m-0 text-[12px] text-[var(--text-muted)]">No data</p>
        )}
      </Card>

      {/* Remount on selector change so log buffer/stream state never leaks. */}
      <LogsPane key={selector} selector={selector} />
    </div>
  );
}
