"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useMemo, useState } from "react";
import { AppShell } from "@/components/app-shell";
import { useSession } from "@/components/session-provider";
import { openPalette } from "@/components/command-palette";
import {
  Button, Card, EmptyState, ErrorNote, Icon, Kbd, Skeleton, StatusBadge, useToast, type IconName,
} from "@/components/ui";
import { useFleet } from "@/hooks/use-fleet";
import { can } from "@/lib/api/client";
import { approveDevice, type Device, type DeviceStatus } from "@/lib/api/devices";
import { relativeTime } from "@/lib/format";
import { cn } from "@/lib/cn";

export default function OverviewPage() {
  return (
    <AppShell>
      <Overview />
    </AppShell>
  );
}

function greeting(now = new Date()): string {
  const h = now.getHours();
  if (h < 5) return "Good evening";
  if (h < 12) return "Good morning";
  if (h < 18) return "Good afternoon";
  return "Good evening";
}

/**
 * The home page: how is the fleet, and what needs me.
 *
 * Everything here is derived from the same device list the Devices page shows,
 * so the two can never disagree. The "Needs attention" list is the point of the
 * page — every row in it is something a person should look at, with the one
 * click that resolves it where one exists.
 */
function Overview() {
  const { principal } = useSession();
  const { devices, summary, loading, error, live, reload, applyDevice } = useFleet({ limit: 200 });

  const name = principal?.email.split("@")[0] ?? "";
  const total = summary?.total ?? devices.length;
  const firstLoad = loading && devices.length === 0;

  return (
    <>
      <div className="mb-8 flex flex-wrap items-end justify-between gap-4">
        <div>
          <p className="text-sm font-medium text-ink-muted">{greeting()}{name && `, ${name}`}</p>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight text-ink">Fleet overview</h1>
        </div>
        <div className="flex items-center gap-2">
          <LiveIndicator live={live} />
          <Button icon="search" onClick={openPalette} className="hidden sm:inline-flex">
            Find a device <span className="ml-1 hidden gap-0.5 sm:flex"><Kbd>⌘</Kbd><Kbd>K</Kbd></span>
          </Button>
          {can("enrollment_token:write") && (
            <Link href="/enrollment?new=1">
              <Button variant="primary" icon="plus">Enroll device</Button>
            </Link>
          )}
        </div>
      </div>

      <ErrorNote error={error} onRetry={() => void reload()} />

      {!firstLoad && total === 0 ? (
        <GettingStarted />
      ) : (
        <>
          <KpiRow devices={devices} loading={firstLoad} summary={summary} />

          <div className="mt-6 grid gap-6 xl:grid-cols-3">
            <div className="space-y-6 xl:col-span-2">
              <NeedsAttention devices={devices} loading={firstLoad} onApproved={applyDevice} />
              <ResourceHotspots devices={devices} loading={firstLoad} />
            </div>
            <div className="space-y-6">
              <StatusBreakdown devices={devices} loading={firstLoad} />
              <TopClients devices={devices} loading={firstLoad} />
              <VersionSpread devices={devices} loading={firstLoad} />
            </div>
          </div>
        </>
      )}
    </>
  );
}

