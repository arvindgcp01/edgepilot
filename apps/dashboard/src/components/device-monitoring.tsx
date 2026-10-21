"use client";

/**
 * The monitoring panel for one device.
 *
 * One range control scopes every chart below it. Each chart is one measure —
 * CPU, memory, disk, load, network, temperature — on its own axis; two measures
 * of different scale never share one. The panel stays live: the device's own
 * event topic delivers each new sample and it is appended to the series in
 * place, so the line moves without a refetch.
 */
import { useEffect, useMemo, useState } from "react";
import { Badge, Button, Card, ErrorNote, Facts, Segmented, Skeleton } from "@/components/ui";
import { LineChart, Legend, SERIES, fmtBytesPerSec, fmtCelsius, fmtLoad, fmtPercent, fmtCount, formatUptime, type Series } from "@/components/charts";
import { useAsync } from "@/hooks/use-async";
import { getDeviceMetrics, getLatestMetrics, type Device, type MetricName, type MetricPoint, type MetricSample, type MetricsEvent } from "@/lib/api/devices";
import { connectEvents } from "@/lib/events";
import { bytes, relativeTime } from "@/lib/format";
import { cn } from "@/lib/cn";

type Range = "1h" | "6h" | "24h" | "7d";
const RANGE_MS: Record<Range, number> = { "1h": 3600_000, "6h": 6 * 3600_000, "24h": 24 * 3600_000, "7d": 7 * 24 * 3600_000 };

