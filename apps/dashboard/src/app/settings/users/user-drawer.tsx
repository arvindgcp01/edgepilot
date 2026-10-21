"use client";

import { useRouter } from "next/navigation";
import { useEffect, useMemo, useState } from "react";
import { Avatar, Badge, Button, Code, Drawer, ErrorNote, Facts, Field, Icon, Input, Tabs, useToast } from "@/components/ui";
import { useAsync } from "@/hooks/use-async";
import { listPermissions, updateUser, type OrgUser, type Role } from "@/lib/api/admin";
import { can } from "@/lib/api/client";
import { humanize, RESOURCE_LABEL, RESOURCE_ORDER, roleLabel, USER_STATUS } from "@/lib/labels";
import { absoluteTime, relativeTime } from "@/lib/format";
import { cn } from "@/lib/cn";

type Tab = "overview" | "permissions";

/**
 * One user, in a drawer.
 *
 * Two tabs, because the server has two things to say about a user: who they
 * are, and what their roles let them do. Sessions and activity would be a
 * third and fourth; they arrive when the server can list them.
 */
export function UserDrawer({
  user: u,
  roles,
  onClose,
  onChanged,
  onChangeRole,
  onSuspend,
  onReactivate,
  onRemove,
}: {
  user: OrgUser | null;
  roles: Role[];
  onClose: () => void;
  onChanged: () => void;
  onChangeRole: (user: OrgUser) => void;
  onSuspend: (user: OrgUser) => void;
  onReactivate: (user: OrgUser) => void;
  onRemove: (user: OrgUser) => void;
}) {
  const router = useRouter();
  const [tab, setTab] = useState<Tab>("overview");
  const [editing, setEditing] = useState(false);
  const canWrite = can("user:write");

  useEffect(() => {
    setTab("overview");
    setEditing(false);
  }, [u?.id]);

  const status = u ? USER_STATUS[u.status] : null;

  return (
    <Drawer
      open={u !== null}
      onClose={onClose}
      title="User"
      header={
        u && (
          <>
            <div className="flex items-start gap-4 px-5 pt-5">
              <Avatar name={u.full_name || u.email} size="lg" />
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2">
                  <h3 className="truncate text-base font-semibold text-ink">{u.full_name || u.email}</h3>
                  {u.is_current && <Badge tone="accent">You</Badge>}
                </div>
                <div className="truncate text-sm text-ink-muted">{u.email}</div>
                <div className="mt-2 flex flex-wrap items-center gap-1.5">
                  {status && <Badge tone={status.tone} dot>{status.label}</Badge>}
                  {u.roles.map((r) => <Badge key={r} tone="neutral">{roleLabel(r, roles)}</Badge>)}
                </div>
              </div>
            </div>
            <Tabs<Tab> aria-label="User sections" className="mt-4 px-5" value={tab} onChange={setTab} tabs={[{ value: "overview", label: "Overview" }, { value: "permissions", label: "Permissions" }]} />
          </>
        )
      }
      footer={
        u && (
          <>
            <Button size="sm" icon="pencil" disabled={!canWrite} onClick={() => { setTab("overview"); setEditing(true); }}>Edit name</Button>
            <Button size="sm" icon="key" disabled={!canWrite} onClick={() => onChangeRole(u)}>Change role</Button>
            <Button size="sm" variant="ghost" icon="shield" onClick={() => router.push(`/settings/roles/${u.roles[0] ?? "viewer"}`)}>Role permissions</Button>
            <span className="mx-1 h-5 w-px bg-surface-border" aria-hidden />
            <div className="ml-auto flex gap-2">
              {u.status === "disabled" ? (
                <Button size="sm" icon="check-circle" disabled={!canWrite} onClick={() => onReactivate(u)}>Reactivate</Button>
              ) : (
                <Button size="sm" variant="danger" disabled={!canWrite || u.is_current} onClick={() => onSuspend(u)}>Suspend</Button>
              )}
              <Button size="sm" variant="danger" disabled={!canWrite || u.is_current} onClick={() => onRemove(u)}>Remove</Button>
            </div>
          </>
        )
      }
    >
      {u && tab === "overview" && (editing ? <EditForm user={u} onCancel={() => setEditing(false)} onSaved={() => { setEditing(false); onChanged(); }} /> : <Overview user={u} roles={roles} />)}
      {u && tab === "permissions" && <Permissions user={u} roles={roles} />}
    </Drawer>
  );
}

function SectionTitle({ children }: { children: React.ReactNode }) {
  return <h4 className="mb-3 text-xs font-semibold uppercase tracking-wider text-ink-faint">{children}</h4>;
}

