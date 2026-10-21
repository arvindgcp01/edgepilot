"use client";

import { useRouter } from "next/navigation";
import { Badge, Button, Callout, Card, ErrorNote, IconButton, Menu, PageHeader, Table, TableSkeleton, TBody, Td, Th, THead, Tr, useToast } from "@/components/ui";
import { useAsync } from "@/hooks/use-async";
import { listPermissions, listRoles, type Role } from "@/lib/api/admin";
import { RESOURCE_LABEL, RESOURCE_ORDER, humanize } from "@/lib/labels";

export default function RolesPage() {
  const router = useRouter();
  const { push } = useToast();
  const roles = useAsync(() => listRoles(), []);
  const permissions = useAsync(() => listPermissions(), []);
  const total = permissions.data?.length ?? 0;

  /** "Fleet, Commands, +3" — the resources a role touches. */
  function summary(role: Role): string {
    const resources = [...new Set(role.permissions.map((slug) => slug.split(":")[0] ?? slug))]
      .sort((a, b) => RESOURCE_ORDER.indexOf(a) - RESOURCE_ORDER.indexOf(b))
      .map((r) => RESOURCE_LABEL[r] ?? humanize(r));
    if (resources.length === 0) return "No permissions";
    return resources.length <= 3 ? resources.join(", ") : `${resources.slice(0, 3).join(", ")} +${resources.length - 3}`;
  }

  return (
    <>
      <PageHeader
        title="Roles & Permissions"
        description="A role is a named bundle of permissions. Every user holds at least one; what they can do is the union of their roles."
        actions={<Button variant="primary" icon="plus" onClick={() => push({ title: "Custom roles are not available on this server", description: "The three built-in roles are what can be assigned today.", tone: "warning" })}>Create custom role</Button>}
      />

      <Callout tone="info" className="mb-4">
        Built-in roles are shared by every organization and are read-only. Custom roles need a server-side editor that does not exist yet; until then, assignments are the lever.
      </Callout>

      <ErrorNote error={roles.error} onRetry={() => void roles.reload()} />

      <Card padded={false} className="overflow-hidden">
        {roles.loading ? (
          <TableSkeleton rows={3} columns={5} />
        ) : (
          <Table>
            <THead>
              <tr>
                <Th>Role</Th>
                <Th className="hidden md:table-cell">Description</Th>
                <Th align="right">Users</Th>
                <Th>Permissions</Th>
                <Th align="right"><span className="sr-only">Actions</span></Th>
              </tr>
            </THead>
            <TBody>
              {(roles.data ?? []).map((role) => (
                <Tr key={role.slug} interactive onClick={() => router.push(`/settings/roles/${role.slug}`)}>
                  <Td>
                    <div className="flex items-center gap-2">
                      <span className="font-medium text-ink">{role.name}</span>
                      {role.is_system ? <Badge tone="neutral">Built-in</Badge> : <Badge tone="accent">Custom</Badge>}
                    </div>
                    <div className="mt-0.5 max-w-md truncate text-xs text-ink-muted md:hidden">{role.description}</div>
                  </Td>
                  <Td className="hidden max-w-md text-ink-muted md:table-cell"><span className="line-clamp-2">{role.description}</span></Td>
                  <Td align="right" className="tabular text-ink-muted">{role.user_count}</Td>
                  <Td>
                    <div className="flex items-center gap-3">
                      <div className="h-1.5 w-20 overflow-hidden rounded-full bg-surface-border" aria-hidden>
                        <div className="h-full rounded-full bg-accent/70" style={{ width: total ? `${(role.permissions.length / total) * 100}%` : 0 }} />
                      </div>
                      <div className="min-w-0">
                        <div className="text-sm tabular text-ink">{role.permissions.length}<span className="text-ink-faint"> / {total}</span></div>
                        <div className="truncate text-xs text-ink-faint">{summary(role)}</div>
                      </div>
                    </div>
                  </Td>
                  <Td align="right" onClick={(e) => e.stopPropagation()}>
                    <Menu
                      trigger={({ toggle, open }) => <IconButton icon="more" label={`Actions for ${role.name}`} size="sm" onClick={toggle} aria-expanded={open} />}
                      items={[
                        { label: "View permissions", icon: "eye", onSelect: () => router.push(`/settings/roles/${role.slug}`) },
                        { label: "View users with this role", icon: "users", onSelect: () => router.push(`/settings/users?role=${role.slug}`) },
                      ]}
                    />
                  </Td>
                </Tr>
              ))}
            </TBody>
          </Table>
        )}
      </Card>
    </>
  );
}
