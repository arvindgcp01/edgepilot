import { cn } from "@/lib/cn";
import type { DeviceStatus } from "@/lib/api/devices";

export type BadgeTone = "neutral" | "info" | "success" | "warning" | "danger" | "accent";

/**
 * Badges are small, bordered, and tinted at ten percent. The colour carries the
 * meaning; the fill is only there so the shape reads as a badge and not as a
 * word that happens to be green. A full-saturation pill is louder than the data
 * around it, and the data is what the page is for.
 */
const TONE: Record<BadgeTone, string> = {
  neutral: "border-surface-border bg-surface-hover text-ink-muted",
  info: "border-pending/20 bg-pending/10 text-pending",
  success: "border-online/20 bg-online/10 text-online",
  warning: "border-degraded/20 bg-degraded/10 text-degraded",
  danger: "border-danger/20 bg-danger/10 text-danger",
  accent: "border-accent/20 bg-accent/10 text-accent-soft",
};

export function Badge({
  tone = "neutral",
  dot,
  className,
  children,
  ...props
}: React.HTMLAttributes<HTMLSpanElement> & { tone?: BadgeTone; dot?: boolean }) {
  return (
    <span
      {...props}
      className={cn(
        "inline-flex h-[22px] items-center gap-1.5 whitespace-nowrap rounded-full border px-2 text-xs font-medium leading-none",
        TONE[tone],
        className,
      )}
    >
      {dot && <span className="h-1.5 w-1.5 rounded-full bg-current" aria-hidden />}
      {children}
    </span>
  );
}

const DEVICE_TONE: Record<DeviceStatus, BadgeTone> = {
  online: "success",
  offline: "neutral",
  degraded: "warning",
  pending: "info",
  decommissioned: "neutral",
};

const DEVICE_LABEL: Record<DeviceStatus, string> = {
  online: "Online",
  offline: "Offline",
  degraded: "Degraded",
  pending: "Pending",
  decommissioned: "Decommissioned",
};

export function StatusBadge({ status, className }: { status: DeviceStatus; className?: string }) {
  return (
    <Badge
      dot
      tone={DEVICE_TONE[status] ?? "neutral"}
      className={cn(status === "decommissioned" && "line-through", className)}
    >
      {DEVICE_LABEL[status] ?? status}
    </Badge>
  );
}

/** A short, monospaced token — an id, a slug, a permission. */
export function Code({ className, children }: { className?: string; children: React.ReactNode }) {
  return (
    <code
      className={cn(
        "rounded-md border border-surface-border bg-surface-sunken px-1.5 py-px font-mono text-xs text-ink-muted",
        className,
      )}
    >
      {children}
    </code>
  );
}

export function Kbd({ children }: { children: React.ReactNode }) {
  return (
    <kbd className="inline-flex h-5 min-w-5 items-center justify-center rounded border border-surface-border-strong bg-surface px-1 font-mono text-2xs text-ink-muted">
      {children}
    </kbd>
  );
}
