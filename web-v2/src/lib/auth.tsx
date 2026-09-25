"use client";

import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { apiGet, setApiBaseOverride, type ApiUser } from "./api";

// Token (dev) mode: bearer token pasted/kept in localStorage.
// OIDC mode (UIv2-13): httpOnly session cookie + /proxy; the token never
// reaches the browser. Mode is decided by the server (/auth/config).
const TOKEN_KEY = "***";

export type AuthMode = "token" | "oidc";

type AuthValue = {
  token: string;
  user: ApiUser | null;
  loading: boolean;
  error: string;
  mode: AuthMode;
  // authed is mode-aware: OIDC sessions authenticate via httpOnly cookie, so
  // `token` is legitimately empty there. Gate fetches on `authed`, never on
  // `token` — that pattern silently disabled pages under OIDC (S-121 follow-up).
  authed: boolean;
  setToken: (token: string) => void;
  logout: () => void;
};

const AuthContext = createContext<AuthValue | null>(null);

export function TokenProvider({ children }: { children: ReactNode }) {
  const [token, setTokenState] = useState("");
  const [user, setUser] = useState<ApiUser | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [mode, setMode] = useState<AuthMode>("token");

  useEffect(() => {
    let cancelled = false;
    (async () => {
      let detected: AuthMode = "token";
      try {
        const res = await fetch("/auth/config", { cache: "no-store" });
        if (res.ok) {
          const cfg = await res.json();
          detected = cfg?.mode === "oidc" ? "oidc" : "token";
        }
      } catch {
        // Static/dev fallback: token mode.
      }
      if (cancelled) return;
      setMode(detected);

      if (detected === "oidc") {
        setApiBaseOverride("/proxy");
        try {
          const res = await fetch("/auth/session", { cache: "no-store", credentials: "same-origin" });
          if (res.ok) {
            const me = await apiGet<ApiUser>("/auth/me", "");
            if (!cancelled) {
              setUser(me);
              setError("");
              setLoading(false);
            }
            return;
          }
        } catch (err) {
          if (!cancelled) setError(err instanceof Error ? err.message : "session check failed");
        }
        // Not signed in. Do NOT auto-redirect: the login screen presents a
        // chooser (SSO vs break-glass) instead of firing the user straight at the
        // IdP, which also keeps an unreachable Dex from producing a redirect loop.
        if (!cancelled) {
          setLoading(false);
        }
        return;
      }

      // Token (dev) mode — read localStorage post-hydration to avoid SSR mismatch.
      if (!cancelled) {
        setTokenState(window.localStorage.getItem(TOKEN_KEY) ?? "");
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (mode !== "token") return;
    let cancelled = false;
    const run = async () => {
      if (!token) {
        if (!cancelled) {
          setUser(null);
          setLoading(false);
        }
        return;
      }
      if (!cancelled) setLoading(true);
      try {
        const me = await apiGet<ApiUser>("/auth/me", token);
        if (!cancelled) {
          setUser(me);
          setError("");
        }
      } catch (err) {
        if (!cancelled) {
          setUser(null);
          setError(err instanceof Error ? err.message : "authentication failed");
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    };
    run().catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [token, mode]);

  const setToken = useCallback((next: string) => {
    window.localStorage.setItem(TOKEN_KEY, next);
    setTokenState(next);
  }, []);

  const logout = useCallback(() => {
    if (mode === "oidc") {
      // Full-page navigation: ends the app session and hits the IdP logout.
      // eslint-disable-next-line @next/next/no-location-assign-relative-destination
      window.location.assign("/auth/logout");
      return;
    }
    window.localStorage.removeItem(TOKEN_KEY);
    setTokenState("");
    setUser(null);
  }, [mode]);

  const authed = mode === "oidc" ? !!user : !!token;

  // Memoize the context value so consumers do not re-render on every
  // provider render (S-126 / S6481).
  const value = useMemo<AuthValue>(
    () => ({ token, user, loading, error, mode, authed, setToken, logout }),
    [token, user, loading, error, mode, authed, setToken, logout],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside TokenProvider");
  return ctx;
}
