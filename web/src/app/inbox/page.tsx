"use client";

import Link from "next/link";
import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { EmptyState } from "../../components/EmptyState";
import { useAttention } from "../../lib/useAttention";
import { formatRelativeTime } from "../../lib/format";
import type { AttentionReason } from "../../lib/attention";

const REASON_CHIP: Record<AttentionReason, { label: string; className: string }> = {
  stalled: { label: "stalled", className: "chip chip-stalled" },
  agent_error: { label: "agent error", className: "chip chip-error" },
  blocked: { label: "blocked", className: "chip chip-blocked" },
  action_required: { label: "needs you", className: "chip chip-blocked" },
  stale_review: { label: "stale review", className: "chip chip-in-review" },
  completed: { label: "completed", className: "chip chip-done" },
};

export default function InboxPage() {
  const { items, loading, error, markRead } = useAttention();

  const urgent = items.filter((item) => REASON_CHIP[item.reason].className !== "chip chip-done");
  const done = items.filter((item) => REASON_CHIP[item.reason].className === "chip chip-done");

  const renderRow = (item: (typeof items)[number]) => {
    const chip = REASON_CHIP[item.reason];
    const isMessage = item.id.startsWith("action:") || item.id.startsWith("done:");
    const messageId = isMessage ? item.id.split(":")[1] : null;
    return (
      <div key={item.id} className="entity-row">
        <Link href={item.href} className="entity-main" style={{ display: "block" }}>
          <span className="entity-title">{item.title}</span>
          <span className="entity-meta">
            {item.meta} · {formatRelativeTime(item.createdAt)}
          </span>
        </Link>
        <div className="entity-side" style={{ display: "flex", alignItems: "center", gap: "var(--space-2)" }}>
          <span className={chip.className}>{chip.label}</span>
          {messageId ? (
            <button type="button" className="btn" onClick={() => void markRead(messageId)}>
              Mark read
            </button>
          ) : null}
        </div>
      </div>
    );
  };

  return (
    <AuthGate>
      <AppShell>
        <h1 className="page-title">Inbox</h1>
        {error ? <div className="notice error">{error}</div> : null}
        {loading && items.length === 0 ? (
          <EmptyState title="Loading your attention queue…" hint="Merging stalled work, agent errors, blocked tasks and messages." />
        ) : items.length === 0 ? (
          <EmptyState
            title="All clear — nothing needs you"
            hint="Stalled runs, blocked tasks, stale reviews and agent messages land here, most urgent first."
          />
        ) : (
          <>
            {urgent.length > 0 ? (
              <section>
                <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-3)" }}>
                  Needs attention <span className="chip chip-stalled">{urgent.length}</span>
                </h2>
                <div className="entity-list">{urgent.map(renderRow)}</div>
              </section>
            ) : (
              <EmptyState title="Nothing urgent" hint="No stalled runs, agent errors or blocked work right now." />
            )}
            {done.length > 0 ? (
              <section style={{ marginTop: "var(--space-5)" }}>
                <h2 style={{ fontSize: "var(--text-lg)", margin: "0 0 var(--space-3)" }}>
                  Completed <span className="chip chip-done">{done.length}</span>
                </h2>
                <div className="entity-list">{done.map(renderRow)}</div>
              </section>
            ) : null}
          </>
        )}
      </AppShell>
    </AuthGate>
  );
}
