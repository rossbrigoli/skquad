"use client";

import { useCallback, useEffect, useState } from "react";
import { apiGet, type ApiState } from "./api";
import { useAuth } from "./auth";

// Minimal fetch-on-mount state hook for v2 pages. Polls only while the tab is
// visible to keep background load off the shared control-plane API.
export function useApi<T>(path: string, pollMs = 0): ApiState<T> & { refresh: () => void } {
  const { token } = useAuth();
  const [state, setState] = useState<ApiState<T>>({ data: null, loading: true, error: "" });
  const [tick, setTick] = useState(0);

  const load = useCallback(async () => {
    if (!token) {
      setState({ data: null, loading: false, error: "not authenticated" });
      return;
    }
    try {
      const data = await apiGet<T>(path, token);
      setState({ data, loading: false, error: "" });
    } catch (err) {
      setState({ data: null, loading: false, error: err instanceof Error ? err.message : "request failed" });
    }
  }, [path, token]);

  useEffect(() => {
    // Initial fetch is an effect by design; setState lands async.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    load();
    if (!pollMs) return;
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") load();
    }, pollMs);
    return () => window.clearInterval(timer);
  }, [load, pollMs, tick]);

  const refresh = useCallback(() => setTick((t) => t + 1), []);

  return { ...state, refresh };
}
