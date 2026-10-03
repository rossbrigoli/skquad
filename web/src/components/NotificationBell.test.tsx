// S-209: notification bell UX locks. Follows the repo's established
// source-assertion pattern (see DeadLettersPanel.test.tsx) because the
// suite has no jsdom/testing-library harness. The pure counting logic
// lives in lib/notifications.test.ts; this file locks the wiring:
//   1. the badge is conditional on unread > 0 (hidden at zero) and
//      shows the real count like the Inbox nav badge (no "9+" cap);
//   2. clicking a notification item marks it read via the API;
//   3. the popover header carries a mark-all-read ("clear all") button
//      wired to POST /notifications/read-all.
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const source = readFileSync(new URL("./NotificationBell.tsx", import.meta.url), "utf8");

describe("NotificationBell badge (S-209 req 1)", () => {
  it("hides the badge when nothing is unread", () => {
    expect(source).toMatch(/unread > 0 \? \(\s*\/\/ S-209[\s\S]*?<span className="notif-badge"/);
  });

  it("shows the real unread count like the Inbox nav badge (no 9+ cap)", () => {
    expect(source).not.toContain("9+");
    expect(source).toMatch(/<span className="notif-badge"[\s\S]*?\{unread\}[\s\S]*?<\/span>/);
  });
});

describe("NotificationBell click-to-read (S-209 req 2)", () => {
  it("marks the notification read through the API", () => {
    expect(source).toContain("/notifications/${id}/read");
  });

  it("clicking the whole item marks it read (bubbling covers link + body)", () => {
    expect(source).toMatch(
      /<li[\s\S]*?onClick=\{\(\) => \{\s*if \(isUnread\(n\.read_at\)\) void markRead\(n\.id\);/,
    );
  });

  it("the explicit Mark read button does not double-fire via bubbling", () => {
    expect(source).toMatch(/e\.stopPropagation\(\);\s*\n\s*void markRead\(n\.id\);/);
  });
});

describe("NotificationBell clear-all (S-209 req 3)", () => {
  it("popover header exposes a mark-all-read button when unread > 0", () => {
    expect(source).toMatch(
      /notif-popover-header[\s\S]*?unread > 0[\s\S]*?markAllRead[\s\S]*?Mark all read/,
    );
  });

  it("mark-all-read persists via POST /notifications/read-all", () => {
    expect(source).toContain('apiPost("/notifications/read-all"');
  });
});

describe("NotificationBell unread/read distinction (S-209 req 4)", () => {
  it("unread items carry notif-unread, read items carry notif-read", () => {
    expect(source).toMatch(/isUnread\(n\.read_at\) \? "notif-unread" : "notif-read"/);
  });
});
