"use client";

import { useState } from "react";
import { Modal, ModalForm } from "./Modal";
import type { Agent } from "../lib/api";

// WP8 step-4 cutover: the legacy default_provider_id / default_model
// fields are gone from this form. Model binding is done exclusively via
// the agent's LLM model tab (WP7) against the ai_models registry —
// creating or editing an agent here no longer touches legacy fields.
export type AgentFormValues = {
  name: string;
  role: string;
  system_prompt: string;
  idle_timeout_sec: number;
};

export function AgentFormModal({
  initial,
  title,
  submitLabel,
  onSubmit,
  onClose,
}: {
  initial?: Partial<Agent>;
  title: string;
  submitLabel: string;
  onSubmit: (values: AgentFormValues) => Promise<void>;
  onClose: () => void;
}) {
  const [name, setName] = useState(initial?.name || "");
  const [role, setRole] = useState(initial?.role || "");
  const [systemPrompt, setSystemPrompt] = useState(initial?.system_prompt || "");
  const [idleTimeout, setIdleTimeout] = useState(String(initial?.idle_timeout_sec ?? 300));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

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
          <span>System prompt</span>
          <textarea
            value={systemPrompt}
            onChange={(e) => setSystemPrompt(e.target.value)}
            placeholder="Persona and operating instructions for this agent"
          />
        </label>
        <p className="field-hint">
          Model binding: set the primary (and optional fallback) AI model on the agent's page after saving —
          the LLM model section binds models from the admin registry.
        </p>
      </ModalForm>
    </Modal>
  );
}
