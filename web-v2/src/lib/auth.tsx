"use client";

import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { apiGet, type ApiUser } from "../lib/api";

// Same storage key as v1 so a browser that logged into v1 carries the token
// into v2 when served from the same origin; otherwise it is entered once here.
const TOKEN_KEY = "***";

type AuthValue = {
  token: string;
  user: ApiUser | null;
  loading: boolean;
  error: string;
  setToken: (token: string) => void;
  logout: () => void;
};

const AuthContext = createContext<AuthValue | null>(null);

export function TokenProvider({ children }: { children: ReactNode }) {
  const [token, setTokenState] = useState("");
  const [user, setUser] = useState<ApiUser | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    // Reading localStorage must happen post-hydration to avoid SSR mismatch.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setTokenState(window.localStorage.getItem(TOKEN_KEY) || "");
  }, []);

  useEffect(() => {
    let cancelled = false;
    if (!token) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setUser(null);
       
      setLoading(false);
      return;
    }
    setLoading(true);
    apiGet<ApiUser>("/auth/me", token)
      .then((me) => {
        if (!cancelled) {
          setUser(me);
          setError("");
        }
      })
      .catch((err: Error) => {
        if (!cancelled) {
          setUser(null);
          setError(err.message || "authentication failed");
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [token]);

  const setToken = useCallback((next: string) => {
    window.localStorage.setItem(TOKEN_KEY, next);
    setTokenState(next);
  }, []);

  const logout = useCallback(() => {
    window.localStorage.removeItem(TOKEN_KEY);
    setTokenState("");
    setUser(null);
  }, []);

  return (
    <AuthContext.Provider value={{ token, user, loading, error, setToken, logout }}>
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth(): AuthValue {
  const value = useContext(AuthContext);
  if (!value) {
    throw new Error("useAuth must be used inside TokenProvider");
  }
  return value;
}
