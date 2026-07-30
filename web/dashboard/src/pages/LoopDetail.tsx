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
import { LoopTypeBadge } from "@/components/LoopTypeBadge";
import { PanelError } from "@/components/PanelError";
import { PullRequestLink } from "@/components/PullRequestLink";
import { RecoveryCard } from "@/components/RecoveryCard";
import { StatusChip } from "@/components/StatusChip";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  fetchLoop,
  openLoopLogsStream,
  respondLoop,
  type ActiveRun,
  type Loop,
  type LoopLogsChunk,
  type LoopLogsSnapshot,
  type Project,
} from "@/lib/api";
import { useDashboardData } from "@/lib/DashboardDataContext";
import {
  formatAttempts,
  formatTs,
  issueUrl,
  parseIssueTargetId,
  pullRequestUrl,
  repositoryUrl,
} from "@/lib/format";
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

type ForgeLinkPalette = "accent" | "warn";

const FORGE_LINK_CLASSES: Record<ForgeLinkPalette, string> = {
  accent:
    "text-[var(--accent)] focus-visible:outline-[var(--accent)] [--forge-border:color-mix(in_srgb,var(--accent)_40%,transparent)] [--forge-border-hover:var(--accent)]",
  warn: "text-[var(--warn)] focus-visible:outline-[var(--warn)] [--forge-border:color-mix(in_srgb,var(--warn)_45%,transparent)] [--forge-border-hover:var(--warn)]",
};

function ForgeLink({
  href,
  label,
  children,
  palette = "accent",
}: {
  href: string;
  label: string;
  children: ReactNode;
  palette?: ForgeLinkPalette;
}) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      title={href}
      className={`group inline-flex max-w-full items-baseline gap-1 rounded-[2px] focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 ${FORGE_LINK_CLASSES[palette]}`}
    >
      <span className="text-[10px] font-semibold uppercase tracking-wider opacity-70 group-hover:opacity-100">
        {label}
      </span>
      <span className="mono truncate border-b border-[var(--forge-border)] text-[12px] transition-[border-color] group-hover:border-[var(--forge-border-hover)] group-focus-visible:border-[var(--forge-border-hover)]">
        {children}
      </span>
      <span
        aria-hidden="true"
        className="shrink-0 text-[0.85em] opacity-60 transition-[opacity,transform] group-hover:-translate-y-px group-hover:opacity-100"
      >
        ↗
      </span>
    </a>
  );
}

function TargetLinks({
  loop,
  repoUrl,
  provider,
}: {
  loop: Loop;
  repoUrl?: string | null;
  provider?: string | null;
}) {
  const prHref = pullRequestUrl(loop.repo, loop.prNumber, { repoUrl, provider });
  const prLabel =
    loop.repo && loop.prNumber != null
      ? `${loop.repo}#${loop.prNumber}`
      : prHref
      ? `#${loop.prNumber}`
      : null;

  // Worker loops targeting an issue store targetId as `issue:{repo}:{n}`.
  const isWorker = (loop.type ?? "").toLowerCase() === "worker";
  const parsedIssue =
    isWorker && loop.targetType === "issue"
      ? parseIssueTargetId(loop.targetId)
      : null;
  const issueHref = parsedIssue
    ? issueUrl(parsedIssue.repo, parsedIssue.issueNumber, { repoUrl })
    : null;
  const issueLabel = parsedIssue
    ? `${parsedIssue.repo}#${parsedIssue.issueNumber}`
    : null;

  if (!prHref && !issueHref) return null;

  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
      {prHref && prLabel ? (
        <ForgeLink href={prHref} label="PR" palette="accent">
          {prLabel}
        </ForgeLink>
      ) : null}
      {issueHref && issueLabel ? (
        <ForgeLink href={issueHref} label="Issue" palette="warn">
          {issueLabel}
        </ForgeLink>
      ) : null}
    </div>
  );
}

type HITLAsk = {
  question?: string;
  options?: string[];
  status?: string;
  askedAt?: string;
};

function readHITLAsk(loop: Loop): HITLAsk | null {
  try {
    const metadata = JSON.parse(loop.metadataJson ?? "{}") as {
      hitl?: HITLAsk;
    };
    return metadata.hitl ?? null;
  } catch {
    return null;
  }
}

