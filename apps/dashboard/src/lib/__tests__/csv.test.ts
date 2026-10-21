import { describe, expect, it } from "vitest";
import { toCSV } from "@/lib/csv";

describe("toCSV", () => {
  it("quotes every cell and escapes quotes", () => {
    expect(toCSV(["a", "b"], [["x", 'say "hi"']])).toBe('"a","b"\r\n"x","say ""hi"""');
  });

  it("neutralises cells a spreadsheet would run as a formula", () => {
    // A hostname or client name is agent- or operator-supplied text.
    expect(toCSV(["h"], [["=HYPERLINK(\"x\")"], ["+1"], ["-2"], ["@sum"]])).toBe(
      '"h"\r\n"\'=HYPERLINK(""x"")"\r\n"\'+1"\r\n"\'-2"\r\n"\'@sum"',
    );
  });

  it("renders missing values as empty", () => {
    expect(toCSV(["a"], [[null], [undefined], [0]])).toBe('"a"\r\n""\r\n""\r\n"0"');
  });
});
