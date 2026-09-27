import { describe, expect, it } from "vitest";

import { ApiError } from "./api";
import {
  canEditPromptScope,
  extractPromptError,
  meterLevel,
  meterPercent,
  promptUserMessage,
  PROMPT_VALIDATE_DEBOUNCE_MS,
  revisionRestoreContent,
  saveBlockedByValidation,
  sortTiersForDisplay,
  tierBadge,
  RESERVED_TOKENS_ERROR,
  TOKEN_CAP_EXCEEDED_ERROR,
  UNKNOWN_TEMPLATE_VARS_ERROR,
  type EffectiveTier,
  type PromptValidateResponse,
  type PromptRevision,
} from "./prompt";

function validateResponse(overrides: Partial<PromptValidateResponse> = {}): PromptValidateResponse {
  return {
    valid: true,
    scope: "squad",
    tokens: 100,
    soft_warn: 1500,
    hard_cap: 2000,
    ...overrides,
  };
}

describe("meterLevel", () => {
  it("is ok at or below the soft limit", () => {
    expect(meterLevel(0, 1500, 2000)).toBe("ok");
    expect(meterLevel(1499, 1500, 2000)).toBe("ok");
    expect(meterLevel(1500, 1500, 2000)).toBe("ok");
  });

  it("warns above the soft limit but at or below the hard cap", () => {
    expect(meterLevel(1501, 1500, 2000)).toBe("warn");
    expect(meterLevel(2000, 1500, 2000)).toBe("warn");
  });

  it("is over only when the hard cap is exceeded (CP semantics: > hard)", () => {
    expect(meterLevel(2001, 1500, 2000)).toBe("over");
    expect(meterLevel(10_000, 1500, 2000)).toBe("over");
  });
});

describe("meterPercent", () => {
  it("scales against the hard cap", () => {
    expect(meterPercent(1000, 2000)).toBe(50);
    expect(meterPercent(2000, 2000)).toBe(100);
  });

  it("clamps out-of-range values", () => {
    expect(meterPercent(5000, 2000)).toBe(100);
    expect(meterPercent(-10, 2000)).toBe(0);
    expect(meterPercent(100, 0)).toBe(0);
  });
});

describe("extractPromptError", () => {
  it("extracts the reserved-tokens error from a validate response body", () => {
    const body = { valid: false, scope: "agent", tokens: 0, soft_warn: 6000, hard_cap: 8000, error: { code: RESERVED_TOKENS_ERROR, message: "prompt contains reserved <skquad_ block delimiters" } };
    const err = extractPromptError(body);
    expect(err).not.toBeNull();
    expect(err?.code).toBe(RESERVED_TOKENS_ERROR);
  });

  it("extracts the unknown-template-vars error from an ApiError body", () => {
    const apiErr = new ApiError(400, "bad request", {
      error: { code: UNKNOWN_TEMPLATE_VARS_ERROR, message: "unknown template variable(s): {{agent.credentials}}" },
    });
    const err = extractPromptError(apiErr);
    expect(err?.code).toBe(UNKNOWN_TEMPLATE_VARS_ERROR);
    expect(err?.message).toContain("{{agent.credentials}}");
  });

  it("extracts the token-cap error with the server token report message", () => {
    const apiErr = new ApiError(400, "bad request", {
      error: { code: TOKEN_CAP_EXCEEDED_ERROR, message: "organization prompt has 3000 tokens, hard cap is 2000" },
      scope: "organization",
      tokens: 3000,
      soft_warn: 1500,
      hard_cap: 2000,
    });
    const err = extractPromptError(apiErr);
    expect(err?.code).toBe(TOKEN_CAP_EXCEEDED_ERROR);
    expect(err?.message).toContain("hard cap is 2000");
  });

  it("returns null for non-prompt errors and empty bodies", () => {
    expect(extractPromptError(new ApiError(404, "not found", { error: { code: "not_found", message: "gone" } }))).toBeNull();
    expect(extractPromptError(undefined)).toBeNull();
    expect(extractPromptError({ message: "no error envelope" })).toBeNull();
    expect(extractPromptError({ error: { message: "codeless" } })).toBeNull();
  });
});

