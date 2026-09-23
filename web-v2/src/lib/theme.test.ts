import { describe, expect, it } from "vitest";
import {
  isThemeMode,
  readStoredTheme,
  resolveTheme,
  storeTheme,
  THEME_INIT_SCRIPT,
  THEME_STORAGE_KEY,
} from "./theme";

describe("isThemeMode", () => {
  it("accepts the three supported modes", () => {
    expect(isThemeMode("system")).toBe(true);
    expect(isThemeMode("light")).toBe(true);
    expect(isThemeMode("dark")).toBe(true);
  });

  it("rejects anything else", () => {
    expect(isThemeMode("blue")).toBe(false);
    expect(isThemeMode("")).toBe(false);
    expect(isThemeMode(null)).toBe(false);
    expect(isThemeMode(42)).toBe(false);
  });
});

describe("resolveTheme", () => {
  it("explicit modes win over the system preference", () => {
    expect(resolveTheme("dark", false)).toBe("dark");
    expect(resolveTheme("dark", true)).toBe("dark");
    expect(resolveTheme("light", false)).toBe("light");
    expect(resolveTheme("light", true)).toBe("light");
  });

  it("system mode follows the system preference", () => {
    expect(resolveTheme("system", false)).toBe("light");
    expect(resolveTheme("system", true)).toBe("dark");
  });
});

describe("storage round-trip", () => {
  // Minimal localStorage stub — the module guards all access in try/catch.
  const makeStorage = (initial: Record<string, string> = {}) => {
    const data = new Map(Object.entries(initial));
    return {
      getItem: (k: string) => data.get(k) ?? null,
      setItem: (k: string, v: string) => void data.set(k, v),
    };
  };

  it("reads back a stored valid mode", () => {
    const store = makeStorage({ [THEME_STORAGE_KEY]: "dark" });
    (globalThis as Record<string, unknown>).window = { localStorage: store };
    expect(readStoredTheme()).toBe("dark");
    storeTheme("light");
    expect(readStoredTheme()).toBe("light");
    delete (globalThis as Record<string, unknown>).window;
  });

  it("falls back to system for invalid or missing values", () => {
    const store = makeStorage({ [THEME_STORAGE_KEY]: "chartreuse" });
    (globalThis as Record<string, unknown>).window = { localStorage: store };
    expect(readStoredTheme()).toBe("system");
    delete (globalThis as Record<string, unknown>).window;

    const empty = makeStorage();
    (globalThis as Record<string, unknown>).window = { localStorage: empty };
    expect(readStoredTheme()).toBe("system");
    delete (globalThis as Record<string, unknown>).window;
  });

  it("is SSR-safe (no window)", () => {
    // No window defined at all.
    expect(readStoredTheme()).toBe("system");
  });
});

describe("THEME_INIT_SCRIPT", () => {
  it("embeds the storage key so script and provider agree", () => {
    expect(THEME_INIT_SCRIPT).toContain(THEME_STORAGE_KEY);
  });

  it("sets data-theme and colorScheme for an explicit dark choice", () => {
    const attrs: Record<string, string> = {};
    const doc = {
      documentElement: {
        setAttribute: (k: string, v: string) => {
          attrs[k] = v;
        },
        style: {} as Record<string, string>,
      },
    };
    const win = {
      localStorage: { getItem: (k: string) => (k === THEME_STORAGE_KEY ? "dark" : null) },
      matchMedia: () => ({ matches: false }),
    };
    // Execute the script exactly as the browser would, with scoped globals.
    const run = new Function("document", "window", "localStorage", THEME_INIT_SCRIPT);
    run(doc, win, win.localStorage);
    expect(attrs["data-theme"]).toBe("dark");
    expect(doc.documentElement.style.colorScheme).toBe("dark");
  });

  it("system mode follows prefers-color-scheme", () => {
    const attrs: Record<string, string> = {};
    const doc = {
      documentElement: {
        setAttribute: (k: string, v: string) => {
          attrs[k] = v;
        },
        style: {} as Record<string, string>,
      },
    };
    const win = {
      localStorage: { getItem: () => null },
      matchMedia: () => ({ matches: true }),
    };
    const run = new Function("document", "window", "localStorage", THEME_INIT_SCRIPT);
    run(doc, win, win.localStorage);
    expect(attrs["data-theme"]).toBe("dark");
  });
});
