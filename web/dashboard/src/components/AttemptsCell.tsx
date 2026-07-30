import { Infinity as InfinityIcon } from "lucide-react";

/**
 * Attempts cell for dense tables. Renders `current/max` and swaps the ∞
 * character for a real lucide Infinity glyph when max is unlimited (-1).
 * `formatAttempts` remains the source of truth for text contexts (Summary,
 * tests, tooltips).
 */
export function AttemptsCell({
  attempts,
  maxAttempts,
}: {
  attempts: number | null | undefined;
  maxAttempts: number | null | undefined;
}) {
  if (attempts == null || Number.isNaN(Number(attempts))) {
    return <span className="mono text-[var(--text-muted)]">—</span>;
  }
  const current = Math.trunc(Number(attempts));
  if (maxAttempts == null || Number.isNaN(Number(maxAttempts))) {
    return <span className="mono text-[var(--text-muted)]">{current}</span>;
  }
  const max = Math.trunc(Number(maxAttempts));
  return (
    <span className="mono inline-flex items-center gap-0.5 text-[var(--text-muted)]">
      <span>{current}/</span>
      {max < 0 ? (
        <InfinityIcon
          size={13}
          strokeWidth={2}
          aria-label="unlimited"
          className="shrink-0"
        />
      ) : (
        <span>{max}</span>
      )}
    </span>
  );
}
