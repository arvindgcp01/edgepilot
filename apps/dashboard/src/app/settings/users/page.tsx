"use client";

import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useMemo, useState } from "react";
import {
  Avatar, Badge, BulkBar, Button, Callout, Card, Code, ConfirmDialog, EmptyState, ErrorNote, Field,
  IconButton, Input, Menu, Modal, PageHeader, Pagination, SearchInput, Select, SelectionCheckbox,
  SortableTh, StatStrip, Table, TableSkeleton, TBody, Td, Th, THead, Tr, useToast, usePagination,
  useSelection, useSorted, type SortState,
} from "@/components/ui";
import { useAsync } from "@/hooks/use-async";
import { useSession } from "@/components/session-provider";
import {
  countUsers, createUser, listRoles, listUsers, reactivateUser, removeUser, setUserRoles, suspendUser,
  type CreatedUser, type OrgUser, type Role, type UserStatus,
} from "@/lib/api/admin";
import { can } from "@/lib/api/client";
import { roleLabel, USER_STATUS } from "@/lib/labels";
import { relativeTime } from "@/lib/format";
import { cn } from "@/lib/cn";
import { UserDrawer } from "./user-drawer";

type SortKey = "name" | "role" | "status" | "last_active" | "mfa";
type Filters = { q: string; status: UserStatus | "all"; role: string; mfa: "all" | "on" | "off" };
const EMPTY_FILTERS: Filters = { q: "", status: "all", role: "", mfa: "all" };

export default function UsersPage() {
  return (
    <Suspense fallback={null}>
      <UsersView />
    </Suspense>
  );
}

