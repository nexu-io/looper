import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useNavigate } from "react-router-dom";
import { Inbox, LayoutDashboard, RefreshCw } from "lucide-react";
import { AttemptsCell } from "@/components/AttemptsCell";
import { DataTable, type Column } from "@/components/DataTable";
import { LoopActionBar } from "@/components/LoopActionBar";
import { LoopTypeBadge } from "@/components/LoopTypeBadge";
import { PanelError } from "@/components/PanelError";
import { PullRequestLink } from "@/components/PullRequestLink";
import { StatusChip } from "@/components/StatusChip";
import { Card } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import {
  fetchStatus,
  type ActiveRun,
  type LoopRoleCounts,
  type Project,
  type StatusData,
} from "@/lib/api";
import { useDashboardData } from "@/lib/DashboardDataContext";
import {
  formatAge,
  formatAttempts,
  pullRequestUrl,
  truncateReason,
} from "@/lib/format";
import { useProjectFilter } from "@/lib/ProjectFilterContext";

const STATUS_SLOW_MS = 45_000;

function sumLoopCounts(loops: StatusData["loops"]): Record<string, number> {
  const totals: Record<string, number> = {
    queued: 0,
    running: 0,
    waiting: 0,
    paused: 0,
    failed: 0,
    terminated: 0,
    stopped: 0,
  };
  if (!loops) return totals;
  for (const role of Object.values(loops)) {
    const counts = role as LoopRoleCounts;
    for (const key of Object.keys(totals)) {
      const k = key as keyof LoopRoleCounts;
      totals[key] += counts[k] ?? 0;
    }
  }
  return totals;
}

function FullPageError({ message }: { message: string }) {
  return (
    <div className="mx-auto flex max-w-lg flex-col gap-3 py-10">
      <h1 className="m-0 text-[16px] font-semibold">Daemon unreachable</h1>
      <p className="m-0 text-[var(--text-muted)]">
        Could not reach <span className="mono">GET /api/v1/healthz</span>.
      </p>
      <pre className="m-0 overflow-auto rounded border border-[var(--border)] bg-[var(--bg-muted)] p-2 mono text-[12px] text-[var(--danger)]">
        {message}
      </pre>
      <div className="rounded border border-[var(--border)] bg-[var(--bg-elevated)] p-3 text-[12px]">
        <p className="m-0 mb-1 font-medium">Recovery</p>
        <ol className="m-0 list-decimal pl-4 text-[var(--text-muted)]">
          <li>
            Start the daemon:{" "}
            <code className="mono text-[var(--text)]">looper daemon start</code>
          </li>
          <li>
            Or open via CLI:{" "}
            <code className="mono text-[var(--text)]">looper dashboard</code>
          </li>
        </ol>
      </div>
    </div>
  );
}

function Kv({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="grid grid-cols-[120px_1fr] gap-2 py-0.5 text-[12px]">
      <dt className="text-[var(--text-muted)]">{label}</dt>
      <dd className="m-0 mono">{value}</dd>
    </div>
  );
}

function activeTargetLabel(run: ActiveRun): string {
  const t = run.target;
  if (t?.label) return t.label;
  if (t?.repo && t.prNumber != null) return `${t.repo}#${t.prNumber}`;
  if (t?.repo) return t.repo;
  return t?.type || "—";
}

function ActiveTargetCell({
  run,
  project,
}: {
  run: ActiveRun;
  project?: Project;
}) {
  const label = activeTargetLabel(run);
  const href = pullRequestUrl(run.target?.repo, run.target?.prNumber, {
    repoUrl: project?.repoUrl,
    provider: project?.provider,
  });
  if (!href) {
    return (
      <span className="mono" title={label}>
        {label}
      </span>
    );
  }
  return <PullRequestLink href={href}>{label}</PullRequestLink>;
}

function activeAgentLabel(run: ActiveRun): string {
  const agent = run.agent;
  if (!agent) return "—";
  const pid = agent.pid != null ? `pid ${agent.pid}` : "no pid";
  const vendor = agent.vendor || "agent";
  return `${vendor} · ${pid}`;
}

