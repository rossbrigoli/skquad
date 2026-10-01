import { describe, expect, it } from "vitest";
import { formatMaskedApiKey, MASKED_KEY_MASK_CHARS } from "./providerKey";

describe("formatMaskedApiKey (S-187)", () => {
  it("renders the control-plane hint as fixed mask + tail", () => {
    // Backend shape: "•••••" + last 5 chars.
    expect(formatMaskedApiKey("•••••asdk")).toBe("••••••••••••asdk");
    expect(formatMaskedApiKey("•••••abcde")).toBe("••••••••••••abcde");
  });

  it("uses a fixed mask length regardless of the incoming mask length", () => {
    const short = formatMaskedApiKey("•xy");
    const long = formatMaskedApiKey("••••••••••••••••••••wxyz1");
    expect(short).toBe("•".repeat(MASKED_KEY_MASK_CHARS) + "xy");
    expect(long).toBe("•".repeat(MASKED_KEY_MASK_CHARS) + "wxyz1");
    // The mask prefix is fixed; only the (<=5 char) tail varies.
    expect(short.startsWith("•".repeat(MASKED_KEY_MASK_CHARS))).toBe(true);
    expect(long.startsWith("•".repeat(MASKED_KEY_MASK_CHARS))).toBe(true);
    expect(long.length - MASKED_KEY_MASK_CHARS).toBeLessThanOrEqual(5);
  });

  it("never shows more than the last 5 visible characters", () => {
    const out = formatMaskedApiKey("ABCDEFGHIJKLMNOPQRST");
    expect(out).toBe("•".repeat(MASKED_KEY_MASK_CHARS) + "PQRST");
  });

  it("handles missing or fully-masked hints without leaking length", () => {
    const maskOnly = "•".repeat(MASKED_KEY_MASK_CHARS);
    expect(formatMaskedApiKey(undefined)).toBe(maskOnly);
    expect(formatMaskedApiKey(null)).toBe(maskOnly);
    expect(formatMaskedApiKey("")).toBe(maskOnly);
    // Backend fully-bullets keys of <=5 chars; tail extraction yields nothing.
    expect(formatMaskedApiKey("•••••")).toBe(maskOnly);
  });

  it("treats asterisk masks like bullets", () => {
    expect(formatMaskedApiKey("*****z9qk")).toBe("•".repeat(MASKED_KEY_MASK_CHARS) + "z9qk");
  });
});
