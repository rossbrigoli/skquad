"use client";

import { ApiState, InboxMessage } from "../lib/api";
import { StateNotice, formatRelativeTime } from "./shared";

export function InboxSection({
  inbox,
  onMarkRead,
}: {
  inbox: ApiState<InboxMessage[]>;
  onMarkRead: (id: string) => void;
}) {
  const items = inbox.data || [];
  const unread = items.filter((item) => !item.read_at).length;
  return (
    <div className="workflow-grid">
      <div className="span-3">
        <div className="filter-bar">
          <span className="filter-count">
            {items.length === 0 ? "No notifications" : `${items.length} messages · ${unread} unread`}
          </span>
        </div>
        <StateNotice state={inbox} empty="Nothing in the inbox yet. Agents post here when they finish a task or need your approval." />
        <div className="stack-list">
          {items.map((item) => (
            <article key={item.id} className={item.read_at ? "message-item inbox-read" : "message-item inbox-unread"}>
              <strong>
                <span className={`inbox-kind ${item.kind === "action_required" ? "action" : "done"}`}>
                  {item.kind === "action_required" ? "Action needed" : "Task completed"}
                </span>
              </strong>
              <span>{item.message || "-"}</span>
              <small>
                {formatRelativeTime(item.created_at)}
                {item.task_id && ` · task ${item.task_id}`}
              </small>
              {!item.read_at && (
                <button type="button" className="secondary small" onClick={() => onMarkRead(item.id)}>
                  Mark read
                </button>
              )}
            </article>
          ))}
        </div>
      </div>
    </div>
  );
}
