/** Compact relative age from ISO timestamp (e.g. 12s, 3m, 2h, 1d). */
export function formatAge(iso: string | null | undefined, nowMs = Date.now()): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "—";
  const sec = Math.max(0, Math.floor((nowMs - t) / 1000));
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m`;
  const hr = Math.floor(min / 60);
  if (hr < 48) return `${hr}h`;
  const day = Math.floor(hr / 24);
  return `${day}d`;
}

export function formatTs(iso: string | null | undefined): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  try {
    return new Date(t).toLocaleString(undefined, {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
    });
  } catch {
    return iso;
  }
}

/**
 * Format attempt count as current/max (e.g. "2/5", "1/-1" for unlimited).
 * Returns null when attempt metadata is absent so callers can hide clutter.
 */
export function formatAttempts(
  attempts: number | null | undefined,
  maxAttempts: number | null | undefined,
): string | null {
  if (attempts == null || Number.isNaN(Number(attempts))) {
    return null;
  }
  const current = Math.trunc(Number(attempts));
  if (maxAttempts == null || Number.isNaN(Number(maxAttempts))) {
    return String(current);
  }
  return `${current}/${Math.trunc(Number(maxAttempts))}`;
}

/**
 * Collapse whitespace and truncate for dense list rows. Full text stays in title/tooltip.
 */
export function truncateReason(
  value: string | null | undefined,
  max = 64,
): string | null {
  if (value == null) return null;
  const collapsed = value.replace(/\s+/g, " ").trim();
  if (!collapsed) return null;
  if (max <= 0) return "";
  const runes = Array.from(collapsed);
  if (runes.length <= max) return collapsed;
  if (max <= 3) return runes.slice(0, max).join("");
  return `${runes.slice(0, max - 3).join("")}...`;
}

export function statusColor(status: string | null | undefined): string {
  const s = (status ?? "").toLowerCase();
  if (
    s === "running" ||
    s === "active" ||
    s === "healthy" ||
    s === "ok" ||
    s === "completed" ||
    s === "success"
  ) {
    return "var(--ok)";
  }
  if (
    s === "failed" ||
    s === "error" ||
    s === "stopped" ||
    s === "terminated" ||
    s === "unhealthy"
  ) {
    return "var(--danger)";
  }
  if (
    s === "paused" ||
    s === "waiting" ||
    s === "queued" ||
    s === "backing_off" ||
    s === "manual_intervention" ||
    s.includes("manual") ||
    s.includes("backoff")
  ) {
    return "var(--warn)";
  }
  return "var(--text-muted)";
}
