import { describe, expect, it } from "vitest";
import type { AIModel } from "./aimodels";
import {
  bindingWarnings,
  buildBindingPayload,
  fallbackChoices,
  findModelById,
  isActiveModel,
  modelLabel,
  selectableModels,
  withCurrentOption,
} from "./agentLlm";

function model(overrides: Partial<AIModel> = {}): AIModel {
  return {
    id: "m-primary",
    provider_id: "prov-openai",
    display_name: "GPT-6 Sol",
    model_name: "gpt-6-sol",
    context_window: 200000,
    supports_tools: true,
    pricing: {
      input_per_1m: 2.5,
      cached_input_per_1m: 0.25,
      cache_write_per_1m: 3,
      output_per_1m: 15,
    },
    long_context_threshold_tokens: 272000,
    status: "active",
    ...overrides,
  };
}

const primary = model();
// Cheaper, bigger-context, tool-capable model on a DIFFERENT provider —
// the "ideal fallback" that triggers none of W1/W2/W3.
const cleanFallback = model({
  id: "m-fallback",
  provider_id: "prov-anthropic",
  display_name: "Claude Next",
  model_name: "claude-next",
  context_window: 300000,
  supports_tools: true,
  pricing: { input_per_1m: 2.0, cached_input_per_1m: 0.2, cache_write_per_1m: 2.5, output_per_1m: 12 },
});

describe("selectableModels / isActiveModel", () => {
  it("keeps active models and drops non-active ones", () => {
    const deprecated = model({ id: "m-old", status: "deprecated" });
    const active = model({ id: "m-new", status: "active" });
    const unknownStatus = model({ id: "m-blank", status: "" });
    const list = selectableModels([active, deprecated, unknownStatus]);
    expect(list.map((m) => m.id)).toEqual(["m-new", "m-blank"]);
    expect(isActiveModel(deprecated)).toBe(false);
  });

  it("treats a missing status as active", () => {
    const m = model();
    delete (m as { status?: string }).status;
    expect(isActiveModel(m)).toBe(true);
  });
});

describe("fallbackChoices", () => {
  it("excludes the selected primary from fallback options", () => {
    const models = [primary, cleanFallback];
    const choices = fallbackChoices(models, primary.id);
    expect(choices.map((m) => m.id)).toEqual(["m-fallback"]);
    expect(choices.some((m) => m.id === primary.id)).toBe(false);
  });

  it("lists only granted+active models (deprecated excluded)", () => {
    const deprecated = model({ id: "m-old", status: "deprecated" });
    const choices = fallbackChoices([primary, cleanFallback, deprecated], primary.id);
    expect(choices.map((m) => m.id)).toEqual(["m-fallback"]);
  });

  it("with empty primary returns the full selectable list", () => {
    const choices = fallbackChoices([primary, cleanFallback], "");
    expect(choices.map((m) => m.id)).toEqual(["m-primary", "m-fallback"]);
  });
});

describe("bindingWarnings", () => {
  it("no warnings for a clean fallback (cheaper, bigger, different provider)", () => {
    expect(bindingWarnings(primary, cleanFallback)).toEqual([]);
  });

  it("no warnings when fallback is empty/undefined", () => {
    expect(bindingWarnings(primary, undefined)).toEqual([]);
    expect(bindingWarnings(primary, null)).toEqual([]);
  });

  it("no warnings when primary is missing even if fallback given", () => {
    expect(bindingWarnings(undefined, cleanFallback)).toEqual([]);
  });

  it("W1 (cost) fires when fallback input rate > primary input rate", () => {
    const pricey = model({
      id: "m-pricey",
      provider_id: "prov-x",
      pricing: { ...primary.pricing, input_per_1m: 9 },
    });
    const warnings = bindingWarnings(primary, pricey);
    expect(warnings.map((w) => w.code)).toContain("cost");
    expect(warnings.map((w) => w.code)).not.toContain("capability");
    expect(warnings.map((w) => w.code)).not.toContain("same_provider");
  });

  it("W1 does NOT fire when input rates are equal", () => {
    const equal = model({ id: "m-equal", provider_id: "prov-x", pricing: { ...primary.pricing, input_per_1m: 2.5 } });
    expect(bindingWarnings(primary, equal).map((w) => w.code)).not.toContain("cost");
  });

  it("W1 does NOT fire when fallback is cheaper", () => {
    expect(bindingWarnings(primary, cleanFallback).map((w) => w.code)).not.toContain("cost");
  });

  it("W1 skipped when either input rate is unknown", () => {
    const noPricing = model({ id: "m-nopricing", provider_id: "prov-x", pricing: null });
    expect(bindingWarnings(primary, noPricing).map((w) => w.code)).not.toContain("cost");
    const noInput = model({ id: "m-noinput", provider_id: "prov-x", pricing: { output_per_1m: 5 } });
    expect(bindingWarnings(primary, noInput).map((w) => w.code)).not.toContain("cost");
  });

  it("W2 (capability) fires when fallback lacks tool support", () => {
    const noTools = model({ id: "m-notools", provider_id: "prov-x", supports_tools: false });
    const warnings = bindingWarnings(primary, noTools);
    expect(warnings.map((w) => w.code)).toContain("capability");
    expect(warnings.find((w) => w.code === "capability")?.message).toMatch(/tool calling/);
  });

  it("W2 fires when fallback context window < primary context window", () => {
    const smallCtx = model({ id: "m-small", provider_id: "prov-x", context_window: 8000 });
    const warnings = bindingWarnings(primary, smallCtx);
    expect(warnings.map((w) => w.code)).toContain("capability");
    expect(warnings.find((w) => w.code === "capability")?.message).toMatch(/smaller/);
  });

  it("W2 combines both sub-conditions into one warning", () => {
    const bad = model({ id: "m-bad", provider_id: "prov-x", supports_tools: false, context_window: 8000 });
    const cap = bindingWarnings(primary, bad).filter((w) => w.code === "capability");
    expect(cap).toHaveLength(1);
    expect(cap[0].message).toMatch(/tool calling/);
    expect(cap[0].message).toMatch(/smaller/);
  });

  it("W2 does NOT fire when fallback context >= primary and tools supported", () => {
    const equalCtx = model({ id: "m-eqctx", provider_id: "prov-x", context_window: 200000, supports_tools: true });
    expect(bindingWarnings(primary, equalCtx).map((w) => w.code)).not.toContain("capability");
  });

  it("W2 context comparison skipped when either context is unknown (0)", () => {
    const unknownCtx = model({ id: "m-unk", provider_id: "prov-x", context_window: 0, supports_tools: true });
    expect(bindingWarnings(primary, unknownCtx).map((w) => w.code)).not.toContain("capability");
  });

  it("W3 (same_provider) fires when fallback shares the primary provider", () => {
    const sameProv = model({ id: "m-sameprov", provider_id: primary.provider_id });
    const warnings = bindingWarnings(primary, sameProv);
    expect(warnings.map((w) => w.code)).toContain("same_provider");
  });

  it("W3 does NOT fire across different providers", () => {
    expect(bindingWarnings(primary, cleanFallback).map((w) => w.code)).not.toContain("same_provider");
  });

  it("all three warnings can fire together", () => {
    const awful = model({
      id: "m-awful",
      provider_id: primary.provider_id,
      supports_tools: false,
      context_window: 8000,
      pricing: { ...primary.pricing, input_per_1m: 99 },
    });
    const codes = bindingWarnings(primary, awful).map((w) => w.code);
    expect(codes).toContain("cost");
    expect(codes).toContain("capability");
    expect(codes).toContain("same_provider");
  });
});

