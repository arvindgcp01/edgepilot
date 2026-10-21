import type { Config } from "tailwindcss";

/**
 * Light first, dark on request.
 *
 * Every colour is a CSS variable holding RGB channels (see globals.css), so the
 * same class — `bg-surface-raised`, `text-ink-muted` — renders correctly in both
 * themes and opacity modifiers like `bg-accent/10` keep working. Components never
 * name a theme; they name a role.
 *
 * The palette is deliberately narrow. Surfaces step up in three increments,
 * text steps down in three, and colour is reserved for brand and state — the
 * primary action, a status, a warning. Anything that is neither is grey.
 */
const token = (name: string) => `rgb(var(--${name}) / <alpha-value>)`;

export default {
  content: ["./src/**/*.{ts,tsx}"],
  darkMode: "class",
  theme: {
    extend: {
      colors: {
        surface: {
          // Page background, then cards, then popovers.
          DEFAULT: token("surface"),
          sunken: token("surface-sunken"),
          raised: token("surface-raised"),
          overlay: token("surface-overlay"),
          hover: token("surface-hover"),
          border: token("surface-border"),
          "border-strong": token("surface-border-strong"),
        },
        ink: {
          DEFAULT: token("ink"),
          muted: token("ink-muted"),
          faint: token("ink-faint"),
          inverse: token("ink-inverse"),
        },
        // The brand and the one interactive colour. Status blue (`pending`) is
        // a different token on purpose: a button and a "waiting for approval"
        // badge must be able to diverge without one dragging the other.
        accent: {
          DEFAULT: token("accent"),
          hover: token("accent-hover"),
          soft: token("accent-soft"),
          subtle: token("accent-subtle"),
        },
        // Status colours are semantic, not decorative: they are the fastest
        // read on the page and must not drift between views.
        online: token("online"),
        offline: token("offline"),
        degraded: token("degraded"),
        pending: token("pending"),
        danger: token("danger"),
        success: token("success"),
        warning: token("warning"),
      },
      fontFamily: {
        sans: ["var(--font-sans)", "Inter", "ui-sans-serif", "system-ui", "-apple-system", "Segoe UI", "sans-serif"],
        mono: ["ui-monospace", "SFMono-Regular", "Menlo", "monospace"],
      },
      fontSize: {
        "2xs": ["0.6875rem", { lineHeight: "1rem" }],
      },
      borderRadius: {
        // One step rounder than Tailwind's defaults: the softer corner is most
        // of what reads as "modern product" rather than "admin panel".
        md: "0.5rem",
        lg: "0.75rem",
        xl: "1rem",
      },
      boxShadow: {
        card: "var(--shadow-card)",
        overlay: "var(--shadow-overlay)",
        drawer: "var(--shadow-drawer)",
        focus: "0 0 0 3px rgb(var(--accent) / 0.25)",
      },
      keyframes: {
        "fade-in": { from: { opacity: "0" }, to: { opacity: "1" } },
        "slide-in-right": {
          from: { transform: "translateX(16px)", opacity: "0" },
          to: { transform: "translateX(0)", opacity: "1" },
        },
        "slide-in-left": {
          from: { transform: "translateX(-16px)", opacity: "0" },
          to: { transform: "translateX(0)", opacity: "1" },
        },
        "slide-up": {
          from: { transform: "translateY(6px)", opacity: "0" },
          to: { transform: "translateY(0)", opacity: "1" },
        },
        "scale-in": {
          from: { transform: "scale(0.98)", opacity: "0" },
          to: { transform: "scale(1)", opacity: "1" },
        },
        shimmer: {
          "0%": { backgroundPosition: "-200% 0" },
          "100%": { backgroundPosition: "200% 0" },
        },
      },
      animation: {
        "fade-in": "fade-in 150ms ease-out",
        "slide-in-right": "slide-in-right 180ms ease-out",
        "slide-in-left": "slide-in-left 180ms ease-out",
        "slide-up": "slide-up 150ms ease-out",
        "scale-in": "scale-in 140ms ease-out",
        shimmer: "shimmer 1.6s linear infinite",
      },
    },
  },
  plugins: [],
} satisfies Config;
