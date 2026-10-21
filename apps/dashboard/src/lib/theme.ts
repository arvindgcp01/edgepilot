/**
 * The colour theme.
 *
 * A per-browser preference, not a per-account one: a wall display in a dim
 * operations room and the same operator's laptop want different answers, and a
 * server-side setting would force them to agree. Storage can be unavailable
 * (private windows, locked-down kiosks), so every access is guarded and the
 * fallback is "system".
 */
export type ThemePreference = "light" | "dark" | "system";

export const THEME_KEY = "edgepilot.theme";

export function readTheme(): ThemePreference {
  try {
    const saved = window.localStorage.getItem(THEME_KEY);
    if (saved === "light" || saved === "dark" || saved === "system") return saved;
  } catch {
    // Storage unavailable: follow the OS.
  }
  return "system";
}

export function resolveTheme(preference: ThemePreference): "light" | "dark" {
  if (preference !== "system") return preference;
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

export function applyTheme(preference: ThemePreference) {
  document.documentElement.classList.toggle("dark", resolveTheme(preference) === "dark");
}

export function setTheme(preference: ThemePreference) {
  try {
    window.localStorage.setItem(THEME_KEY, preference);
  } catch {
    // Applied for this page view only.
  }
  applyTheme(preference);
  window.dispatchEvent(new CustomEvent("edgepilot:theme", { detail: preference }));
}

/**
 * Runs inline in <head>, before React and before first paint, so a dark-theme
 * operator never sees a white flash on load. Kept as a string because it must
 * not depend on the bundle.
 */
export const THEME_BOOTSTRAP = `(function(){try{var p=localStorage.getItem(${JSON.stringify(THEME_KEY)})||"system";var d=p==="dark"||(p==="system"&&matchMedia("(prefers-color-scheme: dark)").matches);if(d)document.documentElement.classList.add("dark")}catch(e){}})()`;
