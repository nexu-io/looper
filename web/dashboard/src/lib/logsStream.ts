/** Backoff delays for unexpected SSE disconnects (ms). */
export const RECONNECT_BACKOFF_MS = [1000, 2000, 5000] as const;

export type LogsStreamPhase = "idle" | "connecting" | "live";

export type LogsStreamStatus =
  | "idle"
  | "connecting"
  | "live"
  | "ended"
  | "error";

/**
 * UI status for the logs pane.
 * "connecting" wins over retained buffer text so reconnect never shows "live"
 * until a successful snapshot/chunk arrives on the new connection.
 */
export function resolveLogsStreamStatus(opts: {
  phase: LogsStreamPhase;
  ended: boolean;
  error: string | null;
}): LogsStreamStatus {
  if (opts.error) return "error";
  if (opts.ended) return "ended";
  if (opts.phase === "connecting") return "connecting";
  if (opts.phase === "live") return "live";
  return "idle";
}

/** Bounded reconnect delay for attempt index 0, 1, 2, ... */
export function nextReconnectDelayMs(
  attempt: number,
  delays: readonly number[] = RECONNECT_BACKOFF_MS,
): number {
  if (delays.length === 0) return 0;
  const idx = Math.max(0, Math.min(attempt, delays.length - 1));
  return delays[idx] ?? delays[delays.length - 1]!;
}
