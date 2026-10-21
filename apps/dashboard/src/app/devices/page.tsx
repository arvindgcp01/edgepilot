"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useMemo, useRef, useState } from "react";
import { AppShell } from "@/components/app-shell";
import {
  Badge, Button, Card, EmptyState, ErrorNote, Icon, IconButton, Input, Menu, PageHeader, Pagination,
  SearchInput, Segmented, SortableTh, StatusBadge, Table, TableSkeleton, TBody, Td, Th,
  THead, Tr, useToast, usePagination, useSorted, type SortState,
} from "@/components/ui";
import { useFleet } from "@/hooks/use-fleet";
import { relativeTime } from "@/lib/format";
import { can } from "@/lib/api/client";
import { DeviceTerminal } from "@/components/terminal";
import { FleetGrid } from "@/components/fleet-grid";
import {
  terminateSSHSession, updateDevice,
  type Device, type DeviceStatus, type SSHSession,
} from "@/lib/api/devices";
import { cn } from "@/lib/cn";

type Filter = "all" | DeviceStatus;
type View = "table" | "grid";
type SortKey = "hostname" | "client" | "status" | "cpu" | "mem" | "disk" | "model" | "os" | "memory" | "agent" | "seen";
const VIEW_KEY = "edgepilot.fleet.view";

const STATUS_ORDER: Record<DeviceStatus, number> = { online: 0, degraded: 1, pending: 2, offline: 3, decommissioned: 4 };

export default function FleetPage() {
  return (
    <AppShell>
      <FleetView />
    </AppShell>
  );
}

