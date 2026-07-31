import { afterEach, describe, expect, it, vi } from "vitest";
import { connectEvents } from "@/lib/events";

class FakeSocket {
  static last: FakeSocket | null = null;
  readonly url: string;
  onopen: (() => void) | null = null;
  onmessage: ((e: { data: string }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  sent: string[] = [];

  constructor(url: string) {
    this.url = url;
    FakeSocket.last = this;
  }
  send(data: string) { this.sent.push(data); }
  close() {}
}

afterEach(() => vi.unstubAllGlobals());

describe("the event stream", () => {
  it("builds an absolute URL when the API is same-origin", async () => {
    // API_BASE is empty for a same-origin deployment, and `new URL("/api/v1/ws")`
    // throws without a base — which took the whole fleet page down with
    // "Failed to construct 'URL': Invalid URL" rather than degrading to no live
    // updates.
    vi.stubGlobal("WebSocket", FakeSocket);
    const { signIn } = await import("@/lib/api/client");
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(
      new Response(JSON.stringify({
        access_token: "t", token_type: "Bearer", expires_in: 900,
        principal: { user_id: "u", org_id: "o", email: "a@b.c", roles: [], permissions: [] },
      }), { status: 200, headers: { "Content-Type": "application/json" } }),
    ));
    await signIn("a@b.c", "pw");

    const disconnect = connectEvents({ topics: ["fleet"], onEvent: () => {} });

    expect(FakeSocket.last).not.toBeNull();
    expect(FakeSocket.last?.url).toMatch(/^wss?:\/\/[^/]+\/api\/v1\/ws\?access_token=/);
    disconnect();
  });

  it("subscribes on open and ignores control frames", async () => {
    vi.stubGlobal("WebSocket", FakeSocket);
    const events: unknown[] = [];
    const disconnect = connectEvents({ topics: ["fleet"], onEvent: (e) => events.push(e) });

    FakeSocket.last?.onopen?.();
    expect(FakeSocket.last?.sent[0]).toContain('"subscribe"');

    // The server's own acknowledgements are not fleet events; delivering them
    // as such would trigger a refetch for every subscribe.
    FakeSocket.last?.onmessage?.({ data: JSON.stringify({ type: "subscribed", topics: ["fleet"] }) });
    FakeSocket.last?.onmessage?.({ data: JSON.stringify({ type: "pong" }) });
    expect(events).toHaveLength(0);

    FakeSocket.last?.onmessage?.({ data: JSON.stringify({ type: "device.status", device_id: "d" }) });
    expect(events).toHaveLength(1);

    disconnect();
  });

  it("survives a frame it cannot parse", async () => {
    vi.stubGlobal("WebSocket", FakeSocket);
    const disconnect = connectEvents({ topics: ["fleet"], onEvent: () => {} });
    // A frame we cannot read is a frame we cannot act on. Dropping it is right;
    // tearing down the connection over it is not.
    expect(() => FakeSocket.last?.onmessage?.({ data: "not json" })).not.toThrow();
    disconnect();
  });
});
