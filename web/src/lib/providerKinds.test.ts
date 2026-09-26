import { describe, expect, it } from "vitest";
import { kindOptionsFor, PROVIDER_KINDS } from "./providerKinds";

describe("providerKinds (S-132)", () => {
  it("offers the curated known API kinds", () => {
    const values = PROVIDER_KINDS.map((option) => option.value);
    expect(values).toContain("openai");
    expect(values).toContain("anthropic");
    expect(values).toContain("ollama_chat");
    expect(PROVIDER_KINDS.every((option) => option.label.length > 0)).toBe(true);
  });

  it("returns only curated kinds when there is no current value", () => {
    expect(kindOptionsFor()).toEqual([...PROVIDER_KINDS]);
    expect(kindOptionsFor("")).toEqual([...PROVIDER_KINDS]);
    expect(kindOptionsFor(null)).toEqual([...PROVIDER_KINDS]);
  });

  it("keeps a known current kind without duplicating it", () => {
    const options = kindOptionsFor("anthropic");
    expect(options.filter((option) => option.value === "anthropic")).toHaveLength(1);
    expect(options).toHaveLength(PROVIDER_KINDS.length);
  });

  it("preserves an unknown current kind as a labelled (current) option", () => {
    const options = kindOptionsFor("vllm-legacy");
    expect(options).toHaveLength(PROVIDER_KINDS.length + 1);
    expect(options[options.length - 1]).toEqual({
      value: "vllm-legacy",
      label: "vllm-legacy (current)",
    });
  });
});
