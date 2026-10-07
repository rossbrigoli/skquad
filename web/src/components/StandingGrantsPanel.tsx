"use client";

// TG-8 slice D: Standing Grants panel (docs/tg8-grant-approvals-spec.md
// §C/§D). Owners see standing grants on their own resources; platform
// admins see all (server-side scoping on GET /standing-grants). Revoke
// is a soft delete (DELETE /standing-grants/{id}) effective ≤30s via the
// gateway cache TTL. Expired and revoked rows stay visible but are
// visually distinct — they are the audit trail of pre-authorization.

import { useCallback, useState } from "react";
import { ConfirmDialog } from "./ConfirmDialog";
import { EmptyState } from "./EmptyState";
import { useAuth } from "../lib/auth";
import { useApi } from "../lib/useApi";
import { revokeStandingGrant } from "../lib/grantsApi";
import { standingGrantStatus, type StandingGrant } from "../lib/grantsWorkflow";

const STATUS_META: Record<"live" | "expired" | "revoked", { label: string; chip: string }> = {
  live: { label: "active", chip: "chip chip-done" },
  expired: { label: "expired", chip: "chip chip-paused" },
  revoked: { label: "revoked", chip: "chip chip-blocked" },
};

function errMessage(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback;
}

export function StandingGrantsPanel() {
  const { token } = useAuth();
  const list = useApi<StandingGrant[]>("/standing-grants", 30000);
  const [pendingRevoke, setPendingRevoke] = useState<StandingGrant | null>(null);
  const [error, setError] = useState("");

  const doRevoke = useCallback(async () => {
    if (!pendingRevoke) return;
    try {
      await revokeStandingGrant(token, pendingRevoke.id);
      setError("");
      list.refresh();
    } catch (err) {
      setError(errMessage(err, "revoke failed"));
    } finally {
      setPendingRevoke(null);
    }
  }, [pendingRevoke, token, list]);

  const grants = Array.isArray(list.data) ? list.data : [];
  const now = new Date();

  return (
    <section className="standing-grants-panel">
      <div className="section-head">
        <h2>Standing grants</h2>
      </div>
      <p className="standing-grants-intro">
        &ldquo;Approve this and future&rdquo; decisions pre-authorize an agent&rsquo;s tool calls until
        they expire. Revoking takes effect immediately (gateway cache ≤30s).
      </p>
      {list.error ? <div className="notice error">{list.error}</div> : null}
      {error ? <div className="notice error" role="alert">{error}</div> : null}
      {grants.length === 0 && !list.loading ? (
        <EmptyState
          title="No standing grants"
          hint="Approving a confirmation with “Approve This and Future” creates one here."
        />
      ) : (
        <table className="data-table standing-grants-table" aria-label="Standing grants">
          <thead>
            <tr>
              <th>Resource</th>
              <th>Tool</th>
              <th>Agent</th>
              <th>Expires</th>
              <th>Created by</th>
              <th>Status</th>
              <th aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {grants.map((g) => {
              const status = standingGrantStatus(g, now);
              const meta = STATUS_META[status];
              return (
                <tr key={g.id} className={`standing-grant-row standing-grant-${status}`}>
                  <td>{g.resource_id}</td>
                  <td><code>{g.tool}</code></td>
                  <td>{g.agent_id}</td>
                  <td>{g.expires_at ? g.expires_at.slice(0, 10) : "—"}</td>
                  <td>{g.created_by}</td>
                  <td>
                    <span className={meta.chip}>{meta.label}</span>
                    {g.revoked_at ? (
                      <span className="standing-grant-revoked-at"> since {g.revoked_at.slice(0, 10)}</span>
                    ) : null}
                  </td>
                  <td>
                    {status === "live" ? (
                      <button
                        type="button"
                        className="btn btn-sm btn-danger standing-grant-revoke"
                        onClick={() => setPendingRevoke(g)}
                      >
                        Revoke
                      </button>
                    ) : null}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
      {pendingRevoke ? (
        <ConfirmDialog
          title="Revoke standing grant"
          body={`Stop auto-approving ${pendingRevoke.agent_id} running ${pendingRevoke.tool} on ${pendingRevoke.resource_id}? Future calls will page you for confirmation again.`}
          confirmLabel="Revoke"
          onConfirm={doRevoke}
          onClose={() => setPendingRevoke(null)}
        />
      ) : null}
    </section>
  );
}
