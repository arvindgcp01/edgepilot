/**
 * CSV export, done in the browser from rows already on screen.
 *
 * Every cell is quoted, and a cell beginning with = + - @ or a tab is prefixed
 * with an apostrophe: spreadsheet apps execute those as formulas, and a device
 * hostname or a client name is operator- or agent-supplied text that must not
 * become one (CSV injection).
 */
export function toCSV(headers: string[], rows: (string | number | null | undefined)[][]): string {
  const cell = (v: string | number | null | undefined) => {
    let s = v === null || v === undefined ? "" : String(v);
    if (/^[=+\-@\t\r]/.test(s)) s = `'${s}`;
    return `"${s.replace(/"/g, '""')}"`;
  };
  return [headers, ...rows].map((r) => r.map(cell).join(",")).join("\r\n");
}

export function downloadCSV(filename: string, headers: string[], rows: (string | number | null | undefined)[][]) {
  // The BOM makes Excel read UTF-8 instead of guessing a legacy code page.
  const blob = new Blob(["﻿" + toCSV(headers, rows)], { type: "text/csv;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

export function stamp(now = new Date()): string {
  return now.toISOString().slice(0, 16).replace(/[:T]/g, "-");
}
