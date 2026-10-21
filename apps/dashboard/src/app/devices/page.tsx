"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { AppShell } from "@/components/app-shell";
import {
  Badge, Button, Card, EmptyState, ErrorNote, Field, Icon, IconButton, Input, Kbd, Modal, PageHeader,
  Pagination, Popover, SearchInput, Segmented, SelectionCheckbox, SortableTh, StatusBadge, Table, TableSkeleton,
  TBody, Td, Th, THead, Tr, useToast, usePagination, useSelection, useSorted, type IconName, type SortState,
} from "@/components/ui";
import { ClientNameCell, DeviceGlyph, HealthCell, RowMenu, SSHCell } from "@/components/device-cells";
import { DeviceDrawer } from "@/components/device-drawer";
import { DeviceTerminal } from "@/components/terminal";
import { FleetGrid } from "@/components/fleet-grid";
import { useFleet } from "@/hooks/use-fleet";
import { can } from "@/lib/api/client";
import { approveDevice, updateDevice, type Device, type DeviceStatus } from "@/lib/api/devices";
import { downloadCSV, stamp } from "@/lib/csv";
import { relativeTime } from "@/lib/format";
import { useStored, usePinned } from "@/lib/local-store";
import { cn } from "@/lib/cn";

// --- Model ---------------------------------------------------------------------

type Layout = "table" | "grid";
type SortKey = "hostname" | "client" | "status" | "cpu" | "site" | "os" | "agent" | "seen";
type Facet = "status" | "client" | "site" | "model" | "os" | "agent";
type Preset = "attention" | "pressure" | "approval" | "outdated";

/** Everything that decides which devices are shown. A saved view is one of these. */
type ViewState = { q: string; preset?: Preset; facets: Partial<Record<Facet, string[]>> };

type SavedView = { id: string; name: string; state: ViewState };

type Column = "client" | "status" | "health" | "site" | "os" | "agent" | "seen";

const COLUMNS: { key: Column; label: string }[] = [
  { key: "client", label: "Client" },
  { key: "status", label: "Status" },
  { key: "health", label: "Health" },
  { key: "site", label: "Site" },
  { key: "os", label: "OS" },
  { key: "agent", label: "Agent" },
  { key: "seen", label: "Last seen" },
];
const DEFAULT_COLUMNS: Column[] = ["client", "status", "health", "os", "seen"];

const FACETS: { key: Facet; label: string; icon: IconName }[] = [
  { key: "status", label: "Status", icon: "circle-dot" },
  { key: "client", label: "Client", icon: "building" },
  { key: "site", label: "Site", icon: "map-pin" },
  { key: "model", label: "Model", icon: "monitor" },
  { key: "os", label: "OS", icon: "layers" },
  { key: "agent", label: "Agent", icon: "package" },
];

const STATUS_ORDER: Record<DeviceStatus, number> = { online: 0, degraded: 1, pending: 2, offline: 3, decommissioned: 4 };

const isHot = (d: Device) =>
  (d.last_cpu_percent ?? 0) >= 80 || (d.last_memory_percent ?? 0) >= 80 || (d.last_disk_percent ?? 0) >= 95;

/** Numeric-aware comparison so 2.10.0 sorts after 2.9.0. */
const versionCompare = (a: string, b: string) => a.localeCompare(b, undefined, { numeric: true });

function facetValue(d: Device, facet: Facet): string {
  switch (facet) {
    case "status": return d.status;
    case "client": return d.client_name || "Unassigned";
    case "site": return d.site || "No site";
    case "model": return d.model_name || d.model_identifier || "Unknown";
    case "os": return d.os_version ? `${d.os_name || "macOS"} ${d.os_version}` : "Unknown";
    case "agent": return d.agent_version || "Unknown";
  }
}

const BUILT_IN: { id: string; name: string; icon: IconName; state: ViewState; hint: string }[] = [
  { id: "all", name: "All devices", icon: "monitor", state: { q: "", facets: {} }, hint: "Everything enrolled" },
  { id: "attention", name: "Needs attention", icon: "warning", state: { q: "", preset: "attention", facets: {} }, hint: "Offline, degraded, unapproved, or under pressure" },
  { id: "approval", name: "Awaiting approval", icon: "shield", state: { q: "", preset: "approval", facets: {} }, hint: "Enrolled but not yet approved" },
  { id: "pressure", name: "Under pressure", icon: "trending-up", state: { q: "", preset: "pressure", facets: {} }, hint: "CPU or memory ≥ 80%, or disk ≥ 95%" },
  { id: "outdated", name: "Outdated agent", icon: "package", state: { q: "", preset: "outdated", facets: {} }, hint: "Not running the newest agent in the fleet" },
];

