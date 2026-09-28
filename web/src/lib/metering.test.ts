import { describe, expect, it } from "vitest";
import { monthStartISO } from "./metering";

describe("monthStartISO", () => {
  it("returns local midnight on the first of the viewer's month", () => {
    // 2026-09-28 17:22 local → 2026-09-01T00:00:00 local.
    const now = new Date(2026, 8, 28, 17, 22, 5);
    const expected = new Date(2026, 8, 1, 0, 0, 0, 0).toISOString();
    expect(monthStartISO(now)).toBe(expected);
  });

  it("handles January (year rollover of the month index)", () => {
    const now = new Date(2027, 0, 15, 12, 0, 0);
    expect(monthStartISO(now)).toBe(new Date(2027, 0, 1).toISOString());
  });

  it("is a valid RFC3339 string accepted by the control-plane since parser", () => {
    const iso = monthStartISO();
    expect(new Date(iso).toISOString()).toBe(iso);
  });
});
