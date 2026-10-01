// S-193: pure helpers for the notification bell and the email-like
// inbox. Browser-independent so the presentation logic is unit-testable.

import type { AppNotification, InboxMessage, NotificationSeverity, NotificationType } from "./api";

export const NOTIFICATION_TYPE_META: Record<NotificationType, { label: string; className: string }> = {
  task_failed: { label: "task failed", className: "chip chip-error" },
  task_stuck: { label: "task stuck", className: "chip chip-stalled" },
  agent_died: { label: "agent died", className: "chip chip-error" },
  task_blocked: { label: "needs you", className: "chip chip-blocked" },
};

export const SEVERITY_CLASS: Record<NotificationSeverity, string> = {
  info: "notif-sev-info",
  warning: "notif-sev-warning",
  error: "notif-sev-error",
};

export function notificationMeta(notification: AppNotification): { label: string; className: string } {
  return NOTIFICATION_TYPE_META[notification.type] ?? { label: notification.type, className: "chip" };
}

export function notificationSeverityClass(severity: NotificationSeverity | undefined): string {
  return SEVERITY_CLASS[severity ?? "warning"] ?? SEVERITY_CLASS.warning;
}

// notificationLink resolves the internal navigation target for a
// notification: blocked/failed work is about a task, so link there.
export function notificationLink(notification: AppNotification): string | null {
  if (notification.task_id && notification.squad_id) {
    return `/squads/${notification.squad_id}/tasks/${notification.task_id}`;
  }
  return null;
}

export function isUnread(readAt?: string): boolean {
  return !readAt;
}

export function unreadCount(items: readonly { read_at?: string }[]): number {
  return items.reduce((acc, item) => acc + (isUnread(item.read_at) ? 1 : 0), 0);
}

// buildInboxQuery assembles the /inbox (or /notifications) query string
// from the screen's filter controls. Empty fields are dropped so the API
// never sees `?user_id=` noise. `unread` is only emitted when true.
export function buildScopedListQuery(opts: { unread?: boolean; userId?: string; limit?: number }): string {
  const params = new URLSearchParams();
  if (opts.unread) params.set("unread", "true");
  const userId = (opts.userId ?? "").trim();
  if (userId !== "") params.set("user_id", userId);
  if (opts.limit && opts.limit > 0) params.set("limit", String(opts.limit));
  const qs = params.toString();
  return qs === "" ? "" : `?${qs}`;
}

// inboxDisplay resolves the email-like presentation for an inbox message:
// subject line (falling back to the one-line message) and body (falling
// back to the message for older rows without the S-181 payload).
export function inboxDisplay(message: InboxMessage): { subject: string; body: string } {
  const subject = (message.subject ?? "").trim() || (message.message ?? "").trim();
  const body = (message.body ?? "").trim() || (message.message ?? "").trim();
  return { subject, body };
}

export const INBOX_KIND_META: Record<string, { label: string; className: string }> = {
  task_completed: { label: "completed", className: "chip chip-done" },
  action_required: { label: "needs you", className: "chip chip-blocked" },
  agent_message: { label: "from agent", className: "chip chip-info" },
};

export function inboxKindMeta(kind: string): { label: string; className: string } {
  return INBOX_KIND_META[kind] ?? { label: kind, className: "chip" };
}

// inboxTaskLink resolves the internal link for an inbox message row.
export function inboxTaskLink(message: InboxMessage): string | null {
  if (message.task_id && message.squad_id) {
    return `/squads/${message.squad_id}/tasks/${message.task_id}`;
  }
  return null;
}

// inboxSender (S-201) resolves the "from" column of the Gmail-style
// inbox: the sending agent's display name when known, a short-id
// fallback for agents outside the loaded directory, and "System" for
// rows with no agent origin.
export function inboxSender(
  message: InboxMessage,
  agentName: (id?: string) => string | undefined,
): string {
  if (!message.from_agent_id) return "System";
  return agentName(message.from_agent_id) ?? `agent ${message.from_agent_id.slice(0, 8)}`;
}
