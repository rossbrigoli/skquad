// S-183: platform-admin idle scale-to-zero setting.
//
// Agents scale to zero only after this many minutes with NO activity —
// no task running, no chat turn in progress, no pending inbox work.
// The value is persisted in the control-plane platform_settings store
// (migration 0026) and changed by platform admins from the Settings
// screen; the control-plane fans an agent mirror out through the
// Kubernetes outbox so every Agent CR adopts it without a redeploy.
// A per-agent "idle timeout (sec)" > 0 still overrides the platform
// value; 0/empty means "follow the platform setting".

import { apiGet, apiPut } from "./api";

export const IDLE_SCALE_TO_ZERO_MIN_MINUTES = 1;
export const IDLE_SCALE_TO_ZERO_MAX_MINUTES = 240;
export const DEFAULT_IDLE_SCALE_TO_ZERO_MINUTES = 15;

export type IdleScaleToZeroSettings = {
  idle_scale_to_zero_minutes?: number;
};

// validateIdleMinutes returns a human-readable error, or null when the
// value is a whole number of minutes inside the admin-allowed range
// (mirrors the control-plane PUT validation, 1..240).
export function validateIdleMinutes(value: number): string | null {
  if (!Number.isInteger(value)) {
    return "Enter whole minutes.";
  }
  if (value < IDLE_SCALE_TO_ZERO_MIN_MINUTES || value > IDLE_SCALE_TO_ZERO_MAX_MINUTES) {
    return `Must be between ${IDLE_SCALE_TO_ZERO_MIN_MINUTES} and ${IDLE_SCALE_TO_ZERO_MAX_MINUTES} minutes.`;
  }
  return null;
}

// parseIdleMinutesInput parses the raw settings-input text. Empty text
// means "no explicit value" (value null, no error) so callers can fall
// back to the current/default.
export function parseIdleMinutesInput(raw: string): { value: number | null; error: string | null } {
  const trimmed = raw.trim();
  if (trimmed === "") {
    return { value: null, error: null };
  }
  const parsed = Number(trimmed);
  if (!Number.isFinite(parsed)) {
    return { value: null, error: "Enter a number of minutes." };
  }
  const error = validateIdleMinutes(parsed);
  return { value: error ? null : parsed, error };
}

export async function fetchIdleScaleToZeroMinutes(token: string): Promise<number> {
  const res = await apiGet<IdleScaleToZeroSettings>("/admin/settings", token);
  return res.idle_scale_to_zero_minutes ?? DEFAULT_IDLE_SCALE_TO_ZERO_MINUTES;
}

export async function saveIdleScaleToZeroMinutes(token: string, minutes: number): Promise<number> {
  const error = validateIdleMinutes(minutes);
  if (error) {
    throw new Error(error);
  }
  const res = await apiPut<IdleScaleToZeroSettings>("/admin/settings", token, {
    idle_scale_to_zero_minutes: minutes,
  });
  return res.idle_scale_to_zero_minutes ?? minutes;
}
