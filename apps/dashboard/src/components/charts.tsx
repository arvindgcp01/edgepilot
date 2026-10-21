"use client";

/**
 * Charts, in plain SVG.
 *
 * Three forms: a time-series line with an optional area wash, a sparkline for
 * a table cell, and a meter for a percentage. No library — the product needs
 * exactly these, and the specs are fixed enough that a general charting
 * dependency would mostly be configuration to switch its defaults off.
 *
 * The rules these follow, so that every chart in the product reads as one:
 *   - lines are 2px, round-joined; the area wash is the hue at ~10%;
 *   - gridlines are one-step-off-surface hairlines, never dashed;
 *   - text wears text tokens, never the series colour — a colored key sits
 *     beside it instead;
 *   - a null point is a gap, drawn as a break, never bridged;
 *   - hover is a crosshair that snaps to the nearest X and reads every series.
 */
import { useEffect, useId, useMemo, useRef, useState } from "react";
import { cn } from "@/lib/cn";
import type { MetricPoint } from "@/lib/api/devices";

/** Validated categorical slots for the dark surface (blue, orange, aqua). A
 *  chart here never has more than three series. */
export const SERIES = ["#6366f1", "#f97316", "#10b981"] as const;

export type Series = { name: string; points: MetricPoint[]; color?: string };

export type Formatter = (v: number) => string;

