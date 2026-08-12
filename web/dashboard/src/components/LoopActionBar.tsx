import { useCallback, useMemo, useState } from "react";
import {
  Hand,
  Pause,
  Play,
  RotateCcw,
  Square,
  Undo2,
  type LucideIcon,
} from "lucide-react";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { CopyButton } from "@/components/CopyButton";
import { Button } from "@/components/ui/button";
import {
  ApiError,
  fetchLoopWorktree,
  handbackLoop,
  pauseLoop,
  retryLoop,
  startLoop,
  stopActiveRun,
  takeoverLoop,
  type LoopWorktreeStatus,
  type TakeoverResult,
} from "@/lib/api";
import {
  actionsForLoopStatus,
  type LoopAction,
} from "@/lib/actions";
import { useToast } from "@/lib/toast";
import { classifyRetryWorktree } from "@/lib/worktree";

// Re-export so existing test imports keep working; single authority lives in lib/worktree.
export { classifyRetryWorktree } from "@/lib/worktree";

export type LoopActionBarProps = {
  /** Loop selector (seq or id) used in API paths. */
  selector: string;
  status: string;
  hasActiveRun?: boolean;
  /**
   * Projected display status from the API. When manual_intervention, Unpause is
   * disabled so operators use recovery retry (worktree preflight) instead of
   * generic POST …/start.
   */
  displayStatus?: string | null;
  /**
   * Called after a successful mutation so the page can refetch.
   * Awaited while action buttons stay pending (use forceRefresh).
   */
  onMutated?: () => void | Promise<void>;
  /**
   * compact: only Stop (+ Pause when enabled) for Running table rows.
   * full: all actions for loop detail.
   */
  mode?: "full" | "compact";
};

type PendingConfirm =
  | { action: "stop" }
  | { action: "takeover" }
  | { action: "handback" }
  | { action: "retry-dirty"; worktree: LoopWorktreeStatus }
  | { action: "retry-unusable"; worktree: LoopWorktreeStatus }
  | null;

type InspectGuidance = {
  worktree: LoopWorktreeStatus;
  jumpCommand: string;
  /** When false, hide discard CLI hint (unmanaged paths). */
  offerDiscard: boolean;
  /** When true, show clear-unusable CLI hint. */
  offerClear?: boolean;
} | null;

const LABELS: Record<LoopAction, string> = {
  pause: "Pause",
  unpause: "Unpause",
  retry: "Retry",
  stop: "Stop",
  takeover: "Takeover",
  handback: "Handback",
};

const ICONS: Record<LoopAction, LucideIcon> = {
  pause: Pause,
  unpause: Play,
  retry: RotateCcw,
  stop: Square,
  takeover: Hand,
  handback: Undo2,
};

/** True when preflight is missing on older daemons — fall back to plain retry. */
export function isWorktreeRouteUnavailable(err: unknown): boolean {
  if (!(err instanceof ApiError)) return false;
  if (err.code === "ROUTE_NOT_FOUND") return true;
  if (err.status === 404) {
    const msg = (err.message || "").toLowerCase();
    return msg.includes("unknown route") || msg.includes("not found");
  }
  return false;
}

