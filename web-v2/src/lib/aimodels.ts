// WP6 (S-111) — AI Models admin UI logic layer (ADR-0010).
//
// Browser-independent helpers for the Settings → AI Models / Access tabs:
// payload building against the control-plane contract, error classification
// (409 in_use vs duplicate_model), and display formatting. Kept out of the
// React components so the contract shape is unit-testable in the node
// environment, matching this repo's "logic layer" test philosophy.

import { ApiError } from "./api";

// --- Types mirroring control-plane JSON (domain.AIModel, model_cascade.go) ---

export const PRICING_RATE_KEYS = [
  "input_per_1m",
  "cached_input_per_1m",
  "cache_write_per_1m",
  "output_per_1m",
] as const;

export type PricingRateKey = (typeof PRICING_RATE_KEYS)[number];

export const PRICING_RATE_LABELS: Record<PricingRateKey, string> = {
  input_per_1m: "Input",
  cached_input_per_1m: "Cached input",
  cache_write_per_1m: "Cache write",
  output_per_1m: "Output",
};

export type AIModelPricing = Record<PricingRateKey, number>;

export type AIModel = {
  id: string;
  provider_id: string;
  display_name: string;
  model_name: string;
  context_window: number;
  supports_tools: boolean;
  pricing: Partial<AIModelPricing> | null;
  long_context_threshold_tokens: number;
  status: string;
  registered_by?: string;
  created_at?: string;
  updated_at?: string;
};

// One holder of a live reference to an AI Model: a user grant (no agent)
// or an agent binding with the slot ("primary"/"fallback") named.
export type ModelUsageEntry = {
  user_id: string;
  user_email: string;
  agent_id: string;
  agent_name: string;
  squad_id: string;
  slot: string;
};

export type CascadeKeyAction = {
  agent_id: string;
  agent_name: string;
  squad_id: string;
  action: string;
};

export type CascadeFailure = {
  agent_id: string;
  agent_name: string;
  squad_id: string;
  stage: string;
  error: string;
};

// 200 report from POST /ai-models/{id}/deprecate (NOT 204).
export type CascadeReport = {
  model_id: string;
  operation: string;
  affected_agents: number;
  affected_users: string[];
  key_actions: CascadeKeyAction[];
  failures: CascadeFailure[];
};

export type AdminUser = {
  id: string;
  email: string;
  name: string;
  role: string;
};

// --- Role gating -------------------------------------------------------

export const PLATFORM_ADMIN_ROLE = "platform_admin";

export function isPlatformAdmin(role?: string | null): boolean {
  return (role || "") === PLATFORM_ADMIN_ROLE;
}

// --- Form state --------------------------------------------------------

export type AIModelFormValues = {
  provider_id: string;
  model_name: string;
  display_name: string;
  context_window: string;
  supports_tools: boolean;
  pricing: Record<PricingRateKey, string>;
  long_context_threshold_tokens: string;
};

export function emptyPricingForm(): Record<PricingRateKey, string> {
  return {
    input_per_1m: "",
    cached_input_per_1m: "",
    cache_write_per_1m: "",
    output_per_1m: "",
  };
}

export function emptyAIModelForm(): AIModelFormValues {
  return {
    provider_id: "",
    model_name: "",
    display_name: "",
    context_window: "",
    supports_tools: true,
    pricing: emptyPricingForm(),
    long_context_threshold_tokens: "",
  };
}

export function formFromAIModel(m: AIModel): AIModelFormValues {
  const pricing = emptyPricingForm();
  for (const key of PRICING_RATE_KEYS) {
    const raw = m.pricing?.[key];
    pricing[key] = raw === undefined || raw === null ? "" : String(raw);
  }
  return {
    provider_id: m.provider_id || "",
    model_name: m.model_name || "",
    display_name: m.display_name || "",
    context_window: m.context_window ? String(m.context_window) : "",
    supports_tools: !!m.supports_tools,
    pricing,
    long_context_threshold_tokens: m.long_context_threshold_tokens
      ? String(m.long_context_threshold_tokens)
      : "",
  };
}

function parseRate(key: PricingRateKey, raw: string): number {
  const trimmed = (raw || "").trim();
  if (trimmed === "") {
    throw new Error(`${key} is required in pricing`);
  }
  const value = Number(trimmed);
  if (!Number.isFinite(value)) {
    throw new Error(`${key} in pricing must be numeric`);
  }
  if (value < 0) {
    throw new Error(`${key} in pricing must be non-negative`);
  }
  return value;
}

function parseNonNegativeInt(field: string, raw: string, allowEmpty: boolean): number | undefined {
  const trimmed = (raw || "").trim();
  if (trimmed === "") {
    if (allowEmpty) return undefined;
    throw new Error(`${field} is required`);
  }
  const value = Number(trimmed);
  if (!Number.isInteger(value) || value < 0) {
    throw new Error(`${field} must be a non-negative integer`);
  }
  return value;
}