function UsersView() {
  const router = useRouter();
  const params = useSearchParams();
  const { principal } = useSession();
  const { push } = useToast();
  // Presentation only — the server refuses the call regardless. See AppShell.
  const canWrite = can("user:write");

  const [filters, setFilters] = useState<Filters>({ ...EMPTY_FILTERS, role: params.get("role") ?? "" });
  const [sort, setSort] = useState<SortState<SortKey>>({ key: "name", dir: "asc" });
  const [openUser, setOpenUser] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [created, setCreated] = useState<CreatedUser | null>(null);
  const [changingRole, setChangingRole] = useState<OrgUser[] | null>(null);
  const [suspending, setSuspending] = useState<OrgUser[] | null>(null);
  const [removing, setRemoving] = useState<OrgUser[] | null>(null);
  const [busy, setBusy] = useState(false);

  const users = useAsync(() => listUsers(), []);
  const roles = useAsync(() => listRoles(), []);

  const all = useMemo(
    () => (users.data?.data ?? []).map((u) => ({ ...u, is_current: u.id === principal?.user_id })),
    [users.data, principal?.user_id],
  );
  const counts = useMemo(() => countUsers(all), [all]);

  const rows = useMemo(() => {
    const q = filters.q.trim().toLowerCase();
    return all.filter((u) => {
      if (q && !`${u.full_name} ${u.email}`.toLowerCase().includes(q)) return false;
      if (filters.status !== "all" && u.status !== filters.status) return false;
      if (filters.role && !u.roles.includes(filters.role)) return false;
      if (filters.mfa === "on" && !u.mfa_enabled) return false;
      if (filters.mfa === "off" && u.mfa_enabled) return false;
      return true;
    });
  }, [all, filters]);

  const accessors = useMemo(
    () => ({
      name: (u: OrgUser) => u.full_name || u.email,
      role: (u: OrgUser) => roleLabel(u.roles[0] ?? "", roles.data ?? undefined),
      status: (u: OrgUser) => ({ active: 0, invited: 1, disabled: 2, locked: 3 })[u.status],
      last_active: (u: OrgUser) => (u.last_login_at ? -new Date(u.last_login_at).getTime() : Number.MAX_SAFE_INTEGER),
      mfa: (u: OrgUser) => (u.mfa_enabled ? 0 : 1),
    }),
    [roles.data],
  );
  const sorted = useSorted(rows, sort, accessors);
  const paging = usePagination(sorted, 10);
  const selection = useSelection(paging.rows);
  const activeFilterCount = [filters.status !== "all", filters.role !== "", filters.mfa !== "all"].filter(Boolean).length;
  const selectedUsers = paging.rows.filter((u) => selection.isSelected(u.id));

  async function withBusy(fn: () => Promise<void>) {
    setBusy(true);
    try {
      await fn();
    } catch (err) {
      push({ title: "Something went wrong", description: err instanceof Error ? err.message : undefined, tone: "error" });
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <PageHeader
        title="Users"
        description="Administrators manage who belongs to the organization, what they can do, and how they sign in."
        actions={canWrite && <Button variant="primary" icon="plus" onClick={() => setCreating(true)}>Add user</Button>}
      />

      {created && (
        <Callout tone="warning" title={`${created.email} was created — copy the temporary password now`} className="mb-4">
          <p className="mt-0.5">It is shown once and never stored. They sign in with it and change it under Profile.</p>
          <code className="mt-2 block break-all rounded-md border border-surface-border bg-surface px-3 py-2 font-mono text-sm text-ink">{created.temporary_password}</code>
          <div className="mt-2 flex gap-2">
            <Button size="sm" variant="primary" icon="copy" onClick={() => { void navigator.clipboard?.writeText(created.temporary_password); push({ title: "Password copied", tone: "success" }); }}>Copy</Button>
            <Button size="sm" variant="ghost" onClick={() => setCreated(null)}>Dismiss</Button>
          </div>
        </Callout>
      )}

      <StatStrip
        className="mb-5"
        items={[
          { label: "Total users", value: users.data ? counts.total : "—" },
          { label: "Active", value: users.data ? counts.active : "—", tone: "text-online" },
          { label: "Suspended", value: users.data ? counts.disabled : "—", tone: "text-degraded" },
          { label: "MFA enrolled", value: users.data ? all.filter((u) => u.mfa_enabled).length : "—" },
        ]}
      />

      <Card padded={false} className="overflow-hidden">
        <div className="flex flex-wrap items-center gap-2 border-b border-surface-border px-4 py-3">
          <SearchInput className="w-full sm:w-72" placeholder="Search by name or email" value={filters.q} onChange={(e) => { setFilters({ ...filters, q: e.target.value }); paging.reset(); }} aria-label="Search users" />
          <Select aria-label="Status" className="w-auto" value={filters.status} onChange={(e) => { setFilters({ ...filters, status: e.target.value as Filters["status"] }); paging.reset(); }}>
            <option value="all">All statuses</option>
            <option value="active">Active</option>
            <option value="disabled">Suspended</option>
            <option value="locked">Locked</option>
          </Select>
          <Select aria-label="Role" className="w-auto" value={filters.role} onChange={(e) => { setFilters({ ...filters, role: e.target.value }); paging.reset(); }}>
            <option value="">All roles</option>
            {roles.data?.map((r) => <option key={r.slug} value={r.slug}>{r.name}</option>)}
          </Select>
          <Select aria-label="MFA" className="w-auto" value={filters.mfa} onChange={(e) => { setFilters({ ...filters, mfa: e.target.value as Filters["mfa"] }); paging.reset(); }}>
            <option value="all">Any MFA</option>
            <option value="on">MFA enrolled</option>
            <option value="off">No MFA</option>
          </Select>
          {(activeFilterCount > 0 || filters.q) && (
            <Button variant="ghost" onClick={() => { setFilters(EMPTY_FILTERS); paging.reset(); }}>Clear filters</Button>
          )}
        </div>

        <BulkBar count={selection.count} onClear={selection.clear}>
          <Button size="sm" onClick={() => setChangingRole(selectedUsers)}>Change role</Button>
          <Button size="sm" variant="danger" onClick={() => setSuspending(selectedUsers.filter((u) => u.status !== "disabled" && !u.is_current))}>Suspend</Button>
          <Button size="sm" variant="danger" onClick={() => setRemoving(selectedUsers.filter((u) => !u.is_current))}>Remove</Button>
        </BulkBar>

        <ErrorNote error={users.error} onRetry={() => void users.reload()} />

        {users.loading && all.length === 0 ? (
          <TableSkeleton rows={4} columns={5} />
        ) : rows.length === 0 ? (
          <EmptyState
            icon="users"
            title={activeFilterCount > 0 || filters.q ? "No users match these filters" : "No users"}
            description={activeFilterCount > 0 || filters.q ? "Try a broader search or clear the filters." : "Add the first member of your organization."}
            action={activeFilterCount > 0 || filters.q ? <Button onClick={() => setFilters(EMPTY_FILTERS)}>Clear filters</Button> : canWrite ? <Button variant="primary" onClick={() => setCreating(true)}>Add user</Button> : undefined}
          />
        ) : (
          <>
            <Table>
              <THead>
                <tr>
                  <Th className="w-10"><SelectionCheckbox checked={selection.allSelected} indeterminate={selection.someSelected} onChange={selection.toggleAll} label="Select all on this page" /></Th>
                  <SortableTh sortKey="name" sort={sort} onSort={setSort}>User</SortableTh>
                  <SortableTh sortKey="role" sort={sort} onSort={setSort}>Role</SortableTh>
                  <SortableTh sortKey="status" sort={sort} onSort={setSort}>Status</SortableTh>
                  <SortableTh sortKey="last_active" sort={sort} onSort={setSort} className="hidden md:table-cell">Last sign-in</SortableTh>
                  <SortableTh sortKey="mfa" sort={sort} onSort={setSort} className="hidden lg:table-cell">Authentication</SortableTh>
                  <Th align="right"><span className="sr-only">Actions</span></Th>
                </tr>
              </THead>
              <TBody>
                {paging.rows.map((user) => {
                  const status = USER_STATUS[user.status];
                  return (
                    <Tr key={user.id} interactive selected={selection.isSelected(user.id)} onClick={() => setOpenUser(user.id)}>
                      <Td onClick={(e) => e.stopPropagation()}>
                        <SelectionCheckbox checked={selection.isSelected(user.id)} onChange={() => selection.toggle(user.id)} label={`Select ${user.full_name || user.email}`} />
                      </Td>
                      <Td>
                        <div className="flex items-center gap-3">
                          <Avatar name={user.full_name || user.email} />
                          <div className="min-w-0">
                            <div className="flex items-center gap-2">
                              <span className={cn("truncate font-medium", user.full_name ? "text-ink" : "text-ink-muted")}>{user.full_name || user.email}</span>
                              {user.is_current && <Badge tone="accent">You</Badge>}
                            </div>
                            <div className="truncate text-xs text-ink-muted">{user.email}</div>
                            <div className="mt-0.5 text-xs text-ink-faint lg:hidden">
                              {user.mfa_enabled ? "Password + MFA" : "Password"}
                              <span className="md:hidden"> · {user.last_login_at ? relativeTime(user.last_login_at) : "never signed in"}</span>
                            </div>
                          </div>
                        </div>
                      </Td>
                      <Td>
                        <div className="flex flex-wrap gap-1">
                          {user.roles.map((r) => <Badge key={r} tone={r === "admin" ? "accent" : "neutral"}>{roleLabel(r, roles.data ?? undefined)}</Badge>)}
                          {user.roles.length === 0 && <span className="text-xs text-ink-faint">No role</span>}
                        </div>
                      </Td>
                      <Td><Badge tone={status.tone} dot>{status.label}</Badge></Td>
                      <Td className="hidden whitespace-nowrap text-ink-muted md:table-cell" title={user.last_login_at ?? undefined}>{user.last_login_at ? relativeTime(user.last_login_at) : "Never"}</Td>
                      <Td className="hidden text-ink-muted lg:table-cell">{user.mfa_enabled ? "Password + MFA" : "Password"}</Td>
                      <Td align="right" onClick={(e) => e.stopPropagation()}>
                        <Menu
                          trigger={({ toggle, open }) => <IconButton icon="more" label={`Actions for ${user.full_name || user.email}`} size="sm" onClick={toggle} aria-expanded={open} />}
                          items={[
                            { label: "View details", icon: "eye", onSelect: () => setOpenUser(user.id) },
                            { label: "Change role", icon: "key", disabled: !canWrite, onSelect: () => setChangingRole([user]) },
                            { label: "View role permissions", icon: "shield", onSelect: () => router.push(`/settings/roles/${user.roles[0] ?? "viewer"}`) },
                            { kind: "separator" },
                            user.status === "disabled"
                              ? { label: "Reactivate user", icon: "check-circle", disabled: !canWrite, onSelect: () => void withBusy(async () => { await reactivateUser(user.id); void users.reload(); push({ title: `${user.full_name || user.email} reactivated`, tone: "success" }); }) }
                              : { label: "Suspend user", icon: "ban", tone: "danger", disabled: !canWrite || user.is_current, onSelect: () => setSuspending([user]) },
                            { label: "Remove user", icon: "trash", tone: "danger", disabled: !canWrite || user.is_current, onSelect: () => setRemoving([user]) },
                          ]}
                        />
                      </Td>
                    </Tr>
                  );
                })}
              </TBody>
            </Table>
            <Pagination page={paging.page} pages={paging.pages} pageSize={paging.pageSize} total={paging.total} onPage={paging.setPage} onPageSize={paging.setPageSize} />
          </>
        )}
      </Card>

      <UserDrawer
        user={all.find((u) => u.id === openUser) ?? null}
        roles={roles.data ?? []}
        onClose={() => setOpenUser(null)}
        onChanged={() => void users.reload()}
        onChangeRole={(u) => setChangingRole([u])}
        onSuspend={(u) => setSuspending([u])}
        onReactivate={(u) => void withBusy(async () => { await reactivateUser(u.id); void users.reload(); push({ title: "User reactivated", tone: "success" }); })}
        onRemove={(u) => setRemoving([u])}
      />

      <CreateUserModal
        open={creating}
        roles={roles.data ?? []}
        onClose={() => setCreating(false)}
        onCreated={(user) => { setCreating(false); setCreated(user); void users.reload(); }}
      />

      <ChangeRoleModal
        users={changingRole}
        roles={roles.data ?? []}
        onClose={() => setChangingRole(null)}
        onSaved={(n) => { setChangingRole(null); selection.clear(); void users.reload(); push({ title: n === 1 ? "Role updated" : `Role updated for ${n} users`, tone: "success" }); }}
      />

      <ConfirmDialog
        open={suspending !== null && suspending.length > 0}
        onClose={() => setSuspending(null)}
        title={suspending?.length === 1 ? `Suspend ${suspending[0]?.full_name || suspending[0]?.email}?` : `Suspend ${suspending?.length} users?`}
        confirmLabel={suspending?.length === 1 ? "Suspend user" : `Suspend ${suspending?.length} users`}
        loading={busy}
        description="They cannot sign in until an administrator reactivates them. Existing sessions end at their next token refresh, within fifteen minutes. Their roles and history are kept."
        onConfirm={() => withBusy(async () => { for (const u of suspending ?? []) await suspendUser(u.id); setSuspending(null); selection.clear(); void users.reload(); push({ title: suspending?.length === 1 ? "User suspended" : `${suspending?.length} users suspended`, tone: "success" }); })}
      />

      <ConfirmDialog
        open={removing !== null && removing.length > 0}
        onClose={() => setRemoving(null)}
        title={removing?.length === 1 ? `Remove ${removing[0]?.full_name || removing[0]?.email}?` : `Remove ${removing?.length} users?`}
        confirmLabel={removing?.length === 1 ? "Remove user" : `Remove ${removing?.length} users`}
        loading={busy}
        confirmText={removing?.length === 1 ? removing[0]?.email : `remove ${removing?.length} users`}
        description={<>This removes the account{removing?.length === 1 ? "" : "s"} from the organization. <strong className="text-ink">This cannot be undone.</strong> To keep the account but block access, suspend instead.</>}
        onConfirm={() => withBusy(async () => { for (const u of removing ?? []) await removeUser(u.id); setRemoving(null); setOpenUser(null); selection.clear(); void users.reload(); push({ title: removing?.length === 1 ? "User removed" : `${removing?.length} users removed`, tone: "success" }); })}
      />
    </>
  );
}

function CreateUserModal({ open, roles, onClose, onCreated }: { open: boolean; roles: Role[]; onClose: () => void; onCreated: (u: CreatedUser) => void }) {
  const [email, setEmail] = useState("");
  const [fullName, setFullName] = useState("");
  const [role, setRole] = useState("viewer");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const selected = roles.find((r) => r.slug === role);

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="Add user"
      description="The account is created with a temporary password, shown once. There is no invitation email on this server — send it to them yourself."
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
          <Button variant="primary" type="submit" form="create-user" loading={busy}>Create user</Button>
        </>
      }
    >
      <form
        id="create-user"
        className="space-y-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError(null);
          try {
            const user = await createUser({ email: email.trim(), full_name: fullName.trim(), roles: [role] });
            setEmail(""); setFullName(""); setRole("viewer");
            onCreated(user);
          } catch (err) {
            setError(err);
          } finally {
            setBusy(false);
          }
        }}
      >
        <Field label="Email address" required htmlFor="new-email"><Input id="new-email" type="email" required autoFocus value={email} onChange={(e) => setEmail(e.target.value)} placeholder="name@company.com" /></Field>
        <Field label="Full name" htmlFor="new-name"><Input id="new-name" value={fullName} onChange={(e) => setFullName(e.target.value)} maxLength={200} /></Field>
        <Field label="Role" required hint={selected?.description} htmlFor="new-role">
          <Select id="new-role" value={role} onChange={(e) => setRole(e.target.value)}>
            {roles.map((r) => <option key={r.slug} value={r.slug}>{r.name}</option>)}
          </Select>
        </Field>
        <ErrorNote error={error} />
      </form>
    </Modal>
  );
}

