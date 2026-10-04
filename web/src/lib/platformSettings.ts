// S-219: Platform settings tab — one Save button for ALL platform
// settings (idle scale-to-zero + embedder runtime). The tab diffs the
// form against the loaded current values and sends a single PUT
// /admin/settings containing ONLY the settings that actually changed —
// no blanket PUT of unchanged values. The control-plane PUT accepts
// partial bodies (pointer fields; absent fields are left untouched), so
// the diff maps directly onto the wire format.

import { apiGet, apiPut } from "./api";
import {
  DEFAULT_EMBEDDER_RUNTIME,
  type EmbedderRuntime,
  validateEmbedderRuntime,
} from "./embedderRuntime";
import {
  DEFAULT_IDLE_SCALE_TO_ZERO_MINUTES,
  parseIdleMinutesInput,
} from "./idleScaleToZero";

// PlatformSettingsCurrent is the persisted platform settings block.
export type PlatformSettingsCurrent = {
  idleMinutes: number;
  embedderRuntime: EmbedderRuntime;
};

// PlatformSettingsFormValues mirrors the tab form (raw text for the
// minutes input so mid-typing states are representable).
export type PlatformSettingsFormValues = {
  idleMinutes: string;
  embedderRuntime: EmbedderRuntime;
};

// PlatformSettingsChanges is the wire payload: only changed keys.
export type PlatformSettingsChanges = {
  idle_scale_to_zero_minutes?: number;
  embedder_runtime?: EmbedderRuntime;
};

export type PlatformSettingsDiff = {
  changes: PlatformSettingsChanges;
  errors: string[];
};

// normalizeEmbedderRuntime coerces a stored value to a known runtime,
// falling back to the default (mirrors fetchEmbedderRuntime).
export function normalizeEmbedderRuntime(raw: string | undefined): EmbedderRuntime {
  const lowered = (raw ?? DEFAULT_EMBEDDER_RUNTIME).toLowerCase() as EmbedderRuntime;
  return validateEmbedderRuntime(lowered) ? DEFAULT_EMBEDDER_RUNTIME : lowered;
}

export function formFromPlatformSettings(current: PlatformSettingsCurrent): PlatformSettingsFormValues {
  return {
    idleMinutes: String(current.idleMinutes),
    embedderRuntime: current.embedderRuntime,
  };
}

// diffPlatformSettings validates the form and produces the minimal
// change set: a key is included only when its parsed value differs from
// the current persisted value. Invalid input yields an error and never a
// change entry.
export function diffPlatformSettings(
  current: PlatformSettingsCurrent,
  form: PlatformSettingsFormValues,
): PlatformSettingsDiff {
  const changes: PlatformSettingsChanges = {};
  const errors: string[] = [];

  const { value: minutes, error: minutesError } = parseIdleMinutesInput(form.idleMinutes);
  if (minutesError) {
    errors.push(minutesError);
  } else if (minutes === null) {
    errors.push("Enter a number of minutes.");
  } else if (minutes !== current.idleMinutes) {
    changes.idle_scale_to_zero_minutes = minutes;
  }

  const runtimeError = validateEmbedderRuntime(form.embedderRuntime);
  if (runtimeError) {
    errors.push(runtimeError);
  } else if (form.embedderRuntime !== current.embedderRuntime) {
    changes.embedder_runtime = form.embedderRuntime;
  }

  return { changes, errors };
}

export function hasPlatformSettingsChanges(changes: PlatformSettingsChanges): boolean {
  return changes.idle_scale_to_zero_minutes !== undefined || changes.embedder_runtime !== undefined;
}

// loadPlatformSettings fetches the whole platform settings block in one
// GET (both settings live on the same endpoint).
export async function loadPlatformSettings(token: string): Promise<PlatformSettingsCurrent> {
  const res = await apiGet<{
    idle_scale_to_zero_minutes?: number;
    embedder_runtime?: string;
  }>("/admin/settings", token);
  return {
    idleMinutes: res.idle_scale_to_zero_minutes ?? DEFAULT_IDLE_SCALE_TO_ZERO_MINUTES,
    embedderRuntime: normalizeEmbedderRuntime(res.embedder_runtime),
  };
}

// savePlatformSettingsChanges sends ONE PUT with only the changed keys
// and returns the updated current values merged over `current` (the
// control-plane echoes back just the keys it applied).
export async function savePlatformSettingsChanges(
  token: string,
  current: PlatformSettingsCurrent,
  changes: PlatformSettingsChanges,
): Promise<PlatformSettingsCurrent> {
  if (!hasPlatformSettingsChanges(changes)) {
    throw new Error("Nothing to save — no settings changed.");
  }
  const res = await apiPut<{
    idle_scale_to_zero_minutes?: number;
    embedder_runtime?: string;
  }>("/admin/settings", token, changes);
  return {
    idleMinutes: res.idle_scale_to_zero_minutes ?? current.idleMinutes,
    embedderRuntime:
      res.embedder_runtime !== undefined
        ? normalizeEmbedderRuntime(res.embedder_runtime)
        : current.embedderRuntime,
  };
}
