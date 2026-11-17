/**
 * Conservative enablement for loop operator actions.
 * When unsure, disable rather than invent — API 4xx still surfaces if enabled wrongly.
 */

export type LoopAction =
  | "pause"
  | "unpause"
  | "retry"
  | "stop"
  | "takeover"
  | "handback";

export type LoopActionsEnabled = Record<LoopAction, boolean>;

export type ActionsForLoopStatusOpts = {
  /** True when an active run is known for this loop (e.g. from /runs/active). */
  hasActiveRun?: boolean;
  /**
   * Projected display status (read-only). When manual_intervention, Unpause is
   * disabled so operators cannot bypass recovery-card worktree preflight via
   * POST …/start (which requeues without classifying /worktree).
   */
  displayStatus?: string | null;
};

/**
 * Pure enablement matrix for loop mutation buttons.
 *
 * - pause: running, queued, waiting, idle
 * - unpause: paused (maps to POST …/start — not CLI "resume"); disabled when
 *   displayStatus is manual_intervention (use recovery retry preflight instead)
 * - retry: failed, paused, interrupted (not running, not stopped)
 * - stop: hasActiveRun or status running
 * - takeover: running, waiting, awaiting_human
 * - handback: human_takeover
 */
export function actionsForLoopStatus(
  status: string,
  opts?: ActionsForLoopStatusOpts,
): LoopActionsEnabled {
  const s = (status ?? "").trim().toLowerCase();
  const hasActiveRun = opts?.hasActiveRun === true;
  const display = (opts?.displayStatus ?? "").trim().toLowerCase();
  const manualHold = display === "manual_intervention";

  return {
    pause:
      s === "running" || s === "queued" || s === "waiting" || s === "idle",
    // Generic Unpause → /start skips worktree classification; recovery card owns restart under MI.
    unpause: s === "paused" && !manualHold,
    retry: s === "failed" || s === "paused" || s === "interrupted",
    stop: hasActiveRun || s === "running",
    // human_takeover omitted (spec marked uncertain) — disable when unsure
    takeover:
      s === "running" || s === "waiting" || s === "awaiting_human",
    handback: s === "human_takeover",
  };
}
