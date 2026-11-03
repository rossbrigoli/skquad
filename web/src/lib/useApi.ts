"use client";

import { useCallback, useEffect, useState } from "react";
import { apiGet, type ApiState } from "./api";
import { useAuth } from "./auth";

// Minimal fetch-on-mount state hook for v2 pages. Polls only while the tab is
// visible to keep background load off the shared control-plane API.
export function useApi<T>(
  path: string,
  pollMs = 0,
): ApiState<T> & { refresh: () => Promise<T | null> } {
  const { token, mode, user } = useAuth();
  const [state, setState] = useState<ApiState<T>>({ data: null, loading: true, error: "" });

  const load = useCallback(async (): Promise<T | null> => {
    // Empty path = caller has nothing to fetch (e.g. menu data that only
    // exists in a certain route context). Skip without an error state.
    if (!path) {
      setState({ data: null, loading: false, error: "" });
      return null;
    }
    const authed = mode === "oidc" ? !!user : !!token;
    if (!authed) {
      setState({ data: null, loading: false, error: "not authenticated" });
      return null;
    }
    try {
      const data = await apiGet<T>(path, mode === "oidc" ? "" : token);
      setState({ data, loading: false, error: "" });
      return data;
    } catch (err) {
      setState({ data: null, loading: false, error: err instanceof Error ? err.message : "request failed" });
      return null;
    }
  }, [path, token, mode, user]);

  useEffect(() => {
    // Initial fetch is an effect by design; setState lands async.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    load();
    if (!pollMs) return;
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") void load();
    }, pollMs);
    return () => window.clearInterval(timer);
  }, [load, pollMs]);

  // refresh() performs the load itself and RESOLVES with the fresh
  // payload once it has landed. Callers that re-derive state from — or
  // remount a keyed form around — refreshed data must await it and use
  // the RETURNED value. Reading the hook's own `.data` after an
  // un-awaited refresh is what made saved fields snap back to their old
  // values: the remount ran against the still-stale cached payload, and
  // the new values only appeared on the next poll (or a page reload).
  const refresh = useCallback(async (): Promise<T | null> => load(), [load]);

  return { ...state, refresh };
}
