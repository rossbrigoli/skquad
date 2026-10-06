"use client";

// TG-4 (S-250): BYO REST resource registration panel.
//
// Admin/owner surface for registering governed REST resources with
// write-only credentials (stored as managed K8s Secrets by the control
// plane — never re-displayed) and a policy ceiling (methods, path
// allow/deny, size caps, rate, egress class). Grants to agents reuse the
// existing agent-page grant flow; this page shows the resource list with
// a read-only ceiling summary.

import { useState } from "react";
import { apiPatch, apiPost, type RegistryResource } from "../lib/api";
import { useApi } from "../lib/useApi";
import { useAuth } from "../lib/auth";
import { DeleteResourceButton } from "./DeleteResourceButton";
import { EmptyState } from "./EmptyState";
import { Modal, ModalForm } from "./Modal";
import { StatusChip } from "./StatusChip";
import {
  REST_AUTH_FIELDS,
  REST_AUTH_KINDS,
  REST_HTTP_METHODS,
  buildRestResourcePayload,
  emptyRestForm,
  restCeilingSummary,
  validateRestForm,
  type RestAuthKind,
  type RestCeiling,
  type RestEndpointConfig,
  type RestResourceForm,
} from "../lib/restResources";

function asObject(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};
}

function asStringArray(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((v): v is string => typeof v === "string") : [];
}

function asNumberOpt(value: unknown): string {
  return typeof value === "number" ? String(value) : "";
}

// restFormFromResource prefills the edit form from a registry row.
// Secret fields are NEVER prefilled — the API never returns them.
export function restFormFromResource(resource: RegistryResource): RestResourceForm {
  const cfg = asObject(resource.endpoint_config) as Partial<RestEndpointConfig>;
  const ceiling = asObject(resource.policy_ceiling) as RestCeiling;
  return {
    ...emptyRestForm(),
    name: resource.name ?? "",
    description: resource.description ?? "",
    baseUrl: cfg.base_url ?? "",
    authKind: (cfg.auth_kind ?? "none") as RestAuthKind,
    headerName: cfg.header_name ?? "",
    methods: ceiling.methods ?? [],
    pathAllow: (ceiling.path_allow ?? []).join(", "),
    pathDeny: (ceiling.path_deny ?? []).join(", "),
    maxRequestBytes: asNumberOpt(ceiling.max_request_bytes),
    maxResponseBytes: asNumberOpt(ceiling.max_response_bytes),
    ratePerMin: asNumberOpt(ceiling.rate_per_min),
    egressClass: ceiling.egress_class === "internal" ? "internal" : "public",
  };
}

