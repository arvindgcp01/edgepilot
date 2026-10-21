import type { Config } from "tailwindcss";

// Dark first. These machines are watched on a wall display in a room that is
// usually dim, and a white dashboard at 2 a.m. is a dashboard nobody looks at.
//
// The palette is deliberately narrow. Surfaces step up in three increments,
// text steps down in three, and colour is reserved for state — a status, an
// action, a warning. Anything that is neither structure nor state is grey.
export default {
  content: ["./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        surface: {
          // Page background, then panels, then popovers. Each step is the
          // smallest that still reads as "in front of" the one below it.
          DEFAULT: "#0b0e14",
          sunken: "#07090d",
          raised: "#10151e",
          overlay: "#151b26",
          hover: "#161d29",
          border: "#1f2733",
          "border-strong": "#2c3746",
        },
        ink: {
          DEFAULT: "#e6edf3",
          muted: "#8b98a9",
          faint: "#5c6874",
          inverse: "#0b0e14",
        },
        // The one interactive colour. Status blue (`pending`) is a different
        // token on purpose: a button and a "waiting for approval" badge must be
        // able to diverge without one dragging the other.
        accent: {
          DEFAULT: "#3b82f6",
          hover: "#2f74e6",
          soft: "#58a6ff",
        },
        // Status colours are semantic, not decorative: they are the fastest
        // read on the page and must not drift between views.
        online: "#3fb950",
        offline: "#6e7681",
        degraded: "#d29922",
        pending: "#58a6ff",
        danger: "#f85149",
        success: "#3fb950",
        warning: "#d29922",
      },
      fontFamily: {
        sans: ["Inter", "ui-sans-serif", "system-ui", "-apple-system", "Segoe UI", "sans-serif"],
        mono: ["ui-monospace", "SFMono-Regular", "Menlo", "monospace"],
      },
      fontSize: {
        "2xs": ["0.6875rem", { lineHeight: "1rem" }],
      },
      boxShadow: {
        // One ring, one soft drop. Popovers and drawers sit on a dark page, so
        // the ring does most of the work; the shadow only separates overlapping
        // dark surfaces.
        overlay: "0 0 0 1px #1f2733, 0 12px 32px -8px rgba(0, 0, 0, 0.6)",
        drawer: "-8px 0 32px -8px rgba(0, 0, 0, 0.6)",
      },
      keyframes: {
        "fade-in": { from: { opacity: "0" }, to: { opacity: "1" } },
        "slide-in-right": {
          from: { transform: "translateX(16px)", opacity: "0" },
          to: { transform: "translateX(0)", opacity: "1" },
        },
        "slide-up": {
          from: { transform: "translateY(6px)", opacity: "0" },
          to: { transform: "translateY(0)", opacity: "1" },
        },
        shimmer: {
          "0%": { backgroundPosition: "-200% 0" },
          "100%": { backgroundPosition: "200% 0" },
        },
      },
      animation: {
        "fade-in": "fade-in 120ms ease-out",
        "slide-in-right": "slide-in-right 160ms ease-out",
        "slide-up": "slide-up 120ms ease-out",
        shimmer: "shimmer 1.6s linear infinite",
      },
    },
  },
  plugins: [],
} satisfies Config;
