"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useCallback, useEffect, useState } from "react";
import { can, signOut } from "@/lib/api/client";
import { listNotifications, type Notification } from "@/lib/api/admin";
import { useSession } from "@/components/session-provider";
import {
  Avatar, Badge, Button, Icon, IconButton, Kbd, Menu, Popover, Spinner, type IconName,
} from "@/components/ui";
import { cn } from "@/lib/cn";
import { relativeTime } from "@/lib/format";

const NAV: { href: string; label: string; permission: string }[] = [
  { href: "/", label: "Fleet", permission: "device:read" },
  { href: "/enrollment", label: "Enrollment", permission: "enrollment_token:read" },
  { href: "/policies", label: "Policies", permission: "device:read" },
  { href: "/reports", label: "Reports", permission: "device:read" },
];

function isActive(pathname: string, href: string): boolean {
  if (href === "/") return pathname === "/" || pathname.startsWith("/devices");
  return pathname === href || pathname.startsWith(`${href}/`);
}

/**
 * The signed-in frame.
 *
 * Navigation is filtered by permission, but that is presentation only: the
 * server refuses the call regardless. Hiding a link an operator cannot use is
 * courtesy; it is not the access control, and treating it as one is how a UI
 * ends up being the only thing standing between a viewer and a delete.
 *
 * Settings is not in the primary nav. It is a place operators go on purpose,
 * from the account menu, and putting it beside Fleet would suggest it is
 * somewhere they should be several times a day.
 */
