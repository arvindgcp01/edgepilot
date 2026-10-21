"use client";

import { useCallback, useEffect, useState } from "react";

/**
 * Per-browser UI state: pinned devices, recently viewed devices, saved views,
 * column choices.
 *
 * These describe how one operator likes to work on one screen, not anything
 * about the fleet, so they never reach the server. Storage can be unavailable
 * (private windows, locked-down kiosks); every access is guarded and the
 * fallback is the initial value, so the UI degrades to "nothing remembered"
 * rather than breaking.
 *
 * Writes broadcast an event so every component reading the same key — the
 * sidebar's pinned list and a device page's pin button — stays in step.
 */
const EVENT = "edgepilot:store";

function read<T>(key: string, initial: T): T {
  try {
    const raw = window.localStorage.getItem(key);
    return raw === null ? initial : (JSON.parse(raw) as T);
  } catch {
    return initial;
  }
}

export function writeStored<T>(key: string, value: T) {
  try {
    window.localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Holds for this page view only.
  }
  window.dispatchEvent(new CustomEvent(EVENT, { detail: key }));
}

export function useStored<T>(key: string, initial: T): [T, (next: T | ((current: T) => T)) => void] {
  const [value, setValue] = useState<T>(initial);

  useEffect(() => {
    setValue(read(key, initial));
    const onChange = (e: Event) => {
      if ((e as CustomEvent<string>).detail === key) setValue(read(key, initial));
    };
    const onStorage = (e: StorageEvent) => {
      if (e.key === key) setValue(read(key, initial));
    };
    window.addEventListener(EVENT, onChange);
    window.addEventListener("storage", onStorage);
    return () => {
      window.removeEventListener(EVENT, onChange);
      window.removeEventListener("storage", onStorage);
    };
    // `initial` is a default, not an input: re-reading when a caller passes a
    // fresh array literal each render would loop.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  const update = useCallback(
    (next: T | ((current: T) => T)) => {
      const resolved = typeof next === "function" ? (next as (c: T) => T)(read(key, initial)) : next;
      setValue(resolved);
      writeStored(key, resolved);
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [key],
  );

  return [value, update];
}

// --- Pinned and recent devices ------------------------------------------------

/** Just enough to render a link without fetching: the id and what to call it. */
export type DeviceRef = { id: string; name: string; client?: string };

export const PINNED_KEY = "edgepilot.pinned";
export const RECENT_KEY = "edgepilot.recent";
const RECENT_MAX = 8;

export function usePinned() {
  const [pinned, setPinned] = useStored<DeviceRef[]>(PINNED_KEY, []);
  return {
    pinned,
    isPinned: (id: string) => pinned.some((d) => d.id === id),
    toggle: (ref: DeviceRef) =>
      setPinned((current) =>
        current.some((d) => d.id === ref.id) ? current.filter((d) => d.id !== ref.id) : [...current, ref].slice(-12),
      ),
    remove: (id: string) => setPinned((current) => current.filter((d) => d.id !== id)),
  };
}

export function rememberRecent(ref: DeviceRef) {
  const current = read<DeviceRef[]>(RECENT_KEY, []);
  writeStored(RECENT_KEY, [ref, ...current.filter((d) => d.id !== ref.id)].slice(0, RECENT_MAX));
}

export function useRecent() {
  return useStored<DeviceRef[]>(RECENT_KEY, [])[0];
}
