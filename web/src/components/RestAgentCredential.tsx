"use client";

// TG-4c (S-259): per-agent BYO REST credential control on a grant row.
//
// A granted REST resource resolves credentials per (resource, agent):
// the agent's OWN secret wins, otherwise the resource-level default is
// used. This control shows which is in effect and lets the resource
// owner / admin set, rotate or clear the agent's own credential.
// Inputs are WRITE-ONLY (type=password, never prefilled, never
// re-displayed) — mirroring the BYO registration posture. The probe
// endpoint exposes existence + kind only, never values.

import { useCallback, useEffect, useState } from "react";

import { apiDelete, apiGet, apiPut } from "../lib/api";
import { REST_AUTH_FIELDS, type RestAuthKind } from "../lib/restResources";

type CredentialProbe = {
  resource_id: string;
  agent_id: string;
  auth_kind: RestAuthKind;
  has_own_credential: boolean;
  source: "agent" | "resource_default";
};

function credPath(resourceId: string, agentId: string): string {
  return `/registry/rest/${resourceId}/agent-credentials/${agentId}`;
}

export function RestAgentCredential({
  resourceId,
  agentId,
  token,
}: Readonly<{
  resourceId: string;
  agentId: string;
  token: string;
}>) {
  const [probe, setProbe] = useState<CredentialProbe | null>(null);
  const [editing, setEditing] = useState(false);
  const [values, setValues] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const refresh = useCallback(async () => {
    try {
      setProbe(await apiGet<CredentialProbe>(credPath(resourceId, agentId), token));
    } catch {
      setProbe(null);
    }
  }, [resourceId, agentId, token]);

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot probe on mount / identity change
    void refresh();
  }, [refresh]);

  if (!probe) return null;

  const fields = REST_AUTH_FIELDS[probe.auth_kind] ?? [];

  async function save() {
    setBusy(true);
    setError("");
    try {
      await apiPut(credPath(resourceId, agentId), token, { auth: values });
      setEditing(false);
      setValues({});
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "could not store the credential");
    } finally {
      setBusy(false);
    }
  }

  async function clearOwn() {
    setBusy(true);
    setError("");
    try {
      await apiDelete(credPath(resourceId, agentId), token);
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "could not clear the credential");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="entity-meta" style={{ display: "grid", gap: "4px" }}>
      <span data-testid="cred-indicator">
        credential:{" "}
        {probe.has_own_credential ? (
          <strong>this agent&apos;s own ({probe.auth_kind})</strong>
        ) : (
          <span>resource default ({probe.auth_kind})</span>
        )}
      </span>
      <span style={{ display: "flex", gap: "6px" }}>
        {!editing && fields.length > 0 ? (
          <button type="button" className="btn btn-sm" disabled={busy} onClick={() => setEditing(true)}>
            {probe.has_own_credential ? "Rotate own" : "Set own"}
          </button>
        ) : null}
        {editing ? (
          <span style={{ display: "grid", gap: "4px", width: "100%" }}>
            {fields.map((f) => (
              <input
                key={f.field}
                type={f.secret ? "password" : "text"}
                placeholder={f.label}
                aria-label={f.label}
                autoComplete="new-password"
                value={values[f.field] ?? ""}
                onChange={(e) => setValues((v) => ({ ...v, [f.field]: e.target.value }))}
              />
            ))}
            <span style={{ display: "flex", gap: "6px" }}>
              <button type="button" className="btn btn-sm" disabled={busy} onClick={() => void save()}>
                {busy ? "Saving…" : "Save credential"}
              </button>
              <button
                type="button"
                className="btn btn-sm"
                disabled={busy}
                onClick={() => {
                  setEditing(false);
                  setValues({});
                }}
              >
                Cancel
              </button>
            </span>
          </span>
        ) : null}
        {probe.has_own_credential && !editing ? (
          <button type="button" className="btn btn-sm btn-danger" disabled={busy} onClick={() => void clearOwn()}>
            Clear (use default)
          </button>
        ) : null}
      </span>
      {error ? (
        <span className="entity-meta" role="alert" style={{ color: "var(--color-danger, #b00020)" }}>
          {error}
        </span>
      ) : null}
    </div>
  );
}
