/** Device and enrollment-token calls, typed against the generated schema. */
import { request } from "./client";

export type DeviceStatus = "pending" | "online" | "offline" | "degraded" | "decommissioned";

export type Device = {
  id: string;
  org_id: string;
  status: DeviceStatus;
  hostname: string;
  serial_number: string;
  hardware_uuid?: string;
  model_identifier?: string;
  model_name?: string;
  chip?: string;
  arch?: string;
  cpu_cores?: number;
  memory_bytes?: number;
  disk_total_bytes?: number;
  os_name?: string;
  os_version?: string;
  os_build?: string;
  agent_version?: string;
  local_ip?: string;
  public_ip?: string;
  site?: string;
  notes?: string;
  labels?: Record<string, string>;
  allow_custom_scripts: boolean;
  enrollment_state: string;
  approved: boolean;
  first_enrolled_at?: string;
  last_seen_at?: string;
  last_boot_at?: string;
  active_alert_count: number;
  created_at: string;
  updated_at: string;
};

export type Page<T> = { data: T[]; next_cursor: string; has_more: boolean };

export type FleetSummary = {
  total: number;
  online: number;
  offline: number;
  degraded: number;
  pending_approval: number;
  updates_available: number;
};

export type DeviceQuery = {
  q?: string;
  status?: DeviceStatus[];
  enrollment_state?: string;
  sort?: string;
  cursor?: string;
  limit?: number;
};

function toQuery(query: DeviceQuery): string {
  const params = new URLSearchParams();
  if (query.q) params.set("q", query.q);
  // Repeated rather than comma-joined: the server accepts both, and repeating
  // survives a status that ever contains a comma.
  for (const status of query.status ?? []) params.append("status", status);
  if (query.enrollment_state) params.set("enrollment_state", query.enrollment_state);
  if (query.sort) params.set("sort", query.sort);
  if (query.cursor) params.set("cursor", query.cursor);
  if (query.limit) params.set("limit", String(query.limit));
  const encoded = params.toString();
  return encoded ? `?${encoded}` : "";
}

export const listDevices = (query: DeviceQuery = {}) =>
  request<Page<Device>>(`/devices${toQuery(query)}`);

export const getDevice = (id: string) => request<Device>(`/devices/${id}`);

export const fleetSummary = () => request<FleetSummary>("/devices/summary");

export const updateDevice = (
  id: string,
  patch: Partial<Pick<Device, "site" | "notes" | "labels" | "allow_custom_scripts">>,
) => request<Device>(`/devices/${id}`, { method: "PATCH", body: JSON.stringify(patch) });

export const approveDevice = (id: string) =>
  request<Device>(`/devices/${id}/approve`, { method: "POST" });

export const rejectDevice = (id: string) =>
  request<Device>(`/devices/${id}/reject`, { method: "POST" });

export const decommissionDevice = (id: string) =>
  request<void>(`/devices/${id}`, { method: "DELETE" });

export type EnrollmentToken = {
  id: string;
  org_id: string;
  token_prefix: string;
  description?: string;
  max_uses?: number;
  use_count: number;
  auto_approve: boolean;
  labels?: Record<string, string>;
  expires_at: string;
  revoked_at?: string;
  created_at: string;
  usable: boolean;
  /** Present only in the creation response. It is never recoverable afterwards. */
  token?: string;
};

export const listEnrollmentTokens = () =>
  request<Page<EnrollmentToken>>("/enrollment-tokens");

export const createEnrollmentToken = (body: {
  description: string;
  expires_at: string;
  auto_approve?: boolean;
  max_uses?: number | null;
  labels?: Record<string, string>;
}) =>
  request<EnrollmentToken>("/enrollment-tokens", {
    method: "POST",
    body: JSON.stringify(body),
  });

export const revokeEnrollmentToken = (id: string) =>
  request<void>(`/enrollment-tokens/${id}`, { method: "DELETE" });
