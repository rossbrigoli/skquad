"use client";

// S-204: Tool configuration page.
//
// Clicking a tile on the unified Tools page lands here. Two flavours
// share one route (/settings/resources/tools/{toolId}):
//
//  * built-in tools — toolId is the tool name (exec / web_fetch /
//    web_search / send_message). The pinned policy form from BT-4
//    (ADR-0012) is reused verbatim; config comes from the admin API.
//    Built-ins CANNOT be deleted — no delete affordance is rendered and
//    the API has no delete route for them.
//  * registered tools — toolId is the registry UUID. The classic
//    resource fields (name/description/endpoint/auth ref/manifest) are
//    edited inline against /registry/tools/{id}; admins get delete with
//    the S-103 grant-aware flow.

import { useMemo, useState } from "react";
import Link from "next/link";
import { useRouter, useParams } from "next/navigation";
import { AppShell } from "../../../../../components/AppShell";
import { AuthGate } from "../../../../../components/AuthGate";
import { BuiltinToolConfig } from "../../../../../components/BuiltinToolConfig";
import { DeleteResourceButton } from "../../../../../components/DeleteResourceButton";
import { EmptyState } from "../../../../../components/EmptyState";
import { StatusChip } from "../../../../../components/StatusChip";
import { useAuth } from "../../../../../lib/auth";
import { useApi } from "../../../../../lib/useApi";
import { apiPatch, type RegistryResource } from "../../../../../lib/api";
import { isBuiltinToolName, type BuiltinToolName } from "../../../../../lib/builtinTools";
import { isPlatformAdmin } from "../../../../../lib/aimodels";

export default function ToolConfigPage() {
  const params = useParams<{ toolId: string }>();
  const toolId = Array.isArray(params?.toolId) ? params.toolId[0] : params?.toolId ?? "";
  const { user } = useAuth();
  const isAdmin = isPlatformAdmin(user?.role);

  return (
    <AuthGate>
      <AppShell>
        {isBuiltinToolName(toolId) ? (
          <BuiltinToolRoute name={toolId as BuiltinToolName} isAdmin={isAdmin} />
        ) : (
          <RegistryToolRoute id={toolId} isAdmin={isAdmin} />
        )}
      </AppShell>
    </AuthGate>
  );
}

function BackLink() {
  // S-204 follow-up: a real link to the canonical Tools tiles page.
  // The old implementation relied on browser history, which strands
  // deep-link visitors (no in-app history) — a real href always works
  // and matches the "Tools" breadcrumb.
  return (
    <Link href="/settings/resources/tools" className="btn btn-sm">
      ← Back to Tools
    </Link>
  );
}

// BuiltinToolRoute loads the built-in tool config from the admin API
// and renders the pinned policy form. Non-admins get a plain notice —
// /admin/tools is platform-admin only (ADR-0012 §2).
function BuiltinToolRoute({ name, isAdmin }: { readonly name: BuiltinToolName; readonly isAdmin: boolean }) {
  if (!isAdmin) {
    return (
      <section>
        <BackLink />
        <h1 className="page-title" style={{ marginTop: "var(--space-3)" }}>
          Built-in tool: {name}
        </h1>
        <div className="notice">
          Built-in tool configuration requires the <strong>platform_admin</strong> role.
        </div>
      </section>
    );
  }
  return (
    <section>
      <BackLink />
      <BuiltinToolConfig name={name} />
    </section>
  );
}

// RegistryToolRoute edits one registered tool inline.
function RegistryToolRoute({ id, isAdmin }: { readonly id: string; readonly isAdmin: boolean }) {
  const resource = useApi<RegistryResource>(`/registry/tools/${encodeURIComponent(id)}`, 0);

  return (
    <section>
      <BackLink />
      {resource.error ? (
        <EmptyState
          title="Tool not found"
          hint={resource.error || "This registered tool does not exist (or you cannot access it)."}
        />
      ) : resource.loading && !resource.data ? (
        <div className="notice">Loading tool…</div>
      ) : resource.data ? (
        <RegistryToolForm resource={resource.data} isAdmin={isAdmin} onSaved={() => resource.refresh()} />
      ) : null}
    </section>
  );
}

