"use client";

// S-199: per-user notification preferences. One checkbox per
// notification type; everything is ON by default (no stored row).
// Muting a type stops the control plane from filing those alerts to
// the bell — deliveries are skipped silently, never errored.

import { useEffect, useState } from "react";
import { useAuth } from "../lib/auth";
import {
  ALL_NOTIFICATION_TYPES,
  NOTIFICATION_PREF_OPTIONS,
  buildMutedList,
  fetchNotificationPreferences,
  hasChanges,
  isTypeEnabled,
  saveNotificationPreferences,
  type NotificationPreferences,
} from "../lib/notificationPrefs";
import type { NotificationType } from "../lib/api";

function defaultEnabled(): Record<NotificationType, boolean> {
  const map = {} as Record<NotificationType, boolean>;
  for (const t of ALL_NOTIFICATION_TYPES) map[t] = true;
  return map;
}

export function NotificationPreferencesPanel() {
  const { token } = useAuth();
  const [prefs, setPrefs] = useState<NotificationPreferences | null>(null);
  const [enabled, setEnabled] = useState<Record<NotificationType, boolean>>(defaultEnabled);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [note, setNote] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    // eslint-disable-next-line react-hooks/set-state-in-effect -- fetch-on-mount loading flag; same pattern as PlatformSettingsTab
    setLoading(true);
    void (async () => {
      try {
        const loaded = await fetchNotificationPreferences(token);
        if (!cancelled) {
          setPrefs(loaded);
          const next = defaultEnabled();
          for (const t of ALL_NOTIFICATION_TYPES) next[t] = isTypeEnabled(loaded, t);
          setEnabled(next);
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
  }, [token]);

  function toggle(type: NotificationType) {
    setEnabled((e) => ({ ...e, [type]: !e[type] }));
    setNote("");
  }

  async function save() {
    setSaving(true);
    setError("");
    setNote("");
    try {
      const saved = await saveNotificationPreferences(token, buildMutedList(enabled));
      setPrefs(saved);
      setNote("Notification preferences saved.");
    } catch (err) {
      setError(err instanceof Error ? `Save failed: ${err.message}` : "Save failed.");
    } finally {
      setSaving(false);
    }
  }

  const dirty = hasChanges(prefs, enabled);

  return (
    <section>
      <div className="section-head">
        <h2>Notifications</h2>
      </div>
      <p className="field-hint" style={{ marginBottom: "var(--space-4)" }}>
        Choose which alerts appear on the top-bar bell. Everything is on by default — uncheck a type to mute it.
      </p>
      {error ? <div className="notice error" role="alert">{error}</div> : null}
      {note ? <output className="notice" aria-live="polite">{note}</output> : null}
      {loading ? (
        <div className="entity-list">
          <div className="entity-row">Loading preferences…</div>
        </div>
      ) : (
        <div className="entity-list">
          {NOTIFICATION_PREF_OPTIONS.map((opt) => (
            <div key={opt.type} className="entity-row">
              <label className="entity-main" style={{ display: "flex", alignItems: "center", gap: 10, cursor: "pointer" }}>
                <input
                  type="checkbox"
                  checked={enabled[opt.type]}
                  onChange={() => {
                    toggle(opt.type);
                  }}
                  disabled={saving}
                />
                <span className="entity-title">{opt.label}</span>
                <span className="entity-meta">{opt.description}</span>
              </label>
            </div>
          ))}
        </div>
      )}
      <div style={{ marginTop: "var(--space-4)", display: "flex", gap: 8, alignItems: "center" }}>
        <button
          type="button"
          className="btn btn-primary"
          disabled={!dirty || saving || loading}
          onClick={() => {
            void save();
          }}
        >
          {saving ? "Saving…" : "Save preferences"}
        </button>
      </div>
    </section>
  );
}
