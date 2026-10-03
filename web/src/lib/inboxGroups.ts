// S-207: recency grouping, page-title and selection helpers for the
// Inbox page. Pure and browser-independent (all date math is done in
// the machine's local timezone via Date's local methods, matching the
// app's existing date handling) so the bucketing and bulk-selection
// logic is unit-testable in the node test environment.

import type { InboxMessage } from "./api";
import { isUnread } from "./notifications";

export type RecencyGroup =
  | "today"
  | "yesterday"
  | "this-week"
  | "last-week"
  | "last-month"
  | "all-others";

export const RECENCY_GROUP_LABELS: Record<RecencyGroup, string> = {
  today: "Today",
  yesterday: "Yesterday",
  "this-week": "This Week",
  "last-week": "Last Week",
  "last-month": "Last Month",
  "all-others": "All Others",
};

const GROUP_ORDER: readonly RecencyGroup[] = [
  "today",
  "yesterday",
  "this-week",
  "last-week",
  "last-month",
  "all-others",
];

// Local midnight for the calendar day the date falls on.
function dayStart(d: Date): number {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
}

// Week start under the ISO convention: weeks begin on Monday.
// (getDay() is 0=Sunday, so Monday maps to offset 0.)
function weekStart(d: Date): number {
  const shift = (d.getDay() + 6) % 7;
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() - shift).getTime();
}

// recencyGroup buckets a message's created_at into one of the six
// inbox groups relative to `now` (local time). Day arithmetic is done
// on calendar days (not fixed millisecond deltas) so DST transitions
// cannot shift a message into the wrong bucket.
//
//   Today      = same calendar day
//   Yesterday  = previous calendar day
//   This Week  = current Monday..today (exclusive of the two above)
//   Last Week  = previous Monday..Sunday
//   Last Month = previous *calendar* month
//   All Others= everything older (or unparseable timestamps)
export function recencyGroup(createdAt: string, now: Date): RecencyGroup {
  const at = new Date(createdAt);
  if (Number.isNaN(at.getTime())) return "all-others";

  const today = dayStart(now);
  const target = dayStart(at);
  if (target === today) return "today";
  if (target === new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1).getTime()) {
    return "yesterday";
  }

  const thisWeek = weekStart(now);
  const lastWeek = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 7 - ((now.getDay() + 6) % 7)).getTime();
  if (target >= thisWeek) return "this-week";
  if (target >= lastWeek) return "last-week";

  const thisMonthStart = new Date(now.getFullYear(), now.getMonth(), 1).getTime();
  const prevMonthStart = new Date(now.getFullYear(), now.getMonth() - 1, 1).getTime();
  if (target >= prevMonthStart && target < thisMonthStart) return "last-month";

  return "all-others";
}

export type InboxGroupSection = {
  group: RecencyGroup;
  label: string;
  items: InboxMessage[];
};

// groupInboxByRecency buckets messages into ordered sections. Empty
// groups are omitted entirely (requirement: only show non-empty group
// headers). Within a group the incoming order is preserved — the API
// returns newest-first, so each group reads newest-first too.
export function groupInboxByRecency(
  messages: readonly InboxMessage[],
  now: Date,
): InboxGroupSection[] {
  const buckets = new Map<RecencyGroup, InboxMessage[]>();
  for (const message of messages) {
    const group = recencyGroup(message.created_at, now);
    const bucket = buckets.get(group);
    if (bucket) bucket.push(message);
    else buckets.set(group, [message]);
  }
  const sections: InboxGroupSection[] = [];
  for (const group of GROUP_ORDER) {
    const items = buckets.get(group);
    if (items && items.length > 0) {
      sections.push({ group, label: RECENCY_GROUP_LABELS[group], items });
    }
  }
  return sections;
}

// inboxPageTitle renders the S-207 page title: "Inbox — N new" when
// there are unread messages, plain "Inbox" otherwise.
export function inboxPageTitle(unread: number): string {
  return unread > 0 ? `Inbox — ${unread} new` : "Inbox";
}

// toggleItem flips one message in/out of the selection.
export function toggleItem(selected: ReadonlySet<string>, id: string): Set<string> {
  const next = new Set(selected);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  return next;
}

// toggleSelectAll implements the column-header checkbox: when every
// visible item is already selected it clears the selection, otherwise
// it selects all visible items (visible-only, per the card spec).
export function toggleSelectAll(
  selected: ReadonlySet<string>,
  visibleIds: readonly string[],
): Set<string> {
  const allSelected =
    visibleIds.length > 0 && visibleIds.every((id) => selected.has(id));
  return allSelected ? new Set<string>() : new Set<string>(visibleIds);
}

// selectedUnreadIds returns the selected messages that are still
// unread — the "Mark as Read" bulk action only needs to touch those.
export function selectedUnreadIds(
  messages: readonly InboxMessage[],
  selected: ReadonlySet<string>,
): string[] {
  return messages
    .filter((m) => selected.has(m.id) && isUnread(m.read_at))
    .map((m) => m.id);
}

// inboxPreview flattens the body into a single-line snippet and caps it
// so the "Title — snippet" row stays on one line (CSS ellipsis handles
// the final viewport truncation; this keeps the DOM small).
export function inboxPreview(body: string, maxChars = 160): string {
  const flat = body.replace(/\s+/g, " ").trim();
  if (flat.length <= maxChars) return flat;
  return `${flat.slice(0, maxChars).trimEnd()}…`;
}