function normalize(s: ViewState): ViewState {
  const facets: ViewState["facets"] = {};
  for (const f of FACETS) {
    const v = s.facets[f.key];
    if (v && v.length) facets[f.key] = [...v].sort();
  }
  return { q: s.q.trim(), preset: s.preset, facets };
}
const sameState = (a: ViewState, b: ViewState) => JSON.stringify(normalize(a)) === JSON.stringify(normalize(b));
const pct = (v: number | null | undefined) => (v === null || v === undefined ? "" : Math.round(v));

// --- Page ----------------------------------------------------------------------

export default function DevicesPage() {
  return (
    <AppShell>
      <DevicesView />
    </AppShell>
  );
}

function DevicesView() {
  const router = useRouter();
  const { push } = useToast();
  const { toggle: togglePin } = usePinned();

  // Everything is fetched once and filtered here: facet counts, instant search,
  // and saved views all need the whole list. The server caps a page at its
  // maximum, which is far above the size of any fleet on this deployment.
  const { devices, sshByDevice, loading, error, live, reload, applyDevice } = useFleet({ limit: 200 });

  const [state, setState] = useState<ViewState>({ q: "", facets: {} });
  const [activeView, setActiveView] = useState("all");
  const [saved, setSaved] = useStored<SavedView[]>("edgepilot.devices.views", []);
  const [columns, setColumns] = useStored<Column[]>("edgepilot.devices.columns", DEFAULT_COLUMNS);
  const [layout, setLayout] = useStored<Layout>("edgepilot.fleet.view", "table");
  const [sort, setSort] = useState<SortState<SortKey>>({ key: "status", dir: "asc" });
  const [terminal, setTerminal] = useState<{ id: string; hostname: string } | null>(null);
  const [peekId, setPeekId] = useState<string | null>(null);
  const [cursor, setCursor] = useState(0);
  const [savingView, setSavingView] = useState(false);
  const [bulkClient, setBulkClient] = useState(false);
  const searchRef = useRef<HTMLInputElement>(null);

  const latestAgent = useMemo(
    () => devices.map((d) => d.agent_version).filter((v): v is string => Boolean(v)).sort((a, b) => versionCompare(b, a))[0],
    [devices],
  );

  const matchesPreset = useCallback(
    (d: Device, preset?: Preset) => {
      switch (preset) {
        case "attention": return d.status === "offline" || d.status === "degraded" || !d.approved || isHot(d);
        case "pressure": return d.status !== "offline" && isHot(d);
        case "approval": return !d.approved && d.status !== "decommissioned";
        case "outdated": return Boolean(latestAgent) && d.agent_version !== latestAgent;
        default: return true;
      }
    },
    [latestAgent],
  );

  const filtered = useMemo(() => {
    const q = state.q.trim().toLowerCase();
    return devices.filter((d) => {
      if (!matchesPreset(d, state.preset)) return false;
      for (const f of FACETS) {
        const want = state.facets[f.key];
        if (want?.length && !want.includes(facetValue(d, f.key))) return false;
      }
      if (!q) return true;
      return [d.hostname, d.client_name, d.serial_number, d.site, d.model_name, d.local_ip, d.notes]
        .some((v) => v?.toLowerCase().includes(q));
    });
  }, [devices, state, matchesPreset]);

  const accessors = useMemo(
    () => ({
      hostname: (d: Device) => d.hostname || d.serial_number,
      client: (d: Device) => d.client_name || null,
      status: (d: Device) => STATUS_ORDER[d.status] ?? 9,
      cpu: (d: Device) => d.last_cpu_percent,
      site: (d: Device) => d.site || null,
      os: (d: Device) => d.os_version || null,
      agent: (d: Device) => d.agent_version || null,
      seen: (d: Device) => (d.last_seen_at ? new Date(d.last_seen_at).getTime() : null),
    }),
    [],
  );
  const sorted = useSorted(filtered, sort, accessors);
  const paging = usePagination(sorted, 25);
  const selection = useSelection(paging.rows);
  const selectedDevices = useMemo(() => devices.filter((d) => selection.isSelected(d.id)), [devices, selection]);

  // Views ------------------------------------------------------------------------

  const viewCounts = useMemo(() => {
    const out: Record<string, number> = {};
    for (const v of BUILT_IN) out[v.id] = devices.filter((d) => matchesPreset(d, v.state.preset)).length;
    return out;
  }, [devices, matchesPreset]);

  const currentBase =
    BUILT_IN.find((v) => v.id === activeView)?.state ?? saved.find((v) => v.id === activeView)?.state ?? BUILT_IN[0]!.state;
  const dirty = !sameState(state, currentBase);

  function applyView(id: string) {
    const view = BUILT_IN.find((v) => v.id === id) ?? saved.find((v) => v.id === id);
    if (!view) return;
    setActiveView(id);
    setState(view.state);
    paging.reset();
    selection.clear();
  }

  function setFacet(facet: Facet, values: string[]) {
    setState((s) => ({ ...s, facets: { ...s.facets, [facet]: values } }));
    paging.reset();
  }

  const facetOptions = useMemo(() => {
    const out = {} as Record<Facet, [string, number][]>;
    for (const f of FACETS) {
      const counts = new Map<string, number>();
      for (const d of devices) {
        const v = facetValue(d, f.key);
        counts.set(v, (counts.get(v) ?? 0) + 1);
      }
      out[f.key] = [...counts.entries()].sort((a, b) => b[1] - a[1] || versionCompare(a[0], b[0]));
    }
    return out;
  }, [devices]);

  const activeFacets = FACETS.filter((f) => state.facets[f.key]?.length);

  // Keyboard ---------------------------------------------------------------------

  const rows = paging.rows;
  const peekIndex = peekId ? sorted.findIndex((d) => d.id === peekId) : -1;
  const peekDevice = peekIndex >= 0 ? sorted[peekIndex]! : null;

  const step = useCallback(
    (delta: -1 | 1) => {
      if (peekIndex < 0) return;
      const next = sorted[Math.min(sorted.length - 1, Math.max(0, peekIndex + delta))];
      if (next) setPeekId(next.id);
    },
    [peekIndex, sorted],
  );

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const tag = (e.target as HTMLElement | null)?.tagName ?? "";
      if (["INPUT", "TEXTAREA", "SELECT", "BUTTON"].includes(tag) || e.metaKey || e.ctrlKey || e.altKey) return;
      if (peekId) return; // the drawer owns the arrows while it is open
      if (e.key === "/") {
        e.preventDefault();
        searchRef.current?.focus();
      } else if (e.key === "j" || e.key === "ArrowDown") {
        e.preventDefault();
        setCursor((c) => Math.min(rows.length - 1, c + 1));
      } else if (e.key === "k" || e.key === "ArrowUp") {
        e.preventDefault();
        setCursor((c) => Math.max(0, c - 1));
      } else if (e.key === " " && rows[cursor]) {
        e.preventDefault();
        setPeekId(rows[cursor]!.id);
      } else if (e.key === "Enter" && rows[cursor]) {
        router.push(`/devices/${rows[cursor]!.id}`);
      } else if (e.key === "x" && rows[cursor]) {
        selection.toggle(rows[cursor]!.id);
      } else if (e.key === "Escape" && selection.count) {
        selection.clear();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [rows, cursor, peekId, router, selection]);

  useEffect(() => setCursor(0), [state, sort]);

  // Actions ----------------------------------------------------------------------

  function exportRows(list: Device[], label: string) {
    downloadCSV(
      `edgepilot-devices-${label}-${stamp()}.csv`,
      ["Hostname", "Client", "Status", "Approved", "Site", "Model", "Chip", "OS", "Agent", "Serial", "Local IP", "CPU %", "Memory %", "Disk %", "Last seen"],
      list.map((d) => [
        d.hostname, d.client_name, d.status, d.approved ? "yes" : "no", d.site, d.model_name || d.model_identifier, d.chip,
        d.os_version ? `${d.os_name || "macOS"} ${d.os_version}` : "", d.agent_version, d.serial_number, d.local_ip,
        pct(d.last_cpu_percent), pct(d.last_memory_percent), pct(d.last_disk_percent), d.last_seen_at ?? "",
      ]),
    );
    push({ title: `Exported ${list.length} device${list.length === 1 ? "" : "s"}`, tone: "success" });
  }

  async function settle(tasks: Promise<Device>[], what: string) {
    const results = await Promise.allSettled(tasks);
    let ok = 0;
    for (const r of results) {
      if (r.status === "fulfilled") {
        ok += 1;
        applyDevice(r.value);
      }
    }
    push({
      title: `${what} ${ok} of ${results.length}`,
      description: ok < results.length ? "The ones that failed are still selected." : undefined,
      tone: ok === results.length ? "success" : "error",
    });
    if (ok === results.length) selection.clear();
  }

  const canConnect = can("ssh:connect");
  const show = (c: Column) => columns.includes(c);
  const unapprovedSelected = selectedDevices.filter((d) => !d.approved);
  const connect = (d: Device) => setTerminal({ id: d.id, hostname: d.hostname || d.serial_number });

  return (
    <>
      <PageHeader
        title="Devices"
        description="Every machine enrolled in your organization. Click a row for a quick view, or the name to open the full page."
        meta={<LivePill live={live} />}
        actions={
          <>
            <IconButton icon="refresh" label="Refresh" variant="secondary" onClick={() => void reload()} />
            <Button icon="download" onClick={() => exportRows(sorted, "filtered")} disabled={sorted.length === 0}>
              Export
            </Button>
            {can("enrollment_token:write") && (
              <Button variant="primary" icon="plus" onClick={() => router.push("/enrollment?new=1")}>
                Enroll device
              </Button>
            )}
          </>
        }
      />

      {/* Views */}
      <div className="-mx-1 mb-4 flex items-center gap-1 overflow-x-auto px-1 pb-1">
        {BUILT_IN.map((v) => (
          <ViewTab
            key={v.id}
            active={activeView === v.id}
            icon={v.icon}
            label={v.name}
            count={viewCounts[v.id]}
            title={v.hint}
            onClick={() => applyView(v.id)}
            tone={v.id !== "all" && (viewCounts[v.id] ?? 0) > 0 ? "warning" : undefined}
          />
        ))}
        {saved.length > 0 && <span className="mx-1 h-5 w-px shrink-0 bg-surface-border" aria-hidden />}
        {saved.map((v) => (
          <ViewTab
            key={v.id}
            active={activeView === v.id}
            icon="bookmark"
            label={v.name}
            onClick={() => applyView(v.id)}
            onRemove={() => {
              setSaved((all) => all.filter((x) => x.id !== v.id));
              if (activeView === v.id) applyView("all");
              push({ title: `View “${v.name}” deleted`, tone: "success" });
            }}
          />
        ))}
      </div>

      {terminal && (
        <Card className="mb-6 h-[28rem] overflow-hidden" padded={false}>
          <DeviceTerminal deviceId={terminal.id} hostname={terminal.hostname} onClose={() => setTerminal(null)} />
        </Card>
      )}

      <Card padded={false} className="overflow-hidden">
        {selection.count > 0 ? (
          <div className="flex flex-wrap items-center gap-2 border-b border-accent/20 bg-accent-subtle/60 px-4 py-3 sm:px-5">
            <span className="flex h-7 min-w-7 items-center justify-center rounded-md bg-accent px-2 text-xs font-semibold text-white">
              {selection.count}
            </span>
            <span className="text-sm font-medium text-ink">selected</span>
            <Button size="sm" variant="ghost" onClick={selection.clear}>
              Clear <Kbd>Esc</Kbd>
            </Button>
            <div className="ml-auto flex flex-wrap gap-2">
              {can("device:approve") && unapprovedSelected.length > 0 && (
                <Button size="sm" variant="primary" icon="check" onClick={() => void settle(unapprovedSelected.map((d) => approveDevice(d.id)), "Approved")}>
                  Approve {unapprovedSelected.length}
                </Button>
              )}
              {can("device:write") && (
                <Button size="sm" icon="building" onClick={() => setBulkClient(true)}>Set client</Button>
              )}
              <Button
                size="sm"
                icon="bookmark"
                onClick={() => {
                  selectedDevices.forEach((d) => togglePin({ id: d.id, name: d.hostname || d.serial_number, client: d.client_name || undefined }));
                  push({ title: "Sidebar pins updated", tone: "success" });
                }}
              >
                Toggle pin
              </Button>
              <Button size="sm" icon="download" onClick={() => exportRows(selectedDevices, "selected")}>Export</Button>
            </div>
          </div>
        ) : (
          <div className="space-y-3 border-b border-surface-border px-4 py-4 sm:px-5">
            <div className="flex flex-wrap items-center gap-2">
              <SearchInput
                ref={searchRef}
                className="w-full sm:w-72"
                placeholder="Search name, client, serial, IP…"
                value={state.q}
                onChange={(e) => {
                  setState((s) => ({ ...s, q: e.target.value }));
                  paging.reset();
                }}
                aria-label="Search devices"
              />
              <div className="flex flex-wrap items-center gap-1.5">
                {FACETS.map((f) => (
                  <FacetFilter
                    key={f.key}
                    facet={f}
                    options={facetOptions[f.key] ?? []}
                    selected={state.facets[f.key] ?? []}
                    onChange={(v) => setFacet(f.key, v)}
                  />
                ))}
              </div>
              <div className="ml-auto flex items-center gap-2">
                {dirty && (
                  <Button size="sm" variant="ghost" icon="save" onClick={() => setSavingView(true)}>
                    Save view
                  </Button>
                )}
                <ColumnPicker columns={columns} onChange={setColumns} />
                <Segmented<Layout>
                  aria-label="Layout"
                  size="sm"
                  value={layout}
                  onChange={setLayout}
                  options={[
                    { value: "table", label: <Icon name="menu" size={14} aria-label="Table" /> },
                    { value: "grid", label: <Icon name="grid" size={14} aria-label="Tiles" /> },
                  ]}
                />
              </div>
            </div>
            {(activeFacets.length > 0 || state.q || state.preset) && (
              <div className="flex flex-wrap items-center gap-1.5 text-xs">
                <span className="mr-1 text-ink-faint">
                  Showing <span className="font-semibold text-ink">{sorted.length}</span> of {devices.length}
                </span>
                {activeFacets.map((f) => (
                  <span
                    key={f.key}
                    className="inline-flex items-center gap-1 rounded-full border border-accent/20 bg-accent-subtle py-0.5 pl-2.5 pr-1 font-medium text-accent dark:text-accent-soft"
                  >
                    {f.label}: {state.facets[f.key]!.join(", ")}
                    <button
                      type="button"
                      aria-label={`Clear ${f.label} filter`}
                      onClick={() => setFacet(f.key, [])}
                      className="rounded-full p-0.5 hover:bg-accent/10"
                    >
                      <Icon name="x" size={12} />
                    </button>
                  </span>
                ))}
                <button type="button" className="ml-1 font-medium text-ink-muted hover:text-ink" onClick={() => applyView("all")}>
                  Reset all
                </button>
              </div>
            )}
          </div>
        )}

        <ErrorNote error={error} onRetry={() => void reload()} />

        {loading && devices.length === 0 ? (
          <TableSkeleton rows={8} columns={6} />
        ) : sorted.length === 0 ? (
          <EmptyState
            icon={devices.length === 0 ? "monitor" : activeView === "all" ? "filter" : "check-circle"}
            title={devices.length === 0 ? "No devices yet" : activeView !== "all" && !dirty ? "Nothing here" : "No devices match"}
            description={
              devices.length === 0
                ? "Install the agent with an enrollment token and the device appears here within seconds."
                : activeView !== "all" && !dirty
                  ? "No device fits this view right now — which is usually good news."
                  : "Try a different search, or clear a filter."
            }
            action={
              devices.length === 0 ? (
                can("enrollment_token:write") && (
                  <Button variant="primary" icon="plus" onClick={() => router.push("/enrollment?new=1")}>Enroll your first device</Button>
                )
              ) : (
                <Button onClick={() => applyView("all")}>Show all devices</Button>
              )
            }
          />
        ) : layout === "grid" ? (
          <>
            <FleetGrid devices={paging.rows} onConnect={connect} />
            <Pagination page={paging.page} pages={paging.pages} pageSize={paging.pageSize} total={paging.total} onPage={paging.setPage} onPageSize={paging.setPageSize} />
          </>
        ) : (
          <>
            <Table>
              <THead>
                <tr>
                  <Th className="w-10 !pr-0">
                    <SelectionCheckbox
                      checked={selection.allSelected}
                      indeterminate={selection.someSelected}
                      onChange={selection.toggleAll}
                      label="Select all on this page"
                    />
                  </Th>
                  <SortableTh sortKey="hostname" sort={sort} onSort={setSort}>Device</SortableTh>
                  {show("client") && <SortableTh sortKey="client" sort={sort} onSort={setSort} className="hidden sm:table-cell">Client</SortableTh>}
                  {show("status") && <SortableTh sortKey="status" sort={sort} onSort={setSort}>Status</SortableTh>}
                  {show("health") && <SortableTh sortKey="cpu" sort={sort} onSort={setSort} className="hidden md:table-cell">Health</SortableTh>}
                  {show("site") && <SortableTh sortKey="site" sort={sort} onSort={setSort} className="hidden lg:table-cell">Site</SortableTh>}
                  {show("os") && <SortableTh sortKey="os" sort={sort} onSort={setSort} className="hidden xl:table-cell">OS</SortableTh>}
                  {show("agent") && <SortableTh sortKey="agent" sort={sort} onSort={setSort} className="hidden xl:table-cell">Agent</SortableTh>}
                  {show("seen") && <SortableTh sortKey="seen" sort={sort} onSort={setSort} className="hidden sm:table-cell">Last seen</SortableTh>}
                  <Th align="right"><span className="sr-only">Actions</span></Th>
                </tr>
              </THead>
              <TBody>
                {paging.rows.map((device, i) => (
                  <Tr
                    key={device.id}
                    interactive
                    selected={selection.isSelected(device.id) || peekId === device.id}
                    onClick={(e) => {
                      setCursor(i);
                      if (e.metaKey || e.ctrlKey) router.push(`/devices/${device.id}`);
                      else setPeekId(device.id);
                    }}
                    className={cn(cursor === i && "shadow-[inset_3px_0_0_rgb(var(--accent))]")}
                  >
                    <Td className="w-10 !pr-0" onClick={(e) => e.stopPropagation()}>
                      <SelectionCheckbox
                        checked={selection.isSelected(device.id)}
                        onChange={() => selection.toggle(device.id)}
                        label={`Select ${device.hostname || device.serial_number}`}
                      />
                    </Td>
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
                          <div className="truncate text-xs text-ink-faint" title={`Serial ${device.serial_number}`}>
                            {[device.model_name || device.model_identifier, device.chip].filter(Boolean).join(" · ") || device.serial_number}
                          </div>
                        </div>
                      </div>
                    </Td>
                    {show("client") && (
                      <Td className="hidden min-w-[10rem] whitespace-nowrap sm:table-cell" onClick={(e) => e.stopPropagation()}>
                        <ClientNameCell device={device} onSaved={applyDevice} />
                      </Td>
                    )}
                    {show("status") && (
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
                    )}
                    {show("health") && <Td className="hidden w-48 md:table-cell"><HealthCell device={device} /></Td>}
                    {show("site") && <Td className="hidden whitespace-nowrap text-ink-muted lg:table-cell">{device.site || "—"}</Td>}
                    {show("os") && (
                      <Td className="hidden whitespace-nowrap text-ink-muted xl:table-cell">
                        {device.os_version ? `${device.os_name || "macOS"} ${device.os_version}` : "—"}
                      </Td>
                    )}
                    {show("agent") && (
                      <Td className="hidden whitespace-nowrap xl:table-cell">
                        <span
                          className={cn("font-mono text-xs", latestAgent && device.agent_version !== latestAgent ? "text-degraded" : "text-ink-muted")}
                          title={latestAgent && device.agent_version !== latestAgent ? `Newest in fleet: ${latestAgent}` : undefined}
                        >
                          {device.agent_version || "—"}
                        </span>
                      </Td>
                    )}
                    {show("seen") && (
                      <Td className="hidden whitespace-nowrap text-ink-muted sm:table-cell" title={device.last_seen_at}>
                        {relativeTime(device.last_seen_at)}
                      </Td>
                    )}
                    <Td align="right" onClick={(e) => e.stopPropagation()}>
                      <div className="flex items-center justify-end gap-1">
                        {canConnect && device.status === "online" && device.approved && (
                          <Button size="sm" icon="terminal" onClick={() => connect(device)}>Connect</Button>
                        )}
                        <RowMenu device={device} onPeek={() => setPeekId(device.id)} onConnect={canConnect ? () => connect(device) : undefined} />
                      </div>
                    </Td>
                  </Tr>
                ))}
              </TBody>
            </Table>
            <div className="flex flex-wrap items-center justify-between border-t border-surface-border">
              <KeyboardHints />
              <div className="ml-auto">
                <Pagination page={paging.page} pages={paging.pages} pageSize={paging.pageSize} total={paging.total} onPage={paging.setPage} onPageSize={paging.setPageSize} className="border-t-0" />
              </div>
            </div>
          </>
        )}
      </Card>

      <DeviceDrawer
        device={peekDevice}
        onClose={() => setPeekId(null)}
        onStep={step}
        position={peekIndex >= 0 ? { index: peekIndex, total: sorted.length } : undefined}
        onConnect={(d) => {
          setPeekId(null);
          connect(d);
        }}
        onChanged={applyDevice}
      />

      <SaveViewModal
        open={savingView}
        onClose={() => setSavingView(false)}
        onSave={(name) => {
          const view: SavedView = { id: `v${Date.now().toString(36)}`, name, state: normalize(state) };
          setSaved((all) => [...all, view]);
          setActiveView(view.id);
          setSavingView(false);
          push({ title: `View “${name}” saved`, description: "Stored in this browser.", tone: "success" });
        }}
      />

      <BulkClientModal
        open={bulkClient}
        count={selection.count}
        onClose={() => setBulkClient(false)}
        suggestions={(facetOptions.client ?? []).map(([c]) => c).filter((c) => c !== "Unassigned")}
        onApply={async (client) => {
          await settle(selectedDevices.map((d) => updateDevice(d.id, { client_name: client })), "Client set on");
          setBulkClient(false);
        }}
      />
    </>
  );
}