export function AppShell({
  children,
  width = "default",
}: {
  children: React.ReactNode;
  /** Settings pages get a wider frame for their sidebar. */
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

  const doSignOut = useCallback(async () => {
    await signOut();
    router.replace("/login");
  }, [router]);

  if (loading) return <Spinner className="min-h-screen items-center" />;
  if (!principal) return null;

  const links = NAV.filter((item) => can(item.permission));
  const inSettings = pathname.startsWith("/settings");
  const displayName = principal.email.split("@")[0] ?? principal.email;

  return (
    <div className="flex min-h-screen flex-col">
      <header className="sticky top-0 z-30 border-b border-surface-border bg-surface/90 backdrop-blur">
        <div className={cn("mx-auto flex h-12 items-center gap-2 px-4", width === "wide" ? "max-w-[96rem]" : "max-w-[88rem]")}>
          <IconButton
            icon="menu"
            label="Open navigation"
            className="md:hidden"
            onClick={() => setMobileOpen((v) => !v)}
            aria-expanded={mobileOpen}
          />

          <Link href="/" className="flex items-center gap-2 rounded-md py-1 pr-2 text-sm font-semibold tracking-tight text-ink">
            <span className="flex h-5 w-5 items-center justify-center rounded bg-accent text-white">
              <Icon name="grid" size={12} strokeWidth={2.25} />
            </span>
            EdgePilot
          </Link>

          <span className="mx-1 hidden h-4 w-px bg-surface-border md:block" aria-hidden />

          <nav className="hidden items-center gap-0.5 md:flex" aria-label="Main">
            {links.map((item) => {
              const active = !inSettings && isActive(pathname, item.href);
              return (
                <Link
                  key={item.href}
                  href={item.href}
                  aria-current={active ? "page" : undefined}
                  className={cn(
                    "relative rounded-md px-2.5 py-1.5 text-sm font-medium transition-colors",
                    active ? "text-ink" : "text-ink-muted hover:text-ink",
                  )}
                >
                  {item.label}
                  {active && (
                    <span className="absolute inset-x-2.5 -bottom-[11px] h-px bg-ink" aria-hidden />
                  )}
                </Link>
              );
            })}
            {inSettings && (
              <span className="relative rounded-md px-2.5 py-1.5 text-sm font-medium text-ink">
                Settings
                <span className="absolute inset-x-2.5 -bottom-[11px] h-px bg-ink" aria-hidden />
              </span>
            )}
          </nav>

          <div className="ml-auto flex items-center gap-1">
            <NotificationsMenu />
            <HelpMenu />
            <Menu
              width="w-64"
              trigger={({ open, toggle }) => (
                <button
                  type="button"
                  onClick={toggle}
                  aria-haspopup="menu"
                  aria-expanded={open}
                  className={cn(
                    "ml-1 flex h-8 items-center gap-2 rounded-md border border-transparent pl-1 pr-2 text-sm hover:bg-surface-hover",
                    open && "bg-surface-hover",
                  )}
                >
                  <Avatar name={principal.email} size="sm" />
                  <span className="hidden max-w-[10rem] truncate text-ink-muted sm:block">{displayName}</span>
                  <Icon name="chevron-down" size={14} className="text-ink-faint" />
                </button>
              )}
              items={[
                { kind: "label", label: principal.email },
                { label: "Profile", icon: "user", onSelect: () => router.push("/settings/profile") },
                { label: "Preferences", icon: "sliders", onSelect: () => router.push("/settings/preferences") },
                { label: "Settings", icon: "settings", onSelect: () => router.push("/settings/general"), hint: "G S" },
                { kind: "separator" },
                { label: "Documentation", icon: "book", onSelect: () => window.open("https://docs.edgepilot.dev", "_blank", "noopener") },
                { kind: "separator" },
                { label: "Sign out", icon: "log-out", onSelect: () => void doSignOut() },
              ]}
            />
          </div>
        </div>

        {mobileOpen && (
          <nav className="border-t border-surface-border bg-surface px-3 py-2 md:hidden" aria-label="Main">
            {links.map((item) => (
              <Link
                key={item.href}
                href={item.href}
                className={cn(
                  "block rounded-md px-3 py-2 text-sm font-medium",
                  !inSettings && isActive(pathname, item.href) ? "bg-surface-hover text-ink" : "text-ink-muted",
                )}
              >
                {item.label}
              </Link>
            ))}
            <Link
              href="/settings/general"
              className={cn(
                "block rounded-md px-3 py-2 text-sm font-medium",
                inSettings ? "bg-surface-hover text-ink" : "text-ink-muted",
              )}
            >
              Settings
            </Link>
          </nav>
        )}
      </header>

      <main className={cn("mx-auto w-full flex-1 px-4 py-6", width === "wide" ? "max-w-[96rem]" : "max-w-[88rem]")}>
        {children}
      </main>
    </div>
  );
}

const KIND_ICON: Record<Notification["kind"], { name: IconName; className: string }> = {
  device: { name: "monitor", className: "text-ink-muted" },
  security: { name: "shield", className: "text-degraded" },
  user: { name: "user", className: "text-accent-soft" },
  system: { name: "zap", className: "text-ink-muted" },
};

function NotificationsMenu() {
  const [items, setItems] = useState<Notification[]>([]);
  const router = useRouter();
  useEffect(() => {
    void listNotifications().then(setItems);
  }, []);
  const unread = items.filter((n) => !n.read).length;

  return (
    <Popover
      width="w-96"
      trigger={({ open, toggle }) => (
        <button
          type="button"
          onClick={toggle}
          aria-label={unread ? `Notifications, ${unread} unread` : "Notifications"}
          aria-expanded={open}
          className={cn(
            "relative flex h-8 w-8 items-center justify-center rounded-md text-ink-muted hover:bg-surface-hover hover:text-ink",
            open && "bg-surface-hover text-ink",
          )}
        >
          <Icon name="bell" />
          {unread > 0 && (
            <span className="absolute right-1.5 top-1.5 h-1.5 w-1.5 rounded-full bg-accent ring-2 ring-surface" aria-hidden />
          )}
        </button>
      )}
    >
      {(close) => (
        <div>
          <div className="flex items-center justify-between border-b border-surface-border px-3 py-2">
            <span className="text-sm font-medium text-ink">Notifications</span>
            {unread > 0 && (
              <Button size="sm" variant="ghost" onClick={() => setItems((current) => current.map((n) => ({ ...n, read: true })))}>
                Mark all read
              </Button>
            )}
          </div>
          <ul className="max-h-96 overflow-y-auto">
            {items.length === 0 && (
              <li className="px-3 py-8 text-center text-sm text-ink-muted">
                Nothing to show. Notifications need an alert engine this server does not have yet.
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
                    className="flex w-full gap-3 border-b border-surface-border px-3 py-2.5 text-left last:border-0 hover:bg-surface-hover"
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
        </div>
      )}
    </Popover>
  );
}

function HelpMenu() {
  return (
    <Popover
      width="w-72"
      trigger={({ open, toggle }) => (
        <button
          type="button"
          onClick={toggle}
          aria-label="Help"
          aria-expanded={open}
          className={cn(
            "flex h-8 w-8 items-center justify-center rounded-md text-ink-muted hover:bg-surface-hover hover:text-ink",
            open && "bg-surface-hover text-ink",
          )}
        >
          <Icon name="help" />
        </button>
      )}
    >
      <div className="p-1">
        {[
          { label: "Documentation", icon: "book" as IconName, href: "https://docs.edgepilot.dev" },
          { label: "API reference", icon: "file-text" as IconName, href: "https://docs.edgepilot.dev/api" },
          { label: "Contact support", icon: "mail" as IconName, href: "mailto:support@visionify.ai" },
        ].map((item) => (
          <a
            key={item.label}
            href={item.href}
            target="_blank"
            rel="noopener"
            className="flex items-center gap-2 rounded px-2 py-1.5 text-sm text-ink hover:bg-surface-hover"
          >
            <Icon name={item.icon} size={14} className="text-ink-muted" />
            <span className="flex-1">{item.label}</span>
            <Icon name="external" size={12} className="text-ink-faint" />
          </a>
        ))}
        <div className="my-1 border-t border-surface-border" />
        <div className="px-2 py-1.5 text-xs text-ink-faint">
          <div className="mb-1.5 font-medium text-ink-muted">Keyboard</div>
          <div className="flex items-center justify-between py-0.5"><span>Search fleet</span><Kbd>/</Kbd></div>
          <div className="flex items-center justify-between py-0.5"><span>Settings</span><span className="flex gap-1"><Kbd>G</Kbd><Kbd>S</Kbd></span></div>
          <div className="flex items-center justify-between py-0.5"><span>Close panel</span><Kbd>Esc</Kbd></div>
        </div>
        <div className="my-1 border-t border-surface-border" />
        <div className="flex items-center justify-between px-2 py-1.5 text-xs text-ink-faint">
          <span>All systems operational</span>
          <Badge tone="success" dot>Live</Badge>
        </div>
      </div>
    </Popover>
  );
}
