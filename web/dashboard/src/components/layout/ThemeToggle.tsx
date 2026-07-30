import { Monitor, Moon, Sun } from "lucide-react";
import { useTheme, type ThemeMode } from "@/lib/theme";

const OPTIONS: { mode: ThemeMode; label: string; Icon: typeof Sun }[] = [
  { mode: "light", label: "Light", Icon: Sun },
  { mode: "dark", label: "Dark", Icon: Moon },
  { mode: "system", label: "System", Icon: Monitor },
];

/**
 * Segmented tri-state theme control for the header. Matches the density of
 * neighboring header controls (Project select). Uses `aria-pressed` to expose
 * the active mode; label text lives in `aria-label`/`title`, icons are decorative.
 */
export function ThemeToggle() {
  const { mode, setMode } = useTheme();
  return (
    <div
      role="group"
      aria-label="Theme"
      className="inline-flex items-center rounded border border-[var(--border)] bg-[var(--bg)] p-[1px]"
    >
      {OPTIONS.map(({ mode: value, label, Icon }) => {
        const active = mode === value;
        return (
          <button
            key={value}
            type="button"
            aria-label={label}
            aria-pressed={active}
            title={label}
            onClick={() => setMode(value)}
            className={[
              "inline-flex h-5 w-6 items-center justify-center rounded-[3px]",
              "transition-colors focus-visible:outline focus-visible:outline-2",
              "focus-visible:outline-offset-1 focus-visible:outline-[var(--accent)]",
              active
                ? "bg-[var(--bg-muted)] text-[var(--text)]"
                : "text-[var(--text-muted)] hover:text-[var(--text)]",
            ].join(" ")}
          >
            <Icon size={13} className="shrink-0" aria-hidden />
          </button>
        );
      })}
    </div>
  );
}