function FleetView() {
  const router = useRouter();
  const [search, setSearch] = useState("");
  const [filter, setFilter] = useState<Filter>("all");
  const [sort, setSort] = useState<SortState<SortKey>>({ key: "status", dir: "asc" });
  const [view, setView] = useState<View>("table");
  useEffect(() => {
    try {
      const saved = window.localStorage.getItem(VIEW_KEY);
      if (saved === "grid" || saved === "table") setView(saved);
    } catch {
      // Storage unavailable: the table is fine.
    }
  }, []);
  function chooseView(next: View) {
    setView(next);
    try { window.localStorage.setItem(VIEW_KEY, next); } catch { /* ignore */ }
  }
  /** The device whose shell is open, if any. One at a time: a terminal wants
   *  the whole width, and tabs would be a feature of their own. */
  const [terminal, setTerminal] = useState<{ id: string; hostname: string } | null>(null);
  const searchRef = useRef<HTMLInputElement>(null);

  const { devices, summary, sshByDevice, loading, error, live, reload, applyDevice } = useFleet({
    q: search || undefined,
    status: filter === "all" ? undefined : [filter],
    limit: 100,
  });

  // "/" focuses search from anywhere on the page, the way it does on GitHub.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement | null;
      if (e.key === "/" && !["INPUT", "TEXTAREA", "SELECT"].includes(target?.tagName ?? "")) {
        e.preventDefault();
        searchRef.current?.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  const accessors = useMemo(
    () => ({
      hostname: (d: Device) => d.hostname || d.serial_number,
      client: (d: Device) => d.client_name || null,
      status: (d: Device) => STATUS_ORDER[d.status] ?? 9,
      cpu: (d: Device) => d.last_cpu_percent,
      mem: (d: Device) => d.last_memory_percent,
      disk: (d: Device) => d.last_disk_percent,
      model: (d: Device) => d.model_name || d.model_identifier || null,
      os: (d: Device) => d.os_version || null,
      memory: (d: Device) => d.memory_bytes ?? null,
      agent: (d: Device) => d.agent_version || null,
      seen: (d: Device) => (d.last_seen_at ? new Date(d.last_seen_at).getTime() : null),
    }),
    [],
  );
  const sorted = useSorted(devices, sort, accessors);
  const paging = usePagination(sorted, 25);

  const canConnect = can("ssh:connect");

  return (
    <>
      <PageHeader
        title="Devices"
        description="Every machine enrolled in your organization. Click a row for details, or connect straight from the list."
        meta={
          <span
            className={cn(
              "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium",
              live ? "border-online/20 bg-online/10 text-online" : "border-surface-border text-ink-faint",
            )}
            title={live ? "Receiving live updates" : "Reconnecting to the event stream"}
          >
            <span className={cn("h-1.5 w-1.5 rounded-full bg-current", live && "animate-pulse")} aria-hidden />
            {live ? "Live" : "Reconnecting"}
          </span>
        }
        actions={
          <>
            <IconButton icon="refresh" label="Refresh" variant="secondary" onClick={() => void reload()} />
            {can("enrollment_token:write") && (
              <Button variant="primary" icon="plus" onClick={() => router.push("/enrollment?new=1")}>
                Enroll device
              </Button>
            )}
          </>
        }
      />

      {terminal && (
        <Card className="mb-6 h-[28rem] overflow-hidden" padded={false}>
          <DeviceTerminal deviceId={terminal.id} hostname={terminal.hostname} onClose={() => setTerminal(null)} />
        </Card>
      )}

      <Card padded={false} className="overflow-hidden">
        <div className="flex flex-wrap items-center gap-3 border-b border-surface-border px-4 py-4 sm:px-5">
          <div className="-mx-1 max-w-full overflow-x-auto px-1">
          <Segmented<Filter>
            aria-label="Filter by status"
            value={filter}
            onChange={(next) => {
              setFilter(next);
              paging.reset();
            }}
            options={[
              { value: "all", label: "All", count: summary?.total },
              { value: "online", label: "Online", count: summary?.online },
              { value: "offline", label: "Offline", count: summary?.offline },
              { value: "degraded", label: "Degraded", count: summary?.degraded },
              { value: "pending", label: "Pending", count: summary?.pending_approval },
            ]}
          />
          </div>
          <div className="ml-auto flex w-full items-center gap-2 sm:w-auto">
            <SearchInput
              ref={searchRef}
              className="w-full sm:w-72"
              placeholder="Search devices…"
              value={search}
              onChange={(e) => {
                setSearch(e.target.value);
                paging.reset();
              }}
              aria-label="Search devices"
            />
            <Segmented<View>
              aria-label="View"
              value={view}
              onChange={chooseView}
              options={[
                { value: "table", label: <Icon name="menu" size={15} aria-label="Table view" /> },
                { value: "grid", label: <Icon name="grid" size={15} aria-label="Tile view" /> },
              ]}
            />
          </div>
        </div>

        <ErrorNote error={error} onRetry={() => void reload()} />

        {loading && devices.length === 0 ? (
          <TableSkeleton rows={8} columns={6} />
        ) : devices.length === 0 ? (
          <EmptyState
            icon="monitor"
            title={search || filter !== "all" ? "No devices match" : "No devices yet"}
            description={
              search || filter !== "all"
                ? "Try a different search or clear the status filter."
                : "Install the agent with an enrollment token and the device appears here within seconds."
            }
            action={
              search || filter !== "all" ? (
                <Button onClick={() => { setSearch(""); setFilter("all"); }}>Clear filters</Button>
              ) : can("enrollment_token:write") ? (
                <Button variant="primary" icon="plus" onClick={() => router.push("/enrollment?new=1")}>Enroll your first device</Button>
              ) : undefined
            }
          />
        ) : view === "grid" ? (
          <>
            <FleetGrid
              devices={paging.rows}
              onConnect={(d) => setTerminal({ id: d.id, hostname: d.hostname || d.serial_number })}
            />
            <Pagination
              page={paging.page}
              pages={paging.pages}
              pageSize={paging.pageSize}
              total={paging.total}
              onPage={paging.setPage}
              onPageSize={paging.setPageSize}
            />
          </>
        ) : (
          <>
            <Table>
              <THead>
                <tr>
                  <SortableTh sortKey="hostname" sort={sort} onSort={setSort}>Device</SortableTh>
                  <SortableTh sortKey="client" sort={sort} onSort={setSort} className="hidden sm:table-cell">Client</SortableTh>
                  <SortableTh sortKey="status" sort={sort} onSort={setSort}>Status</SortableTh>
                  <SortableTh sortKey="cpu" sort={sort} onSort={setSort} className="hidden md:table-cell">Health</SortableTh>
                  <SortableTh sortKey="os" sort={sort} onSort={setSort} className="hidden xl:table-cell">OS</SortableTh>
                  <SortableTh sortKey="seen" sort={sort} onSort={setSort} className="hidden sm:table-cell">Last seen</SortableTh>
                  <Th align="right"><span className="sr-only">Actions</span></Th>
                </tr>
              </THead>
              <TBody>
                {paging.rows.map((device) => (
                  <Tr key={device.id} interactive onClick={() => router.push(`/devices/${device.id}`)}>
                    <Td className="min-w-[12rem]">
                      <div className="flex items-center gap-3">
                        <DeviceGlyph device={device} />
                        <div className="min-w-0">
                          <Link
                            href={`/devices/${device.id}`}
                            onClick={(e) => e.stopPropagation()}
                            className="block truncate font-semibold normal-nums text-ink hover:text-accent"
                          >
                            {device.hostname || "(unnamed)"}
                          </Link>
                          <div className="truncate text-xs text-ink-faint" title={`Serial ${device.serial_number} · agent ${device.agent_version || "unknown"}`}>
                            {[device.model_name || device.model_identifier, device.chip].filter(Boolean).join(" · ") || device.serial_number}
                          </div>
                        </div>
                      </div>
                    </Td>
                    <Td className="hidden sm:table-cell" onClick={(e) => e.stopPropagation()}>
                      <ClientNameCell device={device} onSaved={applyDevice} />
                    </Td>
                    <Td>
                      <div className="flex flex-col items-start gap-1.5">
                        <div className="flex flex-wrap items-center gap-1.5">
                          <StatusBadge status={device.status} />
                          {!device.approved && device.status !== "pending" && <Badge tone="info">Needs approval</Badge>}
                        </div>
                        <div onClick={(e) => e.stopPropagation()}>
                          <SSHCell sessions={sshByDevice[device.id]} onChange={reload} />
                        </div>
                      </div>
                    </Td>
                    <Td className="hidden w-48 md:table-cell">
                      <HealthCell device={device} />
                    </Td>
                    <Td className="hidden whitespace-nowrap text-ink-muted xl:table-cell">
                      {device.os_version ? `${device.os_name || "macOS"} ${device.os_version}` : "—"}
                    </Td>
                    <Td className="hidden whitespace-nowrap text-ink-muted sm:table-cell" title={device.last_seen_at}>
                      {relativeTime(device.last_seen_at)}
                    </Td>
                    <Td align="right" onClick={(e) => e.stopPropagation()}>
                      <div className="flex items-center justify-end gap-1">
                        {canConnect && device.status === "online" && device.approved && (
                          <Button
                            size="sm"
                            icon="terminal"
                            onClick={() => setTerminal({ id: device.id, hostname: device.hostname || device.serial_number })}
                          >
                            Connect
                          </Button>
                        )}
                        <RowMenu device={device} onConnect={canConnect ? () => setTerminal({ id: device.id, hostname: device.hostname || device.serial_number }) : undefined} />
                      </div>
                    </Td>
                  </Tr>
                ))}
              </TBody>
            </Table>
            <Pagination
              page={paging.page}
              pages={paging.pages}
              pageSize={paging.pageSize}
              total={paging.total}
              onPage={paging.setPage}
              onPageSize={paging.setPageSize}
            />
          </>
        )}
      </Card>
    </>
  );
}

/** A small tile that tells a Mac Studio from a Mac mini at a glance, tinted by
 *  status so a column of them reads like a heat strip. */
function DeviceGlyph({ device }: { device: Device }) {
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
function HealthCell({ device }: { device: Device }) {
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

function RowMenu({ device, onConnect }: { device: Device; onConnect?: () => void }) {
  const router = useRouter();
  const { push } = useToast();
  return (
    <Menu
      trigger={({ toggle, open }) => (
        <IconButton icon="more" label={`Actions for ${device.hostname || device.serial_number}`} size="sm" onClick={toggle} aria-expanded={open} />
      )}
      items={[
        { label: "View details", icon: "eye", onSelect: () => router.push(`/devices/${device.id}`) },
        ...(onConnect && device.status === "online" && device.approved
          ? [{ label: "Open terminal", icon: "terminal" as const, onSelect: onConnect }]
          : []),
        {
          label: "Copy serial number",
          icon: "copy",
          onSelect: () => {
            void navigator.clipboard?.writeText(device.serial_number);
            push({ title: "Serial number copied", description: device.serial_number, tone: "success" });
          },
        },
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
function ClientNameCell({ device, onSaved }: { device: Device; onSaved: (device: Device) => void }) {
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
function SSHCell({ sessions, onChange }: { sessions?: SSHSession[]; onChange: () => void }) {
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
