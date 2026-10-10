// TG-9 slice C: Audit drill-down logic — pure, browser-independent.
//
// Backend endpoints (control-plane/internal/httpapi/server.go):
//   GET /api/v1/audit?squad_id=<id>&limit=<n>      — platform admin only
//   GET /api/v1/squads/{squadID}/audit?limit=<n>   — squad owner or platform admin
//
// Both return a flat array of AuditEntry (newest first). The server caps
// `limit` at 500 (default 100) and supports NO other query parameters.
//
// Backend gaps (reported, deliberately not fixed here):
//   * Agent / task / resource / time-range / decision filters are NOT
//     supported server-side — they are applied client-side over the
//     fetched window (max 500 events). A server-side filter API would
//     remove the window truncation.
//   * tool-gateway dispatch audit events (tool-gateway/internal/audit
//     Event: gate outcome, resource, operation, decision) are emitted to
//     the stdout sink only — they are not queryable through the control
//     plane, so gateway-level gate rows appear here only when the CP
//     itself recorded a matching audit entry (confirmation.* actions).
//
// Decision-code mapping mirrors the stable codes owned by the backend:
//   - control-plane/internal/httpapi/confirmations.go (consume denials)
//   - tool-gateway/internal/httpapi/confirmationgate.go (gate outcomes)
//   - tool-gateway/internal/drivers/browser (session/ceiling/busy codes)

import type { AuditEntry } from "./api";

export type DecisionKind = "allow" | "deny" | "pending";

export type DecisionInfo = {
  kind: DecisionKind;
  code: string;
  label: string;
};

// STABLE_DECISION_CODES maps every stable decision/denial code the
// backend surfaces to a human-readable label and its kind. Codes come
// from the TG-8 confirmation flow (CP + gateway) and the browser
// driver. Unknown codes fall back to a humanised form of the raw code.
export const STABLE_DECISION_CODES: Record<string, { label: string; kind: DecisionKind }> = {
  denied_replayed: { label: "already used", kind: "deny" },
  args_hash_mismatch: { label: "arguments changed since approval", kind: "deny" },
  approval_expired: { label: "approval expired", kind: "deny" },
  standing_grant_not_live: { label: "standing grant not active", kind: "deny" },
  denied_by_owner: { label: "denied by owner", kind: "deny" },
  confirmation_expired: { label: "confirmation window expired", kind: "deny" },
  confirmation_pending: { label: "awaiting owner", kind: "pending" },
  pending_confirmation: { label: "awaiting owner approval", kind: "pending" },
  session_invalid: { label: "session invalid", kind: "deny" },
  ceiling_exceeded: { label: "limit exceeded", kind: "deny" },
  browser_busy: { label: "browser busy", kind: "deny" },
  confirmation_unavailable: { label: "approval service unavailable", kind: "deny" },
};

// GATE_LABELS covers the confirmation-gate outcomes recorded in audit
// metadata (`gate` field written by the gateway gate / CP events).
const GATE_LABELS: Record<string, { label: string; kind: DecisionKind }> = {
  auto: { label: "allowed (standing grant)", kind: "allow" },
  once: { label: "allowed (one-shot approval)", kind: "allow" },
  standing: { label: "allowed (standing grant)", kind: "allow" },
  pending: { label: "awaiting owner approval", kind: "pending" },
  denied: { label: "denied by owner", kind: "deny" },
  unavailable: { label: "approval service unavailable", kind: "deny" },
};

// baseCode strips the free-text detail off a CP denial reason
// ("denied_by_owner: not today" → "denied_by_owner"). Mirrors the
// gateway's confirmationDenialCode.
export function baseCode(reason: string): string {
  const trimmed = (reason ?? "").trim();
  const idx = trimmed.indexOf(":");
  return (idx >= 0 ? trimmed.slice(0, idx) : trimmed).trim();
}

