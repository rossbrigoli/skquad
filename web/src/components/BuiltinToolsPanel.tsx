"use client";

// BT-4 (S-151): "Built-in Tools" admin settings panel.
// One card per built-in tool (exec / web_fetch / web_search) with an
// enable toggle, the tool's pinned policy form (ADR-0012 §3), inline
// validation errors from the server (400), success feedback, and the
// per-tool updatedAt/updatedBy line. Rendered only for platform_admin —
// gating lives in the settings page (mirrors the Access/Prompt tabs);
// the server enforces the same check on every admin endpoint.

import { useMemo, useState } from "react";
import { apiPatch, ApiError } from "../lib/api";
import { useAuth } from "../lib/auth";
import { useApi } from "../lib/useApi";
import {
  BUILTIN_TOOL_NAMES,
  SEARCH_PROVIDERS,
  buildToolPayload,
  formFromTool,
  formatToolUpdate,
  isBuiltinToolName,
  toolsFromList,
  toolSaveErrorMessage,
  validateToolForm,
  type BuiltinTool,
  type BuiltinToolName,
  type BuiltinToolsList,
  type ToolFormValues,
} from "../lib/builtinTools";

const TOOL_LABELS: Record<BuiltinToolName, string> = {
  exec: "exec — run terminal commands",
  web_fetch: "web_fetch — fetch a URL",
  web_search: "web_search — web search",
};

const TOOL_HINTS: Record<BuiltinToolName, string> = {
  exec: "Runs inside the agent pod (the container is the sandbox). Denied patterns are regexes matched against the full command before execution.",
  web_fetch: "GET only, ≤3 redirects with an SSRF re-check per hop. Private-network targets stay blocked unless explicitly allowed.",
  web_search: "Provider API keys live only in the control plane; agents call the search proxy and never see credentials.",
};

export function BuiltinToolsPanel() {
  const { token, mode } = useAuth();
  const list = useApi<BuiltinToolsList>("/admin/tools", 0);
  // Locally-saved cards overlay the loaded list (no effect-driven setState).
  const [overrides, setOverrides] = useState<Record<string, BuiltinTool>>({});
  const tools = useMemo(
    () =>
      toolsFromList(list.data)
        .filter((t) => isBuiltinToolName(t.name))
        .map((t) => overrides[t.name] ?? t),
    [list.data, overrides],
  );

  const missing = BUILTIN_TOOL_NAMES.filter((n) => !tools.some((t) => t.name === n));

  async function saveTool(name: BuiltinToolName, form: ToolFormValues): Promise<BuiltinTool> {
    const authedToken = mode === "oidc" ? "" : token;
    return apiPatch<BuiltinTool>(`/admin/tools/${name}`, authedToken, buildToolPayload(name, form));
  }

  function applyUpdated(updated: BuiltinTool) {
    setOverrides((prev) => ({ ...prev, [updated.name]: updated }));
  }

  return (
    <section>
      <div className="section-head">
        <h2>Built-in tools</h2>
      </div>
      {list.error ? <div className="notice error">{list.error}</div> : null}
      {list.loading && tools.length === 0 ? (
        <div className="notice">Loading built-in tool configuration…</div>
      ) : null}
      {missing.length > 0 && !list.loading && !list.error ? (
        <div className="notice">
          The control plane has not seeded: {missing.join(", ")}. (BT-2 migration pending?)
        </div>
      ) : null}
      <div className="entity-list">
        {tools.map((tool) => (
          <ToolCard key={tool.name} tool={tool} onSave={saveTool} onUpdated={applyUpdated} />
        ))}
      </div>
    </section>
  );
}

