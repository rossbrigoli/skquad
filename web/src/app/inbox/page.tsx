"use client";

// S-193: the Inbox rebuilt as an email-like list. Messages from agents
// (send_inbox) and the system land here for the human owner.
//
// S-201: the inbox now reads like Gmail. The list shows a sender column
// (the agent's name, resolved through the AttentionProvider's agent
// directory), unread rows carry a bold subject and a dot, and clicking a
// row drills through to a full-message reading view with a ← back arrow
// instead of expanding inline. Opening a message marks it read, which
// also decrements the nav "Inbox" badge. Messages are NEVER auto-removed
// — the only removal path is the explicit Delete in the reading view.
// Platform admins keep the Filter control to view any user's inbox.

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { ConfirmDialog } from "../../components/ConfirmDialog";
import { EmptyState } from "../../components/EmptyState";
import { apiDelete, apiGet, type ApiUser, type InboxMessage } from "../../lib/api";
import { useAuth } from "../../lib/auth";
import { useAttention } from "../../lib/useAttention";
import { formatRelativeTime } from "../../lib/format";
import {
  buildScopedListQuery,
  inboxDisplay,
  inboxKindMeta,
  inboxSender,
  inboxTaskLink,
  isUnread,
  unreadCount,
} from "../../lib/notifications";

type UserFilter = { mode: "own" } | { mode: "user"; userId: string };

export default function InboxPage() {
  const { token, user, authed } = useAuth();
  const { agentName, markRead: markAttentionRead } = useAttention();
  const isAdmin = user?.role === "platform_admin";

  const [messages, setMessages] = useState<InboxMessage[]>([]);
  const [users, setUsers] = useState<ApiUser[]>([]);
  const [filter, setFilter] = useState<UserFilter>({ mode: "own" });
  const [unreadOnly, setUnreadOnly] = useState(false);
  const [selectedId, setSelectedId] = useState<string | null>(null);
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
  const selected = useMemo(
    () => (selectedId === null ? null : (messages.find((m) => m.id === selectedId) ?? null)),
    [messages, selectedId],
  );

  const markRead = useCallback(
    async (message: InboxMessage) => {
      if (!isUnread(message.read_at)) return;
      // Optimistic: the row (and the nav badge via the provider) flips to
      // read immediately; the provider owns the POST so one request keeps
      // both surfaces in sync.
      setMessages((prev) =>
        prev.map((m) => (m.id === message.id ? { ...m, read_at: new Date().toISOString() } : m)),
      );
      try {
        await markAttentionRead(message.id);
      } catch {
        // Transient: a manual refresh re-syncs; never block the UI.
      }
    },
    [markAttentionRead],
  );

  const openMessage = useCallback(
    (message: InboxMessage) => {
      setSelectedId(message.id);
      void markRead(message);
    },
    [markRead],
  );

  const doDelete = useCallback(async () => {
    if (!pendingDelete) return;
    try {
      await apiDelete(`/inbox/${pendingDelete.id}`, token);
      setMessages((prev) => prev.filter((m) => m.id !== pendingDelete.id));
      if (selectedId === pendingDelete.id) setSelectedId(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "delete failed");
    } finally {
      setPendingDelete(null);
    }
  }, [pendingDelete, token, selectedId]);

  const renderRow = (message: InboxMessage) => {
    const unreadRow = isUnread(message.read_at);
    const kind = inboxKindMeta(message.kind);
    const { subject } = inboxDisplay(message);
    return (
      <div
        key={message.id}
        className={`inbox-row ${unreadRow ? "inbox-row-unread" : ""}`}
        role="button"
        tabIndex={0}
        aria-label={`Open message: ${subject}`}
        onClick={() => openMessage(message)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            openMessage(message);
          }
        }}
      >
        <span className="inbox-dot" aria-hidden="true" data-unread={unreadRow ? "yes" : "no"} />
        <span className="inbox-sender">{inboxSender(message, agentName)}</span>
        <span className="inbox-subject">{subject}</span>
        <span className={`chip ${kind.className}`}>{kind.label}</span>
        <span className="inbox-time">{formatRelativeTime(message.created_at)}</span>
      </div>
    );
  };

  const renderDetail = (message: InboxMessage) => {
    const kind = inboxKindMeta(message.kind);
    const { subject, body } = inboxDisplay(message);
    const taskHref = inboxTaskLink(message);
    return (
      <div className="inbox-detail">
        <div className="inbox-detail-topbar">
          <button
            type="button"
            className="btn btn-small inbox-back"
            aria-label="Back to inbox"
            onClick={() => setSelectedId(null)}
          >
            ← Inbox
          </button>
          <button
            type="button"
            className="btn btn-small btn-danger inbox-delete"
            aria-label="Delete message"
            onClick={() => setPendingDelete(message)}
          >
            Delete
          </button>
        </div>
        <h2 className="inbox-detail-subject">{subject}</h2>
        <div className="inbox-detail-meta">
          <span className="inbox-detail-from">{inboxSender(message, agentName)}</span>
          <span className={`chip ${kind.className}`}>{kind.label}</span>
          <span className="inbox-time">{formatRelativeTime(message.created_at)}</span>
        </div>
        <div className="inbox-detail-body">
          <p className="inbox-body-text">{body}</p>
        </div>
        {taskHref ? (
          <div className="inbox-detail-footer">
            <Link className="inbox-task-link" href={taskHref}>
              Open task →
            </Link>
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
          {!selected ? (
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
                      setSelectedId(null);
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
          ) : null}
        </div>
        {error ? <div className="notice error">{error}</div> : null}
        {selected ? (
          renderDetail(selected)
        ) : loading && messages.length === 0 ? (
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