export function RestResourcePanel({ isAdmin }: { readonly isAdmin: boolean }) {
  const resources = useApi<RegistryResource[]>("/registry/apis", 60000);
  const [editing, setEditing] = useState<RegistryResource | null>(null);
  const [creating, setCreating] = useState(false);
  const items = resources.data || [];

  return (
    <section>
      <div className="section-head">
        <h2>REST APIs (BYO)</h2>
        {isAdmin ? (
          <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
            + Register REST resource
          </button>
        ) : null}
      </div>
      {resources.error ? <div className="notice error">{resources.error}</div> : null}
      {items.length === 0 && !resources.loading ? (
        <EmptyState title="No REST resources registered" hint="Register a BYO REST API so agents can be granted governed access." />
      ) : (
        <div className="entity-list">
          {items.map((r) => (
            <div key={r.id} className="entity-row">
              <div className="entity-main">
                <span className="entity-title">{r.name}</span>
                <span className="entity-meta mono">{restCeilingSummary(asObject(r.policy_ceiling) as RestCeiling)}</span>
                <span className="entity-meta">
                  auth: {String(asObject(r.endpoint_config).auth_kind ?? "none")}
                  {asObject(r.endpoint_config).header_name ? ` (${String(asObject(r.endpoint_config).header_name)})` : ""}
                </span>
              </div>
              <div className="entity-side">
                <StatusChip status={r.status === "active" ? "idle" : r.status === "deprecated" ? "paused" : "error"} />
                {isAdmin ? (
                  <button type="button" className="btn btn-sm" onClick={() => setEditing(r)}>
                    Edit
                  </button>
                ) : null}
                {isAdmin ? (
                  <DeleteResourceButton
                    path={`/registry/apis/${r.id}`}
                    name={r.name}
                    onDeleted={() => resources.refresh()}
                  />
                ) : null}
              </div>
            </div>
          ))}
        </div>
      )}
      {(creating || editing) && (
        <RestResourceModal
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

export function RestResourceModal({
  resource,
  onClose,
  onSaved,
}: {
  readonly resource: RegistryResource | null;
  readonly onClose: () => void;
  readonly onSaved: () => void;
}) {
  const { token } = useAuth();
  const [form, setForm] = useState<RestResourceForm>(() => (resource ? restFormFromResource(resource) : emptyRestForm()));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const isEdit = resource !== null;
  const anySecret = Object.values(form.secrets).some((v) => v.trim() !== "");
  const authKindChanged = isEdit && form.authKind !== (asObject(resource?.endpoint_config).auth_kind ?? "none");

  function set<K extends keyof RestResourceForm>(key: K, value: RestResourceForm[K]) {
    setForm((f) => ({ ...f, [key]: value }));
  }
  function setSecret(field: string, value: string) {
    setForm((f) => ({ ...f, secrets: { ...f.secrets, [field]: value } }));
  }
  function toggleMethod(method: string) {
    setForm((f) => ({
      ...f,
      methods: f.methods.includes(method) ? f.methods.filter((m) => m !== method) : [...f.methods, method],
    }));
  }

  // includeAuth: create always sends auth; edit sends it only when the
  // user entered secrets (rotate) or changed the auth kind. Rotating to
  // "none" sends an empty object so the control plane drops the Secret.
  const includeAuth = !isEdit || anySecret || authKindChanged;

  return (
    <Modal title={resource ? `Edit “${resource.name}”` : "Register BYO REST resource"} onClose={onClose}>
      <ModalForm
        busy={busy}
        error={error}
        submitLabel={resource ? "Save changes" : "Register"}
        submitDisabled={form.name.trim() === "" || form.baseUrl.trim() === ""}
        onCancel={onClose}
        onSubmit={async () => {
          setError("");
          if (isEdit && authKindChanged && form.authKind !== "none" && !anySecret) {
            setError("Changing the auth kind requires re-entering the credentials for the new kind.");
            return;
          }
          const validation = validateRestForm(form);
          if (validation) {
            setError(validation);
            return;
          }
          setBusy(true);
          try {
            const payload = buildRestResourcePayload(form, includeAuth);
            if (resource) {
              await apiPatch(`/registry/apis/${resource.id}`, token, payload);
            } else {
              await apiPost(`/registry/apis`, token, payload);
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
          <input value={form.name} onChange={(e) => set("name", e.target.value)} autoFocus />
        </label>
        <label className="field">
          <span>Description</span>
          <textarea value={form.description} onChange={(e) => set("description", e.target.value)} />
        </label>
        <label className="field">
          <span>Base URL</span>
          <input value={form.baseUrl} onChange={(e) => set("baseUrl", e.target.value)} placeholder="https://api.example.com/v2" />
        </label>
        <div className="field-row">
          <label className="field">
            <span>Auth kind</span>
            <select value={form.authKind} onChange={(e) => set("authKind", e.target.value as RestAuthKind)}>
              {REST_AUTH_KINDS.map((k) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </select>
          </label>
          {form.authKind === "api_key_header" ? (
            <label className="field">
              <span>Header name</span>
              <input value={form.headerName} onChange={(e) => set("headerName", e.target.value)} placeholder="X-Api-Key" />
            </label>
          ) : null}
        </div>
        {REST_AUTH_FIELDS[form.authKind].map((f) => (
          <label className="field" key={f.field}>
            <span>{f.label}</span>
            <input
              type={f.secret ? "password" : "text"}
              value={form.secrets[f.field] ?? ""}
              onChange={(e) => setSecret(f.field, e.target.value)}
              autoComplete="new-password"
              placeholder={isEdit ? "write-only — leave blank to keep current" : "stored as a managed K8s Secret, never re-displayed"}
            />
          </label>
        ))}
        <div className="field">
          <span>Allowed methods (ceiling)</span>
          <div className="field-row">
            {REST_HTTP_METHODS.map((m) => (
              <label key={m} className="field-checkbox">
                <input type="checkbox" checked={form.methods.includes(m)} onChange={() => toggleMethod(m)} />
                {m}
              </label>
            ))}
          </div>
        </div>
        <div className="field-row">
          <label className="field">
            <span>Path allow (globs, comma-separated)</span>
            <input value={form.pathAllow} onChange={(e) => set("pathAllow", e.target.value)} placeholder="/issues/**, /search" />
          </label>
          <label className="field">
            <span>Path deny</span>
            <input value={form.pathDeny} onChange={(e) => set("pathDeny", e.target.value)} placeholder="/admin/**" />
          </label>
        </div>
        <div className="field-row">
          <label className="field">
            <span>Max request bytes</span>
            <input value={form.maxRequestBytes} onChange={(e) => set("maxRequestBytes", e.target.value)} placeholder="65536" />
          </label>
          <label className="field">
            <span>Max response bytes</span>
            <input value={form.maxResponseBytes} onChange={(e) => set("maxResponseBytes", e.target.value)} placeholder="262144" />
          </label>
          <label className="field">
            <span>Rate / min</span>
            <input value={form.ratePerMin} onChange={(e) => set("ratePerMin", e.target.value)} placeholder="60" />
          </label>
          <label className="field">
            <span>Egress class</span>
            <select value={form.egressClass} onChange={(e) => set("egressClass", e.target.value as "public" | "internal")}>
              <option value="public">public</option>
              <option value="internal">internal</option>
            </select>
          </label>
        </div>
        <div className="notice">
          Effective ceiling: {restCeilingSummary(asObject(buildRestResourcePayload(form, false).policy_ceiling) as RestCeiling)}
        </div>
      </ModalForm>
    </Modal>
  );
}