export function LoopActionBar({
  selector,
  status,
  hasActiveRun,
  displayStatus,
  onMutated,
  mode = "full",
}: LoopActionBarProps) {
  const toast = useToast();
  const enabled = useMemo(
    () => actionsForLoopStatus(status, { hasActiveRun, displayStatus }),
    [status, hasActiveRun, displayStatus],
  );

  const [pending, setPending] = useState<LoopAction | null>(null);
  const [confirm, setConfirm] = useState<PendingConfirm>(null);
  const [takeoverResult, setTakeoverResult] = useState<TakeoverResult | null>(
    null,
  );
  const [inspectGuidance, setInspectGuidance] =
    useState<InspectGuidance>(null);
  const [inlineError, setInlineError] = useState<string | null>(null);

  const busy = pending !== null;

  const finishRetry = useCallback(
    async (opts?: {
      discardWorktreeChanges?: boolean;
      clearUnusableWorktreePath?: boolean;
      expectedWorktreePath?: string;
    }) => {
      const discardWorktreeChanges = opts?.discardWorktreeChanges === true;
      const clearUnusableWorktreePath = opts?.clearUnusableWorktreePath === true;
      const expectedWorktreePath = opts?.expectedWorktreePath?.trim() ?? "";
      await retryLoop(selector, {
        discardWorktreeChanges,
        ...(clearUnusableWorktreePath
          ? {
              clearUnusableWorktreePath: true,
              expectedWorktreePath,
            }
          : {}),
      });
      toast.success(
        clearUnusableWorktreePath
          ? "Retry queued (unusable path cleared)"
          : discardWorktreeChanges
            ? "Retry queued (worktree discarded)"
            : "Retry queued",
      );
      await onMutated?.();
    },
    [selector, toast, onMutated],
  );

  const runAction = useCallback(
    async (action: Exclude<LoopAction, "retry">) => {
      setPending(action);
      setInlineError(null);
      try {
        switch (action) {
          case "pause":
            await pauseLoop(selector);
            toast.success("Paused");
            break;
          case "unpause":
            await startLoop(selector);
            toast.success("Unpaused (started)");
            break;
          case "stop":
            await stopActiveRun(selector);
            toast.success("Stop requested");
            break;
          case "takeover": {
            const result = await takeoverLoop(selector);
            setTakeoverResult(result);
            toast.success(
              result.supported
                ? "Takeover: loop parked"
                : "Takeover: parked (interactive resume unsupported)",
            );
            break;
          }
          case "handback":
            await handbackLoop(selector);
            toast.success("Handback queued");
            break;
        }
        await onMutated?.();
      } catch (err) {
        const message = err instanceof Error ? err.message : String(err);
        setInlineError(message);
        toast.error(message);
      } finally {
        setPending(null);
        setConfirm(null);
      }
    },
    [selector, toast, onMutated],
  );

  const onRetryClick = useCallback(async () => {
    if (busy || !enabled.retry) return;
    setPending("retry");
    setInlineError(null);
    try {
      let worktree: LoopWorktreeStatus;
      try {
        worktree = await fetchLoopWorktree(selector);
      } catch (err) {
        if (isWorktreeRouteUnavailable(err)) {
          // Older daemon without /worktree — keep prior plain-retry behavior.
          await finishRetry();
          return;
        }
        throw err;
      }

      const decision = classifyRetryWorktree(worktree);
      if (decision === "offer-discard") {
        setConfirm({ action: "retry-dirty", worktree });
        return;
      }
      if (decision === "offer-clear") {
        setConfirm({ action: "retry-unusable", worktree });
        return;
      }
      if (decision === "inspect-only") {
        setInspectGuidance({
          worktree,
          jumpCommand: `looper jump ${selector}`,
          offerDiscard: false,
        });
        toast.error(
          worktree.managed
            ? "Worktree dirty state could not be verified; inspect before retrying"
            : "Dirty worktree is not Looper-managed; inspect before retrying",
        );
        return;
      }
      await finishRetry();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setInlineError(message);
      toast.error(message);
    } finally {
      setPending(null);
    }
  }, [busy, enabled.retry, selector, toast, finishRetry]);

  const onDiscardRetry = useCallback(async () => {
    setPending("retry");
    setInlineError(null);
    try {
      await finishRetry({ discardWorktreeChanges: true });
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setInlineError(message);
      toast.error(message);
    } finally {
      setPending(null);
      setConfirm(null);
    }
  }, [finishRetry, toast]);

  const onClearRetry = useCallback(async () => {
    setPending("retry");
    setInlineError(null);
    try {
      const expectedWorktreePath =
        confirm?.action === "retry-unusable"
          ? (confirm.worktree.worktreePath?.trim() ?? "")
          : "";
      if (!expectedWorktreePath) {
        throw new Error(
          "Confirmed worktree path is missing; re-open clear confirm from a fresh worktree status",
        );
      }
      await finishRetry({
        clearUnusableWorktreePath: true,
        expectedWorktreePath,
      });
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setInlineError(message);
      toast.error(message);
    } finally {
      setPending(null);
      setConfirm(null);
    }
  }, [confirm, finishRetry, toast]);

  const onClick = (action: LoopAction) => {
    if (busy || !enabled[action]) return;
    if (action === "retry") {
      void onRetryClick();
      return;
    }
    if (action === "stop" || action === "takeover" || action === "handback") {
      setConfirm({ action });
      return;
    }
    void runAction(action);
  };

  const visibleActions: LoopAction[] =
    mode === "compact"
      ? (["stop", "pause"] as LoopAction[])
      : (["pause", "unpause", "retry", "stop", "takeover", "handback"] as LoopAction[]);

  const confirmCopy = (() => {
    if (!confirm) return null;
    switch (confirm.action) {
      case "stop":
        return {
          title: "Stop active run?",
          body: "Pauses the loop and stops the active execution. The loop stays paused until you unpause or retry.",
          confirmLabel: "Stop",
          danger: true,
        };
      case "takeover":
        return {
          title: "Take over loop?",
          body: "Parks the loop in human_takeover and stops the daemon run. You will get a worktree path and resume command (if supported) to continue interactively. Hand back when done.",
          confirmLabel: "Takeover",
          danger: true,
        };
      case "handback":
        return {
          title: "Hand back to daemon?",
          body: "Re-queues the loop so the daemon resumes after your interactive session. Worktree edits are preserved (discard is not allowed on handback).",
          confirmLabel: "Handback",
          danger: false,
        };
      case "retry-dirty":
        return {
          title: "Dirty worktree — discard and retry?",
          body: "Local uncommitted changes were found in the loop worktree. Discard them before retrying, or inspect the worktree first.",
          confirmLabel: "Discard & retry",
          danger: true,
          cancelLabel: "Inspect first",
        };
      case "retry-unusable":
        return {
          title: "Unusable worktree path — clear and retry?",
          body: "Looper could not verify this as a usable checkout. Confirming will delete the entire managed path, including uncommitted or leftover files, then re-queue the loop. Inspect first if unsure.",
          confirmLabel: "Clear path & retry",
          danger: true,
          cancelLabel: "Inspect first",
        };
    }
  })();

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex flex-wrap items-center gap-1">
        {visibleActions.map((action) => {
          if (!enabled[action] && mode === "compact") return null;
              const Icon = ICONS[action];
          // Fill Stop/Pause so small media glyphs read solid, not empty outlines.
          const iconProps =
            action === "stop" || action === "pause"
              ? { size: 10, className: "shrink-0", fill: "currentColor" }
              : { size: 11, className: "shrink-0" };
          return (
            <Button
              key={action}
              variant={
                action === "stop" || action === "takeover" ? "danger" : "ghost"
              }
              size="sm"
              className="gap-1"
              disabled={busy || !enabled[action]}
              onClick={() => onClick(action)}
              title={
                !enabled[action]
                  ? `Not available for status ${status || "—"}`
                  : LABELS[action]
              }
            >
              <Icon {...iconProps} aria-hidden />
              {pending === action ? "…" : LABELS[action]}
            </Button>
          );
        })}
      </div>
      {inlineError ? (
        <p className="m-0 text-[11px] text-[var(--danger)]">{inlineError}</p>
      ) : null}

      {confirm && confirmCopy ? (
        <ConfirmDialog
          open
          title={confirmCopy.title}
          confirmLabel={confirmCopy.confirmLabel}
          cancelLabel={
            "cancelLabel" in confirmCopy ? confirmCopy.cancelLabel : undefined
          }
          danger={confirmCopy.danger}
          busy={busy}
          onCancel={() => {
            if (busy) return;
            if (confirm.action === "retry-dirty") {
              const wt = confirm.worktree;
              setConfirm(null);
              setInspectGuidance({
                worktree: wt,
                jumpCommand: `looper jump ${selector}`,
                offerDiscard: true,
              });
              return;
            }
            if (confirm.action === "retry-unusable") {
              const wt = confirm.worktree;
              setConfirm(null);
              setInspectGuidance({
                worktree: wt,
                jumpCommand: `looper jump ${selector}`,
                offerDiscard: false,
                offerClear: true,
              });
              return;
            }
            setConfirm(null);
          }}
          onConfirm={() => {
            if (confirm.action === "retry-dirty") {
              void onDiscardRetry();
              return;
            }
            if (confirm.action === "retry-unusable") {
              void onClearRetry();
              return;
            }
            void runAction(confirm.action);
          }}
        >
          <p className="m-0 text-[var(--text-muted)]">{confirmCopy.body}</p>
          {confirm.action === "retry-dirty" ||
          confirm.action === "retry-unusable" ? (
            <div className="mt-2 flex flex-col gap-2">
              {confirm.worktree.branch ? (
                <p className="m-0 mono text-[11px] text-[var(--text-muted)]">
                  branch: {confirm.worktree.branch}
                </p>
              ) : null}
              <div className="rounded border border-[var(--border)] bg-[var(--bg)] p-2">
                <div className="mb-1 flex items-center justify-between gap-2">
                  <span className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
                    {confirm.action === "retry-unusable"
                      ? "Path to remove"
                      : "Worktree"}
                  </span>
                  <CopyButton text={confirm.worktree.worktreePath ?? ""} />
                </div>
                <p className="m-0 break-all mono text-[11px]">
                  {confirm.worktree.worktreePath || "—"}
                </p>
              </div>
            </div>
          ) : (
            <p className="mt-2 mb-0 mono text-[11px] text-[var(--text-muted)]">
              selector: {selector}
            </p>
          )}
        </ConfirmDialog>
      ) : null}

      {inspectGuidance ? (
        <ConfirmDialog
          open
          title={
            inspectGuidance.offerDiscard
              ? "Inspect dirty worktree"
              : inspectGuidance.offerClear
                ? "Inspect unusable worktree path"
                : "Unmanaged dirty worktree"
          }
          confirmLabel="Close"
          showCancel={false}
          onCancel={() => setInspectGuidance(null)}
          onConfirm={() => setInspectGuidance(null)}
        >
          <div className="flex flex-col gap-2">
            <p className="m-0 text-[var(--text-muted)]">
              {inspectGuidance.offerDiscard
                ? "Review local changes in the worktree, then retry again. Use jump from a terminal on this machine."
                : inspectGuidance.offerClear
                  ? "Looper could not verify this as a usable checkout. Inspect leftovers, then clear the path and retry if appropriate."
                  : "This path is not a Looper-managed worktree, so discard is unavailable. Inspect manually, then retry only after the tree is clean or the path is fixed."}
            </p>
            {inspectGuidance.worktree.branch ? (
              <p className="m-0 mono text-[11px] text-[var(--text-muted)]">
                branch: {inspectGuidance.worktree.branch}
              </p>
            ) : null}
            <div className="rounded border border-[var(--border)] bg-[var(--bg)] p-2">
              <div className="mb-1 flex items-center justify-between gap-2">
                <span className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
                  Worktree
                </span>
                <CopyButton
                  text={inspectGuidance.worktree.worktreePath ?? ""}
                />
              </div>
              <p className="m-0 break-all mono text-[11px]">
                {inspectGuidance.worktree.worktreePath || "—"}
              </p>
            </div>
            {inspectGuidance.worktree.present ? (
              <div className="rounded border border-[var(--border)] bg-[var(--bg)] p-2">
                <div className="mb-1 flex items-center justify-between gap-2">
                  <span className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
                    Jump command
                  </span>
                  <CopyButton text={inspectGuidance.jumpCommand} />
                </div>
                <p className="m-0 break-all mono text-[11px]">
                  {inspectGuidance.jumpCommand}
                </p>
              </div>
            ) : null}
            {inspectGuidance.offerDiscard ? (
              <p className="m-0 text-[11px] text-[var(--text-muted)]">
                After fixing or deciding to drop changes: Retry again, or run{" "}
                <span className="mono">
                  looper retry {selector} --discard-worktree-changes --confirm
                </span>
              </p>
            ) : null}
            {inspectGuidance.offerClear ? (
              <p className="m-0 text-[11px] text-[var(--text-muted)]">
                After inspecting leftovers: Retry again, or run{" "}
                <span className="mono">
                  looper retry {selector} --clear-unusable-worktree --confirm
                </span>
              </p>
            ) : null}
          </div>
        </ConfirmDialog>
      ) : null}

      {takeoverResult ? (
        <ConfirmDialog
          open
          title="Takeover result"
          confirmLabel="Close"
          showCancel={false}
          onCancel={() => setTakeoverResult(null)}
          onConfirm={() => setTakeoverResult(null)}
        >
          <div className="flex flex-col gap-2">
            {takeoverResult.message ? (
              <p className="m-0 text-[var(--text-muted)]">
                {takeoverResult.message}
              </p>
            ) : (
              <p className="m-0 text-[var(--text-muted)]">
                {takeoverResult.supported
                  ? "Loop parked. Use the resume command in the worktree."
                  : "Loop parked. Interactive resume is not supported for this agent/session."}
              </p>
            )}
            <div className="rounded border border-[var(--border)] bg-[var(--bg)] p-2">
              <div className="mb-1 flex items-center justify-between gap-2">
                <span className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
                  Worktree
                </span>
                <CopyButton text={takeoverResult.worktreePath ?? ""} />
              </div>
              <p className="m-0 break-all mono text-[11px]">
                {takeoverResult.worktreePath || "—"}
              </p>
            </div>
            <div className="rounded border border-[var(--border)] bg-[var(--bg)] p-2">
              <div className="mb-1 flex items-center justify-between gap-2">
                <span className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
                  Resume command
                </span>
                <CopyButton text={takeoverResult.resumeCommand ?? ""} />
              </div>
              <p className="m-0 break-all mono text-[11px]">
                {takeoverResult.resumeCommand ||
                  (takeoverResult.supported
                    ? "—"
                    : "(unsupported — copy worktree and resume manually)")}
              </p>
            </div>
            {takeoverResult.sessionId ? (
              <p className="m-0 mono text-[11px] text-[var(--text-muted)]">
                session: {takeoverResult.sessionId}
              </p>
            ) : null}
          </div>
        </ConfirmDialog>
      ) : null}
    </div>
  );
}
