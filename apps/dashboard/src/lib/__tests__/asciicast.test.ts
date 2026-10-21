import { describe, expect, it } from "vitest";
import { compressIdle, formatClock, parseCast } from "@/lib/asciicast";

const cast = [
  JSON.stringify({ version: 2, width: 100, height: 30, timestamp: 1700000000 }),
  JSON.stringify([0.5, "o", "hello "]),
  JSON.stringify([0.1, "o", "first "]),
  JSON.stringify([1.0, "i", "typed"]),
  JSON.stringify([12.0, "o", "after a long pause"]),
  '[12.5, "o", "trunc', // killed mid-write
].join("\n");

describe("parseCast", () => {
  it("keeps output events, in time order, and survives a truncated tail", () => {
    const c = parseCast(cast);
    expect(c.width).toBe(100);
    expect(c.height).toBe(30);
    expect(c.startedAt?.getTime()).toBe(1700000000 * 1000);
    expect(c.events.map((e) => e[1])).toEqual(["first ", "hello ", "after a long pause"]);
    expect(c.duration).toBe(12);
  });

  it("refuses what it cannot replay", () => {
    expect(() => parseCast("")).toThrow(/empty/);
    expect(() => parseCast("not json")).toThrow(/header/);
    expect(() => parseCast(JSON.stringify({ version: 1 }))).toThrow(/version 1/);
  });
});

describe("compressIdle", () => {
  it("caps long gaps so an idle session replays quickly", () => {
    const { times, duration } = compressIdle([[0.1, "a"], [0.5, "b"], [12, "c"]], 2);
    expect(times).toEqual([0.1, 0.5, 2.5]);
    expect(duration).toBe(2.5);
  });
});

describe("formatClock", () => {
  it("reads like a media player", () => {
    expect(formatClock(5)).toBe("0:05");
    expect(formatClock(75)).toBe("1:15");
    expect(formatClock(3725)).toBe("1:02:05");
  });
});
