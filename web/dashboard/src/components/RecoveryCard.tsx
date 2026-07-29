import { useCallback, useEffect, useMemo, useState } from "react";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { CopyButton } from "@/components/CopyButton";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  ApiError,
  fetchLoopWorktree,
  retryLoop,
  stopActiveRun,
  takeoverLoop,
  type Loop,
  type LoopWorktreeStatus,
  type TakeoverResult,
} from "@/lib/api";
import { actionsForLoopStatus } from "@/lib/actions";
import { isWorktreeRouteUnavailable } from "@/components/LoopActionBar";
import {
  classifyRetryWorktree,
  recoveryDiscardCliHint,
  recoveryGuidance,
  recoveryJumpCommand,
  recoveryOffersDiscard,
  recoveryRecommendsRetry,
  recoveryWorktreeDecision,
  shouldShowRecoveryCard,
} from "@/lib/recovery";
import { useToast } from "@/lib/toast";

export type RecoveryCardProps = {
  loop: Loop;
  selector: string;
  hasActiveRun?: boolean;
  onMutated?: () => void | Promise<void>;
};

function ownershipLabel(worktree: LoopWorktreeStatus | null): string {
  if (!worktree) return "—";
  if (!worktree.present) return "missing";
  return worktree.managed ? "managed" : "unmanaged";
}

function dirtyLabel(worktree: LoopWorktreeStatus | null): string {
  if (!worktree?.present) return "—";
  if (worktree.dirty === true) return "dirty";
  if (worktree.dirty === false || worktree.clean === true) return "clean";
  return "unknown";
}

/**
 * Prominent recovery card for displayStatus=manual_intervention.
 * View/inspect/dismiss are non-mutating; only explicit action buttons change state.
 */