// ToolCard renders one tool's enable toggle + policy form. The card owns
// its form state; Save sends PATCH {enabled, policy} and the server's
// full config replaces the card on success.
export function ToolCard({
  tool,
  onSave,
  onUpdated,
}: {
  readonly tool: BuiltinTool;
  readonly onSave: (name: BuiltinToolName, form: ToolFormValues) => Promise<BuiltinTool>;
  readonly onUpdated: (updated: BuiltinTool) => void;
}) {
  const [form, setForm] = useState<ToolFormValues>(() => formFromTool(tool));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [savedNote, setSavedNote] = useState("");

  function setField<K extends keyof ToolFormValues>(key: K, value: ToolFormValues[K]) {
    setForm((f) => ({ ...f, [key]: value }));
    setSavedNote("");
  }

  function setDeniedPatterns(rows: string[]) {
    setField("deniedPatterns", rows);
  }

  async function save() {
    const clientErr = validateToolForm(tool.name, form);
    if (clientErr) {
      setError(clientErr);
      return;
    }
    setBusy(true);
    setError("");
    try {
      const updated = await onSave(tool.name, form);
      onUpdated(updated);
      setForm(formFromTool(updated));
      setSavedNote("Saved.");
    } catch (err) {
      // 400 → the server's policy validation message, inline.
      if (err instanceof ApiError && err.status === 403) {
        setError("Forbidden — this settings surface requires the platform_admin role.");
      } else {
        setError(toolSaveErrorMessage(err));
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card" style={{ marginBottom: "var(--space-4)" }}>
      <div className="section-head">
        <h3>{TOOL_LABELS[tool.name]}</h3>
        <label className="field" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
          <input
            type="checkbox"
            checked={form.enabled}
            disabled={busy}
            onChange={(e) => setField("enabled", e.target.checked)}
            aria-label={`Enable ${tool.name}`}
          />
          <span>{form.enabled ? "Enabled" : "Disabled"}</span>
        </label>
      </div>
      <p className="entity-meta" style={{ marginTop: 0 }}>
        {TOOL_HINTS[tool.name]}
      </p>

      <div className="field-row">
        <label className="field">
          <span>Timeout (seconds)</span>
          <input
            type="number"
            min={1}
            value={form.timeoutSeconds}
            disabled={busy}
            onChange={(e) => setField("timeoutSeconds", e.target.value)}
          />
        </label>
        {tool.name === "exec" ? (
          <label className="field">
            <span>Max output (bytes)</span>
            <input
              type="number"
              min={1}
              value={form.maxOutputBytes}
              disabled={busy}
              onChange={(e) => setField("maxOutputBytes", e.target.value)}
            />
          </label>
        ) : null}
        {tool.name === "web_fetch" ? (
          <label className="field">
            <span>Max bytes</span>
            <input
              type="number"
              min={1}
              value={form.maxBytes}
              disabled={busy}
              onChange={(e) => setField("maxBytes", e.target.value)}
            />
          </label>
        ) : null}
        {tool.name === "web_search" ? (
          <>
            <label className="field">
              <span>Max results</span>
              <input
                type="number"
                min={1}
                value={form.maxResults}
                disabled={busy}
                onChange={(e) => setField("maxResults", e.target.value)}
              />
            </label>
            <label className="field">
              <span>Provider</span>
              <select
                value={form.provider}
                disabled={busy}
                onChange={(e) => setField("provider", e.target.value)}
              >
                {SEARCH_PROVIDERS.map((p) => (
                  <option key={p} value={p}>
                    {p}
                  </option>
                ))}
              </select>
            </label>
          </>
        ) : null}
      </div>

      {tool.name === "web_fetch" ? (
        <label className="field" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
          <input
            type="checkbox"
            checked={form.allowPrivateNetwork}
            disabled={busy}
            onChange={(e) => setField("allowPrivateNetwork", e.target.checked)}
            aria-label="Allow private network targets"
          />
          <span>Allow private network (SSRF guard off for RFC1918/CGNAT/link-local)</span>
        </label>
      ) : null}

      {tool.name === "exec" ? (
        <DeniedPatternsEditor rows={form.deniedPatterns} disabled={busy} onChange={setDeniedPatterns} />
      ) : null}

      <div style={{ display: "flex", gap: 8, alignItems: "center", marginTop: "var(--space-4)" }}>
        <button type="button" className="btn btn-primary" disabled={busy} onClick={() => { void save(); }}>
          Save {tool.name}
        </button>
        {savedNote ? (
          <output className="notice" aria-live="polite">
            {savedNote}
          </output>
        ) : null}
        {error ? (
          <span className="notice error" role="alert">
            {error}
          </span>
        ) : null}
      </div>
      <p className="entity-meta" style={{ marginBottom: 0, marginTop: "var(--space-2)" }}>
        {formatToolUpdate(tool)}
      </p>
    </div>
  );
}

// DeniedPatternsEditor: add/remove rows of regex strings.
export function DeniedPatternsEditor({
  rows,
  disabled,
  onChange,
}: {
  readonly rows: string[];
  readonly disabled: boolean;
  readonly onChange: (rows: string[]) => void;
}) {
  return (
    <div className="field">
      <span>Denied patterns (regex, matched against the full command)</span>
      {rows.map((row, i) => (
        <div key={i} style={{ display: "flex", gap: 8, alignItems: "center", marginTop: 4 }}>
          <input
            type="text"
            value={row}
            disabled={disabled}
            style={{ flex: 1 }}
            placeholder="rm\\s+-rf\\s+/"
            onChange={(e) => {
              const next = [...rows];
              next[i] = e.target.value;
              onChange(next);
            }}
          />
          <button
            type="button"
            className="btn btn-sm btn-danger"
            disabled={disabled}
            aria-label={`Remove denied pattern ${i + 1}`}
            onClick={() => {
              onChange(rows.filter((_, idx) => idx !== i));
            }}
          >
            Remove
          </button>
        </div>
      ))}
      <div style={{ marginTop: 4 }}>
        <button
          type="button"
          className="btn btn-sm"
          disabled={disabled}
          onClick={() => {
            onChange([...rows, ""]);
          }}
        >
          + Add pattern
        </button>
      </div>
    </div>
  );
}
