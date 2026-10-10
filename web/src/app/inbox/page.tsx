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
import { ConfirmationDecisionCard } from "../../components/ConfirmationDecisionCard";
import { EmptyState } from "../../components/EmptyState";
import { IconEnvelopeRead, IconEnvelopeUnread } from "../../components/icons";
import { InboxAttachments } from "../../components/InboxAttachments";
import {
  apiDelete,
  apiGet,
  apiGetWithTotal,
  type ApiUser,
  type InboxMessage,
} from "../../lib/api";
import { DEFAULT_PAGE_SIZE, Pager, pageCount } from "../../components/Pager";
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

function allVisibleInSelection(
  visibleIds: readonly string[],
  selectedIds: ReadonlySet<string>,
): boolean {
  return visibleIds.length > 0 && visibleIds.every((id) => selectedIds.has(id));
}

// inboxEmptyState returns the loading/empty placeholder for the inbox list, or
// null when there are messages. Extracted from InboxPage to keep its cognitive
// complexity within limits (S-268 / S3776).
function inboxEmptyState(loading: boolean, count: number) {
  if (loading && count === 0) {
    return (
      <EmptyState
        title="Loading your inbox…"
        hint="Agent and system messages addressed to you."
      />
    );
  }
  if (count === 0) {
    return (
      <EmptyState
        title="Your inbox is empty"
        hint="Ask an agent to send something to your inbox and it will land here — unread until you open it."
      />
    );
  }
  return null;
}

type InboxControlsProps = {
  readonly selectionSize: number;
  readonly unreadOnly: boolean;
  readonly isAdmin: boolean;
  readonly filter: UserFilter;
  readonly users: ApiUser[];
  readonly currentUserId?: string;
  readonly onBulkMarkRead: () => Promise<unknown>;
  readonly onBulkDelete: () => void;
  readonly onChangeUnread: (v: boolean) => void;
  readonly onChangeUserFilter: (v: UserFilter) => void;
};