// buildAIModelPayload produces the exact POST/PATCH body the API accepts
// (decodeJSON DisallowUnknownFields — no extra keys). Critical contract
// points: `long_context_threshold_tokens` is TOP-LEVEL (not inside
// pricing), and pricing carries exactly the four per-1M rates.
// display_name is omitted when blank so the backend default (= model_name)
// applies; PATCH rejects an empty display_name outright.
export function buildAIModelPayload(v: AIModelFormValues): Record<string, unknown> {
  const providerId = (v.provider_id || "").trim();
  if (providerId === "") throw new Error("provider_id is required");
  const modelName = (v.model_name || "").trim();
  if (modelName === "") throw new Error("model_name is required");

  const contextWindow = parseNonNegativeInt("context_window", v.context_window, true) ?? 0;

  const pricing: AIModelPricing = {
    input_per_1m: parseRate("input_per_1m", v.pricing.input_per_1m),
    cached_input_per_1m: parseRate("cached_input_per_1m", v.pricing.cached_input_per_1m),
    cache_write_per_1m: parseRate("cache_write_per_1m", v.pricing.cache_write_per_1m),
    output_per_1m: parseRate("output_per_1m", v.pricing.output_per_1m),
  };

  const payload: Record<string, unknown> = {
    provider_id: providerId,
    model_name: modelName,
    context_window: contextWindow,
    supports_tools: !!v.supports_tools,
    pricing,
  };
  const displayName = (v.display_name || "").trim();
  if (displayName !== "") {
    payload.display_name = displayName;
  }
  const threshold = parseNonNegativeInt("long_context_threshold_tokens", v.long_context_threshold_tokens, true);
  if (threshold !== undefined) {
    payload.long_context_threshold_tokens = threshold;
  }
  return payload;
}

// --- Error classification ---------------------------------------------

// isInUseConflict detects the S-103-shaped 409 ({"error":"in_use",
// "usage":[...]}) shared by model delete and grant-set PUT removals.
export function inUseConflict(err: unknown): ModelUsageEntry[] | null {
  if (!(err instanceof ApiError) || err.status !== 409) return null;
  const body = err.body as { error?: unknown; usage?: unknown } | undefined;
  if (!body || body.error !== "in_use") return null;
  return Array.isArray(body.usage) ? (body.usage as ModelUsageEntry[]) : [];
}

export function isDuplicateModel(err: unknown): boolean {
  if (!(err instanceof ApiError) || err.status !== 409) return false;
  const body = err.body as { error?: unknown } | undefined;
  return !!body && typeof body.error === "object" && (body.error as { code?: string })?.code === "duplicate_model";
}

// withForce appends the ?force=true retry parameter exactly once.
export function withForce(path: string, force: boolean): string {
  if (!force) return path;
  return `${path}${path.includes("?") ? "&" : "?"}force=true`;
}

// --- Display formatting ------------------------------------------------

export function formatRate(raw: unknown): string {
  if (raw === null || raw === undefined || raw === "") return "—";
  const value = Number(raw);
  if (!Number.isFinite(value) || value < 0) return "—";
  return `$${value.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 6 })}/1M`;
}

export type AIModelRow = {
  title: string;
  subtitle: string;
  contextWindow: string;
  tools: string;
  rates: { key: PricingRateKey; label: string; value: string }[];
  longContextThreshold: string;
  status: string;
};

// modelRowFields is the single source of truth for what the AI Models
// list row renders — every contract field, including all four rates.
export function modelRowFields(m: AIModel, providerName?: string): AIModelRow {
  return {
    title: m.display_name || m.model_name,
    subtitle: `${m.model_name} · ${providerName || "unknown provider"}`,
    contextWindow:
      m.context_window > 0 ? `${m.context_window.toLocaleString("en-US")} tokens` : "unknown",
    tools: m.supports_tools ? "tools ✓" : "no tools",
    rates: PRICING_RATE_KEYS.map((key) => ({
      key,
      label: PRICING_RATE_LABELS[key],
      value: formatRate(m.pricing?.[key]),
    })),
    longContextThreshold:
      m.long_context_threshold_tokens > 0
        ? `long-context > ${m.long_context_threshold_tokens.toLocaleString("en-US")} tokens`
        : "no long-context tier",
    status: m.status || "unknown",
  };
}

// formatInUseMessage renders the ConfirmDialog body for a 409 in_use:
// affected users and agents, with the binding slot shown.
export function formatInUseMessage(usage: ModelUsageEntry[]): string {
  if (usage.length === 0) {
    return "This is still referenced. Continuing will force the change and converge any virtual keys.";
  }
  const parts = usage.map((u) => {
    if (u.agent_id) {
      const slot = u.slot ? ` (${u.slot})` : "";
      const owner = u.user_email ? ` — ${u.user_email}` : "";
      return `${u.agent_name || "unnamed agent"}${slot}${owner}`;
    }
    return `${u.user_email || "a user"} (grant)`;
  });
  return `Still referenced by ${usage.length} user(s)/agent(s): ${parts.join("; ")}. Continuing will revoke the grants, unbind those agents and converge their virtual keys.`;
}

// formatCascadeReport summarises the deprecate/force 200 report so the
// admin sees the blast radius (affected agents/users) up front.
export function formatCascadeReport(report: CascadeReport): string {
  const op = report.operation || "change";
  const users = Array.isArray(report.affected_users) ? report.affected_users.length : 0;
  const agents = report.affected_agents ?? 0;
  const keys = Array.isArray(report.key_actions) ? report.key_actions.length : 0;
  const failures = Array.isArray(report.failures) ? report.failures.length : 0;
  const tail =
    failures > 0
      ? ` — WARNING: ${failures} key convergence failure(s); retry or run the gateway key reconcile.`
      : ` — ${keys} virtual key(s) converged.`;
  return `${op}: ${agents} agent(s) and ${users} user(s) affected${tail}`;
}

// --- Grant editor helpers ----------------------------------------------

export function grantedModelIds(models: AIModel[]): string[] {
  return models.map((m) => m.id);
}

export function grantDiff(current: string[], desired: string[]): { added: string[]; removed: string[] } {
  const currentSet = new Set(current);
  const desiredSet = new Set(desired);
  return {
    added: [...desiredSet].filter((id) => !currentSet.has(id)),
    removed: [...currentSet].filter((id) => !desiredSet.has(id)),
  };
}
