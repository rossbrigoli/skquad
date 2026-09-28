"use client";

// S-158: "Prompt Templates" admin settings panel. Platform admins create
// and edit the reusable prompt starting points shown in the squad/agent
// create pickers. Gated in the settings page (mirrors the other admin
// tabs); the server enforces platform_admin on every mutation.

import { useState } from "react";
import { Modal, ModalForm } from "./Modal";
import { useAuth } from "../lib/auth";
import {
  createPromptTemplate,
  deletePromptTemplate,
  emptyTemplateForm,
  formFromTemplate,
  updatePromptTemplate,
  validateTemplateForm,
  type PromptTemplate,
  type PromptTemplateFormValues,
  type TemplateAppliesTo,
} from "../lib/promptTemplates";

const APPLIES_LABELS: Record<TemplateAppliesTo, string> = {
  agent: "Agents",
  squad: "Squads",
  both: "Squads + Agents",
};

export function PromptTemplatesPanel({ templates }: { readonly templates: PromptTemplate[] }) {
  const { token } = useAuth();
  const [editing, setEditing] = useState<PromptTemplate | null>(null);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");

  return (
    <section className="panel">
      <div className="panel-header">
        <h2>Prompt Templates</h2>
        <button type="button" className="primary" onClick={() => setCreating(true)}>
          + New template
        </button>
      </div>
      {error ? <div className="error-banner">{error}</div> : null}
      {templates.length === 0 ? (
        <p className="empty">No prompt templates yet. Create one to give new squads and agents a head start.</p>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Applies to</th>
              <th>Description</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            {templates.map((t) => (
              <tr key={t.id}>
                <td>{t.name}</td>
                <td>{APPLIES_LABELS[t.applies_to] ?? t.applies_to}</td>
                <td>{t.description}</td>
                <td>
                  <button type="button" onClick={() => setEditing(t)}>
                    Edit
                  </button>
                  <button
                    type="button"
                    className="danger"
                    onClick={async () => {
                      if (!window.confirm(`Delete template "${t.name}"? Existing squads/agents are unaffected.`)) return;
                      try {
                        setError("");
                        await deletePromptTemplate(token, t.id);
                        window.location.reload();
                      } catch (err) {
                        setError(err instanceof Error ? err.message : "delete failed");
                      }
                    }}
                  >
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {creating ? (
        <TemplateEditorModal
          title="New prompt template"
          initial={emptyTemplateForm()}
          onClose={() => setCreating(false)}
          onSaved={() => {
            setCreating(false);
            window.location.reload();
          }}
        />
      ) : null}
      {editing ? (
        <TemplateEditorModal
          title={`Edit template: ${editing.name}`}
          templateId={editing.id}
          initial={formFromTemplate(editing)}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            window.location.reload();
          }}
        />
      ) : null}
    </section>
  );
}

function TemplateEditorModal({
  title,
  templateId,
  initial,
  onClose,
  onSaved,
}: {
  title: string;
  templateId?: string;
  initial: PromptTemplateFormValues;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { token } = useAuth();
  const [values, setValues] = useState<PromptTemplateFormValues>(initial);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  function setField<K extends keyof PromptTemplateFormValues>(key: K, value: PromptTemplateFormValues[K]) {
    setValues((prev) => ({ ...prev, [key]: value }));
  }

  return (
    <Modal title={title} onClose={onClose}>
      <ModalForm
        busy={busy}
        error={error}
        submitLabel="Save template"
        submitDisabled={validateTemplateForm(values) !== ""}
        onCancel={onClose}
        onSubmit={async () => {
          const problem = validateTemplateForm(values);
          if (problem) {
            setError(problem);
            return;
          }
          setBusy(true);
          setError("");
          try {
            if (templateId) {
              await updatePromptTemplate(token, templateId, values);
            } else {
              await createPromptTemplate(token, values);
            }
            onSaved();
          } catch (err) {
            setError(err instanceof Error ? err.message : "save failed");
            setBusy(false);
          }
        }}
      >
        <label className="field">
          <span>Name</span>
          <input value={values.name} onChange={(e) => setField("name", e.target.value)} placeholder="e.g. Code Reviewer" autoFocus />
        </label>
        <label className="field">
          <span>Applies to</span>
          <select value={values.applies_to} onChange={(e) => setField("applies_to", e.target.value as TemplateAppliesTo)}>
            <option value="agent">Agents</option>
            <option value="squad">Squads</option>
            <option value="both">Squads + Agents</option>
          </select>
        </label>
        <label className="field">
          <span>Description</span>
          <input
            value={values.description}
            onChange={(e) => setField("description", e.target.value)}
            placeholder="What this template is for (shown in the picker)"
          />
        </label>
        <label className="field">
          <span>Content</span>
          <textarea
            rows={10}
            value={values.content}
            onChange={(e) => setField("content", e.target.value)}
            placeholder="The system prompt this template pre-populates"
          />
          <small className="field-hint">
            Template variables like {"{{agent.name}}"} are substituted at compose time. The content is copied into the
            draft — editing this template later does not change squads or agents already created from it.
          </small>
        </label>
      </ModalForm>
    </Modal>
  );
}
