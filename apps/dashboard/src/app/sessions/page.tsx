"use client";

import Link from "next/link";
import { useCallback, useEffect, useMemo, useState } from "react";
import { AppShell } from "@/components/app-shell";
import { SessionPlayer } from "@/components/session-player";
import {
  Avatar, Badge, Button, Card, ConfirmDialog, EmptyState, ErrorNote, Icon, IconButton, PageHeader, Pagination,
  SearchInput, Segmented, Table, TableSkeleton, TBody, Td, Th, THead, Tr, useToast, usePagination, type IconName,
} from "@/components/ui";
import { can } from "@/lib/api/client";
import { listUsers, type OrgUser } from "@/lib/api/admin";
import {
  listDevices, listDeviceSSHSessions, listSSHSessions, terminateSSHSession, type Device, type SSHSession,
} from "@/lib/api/devices";
import { downloadCSV, stamp } from "@/lib/csv";
import { absoluteTime, bytes, relativeTime } from "@/lib/format";
import { cn } from "@/lib/cn";

export default function SessionsPage() {
  return (
    <AppShell>
      {can("ssh:read") ? (
        <SessionsView />
      ) : (
        <EmptyState icon="lock" title="You don't have access to sessions" description="Ask an administrator for the ssh:read permission." />
      )}
    </AppShell>
  );
}

type Row = SSHSession & { device?: Device; user?: OrgUser };
type Scope = "all" | "active" | "closed" | "problem";

const STATUS: Record<SSHSession["status"], { label: string; tone: "success" | "neutral" | "danger" | "warning" }> = {
  active: { label: "Live", tone: "success" },
  closed: { label: "Closed", tone: "neutral" },
  denied: { label: "Denied", tone: "danger" },
  failed: { label: "Failed", tone: "warning" },
};

/** Run `fn` over `items` with at most `limit` in flight. */
async function pool<T, R>(items: T[], limit: number, fn: (t: T) => Promise<R>): Promise<R[]> {
  const out: R[] = [];
  let i = 0;
  await Promise.all(
    Array.from({ length: Math.min(limit, items.length) }, async () => {
      while (i < items.length) {
        const idx = i++;
        out[idx] = await fn(items[idx]!);
      }
    }),
  );
  return out;
}

