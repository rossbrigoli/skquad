"use client";

// S-193: the Inbox rebuilt as an email-like list. Messages from agents
// (send_inbox) and the system land here for the human owner. Unread
// rows are bold with a dot; clicking a row marks it read and expands the
// body. Messages are NEVER auto-removed — the only removal path is the
// explicit Delete on a row. Platform admins get a Filter control to
// view any user's inbox (default: own).

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { ConfirmDialog } from "../../components/ConfirmDialog";
import { EmptyState } from "../../components/EmptyState";
import { apiDelete, apiGet, apiPost, type ApiUser, type InboxMessage } from "../../lib/api";
import { useAuth } from "../../lib/auth";
import { formatRelativeTime } from "../../lib/format";
import {
  buildScopedListQuery,
  inboxDisplay,
  inboxKindMeta,
  inboxTaskLink,
  isUnread,
  unreadCount,
} from "../../lib/notifications";

type UserFilter = { mode: "own" } | { mode: "user"; userId: string };

export default function InboxPage() {
  const { token, user, authed } = useAuth();
  const isAdmin = user?.role === "platform_admin";

  const [messages, setMessages] = useState<InboxMessage[]>([]);
  const [users, setUsers] = useState<ApiUser[]>([]);
  const [filter, setFilter] = useState<UserFilter>({ mode: "own" });
  const [unreadOnly, setUnreadOnly] = useState(false);
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [pendingDelete, setPendingDelete] = useState<InboxMessage | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const effectiveUserId = filter.mode === "user" ? filter.userId : undefined;

  const load = useCallback(async () => {
    if (!authed) return;
    try {
      const query = buildScopedListQuery({ unread: unreadOnly, userId: effectiveUserId, limit: 200 });
      const next = await apiGet<InboxMessage[]>(`/inbox${query}`, token);
      setMessages(Array.isArray(next) ? next : []);
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "inbox fetch failed");
    } finally {
      setLoading(false);
    }
  }, [authed, token, unreadOnly, effectiveUserId]);

  useEffect(() => {
    void load();
  }, [load]);

  // The admin filter needs the user directory once.
  useEffect(() => {
    if (!isAdmin || !authed) return;
    apiGet<ApiUser[]>("/users", token)
      .then((list) => setUsers(Array.isArray(list) ? list : []))
      .catch(() => setUsers([]));
  }, [isAdmin, authed, token]);

  const unread = useMemo(() => unreadCount(messages), [messages]);

  const markRead = useCallback(
    async (message: InboxMessage) => {
      if (!isUnread(message.read_at)) return;
      try {
        const updated = await apiPost<InboxMessage>(`/inbox/${message.id}/read`, token, {});
        setMessages((prev) => prev.map((m) => (m.id === message.id ? updated : m)));
      } catch {
        // Transient: a manual refresh re-syncs; never block the UI.
      }
    },
    [token],
  );

  const toggleRow = useCallback(
    (message: InboxMessage) => {
      setExpandedId((current) => (current === message.id ? null : message.id));
      void markRead(message);
    },
    [markRead],
  );

  const doDelete = useCallback(async () => {
    if (!pendingDelete) return;
    try {
      await apiDelete(`/inbox/${pendingDelete.id}`, token);
      setMessages((prev) => prev.filter((m) => m.id !== pendingDelete.id));
      if (expandedId === pendingDelete.id) setExpandedId(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "delete failed");
    } finally {
      setPendingDelete(null);
    }
  }, [pendingDelete, token, expandedId]);

  const renderRow = (message: InboxMessage) => {
    const unreadRow = isUnread(message.read_at);
    const kind = inboxKindMeta(message.kind);
    const { subject, body } = inboxDisplay(message);
    const taskHref = inboxTaskLink(message);
    const expanded = expandedId === message.id;
    return (
      <div key={message.id} className={`inbox-row ${unreadRow ? "inbox-row-unread" : ""}`}>
        <div className="inbox-row-head" role="button" tabIndex={0} onClick={() => toggleRow(message)}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === " ") {
              e.preventDefault();
              toggleRow(message);
            }
          }}
        >
          <span className="inbox-dot" aria-hidden="true" data-unread={unreadRow ? "yes" : "no"} />
          <span className="inbox-subject">{subject}</span>
          <span className={`chip ${kind.className}`}>{kind.label}</span>
          <span className="inbox-time">{formatRelativeTime(message.created_at)}</span>
          <button
            type="button"
            className="btn btn-small inbox-delete"
            aria-label="Delete message"
            onClick={(e) => {
              e.stopPropagation();
              setPendingDelete(message);
            }}
          >
            Delete
          </button>
        </div>
        {expanded ? (
          <div className="inbox-row-body">
            <p className="inbox-body-text">{body}</p>
            <div className="inbox-body-meta">
              {message.from_agent_id ? <span>from agent {message.from_agent_id.slice(0, 8)}</span> : null}
              {taskHref ? (
                <Link className="inbox-task-link" href={taskHref} onClick={() => void markRead(message)}>
                  Open task →
                </Link>
              ) : null}
            </div>
          </div>
        ) : null}
      </div>
    );
  };

  return (
    <AuthGate>
      <AppShell>
        <div className="inbox-header">
          <h1 className="page-title">
            Inbox{" "}
            {unread > 0 ? <span className="chip chip-blocked">{unread} unread</span> : null}
          </h1>
          <div className="inbox-controls">
            <label className="inbox-filter">
              <span>Show</span>
              <select
                value={unreadOnly ? "unread" : "all"}
                onChange={(e) => setUnreadOnly(e.target.value === "unread")}
              >
                <option value="all">All messages</option>
                <option value="unread">Unread only</option>
              </select>
            </label>
            {isAdmin ? (
              <label className="inbox-filter">
                <span>User</span>
                <select
                  value={filter.mode === "user" ? filter.userId : "own"}
                  onChange={(e) => {
                    const v = e.target.value;
                    setFilter(v === "own" ? { mode: "own" } : { mode: "user", userId: v });
                    setExpandedId(null);
                  }}
                >
                  <option value="own">My inbox</option>
                  {users
                    .filter((u) => u.id !== user?.id)
                    .map((u) => (
                      <option key={u.id} value={u.id}>
                        {u.name || u.email}
                      </option>
                    ))}
                </select>
              </label>
            ) : null}
          </div>
        </div>
        {error ? <div className="notice error">{error}</div> : null}
        {loading && messages.length === 0 ? (
          <EmptyState title="Loading your inbox…" hint="Agent and system messages addressed to you." />
        ) : messages.length === 0 ? (
          <EmptyState
            title="Your inbox is empty"
            hint="Ask an agent to send something to your inbox and it will land here — unread until you open it."
          />
        ) : (
          <div className="entity-list inbox-list">{messages.map(renderRow)}</div>
        )}
        {pendingDelete ? (
          <ConfirmDialog
            title="Delete inbox message"
            body="Inbox messages are never removed automatically — deleting this one is permanent. Continue?"
            confirmLabel="Delete"
            onConfirm={doDelete}
            onClose={() => setPendingDelete(null)}
          />
        ) : null}
      </AppShell>
    </AuthGate>
  );
}