function Overview({ user, roles }: { user: OrgUser; roles: Role[] }) {
  return (
    <div className="space-y-6 p-5">
      <section>
        <SectionTitle>Profile</SectionTitle>
        <Facts rows={[
          { label: "Full name", value: user.full_name || <span className="text-ink-faint">Not set</span> },
          { label: "Email", value: user.email },
          { label: "Member since", value: absoluteTime(user.created_at) },
          { label: "User ID", value: <Code>{user.id}</Code> },
        ]} />
      </section>
      <section>
        <SectionTitle>Access</SectionTitle>
        <Facts rows={[
          { label: "Roles", value: user.roles.map((r) => roleLabel(r, roles)).join(", ") || "—" },
          { label: "Device access", value: "All devices in the organization" },
        ]} />
      </section>
      <section>
        <SectionTitle>Authentication</SectionTitle>
        <Facts rows={[
          { label: "Method", value: user.mfa_enabled ? "Password + authenticator app" : "Password" },
          { label: "MFA", value: user.mfa_enabled ? <span className="text-online">Enrolled</span> : <span className="text-degraded">Not enrolled</span> },
          { label: "Last sign-in", value: user.last_login_at ? `${relativeTime(user.last_login_at)} · ${absoluteTime(user.last_login_at)}` : "Never signed in" },
          { label: "Status", value: <Badge tone={USER_STATUS[user.status].tone} dot>{USER_STATUS[user.status].label}</Badge> },
        ]} />
      </section>
    </div>
  );
}

function EditForm({ user, onCancel, onSaved }: { user: OrgUser; onCancel: () => void; onSaved: () => void }) {
  const { push } = useToast();
  const [fullName, setFullName] = useState(user.full_name);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  return (
    <form
      className="space-y-4 p-5"
      onSubmit={async (e) => {
        e.preventDefault();
        setBusy(true);
        setError(null);
        try {
          await updateUser(user.id, { full_name: fullName.trim() });
          push({ title: "User updated", tone: "success" });
          onSaved();
        } catch (err) {
          setError(err);
        } finally {
          setBusy(false);
        }
      }}
    >
      <Field label="Full name" htmlFor="edit-name"><Input id="edit-name" value={fullName} onChange={(e) => setFullName(e.target.value)} autoFocus maxLength={200} /></Field>
      <p className="text-xs text-ink-faint">Email addresses cannot be changed; remove the account and add it under the new address.</p>
      <ErrorNote error={error} />
      <div className="flex justify-end gap-2 pt-2">
        <Button variant="ghost" onClick={onCancel} disabled={busy}>Cancel</Button>
        <Button variant="primary" type="submit" loading={busy}>Save</Button>
      </div>
    </form>
  );
}

function Permissions({ user, roles }: { user: OrgUser; roles: Role[] }) {
  const perms = useAsync(() => listPermissions(), []);
  const granted = useMemo(() => {
    const set = new Set<string>();
    for (const slug of user.roles) for (const p of roles.find((r) => r.slug === slug)?.permissions ?? []) set.add(p);
    return set;
  }, [user.roles, roles]);

  if (perms.loading) return <p className="p-5 text-sm text-ink-muted">Loading…</p>;
  const byResource = new Map<string, typeof perms.data>();
  for (const p of perms.data ?? []) byResource.set(p.resource, [...(byResource.get(p.resource) ?? []), p]);
  const resources = [...byResource.keys()].sort((a, b) => RESOURCE_ORDER.indexOf(a) - RESOURCE_ORDER.indexOf(b));

  return (
    <div className="p-5">
      <p className="mb-4 text-sm text-ink-muted">
        Resolved from <span className="text-ink">{user.roles.map((r) => roleLabel(r, roles)).join(", ") || "no role"}</span>. {granted.size} of {perms.data?.length ?? 0} permissions. Permissions belong to the role, not the user.
      </p>
      <div className="space-y-4">
        {resources.map((resource) => {
          const list = byResource.get(resource) ?? [];
          const count = list.filter((p) => granted.has(p.slug)).length;
          return (
            <div key={resource}>
              <div className="mb-1.5 flex items-center justify-between">
                <span className="text-sm font-medium text-ink">{RESOURCE_LABEL[resource] ?? humanize(resource)}</span>
                <span className="text-xs tabular text-ink-faint">{count}/{list.length}</span>
              </div>
              <ul className="grid gap-1 sm:grid-cols-2">
                {list.map((p) => {
                  const has = granted.has(p.slug);
                  return (
                    <li key={p.slug} className={cn("flex items-center gap-2 text-sm", has ? "text-ink" : "text-ink-faint")} title={p.description}>
                      <Icon name={has ? "check" : "x"} size={13} className={has ? "text-online" : "text-ink-faint/60"} />
                      {humanize(p.action)}
                    </li>
                  );
                })}
              </ul>
            </div>
          );
        })}
      </div>
    </div>
  );
}
