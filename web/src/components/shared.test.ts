import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  agentUsesSquadLLM,
  formatCost,
  formatRelativeTime,
  leaseState,
  messageDeliveryNote,
  messageText,
  permissionsWithLLM,
  pickModel,
  providerModels,
  resolveSquadLLM,
  resourceLabel,
  squadLLM,
  withSquadLLM,
} from "../components/shared";
import type { SquadLLMStatus } from "../components/shared";
import type {
  AgentPermission,
  LLMProvider,
  Message,
  MeteringSummary,
  RegistryResource,
  Task,
} from "../lib/api";

const NOW = new Date("2026-09-03T12:00:00Z");

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
});

afterEach(() => {
  vi.useRealTimers();
});

function msg(overrides: Partial<Message>): Message {
  return {
    id: "m-1",
    from_type: "agent",
    from_id: "a-1",
    to_agent_id: "a-2",
    squad_id: "s-1",
    type: "consult",
    status: "pending",
    ...overrides,
  };
}

describe("messageText", () => {
  it("prefers the payload message", () => {
    const message = msg({ payload: { message: "hello agent" } });
    expect(messageText(message)).toBe("hello agent");
  });

  it("falls back to serialising the payload", () => {
    const message = msg({ payload: { kind: "consult", ids: [1, 2] } });
    expect(messageText(message)).toBe(JSON.stringify({ kind: "consult", ids: [1, 2] }));
  });

  it("handles a missing payload", () => {
    expect(messageText(msg({}))).toBe("{}");
  });
});

describe("resourceLabel", () => {
  const providers = [{ id: "p-1", name: "OpenAI" }] as LLMProvider[];
  const resources = [{ id: "r-1", name: "Git Tool" }] as RegistryResource[];

  function permission(resource_type: string, resource_id: string): AgentPermission {
    return { id: "perm-1", agent_id: "a-1", resource_type, resource_id } as AgentPermission;
  }

  it("resolves LLM provider names from the provider list", () => {
    expect(resourceLabel(permission("llm_provider", "p-1"), providers, resources)).toBe("OpenAI");
  });

  it("resolves registry resource names from the resource list", () => {
    expect(resourceLabel(permission("tool", "r-1"), providers, resources)).toBe("Git Tool");
  });

  it("falls back to the raw id when the resource is unknown", () => {
    expect(resourceLabel(permission("llm_provider", "missing"), providers, resources)).toBe("missing");
    expect(resourceLabel(permission("skill", "missing"), providers, resources)).toBe("missing");
  });

  it("does not resolve a tool id against the provider list", () => {
    // p-1 exists as a provider id; a tool grant with the same id must not match.
    expect(resourceLabel(permission("tool", "p-1"), providers, resources)).toBe("p-1");
  });
});

describe("formatCost", () => {
  it("shows a placeholder when there is no summary", () => {
    expect(formatCost(null)).toBe("-");
  });

  it("defaults to USD and four decimal places", () => {
    const summary = { cost: 0.5 } as MeteringSummary;
    expect(formatCost(summary)).toBe("USD 0.5000");
  });

  it("treats a missing cost as zero", () => {
    expect(formatCost({ currency: "EUR" } as MeteringSummary)).toBe("EUR 0.0000");
  });
});

describe("leaseState", () => {
  function task(overrides: Partial<Task>): Task {
    return { id: "t-1", board_id: "b-1", squad_id: "s-1", title: "task", status: "in-progress", ...overrides } as Task;
  }

  it("is idle without an execution", () => {
    expect(leaseState(task({}))).toBe("idle");
  });

  it("is idle without a lease timestamp", () => {
    expect(leaseState(task({ execution_id: "e-1" }))).toBe("idle");
  });

  it("treats the Go zero time as no lease rather than a stalled one", () => {
    expect(leaseState(task({ execution_id: "e-1", lease_expires_at: "0001-01-01T00:00:00Z" }))).toBe("idle");
  });

  it("treats an unparseable timestamp as no lease", () => {
    expect(leaseState(task({ execution_id: "e-1", lease_expires_at: "not-a-date" }))).toBe("idle");
  });

  it("is running while the lease is in the future", () => {
    const future = new Date(NOW.getTime() + 60_000).toISOString();
    expect(leaseState(task({ execution_id: "e-1", lease_expires_at: future }))).toBe("running");
  });

  it("is stalled once the lease has lapsed", () => {
    const past = new Date(NOW.getTime() - 60_000).toISOString();
    expect(leaseState(task({ execution_id: "e-1", lease_expires_at: past }))).toBe("stalled");
  });
});

