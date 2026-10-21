"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useCallback, useEffect, useState } from "react";
import { can, signOut } from "@/lib/api/client";
import { listNotifications, type Notification } from "@/lib/api/admin";
import { fleetSummary, type FleetSummary } from "@/lib/api/devices";
import { useSession } from "@/components/session-provider";
import { CommandPalette, openPalette } from "@/components/command-palette";
import {
  Avatar, Badge, Button, Icon, IconButton, Kbd, Menu, Popover, Spinner, type IconName,
} from "@/components/ui";
import { cn } from "@/lib/cn";
import { relativeTime } from "@/lib/format";
import { readTheme, setTheme, type ThemePreference } from "@/lib/theme";

type NavItem = { href: string; label: string; icon: IconName; permission?: string; badge?: "pending" };

const NAV: { title?: string; items: NavItem[] }[] = [
  {
    items: [
      { href: "/", label: "Overview", icon: "home" },
      { href: "/devices", label: "Devices", icon: "monitor", permission: "device:read", badge: "pending" },
      { href: "/enrollment", label: "Enrollment", icon: "key", permission: "enrollment_token:read" },
    ],
  },
  {
    title: "Fleet",
    items: [
      { href: "/policies", label: "Policies", icon: "shield", permission: "device:read" },
      { href: "/reports", label: "Reports", icon: "bar-chart", permission: "device:read" },
    ],
  },
  {
    title: "Workspace",
    items: [{ href: "/settings/general", label: "Settings", icon: "settings" }],
  },
];

function isActive(pathname: string, href: string): boolean {
  if (href === "/") return pathname === "/";
  if (href.startsWith("/settings")) return pathname.startsWith("/settings");
  return pathname === href || pathname.startsWith(`${href}/`);
}

/**
 * The signed-in frame: a sidebar that holds every way out of the current page,
 * and a content column that holds nothing else.
 *
 * Navigation is filtered by permission, but that is presentation only: the
 * server refuses the call regardless. Hiding a link an operator cannot use is
 * courtesy; it is not the access control.
 */
export function AppShell({
  children,
  width = "default",
}: {
  children: React.ReactNode;
  /** Settings pages get a wider frame for their own sub-navigation. */
  width?: "default" | "wide";
}) {
  const { principal, loading } = useSession();
  const router = useRouter();
  const pathname = usePathname();
  const [mobileOpen, setMobileOpen] = useState(false);

  useEffect(() => {
    if (!loading && !principal) router.replace("/login");
  }, [loading, principal, router]);

  useEffect(() => setMobileOpen(false), [pathname]);

  if (loading) return <Spinner className="min-h-screen items-center" />;
  if (!principal) return null;

  return (
    <div className="min-h-screen">
      <CommandPalette />

      {/* Desktop sidebar */}
      <aside className="fixed inset-y-0 left-0 z-30 hidden w-64 border-r border-surface-border bg-surface-raised lg:flex">
        <Sidebar />
      </aside>

      {/* Mobile drawer */}
      {mobileOpen && (
        <div className="fixed inset-0 z-40 lg:hidden">
          <div className="fixed inset-0 animate-fade-in bg-slate-900/30 backdrop-blur-[2px] dark:bg-black/60" onClick={() => setMobileOpen(false)} aria-hidden />
          <aside className="fixed inset-y-0 left-0 flex w-72 animate-slide-in-left border-r border-surface-border bg-surface-raised shadow-overlay">
            <Sidebar onNavigate={() => setMobileOpen(false)} />
          </aside>
        </div>
      )}

      <div className="lg:pl-64">
        {/* Mobile top bar */}
        <header className="sticky top-0 z-20 flex h-14 items-center gap-2 border-b border-surface-border bg-surface-raised/90 px-4 backdrop-blur lg:hidden">
          <IconButton icon="menu" label="Open navigation" onClick={() => setMobileOpen(true)} aria-expanded={mobileOpen} />
          <Brand />
          <div className="ml-auto flex items-center gap-1">
            <IconButton icon="search" label="Search" onClick={openPalette} />
            <NotificationsMenu />
          </div>
        </header>

        <main
          className={cn(
            "mx-auto w-full px-4 py-6 sm:px-6 lg:px-10 lg:py-10",
            width === "wide" ? "max-w-[96rem]" : "max-w-[84rem]",
          )}
        >
          {children}
        </main>
      </div>
    </div>
  );
}

function Brand() {
  return (
    <Link href="/" className="flex items-center gap-2.5 rounded-lg py-1 text-[15px] font-semibold tracking-tight text-ink">
      <span className="flex h-8 w-8 items-center justify-center rounded-lg bg-gradient-to-br from-indigo-500 to-violet-600 text-white shadow-sm shadow-indigo-500/30">
        <Icon name="layers" size={16} strokeWidth={2} />
      </span>
      EdgePilot
    </Link>
  );
}