// --- Pieces --------------------------------------------------------------------

function LivePill({ live }: { live: boolean }) {
  return (
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
  );
}

function ViewTab({
  active, icon, label, count, title, onClick, onRemove, tone,
}: {
  active: boolean;
  icon: IconName;
  label: string;
  count?: number;
  title?: string;
  onClick: () => void;
  onRemove?: () => void;
  tone?: "warning";
}) {
  return (
    <span
      className={cn(
        "group relative inline-flex shrink-0 items-center rounded-lg",
        active && "bg-surface-raised shadow-sm ring-1 ring-surface-border",
      )}
    >
      <button
        type="button"
        onClick={onClick}
        title={title}
        aria-pressed={active}
        className={cn(
          "inline-flex h-9 items-center gap-2 rounded-lg px-3 text-sm font-medium transition-colors",
          active ? "text-ink" : "text-ink-muted hover:bg-surface-hover hover:text-ink",
          onRemove && "pr-1.5",
        )}
      >
        <Icon name={icon} size={15} className={active ? "text-accent" : "text-ink-faint"} />
        {label}
        {count !== undefined && (
          <span
            className={cn(
              "rounded-full px-1.5 py-px text-2xs font-semibold tabular",
              tone === "warning" ? "bg-degraded/15 text-degraded" : active ? "bg-accent-subtle text-accent" : "bg-surface-hover text-ink-muted",
            )}
          >
            {count}
          </span>
        )}
      </button>
      {onRemove && (
        <button
          type="button"
          onClick={onRemove}
          aria-label={`Delete view ${label}`}
          className="mr-1.5 rounded p-0.5 text-ink-faint opacity-0 hover:bg-surface-hover hover:text-ink focus-visible:opacity-100 group-hover:opacity-100"
        >
          <Icon name="x" size={12} />
        </button>
      )}
    </span>
  );
}

