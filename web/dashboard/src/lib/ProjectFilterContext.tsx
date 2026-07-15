import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { useDashboardData } from "./DashboardDataContext";
import {
  getProjectFilter,
  resolveValidatedProjectFilter,
  setProjectFilter as persistProjectFilter,
} from "./projectFilter";

type ProjectFilterContextValue = {
  /** Persisted selection (may be invalid until projects load). */
  storedProjectId: string;
  /**
   * Effective filter for Running/Loops.
   * Empty while projects loading/error or when stored id is invalid.
   */
  projectId: string;
  setProjectId: (id: string) => void;
  projectsReady: boolean;
};

const ProjectFilterContext = createContext<ProjectFilterContextValue | null>(
  null,
);

export function ProjectFilterProvider({ children }: { children: ReactNode }) {
  const { projects } = useDashboardData();
  const [storedProjectId, setStoredProjectId] = useState(() =>
    getProjectFilter(),
  );

  const setProjectId = useCallback((id: string) => {
    persistProjectFilter(id);
    setStoredProjectId(id);
  }, []);

  const projectItems = projects.data?.items ?? null;
  const projectsReady = projectItems != null;

  const projectId = useMemo(
    () => resolveValidatedProjectFilter(storedProjectId, projectItems),
    [storedProjectId, projectItems],
  );

  // Clear invalid sticky id once list is known.
  useEffect(() => {
    if (!projectsReady || !storedProjectId) return;
    if (projectId === storedProjectId) return;
    // Stored id not in list → fall back to All and persist.
    setProjectId("");
  }, [projectsReady, storedProjectId, projectId, setProjectId]);

  const value = useMemo(
    () => ({
      storedProjectId,
      projectId,
      setProjectId,
      projectsReady,
    }),
    [storedProjectId, projectId, setProjectId, projectsReady],
  );

  return (
    <ProjectFilterContext.Provider value={value}>
      {children}
    </ProjectFilterContext.Provider>
  );
}

export function useProjectFilter(): ProjectFilterContextValue {
  const ctx = useContext(ProjectFilterContext);
  if (!ctx) {
    throw new Error(
      "useProjectFilter must be used within ProjectFilterProvider",
    );
  }
  return ctx;
}