function Sidebar({ onNavigate }: { onNavigate?: () => void }) {
  const pathname = usePathname();
  const router = useRouter();
  const { principal } = useSession();
  const [summary, setSummary] = useState<FleetSummary | null>(null);

  // The pending-approval count is the one number worth a badge in the nav: it
  // is the only thing on the list that waits on a human.
  useEffect(() => {
    if (!can("device:read")) return;
    let cancelled = false;
    const load = () => fleetSummary().then((s) => !cancelled && setSummary(s)).catch(() => {});
    void load();
    const timer = setInterval(load, 60_000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, []);

  const doSignOut = useCallback(async () => {
    await signOut();
    router.replace("/login");
  }, [router]);

  if (!principal) return null;
  const displayName = principal.email.split("@")[0] ?? principal.email;

  return (
    <div className="flex min-h-0 w-full flex-col">
      <div className="flex h-16 items-center justify-between px-5">
        <Brand />
        <div className="hidden items-center lg:flex">
          <NotificationsMenu align="start" />
        </div>
      </div>

      <div className="px-3">
        <button
          type="button"
          onClick={() => {
            onNavigate?.();
            openPalette();
          }}
          className="flex h-9 w-full items-center gap-2 rounded-lg border border-surface-border bg-surface px-3 text-sm text-ink-faint transition-colors hover:border-surface-border-strong hover:text-ink-muted"
        >
          <Icon name="search" size={14} />
          <span className="flex-1 text-left">Search…</span>
          <span className="flex gap-0.5"><Kbd>⌘</Kbd><Kbd>K</Kbd></span>
        </button>
      </div>

      <nav className="mt-4 flex-1 space-y-6 overflow-y-auto px-3 pb-4" aria-label="Main">
        {NAV.map((section, i) => {
          const items = section.items.filter((item) => !item.permission || can(item.permission));
          if (items.length === 0) return null;
          return (
            <div key={i}>
              {section.title && (
                <div className="mb-1.5 px-3 text-2xs font-semibold uppercase tracking-wider text-ink-faint">
                  {section.title}
                </div>
              )}
              <ul className="space-y-0.5">
                {items.map((item) => {
                  const active = isActive(pathname, item.href);
                  const count = item.badge === "pending" ? summary?.pending_approval : undefined;
                  return (
                    <li key={item.href}>
                      <Link
                        href={item.href}
                        onClick={onNavigate}
                        aria-current={active ? "page" : undefined}
                        className={cn(
                          "group flex h-9 items-center gap-3 rounded-lg px-3 text-sm font-medium transition-colors",
                          active
                            ? "bg-accent-subtle text-accent dark:text-accent-soft"
                            : "text-ink-muted hover:bg-surface-hover hover:text-ink",
                        )}
                      >
                        <Icon
                          name={item.icon}
                          size={17}
                          className={cn(active ? "text-accent dark:text-accent-soft" : "text-ink-faint group-hover:text-ink-muted")}
                        />
                        <span className="flex-1">{item.label}</span>
                        {count ? (
                          <span
                            className="rounded-full bg-pending/10 px-2 py-0.5 text-2xs font-semibold tabular text-pending"
                            title={`${count} awaiting approval`}
                          >
                            {count}
                          </span>
                        ) : null}
                      </Link>
                    </li>
                  );
                })}
              </ul>
            </div>
          );
        })}
      </nav>

      {summary && summary.total > 0 && (
        <div className="mx-3 mb-3 rounded-xl border border-surface-border bg-surface p-3">
          <div className="flex items-center justify-between text-xs">
            <span className="font-medium text-ink-muted">Fleet online</span>
            <span className="font-semibold tabular text-ink">
              {summary.online}/{summary.total}
            </span>
          </div>
          <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-surface-hover">
            <div
              className="h-full rounded-full bg-online transition-all"
              style={{ width: `${Math.round((summary.online / summary.total) * 100)}%` }}
            />
          </div>
        </div>
      )}

      <div className="border-t border-surface-border p-3">
        <Menu
          width="w-60"
          side="top"
          align="start"
          className="block"
          trigger={({ open, toggle }) => (
            <button
              type="button"
              onClick={toggle}
              aria-haspopup="menu"
              aria-expanded={open}
              className={cn(
                "flex w-full items-center gap-3 rounded-lg p-2 text-left transition-colors hover:bg-surface-hover",
                open && "bg-surface-hover",
              )}
            >
              <Avatar name={principal.email} size="md" />
              <span className="min-w-0 flex-1">
                <span className="block truncate text-sm font-medium text-ink">{displayName}</span>
                <span className="block truncate text-xs text-ink-faint">{principal.roles.join(", ") || "member"}</span>
              </span>
              <Icon name="chevrons-up-down" size={14} className="text-ink-faint" />
            </button>
          )}
          items={[
            { kind: "label", label: principal.email },
            { label: "Profile", icon: "user", onSelect: () => router.push("/settings/profile") },
            { label: "Preferences", icon: "sliders", onSelect: () => router.push("/settings/preferences") },
            { label: "Settings", icon: "settings", onSelect: () => router.push("/settings/general") },
            { kind: "separator" },
            { label: "Documentation", icon: "book", onSelect: () => window.open("https://docs.edgepilot.dev", "_blank", "noopener") },
            { label: "Contact support", icon: "mail", onSelect: () => window.open("mailto:support@visionify.ai") },
            { kind: "separator" },
            { label: "Sign out", icon: "log-out", onSelect: () => void doSignOut() },
          ]}
        />
        <ThemeSwitch />
      </div>
    </div>
  );
}

/** Light / dark / system, in the sidebar where it is always one click away. */
function ThemeSwitch() {
  const [pref, setPref] = useState<ThemePreference>("system");
  useEffect(() => {
    setPref(readTheme());
    const onChange = (e: Event) => setPref((e as CustomEvent<ThemePreference>).detail);
    window.addEventListener("edgepilot:theme", onChange);
    return () => window.removeEventListener("edgepilot:theme", onChange);
  }, []);

  const options: { value: ThemePreference; icon: IconName; label: string }[] = [
    { value: "light", icon: "sun", label: "Light" },
    { value: "dark", icon: "moon", label: "Dark" },
    { value: "system", icon: "monitor", label: "System" },
  ];

  return (
    <div role="radiogroup" aria-label="Theme" className="mt-2 grid grid-cols-3 gap-1 rounded-lg bg-surface-sunken p-1">
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          role="radio"
          aria-checked={pref === o.value}
          title={`${o.label} theme`}
          onClick={() => setTheme(o.value)}
          className={cn(
            "flex h-7 items-center justify-center gap-1.5 rounded-md text-xs font-medium transition-all",
            pref === o.value ? "bg-surface-raised text-ink shadow-sm ring-1 ring-surface-border" : "text-ink-faint hover:text-ink-muted",
          )}
        >
          <Icon name={o.icon} size={13} />
          <span className="hidden xl:inline">{o.label}</span>
        </button>
      ))}
    </div>
  );
}

