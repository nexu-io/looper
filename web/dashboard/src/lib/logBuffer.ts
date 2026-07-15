/** Rendered log buffer caps (after append). */
export const MAX_LOG_CHARS = 500_000;
export const MAX_LOG_LINES = 5000;

/** Cap snapshot seed before it becomes the buffer. */
export const MAX_SNAPSHOT_SEED_CHARS = 200_000;

/** Cap each chunk before append. */
export const MAX_CHUNK_CHARS = 64_000;

/** Cap raw SSE event data length before JSON parse / append. */
export const MAX_SSE_EVENT_DATA_CHARS = 256_000;

/** Keep the tail of oversized text (newest content). */
export function capFromEnd(text: string, maxChars: number): string {
  if (maxChars <= 0) return "";
  if (text.length <= maxChars) return text;
  let next = text.slice(text.length - maxChars);
  const nl = next.indexOf("\n");
  if (nl !== -1 && nl < 200) {
    next = next.slice(nl + 1);
  }
  return next;
}

export function capSSEEventData(data: string): string {
  return capFromEnd(data, MAX_SSE_EVENT_DATA_CHARS);
}

export function capLogSeed(text: string): string {
  return capFromEnd(text, MAX_SNAPSHOT_SEED_CHARS);
}

export function capLogChunk(text: string): string {
  return capFromEnd(text, MAX_CHUNK_CHARS);
}

/** Cap rendered buffer by chars and line count (tail kept). */
export function trimLogBuffer(text: string): string {
  let next = text;
  if (next.length > MAX_LOG_CHARS) {
    next = next.slice(next.length - MAX_LOG_CHARS);
    const nl = next.indexOf("\n");
    if (nl !== -1 && nl < 200) {
      next = next.slice(nl + 1);
    }
  }
  const lines = next.split("\n");
  if (lines.length > MAX_LOG_LINES) {
    next = lines.slice(lines.length - MAX_LOG_LINES).join("\n");
  }
  return next;
}
