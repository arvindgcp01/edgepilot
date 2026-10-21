"use client";

import { forwardRef } from "react";
import { cn } from "@/lib/cn";
import { Icon, type IconName } from "./icons";

export type ButtonVariant = "primary" | "secondary" | "ghost" | "danger" | "danger-solid";
export type ButtonSize = "sm" | "md";

/**
 * There is one primary action on a screen and it is the one solid button. Every
 * other action is secondary (bordered) or ghost (text). Destructive actions are
 * red, and the *solid* red is reserved for the confirming button inside a
 * dialog — the row-level "Remove" that opens the dialog is an outline, so the
 * page never shows a filled red control that acts on first click.
 */
const VARIANT: Record<ButtonVariant, string> = {
  primary:
    "bg-accent text-white hover:bg-accent-hover border border-transparent shadow-sm shadow-accent/20",
  secondary:
    "border border-surface-border-strong bg-surface-raised text-ink shadow-sm hover:bg-surface-hover",
  ghost: "border border-transparent text-ink-muted hover:bg-surface-hover hover:text-ink",
  danger: "border border-danger/30 bg-surface-raised text-danger hover:bg-danger/5 hover:border-danger/60",
  "danger-solid": "border border-transparent bg-danger text-white shadow-sm hover:bg-danger/90",
};

const SIZE: Record<ButtonSize, string> = {
  sm: "h-8 px-3 text-xs gap-1.5",
  md: "h-9 px-3.5 text-sm gap-2",
};

export type ButtonProps = React.ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: ButtonVariant;
  size?: ButtonSize;
  icon?: IconName;
  /** Shows a spinner and disables the control. The label stays visible so the
   *  button does not change width mid-click. */
  loading?: boolean;
};

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = "secondary", size = "md", icon, loading, className, children, disabled, type = "button", ...props },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      {...props}
      className={cn(
        "relative inline-flex select-none items-center justify-center whitespace-nowrap rounded-lg font-medium",
        "transition-all duration-150 active:scale-[0.98] disabled:cursor-not-allowed disabled:opacity-50",
        VARIANT[variant],
        SIZE[size],
        className,
      )}
    >
      {loading ? (
        <span
          className="h-3.5 w-3.5 animate-spin rounded-full border-[1.5px] border-current border-t-transparent"
          aria-hidden
        />
      ) : (
        icon && <Icon name={icon} size={size === "sm" ? 14 : 16} />
      )}
      {children}
    </button>
  );
});

/** A square button holding only an icon. The label is required, for the screen
 *  reader and the tooltip alike. */
export const IconButton = forwardRef<
  HTMLButtonElement,
  Omit<ButtonProps, "icon" | "children"> & { icon: IconName; label: string }
>(function IconButton({ icon, label, variant = "ghost", size = "md", className, ...props }, ref) {
  return (
    <Button
      ref={ref}
      variant={variant}
      size={size}
      aria-label={label}
      title={label}
      {...props}
      className={cn("px-0 shadow-none", size === "sm" ? "w-8" : "w-9", className)}
    >
      <Icon name={icon} size={size === "sm" ? 14 : 16} />
    </Button>
  );
});
