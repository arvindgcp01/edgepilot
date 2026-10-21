"use client";

import { useEffect, useId, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { cn } from "@/lib/cn";
import { Button, type ButtonVariant } from "./button";
import { Icon, type IconName } from "./icons";
import { Input } from "./field";

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

/** Escape closes, body scroll stops, focus lands inside. Every overlay wants the
 *  same three things and gets them from here. */
function useOverlay(open: boolean, onClose: () => void) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const previous = document.activeElement as HTMLElement | null;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        onClose();
      }
    };
    document.addEventListener("keydown", onKey);
    const overflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";

    // Focus the first control, or the panel itself so Escape has a target.
    const timer = setTimeout(() => {
      const panel = ref.current;
      if (!panel) return;
      const first = panel.querySelector<HTMLElement>(
        'input:not([type="hidden"]), select, textarea, button:not([data-overlay-close]), [href]',
      );
      (first ?? panel).focus();
    }, 10);

    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = overflow;
      clearTimeout(timer);
      previous?.focus?.();
    };
  }, [open, onClose]);

  return ref;
}

function Portal({ children }: { children: React.ReactNode }) {
  const [mounted, setMounted] = useState(false);
  useEffect(() => setMounted(true), []);
  if (!mounted) return null;
  return createPortal(children, document.body);
}

// ---------------------------------------------------------------------------
// Modal
// ---------------------------------------------------------------------------

export function Modal({
  open,
  onClose,
  title,
  description,
  children,
  footer,
  size = "md",
}: {
  open: boolean;
  onClose: () => void;
  title: string;
  description?: React.ReactNode;
  children: React.ReactNode;
  footer?: React.ReactNode;
  size?: "sm" | "md" | "lg";
}) {
  const ref = useOverlay(open, onClose);
  const titleId = useId();
  if (!open) return null;

  return (
    <Portal>
      <div className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto p-4 pt-[10vh] sm:pt-[14vh]">
        <div className="fixed inset-0 animate-fade-in bg-black/60" onClick={onClose} aria-hidden />
        <div
          ref={ref}
          role="dialog"
          aria-modal="true"
          aria-labelledby={titleId}
          tabIndex={-1}
          className={cn(
            "relative w-full animate-slide-up rounded-lg border border-surface-border bg-surface-raised shadow-overlay outline-none",
            size === "sm" && "max-w-sm",
            size === "md" && "max-w-lg",
            size === "lg" && "max-w-2xl",
          )}
        >
          <div className="flex items-start justify-between gap-4 px-5 pt-5">
            <div className="min-w-0">
              <h2 id={titleId} className="text-base font-semibold text-ink">{title}</h2>
              {description && <p className="mt-1 text-sm text-ink-muted">{description}</p>}
            </div>
            <button
              type="button"
              data-overlay-close
              onClick={onClose}
              aria-label="Close"
              className="-m-1.5 rounded p-1.5 text-ink-faint hover:bg-surface-hover hover:text-ink"
            >
              <Icon name="x" />
            </button>
          </div>
          <div className="px-5 py-5">{children}</div>
          {footer && (
            <div className="flex flex-wrap items-center justify-end gap-2 border-t border-surface-border px-5 py-3">
              {footer}
            </div>
          )}
        </div>
      </div>
    </Portal>
  );
}

// ---------------------------------------------------------------------------
// Confirmation
// ---------------------------------------------------------------------------

/**
 * The dialog before something that cannot be undone.
 *
 * It says what will happen, in the second person, in one paragraph. The
 * confirming button names the action ("Suspend user"), never "OK" or "Yes", so
 * the operator reads what they are agreeing to at the moment they agree. For
 * the most severe actions the dialog asks them to type the name of the thing
 * — a speed bump that is annoying exactly once per year and cheap every time.
 */
