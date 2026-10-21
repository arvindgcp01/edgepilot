"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useCallback, useEffect, useState } from "react";
import { AppShell } from "@/components/app-shell";
import {
  Badge, Button, Card, Code, ConfirmDialog, ErrorNote, Facts, Field, Icon, Input, Skeleton,
  StatusBadge, Table, TBody, Td, Th, THead, Tr, useToast,
} from "@/components/ui";
import { can } from "@/lib/api/client";
import {
  approveDevice, decommissionDevice, getDevice, listDeviceSSHSessions, rejectDevice,
  setDeviceScreenSharing, updateDevice,
  type Device, type SSHSession,
} from "@/lib/api/devices";
import { absoluteTime, bytes, relativeTime } from "@/lib/format";
import { DeviceMonitoring } from "@/components/device-monitoring";
import { DeviceScreen } from "@/components/device-screen";
import { Card as _Card } from "@/components/ui";

/**
 * The page is a shell around the content.
 *
 * AppShell renders nothing until the session is resolved, so DeviceView only
 * mounts once there is a token to fetch with. Calling the API from the page
 * body instead would fire every request before sign-in had finished and answer
 * itself with a 401.
 */
export default function DevicePage() {
  return (
    <AppShell>
      <DeviceView />
    </AppShell>
  );
}

