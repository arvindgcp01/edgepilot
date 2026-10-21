"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { Icon, type IconName } from "@/components/ui";
import { cn } from "@/lib/cn";

export type SettingsSection = { href: string; label: string; icon: IconName; permission?: string };

/**
 * The settings map. Grouped by who touches it: the operator's own account, the
 * organization's identity, who can get in, how they are kept out, and the
 * plumbing. An administrator looking for "where do I turn on MFA" should find
 * it by reading the group headings alone.
 */
export const SETTINGS_GROUPS: { label: string; items: SettingsSection[] }[] = [
  {
    label: "Account",
    items: [
      { href: "/settings/profile", label: "Profile", icon: "user" },
      { href: "/settings/preferences", label: "Preferences", icon: "sliders" },
    ],
  },
  {
    label: "General",
    items: [
      { href: "/settings/general", label: "General", icon: "settings" },
      { href: "/settings/organization", label: "Organization", icon: "building" },
    ],
  },
  {
    label: "Access",
    items: [
      { href: "/settings/users", label: "Users", icon: "users" },
      { href: "/settings/teams", label: "Teams", icon: "layers" },
      { href: "/settings/roles", label: "Roles & Permissions", icon: "key" },
    ],
  },
  {
    label: "Security",
    items: [
      { href: "/settings/sso", label: "SSO / Authentication", icon: "fingerprint" },
      { href: "/settings/security", label: "Security", icon: "shield" },
      { href: "/settings/audit-logs", label: "Audit Logs", icon: "file-text" },
    ],
  },
  {
    label: "System",
    items: [
      { href: "/settings/notifications", label: "Notifications", icon: "bell" },
      { href: "/settings/integrations", label: "API / Integrations", icon: "link" },
      { href: "/settings/billing", label: "Billing", icon: "credit-card" },
      { href: "/settings/advanced", label: "Advanced", icon: "server" },
    ],
  },
];

export function currentSection(pathname: string): SettingsSection | undefined {
  for (const group of SETTINGS_GROUPS) {
    for (const item of group.items) {
      if (pathname === item.href || pathname.startsWith(`${item.href}/`)) return item;
    }
  }
  return undefined;
}

export function SettingsNav() {
  const pathname = usePathname();
  return (
    <>
      {/* Desktop: a fixed rail with group headings. */}
      <nav aria-label="Settings" className="hidden w-52 shrink-0 lg:block">
        <div className="sticky top-[4.5rem] space-y-5">
          {SETTINGS_GROUPS.map((group) => (
            <div key={group.label}>
              <div className="mb-1 px-2 text-2xs font-semibold uppercase tracking-wider text-ink-faint">
                {group.label}
              </div>
              <ul className="space-y-px">
                {group.items.map((item) => {
                  const active = pathname === item.href || pathname.startsWith(`${item.href}/`);
                  return (
                    <li key={item.href}>
                      <Link
                        href={item.href}
                        aria-current={active ? "page" : undefined}
                        className={cn(
                          "flex items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-sm transition-colors",
                          active
                            ? "bg-accent-subtle font-medium text-accent dark:text-accent-soft"
                            : "text-ink-muted hover:bg-surface-hover hover:text-ink",
                        )}
                      >
                        <Icon name={item.icon} size={15} className={active ? "text-accent dark:text-accent-soft" : "text-ink-faint"} />
                        {item.label}
                      </Link>
                    </li>
                  );
                })}
              </ul>
            </div>
          ))}
        </div>
      </nav>

      {/* Below lg: the same map as a scrolling strip. Headings go; the icons
          carry the grouping well enough at this width. */}
      <nav
        aria-label="Settings"
        className="-mx-4 mb-4 flex gap-1 overflow-x-auto border-b border-surface-border px-4 pb-2 lg:hidden"
      >
        {SETTINGS_GROUPS.flatMap((group) => group.items).map((item) => {
          const active = pathname === item.href || pathname.startsWith(`${item.href}/`);
          return (
            <Link
              key={item.href}
              href={item.href}
              aria-current={active ? "page" : undefined}
              className={cn(
                "flex shrink-0 items-center gap-1.5 rounded-md px-2.5 py-1.5 text-sm whitespace-nowrap",
                active ? "bg-accent-subtle font-medium text-accent dark:text-accent-soft" : "text-ink-muted hover:text-ink",
              )}
            >
              <Icon name={item.icon} size={14} className={active ? "text-ink" : "text-ink-faint"} />
              {item.label}
            </Link>
          );
        })}
      </nav>
    </>
  );
}