function ChangeRoleModal({ users, roles, onClose, onSaved }: { users: OrgUser[] | null; roles: Role[]; onClose: () => void; onSaved: (count: number) => void }) {
  const single = users?.length === 1 ? users[0] : undefined;
  const [role, setRole] = useState<string>("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const current = role || single?.roles[0] || "";

  return (
    <Modal
      open={users !== null && users.length > 0}
      onClose={() => { onClose(); setRole(""); setError(null); }}
      title={single ? `Change role for ${single.full_name || single.email}` : `Change role for ${users?.length ?? 0} users`}
      description="Roles are bundles of permissions. The change applies at the user's next token refresh, within fifteen minutes."
      footer={
        <>
          <Button variant="ghost" onClick={() => { onClose(); setRole(""); }} disabled={busy}>Cancel</Button>
          <Button
            variant="primary"
            loading={busy}
            disabled={!current || (single ? current === single.roles[0] : false)}
            onClick={async () => {
              setBusy(true);
              setError(null);
              try {
                for (const u of users ?? []) await setUserRoles(u.id, [current]);
                onSaved(users?.length ?? 0);
                setRole("");
              } catch (err) {
                setError(err);
              } finally {
                setBusy(false);
              }
            }}
          >
            Save role
          </Button>
        </>
      }
    >
      <div role="radiogroup" aria-label="Role" className="space-y-1.5">
        {roles.map((r) => {
          const selected = current === r.slug;
          return (
            <label key={r.slug} className={cn("flex cursor-pointer items-start gap-3 rounded-md border p-3 transition-colors", selected ? "border-accent bg-accent/5" : "border-surface-border hover:border-surface-border-strong")}>
              <input type="radio" name="role" className="mt-0.5" checked={selected} onChange={() => setRole(r.slug)} />
              <span className="min-w-0 flex-1">
                <span className="flex items-center gap-2 text-sm font-medium text-ink">{r.name}{r.is_system && <Badge tone="neutral">Built-in</Badge>}</span>
                <span className="mt-0.5 block text-xs text-ink-muted">{r.description}</span>
                <span className="mt-1 block text-2xs text-ink-faint">{r.permissions.length} permissions · {r.user_count} {r.user_count === 1 ? "user" : "users"}</span>
              </span>
            </label>
          );
        })}
      </div>
      {single?.is_current && <p className="mt-3 text-xs text-degraded">You cannot remove the admin role from your own account.</p>}
      <ErrorNote error={error} />
      <p className="mt-3 text-xs text-ink-faint">Slugs: {roles.map((r) => <Code key={r.slug} className="mr-1">{r.slug}</Code>)}</p>
    </Modal>
  );
}
