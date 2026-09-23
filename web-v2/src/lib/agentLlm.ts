// WP7 (S-112) — Agent LLM binding logic layer (ADR-0010 D4).
//
// Browser-independent helpers for the agent page's LLM tab: option
// filtering against the caller's granted+active models (GET /models/me),
// soft bind-time warning evaluation, and the PATCH payload shape for
// /agents/{id}. Kept out of the React components so the contract shape
// is unit-testable in the node environment, matching the aimodels.ts
// "logic layer" philosophy.
//
// Backend contract (control-plane/internal/httpapi/server.go, PATCH agent):
//   ai_model_id / fallback_ai_model_id are pointer-semantics strings:
//   omitted = leave unchanged, "" = clear the slot, id = bind.
//   Clearing primary while a fallback remains is a 400; fallback ==
//   primary is a 400 (fallback_same_as_primary).

import { formatRate, type AIModel } from "./aimodels";

// --- Option filtering --------------------------------------------------

export function isActiveModel(m: AIModel): boolean {
  // Treat a missing status as active (mirrors the registry convention);
  // anything other than "active" (e.g. "deprecated") is not selectable.
  return (m.status || "active") === "active";
}

// selectableModels is the picker source: granted + active only.
// /models/me already filters both server-side, but stale client caches
// and defensive rendering mean we filter again here.
export function selectableModels(models: AIModel[]): AIModel[] {
  return (models || []).filter(isActiveModel);
}

// fallbackChoices returns the fallback picker options: the selectable
// list minus the currently-selected primary (a model may not be its own
// fallback — the backend rejects that with 400 fallback_same_as_primary).
export function fallbackChoices(models: AIModel[], primaryId: string): AIModel[] {
  return selectableModels(models).filter((m) => m.id !== (primaryId || ""));
}

export function findModelById(models: AIModel[], id: string | undefined | null): AIModel | undefined {
  if (!id) return undefined;
  return (models || []).find((m) => m.id === id);
}

// withCurrentOption keeps a select renderable when the agent's existing
// binding points at a model the caller can no longer select (deprecated
// or revoked since the binding was made): if the model object is known
// it is prepended to the options; if it is unknown entirely (revoked out
// of /models/me) staleCurrent is reported so the UI can show the bare
// id as a placeholder rather than silently blanking the saved value.
export function withCurrentOption(
  options: AIModel[],
  allModels: AIModel[],
  currentId: string | undefined | null,
): { models: AIModel[]; staleCurrent: boolean } {
  const id = currentId || "";
  if (id === "") return { models: options, staleCurrent: false };
  if (options.some((m) => m.id === id)) return { models: options, staleCurrent: false };
  const current = findModelById(allModels, id);
  return { models: current ? [current, ...options] : options, staleCurrent: true };
}

// --- Soft bind-time warnings -------------------------------------------
//
// Advisory only: warnings NEVER block Save (the backend performs the real
// validation). They surface consequences the picker cannot express:
//   cost          fallback input rate > primary input rate
//   capability    fallback lacks tool calling or has a smaller context window
//   same_provider fallback shares the primary's provider — no availability gain

export const BINDING_WARNING_CODES = ["cost", "capability", "same_provider"] as const;
export type BindingWarningCode = (typeof BINDING_WARNING_CODES)[number];

export type BindingWarning = {
  code: BindingWarningCode;
  message: string;
};

function inputRate(m: AIModel): number | null {
  const raw = m.pricing?.input_per_1m;
  return typeof raw === "number" && Number.isFinite(raw) ? raw : null;
}

// bindingWarnings evaluates W1–W3. Only meaningful when BOTH a primary
// and a fallback are selected — with no fallback there is nothing to
// warn about, and an unresolvable model id (not in the caller's list)
// is treated the same as "no selection" rather than guessed at.
export function bindingWarnings(
  primary: AIModel | null | undefined,
  fallback: AIModel | null | undefined,
): BindingWarning[] {
  if (!primary || !fallback) return [];
  const warnings: BindingWarning[] = [];

  // W1 — cost: fallback charges more per input token than the primary.
  // Only evaluated when BOTH rates are known; unknown pricing must not
  // fabricate a comparison.
  const pIn = inputRate(primary);
  const fIn = inputRate(fallback);
  if (pIn !== null && fIn !== null && fIn > pIn) {
    warnings.push({
      code: "cost",
      message: `Fallback input rate (${formatRate(fIn)}) is higher than the primary (${formatRate(pIn)}) — failover costs more per token.`,
    });
  }

  // W2 — capability: tool calling may break, or context may truncate.
  const capabilityIssues: string[] = [];
  if (!fallback.supports_tools) {
    capabilityIssues.push("it does not support tool calling");
  }
  if (
    primary.context_window > 0 &&
    fallback.context_window > 0 &&
    fallback.context_window < primary.context_window
  ) {
    capabilityIssues.push(
      `its context window (${fallback.context_window.toLocaleString("en-US")} tokens) is smaller than the primary's (${primary.context_window.toLocaleString("en-US")} tokens)`,
    );
  }
  if (capabilityIssues.length > 0) {
    warnings.push({
      code: "capability",
      message: `Fallback may break tool calling or truncate context — ${capabilityIssues.join(" and ")}.`,
    });
  }

  // W3 — availability: same provider means the failover shares the
  // primary's outage domain.
  if (fallback.provider_id && fallback.provider_id === primary.provider_id) {
    warnings.push({
      code: "same_provider",
      message: "Fallback is on the same provider as the primary — no availability gain if that provider has an outage.",
    });
  }

  return warnings;
}

// --- PATCH payload ------------------------------------------------------

export type AgentBindingPatch = {
  ai_model_id: string;
  fallback_ai_model_id: string;
};

// buildBindingPayload produces the PATCH /agents/{id} body. Both keys
// are always sent: the backend's pointer semantics treat "" as "clear
// the slot" and omission as "leave unchanged", so an explicit "" is how
// a fallback is cleared. Primary is required by the UI (empty → throw).
export function buildBindingPayload(
  primaryId: string,
  fallbackId?: string | null,
): AgentBindingPatch {
  const primary = (primaryId || "").trim();
  if (primary === "") {
    throw new Error("ai_model_id is required");
  }
  const fallback = (fallbackId || "").trim();
  if (fallback !== "" && fallback === primary) {
    throw new Error("fallback_ai_model_id must differ from ai_model_id");
  }
  return { ai_model_id: primary, fallback_ai_model_id: fallback };
}

// modelLabel is the picker display text for a model.
export function modelLabel(m: AIModel): string {
  return m.display_name || m.model_name;
}
