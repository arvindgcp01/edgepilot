"use client";

import { AppShell } from "@/components/app-shell";
import { SettingsNav } from "@/components/settings-nav";

/**
 * Settings is one frame with many pages, not one page with many sections. The
 * rail on the left stays put; only the column on the right changes. A single
 * scrolling page would put Billing three screens below Users and make the
 * browser's back button useless for undoing a wrong turn.
 */
export default function SettingsLayout({ children }: { children: React.ReactNode }) {
  return (
    <AppShell width="wide">
      <div className="lg:flex lg:gap-10">
        <SettingsNav />
        <div className="min-w-0 flex-1">
          <div className="mx-auto max-w-6xl">{children}</div>
        </div>
      </div>
    </AppShell>
  );
}