export function OverviewPage({
  onHealthChange,
}: {
  onHealthChange?: (healthy: boolean | null, version?: string) => void;
}) {
  const navigate = useNavigate();
  const { projectId } = useProjectFilter();
  const {
    health,
    healthy: sharedHealthy,
    activeRuns,
    projects,
  } = useDashboardData();
  const projectsById = useMemo(() => {
    const map = new Map<string, Project>();
    for (const p of projects.data?.items ?? []) {
      map.set(p.id, p);
    }
    return map;
  }, [projects.data]);

  const [status, setStatus] = useState<StatusData | null>(null);
  const [statusError, setStatusError] = useState<string | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const statusAbort = useRef<AbortController | null>(null);
  const statusInFlight = useRef(false);

  const loadStatus = useCallback(async () => {
    if (statusInFlight.current) return;
    if (
      typeof document !== "undefined" &&
      document.visibilityState === "hidden"
    ) {
      return;
    }
    statusInFlight.current = true;
    statusAbort.current?.abort();
    const controller = new AbortController();
    statusAbort.current = controller;
    setStatusLoading(true);
    try {
      const next = await fetchStatus(controller.signal);
      if (controller.signal.aborted) return;
      setStatus(next);
      setStatusError(null);
      if (next?.service?.version) {
        // Prefer shared healthz; never default missing healthy to true.
        const fromStatus =
          next.service.healthy === true
            ? true
            : next.service.healthy === false
              ? false
              : null;
        onHealthChange?.(
          sharedHealthy !== null ? sharedHealthy : fromStatus,
          next.service.version,
        );
      }
    } catch (err) {
      if (controller.signal.aborted) return;
      if (err instanceof Error && err.name === "AbortError") return;
      if (err instanceof DOMException && err.name === "AbortError") return;
      setStatusError(err instanceof Error ? err.message : String(err));
      // Clear status on error so UI does not present stale scheduler totals as fresh.
      setStatus(null);
    } finally {
      if (statusAbort.current === controller) {
        statusInFlight.current = false;
      }
      if (!controller.signal.aborted) {
        setStatusLoading(false);
      }
    }
  }, [onHealthChange, sharedHealthy]);

  // Status: on mount + slow poll + immediate refresh when tab becomes visible.
  useEffect(() => {
    void loadStatus();
    const id = window.setInterval(() => {
      void loadStatus();
    }, STATUS_SLOW_MS);

    const onVisibility = () => {
      if (document.visibilityState === "visible") {
        void loadStatus();
      } else {
        statusAbort.current?.abort();
        statusAbort.current = null;
        statusInFlight.current = false;
      }
    };
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      window.clearInterval(id);
      document.removeEventListener("visibilitychange", onVisibility);
      statusAbort.current?.abort();
    };
  }, [loadStatus]);

  useEffect(() => {
    onHealthChange?.(sharedHealthy, status?.service?.version);
  }, [sharedHealthy, onHealthChange, status?.service?.version]);

  const runningRows = useMemo(() => {
    const items = activeRuns.data?.items ?? [];
    if (!projectId) return items;
    return items.filter((r) => r.projectId === projectId);
  }, [activeRuns.data, projectId]);

  const onRunningMutated = useCallback(async () => {
    await activeRuns.forceRefresh();
  }, [activeRuns]);

  const onRunningRowClick = useCallback(
    (run: ActiveRun) => {
      navigate(`/loops/${run.seq}`);
    },
    [navigate],
  );

  const runningColumns: Column<ActiveRun>[] = useMemo(
    () => [
      {
        key: "seq",
        header: "Seq",
        width: "4.5rem",
        cell: (r) => (
          <span className="mono text-[var(--accent)]">{r.seq}</span>
        ),
      },
      {
        key: "type",
        header: "Type",
        width: "6.5rem",
        cell: (r) => <LoopTypeBadge type={r.type} size="sm" />,
      },
      {
        key: "projectId",
        header: "Project",
        width: "8rem",
        cell: (r) => (
          <span
            className="mono block min-w-0 truncate text-[var(--text-muted)]"
            title={r.projectId}
          >
            {r.projectId}
          </span>
        ),
      },
      {
        key: "status",
        header: "Status",
        width: "7.5rem",
        cell: (r) => (
          <StatusChip status={r.displayStatus || r.loopStatus || r.status} />
        ),
      },
      {
        key: "step",
        header: "Step",
        width: "8rem",
        cell: (r) => (
          <span
            className="mono block min-w-0 truncate text-[var(--text-muted)]"
            title={r.currentStep ?? undefined}
          >
            {r.currentStep ?? "—"}
          </span>
        ),
      },
      {
        key: "target",
        header: "Target",
        width: "14rem",
        cell: (r) => (
          <ActiveTargetCell run={r} project={projectsById.get(r.projectId)} />
        ),
      },
      {
        key: "agent",
        header: "Agent / PID",
        width: "9rem",
        cell: (r) => (
          <span
            className="mono block min-w-0 truncate text-[var(--text-muted)]"
            title={activeAgentLabel(r)}
          >
            {activeAgentLabel(r)}
          </span>
        ),
      },
      {
        key: "attempts",
        header: "Attempts",
        width: "5rem",
        cell: (r) => (
          <span title={formatAttempts(r.attempts, r.maxAttempts) ?? "—"}>
            <AttemptsCell attempts={r.attempts} maxAttempts={r.maxAttempts} />
          </span>
        ),
      },
      {
        key: "reason",
        header: "Reason",
        width: "12rem",
        cell: (r) => {
          const full = r.lastFailureReason?.trim() || null;
          const display = full ? truncateReason(full, 48) ?? "—" : "—";
          return (
            <span
              className="mono block min-w-0 truncate text-[var(--text-muted)]"
              title={full ?? undefined}
            >
              {display}
            </span>
          );
        },
      },
      {
        key: "age",
        header: "Age",
        width: "5.5rem",
        cell: (r) => (
          <span className="mono block min-w-0 truncate text-[var(--text-muted)]">
            {r.startedAt ? formatAge(r.startedAt) : "—"}
          </span>
        ),
      },
      {
        key: "actions",
        header: "Actions",
        width: "11rem",
        stopRowClick: true,
        cell: (r) => (
          <LoopActionBar
            selector={String(r.seq)}
            status={r.loopStatus || r.status}
            hasActiveRun
            onMutated={onRunningMutated}
            mode="compact"
          />
        ),
      },
    ],
    [onRunningMutated, projectsById],
  );

  if (health.loading && !health.data && !health.error) {
    return (
      <p className="m-0 text-[var(--text-muted)] text-[12px]">
        Loading overview…
      </p>
    );
  }

  if (health.error && !health.data) {
    return (
      <div>
        <FullPageError message={health.error} />
        <div className="mt-3 flex justify-center">
          <Button variant="ghost" onClick={health.refresh}>
            Retry
          </Button>
        </div>
      </div>
    );
  }

  const healthData = health.data;
  const loopTotals = sumLoopCounts(status?.loops);
  // Never default missing healthy to true / green.
  const serviceHealthy =
    status?.service?.healthy === true
      ? true
      : status?.service?.healthy === false
        ? false
        : sharedHealthy === true
          ? true
          : sharedHealthy === false
            ? false
            : undefined;
  const schedulerHealthy =
    status?.scheduler?.healthy === true
      ? true
      : status?.scheduler?.healthy === false
        ? false
        : undefined;
  const storageHealthy = (() => {
    if (healthData?.storage?.ok === false || status?.storage?.healthy === false) {
      return false;
    }
    if (healthData?.storage?.ok === true || status?.storage?.healthy === true) {
      return true;
    }
    return undefined;
  })();

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between gap-2">
        <h1 className="m-0 inline-flex items-center gap-1.5 text-[15px] font-semibold">
          <LayoutDashboard
            size={15}
            className="shrink-0 text-[var(--text-muted)]"
            aria-hidden
          />
          Overview
        </h1>
        <Button
          variant="ghost"
          className="gap-1.5"
          onClick={() => {
            health.refresh();
            void loadStatus();
          }}
        >
          <RefreshCw size={13} className="shrink-0" aria-hidden />
          Refresh
        </Button>
      </div>

      <div className="grid gap-3 md:grid-cols-2">
        <Card title="Service">
          <dl className="m-0">
            <Kv
              label="Healthy"
              value={
                serviceHealthy === undefined ? (
                  "—"
                ) : (
                  <span
                    style={{
                      color: serviceHealthy ? "var(--ok)" : "var(--danger)",
                    }}
                  >
                    {serviceHealthy ? "yes" : "no"}
                  </span>
                )
              }
            />
            <Kv label="Version" value={status?.service?.version ?? "—"} />
            <Kv label="Mode" value={status?.service?.daemonMode ?? "—"} />
            <Kv
              label="Started"
              value={status?.service?.startedAt ?? healthData?.startedAt ?? "—"}
            />
            <Kv label="Agent" value={status?.agent?.vendor ?? "—"} />
          </dl>
        </Card>

        <Card title="Scheduler">
          {statusError && !status ? (
            <div className="flex flex-col gap-2">
              <p className="m-0 text-[12px] text-[var(--danger)]">
                Failed to load status: {statusError}
              </p>
              <Button variant="ghost" size="sm" onClick={() => void loadStatus()}>
                Retry
              </Button>
            </div>
          ) : statusLoading && !status ? (
            <p className="m-0 text-[12px] text-[var(--text-muted)]">
              Loading status…
            </p>
          ) : (
            <dl className="m-0">
              {statusError ? (
                <div className="mb-2">
                  <p className="m-0 text-[12px] text-[var(--danger)]">
                    Status refresh failed: {statusError}
                  </p>
                </div>
              ) : null}
              <Kv
                label="Healthy"
                value={
                  schedulerHealthy === undefined ? (
                    "—"
                  ) : (
                    <span
                      style={{
                        color: schedulerHealthy ? "var(--ok)" : "var(--danger)",
                      }}
                    >
                      {schedulerHealthy ? "yes" : "no"}
                    </span>
                  )
                }
              />
              <Kv
                label="Active runs"
                value={status?.scheduler?.activeRuns ?? "—"}
              />
              <Kv label="Queued" value={status?.scheduler?.queuedItems ?? "—"} />
              <Kv
                label="Running"
                value={status?.scheduler?.runningItems ?? "—"}
              />
              <Kv label="Failed" value={status?.scheduler?.failedItems ?? "—"} />
              <Kv
                label="Total runs"
                value={status?.scheduler?.totalRuns ?? "—"}
              />
            </dl>
          )}
        </Card>

        <Card title="Loops">
          {statusError && !status ? (
            <p className="m-0 text-[12px] text-[var(--danger)]">
              Unavailable (status failed)
            </p>
          ) : (
            <dl className="m-0">
              {Object.entries(loopTotals).map(([key, value]) => (
                <Kv key={key} label={key} value={value} />
              ))}
            </dl>
          )}
        </Card>

        <Card title="Storage">
          <dl className="m-0">
            <Kv
              label="Healthy"
              value={
                storageHealthy === undefined ? (
                  "—"
                ) : (
                  <span
                    style={{
                      color: storageHealthy ? "var(--ok)" : "var(--danger)",
                    }}
                  >
                    {storageHealthy ? "yes" : "no"}
                  </span>
                )
              }
            />
            <Kv
              label="Mode"
              value={status?.storage?.mode ?? healthData?.storage?.mode ?? "—"}
            />
            <Kv
              label="DB"
              value={
                status?.storage?.dbPath ?? healthData?.storage?.dbPath ?? "—"
              }
            />
          </dl>
        </Card>
      </div>

      <Card
        title={
          projectId
            ? `Running loops · project ${projectId}`
            : "Running loops"
        }
      >
        {activeRuns.error && !activeRuns.data ? (
          <PanelError
            message={activeRuns.error}
            onRetry={activeRuns.refresh}
          />
        ) : activeRuns.loading && !activeRuns.data ? (
          <p className="m-0 text-[12px] text-[var(--text-muted)]">
            Loading running loops…
          </p>
        ) : (
          <>
            {activeRuns.error ? (
              <div className="mb-2">
                <PanelError
                  message={activeRuns.error}
                  onRetry={activeRuns.refresh}
                />
              </div>
            ) : null}
            <DataTable
              columns={runningColumns}
              rows={runningRows}
              rowKey={(r) => r.loopId || String(r.seq)}
              empty={
                <span className="inline-flex items-center gap-1.5 text-[var(--text-muted)]">
                  <Inbox size={14} className="shrink-0" aria-hidden />
                  {projectId
                    ? "No running loops for this project"
                    : "No running loops"}
                </span>
              }
              onRowClick={onRunningRowClick}
            />
          </>
        )}
      </Card>
    </div>
  );
}
