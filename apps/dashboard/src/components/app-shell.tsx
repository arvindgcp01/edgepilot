"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useEffect } from "react";
import { can, signOut } from "@/lib/api/client";
import { useSession } from "@/components/session-provider";
import { Button, Spinner } from "@/components/ui";
import { cn } from "@/lib/cn";

const NAV = [
  { href: "/", label: "Fleet", permission: "device:read" },
  { href: "/enrollment", label: "Enrollment", permission: "enrollment_token:read" },
];

/**
 * The signed-in frame.
 *
 * Navigation is filtered by permission, but that is presentation only: the
 * server refuses the call regardless. Hiding a link an operator cannot use is
 * courtesy; it is not the access control, and treating it as one is how a UI
 * ends up being the only thing standing between a viewer and a delete.
 */
export function AppShell({ children }: { children: React.ReactNode }) {
  const { principal, loading } = useSession();
  const router = useRouter();
  const pathname = usePathname();

  useEffect(() => {
    if (!loading && !principal) router.replace("/login");
  }, [loading, principal, router]);

  if (loading) return <Spinner />;
  if (!principal) return null;

  return (
    <div className="min-h-screen">
      <header className="border-b border-surface-border">
        <div className="mx-auto flex max-w-7xl items-center gap-6 px-4 py-3">
          <Link href="/" className="text-sm font-semibold tracking-tight">
            EdgePilot
          </Link>

          <nav className="flex gap-1" aria-label="Main">
            {NAV.filter((item) => can(item.permission)).map((item) => (
              <Link
                key={item.href}
                href={item.href}
                aria-current={pathname === item.href ? "page" : undefined}
                className={cn(
                  "rounded-md px-3 py-1.5 text-sm",
                  pathname === item.href
                    ? "bg-surface-raised text-ink"
                    : "text-ink-muted hover:text-ink",
                )}
              >
                {item.label}
              </Link>
            ))}
          </nav>

          <div className="ml-auto flex items-center gap-3">
            <span className="text-sm text-ink-muted">{principal.email}</span>
            <Button
              variant="ghost"
              onClick={async () => {
                await signOut();
                router.replace("/login");
              }}
            >
              Sign out
            </Button>
          </div>
        </div>
      </header>

      <main className="mx-auto max-w-7xl px-4 py-6">{children}</main>
    </div>
  );
}
