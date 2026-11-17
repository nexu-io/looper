import type { LoopWorktreeStatus } from "@/lib/api";
import {
  classifyRetryWorktree,
  type WorktreeActionDecision,
  worktreeAllowsDashboardClear,
  worktreeAllowsDashboardDiscard,
} from "@/lib/worktree";

export type { WorktreeActionDecision };
export {
  classifyRetryWorktree,
  worktreeAllowsDashboardClear,
  worktreeAllowsDashboardDiscard,
};

/**
 * Whether recovery has no classifiable worktree preflight response.
 * Reserved for fetch/unverifiable failures (null payload or request error).
 * Legitimate present=false responses (no_worktree / worktree_missing / etc.)
 * are NOT unavailable — they follow classifyRetryWorktree (retryable "ok").
 */
export function isRecoveryWorktreeUnavailable(
  worktree: LoopWorktreeStatus | null | undefined,
  opts?: { fetchFailed?: boolean },
): boolean {
  if (opts?.fetchFailed) return true;
  if (!worktree) return true;
  return false;
}

/**
 * Action decision for the recovery card. Uses the shared classifyRetryWorktree
 * policy for any successful GET /worktree payload (including present=false).
 * Returns null only when preflight could not be loaded/classified.
 */
export function recoveryWorktreeDecision(
  worktree: LoopWorktreeStatus | null | undefined,
  opts?: { fetchFailed?: boolean },
): WorktreeActionDecision | null {
  if (isRecoveryWorktreeUnavailable(worktree, opts)) return null;
  return classifyRetryWorktree(worktree as LoopWorktreeStatus);
}

/** Whether the recovery card may offer Dashboard Discard & Retry. */
export function recoveryOffersDiscard(
  worktree: LoopWorktreeStatus | null | undefined,
  opts?: { fetchFailed?: boolean },
): boolean {
  if (isRecoveryWorktreeUnavailable(worktree, opts)) return false;
  return worktreeAllowsDashboardDiscard(worktree);
}

/** Whether the recovery card may offer Clear unusable path & Retry. */
export function recoveryOffersClear(
  worktree: LoopWorktreeStatus | null | undefined,
  opts?: { fetchFailed?: boolean },
): boolean {
  if (isRecoveryWorktreeUnavailable(worktree, opts)) return false;
  return worktreeAllowsDashboardClear(worktree);
}

/** Whether the recovery card should present Retry as the recommended action. */
export function recoveryRecommendsRetry(
  worktree: LoopWorktreeStatus | null | undefined,
  opts?: { fetchFailed?: boolean },
): boolean {
  return recoveryWorktreeDecision(worktree, opts) === "ok";
}

export function recoveryJumpCommand(selector: string): string {
  return `looper jump ${selector}`;
}

export function recoveryDiscardCliHint(selector: string): string {
  return `looper retry ${selector} --discard-worktree-changes --confirm`;
}

export function recoveryClearCliHint(selector: string): string {
  return `looper retry ${selector} --clear-unusable-worktree --confirm`;
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

/** Operator-facing guidance for the recovery card action matrix. */
export function recoveryGuidance(
  decision: WorktreeActionDecision | null,
): string {
  if (decision === null) {
    return "Automation stopped for manual intervention, but no safe worktree repair path is available. Review the reason and logs; use Takeover or Stop when appropriate. Do not guess a repair.";
  }
  switch (decision) {
    case "ok":
      return "Plain retry is safe (worktree clean or not required yet). Re-queue automation without discarding changes.";
    case "offer-discard":
      return "Managed worktree has local uncommitted changes. Inspect or jump first, or confirm Discard & Retry to drop them and re-queue.";
    case "offer-clear":
      return "Looper could not verify this managed path as a usable checkout (hollow leftovers). Inspect first if unsure — leftovers may include agent output — then confirm Clear unusable path & Retry.";
    case "inspect-only":
      return "Worktree is unmanaged or its dirty state cannot be verified. Inspect manually; Dashboard discard is unavailable.";
  }
}
