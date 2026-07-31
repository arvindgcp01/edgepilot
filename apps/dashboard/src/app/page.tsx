"use client";

import Link from "next/link";
import { useState } from "react";
import { AppShell } from "@/components/app-shell";
import { Card, Empty, ErrorNote, Input, Spinner, StatusBadge } from "@/components/ui";
import { useFleet } from "@/hooks/use-fleet";
import { bytes, relativeTime } from "@/lib/format";
import type { DeviceStatus } from "@/lib/api/devices";
import { cn } from "@/lib/cn";

const FILTERS: { label: string; status?: DeviceStatus[] }[] = [
  { label: "All" },
  { label: "Online", status: ["online"] },
  { label: "Offline", status: ["offline"] },
  { label: "Pending", status: ["pending"] },
  { label: "Degraded", status: ["degraded"] },
];

export default function FleetPage() {
  return (
    <AppShell>
      <FleetView />
    </AppShell>
  );
}

function FleetView() {
  const [search, setSearch] = useState("");
  const [filter, setFilter] = useState(0);

  const { devices, summary, loading, error, live } = useFleet({
    q: search || undefined,
    status: FILTERS[filter]?.status,
    limit: 100,
  });

  return (
    <>
      <div className="mb-6 flex items-center gap-3">
        <h1 className="text-xl font-semibold">Fleet</h1>
        <span
          className={cn(
            "inline-flex items-center gap-1.5 text-xs",
            live ? "text-online" : "text-ink-faint",
          )}
          title={live ? "Receiving live updates" : "Reconnecting to the event stream"}
        >
          <span className="h-1.5 w-1.5 rounded-full bg-current" aria-hidden />
          {live ? "live" : "reconnecting"}
        </span>
      </div>

      <div className="mb-6 grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
        <SummaryCard label="Devices" value={summary?.total} />
        <SummaryCard label="Online" value={summary?.online} tone="text-online" />
        <SummaryCard label="Offline" value={summary?.offline} tone="text-offline" />
        <SummaryCard label="Degraded" value={summary?.degraded} tone="text-degraded" />
        <SummaryCard label="Awaiting approval" value={summary?.pending_approval} tone="text-pending" />
      </div>

      <div className="mb-4 flex flex-wrap items-center gap-2">
        <Input
          className="max-w-xs"
          placeholder="Search hostname, serial, or site…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          aria-label="Search devices"
        />
        <div className="flex gap-1">
          {FILTERS.map((option, index) => (
            <button
              key={option.label}
              onClick={() => setFilter(index)}
              aria-pressed={filter === index}
              className={cn(
                "rounded-md px-2.5 py-1.5 text-sm",
                filter === index
                  ? "bg-surface-raised text-ink"
                  : "text-ink-muted hover:text-ink",
              )}
            >
              {option.label}
            </button>
          ))}
        </div>
      </div>

      <ErrorNote error={error} />

      {loading ? (
        <Spinner />
      ) : devices.length === 0 ? (
        <Empty>
          No devices match. Install an agent with an enrollment token to add one.
        </Empty>
      ) : (
        <Card className="overflow-x-auto p-0">
          <table className="w-full text-sm">
            <thead className="border-b border-surface-border text-left text-xs uppercase tracking-wide text-ink-faint">
              <tr>
                <th scope="col" className="px-4 py-2.5 font-medium">Hostname</th>
                <th scope="col" className="px-4 py-2.5 font-medium">Status</th>
                <th scope="col" className="px-4 py-2.5 font-medium">Model</th>
                <th scope="col" className="px-4 py-2.5 font-medium">macOS</th>
                <th scope="col" className="px-4 py-2.5 font-medium">Memory</th>
                <th scope="col" className="px-4 py-2.5 font-medium">Agent</th>
                <th scope="col" className="px-4 py-2.5 font-medium">Last seen</th>
              </tr>
            </thead>
            <tbody>
              {devices.map((device) => (
                <tr
                  key={device.id}
                  className="border-b border-surface-border/60 last:border-0 hover:bg-surface-border/30"
                >
                  <td className="px-4 py-2.5">
                    <Link href={`/devices/${device.id}`} className="font-medium hover:text-pending">
                      {device.hostname || "(unnamed)"}
                    </Link>
                    <div className="font-mono text-xs text-ink-faint">{device.serial_number}</div>
                  </td>
                  <td className="px-4 py-2.5">
                    <StatusBadge status={device.status} />
                    {!device.approved && (
                      <span className="ml-2 text-xs text-pending">needs approval</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5 text-ink-muted">
                    {device.model_name || device.model_identifier || "—"}
                    {device.chip && <div className="text-xs text-ink-faint">{device.chip}</div>}
                  </td>
                  <td className="px-4 py-2.5 text-ink-muted">{device.os_version || "—"}</td>
                  <td className="px-4 py-2.5 text-ink-muted">{bytes(device.memory_bytes)}</td>
                  <td className="px-4 py-2.5 font-mono text-xs text-ink-muted">
                    {device.agent_version || "—"}
                  </td>
                  <td className="px-4 py-2.5 text-ink-muted" title={device.last_seen_at}>
                    {relativeTime(device.last_seen_at)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
    </>
  );
}

function SummaryCard({ label, value, tone }: { label: string; value?: number; tone?: string }) {
  return (
    <Card>
      <div className="text-xs uppercase tracking-wide text-ink-faint">{label}</div>
      <div className={cn("mt-1 text-2xl font-semibold tabular-nums", tone)}>
        {value ?? "—"}
      </div>
    </Card>
  );
}
