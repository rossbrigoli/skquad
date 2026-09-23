"use client";

import { useState } from "react";
import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { EmptyState } from "../../components/EmptyState";
import { Modal, ModalForm } from "../../components/Modal";
import { StatusChip } from "../../components/StatusChip";
import { useAuth } from "../../lib/auth";
import { useApi } from "../../lib/useApi";
import {
  apiPatch,
  apiPost,
  type LLMProvider,
  type RegistryResource,
} from "../../lib/api";

type Tab = "providers" | "resources" | "session";

const RESOURCE_TABS: { key: string; label: string }[] = [
  { key: "skills", label: "Skills" },
  { key: "tools", label: "Tools" },
  { key: "apis", label: "APIs" },
  { key: "knowledge-bases", label: "Knowledge bases" },
  { key: "project-workspaces", label: "Project workspaces" },
];

export default function SettingsPage() {
  const { user, logout } = useAuth();
  const [tab, setTab] = useState<Tab>("providers");
  const isAdmin = (user?.role || "") === "platform_admin";

  return (
    <AuthGate>
      <AppShell>
        <h1 className="page-title">Settings</h1>
        <div className="tabs">
          <button type="button" className={tab === "providers" ? "active" : ""} onClick={() => setTab("providers")}>
            LLM providers
          </button>
          <button type="button" className={tab === "resources" ? "active" : ""} onClick={() => setTab("resources")}>
            Resources
          </button>
          <button type="button" className={tab === "session" ? "active" : ""} onClick={() => setTab("session")}>
            Session
          </button>
        </div>

        {!isAdmin && tab !== "session" ? (
          <div className="notice" style={{ marginBottom: "var(--space-4)" }}>
            You are signed in as <strong>{user?.role || "user"}</strong>. Registering or changing providers and
            resources requires the <strong>platform_admin</strong> role.
          </div>
        ) : null}

        {tab === "providers" ? <ProvidersTab isAdmin={isAdmin} /> : null}
        {tab === "resources" ? <ResourcesTab isAdmin={isAdmin} /> : null}
        {tab === "session" ? (
          <div className="card" style={{ maxWidth: 480 }}>
            <div className="field">
              <span>Signed in as</span>
              <div style={{ fontSize: "var(--text-md)" }}>
                {user?.name || "—"} &lt;{user?.email || "?"}&gt; · role <strong>{user?.role || "?"}</strong>
              </div>
            </div>
            <button type="button" className="btn" onClick={logout}>
              Sign out
            </button>
          </div>
        ) : null}
      </AppShell>
    </AuthGate>
  );
}

function ProvidersTab({ isAdmin }: { isAdmin: boolean }) {
  const { token } = useAuth();
  const providers = useApi<LLMProvider[]>("/registry/llm-providers", 60000);
  const [editing, setEditing] = useState<LLMProvider | null>(null);
  const [creating, setCreating] = useState(false);
  const items = providers.data || [];

  return (
    <section>
      <div className="section-head">
        <h2>LLM providers</h2>
        {isAdmin ? (
          <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
            + Register provider
          </button>
        ) : null}
      </div>
      {providers.error ? <div className="notice error">{providers.error}</div> : null}
      {items.length === 0 && !providers.loading ? (
        <EmptyState title="No LLM providers registered" hint="Agents fall back to the platform default until a provider exists." />
      ) : (
        <div className="entity-list">
          {items.map((p) => (
            <div key={p.id} className="entity-row">
              <div className="entity-main">
                <span className="entity-title">{p.name}</span>
                <span className="entity-meta">
                  {p.kind} · {p.base_url} · default {p.default_model || "—"}
                </span>
              </div>
              <div className="entity-side">
                <StatusChip status={p.status === "active" ? "idle" : p.status === "deprecated" ? "paused" : "error"} />
                {isAdmin ? (
                  <button type="button" className="btn btn-sm" onClick={() => setEditing(p)}>
                    Edit
                  </button>
                ) : null}
                {isAdmin && p.status === "active" ? (
                  <button
                    type="button"
                    className="btn btn-sm btn-danger"
                    onClick={async () => {
                      await apiPost(`/registry/llm-providers/${p.id}/deprecate`, token, {});
                      await providers.refresh();
                    }}
                  >
                    Deprecate
                  </button>
                ) : null}
              </div>
            </div>
          ))}
        </div>
      )}
      {(creating || editing) && (
        <ProviderModal
          provider={editing}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
          onSaved={() => {
            setCreating(false);
            setEditing(null);
            providers.refresh();
          }}
        />
      )}
    </section>
  );
}

