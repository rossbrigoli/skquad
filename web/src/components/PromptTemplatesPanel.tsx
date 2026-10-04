"use client";

// S-158: "Prompt Templates" admin settings panel. Platform admins create
// and edit the reusable prompt starting points shown in the squad/agent
// create pickers. Gated in the settings page (mirrors the other admin
// tabs); the server enforces platform_admin on every mutation.
//
// S-214: restyled to the shared app design system — .section-head,
// .entity-list/.entity-row rows, .btn/.btn-primary/.btn-danger controls,
// EmptyState and ConfirmDialog — replacing the one-off .panel/.table
// classes that never existed in globals.css. Adds checkbox multi-select
// (header checkbox with indeterminate state) and a top "Delete selected"
// bulk action backed by DELETE /prompt-templates/bulk.

import { useEffect, useMemo, useRef, useState } from "react";
import { Modal, ModalForm } from "./Modal";
import { ConfirmDialog } from "./ConfirmDialog";
import { EmptyState } from "./EmptyState";
import { useAuth } from "../lib/auth";
import {
  bulkDeletePromptTemplates,
  createPromptTemplate,
  deletePromptTemplate,
  emptyTemplateForm,
  formFromTemplate,
  pruneTemplateSelection,
  selectAllTemplates,
  toggleTemplateSelection,
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

export function PromptTemplatesPanel({
  templates,
  loading = false,
  onChanged,
}: {
  readonly templates: PromptTemplate[];
  readonly loading?: boolean;
  readonly onChanged?: () => void;
}) {
  const { token } = useAuth();
  const [editing, setEditing] = useState<PromptTemplate | null>(null);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const [pendingBulk, setPendingBulk] = useState(false);
  const [pendingSingle, setPendingSingle] = useState<PromptTemplate | null>(null);
  const [bulkBusy, setBulkBusy] = useState(false);
  const headerCheckbox = useRef<HTMLInputElement>(null);

  function renderTemplateList() {
    if (loading && templates.length === 0) {
      return <p className="field-hint">Loading templates…</p>;
    }
    if (templates.length === 0) {
      return (
        <EmptyState
          title="No prompt templates yet"
          hint="Create one to give new squads and agents a head start."
        />
      );
    }
    return (
      <div className="entity-list">
        {templates.map((t) => (
          <div key={t.id} className="entity-row">
            <input
              type="checkbox"
              className="entity-checkbox"
              checked={selected.has(t.id)}
              onChange={() => setSelectedIds((prev) => toggleTemplateSelection(prev, t.id))}
              aria-label={`Select template ${t.name}`}
            />
            <div className="entity-main">
              <span className="entity-title">{t.name}</span>
              <span className="entity-meta">{t.description || "—"}</span>
            </div>
            <div className="entity-side">
              <span className="entity-meta">{APPLIES_LABELS[t.applies_to] ?? t.applies_to}</span>
              <button type="button" className="btn btn-sm" onClick={() => setEditing(t)}>
                Edit
              </button>
            </div>
          </div>
        ))}
      </div>
    );
  }

  // Selection is pruned against the live list at render time so deleted
  // or vanished templates can never linger in the selection.
  const selected = useMemo(() => pruneTemplateSelection(selectedIds, templates), [selectedIds, templates]);
  const selectionSize = selected.size;
  const allSelected = templates.length > 0 && templates.every((t) => selected.has(t.id));
  const someSelected = selectionSize > 0 && !allSelected;

  // Header checkbox: checked when all selected, indeterminate when a
  // subset is (same pattern as the S-207 inbox).
  useEffect(() => {
    if (headerCheckbox.current) {
      headerCheckbox.current.indeterminate = someSelected;
    }
  }, [someSelected]);

  async function doBulkDelete() {
    const ids = [...selected];
    if (ids.length === 0) return;
    setBulkBusy(true);
    setError("");
    try {
      const deleted = await bulkDeletePromptTemplates(token, ids);
      setNotice(`Deleted ${deleted} template${deleted === 1 ? "" : "s"}.`);
      setSelectedIds(new Set());
      onChanged?.();
    } catch (err) {
      setError(err instanceof Error ? err.message : "bulk delete failed");
    } finally {
      setBulkBusy(false);
      setPendingBulk(false);
    }
  }

  async function doSingleDelete(template: PromptTemplate) {
    setError("");
    try {
      await deletePromptTemplate(token, template.id);
      setNotice(`Deleted template “${template.name}”.`);
      onChanged?.();
    } catch (err) {
      setError(err instanceof Error ? err.message : "delete failed");
    } finally {
      setPendingSingle(null);
    }
  }

  return (
    <section>
      <div className="section-head">
        <h2>Prompt Templates</h2>
        <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
          + New template
        </button>
      </div>
      <p className="field-hint">
        Reusable starting points for squad and agent prompts. Picking a template copies its content into the
        draft — editing a template later does not change what was already created from it.
      </p>
      {error ? <div className="notice error">{error}</div> : null}
      {notice && !error ? <div className="notice">{notice}</div> : null}

      {templates.length > 0 ? (
        <div className="templates-toolbar" role="group" aria-label="Template bulk actions">
          <label className="templates-selectall">
            <input
              ref={headerCheckbox}
              type="checkbox"
              checked={allSelected}
              onChange={(e) => setSelectedIds(selectAllTemplates(templates, e.target.checked))}
              aria-label="Select all templates"
            />
            <span>Select all</span>
          </label>
          {selectionSize > 0 ? <span className="bulk-count">{selectionSize} selected</span> : null}
          <button
            type="button"
            className="btn btn-sm btn-danger"
            disabled={selectionSize === 0 || bulkBusy}
            aria-label={`Delete ${selectionSize} selected templates`}
            onClick={() => setPendingBulk(true)}
          >
            {bulkBusy ? "Deleting…" : "Delete selected"}
          </button>
        </div>
      ) : null}

      {renderTemplateList()}

      {pendingBulk ? (
        <ConfirmDialog
          title="Delete selected templates?"
          body={`This will permanently delete ${selectionSize} prompt template${selectionSize === 1 ? "" : "s"}. Existing squads and agents are unaffected.`}
          confirmLabel={`Delete ${selectionSize}`}
          onConfirm={doBulkDelete}
          onClose={() => setPendingBulk(false)}
        />
      ) : null}

      {pendingSingle ? (
        <ConfirmDialog
          title={`Delete “${pendingSingle.name}”?`}
          body="This will permanently delete this template. Existing squads and agents are unaffected."
          onConfirm={() => doSingleDelete(pendingSingle)}
          onClose={() => setPendingSingle(null)}
        />
      ) : null}

      {creating ? (
        <TemplateEditorModal
          title="New prompt template"
          initial={emptyTemplateForm()}
          onClose={() => setCreating(false)}
          onSaved={() => {
            setCreating(false);
            setNotice("Template created.");
            onChanged?.();
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
            setNotice("Template saved.");
            onChanged?.();
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
  readonly title: string;
  readonly templateId?: string;
  readonly initial: PromptTemplateFormValues;
  readonly onClose: () => void;
  readonly onSaved: () => void;
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
