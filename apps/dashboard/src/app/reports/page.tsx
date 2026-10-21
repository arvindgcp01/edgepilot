"use client";

import Link from "next/link";
import { useMemo } from "react";
import { AppShell } from "@/components/app-shell";
import { Button, Card, EmptyState, ErrorNote, Icon, IconButton, PageHeader, Skeleton, type IconName } from "@/components/ui";
import { useFleet } from "@/hooks/use-fleet";
import type { Device } from "@/lib/api/devices";
import { downloadCSV, stamp } from "@/lib/csv";
import { bytes, relativeTime } from "@/lib/format";
import { cn } from "@/lib/cn";

export default function ReportsPage() {
  return (
    <AppShell>
      <ReportsView />
    </AppShell>
  );
}

const versionCompare = (a: string, b: string) => a.localeCompare(b, undefined, { numeric: true });
const n = (v: number | null | undefined) => (v === null || v === undefined ? "" : Math.round(v));

function tally(devices: Device[], key: (d: Device) => string): [string, number][] {
  const m = new Map<string, number>();
  for (const d of devices) m.set(key(d), (m.get(key(d)) ?? 0) + 1);
  return [...m.entries()].sort((a, b) => b[1] - a[1] || versionCompare(a[0], b[0]));
}

/**
 * Fleet reports, computed in the browser from the live device list.
 *
 * Every figure here is a snapshot of now, not a history: the server keeps
 * metric history per device but no fleet-level rollups yet. Each section can be
 * downloaded as CSV for a spreadsheet or an audit binder.
 */