/** One facet as a dropdown of checkboxes, each with how many devices it matches. */
function FacetFilter({
  facet, options, selected, onChange,
}: {
  facet: { key: Facet; label: string; icon: IconName };
  options: [string, number][];
  selected: string[];
  onChange: (v: string[]) => void;
}) {
  const [query, setQuery] = useState("");
  const shown = options.filter(([v]) => v.toLowerCase().includes(query.toLowerCase()));
  const active = selected.length > 0;
  const label = (v: string) => (facet.key === "status" ? v.charAt(0).toUpperCase() + v.slice(1) : v);
  return (
    <Popover
      align="start"
      width="w-64"
      trigger={({ open, toggle }) => (
        <button
          type="button"
          onClick={toggle}
          aria-expanded={open}
          className={cn(
            "inline-flex h-8 items-center gap-1.5 rounded-lg border px-2.5 text-xs font-medium transition-colors",
            active
              ? "border-accent/30 bg-accent-subtle text-accent dark:text-accent-soft"
              : "border-dashed border-surface-border-strong text-ink-muted hover:border-solid hover:bg-surface-hover hover:text-ink",
          )}
        >
          <Icon name={active ? facet.icon : "plus"} size={13} />
          {facet.label}
          {active && <span className="rounded-full bg-accent px-1.5 text-2xs text-white">{selected.length}</span>}
        </button>
      )}
    >
      <div>
        {options.length > 6 && (
          <div className="border-b border-surface-border p-2">
            <Input autoFocus className="h-8" placeholder={`Filter ${facet.label.toLowerCase()}…`} value={query} onChange={(e) => setQuery(e.target.value)} />
          </div>
        )}
        <ul className="max-h-64 overflow-y-auto p-1">
          {shown.length === 0 && <li className="px-2.5 py-3 text-center text-xs text-ink-faint">No matches</li>}
          {shown.map(([value, count]) => {
            const checked = selected.includes(value);
            return (
              <li key={value}>
                <label className="flex cursor-pointer items-center gap-2.5 rounded-md px-2.5 py-1.5 text-sm hover:bg-surface-hover">
                  <input
                    type="checkbox"
                    checked={checked}
                    onChange={() => onChange(checked ? selected.filter((v) => v !== value) : [...selected, value])}
                    className="h-4 w-4 rounded border-surface-border-strong"
                  />
                  <span className="min-w-0 flex-1 truncate text-ink">{label(value)}</span>
                  <span className="text-xs tabular text-ink-faint">{count}</span>
                </label>
              </li>
            );
          })}
        </ul>
        {active && (
          <div className="border-t border-surface-border p-1">
            <button
              type="button"
              onClick={() => onChange([])}
              className="w-full rounded-md px-2.5 py-1.5 text-left text-xs font-medium text-ink-muted hover:bg-surface-hover hover:text-ink"
            >
              Clear {facet.label.toLowerCase()} filter
            </button>
          </div>
        )}
      </div>
    </Popover>
  );
}

