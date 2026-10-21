import { cn } from "@/lib/cn";

/** A bordered panel. Padding is opt-out for tables, which want their rows to
 *  run edge to edge. */
export function Card({
  className,
  padded = true,
  children,
}: {
  className?: string;
  padded?: boolean;
  children: React.ReactNode;
}) {
  return (
    <div
      className={cn(
        "rounded-lg border border-surface-border bg-surface-raised",
        padded && "p-5",
        className,
      )}
    >
      {children}
    </div>
  );
}

/**
 * The top of every page. Title on the left, actions on the right, and one line
 * of description that says what the page is for — read once, then ignored, which
 * is why it is muted.
 */
export function PageHeader({
  title,
  description,
  actions,
  meta,
  className,
}: {
  title: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  /** Small inline status next to the title — a live indicator, a count. */
  meta?: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("mb-6 flex flex-wrap items-start justify-between gap-4", className)}>
      <div className="min-w-0">
        <div className="flex items-center gap-3">
          <h1 className="text-xl font-semibold tracking-tight text-ink">{title}</h1>
          {meta}
        </div>
        {description && <p className="mt-1 max-w-2xl text-sm text-ink-muted">{description}</p>}
      </div>
      {actions && <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}

/**
 * Summary numbers as one quiet row, not a grid of cards. Five numbers in five
 * boxes take a third of the viewport and say the same thing this says in one
 * line; the table beneath them is what the operator came for.
 */
export function StatStrip({
  items,
  className,
}: {
  items: { label: string; value: React.ReactNode; tone?: string; hint?: string }[];
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex flex-wrap divide-surface-border rounded-lg border border-surface-border bg-surface-raised sm:divide-x",
        className,
      )}
    >
      {items.map((item) => (
        <div key={item.label} className="min-w-[8rem] flex-1 px-4 py-3" title={item.hint}>
          <div className="text-xs font-medium text-ink-faint">{item.label}</div>
          <div className={cn("mt-0.5 text-lg font-semibold tabular leading-tight", item.tone ?? "text-ink")}>
            {item.value}
          </div>
        </div>
      ))}
    </div>
  );
}

/**
 * A titled section inside a settings page: heading and description on the left
 * rail, the form on the right. The two-column shape keeps every setting's
 * explanation next to its control rather than in a paragraph above the form.
 */
export function Section({
  title,
  description,
  children,
  footer,
  className,
  tone,
}: {
  title: React.ReactNode;
  description?: React.ReactNode;
  children: React.ReactNode;
  footer?: React.ReactNode;
  className?: string;
  tone?: "danger";
}) {
  return (
    <section
      className={cn(
        "rounded-lg border bg-surface-raised",
        tone === "danger" ? "border-danger/40" : "border-surface-border",
        className,
      )}
    >
      <div className="grid gap-6 p-5 md:grid-cols-[minmax(0,14rem)_1fr]">
        <div>
          <h2 className={cn("text-sm font-semibold", tone === "danger" ? "text-danger" : "text-ink")}>
            {title}
          </h2>
          {description && <p className="mt-1 text-sm text-ink-muted">{description}</p>}
        </div>
        <div className="min-w-0 space-y-4">{children}</div>
      </div>
      {footer && (
        <div
          className={cn(
            "flex items-center justify-end gap-2 border-t px-5 py-3",
            tone === "danger" ? "border-danger/30" : "border-surface-border",
          )}
        >
          {footer}
        </div>
      )}
    </section>
  );
}

/** A label/value grid for read-only facts. */
export function Facts({
  rows,
  className,
  mono,
}: {
  rows: { label: string; value: React.ReactNode }[];
  className?: string;
  mono?: boolean;
}) {
  return (
    <dl className={cn("grid grid-cols-[minmax(0,9rem)_1fr] gap-x-4 gap-y-2.5 text-sm", className)}>
      {rows.map((row) => (
        <div key={row.label} className="contents">
          <dt className="text-ink-faint">{row.label}</dt>
          <dd className={cn("min-w-0 truncate text-ink-muted", mono && "font-mono text-xs")}>
            {row.value}
          </dd>
        </div>
      ))}
    </dl>
  );
}

const AVATAR_TONES = [
  "bg-accent/20 text-accent-soft",
  "bg-online/20 text-online",
  "bg-degraded/20 text-degraded",
  "bg-[#a371f7]/20 text-[#c4a6ff]",
  "bg-[#f778ba]/20 text-[#ffadd6]",
  "bg-[#39c5cf]/20 text-[#7ee7ef]",
];

/** Initials on a tinted disc. The tint is derived from the name so the same
 *  person is the same colour everywhere. */
export function Avatar({
  name,
  size = "md",
  className,
}: {
  name: string;
  size?: "sm" | "md" | "lg";
  className?: string;
}) {
  const initials = name
    .split(/[\s@.]+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part[0]?.toUpperCase() ?? "")
    .join("");
  let hash = 0;
  for (const char of name) hash = (hash * 31 + char.charCodeAt(0)) >>> 0;
  const tone = AVATAR_TONES[hash % AVATAR_TONES.length];
  return (
    <span
      aria-hidden
      className={cn(
        "inline-flex shrink-0 items-center justify-center rounded-full font-semibold",
        size === "sm" && "h-6 w-6 text-2xs",
        size === "md" && "h-8 w-8 text-xs",
        size === "lg" && "h-12 w-12 text-base",
        tone,
        className,
      )}
    >
      {initials || "?"}
    </span>
  );
}

export function Divider({ className }: { className?: string }) {
  return <hr className={cn("border-surface-border", className)} />;
}