export function DeviceMonitoring({ device }: { device: Device }) {
  const [range, setRange] = useState<Range>("6h");
  const [refreshKey, setRefreshKey] = useState(0);
  const [live, setLive] = useState(false);

  // The window is anchored when the range changes and slides as samples
  // arrive, so a chart left open for an hour still shows the last hour.
  const [now, setNow] = useState(() => Date.now());
  const from = now - RANGE_MS[range];

  const history = useAsync(
    () => getDeviceMetrics(device.id, { from: new Date(now - RANGE_MS[range]), to: new Date(now) }),
    [device.id, range, refreshKey],
  );
  const latest = useAsync(() => getLatestMetrics(device.id).catch(() => null), [device.id, refreshKey]);

  // Live samples, appended in place.
  const [appended, setAppended] = useState<Record<string, MetricPoint[]>>({});
  useEffect(() => {
    setAppended({});
    const disconnect = connectEvents({
      topics: [`device:${device.id}`],
      onStatus: setLive,
      onEvent: (event) => {
        if (event.type !== "device.metrics") return;
        const data = event.data as MetricsEvent | undefined;
        if (!data) return;
        const ms = new Date(data.at).getTime();
        setNow(Date.now());
        setAppended((current) => ({
          ...current,
          cpu_percent: [...(current.cpu_percent ?? []), [ms, data.cpu_percent]],
          memory_percent: [...(current.memory_percent ?? []), [ms, data.memory_percent]],
          disk_percent: [...(current.disk_percent ?? []), [ms, data.disk_percent]],
        }));
        void latest.reload();
      },
    });
    return () => disconnect();
    // latest.reload is stable for the device.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [device.id]);

  const points = useMemo(() => {
    const byName: Partial<Record<MetricName, MetricPoint[]>> = {};
    for (const s of history.data?.series ?? []) byName[s.name] = s.points;
    for (const [name, extra] of Object.entries(appended)) {
      const base = byName[name as MetricName] ?? [];
      const lastMs = base[base.length - 1]?.[0] ?? 0;
      byName[name as MetricName] = [...base, ...extra.filter(([ms]) => ms > lastMs)];
    }
    return byName;
  }, [history.data, appended]);

  const series = (name: MetricName, label: string, color?: string): Series => ({
    name: label,
    points: points[name] ?? [],
    color,
  });

  const sample = latest.data;
  const never = !history.loading && !history.error && (history.data?.series ?? []).every((s) => s.points.every(([, v]) => v === null)) && !sample;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-3">
          <h2 className="text-sm font-semibold text-ink">Monitoring</h2>
          <span className={cn("inline-flex items-center gap-1.5 text-xs", live ? "text-online" : "text-ink-faint")}>
            <span className={cn("h-1.5 w-1.5 rounded-full bg-current", live && "animate-pulse")} aria-hidden />
            {live ? "Live" : "Reconnecting"}
          </span>
          {sample && <span className="text-xs text-ink-faint">Last reading {relativeTime(sample.time)}</span>}
        </div>
        <div className="flex items-center gap-2">
          <Segmented<Range>
            aria-label="Time range"
            size="sm"
            value={range}
            onChange={(r) => { setNow(Date.now()); setRange(r); }}
            options={[
              { value: "1h", label: "1h" },
              { value: "6h", label: "6h" },
              { value: "24h", label: "24h" },
              { value: "7d", label: "7d" },
            ]}
          />
          <Button size="sm" variant="ghost" icon="refresh" onClick={() => { setNow(Date.now()); setRefreshKey((k) => k + 1); }}>
            <span className="sr-only">Refresh</span>
          </Button>
        </div>
      </div>

      <ErrorNote error={history.error} onRetry={() => setRefreshKey((k) => k + 1)} />

      {never ? (
        <Card>
          <div className="flex items-start gap-3">
            <Badge tone="neutral">No metrics</Badge>
            <div className="text-sm text-ink-muted">
              This device has never reported a health sample. Metrics are sent by agent <span className="font-mono text-ink">2.1.0</span> and
              later; it is running <span className="font-mono text-ink">{device.agent_version || "an unknown version"}</span>.
            </div>
          </div>
        </Card>
      ) : (
        <>
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
            <Stat label="CPU" value={sample ? fmtPercent(sample.cpu_percent) : undefined} sub={sample?.load_1 != null ? `load ${sample.load_1.toFixed(2)}` : undefined} loading={latest.loading} />
            <Stat label="Memory" value={sample?.memory_percent != null ? fmtPercent(sample.memory_percent) : undefined} sub={sample?.memory_used_bytes != null ? `${bytes(sample.memory_used_bytes)} of ${bytes(sample.memory_total_bytes ?? undefined)}` : undefined} loading={latest.loading} />
            <Stat label="Disk" value={sample?.disk_percent != null ? fmtPercent(sample.disk_percent) : undefined} sub={sample?.disk_used_bytes != null ? `${bytes(sample.disk_used_bytes)} of ${bytes(sample.disk_total_bytes ?? undefined)}` : undefined} loading={latest.loading} />
            <Stat label="Uptime" value={sample?.uptime_seconds != null ? formatUptime(sample.uptime_seconds) : undefined} sub={sample?.process_count != null ? `${sample.process_count} processes` : undefined} loading={latest.loading} />
          </div>

          <div className={cn("grid gap-4 lg:grid-cols-2", history.loading && history.data && "opacity-60 transition-opacity")}>
            <ChartCard title="CPU" subtitle="Percent busy, all cores" loading={history.loading && !history.data}>
              <LineChart series={[series("cpu_percent", "CPU")]} format={fmtPercent} max={100} from={from} to={now} />
            </ChartCard>
            <ChartCard title="Memory" subtitle="Used, as a share of physical memory" loading={history.loading && !history.data}>
              <LineChart series={[series("memory_percent", "Memory")]} format={fmtPercent} max={100} from={from} to={now} />
            </ChartCard>
            <ChartCard title="Disk" subtitle="Startup volume, used" loading={history.loading && !history.data}>
              <LineChart series={[series("disk_percent", "Disk")]} format={fmtPercent} max={100} from={from} to={now} />
            </ChartCard>
            <ChartCard
              title="Load average"
              subtitle={device.cpu_cores ? `${device.cpu_cores} cores — a load above that means waiting` : undefined}
              loading={history.loading && !history.data}
              legend={<Legend series={[series("load_1", "1 min", SERIES[0]), series("load_5", "5 min", SERIES[1]), series("load_15", "15 min", SERIES[2])]} />}
            >
              <LineChart
                series={[series("load_1", "1 min", SERIES[0]), series("load_5", "5 min", SERIES[1]), series("load_15", "15 min", SERIES[2])]}
                format={fmtLoad}
                area={false}
                from={from}
                to={now}
              />
            </ChartCard>
            <ChartCard
              title="Network"
              subtitle="All physical interfaces"
              loading={history.loading && !history.data}
              legend={<Legend series={[series("net_rx_bytes_per_sec", "Received", SERIES[0]), series("net_tx_bytes_per_sec", "Sent", SERIES[1])]} />}
            >
              <LineChart
                series={[series("net_rx_bytes_per_sec", "Received", SERIES[0]), series("net_tx_bytes_per_sec", "Sent", SERIES[1])]}
                format={fmtBytesPerSec}
                area={false}
                from={from}
                to={now}
              />
            </ChartCard>
            <ChartCard title="Temperature" subtitle="CPU die. Apple silicon exposes no sensor" loading={history.loading && !history.data}>
              <LineChart
                series={[series("temperature_c", "Temperature", SERIES[1])]}
                format={fmtCelsius}
                from={from}
                to={now}
                emptyLabel="No temperature sensor readable on this Mac"
              />
            </ChartCard>
          </div>

          {sample && <LatestFacts sample={sample} />}
        </>
      )}
    </div>
  );
}

function Stat({ label, value, sub, loading }: { label: string; value?: string; sub?: string; loading: boolean }) {
  return (
    <div className="rounded-lg border border-surface-border bg-surface-raised px-4 py-3">
      <div className="text-xs font-medium text-ink-faint">{label}</div>
      {loading && value === undefined ? (
        <Skeleton className="mt-1.5 h-6 w-16" />
      ) : (
        <div className={cn("mt-0.5 text-xl font-semibold", value === undefined ? "text-ink-faint" : "text-ink")}>{value ?? "—"}</div>
      )}
      <div className="mt-0.5 h-4 text-xs text-ink-muted">{sub ?? ""}</div>
    </div>
  );
}

function ChartCard({ title, subtitle, legend, loading, children }: { title: string; subtitle?: string; legend?: React.ReactNode; loading: boolean; children: React.ReactNode }) {
  return (
    <Card padded={false} className="p-4">
      <div className="mb-3 flex flex-wrap items-baseline justify-between gap-2">
        <div>
          <div className="text-sm font-medium text-ink">{title}</div>
          {subtitle && <div className="text-xs text-ink-faint">{subtitle}</div>}
        </div>
        {legend}
      </div>
      {loading ? <Skeleton className="h-40 w-full" /> : children}
    </Card>
  );
}

function LatestFacts({ sample }: { sample: MetricSample }) {
  return (
    <Card>
      <h3 className="mb-3 text-sm font-semibold text-ink">Latest sample</h3>
      <div className="grid gap-x-8 gap-y-2 md:grid-cols-2">
        <Facts rows={[
          { label: "Collected", value: new Date(sample.time).toLocaleString() },
          { label: "CPU user / system", value: sample.cpu_user_percent != null ? `${sample.cpu_user_percent.toFixed(1)}% / ${(sample.cpu_system_percent ?? 0).toFixed(1)}%` : "—" },
          { label: "Memory pressure", value: sample.memory_pressure != null ? fmtPercent(sample.memory_pressure) : "—" },
          { label: "Swap used", value: sample.swap_used_bytes != null ? bytes(sample.swap_used_bytes) : "—" },
        ]} />
        <Facts rows={[
          { label: "Network since boot", value: sample.net_rx_bytes != null ? `↓ ${bytes(sample.net_rx_bytes)} · ↑ ${bytes(sample.net_tx_bytes ?? undefined)}` : "—" },
          { label: "Processes", value: sample.process_count != null ? fmtCount(sample.process_count) : "—" },
          { label: "Temperature", value: sample.temperature_c != null ? fmtCelsius(sample.temperature_c) : "No sensor" },
          { label: "Uptime", value: formatUptime(sample.uptime_seconds) },
        ]} />
      </div>
    </Card>
  );
}
