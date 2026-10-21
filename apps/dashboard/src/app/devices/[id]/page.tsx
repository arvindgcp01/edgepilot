"use client";

import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useCallback, useEffect, useState } from "react";
import { AppShell } from "@/components/app-shell";
import {
  Badge, Button, Card, Code, ConfirmDialog, EmptyState, ErrorNote, Facts, Field, Icon, IconButton, Input, Section, Skeleton,
  StatusBadge, Table, Tabs, TBody, Td, Textarea, Th, THead, Tr, useToast, type IconName,
} from "@/components/ui";
import { cn } from "@/lib/cn";
import { can } from "@/lib/api/client";
import {
  approveDevice, decommissionDevice, getDevice, listDeviceSSHSessions, rejectDevice,
  setDeviceScreenSharing, updateDevice,
  type Device, type SSHSession,
} from "@/lib/api/devices";
import { absoluteTime, bytes, relativeTime } from "@/lib/format";
import { DeviceMonitoring } from "@/components/device-monitoring";
import { DeviceScreen } from "@/components/device-screen";
import { DeviceTerminal } from "@/components/terminal";
import { DeviceFiles } from "@/components/device-files";
import { rememberRecent, usePinned } from "@/lib/local-store";

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
  const [terminalOpen, setTerminalOpen] = useState(false);
  const [filesOpen, setFilesOpen] = useState(false);
  const { isPinned, toggle: togglePin } = usePinned();
  const [tab, setTab] = useState<DeviceTab>("overview");

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

  // Feeds "Recently viewed" in the command palette.
  useEffect(() => {
    if (device) rememberRecent({ id: device.id, name: device.hostname || device.serial_number, client: device.client_name || undefined });
  }, [device]);

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
  const canConnect = can("ssh:connect") && device.status === "online" && device.approved;

  const screenButton = can("screen:connect") && device.approved && (
    <Button
      icon="monitor"
      disabled={busy || (!device.screen_sharing_enabled && togglingScreen)}
      title={device.screen_sharing_enabled ? "Open the device's desktop" : "Enable remote screen for this device"}
      onClick={async () => {
        if (device.screen_sharing_enabled) {
          setScreenOpen(true);
          return;
        }
        // First use: enable it, then wait for the agent to apply the setting
        // before opening — the VNC server is not listening the instant the row
        // flips.
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
  );

  const specs: { icon: IconName; label: string; value: string }[] = [
    { icon: "monitor", label: "Model", value: device.model_name || device.model_identifier || "—" },
    { icon: "cpu", label: "Chip", value: [device.chip, device.memory_bytes ? bytes(device.memory_bytes) : null].filter(Boolean).join(" · ") || "—" },
    { icon: "layers", label: "OS", value: device.os_version ? `${device.os_name || "macOS"} ${device.os_version}` : "—" },
    { icon: "package", label: "Agent", value: device.agent_version || "—" },
    { icon: "map-pin", label: "Site", value: device.site || "—" },
    { icon: "clock", label: "Last seen", value: relativeTime(device.last_seen_at) },
  ];

  return (
    <>
      <nav aria-label="Breadcrumb" className="mb-4 flex items-center gap-1.5 text-sm text-ink-muted">
        <Link href="/devices" className="hover:text-ink">Devices</Link>
        <Icon name="chevron-right" size={14} className="text-ink-faint" />
        <span className="truncate font-medium text-ink">{name}</span>
      </nav>

      <Card padded={false} className="mb-6 overflow-hidden">
        <div className="flex flex-wrap items-start justify-between gap-6 p-6">
          <div className="flex min-w-0 items-start gap-4">
            <span
              className={cn(
                "flex h-14 w-14 shrink-0 items-center justify-center rounded-2xl",
                device.status === "online" ? "bg-online/10 text-online"
                  : device.status === "degraded" ? "bg-degraded/10 text-degraded"
                    : device.status === "pending" ? "bg-pending/10 text-pending"
                      : "bg-surface-hover text-ink-faint",
              )}
            >
              <Icon name={/studio/i.test(device.model_name ?? "") ? "server" : "monitor"} size={26} />
            </span>
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2.5">
                <h1 className="truncate text-2xl font-semibold tracking-tight text-ink">{name}</h1>
                <StatusBadge status={device.status} />
                {!device.approved && <Badge tone="info">Awaiting approval</Badge>}
              </div>
              <div className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-ink-muted">
                {device.client_name ? (
                  <span className="inline-flex items-center gap-1.5 font-medium text-ink">
                    <Icon name="building" size={14} className="text-ink-faint" /> {device.client_name}
                  </span>
                ) : (
                  <span className="text-ink-faint">No client set</span>
                )}
                <span className="text-ink-faint">·</span>
                <span className="font-mono text-xs">{device.serial_number}</span>
              </div>
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-2">
            {!device.approved && can("device:approve") && (
              <>
                <Button disabled={busy} onClick={() => void act(() => rejectDevice(device.id), `${name} rejected`)}>
                  Reject
                </Button>
                <Button variant="primary" icon="check" disabled={busy} onClick={() => void act(() => approveDevice(device.id), `${name} approved`)}>
                  Approve device
                </Button>
              </>
            )}
            <IconButton
              icon="bookmark"
              variant="secondary"
              label={isPinned(device.id) ? "Unpin from sidebar" : "Pin to sidebar"}
              className={cn(isPinned(device.id) && "border-accent/40 bg-accent-subtle text-accent hover:bg-accent-subtle")}
              onClick={() => {
                const pinnedNow = isPinned(device.id);
                togglePin({ id: device.id, name, client: device.client_name || undefined });
                push({ title: pinnedNow ? `${name} unpinned` : `${name} pinned to the sidebar`, tone: "success" });
              }}
            />
            {can("ssh:connect") && device.status === "online" && device.approved && (
              <Button icon="folder" onClick={() => setFilesOpen(true)}>Files</Button>
            )}
            {screenButton}
            {canConnect && (
              <Button variant={device.approved ? "primary" : "secondary"} icon="terminal" onClick={() => setTerminalOpen(true)}>
                Open terminal
              </Button>
            )}
          </div>
        </div>

        <dl className="grid grid-cols-2 border-t border-surface-border bg-surface-sunken/50 sm:grid-cols-3 xl:grid-cols-6">
          {specs.map((spec) => (
            <div key={spec.label} className="min-w-0 border-surface-border px-6 py-4 [&:not(:last-child)]:border-r">
              <dt className="flex items-center gap-1.5 text-xs font-medium text-ink-faint">
                <Icon name={spec.icon} size={13} /> {spec.label}
              </dt>
              <dd className="mt-1 truncate text-sm font-medium text-ink" title={spec.value}>{spec.value}</dd>
            </div>
          ))}
        </dl>
      </Card>

      <ErrorNote error={error} />

      {terminalOpen && (
        <Card className="mb-6 h-[28rem] overflow-hidden" padded={false}>
          <DeviceTerminal deviceId={device.id} hostname={name} onClose={() => setTerminalOpen(false)} />
        </Card>
      )}

      {screenOpen && device.screen_sharing_enabled && (
        <Card className="mb-6 h-[36rem] overflow-hidden" padded={false}>
          <DeviceScreen deviceId={device.id} hostname={name} onClose={() => setScreenOpen(false)} />
        </Card>
      )}

      <Tabs<DeviceTab>
        aria-label="Device sections"
        className="mb-6"
        value={tab}
        onChange={setTab}
        tabs={[
          { value: "overview", label: "Monitoring" },
          { value: "inventory", label: "Hardware & network" },
          ...(can("ssh:read") ? [{ value: "access" as DeviceTab, label: "Access log" }] : []),
          { value: "settings", label: "Settings" },
        ]}
      />

      {tab === "overview" && <DeviceMonitoring device={device} />}

      {tab === "inventory" && (
        <div className="grid gap-6 lg:grid-cols-3">
          <Card>
            <CardTitle icon="fingerprint">Identity</CardTitle>
            <Facts mono rows={[
              { label: "Serial number", value: <CopyValue value={device.serial_number} /> },
              { label: "Device ID", value: <CopyValue value={device.id} /> },
              { label: "Enrollment", value: device.enrollment_state },
              { label: "First enrolled", value: absoluteTime(device.first_enrolled_at) },
              { label: "Last seen", value: absoluteTime(device.last_seen_at) },
              { label: "Last boot", value: absoluteTime(device.last_boot_at) },
              { label: "Agent", value: device.agent_version || "—" },
            ]} />
          </Card>
          <Card>
            <CardTitle icon="cpu">Hardware</CardTitle>
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
            <CardTitle icon="globe">Software & network</CardTitle>
            <Facts mono rows={[
              { label: "OS", value: device.os_name || "—" },
              { label: "Version", value: device.os_version || "—" },
              { label: "Build", value: device.os_build || "—" },
              { label: "Local IP", value: device.local_ip ? <CopyValue value={device.local_ip} /> : "—" },
              { label: "Public IP", value: device.public_ip ? <CopyValue value={device.public_ip} /> : "—" },
            ]} />
          </Card>
        </div>
      )}

      {tab === "access" && can("ssh:read") && <SessionsCard deviceId={device.id} />}

      {tab === "settings" && (
        <div className="space-y-6">
          <EditableCard device={device} onSaved={load} />
          {can("device:delete") && (
            <Section
              tone="danger"
              title="Decommission device"
              description="Revoke its certificates and remove it from the fleet. Its history is kept."
            >
              <div className="flex flex-wrap items-center justify-between gap-4">
                <p className="max-w-lg text-sm text-ink-muted">
                  The device disconnects immediately and cannot reconnect. Its serial number becomes available for a replacement.
                </p>
                <Button variant="danger" icon="trash" disabled={busy} onClick={() => setConfirmDecommission(true)}>
                  Decommission
                </Button>
              </div>
            </Section>
          )}
        </div>
      )}

      <DeviceFiles deviceId={device.id} hostname={name} open={filesOpen} onClose={() => setFilesOpen(false)} />

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
            router.replace("/devices");
          }, `${name} decommissioned`);
        }}
      />
    </>
  );
}

