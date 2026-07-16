import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { DataTable, type Column } from "@/components/DataTable";
import { LoopActionBar } from "@/components/LoopActionBar";
import { PanelError } from "@/components/PanelError";
import { StatusChip } from "@/components/StatusChip";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { fetchLoops, type ActiveRun, type Loop } from "@/lib/api";
import { useDashboardData } from "@/lib/DashboardDataContext";
import {
  formatAge,
  formatAttempts,
  formatTs,
  truncateReason,
} from "@/lib/format";
import { useProjectFilter } from "@/lib/ProjectFilterContext";
import { usePolling } from "@/lib/usePolling";

/** Default list filter: all loops (running live view lives on Overview). */
const DEFAULT_STATUS_FILTER = "all";
const DEFAULT_PAGE_SIZE = 25;
const PAGE_SIZE_OPTIONS = [10, 25, 50, 100] as const;

const STATUS_FILTER_OPTIONS = [
  { value: "all", label: "all" },
  { value: "running", label: "running" },
  { value: "queued", label: "queued" },
  { value: "paused", label: "paused" },
  { value: "waiting", label: "waiting" },
  { value: "failed", label: "failed" },
  { value: "stopped", label: "stopped" },
  { value: "interrupted", label: "interrupted" },
  { value: "awaiting_human", label: "awaiting_human" },
  { value: "human_takeover", label: "human_takeover" },
  { value: "completed", label: "completed" },
  { value: "terminated", label: "terminated" },
  { value: "idle", label: "idle" },
] as const;

type StatusFilter = (typeof STATUS_FILTER_OPTIONS)[number]["value"];

type LoopRow = Loop & {
  activeRun?: ActiveRun;
};

function targetLabel(loop: Loop): string {
  if (loop.repo && loop.prNumber != null) {
    return `${loop.repo}#${loop.prNumber}`;
  }
  if (loop.repo) return loop.repo;
  if (loop.targetId) return loop.targetId;
  return loop.targetType || "—";
}

function agentLabel(run: ActiveRun | undefined): string {
  if (!run?.agent) return "—";
  const agent = run.agent;
  const pid = agent.pid != null ? `pid ${agent.pid}` : "no pid";
  const vendor = agent.vendor || "agent";
  return `${vendor} · ${pid}`;
}

/** Prefer loop fields; fall back to joined active-run diagnostics when present. */
function rowAttempts(l: LoopRow): string {
  return (
    formatAttempts(l.attempts, l.maxAttempts) ??
    formatAttempts(l.activeRun?.attempts, l.activeRun?.maxAttempts) ??
    "—"
  );
}

function rowReason(l: LoopRow): { display: string; full: string | null } {
  const full =
    (l.lastFailureReason && l.lastFailureReason.trim()) ||
    (l.activeRun?.lastFailureReason &&
      l.activeRun.lastFailureReason.trim()) ||
    null;
  if (!full) return { display: "—", full: null };
  return { display: truncateReason(full, 48) ?? "—", full };
}