describe("promptUserMessage", () => {
  it("renders a stable line for reserved tokens", () => {
    expect(promptUserMessage({ code: RESERVED_TOKENS_ERROR, message: "whatever" })).toContain("Reserved block delimiters");
  });

  it("passes through the server message for unknown vars and cap over", () => {
    expect(promptUserMessage({ code: UNKNOWN_TEMPLATE_VARS_ERROR, message: "unknown template variable(s): {{x}}" })).toContain("{{x}}");
    expect(promptUserMessage({ code: TOKEN_CAP_EXCEEDED_ERROR, message: "organization prompt has 3000 tokens, hard cap is 2000" })).toContain("hard cap");
  });
});

describe("saveBlockedByValidation", () => {
  it("blocks only on an explicit invalid result", () => {
    expect(saveBlockedByValidation(null)).toBe(false);
    expect(saveBlockedByValidation(validateResponse())).toBe(false);
    expect(saveBlockedByValidation(validateResponse({ valid: false, error: { code: RESERVED_TOKENS_ERROR, message: "x" } }))).toBe(true);
  });
});

describe("tierBadge", () => {
  it("labels the four tiers with distinct trust copy", () => {
    expect(tierBadge("platform").label).toBe("Platform");
    expect(tierBadge("organization").label).toBe("Organization");
    expect(tierBadge("squad").label).toBe("Squad");
    expect(tierBadge("agent").label).toBe("Agent");
    const classes = new Set(["platform", "organization", "squad", "agent"].map((t) => tierBadge(t).className));
    expect(classes.size).toBe(4);
    expect(tierBadge("agent").trust).toContain("cannot relax");
  });

  it("degrades gracefully for unknown tiers", () => {
    expect(tierBadge("mystery").className).toBe("tier-unknown");
  });
});

describe("sortTiersForDisplay", () => {
  function tier(name: string): EffectiveTier {
    return { name, tokens: 10, soft_warn: 100, hard_cap: 200 };
  }

  it("orders the four tiers platform → organization → squad → agent", () => {
    const shuffled = [tier("agent"), tier("squad"), tier("platform"), tier("organization")];
    expect(sortTiersForDisplay(shuffled).map((t) => t.name)).toEqual(["platform", "organization", "squad", "agent"]);
  });

  it("keeps unknown tiers at the end without dropping known ones", () => {
    const mixed = [tier("weird"), tier("agent"), tier("platform"), tier("organization"), tier("squad")];
    expect(sortTiersForDisplay(mixed).map((t) => t.name)).toEqual(["platform", "organization", "squad", "agent", "weird"]);
  });

  it("handles an empty tier list", () => {
    expect(sortTiersForDisplay([])).toEqual([]);
  });
});

describe("revisionRestoreContent", () => {
  function rev(content: string): PromptRevision {
    return {
      id: "r1",
      scope: "squad",
      scope_id: "s1",
      content,
      tokens: 42,
      sha256: "abc",
      saved_by: "ross",
      saved_at: "2026-09-27T00:00:00Z",
    };
  }

  it("prefills the editor with the revision content verbatim", () => {
    expect(revisionRestoreContent(rev("hello prompt"))).toBe("hello prompt");
  });

  it("falls back to empty string for missing revisions", () => {
    expect(revisionRestoreContent(null)).toBe("");
    expect(revisionRestoreContent(undefined)).toBe("");
  });
});

describe("canEditPromptScope (admin gating)", () => {
  it("restricts the organization tier to platform admins", () => {
    expect(canEditPromptScope("platform_admin", "organization")).toBe(true);
    expect(canEditPromptScope("user", "organization")).toBe(false);
    expect(canEditPromptScope(undefined, "organization")).toBe(false);
    expect(canEditPromptScope(null, "organization")).toBe(false);
  });

  it("opens squad/agent tiers to any role (ownership enforced server-side)", () => {
    for (const role of ["platform_admin", "user", undefined, null]) {
      expect(canEditPromptScope(role, "squad")).toBe(true);
      expect(canEditPromptScope(role, "agent")).toBe(true);
    }
  });
});

describe("validate-on-edit timing", () => {
  it("debounces the dry-run at a keystroke-friendly interval", () => {
    expect(PROMPT_VALIDATE_DEBOUNCE_MS).toBeGreaterThan(100);
    expect(PROMPT_VALIDATE_DEBOUNCE_MS).toBeLessThanOrEqual(1000);
  });
});
