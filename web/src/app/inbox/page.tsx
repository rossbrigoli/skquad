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
//
// S-207 enhancements on top of the S-201 layout:
//  1. filter dropdowns restyled to the platform field standard,
//  2. prominent separators + roomier vertical spacing between rows,
//  3. Gmail/Outlook-style envelope icon per row (unread = filled,
//     read = open outline),
//  4. per-row checkbox for multi-select,
//  5. bulk "Mark as Read" / "Delete" in the toolbar (disabled until
//     something is selected; delete asks for confirmation),
//  6. rows read "Title — body preview" with ellipsis truncation,
//  7. a column header row above the list,
//  8. page title shows "Inbox — N new" when unread items exist,
//  9. items grouped under Today / Yesterday / This Week / Last Week /
//     Last Month / All Others (empty groups hidden; weeks start Monday),
// 10. nav badge shows unread only and hides at zero (already true from
//     S-201 — verified, see AppShell inboxBadge).

import { useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { ConfirmDialog } from "../../components/ConfirmDialog";
import { EmptyState } from "../../components/EmptyState";
import { IconEnvelopeRead, IconEnvelopeUnread } from "../../components/icons";
import { apiDelete, apiGet, type ApiUser, type InboxMessage } from "../../lib/api";
import { useAuth } from "../../lib/auth";
import { useAttention } from "../../lib/useAttention";
import { formatRelativeTime } from "../../lib/format";
import {
  groupInboxByRecency,
  inboxPageTitle,
  inboxPreview,
  selectedUnreadIds,
  toggleItem,
  toggleSelectAll,
} from "../../lib/inboxGroups";
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

function errMessage(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback;
}

// S-189/S3776: defensive list coercion helper (shared by the fetch
// callbacks) so the page component stays under the complexity limit.
function asArray<T>(value: unknown): T[] {
  return Array.isArray(value) ? value : [];
}

// S-189/S3776: pure helpers extracted from InboxPage.
function effectiveUserIdFor(filter: UserFilter): string | undefined {
  return filter.mode === "user" ? filter.userId : undefined;
}

function allVisibleInSelection(visibleIds: readonly string[], selectedIds: ReadonlySet<string>): boolean {
  return visibleIds.length > 0 && visibleIds.every((id) => selectedIds.has(id));
}

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
  // S-207: multi-select set of message ids (list view only).
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const [pendingBulkDelete, setPendingBulkDelete] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const effectiveUserId = effectiveUserIdFor(filter);

  const load = useCallback(async () => {
    if (!authed) return;
    try {
      const query = buildScopedListQuery({ unread: unreadOnly, userId: effectiveUserId, limit: 200 });
      const next = await apiGet<InboxMessage[]>(`/inbox${query}`, token);
      setMessages(asArray<InboxMessage>(next));
      setError("");
    } catch (err) {
      setError(errMessage(err, "inbox fetch failed"));
    } finally {
      setLoading(false);
    }
  }, [authed, token, unreadOnly, effectiveUserId]);

  useEffect(() => {
    load().catch(() => undefined);
  }, [load]);

  // The admin filter needs the user directory once.
  useEffect(() => {
    if (!isAdmin || !authed) return;
    apiGet<ApiUser[]>("/users", token)
      .then((list) => setUsers(asArray<ApiUser>(list)))
      .catch(() => setUsers([]));
  }, [isAdmin, authed, token]);

  const unread = useMemo(() => unreadCount(messages), [messages]);
  const selected = useMemo(
    () => (selectedId === null ? null : (messages.find((m) => m.id === selectedId) ?? null)),
    [messages, selectedId],
  );

  // S-207: group the visible messages by recency (empty groups omitted).
  const sections = useMemo(() => groupInboxByRecency(messages, new Date()), [messages]);
  const visibleIds = useMemo(() => messages.map((m) => m.id), [messages]);
  const selectionSize = selectedIds.size;
  const allVisibleSelected = allVisibleInSelection(visibleIds, selectedIds);

  // S-207: any filter change resets the selection so bulk actions can
  // never touch rows the user can no longer see.
  const changeUnreadOnly = useCallback((value: boolean) => {
    setUnreadOnly(value);
    setSelectedIds(new Set());
  }, []);
  const changeUserFilter = useCallback((value: UserFilter) => {
    setFilter(value);
    setSelectedIds(new Set());
    setSelectedId(null);
  }, []);

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
      markRead(message).catch(() => undefined);
    },
    [markRead],
  );

  const doDelete = useCallback(async () => {
    if (!pendingDelete) return;
    try {
      await apiDelete(`/inbox/${pendingDelete.id}`, token);
      setMessages((prev) => prev.filter((m) => m.id !== pendingDelete.id));
      setSelectedIds((prev) => {
        const next = new Set(prev);
        next.delete(pendingDelete.id);
        return next;
      });
      if (selectedId === pendingDelete.id) setSelectedId(null);
    } catch (err) {
      setError(errMessage(err, "delete failed"));
    } finally {
      setPendingDelete(null);
    }
  }, [pendingDelete, token, selectedId]);

  // S-207 bulk: mark every selected unread message read. The provider's
  // markRead keeps the nav badge in sync per message; there is no bulk
  // endpoint server-side, so we loop the single-message route.
  const doBulkMarkRead = useCallback(async () => {
    const ids = selectedUnreadIds(messages, selectedIds);
    if (ids.length === 0) {
      setSelectedIds(new Set());
      return;
    }
    const idSet = new Set(ids);
    setMessages((prev) =>
      prev.map((m) => (idSet.has(m.id) ? { ...m, read_at: new Date().toISOString() } : m)),
    );
    for (const id of ids) {
      try {
        await markAttentionRead(id);
      } catch {
        // Transient: a manual refresh re-syncs; keep going with the rest.
      }
    }
    setSelectedIds(new Set());
  }, [messages, selectedIds, markAttentionRead]);

  // S-207 bulk: delete the selected messages (confirm-gated).
  const doBulkDelete = useCallback(async () => {
    const ids = [...selectedIds];
    try {
      for (const id of ids) {
        await apiDelete(`/inbox/${id}`, token);
      }
      setMessages((prev) => prev.filter((m) => !selectedIds.has(m.id)));
      if (selectedId !== null && selectedIds.has(selectedId)) setSelectedId(null);
      setError("");
    } catch (err) {
      setError(errMessage(err, "bulk delete failed"));
    } finally {
      setSelectedIds(new Set());
      setPendingBulkDelete(false);
    }
  }, [selectedIds, token, selectedId]);

  // S-189/S2004: hoisted row-toggle so the list JSX stays under the
  // function-nesting limit.
  const toggleSelected = useCallback((id: string) => {
    setSelectedIds((prev) => toggleItem(prev, id));
  }, []);

  function renderList() {
    if (loading && messages.length === 0) {
      return <EmptyState title="Loading your inbox…" hint="Agent and system messages addressed to you." />;
    }
    if (messages.length === 0) {
      return (
        <EmptyState
          title="Your inbox is empty"
          hint="Ask an agent to send something to your inbox and it will land here — unread until you open it."
        />
      );
    }
    return (
      <div className="inbox-groups">
        {/* S-207 req 7: column header above the list. */}
        <div className="inbox-columns">
          <input
            type="checkbox"
            className="inbox-checkbox"
            checked={allVisibleSelected}
            aria-label="Select all visible messages"
            onChange={() => setSelectedIds((prev) => toggleSelectAll(prev, visibleIds))}
          />
          <span className="inbox-status" aria-hidden="true" />
          <span className="inbox-col-sender">From</span>
          <span className="inbox-col-title">Message</span>
          <span className="inbox-col-kind">Type</span>
          <span className="inbox-col-time">Received</span>
        </div>
        {/* S-207 req 9: recency groups, empty ones hidden. */}
        {sections.map((section) => (
          <section key={section.group} className="inbox-group" aria-label={section.label}>
            <h2 className="inbox-group-header">{section.label}</h2>
            <div className="entity-list inbox-list">
              {section.items.map((m) => (
                <InboxRow
                  key={m.id}
                  message={m}
                  agentName={agentName}
                  checked={selectedIds.has(m.id)}
                  onOpen={openMessage}
                  onToggleSelect={toggleSelected}
                />
              ))}
            </div>
          </section>
        ))}
      </div>
    );
  }

  return (
    <AuthGate>
      <AppShell>
        <div className="inbox-header">
          {/* S-207 req 8: "Inbox — N new" when unread, plain otherwise. */}
          <h1 className="page-title">{inboxPageTitle(unread)}</h1>
          {!selected ? (
            <div className="inbox-controls">
              {/* S-207 req 5: bulk actions, enabled only with a selection. */}
              <fieldset className="inbox-bulk" style={{ border: "none", padding: 0, margin: 0, minWidth: 0 }} aria-label="Bulk actions">
                <button
                  type="button"
                  className="btn btn-small inbox-bulk-read"
                  disabled={selectionSize === 0}
                  aria-label={`Mark ${selectionSize} selected as read`}
                  onClick={() => { doBulkMarkRead().catch(() => undefined); }}
                >
                  Mark as Read
                </button>
                <button
                  type="button"
                  className="btn btn-small btn-danger inbox-bulk-delete"
                  disabled={selectionSize === 0}
                  aria-label={`Delete ${selectionSize} selected messages`}
                  onClick={() => setPendingBulkDelete(true)}
                >
                  Delete
                </button>
                {selectionSize > 0 ? (
                  <span className="inbox-selection-count">{selectionSize} selected</span>
                ) : null}
              </fieldset>
              {/* S-207 req 1: restyled to the platform field standard. */}
              <label className="inbox-filter">
                <span>Show</span>
                <select
                  className="form-control"
                  value={unreadOnly ? "unread" : "all"}
                  onChange={(e) => changeUnreadOnly(e.target.value === "unread")}
                >
                  <option value="all">All messages</option>
                  <option value="unread">Unread only</option>
                </select>
              </label>
              {isAdmin ? (
                <label className="inbox-filter">
                  <span>User</span>
                  <select
                    className="form-control"
                    value={filter.mode === "user" ? filter.userId : "own"}
                    onChange={(e) => {
                      const v = e.target.value;
                      changeUserFilter(v === "own" ? { mode: "own" } : { mode: "user", userId: v });
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
          <InboxDetail
            message={selected}
            agentName={agentName}
            onBack={() => setSelectedId(null)}
            onDelete={(m) => setPendingDelete(m)}
          />
        ) : (
          renderList()
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
        {pendingBulkDelete ? (
          <ConfirmDialog
            title="Delete selected inbox messages"
            body={`Inbox messages are never removed automatically — deleting these ${selectionSize} messages is permanent. Continue?`}
            confirmLabel="Delete"
            onConfirm={doBulkDelete}
            onClose={() => setPendingBulkDelete(false)}
          />
        ) : null}
      </AppShell>
    </AuthGate>
  );
}

function InboxRow({
  message,
  agentName,
  checked,
  onOpen,
  onToggleSelect,
}: {
  readonly message: InboxMessage;
  readonly agentName: (id?: string) => string | undefined;
  readonly checked: boolean;
  readonly onOpen: (m: InboxMessage) => void;
  readonly onToggleSelect: (id: string) => void;
}) {
    const unreadRow = isUnread(message.read_at);
    const kind = inboxKindMeta(message.kind);
    const { subject, body } = inboxDisplay(message);
      return (
      <div
        className={`inbox-row ${unreadRow ? "inbox-row-unread" : ""}`}
        role="button"
        tabIndex={0}
        aria-label={`Open message: ${subject}`}
        onClick={() => onOpen(message)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onOpen(message);
          }
        }}
      >
        {/* S-207 req 4: multi-select checkbox (never opens the row). */}
        <input
          type="checkbox"
          className="inbox-checkbox"
          checked={checked}
          aria-label={`Select message: ${subject}`}
          onClick={(e) => e.stopPropagation()}
          onChange={() => onToggleSelect(message.id)}
        />
        {/* S-207 req 3: envelope read/unread marker. */}
        <span className="inbox-status" aria-hidden={unreadRow ? undefined : "true"}>
          {unreadRow ? (
            <IconEnvelopeUnread size={18} />
          ) : (
            <IconEnvelopeRead size={18} />
          )}
        </span>
        <span className="inbox-sender">{inboxSender(message, agentName)}</span>
        {/* S-207 req 6: "Title — body preview", ellipsis-truncated. */}
        <span className="inbox-titleline">
          <span className="inbox-subject">{subject}</span>
          {inboxPreview(body) ? (
            <span className="inbox-preview">{" — " + inboxPreview(body)}</span>
          ) : null}
        </span>
        <span className={`chip ${kind.className}`}>{kind.label}</span>
        <span className="inbox-time">{formatRelativeTime(message.created_at)}</span>
      </div>
    );
  }

function InboxDetail({
  message,
  agentName,
  onBack,
  onDelete,
}: {
  readonly message: InboxMessage;
  readonly agentName: (id?: string) => string | undefined;
  readonly onBack: () => void;
  readonly onDelete: (m: InboxMessage) => void;
}) {
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
            onClick={onBack}
          >
            ← Inbox
          </button>
          <button
            type="button"
            className="btn btn-small btn-danger inbox-delete"
            aria-label="Delete message"
            onClick={() => onDelete(message)}
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
  }