// parseJsonField mirrors the settings-page manifest validation: blank →
// omit, non-object JSON → throw with a readable message.
function parseJsonField(value: string, label: string): string | undefined {
  const trimmed = value.trim();
  if (trimmed === "") return undefined;
  JSON.parse(trimmed);
  if (typeof JSON.parse(trimmed) !== "object") throw new Error(`${label} must be a JSON object or array`);
  return trimmed;
}

// RegistryToolForm is the inline edit surface for a registered tool.
// Mounted with its resource already loaded, so no effect-driven state
// hydration is needed.
function RegistryToolForm({
  resource,
  isAdmin,
  onSaved,
}: {
  readonly resource: RegistryResource;
  readonly isAdmin: boolean;
  readonly onSaved: () => void;
}) {
  const { token } = useAuth();
  const router = useRouter();
  const [name, setName] = useState(resource.name);
  const [description, setDescription] = useState(resource.description ?? "");
  const [endpoint, setEndpoint] = useState(resource.endpoint ?? "");
  const [authRef, setAuthRef] = useState(resource.auth_ref ?? "");
  const [manifest, setManifest] = useState(
    resource.manifest ? JSON.stringify(resource.manifest, null, 2) : "",
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [savedNote, setSavedNote] = useState("");

  const dirty = useMemo(() => {
    return (
      name !== resource.name ||
      description !== (resource.description ?? "") ||
      endpoint !== (resource.endpoint ?? "") ||
      authRef !== (resource.auth_ref ?? "") ||
      manifest !== (resource.manifest ? JSON.stringify(resource.manifest, null, 2) : "")
    );
  }, [name, description, endpoint, authRef, manifest, resource]);

  async function save() {
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
      await apiPatch(`/registry/tools/${resource.id}`, token, body);
      setSavedNote("Saved.");
      onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : "save failed");
    } finally {
      setBusy(false);
    }
  }

  const field = "field";
  return (
    <>
      <div className="section-head" style={{ marginTop: "var(--space-3)" }}>
        <h1 className="page-title">{resource.name}</h1>
        <span className="entity-side">
          <StatusChip status={resource.status === "active" ? "idle" : resource.status === "deprecated" ? "paused" : "error"} />
          {isAdmin ? (
            <DeleteResourceButton
              path={`/registry/tools/${resource.id}`}
              name={resource.name}
              onDeleted={() => router.push("/settings/resources/tools")}
            />
          ) : null}
        </span>
      </div>
      {!isAdmin ? (
        <div className="notice">
          You have read-only access. Editing registered tools requires the <strong>platform_admin</strong> role.
        </div>
      ) : null}
      <div className="card" style={{ display: "flex", flexDirection: "column", gap: "var(--space-3)", maxWidth: 640 }}>
        <label className={field}>
          <span>Name</span>
          <input value={name} disabled={!isAdmin || busy} onChange={(e) => { setName(e.target.value); setSavedNote(""); }} />
        </label>
        <label className={field}>
          <span>Description</span>
          <textarea value={description} disabled={!isAdmin || busy} onChange={(e) => { setDescription(e.target.value); setSavedNote(""); }} />
        </label>
        <div className="field-row">
          <label className={field}>
            <span>Endpoint</span>
            <input value={endpoint} disabled={!isAdmin || busy} placeholder="https://… (if applicable)" onChange={(e) => { setEndpoint(e.target.value); setSavedNote(""); }} />
          </label>
          <label className={field}>
            <span>Auth ref</span>
            <input value={authRef} disabled={!isAdmin || busy} placeholder="secret / vault ref (never the secret itself)" onChange={(e) => { setAuthRef(e.target.value); setSavedNote(""); }} />
          </label>
        </div>
        <label className={field}>
          <span>Manifest (JSON, optional)</span>
          <textarea value={manifest} disabled={!isAdmin || busy} placeholder='{"version": 1, ...}' onChange={(e) => { setManifest(e.target.value); setSavedNote(""); }} />
        </label>
        {isAdmin ? (
          <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
            <button
              type="button"
              className="btn btn-primary"
              disabled={busy || !dirty || name.trim() === ""}
              onClick={() => { void save(); }}
            >
              Save changes
            </button>
            {savedNote ? (
              <output className="notice" aria-live="polite">{savedNote}</output>
            ) : null}
            {error ? (
              <span className="notice error" role="alert">{error}</span>
            ) : null}
          </div>
        ) : null}
      </div>
    </>
  );
}
