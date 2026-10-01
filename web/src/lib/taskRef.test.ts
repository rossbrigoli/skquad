import { describe, expect, it } from "vitest";

import { formatTaskRef } from "./taskRef";

describe("formatTaskRef", () => {
  it("renders the per-squad task number as T-<n>", () => {
    expect(formatTaskRef({ task_number: 1 })).toBe("T-1");
    expect(formatTaskRef({ task_number: 184 })).toBe("T-184");
  });

  it("returns empty string for missing or unnumbered tasks", () => {
    expect(formatTaskRef(null)).toBe("");
    expect(formatTaskRef(undefined)).toBe("");
    expect(formatTaskRef({})).toBe("");
    expect(formatTaskRef({ task_number: 0 })).toBe("");
  });
});
