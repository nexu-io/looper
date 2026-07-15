import { useCallback, useMemo, useState } from "react";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { CopyButton } from "@/components/CopyButton";
import { Button } from "@/components/ui/button";
import {
  handbackLoop,
  pauseLoop,
  retryLoop,
  startLoop,
  stopActiveRun,
  takeoverLoop,
  type TakeoverResult,
} from "@/lib/api";
import {
  actionsForLoopStatus,
  type LoopAction,
} from "@/lib/actions";
import { useToast } from "@/lib/toast";

export type LoopActionBarProps = {
  /** Loop selector (seq or id) used in API paths. */
  selector: string;
  status: string;
  hasActiveRun?: boolean;
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
  | null;

const LABELS: Record<LoopAction, string> = {
  pause: "Pause",
  unpause: "Unpause",
  retry: "Retry",
  stop: "Stop",
  takeover: "Takeover",
  handback: "Handback",
};

export function LoopActionBar({
  selector,
  status,
  hasActiveRun,
  onMutated,
  mode = "full",
}: LoopActionBarProps) {
  const toast = useToast();
  const enabled = useMemo(
    () => actionsForLoopStatus(status, { hasActiveRun }),
    [status, hasActiveRun],
  );

  const [pending, setPending] = useState<LoopAction | null>(null);
  const [confirm, setConfirm] = useState<PendingConfirm>(null);
  const [takeoverResult, setTakeoverResult] = useState<TakeoverResult | null>(
    null,
  );
  const [inlineError, setInlineError] = useState<string | null>(null);

  const busy = pending !== null;

  const runAction = useCallback(
    async (action: LoopAction) => {
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
          case "retry":
            await retryLoop(selector);
            toast.success("Retry queued");
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
        // Keep buttons pending until post-mutation refresh finishes (or fails).
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

  const onClick = (action: LoopAction) => {
    if (busy || !enabled[action]) return;
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
    }
  })();

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex flex-wrap items-center gap-1">
        {visibleActions.map((action) => {
          if (!enabled[action] && mode === "compact") return null;
          return (
            <Button
              key={action}
              variant={
                action === "stop" || action === "takeover" ? "danger" : "ghost"
              }
              size="sm"
              disabled={busy || !enabled[action]}
              onClick={() => onClick(action)}
              title={
                !enabled[action]
                  ? `Not available for status ${status || "—"}`
                  : LABELS[action]
              }
            >
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
          danger={confirmCopy.danger}
          busy={busy}
          onCancel={() => {
            if (!busy) setConfirm(null);
          }}
          onConfirm={() => void runAction(confirm.action)}
        >
          <p className="m-0 text-[var(--text-muted)]">{confirmCopy.body}</p>
          <p className="mt-2 mb-0 mono text-[11px] text-[var(--text-muted)]">
            selector: {selector}
          </p>
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
