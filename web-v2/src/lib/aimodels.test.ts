import { describe, expect, it } from "vitest";
import { ApiError } from "./api";
import {
  buildAIModelPayload,
  emptyAIModelForm,
  formFromAIModel,
  formatCascadeReport,
  formatInUseMessage,
  formatRate,
  grantedModelIds,
  grantDiff,
  groupModelsByProvider,
  inUseConflict,
  isDuplicateModel,
  isPlatformAdmin,
  modelRowFields,
  PRICING_RATE_KEYS,
  withForce,
  type AIModel,
  type AIModelFormValues,
  type CascadeReport,
  type ModelUsageEntry,
} from "./aimodels";

function form(overrides: Partial<AIModelFormValues> = {}): AIModelFormValues {
  return {
    ...emptyAIModelForm(),
    provider_id: "prov-1",
    model_name: "gpt-6-sol",
    display_name: "GPT-6 Sol",
    context_window: "200000",
    supports_tools: true,
    pricing: {
      input_per_1m: "2.5",
      cached_input_per_1m: "0.25",
      cache_write_per_1m: "3",
      output_per_1m: "15",
    },
    long_context_threshold_tokens: "272000",
    ...overrides,
  };
}

function model(overrides: Partial<AIModel> = {}): AIModel {
  return {
    id: "m1",
    provider_id: "prov-1",
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

describe("buildAIModelPayload", () => {
  it("produces the exact POST/PATCH contract shape with long_context_threshold_tokens at top level", () => {
    const payload = buildAIModelPayload(form());
    expect(payload).toEqual({
      provider_id: "prov-1",
      model_name: "gpt-6-sol",
      display_name: "GPT-6 Sol",
      context_window: 200000,
      supports_tools: true,
      pricing: {
        input_per_1m: 2.5,
        cached_input_per_1m: 0.25,
        cache_write_per_1m: 3,
        output_per_1m: 15,
      },
      long_context_threshold_tokens: 272000,
    });
    // The critical contract point: the threshold is NOT inside pricing.
    expect(payload.pricing).not.toHaveProperty("long_context_threshold_tokens");
    // decodeJSON DisallowUnknownFields — no stray keys anywhere.
    expect(Object.keys(payload).sort((a, b) => a.localeCompare(b))).toEqual(
      ["context_window", "display_name", "long_context_threshold_tokens", "model_name", "pricing", "provider_id", "supports_tools"],
    );
    expect(Object.keys(payload.pricing as object).sort((a, b) => a.localeCompare(b))).toEqual(
      [...PRICING_RATE_KEYS].sort((a, b) => a.localeCompare(b)),
    );
  });

  it("omits display_name when blank so the backend default (= model_name) applies", () => {
    const payload = buildAIModelPayload(form({ display_name: "   " }));
    expect(payload).not.toHaveProperty("display_name");
  });

  it("omits long_context_threshold_tokens when blank (PATCH: leave unchanged)", () => {
    const payload = buildAIModelPayload(form({ long_context_threshold_tokens: "" }));
    expect(payload).not.toHaveProperty("long_context_threshold_tokens");
  });

  it("names the offending rate when a pricing field is missing", () => {
    const bad = form();
    delete (bad.pricing as Record<string, string>).cached_input_per_1m;
    expect(() => buildAIModelPayload(bad)).toThrow(/cached_input_per_1m is required in pricing/);
  });

  it("names the offending rate when it is non-numeric or negative", () => {
    expect(() => buildAIModelPayload(form({ pricing: { ...form().pricing, output_per_1m: "free" } }))).toThrow(
      /output_per_1m in pricing must be numeric/,
    );
    expect(() => buildAIModelPayload(form({ pricing: { ...form().pricing, input_per_1m: "-1" } }))).toThrow(
      /input_per_1m in pricing must be non-negative/,
    );
  });

  it("rejects missing provider/model and bad context_window with field-named errors", () => {
    expect(() => buildAIModelPayload(form({ provider_id: " " }))).toThrow(/provider_id is required/);
    expect(() => buildAIModelPayload(form({ model_name: "" }))).toThrow(/model_name is required/);
    expect(() => buildAIModelPayload(form({ context_window: "lots" }))).toThrow(/context_window must be a non-negative integer/);
    expect(() => buildAIModelPayload(form({ long_context_threshold_tokens: "-5" }))).toThrow(
      /long_context_threshold_tokens must be a non-negative integer/,
    );
  });

  it("round-trips through formFromAIModel without changing the payload", () => {
    const original = model();
    const payload = buildAIModelPayload(formFromAIModel(original));
    expect(payload).toEqual({
      provider_id: "prov-1",
      model_name: "gpt-6-sol",
      display_name: "GPT-6 Sol",
      context_window: 200000,
      supports_tools: true,
      pricing: { input_per_1m: 2.5, cached_input_per_1m: 0.25, cache_write_per_1m: 3, output_per_1m: 15 },
      long_context_threshold_tokens: 272000,
    });
  });
});

describe("error classification", () => {
  const usage: ModelUsageEntry[] = [
    { user_id: "u1", user_email: "alice@example.com", agent_id: "a1", agent_name: "alpha", squad_id: "s1", slot: "primary" },
    { user_id: "u2", user_email: "bob@example.com", agent_id: "", agent_name: "", squad_id: "", slot: "" },
  ];

  it("detects the S-103-shaped 409 in_use and returns the usage list", () => {
    const err = new ApiError(409, "in use", { error: "in_use", message: "in use", usage });
    expect(inUseConflict(err)).toEqual(usage);
  });

  it("returns null for a missing usage array but keeps the empty conflict", () => {
    const err = new ApiError(409, "in use", { error: "in_use" });
    expect(inUseConflict(err)).toEqual([]);
  });

  it("does not confuse duplicate_model with in_use", () => {
    const dup = new ApiError(409, "exists", { error: { code: "duplicate_model", message: "exists" } });
    expect(inUseConflict(dup)).toBeNull();
    expect(isDuplicateModel(dup)).toBe(true);
    expect(isDuplicateModel(new ApiError(409, "x", { error: "in_use" }))).toBe(false);
  });

  it("ignores non-409 and non-ApiError values", () => {
    expect(inUseConflict(new ApiError(500, "boom", { error: "in_use" }))).toBeNull();
    expect(inUseConflict(new Error("nope"))).toBeNull();
    expect(isDuplicateModel("nope")).toBe(false);
  });

  it("withForce appends ?force=true exactly once, merging with existing query params", () => {
    expect(withForce("/ai-models/m1", false)).toBe("/ai-models/m1");
    expect(withForce("/ai-models/m1", true)).toBe("/ai-models/m1?force=true");
    expect(withForce("/users/u1/models?x=1", true)).toBe("/users/u1/models?x=1&force=true");
  });
});

describe("in-use dialog copy", () => {
  it("lists affected agents with their slot and owner email", () => {
    const msg = formatInUseMessage([
      { user_id: "u1", user_email: "alice@example.com", agent_id: "a1", agent_name: "alpha", squad_id: "s1", slot: "primary" },
      { user_id: "u1", user_email: "alice@example.com", agent_id: "a2", agent_name: "beta", squad_id: "s1", slot: "fallback" },
    ]);
    expect(msg).toContain("alpha (primary) — alice@example.com");
    expect(msg).toContain("beta (fallback) — alice@example.com");
    expect(msg).toContain("2 user(s)/agent(s)");
  });

  it("labels grant-only entries as grants", () => {
    const msg = formatInUseMessage([
      { user_id: "u2", user_email: "bob@example.com", agent_id: "", agent_name: "", squad_id: "", slot: "" },
    ]);
    expect(msg).toContain("bob@example.com (grant)");
  });
});

describe("deprecate 200 report", () => {
  it("summarises affected agents and users", () => {
    const report: CascadeReport = {
      model_id: "m1",
      operation: "deprecate",
      affected_agents: 3,
      affected_users: ["u1", "u2"],
      key_actions: [
        { agent_id: "a1", agent_name: "alpha", squad_id: "s1", action: "updated" },
        { agent_id: "a2", agent_name: "beta", squad_id: "s1", action: "revoked" },
        { agent_id: "a3", agent_name: "gamma", squad_id: "s2", action: "updated" },
      ],
      failures: [],
    };
    const msg = formatCascadeReport(report);
    expect(msg).toContain("3 agent(s)");
    expect(msg).toContain("2 user(s)");
    expect(msg).toContain("3 virtual key(s) converged");
    expect(msg).not.toContain("WARNING");
  });

  it("surfaces convergence failures loudly", () => {
    const report: CascadeReport = {
      model_id: "m1",
      operation: "deprecate",
      affected_agents: 1,
      affected_users: ["u1"],
      key_actions: [],
      failures: [{ agent_id: "a1", agent_name: "alpha", squad_id: "s1", stage: "converge", error: "gateway down" }],
    };
    const msg = formatCascadeReport(report);
    expect(msg).toContain("WARNING");
    expect(msg).toContain("1 key convergence failure(s)");
  });
});

describe("AI Models list rendering", () => {
  it("renders every field including all four pricing rates", () => {
    const row = modelRowFields(model(), "openai-prod");
    expect(row.title).toBe("GPT-6 Sol");
    expect(row.subtitle).toBe("gpt-6-sol · openai-prod");
    expect(row.contextWindow).toBe("200,000 tokens");
    expect(row.tools).toBe("tools ✓");
    expect(row.rates.map((r) => r.label)).toEqual(["Input", "Cached input", "Cache write", "Output"]);
    expect(row.rates.map((r) => r.value)).toEqual(["$2.50/1M", "$0.25/1M", "$3.00/1M", "$15.00/1M"]);
    expect(row.longContextThreshold).toBe("long-context > 272,000 tokens");
    expect(row.status).toBe("active");
  });

  it("shows placeholders for missing capability/pricing data", () => {
    const row = modelRowFields(
      model({ supports_tools: false, context_window: 0, pricing: null, long_context_threshold_tokens: 0 }),
    );
    expect(row.tools).toBe("no tools");
    expect(row.contextWindow).toBe("unknown");
    expect(row.rates.every((r) => r.value === "—")).toBe(true);
    expect(row.longContextThreshold).toBe("no long-context tier");
    expect(row.subtitle).toContain("unknown provider");
  });

  it("formatRate handles zero, fractions and junk", () => {
    expect(formatRate(0)).toBe("$0.00/1M");
    expect(formatRate(0.000123)).toBe("$0.000123/1M");
    expect(formatRate("nope")).toBe("—");
    expect(formatRate(null)).toBe("—");
  });
});

describe("grant editor helpers", () => {
  it("grantedModelIds extracts ids", () => {
    expect(grantedModelIds([model({ id: "m1" }), model({ id: "m2" })])).toEqual(["m1", "m2"]);
  });

  it("grantDiff computes added and removed sets", () => {
    expect(grantDiff(["a", "b", "c"], ["b", "d"])).toEqual({ added: ["d"], removed: ["a", "c"] });
    expect(grantDiff(["a"], ["a"])).toEqual({ added: [], removed: [] });
  });
});

describe("role gating", () => {
  it("only platform_admin sees the admin tabs", () => {
    expect(isPlatformAdmin("platform_admin")).toBe(true);
    expect(isPlatformAdmin("user")).toBe(false);
    expect(isPlatformAdmin("")).toBe(false);
    expect(isPlatformAdmin(undefined)).toBe(false);
    expect(isPlatformAdmin(null)).toBe(false);
  });
});

// S-128 — grouping for the merged AI Models hierarchy tab
// (providers as groups, models nested underneath).
function provider(id: string, name = id) {
  return { id, name, kind: "openai", base_url: `https://${id}.test`, status: "active" };
}

function nestedModel(id: string, providerId: string, name = id): AIModel {
  return {
    id,
    provider_id: providerId,
    display_name: name,
    model_name: name,
    context_window: 200000,
    supports_tools: true,
    pricing: { input_per_1m: 1, cached_input_per_1m: 0.1, cache_write_per_1m: 1.25, output_per_1m: 2 },
    long_context_threshold_tokens: 0,
    status: "active",
  };
}

describe("groupModelsByProvider (S-128)", () => {
  it("nests models under their provider in provider-list order", () => {
    const providers = [provider("p-1"), provider("p-2")];
    const models = [nestedModel("m-2", "p-2"), nestedModel("m-1a", "p-1"), nestedModel("m-1b", "p-1")];
    const { groups, orphans } = groupModelsByProvider(providers, models);
    expect(orphans).toEqual([]);
    expect(groups.map((g) => g.provider.id)).toEqual(["p-1", "p-2"]);
    expect(groups[0].models.map((m) => m.id)).toEqual(["m-1a", "m-1b"]);
    expect(groups[1].models.map((m) => m.id)).toEqual(["m-2"]);
  });

  it("keeps providers with no models as empty groups", () => {
    const { groups, orphans } = groupModelsByProvider([provider("p-empty")], []);
    expect(groups).toHaveLength(1);
    expect(groups[0].models).toEqual([]);
    expect(orphans).toEqual([]);
  });

  it("returns models with unknown/missing providers as orphans", () => {
    const models = [nestedModel("m-ghost", "p-gone"), nestedModel("m-ok", "p-1")];
    const { groups, orphans } = groupModelsByProvider([provider("p-1")], models);
    expect(groups[0].models.map((m) => m.id)).toEqual(["m-ok"]);
    expect(orphans.map((m) => m.id)).toEqual(["m-ghost"]);
  });

  it("handles empty provider list (everything orphaned)", () => {
    const { groups, orphans } = groupModelsByProvider([], [nestedModel("m-1", "p-x")]);
    expect(groups).toEqual([]);
    expect(orphans.map((m) => m.id)).toEqual(["m-1"]);
  });
});
