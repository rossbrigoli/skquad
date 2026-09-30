"use client";

// S-183: platform-admin control for the idle scale-to-zero window.
// "Only scale to zero when there have been no agent activities for the
// past N minutes." Saved via the admin settings API; the control-plane
// pushes the new value to every Agent CR through the Kubernetes outbox,
// so it takes effect without redeploying Helm. Per-agent overrides
// (agent "idle timeout (sec)" > 0) still win.

import { useEffect, useState } from "react";
import { ApiError } from "../lib/api";
import { useAuth } from "../lib/auth";
import {
  DEFAULT_IDLE_SCALE_TO_ZERO_MINUTES,
  fetchIdleScaleToZeroMinutes,
  parseIdleMinutesInput,
  saveIdleScaleToZeroMinutes,
} from "../lib/idleScaleToZero";

export function IdleScaleToZeroPanel() {
  const { token, mode } = useAuth();
  const authedToken = mode === "oidc" ? "" : token;
  const [minutes, setMinutes] = useState(String(DEFAULT_IDLE_SCALE_TO_ZERO_MINUTES));
  const [current, setCurrent] = useState<number | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [note, setNote] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    void (async () => {
      try {
        const value = await fetchIdleScaleToZeroMinutes(authedToken);
        if (!cancelled) {
          setCurrent(value);
          setMinutes(String(value));
        }
      } catch (err) {
        if (!cancelled) {
          setError(err instanceof Error ? `Load failed: ${err.message}` : "Load failed.");
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [authedToken]);

  async function save() {
    const { value, error: parseError } = parseIdleMinutesInput(minutes);
    setError(parseError ?? "");
    if (parseError || value === null) return;
    setSaving(true);
    setError("");
    setNote("");
    try {
      const saved = await saveIdleScaleToZeroMinutes(authedToken, value);
      setCurrent(saved);
      setNote(`Saved. Agents now scale to zero after ${saved} minute(s) without activity.`);
    } catch (err) {
      setError(err instanceof ApiError ? `Save failed: ${err.message}` : "Save failed.");
    } finally {
      setSaving(false);
    }
  }

  return (
    <section style={{ marginTop: "var(--space-4, 16px)" }}>
      <div className="section-head">
        <h2>Scaling</h2>
      </div>
      <p className="field-hint">
        Agents scale to zero only after this many minutes with no activity — no task running, no chat
        turn in progress, no pending inbox work. Agents with their own idle timeout set keep it; this is
        the platform-wide default. Changing it re-syncs every agent immediately.
      </p>
      <div style={{ display: "flex", gap: "8px", alignItems: "center", flexWrap: "wrap", marginBottom: "8px" }}>
        <label className="field" style={{ marginBottom: 0 }}>
          <span>Scale to zero after (minutes) of inactivity</span>
          <input
            aria-label="Scale to zero after minutes of inactivity"
            value={minutes}
            onChange={(e) => {
              setMinutes(e.target.value.replace(/[^0-9]/g, ""));
              setNote("");
              setError("");
            }}
            inputMode="numeric"
            disabled={loading || saving}
          />
        </label>
        <button type="button" className="btn btn-sm" disabled={loading || saving} onClick={() => void save()}>
          {saving ? "Saving…" : "Save"}
        </button>
      </div>
      {current !== null ? (
        <p className="metric-chip-sub">Current platform setting: {current} minute(s).</p>
      ) : null}
      {error ? (
        <p className="notice error" role="alert">
          {error}
        </p>
      ) : null}
      {note ? <p className="field-hint">{note}</p> : null}
    </section>
  );
}
