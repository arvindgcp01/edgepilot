/**
 * Human labels for machine values. One place, so "disabled" is "Suspended"
 * everywhere and never "Disabled" on one screen and "Inactive" on another.
 */
import type { AuditEvent, UserStatus } from "@/lib/api/admin";
import type { BadgeTone } from "@/components/ui";

export const USER_STATUS: Record<UserStatus, { label: string; tone: BadgeTone }> = {
  active: { label: "Active", tone: "success" },
  invited: { label: "Invited", tone: "info" },
  disabled: { label: "Suspended", tone: "warning" },
  locked: { label: "Locked", tone: "danger" },
};

export const OUTCOME: Record<AuditEvent["outcome"], { label: string; tone: BadgeTone }> = {
  success: { label: "Success", tone: "success" },
  failure: { label: "Failure", tone: "danger" },
  denied: { label: "Denied", tone: "warning" },
};

export const ACTOR_TYPE: Record<AuditEvent["actor_type"], string> = {
  user: "User",
  agent: "Agent",
  system: "System",
  api_key: "API key",
};

/** The resource half of a permission slug, as a heading. */
export const RESOURCE_LABEL: Record<string, string> = {
  device: "Fleet",
  command: "Commands",
  file: "Files",
  deployment: "Deployments",
  log: "Logs",
  ssh: "SSH access",
  alert: "Alerts",
  user: "Users",
  role: "Roles",
  audit: "Audit log",
  org: "Organization",
  agent: "Agent releases",
};

export const RESOURCE_ORDER = ["device", "command", "file", "deployment", "log", "ssh", "alert", "user", "role", "audit", "org", "agent"];

/** "custom_script" → "Custom script". */
export function humanize(slug: string): string {
  const s = slug.replace(/[_-]+/g, " ").trim();
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** The label a role is shown under; system roles have proper names. */
export function roleLabel(slug: string, roles?: { slug: string; name: string }[]): string {
  return roles?.find((r) => r.slug === slug)?.name ?? humanize(slug);
}
