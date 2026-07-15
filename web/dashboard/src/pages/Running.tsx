import { useCallback, useMemo } from "react";
import { Link } from "react-router-dom";
import { DataTable, type Column } from "@/components/DataTable";
import { LoopActionBar } from "@/components/LoopActionBar";
import { PanelError } from "@/components/PanelError";
import { StatusChip } from "@/components/StatusChip";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import type { ActiveRun } from "@/lib/api";
import { useDashboardData } from "@/lib/DashboardDataContext";
import { formatAge } from "@/lib/format";
import { useProjectFilter } from "@/lib/ProjectFilterContext";

function agentLabel(run: ActiveRun): string {
  const agent = run.agent;
  if (!agent) return "—";
  const pid = agent.pid != null ? `pid ${agent.pid}` : "no pid";
  const vendor = agent.vendor || "agent";
  return `${vendor} · ${pid}`;
}

export function RunningPage() {
  const { projectId } = useProjectFilter();
  const { activeRuns } = useDashboardData();
  const { data, error, loading, refresh, forceRefresh } = activeRuns;

  const rows = useMemo(() => {
    const items = data?.items ?? [];
    if (!projectId) return items;
    return items.filter((r) => r.projectId === projectId);
  }, [data, projectId]);

  const onMutated = useCallback(async () => {
    await forceRefresh();
  }, [forceRefresh]);

  const columns: Column<ActiveRun>[] = useMemo(
    () => [
      {
        key: "type",
        header: "Type",
        cell: (r) => <span className="mono">{r.type}</span>,
      },
      {
        key: "target",
        header: "Target",
        cell: (r) => (
          <span className="mono" title={r.target.label}>
            {r.target.label || "—"}
          </span>
        ),
      },
      {
        key: "seq",
        header: "Seq",
        cell: (r) => (
          <Link
            to={`/loops/${r.seq}`}
            className="mono text-[var(--accent)] underline-offset-2 hover:underline"
          >
            {r.seq}
          </Link>
        ),
      },
      {
        key: "step",
        header: "Step",
        cell: (r) => (
          <span className="mono text-[var(--text-muted)]">
            {r.currentStep ?? "—"}
          </span>
        ),
      },
      {
        key: "status",
        header: "Status",
        cell: (r) => <StatusChip status={r.displayStatus || r.status} />,
      },
      {
        key: "agent",
        header: "Agent / PID",
        cell: (r) => (
          <span className="mono text-[var(--text-muted)]">{agentLabel(r)}</span>
        ),
      },
      {
        key: "age",
        header: "Age",
        cell: (r) => (
          <span className="mono text-[var(--text-muted)]">
            {formatAge(r.startedAt)}
          </span>
        ),
      },
      {
        key: "actions",
        header: "Actions",
        cell: (r) => (
          <LoopActionBar
            selector={String(r.seq)}
            status={r.loopStatus || r.status}
            hasActiveRun
            onMutated={onMutated}
            mode="compact"
          />
        ),
      },
    ],
    [onMutated],
  );

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between gap-2">
        <h1 className="m-0 text-[15px] font-semibold">Running</h1>
        <div className="flex items-center gap-2 text-[11px] text-[var(--text-muted)]">
          {projectId ? (
            <span className="mono">filter: {projectId}</span>
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
            Loading active runs…
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
              rowKey={(r) => `${r.loopId}:${r.runId ?? "none"}:${r.seq}`}
              empty="No active runs"
            />
          </>
        )}
      </Card>
    </div>
  );
}
