import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { Button } from "@/components/ui/button";

export type ToastKind = "ok" | "err" | "info";

export type ToastItem = {
  id: number;
  kind: ToastKind;
  message: string;
};

type ToastContextValue = {
  push: (kind: ToastKind, message: string) => void;
  success: (message: string) => void;
  error: (message: string) => void;
  info: (message: string) => void;
};

const ToastContext = createContext<ToastContextValue | null>(null);

let nextId = 1;

const KIND_STYLE: Record<ToastKind, string> = {
  ok: "border-[var(--ok)] text-[var(--ok)]",
  err: "border-[var(--danger)] text-[var(--danger)]",
  info: "border-[var(--border)] text-[var(--text)]",
};

const AUTO_DISMISS_MS = 5000;

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);

  const dismiss = useCallback((id: number) => {
    setItems((prev) => prev.filter((t) => t.id !== id));
  }, []);

  const push = useCallback(
    (kind: ToastKind, message: string) => {
      const id = nextId++;
      setItems((prev) => [...prev.slice(-4), { id, kind, message }]);
      window.setTimeout(() => dismiss(id), AUTO_DISMISS_MS);
    },
    [dismiss],
  );

  const value = useMemo<ToastContextValue>(
    () => ({
      push,
      success: (m) => push("ok", m),
      error: (m) => push("err", m),
      info: (m) => push("info", m),
    }),
    [push],
  );

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div
        className="pointer-events-none fixed bottom-3 right-3 z-50 flex w-[min(360px,calc(100vw-1.5rem))] flex-col gap-1.5"
        aria-live="polite"
      >
        {items.map((t) => (
          <div
            key={t.id}
            className={`pointer-events-auto flex items-start gap-2 rounded border bg-[var(--bg-elevated)] px-2.5 py-1.5 text-[12px] shadow-sm ${KIND_STYLE[t.kind]}`}
          >
            <span className="min-w-0 flex-1 break-words">{t.message}</span>
            <Button
              variant="ghost"
              size="sm"
              className="shrink-0 px-1 py-0 text-[10px]"
              onClick={() => dismiss(t.id)}
              aria-label="Dismiss"
            >
              ×
            </Button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext);
  if (!ctx) {
    throw new Error("useToast must be used within ToastProvider");
  }
  return ctx;
}
