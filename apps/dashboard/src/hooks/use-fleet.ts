"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { fleetSummary, listDevices, type Device, type DeviceQuery, type FleetSummary } from "@/lib/api/devices";
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
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>(null);
  const [live, setLive] = useState(false);

  const pending = useRef<ReturnType<typeof setTimeout> | null>(null);
  const queryKey = JSON.stringify(query);

  const load = useCallback(async () => {
    if (!principal) return;
    try {
      const [page, counts] = await Promise.all([listDevices(query), fleetSummary()]);
      setDevices(page.data);
      setSummary(counts);
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
      onEvent: () => {
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

  return { devices, summary, loading, error, live, reload: load };
}