function ReportsView() {
  const { devices, loading, error, reload } = useFleet({ limit: 200 });
  const active = useMemo(() => devices.filter((d) => d.status !== "decommissioned"), [devices]);

  const latestAgent = useMemo(
    () => active.map((d) => d.agent_version).filter((v): v is string => Boolean(v)).sort((a, b) => versionCompare(b, a))[0],
    [active],
  );
  const outdated = active.filter((d) => latestAgent && d.agent_version !== latestAgent);
  const compliance = active.length ? Math.round(((active.length - outdated.length) / active.length) * 100) : 0;
  const online = active.filter((d) => d.status === "online" || d.status === "degraded").length;
  const availability = active.length ? Math.round((online / active.length) * 100) : 0;
  const offline = active
    .filter((d) => d.status === "offline")
    .sort((a, b) => (a.last_seen_at ?? "").localeCompare(b.last_seen_at ?? ""));
  const totalMem = active.reduce((s, d) => s + (d.memory_bytes ?? 0), 0);
  const totalDisk = active.reduce((s, d) => s + (d.disk_total_bytes ?? 0), 0);
  const diskFull = active
    .filter((d) => (d.last_disk_percent ?? 0) >= 85)
    .sort((a, b) => (b.last_disk_percent ?? 0) - (a.last_disk_percent ?? 0));

  const inventory = () =>
    downloadCSV(
      `edgepilot-inventory-${stamp()}.csv`,
      ["Hostname", "Client", "Site", "Status", "Approved", "Model", "Identifier", "Chip", "Arch", "CPU cores", "Memory (GiB)", "Disk (GiB)", "OS", "Build", "Agent", "Serial", "Hardware UUID", "Local IP", "Public IP", "First enrolled", "Last seen"],
      active.map((d) => [
        d.hostname, d.client_name, d.site, d.status, d.approved ? "yes" : "no", d.model_name, d.model_identifier, d.chip, d.arch,
        d.cpu_cores, d.memory_bytes ? Math.round(d.memory_bytes / 2 ** 30) : "", d.disk_total_bytes ? Math.round(d.disk_total_bytes / 2 ** 30) : "",
        d.os_version ? `${d.os_name || "macOS"} ${d.os_version}` : "", d.os_build, d.agent_version, d.serial_number, d.hardware_uuid,
        d.local_ip, d.public_ip, d.first_enrolled_at, d.last_seen_at,
      ]),
    );

  const health = () =>
    downloadCSV(
      `edgepilot-health-${stamp()}.csv`,
      ["Hostname", "Client", "Status", "CPU %", "Memory %", "Disk %", "Last seen"],
      active.map((d) => [d.hostname, d.client_name, d.status, n(d.last_cpu_percent), n(d.last_memory_percent), n(d.last_disk_percent), d.last_seen_at]),
    );

  const breakdown = (name: string, rows: [string, number][]) => () =>
    downloadCSV(`edgepilot-${name.toLowerCase().replace(/\s+/g, "-")}-${stamp()}.csv`, [name, "Devices"], rows);

  if (!loading && devices.length === 0) {
    return (
      <>
        <PageHeader title="Reports" description="Fleet inventory, availability, compliance, and capacity — exportable as CSV." />
        <Card padded={false}>
          <EmptyState icon="bar-chart" title="Nothing to report yet" description="Reports fill in as soon as the first device enrolls." />
        </Card>
      </>
    );
  }

  const byModel = tally(active, (d) => d.model_name || d.model_identifier || "Unknown");
  const byOS = tally(active, (d) => (d.os_version ? `${d.os_name || "macOS"} ${d.os_version}` : "Unknown"));
  const byAgent = tally(active, (d) => d.agent_version || "Unknown");
  const byClient = tally(active, (d) => d.client_name || "Unassigned");
  const bySite = tally(active, (d) => d.site || "No site");

  return (
    <>
      <PageHeader
        title="Reports"
        description="Fleet inventory, availability, compliance, and capacity — computed live from your devices and exportable as CSV."
        actions={
          <>
            <IconButton icon="refresh" label="Refresh" variant="secondary" onClick={() => void reload()} />
            <Button icon="download" onClick={health} disabled={loading}>Health snapshot</Button>
            <Button variant="primary" icon="download" onClick={inventory} disabled={loading}>Full inventory</Button>
          </>
        }
      />
      <ErrorNote error={error} onRetry={() => void reload()} />

      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <Gauge
          loading={loading}
          icon="activity"
          label="Availability"
          value={availability}
          unit="%"
          detail={`${online} of ${active.length} reachable now`}
          tone={availability >= 95 ? "good" : availability >= 80 ? "warn" : "bad"}
        />
        <Gauge
          loading={loading}
          icon="package"
          label="Agent compliance"
          value={compliance}
          unit="%"
          detail={latestAgent ? `on the newest agent, ${latestAgent}` : "No agent versions reported"}
          tone={compliance >= 95 ? "good" : compliance >= 70 ? "warn" : "bad"}
        />
        <Gauge loading={loading} icon="cpu" label="Fleet memory" text={bytes(totalMem)} detail={`across ${active.length} devices`} />
        <Gauge
          loading={loading}
          icon="server"
          label="Fleet storage"
          text={bytes(totalDisk)}
          detail={`${diskFull.length} device${diskFull.length === 1 ? "" : "s"} above 85% disk`}
        />
      </div>

      <h2 className="mb-3 mt-10 text-sm font-semibold text-ink">Inventory</h2>
      <div className="grid gap-4 lg:grid-cols-2 xl:grid-cols-3">
        <Breakdown title="By model" icon="monitor" rows={byModel} loading={loading} onExport={breakdown("Model", byModel)} />
        <Breakdown title="By operating system" icon="layers" rows={byOS} loading={loading} onExport={breakdown("Operating system", byOS)} />
        <Breakdown title="By agent version" icon="package" rows={byAgent} loading={loading} onExport={breakdown("Agent version", byAgent)} highlight={latestAgent} />
        <Breakdown title="By client" icon="building" rows={byClient} loading={loading} onExport={breakdown("Client", byClient)} />
        <Breakdown title="By site" icon="map-pin" rows={bySite} loading={loading} onExport={breakdown("Site", bySite)} />
        <StatusMix devices={active} loading={loading} />
      </div>

      <h2 className="mb-3 mt-10 text-sm font-semibold text-ink">Action items</h2>
      <div className="grid gap-4 lg:grid-cols-3">
        <ActionList
          title="Offline devices"
          icon="x-circle"
          empty="Every device is reachable."
          loading={loading}
          items={offline.map((d) => ({ device: d, meta: `last seen ${relativeTime(d.last_seen_at)}` }))}
        />
        <ActionList
          title="Outdated agents"
          icon="package"
          empty="Every device runs the newest agent."
          loading={loading}
          items={outdated.map((d) => ({ device: d, meta: `${d.agent_version || "unknown"} → ${latestAgent}` }))}
        />
        <ActionList
          title="Disks filling up"
          icon="server"
          empty="No device is above 85% disk."
          loading={loading}
          items={diskFull.map((d) => ({ device: d, meta: `${Math.round(d.last_disk_percent ?? 0)}% of ${bytes(d.disk_total_bytes)}` }))}
        />
      </div>
    </>
  );
}