function ColumnPicker({ columns, onChange }: { columns: Column[]; onChange: (c: Column[]) => void }) {
  return (
    <Popover
      width="w-52"
      trigger={({ open, toggle }) => (
        <IconButton icon="columns" label="Choose columns" variant="secondary" size="sm" onClick={toggle} aria-expanded={open} />
      )}
    >
      <div className="p-1">
        <div className="px-2.5 pb-1 pt-1.5 text-2xs font-semibold uppercase tracking-wider text-ink-faint">Columns</div>
        {COLUMNS.map((c) => (
          <label key={c.key} className="flex cursor-pointer items-center gap-2.5 rounded-md px-2.5 py-1.5 text-sm text-ink hover:bg-surface-hover">
            <input
              type="checkbox"
              checked={columns.includes(c.key)}
              onChange={() =>
                onChange(
                  columns.includes(c.key)
                    ? columns.filter((x) => x !== c.key)
                    : COLUMNS.map((x) => x.key).filter((k) => k === c.key || columns.includes(k)),
                )
              }
              className="h-4 w-4 rounded border-surface-border-strong"
            />
            {c.label}
          </label>
        ))}
        <div className="mt-1 border-t border-surface-border pt-1">
          <button
            type="button"
            onClick={() => onChange(DEFAULT_COLUMNS)}
            className="w-full rounded-md px-2.5 py-1.5 text-left text-xs font-medium text-ink-muted hover:bg-surface-hover hover:text-ink"
          >
            Reset to default
          </button>
        </div>
      </div>
    </Popover>
  );
}