describe("formatRelativeTime", () => {
  it("returns an empty string for missing or invalid values", () => {
    expect(formatRelativeTime(undefined)).toBe("");
    expect(formatRelativeTime("")).toBe("");
    expect(formatRelativeTime("nonsense")).toBe("");
  });

  it("formats seconds, minutes, hours and days", () => {
    const iso = (offsetMs: number) => new Date(NOW.getTime() + offsetMs).toISOString();

    expect(formatRelativeTime(iso(45_000))).toMatch(/45 second/);
    expect(formatRelativeTime(iso(5 * 60_000))).toMatch(/5 minute/);
    expect(formatRelativeTime(iso(3 * 3_600_000))).toMatch(/3 hour/);
    expect(formatRelativeTime(iso(2 * 86_400_000))).toMatch(/2 day/);
  });

  it("distinguishes past from future", () => {
    const iso = (offsetMs: number) => new Date(NOW.getTime() + offsetMs).toISOString();
    const future = formatRelativeTime(iso(600_000));
    const past = formatRelativeTime(iso(-600_000));

    expect(future).not.toBe(past);
    expect(past).toMatch(/ago/);
  });
});

describe("messageDeliveryNote", () => {
  it("is empty for a message with no delivery metadata", () => {
    expect(messageDeliveryNote(msg({}))).toBe("");
  });

  it("omits zero attempts", () => {
    expect(messageDeliveryNote(msg({ attempts: 0 }))).toBe("");
  });

  it("includes the attempt count and max when known", () => {
    expect(messageDeliveryNote(msg({ attempts: 2 }))).toBe("attempt 2");
    expect(messageDeliveryNote(msg({ attempts: 2, max_attempts: 5 }))).toBe("attempt 2/5");
  });

  it("says a retry is due when the scheduled time already passed", () => {
    const past = new Date(NOW.getTime() - 30_000).toISOString();
    expect(messageDeliveryNote(msg({ next_retry_at: past }))).toBe("retry due");
  });

  it("shows a future retry as a relative time", () => {
    const future = new Date(NOW.getTime() + 120_000).toISOString();
    expect(messageDeliveryNote(msg({ next_retry_at: future }))).toMatch(/^retry .*minute/);
  });

  it("joins every part with a separator", () => {
    const expires = new Date(NOW.getTime() + 3_600_000).toISOString();
    const note = messageDeliveryNote(msg({ attempts: 1, max_attempts: 3, expires_at: expires }));

    expect(note).toMatch(/^attempt 1\/3/);
    expect(note).toContain("·");
    expect(note).toMatch(/expires .*hour/);
  });

  it("ignores an unparseable expiry rather than throwing", () => {
    const note = messageDeliveryNote(msg({ attempts: 1, expires_at: "garbage" }));
    expect(note).toContain("attempt 1");
  });
});

function provider(overrides: Partial<LLMProvider>): LLMProvider {
  return { id: "p-1", name: "Provider", kind: "openai", base_url: "", status: "active", ...overrides } as LLMProvider;
}

describe("squadLLM", () => {
  it("reads the provider and model from operating_model", () => {
    expect(squadLLM({ operating_model: { llm: { provider_id: "p-1", model: "m-1" } } })).toEqual({ provider_id: "p-1", model: "m-1" });
  });

  it("is null when the squad has no LLM", () => {
    expect(squadLLM(null)).toBeNull();
    expect(squadLLM({ operating_model: {} })).toBeNull();
    expect(squadLLM({ operating_model: { llm: { model: "m-1" } } })).toBeNull();
  });

  it("treats a malformed operating model as unset rather than throwing", () => {
    expect(squadLLM({ operating_model: "garbage" })).toBeNull();
    expect(squadLLM({ operating_model: [1, 2] })).toBeNull();
    expect(squadLLM({ operating_model: { llm: "p-1" } })).toBeNull();
    expect(squadLLM({ operating_model: { llm: { provider_id: "  " } } })).toBeNull();
  });

  it("tolerates a missing model", () => {
    expect(squadLLM({ operating_model: { llm: { provider_id: "p-1" } } })).toEqual({ provider_id: "p-1", model: "" });
  });
});

describe("withSquadLLM", () => {
  it("keeps other operating-model keys when setting the LLM", () => {
    expect(withSquadLLM({ cadence: "weekly" }, { provider_id: "p-1", model: "m-1" }))
      .toEqual({ cadence: "weekly", llm: { provider_id: "p-1", model: "m-1" } });
  });

  it("replaces a previous LLM", () => {
    expect(withSquadLLM({ llm: { provider_id: "old", model: "x" } }, { provider_id: "p-2", model: "m-2" }))
      .toEqual({ llm: { provider_id: "p-2", model: "m-2" } });
  });

  it("removes the LLM when given none", () => {
    expect(withSquadLLM({ cadence: "weekly", llm: { provider_id: "p-1", model: "m" } }, null)).toEqual({ cadence: "weekly" });
  });

  it("starts from an empty object when the operating model is not an object", () => {
    expect(withSquadLLM(undefined, { provider_id: "p-1", model: "m" })).toEqual({ llm: { provider_id: "p-1", model: "m" } });
    expect(withSquadLLM([1], { provider_id: "p-1", model: "m" })).toEqual({ llm: { provider_id: "p-1", model: "m" } });
  });

  it("does not mutate its input", () => {
    const original = { cadence: "weekly" };
    withSquadLLM(original, { provider_id: "p-1", model: "m" });
    expect(original).toEqual({ cadence: "weekly" });
  });
});

