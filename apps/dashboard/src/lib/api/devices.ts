/** Device and enrollment-token calls, typed against the generated schema. */
import { request, currentAccessToken, API_BASE } from "./client";

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
  /** Operator-set. Always present, unlike the optional fields around it — the
   *  fleet table has a column for it, so the server sends "" rather than
   *  omitting it. */
  client_name: string;
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
  /** Last-known utilisation, denormalised by the server's metrics flusher.
   *  Null until the device has reported at least one sample — an agent older
   *  than 2.1.0 never does, and the fleet table must say so rather than 0%. */
  last_cpu_percent: number | null;
  last_memory_percent: number | null;
  last_disk_percent: number | null;
  /** Remote screen (VNC) access. Toggled per device; the password never
   *  reaches the browser — the relay authenticates on the operator's behalf. */
  screen_sharing_enabled: boolean;
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
  patch: Partial<Pick<Device, "client_name" | "site" | "notes" | "labels" | "allow_custom_scripts">>,
) => request<Device>(`/devices/${id}`, { method: "PATCH", body: JSON.stringify(patch) });

/** Turn remote screen access on or off. Returns the updated device. On first
 *  enable the server generates and seals a VNC password and nudges the agent to
 *  configure macOS Screen Sharing. */
export const setDeviceScreenSharing = (id: string, enabled: boolean) =>
  request<Device>(`/devices/${id}/screen-sharing`, { method: "POST", body: JSON.stringify({ enabled }) });

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

/**
 * A live SSH relay from an operator to a device.
 *
 * Metadata only: the bastion forwards an encrypted session and cannot read it,
 * so there is no transcript to show and never will be. What can be shown is who
 * connected, from where, to which account, and how much has crossed.
 */
export type SSHSession = {
  id: string;
  device_id: string;
  user_id?: string;
  ssh_key_id?: string;
  client_ip?: string;
  target_username?: string;
  bastion_replica?: string;
  status: "active" | "closed" | "denied" | "failed";
  bytes_in: number;
  bytes_out: number;
  close_reason?: string;
  started_at: string;
  ended_at?: string;
};

/** Every session currently open across the fleet. */
export const listSSHSessions = () =>
  request<Page<SSHSession>>("/ssh/sessions");

/** One device's session history, most recent first. */
export const listDeviceSSHSessions = (deviceId: string) =>
  request<Page<SSHSession>>(`/devices/${deviceId}/ssh-sessions`);

/** Ends a live session. The operator's terminal disconnects immediately. */
export const terminateSSHSession = (id: string) =>
  request<void>(`/ssh/sessions/${id}`, { method: "DELETE" });

// ---------------------------------------------------------------------------
// Device health
// ---------------------------------------------------------------------------

/**
 * One reading, as the server stores it. Everything but `time` and
 * `cpu_percent` is nullable: an agent too old to report it, or hardware with
 * no such sensor, yields null — never zero.
 */
export type MetricSample = {
  time: string;
  cpu_percent: number;
  cpu_user_percent: number | null;
  cpu_system_percent: number | null;
  load_1: number | null;
  load_5: number | null;
  load_15: number | null;
  memory_used_bytes: number | null;
  memory_total_bytes: number | null;
  memory_percent: number | null;
  memory_pressure: number | null;
  swap_used_bytes: number | null;
  disk_used_bytes: number | null;
  disk_total_bytes: number | null;
  disk_percent: number | null;
  net_rx_bytes: number | null;
  net_tx_bytes: number | null;
  temperature_c: number | null;
  uptime_seconds: number | null;
  process_count: number | null;
};

export type MetricName =
  | "cpu_percent" | "memory_percent" | "memory_used_bytes" | "memory_total_bytes" | "memory_pressure"
  | "disk_percent" | "disk_used_bytes" | "disk_total_bytes" | "load_1" | "load_5" | "load_15"
  | "net_rx_bytes_per_sec" | "net_tx_bytes_per_sec" | "disk_read_bytes_per_sec" | "disk_write_bytes_per_sec"
  | "temperature_c" | "uptime_seconds" | "process_count";

