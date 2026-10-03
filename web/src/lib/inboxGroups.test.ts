// S-207: recency grouping, page-title and selection helper tests.
//
// All fixtures use an explicit `now` so bucketing is deterministic.
// The reference "now" is Wed 2026-10-07 (local), so:
//   this week (Mon start) = Mon 05 .. Sun 11
//   last week             = Mon 28 Sep .. Sun 04 Oct
//   last month            = September

import { describe, expect, it } from "vitest";

import type { InboxMessage } from "./api";
import {
  groupInboxByRecency,
  inboxPageTitle,
  inboxPreview,
  recencyGroup,
  selectedUnreadIds,
  toggleItem,
  toggleSelectAll,
} from "./inboxGroups";

// Build a Date at local noon (by default) to dodge any midnight/DST edge in fixtures.
const at = (y: number, mo: number, d: number, h = 12, min = 0) =>
  new Date(y, mo - 1, d, h, min, 0);
const NOW = at(2026, 10, 7); // Wednesday
const iso = (d: Date) => d.toISOString();

const msg = (id: string, created: Date, readAt?: string): InboxMessage =>
  ({
    id,
    user_id: "u1",
    kind: "agent_message",
    subject: `subject ${id}`,
    body: `body ${id}`,
    message: `message ${id}`,
    created_at: iso(created),
    ...(readAt ? { read_at: readAt } : {}),
  }) as InboxMessage;

describe("recencyGroup", () => {
  it("buckets same calendar day as today", () => {
    expect(recencyGroup(iso(at(2026, 10, 7, 0, 5)), NOW)).toBe("today");
    expect(recencyGroup(iso(at(2026, 10, 7, 23, 55)), NOW)).toBe("today");
  });

  it("buckets previous calendar day as yesterday", () => {
    expect(recencyGroup(iso(at(2026, 10, 6, 18, 0)), NOW)).toBe("yesterday");
  });

  it("buckets Monday..today (minus today/yesterday) as this-week", () => {
    // Monday of the current week (two days before Wed) is this-week, not yesterday.
    expect(recencyGroup(iso(at(2026, 10, 5, 9, 0)), NOW)).toBe("this-week");
    expect(recencyGroup(iso(at(2026, 10, 6, 23, 0)), NOW)).toBe("yesterday");
  });

  it("buckets previous Mon..Sun as last-week", () => {
    expect(recencyGroup(iso(at(2026, 9, 28, 12, 0)), NOW)).toBe("last-week"); // Mon
    expect(recencyGroup(iso(at(2026, 10, 4, 12, 0)), NOW)).toBe("last-week"); // Sun
  });

  it("buckets the previous calendar month as last-month", () => {
    expect(recencyGroup(iso(at(2026, 9, 1, 0, 1)), NOW)).toBe("last-month");
    expect(recencyGroup(iso(at(2026, 9, 27, 23, 59)), NOW)).toBe("last-month");
  });

  it("buckets anything older as all-others", () => {
    expect(recencyGroup(iso(at(2026, 8, 31, 12, 0)), NOW)).toBe("all-others");
    expect(recencyGroup(iso(at(2025, 1, 1, 12, 0)), NOW)).toBe("all-others");
  });

  it("buckets unparseable timestamps as all-others", () => {
    expect(recencyGroup("not-a-date", NOW)).toBe("all-others");
    expect(recencyGroup("", NOW)).toBe("all-others");
  });

  it("handles a Sunday 'now' (week starts Monday, so the week spans back to Monday)", () => {
    const sunday = at(2026, 10, 4, 12, 0);
    expect(recencyGroup(iso(at(2026, 10, 4, 8, 0)), sunday)).toBe("today");
    // Mon 28 Sep is the START of the current week when 'now' is Sun 4 Oct.
    expect(recencyGroup(iso(at(2026, 9, 28, 12, 0)), sunday)).toBe("this-week");
    // Last week is Mon 21 .. Sun 27 Sep.
    expect(recencyGroup(iso(at(2026, 9, 21, 12, 0)), sunday)).toBe("last-week");
    expect(recencyGroup(iso(at(2026, 9, 27, 12, 0)), sunday)).toBe("last-week");
  });

  it("handles month/year boundaries — week buckets take precedence over month", () => {
    const nowJan2 = at(2026, 1, 2, 12, 0); // Friday
    expect(recencyGroup(iso(at(2026, 1, 1, 12, 0)), nowJan2)).toBe("yesterday");
    // This week = Mon 29 Dec 2025 .. Sun 4 Jan 2026, so late December reads
    // as this-week/last-week, not last-month.
    expect(recencyGroup(iso(at(2025, 12, 31, 12, 0)), nowJan2)).toBe("this-week");
    expect(recencyGroup(iso(at(2025, 12, 29, 12, 0)), nowJan2)).toBe("this-week");
    expect(recencyGroup(iso(at(2025, 12, 22, 12, 0)), nowJan2)).toBe("last-week");
    // Earlier December (before last week) is last-month.
    expect(recencyGroup(iso(at(2025, 12, 20, 12, 0)), nowJan2)).toBe("last-month");
    expect(recencyGroup(iso(at(2025, 12, 1, 12, 0)), nowJan2)).toBe("last-month");
  });
});

