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
 * Always open a dedicated `stderr=1` follow alongside the default stream.
 *
 * Default follow tracks stdout unless stdout is empty and stderr already has
 * content (then stderr). That choice is re-evaluated each poll, so an initially
 * stderr-only snapshot later switches to stdout and would drop subsequent
 * stderr appends without a secondary stream. Empty snapshots need the same
 * secondary follow so stderr that appears after stdout locks is not lost.
 */
export function needsSeparateStderrFollow(_agent?: {
  stdout?: string | null;
  stderr?: string | null;
} | null): boolean {
  return true;
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

/**
 * Bytes present only in the secondary stderr=1 snapshot relative to the
 * primary (default-follow) snapshot already seeded into the logs pane.
 *
 * The server baselines follow chunks from its own snapshot, so any stderr
 * written between the two snapshots would never appear as a chunk unless the
 * client applies this gap from the secondary snapshot event.
 *
 * Mirrors server appendedLogChunk suffix logic when content is append-only.
 */
export function stderrGapFromSecondarySnapshot(
  primaryStderr: string,
  secondaryStderr: string,
): string {
  if (!secondaryStderr) return "";
  if (!primaryStderr) return secondaryStderr;
  if (secondaryStderr === primaryStderr) return "";
  if (secondaryStderr.startsWith(primaryStderr)) {
    return secondaryStderr.slice(primaryStderr.length);
  }
  // Non-prefix rewrite (e.g. new execution): surface full secondary content.
  return secondaryStderr;
}