// decisionLabel renders a stable code human-readably; unknown codes
// degrade to the underscore-spaced raw code.
export function decisionLabel(code: string): string {
  const known = STABLE_DECISION_CODES[code];
  if (known) return known.label;
  if (!code) return "allowed";
  return code.replace(/_/g, " ");
}

// parseAuditMetadata normalises the audit `metadata` field: the Go
// server sends JSON (parsed into an object by fetch), but a raw string
// shows up in fixtures and older payloads. Returns null when absent or
// unparseable.
export function parseAuditMetadata(entry: AuditEntry): Record<string, unknown> | null {
  const raw = entry.metadata;
  if (raw == null) return null;
  if (typeof raw === "object") return raw as Record<string, unknown>;
  if (typeof raw === "string" && raw.trim() !== "") {
    try {
      const parsed = JSON.parse(raw);
      return parsed && typeof parsed === "object" ? (parsed as Record<string, unknown>) : null;
    } catch {
      return null;
    }
  }
  return null;
}

function str(v: unknown): string {
  return typeof v === "string" ? v.trim() : "";
}

// deriveDecision extracts the allow/deny/pending decision and stable
// code for one audit entry. Precedence:
//   1. metadata.gate (gateway/CP gate outcome)
//   2. confirmation.* action names (owner decisions)
//   3. metadata.reason carrying a stable code
//   4. "*_denied" / "deny" action names
//   5. otherwise: a plain successful mutation → allow
export function deriveDecision(entry: AuditEntry): DecisionInfo {
  const meta = parseAuditMetadata(entry);
  const gate = str(meta?.gate);
  if (gate) {
    const mapped = GATE_LABELS[gate];
    if (mapped) {
      // A denied gate carries the stable reason code in metadata.reason.
      if (gate === "denied") {
        const code = baseCode(str(meta?.reason)) || "denied_by_owner";
        return { kind: "deny", code, label: decisionLabel(code) };
      }
      // The gate's "unavailable" outcome maps to the stable
      // confirmation_unavailable code.
      if (gate === "unavailable") {
        return {
          kind: "deny",
          code: "confirmation_unavailable",
          label: decisionLabel("confirmation_unavailable"),
        };
      }
      return { kind: mapped.kind, code: gate, label: mapped.label };
    }
    return { kind: "deny", code: gate, label: decisionLabel(gate) };
  }

  const action = entry.action ?? "";
  if (action.startsWith("confirmation.approve")) {
    return { kind: "allow", code: "owner_approved", label: "approved by owner" };
  }
  if (action === "confirmation.standing_match") {
    return { kind: "allow", code: "standing_grant", label: "allowed (standing grant)" };
  }
  if (action === "confirmation.deny") {
    const code = baseCode(str(meta?.reason)) || "denied_by_owner";
    return { kind: "deny", code, label: decisionLabel(code) };
  }

  const reason = baseCode(str(meta?.reason));
  if (reason && STABLE_DECISION_CODES[reason]) {
    const mapped = STABLE_DECISION_CODES[reason];
    return { kind: mapped.kind, code: reason, label: mapped.label };
  }
  if (action.endsWith("_denied") || action.includes(".deny")) {
    const code = reason || "denied_by_owner";
    return { kind: "deny", code, label: decisionLabel(code) };
  }
  return { kind: "allow", code: "", label: "allowed" };
}

// deriveTier returns the risk tier recorded on the entry, if any.
export function deriveTier(entry: AuditEntry): string {
  const meta = parseAuditMetadata(entry);
  return str(meta?.risk_tier) || str(meta?.tier);
}

// taskRefFor returns the task a entry relates to: an explicit
// metadata.task_id, or the resource itself when it IS a task.
export function taskRefFor(entry: AuditEntry): string {
  const meta = parseAuditMetadata(entry);
  const fromMeta = str(meta?.task_id);
  if (fromMeta) return fromMeta;
  return entry.resource_type === "task" ? entry.resource_id : "";
}

