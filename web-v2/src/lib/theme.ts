// Theme mode selection for Skquad UIv2 (S-101).
// Pure logic lives here so it is testable without DOM providers.

export type ThemeMode = "system" | "light" | "dark";
export type ResolvedTheme = "light" | "dark";

export const THEME_STORAGE_KEY = "***";
export const THEME_MODES: ThemeMode[] = ["system", "light", "dark"];

export function isThemeMode(value: unknown): value is ThemeMode {
  return typeof value === "string" && (THEME_MODES as string[]).includes(value);
}

// Read the persisted mode. Returns "system" when nothing valid is stored.
// SSR-safe: returns the default when window is undefined.
export function readStoredTheme(): ThemeMode {
  if (typeof window === "undefined") return "system";
  try {
    const raw = window.localStorage.getItem(THEME_STORAGE_KEY);
    return isThemeMode(raw) ? raw : "system";
  } catch {
    return "system";
  }
}

export function storeTheme(mode: ThemeMode): void {
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, mode);
  } catch {
    // Storage disabled (private mode etc.) — theme still applies for the session.
  }
}

// Resolve a mode against the system preference into a concrete theme.
export function resolveTheme(mode: ThemeMode, systemPrefersDark: boolean): ResolvedTheme {
  if (mode === "dark") return "dark";
  if (mode === "light") return "light";
  return systemPrefersDark ? "dark" : "light";
}

// The pre-paint inline script (layout <head>) uses this to avoid a flash of
// the wrong theme. Kept as a string constant so it can be snapshot-tested.
export const THEME_INIT_SCRIPT = `(function(){try{var m=localStorage.getItem('${THEME_STORAGE_KEY}')||'system';var d=m==='dark'||(m==='system'&&window.matchMedia('(prefers-color-scheme: dark)').matches);var e=document.documentElement;e.setAttribute('data-theme',d?'dark':'light');e.style.colorScheme=d?'dark':'light';}catch(e){}})();`;
