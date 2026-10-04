"use client";

// S-219: Platform settings tab. Formerly the "Scaling" tab — it had
// grown past scaling alone (embedder runtime joined in S-212), so the
// two panels are merged into ONE form with a single Save button. On
// save, the form is diffed against the loaded current values and a
// single PUT /admin/settings carries ONLY the changed settings
// (see lib/platformSettings.ts). The per-section Save buttons of the
// old IdleScaleToZeroPanel / EmbedderRuntimePanel are gone.

import { useEffect, useMemo, useState } from "react";
import { ApiError } from "../lib/api";
import { useAuth } from "../lib/auth";
import {
  DEFAULT_EMBEDDER_RUNTIME,
  EMBEDDER_RUNTIMES,
  RUNTIME_LABELS,
} from "../lib/embedderRuntime";
import { DEFAULT_IDLE_SCALE_TO_ZERO_MINUTES } from "../lib/idleScaleToZero";
import {
  type PlatformSettingsChanges,
  type PlatformSettingsCurrent,
  type PlatformSettingsFormValues,
  diffPlatformSettings,
  formFromPlatformSettings,
  hasPlatformSettingsChanges,
  loadPlatformSettings,
  savePlatformSettingsChanges,
} from "../lib/platformSettings";

// describeSaved renders the post-save confirmation for the keys that
// were actually sent.
function describeSaved(changes: PlatformSettingsChanges): string {
  const parts: string[] = [];
  if (changes.idle_scale_to_zero_minutes !== undefined) {
    parts.push(`agents scale to zero after ${changes.idle_scale_to_zero_minutes} minute(s) without activity`);
  }
  if (changes.embedder_runtime !== undefined) {
    parts.push(
      changes.embedder_runtime === "auto"
        ? "the embedder uses whatever GPU it detects (CPU if none)"
        : `the embedder rolls onto the ${changes.embedder_runtime.toUpperCase()} runtime`,
    );
  }
  return `Saved: ${parts.join("; ")}.`;
}

export function PlatformSettingsTab() {
  const { token, mode } = useAuth();
  const authedToken = mode === "oidc" ? "" : token;
  const [current, setCurrent] = useState<PlatformSettingsCurrent | null>(null);
  const [form, setForm] = useState<PlatformSettingsFormValues>(() =>
    formFromPlatformSettings({
      idleMinutes: DEFAULT_IDLE_SCALE_TO_ZERO_MINUTES,
      embedderRuntime: DEFAULT_EMBEDDER_RUNTIME,
    }),
  );
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [note, setNote] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    // eslint-disable-next-line react-hooks/set-state-in-effect -- fetch-on-mount loading flag; same pattern as NotificationPreferencesPanel
    setLoading(true);
    void (async () => {
      try {
        const value = await loadPlatformSettings(authedToken);
        if (!cancelled) {
          setCurrent(value);
          setForm(formFromPlatformSettings(value));
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

  // diff drives both the disabled state and the change summary. Errors
  // are only surfaced after a save attempt (see submit) so typing a
  // partial number doesn't shout mid-edit.
  const diff = useMemo(
    () => diffPlatformSettings(current ?? { idleMinutes: -1, embedderRuntime: DEFAULT_EMBEDDER_RUNTIME }, form),
    [current, form],
  );
  const dirty = current !== null && hasPlatformSettingsChanges(diff.changes);

  async function submit() {
    if (!current) return;
    const { changes, errors } = diffPlatformSettings(current, form);
    if (errors.length > 0) {
      setError(errors.join(" "));
      return;
    }
    if (!hasPlatformSettingsChanges(changes)) {
      setError("");
      setNote("Nothing to save — no settings changed.");
      return;
    }
    setSaving(true);
    setError("");
    setNote("");
    try {
      const updated = await savePlatformSettingsChanges(authedToken, current, changes);
      setCurrent(updated);
      setForm(formFromPlatformSettings(updated));
      setNote(describeSaved(changes));
    } catch (err) {
      setError(err instanceof ApiError ? `Save failed: ${err.message}` : "Save failed.");
    } finally {
      setSaving(false);
    }
  }

  return (
    <section style={{ marginTop: "var(--space-4, 16px)" }}>
      <div className="section-head">
        <h2>Platform</h2>
      </div>
      {error ? (
        <p className="notice error" role="alert">
          {error}
        </p>
      ) : null}
      {note ? <p className="field-hint">{note}</p> : null}

      <div className="card" style={{ marginBottom: "var(--space-4, 16px)" }}>
        <h3>Scaling</h3>
        <p className="field-hint">
          Agents scale to zero only after this many minutes with no activity — no task running, no chat
          turn in progress, no pending inbox work. Agents with their own idle timeout set keep it; this is
          the platform-wide default. Changing it re-syncs every agent immediately.
        </p>
        <label className="field" style={{ marginBottom: 0 }}>
          <span>Scale to zero after (minutes) of inactivity</span>
          <input
            aria-label="Scale to zero after minutes of inactivity"
            value={form.idleMinutes}
            onChange={(e) => {
              setForm((f) => ({ ...f, idleMinutes: e.target.value.replace(/\D/g, "") }));
              setNote("");
              setError("");
            }}
            inputMode="numeric"
            disabled={loading || saving}
          />
        </label>
        {current !== null ? (
          <p className="metric-chip-sub">Current platform setting: {current.idleMinutes} minute(s).</p>
        ) : null}
      </div>

      <div className="card" style={{ marginBottom: "var(--space-4, 16px)" }}>
        <h3>Embedder runtime</h3>
        <p className="field-hint">
          Compute backend for the memory-embedding model (llama.cpp + Qwen3-Embedding-0.6B). Auto
          selects by GPU vendor: NVIDIA → CUDA, AMD/Intel → Vulkan, no GPU → CPU. An explicit choice
          overrides detection. Changing this rolls the embedder pod once; no redeploy required.
        </p>
        <label className="field" style={{ marginBottom: 0 }}>
          <span>Runtime</span>
          <select
            aria-label="Embedder compute runtime"
            value={form.embedderRuntime}
            onChange={(e) => {
              setForm((f) => ({ ...f, embedderRuntime: e.target.value as PlatformSettingsFormValues["embedderRuntime"] }));
              setNote("");
              setError("");
            }}
            disabled={loading || saving}
          >
            {EMBEDDER_RUNTIMES.map((r) => (
              <option key={r} value={r}>
                {RUNTIME_LABELS[r]}
              </option>
            ))}
          </select>
        </label>
        {current !== null ? (
          <p className="metric-chip-sub">Current platform setting: {RUNTIME_LABELS[current.embedderRuntime]}.</p>
        ) : null}
      </div>

      <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
        <button
          type="button"
          className="btn btn-primary"
          disabled={loading || saving || !dirty}
          onClick={() => {
            void submit();
          }}
        >
          {saving ? "Saving…" : "Save"}
        </button>
        {dirty ? <span className="entity-meta">Unsaved changes</span> : null}
      </div>
    </section>
  );
}