const KIND_ICON: Record<Notification["kind"], { name: IconName; className: string }> = {
  device: { name: "monitor", className: "text-ink-muted" },
  security: { name: "shield", className: "text-degraded" },
  user: { name: "user", className: "text-accent-soft" },
  system: { name: "zap", className: "text-ink-muted" },
};

function NotificationsMenu({ align = "end" }: { align?: "start" | "end" }) {
  const [items, setItems] = useState<Notification[]>([]);
  const router = useRouter();
  useEffect(() => {
    void listNotifications().then(setItems);
  }, []);
  const unread = items.filter((n) => !n.read).length;

  return (
    <Popover
      width="w-[22rem]"
      align={align}
      trigger={({ open, toggle }) => (
        <button
          type="button"
          onClick={toggle}
          aria-label={unread ? `Notifications, ${unread} unread` : "Notifications"}
          aria-expanded={open}
          className={cn(
            "relative flex h-9 w-9 items-center justify-center rounded-lg text-ink-muted hover:bg-surface-hover hover:text-ink",
            open && "bg-surface-hover text-ink",
          )}
        >
          <Icon name="bell" />
          {unread > 0 && (
            <span className="absolute right-2 top-2 h-2 w-2 rounded-full bg-accent ring-2 ring-surface-raised" aria-hidden />
          )}
        </button>
      )}
    >
      {(close) => (
        <div>
          <div className="flex items-center justify-between border-b border-surface-border px-4 py-3">
            <span className="text-sm font-semibold text-ink">Notifications</span>
            {unread > 0 && (
              <Button size="sm" variant="ghost" onClick={() => setItems((current) => current.map((n) => ({ ...n, read: true })))}>
                Mark all read
              </Button>
            )}
          </div>
          <ul className="max-h-96 overflow-y-auto">
            {items.length === 0 && (
              <li className="flex flex-col items-center px-6 py-10 text-center">
                <span className="mb-3 flex h-10 w-10 items-center justify-center rounded-xl bg-accent-subtle text-accent">
                  <Icon name="inbox" />
                </span>
                <span className="text-sm font-medium text-ink">You&apos;re all caught up</span>
                <span className="mt-1 text-xs text-ink-faint">Alerts about your fleet will show up here.</span>
              </li>
            )}
            {items.map((n) => {
              const icon = KIND_ICON[n.kind];
              return (
                <li key={n.id}>
                  <button
                    type="button"
                    onClick={() => {
                      close();
                      if (n.href) router.push(n.href);
                    }}
                    className="flex w-full gap-3 border-b border-surface-border px-4 py-3 text-left last:border-0 hover:bg-surface-hover"
                  >
                    <Icon name={icon.name} className={cn("mt-0.5", icon.className)} />
                    <span className="min-w-0 flex-1">
                      <span className={cn("block truncate text-sm", n.read ? "text-ink-muted" : "font-medium text-ink")}>
                        {n.title}
                      </span>
                      <span className="mt-0.5 block text-xs text-ink-faint">{n.body}</span>
                    </span>
                    <span className="shrink-0 text-2xs text-ink-faint">{relativeTime(n.at)}</span>
                  </button>
                </li>
              );
            })}
          </ul>
          <div className="flex items-center justify-between border-t border-surface-border bg-surface-sunken/60 px-4 py-2 text-xs text-ink-faint">
            <span>All systems operational</span>
            <Badge tone="success" dot>Live</Badge>
          </div>
        </div>
      )}
    </Popover>
  );
}
