// S-PROMPT WP4 — UI logic for the layered prompt system (ADR-0011, plan §5/§6).
// Browser-independent pieces (meter thresholds, error-code parsing, tier
// badges, revision helpers, scope gating) live here so they are unit-testable
// in the node vitest environment, matching the web/ test conventions.

import { ApiError } from "./api";

// --- Scopes & error codes -------------------------------------------------

export type PromptScope = "organization" | "squad" | "agent";

export const PROMPT_SCOPES: readonly PromptScope[] = ["organization", "squad", "agent"];

export const RESERVED_TOKENS_ERROR = "prompt_contains_reserved_tokens";
export const UNKNOWN_TEMPLATE_VARS_ERROR = "prompt_unknown_template_vars";
export const TOKEN_CAP_EXCEEDED_ERROR = "prompt_token_cap_exceeded";

export function isPromptScope(value: string): value is PromptScope {
  return (PROMPT_SCOPES as readonly string[]).includes(value);
}

// --- Token meter ----------------------------------------------------------

export type TokenMeterLevel = "ok" | "warn" | "over";

// meterLevel mirrors the control-plane semantics in checkPromptDraft:
// over only when tokens EXCEED the cap (== cap still passes).
export function meterLevel(tokens: number, softWarn: number, hardCap: number): TokenMeterLevel {
  if (tokens > hardCap) return "over";
  if (tokens > softWarn) return "warn";
  return "ok";
}

// meterPercent scales the bar against the hard cap, clamped to [0, 100].
export function meterPercent(tokens: number, hardCap: number): number {
  if (hardCap <= 0) return 0;
  const pct = (tokens / hardCap) * 100;
  if (pct < 0) return 0;
  if (pct > 100) return 100;
  return Math.round(pct);
}

// --- Validation / error parsing -------------------------------------------

export type PromptValidationError = { code: string; message: string };

export type PromptValidateResponse = {
  valid: boolean;
  scope: string;
  tokens: number;
  soft_warn: number;
  hard_cap: number;
  warnings?: string[];
  error?: { code: string; message: string };
};

// extractPromptError pulls the structured prompt error out of either a
// POST /prompt/validate response body or a failed save (ApiError whose
// body is the {error:{code,message}} envelope). Returns null when the
// error is not one of the three prompt codes (or absent).
export function extractPromptError(src: unknown): PromptValidationError | null {
  const body = src instanceof ApiError ? src.body : src;
  const errObj = (body as { error?: { code?: unknown; message?: unknown } } | null | undefined)?.error;
  if (!errObj || typeof errObj.code !== "string") return null;
  const known =
    errObj.code === RESERVED_TOKENS_ERROR ||
    errObj.code === UNKNOWN_TEMPLATE_VARS_ERROR ||
    errObj.code === TOKEN_CAP_EXCEEDED_ERROR;
  if (!known) return null;
  return { code: errObj.code, message: typeof errObj.message === "string" ? errObj.message : "invalid prompt" };
}

// promptUserMessage renders a plain-language line for each error code.
// The server message is already human-readable; this keeps the copy stable
// even if a message ever changes upstream.
export function promptUserMessage(err: PromptValidationError): string {
  switch (err.code) {
    case RESERVED_TOKENS_ERROR:
      return "Reserved block delimiters: prompt text may not contain <skquad_ …> tags — they belong to the platform.";
    case UNKNOWN_TEMPLATE_VARS_ERROR:
      return err.message || "Unknown template variable(s) in prompt.";
    case TOKEN_CAP_EXCEEDED_ERROR:
      return err.message || "Prompt exceeds the hard token cap for this tier.";
    default:
      return err.message;
  }
}

// saveBlockedByValidation reports whether a draft must not be saved.
export function saveBlockedByValidation(result: PromptValidateResponse | null): boolean {
  return !!result && result.valid === false;
}

// --- Revisions ------------------------------------------------------------

export type PromptRevision = {
  id: string;
  scope: string;
  scope_id: string;
  content: string;
  tokens: number;
  sha256: string;
  saved_by: string;
  saved_at: string;
};

// revisionRestoreContent is the editor prefill for a 'restore' click.
// Rollback is intentionally NOT a dedicated API: the prefilled content is
// re-saved through the normal save path, which appends a NEW revision
// (append-only history, resolved decision Q5).
export function revisionRestoreContent(rev: PromptRevision | null | undefined): string {
  return rev?.content ?? "";
}

// --- Effective-prompt preview ---------------------------------------------

export type EffectiveTier = {
  name: string;
  tokens: number;
  soft_warn: number;
  hard_cap: number;
  content?: string;
};

export type EffectivePrompt = {
  prompt?: string;
  sha256: string;
  total_tokens: number;
  warnings?: string[];
  tiers: EffectiveTier[];
};

// Display order: trust flows top-down, earliest block wins (plan §2.4 D1).
export const TIER_DISPLAY_ORDER: readonly string[] = ["platform", "organization", "squad", "agent"];

export type TierBadge = { label: string; trust: string; className: string };

export function tierBadge(name: string): TierBadge {
  switch (name) {
    case "platform":
      return { label: "Platform", trust: "highest trust · operator-controlled", className: "tier-platform" };
    case "organization":
      return { label: "Organization", trust: "org-wide context · admin-maintained", className: "tier-organization" };
    case "squad":
      return { label: "Squad", trust: "squad context · owner-maintained", className: "tier-squad" };
    case "agent":
      return { label: "Agent", trust: "agent identity · lowest tier, cannot relax higher ones", className: "tier-agent" };
    default:
      return { label: name, trust: "unknown tier", className: "tier-unknown" };
  }
}

// sortTiersForDisplay orders tiers platform→organization→squad→agent;
// any unexpected tier keeps relative order at the end (defensive — the
// composer emits the four known tiers).
export function sortTiersForDisplay(tiers: EffectiveTier[]): EffectiveTier[] {
  const known: EffectiveTier[] = [];
  const rest: EffectiveTier[] = [];
  for (const name of TIER_DISPLAY_ORDER) {
    const found = tiers.filter((t) => t.name === name);
    known.push(...found);
  }
  for (const t of tiers) {
    if (!TIER_DISPLAY_ORDER.includes(t.name)) rest.push(t);
  }
  return [...known, ...rest];
}

// --- Scope-based edit gating ----------------------------------------------
// Organization prompt is platform-admin only (mirrors requirePlatformAdmin
// on PUT /settings/prompt). Squad/agent tiers are editable by their
// owners — ownership itself is enforced server-side, so the UI opens the
// editor to any authenticated user on the entity's own page.

export function canEditPromptScope(role: string | null | undefined, scope: PromptScope): boolean {
  if (scope === "organization") {
    return (role ?? "") === "platform_admin";
  }
  return true;
}

// --- Validate-on-edit timing -----------------------------------------------

// PROMPT_VALIDATE_DEBOUNCE_MS paces POST /prompt/validate while typing:
// responsive enough to flag reserved tokens mid-sentence, cheap enough to
// keep the control-plane out of the keystroke path.
export const PROMPT_VALIDATE_DEBOUNCE_MS = 450;
