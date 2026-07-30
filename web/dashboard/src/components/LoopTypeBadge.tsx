// Distinct role badge for loop.type. Colors are restrained and reuse CSS
// vars where the role maps naturally to a semantic hue; other roles get a
// dedicated tint so they never fall back to the same neutral as unknown types.

type Palette = {
  bg: string;
  fg: string;
  border: string;
};

const PALETTES: Record<string, Palette> = {
  reviewer: {
    bg: "color-mix(in srgb, #a855f7 18%, transparent)",
    fg: "color-mix(in srgb, #a855f7 92%, var(--text))",
    border: "color-mix(in srgb, #a855f7 45%, var(--border))",
  },
  fixer: {
    bg: "color-mix(in srgb, var(--warn) 20%, transparent)",
    fg: "var(--warn)",
    border: "color-mix(in srgb, var(--warn) 55%, var(--border))",
  },
  worker: {
    bg: "color-mix(in srgb, var(--accent) 18%, transparent)",
    fg: "var(--accent)",
    border: "color-mix(in srgb, var(--accent) 50%, var(--border))",
  },
  planner: {
    bg: "color-mix(in srgb, var(--ok) 18%, transparent)",
    fg: "var(--ok)",
    border: "color-mix(in srgb, var(--ok) 50%, var(--border))",
  },
  coordinator: {
    bg: "color-mix(in srgb, #06b6d4 18%, transparent)",
    fg: "color-mix(in srgb, #06b6d4 90%, var(--text))",
    border: "color-mix(in srgb, #06b6d4 45%, var(--border))",
  },
};

const NEUTRAL: Palette = {
  bg: "var(--bg-muted)",
  fg: "var(--text)",
  border: "var(--border)",
};

function toLabel(raw: string): string {
  const t = raw.trim();
  if (!t) return "Unknown";
  return t.charAt(0).toUpperCase() + t.slice(1).toLowerCase();
}

export function LoopTypeBadge({
  type,
  size = "md",
}: {
  type: string | null | undefined;
  size?: "sm" | "md";
}) {
  const key = (type ?? "").trim().toLowerCase();
  const palette = PALETTES[key] ?? NEUTRAL;
  const label = toLabel(type ?? "");
  const sizing =
    size === "sm"
      ? "px-1.5 py-0 text-[10px]"
      : "px-2 py-[1px] text-[11px]";
  return (
    <span
      className={`inline-flex items-center rounded border font-semibold uppercase tracking-wide leading-tight ${sizing}`}
      style={{
        backgroundColor: palette.bg,
        color: palette.fg,
        borderColor: palette.border,
      }}
    >
      {label}
    </span>
  );
}
