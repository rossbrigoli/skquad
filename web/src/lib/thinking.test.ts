import { describe, expect, it } from "vitest";
import {
  DEFAULT_THINKING_LEVEL,
  THINKING_LEVELS,
  resolveThinkingLevel,
  thinkingLevelLabel,
} from "./thinking";

describe("resolveThinkingLevel (S-178)", () => {
  it("passes through valid levels", () => {
    expect(resolveThinkingLevel("low")).toBe("low");
    expect(resolveThinkingLevel("medium")).toBe("medium");
    expect(resolveThinkingLevel("high")).toBe("high");
  });

  it("normalizes case and whitespace", () => {
    expect(resolveThinkingLevel("  HIGH  ")).toBe("high");
    expect(resolveThinkingLevel("Medium")).toBe("medium");
  });

  it("defaults to the median (medium) for unset/unknown values", () => {
    expect(resolveThinkingLevel(undefined)).toBe("medium");
    expect(resolveThinkingLevel("")).toBe("medium");
    expect(resolveThinkingLevel("turbo")).toBe("medium");
    expect(resolveThinkingLevel(null)).toBe("medium");
    expect(resolveThinkingLevel(42)).toBe("medium");
    expect(DEFAULT_THINKING_LEVEL).toBe("medium");
  });
});

describe("thinkingLevelLabel (S-178)", () => {
  it("capitalizes each level", () => {
    expect(THINKING_LEVELS.map(thinkingLevelLabel)).toEqual(["Low", "Medium", "High"]);
  });
});
