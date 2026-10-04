// S-199: per-user notification mute preferences.
//
// The control plane stores one mute list per user (migration 0032:
// user_notification_preferences). Absence of a row means every type is
// enabled — the default is never materialized client-side either, so a
// user who has never visited Settings sees all four toggles ON.
// Pure helpers live here so the panel stays presentational and the
// logic is unit-testable.

import { apiGet, apiPut } from "./api";
import type { NotificationType } from "./api";

export type NotificationPreferences = {
  muted_types?: NotificationType[];
};

// NOTIFICATION_PREF_OPTIONS drives the Settings checkboxes: one row per
// notification type with human copy matching the bell labels.
export const NOTIFICATION_PREF_OPTIONS: readonly {
  type: NotificationType;
  label: string;
  description: string;
}[] = [
  {
    type: "task_failed",
    label: "Task failed",
    description: "A task attempt failed — the agent reported an error while working.",
  },
  {
    type: "task_stuck",
    label: "Task stuck",
    description: "A claimed task has made no progress for a long time.",
  },
  {
    type: "agent_died",
    label: "Agent died",
    description: "An agent's lease expired mid-task; the task was re-queued.",
  },
  {
    type: "task_blocked",
    label: "Task blocked / needs you",
    description: "A task is waiting for your input or a decision.",
  },
  {
    type: "budget_warning",
    label: "Budget warning",
    description: "Your monthly budget (or the platform limit) hit 80% or 90%.",
  },
  {
    type: "budget_stopped",
    label: "Budget stopped",
    description: "Your budget is exhausted; your agents stop at the end of their turn.",
  },
];

export const ALL_NOTIFICATION_TYPES: readonly NotificationType[] = NOTIFICATION_PREF_OPTIONS.map((o) => o.type);

// mutedSet normalizes a preferences payload into a lookup set.
export function mutedSet(prefs: NotificationPreferences | null | undefined): Set<NotificationType> {
  return new Set((prefs?.muted_types ?? []).filter((t) => ALL_NOTIFICATION_TYPES.includes(t)));
}

// isTypeEnabled is the checkbox state: on unless explicitly muted.
export function isTypeEnabled(prefs: NotificationPreferences | null | undefined, type: NotificationType): boolean {
  return !mutedSet(prefs).has(type);
}

// buildMutedList converts the checkbox map (enabled flags) into the
// muted_types payload sent to PUT /notifications/preferences.
export function buildMutedList(enabled: Record<NotificationType, boolean>): NotificationType[] {
  return ALL_NOTIFICATION_TYPES.filter((t) => !enabled[t]);
}

// hasChanges compares the current checkbox map against the loaded prefs.
export function hasChanges(prefs: NotificationPreferences | null | undefined, enabled: Record<NotificationType, boolean>): boolean {
  const muted = mutedSet(prefs);
  return ALL_NOTIFICATION_TYPES.some((t) => muted.has(t) !== !enabled[t]);
}

export async function fetchNotificationPreferences(token: string): Promise<NotificationPreferences> {
  return apiGet<NotificationPreferences>("/notifications/preferences", token);
}

export async function saveNotificationPreferences(token: string, muted: NotificationType[]): Promise<NotificationPreferences> {
  return apiPut<NotificationPreferences>("/notifications/preferences", token, { muted_types: muted });
}
