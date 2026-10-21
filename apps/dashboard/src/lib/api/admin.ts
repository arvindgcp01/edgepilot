/**
 * Organization administration: users, roles, permissions, the organization.
 *
 * Everything here is a real call. What the server does not have — teams,
 * an audit log API, notification channels, API keys, webhooks, billing — is
 * not faked: the functions for those return nothing, and their pages say so.
 * A settings screen that shows invented rows is worse than an empty one,
 * because an operator will act on it.
 */
import { request } from "./client";
import type { Page } from "./devices";

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

/** Mirrors `domain.UserStatus`. "disabled" is shown as Suspended. */
export type UserStatus = "invited" | "active" | "disabled" | "locked";

export type OrgUser = {
  id: string;
  email: string;
  full_name: string;
  status: UserStatus;
  /** Role slugs, resolved to permissions by the server. */
  roles: string[];
  mfa_enabled: boolean;
  last_login_at: string | null;
  created_at: string;
  updated_at: string;
  /** True for the row that is the signed-in operator. Set by the caller. */
  is_current?: boolean;
};

export type UserCounts = { total: number; active: number; invited: number; disabled: number; locked: number };

export const listUsers = () => request<Page<OrgUser>>("/users?limit=500");
export const getUser = (id: string) => request<OrgUser>(`/users/${id}`);

/** The one-time password is returned exactly once, like an enrollment token. */
export type CreatedUser = OrgUser & { temporary_password: string };

export const createUser = (input: { email: string; full_name: string; roles: string[] }) =>
  request<CreatedUser>("/users", { method: "POST", body: JSON.stringify(input) });

export const updateUser = (
  id: string,
  patch: { full_name?: string; status?: "active" | "disabled"; roles?: string[] },
) => request<OrgUser>(`/users/${id}`, { method: "PATCH", body: JSON.stringify(patch) });

export const setUserRoles = (id: string, roles: string[]) => updateUser(id, { roles });
export const suspendUser = (id: string) => updateUser(id, { status: "disabled" });
export const reactivateUser = (id: string) => updateUser(id, { status: "active" });
export const removeUser = (id: string) => request<void>(`/users/${id}`, { method: "DELETE" });

export function countUsers(users: OrgUser[]): UserCounts {
  const count = (s: UserStatus) => users.filter((u) => u.status === s).length;
  return { total: users.length, active: count("active"), invited: count("invited"), disabled: count("disabled"), locked: count("locked") };
}

// ---------------------------------------------------------------------------
// Roles and permissions
// ---------------------------------------------------------------------------

export type Role = {
  id: string;
  slug: string;
  name: string;
  description: string;
  /** Built-in roles are shared by every organization and cannot be edited. */
  is_system: boolean;
  permissions: string[];
  user_count: number;
};

export type Permission = { slug: string; resource: string; action: string; description: string };

export const listRoles = () => request<{ data: Role[] }>("/roles").then((r) => r.data);

/**
 * The server returns permission slugs; the descriptions are the seed's,
 * keyed by slug so a permission the server adds still renders (without a
 * description) rather than being hidden.
 */
const DESCRIPTIONS: Record<string, string> = {
  "device:read": "View devices, inventory, and metrics",
  "device:write": "Edit device metadata, labels, and notes",
  "device:delete": "Decommission and delete devices",
  "device:approve": "Approve or reject device enrollment",
  "command:read": "View command history and results",
  "command:execute": "Run non-destructive allowlisted commands",
  "command:destructive": "Run destructive commands such as restart and shutdown",
  "command:custom_script": "Request execution of a custom script",
  "command:approve": "Approve another user's command request",
  "file:read": "Browse and download files from devices",
  "file:write": "Upload, rename, and create folders on devices",
  "file:delete": "Delete files on devices",
  "deployment:read": "View packages and deployments",
  "deployment:write": "Upload packages and create deployments",
  "deployment:execute": "Start, pause, cancel, and roll back deployments",
  "log:read": "View and download logs",
  "log:collect": "Request log collection from a device",
  "ssh:read": "View SSH keys and session history",
  "ssh:connect": "Open an SSH session to a device",
  "ssh:manage_keys": "Add, scope, and revoke SSH keys",
  "alert:read": "View alerts",
  "alert:write": "Acknowledge, resolve, and configure alerts",
  "user:read": "View users and their roles",
  "user:write": "Invite, edit, and disable users",
  "role:write": "Create roles and assign permissions",
  "audit:read": "Read the audit log",
  "org:write": "Change organization settings",
  "agent:release": "Publish and roll out agent versions",
};

export const listPermissions = () =>
  request<{ data: string[] }>("/permissions").then((r) =>
    r.data.map((slug): Permission => {
      const [resource = slug, action = ""] = slug.split(":");
      return { slug, resource, action, description: DESCRIPTIONS[slug] ?? "" };
    }),
  );

// ---------------------------------------------------------------------------
// Organization
// ---------------------------------------------------------------------------

/** Mirrors `domain.OrgSettings`. Zero retention means the server default. */
export type OrgSettings = {
  require_approval_for_destructive?: boolean;
  auto_approve_enrollment?: boolean;
  metrics_retention_days?: number;
  audit_retention_days?: number;
};

export type Organization = {
  id: string;
  name: string;
  slug: string;
  status: string;
  settings: OrgSettings;
  device_count: number;
  created_at: string;
};

export const getOrganization = () => request<Organization>("/organization");
export const updateOrganization = (patch: { name?: string; settings?: OrgSettings }) =>
  request<Organization>("/organization", { method: "PATCH", body: JSON.stringify(patch) });

// ---------------------------------------------------------------------------
// Not on this server
// ---------------------------------------------------------------------------

/**
 * Thrown by anything the server has no endpoint for. Pages catch it to show
 * their "not available" state; it is never a surprise to the operator.
 */
export class NotAvailable extends Error {
  constructor(feature: string) {
    super(`${feature} is not available on this server.`);
    this.name = "NotAvailable";
  }
}

export type AuditEvent = {
  id: string;
  occurred_at: string;
  actor_type: "user" | "agent" | "system" | "api_key";
  actor_label: string;
  action: string;
  resource_type: string;
  resource_id?: string;
  outcome: "success" | "failure" | "denied";
  ip?: string;
  user_agent: string;
  request_id: string;
  metadata: Record<string, unknown>;
};

/** The server writes no audit rows yet and has no read endpoint. */
export const listAuditEvents = async (): Promise<{ data: AuditEvent[]; total: number }> => ({ data: [], total: 0 });

export type Notification = { id: string; kind: "device" | "security" | "user" | "system"; title: string; body: string; at: string; read: boolean; href?: string };
export const listNotifications = async (): Promise<Notification[]> => [];
