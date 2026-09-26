"use client";

import { useEffect, useState } from "react";
import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { ConfirmDialog } from "../../components/ConfirmDialog";
import { EmptyState } from "../../components/EmptyState";
import { Modal, ModalForm } from "../../components/Modal";
import { StatusChip } from "../../components/StatusChip";
import { useAuth } from "../../lib/auth";
import { useApi } from "../../lib/useApi";
import {
  apiDelete,
  apiGet,
  apiPatch,
  apiPost,
  apiPut,
  ApiError,
  type LLMProvider,
  type RegistryResource,
} from "../../lib/api";
import {
  buildAIModelPayload,
  emptyAIModelForm,
  formFromAIModel,
  formatCascadeReport,
  formatInUseMessage,
  grantedModelIds,
  grantDiff,
  groupModelsByProvider,
  inUseConflict,
  isDuplicateModel,
  isPlatformAdmin,
  modelFieldMode,
  modelRowFields,
  PLATFORM_ADMIN_ROLE,
  parseProviderModels,
  PRICING_RATE_KEYS,
  PRICING_RATE_LABELS,
  withForce,
  type AIModel,
  type AIModelFormValues,
  type AdminUser,
  type CascadeReport,
  type ModelUsageEntry,
} from "../../lib/aimodels";
import { kindOptionsFor } from "../../lib/providerKinds";

type DeleteUsage = { agent_id: string; agent_name: string; squad_id: string };

// resolveSettingsTab maps the requested tab onto a tab the caller may see:
// admins never get the standalone providers tab (it is merged into
// "ai-models"), non-admins never get the admin-only tabs. Extracted from
// SettingsPage for cognitive complexity (S-126 / S3776).
function resolveSettingsTab(tab: Tab, isAdmin: boolean): Tab {
  if (isAdmin) {
    return tab === "providers" ? "ai-models" : tab;
  }
  return tab === "ai-models" || tab === "access" ? "providers" : tab;
}

// TabButton: one settings tab button (S-126 / S3358: keeps the ternary
// class logic out of the JSX tree).
function TabButton({ active, label, onClick }: { readonly active: boolean; readonly label: string; readonly onClick: () => void }) {
  return (
    <button type="button" className={active ? "active" : ""} onClick={onClick}>
      {label}
    </button>
  );
}

// statusChipFor maps a lifecycle status to the StatusChip variant.
// Extracted from the settings rows (S-126 / S3358).
function statusChipFor(status: string): "idle" | "paused" | "error" {
  if (status === "active") return "idle";
  if (status === "deprecated") return "paused";
  return "error";
}

function errorMessage(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback;
}

function duplicateModelMessage(err: unknown): string {
  const detail = err instanceof Error ? err.message : "";
  return `Duplicate model: ${detail}`;
}

