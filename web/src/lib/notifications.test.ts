// S-193: pure helper tests for the bell + email-like inbox.

import { describe, expect, it } from "vitest";

import type { AppNotification, InboxMessage } from "./api";
import {
  buildScopedListQuery,
  inboxDisplay,
  inboxKindMeta,
  inboxTaskLink,
  isUnread,
  notificationLink,
  notificationMeta,
  notificationSeverityClass,
  unreadCount,
} from "./notifications";

const baseNotification: AppNotification = {
  id: "n1",
  user_id: "u1",
  squad_id: "s1",
  type: "task_blocked",
  severity: "warning",
  message: "Task T-2345 requires attention.",
  created_at: "2026-10-01T01:00:00Z",
};

describe("isUnread / unreadCount", () => {
  it("treats missing read_at as unread", () => {
    expect(isUnread(undefined)).toBe(true);
    expect(isUnread("2026-10-01T02:00:00Z")).toBe(false);
  });

  it("counts unread only", () => {
    expect(
      unreadCount([
        { read_at: undefined },
        { read_at: "2026-10-01T02:00:00Z" },
        { read_at: "" },
      ]),
    ).toBe(2);
  });
});

describe("notification presentation", () => {
  it("maps known types to labelled chips", () => {
    expect(notificationMeta(baseNotification).label).toBe("needs you");
    expect(notificationMeta({ ...baseNotification, type: "task_failed" }).className).toContain("chip-error");
    expect(notificationMeta({ ...baseNotification, type: "agent_died" }).label).toBe("agent died");
    expect(notificationMeta({ ...baseNotification, type: "task_stuck" }).label).toBe("task stuck");
  });

  it("falls back gracefully for unknown types", () => {
    const meta = notificationMeta({ ...baseNotification, type: "weird" as AppNotification["type"] });
    expect(meta.label).toBe("weird");
  });

  it("resolves severity classes with warning default", () => {
    expect(notificationSeverityClass("error")).toBe("notif-sev-error");
    expect(notificationSeverityClass(undefined)).toBe("notif-sev-warning");
  });

  it("links to the task when both refs exist", () => {
    expect(notificationLink({ ...baseNotification, task_id: "t9" })).toBe("/squads/s1/tasks/t9");
    expect(notificationLink(baseNotification)).toBeNull();
    expect(notificationLink({ ...baseNotification, task_id: "t9", squad_id: "" })).toBeNull();
  });
});

describe("buildScopedListQuery", () => {
  it("returns empty string with no filters", () => {
    expect(buildScopedListQuery({})).toBe("");
  });

  it("emits only meaningful params", () => {
    expect(buildScopedListQuery({ unread: true })).toBe("?unread=true");
    expect(buildScopedListQuery({ userId: "  " })).toBe("");
    expect(buildScopedListQuery({ userId: "u1", limit: 200 })).toBe("?user_id=u1&limit=200");
    expect(buildScopedListQuery({ unread: false, userId: "u1" })).toBe("?user_id=u1");
  });
});

describe("inbox display helpers", () => {
  const msg: InboxMessage = {
    id: "i1",
    squad_id: "s1",
    kind: "agent_message",
    message: "one line summary",
    created_at: "2026-10-01T01:00:00Z",
  };

  it("falls back to message when subject/body are absent", () => {
    expect(inboxDisplay(msg)).toEqual({ subject: "one line summary", body: "one line summary" });
  });

  it("prefers the S-181 subject/body payload", () => {
    expect(inboxDisplay({ ...msg, subject: " Sub ", body: " Body text " })).toEqual({
      subject: "Sub",
      body: "Body text",
    });
  });

  it("labels the agent_message kind", () => {
    expect(inboxKindMeta("agent_message").label).toBe("from agent");
    expect(inboxKindMeta("task_completed").className).toContain("chip-done");
    expect(inboxKindMeta("mystery").label).toBe("mystery");
  });

  it("links inbox rows to their task", () => {
    expect(inboxTaskLink({ ...msg, task_id: "t5" })).toBe("/squads/s1/tasks/t5");
    expect(inboxTaskLink(msg)).toBeNull();
  });
});
