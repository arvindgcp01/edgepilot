"use client";

import { useMemo, useState } from "react";
import { cn } from "@/lib/cn";
import { Icon } from "./icons";
import { Button } from "./button";
import { Select } from "./field";

/**
 * Table primitives.
 *
 * Tables are the product's main surface, so they get the most conservative
 * treatment: one hairline between rows, a header that is quieter than the
 * data, and hover that is barely there. Sorting, selection and paging are
 * plain hooks so a page composes exactly the behaviour it needs.
 */

export function Table({ className, children }: { className?: string; children: React.ReactNode }) {
  return (
    <div className={cn("overflow-x-auto", className)}>
      <table className="w-full border-collapse text-sm">{children}</table>
    </div>
  );
}

export function THead({ children, sticky }: { children: React.ReactNode; sticky?: boolean }) {
  return (
    <thead className={cn("bg-surface-raised", sticky && "sticky top-0 z-10")}>
      {children}
    </thead>
  );
}

export function TBody({ children }: { children: React.ReactNode }) {
  return <tbody className="divide-y divide-surface-border">{children}</tbody>;
}

export function Tr({
  className,
  selected,
  interactive,
  children,
  ...props
}: React.HTMLAttributes<HTMLTableRowElement> & { selected?: boolean; interactive?: boolean }) {
  return (
    <tr
      {...props}
      className={cn(
        "transition-colors",
        interactive && "cursor-pointer hover:bg-surface-hover/60",
        selected && "bg-accent/5",
        className,
      )}
    >
      {children}
    </tr>
  );
}

export function Th({
  className,
  align = "left",
  children,
  ...props
}: React.ThHTMLAttributes<HTMLTableCellElement> & { align?: "left" | "right" | "center" }) {
  return (
    <th
      scope="col"
      {...props}
      className={cn(
        "h-9 whitespace-nowrap border-b border-surface-border px-3 text-xs font-medium text-ink-muted first:pl-4 last:pr-4",
        align === "right" && "text-right",
        align === "center" && "text-center",
        align === "left" && "text-left",
        className,
      )}
    >
      {children}
    </th>
  );
}

export function Td({
  className,
  align = "left",
  children,
  ...props
}: React.TdHTMLAttributes<HTMLTableCellElement> & { align?: "left" | "right" | "center" }) {
  return (
    <td
      {...props}
      className={cn(
        "px-3 py-2.5 align-middle first:pl-4 last:pr-4",
        align === "right" && "text-right",
        align === "center" && "text-center",
        className,
      )}
    >
      {children}
    </td>
  );
}

// ---------------------------------------------------------------------------
// Sorting
// ---------------------------------------------------------------------------

export type SortDir = "asc" | "desc";
export type SortState<K extends string> = { key: K; dir: SortDir } | null;

export function SortableTh<K extends string>({
  sortKey,
  sort,
  onSort,
  children,
  align,
  className,
}: {
  sortKey: K;
  sort: SortState<K>;
  onSort: (next: SortState<K>) => void;
  children: React.ReactNode;
  align?: "left" | "right" | "center";
  className?: string;
}) {
  const active = sort?.key === sortKey;
  const dir = active ? sort.dir : undefined;
  return (
    <Th
      align={align}
      className={cn("p-0", className)}
      aria-sort={active ? (dir === "asc" ? "ascending" : "descending") : "none"}
    >
      <button
        type="button"
        onClick={() =>
          onSort(
            !active ? { key: sortKey, dir: "asc" } : dir === "asc" ? { key: sortKey, dir: "desc" } : null,
          )
        }
        className={cn(
          "group inline-flex h-9 items-center gap-1 px-3 text-xs font-medium hover:text-ink",
          align === "right" && "flex-row-reverse",
          active ? "text-ink" : "text-ink-muted",
        )}
      >
        {children}
        <Icon
          name={dir === "asc" ? "arrow-up" : dir === "desc" ? "arrow-down" : "chevrons-up-down"}
          size={12}
          className={cn(active ? "text-ink" : "text-ink-faint opacity-0 group-hover:opacity-100")}
        />
      </button>
    </Th>
  );
}

/** Sorts rows by an accessor per key. Stable, and a null sort returns the input. */
export function useSorted<T, K extends string>(
  rows: T[],
  sort: SortState<K>,
  accessors: Record<K, (row: T) => string | number | boolean | null | undefined>,
): T[] {
  return useMemo(() => {
    if (!sort) return rows;
    const get = accessors[sort.key];
    const sign = sort.dir === "asc" ? 1 : -1;
    return [...rows].sort((a, b) => {
      const av = get(a);
      const bv = get(b);
      if (av == null && bv == null) return 0;
      if (av == null) return 1;
      if (bv == null) return -1;
      if (typeof av === "number" && typeof bv === "number") return (av - bv) * sign;
      return String(av).localeCompare(String(bv), undefined, { numeric: true, sensitivity: "base" }) * sign;
    });
  }, [rows, sort, accessors]);
}

