"use client";

// S-212 (ADR-0013 §4): platform-admin control for the embedder
// compute runtime. Auto = the operator picks by available GPU vendor
// (NVIDIA → CUDA, AMD/Intel → Vulkan, none → CPU). An explicit choice
// overrides detection. Saving writes the operator's override ConfigMap
// live — the embedder pod rolls once and no Helm redeploy is needed.

import { useEffect, useState } from "react";
import { ApiError } from "../lib/api";
import { useAuth } from "../lib/auth";
import {
  DEFAULT_EMBEDDER_RUNTIME,
  EMBEDDER_RUNTIMES,
  type EmbedderRuntime,
  fetchEmbedderRuntime,
  saveEmbedderRuntime,
} from "../lib/embedderRuntime";

const RUNTIME_LABELS: Record<EmbedderRuntime, string> = {
  auto: "Auto (detect GPU vendor)",
  cuda: "CUDA (NVIDIA)",
  vulkan: "Vulkan (AMD / Intel)",
  cpu: "CPU only",
};

export function EmbedderRuntimePanel() {
  const { token, mode } = useAuth();
  const authedToken = mode === "oidc" ? "" : token;
  const [runtime, setRuntime] = useState<EmbedderRuntime>(DEFAULT_EMBEDDER_RUNTIME);
  const [current, setCurrent] = useState<EmbedderRuntime | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [note, setNote] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    void (async () => {
      try {
        const value = await fetchEmbedderRuntime(authedToken);
        if (!cancelled) {
          setCurrent(value);
          setRuntime(value);
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
    setSaving(true);
    setError("");
    setNote("");
    try {
      const saved = await saveEmbedderRuntime(authedToken, runtime);
      setCurrent(saved);
      setNote(
        saved === "auto"
          ? "Saved. The operator will use whatever GPU it detects (CPU if none)."
          : `Saved. The embedder will roll onto the ${saved.toUpperCase()} runtime.`,
      );
    } catch (err) {
      setError(err instanceof ApiError ? `Save failed: ${err.message}` : "Save failed.");
    } finally {
      setSaving(false);
    }
  }

  return (
    <section style={{ marginTop: "var(--space-4, 16px)" }}>
      <div className="section-head">
        <h2>Embedder runtime</h2>
      </div>
      <p className="field-hint">
        Compute backend for the memory-embedding model (llama.cpp + Qwen3-Embedding-0.6B). Auto
        selects by GPU vendor: NVIDIA → CUDA, AMD/Intel → Vulkan, no GPU → CPU. An explicit choice
        overrides detection. Changing this rolls the embedder pod once; no redeploy required.
      </p>
      <div style={{ display: "flex", gap: "8px", alignItems: "center", flexWrap: "wrap", marginBottom: "8px" }}>
        <label className="field" style={{ marginBottom: 0 }}>
          <span>Runtime</span>
          <select
            aria-label="Embedder compute runtime"
            value={runtime}
            onChange={(e) => {
              setRuntime(e.target.value as EmbedderRuntime);
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
        <button type="button" className="btn btn-sm" disabled={loading || saving || runtime === current} onClick={() => void save()}>
          {saving ? "Saving…" : "Save"}
        </button>
      </div>
      {current !== null ? (
        <p className="metric-chip-sub">Current platform setting: {RUNTIME_LABELS[current]}.</p>
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