type DeviceTab = "overview" | "inventory" | "access" | "settings";

function CardTitle({ children, icon }: { children: React.ReactNode; icon?: IconName }) {
  return (
    <h2 className="mb-5 flex items-center gap-2 text-base font-semibold text-ink">
      {icon && (
        <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-surface-hover text-ink-muted">
          <Icon name={icon} size={14} />
        </span>
      )}
      {children}
    </h2>
  );
}

/** A value with a copy button that appears on hover. */
function CopyValue({ value }: { value: string }) {
  const { push } = useToast();
  return (
    <span className="group inline-flex max-w-full items-center gap-1.5">
      <span className="truncate">{value}</span>
      <button
        type="button"
        onClick={() => {
          void navigator.clipboard?.writeText(value);
          push({ title: "Copied", description: value, tone: "success" });
        }}
        className="shrink-0 rounded p-0.5 text-ink-faint opacity-0 hover:text-ink group-hover:opacity-100 focus-visible:opacity-100"
        aria-label={`Copy ${value}`}
      >
        <Icon name="copy" size={12} />
      </button>
    </span>
  );
}

function DeviceSkeleton() {
  return (
    <div role="status" aria-label="Loading">
      <Skeleton className="mb-4 h-4 w-40" />
      <Card className="mb-6">
        <div className="flex items-center gap-4">
          <Skeleton className="h-14 w-14 rounded-2xl" />
          <div className="space-y-2">
            <Skeleton className="h-6 w-64" />
            <Skeleton className="h-3.5 w-80" />
          </div>
        </div>
      </Card>
      <div className="grid gap-6 lg:grid-cols-3">
        {[0, 1, 2].map((i) => (
          <Card key={i}>
            <Skeleton className="mb-4 h-4 w-24" />
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
      <CardTitle icon="pencil">Details</CardTitle>
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
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Client name" hint="Whose machine this is. Shown in the device list.">
            <Input value={clientName} maxLength={200} disabled={!editable} onChange={(e) => setClientName(e.target.value)} />
          </Field>
          <Field label="Site" hint="Where it is installed.">
            <Input value={site} disabled={!editable} onChange={(e) => setSite(e.target.value)} placeholder="Chicago DC" />
          </Field>
        </div>
        <Field label="Notes" hint="Anything the next person on call should know about this machine.">
          <Textarea value={notes} disabled={!editable} onChange={(e) => setNotes(e.target.value)} rows={3} />
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
      <div className="flex items-center justify-between px-6 pb-4 pt-6">
        <div>
          <h2 className="text-base font-semibold text-ink">SSH sessions</h2>
          <p className="mt-0.5 text-xs text-ink-faint">Who connected, from where, and for how long. Session content is end-to-end encrypted.</p>
        </div>
        <span className="text-xs text-ink-faint">Most recent first</span>
      </div>
      <ErrorNote error={error} />
      {sessions === null ? (
        <div className="space-y-2 px-5 pb-5">{[0, 1, 2].map((i) => <Skeleton key={i} className="h-3.5 w-full" />)}</div>
      ) : sessions.length === 0 ? (
        <EmptyState icon="terminal" title="No sessions yet" description="No one has opened an SSH session to this device." className="py-10" />
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
