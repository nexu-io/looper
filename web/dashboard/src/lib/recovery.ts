import type { LoopWorktreeStatus } from "@/lib/api";

/**
 * Recovery modes for displayStatus=manual_intervention.
 * Derived only from durable failure reason + worktree preflight facts —
 * never from parsing failure text.
 */
export type RecoveryMode =
  | "clean"
  | "managed_dirty"
  | "unmanaged_or_unverifiable"
  | "unclassifiable";

/**
 * Classify worktree preflight into the recovery-card action matrix.
 *
 * - clean: present + not dirty → recommend Retry (no discard required)
 * - managed_dirty: present + managed + dirty → Inspect/Jump + confirmed Discard & Retry
 * - unmanaged_or_unverifiable: present but unmanaged or dirty unknown → inspect only, never discard
 * - unclassifiable: missing/unresolvable worktree or fetch failure → reason/logs/Takeover/Stop only
 */
export function classifyRecoveryWorktree(
  worktree: LoopWorktreeStatus | null | undefined,
  opts?: { fetchFailed?: boolean },
): RecoveryMode {
  if (opts?.fetchFailed) return "unclassifiable";
  if (!worktree) return "unclassifiable";

  const reason = (worktree.reason ?? "").trim().toLowerCase();
  if (
    !worktree.present ||
    reason === "no_worktree" ||
    reason === "worktree_missing" ||
    reason === "loop_type_without_worktree"
  ) {
    return "unclassifiable";
  }

  // Fail closed on discard when git status is unavailable or path is unmanaged.
  if (!worktree.managed || reason === "unmanaged" || reason === "status_unavailable") {
    if (worktree.dirty === false || worktree.clean === true) {
      // Unmanaged but clean: retry without discard is still safe guidance.
      return "clean";
    }
    return "unmanaged_or_unverifiable";
  }

  if (worktree.dirty === true) return "managed_dirty";
  if (worktree.dirty === false || worktree.clean === true) return "clean";

  // Present + managed but dirty unknown → never offer discard.
  return "unmanaged_or_unverifiable";
}

/** Whether the recovery card may offer Dashboard Discard & Retry. */
export function recoveryOffersDiscard(mode: RecoveryMode): boolean {
  return mode === "managed_dirty";
}

/** Whether the recovery card should present Retry as the recommended action. */
export function recoveryRecommendsRetry(mode: RecoveryMode): boolean {
  return mode === "clean";
}

export function recoveryJumpCommand(selector: string): string {
  return `looper jump ${selector}`;
}

export function recoveryDiscardCliHint(selector: string): string {
  return `looper retry ${selector} --discard-worktree-changes --confirm`;
}

/** True when loop detail should show the manual-recovery card (not HITL decision). */
export function shouldShowRecoveryCard(loop: {
  status?: string | null;
  displayStatus?: string | null;
}): boolean {
  const display = (loop.displayStatus ?? "").trim().toLowerCase();
  const status = (loop.status ?? "").trim().toLowerCase();
  // awaiting_human stays on the decision card exclusively.
  if (status === "awaiting_human") return false;
  return display === "manual_intervention";
}
