"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useMemo } from "react";
import { Badge, Button, Callout, Card, ErrorNote, Icon, Skeleton } from "@/components/ui";
import { useAsync } from "@/hooks/use-async";
import { listPermissions, listRoles, type Permission } from "@/lib/api/admin";
import { RESOURCE_LABEL, RESOURCE_ORDER, humanize } from "@/lib/labels";
import { cn } from "@/lib/cn";

/**
 * The permission matrix for one role, read-only.
 *
 * Grouped by resource, so "what can an Operator do to files" is one glance.
 * Elevated permissions carry a note, because "device:delete" reads as
 * bookkeeping until someone learns it decommissions customer hardware.
 */
export default function RolePage() {
  const { slug } = useParams<{ slug: string }>();
  const router = useRouter();
  const roles = useAsync(() => listRoles(), []);
  const catalogue = useAsync(() => listPermissions(), []);
  const role = roles.data?.find((r) => r.slug === slug);

  const groups = useMemo(() => {
    const map = new Map<string, Permission[]>();
    for (const p of catalogue.data ?? []) map.set(p.resource, [...(map.get(p.resource) ?? []), p]);
    return [...map.entries()].sort(([a], [b]) => RESOURCE_ORDER.indexOf(a) - RESOURCE_ORDER.indexOf(b));
  }, [catalogue.data]);

  if (roles.error) return <ErrorNote error={roles.error} onRetry={() => void roles.reload()} />;
  if (!roles.loading && !role) {
    return (
      <Callout tone="warning" title="No such role">
        There is no role called <code className="font-mono">{slug}</code>. <Link href="/settings/roles" className="text-accent-soft hover:underline">Back to roles</Link>.
      </Callout>
    );
  }
  if (!role || !catalogue.data) {
    return (
      <div role="status" aria-label="Loading">
        <Skeleton className="mb-3 h-3 w-24" />
        <Skeleton className="mb-2 h-6 w-56" />
        <Skeleton className="mb-8 h-3.5 w-96" />
        <Card><div className="space-y-3">{[0, 1, 2, 3, 4, 5].map((i) => <Skeleton key={i} className="h-8 w-full" />)}</div></Card>
      </div>
    );
  }

  const granted = new Set(role.permissions);

  return (
    <>
      <div className="mb-6">
        <Link href="/settings/roles" className="mb-3 inline-flex items-center gap-1 text-xs text-ink-muted hover:text-ink">
          <Icon name="arrow-left" size={12} /> Roles & Permissions
        </Link>
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <h1 className="text-xl font-semibold tracking-tight">{role.name}</h1>
              {role.is_system ? <Badge tone="neutral">Built-in</Badge> : <Badge tone="accent">Custom</Badge>}
            </div>
            <p className="mt-1 max-w-2xl text-sm text-ink-muted">{role.description}</p>
            <p className="mt-1 text-xs text-ink-faint">
              {role.user_count} {role.user_count === 1 ? "user" : "users"} · {role.permissions.length} of {catalogue.data.length} permissions · slug <code className="font-mono">{role.slug}</code>
            </p>
          </div>
          <Button variant="ghost" icon="users" onClick={() => router.push(`/settings/users?role=${role.slug}`)}>View users</Button>
        </div>
      </div>

      <Card padded={false} className="overflow-hidden">
        <div className="grid grid-cols-[auto_1fr_auto] items-center gap-3 border-b border-surface-border bg-surface-raised px-4 py-2 text-xs font-medium text-ink-muted">
          <span className="w-4" />
          <span>Permission</span>
          <span className="tabular">{granted.size} / {catalogue.data.length}</span>
        </div>
        <div className="divide-y divide-surface-border">
          {groups.map(([resource, list]) => {
            const on = list.filter((p) => granted.has(p.slug)).length;
            return (
              <div key={resource}>
                <div className="grid grid-cols-[auto_1fr_auto] items-center gap-3 bg-surface-raised/60 px-4 py-2.5">
                  <Icon name={on === list.length ? "check-circle" : on > 0 ? "info" : "x-circle"} size={16} className={on === list.length ? "text-online" : on > 0 ? "text-ink-muted" : "text-ink-faint/60"} />
                  <span className="text-sm font-semibold text-ink">{RESOURCE_LABEL[resource] ?? humanize(resource)}</span>
                  <span className="text-xs tabular text-ink-faint">{on} / {list.length}</span>
                </div>
                <ul>
                  {list.map((p) => {
                    const has = granted.has(p.slug);
                    const elevated = ["delete", "destructive", "custom_script", "release"].includes(p.action) || p.slug === "role:write" || p.slug === "org:write";
                    return (
                      <li key={p.slug} className="grid grid-cols-[auto_1fr_auto] items-start gap-3 px-4 py-2 pl-10">
                        <Icon name={has ? "check" : "x"} size={14} className={cn("mt-0.5", has ? "text-online" : "text-ink-faint/60")} />
                        <span className="min-w-0">
                          <span className={cn("flex items-center gap-2 text-sm", has ? "text-ink" : "text-ink-muted")}>
                            {humanize(p.action)}
                            {elevated && <Badge tone="warning" className="h-4">Elevated</Badge>}
                          </span>
                          {p.description && <span className="block text-xs text-ink-faint">{p.description}</span>}
                        </span>
                        <code className="mt-0.5 font-mono text-2xs text-ink-faint">{p.slug}</code>
                      </li>
                    );
                  })}
                </ul>
              </div>
            );
          })}
        </div>
      </Card>
    </>
  );
}