// InboxControls renders the bulk-action buttons and the read/user filters shown
// above the inbox list. Extracted from InboxPage to keep its cognitive
// complexity within limits (S-268 / S3776).
function InboxControls(props: InboxControlsProps) {
  const {
    selectionSize,
    unreadOnly,
    isAdmin,
    filter,
    users,
    currentUserId,
    onBulkMarkRead,
    onBulkDelete,
    onChangeUnread,
    onChangeUserFilter,
  } = props;
  return (
    <div className="inbox-controls">
      {/* S-207 req 5: bulk actions, enabled only with a selection. */}
      <fieldset
        className="inbox-bulk"
        style={{ border: "none", padding: 0, margin: 0, minWidth: 0 }}
        aria-label="Bulk actions"
      >
        <button
          type="button"
          className="btn btn-small inbox-bulk-read"
          disabled={selectionSize === 0}
          aria-label={`Mark ${selectionSize} selected as read`}
          onClick={() => {
            onBulkMarkRead().catch(() => undefined);
          }}
        >
          Mark as Read
        </button>
        <button
          type="button"
          className="btn btn-small btn-danger inbox-bulk-delete"
          disabled={selectionSize === 0}
          aria-label={`Delete ${selectionSize} selected messages`}
          onClick={onBulkDelete}
        >
          Delete
        </button>
        {selectionSize > 0 ? (
          <span className="inbox-selection-count">
            {selectionSize} selected
          </span>
        ) : null}
      </fieldset>
      {/* S-207 req 1: restyled to the platform field standard. */}
      <label className="inbox-filter">
        <span>Show</span>
        <select
          className="form-control"
          value={unreadOnly ? "unread" : "all"}
          onChange={(e) => onChangeUnread(e.target.value === "unread")}
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
              onChangeUserFilter(
                v === "own" ? { mode: "own" } : { mode: "user", userId: v },
              );
            }}
          >
            <option value="own">My inbox</option>
            {users
              .filter((u) => u.id !== currentUserId)
              .map((u) => (
                <option key={u.id} value={u.id}>
                  {u.name || u.email}
                </option>
              ))}
          </select>
        </label>
      ) : null}
    </div>
  );
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
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(
    new Set(),
  );
  const [pendingBulkDelete, setPendingBulkDelete] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  // S-239: server-side paging — 1-based page, page size (default 25)
  // and the total messages matching the current filter (X-Total-Count).
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);
  const [total, setTotal] = useState(0);

  const effectiveUserId = effectiveUserIdFor(filter);

  const load = useCallback(async () => {
    if (!authed) return;
    try {
      const query = buildScopedListQuery({
        unread: unreadOnly,
        userId: effectiveUserId,
        limit: pageSize,
        offset: (page - 1) * pageSize,
      });
      const { items, total: totalCount } = await apiGetWithTotal<
        InboxMessage[]
      >(`/inbox${query}`, token);
      setMessages(asArray<InboxMessage>(items));
      setTotal(totalCount);
      setError("");
    } catch (err) {
      setError(errMessage(err, "inbox fetch failed"));
    } finally {
      setLoading(false);
    }
  }, [authed, token, unreadOnly, effectiveUserId, page, pageSize]);

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
    () => messages.find((m) => m.id === selectedId) ?? null,
    [messages, selectedId],
  );

  // S-207: group the visible messages by recency (empty groups omitted).
  const sections = useMemo(
    () => groupInboxByRecency(messages, new Date()),
    [messages],
  );
  const visibleIds = useMemo(() => messages.map((m) => m.id), [messages]);
  const selectionSize = selectedIds.size;
  const allVisibleSelected = allVisibleInSelection(visibleIds, selectedIds);

  // S-207: any filter change resets the selection so bulk actions can
  // never touch rows the user can no longer see. S-239: filters also
  // jump back to the first page.
  const changeUnreadOnly = useCallback((value: boolean) => {
    setUnreadOnly(value);
    setSelectedIds(new Set());
    setPage(1);
  }, []);
  const changeUserFilter = useCallback((value: UserFilter) => {
    setFilter(value);
    setSelectedIds(new Set());
    setSelectedId(null);
    setPage(1);
  }, []);

  // S-239: changing the page size restarts from page 1 (and clears the
  // selection, same rule as filters).
  const changePageSize = useCallback((size: number) => {
    setPageSize(size);
    setSelectedIds(new Set());
    setPage(1);
  }, []);

  // S-239: after removing messages, shrink the known total and step
  // back a page if the current one fell off the end.
  const accountRemoval = useCallback(
    (removed: number) => {
      const nextTotal = Math.max(0, total - removed);
      setTotal(nextTotal);
      const pages = pageCount(nextTotal, pageSize);
      if (page > pages) setPage(pages);
    },
    [total, page, pageSize],
  );

  const markRead = useCallback(
    async (message: InboxMessage) => {
      if (!isUnread(message.read_at)) return;
      // Optimistic: the row (and the nav badge via the provider) flips to
      // read immediately; the provider owns the POST so one request keeps
      // both surfaces in sync.
      setMessages((prev) =>
        prev.map((m) =>
          m.id === message.id ? { ...m, read_at: new Date().toISOString() } : m,
        ),
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
      accountRemoval(1);
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
  }, [pendingDelete, token, selectedId, accountRemoval]);

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
      prev.map((m) =>
        idSet.has(m.id) ? { ...m, read_at: new Date().toISOString() } : m,
      ),
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
    let removed = 0;
    try {
      for (const id of ids) {
        await apiDelete(`/inbox/${id}`, token);
        removed += 1;
      }
      setMessages((prev) => prev.filter((m) => !selectedIds.has(m.id)));
      accountRemoval(removed);
      if (selectedId !== null && selectedIds.has(selectedId))
        setSelectedId(null);
      setError("");
    } catch (err) {
      setError(errMessage(err, "bulk delete failed"));
      if (removed > 0) accountRemoval(removed);
    } finally {
      setSelectedIds(new Set());
      setPendingBulkDelete(false);
    }
  }, [selectedIds, token, selectedId, accountRemoval]);

  // S-189/S2004: hoisted row-toggle so the list JSX stays under the
  // function-nesting limit.
  const toggleSelected = useCallback((id: string) => {
    setSelectedIds((prev) => toggleItem(prev, id));
  }, []);

  function renderList() {
    const empty = inboxEmptyState(loading, messages.length);
    if (empty) return empty;
    return (
      <div className="inbox-groups">
        {/* S-207 req 7: column header above the list. */}
        <div className="inbox-columns">
          <input
            type="checkbox"
            className="inbox-checkbox"
            checked={allVisibleSelected}
            aria-label="Select all visible messages"
            onChange={() =>
              setSelectedIds((prev) => toggleSelectAll(prev, visibleIds))
            }
          />
          <span className="inbox-status" aria-hidden="true" />
          <span className="inbox-col-sender">From</span>
          <span className="inbox-col-title">Message</span>
          <span className="inbox-col-kind">Type</span>
          <span className="inbox-col-time">Received</span>
        </div>
        {/* S-207 req 9: recency groups, empty ones hidden. */}
        {sections.map((section) => (
          <section
            key={section.group}
            className="inbox-group"
            aria-label={section.label}
          >
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
            <InboxControls
              selectionSize={selectionSize}
              unreadOnly={unreadOnly}
              isAdmin={isAdmin}
              filter={filter}
              users={users}
              currentUserId={user?.id}
              onBulkMarkRead={doBulkMarkRead}
              onBulkDelete={() => setPendingBulkDelete(true)}
              onChangeUnread={changeUnreadOnly}
              onChangeUserFilter={changeUserFilter}
            />
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
          <>
            {renderList()}
            {/* S-239: pager below the list (list view only). */}
            {total > 0 ? (
              <Pager
                page={page}
                pageSize={pageSize}
                total={total}
                onPageChange={setPage}
                onPageSizeChange={changePageSize}
              />
            ) : null}
          </>
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
    <div className={`inbox-row ${unreadRow ? "inbox-row-unread" : ""}`}>
      {/* S-207 req 4: multi-select checkbox (never opens the row). */}
      <input
        type="checkbox"
        className="inbox-checkbox"
        checked={checked}
        aria-label={`Select message: ${subject}`}
        onClick={(e) => e.stopPropagation()}
        onChange={() => onToggleSelect(message.id)}
      />
      {/* S-189: the row-open affordance is a native <button> spanning every
            column except the checkbox (was div role="button", S6819). */}
      <button
        type="button"
        className="inbox-row-open"
        aria-label={`Open message: ${subject}`}
        onClick={() => onOpen(message)}
      >
        {/* S-207 req 3: envelope read/unread marker. */}
        <span
          className="inbox-status"
          aria-hidden={unreadRow ? undefined : "true"}
        >
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
          {/* S-258: cheap paperclip so attachment-carrying messages are
              findable in the list before opening. */}
          {(message.attachments?.length ?? 0) > 0 ? (
            <span
              className="inbox-attachment-flag"
              aria-label={`${message.attachments?.length} attachment(s)`}
            >
              📎 {message.attachments?.length}
            </span>
          ) : null}
        </span>
        <span className={`chip ${kind.className}`}>{kind.label}</span>
        <span className="inbox-time">
          {formatRelativeTime(message.created_at)}
        </span>
      </button>
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
        <span className="inbox-detail-from">
          {inboxSender(message, agentName)}
        </span>
        <span className={`chip ${kind.className}`}>{kind.label}</span>
        <span className="inbox-time">
          {formatRelativeTime(message.created_at)}
        </span>
      </div>
      <div className="inbox-detail-body">
        <p className="inbox-body-text">{body}</p>
      </div>
      {/* S-258: attachment previews (images) + download list (all
            other types). Renders nothing when the message has none. */}
      <InboxAttachments attachments={message.attachments} />
      {/* TG-8 slice D: gated-call confirmations ride the action_required
            kind; the card joins the confirmation via its inbox_message_id
            and renders the owner's 3 decision buttons inline. */}
      {message.kind === "action_required" ? (
        <ConfirmationDecisionCard messageId={message.id} />
      ) : null}
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
