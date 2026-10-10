"use client";

// TG-8 slice D: grant-request review view (docs/tg8-grant-approvals-spec.md
// §D). Owners review requests on resources they own (mine=owner); platform
// admins additionally get the admin co-sign queue (mine=admin). Expanding a
// row reveals the linter findings (block = red, warn = amber) and the
// requested-scope summary. Approve buttons follow the slice-B authority
// model: approve-owner for pending_owner (owner or admin), approve-admin
// for pending_admin (platform_admin only). Deny carries a reason.

import { useCallback, useState } from "react";
import { EmptyState } from "./EmptyState";
import { useAuth } from "../lib/auth";
import { useApi } from "../lib/useApi";
import {
  approveGrantAdmin,
  approveGrantOwner,
  denyGrantRequest,
} from "../lib/grantsApi";
import {
  countFindingsBySeverity,
  grantRequestStateMeta,
  findingSeverityClass,
  summarizeScope,
  type GrantRequest,
} from "../lib/grantsWorkflow";

function errMessage(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback;
}

export function GrantRequestsPanel({ isAdmin }: { readonly isAdmin: boolean }) {
  return (
    <div className="grant-requests-panel">
      <RequestsSection
        key="owner"
        title="Requests on resources you own"
        path="/grant-requests?mine=owner"
        isAdmin={isAdmin}
      />
      {isAdmin ? (
        <RequestsSection
          key="admin"
          title="Admin co-sign queue"
          path="/grant-requests?mine=admin"
          isAdmin
        />
      ) : null}
    </div>
  );
}

function RequestsSection({
  title,
  path,
  isAdmin,
}: {
  readonly title: string;
  readonly path: string;
  readonly isAdmin: boolean;
}) {
  const { token } = useAuth();
  const list = useApi<GrantRequest[]>(path, 30000);
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [denyingId, setDenyingId] = useState<string | null>(null);
  const [reason, setReason] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const act = useCallback(
    async (action: () => Promise<unknown>) => {
      if (busy) return;
      setBusy(true);
      setError("");
      try {
        await action();
        setDenyingId(null);
        setReason("");
        list.refresh();
      } catch (err) {
        setError(errMessage(err, "grant request action failed"));
      } finally {
        setBusy(false);
      }
    },
    [busy, list],
  );

  const items = Array.isArray(list.data) ? list.data : [];

  return (
    <section className="grant-requests-section">
      <div className="section-head">
        <h2>{title}</h2>
      </div>
      {list.error ? <div className="notice error">{list.error}</div> : null}
      {error ? <div className="notice error" role="alert">{error}</div> : null}
      {items.length === 0 && !list.loading ? (
        <EmptyState
          title="No grant requests here"
          hint="Requests appear when an agent needs access that requires your decision."
        />
      ) : (
        <div className="entity-list">
          {items.map((req) => (
            <GrantRequestRow
              key={req.id}
              request={req}
              isAdmin={isAdmin}
              expanded={expandedId === req.id}
              denying={denyingId === req.id}
              busy={busy}
              reason={reason}
              onReasonChange={setReason}
              onToggle={() => setExpandedId((cur) => (cur === req.id ? null : req.id))}
              onStartDeny={() => {
                setDenyingId(req.id);
                setExpandedId(req.id);
                setReason("");
              }}
              onCancelDeny={() => setDenyingId(null)}
              onApproveOwner={() => act(() => approveGrantOwner(token, req.id))}
              onApproveAdmin={() => act(() => approveGrantAdmin(token, req.id))}
              onDeny={() => act(() => denyGrantRequest(token, req.id, reason.trim() || "denied"))}
            />
          ))}
        </div>
      )}
    </section>
  );
}

// S-268/S3776: the approve/deny button cluster extracted from
// GrantRequestRow so the row renderer stays under the complexity cap.
function GrantRequestActions({
  request,
  isAdmin,
  busy,
  onApproveOwner,
  onApproveAdmin,
  onStartDeny,
}: {
  readonly request: GrantRequest;
  readonly isAdmin: boolean;
  readonly busy: boolean;
  readonly onApproveOwner: () => void;
  readonly onApproveAdmin: () => void;
  readonly onStartDeny: () => void;
}) {
  const canApproveOwner = request.state === "pending_owner";
  const canApproveAdmin = request.state === "pending_admin" && isAdmin;
  const canDeny = request.state === "pending_owner" || (request.state === "pending_admin" && isAdmin);
  const meta = grantRequestStateMeta(request.state);
  return (
    <div className="entity-side">
      <span className={meta.className}>{meta.label}</span>
      {canApproveOwner ? (
        <button type="button" className="btn btn-sm btn-primary grant-approve-owner" disabled={busy} onClick={onApproveOwner}>
          Approve{isAdmin ? " as owner" : ""}
        </button>
      ) : null}
      {canApproveAdmin ? (
        <button type="button" className="btn btn-sm btn-primary grant-approve-admin" disabled={busy} onClick={onApproveAdmin}>
          Approve as admin
        </button>
      ) : null}
      {canDeny ? (
        <button type="button" className="btn btn-sm btn-danger grant-deny" disabled={busy} onClick={onStartDeny}>
          Deny
        </button>
      ) : null}
    </div>
  );
}

