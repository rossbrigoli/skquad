import { afterEach, describe, expect, it, vi } from "vitest";

import {
  DEFAULT_IDLE_SCALE_TO_ZERO_MINUTES,
  IDLE_SCALE_TO_ZERO_MAX_MINUTES,
  IDLE_SCALE_TO_ZERO_MIN_MINUTES,
  fetchIdleScaleToZeroMinutes,
  parseIdleMinutesInput,
  saveIdleScaleToZeroMinutes,
  validateIdleMinutes,
} from "./idleScaleToZero";
import * as api from "./api";

// S-183 — logic-layer tests for the idle scale-to-zero admin setting.

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    apiGet: vi.fn(),
    apiPut: vi.fn(),
  };
});

describe("validateIdleMinutes", () => {
  it("accepts the bounds inclusive", () => {
    expect(validateIdleMinutes(IDLE_SCALE_TO_ZERO_MIN_MINUTES)).toBeNull();
    expect(validateIdleMinutes(IDLE_SCALE_TO_ZERO_MAX_MINUTES)).toBeNull();
    expect(validateIdleMinutes(DEFAULT_IDLE_SCALE_TO_ZERO_MINUTES)).toBeNull();
  });

  it("rejects out-of-range and fractional values", () => {
    expect(validateIdleMinutes(0)).toMatch(/between/);
    expect(validateIdleMinutes(-1)).toMatch(/between/);
    expect(validateIdleMinutes(IDLE_SCALE_TO_ZERO_MAX_MINUTES + 1)).toMatch(/between/);
    expect(validateIdleMinutes(2.5)).toMatch(/whole/);
    expect(validateIdleMinutes(NaN)).toMatch(/whole/);
  });
});

describe("parseIdleMinutesInput", () => {
  it("treats empty text as no explicit value", () => {
    expect(parseIdleMinutesInput("")).toEqual({ value: null, error: null });
    expect(parseIdleMinutesInput("   ")).toEqual({ value: null, error: null });
  });

  it("parses valid whole minutes", () => {
    expect(parseIdleMinutesInput("15")).toEqual({ value: 15, error: null });
    expect(parseIdleMinutesInput(" 90 ")).toEqual({ value: 90, error: null });
  });

  it("rejects non-numeric and out-of-range input", () => {
    expect(parseIdleMinutesInput("abc")).toEqual({ value: null, error: "Enter a number of minutes." });
    const bad = parseIdleMinutesInput("0");
    expect(bad.value).toBeNull();
    expect(bad.error).toMatch(/between/);
  });
});

describe("fetchIdleScaleToZeroMinutes", () => {
  afterEach(() => vi.clearAllMocks());

  it("returns the stored value", async () => {
    vi.mocked(api.apiGet).mockResolvedValueOnce({ idle_scale_to_zero_minutes: 20 });
    await expect(fetchIdleScaleToZeroMinutes("tok")).resolves.toBe(20);
    expect(api.apiGet).toHaveBeenCalledWith("/admin/settings", "tok");
  });

  it("falls back to the 15-minute default when unset", async () => {
    vi.mocked(api.apiGet).mockResolvedValueOnce({});
    await expect(fetchIdleScaleToZeroMinutes("tok")).resolves.toBe(DEFAULT_IDLE_SCALE_TO_ZERO_MINUTES);
  });
});

describe("saveIdleScaleToZeroMinutes", () => {
  afterEach(() => vi.clearAllMocks());

  it("PUTs the minutes and returns the saved value", async () => {
    vi.mocked(api.apiPut).mockResolvedValueOnce({ idle_scale_to_zero_minutes: 30 });
    await expect(saveIdleScaleToZeroMinutes("tok", 30)).resolves.toBe(30);
    expect(api.apiPut).toHaveBeenCalledWith("/admin/settings", "tok", {
      idle_scale_to_zero_minutes: 30,
    });
  });

  it("refuses to send an invalid value", async () => {
    await expect(saveIdleScaleToZeroMinutes("tok", 0)).rejects.toThrow(/between/);
    expect(api.apiPut).not.toHaveBeenCalled();
  });
});
