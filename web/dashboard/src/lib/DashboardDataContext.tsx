import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  type ReactNode,
} from "react";
import {
  fetchActiveRuns,
  fetchHealthz,
  fetchProjects,
  type ActiveRunsList,
  type HealthzData,
  type ProjectsList,
} from "./api";
import { usePolling, type UsePollingResult } from "./usePolling";

export type DashboardDataContextValue = {
  health: UsePollingResult<HealthzData>;
  activeRuns: UsePollingResult<ActiveRunsList>;
  projects: UsePollingResult<ProjectsList>;
  /** true only when healthz returned healthy === true (never default to green). */
  healthy: boolean | null;
};

const DashboardDataContext = createContext<DashboardDataContextValue | null>(
  null,
);

/**
 * Single owner for shared polled resources:
 * - healthz ~5s
 * - active runs ~2s
 * - projects ~30s
 */
export function DashboardDataProvider({ children }: { children: ReactNode }) {
  const healthFetcher = useCallback(
    (signal: AbortSignal) => fetchHealthz(signal),
    [],
  );
  const health = usePolling<HealthzData>({
    intervalMs: 5000,
    fetcher: healthFetcher,
  });

  const activeFetcher = useCallback(
    (signal: AbortSignal) => fetchActiveRuns(signal),
    [],
  );
  const activeRuns = usePolling<ActiveRunsList>({
    intervalMs: 2000,
    fetcher: activeFetcher,
  });

  const projectsFetcher = useCallback(
    (signal: AbortSignal) => fetchProjects(signal),
    [],
  );
  const projects = usePolling<ProjectsList>({
    intervalMs: 30_000,
    fetcher: projectsFetcher,
  });

  const healthy: boolean | null = useMemo(() => {
    if (health.error) return false;
    if (!health.data) return null;
    // Never treat missing/undefined healthy as green.
    return health.data.healthy === true;
  }, [health.data, health.error]);

  const value = useMemo(
    () => ({ health, activeRuns, projects, healthy }),
    [health, activeRuns, projects, healthy],
  );

  return (
    <DashboardDataContext.Provider value={value}>
      {children}
    </DashboardDataContext.Provider>
  );
}

export function useDashboardData(): DashboardDataContextValue {
  const ctx = useContext(DashboardDataContext);
  if (!ctx) {
    throw new Error(
      "useDashboardData must be used within DashboardDataProvider",
    );
  }
  return ctx;
}
