import { describe, expect, it } from "vitest";
import { bytes, relativeTime } from "@/lib/format";

describe("bytes", () => {
  // Binary units, matching what the agent read off the machine. A dashboard
  // that says 17.2 GB where the Mac's About This Mac says 16 GB is a dashboard
  // nobody trusts.
  it("uses the same binary units the agent reported", () => {
    expect(bytes(16 * 1024 ** 3)).toBe("16 GiB");
    expect(bytes(192 * 1024 ** 3)).toBe("192 GiB");
    expect(bytes(512 * 1024 ** 2)).toBe("512 MiB");
  });

  it("renders an absent or impossible size as a dash rather than zero", () => {
    // "0 B" reads as a fact about the machine; "—" reads as "we do not know",
    // which is the truth for a device that has not reported inventory.
    expect(bytes(undefined)).toBe("—");
    expect(bytes(0)).toBe("—");
    expect(bytes(-1)).toBe("—");
  });
});

describe("relativeTime", () => {
  const now = new Date("2026-07-31T12:00:00Z");

  it("describes recent times the way an operator would", () => {
    expect(relativeTime("2026-07-31T11:59:58Z", now)).toBe("just now");
    expect(relativeTime("2026-07-31T11:59:30Z", now)).toBe("30s ago");
    expect(relativeTime("2026-07-31T11:30:00Z", now)).toBe("30m ago");
    expect(relativeTime("2026-07-31T09:00:00Z", now)).toBe("3h ago");
    expect(relativeTime("2026-07-29T12:00:00Z", now)).toBe("2d ago");
  });

  it("says never rather than inventing a date", () => {
    // A device that has never connected has no last_seen_at, and rendering the
    // epoch for it would put 1970 in the fleet table.
    expect(relativeTime(undefined, now)).toBe("never");
  });

  it("tolerates a clock slightly ahead of ours", () => {
    // Server and browser clocks disagree by seconds routinely. "in 3 seconds"
    // is noise; "just now" is the honest reading.
    expect(relativeTime("2026-07-31T12:00:03Z", now)).toBe("just now");
  });
});
