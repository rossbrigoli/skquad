"use client";

// BT-4 (S-151) / S-204: built-in tool configuration form.
// ToolCard renders one built-in tool (exec / web_fetch / web_search /
// send_message) with an enable toggle, the tool's pinned policy form
// (ADR-0012 §3), inline validation errors from the server (400),
// success feedback, and the per-tool updatedAt/updatedBy line.
//
// S-204: built-in tools are no longer a separate settings screen —
// they live on the unified Tools page and each tool's form opens on the
// tool configuration route (/settings/resources/tools/{name}). This
// component is the form itself; loading/saving lives in the route.

import { useId, useMemo, useState } from "react";
import { apiPatch, ApiError } from "../lib/api";
import { useAuth } from "../lib/auth";
import { useApi } from "../lib/useApi";
import {
  buildToolPayload,
  SEARCH_PROVIDERS,
  formFromTool,
  formatToolUpdate,
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
  send_message: "send_message — agent-to-agent messaging",
};

const TOOL_HINTS: Record<BuiltinToolName, string> = {
  exec: "Runs inside the agent pod (the container is the sandbox). Denied patterns are regexes matched against the full command before execution.",
  web_fetch: "GET only, ≤3 redirects with an SSRF re-check per hop. Private-network targets stay blocked unless explicitly allowed.",
  web_search: "Provider API keys live only in the control plane; agents call the search proxy and never see credentials.",
  send_message: "Queued through the control plane: same-squad always allowed, cross-squad needs an access grant. Reply threads are capped at 12 messages per correlation chain (S-164).",
};

// BuiltinToolConfig is the admin config surface for ONE built-in tool,
// loaded from GET /admin/tools (platform_admin only). Rendered by the
// S-204 tool configuration route. Built-in tools are shipped with the
// platform and cannot be deleted — there is deliberately no delete
// affordance (and no delete route on the API).
export function BuiltinToolConfig({ name }: { readonly name: BuiltinToolName }) {
  const { token, mode } = useAuth();
  const list = useApi<BuiltinToolsList>("/admin/tools", 0);
  // Locally-saved config overlays the loaded list (no effect-driven setState).
  const [saved, setSaved] = useState<BuiltinTool | null>(null);

  const tool = useMemo(() => {
    if (saved && saved.name === name) return saved;
    return toolsFromList(list.data).find((t) => t.name === name) ?? null;
  }, [list.data, saved, name]);

  async function saveTool(n: BuiltinToolName, form: ToolFormValues): Promise<BuiltinTool> {
    const authedToken = mode === "oidc" ? "" : token;
    return apiPatch<BuiltinTool>(`/admin/tools/${n}`, authedToken, buildToolPayload(n, form));
  }

  return (
    <>
      <div className="section-head" style={{ marginTop: "var(--space-3)" }}>
        <h1 className="page-title">Built-in tool: {name}</h1>
        <span className="tool-badge">Built-in</span>
      </div>
      <p className="entity-meta">
        Built-in tools ship pre-installed with the platform. They can be enabled/disabled and
        policy-tuned here, but they cannot be deleted.
      </p>
      {list.error ? <div className="notice error">{list.error}</div> : null}
      {!tool && list.loading ? <div className="notice">Loading tool configuration…</div> : null}
      {!tool && !list.loading && !list.error ? (
        <div className="notice">
          The control plane has not seeded <code>{name}</code>. (BT-2 migration pending?)
        </div>
      ) : null}
      {tool ? (
        <ToolCard
          tool={tool}
          onSave={saveTool}
          onUpdated={(updated) => setSaved(updated)}
        />
      ) : null}
    </>
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
        <h3>
          {TOOL_LABELS[tool.name]}{" "}
          <span className="entity-meta">
            ({form.enabled ? "status: ENABLED" : "status: disabled"})
          </span>
        </h3>
        <label className="field" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
          <input
            type="checkbox"
            checked={form.enabled}
            disabled={busy}
            onChange={(e) => setField("enabled", e.target.checked)}
            aria-label={`Enable ${tool.name}`}
          />
          <span>Enable this tool</span>
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
        {tool.name === "send_message" ? (
          <label className="field">
            <span>Max message chars</span>
            <input
              type="number"
              min={1}
              value={form.maxMessageChars}
              disabled={busy}
              onChange={(e) => setField("maxMessageChars", e.target.value)}
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
let patternSeq = 0;
export function DeniedPatternsEditor({
  rows,
  disabled,
  onChange,
}: {
  readonly rows: string[];
  readonly disabled: boolean;
  readonly onChange: (rows: string[]) => void;
}) {
  // S-189: stable per-row ids so editing a row never remounts it (content
  // keys would lose focus on every keystroke). Ids are owned by this
  // component and updated alongside every add/remove it performs; the
  // parent's rows only change through these handlers.
  const uid = useId();
  const [rowIds, setRowIds] = useState<string[]>(() => rows.map(() => `${uid}-p${++patternSeq}`));
  return (
    <div className="field">
      <span>Denied patterns (regex, matched against the full command)</span>
      {rows.map((row, i) => (
        <div key={rowIds[i]} style={{ display: "flex", gap: 8, alignItems: "center", marginTop: 4 }}>
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
              setRowIds((prev) => prev.filter((_, idx) => idx !== i));
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
            setRowIds((prev) => [...prev, `${uid}-p${++patternSeq}`]);
            onChange([...rows, ""]);
          }}
        >
          + Add pattern
        </button>
      </div>
    </div>
  );
}