export function RecoveryCard({
  loop,
  selector,
  hasActiveRun,
  onMutated,
}: RecoveryCardProps) {
  const toast = useToast();
  const [worktree, setWorktree] = useState<LoopWorktreeStatus | null>(null);
  const [worktreeError, setWorktreeError] = useState<string | null>(null);
  const [fetchFailed, setFetchFailed] = useState(false);
  const [loadingWt, setLoadingWt] = useState(false);
  const [pending, setPending] = useState<
    "retry" | "discard-retry" | "takeover" | "stop" | null
  >(null);
  const [confirmDiscard, setConfirmDiscard] = useState(false);
  const [confirmStop, setConfirmStop] = useState(false);
  const [confirmTakeover, setConfirmTakeover] = useState(false);
  const [inspectOpen, setInspectOpen] = useState(false);
  const [takeoverResult, setTakeoverResult] = useState<TakeoverResult | null>(
    null,
  );
  const [inlineError, setInlineError] = useState<string | null>(null);

  const visible = shouldShowRecoveryCard(loop);

  // Drop every loop-scoped UI bit immediately when the selector changes so
  // client-side navigation cannot show loop A's worktree under loop B's actions.
  const resetLoopScopedState = useCallback(() => {
    setWorktree(null);
    setWorktreeError(null);
    setFetchFailed(false);
    setLoadingWt(false);
    setPending(null);
    setConfirmDiscard(false);
    setConfirmStop(false);
    setConfirmTakeover(false);
    setInspectOpen(false);
    setTakeoverResult(null);
    setInlineError(null);
  }, []);

  const loadWorktree = useCallback(
    async (signal?: AbortSignal) => {
      if (!visible) return;
      setLoadingWt(true);
      setWorktreeError(null);
      setFetchFailed(false);
      try {
        const wt = await fetchLoopWorktree(selector, signal);
        if (signal?.aborted) return;
        setWorktree(wt);
      } catch (err) {
        if (signal?.aborted) return;
        if (err instanceof DOMException && err.name === "AbortError") return;
        if (err instanceof Error && err.name === "AbortError") return;
        // Older daemons without /worktree: treat as unclassifiable, not a hard fail.
        if (err instanceof ApiError && err.status === 404) {
          setWorktree(null);
          setFetchFailed(true);
          setWorktreeError(null);
          return;
        }
        setWorktree(null);
        setFetchFailed(true);
        setWorktreeError(err instanceof Error ? err.message : String(err));
      } finally {
        if (!signal?.aborted) setLoadingWt(false);
      }
    },
    [selector, visible],
  );

  // Read-only preflight on mount / selector change — never mutates loop state.
  // Reset is synchronous in the effect body so a mid-flight response from the
  // previous selector cannot paint under the new one (abort still races paint).
  useEffect(() => {
    resetLoopScopedState();
    if (!visible) {
      return;
    }
    const controller = new AbortController();
    void loadWorktree(controller.signal);
    return () => controller.abort();
  }, [selector, visible, loadWorktree, resetLoopScopedState]);

  const decision = useMemo(
    () => recoveryWorktreeDecision(worktree, { fetchFailed }),
    [worktree, fetchFailed],
  );
  const actions = useMemo(
    () => actionsForLoopStatus(loop.status, { hasActiveRun }),
    [loop.status, hasActiveRun],
  );

  const busy = pending !== null;
  const reason =
    loop.lastFailureReason?.trim() ||
    loop.lastFailureKind?.trim() ||
    "(no failure reason recorded)";
  const jumpCmd = recoveryJumpCommand(selector);
  const discardCli = recoveryDiscardCliHint(selector);
  const offersDiscard = recoveryOffersDiscard(worktree, { fetchFailed });
  const recommendsRetry = recoveryRecommendsRetry(worktree, { fetchFailed });

  const finishRetry = useCallback(
    async (discardWorktreeChanges: boolean) => {
      await retryLoop(selector, { discardWorktreeChanges });
      toast.success(
        discardWorktreeChanges
          ? "Retry queued (worktree discarded)"
          : "Retry queued",
      );
      await onMutated?.();
    },
    [selector, toast, onMutated],
  );

  // Plain retry must re-fetch /worktree at click time (same authority as
  // LoopActionBar): a stale "ok" preflight or "Retry without discard" on a
  // dirty/unmanaged tree must not requeue until classification is freshly ok.
  const onRetry = useCallback(async () => {
    if (busy || !actions.retry) return;
    setPending("retry");
    setInlineError(null);
    try {
      let fresh: LoopWorktreeStatus;
      try {
        fresh = await fetchLoopWorktree(selector);
      } catch (err) {
        if (isWorktreeRouteUnavailable(err)) {
          // Older daemon without /worktree — match LoopActionBar plain retry.
          await finishRetry(false);
          return;
        }
        setWorktree(null);
        setFetchFailed(true);
        const message = err instanceof Error ? err.message : String(err);
        setWorktreeError(message);
        setInlineError(message);
        toast.error(message);
        return;
      }

      setWorktree(fresh);
      setFetchFailed(false);
      setWorktreeError(null);

      const decision = classifyRetryWorktree(fresh);
      if (decision === "offer-discard") {
        toast.error(
          "Worktree has local uncommitted changes; discard or inspect before plain retry",
        );
        return;
      }
      if (decision === "inspect-only") {
        toast.error(
          fresh.managed
            ? "Worktree dirty state could not be verified; inspect before retrying"
            : "Dirty worktree is not Looper-managed; inspect before retrying",
        );
        return;
      }

      await finishRetry(false);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setInlineError(message);
      toast.error(message);
    } finally {
      setPending(null);
    }
  }, [busy, actions.retry, selector, finishRetry, toast]);

  const onDiscardRetry = useCallback(async () => {
    if (busy || !offersDiscard || !actions.retry) return;
    setPending("discard-retry");
    setInlineError(null);
    try {
      await finishRetry(true);
      setConfirmDiscard(false);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setInlineError(message);
      toast.error(message);
    } finally {
      setPending(null);
    }
  }, [busy, offersDiscard, actions.retry, finishRetry, toast]);

  const onTakeover = useCallback(async () => {
    if (busy || !actions.takeover) return;
    setPending("takeover");
    setInlineError(null);
    try {
      const result = await takeoverLoop(selector);
      setTakeoverResult(result);
      setConfirmTakeover(false);
      toast.success(
        result.supported
          ? "Takeover: loop parked"
          : "Takeover: parked (interactive resume unsupported)",
      );
      await onMutated?.();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setInlineError(message);
      toast.error(message);
    } finally {
      setPending(null);
    }
  }, [busy, actions.takeover, selector, toast, onMutated]);

  const onStop = useCallback(async () => {
    if (busy || !actions.stop) return;
    setPending("stop");
    setInlineError(null);
    try {
      await stopActiveRun(selector);
      toast.success("Stop requested");
      setConfirmStop(false);
      await onMutated?.();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setInlineError(message);
      toast.error(message);
    } finally {
      setPending(null);
    }
  }, [busy, actions.stop, selector, toast, onMutated]);

  if (!visible) return null;

  return (
    <>
      <Card
        title="Manual intervention required"
        className="border-[var(--warn)]"
        actions={
          <span className="mono text-[10px] uppercase tracking-wide text-[var(--warn)]">
            recovery
          </span>
        }
      >
        <div className="flex flex-col gap-3">
          <p className="m-0 text-[12px] text-[var(--text-muted)]">
            Automation stopped. Review the durable failure reason and worktree
            facts, then take an explicit recovery action. Viewing this card does
            not acknowledge or advance the loop.
          </p>

          <div className="rounded border border-[var(--border)] bg-[var(--bg)] p-2">
            <div className="mb-1 text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
              Failure reason
            </div>
            <p className="m-0 whitespace-pre-wrap break-words text-[13px]">
              {reason}
            </p>
            {loop.lastFailureKind?.trim() ? (
              <p className="mt-1 mb-0 mono text-[11px] text-[var(--text-muted)]">
                kind: {loop.lastFailureKind.trim()}
              </p>
            ) : null}
          </div>

          <div className="rounded border border-[var(--border)] bg-[var(--bg)] p-2">
            <div className="mb-1 flex items-center justify-between gap-2">
              <span className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
                Worktree
              </span>
              {loadingWt ? (
                <span className="text-[10px] text-[var(--text-muted)]">
                  loading…
                </span>
              ) : (
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={busy}
                  onClick={() => void loadWorktree()}
                >
                  Refresh
                </Button>
              )}
            </div>
            {worktreeError ? (
              <p className="m-0 text-[11px] text-[var(--danger)]">
                {worktreeError}
              </p>
            ) : null}
            <dl className="m-0 grid gap-1 text-[12px]">
              <div className="grid grid-cols-[90px_1fr] gap-2">
                <dt className="text-[var(--text-muted)]">Path</dt>
                <dd className="m-0 flex items-start gap-1 break-all mono">
                  <span className="min-w-0 flex-1">
                    {worktree?.worktreePath?.trim() || "—"}
                  </span>
                  {worktree?.worktreePath?.trim() ? (
                    <CopyButton text={worktree.worktreePath} />
                  ) : null}
                </dd>
              </div>
              <div className="grid grid-cols-[90px_1fr] gap-2">
                <dt className="text-[var(--text-muted)]">Present</dt>
                <dd className="m-0 mono">
                  {worktree
                    ? worktree.present
                      ? "yes"
                      : "no"
                    : fetchFailed
                      ? "—"
                      : loadingWt
                        ? "…"
                        : "—"}
                </dd>
              </div>
              <div className="grid grid-cols-[90px_1fr] gap-2">
                <dt className="text-[var(--text-muted)]">Ownership</dt>
                <dd className="m-0 mono">{ownershipLabel(worktree)}</dd>
              </div>
              <div className="grid grid-cols-[90px_1fr] gap-2">
                <dt className="text-[var(--text-muted)]">Dirty</dt>
                <dd className="m-0 mono">{dirtyLabel(worktree)}</dd>
              </div>
              {worktree?.branch ? (
                <div className="grid grid-cols-[90px_1fr] gap-2">
                  <dt className="text-[var(--text-muted)]">Branch</dt>
                  <dd className="m-0 mono">{worktree.branch}</dd>
                </div>
              ) : null}
              {worktree?.reason ? (
                <div className="grid grid-cols-[90px_1fr] gap-2">
                  <dt className="text-[var(--text-muted)]">Preflight</dt>
                  <dd className="m-0 mono">{worktree.reason}</dd>
                </div>
              ) : null}
            </dl>
          </div>

          <p className="m-0 text-[12px]">{recoveryGuidance(decision)}</p>

          <div className="flex flex-wrap items-center gap-1.5">
            {recommendsRetry && actions.retry ? (
              <Button
                size="sm"
                disabled={busy}
                onClick={() => void onRetry()}
              >
                {pending === "retry" ? "…" : "Retry"}
              </Button>
            ) : null}

            {decision === "offer-discard" ? (
              <>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={busy}
                  onClick={() => setInspectOpen(true)}
                >
                  Inspect / Jump
                </Button>
                {actions.retry ? (
                  <Button
                    variant="danger"
                    size="sm"
                    disabled={busy}
                    onClick={() => setConfirmDiscard(true)}
                  >
                    Discard &amp; Retry
                  </Button>
                ) : null}
              </>
            ) : null}

            {decision === "inspect-only" ? (
              <Button
                variant="ghost"
                size="sm"
                disabled={busy}
                onClick={() => setInspectOpen(true)}
              >
                Inspect
              </Button>
            ) : null}

            {decision === null ? (
              <>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => {
                    document
                      .getElementById("loop-logs")
                      ?.scrollIntoView({ behavior: "smooth", block: "start" });
                  }}
                >
                  View logs
                </Button>
                {actions.takeover ? (
                  <Button
                    variant="danger"
                    size="sm"
                    disabled={busy}
                    onClick={() => setConfirmTakeover(true)}
                  >
                    {pending === "takeover" ? "…" : "Takeover"}
                  </Button>
                ) : null}
                {actions.stop ? (
                  <Button
                    variant="danger"
                    size="sm"
                    disabled={busy}
                    onClick={() => setConfirmStop(true)}
                  >
                    {pending === "stop" ? "…" : "Stop"}
                  </Button>
                ) : null}
              </>
            ) : null}

            {/* Allow plain retry on dirty/inspect paths when operator has fixed the tree */}
            {(decision === "offer-discard" || decision === "inspect-only") &&
            actions.retry ? (
              <Button
                variant="ghost"
                size="sm"
                disabled={busy}
                title="Retry without discarding (use after cleaning the tree yourself)"
                onClick={() => void onRetry()}
              >
                {pending === "retry" ? "…" : "Retry without discard"}
              </Button>
            ) : null}
          </div>

          {inlineError ? (
            <p className="m-0 text-[11px] text-[var(--danger)]">{inlineError}</p>
          ) : null}
        </div>
      </Card>

      {confirmDiscard ? (
        <ConfirmDialog
          open
          title="Discard worktree changes and retry?"
          confirmLabel="Discard & retry"
          danger
          busy={busy}
          onCancel={() => {
            if (!busy) setConfirmDiscard(false);
          }}
          onConfirm={() => void onDiscardRetry()}
        >
          <p className="m-0 text-[var(--text-muted)]">
            Local uncommitted changes in the managed worktree will be discarded,
            then the loop will be re-queued. This requires an explicit confirm.
          </p>
          {worktree?.worktreePath ? (
            <div className="mt-2 rounded border border-[var(--border)] bg-[var(--bg)] p-2">
              <div className="mb-1 flex items-center justify-between gap-2">
                <span className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
                  Worktree
                </span>
                <CopyButton text={worktree.worktreePath} />
              </div>
              <p className="m-0 break-all mono text-[11px]">
                {worktree.worktreePath}
              </p>
            </div>
          ) : null}
        </ConfirmDialog>
      ) : null}

      {confirmStop ? (
        <ConfirmDialog
          open
          title="Stop active run?"
          confirmLabel="Stop"
          danger
          busy={busy}
          onCancel={() => {
            if (!busy) setConfirmStop(false);
          }}
          onConfirm={() => void onStop()}
        >
          <p className="m-0 text-[var(--text-muted)]">
            Pauses the loop and stops the active execution. The loop stays paused
            until you unpause or retry.
          </p>
        </ConfirmDialog>
      ) : null}

      {confirmTakeover ? (
        <ConfirmDialog
          open
          title="Take over loop?"
          confirmLabel="Takeover"
          danger
          busy={busy}
          onCancel={() => {
            if (!busy) setConfirmTakeover(false);
          }}
          onConfirm={() => void onTakeover()}
        >
          <p className="m-0 text-[var(--text-muted)]">
            Parks the loop in human_takeover and stops the daemon run. You will
            get a worktree path and resume command (if supported) to continue
            interactively. Hand back when done.
          </p>
        </ConfirmDialog>
      ) : null}

      {inspectOpen ? (
        <ConfirmDialog
          open
          title={
            decision === "offer-discard"
              ? "Inspect dirty worktree"
              : "Inspect worktree"
          }
          confirmLabel="Close"
          showCancel={false}
          onCancel={() => setInspectOpen(false)}
          onConfirm={() => setInspectOpen(false)}
        >
          <div className="flex flex-col gap-2">
            <p className="m-0 text-[var(--text-muted)]">
              Copy the path or jump command into a terminal on this machine.
              Dashboard never executes shell commands or launches Terminal/editor
              apps.
            </p>
            <div className="rounded border border-[var(--border)] bg-[var(--bg)] p-2">
              <div className="mb-1 flex items-center justify-between gap-2">
                <span className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
                  Worktree path
                </span>
                <CopyButton text={worktree?.worktreePath ?? ""} />
              </div>
              <p className="m-0 break-all mono text-[11px]">
                {worktree?.worktreePath || "—"}
              </p>
            </div>
            {worktree?.present ? (
              <div className="rounded border border-[var(--border)] bg-[var(--bg)] p-2">
                <div className="mb-1 flex items-center justify-between gap-2">
                  <span className="text-[10px] uppercase tracking-wide text-[var(--text-muted)]">
                    Jump command
                  </span>
                  <CopyButton text={jumpCmd} />
                </div>
                <p className="m-0 break-all mono text-[11px]">{jumpCmd}</p>
              </div>
            ) : null}
            {offersDiscard ? (
              <p className="m-0 text-[11px] text-[var(--text-muted)]">
                After fixing or deciding to drop changes: use Discard &amp;
                Retry, or run{" "}
                <span className="mono">{discardCli}</span>
              </p>
            ) : (
              <p className="m-0 text-[11px] text-[var(--text-muted)]">
                Discard is unavailable for this worktree from Dashboard. Clean or
                repair the path yourself, then Retry without discard if
                appropriate.
              </p>
            )}
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
            <p className="m-0 text-[var(--text-muted)]">
              {takeoverResult.message ||
                (takeoverResult.supported
                  ? "Loop parked. Use the resume command in the worktree."
                  : "Loop parked. Interactive resume is not supported for this agent/session.")}
            </p>
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
          </div>
        </ConfirmDialog>
      ) : null}
    </>
  );
}
