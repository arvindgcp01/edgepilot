"use client";

/**
 * Cells and controls shared by the device list, the tile grid, and the
 * quick-view drawer — so a device reads the same wherever it appears.
 */
import { useRouter } from "next/navigation";
import { useRef, useState } from "react";
import { Badge, Button, Icon, IconButton, Input, Menu, useToast } from "@/components/ui";
import { can } from "@/lib/api/client";
import { terminateSSHSession, updateDevice, type Device, type SSHSession } from "@/lib/api/devices";
import { cn } from "@/lib/cn";
import { usePinned } from "@/lib/local-store";

/** A small tile that tells a Mac Studio from a Mac mini at a glance, tinted by
 *  status so a column of them reads like a heat strip. */
export function DeviceGlyph({ device }: { device: Device }) {
  const tone =
    device.status === "online"
      ? "bg-online/10 text-online"
      : device.status === "degraded"
        ? "bg-degraded/10 text-degraded"
        : device.status === "pending"
          ? "bg-pending/10 text-pending"
          : "bg-surface-hover text-ink-faint";
  return (
    <span className={cn("flex h-9 w-9 shrink-0 items-center justify-center rounded-lg", tone)}>
      <Icon name={/studio/i.test(device.model_name ?? "") ? "server" : "monitor"} size={17} />
    </span>
  );
}

/** CPU, memory, and disk as three hairline bars in one cell. The exact numbers
 *  are in the tooltip and on the device page; the list only needs to show which
 *  machines are running hot. */
export function HealthCell({ device }: { device: Device }) {
  if (device.last_cpu_percent === null && device.last_memory_percent === null) {
    return <span className="text-xs text-ink-faint">No metrics</span>;
  }
  const rows: [string, number | null | undefined, number][] = [
    ["CPU", device.last_cpu_percent, 80],
    ["MEM", device.last_memory_percent, 80],
    ["DSK", device.last_disk_percent, 95],
  ];
  return (
    <div
      className="space-y-1"
      title={rows.map(([l, v]) => `${l} ${v === null || v === undefined ? "—" : `${Math.round(v)}%`}`).join(" · ")}
    >
      {rows.map(([label, value, warn]) => {
        const v = value === null || value === undefined ? null : Math.round(value);
        const tone = v === null ? "" : v >= warn ? "bg-danger" : v >= warn - 20 ? "bg-degraded" : "bg-accent";
        return (
          <div key={label} className="flex items-center gap-2">
            <span className="w-7 text-2xs font-medium text-ink-faint">{label}</span>
            <div className="h-1 flex-1 overflow-hidden rounded-full bg-surface-hover">
              {v !== null && <div className={cn("h-full rounded-full", tone)} style={{ width: `${Math.min(100, v)}%` }} />}
            </div>
            <span className="w-8 text-right text-2xs tabular text-ink-muted">{v === null ? "—" : `${v}%`}</span>
          </div>
        );
      })}
    </div>
  );
}

export function RowMenu({
  device,
  onConnect,
  onPeek,
}: {
  device: Device;
  onConnect?: () => void;
  /** Opens the quick-view drawer, where the page has one. */
  onPeek?: () => void;
}) {
  const router = useRouter();
  const { push } = useToast();
  const { isPinned, toggle } = usePinned();
  const pinned = isPinned(device.id);
  const name = device.hostname || device.serial_number;
  const copy = (value: string, what: string) => {
    void navigator.clipboard?.writeText(value);
    push({ title: `${what} copied`, description: value, tone: "success" });
  };
  return (
    <Menu
      width="w-56"
      trigger={({ toggle: open_, open }) => (
        <IconButton icon="more" label={`Actions for ${name}`} size="sm" onClick={open_} aria-expanded={open} />
      )}
      items={[
        ...(onPeek ? [{ label: "Quick view", icon: "eye" as const, onSelect: onPeek, hint: "Space" }] : []),
        { label: "Open device page", icon: "external", onSelect: () => router.push(`/devices/${device.id}`), hint: "↵" },
        ...(onConnect && device.status === "online" && device.approved
          ? [{ label: "Open terminal", icon: "terminal" as const, onSelect: onConnect }]
          : []),
        {
          label: pinned ? "Unpin from sidebar" : "Pin to sidebar",
          icon: "bookmark",
          onSelect: () => {
            toggle({ id: device.id, name, client: device.client_name || undefined });
            push({ title: pinned ? `${name} unpinned` : `${name} pinned`, tone: "success" });
          },
        },
        { kind: "separator" },
        { label: "Copy serial number", icon: "copy", onSelect: () => copy(device.serial_number, "Serial number") },
        { label: "Copy device ID", icon: "copy", onSelect: () => copy(device.id, "Device ID") },
        ...(device.local_ip ? [{ label: "Copy local IP", icon: "copy" as const, onSelect: () => copy(device.local_ip!, "Local IP") }] : []),
      ]}
    />
  );
}

