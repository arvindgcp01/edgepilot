"use client";

import { useParams, useRouter } from "next/navigation";
import { useCallback, useEffect, useState } from "react";
import { AppShell } from "@/components/app-shell";
import { Button, Card, ErrorNote, Input, Spinner, StatusBadge } from "@/components/ui";
import { can } from "@/lib/api/client";
import {
  approveDevice, decommissionDevice, getDevice, rejectDevice, updateDevice,
  type Device,
} from "@/lib/api/devices";
import { absoluteTime, bytes, relativeTime } from "@/lib/format";

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

  const [device, setDevice] = useState<Device | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

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

  async function act(fn: () => Promise<unknown>) {
    setBusy(true);
    setError(null);
    try {
      await fn();
      await load();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  if (loading) return <Spinner />;
  if (!device) return <ErrorNote error={error} />;

  return (
    <>
      <div className="mb-6 flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">{device.hostname || "(unnamed)"}</h1>
        <StatusBadge status={device.status} />
        {!device.approved && (
          <span className="text-sm text-pending">awaiting approval</span>
        )}

        <div className="ml-auto flex gap-2">
          {!device.approved && can("device:approve") && (
            <>
              <Button disabled={busy} onClick={() => void act(() => approveDevice(device.id))}>
                Approve
              </Button>
              <Button variant="ghost" disabled={busy} onClick={() => void act(() => rejectDevice(device.id))}>
                Reject
              </Button>
            </>
          )}
          {can("device:delete") && (
            <Button
              variant="danger"
              disabled={busy}
              onClick={() => {
                // Decommissioning revokes the device's certificates and frees
                // its serial. It is not undoable from here, so it asks.
                if (!confirm(
                  `Decommission ${device.hostname}?\n\n` +
                  "Its certificates are revoked immediately and it can no longer connect. " +
                  "Its history is kept, and its serial number becomes available for a replacement.",
                )) return;
                void act(async () => {
                  await decommissionDevice(device.id);
                  router.replace("/");
                });
              }}
            >
              Decommission
            </Button>
          )}
        </div>
      </div>

      <ErrorNote error={error} />

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <h2 className="mb-3 text-sm font-semibold text-ink-muted">General</h2>
          <Facts rows={[
            ["Serial number", device.serial_number],
            ["Device ID", device.id],
            ["Enrollment", device.enrollment_state],
            ["Site", device.site || "—"],
            ["First enrolled", absoluteTime(device.first_enrolled_at)],
            ["Last seen", `${relativeTime(device.last_seen_at)} (${absoluteTime(device.last_seen_at)})`],
            ["Agent version", device.agent_version || "—"],
          ]} />
        </Card>

        <Card>
          <h2 className="mb-3 text-sm font-semibold text-ink-muted">Hardware</h2>
          <Facts rows={[
            ["Model", device.model_name || device.model_identifier || "—"],
            ["Identifier", device.model_identifier || "—"],
            ["Chip", device.chip || "—"],
            ["Architecture", device.arch || "—"],
            ["CPU cores", device.cpu_cores ? String(device.cpu_cores) : "—"],
            ["Memory", bytes(device.memory_bytes)],
            ["Disk", bytes(device.disk_total_bytes)],
            ["Hardware UUID", device.hardware_uuid || "—"],
          ]} />
        </Card>

        <Card>
          <h2 className="mb-3 text-sm font-semibold text-ink-muted">Operating system</h2>
          <Facts rows={[
            ["Name", device.os_name || "—"],
            ["Version", device.os_version || "—"],
            ["Build", device.os_build || "—"],
            ["Local IP", device.local_ip || "—"],
            ["Public IP", device.public_ip || "—"],
          ]} />
        </Card>

        <EditableCard device={device} onSaved={load} />
      </div>
    </>
  );
}

function Facts({ rows }: { rows: [string, string][] }) {
  return (
    <dl className="grid grid-cols-[minmax(0,10rem)_1fr] gap-x-4 gap-y-2 text-sm">
      {rows.map(([label, value]) => (
        <div key={label} className="contents">
          <dt className="text-ink-faint">{label}</dt>
          <dd className="truncate font-mono text-xs text-ink-muted" title={value}>{value}</dd>
        </div>
      ))}
    </dl>
  );
}

/** Only the operator-owned fields. Hardware and OS come from inventory, and the
 *  server refuses to let them be set here — so the form does not offer them. */
function EditableCard({ device, onSaved }: { device: Device; onSaved: () => Promise<void> }) {
  const editable = can("device:write");
  const [site, setSite] = useState(device.site ?? "");
  const [notes, setNotes] = useState(device.notes ?? "");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [saved, setSaved] = useState(false);

  return (
    <Card>
      <h2 className="mb-3 text-sm font-semibold text-ink-muted">Operator notes</h2>
      <form
        className="space-y-3"
        onSubmit={async (event) => {
          event.preventDefault();
          setSaving(true);
          setError(null);
          setSaved(false);
          try {
            await updateDevice(device.id, { site, notes });
            await onSaved();
            setSaved(true);
          } catch (err) {
            setError(err);
          } finally {
            setSaving(false);
          }
        }}
      >
        <label className="block text-sm">
          <span className="mb-1 block text-ink-faint">Site</span>
          <Input value={site} disabled={!editable} onChange={(e) => setSite(e.target.value)} />
        </label>
        <label className="block text-sm">
          <span className="mb-1 block text-ink-faint">Notes</span>
          <Input value={notes} disabled={!editable} onChange={(e) => setNotes(e.target.value)} />
        </label>

        {device.labels && Object.keys(device.labels).length > 0 && (
          <div className="flex flex-wrap gap-1.5">
            {Object.entries(device.labels).map(([key, value]) => (
              <span key={key} className="rounded bg-surface-border px-2 py-0.5 font-mono text-xs text-ink-muted">
                {key}:{value}
              </span>
            ))}
          </div>
        )}

        <ErrorNote error={error} />
        {editable && (
          <div className="flex items-center gap-3">
            <Button type="submit" disabled={saving}>{saving ? "Saving…" : "Save"}</Button>
            {saved && <span className="text-xs text-online">Saved</span>}
          </div>
        )}
      </form>
    </Card>
  );
}