const GAUGE_TONE = {
  good: "text-online",
  warn: "text-degraded",
  bad: "text-danger",
} as const;

function Gauge({
  loading, icon, label, value, unit, text, detail, tone,
}: {
  loading: boolean;
  icon: IconName;
  label: string;
  value?: number;
  unit?: string;
  text?: string;
  detail: string;
  tone?: keyof typeof GAUGE_TONE;
}) {
  const r = 22;
  const c = 2 * Math.PI * r;
  return (
    <Card padded={false}>
      <div className="flex items-center gap-4 p-5">
        {value !== undefined ? (
          <svg viewBox="0 0 56 56" className={cn("h-14 w-14 shrink-0 -rotate-90", tone && GAUGE_TONE[tone])} aria-hidden>
            <circle cx="28" cy="28" r={r} fill="none" strokeWidth="6" className="stroke-surface-hover" />
            {!loading && (
              <circle
                cx="28"
                cy="28"
                r={r}
                fill="none"
                strokeWidth="6"
                stroke="currentColor"
                strokeLinecap="round"
                strokeDasharray={`${(value / 100) * c} ${c}`}
              />
            )}
          </svg>
        ) : (
          <span className="flex h-14 w-14 shrink-0 items-center justify-center rounded-2xl bg-accent-subtle text-accent">
            <Icon name={icon} size={22} />
          </span>
        )}
        <div className="min-w-0">
          <div className="text-xs font-medium text-ink-muted">{label}</div>
          {loading ? (
            <Skeleton className="mt-1.5 h-7 w-16" />
          ) : (
            <div className="text-2xl font-semibold tabular tracking-tight text-ink">
              {text ?? value}
              {unit && <span className="text-base text-ink-faint">{unit}</span>}
            </div>
          )}
          <div className="truncate text-xs text-ink-faint">{detail}</div>
        </div>
      </div>
    </Card>
  );
}

function Breakdown({
  title, icon, rows, loading, onExport, highlight,
}: {
  title: string;
  icon: IconName;
  rows: [string, number][];
  loading: boolean;
  onExport: () => void;
  highlight?: string;
}) {
  const max = Math.max(1, ...rows.map(([, c]) => c));
  const total = rows.reduce((s, [, c]) => s + c, 0);
  return (
    <Card padded={false}>
      <div className="flex items-center justify-between px-5 pb-3 pt-5">
        <h3 className="flex items-center gap-2 text-sm font-semibold text-ink">
          <Icon name={icon} size={15} className="text-ink-faint" /> {title}
        </h3>
        <IconButton icon="download" label={`Download ${title.toLowerCase()} as CSV`} size="sm" onClick={onExport} />
      </div>
      <div className="px-5 pb-5">
        {loading ? (
          <div className="space-y-3">{[0, 1, 2].map((i) => <Skeleton key={i} className="h-5 w-full" />)}</div>
        ) : (
          <ul className="space-y-2.5">
            {rows.slice(0, 7).map(([label, count]) => (
              <li key={label} className="text-sm">
                <div className="mb-1 flex items-center justify-between gap-3">
                  <span className={cn("truncate", label === highlight ? "font-semibold text-ink" : "text-ink-muted")}>
                    {label}
                    {label === highlight && (
                      <span className="ml-1.5 rounded-full bg-online/10 px-1.5 py-px text-2xs font-semibold text-online">latest</span>
                    )}
                  </span>
                  <span className="shrink-0 text-xs tabular text-ink-faint">
                    {count} · {Math.round((count / total) * 100)}%
                  </span>
                </div>
                <div className="h-1.5 overflow-hidden rounded-full bg-surface-hover">
                  <div
                    className={cn("h-full rounded-full", label === highlight ? "bg-online" : "bg-accent")}
                    style={{ width: `${(count / max) * 100}%` }}
                  />
                </div>
              </li>
            ))}
            {rows.length > 7 && <li className="pt-1 text-xs text-ink-faint">+ {rows.length - 7} more in the CSV</li>}
          </ul>
        )}
      </div>
    </Card>
  );
}

