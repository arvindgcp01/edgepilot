import type { Config } from "tailwindcss";

// Dark first. These machines are watched on a wall display in a room that is
// usually dim, and a white dashboard at 2 a.m. is a dashboard nobody looks at.
export default {
  content: ["./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        surface: { DEFAULT: "#0b0e14", raised: "#121721", border: "#1f2733" },
        ink: { DEFAULT: "#e6edf3", muted: "#8b98a9", faint: "#5c6874" },
        // Status colours are semantic, not decorative: they are the fastest
        // read on the page and must not drift between views.
        online: "#3fb950",
        offline: "#6e7681",
        degraded: "#d29922",
        pending: "#58a6ff",
        danger: "#f85149",
      },
      fontFamily: { mono: ["ui-monospace", "SFMono-Regular", "Menlo", "monospace"] },
    },
  },
  plugins: [],
} satisfies Config;
