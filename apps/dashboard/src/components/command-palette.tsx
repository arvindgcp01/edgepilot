"use client";

import { useRouter } from "next/navigation";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { can, signOut } from "@/lib/api/client";
import { listDevices, type Device } from "@/lib/api/devices";
import { setTheme } from "@/lib/theme";
import { Icon, Kbd, StatusBadge, type IconName } from "@/components/ui";
import { cn } from "@/lib/cn";

export const PALETTE_EVENT = "edgepilot:palette";

/** Opens the palette from anywhere — a button, a shortcut, an empty state. */
export function openPalette() {
  window.dispatchEvent(new Event(PALETTE_EVENT));
}

type Entry = {
  id: string;
  group: "Devices" | "Go to" | "Actions";
  label: string;
  hint?: string;
  icon: IconName;
  device?: Device;
  run: () => void;
};

/**
 * ⌘K.
 *
 * One box that answers the two questions an operator has most often: "where is
 * that machine" and "where is that page". Devices come from the same search
 * the fleet table uses, so a hostname, client, serial, or site all match.
 * Pages and actions are filtered by permission — presentation only; the server
 * still refuses what the caller cannot do.
 */
export function CommandPalette() {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [devices, setDevices] = useState<Device[]>([]);
  const [searching, setSearching] = useState(false);
  const [active, setActive] = useState(0);
  const input = useRef<HTMLInputElement>(null);
  const list = useRef<HTMLDivElement>(null);

  const close = useCallback(() => {
    setOpen(false);
    setQuery("");
    setDevices([]);
    setActive(0);
  }, []);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((v) => !v);
      }
    };
    const onOpen = () => setOpen(true);
    document.addEventListener("keydown", onKey);
    window.addEventListener(PALETTE_EVENT, onOpen);
    return () => {
      document.removeEventListener("keydown", onKey);
      window.removeEventListener(PALETTE_EVENT, onOpen);
    };
  }, []);

  useEffect(() => {
    if (open) requestAnimationFrame(() => input.current?.focus());
  }, [open]);

  // Device search, debounced. Stale responses are dropped by the cancelled flag
  // so a slow answer for "sto" cannot overwrite a fast one for "store-0142".
  useEffect(() => {
    if (!open || !can("device:read")) return;
    const q = query.trim();
    let cancelled = false;
    const timer = setTimeout(async () => {
      setSearching(true);
      try {
        const page = await listDevices({ q: q || undefined, limit: q ? 8 : 5, sort: "-last_seen_at" });
        if (!cancelled) setDevices(page.data);
      } catch {
        if (!cancelled) setDevices([]);
      } finally {
        if (!cancelled) setSearching(false);
      }
    }, q ? 150 : 0);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [open, query]);

  const entries = useMemo<Entry[]>(() => {
    const go = (href: string) => () => router.push(href);
    const pages: Entry[] = [
      { id: "p-overview", group: "Go to", label: "Overview", icon: "home", run: go("/") },
      ...(can("device:read") ? [{ id: "p-devices", group: "Go to" as const, label: "Devices", icon: "monitor" as IconName, run: go("/devices") }] : []),
      ...(can("enrollment_token:read") ? [{ id: "p-enroll", group: "Go to" as const, label: "Enrollment", icon: "key" as IconName, run: go("/enrollment") }] : []),
      ...(can("device:read") ? [
        { id: "p-policies", group: "Go to" as const, label: "Policies", icon: "shield" as IconName, run: go("/policies") },
        { id: "p-reports", group: "Go to" as const, label: "Reports", icon: "bar-chart" as IconName, run: go("/reports") },
      ] : []),
      { id: "p-settings", group: "Go to", label: "Settings", icon: "settings", run: go("/settings/general") },
      { id: "p-profile", group: "Go to", label: "Your profile", icon: "user", run: go("/settings/profile") },
    ];
    const actions: Entry[] = [
      ...(can("enrollment_token:write") ? [{ id: "a-enroll", group: "Actions" as const, label: "Enroll a new device", icon: "plus" as IconName, run: go("/enrollment?new=1") }] : []),
      { id: "a-light", group: "Actions", label: "Switch to light theme", icon: "sun", run: () => setTheme("light") },
      { id: "a-dark", group: "Actions", label: "Switch to dark theme", icon: "moon", run: () => setTheme("dark") },
      { id: "a-system", group: "Actions", label: "Use system theme", icon: "monitor", run: () => setTheme("system") },
      { id: "a-signout", group: "Actions", label: "Sign out", icon: "log-out", run: () => void signOut().then(() => router.replace("/login")) },
    ];
    const q = query.trim().toLowerCase();
    const match = (e: Entry) => !q || e.label.toLowerCase().includes(q);
    const deviceEntries: Entry[] = devices.map((d) => ({
      id: `d-${d.id}`,
      group: "Devices",
      label: d.hostname || d.serial_number,
      hint: [d.client_name, d.site].filter(Boolean).join(" · ") || d.serial_number,
      icon: "monitor",
      device: d,
      run: go(`/devices/${d.id}`),
    }));
    return [...deviceEntries, ...pages.filter(match), ...actions.filter(match)];
  }, [devices, query, router]);

  useEffect(() => setActive(0), [query]);

  useEffect(() => {
    list.current?.querySelector<HTMLElement>(`[data-index="${active}"]`)?.scrollIntoView({ block: "nearest" });
  }, [active]);

  if (!open) return null;

  function choose(entry: Entry | undefined) {
    if (!entry) return;
    close();
    entry.run();
  }

  const groups = (["Devices", "Go to", "Actions"] as const)
    .map((group) => ({ group, items: entries.filter((e) => e.group === group) }))
    .filter((g) => g.items.length > 0);

  return (
    <div className="fixed inset-0 z-[70] flex items-start justify-center p-4 pt-[12vh]">
      <div className="fixed inset-0 animate-fade-in bg-slate-900/30 backdrop-blur-[2px] dark:bg-black/60" onClick={close} aria-hidden />
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Command palette"
        className="relative w-full max-w-xl animate-scale-in overflow-hidden rounded-xl border border-surface-border bg-surface-raised shadow-overlay"
      >
        <div className="flex items-center gap-3 border-b border-surface-border px-4">
          <Icon name="search" className="text-ink-faint" />
          <input
            ref={input}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "ArrowDown") {
                e.preventDefault();
                setActive((i) => Math.min(i + 1, entries.length - 1));
              } else if (e.key === "ArrowUp") {
                e.preventDefault();
                setActive((i) => Math.max(i - 1, 0));
              } else if (e.key === "Enter") {
                e.preventDefault();
                choose(entries[active]);
              } else if (e.key === "Escape") {
                e.preventDefault();
                close();
              }
            }}
            placeholder="Search devices, pages, and actions…"
            aria-label="Search"
            role="combobox"
            aria-expanded="true"
            aria-controls="palette-results"
            className="h-14 flex-1 bg-transparent text-[15px] text-ink placeholder:text-ink-faint focus:outline-none focus-visible:ring-0 focus-visible:ring-offset-0"
          />
          {searching && <span className="h-4 w-4 animate-spin rounded-full border-2 border-surface-border border-t-accent" aria-hidden />}
          <Kbd>Esc</Kbd>
        </div>

        <div ref={list} id="palette-results" role="listbox" className="max-h-[22rem] overflow-y-auto p-2">
          {groups.length === 0 && (
            <div className="px-3 py-10 text-center text-sm text-ink-muted">
              No matches for <span className="font-medium text-ink">“{query}”</span>
            </div>
          )}
          {groups.map(({ group, items }) => (
            <div key={group} className="mb-1 last:mb-0">
              <div className="px-2.5 pb-1 pt-2 text-2xs font-semibold uppercase tracking-wider text-ink-faint">
                {group === "Devices" && !query.trim() ? "Recently seen" : group}
              </div>
              {items.map((entry) => {
                const index = entries.indexOf(entry);
                const selected = index === active;
                return (
                  <button
                    key={entry.id}
                    type="button"
                    role="option"
                    aria-selected={selected}
                    data-index={index}
                    onMouseMove={() => setActive(index)}
                    onClick={() => choose(entry)}
                    className={cn(
                      "flex w-full items-center gap-3 rounded-lg px-2.5 py-2 text-left text-sm",
                      selected ? "bg-accent-subtle text-ink" : "text-ink",
                    )}
                  >
                    <span
                      className={cn(
                        "flex h-7 w-7 shrink-0 items-center justify-center rounded-md border",
                        selected ? "border-accent/30 bg-surface-raised text-accent" : "border-surface-border bg-surface text-ink-muted",
                      )}
                    >
                      <Icon name={entry.icon} size={14} />
                    </span>
                    <span className="min-w-0 flex-1">
                      <span className="block truncate font-medium">{entry.label}</span>
                      {entry.hint && <span className="block truncate text-xs text-ink-faint">{entry.hint}</span>}
                    </span>
                    {entry.device && <StatusBadge status={entry.device.status} />}
                    {selected && <Icon name="arrow-right" size={14} className="text-ink-faint" />}
                  </button>
                );
              })}
            </div>
          ))}
        </div>

        <div className="flex items-center gap-4 border-t border-surface-border bg-surface-sunken/60 px-4 py-2 text-2xs text-ink-faint">
          <span className="inline-flex items-center gap-1"><Kbd>↑</Kbd><Kbd>↓</Kbd> navigate</span>
          <span className="inline-flex items-center gap-1"><Kbd>↵</Kbd> open</span>
          <span className="ml-auto inline-flex items-center gap-1"><Kbd>⌘</Kbd><Kbd>K</Kbd> toggle</span>
        </div>
      </div>
    </div>
  );
}
