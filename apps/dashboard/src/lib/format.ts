/** Presentation helpers. Pure, so they are trivially testable. */

export function bytes(value?: number): string {
  if (!value || value <= 0) return "—";
  // Binary units, matching how the agent parsed them from the machine. A
  // dashboard that says 17.2 GB where the Mac says 16 GB is a dashboard nobody
  // trusts.
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let n = value;
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n >= 10 || i === 0 ? Math.round(n) : n.toFixed(1)} ${units[i]}`;
}

export function relativeTime(iso?: string, now: Date = new Date()): string {
  if (!iso) return "never";
  const then = new Date(iso);
  const seconds = Math.round((now.getTime() - then.getTime()) / 1000);
  if (!Number.isFinite(seconds)) return "—";
  // A clock skew that puts an event slightly in the future reads better as
  // "just now" than as "in 3 seconds".
  if (seconds < 5) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  if (days < 30) return `${days}d ago`;
  return then.toLocaleDateString();
}

export function absoluteTime(iso?: string): string {
  if (!iso) return "—";
  return new Date(iso).toLocaleString(undefined, {
    year: "numeric", month: "short", day: "numeric",
    hour: "2-digit", minute: "2-digit", second: "2-digit",
  });
}
