"use client";

// TG-4b (S-244-series): BYO git resource registration panel.
//
// Mirror of RestResourcePanel for git resources: admin/owner surface
// for registering governed git resources with a write-only bearer
// token (stored as a managed `skquad-git-` K8s Secret — never
// re-displayed) and a policy ceiling (repo allowlist, push toggle,
// rate). Grants to agents reuse the existing agent-page grant flow;
// this page shows the resource list with a read-only ceiling summary.

import { useState } from "react";
import { apiPatch, apiPost, type RegistryResource } from "../lib/api";
import { useApi } from "../lib/useApi";
import { useAuth } from "../lib/auth";
import { DeleteResourceButton } from "./DeleteResourceButton";
import { EmptyState } from "./EmptyState";
import { Modal, ModalForm } from "./Modal";
import { StatusChip } from "./StatusChip";
import {
  GIT_AUTH_FIELDS,
  buildGitResourcePayload,
  emptyGitForm,
  gitCeilingSummary,
  validateGitForm,
  type GitCeiling,
  type GitEndpointConfig,
  type GitResourceForm,
} from "../lib/gitResources";

function asObject(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};
}

function asStringArray(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((v): v is string => typeof v === "string") : [];
}

function asNumberOpt(value: unknown): string {
  return typeof value === "number" ? String(value) : "";
}

// gitFormFromResource prefills the edit form from a registry row.
// Secret fields are NEVER prefilled — the API never returns them.
export function gitFormFromResource(resource: RegistryResource): GitResourceForm {
  const cfg = asObject(resource.endpoint_config) as Partial<GitEndpointConfig>;
  const ceiling = asObject(resource.policy_ceiling) as GitCeiling;
  return {
    ...emptyGitForm(),
    name: resource.name ?? "",
    description: resource.description ?? "",
    baseUrl: cfg.base_url ?? "",
    reposAllow: asStringArray(ceiling.repos_allow).join(", "),
    allowPush: ceiling.allow_push === true,
    ratePerMin: asNumberOpt(ceiling.rate_per_min),
  };
}

export function GitResourcePanel({ isAdmin }: { readonly isAdmin: boolean }) {
  const resources = useApi<RegistryResource[]>("/registry/git", 60000);
  const [editing, setEditing] = useState<RegistryResource | null>(null);
  const [creating, setCreating] = useState(false);
  const items = resources.data || [];

  return (
    <section>
      <div className="section-head">
        <h2>Git repos (BYO)</h2>
        {isAdmin ? (
          <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
            + Register git resource
          </button>
        ) : null}
      </div>
      {resources.error ? <div className="notice error">{resources.error}</div> : null}
      {items.length === 0 && !resources.loading ? (
        <EmptyState title="No git resources registered" hint="Register a BYO git host (e.g. GitHub) so agents can be granted repo-scoped access." />
      ) : (
        <div className="entity-list">
          {items.map((r) => (
            <div key={r.id} className="entity-row">
              <div className="entity-main">
                <span className="entity-title">{r.name}</span>
                <span className="entity-meta mono">{gitCeilingSummary(asObject(r.policy_ceiling) as GitCeiling)}</span>
                <span className="entity-meta">
                  base: {String(asObject(r.endpoint_config).base_url ?? "—")} · auth: bearer (write-only)
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
                    path={`/registry/git/${r.id}`}
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
        <GitResourceModal
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

export function GitResourceModal({
  resource,
  onClose,
  onSaved,
}: {
  readonly resource: RegistryResource | null;
  readonly onClose: () => void;
  readonly onSaved: () => void;
}) {
  const { token } = useAuth();
  const [form, setForm] = useState<GitResourceForm>(() => (resource ? gitFormFromResource(resource) : emptyGitForm()));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const isEdit = resource !== null;
  const anySecret = Object.values(form.secrets).some((v) => v.trim() !== "");

  function set<K extends keyof GitResourceForm>(key: K, value: GitResourceForm[K]) {
    setForm((f) => ({ ...f, [key]: value }));
  }
  function setSecret(field: string, value: string) {
    setForm((f) => ({ ...f, secrets: { ...f.secrets, [field]: value } }));
  }

  // includeAuth: create always sends auth; edit sends it only when the
  // user re-entered the token (rotate).
  const includeAuth = !isEdit || anySecret;

  return (
    <Modal title={resource ? `Edit “${resource.name}”` : "Register BYO git resource"} onClose={onClose}>
      <ModalForm
        busy={busy}
        error={error}
        submitLabel={resource ? "Save changes" : "Register"}
        submitDisabled={form.name.trim() === "" || form.baseUrl.trim() === ""}
        onCancel={onClose}
        onSubmit={async () => {
          setError("");
          const validation = validateGitForm(form, includeAuth);
          if (validation) {
            setError(validation);
            return;
          }
          setBusy(true);
          try {
            const payload = buildGitResourcePayload(form, includeAuth);
            if (resource) {
              await apiPatch(`/registry/git/${resource.id}`, token, payload);
            } else {
              await apiPost(`/registry/git`, token, payload);
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
          <input value={form.baseUrl} onChange={(e) => set("baseUrl", e.target.value)} placeholder="https://github.com" />
        </label>
        {GIT_AUTH_FIELDS.map((f) => (
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
        <label className="field">
          <span>Allowed repos (org/repo globs, comma- or newline-separated)</span>
          <textarea
            value={form.reposAllow}
            onChange={(e) => set("reposAllow", e.target.value)}
            placeholder={"acme/*\nross/service-a"}
          />
        </label>
        <div className="field-row">
          <label className="field-checkbox">
            <input type="checkbox" checked={form.allowPush} onChange={(e) => set("allowPush", e.target.checked)} />
            Allow push
          </label>
          <label className="field">
            <span>Rate / min</span>
            <input value={form.ratePerMin} onChange={(e) => set("ratePerMin", e.target.value)} placeholder="60" />
          </label>
        </div>
        <div className="notice">
          Effective ceiling: {gitCeilingSummary(asObject(buildGitResourcePayload(form, false).policy_ceiling) as GitCeiling)}
        </div>
      </ModalForm>
    </Modal>
  );
}
