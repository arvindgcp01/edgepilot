// noVNC ships no bundled types Next resolves cleanly. We touch a small surface
// of the RFB class — construct it, set two properties, subscribe to events,
// disconnect — so this declares exactly that, and no more, rather than pulling
// in an inaccurate community type.
declare module "@novnc/novnc/lib/rfb.js" {
  export default class RFB {
    constructor(
      target: HTMLElement,
      url: string,
      options?: { wsProtocols?: string[]; credentials?: { password?: string } },
    );
    viewOnly: boolean;
    scaleViewport: boolean;
    addEventListener(event: string, handler: (e: CustomEvent) => void): void;
    removeEventListener(event: string, handler: (e: CustomEvent) => void): void;
    disconnect(): void;
  }
}
