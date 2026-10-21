"use client";

import { createContext, useCallback, useContext, useMemo, useRef, useState } from "react";
import { cn } from "@/lib/cn";
import { Icon, type IconName } from "./icons";
import { Button } from "./button";

// ---------------------------------------------------------------------------
// Toasts
// ---------------------------------------------------------------------------

export type ToastTone = "neutral" | "success" | "error" | "warning";

export type Toast = {
  id: number;
  title: string;
  description?: string;
  tone: ToastTone;
  action?: { label: string; onClick: () => void };
};

type ToastInput = Omit<Toast, "id" | "tone"> & { tone?: ToastTone; duration?: number };

const ToastContext = createContext<{ push: (toast: ToastInput) => void } | null>(null);

/**
 * Toasts confirm what just happened. They are for outcomes the operator caused
 * a moment ago — saved, invited, revoked — and never for state they need to act
 * on, which belongs in the page. Errors stay until dismissed; successes leave.
 */
export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const counter = useRef(0);

  const dismiss = useCallback((id: number) => {
    setToasts((current) => current.filter((t) => t.id !== id));
  }, []);

  const push = useCallback(
    (input: ToastInput) => {
      const id = ++counter.current;
      const tone = input.tone ?? "neutral";
      setToasts((current) => [...current.slice(-3), { ...input, id, tone }]);
      const duration = input.duration ?? (tone === "error" ? 0 : 4500);
      if (duration > 0) setTimeout(() => dismiss(id), duration);
    },
    [dismiss],
  );

  const value = useMemo(() => ({ push }), [push]);

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div
        className="pointer-events-none fixed bottom-4 right-4 z-[60] flex w-full max-w-sm flex-col gap-2"
        aria-live="polite"
        aria-relevant="additions"
      >
        {toasts.map((toast) => (
          <ToastItem key={toast.id} toast={toast} onDismiss={() => dismiss(toast.id)} />
        ))}
      </div>
    </ToastContext.Provider>
  );
}

const TOAST_ICON: Record<ToastTone, { name: IconName; className: string }> = {
  neutral: { name: "info", className: "text-ink-muted" },
  success: { name: "check-circle", className: "text-online" },
  error: { name: "x-circle", className: "text-danger" },
  warning: { name: "warning", className: "text-degraded" },
};

function ToastItem({ toast, onDismiss }: { toast: Toast; onDismiss: () => void }) {
  const icon = TOAST_ICON[toast.tone];
  return (
    <div
      role={toast.tone === "error" ? "alert" : "status"}
      className="pointer-events-auto flex animate-slide-up items-start gap-3 rounded-lg border border-surface-border bg-surface-overlay p-3 shadow-overlay"
    >
      <Icon name={icon.name} className={cn("mt-0.5", icon.className)} />
      <div className="min-w-0 flex-1">
        <div className="text-sm font-medium text-ink">{toast.title}</div>
        {toast.description && <div className="mt-0.5 text-xs text-ink-muted">{toast.description}</div>}
        {toast.action && (
          <button
            type="button"
            onClick={() => {
              toast.action?.onClick();
              onDismiss();
            }}
            className="mt-1.5 text-xs font-medium text-accent-soft hover:underline"
          >
            {toast.action.label}
          </button>
        )}
      </div>
      <button
        type="button"
        onClick={onDismiss}
        aria-label="Dismiss"
        className="-m-1 rounded p-1 text-ink-faint hover:bg-surface-hover hover:text-ink"
      >
        <Icon name="x" size={14} />
      </button>
    </div>
  );
}

export function useToast() {
  const context = useContext(ToastContext);
  if (!context) throw new Error("useToast must be used inside <ToastProvider>");
  return context;
}

// ---------------------------------------------------------------------------
// Loading, empty, error
// ---------------------------------------------------------------------------

/** A placeholder the shape of the content it is standing in for. Skeletons are
 *  for the first load of a view; a refetch keeps the stale rows visible. */
export function Skeleton({ className }: { className?: string }) {
  return (
    <div
      aria-hidden
      className={cn(
        "animate-shimmer rounded bg-[linear-gradient(90deg,#10151e_25%,#161d29_50%,#10151e_75%)] bg-[length:200%_100%]",
        className,
      )}
    />
  );
}

