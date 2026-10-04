import { describe, expect, it, vi } from "vitest";

import * as api from "./api";
import type { NotificationType } from "./api";
import {
  ALL_NOTIFICATION_TYPES,
  NOTIFICATION_PREF_OPTIONS,
  buildMutedList,
  fetchNotificationPreferences,
  hasChanges,
  isTypeEnabled,
  mutedSet,
  saveNotificationPreferences,
  type NotificationPreferences,
} from "./notificationPrefs";

// S-199 — logic-layer tests for per-user notification mute preferences.

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    apiGet: vi.fn(),
    apiPut: vi.fn(),
  };
});

const allEnabled: Record<NotificationType, boolean> = {
  task_failed: true,
  task_stuck: true,
  agent_died: true,
  task_blocked: true,
  budget_warning: true,
  budget_stopped: true,
};

describe("options", () => {
  it("covers exactly the six notification types", () => {
    expect(NOTIFICATION_PREF_OPTIONS.map((o) => o.type).sort()).toEqual(
      ["agent_died", "budget_stopped", "budget_warning", "task_blocked", "task_failed", "task_stuck"],
    );
    expect(ALL_NOTIFICATION_TYPES).toHaveLength(6);
  });
});

describe("mutedSet / isTypeEnabled", () => {
  it("defaults to enabled when there is no stored row", () => {
    for (const t of ALL_NOTIFICATION_TYPES) {
      expect(isTypeEnabled(null, t)).toBe(true);
      expect(isTypeEnabled(undefined, t)).toBe(true);
      expect(isTypeEnabled({}, t)).toBe(true);
      expect(isTypeEnabled({ muted_types: [] }, t)).toBe(true);
    }
  });

  it("reports muted types as disabled and leaves others enabled", () => {
    const prefs: NotificationPreferences = { muted_types: ["task_failed", "agent_died"] };
    expect(isTypeEnabled(prefs, "task_failed")).toBe(false);
    expect(isTypeEnabled(prefs, "agent_died")).toBe(false);
    expect(isTypeEnabled(prefs, "task_blocked")).toBe(true);
    expect(isTypeEnabled(prefs, "task_stuck")).toBe(true);
  });

  it("ignores unknown types in a payload", () => {
    const prefs = { muted_types: ["not_a_type"] } as unknown as NotificationPreferences;
    expect(mutedSet(prefs).size).toBe(0);
  });
});

describe("buildMutedList", () => {
  it("empty muted list when everything is enabled", () => {
    expect(buildMutedList(allEnabled)).toEqual([]);
  });

  it("mutes exactly the unchecked types", () => {
    expect(
      buildMutedList({ ...allEnabled, task_failed: false, task_blocked: false }),
    ).toEqual(["task_failed", "task_blocked"]);
  });

  it("all muted when everything is unchecked", () => {
    const none: Record<NotificationType, boolean> = {
      task_failed: false,
      task_stuck: false,
      agent_died: false,
      task_blocked: false,
      budget_warning: false,
      budget_stopped: false,
    };
    expect(buildMutedList(none).sort()).toEqual([...ALL_NOTIFICATION_TYPES].sort());
  });

  // S-203 WP4: budget toggles round-trip through the mute payload.
  it("mutes only the budget types when just those are unchecked", () => {
    expect(buildMutedList({ ...allEnabled, budget_warning: false, budget_stopped: false })).toEqual([
      "budget_warning",
      "budget_stopped",
    ]);
    expect(isTypeEnabled({ muted_types: ["budget_stopped"] }, "budget_stopped")).toBe(false);
    expect(isTypeEnabled({ muted_types: ["budget_stopped"] }, "budget_warning")).toBe(true);
  });
});

describe("hasChanges", () => {
  it("is false when the checkbox map matches stored prefs", () => {
    expect(hasChanges(null, allEnabled)).toBe(false);
    expect(hasChanges({ muted_types: ["task_failed"] }, { ...allEnabled, task_failed: false })).toBe(false);
  });

  it("is true when a toggle differs from stored prefs", () => {
    expect(hasChanges({ muted_types: ["task_failed"] }, allEnabled)).toBe(true);
    expect(hasChanges(null, { ...allEnabled, agent_died: false })).toBe(true);
  });
});

describe("fetch/save wiring", () => {
  it("GETs the preferences endpoint", async () => {
    const getSpy = vi.mocked(api.apiGet).mockResolvedValue({ muted_types: ["task_stuck"] });
    const result = await fetchNotificationPreferences("tok");
    expect(getSpy).toHaveBeenCalledWith("/notifications/preferences", "tok");
    expect(result).toEqual({ muted_types: ["task_stuck"] });
  });

  it("PUTs the muted_types payload", async () => {
    const putSpy = vi.mocked(api.apiPut).mockResolvedValue({ muted_types: ["agent_died"] });
    const result = await saveNotificationPreferences("tok", ["agent_died"]);
    expect(putSpy).toHaveBeenCalledWith("/notifications/preferences", "tok", {
      muted_types: ["agent_died"],
    });
    expect(result).toEqual({ muted_types: ["agent_died"] });
  });
});
