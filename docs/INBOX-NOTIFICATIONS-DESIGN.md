# S-193 Epic: Inbox & Notifications — Design

## Separation of concerns

Two distinct human-facing surfaces, deliberately decoupled:

| | Inbox | Notifications |
|---|---|---|
| What | Email-like messages from agents (and system) to humans | Transient alerts when things go wrong |
| Table | `inbox_messages` (exists since 0006, enriched 0025) | `notifications` (new, 0029) |
| Lifecycle | Never auto-removed; explicit user delete only | Read/unread; no delete requirement |
| UI | `/inbox` page (rebuilt as email list) | Bell in TopBar, left of theme toggle |

An inbox message does NOT create a notification, and vice-versa. A blocked
task gets both today: an `action_required` inbox message (existing) and a
`task_blocked` notification (new).

## Data model (migration 0029_inbox_notifications.sql)

1. `inbox_messages.kind` CHECK widened: `('task_completed','action_required','agent_message')`.
   `agent_message` = content the human explicitly asked an agent to put in
   their inbox via the `send_inbox` tool.
2. `notifications`:
   - `id uuid PK`, `user_id` (recipient, ON DELETE CASCADE), `squad_id` (CASCADE),
     `task_id` (SET NULL), `agent_id` (SET NULL)
   - `type` CHECK IN (`task_failed`,`task_stuck`,`agent_died`,`task_blocked`)
   - `severity` CHECK IN (`info`,`warning`,`error`), default `warning`
   - `message` text NOT NULL (self-contained, includes `T-<n>` ref)
   - `read_at` nullable, `created_at`
   - Indexes: `(user_id, created_at)`; partial `(user_id) WHERE read_at IS NULL`
3. `builtin_tools_config` CHECK widened to admit `send_inbox`; seeded ENABLED
   (same pattern as `send_message` in 0023 — bounded blast radius: writes
   only to the sender's own squad owner's inbox).

`task_stuck` is reserved in the enum now so a later stuck-scanner needs no
migration (see follow-up card).

## Control plane

### Storage (InboxStore extension + NotificationStore)

- `GetInboxMessage(ctx, id)` — for delete authorization checks.
- `DeleteInboxMessage(ctx, id, userID)` — userID="" means unrestricted
  (platform-admin path only; handler enforces).
- `CreateNotification`, `ListNotifications(userID, unreadOnly, limit)`,
  `MarkNotificationRead(userID, id)`, `MarkAllNotificationsRead(userID)`.

### HTTP (user-auth)

- `GET /api/v1/inbox` — unchanged default (own inbox). New `user_id` query
  param: present ⇒ platform_admin required (403 otherwise), lists that
  user's inbox. Response stays a bare array (backward compatible).
- `DELETE /api/v1/inbox/{id}` — recipient or platform_admin. 404 otherwise.
- `GET /api/v1/notifications?unread=&limit=&user_id=` — self; `user_id`
  admin-gated like inbox.
- `POST /api/v1/notifications/{id}/read` — recipient only.
- `POST /api/v1/notifications/read-all` — recipient.

### HTTP (agent-cred, /agents/me)

- `POST /api/v1/agents/me/inbox` — the `send_inbox` tool's endpoint.
  Body: `{message, subject?, task_id?}`. Resolves the squad owner
  (`squad.OwnerID`) — this is the agent→human addressing: agents never
  name a user; the control plane routes to the owning human of the
  agent's squad. `task_id` optional, validated to be in the agent's squad,
  so the UI can render an internal navigation link. Kind = `agent_message`.

### Notification emission (best-effort; never fails the underlying flow)

- **task_blocked** (warning): `blockCurrentAgentTask` and
  `notifyDelegationBlocked` — "Task T-<n> requires attention: <note>".
- **agent_died** (error): the execution reaper. `ReapExpiredTaskExecutions`
  now returns the reaped executions (`[]*domain.ReapedExecution` — task +
  agent ids) so the reaper can file one notification per dead attempt:
  "Agent X's lease expired mid-task T-<n>; the task was re-queued."
- **task_failed** (error): heartbeat status transition into `error` while
  the agent has an active execution — "Task T-<n> failed: agent X
  reported an error." Deduped on the status *transition* (previous
  status != error), so a sticky-error agent doesn't spam every 30s beat.
- **task_stuck**: reserved; dedicated stuck-scanner deferred to a
  follow-up card (needs a policy: in-progress with no thread activity for
  N hours).

## Agent runtime

`SendInboxTool` in `builtin_tools.py` (same shape as `SendMessageTool`):
schema `{message: string (required), subject?: string, task_id?: string}`,
POSTs to `/agents/me/inbox`, registered in `_BUILTIN_REGISTRY` as
`send_inbox`. Prompt guidance stays minimal — the tool learns itself from
the schema description ("Use when the human asked you to send something to
their inbox or notify them").

## Web

- `lib/api.ts`: `InboxMessage.kind` gains `"agent_message"`; new
  `Notification` type + `apiInbox(unreadOnly, userId?)`,
  `apiDeleteInboxMessage`, `apiNotifications`, `apiMarkNotificationRead`,
  `apiMarkAllNotificationsRead`.
- `lib/notifications.ts`: pure helpers (severity→label/class,
  notification text → link target resolution) — unit-testable.
- `components/NotificationBell.tsx`: bell icon + unread-count badge in
  `TopBar` immediately left of `<ThemeToggle/>`; click opens a dropdown
  panel (reuse Modal-ish popover styling) listing notifications with
  severity chips, task links, per-item mark-read, and "Mark all read".
  Polls every 30s while open-or-closed (cheap, matches AttentionProvider).
- `/inbox` rebuilt as the email-like list: unread rows bold with a dot +
  unread badge in the header, filter control (Unread / All), admin-only
  user `<select>` (from `GET /users`) defaulting to own inbox, click a
  row → marks read + expands body (subject line, sender agent, squad,
  relative time, internal link to the task when `task_id` present),
  per-row Delete with ConfirmDialog. The old attention-merge queue is
  removed from this page (the bell now carries the "things going wrong"
  role); `attention.ts`/`AttentionProvider` stay for the nav badge.

## Deferred (follow-up cards)

1. `task_stuck` detector (stuck-scanner policy).
2. Notification retention/prune policy (e.g. auto-purge read > 90d).
3. Per-user notification preferences (mute types).