export function TableSkeleton({ rows = 6, columns = 5 }: { rows?: number; columns?: number }) {
  return (
    <div role="status" aria-label="Loading" className="divide-y divide-surface-border">
      {Array.from({ length: rows }).map((_, r) => (
        <div key={r} className="flex items-center gap-4 px-4 py-3">
          {Array.from({ length: columns }).map((_, c) => (
            <Skeleton
              key={c}
              className={cn("h-3.5", c === 0 ? "w-48" : c === columns - 1 ? "ml-auto w-16" : "w-24")}
            />
          ))}
        </div>
      ))}
    </div>
  );
}

/**
 * Nothing here, and what to do about it. One sentence of explanation, one
 * action. No illustration: an empty fleet table does not need a drawing of an
 * empty box to make the point.
 */
export function EmptyState({
  icon = "inbox",
  title,
  description,
  action,
  className,
}: {
  icon?: IconName;
  title: string;
  description?: React.ReactNode;
  action?: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("flex flex-col items-center px-6 py-14 text-center", className)}>
      <div className="mb-3 flex h-9 w-9 items-center justify-center rounded-md border border-surface-border bg-surface text-ink-faint">
        <Icon name={icon} />
      </div>
      <div className="text-sm font-medium text-ink">{title}</div>
      {description && <p className="mt-1 max-w-sm text-sm text-ink-muted">{description}</p>}
      {action && <div className="mt-4">{action}</div>}
    </div>
  );
}

/** An error that is worth reading. Problem Details carry a detail and a request
 *  id; showing both is what makes a support conversation short. */
export function ErrorNote({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  if (!error) return null;
  const message = error instanceof Error ? error.message : String(error);
  const requestID =
    typeof error === "object" && error !== null && "problem" in error
      ? (error as { problem?: { request_id?: string } }).problem?.request_id
      : undefined;

  return (
    <div
      role="alert"
      className="mb-4 flex items-start gap-3 rounded-md border border-danger/40 bg-danger/10 px-3 py-2.5 text-sm text-danger"
    >
      <Icon name="warning" className="mt-0.5" />
      <div className="min-w-0 flex-1">
        {message}
        {requestID && (
          <span className="ml-2 font-mono text-xs text-ink-faint">request {requestID}</span>
        )}
      </div>
      {onRetry && (
        <Button size="sm" variant="ghost" onClick={onRetry} className="-my-1 text-danger hover:text-danger">
          Retry
        </Button>
      )}
    </div>
  );
}

export function Empty({ children }: { children: React.ReactNode }) {
  return <div className="py-12 text-center text-sm text-ink-muted">{children}</div>;
}

export function Spinner({ className }: { className?: string }) {
  return (
    <div className={cn("flex justify-center py-12", className)} role="status" aria-label="Loading">
      <div className="h-5 w-5 animate-spin rounded-full border-2 border-surface-border border-t-accent" />
    </div>
  );
}

/** An inline notice inside a form or page. */
export function Callout({
  tone = "info",
  title,
  children,
  className,
}: {
  tone?: "info" | "warning" | "danger";
  title?: string;
  children: React.ReactNode;
  className?: string;
}) {
  const style = {
    info: "border-pending/30 bg-pending/5 text-ink-muted [&_strong]:text-pending",
    warning: "border-degraded/40 bg-degraded/5 text-ink-muted [&_strong]:text-degraded",
    danger: "border-danger/40 bg-danger/5 text-ink-muted [&_strong]:text-danger",
  }[tone];
  const icon: IconName = tone === "info" ? "info" : "warning";
  const iconTone = { info: "text-pending", warning: "text-degraded", danger: "text-danger" }[tone];
  return (
    <div className={cn("flex gap-3 rounded-md border px-3 py-2.5 text-sm", style, className)}>
      <Icon name={icon} className={cn("mt-0.5", iconTone)} />
      <div className="min-w-0">
        {title && <strong className="block font-medium">{title}</strong>}
        {children}
      </div>
    </div>
  );
}
