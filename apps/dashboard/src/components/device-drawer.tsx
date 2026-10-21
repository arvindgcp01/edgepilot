"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import {
  Badge, Button, Drawer, Facts, Icon, IconButton, Kbd, Skeleton, StatusBadge, useToast,
} from "@/components/ui";
import { DeviceGlyph, HealthCell } from "@/components/device-cells";
import { can } from "@/lib/api/client";
import { approveDevice, listDeviceSSHSessions, type Device, type SSHSession } from "@/lib/api/devices";
import { bytes, relativeTime } from "@/lib/format";
import { usePinned } from "@/lib/local-store";
import { cn } from "@/lib/cn";

/**
 * Quick view: the answer to "what is this machine" without leaving the list.
 *
 * Everything shown comes from the list row the operator clicked, plus one
 * small fetch for recent SSH activity, so it opens instantly. ↑ and ↓ step to
 * the neighbouring device, which turns reviewing twenty pending machines into
 * twenty keypresses rather than twenty page loads.
 */
export function DeviceDrawer({
  device,
  onClose,
  onStep,
  position,
  onConnect,
  onChanged,
}: {
  device: Device | null;
  onClose: () => void;
  /** Move to the previous (-1) or next (+1) device in the current list. */
  onStep: (delta: -1 | 1) => void;
  position?: { index: number; total: number };
  onConnect?: (d: Device) => void;
  onChanged: (d: Device) => void;
}) {
  const { push } = useToast();
  const { isPinned, toggle } = usePinned();
  const [sessions, setSessions] = useState<SSHSession[] | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!device || !can("ssh:read")) return;
    let cancelled = false;
    setSessions(null);
    listDeviceSSHSessions(device.id)
      .then((page) => !cancelled && setSessions(page.data.slice(0, 4)))
      .catch(() => !cancelled && setSessions([]));
    return () => {
      cancelled = true;
    };
  }, [device]);

  useEffect(() => {
    if (!device) return;
    const onKey = (e: KeyboardEvent) => {
      const tag = (e.target as HTMLElement | null)?.tagName ?? "";
      if (["INPUT", "TEXTAREA", "SELECT"].includes(tag)) return;
      if (e.key === "ArrowDown" || e.key === "j") {
        e.preventDefault();
        onStep(1);
      } else if (e.key === "ArrowUp" || e.key === "k") {
        e.preventDefault();
        onStep(-1);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [device, onStep]);

  if (!device) return null;
  const name = device.hostname || device.serial_number;
  const pinned = isPinned(device.id);

  async function approve() {
    if (!device) return;
    setBusy(true);
    try {
      onChanged(await approveDevice(device.id));
      push({ title: `${name} approved`, tone: "success" });
    } catch (err) {
      push({ title: "Could not approve", description: err instanceof Error ? err.message : undefined, tone: "error" });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Drawer
      open
      onClose={onClose}
      title="Quick view"
      width="md"
      header={
        <div className="px-5 pb-5 pt-4">
          <div className="flex items-start gap-3">
            <DeviceGlyph device={device} />
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-2">
                <h3 className="truncate text-lg font-semibold tracking-tight text-ink">{name}</h3>
              </div>
              <div className="mt-1 flex flex-wrap items-center gap-1.5">
                <StatusBadge status={device.status} />
                {!device.approved && <Badge tone="info">Needs approval</Badge>}
                {device.client_name && <Badge tone="neutral">{device.client_name}</Badge>}
              </div>
            </div>
            <IconButton
              icon="bookmark"
              label={pinned ? "Unpin from sidebar" : "Pin to sidebar"}
              className={cn(pinned && "text-accent")}
              onClick={() => toggle({ id: device.id, name, client: device.client_name || undefined })}
            />
          </div>
          <div className="mt-4 flex flex-wrap gap-2">
            {!device.approved && can("device:approve") && (
              <Button size="sm" variant="primary" icon="check" loading={busy} onClick={() => void approve()}>
                Approve
              </Button>
            )}
            {onConnect && can("ssh:connect") && device.status === "online" && device.approved && (
              <Button size="sm" icon="terminal" onClick={() => onConnect(device)}>Terminal</Button>
            )}
            <Link href={`/devices/${device.id}`}>
              <Button size="sm" icon="external">Open device page</Button>
            </Link>
          </div>
        </div>
      }
      footer={
        <div className="flex w-full items-center justify-between text-xs text-ink-faint">
          <span className="inline-flex items-center gap-1.5">
            <Kbd>↑</Kbd><Kbd>↓</Kbd> previous / next
          </span>
          {position && (
            <span className="tabular">
              {position.index + 1} of {position.total}
            </span>
          )}
          <span className="flex gap-1">
            <IconButton icon="chevron-left" size="sm" variant="secondary" label="Previous device" className="rotate-90" onClick={() => onStep(-1)} />
            <IconButton icon="chevron-right" size="sm" variant="secondary" label="Next device" className="rotate-90" onClick={() => onStep(1)} />
          </span>
        </div>
      }
    >
      <div className="space-y-6 p-5">
        <section>
          <SectionTitle>Health</SectionTitle>
          <div className="rounded-xl border border-surface-border bg-surface p-4">
            <HealthCell device={device} />
          </div>
        </section>

        <section>
          <SectionTitle>Details</SectionTitle>
          <Facts
            rows={[
              { label: "Model", value: device.model_name || device.model_identifier || "—" },
              { label: "Chip", value: device.chip || "—" },
              { label: "Memory", value: bytes(device.memory_bytes) },
              { label: "OS", value: device.os_version ? `${device.os_name || "macOS"} ${device.os_version}` : "—" },
              { label: "Agent", value: device.agent_version || "—" },
              { label: "Site", value: device.site || "—" },
              { label: "Local IP", value: device.local_ip || "—" },
              { label: "Serial", value: <span className="font-mono text-xs">{device.serial_number}</span> },
              { label: "Last seen", value: relativeTime(device.last_seen_at) },
            ]}
          />
        </section>

        {device.notes && (
          <section>
            <SectionTitle>Notes</SectionTitle>
            <p className="whitespace-pre-wrap rounded-xl border border-surface-border bg-surface p-3 text-sm text-ink-muted">{device.notes}</p>
          </section>
        )}

        {can("ssh:read") && (
          <section>
            <SectionTitle>Recent access</SectionTitle>
            {sessions === null ? (
              <div className="space-y-2">{[0, 1].map((i) => <Skeleton key={i} className="h-9 w-full" />)}</div>
            ) : sessions.length === 0 ? (
              <p className="text-sm text-ink-faint">No SSH sessions yet.</p>
            ) : (
              <ul className="divide-y divide-surface-border rounded-xl border border-surface-border">
                {sessions.map((s) => (
                  <li key={s.id} className="flex items-center gap-3 px-3 py-2.5 text-sm">
                    <Icon name="terminal" size={14} className={s.status === "active" ? "text-online" : "text-ink-faint"} />
                    <span className="font-mono text-xs text-ink">{s.target_username || "—"}</span>
                    <span className="text-xs text-ink-faint">{s.client_ip}</span>
                    <span className="ml-auto text-xs text-ink-muted">{s.status === "active" ? "Live now" : relativeTime(s.started_at)}</span>
                  </li>
                ))}
              </ul>
            )}
          </section>
        )}
      </div>
    </Drawer>
  );
}

function SectionTitle({ children }: { children: React.ReactNode }) {
  return <h4 className="mb-2.5 text-2xs font-semibold uppercase tracking-wider text-ink-faint">{children}</h4>;
}
