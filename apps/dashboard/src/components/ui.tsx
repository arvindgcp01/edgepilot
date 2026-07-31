"use client";

/** The small set of primitives every view is built from. */
import { cn } from "@/lib/cn";
import type { DeviceStatus } from "@/lib/api/devices";

export function Card({ className, children }: { className?: string; children: React.ReactNode }) {
  return (
    <div className={cn("rounded-lg border border-surface-border bg-surface-raised p-4", className)}>
      {children}
    </div>
  );
}

export function Button({
  variant = "default",
  className,
  ...props
}: React.ButtonHTMLAttributes<HTMLButtonElement> & { variant?: "default" | "ghost" | "danger" }) {
  return (
    <button
      {...props}
      className={cn(
        "inline-flex items-center justify-center gap-2 rounded-md px-3 py-1.5 text-sm font-medium",
        "transition-colors disabled:cursor-not-allowed disabled:opacity-50",
        variant === "default" && "bg-pending/15 text-pending hover:bg-pending/25",
        variant === "ghost" && "text-ink-muted hover:bg-surface-border hover:text-ink",
        variant === "danger" && "bg-danger/15 text-danger hover:bg-danger/25",
        className,
      )}
    />
  );
}

export function Input({ className, ...props }: React.InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      {...props}
      className={cn(
        "w-full rounded-md border border-surface-border bg-surface px-3 py-2 text-sm",
        "placeholder:text-ink-faint focus:border-pending",
        className,
      )}
    />
  );
}

const STATUS_STYLE: Record<DeviceStatus, string> = {
  online: "bg-online/15 text-online",
  offline: "bg-offline/20 text-offline",
  degraded: "bg-degraded/15 text-degraded",
  pending: "bg-pending/15 text-pending",
  decommissioned: "bg-offline/20 text-offline line-through",
};

export function StatusBadge({ status }: { status: DeviceStatus }) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium",
        STATUS_STYLE[status] ?? STATUS_STYLE.offline,
      )}
    >
      <span className="h-1.5 w-1.5 rounded-full bg-current" aria-hidden />
      {status}
    </span>
  );
}

/** An error that is worth reading. Problem Details carry a detail and a request
 *  id; showing both is what makes a support conversation short. */
export function ErrorNote({ error }: { error: unknown }) {
  if (!error) return null;
  const message = error instanceof Error ? error.message : String(error);
  const requestID =
    typeof error === "object" && error !== null && "problem" in error
      ? (error as { problem?: { request_id?: string } }).problem?.request_id
      : undefined;

  return (
    <div role="alert" className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
      {message}
      {requestID && (
        <span className="ml-2 font-mono text-xs text-ink-faint">request {requestID}</span>
      )}
    </div>
  );
}

export function Empty({ children }: { children: React.ReactNode }) {
  return <div className="py-12 text-center text-sm text-ink-muted">{children}</div>;
}

export function Spinner() {
  return (
    <div className="flex justify-center py-12" role="status" aria-label="Loading">
      <div className="h-5 w-5 animate-spin rounded-full border-2 border-surface-border border-t-pending" />
    </div>
  );
}
