"use client";

import { useState } from "react";
import { Modal, ModalForm } from "./Modal";
import type { Agent, LLMProvider } from "../lib/api";

export type AgentFormValues = {
  name: string;
  role: string;
  system_prompt: string;
  default_provider_id: string;
  default_model: string;
  idle_timeout_sec: number;
};

// Model options from a provider's `models` field, which is free-form JSON:
// accepts ["gpt-x", ...], [{id|name: ...}], or falls back to default_model.
export function providerModelOptions(provider?: LLMProvider): string[] {
  if (!provider) return [];
  const out: string[] = [];
  const push = (v: unknown) => {
    if (typeof v === "string" && v.trim() !== "" && !out.includes(v)) out.push(v);
    else if (v && typeof v === "object") {
      const rec = v as Record<string, unknown>;
      const id = rec.id ?? rec.name ?? rec.model;
      if (typeof id === "string" && id.trim() !== "" && !out.includes(id)) out.push(id);
    }
  };
  const models = provider.models;
  if (Array.isArray(models)) models.forEach(push);
  else if (models && typeof models === "object") {
    const vals = (models as Record<string, unknown>).list ?? (models as Record<string, unknown>).models;
    if (Array.isArray(vals)) vals.forEach(push);
  }
  if (provider.default_model && !out.includes(provider.default_model)) out.unshift(provider.default_model);
  return out;
}

export function AgentFormModal({
  providers,
  initial,
  title,
  submitLabel,
  onSubmit,
  onClose,
}: {
  providers: LLMProvider[];
  initial?: Partial<Agent>;
  title: string;
  submitLabel: string;
  onSubmit: (values: AgentFormValues) => Promise<void>;
  onClose: () => void;
}) {
  const [name, setName] = useState(initial?.name || "");
  const [role, setRole] = useState(initial?.role || "");
  const [systemPrompt, setSystemPrompt] = useState(initial?.system_prompt || "");
  const [providerId, setProviderId] = useState(initial?.default_provider_id || "");
  const [model, setModel] = useState(initial?.default_model || "");
  const [idleTimeout, setIdleTimeout] = useState(String(initial?.idle_timeout_sec ?? 300));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const activeProviders = providers.filter((p) => (p.status || "active") === "active");
  const selected = activeProviders.find((p) => p.id === providerId);
  const modelOptions = providerModelOptions(selected);

  return (
    <Modal title={title} onClose={onClose}>
      <ModalForm
        busy={busy}
        error={error}
        submitLabel={submitLabel}
        submitDisabled={name.trim() === ""}
        onCancel={onClose}
        onSubmit={async () => {
          setBusy(true);
          setError("");
          try {
            await onSubmit({
              name: name.trim(),
              role: role.trim(),
              system_prompt: systemPrompt,
              default_provider_id: providerId,
              default_model: model.trim(),
              idle_timeout_sec: Number(idleTimeout) > 0 ? Number(idleTimeout) : 300,
            });
          } catch (err) {
            setError(err instanceof Error ? err.message : "submit failed");
            setBusy(false);
          }
        }}
      >
        <label className="field">
          <span>Name</span>
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. coder-1" autoFocus />
        </label>
        <div className="field-row">
          <label className="field">
            <span>Role</span>
            <input value={role} onChange={(e) => setRole(e.target.value)} placeholder="e.g. implementer" />
          </label>
          <label className="field">
            <span>Idle timeout (sec)</span>
            <input
              value={idleTimeout}
              onChange={(e) => setIdleTimeout(e.target.value.replace(/[^0-9]/g, ""))}
              inputMode="numeric"
            />
          </label>
        </div>
        <label className="field">
          <span>LLM provider</span>
          <select
            value={providerId}
            onChange={(e) => {
              setProviderId(e.target.value);
              const next = activeProviders.find((p) => p.id === e.target.value);
              if (next?.default_model) setModel(next.default_model);
            }}
          >
            <option value="">— platform default —</option>
            {activeProviders.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name} ({p.kind})
              </option>
            ))}
          </select>
          {activeProviders.length === 0 ? (
            <span className="field-hint">No active providers registered — add one in Settings → LLM providers.</span>
          ) : null}
        </label>
        <label className="field">
          <span>Default model</span>
          {modelOptions.length > 0 ? (
            <select value={model} onChange={(e) => setModel(e.target.value)}>
              <option value="">— provider default —</option>
              {modelOptions.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          ) : (
            <input value={model} onChange={(e) => setModel(e.target.value)} placeholder="e.g. gpt-5.5" />
          )}
          {selected && modelOptions.length === 0 ? (
            <span className="field-hint">This provider declares no model list — type the model id.</span>
          ) : null}
        </label>
        <label className="field">
          <span>System prompt</span>
          <textarea
            value={systemPrompt}
            onChange={(e) => setSystemPrompt(e.target.value)}
            placeholder="Persona and operating instructions for this agent"
          />
        </label>
      </ModalForm>
    </Modal>
  );
}