describe("groupInboxByRecency", () => {
  it("omits empty groups and keeps canonical order", () => {
    const sections = groupInboxByRecency(
      [
        msg("old", at(2026, 6, 1)),
        msg("today", at(2026, 10, 7)),
        msg("lastmonth", at(2026, 9, 15)),
        msg("yest", at(2026, 10, 6)),
      ],
      NOW,
    );
    expect(sections.map((s) => s.group)).toEqual(["today", "yesterday", "last-month", "all-others"]);
    expect(sections[0].label).toBe("Today");
  });

  it("preserves newest-first order within a group", () => {
    const sections = groupInboxByRecency(
      [
        msg("a", at(2026, 10, 7, 18, 0)),
        msg("b", at(2026, 10, 7, 9, 0)),
      ],
      NOW,
    );
    expect(sections).toHaveLength(1);
    expect(sections[0].items.map((m) => m.id)).toEqual(["a", "b"]);
  });

  it("returns no sections for an empty list", () => {
    expect(groupInboxByRecency([], NOW)).toEqual([]);
  });
});

describe("inboxPageTitle", () => {
  it("shows the new count when unread items exist", () => {
    expect(inboxPageTitle(3)).toBe("Inbox — 3 new");
  });

  it("is plain when nothing is unread", () => {
    expect(inboxPageTitle(0)).toBe("Inbox");
  });
});

describe("toggleItem", () => {
  it("adds and removes without mutating the input set", () => {
    const start = new Set<string>(["a"]);
    const added = toggleItem(start, "b");
    expect([...added].sort()).toEqual(["a", "b"]);
    expect([...start]).toEqual(["a"]);
    const removed = toggleItem(added, "a");
    expect([...removed]).toEqual(["b"]);
  });
});

describe("toggleSelectAll", () => {
  it("selects all visible ids when not everything is selected", () => {
    const next = toggleSelectAll(new Set(["a"]), ["a", "b", "c"]);
    expect([...next].sort()).toEqual(["a", "b", "c"]);
  });

  it("clears when every visible id is already selected", () => {
    const next = toggleSelectAll(new Set(["a", "b", "c", "z"]), ["a", "b", "c"]);
    expect(next.size).toBe(0);
  });

  it("stays empty when there are no visible ids", () => {
    expect(toggleSelectAll(new Set(["a"]), []).size).toBe(0);
  });
});

describe("selectedUnreadIds", () => {
  it("returns only selected messages that are still unread", () => {
    const messages = [
      msg("u1", at(2026, 10, 7)),
      msg("r1", at(2026, 10, 7), iso(at(2026, 10, 7, 13, 0))),
      msg("u2", at(2026, 10, 6)),
    ];
    expect(selectedUnreadIds(messages, new Set(["u1", "r1", "u2"]))).toEqual(["u1", "u2"]);
    expect(selectedUnreadIds(messages, new Set(["r1"]))).toEqual([]);
  });
});

describe("inboxPreview", () => {
  it("flattens whitespace into a single line", () => {
    expect(inboxPreview("line one\n\n  line two\ttabbed")).toBe("line one line two tabbed");
  });

  it("caps long bodies with an ellipsis", () => {
    const long = "x".repeat(400);
    const preview = inboxPreview(long, 160);
    expect(preview.length).toBe(161); // 160 chars + ellipsis
    expect(preview.endsWith("…")).toBe(true);
  });

  it("handles empty bodies", () => {
    expect(inboxPreview("   \n  ")).toBe("");
  });
});
