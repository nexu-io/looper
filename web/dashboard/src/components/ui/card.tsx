import type { HTMLAttributes, ReactNode } from "react";

type CardProps = HTMLAttributes<HTMLDivElement> & {
  title?: string;
  actions?: ReactNode;
  children: ReactNode;
};

export function Card({ title, actions, children, className = "", ...rest }: CardProps) {
  return (
    <section
      className={`rounded border border-[var(--border)] bg-[var(--bg-elevated)] ${className}`}
      {...rest}
    >
      {(title || actions) && (
        <header className="flex items-center justify-between gap-2 border-b border-[var(--border)] px-3 py-1.5">
          {title ? (
            <h2 className="m-0 text-[12px] font-semibold tracking-wide uppercase text-[var(--text-muted)]">
              {title}
            </h2>
          ) : (
            <span />
          )}
          {actions}
        </header>
      )}
      <div className="px-3 py-2">{children}</div>
    </section>
  );
}
