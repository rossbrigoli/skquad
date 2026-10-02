"use client";

// S-174: platform-admin dead-letter screen. Lists every dead message
// across squads with filters (squad / agent / type / reason / time
// window), and per-message REPLAY (same semantics as the owner replay,
// any target) and PRUNE (hard delete, admin-only).
//
// Bulk prune (all dead older than N days) is a deliberate v1 omission —
// see the card; per-row prune keeps the audit trail granular.

import { useEffect, useState } from "react";
import { apiDelete, apiGet, apiPost, ApiError } from "../lib/api";
import { useAuth } from "../lib/auth";
import { buildDeadLetterQuery, isConsultTimeout, type DeadLetterQuery, type InboxMessageRow } from "../lib/inbox";

type DeadLettersResponse = { dead_letters?: InboxMessageRow[] };

const MESSAGE_TYPES = ["consult", "delegate", "handoff", "ping", "reply"];

function formatTime(value?: string): string {
  if (!value) return "—";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

function shortId(id: string): string {
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}

export function DeadLettersPanel() {
  const { token, mode } = useAuth();
  const authedToken = mode === "oidc" ? "" : token;
  const [filters, setFilters] = useState<DeadLetterQuery>({});
  const [items, setItems] = useState<InboxMessageRow[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [note, setNote] = useState("");
  const [busyId, setBusyId] = useState("");

  function setFilter(key: keyof DeadLetterQuery, value: string) {
    setFilters((prev) => ({ ...prev, [key]: value }));
  }

  async function search() {
    setLoading(true);
    setNote("");
    try {
      // S-205 root cause: this panel used a raw fetch against the
      // NEXT_PUBLIC api-base env var, bypassing apiBaseUrl(). Under OIDC
      // (the only deployed mode) every other screen routes through the
      // server-side /proxy which attaches the bearer token; the direct
      // /api/v1 call carried no credentials and died at 401, so the list
      // never populated. apiGet honours the proxy override.
      const payload = await apiGet<DeadLettersResponse>(`/admin/dead-letters${buildDeadLetterQuery(filters)}`, authedToken);
      setItems(payload.dead_letters ?? []);
      setNote(`${payload.dead_letters?.length ?? 0} dead letter(s) shown.`);
    } catch (err) {
      setNote(err instanceof Error ? `Search failed: ${err.message}` : "Search failed.");
    } finally {
      setLoading(false);
    }
  }

  // S-205: load the first page on mount so the screen isn't blank until
  // an explicit Search click.
  useEffect(() => {
    // Deferred off the effect's synchronous path to avoid the cascading-
    // render lint (state only changes after the fetch resolves).
    const timer = setTimeout(() => void search(), 0);
    return () => clearTimeout(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function replay(message: InboxMessageRow) {
    setBusyId(message.id);
    setNote("");
    try {
      await apiPost<InboxMessageRow>(`/admin/dead-letters/${message.id}/replay`, authedToken, {});
      setNote(`Replayed ${shortId(message.id)}.`);
      void search();
    } catch (err) {
      setNote(err instanceof ApiError ? `Replay failed: ${err.message}` : "Replay failed.");
    } finally {
      setBusyId("");
    }
  }

  async function prune(message: InboxMessageRow) {
    if (!window.confirm(`Permanently delete dead message ${shortId(message.id)}? This cannot be undone.`)) {
      return;
    }
    setBusyId(message.id);
    setNote("");
    try {
      await apiDelete(`/admin/dead-letters/${message.id}`, authedToken);
      setNote(`Pruned ${shortId(message.id)}.`);
      void search();
    } catch (err) {
      setNote(err instanceof ApiError ? `Prune failed: ${err.message}` : "Prune failed.");
    } finally {
      setBusyId("");
    }
  }

  return (
    <section style={{ marginTop: "var(--space-4, 16px)" }}>
      <div className="section-head">
        <h2>Dead letters</h2>
      </div>
      <p className="field-hint">
        Messages that exhausted their delivery attempts or expired. Replay puts one back on the queue; prune deletes it forever.
      </p>
      {/* S-205: filters now use the app's standard .field / .field-row form
          components (same styled inputs + selects as every settings dialog)
          instead of unstyled bare controls. */}
      <div className="field-row">
        <label className="field">
          <span>Squad</span>
          <input aria-label="Squad id" placeholder="squad id" value={filters.squad ?? ""} onChange={(e) => setFilter("squad", e.target.value)} />
        </label>
        <label className="field">
          <span>Agent</span>
          <input aria-label="Agent id" placeholder="agent id" value={filters.agent ?? ""} onChange={(e) => setFilter("agent", e.target.value)} />
        </label>
      </div>
      <div className="field-row">
        <label className="field">
          <span>Message type</span>
          <select aria-label="Message type" value={filters.type ?? ""} onChange={(e) => setFilter("type", e.target.value)}>
            <option value="">any type</option>
            {MESSAGE_TYPES.map((t) => (
              <option key={t} value={t}>{t}</option>
            ))}
          </select>
        </label>
        <label className="field">
          <span>Reason contains</span>
          <input aria-label="Reason contains" placeholder="reason contains" value={filters.reason ?? ""} onChange={(e) => setFilter("reason", e.target.value)} />
        </label>
      </div>
      <div className="field-row">
        <label className="field">
          <span>Since</span>
          <input aria-label="Since" type="datetime-local" value={filters.since ?? ""} onChange={(e) => setFilter("since", e.target.value)} />
        </label>
        <label className="field">
          <span>Until</span>
          <input aria-label="Until" type="datetime-local" value={filters.until ?? ""} onChange={(e) => setFilter("until", e.target.value)} />
        </label>
      </div>
      <button type="button" className="btn btn-sm" disabled={loading} onClick={() => void search()}>
        {loading ? "Searching…" : "Search"}
      </button>
      {note ? <p className="field-hint">{note}</p> : null}
      {items === null ? null : items.length === 0 ? <p className="field-hint">No dead letters match.</p> : null}
      {(items ?? []).map((m) => (
        <div key={m.id} style={{ padding: "8px 0", borderBottom: "1px solid var(--border, #ddd)" }}>
          <div style={{ display: "flex", gap: "8px", alignItems: "center", flexWrap: "wrap" }}>
            <strong>{m.type}</strong>
            <span className="metric-chip-sub" title={m.id}>{shortId(m.id)}</span>
            <span className="metric-chip-sub">to agent {shortId(m.to_agent_id)}</span>
            <span className="metric-chip-sub">squad {shortId(m.squad_id)}</span>
            {isConsultTimeout(m) ? <span className="status-chip" style={{ color: "var(--danger, #b00020)" }}>consult timeout</span> : null}
            <button type="button" className="btn btn-sm" disabled={busyId === m.id} onClick={() => void replay(m)}>
              Replay
            </button>
            <button type="button" className="btn btn-sm" disabled={busyId === m.id} onClick={() => void prune(m)}>
              Prune
            </button>
          </div>
          <div className="metric-chip-sub">
            died {formatTime(m.delivered_at)} · attempts {m.attempts}/{m.max_attempts} · reason: {m.terminal_reason || "unknown"}
          </div>
        </div>
      ))}
    </section>
  );
}
