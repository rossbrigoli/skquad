// TG-8 slice D: client-side types + pure helpers for the approval
// workflows (docs/tg8-grant-approvals-spec.md §D). Shapes mirror the
// control-plane JSON exactly (domain.GrantRequest, domain.PendingConfirmation,
// domain.StandingGrant from slices B/C1 — see control-plane/internal/domain/types.go).

export type GrantLintFinding = {
  code: string;
  severity: string; // "block" | "warn"
  detail: string;
};

export type GrantRequestState = "pending_owner" | "pending_admin" | "approved" | "denied";

export type GrantRequest = {
  id: string;
  resource_id: string;
  agent_id?: string;
  requester_user_id: string;
  tier: string;
  state: GrantRequestState;
  requested_scope?: Record<string, unknown> | null;
  findings?: GrantLintFinding[] | null;
  approved_by_owner_at?: string;
  approved_by_admin_at?: string;
  denied_reason?: string;
  expiry?: string;
  created_at: string;
  updated_at?: string;
};

export type ConfirmationState =
  | "pending"
  | "approved_once"
  | "approved_standing"
  | "denied"
  | "expired";

export type PendingConfirmation = {
  id: string;
  resource_id: string;
  agent_id: string;
  tool: string;
  args_hash: string;
  state: ConfirmationState;
  // The C1 backend links the confirmation → the owner's action_required
  // inbox message via this field (the inbox message itself carries NO
  // structured confirmation id — join from this side).
  inbox_message_id?: string;
  requested_by: string;
  denied_reason?: string;
  approved_at?: string;
  consumed_at?: string;
  created_at: string;
  updated_at?: string;
};

export type StandingGrant = {
  id: string;
  resource_id: string;
  agent_id: string;
  tool: string;
  expires_at: string;
  created_by: string;
  created_at: string;
  revoked_at?: string;
};

// STANDING_GRANT_DEFAULT_DAYS mirrors the backend's standingGrantDefaultExpiry
// (confirmations.go): "approve this and future" defaults to 90 days.
export const STANDING_GRANT_DEFAULT_DAYS = 90;

const DAY_MS = 24 * 60 * 60 * 1000;

// defaultExpiryDateString: the "Approve This and Future" picker default —
// today + 90 days in <input type="date"> YYYY-MM-DD form.
export function defaultExpiryDateString(now: Date = new Date()): string {
  return new Date(now.getTime() + STANDING_GRANT_DEFAULT_DAYS * DAY_MS)
    .toISOString()
    .slice(0, 10);
}

// dateInputToIso converts a date-picker value into an RFC3339 timestamp at
// the END of that UTC day, so "expires 2027-01-05" stays alive through the
// whole final day instead of dying at midnight. Invalid input yields "".
export function dateInputToIso(value: string): string {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(value)) return "";
  return `${value}T23:59:59Z`;
}

// standingGrantStatus: revoked wins over expired so a revoked grant never
// renders as merely "expired" (the audit trail distinction matters).
export function standingGrantStatus(
  grant: StandingGrant,
  now: Date = new Date(),
): "revoked" | "expired" | "live" {
  if (grant.revoked_at) return "revoked";
  if (new Date(grant.expires_at).getTime() <= now.getTime()) return "expired";
  return "live";
}

export function confirmationIsPending(state: ConfirmationState): boolean {
  return state === "pending";
}

// confirmationDecisionLabel: the badge shown once the owner has decided.
export function confirmationDecisionLabel(state: ConfirmationState): string {
  switch (state) {
    case "approved_once":
      return "Approved (one-shot)";
    case "approved_standing":
      return "Approved — standing grant";
    case "denied":
      return "Denied";
    case "expired":
      return "Expired";
    default:
      return "";
  }
}

export function confirmationDecisionChip(state: ConfirmationState): string {
  switch (state) {
    case "approved_once":
    case "approved_standing":
      return "chip chip-done";
    case "denied":
      return "chip chip-blocked";
    case "expired":
      return "chip chip-paused";
    default:
      return "chip";
  }
}

export function grantRequestStateMeta(state: GrantRequestState): {
  label: string;
  className: string;
} {
  switch (state) {
    case "pending_owner":
      return { label: "awaiting owner", className: "chip chip-in-review" };
    case "pending_admin":
      return { label: "awaiting admin", className: "chip chip-in-review" };
    case "approved":
      return { label: "approved", className: "chip chip-done" };
    case "denied":
      return { label: "denied", className: "chip chip-blocked" };
    default:
      return { label: String(state), className: "chip" };
  }
}

// findingSeverityClass maps linter severities to the visual language:
// block = red, warn = amber (spec §D).
export function findingSeverityClass(severity: string): string {
  return severity === "block" ? "finding finding-block" : "finding finding-warn";
}

export function countFindingsBySeverity(
  findings: GrantLintFinding[] | null | undefined,
): { block: number; warn: number } {
  const counts = { block: 0, warn: 0 };
  for (const f of findings ?? []) {
    if (f.severity === "block") counts.block += 1;
    else if (f.severity === "warn") counts.warn += 1;
  }
  return counts;
}

// matchConfirmationForInboxMessage joins the confirmation onto the inbox
// message. The C1 inbox payload has NO structured confirmation id — the
// confirmation row carries inbox_message_id, so the join runs from the
// confirmation side against the pending-confirmations list.
export function matchConfirmationForInboxMessage(
  confirmations: PendingConfirmation[] | null | undefined,
  messageId: string,
): PendingConfirmation | null {
  return (confirmations ?? []).find((c) => c.inbox_message_id === messageId) ?? null;
}

// shortArgsHash mirrors the backend's display truncation (12 chars + …).
export function shortArgsHash(hash: string): string {
  return hash.length <= 12 ? hash : `${hash.slice(0, 12)}…`;
}

// summarizeScope renders a grant request's requested_scope object into
// display rows. Backend gap (reported, not fixed): the request row carries
// only the requested ("after") scope — no before snapshot — so the UI's
// "before/after diff summary" is the requested scope plus the linter
// findings detail (which encodes what was gained/widened).
export function summarizeScope(scope: unknown): { label: string; value: string }[] {
  if (!scope || typeof scope !== "object") return [];
  const s = scope as Record<string, unknown>;
  const rows: { label: string; value: string }[] = [];
  const list = (key: string, label: string) => {
    const v = s[key];
    if (Array.isArray(v) && v.length > 0) rows.push({ label, value: v.map(String).join(", ") });
  };
  list("hosts", "Hosts");
  list("http_methods", "HTTP methods");
  list("mcp_tools", "MCP tools");
  if (s.has_credential === true) rows.push({ label: "Credential", value: "yes" });
  const caps = s.numeric_caps;
  if (caps && typeof caps === "object") {
    const parts = Object.entries(caps as Record<string, unknown>)
      .map(([k, v]) => `${k}=${String(v)}`)
      .join(", ");
    if (parts !== "") rows.push({ label: "Numeric caps", value: parts });
  }
  return rows;
}