// S-268/S3776: the expanded detail (findings, scope, stamps, deny
// form) extracted from GrantRequestRow.
function GrantRequestDetail({
  request,
  denying,
  busy,
  reason,
  onReasonChange,
  onDeny,
  onCancelDeny,
}: {
  readonly request: GrantRequest;
  readonly denying: boolean;
  readonly busy: boolean;
  readonly reason: string;
  readonly onReasonChange: (v: string) => void;
  readonly onDeny: () => void;
  readonly onCancelDeny: () => void;
}) {
  const scopeRows = summarizeScope(request.requested_scope);
  return (
    <div className="grant-request-detail">
      {request.findings && request.findings.length > 0 ? (
        <div className="findings-list" aria-label="Linter findings">
          {request.findings.map((f, idx) => (
            <div key={`${f.code}-${idx}`} className={findingSeverityClass(f.severity)}>
              <strong>{f.severity.toUpperCase()}</strong> <code>{f.code}</code> — {f.detail}
            </div>
          ))}
        </div>
      ) : (
        <p className="grant-request-no-findings">No linter findings on this change.</p>
      )}
      {scopeRows.length > 0 ? (
        <table className="grant-scope-table" aria-label="Requested scope">
          <tbody>
            {scopeRows.map((row) => (
              <tr key={row.label}>
                <th scope="row">{row.label}</th>
                <td>{row.value}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        <p className="grant-request-no-findings">Requested scope: default (no overrides).</p>
      )}
      {request.denied_reason ? <p className="grant-request-denied-reason">Denied: {request.denied_reason}</p> : null}
      {request.approved_by_owner_at ? (
        <p className="grant-request-stamp">Owner approved: {request.approved_by_owner_at}</p>
      ) : null}
      {request.approved_by_admin_at ? (
        <p className="grant-request-stamp">Admin approved: {request.approved_by_admin_at}</p>
      ) : null}
      {denying ? (
        <div className="grant-deny-form">
          <label className="field">
            <span>Denial reason (shared with the requester)</span>
            <input value={reason} onChange={(e) => onReasonChange(e.target.value)} aria-label="Denial reason" />
          </label>
          <div className="confirmation-action-row">
            <button type="button" className="btn btn-danger" disabled={busy} onClick={onDeny}>
              Confirm Deny
            </button>
            <button type="button" className="btn" disabled={busy} onClick={onCancelDeny}>
              Cancel
            </button>
          </div>
        </div>
      ) : null}
    </div>
  );
}

function GrantRequestRow({
  request,
  isAdmin,
  expanded,
  denying,
  busy,
  reason,
  onReasonChange,
  onToggle,
  onStartDeny,
  onCancelDeny,
  onApproveOwner,
  onApproveAdmin,
  onDeny,
}: {
  readonly request: GrantRequest;
  readonly isAdmin: boolean;
  readonly expanded: boolean;
  readonly denying: boolean;
  readonly busy: boolean;
  readonly reason: string;
  readonly onReasonChange: (v: string) => void;
  readonly onToggle: () => void;
  readonly onStartDeny: () => void;
  readonly onCancelDeny: () => void;
  readonly onApproveOwner: () => void;
  readonly onApproveAdmin: () => void;
  readonly onDeny: () => void;
}) {
  const counts = countFindingsBySeverity(request.findings);

  return (
    <div className={`entity-row grant-request-row${counts.block > 0 ? " grant-request-blocked" : ""}`}>
      <div className="entity-main">
        <button type="button" className="grant-request-toggle" onClick={onToggle} aria-expanded={expanded}>
          <span className="entity-title">
            {request.agent_id ?? "squad grant"} → {request.resource_id}
          </span>
          <span className="entity-meta">
            tier {request.tier} · requested by {request.requester_user_id}
            {counts.block > 0 ? ` · ${counts.block} block` : ""}
            {counts.warn > 0 ? ` · ${counts.warn} warn` : ""}
          </span>
        </button>
      </div>
      <GrantRequestActions
        request={request}
        isAdmin={isAdmin}
        busy={busy}
        onApproveOwner={onApproveOwner}
        onApproveAdmin={onApproveAdmin}
        onStartDeny={onStartDeny}
      />
      {expanded ? (
        <GrantRequestDetail
          request={request}
          denying={denying}
          busy={busy}
          reason={reason}
          onReasonChange={onReasonChange}
          onDeny={onDeny}
          onCancelDeny={onCancelDeny}
        />
      ) : null}
    </div>
  );
}