export function ConfirmDialog({
  open,
  onClose,
  onConfirm,
  title,
  description,
  confirmLabel,
  tone = "danger",
  loading,
  confirmText,
  children,
}: {
  open: boolean;
  onClose: () => void;
  onConfirm: () => void | Promise<void>;
  title: string;
  description: React.ReactNode;
  confirmLabel: string;
  tone?: "danger" | "default";
  loading?: boolean;
  /** When set, the confirm button stays disabled until this is typed exactly. */
  confirmText?: string;
  children?: React.ReactNode;
}) {
  const [typed, setTyped] = useState("");
  useEffect(() => {
    if (!open) setTyped("");
  }, [open]);
  const blocked = confirmText !== undefined && typed !== confirmText;
  const variant: ButtonVariant = tone === "danger" ? "danger-solid" : "primary";

  return (
    <Modal open={open} onClose={onClose} title={title} size="sm"
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={loading}>Cancel</Button>
          <Button variant={variant} onClick={() => void onConfirm()} disabled={blocked} loading={loading}>
            {confirmLabel}
          </Button>
        </>
      }
    >
      <div className="flex gap-3">
        {tone === "danger" && (
          <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-danger/10 text-danger">
            <Icon name="warning" />
          </div>
        )}
        <div className="min-w-0 flex-1 space-y-3 text-sm text-ink-muted">
          <div>{description}</div>
          {children}
          {confirmText !== undefined && (
            <label className="block">
              <span className="mb-1.5 block text-xs text-ink-faint">
                Type <span className="font-mono text-ink">{confirmText}</span> to confirm
              </span>
              <Input
                value={typed}
                onChange={(e) => setTyped(e.target.value)}
                autoComplete="off"
                spellCheck={false}
              />
            </label>
          )}
        </div>
      </div>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Drawer
// ---------------------------------------------------------------------------

/**
 * A right-side panel for one record's detail. The page behind it stays visible
 * and scrolls with the drawer closed, so the operator keeps their place in the
 * table — the reason to prefer a drawer over a page for anything they will open
 * a dozen times in a row.
 */
export function Drawer({
  open,
  onClose,
  title,
  header,
  children,
  footer,
  width = "md",
}: {
  open: boolean;
  onClose: () => void;
  title: string;
  /** Rich header content rendered under the title bar — an avatar block, tabs. */
  header?: React.ReactNode;
  children: React.ReactNode;
  footer?: React.ReactNode;
  width?: "md" | "lg";
}) {
  const ref = useOverlay(open, onClose);
  const titleId = useId();
  if (!open) return null;

  return (
    <Portal>
      <div className="fixed inset-0 z-50">
        <div className="fixed inset-0 animate-fade-in bg-black/50" onClick={onClose} aria-hidden />
        <div
          ref={ref}
          role="dialog"
          aria-modal="true"
          aria-labelledby={titleId}
          tabIndex={-1}
          className={cn(
            "fixed inset-y-0 right-0 flex w-full animate-slide-in-right flex-col border-l border-surface-border bg-surface-raised shadow-drawer outline-none",
            width === "md" ? "sm:max-w-[30rem]" : "sm:max-w-[40rem]",
          )}
        >
          <div className="flex items-center justify-between gap-3 border-b border-surface-border px-5 py-3">
            <h2 id={titleId} className="text-sm font-medium text-ink-muted">{title}</h2>
            <button
              type="button"
              data-overlay-close
              onClick={onClose}
              aria-label="Close"
              className="-m-1.5 rounded p-1.5 text-ink-faint hover:bg-surface-hover hover:text-ink"
            >
              <Icon name="x" />
            </button>
          </div>
          {header}
          <div className="min-h-0 flex-1 overflow-y-auto">{children}</div>
          {footer && (
            <div className="flex flex-wrap items-center gap-2 border-t border-surface-border px-5 py-3">
              {footer}
            </div>
          )}
        </div>
      </div>
    </Portal>
  );
}

// ---------------------------------------------------------------------------
// Menu and popover
// ---------------------------------------------------------------------------

export type MenuItem =
  | { kind?: "item"; label: string; icon?: IconName; onSelect: () => void; tone?: "danger"; disabled?: boolean; hint?: string }
  | { kind: "separator" }
  | { kind: "label"; label: string };

function useDismiss(open: boolean, onClose: () => void) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onPointer = (e: PointerEvent) => {
      if (!ref.current?.contains(e.target as Node)) onClose();
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("pointerdown", onPointer);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("pointerdown", onPointer);
      document.removeEventListener("keydown", onKey);
    };
  }, [open, onClose]);
  return ref;
}

