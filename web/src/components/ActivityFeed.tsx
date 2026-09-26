"use client";

import Link from "next/link";
import type { AuditEntry } from "../lib/api";
import { formatRelativeTime } from "../lib/format";

// Humanise audit actions ("task.created" -> "created task") so the feed reads
// like a story instead of an event log. Unknown actions render verbatim.
function humanizeAction(action: string): string {
  const parts = action.split(/[._]/);
  if (parts.length < 2) {
    return action;
  }
  const verb = parts[0].replace(/_/g, " ");
  const noun = parts.slice(1).join(" ");
  return `${verb} ${noun}`;
}

function entityHref(squadId: string, entry: AuditEntry): string | null {
  switch (entry.resource_type) {
    case "task":
      return `/squads/${squadId}/tasks/${entry.resource_id}`;
    case "agent":
      return `/squads/${squadId}/agents/${entry.resource_id}`;
    case "board":
      return `/squads/${squadId}/board`;
    default:
      return null;
  }
}

// ActivityFeed renders a compact, deep-linked audit trail. Empty input renders
// the supplied empty state so callers control the copy per surface.
export function ActivityFeed({
  squadId,
  entries,
  emptyTitle,
  emptyHint,
  nameFor,
}: {
  squadId: string;
  entries: AuditEntry[];
  emptyTitle: string;
  emptyHint: string;
  nameFor?: (entry: AuditEntry) => string | undefined;
}) {
  if (entries.length === 0) {
    return (
      <div className="empty-state">
        <div className="empty-title">{emptyTitle}</div>
        <div className="empty-hint">{emptyHint}</div>
      </div>
    );
  }
  return (
    <div className="entity-list">
      {entries.map((entry) => {
        const actor = nameFor?.(entry) || (entry.actor_type === "user" ? entry.actor_id : entry.actor_type);
        const href = entityHref(squadId, entry);
        const body = (
          <div className="entity-main">
            <span className="entity-title">
              <strong>{actor}</strong> {humanizeAction(entry.action)}
            </span>
            <span className="entity-meta mono">
              {entry.resource_type} {entry.resource_id.slice(0, 8)}
            </span>
          </div>
        );
        const side = <span className="entity-meta">{formatRelativeTime(entry.timestamp)}</span>;
        return href ? (
          <Link key={entry.id} href={href} className="entity-row">
            {body}
            <div className="entity-side">{side}</div>
          </Link>
        ) : (
          <div key={entry.id} className="entity-row">
            {body}
            <div className="entity-side">{side}</div>
          </div>
        );
      })}
    </div>
  );
}