function KeyboardHints() {
  return (
    <div className="hidden items-center gap-3 px-5 py-3 text-2xs text-ink-faint lg:flex">
      <span className="inline-flex items-center gap-1"><Kbd>J</Kbd><Kbd>K</Kbd> move</span>
      <span className="inline-flex items-center gap-1"><Kbd>Space</Kbd> quick view</span>
      <span className="inline-flex items-center gap-1"><Kbd>↵</Kbd> open</span>
      <span className="inline-flex items-center gap-1"><Kbd>X</Kbd> select</span>
      <span className="inline-flex items-center gap-1"><Kbd>/</Kbd> search</span>
    </div>
  );
}

function SaveViewModal({ open, onClose, onSave }: { open: boolean; onClose: () => void; onSave: (name: string) => void }) {
  const [name, setName] = useState("");
  useEffect(() => {
    if (!open) setName("");
  }, [open]);
  return (
    <Modal
      open={open}
      onClose={onClose}
      size="sm"
      title="Save view"
      description="Keep this search and set of filters one click away. Saved in this browser."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form="save-view" disabled={!name.trim()}>Save view</Button>
        </>
      }
    >
      <form
        id="save-view"
        onSubmit={(e) => {
          e.preventDefault();
          if (name.trim()) onSave(name.trim());
        }}
      >
        <Field label="Name" htmlFor="view-name">
          <Input id="view-name" autoFocus maxLength={40} value={name} onChange={(e) => setName(e.target.value)} placeholder="Northwind stores" />
        </Field>
      </form>
    </Modal>
  );
}

