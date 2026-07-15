import { useCallback, useMemo } from "react";
import { Link } from "react-router-dom";
import { DataTable, type Column } from "@/components/DataTable";
import { PanelError } from "@/components/PanelError";
import { StatusChip } from "@/components/StatusChip";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { fetchLoops, type Loop } from "@/lib/api";
import { formatTs } from "@/lib/format";
import { useProjectFilter } from "@/lib/ProjectFilterContext";
import { usePolling } from "@/lib/usePolling";

function targetLabel(loop: Loop): string {
  if (loop.repo && loop.prNumber != null) {
    return `${loop.repo}#${loop.prNumber}`;
  }
  if (loop.repo) return loop.repo;
  if (loop.targetId) return loop.targetId;
  return loop.targetType || "—";
}

export function LoopsPage() {
  const { projectId } = useProjectFilter();
  const fetcher = useCallback((signal: AbortSignal) => fetchLoops(signal), []);
  const { data, error, loading, refresh } = usePolling({
    intervalMs: 5000,
    fetcher,
  });

  const rows = useMemo(() => {
    const items = data?.items ?? [];
    if (!projectId) return items;
    return items.filter((l) => l.projectId === projectId);
  }, [data, projectId]);

  const columns: Column<Loop>[] = useMemo(
    () => [
      {
        key: "seq",
        header: "Seq",
        cell: (l) => (
          <Link
            to={`/loops/${l.seq}`}
            className="mono text-[var(--accent)] underline-offset-2 hover:underline"
          >
            {l.seq}
          </Link>
        ),
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
        key: "target",
        header: "Target",
        cell: (l) => (
          <span className="mono" title={targetLabel(l)}>
            {targetLabel(l)}
          </span>
        ),
      },
      {
        key: "updatedAt",
        header: "Updated",
        cell: (l) => (
          <span className="mono text-[var(--text-muted)]">
            {formatTs(l.updatedAt)}
          </span>
        ),
      },
    ],
    [],
  );

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between gap-2">
        <h1 className="m-0 text-[15px] font-semibold">Loops</h1>
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
              empty="No loops"
            />
          </>
        )}
      </Card>
    </div>
  );
}
