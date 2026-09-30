// S-178: per-agent thinking level helpers for the agent screen composer.
// The selector defaults to the median value ("medium") whenever the
// agent has no explicit (or a malformed) thinking_level, mirroring the
// control-plane's accepted set: "", "low", "medium", "high".

export const THINKING_LEVELS = ["low", "medium", "high"] as const;

export type ThinkingLevel = (typeof THINKING_LEVELS)[number];

export const DEFAULT_THINKING_LEVEL: ThinkingLevel = "medium";

/** Normalize a stored thinking_level to a selectable level. Unknown,
 * empty, or missing values fall back to the median ("medium") so the
 * composer always shows a valid selection. */
export function resolveThinkingLevel(value: unknown): ThinkingLevel {
  const v = typeof value === "string" ? value.trim().toLowerCase() : "";
  return (THINKING_LEVELS as readonly string[]).includes(v)
    ? (v as ThinkingLevel)
    : DEFAULT_THINKING_LEVEL;
}

/** Human label for a level (capitalized). */
export function thinkingLevelLabel(level: ThinkingLevel): string {
  return level.charAt(0).toUpperCase() + level.slice(1);
}
