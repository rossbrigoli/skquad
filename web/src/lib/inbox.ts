// S-174: agent inbox + dead-letter client logic.
//
// Pure, browser-independent helpers for the Inbox tab (agent screen) and
// the admin Dead letters screen. Shapes mirror the control-plane contract:
//   GET    /api/v1/agents/{id}/inbox?limit=N        owner | platform_admin
//   POST   /api/v1/agents/{id}/messages/{mid}/replay owner | platform_admin
//   GET    /api/v1/admin/dead-letters?...            platform_admin
//   POST   /api/v1/admin/dead-letters/{mid}/replay   platform_admin
//   DELETE /api/v1/admin/dead-letters/{mid}          platform_admin

export type InboxMessageRow = {
  id: string;
  from_type: string;
  from_id: string;
  to_agent_id: string;
  squad_id: string;
  type: string;
  status: string;
  correlation_id?: string;
  attempts: number;
  max_attempts: number;
  next_retry_at?: string;
  expires_at?: string;
  timeout_at?: string;
  terminal_reason?: string;
  created_at: string;
  delivered_at?: string;
  payload?: Record<string, unknown>;
};

export type AgentInboxSnapshot = {
  pending_count: number;
  oldest_pending_at?: string;
  retrying_count: number;
  delivered_count: number;
  dead_count: number;
  pending: InboxMessageRow[];
  retrying: InboxMessageRow[];
  delivered: InboxMessageRow[];
  dead: InboxMessageRow[];
};

export type DeadLetterQuery = {
  squad?: string;
  agent?: string;
  type?: string;
  reason?: string;
  since?: string;
  until?: string;
  limit?: number;
};

// buildDeadLetterQuery turns the admin screen's filter form into a query
// string. Empty/whitespace-only fields are dropped so the API never sees
// `?squad=` noise.
export function buildDeadLetterQuery(query: DeadLetterQuery): string {
  const params = new URLSearchParams();
  const assign = (key: string, value?: string) => {
    const trimmed = (value ?? "").trim();
    if (trimmed !== "") params.set(key, trimmed);
  };
  assign("squad", query.squad);
  assign("agent", query.agent);
  assign("type", query.type);
  assign("reason", query.reason);
  assign("since", query.since);
  assign("until", query.until);
  if (query.limit && query.limit > 0) params.set("limit", String(query.limit));
  const qs = params.toString();
  return qs === "" ? "" : `?${qs}`;
}

// retryLabel renders "attempt 2 of 3" for the retrying section.
export function retryLabel(message: InboxMessageRow): string {
  return `attempt ${message.attempts} of ${message.max_attempts}`;
}

// isConsultTimeout marks the synthetic S-173 notices so the UI can badge
// them instead of rendering them as ordinary dead letters or replies.
export function isConsultTimeout(message: InboxMessageRow): boolean {
  return message.terminal_reason === "consult_timeout" || message.payload?.consult_timeout === true;
}

// inboxIsEmpty drives the empty state.
export function inboxIsEmpty(snapshot: AgentInboxSnapshot | null): boolean {
  if (!snapshot) return true;
  return (
    snapshot.pending_count === 0 &&
    snapshot.retrying_count === 0 &&
    snapshot.delivered_count === 0 &&
    snapshot.dead_count === 0
  );
}
