"use client";

import { forwardRef, useId } from "react";
import { cn } from "@/lib/cn";
import { Icon } from "./icons";

const CONTROL =
  "w-full rounded-md border border-surface-border-strong bg-surface text-sm text-ink " +
  "placeholder:text-ink-faint transition-colors hover:border-ink-faint/60 " +
  "focus:border-accent focus:ring-0 disabled:cursor-not-allowed disabled:opacity-50 " +
  "aria-[invalid=true]:border-danger";

export const Input = forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement>>(
  function Input({ className, ...props }, ref) {
    return <input ref={ref} {...props} className={cn(CONTROL, "h-8 px-2.5", className)} />;
  },
);

export const Textarea = forwardRef<HTMLTextAreaElement, React.TextareaHTMLAttributes<HTMLTextAreaElement>>(
  function Textarea({ className, ...props }, ref) {
    return <textarea ref={ref} {...props} className={cn(CONTROL, "min-h-20 px-2.5 py-2", className)} />;
  },
);

export const Select = forwardRef<HTMLSelectElement, React.SelectHTMLAttributes<HTMLSelectElement>>(
  function Select({ className, children, ...props }, ref) {
    return (
      <select ref={ref} {...props} className={cn(CONTROL, "h-8 appearance-none pl-2.5 pr-8", className)}>
        {children}
      </select>
    );
  },
);

export const SearchInput = forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement>>(
  function SearchInput({ className, ...props }, ref) {
    return (
      <div className={cn("relative", className)}>
        <Icon
          name="search"
          size={14}
          className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-faint"
        />
        <Input ref={ref} type="search" {...props} className="pl-8" />
      </div>
    );
  },
);

/**
 * A labelled control. The label is above, the hint below, and an error replaces
 * the hint rather than stacking under it — a form that grows taller as it fails
 * is a form whose submit button walks away from the cursor.
 */
export function Field({
  label,
  hint,
  error,
  required,
  htmlFor,
  className,
  children,
}: {
  label: string;
  hint?: React.ReactNode;
  error?: string;
  required?: boolean;
  htmlFor?: string;
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div className={cn("space-y-1.5", className)}>
      <label htmlFor={htmlFor} className="block text-sm font-medium text-ink">
        {label}
        {required && <span className="ml-0.5 text-danger" aria-hidden>*</span>}
      </label>
      {children}
      {error ? (
        <p className="text-xs text-danger" role="alert">{error}</p>
      ) : hint ? (
        <p className="text-xs text-ink-faint">{hint}</p>
      ) : null}
    </div>
  );
}

export function Checkbox({
  label,
  description,
  className,
  id: givenId,
  ...props
}: React.InputHTMLAttributes<HTMLInputElement> & { label?: React.ReactNode; description?: React.ReactNode }) {
  const generated = useId();
  const id = givenId ?? generated;
  return (
    <label htmlFor={id} className={cn("flex cursor-pointer items-start gap-2.5 text-sm", className)}>
      <input
        id={id}
        type="checkbox"
        {...props}
        className="mt-0.5 h-4 w-4 shrink-0 rounded border-surface-border-strong bg-surface"
      />
      {(label || description) && (
        <span className="min-w-0">
          {label && <span className="block text-ink">{label}</span>}
          {description && <span className="block text-xs text-ink-faint">{description}</span>}
        </span>
      )}
    </label>
  );
}

/**
 * An on/off control for a setting that takes effect when saved. It is a button
 * with role="switch", so it reads correctly to assistive technology and does
 * not submit a form when toggled with the keyboard.
 */
export function Switch({
  checked,
  onChange,
  label,
  description,
  disabled,
  className,
}: {
  checked: boolean;
  onChange: (next: boolean) => void;
  label?: React.ReactNode;
  description?: React.ReactNode;
  disabled?: boolean;
  className?: string;
}) {
  const id = useId();
  return (
    <div className={cn("flex items-start justify-between gap-4", className)}>
      {(label || description) && (
        <label htmlFor={id} className="min-w-0 cursor-pointer text-sm">
          {label && <span className="block text-ink">{label}</span>}
          {description && <span className="block text-xs text-ink-faint">{description}</span>}
        </label>
      )}
      <button
        id={id}
        type="button"
        role="switch"
        aria-checked={checked}
        disabled={disabled}
        onClick={() => onChange(!checked)}
        className={cn(
          "relative mt-0.5 h-5 w-9 shrink-0 rounded-full border transition-colors disabled:cursor-not-allowed disabled:opacity-50",
          checked ? "border-accent bg-accent" : "border-surface-border-strong bg-surface-hover",
        )}
      >
        <span
          className={cn(
            "absolute top-0.5 h-3.5 w-3.5 rounded-full bg-white shadow transition-transform",
            checked ? "translate-x-[18px]" : "translate-x-0.5",
          )}
        />
      </button>
    </div>
  );
}

/**
 * A set of mutually exclusive options that fits on one line — status filters,
 * view modes. Rendered as a toolbar of pressed buttons, not radios, because
 * changing one takes effect immediately.
 */
export function Segmented<T extends string>({
  value,
  onChange,
  options,
  size = "md",
  "aria-label": ariaLabel,
}: {
  value: T;
  onChange: (next: T) => void;
  options: { value: T; label: React.ReactNode; count?: number }[];
  size?: "sm" | "md";
  "aria-label": string;
}) {
  return (
    <div
      role="group"
      aria-label={ariaLabel}
      className={cn(
        "inline-flex items-center rounded-md border border-surface-border bg-surface p-0.5",
        size === "sm" ? "h-7" : "h-8",
      )}
    >
      {options.map((option) => {
        const selected = option.value === value;
        return (
          <button
            key={option.value}
            type="button"
            aria-pressed={selected}
            onClick={() => onChange(option.value)}
            className={cn(
              "inline-flex h-full items-center gap-1.5 rounded px-2.5 font-medium transition-colors",
              size === "sm" ? "text-xs" : "text-sm",
              selected ? "bg-surface-hover text-ink shadow-sm" : "text-ink-muted hover:text-ink",
            )}
          >
            {option.label}
            {option.count !== undefined && (
              <span className={cn("tabular text-xs", selected ? "text-ink-muted" : "text-ink-faint")}>
                {option.count}
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
}