function BulkClientModal({
  open, count, onClose, onApply, suggestions,
}: {
  open: boolean;
  count: number;
  onClose: () => void;
  onApply: (client: string) => Promise<void>;
  suggestions: string[];
}) {
  const [client, setClient] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (!open) setClient("");
  }, [open]);
  return (
    <Modal
      open={open}
      onClose={onClose}
      size="sm"
      title={`Set client on ${count} device${count === 1 ? "" : "s"}`}
      description="Leave empty to clear the client name."
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
          <Button variant="primary" type="submit" form="bulk-client" loading={busy}>Apply</Button>
        </>
      }
    >
      <form
        id="bulk-client"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          try {
            await onApply(client.trim());
          } finally {
            setBusy(false);
          }
        }}
        className="space-y-3"
      >
        <Field label="Client name" htmlFor="bulk-client-name">
          <Input id="bulk-client-name" autoFocus maxLength={200} value={client} onChange={(e) => setClient(e.target.value)} placeholder="Northwind Retail" />
        </Field>
        {suggestions.length > 0 && (
          <div className="flex flex-wrap gap-1.5">
            {suggestions.slice(0, 8).map((s) => (
              <button
                key={s}
                type="button"
                onClick={() => setClient(s)}
                className={cn(
                  "rounded-full border px-2.5 py-1 text-xs transition-colors",
                  client === s ? "border-accent/40 bg-accent-subtle text-accent" : "border-surface-border text-ink-muted hover:border-accent/40 hover:text-ink",
                )}
              >
                {s}
              </button>
            ))}
          </div>
        )}
      </form>
    </Modal>
  );
}
