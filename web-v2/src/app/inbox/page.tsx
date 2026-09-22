"use client";

import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { EmptyState } from "../../components/EmptyState";
import { EntityRow } from "../../components/EntityRow";
import { useApi } from "../../lib/useApi";
import { formatRelativeTime } from "../../lib/format";
import type { InboxMessage } from "../../lib/api";

// Scaffold version: raw inbox feed. The full attention queue (stalled tasks,
// stale reviews, budget warnings merged + prioritised) lands in UIv2-4.
export default function InboxPage() {
  const inbox = useApi<InboxMessage[]>("/inbox", 15000);
  const items = inbox.data || [];

  return (
    <AuthGate>
      <AppShell>
        <h1 className="page-title">Inbox</h1>
        {inbox.error ? <div className="notice error">{inbox.error}</div> : null}
        {!inbox.loading && items.length === 0 ? (
          <EmptyState
            title="Nothing needs you right now"
            hint="Completed work and action-required items from your squads land here."
          />
        ) : (
          <div className="entity-list">
            {items.map((item) => (
              <EntityRow
                key={item.id}
                href={item.task_id ? `/squads/${item.squad_id}` : `/squads/${item.squad_id}`}
                title={item.message}
                meta={formatRelativeTime(item.created_at)}
                side={
                  item.kind === "action_required" ? (
                    <span className="chip chip-stalled">action required</span>
                  ) : (
                    <span className="chip chip-done">completed</span>
                  )
                }
              />
            ))}
          </div>
        )}
      </AppShell>
    </AuthGate>
  );
}