function ResourcesTab({ isAdmin }: { isAdmin: boolean }) {
  const [typeIdx, setTypeIdx] = useState(0);
  const active = RESOURCE_TABS[typeIdx];
  const resources = useApi<RegistryResource[]>(`/registry/${active.key}`, 60000);
  const [editing, setEditing] = useState<RegistryResource | null>(null);
  const [creating, setCreating] = useState(false);
  const items = resources.data || [];

  return (
    <section>
      <div className="tabs">
        {RESOURCE_TABS.map((t, i) => (
          <button
            key={t.key}
            type="button"
            className={typeIdx === i ? "active" : ""}
            onClick={() => {
              setTypeIdx(i);
              setEditing(null);
            }}
          >
            {t.label}
          </button>
        ))}
      </div>
      <div className="section-head">
        <h2>{active.label}</h2>
        {isAdmin ? (
          <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
            + Register {active.label.replace(/s$/, "").toLowerCase()}
          </button>
        ) : null}
      </div>
      {resources.error ? <div className="notice error">{resources.error}</div> : null}
      {items.length === 0 && !resources.loading ? (
        <EmptyState title={`No ${active.label.toLowerCase()} registered`} hint="Register one so agents can be granted access." />
      ) : (
        <div className="entity-list">
          {items.map((r) => (
            <div key={r.id} className="entity-row">
              <div className="entity-main">
                <span className="entity-title">{r.name}</span>
                <span className="entity-meta">{r.description || r.endpoint || "—"}</span>
              </div>
              <div className="entity-side">
                <StatusChip status={r.status === "active" ? "idle" : r.status === "deprecated" ? "paused" : "error"} />
                {isAdmin ? (
                  <button type="button" className="btn btn-sm" onClick={() => setEditing(r)}>
                    Edit
                  </button>
                ) : null}
              </div>
            </div>
          ))}
        </div>
      )}
      {(creating || editing) && (
        <ResourceModal
          resourceType={active.key}
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

function parseJsonField(value: string, label: string): string | undefined {
  const trimmed = value.trim();
  if (trimmed === "") return undefined;
  JSON.parse(trimmed); // throws → caller surfaces message
  if (typeof JSON.parse(trimmed) !== "object") throw new Error(`${label} must be a JSON object or array`);
  return trimmed;
}

function ProviderModal({
  provider,
  onClose,
  onSaved,
}: {
  provider: LLMProvider | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { token } = useAuth();
  const [name, setName] = useState(provider?.name || "");
  const [kind, setKind] = useState(provider?.kind || "openai");
  const [baseUrl, setBaseUrl] = useState(provider?.base_url || "");
  const [apiKeyRef, setApiKeyRef] = useState(provider?.api_key_ref || "");
  const [defaultModel, setDefaultModel] = useState(provider?.default_model || "");
  const [models, setModels] = useState(provider?.models ? JSON.stringify(provider.models, null, 2) : "");
  const [pricing, setPricing] = useState(provider?.pricing ? JSON.stringify(provider.pricing, null, 2) : "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  return (
    <Modal title={provider ? `Edit provider “${provider.name}”` : "Register LLM provider"} onClose={onClose}>
      <ModalForm
        busy={busy}
        error={error}
        submitLabel={provider ? "Save changes" : "Register"}
        submitDisabled={name.trim() === "" || baseUrl.trim() === "" || kind.trim() === ""}
        onCancel={onClose}
        onSubmit={async () => {
          setBusy(true);
          setError("");
          try {
            const body = {
              name: name.trim(),
              kind: kind.trim(),
              base_url: baseUrl.trim(),
              api_key_ref: apiKeyRef.trim(),
              default_model: defaultModel.trim(),
              models: parseJsonField(models, "Models"),
              pricing: parseJsonField(pricing, "Pricing"),
            };
            if (provider) {
              await apiPatch(`/registry/llm-providers/${provider.id}`, token, body);
            } else {
              await apiPost("/registry/llm-providers", token, body);
            }
            onSaved();
          } catch (err) {
            setError(err instanceof Error ? err.message : "save failed");
            setBusy(false);
          }
        }}
      >
        <div className="field-row">
          <label className="field">
            <span>Name</span>
            <input value={name} onChange={(e) => setName(e.target.value)} placeholder="openai-prod" autoFocus />
          </label>
          <label className="field">
            <span>Kind</span>
            <input value={kind} onChange={(e) => setKind(e.target.value)} placeholder="openai | anthropic | ollama …" />
          </label>
        </div>
        <label className="field">
          <span>Base URL</span>
          <input value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} placeholder="https://api.openai.com/v1" />
        </label>
        <div className="field-row">
          <label className="field">
            <span>API key ref</span>
            <input value={apiKeyRef} onChange={(e) => setApiKeyRef(e.target.value)} placeholder="k8s secret / vault ref (never the key itself)" />
          </label>
          <label className="field">
            <span>Default model</span>
            <input value={defaultModel} onChange={(e) => setDefaultModel(e.target.value)} placeholder="gpt-5.5" />
          </label>
        </div>
        <label className="field">
          <span>Models (JSON, optional)</span>
          <textarea value={models} onChange={(e) => setModels(e.target.value)} placeholder='["gpt-5.5", "gpt-5.4-mini"]' />
        </label>
        <label className="field">
          <span>Pricing (JSON, optional)</span>
          <textarea value={pricing} onChange={(e) => setPricing(e.target.value)} placeholder='{"input_per_1k": 0.01, "output_per_1k": 0.03}' />
        </label>
      </ModalForm>
    </Modal>
  );
}

function ResourceModal({
  resourceType,
  resource,
  onClose,
  onSaved,
}: {
  resourceType: string;
  resource: RegistryResource | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { token } = useAuth();
  const [name, setName] = useState(resource?.name || "");
  const [description, setDescription] = useState(resource?.description || "");
  const [endpoint, setEndpoint] = useState(resource?.endpoint || "");
  const [authRef, setAuthRef] = useState(resource?.auth_ref || "");
  const [manifest, setManifest] = useState(resource?.manifest ? JSON.stringify(resource.manifest, null, 2) : "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  return (
    <Modal title={resource ? `Edit “${resource.name}”` : `Register ${resourceType.replace(/-/g, " ")}`} onClose={onClose}>
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
