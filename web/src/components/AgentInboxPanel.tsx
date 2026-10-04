"use client";

// S-174: Agent Inbox tab — a read-only observability panel (not a mail
// client). Sections: pending (count + oldest waiting), retrying
// (attempts / max / next retry), recently delivered, and dead letters
// with their terminal reason. The agent's owner (or a platform admin)
// can replay a dead message straight from the dead section.
//
// Correlation grouping: every row carries its correlation_id so a
// consult→reply chain is traceable across sections without extra joins.
//
// S-178: restyled to read like an email inbox — left-aligned rows with
// a status accent bar, a subject line (type + sender), the message
// excerpt, and the timestamp pinned to the right.

import { useState } from "react";
import { apiPost, ApiError } from "../lib/api";
import { useAuth } from "../lib/auth";
import { useApi } from "../lib/useApi";
import {
  inboxIsEmpty,
  isConsultTimeout,
  retryLabel,
  type AgentInboxSnapshot,
  type InboxMessageRow,
} from "../lib/inbox";

function formatTime(value?: string): string {
  if (!value) return "—";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

function shortId(id: string): string {
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}

function messageExcerpt(message: InboxMessageRow): string {
  const raw = message.payload?.message;
  if (typeof raw === "string" && raw.trim() !== "") {
    return raw.length > 120 ? `${raw.slice(0, 120)}…` : raw;
  }
  return "(no message body)";
}

/** The most meaningful timestamp for the row's right-hand column. */
function rowTime(message: InboxMessageRow): string {
  if (message.status === "delivered") return formatTime(message.delivered_at ?? message.created_at);
  if (message.status === "dead") return formatTime(message.delivered_at ?? message.created_at);
  if (message.status === "pending" && message.attempts > 0) return formatTime(message.next_retry_at ?? message.created_at);
  return formatTime(message.created_at);
}

function MessageRow({
  message,
  onReplay,
  busyId,
  showReplay,
}: {
  readonly message: InboxMessageRow;
  readonly onReplay?: (message: InboxMessageRow) => void;
  readonly busyId?: string;
  readonly showReplay?: boolean;
}) {
  const timeout = isConsultTimeout(message);
  return (
    <div className={`inbox-row inbox-${message.status}`}>
      <div className="inbox-main">
        <div className="inbox-subject">
          <span className="inbox-type">{message.type}</span>
          <span className="inbox-from">from {message.from_type}</span>
          {message.correlation_id ? (
            <span className="inbox-thread mono" title={message.correlation_id}>
              thread {shortId(message.correlation_id)}
            </span>
          ) : null}
          {timeout ? <span className="inbox-flag">consult timeout</span> : null}
          <span className="inbox-id mono">{shortId(message.id)}</span>
        </div>
        <div className="inbox-excerpt">{messageExcerpt(message)}</div>
        <div className="inbox-status">
          {message.status === "pending" && message.attempts > 0 ? (
            <>{retryLabel(message)} · next retry {formatTime(message.next_retry_at)}</>
          ) : null}
          {message.status === "pending" && message.attempts === 0 ? <>waiting</> : null}
          {message.status === "delivered" ? <>delivered</> : null}
          {message.status === "dead" ? <>died · reason: {message.terminal_reason ?? "unknown"}</> : null}
        </div>
      </div>
      <div className="inbox-side">
        <span className="inbox-time">{rowTime(message)}</span>
        {showReplay && onReplay ? (
          <button
            type="button"
            className="btn btn-sm"
            disabled={busyId === message.id}
            onClick={() => onReplay(message)}
          >
            {busyId === message.id ? "Replaying…" : "Replay"}
          </button>
        ) : null}
      </div>
    </div>
  );
}

function Section({ title, count, children }: { readonly title: string; readonly count: number; readonly children: React.ReactNode }) {
  return (
    <section className="inbox-section">
      <div className="section-head">
        <h3>{title} <span className="metric-chip-sub">({count})</span></h3>
      </div>
      {count === 0 ? <p className="field-hint">Nothing here.</p> : <div className="inbox-list">{children}</div>}
    </section>
  );
}

export function AgentInboxPanel({ agentId }: { readonly agentId: string }) {
  const { token, mode } = useAuth();
  const snapshot = useApi<AgentInboxSnapshot>(`/agents/${agentId}/inbox?limit=50`, 15000);
  const [busyId, setBusyId] = useState("");
  const [note, setNote] = useState("");

  const authedToken = mode === "oidc" ? "" : token;

  async function replayDeadMessage(message: InboxMessageRow) {
    setBusyId(message.id);
    setNote("");
    try {
      await apiPost<InboxMessageRow>(`/agents/${agentId}/messages/${message.id}/replay`, authedToken, {});
      setNote(`Replayed ${shortId(message.id)} — it is pending again.`);
      snapshot.refresh();
    } catch (err) {
      setNote(err instanceof ApiError ? `Replay failed: ${err.message}` : "Replay failed.");
    } finally {
      setBusyId("");
    }
  }

  if (snapshot.error) {
    return <p className="field-hint">Inbox unavailable: {snapshot.error}</p>;
  }
  if (inboxIsEmpty(snapshot.data)) {
    return <p className="field-hint">This agent&apos;s inbox is empty — nothing pending, retrying, delivered, or dead.</p>;
  }

  const data = snapshot.data;
  return (
    <section className="inbox-panel" style={{ marginTop: "var(--space-4, 16px)" }}>
      {note ? <p className="field-hint">{note}</p> : null}
      <Section title="Pending" count={data?.pending_count ?? 0}>
        {data?.oldest_pending_at ? (
          <p className="field-hint">Oldest waiting since {formatTime(data.oldest_pending_at)}</p>
        ) : null}
        {(data?.pending ?? []).map((m) => (
          <MessageRow key={m.id} message={m} />
        ))}
      </Section>
      <Section title="Retrying" count={data?.retrying_count ?? 0}>
        {(data?.retrying ?? []).map((m) => (
          <MessageRow key={m.id} message={m} />
        ))}
      </Section>
      <Section title="Recently delivered" count={data?.delivered_count ?? 0}>
        {(data?.delivered ?? []).map((m) => (
          <MessageRow key={m.id} message={m} />
        ))}
      </Section>
      <Section title="Dead letters" count={data?.dead_count ?? 0}>
        {(data?.dead ?? []).map((m) => (
          <MessageRow key={m.id} message={m} showReplay busyId={busyId} onReplay={(msg) => void replayDeadMessage(msg)} />
        ))}
      </Section>
    </section>
  );
}