/** `[epoch_millis, value]`. A null value is a genuine gap: render a break. */
export type MetricPoint = [number, number | null];

export type MetricSeries = {
  device_id: string;
  from: string;
  to: string;
  resolution: string;
  source: "raw" | "hourly";
  series: { name: MetricName; unit: string; points: MetricPoint[] }[];
};

export type MetricGroup =
  | "cpu" | "memory" | "memory_pressure" | "disk" | "load" | "network" | "disk_io" | "temperature" | "uptime";

export const getLatestMetrics = (deviceId: string) =>
  request<MetricSample>(`/devices/${deviceId}/metrics/latest`);

export const getDeviceMetrics = (
  deviceId: string,
  query: { from: Date; to?: Date; resolution?: "auto" | "raw" | "1m" | "5m" | "15m" | "1h" | "1d"; metrics?: MetricGroup[] },
) => {
  const params = new URLSearchParams({ from: query.from.toISOString() });
  if (query.to) params.set("to", query.to.toISOString());
  if (query.resolution) params.set("resolution", query.resolution);
  if (query.metrics?.length) params.set("metrics", query.metrics.join(","));
  return request<MetricSeries>(`/devices/${deviceId}/metrics?${params.toString()}`);
};

/** The payload of a `device.metrics` event on the fleet topic. */
export type MetricsEvent = {
  cpu_percent: number;
  memory_percent: number | null;
  disk_percent: number | null;
  uptime_seconds: number | null;
  at: string;
};

// ---------------------------------------------------------------------------
// File transfer (SFTP over the SSH tunnel)
// ---------------------------------------------------------------------------

export type FileEntry = {
  name: string;
  size: number;
  mode: string;
  is_dir: boolean;
  mod_time: string;
};

export type FileListing = {
  /** The absolute path listed; empty request lists the account's home. */
  path: string;
  /** The account the server logged in as. */
  username: string;
  entries: FileEntry[];
};

export const listFiles = (deviceId: string, path?: string) =>
  request<FileListing>(`/devices/${deviceId}/files${path ? `?path=${encodeURIComponent(path)}` : ""}`);

/** Download a remote file to the operator's machine. The endpoint is bearer
 *  authed, so this fetches with the token and saves the blob rather than
 *  pointing an <a href> at it (which could not carry the header). */
export async function downloadDeviceFile(deviceId: string, path: string, filename: string): Promise<void> {
  const base = API_BASE || window.location.origin;
  const url = `${base}/api/v1/devices/${deviceId}/files/download?path=${encodeURIComponent(path)}`;
  const token = currentAccessToken();
  const res = await fetch(url, {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
    credentials: "include",
  });
  if (!res.ok) {
    let detail = `Download failed (${res.status})`;
    try {
      const body = (await res.json()) as { detail?: string };
      if (body.detail) detail = body.detail;
    } catch {
      // Non-JSON error body.
    }
    throw new Error(detail);
  }
  const blob = await res.blob();
  const objectUrl = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = objectUrl;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(objectUrl);
}

/** Upload a file into a directory on the device. */
export async function uploadDeviceFile(deviceId: string, dir: string, file: File): Promise<{ path: string; bytes: number }> {
  const base = API_BASE || window.location.origin;
  const url = `${base}/api/v1/devices/${deviceId}/files/upload?path=${encodeURIComponent(dir)}&name=${encodeURIComponent(file.name)}`;
  const token = currentAccessToken();
  const res = await fetch(url, {
    method: "POST",
    headers: token ? { Authorization: `Bearer ${token}` } : {},
    credentials: "include",
    body: file,
  });
  if (!res.ok) {
    let detail = `Upload failed (${res.status})`;
    try {
      const body = (await res.json()) as { detail?: string };
      if (body.detail) detail = body.detail;
    } catch {
      // Non-JSON.
    }
    throw new Error(detail);
  }
  return (await res.json()) as { path: string; bytes: number };
}
