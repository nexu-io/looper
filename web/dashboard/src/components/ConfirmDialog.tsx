import { useEffect, useId, useRef, type ReactNode } from "react";
import { Button } from "@/components/ui/button";

export type ConfirmDialogProps = {
  open: boolean;
  title: string;
  /** Dense body: string or custom nodes (e.g. takeover copy fields). */
  children: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  /** When false, only the confirm/close button is shown (e.g. result dialogs). */
  showCancel?: boolean;
  /** danger styling for destructive actions */
  danger?: boolean;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
};

/**
 * Minimal hand-rolled confirm modal (dense operator UI).
 */
export function ConfirmDialog({
  open,
  title,
  children,
  confirmLabel = "Confirm",
  cancelLabel = "Cancel",
  showCancel = true,
  danger = false,
  busy = false,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  const titleId = useId();
  const panelRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !busy) {
        onCancel();
      }
    };
    document.addEventListener("keydown", onKey);
    // Focus panel for a11y
    panelRef.current?.focus();
    return () => document.removeEventListener("keydown", onKey);
  }, [open, busy, onCancel]);

  if (!open) return null;

  return (
    <div className="fixed inset-0 z-40 flex items-center justify-center p-3">
      <button
        type="button"
        className="absolute inset-0 border-0 bg-black/40 p-0"
        aria-label="Close dialog"
        disabled={busy}
        onClick={() => {
          if (!busy) onCancel();
        }}
      />
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        className="relative z-10 w-full max-w-md rounded border border-[var(--border)] bg-[var(--bg-elevated)] shadow-lg outline-none"
      >
        <header className="border-b border-[var(--border)] px-3 py-2">
          <h2 id={titleId} className="m-0 text-[13px] font-semibold">
            {title}
          </h2>
        </header>
        <div className="px-3 py-2 text-[12px] text-[var(--text)]">
          {children}
        </div>
        <footer className="flex justify-end gap-1.5 border-t border-[var(--border)] px-3 py-2">
          {showCancel ? (
            <Button
              variant="ghost"
              size="sm"
              onClick={onCancel}
              disabled={busy}
            >
              {cancelLabel}
            </Button>
          ) : null}
          <Button
            variant={danger ? "danger" : "default"}
            size="sm"
            onClick={onConfirm}
            disabled={busy}
          >
            {busy ? "…" : confirmLabel}
          </Button>
        </footer>
      </div>
    </div>
  );
}