describe("providerModels", () => {
  it("lists the default model first, then the models list, without duplicates", () => {
    expect(providerModels(provider({ default_model: "b", models: ["a", "b", " c "] }))).toEqual(["b", "a", "c"]);
  });

  it("ignores non-string and blank entries", () => {
    expect(providerModels(provider({ default_model: " ", models: ["a", 3, "", null] }))).toEqual(["a"]);
  });

  it("handles a missing provider or models list", () => {
    expect(providerModels(null)).toEqual([]);
    expect(providerModels(provider({ default_model: "only" }))).toEqual(["only"]);
    expect(providerModels(provider({ models: "not-an-array" }))).toEqual([]);
  });
});

describe("resolveSquadLLM", () => {
  const providers = [
    provider({ id: "p-active", default_model: "m-1", models: ["m-2"] }),
    provider({ id: "p-old", status: "deprecated", default_model: "m-9" }),
    provider({ id: "p-empty" }),
  ];
  const squadOn = (providerID: string) => ({ operating_model: { llm: { provider_id: providerID, model: "m-1" } } });

  it("is unset when the squad has no LLM", () => {
    expect(resolveSquadLLM({ operating_model: {} }, providers)).toEqual({ state: "unset" });
  });

  it("is unknown when the provider is not in the list", () => {
    expect(resolveSquadLLM(squadOn("p-gone"), providers).state).toBe("unknown");
  });

  it("flags a deprecated provider, which the gateway would skip", () => {
    expect(resolveSquadLLM(squadOn("p-old"), providers).state).toBe("deprecated");
  });

  it("flags an active provider that serves no models", () => {
    expect(resolveSquadLLM(squadOn("p-empty"), providers).state).toBe("no-models");
  });

  it("is ready with the provider's models", () => {
    const status = resolveSquadLLM(squadOn("p-active"), providers);
    expect(status.state).toBe("ready");
    expect(status.state === "ready" && status.models).toEqual(["m-1", "m-2"]);
  });
});

describe("pickModel", () => {
  const models = ["m-1", "m-2"];

  it("returns the first preferred model the provider serves", () => {
    expect(pickModel(models, "m-9", "m-2")).toBe("m-2");
  });

  it("falls back to the provider's first model", () => {
    expect(pickModel(models, "nope", undefined, "")).toBe("m-1");
  });

  it("is empty when the provider serves nothing", () => {
    expect(pickModel([], "m-1")).toBe("");
  });
});

describe("agentUsesSquadLLM", () => {
  const ready: SquadLLMStatus = {
    state: "ready",
    llm: { provider_id: "p-1", model: "m-1" },
    provider: provider({ id: "p-1" }),
    models: ["m-1", "m-2"],
  };
  const grant = [{ resource_type: "llm_provider", resource_id: "p-1" }] as AgentPermission[];

  it("is true when provider, grant and model all line up", () => {
    expect(agentUsesSquadLLM({ default_provider_id: "p-1", default_model: "m-2" }, grant, ready)).toBe(true);
  });

  it("is false without the gateway grant", () => {
    expect(agentUsesSquadLLM({ default_provider_id: "p-1", default_model: "m-1" }, [], ready)).toBe(false);
  });

  it("is false on a different provider", () => {
    expect(agentUsesSquadLLM({ default_provider_id: "p-2", default_model: "m-1" }, grant, ready)).toBe(false);
  });

  it("is false for a model the provider does not serve", () => {
    expect(agentUsesSquadLLM({ default_provider_id: "p-1", default_model: "gpt-x" }, grant, ready)).toBe(false);
  });

  it("is false whenever the squad LLM is not ready", () => {
    expect(agentUsesSquadLLM({ default_provider_id: "p-1", default_model: "m-1" }, grant, { state: "unset" })).toBe(false);
  });
});

describe("permissionsWithLLM", () => {
  it("replaces existing provider grants and keeps everything else", () => {
    const current = [
      { resource_type: "tool", resource_id: "t-1" },
      { resource_type: "llm_provider", resource_id: "old" },
      { resource_type: "skill", resource_id: "s-1" },
    ] as AgentPermission[];
    expect(permissionsWithLLM(current, "p-1")).toEqual([
      { resource_type: "tool", resource_id: "t-1" },
      { resource_type: "skill", resource_id: "s-1" },
      { resource_type: "llm_provider", resource_id: "p-1" },
    ]);
  });

  it("grants the provider to an agent with no permissions", () => {
    expect(permissionsWithLLM([], "p-1")).toEqual([{ resource_type: "llm_provider", resource_id: "p-1" }]);
  });
});