function HITLDecisionCard({
  loop,
  onMutated,
}: {
  loop: Loop;
  onMutated: () => Promise<void>;
}) {
  const ask = readHITLAsk(loop);
  const [answer, setAnswer] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    setAnswer("");
  }, [loop.id, ask?.askedAt, ask?.question]);
  if (
    loop.status !== "awaiting_human" ||
    ask?.status !== "awaiting" ||
    !ask.question?.trim()
  ) {
    return null;
  }

  const submit = async (value: string) => {
    if (!value.trim() || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      await respondLoop(String(loop.seq), value.trim());
      setAnswer("");
      await onMutated();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to send response");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Card title="Human decision required">
      <p className="mt-0 whitespace-pre-wrap text-[13px]">{ask.question}</p>
      <div className="mb-3 flex flex-wrap gap-2">
        {ask.options
          ?.filter((option) => option !== "Provide different guidance")
          .map((option) => (
            <Button
              key={option}
              size="sm"
              disabled={submitting}
              onClick={() => void submit(option)}
            >
              {option}
            </Button>
          ))}
      </div>
      <form
        className="flex gap-2"
        onSubmit={(event) => {
          event.preventDefault();
          void submit(answer);
        }}
      >
        <input
          className="min-w-0 flex-1 rounded border border-[var(--border)] bg-transparent px-2 text-[12px]"
          value={answer}
          onChange={(event) => setAnswer(event.target.value)}
          placeholder="Or provide different guidance"
          disabled={submitting}
        />
        <Button size="sm" type="submit" disabled={submitting || !answer.trim()}>
          Respond
        </Button>
      </form>
      {error ? <p className="mb-0 text-[12px] text-red-500">{error}</p> : null}
    </Card>
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

function agentLabel(run: ActiveRun): string {
  const agent = run.agent;
  if (!agent) return "";
  const pid = agent.pid != null ? ` · pid ${agent.pid}` : "";
  return `${agent.vendor || "agent"}${pid}`;
}

function targetSummaryText(loop: Loop): string {
  if (loop.repo && loop.prNumber != null) {
    return `${loop.repo}#${loop.prNumber}`;
  }
  if (loop.targetType === "issue" && loop.targetId) {
    const parsed = parseIssueTargetId(loop.targetId);
    if (parsed) return `${parsed.repo}#${parsed.issueNumber}`;
  }
  if (loop.repo) return loop.repo;
  if (loop.targetId) return loop.targetId;
  return loop.targetType || "—";
}

function targetSummaryHref(
  loop: Loop,
  project?: Project,
): string | null {
  const repoUrl = project?.repoUrl;
  const provider = project?.provider;
  const prHref = pullRequestUrl(loop.repo, loop.prNumber, { repoUrl, provider });
  if (prHref) return prHref;
  if (loop.targetType === "issue" && loop.targetId) {
    const parsed = parseIssueTargetId(loop.targetId);
    if (parsed) {
      const href = issueUrl(parsed.repo, parsed.issueNumber, { repoUrl });
      if (href) return href;
    }
  }
  return repositoryUrl(loop.repo, repoUrl);
}

function TargetSummaryValue({
  loop,
  project,
}: {
  loop: Loop;
  project?: Project;
}) {
  const label = targetSummaryText(loop);
  const href = targetSummaryHref(loop, project);
  if (!href) return label;
  return <PullRequestLink href={href}>{label}</PullRequestLink>;
}

function SummaryCard({
  loop,
  project,
  activeRun,
  error,
  onRetry,
}: {
  loop: Loop;
  project?: Project;
  activeRun?: ActiveRun;
  error?: string | null;
  onRetry: () => void;
}) {
  const attempts = formatAttempts(loop.attempts, loop.maxAttempts);
  const failureKind = loop.lastFailureKind?.trim();
  const failureReason = loop.lastFailureReason?.trim();
  const resumePolicy = loop.resumePolicy?.trim();
  const currentStep = activeRun?.currentStep?.trim();
  const agent = activeRun ? agentLabel(activeRun) : "";

  return (
    <Card title="Summary">
      {error ? (
        <div className="mb-2">
          <PanelError message={error} onRetry={onRetry} />
        </div>
      ) : null}
      <dl className="m-0 columns-1 gap-x-6 md:columns-2">
        <Kv
          label="Project"
          value={
            project ? (
              <span>
                <span>{project.name}</span>
                <span className="ml-1 text-[var(--text-muted)]">
                  {loop.projectId}
                </span>
              </span>
            ) : (
              loop.projectId
            )
          }
        />
        <Kv
          label="Target"
          value={<TargetSummaryValue loop={loop} project={project} />}
        />
        <Kv label="Target type" value={loop.targetType} />
        <Kv label="Target ID" value={loop.targetId ?? "—"} />
        <Kv label="ID" value={loop.id} />
        <Kv label="Attempts" value={attempts ?? "—"} />
        {currentStep ? <Kv label="Current step" value={currentStep} /> : null}
        {agent ? <Kv label="Agent" value={agent} /> : null}
        <Kv label="Last run" value={formatTs(loop.lastRunAt)} />
        <Kv label="Next run" value={formatTs(loop.nextRunAt)} />
        <Kv label="Created" value={formatTs(loop.createdAt)} />
        <Kv label="Updated" value={formatTs(loop.updatedAt)} />
        {resumePolicy ? (
          <Kv label="Resume policy" value={resumePolicy} />
        ) : null}
        {failureKind ? <Kv label="Failure kind" value={failureKind} /> : null}
      </dl>
      {failureReason ? (
        <div className="mt-2">
          <div className="mb-1 text-[10px] font-semibold uppercase tracking-wider text-[var(--text-muted)]">
            Failure reason
          </div>
          <pre
            title={failureReason}
            className="mono m-0 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded border p-2 text-[11px] leading-snug"
            style={{
              borderColor:
                "color-mix(in srgb, var(--danger) 40%, var(--border))",
              background: "var(--bg)",
            }}
          >
            {failureReason}
          </pre>
        </div>
      ) : null}
    </Card>
  );
}

export function LoopDetailPage() {
  const { selector = "" } = useParams<{ selector: string }>();
  const { activeRuns, projects } = useDashboardData();

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

  const activeRun = useMemo(() => {
    if (!data) return undefined;
    const items = activeRunItems ?? [];
    return items.find((r) => r.loopId === data.id || r.seq === data.seq);
  }, [activeRunItems, data]);

  const hasActiveRun = Boolean(activeRun);

  const onMutated = useCallback(async () => {
    await Promise.all([forceRefresh(), forceRefreshActiveRuns()]);
  }, [forceRefresh, forceRefreshActiveRuns]);

  const project = useMemo(() => {
    if (!data) return undefined;
    return projects.data?.items.find((p) => p.id === data.projectId);
  }, [data, projects.data]);

  if (!selector) {
    return <PanelError message="Missing loop selector" />;
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <Link
            to="/loops"
            className="text-[12px] text-[var(--text-muted)] hover:text-[var(--text)]"
          >
            ← Loops
          </Link>
          <h1 className="m-0 flex items-center gap-2 text-[15px] font-semibold">
            {data ? (
              <>
                <LoopTypeBadge type={data.type} />
                <span className="mono">#{data.seq}</span>
              </>
            ) : (
              <>
                <span>Loop</span>
                <span className="mono">{selector}</span>
              </>
            )}
          </h1>
          {data ? (
            <>
              <StatusChip status={data.status} />
              {data.displayStatus &&
              data.displayStatus !== data.status ? (
                <StatusChip status={data.displayStatus} />
              ) : null}
            </>
          ) : null}
        </div>
        {data ? (
          <div className="ml-auto flex flex-wrap items-center justify-end gap-x-3 gap-y-1">
            <TargetLinks
              loop={data}
              repoUrl={project?.repoUrl}
              provider={project?.provider}
            />
          </div>
        ) : null}
      </div>

      {/* Recovery card is prominent and appears above generic actions when
          durable facts produce displayStatus=manual_intervention. */}
      {data ? (
        <RecoveryCard
          key={String(data.seq)}
          loop={data}
          selector={String(data.seq)}
          hasActiveRun={hasActiveRun}
          onMutated={onMutated}
        />
      ) : null}

      {data ? <HITLDecisionCard loop={data} onMutated={onMutated} /> : null}

      {data ? (
        <Card title="Actions">
          <LoopActionBar
            selector={String(data.seq)}
            status={data.status}
            displayStatus={data.displayStatus}
            hasActiveRun={hasActiveRun}
            onMutated={onMutated}
            mode="full"
          />
        </Card>
      ) : null}

      {data ? (
        <SummaryCard
          loop={data}
          project={project}
          activeRun={activeRun}
          error={error}
          onRetry={refresh}
        />
      ) : error ? (
        <Card title="Summary">
          <PanelError message={error} onRetry={refresh} />
        </Card>
      ) : loading ? (
        <Card title="Summary">
          <p className="m-0 text-[12px] text-[var(--text-muted)]">
            Loading loop…
          </p>
        </Card>
      ) : null}

      {/* Remount on selector change so log buffer/stream state never leaks. */}
      <div id="loop-logs">
        <LogsPane key={selector} selector={selector} />
      </div>
    </div>
  );
}
