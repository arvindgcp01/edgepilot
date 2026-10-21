"use client";

/**
 * The fleet as tiles: one card per device with its three gauges and an hour
 * of CPU as a sparkline. This is the wall-display view — glanceable from
 * across a room — where the table is the working view.
 *
 * Sparklines are fetched lazily and a few at a time. Twenty-five tiles is
 * twenty-five requests; firing them at once on a page load is how a dashboard
 * DDoSes its own API every time someone changes a filter.
 */
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useRef, useState } from "react";
import { Badge, Button, StatusBadge } from "@/components/ui";
import { Meter, Sparkline } from "@/components/charts";
import { getDeviceMetrics, type Device, type MetricPoint } from "@/lib/api/devices";
import { relativeTime } from "@/lib/format";
import { can } from "@/lib/api/client";
import { cn } from "@/lib/cn";

const SPARKLINE_WINDOW_MS = 3600_000;
const CONCURRENCY = 4;

/** An hour of CPU per device, fetched a few at a time and cached for the
 *  page's life. Live events extend the cached series in place. */
export function useSparklines(devices: Device[]) {
  const [series, setSeries] = useState<Record<string, MetricPoint[]>>({});
  const inFlight = useRef(new Set<string>());
  const fetched = useRef(new Set<string>());

  useEffect(() => {
    let cancelled = false;
    const queue = devices
      .filter((d) => d.last_cpu_percent !== null && !fetched.current.has(d.id) && !inFlight.current.has(d.id))
      .map((d) => d.id);

    async function worker() {
      while (queue.length > 0 && !cancelled) {
        const id = queue.shift();
        if (!id) return;
        inFlight.current.add(id);
        try {
          const result = await getDeviceMetrics(id, {
            from: new Date(Date.now() - SPARKLINE_WINDOW_MS), resolution: "1m", metrics: ["cpu"],
          });
          const cpu = result.series.find((s) => s.name === "cpu_percent")?.points ?? [];
          if (!cancelled) setSeries((current) => ({ ...current, [id]: cpu }));
          fetched.current.add(id);
        } catch {
          // A tile without a sparkline still has its gauges.
        } finally {
          inFlight.current.delete(id);
        }
      }
    }
    for (let i = 0; i < CONCURRENCY; i++) void worker();
    return () => { cancelled = true; };
  }, [devices]);

  // Extend from the fleet's live values, which arrive every thirty seconds.
  useEffect(() => {
    setSeries((current) => {
      let changed = false;
      const next = { ...current };
      for (const d of devices) {
        const points = next[d.id];
        if (!points || d.last_cpu_percent === null || !d.last_seen_at) continue;
        const ms = new Date(d.last_seen_at).getTime();
        const last = points[points.length - 1]?.[0] ?? 0;
        if (ms > last + 15_000) {
          next[d.id] = [...points.filter(([p]) => p > Date.now() - SPARKLINE_WINDOW_MS), [ms, d.last_cpu_percent]];
          changed = true;
        }
      }
      return changed ? next : current;
    });
  }, [devices]);

  return series;
}

export function FleetGrid({
  devices,
  onConnect,
}: {
  devices: Device[];
  onConnect?: (device: Device) => void;
}) {
  const router = useRouter();
  const sparklines = useSparklines(devices);
  const canConnect = can("ssh:connect");

  return (
    <div className="grid gap-3 p-4 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
      {devices.map((device) => {
        const reported = device.last_cpu_percent !== null;
        const hot = (device.last_cpu_percent ?? 0) >= 80 || (device.last_memory_percent ?? 0) >= 80 || (device.last_disk_percent ?? 0) >= 95;
        return (
          <div
            key={device.id}
            role="link"
            tabIndex={0}
            onClick={() => router.push(`/devices/${device.id}`)}
            onKeyDown={(e) => { if (e.key === "Enter") router.push(`/devices/${device.id}`); }}
            className={cn(
              "group cursor-pointer rounded-lg border bg-surface-raised p-4 transition-colors hover:border-surface-border-strong hover:bg-surface-hover/40",
              hot ? "border-degraded/40" : "border-surface-border",
              device.status === "offline" && "opacity-70",
            )}
          >
            <div className="mb-3 flex items-start justify-between gap-2">
              <div className="min-w-0">
                <Link href={`/devices/${device.id}`} onClick={(e) => e.stopPropagation()} className="block truncate text-sm font-medium text-ink hover:text-accent-soft">
                  {device.hostname || "(unnamed)"}
                </Link>
                <div className="truncate text-xs text-ink-muted">
                  {device.client_name || device.model_name || device.serial_number}
                  {device.site && <span className="text-ink-faint"> · {device.site}</span>}
                </div>
              </div>
              <div className="flex shrink-0 flex-col items-end gap-1">
                <StatusBadge status={device.status} />
                {!device.approved && <Badge tone="info">Needs approval</Badge>}
              </div>
            </div>

            <div className="mb-3 flex items-end justify-between gap-3">
              <div className="space-y-1.5">
                <Row label="CPU" value={device.last_cpu_percent} />
                <Row label="Mem" value={device.last_memory_percent} />
                <Row label="Disk" value={device.last_disk_percent} />
              </div>
              <div className="flex flex-col items-end">
                {reported ? (
                  <Sparkline points={sparklines[device.id] ?? []} max={100} width={104} height={32} />
                ) : (
                  <span className="text-2xs text-ink-faint">No metrics</span>
                )}
                <span className="mt-1 text-2xs text-ink-faint">last hour</span>
              </div>
            </div>

            <div className="flex items-center justify-between gap-2 border-t border-surface-border pt-2.5 text-xs text-ink-faint">
              <span>
                <span className="font-mono">{device.agent_version || "—"}</span> · seen {relativeTime(device.last_seen_at)}
              </span>
              {canConnect && onConnect && device.status === "online" && device.approved && (
                <Button
                  size="sm"
                  variant="ghost"
                  icon="terminal"
                  className="-my-1 opacity-0 transition-opacity group-hover:opacity-100 focus:opacity-100"
                  onClick={(e) => { e.stopPropagation(); onConnect(device); }}
                >
                  Connect
                </Button>
              )}
            </div>
          </div>
        );
      })}
    </div>
  );
}

function Row({ label, value }: { label: string; value: number | null }) {
  return (
    <div className="flex items-center gap-2 text-xs">
      <span className="w-8 text-ink-faint">{label}</span>
      <Meter value={value} label={label} width="w-24" />
    </div>
  );
}
