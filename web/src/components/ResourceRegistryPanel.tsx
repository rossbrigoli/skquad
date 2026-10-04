"use client";

// S-204 follow-up: shared registry resource list + register/edit modal.
//
// Extracted from the Settings "Resources" tab so the new real routes
// (/settings/resources/<type>) and any in-page tab can render the same
// classic entity-list surface without duplicating the CRUD code. The
// unified Tools tile grid (ToolsPanel) stays separate — tools have their
// own presentation; this panel covers the classic list types and also
// provides the register modal the Tools page header uses.

import { useState } from "react";
import { apiPatch, apiPost, type RegistryResource } from "../lib/api";
import { useApi } from "../lib/useApi";
import { useAuth } from "../lib/auth";
import { DeleteResourceButton } from "./DeleteResourceButton";
import { EmptyState } from "./EmptyState";
import { Modal, ModalForm } from "./Modal";
import { StatusChip } from "./StatusChip";

// resourceLabel turns a registry type key into the register-button label
// ("skills" → "skill", "knowledge-bases" → "knowledge base").
export function resourceLabel(type: string): string {
  return type.replace(/-/g, " ").replace(/s$/, "");
}

// statusChipFor maps a lifecycle status to the StatusChip variant.
function statusChipFor(status: string): "idle" | "paused" | "error" {
  if (status === "active") return "idle";
  if (status === "deprecated") return "paused";
  return "error";
}

function parseJsonField(value: string, label: string): string | undefined {
  const trimmed = value.trim();
  if (trimmed === "") return undefined;
  JSON.parse(trimmed); // throws → caller surfaces message
  if (typeof JSON.parse(trimmed) !== "object") throw new Error(`${label} must be a JSON object or array`);
  return trimmed;
}

export function ResourceRegistryPanel({
  resourceType,
  label,
  isAdmin,
}: {
  readonly resourceType: string;
  readonly label: string;
  readonly isAdmin: boolean;
}) {
  const resources = useApi<RegistryResource[]>(`/registry/${resourceType}`, 60000);
  const [editing, setEditing] = useState<RegistryResource | null>(null);
  const [creating, setCreating] = useState(false);
  const items = resources.data || [];

  return (
    <section>
      <div className="section-head">
        <h2>{label}</h2>
        {isAdmin ? (
          <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
            + Register {resourceLabel(resourceType)}
          </button>
        ) : null}
      </div>
      {resources.error ? <div className="notice error">{resources.error}</div> : null}
      {items.length === 0 && !resources.loading ? (
        <EmptyState title={`No ${label.toLowerCase()} registered`} hint="Register one so agents can be granted access." />
      ) : (
        <div className="entity-list">
          {items.map((r) => (
            <div key={r.id} className="entity-row">
              <div className="entity-main">
                <span className="entity-title">{r.name}</span>
                <span className="entity-meta">{r.description ?? r.endpoint ?? "—"}</span>
              </div>
              <div className="entity-side">
                <StatusChip status={statusChipFor(r.status)} />
                {isAdmin ? (
                  <button type="button" className="btn btn-sm" onClick={() => setEditing(r)}>
                    Edit
                  </button>
                ) : null}
                {isAdmin ? (
                  <DeleteResourceButton
                    path={`/registry/${resourceType}/${r.id}`}
                    name={r.name}
                    onDeleted={() => {
                      resources.refresh();
                    }}
                  />
                ) : null}
              </div>
            </div>
          ))}
        </div>
      )}
      {(creating || editing) && (
        <ResourceModal
          resourceType={resourceType}
          resource={editing}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
          onSaved={() => {
            setCreating(false);
            setEditing(null);
            resources.refresh();
          }}
        />
      )}
    </section>
  );
}

export function ResourceModal({
  resourceType,
  resource,
  onClose,
  onSaved,
}: {
  readonly resourceType: string;
  readonly resource: RegistryResource | null;
  readonly onClose: () => void;
  readonly onSaved: () => void;
}) {
  const { token } = useAuth();
  const [name, setName] = useState(resource?.name ?? "");
  const [description, setDescription] = useState(resource?.description ?? "");
  const [endpoint, setEndpoint] = useState(resource?.endpoint ?? "");
  const [authRef, setAuthRef] = useState(resource?.auth_ref ?? "");
  const [manifest, setManifest] = useState(resource?.manifest ? JSON.stringify(resource.manifest, null, 2) : "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  return (
    <Modal title={resource ? `Edit “${resource.name}”` : `Register ${resourceLabel(resourceType)}`} onClose={onClose}>
      <ModalForm
        busy={busy}
        error={error}
        submitLabel={resource ? "Save changes" : "Register"}
        submitDisabled={name.trim() === ""}
        onCancel={onClose}
        onSubmit={async () => {
          setBusy(true);
          setError("");
          try {
            const body = {
              name: name.trim(),
              description: description.trim(),
              endpoint: endpoint.trim(),
              auth_ref: authRef.trim(),
              manifest: parseJsonField(manifest, "Manifest"),
            };
            if (resource) {
              await apiPatch(`/registry/${resourceType}/${resource.id}`, token, body);
            } else {
              await apiPost(`/registry/${resourceType}`, token, body);
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
          <input value={name} onChange={(e) => setName(e.target.value)} autoFocus />
        </label>
        <label className="field">
          <span>Description</span>
          <textarea value={description} onChange={(e) => setDescription(e.target.value)} />
        </label>
        <div className="field-row">
          <label className="field">
            <span>Endpoint</span>
            <input value={endpoint} onChange={(e) => setEndpoint(e.target.value)} placeholder="https://… (if applicable)" />
          </label>
          <label className="field">
            <span>Auth ref</span>
            <input value={authRef} onChange={(e) => setAuthRef(e.target.value)} placeholder="secret / vault ref (never the secret itself)" />
          </label>
        </div>
        <label className="field">
          <span>Manifest (JSON, optional)</span>
          <textarea value={manifest} onChange={(e) => setManifest(e.target.value)} placeholder='{"version": 1, ...}' />
        </label>
      </ModalForm>
    </Modal>
  );
}
