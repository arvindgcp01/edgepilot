/**
 * The realtime connection.
 *
 * # Why the token goes in the URL and not a header
 *
 * The browser's WebSocket API cannot set request headers. The access token is
 * therefore a query parameter, which is a real trade: query strings turn up in
 * proxy logs in a way headers do not.
 *
 * It is acceptable here because the access token is short-lived (fifteen
 * minutes) and is not the credential worth stealing — the refresh cookie is,
 * and it stays HttpOnly and never appears in a URL. Over TLS the parameter is
 * not visible on the wire; the exposure is to whatever logs the request line at
 * the edge.
 *
 * # What arrives
 *
 * Events say *that* something changed, never enough to act on. The dashboard
 * refetches. That is what makes a stale or replayed frame harmless.
 */
import { API_BASE, currentAccessToken } from "./api/client";

export type FleetEvent = {
  type: string;
  device_id?: string;
  data?: unknown;
  at: string;
};

export type EventStreamOptions = {
  topics: string[];
  onEvent: (event: FleetEvent) => void;
  onStatus?: (connected: boolean) => void;
};

/** Full jitter, for the same reason the agent uses it: a server restart must
 *  not bring every open dashboard back in the same millisecond. */
function backoff(attempt: number): number {
  const window = Math.min(30_000, 1000 * 2 ** attempt);
  return Math.random() * window;
}

export function connectEvents(options: EventStreamOptions): () => void {
  let socket: WebSocket | null = null;
  let attempt = 0;
  let closed = false;
  let timer: ReturnType<typeof setTimeout> | null = null;

  const open = () => {
    if (closed) return;

    const token = currentAccessToken();
    if (!token) {
      // No session yet. Try again shortly rather than opening a socket that
      // will only be rejected.
      timer = setTimeout(open, 1000);
      return;
    }

    // API_BASE is empty when the API is same-origin, and `new URL` throws on a
    // relative path with no base. The page's own origin is that base.
    const url = new URL(`${API_BASE}/api/v1/ws`, window.location.origin);
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
    url.searchParams.set("access_token", token);

    socket = new WebSocket(url.toString());

    socket.onopen = () => {
      attempt = 0;
      options.onStatus?.(true);
      socket?.send(JSON.stringify({ type: "subscribe", topics: options.topics }));
    };

    socket.onmessage = (message) => {
      try {
        const frame = JSON.parse(message.data as string) as FleetEvent;
        // Control frames are the server answering our own subscribe; they are
        // not fleet events and must not reach the UI as though they were.
        if (["subscribed", "subscription.refused", "pong", "error"].includes(frame.type)) {
          return;
        }
        options.onEvent(frame);
      } catch {
        // A frame we cannot parse is a frame we cannot act on. Dropping it is
        // right; tearing down the connection over it is not.
      }
    };

    socket.onclose = () => {
      options.onStatus?.(false);
      if (closed) return;
      timer = setTimeout(open, backoff(attempt++));
    };

    socket.onerror = () => socket?.close();
  };

  open();

  return () => {
    closed = true;
    if (timer) clearTimeout(timer);
    socket?.close();
  };
}
