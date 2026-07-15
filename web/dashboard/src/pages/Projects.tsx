import { useMemo } from "react";
import { useNavigate } from "react-router-dom";
import { DataTable, type Column } from "@/components/DataTable";
import { PanelError } from "@/components/PanelError";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import type { Project } from "@/lib/api";
import { useDashboardData } from "@/lib/DashboardDataContext";
import { formatTs } from "@/lib/format";
import { useProjectFilter } from "@/lib/ProjectFilterContext";

export function ProjectsPage() {
  const navigate = useNavigate();
  const { setProjectId } = useProjectFilter();
  const { projects } = useDashboardData();
  const { data, error, loading, refresh } = projects;

  const rows = data?.items ?? [];

  const columns: Column<Project>[] = useMemo(
    () => [
      {
        key: "name",
        header: "Name",
        cell: (p) => (
          <span className="font-medium">
            {p.name}
            {p.archived ? (
              <span className="ml-1 text-[var(--text-muted)]">(archived)</span>
            ) : null}
          </span>
        ),
      },
      {
        key: "id",
        header: "ID",
        cell: (p) => (
          <span className="mono text-[var(--text-muted)]">{p.id}</span>
        ),
      },
      {
        key: "provider",
        header: "Provider",
        cell: (p) => <span className="mono">{p.provider || "—"}</span>,
      },
      {
        key: "repo",
        header: "Repo",
        cell: (p) => (
          <span className="mono" title={p.repo ?? undefined}>
            {p.repo ?? "—"}
          </span>
        ),
      },
      {
        key: "repoPath",
        header: "Path",
        cell: (p) => (
          <span className="mono text-[var(--text-muted)]" title={p.repoPath}>
            {p.repoPath}
          </span>
        ),
      },
      {
        key: "baseBranch",
        header: "Base",
        cell: (p) => <span className="mono">{p.baseBranch}</span>,
      },
      {
        key: "updatedAt",
        header: "Updated",
        cell: (p) => (
          <span className="mono text-[var(--text-muted)]">
            {formatTs(p.updatedAt)}
          </span>
        ),
      },
    ],
    [],
  );

  const onRowClick = (p: Project) => {
    setProjectId(p.id);
    void navigate("/loops");
  };

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between gap-2">
        <div>
          <h1 className="m-0 text-[15px] font-semibold">Projects</h1>
          <p className="m-0 mt-0.5 text-[11px] text-[var(--text-muted)]">
            Click a row to set the project filter and open Loops.
          </p>
        </div>
        <Button variant="ghost" size="sm" onClick={refresh}>
          Refresh
        </Button>
      </div>

      <Card>
        {error && !data ? (
          <PanelError message={error} onRetry={refresh} />
        ) : loading && !data ? (
          <p className="m-0 text-[12px] text-[var(--text-muted)]">
            Loading projects…
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
              rowKey={(p) => p.id}
              empty="No projects"
              onRowClick={onRowClick}
            />
          </>
        )}
      </Card>
    </div>
  );
}