/**
 * A dropdown of actions. Destructive items sit after a separator and in red, so
 * "Remove" is never one slip below "Edit".
 */
export function Menu({
  trigger,
  items,
  align = "end",
  width = "w-52",
}: {
  trigger: (props: { open: boolean; toggle: () => void }) => React.ReactNode;
  items: MenuItem[];
  align?: "start" | "end";
  width?: string;
}) {
  const [open, setOpen] = useState(false);
  const ref = useDismiss(open, () => setOpen(false));

  return (
    <div ref={ref} className="relative inline-block text-left">
      {trigger({ open, toggle: () => setOpen((v) => !v) })}
      {open && (
        <div
          role="menu"
          className={cn(
            "absolute z-40 mt-1 animate-slide-up rounded-md border border-surface-border bg-surface-overlay p-1 shadow-overlay",
            align === "end" ? "right-0" : "left-0",
            width,
          )}
        >
          {items.map((item, index) => {
            if (item.kind === "separator") {
              return <div key={index} role="separator" className="my-1 border-t border-surface-border" />;
            }
            if (item.kind === "label") {
              return (
                <div key={index} className="px-2 pb-1 pt-1.5 text-2xs font-medium uppercase tracking-wide text-ink-faint">
                  {item.label}
                </div>
              );
            }
            return (
              <button
                key={index}
                type="button"
                role="menuitem"
                disabled={item.disabled}
                onClick={() => {
                  setOpen(false);
                  item.onSelect();
                }}
                className={cn(
                  "flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-sm disabled:cursor-not-allowed disabled:opacity-50",
                  item.tone === "danger"
                    ? "text-danger hover:bg-danger/10"
                    : "text-ink hover:bg-surface-hover",
                )}
              >
                {item.icon && <Icon name={item.icon} size={14} className={item.tone === "danger" ? "" : "text-ink-muted"} />}
                <span className="flex-1">{item.label}</span>
                {item.hint && <span className="text-xs text-ink-faint">{item.hint}</span>}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

/** Arbitrary content anchored to a trigger — notifications, a help panel. */
export function Popover({
  trigger,
  children,
  align = "end",
  width = "w-80",
}: {
  trigger: (props: { open: boolean; toggle: () => void }) => React.ReactNode;
  children: React.ReactNode | ((close: () => void) => React.ReactNode);
  align?: "start" | "end";
  width?: string;
}) {
  const [open, setOpen] = useState(false);
  const close = () => setOpen(false);
  const ref = useDismiss(open, close);
  return (
    <div ref={ref} className="relative inline-block">
      {trigger({ open, toggle: () => setOpen((v) => !v) })}
      {open && (
        <div
          className={cn(
            "absolute z-40 mt-1 animate-slide-up overflow-hidden rounded-md border border-surface-border bg-surface-overlay shadow-overlay",
            align === "end" ? "right-0" : "left-0",
            width,
          )}
        >
          {typeof children === "function" ? children(close) : children}
        </div>
      )}
    </div>
  );
}

/** A label that appears on hover. CSS only, so it works on every element and
 *  never gets stuck open. */
export function Tooltip({
  label,
  children,
  side = "top",
}: {
  label: string;
  children: React.ReactNode;
  side?: "top" | "bottom";
}) {
  return (
    <span className="group/tip relative inline-flex">
      {children}
      <span
        role="tooltip"
        className={cn(
          "pointer-events-none absolute left-1/2 z-40 -translate-x-1/2 whitespace-nowrap rounded border border-surface-border bg-surface-overlay px-2 py-1 text-xs text-ink opacity-0 shadow-overlay transition-opacity delay-150 group-hover/tip:opacity-100 group-focus-within/tip:opacity-100",
          side === "top" ? "bottom-full mb-1.5" : "top-full mt-1.5",
        )}
      >
        {label}
      </span>
    </span>
  );
}