export function LoopsPage() {
  const navigate = useNavigate();
  const { projectId } = useProjectFilter();
  const { activeRuns } = useDashboardData();
  const [statusFilter, setStatusFilter] = useState<StatusFilter>(
    DEFAULT_STATUS_FILTER,
  );
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState<number>(DEFAULT_PAGE_SIZE);

  useEffect(() => {
    setPage(1);
  }, [projectId]);

  const fetcher = useCallback(
    (signal: AbortSignal) =>
      fetchLoops({
        signal,
        limit: pageSize,
        offset: (page - 1) * pageSize,
        status: statusFilter === "all" ? undefined : statusFilter,
        projectId: projectId || undefined,
      }),
    [page, pageSize, statusFilter, projectId],
  );
  // Poll faster when focused on running (former Running cadence).
  const intervalMs = statusFilter === "running" ? 2000 : 5000;
  const { data, error, loading, refresh, forceRefresh } = usePolling({
    intervalMs,
    fetcher,
    key: `loops:${statusFilter}:${projectId ?? ""}:${page}:${pageSize}`,
  });

  const activeByLoopId = useMemo(() => {
    const map = new Map<string, ActiveRun>();
    for (const run of activeRuns.data?.items ?? []) {
      map.set(run.loopId, run);
    }
    return map;
  }, [activeRuns.data]);

  const rows = useMemo(() => {
    return (data?.items ?? []).map((l) => ({
      ...l,
      activeRun: activeByLoopId.get(l.id),
    }));
  }, [data, activeByLoopId]);

  // Only trust total/pages when a response is present. usePolling clears data on
  // key (page) change; clamping against total=0 would snap page back to 1.
  const total = data?.total;
  const totalPages =
    total == null ? null : Math.max(1, Math.ceil(total / pageSize) || 1);

  useEffect(() => {
    if (totalPages != null && page > totalPages) {
      setPage(totalPages);
    }
  }, [page, totalPages]);

  const currentPage = page;
  const rangeStart =
    total == null || total === 0 ? 0 : (currentPage - 1) * pageSize + 1;
  const rangeEnd =
    total == null ? 0 : Math.min(currentPage * pageSize, total);

  const onMutated = useCallback(async () => {
    await Promise.all([forceRefresh(), activeRuns.forceRefresh()]);
  }, [forceRefresh, activeRuns]);

  const onRowClick = useCallback(
    (l: LoopRow) => {
      navigate(`/loops/${l.seq}`);
    },
    [navigate],
  );

  const columns: Column<LoopRow>[] = useMemo(
    () => [
      {
        key: "seq",
        header: "Seq",
        cell: (l) => <span className="mono text-[var(--accent)]">{l.seq}</span>,
      },
      {
        key: "type",
        header: "Type",
        cell: (l) => <span className="mono">{l.type}</span>,
      },
      {
        key: "projectId",
        header: "Project",
        cell: (l) => (
          <span className="mono text-[var(--text-muted)]" title={l.projectId}>
            {l.projectId}
          </span>
        ),
      },
      {
        key: "status",
        header: "Status",
        cell: (l) => <StatusChip status={l.status} />,
      },
      {
        key: "step",
        header: "Step",
        cell: (l) => (
          <span className="mono text-[var(--text-muted)]">
            {l.activeRun?.currentStep ?? "—"}
          </span>
        ),
      },
      {
        key: "target",
        header: "Target",
        cell: (l) => (
          <span className="mono" title={targetLabel(l)}>
            {targetLabel(l)}
          </span>
        ),
      },
      {
        key: "agent",
        header: "Agent / PID",
        cell: (l) => (
          <span className="mono text-[var(--text-muted)]">
            {agentLabel(l.activeRun)}
          </span>
        ),
      },
      {
        key: "attempts",
        header: "Attempts",
        cell: (l) => (
          <span className="mono text-[var(--text-muted)]">{rowAttempts(l)}</span>
        ),
      },
      {
        key: "reason",
        header: "Reason",
        cell: (l) => {
          const { display, full } = rowReason(l);
          return (
            <span
              className="mono text-[var(--text-muted)] max-w-[14rem] truncate inline-block align-bottom"
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
        cell: (l) => (
          <span className="mono text-[var(--text-muted)]">
            {l.activeRun?.startedAt
              ? formatAge(l.activeRun.startedAt)
              : formatTs(l.updatedAt)}
          </span>
        ),
      },
      {
        key: "actions",
        header: "Actions",
        stopRowClick: true,
        cell: (l) => (
          <LoopActionBar
            selector={String(l.seq)}
            status={l.status}
            hasActiveRun={Boolean(l.activeRun)}
            onMutated={onMutated}
            mode="compact"
          />
        ),
      },
    ],
    [onMutated],
  );

  const emptyLabel =
    statusFilter === "all"
      ? "No loops"
      : `No loops with status=${statusFilter}`;

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h1 className="m-0 text-[15px] font-semibold">Loops</h1>
        <div className="flex flex-wrap items-center gap-2 text-[11px] text-[var(--text-muted)]">
          <label className="flex items-center gap-1.5">
            <span className="uppercase tracking-wide">Status</span>
            <select
              className="rounded border border-[var(--border)] bg-[var(--bg)] px-1.5 py-0.5 text-[12px] text-[var(--text)] mono"
              value={statusFilter}
              onChange={(e) => {
                setStatusFilter(e.target.value as StatusFilter);
                setPage(1);
              }}
            >
              {STATUS_FILTER_OPTIONS.map((opt) => (
                <option key={opt.value} value={opt.value}>
                  {opt.label}
                </option>
              ))}
            </select>
          </label>
          {projectId ? (
            <span className="mono">project: {projectId}</span>
          ) : null}
          <Button variant="ghost" size="sm" onClick={refresh}>
            Refresh
          </Button>
        </div>
      </div>

      <Card>
        {error && !data ? (
          <PanelError message={error} onRetry={refresh} />
        ) : loading && !data ? (
          <p className="m-0 text-[12px] text-[var(--text-muted)]">
            Loading loops…
          </p>
        ) : (
          <>
            {error ? (
              <div className="mb-2">
                <PanelError message={error} onRetry={refresh} />
              </div>
            ) : null}
            <DataTable
              columns={columns}
              rows={rows}
              rowKey={(l) => l.id}
              empty={emptyLabel}
              onRowClick={onRowClick}
            />
            {total != null && total > 0 && totalPages != null ? (
              <div className="mt-2 flex flex-wrap items-center justify-between gap-2 border-t border-[var(--border)] pt-2 text-[11px] text-[var(--text-muted)]">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="mono">
                    {rangeStart}–{rangeEnd} of {total}
                  </span>
                  <label className="flex items-center gap-1">
                    <span className="uppercase tracking-wide">Per page</span>
                    <select
                      className="rounded border border-[var(--border)] bg-[var(--bg)] px-1.5 py-0.5 text-[12px] text-[var(--text)] mono"
                      value={pageSize}
                      onChange={(e) => {
                        setPageSize(Number(e.target.value) || DEFAULT_PAGE_SIZE);
                        setPage(1);
                      }}
                    >
                      {PAGE_SIZE_OPTIONS.map((n) => (
                        <option key={n} value={n}>
                          {n}
                        </option>
                      ))}
                    </select>
                  </label>
                </div>
                <div className="flex items-center gap-1">
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={currentPage <= 1}
                    onClick={() => setPage(1)}
                    aria-label="First page"
                  >
                    «
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={currentPage <= 1}
                    onClick={() => setPage(Math.max(1, currentPage - 1))}
                    aria-label="Previous page"
                  >
                    ‹
                  </Button>
                  <span className="mono px-1">
                    {currentPage} / {totalPages}
                  </span>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={currentPage >= totalPages}
                    onClick={() =>
                      setPage(Math.min(totalPages, currentPage + 1))
                    }
                    aria-label="Next page"
                  >
                    ›
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={currentPage >= totalPages}
                    onClick={() => setPage(totalPages)}
                    aria-label="Last page"
                  >
                    »
                  </Button>
                </div>
              </div>
            ) : null}
          </>
        )}
      </Card>
    </div>
  );
}
