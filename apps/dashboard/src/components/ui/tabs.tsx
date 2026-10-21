"use client";

import { cn } from "@/lib/cn";

/**
 * Underlined tabs. The selected tab is marked by a 2px line on the same colour
 * as the primary action, and nothing else changes — no fill, no pill. A tab
 * strip is navigation, and navigation should not compete with the content it
 * reveals.
 */
export function Tabs<T extends string>({
  value,
  onChange,
  tabs,
  className,
  "aria-label": ariaLabel,
}: {
  value: T;
  onChange: (next: T) => void;
  tabs: { value: T; label: React.ReactNode; count?: number }[];
  className?: string;
  "aria-label": string;
}) {
  return (
    <div
      role="tablist"
      aria-label={ariaLabel}
      className={cn("flex gap-1 overflow-x-auto border-b border-surface-border", className)}
    >
      {tabs.map((tab) => {
        const selected = tab.value === value;
        return (
          <button
            key={tab.value}
            type="button"
            role="tab"
            aria-selected={selected}
            onClick={() => onChange(tab.value)}
            className={cn(
              "-mb-px inline-flex h-9 items-center gap-2 whitespace-nowrap border-b-2 px-3 text-sm font-medium transition-colors",
              selected
                ? "border-accent text-ink"
                : "border-transparent text-ink-muted hover:border-surface-border-strong hover:text-ink",
            )}
          >
            {tab.label}
            {tab.count !== undefined && (
              <span className="rounded bg-surface-hover px-1.5 py-px text-xs tabular text-ink-muted">
                {tab.count}
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
}