function duration(s: SSHSession, now: number): string {
  const end = s.ended_at ? new Date(s.ended_at).getTime() : now;
  const secs = Math.max(0, Math.round((end - new Date(s.started_at).getTime()) / 1000));
  if (secs < 60) return `${secs}s`;
  const m = Math.floor(secs / 60);
  if (m < 60) return `${m}m ${secs % 60}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

/**
 * Who has been on which machine, across the whole fleet.
 *
 * The server keeps a fleet-wide list of *live* sessions and a per-device
 * history, so this page asks for both and merges them. Sessions opened in the
 * browser terminal are recorded and can be replayed here; `ssh -J` sessions are
 * end-to-end encrypted through the bastion and carry metadata only.
 */
function SessionsView() {
  const { push } = useToast();
  const [rows, setRows] = useState<Row[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>(null);
  const [scope, setScope] = useState<Scope>("all");
  const [q, setQ] = useState("");
  const [replay, setReplay] = useState<Row | null>(null);
  const [ending, setEnding] = useState<Row | null>(null);
  const [busy, setBusy] = useState(false);
  const [now, setNow] = useState(() => Date.now());

  const load = useCallback(async () => {
    try {
      const [devicePage, active, users] = await Promise.all([
        listDevices({ limit: 200 }),
        listSSHSessions(),
        can("user:read") ? listUsers().then((p) => p.data).catch(() => [] as OrgUser[]) : Promise.resolve([] as OrgUser[]),
      ]);
      const devices = devicePage.data;
      const histories = await pool(devices, 6, (d) =>
        listDeviceSSHSessions(d.id).then((p) => p.data).catch(() => [] as SSHSession[]),
      );
      const byId = new Map<string, SSHSession>();
      for (const s of [...histories.flat(), ...active.data]) byId.set(s.id, s);
      const deviceById = new Map(devices.map((d) => [d.id, d]));
      const userById = new Map(users.map((u) => [u.id, u]));
      setRows(
        [...byId.values()]
          .map((s) => ({ ...s, device: deviceById.get(s.device_id), user: s.user_id ? userById.get(s.user_id) : undefined }))
          .sort((a, b) => new Date(b.started_at).getTime() - new Date(a.started_at).getTime()),
      );
      setError(null);
    } catch (err) {
      setError(err);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
    const refresh = setInterval(() => void load(), 30_000);
    const tick = setInterval(() => setNow(Date.now()), 1_000);
    return () => {
      clearInterval(refresh);
      clearInterval(tick);
    };
  }, [load]);

  const dayAgo = now - 24 * 3600_000;
  const weekAgo = now - 7 * 24 * 3600_000;
  const live = rows.filter((r) => r.status === "active");
  const stats: { label: string; value: number; icon: IconName; tone: string }[] = [
    { label: "Live now", value: live.length, icon: "activity", tone: "bg-emerald-500/10 text-emerald-600 dark:text-emerald-300" },
    { label: "Last 24 hours", value: rows.filter((r) => new Date(r.started_at).getTime() >= dayAgo).length, icon: "clock", tone: "bg-indigo-500/10 text-indigo-600 dark:text-indigo-300" },
    { label: "Last 7 days", value: rows.filter((r) => new Date(r.started_at).getTime() >= weekAgo).length, icon: "calendar", tone: "bg-sky-500/10 text-sky-600 dark:text-sky-300" },
    { label: "Denied or failed", value: rows.filter((r) => r.status === "denied" || r.status === "failed").length, icon: "shield", tone: "bg-amber-500/10 text-amber-600 dark:text-amber-300" },
  ];

  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return rows.filter((r) => {
      if (scope === "active" && r.status !== "active") return false;
      if (scope === "closed" && r.status !== "closed") return false;
      if (scope === "problem" && r.status !== "denied" && r.status !== "failed") return false;
      if (!needle) return true;
      return [r.device?.hostname, r.device?.client_name, r.user?.email, r.user?.full_name, r.target_username, r.client_ip]
        .some((v) => v?.toLowerCase().includes(needle));
    });
  }, [rows, scope, q]);
  const paging = usePagination(shown, 25);

  function operator(r: Row) {
    return r.user?.full_name || r.user?.email || (r.user_id ? `${r.user_id.slice(0, 8)}…` : "Unknown");
  }

  return (
    <>
      <PageHeader
        title="Sessions"
        description="Every remote session across the fleet: who connected, to which device and account, from where, and for how long. Browser-terminal sessions can be replayed."
        actions={
          <>
            <IconButton icon="refresh" label="Refresh" variant="secondary" onClick={() => void load()} />
            <Button
              icon="download"
              disabled={shown.length === 0}
              onClick={() =>
                downloadCSV(
                  `edgepilot-sessions-${stamp()}.csv`,
                  ["Started", "Ended", "Status", "Device", "Client", "Operator", "Account", "From IP", "Bytes in", "Bytes out", "Close reason"],
                  shown.map((r) => [
                    r.started_at, r.ended_at ?? "", r.status, r.device?.hostname ?? r.device_id, r.device?.client_name,
                    r.user?.email ?? r.user_id, r.target_username, r.client_ip, r.bytes_in, r.bytes_out, r.close_reason,
                  ]),
                )
              }
            >
              Export
            </Button>
          </>
        }
      />

      <div className="mb-6 grid grid-cols-2 gap-3 sm:gap-4 xl:grid-cols-4">
        {stats.map((s) => (
          <Card key={s.label} padded={false}>
            <div className="flex items-center gap-4 p-4 sm:p-5">
              <span className={cn("hidden h-10 w-10 shrink-0 items-center justify-center rounded-xl sm:flex", s.tone)}>
                <Icon name={s.icon} size={18} />
              </span>
              <div>
                <div className="text-xs font-medium text-ink-muted">{s.label}</div>
                <div className="mt-0.5 text-2xl font-semibold tabular tracking-tight text-ink">{loading ? "—" : s.value}</div>
              </div>
            </div>
          </Card>
        ))}
      </div>

      {live.length > 0 && (
        <Card padded={false} className="mb-6 overflow-hidden border-online/30">
          <div className="flex items-center gap-2 border-b border-surface-border bg-online/5 px-5 py-3">
            <span className="relative flex h-2 w-2">
              <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-online opacity-60" />
              <span className="relative inline-flex h-2 w-2 rounded-full bg-online" />
            </span>
            <h2 className="text-sm font-semibold text-ink">Live now</h2>
          </div>
          <ul className="divide-y divide-surface-border">
            {live.map((r) => (
              <li key={r.id} className="flex flex-wrap items-center gap-4 px-5 py-3">
                <Avatar name={r.user?.email ?? r.user_id ?? "?"} />
                <div className="min-w-0 flex-1">
                  <div className="text-sm text-ink">
                    <span className="font-semibold">{operator(r)}</span>
                    <span className="text-ink-muted"> as </span>
                    <span className="font-mono text-xs">{r.target_username || "—"}</span>
                    <span className="text-ink-muted"> on </span>
                    {r.device ? (
                      <Link href={`/devices/${r.device_id}`} className="font-semibold hover:text-accent">{r.device.hostname}</Link>
                    ) : (
                      <span className="font-mono text-xs">{r.device_id.slice(0, 8)}</span>
                    )}
                  </div>
                  <div className="text-xs text-ink-faint">
                    from {r.client_ip || "unknown"} · {duration(r, now)} · {bytes(r.bytes_in + r.bytes_out)} transferred
                  </div>
                </div>
                {can("ssh:connect") && (
                  <Button size="sm" variant="danger" icon="x-circle" onClick={() => setEnding(r)}>End session</Button>
                )}
              </li>
            ))}
          </ul>
        </Card>
      )}

      <Card padded={false} className="overflow-hidden">
        <div className="flex flex-wrap items-center gap-3 border-b border-surface-border px-4 py-4 sm:px-5">
          <Segmented<Scope>
            aria-label="Session status"
            value={scope}
            onChange={(s) => {
              setScope(s);
              paging.reset();
            }}
            options={[
              { value: "all", label: "All", count: rows.length },
              { value: "active", label: "Live", count: live.length },
              { value: "closed", label: "Closed" },
              { value: "problem", label: "Denied & failed" },
            ]}
          />
          <SearchInput
            className="ml-auto w-full sm:w-72"
            placeholder="Search device, operator, account, IP…"
            value={q}
            onChange={(e) => {
              setQ(e.target.value);
              paging.reset();
            }}
            aria-label="Search sessions"
          />
        </div>
        <ErrorNote error={error} onRetry={() => void load()} />
        {loading ? (
          <TableSkeleton rows={6} columns={6} />
        ) : shown.length === 0 ? (
          <EmptyState
            icon="terminal"
            title={rows.length === 0 ? "No sessions yet" : "No sessions match"}
            description={rows.length === 0 ? "Sessions appear here as soon as anyone opens a terminal or SSH connection to a device." : "Try a different search or status."}
          />
        ) : (
          <>
            <Table>
              <THead>
                <tr>
                  <Th>Started</Th>
                  <Th>Device</Th>
                  <Th className="hidden md:table-cell">Operator</Th>
                  <Th className="hidden lg:table-cell">Account</Th>
                  <Th className="hidden lg:table-cell">From</Th>
                  <Th>Duration</Th>
                  <Th className="hidden sm:table-cell" align="right">Data</Th>
                  <Th>Status</Th>
                  <Th align="right"><span className="sr-only">Actions</span></Th>
                </tr>
              </THead>
              <TBody>
                {paging.rows.map((r) => (
                  <Tr key={r.id}>
                    <Td className="whitespace-nowrap" title={absoluteTime(r.started_at)}>
                      <div className="text-ink">{relativeTime(r.started_at)}</div>
                      <div className="text-xs text-ink-faint">{new Date(r.started_at).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}</div>
                    </Td>
                    <Td>
                      {r.device ? (
                        <Link href={`/devices/${r.device_id}`} className="font-semibold normal-nums text-ink hover:text-accent">{r.device.hostname}</Link>
                      ) : (
                        <span className="font-mono text-xs text-ink-faint">{r.device_id.slice(0, 8)}…</span>
                      )}
                      {r.device?.client_name && <div className="text-xs text-ink-faint">{r.device.client_name}</div>}
                    </Td>
                    <Td className="hidden md:table-cell">
                      <div className="flex items-center gap-2">
                        <Avatar name={r.user?.email ?? r.user_id ?? "?"} size="sm" />
                        <span className="truncate text-ink">{operator(r)}</span>
                      </div>
                    </Td>
                    <Td className="hidden font-mono text-xs text-ink-muted lg:table-cell">{r.target_username || "—"}</Td>
                    <Td className="hidden font-mono text-xs text-ink-muted lg:table-cell">{r.client_ip || "—"}</Td>
                    <Td className="whitespace-nowrap tabular text-ink-muted">{duration(r, now)}</Td>
                    <Td className="hidden whitespace-nowrap tabular text-ink-muted sm:table-cell" align="right">{bytes(r.bytes_in + r.bytes_out)}</Td>
                    <Td>
                      <Badge tone={STATUS[r.status].tone} dot={r.status === "active"} title={r.close_reason || undefined}>
                        {STATUS[r.status].label}
                      </Badge>
                    </Td>
                    <Td align="right">
                      <div className="flex justify-end gap-1">
                        {r.status === "active" && can("ssh:connect") ? (
                          <Button size="sm" variant="danger" onClick={() => setEnding(r)}>End</Button>
                        ) : r.status === "closed" ? (
                          <Button size="sm" icon="play" onClick={() => setReplay(r)}>Replay</Button>
                        ) : null}
                      </div>
                    </Td>
                  </Tr>
                ))}
              </TBody>
            </Table>
            <Pagination page={paging.page} pages={paging.pages} pageSize={paging.pageSize} total={paging.total} onPage={paging.setPage} onPageSize={paging.setPageSize} />
          </>
        )}
      </Card>

      <SessionPlayer
        sessionId={replay?.id ?? null}
        title={replay ? `Replay · ${replay.device?.hostname ?? "device"}` : ""}
        subtitle={replay ? `${operator(replay)} as ${replay.target_username || "unknown"} · ${absoluteTime(replay.started_at)} · ${duration(replay, now)}` : undefined}
        onClose={() => setReplay(null)}
      />

      <ConfirmDialog
        open={ending !== null}
        onClose={() => setEnding(null)}
        title="End this session?"
        confirmLabel="End session"
        loading={busy}
        description={
          <>
            {ending && operator(ending)} is disconnected from <strong className="text-ink">{ending?.device?.hostname ?? "the device"}</strong> immediately.
            Anything running in their shell is interrupted.
          </>
        }
        onConfirm={async () => {
          if (!ending) return;
          setBusy(true);
          try {
            await terminateSSHSession(ending.id);
            push({ title: "Session ended", tone: "success" });
            setEnding(null);
            await load();
          } catch (err) {
            push({ title: "Could not end the session", description: err instanceof Error ? err.message : undefined, tone: "error" });
          } finally {
            setBusy(false);
          }
        }}
      />
    </>
  );
}
