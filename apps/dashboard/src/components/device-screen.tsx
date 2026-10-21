"use client";

/**
 * The device's desktop, in the browser.
 *
 * This mounts noVNC against the server's screen relay. The relay has already
 * authenticated to the Mac and presents this connection as "no auth", so noVNC
 * connects with no credentials — the VNC password lives on the server and the
 * agent, never here. What crosses this socket is the framebuffer one way and
 * keyboard and pointer events the other.
 *
 * noVNC touches `window` and `document` at import time and cannot be in a
 * server-rendered module graph, so it is imported dynamically inside the effect
 * — the same reason xterm is, and it keeps noVNC out of the bundle for the many
 * operators who never open a screen.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { API_BASE, currentAccessToken } from "@/lib/api/client";
import { Button, Icon, Switch, Tooltip, useToast } from "@/components/ui";
import { DeviceFiles } from "@/components/device-files";
import { cn } from "@/lib/cn";

type Status = "connecting" | "connected" | "disconnected";

// The noVNC RFB surface we touch. Declared locally rather than pulled from the
// package's types, which are not shipped in a form Next resolves cleanly.
type RFBInstance = {
  viewOnly: boolean;
  scaleViewport: boolean;
  showDotCursor: boolean;
  clipboardPasteFrom: (text: string) => void;
  addEventListener: (event: string, handler: (e: CustomEvent) => void) => void;
  focus: (options?: { preventScroll?: boolean }) => void;
  disconnect: () => void;
};

export function DeviceScreen({
  deviceId,
  hostname,
  onClose,
  /** True when this is already its own pop-out window: the "open in new window"
   *  control is hidden, and closing is the browser's job, not a button. */
  standalone = false,
}: {
  deviceId: string;
  hostname: string;
  onClose?: () => void;
  standalone?: boolean;
}) {
  const root = useRef<HTMLDivElement | null>(null);
  const holder = useRef<HTMLDivElement | null>(null);
  const rfb = useRef<RFBInstance | null>(null);
  const [status, setStatus] = useState<Status>("connecting");
  const [note, setNote] = useState<string | null>(null);
  const [viewOnly, setViewOnly] = useState(false);
  const [fullscreen, setFullscreen] = useState(false);
  const [filesOpen, setFilesOpen] = useState(false);
  const { push } = useToast();

  const toggleViewOnly = useCallback((next: boolean) => {
    setViewOnly(next);
    if (rfb.current) rfb.current.viewOnly = next;
  }, []);

  const toggleFullscreen = useCallback(() => {
    // Fullscreen the whole panel — toolbar included — so the operator keeps the
    // View-only toggle and the connection status while filling the display.
    if (document.fullscreenElement) {
      void document.exitFullscreen();
    } else {
      void root.current?.requestFullscreen();
    }
  }, []);

  useEffect(() => {
    const onChange = () => setFullscreen(Boolean(document.fullscreenElement));
    document.addEventListener("fullscreenchange", onChange);
    return () => document.removeEventListener("fullscreenchange", onChange);
  }, []);

  const sendClipboard = useCallback(async () => {
    // Local → remote: browsers do not let a page read the clipboard on a
    // keypress it cannot see, so Cmd/Ctrl+V into the canvas cannot be trusted
    // to carry text. An explicit button reads the clipboard under a real user
    // gesture and pushes it to the device, where the operator then pastes.
    try {
      const text = await navigator.clipboard.readText();
      if (!text) {
        push({ title: "Your clipboard is empty", tone: "warning" });
        return;
      }
      rfb.current?.clipboardPasteFrom(text);
      push({ title: "Clipboard sent to the device", description: "Paste on the remote screen with ⌘V.", tone: "success" });
    } catch {
      push({ title: "Could not read your clipboard", description: "Allow clipboard access for this site, then try again.", tone: "error" });
    }
  }, [push]);

  const openInWindow = useCallback(() => {
    // A named window, so a second click focuses the existing pop-out rather
    // than stacking a new one. The route re-authenticates from the refresh
    // cookie, so the new window needs nothing carried across to it.
    window.open(
      `/screen/${deviceId}`,
      `edgepilot-screen-${deviceId}`,
      "width=1280,height=832,menubar=no,toolbar=no,location=no,status=no",
    );
    onClose?.();
  }, [deviceId, onClose]);

  useEffect(() => {
    if (!holder.current) return;
    let disposed = false;

    void (async () => {
      const { default: RFB } = await import("@novnc/novnc/lib/rfb.js");
      if (disposed || !holder.current) return;

      const token = currentAccessToken();
      const base = API_BASE || window.location.origin;
      const url = new URL(`${base}/api/v1/devices/${deviceId}/screen`);
      url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
      if (token) url.searchParams.set("access_token", token);

      const instance = new RFB(holder.current, url.toString(), {
        wsProtocols: ["binary"],
      }) as unknown as RFBInstance;
      instance.scaleViewport = true;
      // Always draw a cursor. macOS Screen Sharing often sends the pointer
      // position without a cursor shape noVNC can render, which leaves the
      // operator moving an invisible pointer — visible clicks, no visible
      // cursor. A dot at the pointer is the reliable fallback.
      instance.showDotCursor = true;
      instance.viewOnly = viewOnly;
      rfb.current = instance;

      instance.addEventListener("connect", () => {
        if (disposed) return;
        setStatus("connected");
        // Capture the keyboard immediately. Without an explicit focus, noVNC
        // forwards pointer events (which work on hover) but not keystrokes
        // until the canvas is clicked — which reads as "the mouse works but I
        // can't type", the usual "can't control" report.
        try {
          instance.focus();
        } catch {
          // Older noVNC without focus(): the click handler covers it.
        }
      });
      // Remote → local: when something is copied on the Mac, RFB delivers the
      // text here. Mirror it into the operator's own clipboard, best-effort —
      // the browser may refuse without focus or permission, and that is fine.
      instance.addEventListener("clipboard", (e: CustomEvent<{ text?: string }>) => {
        const text = e.detail?.text;
        if (text) void navigator.clipboard?.writeText(text).catch(() => {});
      });
      instance.addEventListener("disconnect", (e: CustomEvent<{ clean: boolean }>) => {
        if (disposed) return;
        setStatus("disconnected");
        setNote(e.detail?.clean ? "The session ended." : "The connection was lost.");
      });
      instance.addEventListener("securityfailure", (e: CustomEvent<{ reason?: string }>) => {
        if (disposed) return;
        setStatus("disconnected");
        setNote(e.detail?.reason || "The device refused the screen connection.");
      });
    })();

    return () => {
      disposed = true;
      try {
        rfb.current?.disconnect();
      } catch {
        // Already gone.
      }
      rfb.current = null;
    };
    // viewOnly is applied live via the ref; it must not re-open the socket.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [deviceId]);

  return (
    <div ref={root} className="flex h-full flex-col bg-surface-raised">
      <div className="flex flex-wrap items-center gap-3 border-b border-surface-border px-4 py-2">
        <div className="flex items-center gap-2">
          <Icon name="monitor" size={15} className="text-ink-muted" />
          <span className="text-sm font-medium text-ink">{hostname}</span>
          <span
            className={cn(
              "inline-flex items-center gap-1.5 text-xs",
              status === "connected" ? "text-online" : status === "connecting" ? "text-degraded" : "text-ink-faint",
            )}
          >
            <span className={cn("h-1.5 w-1.5 rounded-full bg-current", status === "connecting" && "animate-pulse")} aria-hidden />
            {status === "connected" ? "Live" : status === "connecting" ? "Connecting" : "Ended"}
          </span>
        </div>
        <div className="ml-auto flex items-center gap-2">
          <Switch checked={viewOnly} onChange={toggleViewOnly} label="View only" />
          <span className="mx-1 h-5 w-px bg-surface-border" aria-hidden />
          <Tooltip label="Send your clipboard to the device">
            <Button size="sm" variant="ghost" icon="copy" onClick={() => void sendClipboard()}>
              <span className="sr-only">Send clipboard to device</span>
            </Button>
          </Tooltip>
          <Tooltip label="Browse and transfer files">
            <Button size="sm" variant="ghost" icon="file-text" onClick={() => setFilesOpen(true)}>
              <span className="sr-only">Files</span>
            </Button>
          </Tooltip>
          <Tooltip label={fullscreen ? "Exit fullscreen" : "Fullscreen"}>
            <Button size="sm" variant="ghost" icon={fullscreen ? "minimize" : "maximize"} onClick={toggleFullscreen}>
              <span className="sr-only">{fullscreen ? "Exit fullscreen" : "Fullscreen"}</span>
            </Button>
          </Tooltip>
          {!standalone && (
            <Tooltip label="Open in a new window">
              <Button size="sm" variant="ghost" icon="external" onClick={openInWindow}>
                <span className="sr-only">Open in a new window</span>
              </Button>
            </Tooltip>
          )}
          {onClose && !standalone && (
            <Button size="sm" variant="ghost" icon="x" onClick={onClose}>
              Close
            </Button>
          )}
        </div>
      </div>

      <div className="relative min-h-0 flex-1 bg-surface-sunken">
        <div
          ref={holder}
          tabIndex={0}
          className="h-full w-full outline-none"
          onPointerDown={() => {
            try {
              rfb.current?.focus();
            } catch {
              // No-op if not connected yet.
            }
          }}
        />
        {status !== "connected" && (
          <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center gap-2 text-sm text-ink-muted">
            {status === "connecting" ? (
              <>
                <span className="h-5 w-5 animate-spin rounded-full border-2 border-surface-border border-t-accent" aria-hidden />
                Connecting to the desktop…
              </>
            ) : (
              <>
                <Icon name="monitor" className="text-ink-faint" />
                {note ?? "The session ended."}
              </>
            )}
          </div>
        )}
      </div>

      <div className="border-t border-surface-border px-4 py-1.5 text-2xs text-ink-faint">
        Remote control of a customer desktop. The macOS menu bar shows a screen-sharing indicator while you are connected.
        {viewOnly ? " You are watching; input is disabled." : " Your keyboard and pointer control the machine — click the screen if typing does not register."}
      </div>

      <DeviceFiles deviceId={deviceId} hostname={hostname} open={filesOpen} onClose={() => setFilesOpen(false)} />
    </div>
  );
}
