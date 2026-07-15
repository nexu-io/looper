import { statusColor } from "@/lib/format";

export function StatusChip({ status }: { status: string | null | undefined }) {
  const label = status?.trim() || "—";
  return (
    <span
      className="inline-flex items-center rounded border border-[var(--border)] px-1.5 py-0 mono text-[11px] leading-tight"
      style={{ color: statusColor(label) }}
    >
      {label}
    </span>
  );
}
