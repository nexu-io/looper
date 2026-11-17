import type { ReactNode } from "react";

/**
 * External forge link (PR, repo, …) styled for dense mono tables.
 * - At rest reads as a link (accent color + subtle underline + external cue).
 * - Hover/focus strengthens the affordance without breaking row density.
 * - Callers pass a resolved `href`; render plain text upstream when none.
 */
export function PullRequestLink({
  href,
  children,
  title,
}: {
  href: string;
  children: ReactNode;
  title?: string;
}) {
  // Underline lives on the label span (not the flex <a>): global `a {
  // text-decoration: none }` plus inline-flex both suppress decoration on the
  // anchor itself. border-b is the reliable affordance here.
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      title={title ?? href}
      onClick={(e) => e.stopPropagation()}
      onKeyDown={(e) => {
        // DataTable rows handle Enter/Space for navigation; keep those keys on
        // the forge link so keyboard users can activate the external href.
        if (e.key === "Enter" || e.key === " ") {
          e.stopPropagation();
        }
      }}
      className={[
        "mono group inline-flex max-w-full items-baseline gap-1",
        "text-[var(--accent)]",
        "focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--accent)] rounded-[2px]",
      ].join(" ")}
    >
      <span className="truncate border-b border-[color-mix(in_srgb,var(--accent)_40%,transparent)] transition-[border-color] group-hover:border-[var(--accent)] group-focus-visible:border-[var(--accent)]">
        {children}
      </span>
      <span
        aria-hidden="true"
        className="shrink-0 text-[0.85em] opacity-60 transition-[opacity,transform] group-hover:opacity-100 group-hover:-translate-y-px group-focus-visible:opacity-100"
      >
        ↗
      </span>
    </a>
  );
}
