"use client";

/**
 * Replay of a recorded browser-terminal session.
 *
 * The recording is the exact byte stream the operator saw, so replay is just
 * writing it back into an xterm at the recorded pace. Seeking resets the
 * terminal and writes every event up to the target instantly — terminals are
 * stateful (cursor moves, clears, alternate screens), so there is no way to
 * jump into the middle of the stream without re-running what came before.
 *
 * Idle gaps are capped at two seconds for playback; the real timestamps are
 * still what the progress bar's tooltip and the audit log show.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { Terminal as XTerm } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { Button, ErrorNote, Icon, IconButton, Kbd, Modal, Segmented, Spinner } from "@/components/ui";
import { fetchRecording } from "@/lib/api/devices";
import { compressIdle, formatClock, parseCast, type Cast } from "@/lib/asciicast";

const SPEEDS = ["1", "2", "4", "8"] as const;
type Speed = (typeof SPEEDS)[number];

export function SessionPlayer({
  sessionId,
  title,
  subtitle,
  onClose,
}: {
  sessionId: string | null;
  title: string;
  subtitle?: string;
  onClose: () => void;
}) {
  const [cast, setCast] = useState<Cast | null>(null);
  const [raw, setRaw] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    if (!sessionId) return;
    let cancelled = false;
    setCast(null);
    setError(null);
    fetchRecording(sessionId)
      .then((text) => {
        if (cancelled) return;
        setRaw(text);
        setCast(parseCast(text));
      })
      .catch((err) => !cancelled && setError(err));
    return () => {
      cancelled = true;
    };
  }, [sessionId]);

  return (
    <Modal
      open={sessionId !== null}
      onClose={onClose}
      size="lg"
      title={title}
      description={subtitle}
      footer={
        <>
          {raw && (
            <Button
              variant="ghost"
              icon="download"
              onClick={() => {
                const url = URL.createObjectURL(new Blob([raw], { type: "application/x-asciicast" }));
                const a = document.createElement("a");
                a.href = url;
                a.download = `${sessionId}.cast`;
                a.click();
                URL.revokeObjectURL(url);
              }}
            >
              Download .cast
            </Button>
          )}
          <Button onClick={onClose}>Close</Button>
        </>
      }
    >
      {error ? (
        <ErrorNote error={error} />
      ) : !cast ? (
        <Spinner />
      ) : cast.events.length === 0 ? (
        <p className="py-8 text-center text-sm text-ink-muted">This session produced no output.</p>
      ) : (
        <Player cast={cast} />
      )}
    </Modal>
  );
}

function Player({ cast }: { cast: Cast }) {
  const holder = useRef<HTMLDivElement>(null);
  const term = useRef<XTerm | null>(null);
  const { times, duration } = useMemo(() => compressIdle(cast.events), [cast]);

  const [ready, setReady] = useState(false);
  const [playing, setPlaying] = useState(false);
  const [position, setPosition] = useState(0);
  const [speed, setSpeed] = useState<Speed>("1");

  // Playback state that the animation loop reads every frame without
  // re-rendering: the index of the next event to write, and the clock.
  const next = useRef(0);
  const clock = useRef({ at: 0, wall: 0 });
  const frame = useRef<number | null>(null);

  useEffect(() => {
    let disposed = false;
    void (async () => {
      const { Terminal } = await import("@xterm/xterm");
      if (disposed || !holder.current) return;
      const t = new Terminal({
        cols: cast.width,
        rows: cast.height,
        fontSize: 12,
        fontFamily: 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
        theme: { background: "#0b0e14", foreground: "#c8ccd4", cursor: "#e6b450" },
        disableStdin: true,
        cursorBlink: false,
        scrollback: 2000,
      });
      t.open(holder.current);
      term.current = t;
      setReady(true);
      setPlaying(true);
    })();
    return () => {
      disposed = true;
      if (frame.current) cancelAnimationFrame(frame.current);
      term.current?.dispose();
      term.current = null;
    };
  }, [cast]);

  /** Re-run the stream from the start up to `target` seconds, instantly. */
  const seek = useCallback(
    (target: number) => {
      const t = term.current;
      if (!t) return;
      t.reset();
      let i = 0;
      let chunk = "";
      while (i < times.length && times[i]! <= target) {
        chunk += cast.events[i]![1];
        i += 1;
      }
      if (chunk) t.write(chunk);
      next.current = i;
      clock.current = { at: target, wall: performance.now() };
      setPosition(target);
    },
    [cast, times],
  );

  useEffect(() => {
    if (!ready || !playing) return;
    clock.current = { at: position, wall: performance.now() };
    const rate = Number(speed);
    const tick = () => {
      const t = term.current;
      if (!t) return;
      const now = clock.current.at + ((performance.now() - clock.current.wall) / 1000) * rate;
      let chunk = "";
      while (next.current < times.length && times[next.current]! <= now) {
        chunk += cast.events[next.current]![1];
        next.current += 1;
      }
      if (chunk) t.write(chunk);
      setPosition(Math.min(now, duration));
      if (next.current >= times.length) {
        setPlaying(false);
        return;
      }
      frame.current = requestAnimationFrame(tick);
    };
    frame.current = requestAnimationFrame(tick);
    return () => {
      if (frame.current) cancelAnimationFrame(frame.current);
    };
    // `position` is read once to anchor the clock when playback (re)starts.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ready, playing, speed, times, cast, duration]);

  const toggle = useCallback(() => {
    if (!playing && position >= duration) seek(0);
    setPlaying((p) => !p);
  }, [playing, position, duration, seek]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === " ") {
        e.preventDefault();
        toggle();
      } else if (e.key === "ArrowRight") {
        seek(Math.min(duration, position + 5));
      } else if (e.key === "ArrowLeft") {
        seek(Math.max(0, position - 5));
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [toggle, seek, position, duration]);

  const progress = duration ? (position / duration) * 100 : 0;

  return (
    <div>
      <div className="overflow-auto rounded-xl bg-[#0b0e14] p-3">
        <div ref={holder} />
      </div>

      <div className="mt-4 flex items-center gap-3">
        <IconButton icon={playing ? "pause" : "play"} label={playing ? "Pause" : "Play"} variant="secondary" onClick={toggle} />
        <IconButton icon="skip-back" label="Restart" onClick={() => { seek(0); setPlaying(true); }} />
        <span className="w-24 shrink-0 text-xs tabular text-ink-muted">
          {formatClock(position)} / {formatClock(duration)}
        </span>
        <div
          role="slider"
          tabIndex={0}
          aria-label="Playback position"
          aria-valuemin={0}
          aria-valuemax={Math.round(duration)}
          aria-valuenow={Math.round(position)}
          className="group relative h-2 flex-1 cursor-pointer rounded-full bg-surface-hover"
          onClick={(e) => {
            const rect = e.currentTarget.getBoundingClientRect();
            seek(((e.clientX - rect.left) / rect.width) * duration);
          }}
        >
          <div className="h-full rounded-full bg-accent" style={{ width: `${progress}%` }} />
          <div
            className="absolute top-1/2 h-3.5 w-3.5 -translate-x-1/2 -translate-y-1/2 rounded-full border-2 border-white bg-accent shadow transition-transform group-hover:scale-110"
            style={{ left: `${progress}%` }}
          />
        </div>
        <Segmented<Speed>
          aria-label="Playback speed"
          size="sm"
          value={speed}
          onChange={setSpeed}
          options={SPEEDS.map((s) => ({ value: s, label: `${s}×` }))}
        />
      </div>
      <p className="mt-3 flex flex-wrap items-center gap-x-3 gap-y-1 text-2xs text-ink-faint">
        <span className="inline-flex items-center gap-1"><Kbd>Space</Kbd> play / pause</span>
        <span className="inline-flex items-center gap-1"><Kbd>←</Kbd><Kbd>→</Kbd> 5 seconds</span>
        <span className="inline-flex items-center gap-1"><Icon name="clock" size={11} /> Pauses longer than 2s are shortened</span>
      </p>
    </div>
  );
}