function StatusMix({ devices, loading }: { devices: Device[]; loading: boolean }) {
  const parts = [
    { label: "Online", count: devices.filter((d) => d.status === "online").length, cls: "bg-online" },
    { label: "Degraded", count: devices.filter((d) => d.status === "degraded").length, cls: "bg-degraded" },
    { label: "Pending", count: devices.filter((d) => d.status === "pending").length, cls: "bg-pending" },
    { label: "Offline", count: devices.filter((d) => d.status === "offline").length, cls: "bg-offline" },
  ];
  const total = Math.max(1, devices.length);
  return (
    <Card>
      <h3 className="flex items-center gap-2 text-sm font-semibold text-ink">
        <Icon name="circle-dot" size={15} className="text-ink-faint" /> Status mix
      </h3>
      {loading ? (
        <Skeleton className="mt-4 h-4 w-full" />
      ) : (
        <>
          <div className="mt-4 flex h-3 overflow-hidden rounded-full bg-surface-hover">
            {parts.map(
              (p) =>
                p.count > 0 && (
                  <div key={p.label} className={cn("h-full", p.cls)} style={{ width: `${(p.count / total) * 100}%` }} title={`${p.label}: ${p.count}`} />
                ),
            )}
          </div>
          <ul className="mt-4 grid grid-cols-2 gap-2.5">
            {parts.map((p) => (
              <li key={p.label} className="flex items-center gap-2 text-sm">
                <span className={cn("h-2.5 w-2.5 rounded-full", p.cls)} />
                <span className="flex-1 text-ink-muted">{p.label}</span>
                <span className="font-semibold tabular text-ink">{p.count}</span>
              </li>
            ))}
          </ul>
        </>
      )}
    </Card>
  );
}

function ActionList({
  title, icon, items, empty, loading,
}: {
  title: string;
  icon: IconName;
  items: { device: Device; meta: string }[];
  empty: string;
  loading: boolean;
}) {
  return (
    <Card padded={false}>
      <div className="flex items-center justify-between px-5 pb-3 pt-5">
        <h3 className="flex items-center gap-2 text-sm font-semibold text-ink">
          <Icon name={icon} size={15} className="text-ink-faint" /> {title}
        </h3>
        <span
          className={cn(
            "rounded-full px-2 py-0.5 text-xs font-semibold tabular",
            items.length ? "bg-degraded/10 text-degraded" : "bg-online/10 text-online",
          )}
        >
          {loading ? "—" : items.length}
        </span>
      </div>
      {loading ? (
        <div className="space-y-2 px-5 pb-5">{[0, 1].map((i) => <Skeleton key={i} className="h-8 w-full" />)}</div>
      ) : items.length === 0 ? (
        <p className="flex items-center gap-2 px-5 pb-5 text-sm text-ink-muted">
          <Icon name="check-circle" size={15} className="text-online" /> {empty}
        </p>
      ) : (
        <ul className="divide-y divide-surface-border border-t border-surface-border">
          {items.slice(0, 6).map(({ device, meta }) => (
            <li key={device.id}>
              <Link href={`/devices/${device.id}`} className="flex items-center justify-between gap-3 px-5 py-2.5 hover:bg-surface-hover/70">
                <span className="min-w-0">
                  <span className="block truncate text-sm font-medium normal-nums text-ink">{device.hostname || device.serial_number}</span>
                  <span className="block truncate text-xs text-ink-faint">{device.client_name || "No client"}</span>
                </span>
                <span className="shrink-0 text-xs text-ink-muted">{meta}</span>
              </Link>
            </li>
          ))}
          {items.length > 6 && (
            <li className="px-5 py-2.5 text-xs text-ink-faint">
              + {items.length - 6} more — see <Link href="/devices" className="font-medium text-accent">Devices</Link>
            </li>
          )}
        </ul>
      )}
    </Card>
  );
}
