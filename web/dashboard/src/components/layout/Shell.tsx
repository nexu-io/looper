import { useEffect, useMemo } from "react";
import { Link, NavLink, Outlet } from "react-router-dom";
import { useDashboardData } from "@/lib/DashboardDataContext";
import { useProjectFilter } from "@/lib/ProjectFilterContext";

export const navItems: { to: string; label: string; end?: boolean }[] = [
  { to: "/", label: "Overview", end: true },
  { to: "/loops", label: "Loops" },
  { to: "/projects", label: "Projects" },
  { to: "/config", label: "Config" },
];

/** Public asset under Vite base (/dashboard/). */
const logoSrc = `${import.meta.env.BASE_URL}apple-touch-icon.png`;

export type ShellProps = {
  hostPort: string;
  healthy: boolean | null;
  version?: string;
  onHealthChange?: (healthy: boolean | null, version?: string) => void;
};

function HealthDot({ healthy }: { healthy: boolean | null }) {
  const color =
    healthy === null
      ? "var(--text-muted)"
      : healthy
        ? "var(--ok)"
        : "var(--danger)";
  const label =
    healthy === null ? "unknown" : healthy ? "healthy" : "unhealthy";

  return (
    <span
      className="inline-flex items-center gap-1.5 text-[12px] text-[var(--text-muted)]"
      title={`Daemon ${label}`}
    >
      <span
        className="inline-block h-2 w-2 rounded-full"
        style={{ background: color }}
        aria-hidden
      />
      <span className="sr-only">status:</span>
      {label}
    </span>
  );
}

export function Shell({
  hostPort,
  healthy,
  version,
  onHealthChange,
}: ShellProps) {
  const { health, activeRuns, projects, healthy: sharedHealthy } =
    useDashboardData();
  const { projectId, setProjectId, projectsReady } = useProjectFilter();

  useEffect(() => {
    onHealthChange?.(sharedHealthy);
  }, [sharedHealthy, onHealthChange]);

  const projectItems = projects.data?.items ?? [];

  const activeCount = useMemo(() => {
    const items = activeRuns.data?.items ?? [];
    if (!projectId) return items.length;
    return items.filter((r) => r.projectId === projectId).length;
  }, [activeRuns.data, projectId]);

  // Prefer live shared health; fall back to App-level only when still unknown.
  const chromeHealthy = sharedHealthy !== null ? sharedHealthy : healthy;

  const showStaleBanner =
    Boolean(health.error) ||
    Boolean(activeRuns.error) ||
    Boolean(projects.error);

  return (
    <div className="flex min-h-full flex-col">
      <header className="sticky top-0 z-10 border-b border-[var(--border)] bg-[var(--bg-elevated)]">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 px-3 py-1.5">
          <div className="flex items-center gap-3">
            <Link
              to="/"
              className="flex items-center gap-2 text-[14px] font-semibold tracking-tight"
              title="Overview"
            >
              <img
                src={logoSrc}
                alt=""
                width={20}
                height={20}
                className="h-5 w-5 shrink-0 rounded-[4px]"
                decoding="async"
              />
              <span>Looper</span>
            </Link>
            <span className="mono text-[12px] text-[var(--text-muted)]">
              {hostPort}
            </span>
            <HealthDot healthy={chromeHealthy} />
            <span
              className="inline-flex items-center gap-1 rounded border border-[var(--border)] px-1.5 py-0.5 text-[11px] text-[var(--text-muted)]"
              title="Active runs"
            >
              <span className="uppercase tracking-wide">runs</span>
              <span
                className="mono font-medium"
                style={{
                  color:
                    activeRuns.error || activeRuns.data == null
                      ? "var(--text-muted)"
                      : activeCount > 0
                        ? "var(--ok)"
                        : "var(--text-muted)",
                }}
              >
                {activeRuns.error || activeRuns.data == null
                  ? "—"
                  : activeCount}
              </span>
            </span>
          </div>
          <nav className="flex flex-wrap items-center gap-1">
            {navItems.map((item) => (
              <NavLink
                key={item.to}
                to={item.to}
                end={item.end}
                className={({ isActive }) =>
                  [
                    "rounded px-2 py-0.5 text-[12px] font-medium",
                    isActive
                      ? "bg-[var(--bg-muted)] text-[var(--text)]"
                      : "text-[var(--text-muted)] hover:bg-[var(--bg-muted)] hover:text-[var(--text)]",
                  ].join(" ")
                }
              >
                {item.label}
                {item.to === "/loops" &&
                !activeRuns.error &&
                activeRuns.data != null &&
                activeCount > 0 ? (
                  <span className="ml-1 mono text-[10px] text-[var(--ok)]">
                    {activeCount}
                  </span>
                ) : null}
              </NavLink>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-2">
            <label className="flex items-center gap-1.5 text-[11px] text-[var(--text-muted)]">
              <span className="uppercase tracking-wide">Project</span>
              <select
                className="max-w-[200px] rounded border border-[var(--border)] bg-[var(--bg)] px-1.5 py-0.5 text-[12px] text-[var(--text)]"
                value={projectsReady ? projectId : ""}
                onChange={(e) => setProjectId(e.target.value)}
                disabled={!projectsReady}
                title={
                  projectsReady
                    ? "Filter Loops by project"
                    : projects.error
                      ? "Projects unavailable — filter disabled"
                      : "Loading projects…"
                }
              >
                <option value="">All projects</option>
                {projectItems.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                    {p.archived ? " (archived)" : ""}
                  </option>
                ))}
              </select>
            </label>
          </div>
        </div>
        {showStaleBanner ? (
          <div
            className="border-t border-[var(--border)] bg-[var(--bg-muted)] px-3 py-1 text-[11px] text-[var(--danger)]"
            aria-live="polite"
          >
            Stale data
            {health.error ? ` · health: ${health.error}` : ""}
            {activeRuns.error ? ` · runs: ${activeRuns.error}` : ""}
            {projects.error ? ` · projects: ${projects.error}` : ""}
          </div>
        ) : null}
      </header>

      <main className="flex-1 px-3 py-3">
        <Outlet />
      </main>

      <footer className="border-t border-[var(--border)] px-3 py-1.5 text-[11px] text-[var(--text-muted)]">
        <span className="mono">{version ? `v${version}` : "—"}</span>
      </footer>
    </div>
  );
}