describe("buildBindingPayload", () => {
  it("primary set, fallback set", () => {
    expect(buildBindingPayload("m-primary", "m-fallback")).toEqual({
      ai_model_id: "m-primary",
      fallback_ai_model_id: "m-fallback",
    });
  });

  it("fallback cleared sends explicit empty string", () => {
    expect(buildBindingPayload("m-primary", "")).toEqual({
      ai_model_id: "m-primary",
      fallback_ai_model_id: "",
    });
    expect(buildBindingPayload("m-primary", null)).toEqual({
      ai_model_id: "m-primary",
      fallback_ai_model_id: "",
    });
    expect(buildBindingPayload("m-primary")).toEqual({
      ai_model_id: "m-primary",
      fallback_ai_model_id: "",
    });
  });

  it("trims whitespace on both ids", () => {
    expect(buildBindingPayload("  m-primary  ", "  m-fallback  ")).toEqual({
      ai_model_id: "m-primary",
      fallback_ai_model_id: "m-fallback",
    });
  });

  it("throws when primary is empty (required)", () => {
    expect(() => buildBindingPayload("")).toThrow(/ai_model_id is required/);
    expect(() => buildBindingPayload("   ")).toThrow(/ai_model_id is required/);
  });

  it("throws when fallback equals primary (backend 400 mirror)", () => {
    expect(() => buildBindingPayload("m-primary", "m-primary")).toThrow(/must differ/);
  });
});

describe("helpers", () => {
  it("findModelById resolves by id and tolerates empty id", () => {
    expect(findModelById([primary, cleanFallback], "m-fallback")?.id).toBe("m-fallback");
    expect(findModelById([primary], "")).toBeUndefined();
    expect(findModelById([primary], "nope")).toBeUndefined();
  });

  it("withCurrentOption: current already selectable → unchanged, not stale", () => {
    const res = withCurrentOption([primary, cleanFallback], [primary, cleanFallback], primary.id);
    expect(res.staleCurrent).toBe(false);
    expect(res.models.map((m) => m.id)).toEqual(["m-primary", "m-fallback"]);
  });

  it("withCurrentOption: current deprecated but known → prepended, stale", () => {
    const deprecated = model({ id: "m-old", status: "deprecated" });
    const res = withCurrentOption([primary], [primary, deprecated], "m-old");
    expect(res.staleCurrent).toBe(true);
    expect(res.models.map((m) => m.id)).toEqual(["m-old", "m-primary"]);
  });

  it("withCurrentOption: current unknown (revoked) → stale with no prepend", () => {
    const res = withCurrentOption([primary], [primary], "m-gone");
    expect(res.staleCurrent).toBe(true);
    expect(res.models.map((m) => m.id)).toEqual(["m-primary"]);
  });

  it("withCurrentOption: empty current → unchanged", () => {
    const res = withCurrentOption([primary], [primary], "");
    expect(res.staleCurrent).toBe(false);
  });

  it("modelLabel prefers display_name, falls back to model_name", () => {
    expect(modelLabel(primary)).toBe("GPT-6 Sol");
    expect(modelLabel(model({ display_name: "" }))).toBe("gpt-6-sol");
  });
});