/**
 * The Client column, edited in place.
 *
 * Hostname sits one column to the left and is not editable, because the agent
 * reports it on every connect and would overwrite anything typed here. This is
 * the field that answers the question an operator actually has of the fleet
 * table — whose machine is this — and it is theirs to set.
 *
 * Editing happens in the grid rather than behind the device page: naming a
 * fleet is a dozen small edits in a row, and making each one a navigation would
 * turn a two-minute job into a tedious one.
 */
export function ClientNameCell({ device, onSaved }: { device: Device; onSaved: (device: Device) => void }) {
  const editable = can("device:write");
  const { push } = useToast();
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const [saving, setSaving] = useState(false);
  const [failed, setFailed] = useState<string | null>(null);

  // Escape closes the input, which fires blur, which would otherwise save the
  // very edit the operator just abandoned. The ref lets blur know it was asked
  // to leave rather than asked to commit.
  const abandoned = useRef(false);

  function begin() {
    setDraft(device.client_name ?? "");
    setFailed(null);
    setEditing(true);
  }

  async function save() {
    const next = draft.trim();
    if (next === (device.client_name ?? "")) {
      setEditing(false);
      return;
    }
    setSaving(true);
    setFailed(null);
    try {
      // The response is the row the server wrote, so it goes straight into the
      // list. Sending "" is how the field is cleared — the PATCH distinguishes
      // an empty string from an absent one.
      onSaved(await updateDevice(device.id, { client_name: next }));
      setEditing(false);
      push({ title: next ? `Client set to ${next}` : "Client name cleared", tone: "success" });
    } catch (err) {
      // Stay in the input on failure. Dropping back to the display value would
      // discard what they typed and show the old name as though it had saved.
      setFailed(err instanceof Error ? err.message : "could not save");
    } finally {
      setSaving(false);
    }
  }

  if (!editable) {
    return <span className={cn(device.client_name ? "" : "text-ink-faint")}>{device.client_name || "—"}</span>;
  }

  if (editing) {
    return (
      <div className="flex flex-col gap-1">
        <Input
          autoFocus
          className="h-7 w-40"
          value={draft}
          maxLength={200}
          disabled={saving}
          aria-label={`Client name for ${device.hostname || device.serial_number}`}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              void save();
            } else if (e.key === "Escape") {
              e.preventDefault();
              abandoned.current = true;
              setEditing(false);
            }
          }}
          onBlur={() => {
            if (abandoned.current) {
              abandoned.current = false;
              return;
            }
            void save();
          }}
        />
        {failed && <span className="text-xs text-danger">{failed}</span>}
      </div>
    );
  }

  return (
    <button
      type="button"
      onClick={begin}
      title="Click to edit"
      className={cn(
        "group -mx-1.5 inline-flex max-w-full items-center gap-1.5 rounded px-1.5 py-0.5 text-left hover:bg-surface-hover",
        device.client_name ? "text-ink" : "text-ink-faint",
      )}
    >
      <span className="truncate">{device.client_name || "Set client"}</span>
      <span className="text-ink-faint opacity-0 group-hover:opacity-100" aria-hidden>
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round"><path d="M21.17 6.83a2.83 2.83 0 0 0-4-4L4 16v4h4z" /><path d="m15 5 4 4" /></svg>
      </span>
    </button>
  );
}

/**
 * The SSH column.
 *
 * Empty for the overwhelming majority of rows, which is the point: a live
 * session on a customer's Mac is worth noticing, and it should not be something
 * an operator has to open a device page to discover.
 */
export function SSHCell({ sessions, onChange }: { sessions?: SSHSession[]; onChange: () => void }) {
  const { push } = useToast();
  const [ending, setEnding] = useState<string | null>(null);

  const live = sessions?.filter((s) => s.status === "active") ?? [];
  if (live.length === 0) return null;

  async function end(session: SSHSession) {
    setEnding(session.id);
    try {
      await terminateSSHSession(session.id);
      // Refetch rather than dropping the row locally: the server decides what
      // "ended" means, and the audit record is written by the bastion's own
      // teardown after this returns.
      onChange();
      push({ title: "Session ended", description: `Disconnected ${session.target_username ?? "the operator"}.`, tone: "success" });
    } catch (err) {
      push({ title: "Could not end the session", description: err instanceof Error ? err.message : undefined, tone: "error" });
    } finally {
      setEnding(null);
    }
  }

  return (
    <div className="flex flex-col gap-1">
      {live.map((session) => (
        <div key={session.id} className="flex items-center gap-1.5">
          <Badge tone="success" dot title={`from ${session.client_ip ?? "unknown"} since ${session.started_at}`}>
            {session.target_username || "connected"}
          </Badge>
          {can("ssh:connect") && (
            <Button
              variant="danger"
              size="sm"
              className="h-5 px-1.5 text-2xs"
              loading={ending === session.id}
              onClick={() => void end(session)}
              title="Disconnect this session immediately"
            >
              End
            </Button>
          )}
        </div>
      ))}
    </div>
  );
}
