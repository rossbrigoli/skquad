"use client";

import { useState } from "react";
import { Modal, ModalForm } from "./Modal";
import type { Agent } from "../lib/api";
import {
  DEFAULT_AGENT_STORAGE_SIZE,
  STORAGE_PRESETS,
  isValidStorageSize,
} from "../lib/agentStorage";

// WP8 step-4 cutover: the legacy default_provider_id / default_model
// fields are gone from this form. Model binding is done exclusively via
// the agent's LLM model tab (WP7) against the ai_models registry —
// creating or editing an agent here no longer touches legacy fields.
//
// S-138 adds durable workspace storage: owners choose whether the agent
// gets a persistent PVC and how large. storageClass is platform-admin
// only and deliberately not exposed here.
export type AgentFormValues = {
  name: string;
  role: string;
  system_prompt: string;
  idle_timeout_sec: number;
  storage_enabled: boolean;
  storage_size: string;
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
  const [name, setName] = useState(initial?.name ?? "");
  const [role, setRole] = useState(initial?.role ?? "");
  const [systemPrompt, setSystemPrompt] = useState(initial?.system_prompt ?? "");
  const [idleTimeout, setIdleTimeout] = useState(String(initial?.idle_timeout_sec ?? 300));
  const [storageEnabled, setStorageEnabled] = useState(initial?.storage_enabled ?? false);
  const [storageSize, setStorageSize] = useState(initial?.storage_size || DEFAULT_AGENT_STORAGE_SIZE);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  return (
    <Modal title={title} onClose={onClose}>
      <ModalForm
        busy={busy}
        error={error}
        submitLabel={submitLabel}
        submitDisabled={name.trim() === "" || (storageEnabled && !isValidStorageSize(storageSize))}
        onCancel={onClose}
        onSubmit={async () => {
          if (storageEnabled && !isValidStorageSize(storageSize)) {
            setError("Enter a valid storage size, e.g. 1Gi, 2Gi or 500M.");
            return;
          }
          setBusy(true);
          setError("");
          try {
            await onSubmit({
              name: name.trim(),
              role: role.trim(),
              system_prompt: systemPrompt,
              idle_timeout_sec: Number(idleTimeout) > 0 ? Number(idleTimeout) : 300,
              storage_enabled: storageEnabled,
              storage_size: storageEnabled ? storageSize.trim() : "",
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
        <div className="field">
          <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", cursor: "pointer" }}>
            <input
              type="checkbox"
              checked={storageEnabled}
              onChange={(e) => setStorageEnabled(e.target.checked)}
            />
            <span>Durable workspace storage</span>
          </label>
          {storageEnabled ? (
            <>
              <div style={{ display: "flex", gap: "0.4rem", marginTop: "0.5rem", flexWrap: "wrap" }}>
                {STORAGE_PRESETS.map((preset) => (
                  <button
                    key={preset}
                    type="button"
                    className={`btn btn-sm${storageSize === preset ? " btn-primary" : ""}`}
                    onClick={() => setStorageSize(preset)}
                  >
                    {preset}
                  </button>
                ))}
              </div>
              <input
                value={storageSize}
                onChange={(e) => setStorageSize(e.target.value)}
                placeholder="custom, e.g. 3Gi"
                style={{ marginTop: "0.5rem" }}
                aria-label="Storage size"
              />
              {!isValidStorageSize(storageSize) ? (
                <p className="field-hint" style={{ color: "var(--danger, #c0392b)" }}>
                  Must be a positive quantity like 1Gi, 2Gi or 500M.
                </p>
              ) : null}
            </>
          ) : null}
          <p className="field-hint">
            Durable workspace storage that survives restarts. The platform caps the maximum size;
            the storage class is managed by your platform admin.
          </p>
        </div>
        <p className="field-hint">
          Model binding: set the primary (and optional fallback) AI model on the agent's page after saving —
          the LLM model section binds models from the admin registry.
        </p>
      </ModalForm>
    </Modal>
  );
}
