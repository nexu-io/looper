import { useMemo } from "react";
import { useNavigate } from "react-router-dom";
import { FolderGit2, Inbox, RefreshCw } from "lucide-react";
import { DataTable, type Column } from "@/components/DataTable";
import { PanelError } from "@/components/PanelError";
import { PullRequestLink } from "@/components/PullRequestLink";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import type { Project } from "@/lib/api";
import { useDashboardData } from "@/lib/DashboardDataContext";
import { formatTs, repositoryUrl } from "@/lib/format";
import { useProjectFilter } from "@/lib/ProjectFilterContext";

function RepoCell({ project }: { project: Project }) {
  const label = project.repo?.trim() || "—";
  const href = repositoryUrl(project.repo, project.repoUrl);
  if (!href) {
    return (
      <span className="mono" title={project.repo ?? undefined}>
        {label}
      </span>
    );
  }
  return <PullRequestLink href={href}>{label}</PullRequestLink>;
}

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
        width: "10rem",
        cell: (p) => (
          <span className="block min-w-0 truncate font-medium" title={p.name}>
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
        width: "9rem",
        cell: (p) => (
          <span
            className="mono block min-w-0 truncate text-[var(--text-muted)]"
            title={p.id}
          >
            {p.id}
          </span>
        ),
      },
      {
        key: "provider",
        header: "Provider",
        width: "6rem",
        cell: (p) => <span className="mono">{p.provider || "—"}</span>,
      },
      {
        key: "repo",
        header: "Repo",
        width: "14rem",
        cell: (p) => <RepoCell project={p} />,
      },
      {
        key: "repoPath",
        header: "Path",
        width: "18rem",
        cell: (p) => (
          <span
            className="mono block min-w-0 truncate text-[var(--text-muted)]"
            title={p.repoPath}
          >
            {p.repoPath}
          </span>
        ),
      },
      {
        key: "baseBranch",
        header: "Base",
        width: "6rem",
        cell: (p) => (
          <span className="mono block min-w-0 truncate" title={p.baseBranch}>
            {p.baseBranch}
          </span>
        ),
      },
      {
        key: "updatedAt",
        header: "Updated",
        width: "8rem",
        cell: (p) => (
          <span className="mono block min-w-0 truncate text-[var(--text-muted)]">
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
          <h1 className="m-0 inline-flex items-center gap-1.5 text-[15px] font-semibold">
            <FolderGit2
              size={15}
              className="shrink-0 text-[var(--text-muted)]"
              aria-hidden
            />
            Projects
          </h1>
          <p className="m-0 mt-0.5 text-[11px] text-[var(--text-muted)]">
            Click a row to set the project filter and open Loops.
          </p>
        </div>
        <Button variant="ghost" size="sm" className="gap-1.5" onClick={refresh}>
          <RefreshCw size={13} className="shrink-0" aria-hidden />
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
              empty={
                <span className="inline-flex items-center gap-1.5 text-[var(--text-muted)]">
                  <Inbox size={14} className="shrink-0" aria-hidden />
                  No projects
                </span>
              }
              onRowClick={onRowClick}
            />
          </>
        )}
      </Card>
    </div>
  );
}
