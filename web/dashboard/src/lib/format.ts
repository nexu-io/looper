/** Compact relative age from ISO timestamp (e.g. 12s, 3m, 2h 3m, 1d 4h). */
export function formatAge(iso: string | null | undefined, nowMs = Date.now()): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "—";
  const sec = Math.max(0, Math.floor((nowMs - t) / 1000));
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m`;
  const hr = Math.floor(min / 60);
  if (hr < 48) {
    const remMin = min % 60;
    return remMin > 0 ? `${hr}h ${remMin}m` : `${hr}h`;
  }
  const day = Math.floor(hr / 24);
  const remHr = hr % 24;
  return remHr > 0 ? `${day}d ${remHr}h` : `${day}d`;
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
 * Format attempt count as current/max (e.g. "2/5", "1/∞" for unlimited).
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
  const max = Math.trunc(Number(maxAttempts));
  // -1 means unlimited in queue/loop policy.
  return `${current}/${max < 0 ? "∞" : max}`;
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

/** owner/name slug, or null when not a plain GitHub-style repo id. */
function ownerRepoSlug(repo: string | null | undefined): string | null {
  if (repo == null) return null;
  const trimmed = repo.trim();
  if (!trimmed) return null;
  // owner/name — reject URLs or bare names
  if (!/^[^/\s]+\/[^/\s]+$/.test(trimmed)) return null;
  return trimmed;
}

function normalizeForgeBase(base: string): string {
  return base.trim().replace(/\/+$/, "");
}

/**
 * Prefer server-resolved project.repoUrl (correct Forgejo/GitHub host).
 * Falls back to github.com only when no server URL is available.
 */
export function repositoryUrl(
  repo: string | null | undefined,
  repoUrl?: string | null,
): string | null {
  const fromServer = typeof repoUrl === "string" ? repoUrl.trim() : "";
  if (fromServer) return fromServer;
  const slug = ownerRepoSlug(repo);
  if (!slug) return null;
  return `https://github.com/${slug}`;
}

/**
 * PR HTML URL. Prefer project.repoUrl + provider kind:
 * - github:  {repoUrl}/pull/{n}
 * - forgejo: {repoUrl}/pulls/{n}
 * Falls back to github.com when only owner/repo is known.
 */
export function pullRequestUrl(
  repo: string | null | undefined,
  prNumber: number | null | undefined,
  opts?: { repoUrl?: string | null; provider?: string | null },
): string | null {
  if (prNumber == null || !Number.isFinite(prNumber) || prNumber <= 0) {
    return null;
  }
  const n = Math.trunc(prNumber);
  const fromServer =
    typeof opts?.repoUrl === "string" ? normalizeForgeBase(opts.repoUrl) : "";
  if (fromServer) {
    const kind = (opts?.provider ?? "").toLowerCase();
    const segment = kind === "forgejo" ? "pulls" : "pull";
    return `${fromServer}/${segment}/${n}`;
  }
  const slug = ownerRepoSlug(repo);
  if (!slug) return null;
  return `https://github.com/${slug}/pull/${n}`;
}

/**
 * Issue HTML URL. GitHub and Forgejo both expose `/issues/{n}`.
 * Prefer project.repoUrl (correct host for self-hosted forges); fall back to
 * github.com when only owner/repo is known.
 */
export function issueUrl(
  repo: string | null | undefined,
  issueNumber: number | null | undefined,
  opts?: { repoUrl?: string | null },
): string | null {
  if (issueNumber == null || !Number.isFinite(issueNumber) || issueNumber <= 0) {
    return null;
  }
  const n = Math.trunc(issueNumber);
  const fromServer =
    typeof opts?.repoUrl === "string" ? normalizeForgeBase(opts.repoUrl) : "";
  if (fromServer) {
    return `${fromServer}/issues/${n}`;
  }
  const slug = ownerRepoSlug(repo);
  if (!slug) return null;
  return `https://github.com/${slug}/issues/${n}`;
}

/**
 * Parse a loop's stored issue target key. Worker loops targeting an issue
 * persist targetId as `issue:{owner/repo}:{n}` (see requeue_guard.go).
 * Returns null when the value is missing or not in that shape.
 */
export function parseIssueTargetId(
  targetId: string | null | undefined,
): { repo: string; issueNumber: number } | null {
  if (!targetId) return null;
  const trimmed = targetId.trim();
  if (!trimmed.startsWith("issue:")) return null;
  const rest = trimmed.slice("issue:".length);
  // repo may contain a single "/", issue number is the last ":" segment.
  const lastColon = rest.lastIndexOf(":");
  if (lastColon <= 0) return null;
  const repo = rest.slice(0, lastColon).trim();
  const nStr = rest.slice(lastColon + 1).trim();
  if (!repo.includes("/")) return null;
  const n = Number(nStr);
  if (!Number.isFinite(n) || n <= 0) return null;
  return { repo, issueNumber: Math.trunc(n) };
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
