const STORAGE_KEY = "looper.dashboard.projectFilter";

/** Empty string = global (all projects). */
export function getProjectFilter(): string {
  try {
    return localStorage.getItem(STORAGE_KEY) ?? "";
  } catch {
    return "";
  }
}

export function setProjectFilter(projectId: string): void {
  try {
    if (!projectId) {
      localStorage.removeItem(STORAGE_KEY);
    } else {
      localStorage.setItem(STORAGE_KEY, projectId);
    }
  } catch {
    // ignore quota / private mode
  }
}

export type ProjectIdLike = { id: string };

/**
 * Resolve the effective project filter for list pages.
 *
 * - While projects are loading or failed (items == null): treat as All ("").
 * - Only apply a stored id once the list loaded successfully and the id exists.
 * - Invalid sticky ids resolve to All (caller may also clear storage).
 */
export function resolveValidatedProjectFilter(
  storedId: string,
  projectItems: ProjectIdLike[] | null,
): string {
  if (!storedId) {
    return "";
  }
  if (projectItems == null) {
    return "";
  }
  if (!projectItems.some((p) => p.id === storedId)) {
    return "";
  }
  return storedId;
}
