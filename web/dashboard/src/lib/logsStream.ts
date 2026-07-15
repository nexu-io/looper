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

/**
 * Default follow stream tracks stdout unless stdout is empty (then stderr).
 * When snapshot stdout is non-empty, open a separate `stderr=1` follow so live
 * stderr after the snapshot is not dropped.
 */
export function needsSeparateStderrFollow(agent?: {
  stdout?: string | null;
} | null): boolean {
  if (!agent) return false;
  return Boolean(agent.stdout?.trim());
}

/**
 * Prefix the first live stderr chunk with the same section header used by the
 * snapshot seed when stderr was empty at connect time.
 */
export function formatLiveStderrChunk(
  content: string,
  sectionHeaderPresent: boolean,
): string {
  if (!content) return "";
  if (sectionHeaderPresent) return content;
  return `\n--- stderr ---\n${content}`;
}
