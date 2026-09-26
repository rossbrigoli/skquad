"use client";

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import {
  resolveTheme,
  storeTheme,
  readStoredTheme,
  type ResolvedTheme,
  type ThemeMode,
} from "./theme";

type ThemeContextValue = {
  mode: ThemeMode;
  resolved: ResolvedTheme;
  setMode: (mode: ThemeMode) => void;
};

const ThemeContext = createContext<ThemeContextValue | null>(null);

function systemPrefersDark(): boolean {
  return (
    typeof window !== "undefined" &&
    window.matchMedia("(prefers-color-scheme: dark)").matches
  );
}

function applyTheme(resolved: ResolvedTheme): void {
  const el = document.documentElement;
  el.setAttribute("data-theme", resolved);
  el.style.colorScheme = resolved;
}

export function ThemeProvider({ children }: { readonly children: ReactNode }) {
  const [mode, setMode] = useState<ThemeMode>("system");
  const [sysDark, setSysDark] = useState<boolean>(false);

  // Hydrate from localStorage after mount; the pre-paint inline script has
  // already set the attribute so there is no flash.
  useEffect(() => {
    setMode(readStoredTheme());
    setSysDark(systemPrefersDark());
  }, []);

  // Track live system preference changes while in "system" mode.
  useEffect(() => {
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = (e: MediaQueryListEvent) => setSysDark(e.matches);
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  const resolved = resolveTheme(mode, sysDark);

  useEffect(() => {
    applyTheme(resolved);
  }, [resolved]);

  const chooseMode = useCallback((next: ThemeMode) => {
    setMode(next);
    storeTheme(next);
    applyTheme(resolveTheme(next, systemPrefersDark()));
  }, []);

  const value = useMemo(
    () => ({ mode, resolved, setMode: chooseMode }),
    [mode, resolved, chooseMode],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error("useTheme must be used inside ThemeProvider");
  return ctx;
}
