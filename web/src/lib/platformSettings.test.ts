import { afterEach, describe, expect, it, vi } from "vitest";

import * as api from "./api";
import {
  type PlatformSettingsCurrent,
  diffPlatformSettings,
  formFromPlatformSettings,
  hasPlatformSettingsChanges,
  loadPlatformSettings,
  normalizeEmbedderRuntime,
  savePlatformSettingsChanges,
} from "./platformSettings";

// S-219 — the Platform tab's single Save sends ONLY the settings that
// actually changed. These tests lock the diff/validate/merge logic.

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    apiGet: vi.fn(),
    apiPut: vi.fn(),
  };
});

const CURRENT: PlatformSettingsCurrent = { idleMinutes: 15, embedderRuntime: "auto" };

describe("diffPlatformSettings", () => {
  it("produces no changes when the form matches current", () => {
    const { changes, errors } = diffPlatformSettings(CURRENT, formFromPlatformSettings(CURRENT));
    expect(errors).toEqual([]);
    expect(hasPlatformSettingsChanges(changes)).toBe(false);
  });

  it("includes only the idle minutes when only that changed", () => {
    const { changes, errors } = diffPlatformSettings(CURRENT, {
      idleMinutes: "30",
      embedderRuntime: "auto",
    });
    expect(errors).toEqual([]);
    expect(changes).toEqual({ idle_scale_to_zero_minutes: 30 });
  });

  it("includes only the embedder runtime when only that changed", () => {
    const { changes, errors } = diffPlatformSettings(CURRENT, {
      idleMinutes: "15",
      embedderRuntime: "cuda",
    });
    expect(errors).toEqual([]);
    expect(changes).toEqual({ embedder_runtime: "cuda" });
  });

  it("includes both keys when both changed", () => {
    const { changes, errors } = diffPlatformSettings(CURRENT, {
      idleMinutes: "60",
      embedderRuntime: "cpu",
    });
    expect(errors).toEqual([]);
    expect(changes).toEqual({ idle_scale_to_zero_minutes: 60, embedder_runtime: "cpu" });
  });

  it("reports invalid minutes without emitting a change", () => {
    const { changes, errors } = diffPlatformSettings(CURRENT, {
      idleMinutes: "999",
      embedderRuntime: "auto",
    });
    expect(errors).toEqual(["Must be between 1 and 240 minutes."]);
    expect(hasPlatformSettingsChanges(changes)).toBe(false);
  });

  it("reports empty minutes as an error (no implicit fallback on save)", () => {
    const { changes, errors } = diffPlatformSettings(CURRENT, {
      idleMinutes: "",
      embedderRuntime: "auto",
    });
    expect(errors).toEqual(["Enter a number of minutes."]);
    expect(hasPlatformSettingsChanges(changes)).toBe(false);
  });

  it("reports an unknown runtime without emitting a change", () => {
    const { changes, errors } = diffPlatformSettings(CURRENT, {
      idleMinutes: "15",
      embedderRuntime: "quantum" as PlatformSettingsCurrent["embedderRuntime"],
    });
    expect(errors).toHaveLength(1);
    expect(errors[0]).toMatch(/Must be one of/);
    expect(hasPlatformSettingsChanges(changes)).toBe(false);
  });

  it("accumulates errors from both fields", () => {
    const { errors } = diffPlatformSettings(CURRENT, {
      idleMinutes: "0",
      embedderRuntime: "turbo" as PlatformSettingsCurrent["embedderRuntime"],
    });
    expect(errors).toHaveLength(2);
  });
});

describe("loadPlatformSettings", () => {
  afterEach(() => vi.clearAllMocks());

  it("reads both settings from one GET with defaults when unset", async () => {
    vi.mocked(api.apiGet).mockResolvedValueOnce({ idle_scale_to_zero_minutes: 20, embedder_runtime: "vulkan" });
    await expect(loadPlatformSettings("tok")).resolves.toEqual({ idleMinutes: 20, embedderRuntime: "vulkan" });
    expect(api.apiGet).toHaveBeenCalledTimes(1);
    expect(api.apiGet).toHaveBeenCalledWith("/admin/settings", "tok");
  });

  it("falls back to defaults for missing values", async () => {
    vi.mocked(api.apiGet).mockResolvedValueOnce({});
    await expect(loadPlatformSettings("tok")).resolves.toEqual({ idleMinutes: 15, embedderRuntime: "auto" });
  });
});

describe("savePlatformSettingsChanges", () => {
  afterEach(() => vi.clearAllMocks());

  it("sends ONE PUT containing only the changed keys", async () => {
    vi.mocked(api.apiPut).mockResolvedValueOnce({ idle_scale_to_zero_minutes: 45 });
    const updated = await savePlatformSettingsChanges("tok", CURRENT, { idle_scale_to_zero_minutes: 45 });
    expect(api.apiPut).toHaveBeenCalledTimes(1);
    expect(api.apiPut).toHaveBeenCalledWith("/admin/settings", "tok", { idle_scale_to_zero_minutes: 45 });
    // Untouched settings survive the merge; changed ones update.
    expect(updated).toEqual({ idleMinutes: 45, embedderRuntime: "auto" });
  });

  it("merges a runtime-only change without clobbering minutes", async () => {
    vi.mocked(api.apiPut).mockResolvedValueOnce({ embedder_runtime: "cuda" });
    const updated = await savePlatformSettingsChanges("tok", CURRENT, { embedder_runtime: "cuda" });
    expect(api.apiPut).toHaveBeenCalledWith("/admin/settings", "tok", { embedder_runtime: "cuda" });
    expect(updated).toEqual({ idleMinutes: 15, embedderRuntime: "cuda" });
  });

  it("refuses to PUT an empty change set", async () => {
    await expect(savePlatformSettingsChanges("tok", CURRENT, {})).rejects.toThrow(/Nothing to save/);
    expect(api.apiPut).not.toHaveBeenCalled();
  });
});

describe("normalizeEmbedderRuntime", () => {
  it("lowercases known values and defaults unknown/missing", () => {
    expect(normalizeEmbedderRuntime("CUDA")).toBe("cuda");
    expect(normalizeEmbedderRuntime("bogus")).toBe("auto");
    expect(normalizeEmbedderRuntime(undefined)).toBe("auto");
  });
});