// formatActor renders the actor column: resolved display name first,
// then a type-appropriate fallback.
export function formatActor(entry: AuditEntry): string {
  const display = str(entry.actor_display);
  if (display) return display;
  if (entry.actor_type === "system") return "system";
  return str(entry.actor_id) || entry.actor_type || "unknown";
}

// ---------------------------------------------------------------------------
// Filters
// ---------------------------------------------------------------------------

export const AUDIT_LIMIT_DEFAULT = 200;
export const AUDIT_LIMIT_MAX = 500; // server-side cap (boundedIntQuery)

export type AuditFilters = {
  squadId?: string;
  agentId?: string;
  taskId?: string;
  resourceId?: string;
  decision?: "" | DecisionKind;
  since?: string; // ISO instant
  until?: string; // ISO instant
  limit?: number;
};

// clampAuditLimit keeps the requested limit inside the server's window.
export function clampAuditLimit(limit: number | undefined): number {
  if (limit == null || !Number.isFinite(limit) || limit <= 0) return AUDIT_LIMIT_DEFAULT;
  return Math.min(Math.floor(limit), AUDIT_LIMIT_MAX);
}

// buildAuditPath constructs the API path for the filters. Only
// squad_id and limit are sent — everything else is client-side because
// the backend does not support more (see module header).
// Returns "" when a non-admin has no squad selected (nothing fetchable:
// the global /audit endpoint is platform-admin-only).
export function buildAuditPath(filters: AuditFilters, isAdmin: boolean): string {
  const params = new URLSearchParams();
  const squadId = (filters.squadId ?? "").trim();
  if (isAdmin) {
    if (squadId) params.set("squad_id", squadId);
    params.set("limit", String(clampAuditLimit(filters.limit)));
    return `/audit?${params.toString()}`;
  }
  if (!squadId) return "";
  params.set("limit", String(clampAuditLimit(filters.limit)));
  return `/squads/${encodeURIComponent(squadId)}/audit?${params.toString()}`;
}

// filterAuditEntries applies the client-side filters over the fetched
// window. Empty/whitespace filter values are ignored. Entries without a
// timestamp are dropped only when a time bound is active.
function withinTimeRange(
  entry: AuditEntry,
  sinceMs: number,
  untilMs: number,
): boolean {
  const ts = entry.timestamp ? Date.parse(entry.timestamp) : NaN;
  if (!Number.isFinite(ts)) return false;
  if (Number.isFinite(sinceMs) && ts < sinceMs) return false;
  if (Number.isFinite(untilMs) && ts > untilMs) return false;
  return true;
}

export function filterAuditEntries(entries: AuditEntry[], filters: AuditFilters): AuditEntry[] {
  const agentId = (filters.agentId ?? "").trim();
  const taskId = (filters.taskId ?? "").trim();
  const resourceId = (filters.resourceId ?? "").trim();
  const sinceMs = filters.since ? Date.parse(filters.since) : NaN;
  const untilMs = filters.until ? Date.parse(filters.until) : NaN;
  const hasRange = Number.isFinite(sinceMs) || Number.isFinite(untilMs);

  return entries.filter((entry) => {
    if (agentId && !(entry.actor_type === "agent" && entry.actor_id === agentId)) return false;
    if (taskId && taskRefFor(entry) !== taskId) return false;
    if (resourceId && entry.resource_id !== resourceId) return false;
    if (filters.decision && deriveDecision(entry).kind !== filters.decision) return false;
    if (hasRange && !withinTimeRange(entry, sinceMs, untilMs)) return false;
    return true;
  });
}

// hasActiveFilters powers the "clear filters" affordance.
export function hasActiveFilters(filters: AuditFilters): boolean {
  return (
    (filters.agentId ?? "").trim() !== "" ||
    (filters.taskId ?? "").trim() !== "" ||
    (filters.resourceId ?? "").trim() !== "" ||
    Boolean(filters.decision) ||
    Boolean(filters.since) ||
    Boolean(filters.until)
  );
}