// DeleteResourceButton (S-103): tries a plain delete; if the API reports
// the resource is granted to agents (409), warns with the usage list and
// offers a force delete that also revokes those grants.
function DeleteResourceButton({
  path,
  name,
  onDeleted,
}: {
  readonly path: string;
  readonly name: string;
  readonly onDeleted: () => void;
}) {
  const { token } = useAuth();
  const [usage, setUsage] = useState<DeleteUsage[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function attempt(force: boolean) {
    setBusy(true);
    setError("");
    try {
      await apiDelete(path + (force ? "?force=true" : ""), token);
      setUsage(null);
      onDeleted();
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        const body = err.body as { usage?: DeleteUsage[] } | undefined;
        setUsage(body?.usage ?? []);
      } else {
        setError(err instanceof Error ? err.message : "delete failed");
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <button
        type="button"
        className="btn btn-sm btn-danger"
        disabled={busy}
        onClick={() => {
              attempt(false);
            }}
      >
        Delete
      </button>
      {error ? (
        <span className="notice error" role="alert" style={{ marginLeft: 8 }}>
          {error}
        </span>
      ) : null}
      {usage !== null ? (
        <ConfirmDialog
          title={`Delete “${name}”?`}
          body={
            usage.length > 0
              ? `This is currently granted to ${usage.length} agent(s): ${usage
                  .map((u) => u.agent_name)
                  .join(", ")}. Deleting will revoke those grants.`
              : "Delete this resource?"
          }
          confirmLabel="Delete and revoke"
          onConfirm={async () => {
            await attempt(true);
          }}
          onClose={() => setUsage(null)}
        />
      ) : null}
    </>
  );
}

// S-117: "appearance" and "session" tabs removed — theme switching lives
// in the top-right ThemeToggle and session details/sign-out in the
// bottom-left UserMenu popover, both available on every page.
type Tab = "providers" | "resources" | "ai-models" | "access";

const RESOURCE_TABS: { key: string; label: string }[] = [
  { key: "skills", label: "Skills" },
  { key: "tools", label: "Tools" },
  { key: "apis", label: "APIs" },
  { key: "knowledge-bases", label: "Knowledge bases" },
  { key: "project-workspaces", label: "Project workspaces" },
];

export default function SettingsPage() {
  const { user } = useAuth();
  const [tab, setTab] = useState<Tab>("providers");
  const isAdmin = isPlatformAdmin(user?.role);
  // WP6 (S-111): AI Models + Access are admin-only surfaces. The tab
  // buttons are not rendered for non-admins at all (not just disabled),
  // and the content render is gated again as defence in depth.
  // S-128: providers + AI models are merged into one admin "AI Models"
  // tab. Admins never get the standalone providers tab (it maps to the
  // merged hierarchy view); non-admins keep the read-only providers
  // list and never see the merged tab.
  const activeTab: Tab = resolveSettingsTab(tab, isAdmin);

  return (
    <AuthGate>
      <AppShell>
        <h1 className="page-title">Settings</h1>
        <div className="tabs">
          {isAdmin ? (
            <TabButton active={activeTab === "ai-models"} label="AI Models" onClick={() => setTab("ai-models")} />
          ) : (
            <TabButton active={activeTab === "providers"} label="LLM providers" onClick={() => setTab("providers")} />
          )}
          <TabButton active={activeTab === "resources"} label="Resources" onClick={() => setTab("resources")} />
          {isAdmin ? (
            <TabButton active={activeTab === "access"} label="Access" onClick={() => setTab("access")} />
          ) : null}
        </div>

        {!isAdmin ? (
          <div className="notice" style={{ marginBottom: "var(--space-4)" }}>
            You are signed in as <strong>{user?.role || "user"}</strong>. Registering or changing providers and
            resources requires the <strong>platform_admin</strong> role.
          </div>
        ) : null}

        {!isAdmin && activeTab === "providers" ? <ProvidersTab isAdmin={false} /> : null}
        {activeTab === "resources" ? <ResourcesTab isAdmin={isAdmin} /> : null}
        {isAdmin && activeTab === "ai-models" ? <ModelHierarchyTab /> : null}
        {isAdmin && activeTab === "access" ? <AccessTab /> : null}
      </AppShell>
    </AuthGate>
  );
}

function ProvidersTab({ isAdmin }: { readonly isAdmin: boolean }) {
  const { token } = useAuth();
  const providers = useApi<LLMProvider[]>("/registry/llm-providers", 60000);
  const [editing, setEditing] = useState<LLMProvider | null>(null);
  const [creating, setCreating] = useState(false);
  const items = providers.data || [];

  async function deprecateProvider(providerID: string) {
    await apiPost(`/registry/llm-providers/${providerID}/deprecate`, token, {});
    providers.refresh();
  }

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
                <span className="entity-meta">{p.kind} · {p.base_url}</span>
              </div>
              <div className="entity-side">
                <StatusChip status={statusChipFor(p.status)} />
                {isAdmin ? (
                  <button type="button" className="btn btn-sm" onClick={() => setEditing(p)}>
                    Edit
                  </button>
                ) : null}
                {isAdmin && p.status === "active" ? (
                  <button
                    type="button"
                    className="btn btn-sm btn-danger"
                    onClick={() => {
                      deprecateProvider(p.id);
                    }}
                  >
                    Deprecate
                  </button>
                ) : null}
                {isAdmin ? (
                  <DeleteResourceButton
                    path={`/registry/llm-providers/${p.id}`}
                    name={p.name}
                    onDeleted={() => {
                      providers.refresh();
                    }}
                  />
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

function ResourcesTab({ isAdmin }: { readonly isAdmin: boolean }) {
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
                <StatusChip status={statusChipFor(r.status)} />
                {isAdmin ? (
                  <button type="button" className="btn btn-sm" onClick={() => setEditing(r)}>
                    Edit
                  </button>
                ) : null}
                {isAdmin ? (
                  <DeleteResourceButton
                    path={`/registry/${active.key}/${r.id}`}
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

// S-128 — merged admin tab: the LLM Providers > AI Models hierarchy on
// one screen. Providers render as credential-holder group headers (name,
// kind, base_url, default model) with their registered models nested
// underneath. All pricing lives on the model rows only — provider rows
// and the provider form carry no pricing (ADR-0010 D8 made the model the
// pricing + grant unit; the UI now matches). Create/edit/deprecate/delete
// flows for both levels are preserved from the former ProvidersTab and
// AIModelsTab (S-111).
// ProviderGroupHeader renders the credential-holder header row in the
// merged AI Models hierarchy (S-126 / S3776: extracted from
// ModelHierarchyTab).
function ProviderGroupHeader({
  provider,
  onEdit,
  onDeleted,
}: {
  readonly provider: LLMProvider;
  readonly onEdit: () => void;
  readonly onDeleted: () => void;
}) {
  const { token } = useAuth();

  async function deprecateProvider() {
    await apiPost(`/registry/llm-providers/${provider.id}/deprecate`, token, {});
    onDeleted();
  }

  return (
    <div className="entity-row provider-header-row">
      <div className="entity-main">
        <span className="entity-title">{provider.name}</span>
        <span className="entity-meta">{provider.kind} · {provider.base_url}</span>
      </div>
      <div className="entity-side">
        <StatusChip status={statusChipFor(provider.status)} />
        <button type="button" className="btn btn-sm" onClick={onEdit}>
          Edit
        </button>
        {provider.status === "active" ? (
          <button
            type="button"
            className="btn btn-sm btn-danger"
            onClick={() => {
              deprecateProvider();
            }}
          >
            Deprecate
          </button>
        ) : null}
        <DeleteResourceButton
          path={`/registry/llm-providers/${provider.id}`}
          name={provider.name}
          onDeleted={onDeleted}
        />
      </div>
    </div>
  );
}

function ModelHierarchyTab() {
  const { token } = useAuth();
  const providers = useApi<LLMProvider[]>("/registry/llm-providers", 60000);
  const models = useApi<AIModel[]>("/ai-models", 60000);
  const [editingProvider, setEditingProvider] = useState<LLMProvider | null>(null);
  const [creatingProvider, setCreatingProvider] = useState(false);
  const [editingModel, setEditingModel] = useState<AIModel | null>(null);
  const [creatingModel, setCreatingModel] = useState(false);
  const [report, setReport] = useState("");

  const { groups, orphans } = groupModelsByProvider(providers.data || [], models.data || []);
  const loading = providers.loading || models.loading;

  async function deprecate(model: AIModel) {
    try {
      const rep = await apiPost<CascadeReport>(`/ai-models/${model.id}/deprecate`, token, {});
      setReport(formatCascadeReport(rep));
      models.refresh();
    } catch (err) {
      setReport(`Deprecate failed: ${err instanceof Error ? err.message : "unknown error"}`);
    }
  }

  // modelRow renders one nested model row under its provider group. The
  // provider name is the group header, so the row subtitle omits it; the
  // four per-1M rates stay visible (pricing lives here, not on the
  // provider).
  function modelRow(m: AIModel) {
    const row = modelRowFields(m);
    return (
      <div key={m.id} className="entity-row model-nested-row">
        <div className="entity-main">
          <span className="entity-title">{row.title}</span>
          <span className="entity-meta">
            {m.model_name} · {row.contextWindow} · {row.tools} · {row.longContextThreshold}
          </span>
          <span className="entity-meta">
            {row.rates.map((r) => `${r.label}: ${r.value}`).join(" · ")}
          </span>
        </div>
        <div className="entity-side">
          <StatusChip status={statusChipFor(m.status)} />
          <button type="button" className="btn btn-sm" onClick={() => setEditingModel(m)}>
            Edit
          </button>
          {m.status === "active" ? (
            <button type="button" className="btn btn-sm btn-danger" onClick={() => {
              deprecate(m);
            }}>
              Deprecate
            </button>
          ) : null}
          <DeleteAIModelButton model={m} onDeleted={() => {
              models.refresh();
            }} />
        </div>
      </div>
    );
  }

  return (
    <section>
      <div className="section-head">
        <h2>AI Models</h2>
        <div style={{ display: "flex", gap: 8 }}>
          <button type="button" className="btn btn-primary" onClick={() => setCreatingModel(true)}>
            + Register model
          </button>
          <button type="button" className="btn" onClick={() => setCreatingProvider(true)}>
            + Register provider
          </button>
        </div>
      </div>
      {report ? (
        <output className="notice" aria-live="polite" style={{ display: "block", marginBottom: "var(--space-4)" }}>
          {report}
          <button type="button" className="btn btn-sm" style={{ marginLeft: 8 }} onClick={() => setReport("")}>
            Dismiss
          </button>
        </output>
      ) : null}
      {providers.error ? <div className="notice error">{providers.error}</div> : null}
      {models.error ? <div className="notice error">{models.error}</div> : null}
      {groups.length === 0 && orphans.length === 0 && !loading ? (
        <EmptyState
          title="No AI models registered"
          hint="Register a provider (credential holder), then add its models. Models are the unit granted to users under Access."
        />
      ) : null}
      {groups.length > 0 || orphans.length > 0 ? (
        <div className="entity-list">
          {groups.map(({ provider, models: providerModels }) => (
            <div key={provider.id} className="provider-group">
              <ProviderGroupHeader
                provider={provider}
                onEdit={() => setEditingProvider(provider)}
                onDeleted={() => {
                  providers.refresh();
                }}
              />
              <div className="provider-models">
                {providerModels.length === 0 ? (
                  <div className="provider-empty">No models registered under this provider yet.</div>
                ) : (
                  providerModels.map((m) => modelRow(m))
                )}
              </div>
            </div>
          ))}
          {orphans.length > 0 ? (
            <div className="provider-group orphan-group">
              <div className="entity-row provider-header-row">
                <div className="entity-main">
                  <span className="entity-title">Unassigned models</span>
                  <span className="entity-meta">No matching provider found — re-register the provider or delete these models.</span>
                </div>
              </div>
              <div className="provider-models">{orphans.map((m) => modelRow(m))}</div>
            </div>
          ) : null}
        </div>
      ) : null}
      {(creatingModel || editingModel) && (
        <AIModelModal
          model={editingModel}
          providers={providers.data || []}
          onClose={() => {
            setCreatingModel(false);
            setEditingModel(null);
          }}
          onSaved={() => {
            setCreatingModel(false);
            setEditingModel(null);
            models.refresh();
          }}
        />
      )}
      {(creatingProvider || editingProvider) && (
        <ProviderModal
          provider={editingProvider}
          onClose={() => {
            setCreatingProvider(false);
            setEditingProvider(null);
          }}
          onSaved={() => {
            setCreatingProvider(false);
            setEditingProvider(null);
            providers.refresh();
          }}
        />
      )}
    </section>
  );
}

// DeleteAIModelButton mirrors DeleteResourceButton (S-103): plain delete
// first; on 409 in_use the shared ConfirmDialog lists affected users and
// agents (with slot) and the explicit second click retries with force.
function DeleteAIModelButton({ model, onDeleted }: { readonly model: AIModel; readonly onDeleted: () => void }) {
  const { token } = useAuth();
  const [usage, setUsage] = useState<ModelUsageEntry[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function attempt(force: boolean) {
    setBusy(true);
    setError("");
    try {
      await apiDelete(withForce(`/ai-models/${model.id}`, force), token);
      setUsage(null);
      onDeleted();
    } catch (err) {
      const conflict = inUseConflict(err);
      if (conflict) {
        setUsage(conflict);
      } else {
        setError(err instanceof Error ? err.message : "delete failed");
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <button type="button" className="btn btn-sm btn-danger" disabled={busy} onClick={() => {
        attempt(false);
      }}>
        Delete
      </button>
      {error ? (
        <span className="notice error" role="alert" style={{ marginLeft: 8 }}>
          {error}
        </span>
      ) : null}
      {usage !== null ? (
        <ConfirmDialog
          title={`Delete “${model.display_name || model.model_name}”?`}
          body={formatInUseMessage(usage)}
          confirmLabel="Delete and revoke"
          onConfirm={async () => {
            await attempt(true);
          }}
          onClose={() => setUsage(null)}
        />
      ) : null}
    </>
  );
}

function AIModelModal({
  model,
  providers,
  onClose,
  onSaved,
}: {
  readonly model: AIModel | null;
  readonly providers: LLMProvider[];
  readonly onClose: () => void;
  readonly onSaved: () => void;
}) {
  const { token } = useAuth();
  const [values, setValues] = useState<AIModelFormValues>(() => (model ? formFromAIModel(model) : emptyAIModelForm()));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // S-125: live model list from the selected provider's model endpoint.
  const [providerModels, setProviderModels] = useState<string[]>([]);
  const [modelsLoading, setModelsLoading] = useState(false);
  const [modelsError, setModelsError] = useState("");

  // Re-query whenever the provider changes. The providerId guard drops
  // stale responses (fast provider switches must not clobber the new
  // provider's list). Clearing the provider resets the model name so a
  // name from another provider can't be submitted by accident.
  useEffect(() => {
    const providerId = values.provider_id;
    if (providerId === "") {
      setProviderModels([]);
      setModelsError("");
      setModelsLoading(false);
      return;
    }
    let active = true;
    setModelsLoading(true);
    setModelsError("");
    setProviderModels([]);
    const loadProviderModels = async () => {
      try {
        const body = await apiGet<unknown>(`/registry/llm-providers/${providerId}/models`, token);
        if (!active) return;
        setProviderModels(parseProviderModels(body));
      } catch (err) {
        if (!active) return;
        setModelsError(err instanceof Error ? err.message : "model list fetch failed");
      } finally {
        if (active) setModelsLoading(false);
      }
    };
    // Fire-and-forget: the effect cannot await; staleness is guarded by `active`.
    loadProviderModels();
    return () => {
      active = false;
    };
  }, [values.provider_id, token]);

  // (Model name is cleared explicitly in the provider select's onChange —
  // never implicitly on list load, so an existing edit's value survives.)

  function setField<K extends keyof AIModelFormValues>(key: K, value: AIModelFormValues[K]) {
    setValues((v) => ({ ...v, [key]: value }));
  }

  function setRate(key: (typeof PRICING_RATE_KEYS)[number], raw: string) {
    setValues((v) => ({ ...v, pricing: { ...v.pricing, [key]: raw } }));
  }

  const mode = modelFieldMode(values.provider_id !== "", modelsLoading, modelsError);
  const modelListId = `provider-models-${values.provider_id || "none"}`;

  function modelNameControl() {
    if (mode === "none") {
      return <input value="" disabled placeholder="Select a provider first…" />;
    }
    if (mode === "loading") {
      return <input value="" disabled placeholder="Loading models from provider…" />;
    }
    if (mode === "fallback") {
      return (
        <>
          <input
            value={values.model_name}
            onChange={(e) => setField("model_name", e.target.value)}
            placeholder="gpt-6-sol"
            autoFocus
          />
          <span className="field-hint">
            Couldn&rsquo;t load the provider&rsquo;s model list ({modelsError}) — type the model name manually.
          </span>
        </>
      );
    }
    return (
      <>
        <input
          value={values.model_name}
          onChange={(e) => setField("model_name", e.target.value)}
          placeholder="Select a model, or type to filter…"
          autoFocus
          list={modelListId}
        />
        <datalist id={modelListId}>
          {providerModels.map((m) => (
            <option key={m} value={m} />
          ))}
        </datalist>
      </>
    );
  }

  return (
    <Modal title={model ? `Edit “${model.display_name || model.model_name}”` : "Register AI model"} onClose={onClose}>
      <ModalForm
        busy={busy}
        error={error}
        submitLabel={model ? "Save changes" : "Register"}
        submitDisabled={values.provider_id === "" || values.model_name.trim() === ""}
        onCancel={onClose}
        onSubmit={async () => {
          setBusy(true);
          setError("");
          try {
            const body = buildAIModelPayload(values);
            if (model) {
              await apiPatch(`/ai-models/${model.id}`, token, body);
            } else {
              await apiPost("/ai-models", token, body);
            }
            onSaved();
          } catch (err) {
            // Field-named validation errors from the API (and the local
            // payload builder) surface verbatim — e.g.
            // "cached_input_per_1m in pricing must be numeric".
            const dup = isDuplicateModel(err);
            setError(dup ? duplicateModelMessage(err) : errorMessage(err, "save failed"));
            setBusy(false);
          }
        }}
      >
        <div className="field-row">
          <label className="field">
            <span>Provider (credential holder)</span>
            <select
              value={values.provider_id}
              onChange={(e) => {
                setField("provider_id", e.target.value);
                setField("model_name", "");
              }}
            >
              <option value="" disabled>
                Select provider…
              </option>
              {providers.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </select>
          </label>
          <div className="field">
            <span>Model name</span>
            {modelNameControl()}
          </div>
        </div>
        <div className="field-row">
          <label className="field">
            <span>Display name (optional — defaults to model name)</span>
            <input
              value={values.display_name}
              onChange={(e) => setField("display_name", e.target.value)}
              placeholder="GPT-6 Sol"
            />
          </label>
          <label className="field">
            <span>Context window (tokens)</span>
            <input
              type="number"
              min={0}
              value={values.context_window}
              onChange={(e) => setField("context_window", e.target.value)}
              placeholder="200000"
            />
          </label>
        </div>
        <label className="field" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
          <input
            type="checkbox"
            checked={values.supports_tools}
            onChange={(e) => setField("supports_tools", e.target.checked)}
          />
          <span>Supports tool calling</span>
        </label>
        <div className="field-row">
          {PRICING_RATE_KEYS.map((key) => (
            <label key={key} className="field">
              <span>{PRICING_RATE_LABELS[key]} ($ per 1M tokens)</span>
              <input
                type="number"
                min={0}
                step="any"
                value={values.pricing[key]}
                onChange={(e) => setRate(key, e.target.value)}
                placeholder="0.00"
              />
            </label>
          ))}
        </div>
        <label className="field">
          <span>Long-context threshold (tokens, optional)</span>
          <input
            type="number"
            min={0}
            value={values.long_context_threshold_tokens}
            onChange={(e) => setField("long_context_threshold_tokens", e.target.value)}
            placeholder="272000"
          />
          <span className="field-hint">
            Top-level field: splits short vs long context pricing tiers (ADR-0010 D8). All four rates are required.
          </span>
        </label>
      </ModalForm>
    </Modal>
  );
}

// WP6 (S-111) — Access tab: platform role management + per-user AI model
// grants (ADR-0010 D3).
// Removals from the grant set are guarded server-side (409 in_use); the
// force retry is always an explicit second click so running agents are
// never silently orphaned (D9).
// Role changes are app-managed (PATCH /users/{id}/role): the last
// platform admin cannot be demoted (409 last_admin) and every change is
// audited server-side.
function AccessTab() {
  const { user: me } = useAuth();
  const users = useApi<AdminUser[]>("/users", 60000);
  const [selected, setSelected] = useState<AdminUser | null>(null);
  const items = users.data || [];

  return (
    <section>
      <div className="section-head">
        <h2>Users &amp; access</h2>
      </div>
      {users.error ? (
        <div className="notice error">
          Could not load users: {users.error}. (The control-plane needs the admin <code>GET /users</code> endpoint — see
          WP6 report.)
        </div>
      ) : null}
      {items.length === 0 && !users.loading ? (
        <EmptyState title="No users found" hint="Users appear here after their first OIDC sign-in." />
      ) : (
        <div className="entity-list">
          {items.map((u) => (
            <div key={u.id} className="entity-row">
              <div className="entity-main">
                <span className="entity-title">
                  {u.name || u.email}
                  {me?.id === u.id ? <span className="entity-meta"> (you)</span> : null}
                </span>
                <span className="entity-meta">
                  {u.email} · role {u.role}
                </span>
              </div>
              <div className="entity-side" style={{ display: "flex", gap: 8, alignItems: "center" }}>
                <RoleControl
                  user={u}
                  isSelf={me?.id === u.id}
                  onChanged={() => {
                    users.refresh();
                  }}
                />
                <button
                  type="button"
                  className={`btn btn-sm${selected?.id === u.id ? " btn-primary" : ""}`}
                  onClick={() => setSelected(selected?.id === u.id ? null : u)}
                >
                  {selected?.id === u.id ? "Hide models" : "Manage models"}
                </button>
              </div>
            </div>
          ))}
        </div>
      )}
      {selected ? <GrantEditor key={selected.id} user={selected} /> : null}
    </section>
  );
}

// RoleControl promotes/demotes a user's Layer-1 platform role.
// Every change goes through an explicit confirm dialog; the server's
// 409 last_admin guard is surfaced as a plain-language message.
function RoleControl({
  user,
  isSelf,
  onChanged,
}: {
  readonly user: AdminUser;
  readonly isSelf: boolean;
  readonly onChanged: () => void;
}) {
  const { token } = useAuth();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const isAdminUser = user.role === PLATFORM_ADMIN_ROLE;
  const targetRole = isAdminUser ? "user" : PLATFORM_ADMIN_ROLE;

  async function apply() {
    setBusy(true);
    setError("");
    try {
      await apiPatch(`/users/${user.id}/role`, token, { role: targetRole });
      setConfirming(false);
      onChanged();
    } catch (err) {
      setConfirming(false);
      if (err instanceof ApiError && err.status === 409) {
        setError("Last platform admin — demoting this account would lock everyone out.");
      } else {
        setError(errorMessage(err, "role change failed"));
      }
    } finally {
      setBusy(false);
    }
  }

  const label = isAdminUser ? "Demote" : "Make admin";
  const confirmBody = isSelf
    ? "You are changing your OWN role. You will lose access to this admin surface immediately."
    : isAdminUser
      ? `Remove platform admin from ${user.name || user.email}? They keep their squads, agents and model grants.`
      : `Give ${user.name || user.email} full platform admin (user management, model grants, registry writes)?`;

  return (
    <>
      <button
        type="button"
        className={`btn btn-sm${isAdminUser ? " btn-danger" : ""}`}
        disabled={busy}
        onClick={() => setConfirming(true)}
      >
        {label}
      </button>
      {error ? (
        <span className="notice error" role="alert" style={{ marginLeft: 8 }}>
          {error}
        </span>
      ) : null}
      {confirming ? (
        <ConfirmDialog
          title={`${label} ${user.name || user.email}?`}
          body={confirmBody}
          confirmLabel={label}
          onConfirm={async () => {
            await apply();
          }}
          onClose={() => setConfirming(false)}
        />
      ) : null}
    </>
  );
}

function GrantEditor({ user }: { readonly user: AdminUser }) {
  const { token } = useAuth();
  const allModels = useApi<AIModel[]>("/ai-models?status=active", 60000);
  const granted = useApi<AIModel[]>(`/users/${user.id}/models`, 0);
  const [desired, setDesired] = useState<string[] | null>(null);
  const [usage, setUsage] = useState<ModelUsageEntry[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [savedNote, setSavedNote] = useState("");

  const currentIds = granted.data ? grantedModelIds(granted.data) : [];
  const effective = desired ?? currentIds;
  const dirty = desired !== null && grantDiff(currentIds, desired).added.length + grantDiff(currentIds, desired).removed.length > 0;

  function toggle(modelId: string) {
    setDesired((d) => {
      const base = d ?? currentIds;
      return base.includes(modelId) ? base.filter((id) => id !== modelId) : [...base, modelId];
    });
    setSavedNote("");
  }

  async function save(force: boolean) {
    setBusy(true);
    setError("");
    try {
      await apiPut(withForce(`/users/${user.id}/models`, force), token, { model_ids: effective });
      setUsage(null);
      setDesired(null);
      setSavedNote("Grants saved and virtual keys converged.");
      granted.refresh();
    } catch (err) {
      const conflict = inUseConflict(err);
      if (conflict && !force) {
        setUsage(conflict);
      } else {
        setError(err instanceof Error ? err.message : "save failed");
      }
    } finally {
      setBusy(false);
    }
  }

  const models = allModels.data || [];
  const diff = desired === null ? { added: [], removed: [] } : grantDiff(currentIds, desired);

  return (
    <div className="card" style={{ marginTop: "var(--space-4)" }}>
      <div className="section-head">
        <h2>Models granted to {user.name || user.email}</h2>
      </div>
      {granted.error ? <div className="notice error">{granted.error}</div> : null}
      {allModels.error ? <div className="notice error">{allModels.error}</div> : null}
      {savedNote ? <output className="notice" aria-live="polite">{savedNote}</output> : null}
      {error ? <div className="notice error" role="alert">{error}</div> : null}
      {models.length === 0 && !allModels.loading ? (
        <EmptyState title="No active AI models" hint="Register models under AI Models first." />
      ) : (
        <div className="entity-list">
          {models.map((m) => (
            <div key={m.id} className="entity-row">
              <label className="entity-main" style={{ display: "flex", alignItems: "center", gap: 10, cursor: "pointer" }}>
                <input
                  type="checkbox"
                  checked={effective.includes(m.id)}
                  onChange={() => toggle(m.id)}
                  disabled={busy}
                />
                <span className="entity-title">{m.display_name || m.model_name}</span>
                <span className="entity-meta">
                  {m.model_name} · {m.supports_tools ? "tools ✓" : "no tools"}
                </span>
              </label>
            </div>
          ))}
        </div>
      )}
      <div style={{ marginTop: "var(--space-4)", display: "flex", gap: 8, alignItems: "center" }}>
        <button
          type="button"
          className="btn btn-primary"
          disabled={!dirty || busy || granted.loading}
          onClick={() => {
              save(false);
            }}
        >
          Save grants
        </button>
        {dirty ? (
          <span className="entity-meta">
            {diff.added.length} adding · {diff.removed.length} removing
          </span>
        ) : null}
      </div>
      {usage !== null ? (
        <ConfirmDialog
          title={`Remove grants for ${user.name || user.email}?`}
          body={formatInUseMessage(usage)}
          confirmLabel="Revoke and converge keys"
          onConfirm={async () => {
            await save(true);
          }}
          onClose={() => setUsage(null)}
        />
      ) : null}
    </div>
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
  readonly provider: LLMProvider | null;
  readonly onClose: () => void;
  readonly onSaved: () => void;
}) {
  const { token } = useAuth();
  const [name, setName] = useState(provider?.name ?? "");
  const [kind, setKind] = useState(provider?.kind || "openai");
  const [baseUrl, setBaseUrl] = useState(provider?.base_url ?? "");
  const [apiKeyRef, setApiKeyRef] = useState(provider?.api_key_ref ?? "");
  // WP8 (0014): default_model / models inputs removed from the provider
  // form — model configuration lives on the AI Models underneath.
  // S-128: no provider-level pricing — rates belong on the AI Models.
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
            {/* S-132: dropdown of known API kinds instead of free text. */}
            <select value={kind} onChange={(e) => setKind(e.target.value)}>
              {kindOptionsFor(provider?.kind).map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label}
                </option>
              ))}
            </select>
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
        </div>
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