// ---------------------------------------------------------------------------
// Selection
// ---------------------------------------------------------------------------

export function useSelection<T extends { id: string }>(rows: T[]) {
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const visibleIds = useMemo(() => rows.map((r) => r.id), [rows]);
  const allSelected = visibleIds.length > 0 && visibleIds.every((id) => selected.has(id));
  const someSelected = !allSelected && visibleIds.some((id) => selected.has(id));

  return {
    selected,
    count: selected.size,
    isSelected: (id: string) => selected.has(id),
    toggle: (id: string) =>
      setSelected((current) => {
        const next = new Set(current);
        if (next.has(id)) next.delete(id);
        else next.add(id);
        return next;
      }),
    toggleAll: () =>
      setSelected((current) => {
        if (allSelected) {
          const next = new Set(current);
          for (const id of visibleIds) next.delete(id);
          return next;
        }
        return new Set([...current, ...visibleIds]);
      }),
    clear: () => setSelected(new Set()),
    allSelected,
    someSelected,
  };
}

export function SelectionCheckbox({
  checked,
  indeterminate,
  onChange,
  label,
}: {
  checked: boolean;
  indeterminate?: boolean;
  onChange: () => void;
  label: string;
}) {
  return (
    <input
      type="checkbox"
      aria-label={label}
      checked={checked}
      ref={(el) => {
        if (el) el.indeterminate = Boolean(indeterminate);
      }}
      onChange={onChange}
      onClick={(e) => e.stopPropagation()}
      className="h-4 w-4 rounded border-surface-border-strong bg-surface"
    />
  );
}

/** The bar that replaces the toolbar when rows are selected. */
export function BulkBar({
  count,
  onClear,
  children,
}: {
  count: number;
  onClear: () => void;
  children: React.ReactNode;
}) {
  if (count === 0) return null;
  return (
    <div className="flex flex-wrap items-center gap-2 border-b border-surface-border bg-accent/5 px-4 py-2 text-sm">
      <span className="font-medium text-ink">
        {count} selected
      </span>
      <Button size="sm" variant="ghost" onClick={onClear}>
        Clear
      </Button>
      <div className="ml-auto flex items-center gap-2">{children}</div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------

export function usePagination<T>(rows: T[], initialSize = 10) {
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState(initialSize);
  const pages = Math.max(1, Math.ceil(rows.length / pageSize));
  const clamped = Math.min(page, pages - 1);
  const slice = useMemo(
    () => rows.slice(clamped * pageSize, clamped * pageSize + pageSize),
    [rows, clamped, pageSize],
  );
  return {
    rows: slice,
    page: clamped,
    pages,
    pageSize,
    total: rows.length,
    setPage,
    setPageSize: (size: number) => {
      setPageSize(size);
      setPage(0);
    },
    reset: () => setPage(0),
  };
}

export function Pagination({
  page,
  pages,
  pageSize,
  total,
  onPage,
  onPageSize,
  className,
}: {
  page: number;
  pages: number;
  pageSize: number;
  total: number;
  onPage: (page: number) => void;
  onPageSize?: (size: number) => void;
  className?: string;
}) {
  const from = total === 0 ? 0 : page * pageSize + 1;
  const to = Math.min(total, (page + 1) * pageSize);
  return (
    <div
      className={cn(
        "flex flex-wrap items-center justify-between gap-3 border-t border-surface-border px-4 py-2.5 text-xs text-ink-muted",
        className,
      )}
    >
      <div className="flex items-center gap-3">
        <span className="tabular">
          {from}–{to} of {total}
        </span>
        {onPageSize && (
          <label className="flex items-center gap-1.5">
            <span className="sr-only">Rows per page</span>
            <Select
              value={pageSize}
              onChange={(e) => onPageSize(Number(e.target.value))}
              className="h-7 w-auto py-0 pr-7 text-xs"
            >
              {[10, 25, 50, 100].map((n) => (
                <option key={n} value={n}>{n} / page</option>
              ))}
            </Select>
          </label>
        )}
      </div>
      <div className="flex items-center gap-1">
        <Button size="sm" variant="ghost" icon="chevron-left" disabled={page === 0} onClick={() => onPage(page - 1)}>
          <span className="sr-only sm:not-sr-only">Previous</span>
        </Button>
        <span className="px-2 tabular">
          {page + 1} / {pages}
        </span>
        <Button
          size="sm"
          variant="ghost"
          disabled={page >= pages - 1}
          onClick={() => onPage(page + 1)}
          className="flex-row-reverse"
        >
          <span className="sr-only sm:not-sr-only">Next</span>
          <Icon name="chevron-right" size={14} />
        </Button>
      </div>
    </div>
  );
}
