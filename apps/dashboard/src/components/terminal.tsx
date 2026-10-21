"use client";

/**
 * A shell on a device, in the browser.
 *
 * # What is on the wire
 *
 * Binary frames are terminal bytes, in both directions. Text frames are control
 * messages, currently only `{"type":"resize"}`. Keeping them in different frame
 * types means a resize can never be mistaken for something the operator typed,
 * and terminal output never has to survive a round trip through UTF-8 — half a
 * multi-byte character split across two reads would not.
 *
 * # What this is not
 *
 * It is not the same trust model as `ssh -J`. The server decrypts this session
 * in order to draw it, and it is recorded. The banner says so, every time,
 * because an operator should not have to remember which of two paths they are
 * on.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { API_BASE, currentAccessToken } from "@/lib/api/client";

// The stylesheet is imported statically because a dynamic import of CSS has no
// type declaration. It costs nothing that matters — this is a few kilobytes of
// CSS, and the xterm *runtime* is still loaded lazily below, which is the part
// worth keeping out of the bundle.
import "@xterm/xterm/css/xterm.css";

type Status = "connecting" | "open" | "closed";

export function DeviceTerminal({
  deviceId,
  hostname,
  onClose,
}: {
  deviceId: string;
  hostname: string;
  onClose?: () => void;
}) {
  const holder = useRef<HTMLDivElement | null>(null);
  const socket = useRef<WebSocket | null>(null);
  const [status, setStatus] = useState<Status>("connecting");
  const [note, setNote] = useState<string | null>(null);

  const disconnect = useCallback(() => {
    socket.current?.close();
    socket.current = null;
  }, []);

  useEffect(() => {
    if (!holder.current) return;

    let disposed = false;
    let cleanup: (() => void) | undefined;

    // xterm touches `window` and `document` at import time, so it cannot be in
    // the module graph of a server-rendered page. Importing it here also keeps
    // it out of the bundle for operators who never open a terminal.
    void (async () => {
      const [{ Terminal }, { FitAddon }] = await Promise.all([
        import("@xterm/xterm"),
        import("@xterm/addon-fit"),
      ]);
      if (disposed || !holder.current) return;

      const term = new Terminal({
        cursorBlink: true,
        fontSize: 13,
        fontFamily:
          'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
        // Matches the dashboard's surface colours rather than xterm's defaults,
        // so the terminal reads as part of the page and not an embedded island.
        theme: {
          background: "#0b0e14",
          foreground: "#c8ccd4",
          cursor: "#e6b450",
          selectionBackground: "#2d3343",
        },
        scrollback: 5000,
      });

      const fit = new FitAddon();
      term.loadAddon(fit);
      term.open(holder.current);
      fit.fit();

      const token = currentAccessToken();
      // The API is same-origin behind Caddy, so an empty API_BASE means "here".
      const base = API_BASE || window.location.origin;
      const url = new URL(`${base}/api/v1/devices/${deviceId}/terminal`);
      url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
      if (token) url.searchParams.set("access_token", token);

      const ws = new WebSocket(url.toString());
      ws.binaryType = "arraybuffer";
      socket.current = ws;

      const sendResize = () => {
        fit.fit();
        if (ws.readyState === WebSocket.OPEN) {
          ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
        }
      };

      ws.onopen = () => {
        setStatus("open");
        sendResize();
        term.focus();
      };

      ws.onmessage = (event) => {
        if (typeof event.data === "string") return;
        term.write(new Uint8Array(event.data));
      };

      ws.onclose = (event) => {
        setStatus("closed");
        // `reason` carries the server's own explanation — "the shell exited",
        // "that device is not currently connected" — which is more use than a
        // numeric close code.
        setNote(event.reason || "the session ended");
        term.write("\r\n\x1b[90m— session ended —\x1b[0m\r\n");
      };

      ws.onerror = () => {
        setStatus("closed");
        setNote("the connection failed");
      };

      const typed = term.onData((data) => {
        if (ws.readyState !== WebSocket.OPEN) return;
        ws.send(new TextEncoder().encode(data));
      });

      const observer = new ResizeObserver(() => sendResize());
      observer.observe(holder.current);

      cleanup = () => {
        typed.dispose();
        observer.disconnect();
        ws.close();
        term.dispose();
      };
    })();

    return () => {
      disposed = true;
      cleanup?.();
    };
  }, [deviceId]);

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center justify-between border-b border-surface-border px-3 py-2">
        <div className="flex items-center gap-2 text-sm">
          <span
            className={
              status === "open"
                ? "h-1.5 w-1.5 rounded-full bg-online"
                : status === "connecting"
                  ? "h-1.5 w-1.5 rounded-full bg-pending"
                  : "h-1.5 w-1.5 rounded-full bg-offline"
            }
            aria-hidden
          />
          <span className="font-medium">{hostname}</span>
          <span className="text-xs text-ink-faint">
            {status === "connecting" ? "connecting…" : note ?? "connected"}
          </span>
        </div>

        <div className="flex items-center gap-3">
          {/* Stated in the UI, not only in the docs. This path is not the
              end-to-end one, and an operator should be able to see that
              without remembering which button they pressed. */}
          <span
            className="text-xs text-ink-faint"
            title="Unlike ssh -J, the server decrypts this session in order to render it, and records it."
          >
            recorded · server-side
          </span>
          <button
            onClick={() => {
              disconnect();
              onClose?.();
            }}
            className="rounded-md px-2 py-1 text-xs text-ink-muted hover:bg-surface-border hover:text-ink"
          >
            Close
          </button>
        </div>
      </div>

      <div ref={holder} className="min-h-0 flex-1 bg-[#0b0e14] p-2" />
    </div>
  );
}
