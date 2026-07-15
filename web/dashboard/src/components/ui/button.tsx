import type { ButtonHTMLAttributes } from "react";

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "default" | "ghost" | "danger";
  size?: "sm" | "md";
};

const variants: Record<NonNullable<ButtonProps["variant"]>, string> = {
  default:
    "bg-[var(--accent)] text-[var(--accent-fg)] border-[var(--accent)] hover:opacity-90",
  ghost:
    "bg-transparent text-[var(--text)] border-[var(--border)] hover:bg-[var(--bg-muted)]",
  danger:
    "bg-transparent text-[var(--danger)] border-[var(--danger)] hover:bg-[var(--bg-muted)]",
};

const sizes: Record<NonNullable<ButtonProps["size"]>, string> = {
  sm: "px-2 py-0.5 text-[12px]",
  md: "px-2.5 py-1 text-[13px]",
};

export function Button({
  variant = "default",
  size = "sm",
  className = "",
  type = "button",
  ...rest
}: ButtonProps) {
  return (
    <button
      type={type}
      className={`inline-flex items-center justify-center rounded border font-medium disabled:opacity-50 ${variants[variant]} ${sizes[size]} ${className}`}
      {...rest}
    />
  );
}
