"use client";

import { useCallback, useEffect, useRef, useState } from "react";

/**
 * One async read, with the three states every view needs and nothing more.
 *
 * `reload` refetches without clearing the current value, so a table that is
 * refreshing keeps its rows on screen instead of collapsing into a skeleton
 * and back. `setData` lets a mutation apply the server's response directly —
 * the row the server wrote is not a guess, and refetching a list to learn one
 * change the operator just made puts a round trip between them and their edit.
 */
export function useAsync<T>(fn: () => Promise<T>, deps: React.DependencyList) {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>(null);
  // A stale response from an earlier call must not overwrite a newer one.
  const generation = useRef(0);

  const load = useCallback(async () => {
    const mine = ++generation.current;
    try {
      const result = await fn();
      if (mine !== generation.current) return;
      setData(result);
      setError(null);
    } catch (err) {
      if (mine !== generation.current) return;
      setError(err);
    } finally {
      if (mine === generation.current) setLoading(false);
    }
    // The caller's deps define identity, not the function object.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  useEffect(() => {
    setLoading(true);
    void load();
  }, [load]);

  return { data, loading, error, reload: load, setData };
}

/** A pending mutation: busy flag plus the last error, for a button and a note. */
export function useMutation<A extends unknown[], R>(fn: (...args: A) => Promise<R>) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const run = useCallback(
    async (...args: A): Promise<R | undefined> => {
      setBusy(true);
      setError(null);
      try {
        return await fn(...args);
      } catch (err) {
        setError(err);
        return undefined;
      } finally {
        setBusy(false);
      }
    },
    [fn],
  );
  return { run, busy, error, reset: () => setError(null) };
}
