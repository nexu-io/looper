import type { LoopWorktreeStatus } from "@/lib/api";

/**
 * Single worktree action classification for Dashboard retry and recovery UI.
 * Discard is only offered for present + managed + dirty — matching daemon
 * preflight authority. Clear is offered for present + managed + unusable_path.
 * There is intentionally no parallel recovery taxonomy.
 */
export type WorktreeActionDecision =
  | "ok"
  | "offer-discard"
  | "offer-clear"
  | "inspect-only";

/**
 * Classify worktree preflight for retry / recovery action enablement.
 *
 * - ok: plain retry is safe guidance (missing tree, or present + clean)
 * - offer-discard: present + managed + dirty
 * - offer-clear: present + managed + reason unusable_path (hollow leftovers)
 * - inspect-only: present but discard/clear unsafe (unmanaged dirty, or dirty unknown)
 */
export function classifyRetryWorktree(
  worktree: LoopWorktreeStatus,
): WorktreeActionDecision {
  if (!worktree.present) return "ok";
  if (worktree.reason === "unusable_path") {
    // Require daemon-advertised clear support so a stale dashboard against an
    // older looperd cannot POST clear and get a silent plain retry.
    if (worktree.managed && worktree.supportsClearUnusablePath === true) {
      return "offer-clear";
    }
    return "inspect-only";
  }
  if (worktree.dirty === true) {
    return worktree.managed ? "offer-discard" : "inspect-only";
  }
  if (worktree.dirty === false || worktree.clean === true) return "ok";
  // Dirty state unverifiable: fail closed — inspect, never offer discard or
  // present plain-retry as known-safe.
  return "inspect-only";
}

/** Dashboard discard is allowed only for present + managed + dirty. */
export function worktreeAllowsDashboardDiscard(
  worktree: LoopWorktreeStatus | null | undefined,
): boolean {
  if (!worktree) return false;
  return classifyRetryWorktree(worktree) === "offer-discard";
}

/** Dashboard clear-unusable is allowed only for present + managed + unusable_path. */
export function worktreeAllowsDashboardClear(
  worktree: LoopWorktreeStatus | null | undefined,
): boolean {
  if (!worktree) return false;
  return classifyRetryWorktree(worktree) === "offer-clear";
}