function DeviceView() {
  const { id } = useParams<{ id: string }>();
  const router = useRouter();
  const { push } = useToast();

  const [device, setDevice] = useState<Device | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [confirmDecommission, setConfirmDecommission] = useState(false);
  const [screenOpen, setScreenOpen] = useState(false);
  const [togglingScreen, setTogglingScreen] = useState(false);

  const load = useCallback(async () => {
    try {
      setDevice(await getDevice(id));
      setError(null);
    } catch (err) {
      setError(err);
    } finally {
      setLoading(false);
    }
  }, [id]);

  useEffect(() => { void load(); }, [load]);

  async function act(fn: () => Promise<unknown>, done: string) {
    setBusy(true);
    setError(null);
    try {
      await fn();
      await load();
      push({ title: done, tone: "success" });
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  if (loading) return <DeviceSkeleton />;
  if (!device) return <ErrorNote error={error} onRetry={() => void load()} />;

  const name = device.hostname || "(unnamed)";

  return (
    <>
      <div className="mb-6">
        <Link href="/" className="mb-3 inline-flex items-center gap-1 text-xs text-ink-muted hover:text-ink">
          <Icon name="arrow-left" size={12} /> Fleet
        </Link>
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="text-xl font-semibold tracking-tight">{name}</h1>
              <StatusBadge status={device.status} />
              {!device.approved && <Badge tone="info">Awaiting approval</Badge>}
            </div>
            <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-sm text-ink-muted">
              {device.client_name && <span className="text-ink">{device.client_name}</span>}
              <span className="font-mono text-xs">{device.serial_number}</span>
              {device.site && <span>· {device.site}</span>}
              <span>· seen {relativeTime(device.last_seen_at)}</span>
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-2">
            {!device.approved && can("device:approve") && (
              <>
                <Button variant="primary" icon="check" disabled={busy} onClick={() => void act(() => approveDevice(device.id), `${name} approved`)}>
                  Approve
                </Button>
                <Button disabled={busy} onClick={() => void act(() => rejectDevice(device.id), `${name} rejected`)}>
                  Reject
                </Button>
              </>
            )}
            {can("screen:connect") && device.approved && (
              <Button
                icon="monitor"
                disabled={busy || (!device.screen_sharing_enabled && togglingScreen)}
                title={device.screen_sharing_enabled ? "Open the device's desktop" : "Enable remote screen for this device"}
                onClick={async () => {
                  if (device.screen_sharing_enabled) {
                    setScreenOpen(true);
                    return;
                  }
                  // First use: enable it, then wait for the agent to apply the
                  // setting before opening — the VNC server is not listening the
                  // instant the row flips.
                  setTogglingScreen(true);
                  try {
                    await setDeviceScreenSharing(device.id, true);
                    await load();
                    push({ title: "Remote screen enabled", description: "Giving the device a moment to start Screen Sharing…", tone: "success" });
                    setTimeout(() => setScreenOpen(true), 2500);
                  } catch (err) {
                    push({ title: "Could not enable remote screen", description: err instanceof Error ? err.message : undefined, tone: "error" });
                  } finally {
                    setTogglingScreen(false);
                  }
                }}
              >
                {device.screen_sharing_enabled ? "Remote screen" : togglingScreen ? "Enabling…" : "Enable screen"}
              </Button>
            )}
            {can("device:delete") && (
              <Button variant="danger" disabled={busy} onClick={() => setConfirmDecommission(true)}>
                Decommission
              </Button>
            )}
          </div>
        </div>
      </div>

      <ErrorNote error={error} />

      {screenOpen && device.screen_sharing_enabled && (
        <_Card className="mb-6 h-[36rem] overflow-hidden" padded={false}>
          <DeviceScreen deviceId={device.id} hostname={name} onClose={() => setScreenOpen(false)} />
        </_Card>
      )}

      <div className="mb-6">
        <DeviceMonitoring device={device} />
      </div>

      <h2 className="mb-3 text-sm font-semibold text-ink">Inventory</h2>
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardTitle>General</CardTitle>
          <Facts mono rows={[
            { label: "Serial number", value: device.serial_number },
            { label: "Device ID", value: device.id },
            { label: "Enrollment", value: device.enrollment_state },
            { label: "First enrolled", value: absoluteTime(device.first_enrolled_at) },
            { label: "Last seen", value: `${relativeTime(device.last_seen_at)} · ${absoluteTime(device.last_seen_at)}` },
            { label: "Last boot", value: absoluteTime(device.last_boot_at) },
            { label: "Agent version", value: device.agent_version || "—" },
          ]} />
        </Card>

        <Card>
          <CardTitle>Hardware</CardTitle>
          <Facts mono rows={[
            { label: "Model", value: device.model_name || device.model_identifier || "—" },
            { label: "Identifier", value: device.model_identifier || "—" },
            { label: "Chip", value: device.chip || "—" },
            { label: "Architecture", value: device.arch || "—" },
            { label: "CPU cores", value: device.cpu_cores ? String(device.cpu_cores) : "—" },
            { label: "Memory", value: bytes(device.memory_bytes) },
            { label: "Disk", value: bytes(device.disk_total_bytes) },
            { label: "Hardware UUID", value: device.hardware_uuid || "—" },
          ]} />
        </Card>

        <Card>
          <CardTitle>Operating system & network</CardTitle>
          <Facts mono rows={[
            { label: "Name", value: device.os_name || "—" },
            { label: "Version", value: device.os_version || "—" },
            { label: "Build", value: device.os_build || "—" },
            { label: "Local IP", value: device.local_ip || "—" },
            { label: "Public IP", value: device.public_ip || "—" },
          ]} />
        </Card>

        <EditableCard device={device} onSaved={load} />

        {can("ssh:read") && (
          <div className="lg:col-span-2">
            <SessionsCard deviceId={device.id} />
          </div>
        )}
      </div>

      <ConfirmDialog
        open={confirmDecommission}
        onClose={() => setConfirmDecommission(false)}
        title={`Decommission ${name}?`}
        confirmLabel="Decommission device"
        confirmText={device.hostname || device.serial_number}
        loading={busy}
        description={
          <>
            Its certificates are revoked immediately and it can no longer connect. Its history is kept, and its
            serial number becomes available for a replacement. <strong className="text-ink">This cannot be undone from the dashboard.</strong>
          </>
        }
        onConfirm={async () => {
          await act(async () => {
            await decommissionDevice(device.id);
            router.replace("/");
          }, `${name} decommissioned`);
        }}
      />
    </>
  );
}

function CardTitle({ children }: { children: React.ReactNode }) {
  return <h2 className="mb-4 text-sm font-semibold text-ink">{children}</h2>;
}

function DeviceSkeleton() {
  return (
    <div role="status" aria-label="Loading">
      <Skeleton className="mb-3 h-3 w-12" />
      <Skeleton className="mb-2 h-6 w-64" />
      <Skeleton className="mb-8 h-3.5 w-80" />
      <div className="grid gap-4 lg:grid-cols-2">
        {[0, 1, 2, 3].map((i) => (
          <Card key={i}>
            <Skeleton className="mb-4 h-3.5 w-24" />
            <div className="space-y-2.5">
              {[0, 1, 2, 3, 4].map((j) => <Skeleton key={j} className="h-3 w-full" />)}
            </div>
          </Card>
        ))}
      </div>
    </div>
  );
}

/** Only the operator-owned fields. Hardware and OS come from inventory, and the
 *  server refuses to let them be set here — so the form does not offer them. */
function EditableCard({ device, onSaved }: { device: Device; onSaved: () => Promise<void> }) {
  const editable = can("device:write");
  const { push } = useToast();
  const [clientName, setClientName] = useState(device.client_name ?? "");
  const [site, setSite] = useState(device.site ?? "");
  const [notes, setNotes] = useState(device.notes ?? "");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const dirty = clientName !== (device.client_name ?? "") || site !== (device.site ?? "") || notes !== (device.notes ?? "");

  return (
    <Card>
      <CardTitle>Operator notes</CardTitle>
      <form
        className="space-y-4"
        onSubmit={async (event) => {
          event.preventDefault();
          setSaving(true);
          setError(null);
          try {
            await updateDevice(device.id, { client_name: clientName, site, notes });
            await onSaved();
            push({ title: "Device updated", tone: "success" });
          } catch (err) {
            setError(err);
          } finally {
            setSaving(false);
          }
        }}
      >
        <Field label="Client name" hint="Whose machine this is. Shown in the fleet table.">
          <Input value={clientName} maxLength={200} disabled={!editable} onChange={(e) => setClientName(e.target.value)} />
        </Field>
        <Field label="Site">
          <Input value={site} disabled={!editable} onChange={(e) => setSite(e.target.value)} placeholder="Chicago DC" />
        </Field>
        <Field label="Notes">
          <Input value={notes} disabled={!editable} onChange={(e) => setNotes(e.target.value)} />
        </Field>

        {device.labels && Object.keys(device.labels).length > 0 && (
          <div>
            <div className="mb-1.5 text-sm font-medium text-ink">Labels</div>
            <div className="flex flex-wrap gap-1.5">
              {Object.entries(device.labels).map(([key, value]) => (
                <Code key={key}>{key}:{value}</Code>
              ))}
            </div>
          </div>
        )}

        <ErrorNote error={error} />
        {editable && (
          <div className="flex items-center gap-3">
            <Button type="submit" variant="primary" loading={saving} disabled={!dirty}>Save changes</Button>
            {dirty && <span className="text-xs text-ink-faint">Unsaved changes</span>}
          </div>
        )}
      </form>
    </Card>
  );
}

/**
 * Who has been on this Mac. Metadata only — the bastion relays an encrypted
 * session and cannot read it — but who, from where, to which account, and how
 * much crossed is exactly what an incident review asks for first.
 */
function SessionsCard({ deviceId }: { deviceId: string }) {
  const [sessions, setSessions] = useState<SSHSession[] | null>(null);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    listDeviceSSHSessions(deviceId)
      .then((page) => setSessions(page.data))
      .catch(setError);
  }, [deviceId]);

  const STATUS: Record<SSHSession["status"], { label: string; tone: "success" | "neutral" | "danger" | "warning" }> = {
    active: { label: "Active", tone: "success" },
    closed: { label: "Closed", tone: "neutral" },
    denied: { label: "Denied", tone: "danger" },
    failed: { label: "Failed", tone: "warning" },
  };

  return (
    <Card padded={false}>
      <div className="flex items-center justify-between px-5 pt-5 pb-3">
        <h2 className="text-sm font-semibold text-ink">SSH sessions</h2>
        <span className="text-xs text-ink-faint">Most recent first</span>
      </div>
      <ErrorNote error={error} />
      {sessions === null ? (
        <div className="space-y-2 px-5 pb-5">{[0, 1, 2].map((i) => <Skeleton key={i} className="h-3.5 w-full" />)}</div>
      ) : sessions.length === 0 ? (
        <p className="px-5 pb-5 text-sm text-ink-muted">No SSH sessions have been opened to this device.</p>
      ) : (
        <Table>
          <THead>
            <tr>
              <Th>Started</Th>
              <Th>Account</Th>
              <Th>From</Th>
              <Th>Status</Th>
              <Th align="right">Transferred</Th>
              <Th>Ended</Th>
            </tr>
          </THead>
          <TBody>
            {sessions.slice(0, 10).map((s) => (
              <Tr key={s.id}>
                <Td className="whitespace-nowrap text-ink-muted" title={absoluteTime(s.started_at)}>{relativeTime(s.started_at)}</Td>
                <Td className="font-mono text-xs">{s.target_username || "—"}</Td>
                <Td className="font-mono text-xs text-ink-muted">{s.client_ip || "—"}</Td>
                <Td><Badge tone={STATUS[s.status].tone} dot={s.status === "active"}>{STATUS[s.status].label}</Badge></Td>
                <Td align="right" className="tabular text-ink-muted">{bytes(s.bytes_in + s.bytes_out)}</Td>
                <Td className="text-ink-muted">{s.ended_at ? relativeTime(s.ended_at) : s.close_reason || "—"}</Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      )}
    </Card>
  );
}
