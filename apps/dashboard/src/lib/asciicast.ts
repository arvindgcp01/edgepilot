/**
 * Asciicast v2, the format the server records web-terminal sessions in.
 *
 *   {"version":2,"width":120,"height":32,"timestamp":...}   ← header
 *   [0.248, "o", "Last login: …"]                           ← one event per line
 *
 * Only "o" (output) events are kept: the server records what the operator saw,
 * and replay is a re-run of exactly that.
 */
export type Cast = {
  width: number;
  height: number;
  startedAt?: Date;
  title?: string;
  /** [seconds from start, output], sorted. */
  events: [number, string][];
  duration: number;
};

export function parseCast(text: string): Cast {
  const lines = text.split("\n").filter((l) => l.trim() !== "");
  if (lines.length === 0) throw new Error("The recording is empty.");
  let header: { version?: number; width?: number; height?: number; timestamp?: number; title?: string };
  try {
    header = JSON.parse(lines[0]!);
  } catch {
    throw new Error("The recording header is not valid JSON.");
  }
  if (header.version !== 2) throw new Error(`Unsupported recording version ${String(header.version)}.`);

  const events: [number, string][] = [];
  for (const line of lines.slice(1)) {
    try {
      const e = JSON.parse(line) as [number, string, string];
      if (Array.isArray(e) && e[1] === "o" && typeof e[0] === "number" && typeof e[2] === "string") events.push([e[0], e[2]]);
    } catch {
      // A truncated last line — the session was killed mid-write. Everything
      // before it is still a faithful record.
    }
  }
  events.sort((a, b) => a[0] - b[0]);
  return {
    width: header.width ?? 80,
    height: header.height ?? 24,
    startedAt: header.timestamp ? new Date(header.timestamp * 1000) : undefined,
    title: header.title,
    events,
    duration: events.length ? events[events.length - 1]![0] : 0,
  };
}

/**
 * Long idle gaps are compressed for playback — a session where the operator
 * went for coffee should not replay ten minutes of nothing. Returns the
 * playback timeline and the total compressed duration.
 */
export function compressIdle(events: [number, string][], maxGap = 2): { times: number[]; duration: number } {
  const times: number[] = [];
  let t = 0;
  let prev = 0;
  for (const [at] of events) {
    t += Math.min(Math.max(0, at - prev), maxGap);
    times.push(t);
    prev = at;
  }
  return { times, duration: t };
}

export function formatClock(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  const m = Math.floor(s / 60);
  const h = Math.floor(m / 60);
  const mm = String(m % 60).padStart(h ? 2 : 1, "0");
  const ss = String(s % 60).padStart(2, "0");
  return h ? `${h}:${mm}:${ss}` : `${mm}:${ss}`;
}
