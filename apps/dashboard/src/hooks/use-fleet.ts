"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import {
  fleetSummary, listDevices, listSSHSessions,
  type Device, type DeviceQuery, type FleetSummary, type MetricsEvent, type SSHSession,
} from "@/lib/api/devices";
import { can } from "@/lib/api/client";
import { connectEvents } from "@/lib/events";
import { useSession } from "@/components/session-provider";

/**
 * The fleet, kept current.
 *
 * Events tell this hook that something changed; it refetches rather than
 * patching state from the frame. That is deliberate — events carry no data a
 * caller could not fetch, so there is nothing to patch from, and a refetch
 * cannot drift out of sync with the server the way an incrementally-mutated
 * cache can.
 *
 * Refetches are coalesced: a site whose uplink drops sends fifty devices
 * offline at once, and fifty list queries would be a self-inflicted load spike.
 */
export function useFleet(query: DeviceQuery) {
  // Nothing is fetched until the session is resolved. Without this the first
  // render fires requests with no access token, the server answers 401 — quite
  // correctly — and the console fills with failures that look like a broken
  // API rather than a component that started too early.
  const { principal, loading: sessionLoading } = useSession();

  const [devices, setDevices] = useState<Device[]>([]);
  const [summary, setSummary] = useState<FleetSummary | null>(null);
  /** Live SSH sessions, keyed by device, so the grid can show who is connected. */
  const [sshByDevice, setSSHByDevice] = useState<Record<string, SSHSession[]>>({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>(null);
  const [live, setLive] = useState(false);

  const pending = useRef<ReturnType<typeof setTimeout> | null>(null);
  const queryKey = JSON.stringify(query);

  const load = useCallback(async () => {
    if (!principal) return;
    try {
      // Sessions are fetched alongside, not per row: one request for the fleet
      // rather than one per device. A caller without ssh:read simply gets none,
      // and the column disappears rather than the whole page failing.
      const [page, counts, sessions] = await Promise.all([
        listDevices(query),
        fleetSummary(),
        can("ssh:read")
          ? listSSHSessions().catch(() => ({ data: [] as SSHSession[] }))
          : Promise.resolve({ data: [] as SSHSession[] }),
      ]);

      setDevices(page.data);
      setSummary(counts);

      const grouped: Record<string, SSHSession[]> = {};
      for (const session of sessions.data) {
        (grouped[session.device_id] ??= []).push(session);
      }
      setSSHByDevice(grouped);
      setError(null);
    } catch (err) {
      setError(err);
    } finally {
      setLoading(false);
    }
    // The serialised query is the dependency: the object identity changes on
    // every render, and depending on it would refetch forever.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [queryKey, principal]);

  /**
   * Replace one device with a row the server just returned.
   *
   * The note above says this hook refetches rather than patching, and that holds
   * for events: they carry no data, so there is nothing to patch from and a
   * guess would drift. A PATCH response is not a guess — it is the row the
   * server wrote. Refetching the whole fleet to learn one string the operator
   * just typed would put a round trip between them and their own edit.
   */
  const applyDevice = useCallback((device: Device) => {
    setDevices((current) => current.map((d) => (d.id === device.id ? device : d)));
  }, []);

  useEffect(() => {
    if (sessionLoading) return;
    if (!principal) {
      setLoading(false);
      return;
    }
    setLoading(true);
    void load();
  }, [load, principal, sessionLoading]);

  useEffect(() => {
    if (!principal) return;
    const disconnect = connectEvents({
      topics: ["fleet"],
      onStatus: setLive,
      onEvent: (event) => {
        // A metrics event is the one frame that does carry data — three
        // percentages for one row — and it arrives every thirty seconds per
        // device. Refetching the fleet for each would be a self-inflicted load
        // spike at exactly the rate the fleet grows, so the row is patched in
        // place and everything else still refetches.
        if (event.type === "device.metrics" && event.device_id) {
          const data = event.data as MetricsEvent | undefined;
          if (!data) return;
          setDevices((current) =>
            current.map((d) =>
              d.id === event.device_id
                ? {
                    ...d,
                    last_cpu_percent: data.cpu_percent,
                    last_memory_percent: data.memory_percent,
                    last_disk_percent: data.disk_percent,
                    last_seen_at: data.at,
                    // A device that reports metrics is not offline, whatever
                    // the sweeper last decided.
                    status: d.status === "offline" ? "online" : d.status,
                  }
                : d,
            ),
          );
          return;
        }
        if (pending.current) return;
        pending.current = setTimeout(() => {
          pending.current = null;
          void load();
        }, 400);
      },
    });
    return () => {
      disconnect();
      if (pending.current) clearTimeout(pending.current);
    };
  }, [load, principal]);

  return { devices, summary, sshByDevice, loading, error, live, reload: load, applyDevice };
}