function LiveIndicator({ live }: { live: boolean }) {
  return (
    <span
      className={cn(
        "mr-1 inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium",
        live ? "border-online/20 bg-online/10 text-online" : "border-surface-border bg-surface-raised text-ink-faint",
      )}
      title={live ? "Receiving live updates" : "Reconnecting to the event stream"}
    >
      <span className="relative flex h-1.5 w-1.5">
        {live && <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-online opacity-60" />}
        <span className="relative inline-flex h-1.5 w-1.5 rounded-full bg-current" />
      </span>
      {live ? "Live" : "Reconnecting"}
    </span>
  );
}

// --- KPIs ------------------------------------------------------------------

function isHot(d: Device) {
  return (d.last_cpu_percent ?? 0) >= 80 || (d.last_memory_percent ?? 0) >= 80 || (d.last_disk_percent ?? 0) >= 95;
}

function KpiRow({
  devices,
  summary,
  loading,
}: {
  devices: Device[];
  summary: ReturnType<typeof useFleet>["summary"];
  loading: boolean;
}) {
  const total = summary?.total ?? devices.length;
  const online = summary?.online ?? devices.filter((d) => d.status === "online").length;
  const uptime = total ? Math.round((online / total) * 100) : 0;
  const attention =
    devices.filter((d) => d.status === "offline" || d.status === "degraded" || !d.approved || isHot(d)).length;
  const reporting = devices.filter((d) => d.last_cpu_percent !== null && d.status !== "offline");
  const avgCpu = reporting.length
    ? Math.round(reporting.reduce((sum, d) => sum + (d.last_cpu_percent ?? 0), 0) / reporting.length)
    : null;

  const cards: { label: string; value: React.ReactNode; sub: React.ReactNode; icon: IconName; tone: string; href?: string }[] = [
    {
      label: "Total devices",
      value: total,
      sub: `${summary?.pending_approval ?? 0} awaiting approval`,
      icon: "monitor",
      tone: "bg-indigo-500/10 text-indigo-600 dark:text-indigo-300",
      href: "/devices",
    },
    {
      label: "Online now",
      value: (
        <span className="flex items-baseline gap-2">
          {online}
          <span className="text-sm font-medium text-ink-faint">/ {total}</span>
        </span>
      ),
      sub: <ProgressBar value={uptime} tone="bg-online" label={`${uptime}% reachable`} />,
      icon: "activity",
      tone: "bg-emerald-500/10 text-emerald-600 dark:text-emerald-300",
    },
    {
      label: "Needs attention",
      value: attention,
      sub: attention ? "Offline, degraded, or unapproved" : "Nothing waiting on you",
      icon: "warning",
      tone: attention ? "bg-amber-500/10 text-amber-600 dark:text-amber-300" : "bg-slate-500/10 text-slate-500",
    },
    {
      label: "Average CPU",
      value: avgCpu === null ? "—" : `${avgCpu}%`,
      sub: `${reporting.length} reporting`,
      icon: "cpu",
      tone: "bg-sky-500/10 text-sky-600 dark:text-sky-300",
    },
  ];

  return (
    <div className="grid grid-cols-2 gap-3 sm:gap-4 xl:grid-cols-4">
      {cards.map((card) => {
        const body = (
          <Card
            className={cn("h-full transition-shadow", card.href && "hover:border-surface-border-strong hover:shadow-md")}
            padded={false}
          >
            <div className="p-4 sm:p-5">
              <div className="flex items-center justify-between gap-2">
                <span className="text-xs font-medium text-ink-muted sm:text-sm">{card.label}</span>
                <span className={cn("hidden h-9 w-9 shrink-0 items-center justify-center rounded-lg sm:flex", card.tone)}>
                  <Icon name={card.icon} size={17} />
                </span>
              </div>
              {loading ? (
                <Skeleton className="mt-3 h-8 w-20" />
              ) : (
                <div className="mt-2 text-2xl font-semibold tracking-tight tabular text-ink sm:text-3xl">{card.value}</div>
              )}
              <div className="mt-2 text-xs text-ink-faint">{loading ? <Skeleton className="h-3 w-32" /> : card.sub}</div>
            </div>
          </Card>
        );
        return card.href ? (
          <Link key={card.label} href={card.href} className="block rounded-xl">
            {body}
          </Link>
        ) : (
          <div key={card.label}>{body}</div>
        );
      })}
    </div>
  );
}

function ProgressBar({ value, tone, label }: { value: number; tone: string; label: string }) {
  return (
    <span className="flex items-center gap-2">
      <span className="h-1.5 flex-1 overflow-hidden rounded-full bg-surface-hover">
        <span className={cn("block h-full rounded-full", tone)} style={{ width: `${value}%` }} />
      </span>
      <span className="shrink-0">{label}</span>
    </span>
  );
}

// --- Needs attention ---------------------------------------------------------

type Issue = { device: Device; reason: string; icon: IconName; tone: string; rank: number };

function issuesFor(devices: Device[]): Issue[] {
  const out: Issue[] = [];
  for (const d of devices) {
    if (!d.approved && d.status !== "decommissioned") {
      out.push({ device: d, reason: "Waiting for approval", icon: "shield", tone: "text-pending bg-pending/10", rank: 0 });
    } else if (d.status === "degraded") {
      out.push({ device: d, reason: "Reporting degraded health", icon: "warning", tone: "text-degraded bg-degraded/10", rank: 1 });
    } else if (d.status === "offline") {
      out.push({
        device: d,
        reason: `Offline since ${relativeTime(d.last_seen_at)}`,
        icon: "x-circle",
        tone: "text-offline bg-offline/10",
        rank: 2,
      });
    } else if (isHot(d)) {
      const parts = [
        (d.last_cpu_percent ?? 0) >= 80 && `CPU ${Math.round(d.last_cpu_percent ?? 0)}%`,
        (d.last_memory_percent ?? 0) >= 80 && `memory ${Math.round(d.last_memory_percent ?? 0)}%`,
        (d.last_disk_percent ?? 0) >= 95 && `disk ${Math.round(d.last_disk_percent ?? 0)}%`,
      ].filter(Boolean);
      out.push({ device: d, reason: `Under pressure: ${parts.join(", ")}`, icon: "trending-up", tone: "text-degraded bg-degraded/10", rank: 3 });
    }
  }
  return out.sort((a, b) => a.rank - b.rank);
}

function NeedsAttention({
  devices,
  loading,
  onApproved,
}: {
  devices: Device[];
  loading: boolean;
  onApproved: (d: Device) => void;
}) {
  const router = useRouter();
  const { push } = useToast();
  const [busy, setBusy] = useState<string | null>(null);
  const issues = useMemo(() => issuesFor(devices), [devices]);
  const shown = issues.slice(0, 6);

  async function approve(d: Device) {
    setBusy(d.id);
    try {
      onApproved(await approveDevice(d.id));
      push({ title: `${d.hostname || d.serial_number} approved`, description: "It can now receive commands.", tone: "success" });
    } catch (err) {
      push({ title: "Could not approve", description: err instanceof Error ? err.message : undefined, tone: "error" });
    } finally {
      setBusy(null);
    }
  }

  return (
    <Card padded={false}>
      <PanelHeader
        title="Needs attention"
        description="Devices that are offline, unhealthy, or waiting on a decision."
        action={
          issues.length > shown.length ? (
            <Link href="/devices" className="text-sm font-medium text-accent hover:text-accent-hover dark:text-accent-soft">
              View all {issues.length}
            </Link>
          ) : undefined
        }
      />
      {loading ? (
        <div className="space-y-3 p-5">{[0, 1, 2].map((i) => <Skeleton key={i} className="h-12 w-full" />)}</div>
      ) : shown.length === 0 ? (
        <EmptyState
          icon="check-circle"
          title="All clear"
          description="Every device is online, healthy, and approved."
          className="py-12"
        />
      ) : (
        <ul className="divide-y divide-surface-border">
          {shown.map((issue) => (
            <li key={issue.device.id}>
              <div
                role="button"
                tabIndex={0}
                onClick={() => router.push(`/devices/${issue.device.id}`)}
                onKeyDown={(e) => e.key === "Enter" && router.push(`/devices/${issue.device.id}`)}
                className="flex cursor-pointer items-center gap-4 px-5 py-3.5 transition-colors hover:bg-surface-hover/70"
              >
                <span className={cn("flex h-9 w-9 shrink-0 items-center justify-center rounded-lg", issue.tone)}>
                  <Icon name={issue.icon} size={16} />
                </span>
                <span className="min-w-0 flex-1">
                  <span className="flex items-center gap-2">
                    <span className="truncate text-sm font-semibold text-ink">
                      {issue.device.hostname || issue.device.serial_number}
                    </span>
                    {issue.device.client_name && (
                      <span className="hidden truncate rounded-md bg-surface-hover px-1.5 py-0.5 text-2xs font-medium text-ink-muted sm:inline">
                        {issue.device.client_name}
                      </span>
                    )}
                  </span>
                  <span className="mt-0.5 block truncate text-xs text-ink-muted">{issue.reason}</span>
                </span>
                {!issue.device.approved && can("device:approve") ? (
                  <Button
                    size="sm"
                    variant="primary"
                    loading={busy === issue.device.id}
                    onClick={(e) => {
                      e.stopPropagation();
                      void approve(issue.device);
                    }}
                  >
                    Approve
                  </Button>
                ) : (
                  <StatusBadge status={issue.device.status} />
                )}
                <Icon name="chevron-right" size={16} className="hidden text-ink-faint sm:block" />
              </div>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

// --- Resource hotspots ---------------------------------------------------------

function ResourceHotspots({ devices, loading }: { devices: Device[]; loading: boolean }) {
  const top = useMemo(
    () =>
      devices
        .filter((d) => d.status !== "offline" && d.last_cpu_percent !== null)
        .map((d) => ({ d, peak: Math.max(d.last_cpu_percent ?? 0, d.last_memory_percent ?? 0, d.last_disk_percent ?? 0) }))
        .sort((a, b) => b.peak - a.peak)
        .slice(0, 5),
    [devices],
  );

  return (
    <Card padded={false}>
      <PanelHeader title="Busiest devices" description="Highest current CPU, memory, or disk usage." />
      {loading ? (
        <div className="space-y-3 p-5">{[0, 1, 2].map((i) => <Skeleton key={i} className="h-10 w-full" />)}</div>
      ) : top.length === 0 ? (
        <div className="px-5 pb-6 text-sm text-ink-muted">No device has reported metrics yet.</div>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left text-xs font-medium text-ink-faint">
                <th className="px-5 pb-2 font-medium">Device</th>
                <th className="px-3 pb-2 font-medium">CPU</th>
                <th className="px-3 pb-2 font-medium">Memory</th>
                <th className="px-5 pb-2 font-medium">Disk</th>
              </tr>
            </thead>
            <tbody>
              {top.map(({ d }) => (
                <tr key={d.id} className="border-t border-surface-border">
                  <td className="px-5 py-3">
                    <Link href={`/devices/${d.id}`} className="font-medium normal-nums text-ink hover:text-accent">
                      {d.hostname || d.serial_number}
                    </Link>
                    <div className="text-xs text-ink-faint">{d.model_name || d.model_identifier}</div>
                  </td>
                  <td className="w-[22%] px-3 py-3"><UsageBar value={d.last_cpu_percent} warn={80} /></td>
                  <td className="w-[22%] px-3 py-3"><UsageBar value={d.last_memory_percent} warn={80} /></td>
                  <td className="w-[22%] px-5 py-3"><UsageBar value={d.last_disk_percent} warn={95} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

function UsageBar({ value, warn }: { value: number | null | undefined; warn: number }) {
  if (value === null || value === undefined) return <span className="text-xs text-ink-faint">—</span>;
  const v = Math.round(value);
  const tone = v >= warn ? "bg-danger" : v >= warn - 20 ? "bg-degraded" : "bg-accent";
  return (
    <div className="flex items-center gap-2">
      <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-surface-hover">
        <div className={cn("h-full rounded-full", tone)} style={{ width: `${Math.min(100, v)}%` }} />
      </div>
      <span className="w-9 text-right text-xs tabular text-ink-muted">{v}%</span>
    </div>
  );
}

// --- Side panels -----------------------------------------------------------------

const STATUS_META: { status: DeviceStatus; label: string; color: string }[] = [
  { status: "online", label: "Online", color: "rgb(var(--online))" },
  { status: "degraded", label: "Degraded", color: "rgb(var(--degraded))" },
  { status: "pending", label: "Pending", color: "rgb(var(--pending))" },
  { status: "offline", label: "Offline", color: "rgb(var(--offline))" },
];

function StatusBreakdown({ devices, loading }: { devices: Device[]; loading: boolean }) {
  const counts = STATUS_META.map((m) => ({ ...m, count: devices.filter((d) => d.status === m.status).length }));
  const total = counts.reduce((s, c) => s + c.count, 0);

  // A donut drawn from stroke-dasharray segments: no chart library for four
  // numbers.
  const r = 52;
  const circumference = 2 * Math.PI * r;
  let offset = 0;

  return (
    <Card>
      <h2 className="text-base font-semibold text-ink">Fleet status</h2>
      {loading ? (
        <Skeleton className="mx-auto mt-6 h-36 w-36 rounded-full" />
      ) : (
        <div className="mt-4 flex items-center gap-6">
          <svg viewBox="0 0 128 128" className="h-32 w-32 shrink-0 -rotate-90" role="img" aria-label="Devices by status">
            <circle cx="64" cy="64" r={r} fill="none" strokeWidth="14" className="stroke-surface-hover" />
            {total > 0 &&
              counts.map((c) => {
                if (!c.count) return null;
                const len = (c.count / total) * circumference;
                const seg = (
                  <circle
                    key={c.status}
                    cx="64"
                    cy="64"
                    r={r}
                    fill="none"
                    stroke={c.color}
                    strokeWidth="14"
                    strokeDasharray={`${Math.max(0, len - 2)} ${circumference}`}
                    strokeDashoffset={-offset}
                    strokeLinecap="butt"
                  />
                );
                offset += len;
                return seg;
              })}
            <text x="64" y="64" textAnchor="middle" dominantBaseline="central" className="rotate-90 fill-ink text-[22px] font-semibold" style={{ transformOrigin: "64px 64px" }}>
              {total}
            </text>
          </svg>
          <ul className="flex-1 space-y-2.5">
            {counts.map((c) => (
              <li key={c.status} className="flex items-center gap-2.5 text-sm">
                <span className="h-2.5 w-2.5 rounded-full" style={{ background: c.color }} />
                <span className="flex-1 text-ink-muted">{c.label}</span>
                <span className="font-semibold tabular text-ink">{c.count}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </Card>
  );
}

function TopClients({ devices, loading }: { devices: Device[]; loading: boolean }) {
  const rows = useMemo(() => {
    const by = new Map<string, { total: number; online: number }>();
    for (const d of devices) {
      const key = d.client_name || "Unassigned";
      const row = by.get(key) ?? { total: 0, online: 0 };
      row.total += 1;
      if (d.status === "online") row.online += 1;
      by.set(key, row);
    }
    return [...by.entries()].sort((a, b) => b[1].total - a[1].total).slice(0, 5);
  }, [devices]);
  const max = Math.max(1, ...rows.map(([, r]) => r.total));

  return (
    <Card>
      <h2 className="text-base font-semibold text-ink">Clients</h2>
      <p className="mt-0.5 text-xs text-ink-faint">Devices per customer</p>
      {loading ? (
        <div className="mt-4 space-y-3">{[0, 1, 2].map((i) => <Skeleton key={i} className="h-6 w-full" />)}</div>
      ) : (
        <ul className="mt-4 space-y-3.5">
          {rows.map(([client, r]) => (
            <li key={client}>
              <div className="flex items-center justify-between text-sm">
                <span className={cn("truncate font-medium", client === "Unassigned" ? "text-ink-faint" : "text-ink")}>{client}</span>
                <span className="text-xs tabular text-ink-muted">
                  {r.online}/{r.total} online
                </span>
              </div>
              <div className="mt-1.5 flex h-1.5 overflow-hidden rounded-full bg-surface-hover">
                <div className="h-full bg-online" style={{ width: `${(r.online / max) * 100}%` }} />
                <div className="h-full bg-offline/40" style={{ width: `${((r.total - r.online) / max) * 100}%` }} />
              </div>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

function VersionSpread({ devices, loading }: { devices: Device[]; loading: boolean }) {
  const agents = useMemo(() => tally(devices.map((d) => d.agent_version || "unknown")), [devices]);
  const oses = useMemo(() => tally(devices.map((d) => (d.os_version ? `${d.os_name || "macOS"} ${d.os_version}` : "unknown"))), [devices]);

  return (
    <Card>
      <h2 className="text-base font-semibold text-ink">Versions</h2>
      <p className="mt-0.5 text-xs text-ink-faint">What the fleet is running</p>
      {loading ? (
        <Skeleton className="mt-4 h-20 w-full" />
      ) : (
        <div className="mt-4 space-y-4">
          <VersionGroup title="Agent" rows={agents} />
          <VersionGroup title="Operating system" rows={oses} />
        </div>
      )}
    </Card>
  );
}

function VersionGroup({ title, rows }: { title: string; rows: [string, number][] }) {
  return (
    <div>
      <div className="mb-2 text-2xs font-semibold uppercase tracking-wider text-ink-faint">{title}</div>
      <div className="flex flex-wrap gap-1.5">
        {rows.slice(0, 6).map(([v, n], i) => (
          <span
            key={v}
            className={cn(
              "inline-flex items-center gap-1.5 rounded-lg border px-2 py-1 text-xs",
              i === 0 ? "border-accent/20 bg-accent-subtle text-accent dark:text-accent-soft" : "border-surface-border bg-surface text-ink-muted",
            )}
          >
            <span className="font-mono">{v}</span>
            <span className="font-semibold tabular">{n}</span>
          </span>
        ))}
      </div>
    </div>
  );
}

function tally(values: string[]): [string, number][] {
  const m = new Map<string, number>();
  for (const v of values) m.set(v, (m.get(v) ?? 0) + 1);
  return [...m.entries()].sort((a, b) => b[1] - a[1]);
}

function PanelHeader({ title, description, action }: { title: string; description?: string; action?: React.ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-4 px-5 pb-4 pt-5">
      <div>
        <h2 className="text-base font-semibold text-ink">{title}</h2>
        {description && <p className="mt-0.5 text-xs text-ink-faint">{description}</p>}
      </div>
      {action}
    </div>
  );
}

// --- Empty fleet -------------------------------------------------------------------

function GettingStarted() {
  const steps = [
    { title: "Create an enrollment token", body: "A single-use, expiring credential that lets a machine join your organization.", icon: "key" as IconName },
    { title: "Install the agent", body: "Run one command on the Mac. It enrolls itself and connects over an outbound-only link.", icon: "terminal" as IconName },
    { title: "Approve and manage", body: "The device shows up here within seconds. Approve it and you're in control.", icon: "check-circle" as IconName },
  ];
  return (
    <Card className="overflow-hidden" padded={false}>
      <div className="bg-gradient-to-br from-indigo-500/10 via-violet-500/5 to-transparent px-8 py-10">
        <span className="inline-flex items-center gap-1.5 rounded-full bg-accent-subtle px-3 py-1 text-xs font-semibold text-accent">
          <Icon name="sparkles" size={13} /> Getting started
        </span>
        <h2 className="mt-4 text-2xl font-semibold tracking-tight text-ink">Add your first device</h2>
        <p className="mt-2 max-w-xl text-sm text-ink-muted">
          EdgePilot needs no inbound network access at the customer site. Three steps and you&apos;ll see the machine here, live.
        </p>
        {can("enrollment_token:write") && (
          <Link href="/enrollment?new=1" className="mt-6 inline-block">
            <Button variant="primary" icon="plus">Create enrollment token</Button>
          </Link>
        )}
      </div>
      <ol className="grid divide-y divide-surface-border border-t border-surface-border md:grid-cols-3 md:divide-x md:divide-y-0">
        {steps.map((s, i) => (
          <li key={s.title} className="p-6">
            <div className="flex items-center gap-3">
              <span className="flex h-8 w-8 items-center justify-center rounded-full bg-surface-hover text-sm font-semibold text-ink">{i + 1}</span>
              <Icon name={s.icon} className="text-ink-faint" />
            </div>
            <h3 className="mt-4 text-sm font-semibold text-ink">{s.title}</h3>
            <p className="mt-1 text-sm text-ink-muted">{s.body}</p>
          </li>
        ))}
      </ol>
    </Card>
  );
}