export const fmtPercent: Formatter = (v) => `${Math.round(v)}%`;
export const fmtLoad: Formatter = (v) => v.toFixed(2);
export const fmtCelsius: Formatter = (v) => `${v.toFixed(1)}°C`;
export const fmtBytesPerSec: Formatter = (v) => {
  const units = ["B/s", "KiB/s", "MiB/s", "GiB/s"];
  let n = v;
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n >= 10 || i === 0 ? Math.round(n) : n.toFixed(1)} ${units[i]}`;
};
export const fmtCount: Formatter = (v) => Math.round(v).toLocaleString();

function useWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null);
  const [width, setWidth] = useState(0);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const observer = new ResizeObserver(([entry]) => {
      if (entry) setWidth(entry.contentRect.width);
    });
    observer.observe(el);
    setWidth(el.getBoundingClientRect().width);
    return () => observer.disconnect();
  }, []);
  return { ref, width };
}

/** Clean tick values: 0 / 25 / 50 / 75 / 100 rather than 0 / 23.7 / 47.4. */
function niceTicks(max: number, count = 4): number[] {
  if (max <= 0) return [0];
  const rough = max / count;
  const magnitude = 10 ** Math.floor(Math.log10(rough));
  const residual = rough / magnitude;
  const step = (residual >= 5 ? 10 : residual >= 2 ? 5 : residual >= 1 ? 2 : 1) * magnitude;
  const ticks: number[] = [];
  for (let v = 0; v <= max + step * 0.001; v += step) ticks.push(Number(v.toFixed(10)));
  return ticks;
}

function timeLabel(ms: number, spanMs: number): string {
  const d = new Date(ms);
  if (spanMs > 3 * 24 * 3600_000) {
    return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }
  if (spanMs > 24 * 3600_000) {
    return d.toLocaleString(undefined, { weekday: "short", hour: "2-digit", minute: "2-digit", hour12: false });
  }
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false });
}

/**
 * A time-series line chart.
 *
 * `max` fixes the y-axis top (100 for a percentage) so two charts of the same
 * kind are comparable; without it the axis fits the data with clean ticks.
 */
export function LineChart({
  series,
  format,
  max,
  height = 160,
  area = true,
  from,
  to,
  className,
  emptyLabel = "No readings in this window",
}: {
  series: Series[];
  format: Formatter;
  max?: number;
  height?: number;
  area?: boolean;
  from: number;
  to: number;
  className?: string;
  emptyLabel?: string;
}) {
  const { ref, width } = useWidth<HTMLDivElement>();
  const id = useId();
  const [hoverX, setHoverX] = useState<number | null>(null);

  const pad = { top: 10, right: 12, bottom: 22, left: 40 };
  const innerW = Math.max(0, width - pad.left - pad.right);
  const innerH = height - pad.top - pad.bottom;

  const dataMax = useMemo(() => {
    let m = 0;
    for (const s of series) for (const [, v] of s.points) if (v !== null && v > m) m = v;
    return m;
  }, [series]);
  const ticks = useMemo(() => niceTicks(max ?? (dataMax > 0 ? dataMax * 1.1 : 1)), [max, dataMax]);
  const yMax = ticks[ticks.length - 1] ?? 1;

  const x = (ms: number) => pad.left + ((ms - from) / Math.max(1, to - from)) * innerW;
  const y = (v: number) => pad.top + innerH - (Math.min(v, yMax) / yMax) * innerH;

  // One path per contiguous run; a null point ends a run, which is the gap.
  const paths = useMemo(
    () =>
      series.map((s) => {
        const segments: string[] = [];
        let d = "";
        for (const [ms, v] of s.points) {
          if (v === null) {
            if (d) segments.push(d);
            d = "";
            continue;
          }
          d += `${d ? "L" : "M"}${x(ms).toFixed(1)},${y(v).toFixed(1)}`;
        }
        if (d) segments.push(d);
        return segments;
      }),
    // x/y are derived from these.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [series, from, to, innerW, innerH, yMax],
  );

  const hasData = dataMax > 0 || series.some((s) => s.points.some(([, v]) => v !== null));

  // Snap the pointer to the nearest x across every series.
  const hover = useMemo(() => {
    if (hoverX === null || innerW === 0) return null;
    const ms = from + ((hoverX - pad.left) / innerW) * (to - from);
    let bestMs: number | null = null;
    let bestDist = Infinity;
    for (const s of series) {
      for (const [pms] of s.points) {
        const dist = Math.abs(pms - ms);
        if (dist < bestDist) {
          bestDist = dist;
          bestMs = pms;
        }
      }
    }
    if (bestMs === null) return null;
    const values = series.map((s) => {
      const point = s.points.find(([pms]) => pms === bestMs);
      return { name: s.name, color: s.color, value: point ? point[1] : null };
    });
    return { ms: bestMs, x: x(bestMs), values };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hoverX, series, from, to, innerW]);

  const xTicks = useMemo(() => {
    const n = width < 420 ? 3 : 5;
    return Array.from({ length: n }, (_, i) => from + ((to - from) * i) / (n - 1));
  }, [from, to, width]);

  return (
    <div ref={ref} className={cn("relative w-full select-none", className)} style={{ height }}>
      {width > 0 && (
        <svg
          width={width}
          height={height}
          role="img"
          aria-label={series.map((s) => s.name).join(", ")}
          onPointerMove={(e) => {
            const rect = e.currentTarget.getBoundingClientRect();
            setHoverX(e.clientX - rect.left);
          }}
          onPointerLeave={() => setHoverX(null)}
        >
          <defs>
            {series.map((s, i) => (
              <linearGradient key={s.name} id={`${id}-${i}`} x1="0" x2="0" y1="0" y2="1">
                <stop offset="0%" stopColor={s.color ?? SERIES[i % SERIES.length]} stopOpacity={0.18} />
                <stop offset="100%" stopColor={s.color ?? SERIES[i % SERIES.length]} stopOpacity={0.02} />
              </linearGradient>
            ))}
          </defs>

          {ticks.map((t) => (
            <g key={t}>
              <line x1={pad.left} x2={pad.left + innerW} y1={y(t)} y2={y(t)} className="stroke-surface-border" strokeWidth={1} />
              <text x={pad.left - 6} y={y(t)} textAnchor="end" dominantBaseline="middle" fontSize={10} className="tabular fill-ink-faint">
                {format(t)}
              </text>
            </g>
          ))}
          {xTicks.map((t, i) => (
            <text
              key={t}
              x={x(t)}
              y={height - 6}
              textAnchor={i === 0 ? "start" : i === xTicks.length - 1 ? "end" : "middle"}
              fontSize={10}
              className="fill-ink-faint"
            >
              {timeLabel(t, to - from)}
            </text>
          ))}

          {hasData &&
            series.map((s, i) => {
              const color = s.color ?? SERIES[i % SERIES.length];
              return (
                <g key={s.name}>
                  {area &&
                    series.length === 1 &&
                    paths[i]?.map((d, j) => {
                      // Close each run down to the baseline for the wash.
                      const first = d.match(/^M([\d.]+),/)?.[1];
                      const last = d.match(/([\d.]+),[\d.]+$/)?.[1];
                      if (!first || !last) return null;
                      return (
                        <path
                          key={j}
                          d={`${d}L${last},${pad.top + innerH}L${first},${pad.top + innerH}Z`}
                          fill={`url(#${id}-${i})`}
                          stroke="none"
                        />
                      );
                    })}
                  {paths[i]?.map((d, j) => (
                    <path key={j} d={d} fill="none" stroke={color} strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
                  ))}
                </g>
              );
            })}

          {hover && (
            <g>
              <line x1={hover.x} x2={hover.x} y1={pad.top} y2={pad.top + innerH} className="stroke-ink-faint" strokeWidth={1} />
              {hover.values.map((v, i) =>
                v.value === null ? null : (
                  <circle
                    key={v.name}
                    cx={hover.x}
                    cy={y(v.value)}
                    r={4}
                    fill={v.color ?? SERIES[i % SERIES.length]}
                    className="stroke-surface-raised"
                    strokeWidth={2}
                  />
                ),
              )}
            </g>
          )}
        </svg>
      )}

      {!hasData && width > 0 && (
        <div className="pointer-events-none absolute inset-0 flex items-center justify-center text-xs text-ink-faint">
          {emptyLabel}
        </div>
      )}

      {hover && (
        <div
          role="tooltip"
          className="pointer-events-none absolute top-1 z-10 rounded-md border border-surface-border bg-surface-overlay px-2.5 py-1.5 text-xs shadow-overlay"
          style={{
            left: hover.x + 12 + 180 > width ? hover.x - 12 - 180 : hover.x + 12,
            width: 180,
          }}
        >
          <div className="mb-1 text-ink-faint">
            {new Date(hover.ms).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false })}
          </div>
          {hover.values.map((v, i) => (
            <div key={v.name} className="flex items-center justify-between gap-3 py-px">
              <span className="flex items-center gap-1.5 text-ink-muted">
                <span className="inline-block h-0.5 w-3 rounded" style={{ background: v.color ?? SERIES[i % SERIES.length] }} aria-hidden />
                {v.name}
              </span>
              <span className="font-medium tabular text-ink">{v.value === null ? "—" : format(v.value)}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

/** Legend for charts with more than one series. Single-series charts have
 *  none — the title already names the line. */
export function Legend({ series }: { series: Series[] }) {
  if (series.length < 2) return null;
  return (
    <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-ink-muted">
      {series.map((s, i) => (
        <span key={s.name} className="inline-flex items-center gap-1.5">
          <span className="inline-block h-0.5 w-3 rounded" style={{ background: s.color ?? SERIES[i % SERIES.length] }} aria-hidden />
          {s.name}
        </span>
      ))}
    </div>
  );
}

/** A 12-to-60 point trend in a table cell: no axes, no labels, a wash. */
export function Sparkline({
  points,
  color = SERIES[0],
  max,
  width = 96,
  height = 24,
  className,
}: {
  points: MetricPoint[];
  color?: string;
  max?: number;
  width?: number;
  height?: number;
  className?: string;
}) {
  const id = useId();
  const values = points.map(([, v]) => v);
  const top = max ?? Math.max(1, ...values.filter((v): v is number => v !== null)) * 1.1;
  if (points.length < 2) return <span className={cn("inline-block", className)} style={{ width, height }} />;
  const first = points[0]?.[0] ?? 0;
  const last = points[points.length - 1]?.[0] ?? 1;
  const x = (ms: number) => ((ms - first) / Math.max(1, last - first)) * (width - 2) + 1;
  const y = (v: number) => height - 2 - (Math.min(v, top) / top) * (height - 4);
  let d = "";
  const runs: string[] = [];
  for (const [ms, v] of points) {
    if (v === null) {
      if (d) runs.push(d);
      d = "";
      continue;
    }
    d += `${d ? "L" : "M"}${x(ms).toFixed(1)},${y(v).toFixed(1)}`;
  }
  if (d) runs.push(d);
  return (
    <svg width={width} height={height} className={cn("shrink-0", className)} aria-hidden>
      <defs>
        <linearGradient id={id} x1="0" x2="0" y1="0" y2="1">
          <stop offset="0%" stopColor={color} stopOpacity={0.22} />
          <stop offset="100%" stopColor={color} stopOpacity={0.02} />
        </linearGradient>
      </defs>
      {runs.map((run, i) => {
        const startX = run.match(/^M([\d.]+),/)?.[1];
        const endX = run.match(/([\d.]+),[\d.]+$/)?.[1];
        return (
          <g key={i}>
            {startX && endX && <path d={`${run}L${endX},${height - 1}L${startX},${height - 1}Z`} fill={`url(#${id})`} />}
            <path d={run} fill="none" stroke={color} strokeWidth={1.5} strokeLinejoin="round" strokeLinecap="round" />
          </g>
        );
      })}
    </svg>
  );
}

/**
 * A percentage as a bar. The fill carries severity — accent, then warning past
 * 80, then danger past 95 — and the track is a darker step of the same ramp so
 * the state reads across the whole bar. Null renders as an empty track with a
 * dash, because "not measured" and "0%" are different facts.
 */
export function Meter({
  value,
  label,
  className,
  width = "w-16",
  showValue = true,
}: {
  value: number | null | undefined;
  label: string;
  className?: string;
  width?: string;
  showValue?: boolean;
}) {
  const v = value === null || value === undefined ? null : Math.max(0, Math.min(100, value));
  const tone = v === null ? "" : v >= 95 ? "bg-danger" : v >= 80 ? "bg-degraded" : "bg-accent";
  return (
    <span
      className={cn("inline-flex items-center gap-2", className)}
      title={v === null ? `${label}: not reported` : `${label}: ${v.toFixed(1)}%`}
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={v ?? undefined}
    >
      <span className={cn("h-1.5 overflow-hidden rounded-full bg-surface-border", width)} aria-hidden>
        {v !== null && <span className={cn("block h-full rounded-full", tone)} style={{ width: `${v}%` }} />}
      </span>
      {showValue && (
        <span className={cn("w-9 text-right text-xs tabular", v === null ? "text-ink-faint" : "text-ink-muted")}>
          {v === null ? "—" : `${Math.round(v)}%`}
        </span>
      )}
    </span>
  );
}

/** Seconds as "3d 4h" / "2h 05m" / "45s". */
export function formatUptime(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || seconds <= 0) return "—";
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${String(m).padStart(2, "0")}m`;
  if (m > 0) return `${m}m`;
  return `${Math.floor(seconds)}s`;
}
